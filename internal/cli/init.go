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
	"github.com/Reederey87/DevStrap/internal/id"
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
	var workspaceID string

	cmd := &cobra.Command{
		Use:   "init [root]",
		Short: "Initialize a DevStrap workspace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// P4-SEC-07 pairing: validate the supplied workspace id shape
			// before touching the filesystem so a bad paste never creates a
			// half-initialized state home. Supplying an id IS joining.
			if workspaceID != "" {
				if !id.Valid("ws", workspaceID) {
					return appError{code: exitInvalidConfig, err: fmt.Errorf("invalid --workspace-id %q: want ws_ followed by 32 lowercase hex characters (copy the Workspace ID from `devstrap status` on the founding device)", workspaceID)}
				}
				join = true
			}
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
				if workspaceID != "" {
					if _, err := fmt.Fprintf(stdout, "Would adopt workspace id %s (join)\n", workspaceID); err != nil {
						return err
					}
				}
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
			wroteConfig, err := writeDefaultConfig(paths, workspaceName, role)
			if err != nil {
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
			if workspaceID != "" {
				err = store.EnsureWorkspaceWithID(cmd.Context(), workspaceID, workspaceName, paths.Root)
			} else {
				err = store.EnsureWorkspace(cmd.Context(), workspaceName, paths.Root)
			}
			if err != nil {
				if errors.Is(err, state.ErrWorkspaceIDMismatch) {
					// No post-hoc rewrite: the singleton workspace row cascades
					// into every workspace-scoped table, so adopting a new id
					// means starting from a clean state home (P4-SEC-07).
					return appError{code: exitInvalidConfig, err: fmt.Errorf("%w; this store was initialized under a different workspace id — remove %s and re-run devstrap init --join --workspace-id %s", err, paths.Home, workspaceID)}
				}
				return err
			}
			// Printed only after the workspace ensure succeeded so a refused
			// re-init reports the id mismatch alone, without role noise.
			existingRole := opts.v.GetString("role")
			if existingRole == "" {
				existingRole = "founder"
			}
			if !wroteConfig && join && existingRole != "joiner" {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s already exists and was not modified (role: %q stays); edit it by hand to change the role\n", filepath.Join(paths.Home, "config.yaml"), existingRole)
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
				// P4-SEC-07 pairing: r2/s3 hubs key everything under
				// workspaces/<workspace_id>/, so a joiner that mints its own id
				// reads an empty prefix and never sees the founder. Flat file
				// hubs are unaffected. The hint (and warning, when the id is
				// missing) walks the founder-status → copy-id → re-init path.
				if workspaceID == "" {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: r2/s3 hubs key events by workspace id; without --workspace-id this device minted its own id and will not see the founder's hub data (file hubs are unaffected)\n")
				}
				hint := "Joining an existing workspace. Next:\n"
				steps := []string{
					"pin the founder — and every other existing device — BEFORE your first sync: devstrap devices enroll <device-id> --name <n> --os <os> --arch <arch> --age-recipient <rec> --signing-public-key <sig> --approve --fingerprint <fp>  # closes the TOFU window (P4-SEC-04); events from devices you have not pinned yet quarantine and replay once approved",
					"devstrap devices recipient              # copy this device's age recipient",
					"devstrap devices recipient --signing    # and its signing key",
					"devstrap devices recipient --fingerprint  # and its fingerprint to compare out-of-band during approval",
					"on an approved device: devstrap devices enroll <id> --age-recipient <rec> --signing-public-key <sig> --approve --fingerprint <fp>  # compare the fingerprint against 'devstrap devices recipient --fingerprint' here before approving",
					"set 'hub: r2://<bucket>' in ~/.devstrap/config.yaml, then devstrap sync  # ingests the grant, then pushes your projects",
				}
				if workspaceID == "" {
					// This init already minted a local id, so a plain re-run
					// with --workspace-id would hit the mismatch refusal — the
					// recovery hint must include removing the state home.
					steps = append([]string{fmt.Sprintf("on the founding device: devstrap status  # copy its Workspace ID, then (r2/s3 hubs only — file hubs need no id) rm -r %s here and re-run: devstrap init --join --workspace-id <id>", paths.Home)}, steps...)
				} else {
					if _, err := fmt.Fprintf(stdout, "Adopted workspace id %s.\n", workspaceID); err != nil {
						return err
					}
				}
				for i, step := range steps {
					hint += fmt.Sprintf("  %d. %s\n", i+1, step)
				}
				if _, err := fmt.Fprint(stdout, hint); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintf(stdout, "Next: devstrap status • devstrap scan --adopt • set 'hub: r2://<bucket>' in ~/.devstrap/config.yaml then devstrap sync\n"); err != nil {
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
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "adopt the founding device's workspace id (copy it from `devstrap status` there); implies --join (P4-SEC-07)")
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

// writeDefaultConfig writes config.yaml if missing and reports whether it
// wrote (false = a pre-existing config was left untouched).
func writeDefaultConfig(paths config.Paths, workspaceName, role string) (bool, error) {
	path := filepath.Join(paths.Home, "config.yaml")
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat config: %w", err)
	}
	// role (P6-SEC-02): "founder" (default) or "joiner". A joiner never founds
	// a workspace on first sync; it waits to be granted the fleet WCK.
	content := fmt.Sprintf("home: %q\nroot: %q\nworkspace_name: %q\nrole: %q\n", paths.Home, paths.Root, workspaceName, role)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return false, fmt.Errorf("write config: %w", err)
	}
	return true, nil
}
