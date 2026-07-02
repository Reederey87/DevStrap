package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/scan"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

func newInitCommand(stdout io.Writer, opts *options) *cobra.Command {
	var workspaceName string
	var dryRun bool
	var scanAdopt bool
	var join bool

	cmd := &cobra.Command{
		Use:   "init [root]",
		Short: "Initialize a DevStrap workspace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths := opts.paths()
			if len(args) == 1 {
				if cmd.Root().PersistentFlags().Changed("root") {
					return appError{code: exitInvalidConfig, err: errors.New("use either positional root or --root, not both")}
				}
				paths.Root = args[0]
			}
			if workspaceName == "" {
				workspaceName = "default"
			}
			cleanRoot, err := cleanAbsPath(paths.Root)
			if err != nil {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("resolve root: %w", err)}
			}
			cleanHome, err := cleanAbsPath(paths.Home)
			if err != nil {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("resolve home: %w", err)}
			}
			paths = config.Paths{Home: cleanHome, Root: cleanRoot}

			if dryRun {
				_, err := fmt.Fprintf(stdout, "Would create %s, %s, %s, and %s\n", paths.Root, paths.Home, paths.LogDir(), paths.StateDB())
				return err
			}

			//nolint:gosec // The managed code root is user-facing project storage, not a secret state directory.
			if err := os.MkdirAll(paths.Root, 0o755); err != nil {
				return fmt.Errorf("create root: %w", err)
			}
			if err := os.MkdirAll(paths.Home, 0o700); err != nil {
				return fmt.Errorf("create state home: %w", err)
			}
			if err := os.MkdirAll(paths.LogDir(), 0o700); err != nil {
				return fmt.Errorf("create log dir: %w", err)
			}
			role := "founder"
			if join {
				role = "joiner"
			}
			if err := writeDefaultConfig(paths, workspaceName, role); err != nil {
				return err
			}

			store, err := state.Open(cmd.Context(), paths.StateDB())
			if err != nil {
				return err
			}
			defer closeStore(store)

			if err := store.Migrate(); err != nil {
				return err
			}
			if err := store.EnsureWorkspace(cmd.Context(), workspaceName, paths.Root); err != nil {
				return err
			}
			device, err := store.EnsureDevice(cmd.Context(), "")
			if err != nil {
				return err
			}
			if err := ensureLocalDeviceIdentity(cmd.Context(), paths, store, device); err != nil {
				return err
			}
			// P6-SEC-02: init no longer mints the WCK epoch-1 key. Founding is
			// deferred to the first `devstrap sync` and happens only when the
			// hub is confirmed empty (see runSyncCycle's founder/join gate), so
			// a device JOINING an existing workspace never self-mints a key
			// nobody else holds and never seals its pre-approval events under a
			// never-granted WCK (the SEC-02 data-loss). A joining device
			// receives the fleet WCK via `devices approve` (GrantAllEpochs) on
			// an existing device; a founding device mints epoch 1 on its first
			// sync to the empty hub. A FOUNDER approving another device before
			// ever syncing still founds defensively; a joiner never does
			// (grantWorkspaceKeyToApprovedDevice is founder-gated).

			// PROD-03: an optional --scan adopts existing repos in the root on
			// the very first command, delivering the "my tree just appeared"
			// epiphany without a multi-screen wizard. Non-interactive: it runs
			// the existing scan/adopt path inline.
			adopted := 0
			if scanAdopt {
				result, err := scan.Walk(cmd.Context(), paths.Root, scan.Options{IncludePlainFolders: true})
				if err != nil {
					return fmt.Errorf("scan on init: %w", err)
				}
				adopted, err = adoptFindings(cmd.Context(), store, paths.Root, result)
				if err != nil {
					return fmt.Errorf("adopt on init: %w", err)
				}
			}

			if _, err := fmt.Fprintf(stdout, "Initialized DevStrap workspace %q at %s\n", workspaceName, paths.Root); err != nil {
				return err
			}
			if scanAdopt {
				if _, err := fmt.Fprintf(stdout, "Adopted %d existing project(s).\n", adopted); err != nil {
					return err
				}
			}
			// PROD-03: always print a short next-steps hint (clig.dev: suggest
			// the next command and surface state after every action). The hint
			// is role-aware (P6-SEC-02): a joining device must be approved from
			// an existing device before its events can sync.
			if join {
				if _, err := fmt.Fprintf(stdout, "Joining an existing workspace. Next:\n"+
					"  1. devstrap devices recipient            # copy this device's age recipient\n"+
					"  2. devstrap devices recipient --signing  # and its signing key\n"+
					"  3. on an approved device: devstrap devices enroll <id> --age-recipient <rec> --signing-public-key <sig> --approve\n"+
					"  4. devstrap sync --hub-file <path>        # ingests the grant, then pushes your projects\n"); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintf(stdout, "Next: devstrap status • devstrap scan --adopt • devstrap sync --hub-file <path>\n"); err != nil {
					return err
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&workspaceName, "workspace-name", "", "workspace name")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show planned changes without writing")
	cmd.Flags().BoolVar(&scanAdopt, "scan", false, "scan the root and adopt existing repos on init")
	cmd.Flags().BoolVar(&join, "join", false, "join an existing workspace: do not found a new one; wait to be approved from an existing device (P6-SEC-02)")
	return cmd
}

func ensureLocalDeviceIdentity(ctx context.Context, paths config.Paths, store *state.Store, device state.Device) error {
	keyStore := devicekeys.NewHybridStore(paths.KeyDir(), platform.Detect().Keychain)
	if device.PublicKey != "" {
		identity, err := keyStore.Read(ctx, device.ID)
		if err != nil {
			return fmt.Errorf("read local device identity: %w", err)
		}
		if identity.Recipient != device.PublicKey {
			return fmt.Errorf("local device identity does not match stored public key")
		}
	} else {
		identity, _, err := keyStore.Ensure(ctx, device.ID)
		if err != nil {
			return fmt.Errorf("ensure local device identity: %w", err)
		}
		if err := store.SetDevicePublicKey(ctx, device.ID, identity.Recipient); err != nil {
			return err
		}
	}
	signingIdentity, _, err := keyStore.EnsureSigning(ctx, device.ID)
	if err != nil {
		return fmt.Errorf("ensure local device signing identity: %w", err)
	}
	if device.SigningPublicKey != "" && signingIdentity.Public != device.SigningPublicKey {
		return fmt.Errorf("local device signing identity does not match stored signing public key")
	}
	if device.SigningPublicKey == "" {
		if err := store.SetDeviceSigningPublicKey(ctx, device.ID, signingIdentity.Public); err != nil {
			return err
		}
	}
	return nil
}

func cleanAbsPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path must not be empty")
	}
	return filepath.Abs(filepath.Clean(path))
}

func writeDefaultConfig(paths config.Paths, workspaceName, role string) error {
	path := filepath.Join(paths.Home, "config.yaml")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat config: %w", err)
	}
	// role (P6-SEC-02): "founder" (default) or "joiner". A joiner never founds
	// a workspace on first sync; it waits to be granted the fleet WCK.
	content := fmt.Sprintf("home: %q\nroot: %q\nworkspace_name: %q\nrole: %q\n", paths.Home, paths.Root, workspaceName, role)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
