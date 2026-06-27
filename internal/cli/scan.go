package cli

import (
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
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := opts.paths().Root
			if len(args) == 1 {
				root = args[0]
			}
			rootAbs, err := cleanAbsPath(root)
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			result, err := scan.Walk(cmd.Context(), rootAbs, scan.Options{IncludePlainFolders: true})
			if err != nil {
				return err
			}
			if adopt && !dryRun {
				store, err := opts.openState()
				if err != nil {
					return err
				}
				defer closeStore(store)
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
					event, err := dssync.CreateProjectEvent(cmd.Context(), store, dssync.EventProjectAdded, payload)
					if err != nil {
						return err
					}
					if _, err := store.UpsertProject(cmd.Context(), state.UpsertProjectParams{
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
					}); err != nil {
						return err
					}
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
					_, _ = fmt.Fprintf(stdout, "quarantined secret file %s -> %s\n", m.from, m.to)
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
				_, _ = fmt.Fprintf(stdout, "Adopted %d projects\n", len(result.Findings))
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
