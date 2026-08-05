package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/manifest"
	"github.com/Reederey87/DevStrap/internal/pathkey"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

// ErrPartialImport signals that the import completed but one or more manifest
// entries could not be registered. It mirrors ErrPartialMaterialize: the batch
// is never aborted by a single bad entry — recovering most of a namespace beats
// recovering none of it — but the command exits non-zero so a scripted recovery
// cannot mistake a partial import for a whole one.
var ErrPartialImport = errors.New("one or more manifest entries were not registered")

// fullSHA matches a resolved 40-hex commit id, used only to keep a PINNED
// `version` from being mistaken for a branch name (see importManifest).
var fullSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// importResult is the `devstrap import --json` document (AD-7), a machine
// contract surface: exactly one document on stdout, warnings on stderr, exit
// code alone signals success.
type importResult struct {
	SchemaVersion int    `json:"schema_version"`
	Manifest      string `json:"manifest"`
	// ManifestSchemaVersion is the version the FILE declares, which may be
	// newer than SchemaVersion (this binary's). Evolution is additive-only, so
	// a newer file is read rather than refused.
	ManifestSchemaVersion int      `json:"manifest_schema_version"`
	Registered            int      `json:"registered"`
	AlreadyPresent        int      `json:"already_present"`
	Skipped               int      `json:"skipped"`
	Warnings              []string `json:"warnings,omitempty"`
}

func newImportCommand(stdout io.Writer, opts *options) *cobra.Command {
	var manifestPath string
	cmd := &cobra.Command{
		Use:   "import --manifest <path>",
		Short: "Register projects from a plain-text workspace manifest",
		Long: "Register projects from a workspace manifest written by `devstrap export`\n" +
			"(or any vcstool .repos file).\n\n" +
			"This is a REGISTRATION plane, not a materialization plane: it writes\n" +
			"namespace rows and stops. `devstrap sync` (or `devstrap materialize`)\n" +
			"then clones them through the one existing materialization path.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if manifestPath == "" {
				return appError{code: exitUsage, err: fmt.Errorf("--manifest is required")}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			result, err := importManifest(cmd.Context(), cmd.ErrOrStderr(), store, opts, manifestPath)
			if err != nil {
				return err
			}
			if err := opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Registered %d project(s) from %s (%d already present, %d skipped)\nRun `devstrap sync` to materialize them.\n",
					result.Registered, result.Manifest, result.AlreadyPresent, result.Skipped)
				return err
			}, result); err != nil {
				return err
			}
			if result.Skipped > 0 {
				return appError{code: exitGeneric, err: fmt.Errorf("%w: %d entr(ies) skipped", ErrPartialImport, result.Skipped)}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to the workspace manifest to import (required)")
	return cmd
}

// importManifest reads a manifest and registers every entry it can. It never
// clones, fetches, or writes into the managed root: `spec/13` already refused a
// second materialization path for `/v1/status`, and an importer that cloned
// would be exactly that.
func importManifest(ctx context.Context, stderr io.Writer, store *state.Store, opts *options, path string) (importResult, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the manifest path is the user's own argument, by design.
	if err != nil {
		return importResult{}, appError{code: exitInvalidConfig, err: fmt.Errorf("read workspace manifest: %w", err)}
	}
	m, err := manifest.Decode(raw)
	if err != nil {
		return importResult{}, appError{code: exitInvalidConfig, err: err}
	}

	result := importResult{
		SchemaVersion:         manifest.SchemaVersion,
		Manifest:              path,
		ManifestSchemaVersion: m.DevStrap.SchemaVersion,
	}
	warn := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		result.Warnings = append(result.Warnings, msg)
		_, _ = fmt.Fprintf(stderr, "warning: %s\n", msg)
	}

	if m.DevStrap.SchemaVersion > manifest.SchemaVersion {
		// Additive-only evolution (spec/13 § Machine contract surfaces): every
		// key this binary knows still means the same thing at a higher version,
		// so read it and say what was ignored rather than refusing recovery.
		warn("manifest declares schema_version %d and this devstrap understands %d; unrecognized keys are ignored",
			m.DevStrap.SchemaVersion, manifest.SchemaVersion)
	}
	if local, err := store.WorkspaceID(ctx); err == nil && m.DevStrap.WorkspaceID != "" && m.DevStrap.WorkspaceID != local {
		warn("manifest was exported from workspace %s; this device's workspace is %s", m.DevStrap.WorkspaceID, local)
	}
	if m.DevStrap.Pinned {
		// The two documented recovery paths for one file otherwise diverge in
		// silence: `vcs import` checks out the pinned SHA, while devstrap
		// import + sync clones default-branch TIPS. Registration is a namespace
		// plane and carries no per-project revision, so the honest move is to
		// say so rather than to appear to honour the pin.
		warn("manifest is pinned, but `devstrap import` registers projects only — the recorded SHAs are NOT checked out. " +
			"`devstrap sync` clones each project's default branch; use `vcs import` if you need the pinned revisions")
	}

	root := opts.paths().Root
	for _, p := range manifestPaths(m) {
		entry, ok := resolveManifestEntry(m, p, warn)
		if !ok {
			result.Skipped++
			continue
		}
		clean, err := pathkey.Clean(p)
		if err != nil {
			warn("%s: refusing unsafe namespace path: %v", p, err)
			result.Skipped++
			continue
		}
		status, registered, err := registerManifestEntry(ctx, store, root, clean.Display, entry)
		if err != nil {
			return importResult{}, err
		}
		switch status {
		case manifestEntryRegistered:
			result.Registered++
		case manifestEntryPresent:
			result.AlreadyPresent++
		case manifestEntryConflict:
			warn("%s: already registered as %s; refusing to overwrite it from a manifest (use `devstrap add`/`devstrap scan --adopt` to change it deliberately)", clean.Display, registered)
			result.Skipped++
		}
	}
	return result, nil
}

// manifestEntry is one validated, importable row.
type manifestEntry struct {
	Type          string
	RemoteURL     string
	RemoteKey     string
	DefaultBranch string
	LFSPolicy     string
	ForgeKind     string
	// MaterializationPolicy round-trips rather than being hardcoded. "lazy" is
	// the only value anywhere today, so this is not a behavioural fix — it
	// stops the round trip silently losing the field the day a second value
	// ships (W13-02 review).
	MaterializationPolicy string
}

var importableTypes = map[string]bool{
	"git_repo": true, "local_git": true, "draft_project": true, "plain_folder": true,
}

// resolveManifestEntry validates one path's `repositories` + `devstrap.projects`
// halves into a registerable entry, reporting and rejecting anything it cannot
// register rather than writing a row that could never materialize.
func resolveManifestEntry(m manifest.Manifest, p string, warn func(string, ...any)) (manifestEntry, bool) {
	project := m.DevStrap.Projects[p]
	repo, hasRepo := m.Repositories[p]

	typ := project.Type
	if typ == "" {
		if !hasRepo {
			warn("%s: no type under devstrap.projects and no repositories entry; nothing to register", p)
			return manifestEntry{}, false
		}
		// A bare vcstool .repos file (no `devstrap` key at all) is a legitimate
		// input: every entry it lists is a clonable git repository.
		typ = "git_repo"
	}
	if !importableTypes[typ] {
		warn("%s: unknown project type %q", p, typ)
		return manifestEntry{}, false
	}
	// A manifest is hand-editable plain text and, after a total local loss, may
	// be the only input left. Validate what has a validator here rather than
	// registering a row whose typo only surfaces at materialize time, on the one
	// project that happens to trigger an LFS or fetch operation. Empty stays
	// legitimate in both cases: the field was simply omitted.
	if project.LFSPolicy != "" && !validLFSPolicy(project.LFSPolicy) {
		warn("%s: unsupported lfs_policy %q", p, project.LFSPolicy)
		return manifestEntry{}, false
	}
	if project.DefaultBranch != "" && !dsgit.SafeBranchName(project.DefaultBranch) {
		warn("%s: unusable default_branch %q", p, project.DefaultBranch)
		return manifestEntry{}, false
	}
	entry := manifestEntry{
		Type:                  typ,
		DefaultBranch:         project.DefaultBranch,
		LFSPolicy:             project.LFSPolicy,
		MaterializationPolicy: defaultString(project.MaterializationPolicy, "lazy"),
		ForgeKind:             project.ForgeKind,
	}
	if typ != "git_repo" {
		return entry, true
	}

	if !hasRepo {
		warn("%s: typed git_repo but absent from `repositories`, so it has no URL to clone", p)
		return manifestEntry{}, false
	}
	if repo.Type != manifest.VCSTypeGit {
		// vcstool also speaks hg/svn/bzr. DevStrap's materialization plane is
		// git-only, so registering one of those would create a row that can
		// never materialize.
		warn("%s: repository type %q is not supported (devstrap materializes git only)", p, repo.Type)
		return manifestEntry{}, false
	}
	remoteKey, err := dsgit.CanonicalRemoteKey(repo.URL)
	if err != nil {
		warn("%s: unusable remote URL: %v", p, err)
		return manifestEntry{}, false
	}
	entry.RemoteURL, entry.RemoteKey = repo.URL, remoteKey
	if entry.DefaultBranch == "" && !m.DevStrap.Pinned && !fullSHA.MatchString(repo.Version) && dsgit.SafeBranchName(repo.Version) {
		// A third-party .repos file carries the branch only in `version`. Adopt
		// it as the default branch — but never when the manifest says it is
		// pinned, or when the value is plainly a resolved SHA, because a commit
		// id recorded as a branch name would break every later fetch. An unusable
		// `version` declines the heuristic rather than skipping the entry, as a
		// pin and a SHA already do: the row still registers, and materialize
		// resolves its default branch from the remote.
		entry.DefaultBranch = repo.Version
	}
	return entry, true
}

type manifestEntryStatus int

const (
	manifestEntryRegistered manifestEntryStatus = iota
	manifestEntryPresent
	manifestEntryConflict
)

// registerManifestEntry writes one namespace row plus its `project.added`
// event, exactly as `devstrap add` and `scan --adopt` do, so an imported
// namespace propagates to the fleet like any other.
//
// It deliberately does NOT reuse adoptFindings: that path asks for
// materialization_state "available" and dirty "clean" because a scan just
// observed the checkout on disk. Import observed nothing — after a total local
// loss the tree is gone — so it asks for "skeleton" instead, and the project
// lands in store.SkeletonProjects for the existing sync/materialize pass to
// clone. (Tx.UpsertProject does not itself persist device_project_state, so the
// row's state stays empty until a materialize writes it; SkeletonProjects
// treats empty and "skeleton" identically, which is why the distinction is a
// statement of intent here rather than a behavioural difference today.)
func registerManifestEntry(ctx context.Context, store *state.Store, root, nsPath string, entry manifestEntry) (manifestEntryStatus, string, error) {
	localPath := filepath.Join(root, filepath.FromSlash(nsPath))
	status, registered := manifestEntryRegistered, ""
	// The clobber refusal reads inside the transaction it writes in. The writer
	// DSN is _txlock=immediate over a one-connection pool, so WithTx holds
	// SQLite's write lock from BEGIN and no other writer — another process, or a
	// daemon converging a peer's project.added in the background — can create the
	// row between the check and UpsertProject's ON CONFLICT DO UPDATE.
	err := store.WithTx(ctx, func(tx *state.Tx) error {
		existing, lookupErr := tx.ProjectByPath(ctx, nsPath)
		if lookupErr != nil && !errors.Is(lookupErr, state.ErrProjectNotFound) {
			// Do NOT fall through to the upsert. UpsertProject's ON CONFLICT DO
			// UPDATE overwrites remote_url/remote_key, so treating a transient read
			// failure as "absent" would bypass the very clobber refusal below.
			return fmt.Errorf("look up %s: %w", nsPath, lookupErr)
		}
		if lookupErr == nil {
			switch {
			case existing.Type != entry.Type:
				status, registered = manifestEntryConflict, existing.Type
			case entry.Type == "git_repo" && existing.RemoteKey != entry.RemoteKey:
				status, registered = manifestEntryConflict, existing.RemoteKey
			default:
				status = manifestEntryPresent
			}
			return nil
		}
		event, err := dssync.CreateProjectEventTx(ctx, store, tx, dssync.EventProjectAdded, dssync.ProjectPayload{
			Path:          nsPath,
			Type:          entry.Type,
			RemoteURL:     entry.RemoteURL,
			RemoteKey:     entry.RemoteKey,
			DefaultBranch: entry.DefaultBranch,
		})
		if err != nil {
			return err
		}
		_, err = tx.UpsertProject(ctx, state.UpsertProjectParams{
			Path:                  nsPath,
			Type:                  entry.Type,
			RemoteURL:             entry.RemoteURL,
			RemoteKey:             entry.RemoteKey,
			DefaultBranch:         entry.DefaultBranch,
			LFSPolicy:             entry.LFSPolicy,
			ForgeKind:             entry.ForgeKind,
			MaterializationPolicy: entry.MaterializationPolicy,
			LocalPath:             localPath,
			MaterializationState:  "skeleton",
			DirtyState:            "unknown",
			SourceEventHLC:        event.HLC,
			SourceEventDeviceID:   event.DeviceID,
			SourceEventID:         event.ID,
		})
		return err
	})
	if err != nil {
		return manifestEntryRegistered, "", err
	}
	return status, registered, nil
}

// manifestPaths returns the sorted union of both halves of the document, so a
// path present in only one of them is still considered (and reported) rather
// than silently dropped.
func manifestPaths(m manifest.Manifest) []string {
	seen := make(map[string]bool, len(m.Repositories)+len(m.DevStrap.Projects))
	for p := range m.Repositories {
		seen[p] = true
	}
	for p := range m.DevStrap.Projects {
		seen[p] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// defaultString returns v, or fallback when v is empty. A manifest written
// before a field existed simply omits it.
func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
