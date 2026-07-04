package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/Reederey87/DevStrap/internal/childenv"
	"github.com/Reederey87/DevStrap/internal/draftbundle"
	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/pathkey"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// ErrPartialMaterialize signals that the materialize pass completed but one or
// more projects failed (QUAL-03). The batch is never aborted by a single
// failure (EAGER-04), but the command exits non-zero so CI/cron gates and
// `devstrap materialize && ...` chains can detect a failed clone/hydrate.
var ErrPartialMaterialize = errors.New("one or more projects failed to materialize")

// ErrDraftNotMaterializable signals the expected interim state of a
// local_git/draft_project that has no synced bundle yet (P5-QUAL-01). It is a
// "skipped", not a "failed": a freshly synced device legitimately holds draft
// projects whose content has not been pushed, and counting them as failures
// re-broke the QUAL-03 exit-code gate (any such workspace exited non-zero).
var ErrDraftNotMaterializable = errors.New("content sync not yet materialized (no draft bundle synced)")

var (
	materializeHydrateProjectEnv   = hydrateProjectEnv
	materializeRebuildDependencies = rebuildDependencies
)

// materializeConcurrency returns the bounded worker count for the eager
// materialization pass (EAGER-04). It is capped so clone-everything across a
// large ~/Code does not exhaust file descriptors or network connections.
func materializeConcurrency() int {
	if n := runtime.NumCPU(); n < 4 {
		return n
	}
	return 4
}

func newMaterializeCommand(stdout io.Writer, opts *options) *cobra.Command {
	var partial bool
	cmd := &cobra.Command{
		Use:   "materialize [path]",
		Short: "Eagerly materialize skeleton projects (clone repos, hydrate env)",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			var projects []state.ProjectStatus
			if len(args) == 1 {
				p, err := store.ProjectByPath(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				projects = []state.ProjectStatus{p}
			} else {
				projects, err = store.SkeletonProjects(cmd.Context())
				if err != nil {
					return err
				}
			}
			results := materializePass(cmd.Context(), store, opts, projects, partial)
			// P5-CLI-01: route output through the shared renderer so --json
			// produces a structured result instead of being silently ignored.
			// P5-QUAL-01: report succeeded/skipped/failed so the normal interim
			// "no draft bundle yet" state is visible without polluting the exit
			// code.
			if err := opts.render(stdout, func(w io.Writer) error {
				_, _ = fmt.Fprintf(w, "Materialized %d/%d projects (%d skipped)\n", results.succeeded, results.total, results.skipped)
				if results.failed > 0 {
					_, _ = fmt.Fprintf(w, "%d project(s) failed; run 'devstrap doctor' or 'devstrap status' for details\n", results.failed)
				}
				return nil
			}, struct {
				Total     int `json:"total"`
				Succeeded int `json:"succeeded"`
				Skipped   int `json:"skipped"`
				Failed    int `json:"failed"`
			}{results.total, results.succeeded, results.skipped, results.failed}); err != nil {
				return err
			}
			if results.failed > 0 {
				// QUAL-03: exit non-zero ONLY when a project genuinely failed so
				// automation and CI gating on `devstrap materialize` can detect a
				// failed clone/hydrate. A draft awaiting its first bundle is
				// "skipped", not "failed", so a freshly synced workspace exits 0
				// (P5-QUAL-01). The batch still completes (EAGER-04 isolation).
				return appError{code: exitGeneric, err: fmt.Errorf("%w: %d/%d projects failed", ErrPartialMaterialize, results.failed, results.total)}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&partial, "partial", true, "use partial clone with blob filtering (--partial=false performs a full clone)")
	return cmd
}

type materializeResult struct {
	total     int
	succeeded int
	skipped   int
	failed    int
}

// materializePass runs the eager materialization pass over the given projects
// with bounded concurrency and per-project failure isolation (EAGER-01/04). A
// single project's failure marks it failed and continues; it never aborts the
// batch. Each project that materializes also gets its env profile hydrated
// (EAGER-03).
func materializePass(ctx context.Context, store *state.Store, opts *options, projects []state.ProjectStatus, partial bool) materializeResult {
	res := materializeResult{total: len(projects)}
	if len(projects) == 0 {
		return res
	}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(materializeConcurrency())
	for _, project := range projects {
		project := project
		g.Go(func() error {
			if gctx.Err() != nil {
				return nil
			}
			err := materializeOne(gctx, store, opts, project, partial)
			switch {
			case err == nil:
				mu.Lock()
				res.succeeded++
				mu.Unlock()
			case errors.Is(err, ErrDraftNotMaterializable):
				// P5-QUAL-01: a draft with no synced bundle yet is the normal
				// interim state, not a failure. Count it skipped and stay quiet.
				mu.Lock()
				res.skipped++
				mu.Unlock()
			default:
				logging.Logger(gctx).Warn("materialize failed, isolating failure",
					"path", project.Path, "type", project.Type, "err", err.Error())
				mu.Lock()
				res.failed++
				mu.Unlock()
			}
			return nil // EAGER-04: isolate, never abort the batch
		})
	}
	_ = g.Wait()
	return res
}

// materializeOne materializes a single project by type dispatch (EAGER-01,
// DRAFT-01):
//   - git_repo: blobless clone/fetch from its existing remote (repo content
//     rides git's own transport and never traverses the hub).
//   - local_git / draft_project: decrypt-and-extract the latest draft bundle
//     (DRAFT-02). Until a bundle exists, returns an honest "not yet
//     materialized" error instead of the misleading "not git_repo".
//   - plain_folder: create the skeleton directory (structure, no content).
func materializeOne(ctx context.Context, store *state.Store, opts *options, project state.ProjectStatus, partial bool) error {
	switch project.Type {
	case "git_repo":
		return materializeGitRepo(ctx, store, opts, project, partial)
	case "local_git", "draft_project":
		return materializeDraft(ctx, store, opts, project)
	case "plain_folder":
		return materializePlainFolder(ctx, store, opts, project)
	default:
		return fmt.Errorf("%s is %s; unknown project type", project.Path, project.Type)
	}
}

func materializeGitRepo(ctx context.Context, store *state.Store, opts *options, project state.ProjectStatus, partial bool) error {
	unlock, err := acquireRepoLock(opts.paths().Home, project.ID)
	if err != nil {
		return err
	}
	defer unlock()
	// P5-CLI-02: honor --partial. partial=true (default) does a blobless clone
	// (EAGER-01); --partial=false does a full clone for reliable offline
	// git blame/log -p (the GIT-06 caveat doctor warns about).
	localPath, err := hydrateProjectUnlocked(ctx, store, opts, project, partial)
	if err != nil {
		return err
	}
	// P6-GIT-04: honor the stored lfs_policy on the eager materialize path (the
	// whole-tree clone that must leave real content, not pointer files). This is
	// applied in the caller — NOT inside hydrateProjectUnlocked, which is shared
	// with worktree creation — so it runs on both the fresh clone and the
	// SkeletonProjects retry of a repo previously recorded "failed" (which
	// re-enters via the already-on-disk path), and never flips an unsatisfied
	// always-policy LFS repo to available/clean with pointers. A usable checkout
	// is guaranteed here: materialized-empty/broken-HEAD returns an error above.
	if err := applyMaterializeLFSPolicy(ctx, gitRunner(opts), project, localPath); err != nil {
		_ = store.UpdateProjectLocalState(ctx, project.ID, localPath, "failed", "unknown")
		return err
	}
	// DRAFT-05/P6-GIT-03: dependency restores run lockfile/package lifecycle
	// scripts, which are arbitrary repo-controlled code. Keep the existing
	// global opt-in gate, but run the rebuild before env hydrate so the
	// project's freshly decrypted .env is not sitting at $HOME/.env while those
	// scripts execute. This is defense in depth; the child still is not an OS
	// sandbox.
	if os.Getenv("DEVSTRAP_REBUILD_DEPS") != "" {
		if err := materializeRebuildDependencies(ctx, opts.paths().Home, project.Path, localPath); err != nil {
			logging.Logger(ctx).Warn("dependency rebuild failed", "path", project.Path, "err", err.Error())
		}
	}
	// EAGER-03: hydrate the env profile into the freshly materialized repo so
	// the project is usable, not just cloned. Best-effort: no env profile or an
	// existing .env means we skip silently.
	if err := materializeHydrateProjectEnv(ctx, store, opts, project, localPath); err != nil {
		logging.Logger(ctx).Warn("env hydrate skipped", "path", project.Path, "err", err.Error())
	}
	return nil
}

// materializeDraft handles local_git and draft_project content (DRAFT-01/02).
// It decrypts and extracts the latest age-encrypted draft bundle into the
// skeleton. When no bundle has been synced yet it returns an honest interim
// message so the namespace stops lying with "not git_repo".
func materializeDraft(ctx context.Context, store *state.Store, opts *options, project state.ProjectStatus) error {
	localPath := project.LocalPath
	if localPath == "" {
		localPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
	}
	if root := opts.paths().Root; root != "" {
		if err := pathkey.VerifyWithinRoot(root, localPath); err != nil {
			return fmt.Errorf("refusing to materialize outside managed root: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o750); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	// DRAFT-02: attempt to extract the latest synced draft bundle. The bundle
	// path is wired once draft snapshot events are applied; until a bundle
	// exists for this project, surface an honest interim state.
	bundle, err := store.LatestDraftSnapshot(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("read draft snapshot for %s: %w", project.Path, err)
	}
	if bundle == nil {
		if err := os.MkdirAll(localPath, 0o750); err != nil {
			return fmt.Errorf("create draft skeleton: %w", err)
		}
		if err := store.UpdateProjectLocalState(ctx, project.ID, localPath, "skeleton", "unknown"); err != nil {
			return err
		}
		// P5-QUAL-01: honest interim state, classified as "skipped" upstream
		// (not a failure) so a freshly synced workspace exits 0.
		return fmt.Errorf("%s is %s: %w", project.Path, project.Type, ErrDraftNotMaterializable)
	}
	if err := extractDraftBundle(ctx, store, opts, project, localPath, bundle); err != nil {
		_ = store.UpdateProjectLocalState(ctx, project.ID, localPath, "failed", "unknown")
		return err
	}
	return store.UpdateProjectLocalState(ctx, project.ID, localPath, "available", "clean")
}

func materializePlainFolder(ctx context.Context, store *state.Store, opts *options, project state.ProjectStatus) error {
	localPath := project.LocalPath
	if localPath == "" {
		localPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
	}
	if root := opts.paths().Root; root != "" {
		if err := pathkey.VerifyWithinRoot(root, localPath); err != nil {
			return fmt.Errorf("refusing to materialize outside managed root: %w", err)
		}
	}
	if err := os.MkdirAll(localPath, 0o750); err != nil {
		return fmt.Errorf("create plain folder: %w", err)
	}
	return store.UpdateProjectLocalState(ctx, project.ID, localPath, "available", "clean")
}

// hydrateProjectEnv hydrates a project's bound env profile into the project
// directory after materialization (EAGER-03). It is best-effort: if the project
// has no env profile, or the target .env already exists, it returns nil.
func hydrateProjectEnv(ctx context.Context, store *state.Store, opts *options, project state.ProjectStatus, localPath string) error {
	profile, bindings, err := store.EnvProfileForProject(ctx, project.ID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil // no env profile — nothing to hydrate
		}
		return err
	}
	target := filepath.Join(localPath, ".env")
	if _, err := os.Stat(target); err == nil {
		return nil // .env already exists — do not clobber
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat env target: %w", err)
	}
	content, _, err := hydratedEnvContent(ctx, opts, store, profile, bindings, target, false)
	if err != nil {
		return err
	}
	return writeHydratedEnvFile(target, content, false)
}

// extractDraftBundle reads the content-addressed draft blob from the local blob
// cache, decrypts it with the local device identity, and extracts it into the
// skeleton (DRAFT-02). The blob is fetched from the hub during sync; if it is
// not yet cached locally, the error tells the user to run sync.
func extractDraftBundle(ctx context.Context, store *state.Store, opts *options, project state.ProjectStatus, localPath string, snapshot *state.DraftSnapshot) error {
	ciphertext, err := readEnvBlob(opts.paths(), snapshot.BlobRef)
	if err != nil {
		return fmt.Errorf("read draft blob %s: %w (run 'devstrap sync' to fetch blobs from the hub)", snapshot.BlobRef, err)
	}
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		return err
	}
	keyStore, err := resolveKeyStore(ctx, opts.paths(), store)
	if err != nil {
		return err
	}
	identity, err := keyStore.Read(ctx, device.ID)
	if err != nil {
		return fmt.Errorf("read local device identity: %w", err)
	}
	// QUAL-01: use the same aggregate decompression budget Pack applied so a
	// bundle packed within per-project (or default) limits is never rejected
	// on extract. Accommodate the recorded packed size/count with at least the
	// Pack-side defaults so a customized larger limit is honored on receive.
	limits := draftbundle.Limits{
		MaxBytes: draftbundle.MaxBundleBytes,
		MaxFiles: draftbundle.MaxBundleFiles,
	}
	if snapshot.ByteSize > limits.MaxBytes {
		limits.MaxBytes = snapshot.ByteSize
	}
	if snapshot.FileCount > limits.MaxFiles {
		limits.MaxFiles = snapshot.FileCount
	}
	return draftbundle.ExtractWithLimits(ciphertext, identity.Private, localPath, limits)
}

// rebuildDependencies detects the project toolchain and runs the appropriate
// dependency restore from the lockfile (DRAFT-05). node_modules and build
// artifacts are never synced; they are rebuilt locally. This is opt-in
// (DEVSTRAP_REBUILD_DEPS) and logged, never automatic on metered/offline runs.
func rebuildDependencies(ctx context.Context, home, projectPath, localPath string) error {
	toolchains := []struct {
		marker  string
		command string
		args    []string
	}{
		{"package-lock.json", "npm", []string{"ci"}},
		{"pnpm-lock.yaml", "pnpm", []string{"install", "--frozen-lockfile"}},
		{"yarn.lock", "yarn", []string{"install", "--frozen-lockfile"}},
		{"uv.lock", "uv", []string{"sync", "--frozen"}},
		{"poetry.lock", "poetry", []string{"install", "--no-update"}},
		{"go.mod", "go", []string{"mod", "download"}},
		{"Cargo.lock", "cargo", []string{"fetch"}},
	}
	for _, tc := range toolchains {
		if _, err := os.Stat(filepath.Join(localPath, tc.marker)); err != nil {
			continue
		}
		logPath := rebuildLogPath(home, projectPath)
		return runRebuildCommand(ctx, localPath, tc.command, tc.args, logPath)
	}
	return nil // no recognized lockfile — nothing to rebuild
}

func rebuildLogPath(home, projectPath string) string {
	// The readable sanitized name can collide across distinct paths ("/" maps
	// to "_", which is also a passthrough character), so a short digest of the
	// raw path keeps log files distinct per project (CodeRabbit, PR #69).
	sum := sha256.Sum256([]byte(projectPath))
	name := sanitizeRebuildLogName(projectPath) + "-" + hex.EncodeToString(sum[:4])
	return filepath.Join(home, "logs", "rebuilds", name+".log")
}

func sanitizeRebuildLogName(projectPath string) string {
	var b strings.Builder
	for _, r := range filepath.ToSlash(projectPath) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "._")
	if name == "" {
		return "project"
	}
	return name
}

func runRebuildCommand(ctx context.Context, dir, command string, args []string, logPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("create rebuild log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // Rebuild log path is generated under DevStrap home from the namespace path.
	if err != nil {
		return fmt.Errorf("create rebuild log %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()
	//nolint:gosec // command is from a hardcoded toolchain table, not user input.
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// P5-SEC-03 (+ P5 review): dependency rebuild runs package-manager lifecycle
	// scripts (npm/pnpm postinstall, etc.) which are arbitrary code driven by an
	// attacker-influenceable lockfile/package.json from a cloned repo. Mirror the
	// agent runner's threat model (SECU-02): use AgentAllowlist — which omits
	// SSH_AUTH_SOCK and HOME — and repoint HOME to the project dir, so a malicious
	// postinstall cannot authenticate via the user's live ssh-agent or read the
	// real ~/.ssh, ~/.aws, ~/.npmrc, or ~/.config/gh. Dangerous
	// LD_*/DYLD_*/NODE_OPTIONS names are stripped too.
	env, err := childenv.FromOS(childenv.AgentAllowlist(), map[string]string{"HOME": dir})
	if err != nil {
		return fmt.Errorf("dependency rebuild failed (see %s): build rebuild environment: %w", logPath, err)
	}
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dependency rebuild failed (see %s): %w", logPath, err)
	}
	return nil
}
