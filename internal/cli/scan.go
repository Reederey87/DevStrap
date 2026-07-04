package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/scan"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

func newScanCommand(stdout io.Writer, opts *options) *cobra.Command {
	var adopt bool
	var dryRun bool
	var quarantine bool
	cmd := &cobra.Command{
		Use:   "scan [root]",
		Short: "Scan a workspace root for projects",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := opts.paths().Root
			if len(args) == 1 {
				root = args[0]
			}
			rootAbs, err := cleanAbsPath(root)
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			if adopt {
				wsRoot, err := cleanAbsPath(opts.paths().Root)
				if err != nil {
					return appError{code: exitInvalidConfig, err: err}
				}
				if !sameResolvedDir(rootAbs, wsRoot) {
					return appError{code: exitUsage, err: fmt.Errorf("--adopt only adopts from the workspace root %s (scanned %s); scan without --adopt to inspect, or use 'devstrap add' for a single repo", wsRoot, rootAbs)}
				}
				// Adopt under the canonical root spelling so stored
				// local_paths never carry a symlink-alias prefix.
				rootAbs = wsRoot
			}
			result, err := scan.Walk(cmd.Context(), rootAbs, scan.Options{IncludePlainFolders: true})
			if err != nil {
				return err
			}
			if adopt && !dryRun {
				store, err := opts.openState(cmd.Context())
				if err != nil {
					return err
				}
				defer closeStore(store)
				if _, err := adoptFindings(cmd.Context(), store, rootAbs, result); err != nil {
					return err
				}
				for _, warning := range result.Warnings {
					if isConflictWarning(warning) {
						if err := store.InsertConflict(cmd.Context(), "", "scan.warning", fmt.Sprintf("%q", warning)); err != nil {
							return err
						}
					}
				}
			}
			if quarantine && !dryRun {
				moved, err := quarantineSecrets(opts.paths().Home, rootAbs, result.Secrets)
				if err != nil {
					return err
				}
				for _, m := range moved {
					// CLI-02: route diagnostic/progress output to stderr so it
					// never corrupts the JSON stdout stream.
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "quarantined secret file %s -> %s\n", m.from, m.to)
				}
			}
			if opts.v.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}
			for _, finding := range result.Findings {
				_, _ = fmt.Fprintf(stdout, "%s\t%s", finding.Path, finding.Type)
				if finding.RemoteKey != "" {
					_, _ = fmt.Fprintf(stdout, "\t%s", finding.RemoteKey)
				}
				_, _ = fmt.Fprintln(stdout)
				for _, warning := range finding.Warnings {
					_, _ = fmt.Fprintf(stdout, "  warning: %s\n", warning)
				}
			}
			for _, duplicate := range result.Duplicates {
				_, _ = fmt.Fprintf(stdout, "duplicate remote %s: %v; recommended %s\n", duplicate.RemoteKey, duplicate.Paths, duplicate.RecommendedPath)
			}
			for _, warning := range result.Warnings {
				_, _ = fmt.Fprintf(stdout, "warning: %s\n", warning)
			}
			if adopt && !dryRun {
				opts.progressf(stdout, "Adopted %d projects\n", len(result.Findings))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&adopt, "adopt", false, "write discovered projects to local state")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show planned adoption without writing")
	cmd.Flags().BoolVar(&quarantine, "quarantine", false, "move secret-looking files out of the workspace into the DevStrap quarantine")
	return cmd
}

func isConflictWarning(warning string) bool {
	return strings.Contains(warning, "symlink escape") || strings.Contains(warning, "case-only path conflict")
}

// adoptFindings writes discovered scan findings to local state as project.added
// events + namespace entries, returning the number adopted (PROD-03). Shared by
// `devstrap scan --adopt` and `devstrap init --scan` so the first-run epiphany
// (the user's existing tree appearing) is one command.
func adoptFindings(ctx context.Context, store *state.Store, rootAbs string, result scan.Result) (int, error) {
	adopted := 0
	for _, finding := range result.Findings {
		localPath := filepath.Join(rootAbs, filepath.FromSlash(finding.Path))
		materialization := "available"
		dirty := "unknown"
		if finding.Type == scan.TypeGitRepo {
			dirty = "clean"
		}
		payload := dssync.ProjectPayload{
			Path:          finding.Path,
			Type:          string(finding.Type),
			RemoteURL:     finding.RemoteURL,
			RemoteKey:     finding.RemoteKey,
			DefaultBranch: finding.DefaultBranch,
		}
		if err := store.WithTx(ctx, func(tx *state.Tx) error {
			event, err := dssync.CreateProjectEventTx(ctx, store, tx, dssync.EventProjectAdded, payload)
			if err != nil {
				return err
			}
			_, err = tx.UpsertProject(ctx, state.UpsertProjectParams{
				Path:                  finding.Path,
				Type:                  string(finding.Type),
				RemoteURL:             finding.RemoteURL,
				RemoteKey:             finding.RemoteKey,
				DefaultBranch:         finding.DefaultBranch,
				MaterializationPolicy: "lazy",
				LocalPath:             localPath,
				MaterializationState:  materialization,
				DirtyState:            dirty,
				SourceEventHLC:        event.HLC,
				SourceEventDeviceID:   event.DeviceID,
				SourceEventID:         event.ID,
			})
			return err
		}); err != nil {
			return adopted, err
		}
		adopted++
	}
	return adopted, nil
}

// adoptNewFindings adopts only findings that are not already present in local
// state (P6-XP-03), so a repeated scan — e.g. run-loop's per-tick scan — never
// re-emits a project.added event for an already-known project. It is the
// idempotent variant of adoptFindings used by the loop; the one-shot
// `devstrap scan --adopt` keeps calling adoptFindings directly so its
// re-stamping semantics are unchanged. Warning-class items (secret-looking
// files, escaping symlinks) never reach result.Findings, so they can never be
// adopted here; the loop routes them to stderr separately.
func adoptNewFindings(ctx context.Context, store *state.Store, rootAbs string, result scan.Result) (int, error) {
	novel := scan.Result{Findings: make([]scan.Finding, 0, len(result.Findings))}
	for _, finding := range result.Findings {
		known, err := findingAlreadyAdopted(ctx, store, finding)
		if err != nil {
			return 0, err
		}
		if known {
			continue
		}
		novel.Findings = append(novel.Findings, finding)
	}
	if len(novel.Findings) == 0 {
		return 0, nil
	}
	return adoptFindings(ctx, store, rootAbs, novel)
}

// findingAlreadyAdopted reports whether a scan finding already exists in local
// state as an active project of the same type (and, for git repos, the same
// remote). A ProjectByPath error means the path is not tracked yet — the whole
// codebase treats that lookup as presence/absence (see internal/sync/events.go)
// — so a genuinely new or changed project falls through to adoption rather than
// being silently dropped. A type or remote change is treated as new so the
// namespace converges on the current on-disk reality.
func findingAlreadyAdopted(ctx context.Context, store *state.Store, finding scan.Finding) (bool, error) {
	existing, err := store.ProjectByPath(ctx, finding.Path)
	if err != nil {
		return false, nil
	}
	if existing.Type != string(finding.Type) {
		return false, nil
	}
	if finding.Type == scan.TypeGitRepo && existing.RemoteKey != finding.RemoteKey {
		return false, nil
	}
	return true, nil
}

type quarantinedFile struct {
	from string
	to   string
}

// quarantineSecrets moves secret-looking files out of the managed workspace and
// into a dated quarantine directory under the DevStrap home (0700), so
// discovered credentials stop sitting in the scanned tree. Files that no longer
// exist are skipped silently.
func quarantineSecrets(home, root string, secrets []string) ([]quarantinedFile, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	day := time.Now().UTC().Format("20060102")
	base := filepath.Join(home, "quarantine", day)
	var moved []quarantinedFile
	for _, rel := range secrets {
		src := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Lstat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return moved, fmt.Errorf("stat secret file %s: %w", rel, err)
		}
		dst := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return moved, fmt.Errorf("create quarantine dir: %w", err)
		}
		if err := os.Rename(src, dst); err != nil {
			// Cross-device or other rename failure: surface it rather than
			// leaving the secret half-moved.
			return moved, fmt.Errorf("quarantine %s: %w", rel, err)
		}
		if err := os.Chmod(dst, 0o600); err != nil {
			return moved, fmt.Errorf("chmod quarantined %s: %w", rel, err)
		}
		moved = append(moved, quarantinedFile{from: rel, to: dst})
	}
	return moved, nil
}

// sameResolvedDir reports whether two cleaned absolute paths name the same
// directory, resolving symlinks so an alias of the workspace root (e.g.
// ~/Code -> /Volumes/Dev/Code) is accepted (P6-CLI-02 review). Comparison
// stays byte-exact after resolution: no case-folding, so on a case-sensitive
// filesystem two genuinely different directories can never alias, and on a
// case-insensitive one a different-case spelling of a non-symlinked root is
// still refused (over-refusal is the safe direction). If either path cannot
// be resolved (e.g. it does not exist), the lexical comparison decides.
func sameResolvedDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	return ra == rb
}
