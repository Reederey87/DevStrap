package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Reederey87/DevStrap/internal/manifest"
	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

// exportResult is the `devstrap export --json` document (AD-7). It is a
// machine contract surface (spec/13 § Machine contract surfaces): exactly one
// document on stdout, every warning on stderr, exit code alone signals success.
type exportResult struct {
	SchemaVersion int    `json:"schema_version"`
	Manifest      string `json:"manifest"`
	WorkspaceID   string `json:"workspace_id"`
	// Repositories counts the git_repo entries a third-party `vcs import` can
	// rebuild; Projects counts every namespace row the manifest records. The
	// two are deliberately separate numbers because the interop claim covers
	// only the first (spec/16 § Durability / disaster-recovery drill).
	Repositories int      `json:"repositories"`
	Projects     int      `json:"projects"`
	Pinned       bool     `json:"pinned"`
	Warnings     []string `json:"warnings,omitempty"`
}

func newExportCommand(stdout io.Writer, opts *options) *cobra.Command {
	var manifestPath string
	var pinned bool
	cmd := &cobra.Command{
		Use:   "export --manifest <path>",
		Short: "Export the namespace as a plain-text vcstool workspace manifest",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if manifestPath == "" {
				return appError{code: exitUsage, err: fmt.Errorf("--manifest is required")}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			result, err := exportManifest(cmd.Context(), cmd.ErrOrStderr(), store, opts, manifestPath, pinned, time.Now())
			if err != nil {
				return err
			}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Wrote %s: %d project(s), %d of them git repositories `vcs import` can rebuild\n",
					result.Manifest, result.Projects, result.Repositories)
				return err
			}, result)
		},
	}
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to write the workspace manifest to (required)")
	cmd.Flags().BoolVar(&pinned, "pinned", false, "record each repository's resolved HEAD SHA instead of its branch name (mirrors `vcs export --exact`)")
	return cmd
}

// exportManifest builds and writes the manifest. Warnings go to stderr as they
// are discovered AND into the returned result, so a human sees them live and a
// `--json` consumer sees them in the one stdout document.
func exportManifest(ctx context.Context, stderr io.Writer, store *state.Store, opts *options, path string, pinned bool, now time.Time) (exportResult, error) {
	summary, err := store.Summary(ctx)
	if err != nil {
		return exportResult{}, err
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return exportResult{}, err
	}
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		return exportResult{}, err
	}

	var warnings []string
	warn := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		warnings = append(warnings, msg)
		_, _ = fmt.Fprintf(stderr, "warning: %s\n", msg)
	}

	m := manifest.Manifest{
		Repositories: make(map[string]manifest.RepoEntry, len(projects)),
		DevStrap: manifest.Section{
			SchemaVersion: manifest.SchemaVersion,
			WorkspaceID:   summary.WorkspaceID,
			WorkspaceName: summary.WorkspaceName,
			ExportedAt:    now.UTC().Format(time.RFC3339),
			ExportedBy:    device.ID,
			Pinned:        pinned,
			Projects:      make(map[string]manifest.Project, len(projects)),
		},
	}
	for _, project := range projects {
		entry := manifest.Project{
			Type:                  project.Type,
			DefaultBranch:         project.DefaultBranch,
			LFSPolicy:             project.LFSPolicy,
			ForgeKind:             project.ForgeKind,
			MaterializationPolicy: project.MaterializationPolicy,
			EnvProfile:            hasEnvProfile(ctx, store, project.ID),
		}
		m.DevStrap.Projects[project.Path] = entry

		if project.Type != "git_repo" {
			continue
		}
		// A git_repo with no remote cannot appear under `repositories` — a
		// vcstool entry without a url is exactly what its parser skips — so it
		// stays a devstrap.projects-only row and the export says so out loud
		// rather than emitting a half entry that silently fails on import.
		if project.RemoteURL == "" {
			warn("%s is a git_repo with no remote URL; it is recorded under devstrap.projects but `vcs import` cannot rebuild it", project.Path)
			continue
		}
		version := project.DefaultBranch
		if pinned {
			// Mirrors `vcs export --exact`: a branch name is not a recovery
			// artifact. When HEAD cannot be resolved (the project was never
			// materialized on this device, or the checkout is gone) the version
			// is OMITTED rather than degraded back to a branch — vcstool then
			// clones the remote default, and the file never claims a pin it
			// does not have.
			version = ""
			sha, err := resolveExportHead(ctx, opts, project)
			if err != nil {
				warn("%s: cannot pin to a resolved SHA (%v); the manifest entry records no version", project.Path, err)
			} else {
				version = sha
			}
		}
		m.Repositories[project.Path] = manifest.RepoEntry{
			Type: manifest.VCSTypeGit,
			// Keep the URL usable but never export credentials: for http/https
			// the whole userinfo is dropped, for ssh the login name (typically
			// "git") is kept and any password dropped.
			URL:     redact.StripURLUserinfo(project.RemoteURL),
			Version: version,
		}
	}

	raw, err := manifest.Encode(m)
	if err != nil {
		return exportResult{}, err
	}
	if err := writeManifestFile(path, raw); err != nil {
		return exportResult{}, err
	}
	return exportResult{
		SchemaVersion: manifest.SchemaVersion,
		Manifest:      path,
		WorkspaceID:   summary.WorkspaceID,
		Repositories:  len(m.Repositories),
		Projects:      len(m.DevStrap.Projects),
		Pinned:        pinned,
		Warnings:      warnings,
	}, nil
}

// hasEnvProfile reports whether a project has an env profile bound. Only the
// FACT is exported; the values are age-encrypted to device recipients and a
// plaintext manifest never carries them. A read error is treated as "no
// profile" because a missing marker is a strictly safer wrong answer than a
// failed export.
func hasEnvProfile(ctx context.Context, store *state.Store, namespaceID string) bool {
	_, _, err := store.EnvProfileForProject(ctx, namespaceID)
	return err == nil
}

// resolveExportHead resolves the project checkout's current HEAD for --pinned.
func resolveExportHead(ctx context.Context, opts *options, project state.ProjectStatus) (string, error) {
	localPath := project.LocalPath
	if localPath == "" {
		localPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
	}
	r := gitRunner(opts)
	sha, err := r.RevParse(ctx, localPath, "HEAD")
	if err != nil {
		return "", err
	}
	// A local HEAD is not by itself a recovery artifact. `--pinned` is
	// documented as the flag to use WHEN THE MANIFEST IS A RECOVERY ARTIFACT,
	// which is exactly the case where an unpushed commit — or a HEAD sitting on
	// a topic branch — pins a SHA that exists nowhere after total local loss.
	// `vcs import` would then fail its checkout during the actual recovery.
	//
	// So the pin must be reachable from a remote-tracking ref. Anything else
	// degrades to the same omit-the-version path an unresolvable HEAD takes:
	// the file never claims a pin it does not have.
	reachable, err := r.RemoteTrackingContains(ctx, localPath, sha)
	if err != nil {
		return "", fmt.Errorf("check %s is on a remote: %w", sha, err)
	}
	if !reachable {
		return "", fmt.Errorf("HEAD (%s) is not reachable from any remote-tracking ref — it is unpushed, or HEAD is on a local-only branch", sha)
	}
	return sha, nil
}

// writeManifestFile writes the manifest atomically at 0600. The document holds
// no secrets by construction (see manifest's package doc), but a namespace
// inventory plus every remote URL is still not something to make world-readable
// by default; a user who wants to hand the file to someone else can widen it.
func writeManifestFile(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".devstrap-manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("create manifest temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod manifest temp file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close manifest temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("promote manifest: %w", err)
	}
	return nil
}
