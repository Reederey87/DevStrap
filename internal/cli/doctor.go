package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/spf13/cobra"
)

func newDoctorCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check local prerequisites",
		RunE: func(cmd *cobra.Command, args []string) error {
			paths := opts.paths()
			_, _ = fmt.Fprintf(stdout, "DevStrap home: %s\n", paths.Home)
			_, _ = fmt.Fprintf(stdout, "Managed root: %s\n", paths.Root)
			if info, ok := debug.ReadBuildInfo(); ok {
				_, _ = fmt.Fprintf(stdout, "Go module: %s\n", info.GoVersion)
			}
			for _, tool := range []string{"git", "gh", "go"} {
				path, err := exec.LookPath(tool)
				if err != nil {
					_, _ = fmt.Fprintf(stdout, "%s: missing\n", tool)
					continue
				}
				_, _ = fmt.Fprintf(stdout, "%s: %s\n", tool, path)
			}
			if stat, err := os.Stat(paths.Home); err == nil {
				_, _ = fmt.Fprintf(stdout, "state home permissions: %s\n", stat.Mode().Perm())
			} else if os.IsNotExist(err) {
				_, _ = fmt.Fprintln(stdout, "state home: missing")
			} else {
				_, _ = fmt.Fprintf(stdout, "state home: error: %v\n", err)
			}
			if _, err := os.Stat(paths.StateDB()); err == nil {
				store, err := opts.openState(cmd.Context())
				if err != nil {
					return err
				}
				defer closeStore(store)
				version, err := store.Version()
				if err != nil {
					return err
				}
				check, err := store.QuickCheck(cmd.Context())
				if err != nil {
					return err
				}
				fkCheck, err := store.ForeignKeyCheck(cmd.Context())
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(stdout, "schema version: %d\nsqlite quick_check: %s\nsqlite foreign_key_check: %s\n", version, check, fkCheck)
				if rotate, err := store.CountSecretBindingsNeedingRotation(cmd.Context()); err == nil {
					if rotate > 0 {
						_, _ = fmt.Fprintf(stdout, "secrets needing rotation: %d (rotate at source after a device revoke)\n", rotate)
					} else {
						_, _ = fmt.Fprintln(stdout, "secrets needing rotation: 0")
					}
				}
				device, err := store.CurrentDevice(cmd.Context())
				if err == nil {
					keyStore := devicekeys.NewHybridStore(paths.KeyDir(), platform.Detect().Keychain)
					_, _ = fmt.Fprintf(stdout, "device key: %s\n", deviceKeyStatus(cmd.Context(), keyStore, device.ID, device.PublicKey))
					_, _ = fmt.Fprintf(stdout, "device signing key: %s\n", deviceSigningKeyStatus(cmd.Context(), keyStore, device.ID, device.SigningPublicKey))
				}
			} else if os.IsNotExist(err) {
				_, _ = fmt.Fprintln(stdout, "state database: missing")
			} else {
				_, _ = fmt.Fprintf(stdout, "state database: error: %v\n", err)
			}
			doctorReportLocks(stdout, paths.Home)
			return nil
		},
	}
}

func doctorReportLocks(stdout io.Writer, home string) {
	entries, err := os.ReadDir(repoLockDir(home))
	if err != nil {
		if os.IsNotExist(err) {
			_, _ = fmt.Fprintln(stdout, "repo locks: none held")
		} else {
			_, _ = fmt.Fprintf(stdout, "repo locks: error: %v\n", err)
		}
		return
	}
	held := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".lock") {
			continue
		}
		projectID := strings.TrimSuffix(name, ".lock")
		info, exists, stale, err := readRepoLock(home, projectID)
		if err != nil {
			_, _ = fmt.Fprintf(stdout, "repo lock %s: error: %v\n", projectID, err)
			continue
		}
		if !exists {
			continue
		}
		held++
		state := "live"
		if stale {
			state = "stale (run `devstrap worktree unlock <path>` to clear)"
		}
		_, _ = fmt.Fprintf(stdout, "repo lock %s: %s (pid %d on %s, acquired %s)\n", projectID, state, info.PID, info.Hostname, info.AcquiredAt)
	}
	if held == 0 {
		_, _ = fmt.Fprintln(stdout, "repo locks: none held")
	}
}

func deviceSigningKeyStatus(ctx context.Context, keyStore devicekeys.HybridStore, deviceID, publicKey string) string {
	if publicKey == "" {
		return "missing public key"
	}
	identity, err := keyStore.ReadSigning(ctx, deviceID)
	if err != nil {
		return "missing private identity"
	}
	if identity.Public != publicKey {
		return "public/private mismatch"
	}
	return "ok"
}

func deviceKeyStatus(ctx context.Context, keyStore devicekeys.HybridStore, deviceID, publicKey string) string {
	if publicKey == "" {
		return "missing public key"
	}
	identity, err := keyStore.Read(ctx, deviceID)
	if err != nil {
		return "missing private identity"
	}
	if identity.Recipient != publicKey {
		return "public/private mismatch"
	}
	return "ok"
}
