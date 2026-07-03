package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/id"
	"github.com/Reederey87/DevStrap/internal/pairing"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/scan"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newInitCommand(stdout io.Writer, opts *options) *cobra.Command {
	var workspaceName string
	var dryRun bool
	var scanAdopt bool
	var join bool
	var workspaceID string
	var codeBlob string
	var fingerprint string

	cmd := &cobra.Command{
		Use:   "init [root]",
		Short: "Initialize a DevStrap workspace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var founderCode pairing.Code
			var founderFingerprint string
			// --fingerprint only means anything alongside --code (it confirms
			// the code's carried founder keys); silently ignoring it without
			// --code would let an operator believe something was verified.
			if strings.TrimSpace(fingerprint) != "" && strings.TrimSpace(codeBlob) == "" {
				return appError{code: exitUsage, err: fmt.Errorf("--fingerprint requires --code (it confirms the pairing code's founder keys)")}
			}
			if strings.TrimSpace(codeBlob) != "" {
				if workspaceID != "" {
					return appError{code: exitUsage, err: fmt.Errorf("--code is mutually exclusive with --workspace-id")}
				}
				decoded, err := pairing.Decode(codeBlob)
				if err != nil {
					return appError{code: exitInvalidConfig, err: err}
				}
				fp, err := devicekeys.Fingerprint(decoded.SigningPublicKey, decoded.AgeRecipient)
				if err != nil {
					return appError{code: exitInvalidConfig, err: fmt.Errorf("cannot compute founder fingerprint from pairing code: %w", err)}
				}
				if strings.TrimSpace(fingerprint) != "" && !devicekeys.FingerprintEqual(fingerprint, fp) {
					return appError{code: exitInvalidConfig, err: fmt.Errorf(
						"fingerprint mismatch for device %s: the value you passed does not match the pairing code's keys.\n  expected: %s\nCompare the full value out-of-band (e.g. over a call) before approving; no changes were made",
						decoded.DeviceID, fp)}
				}
				founderCode = decoded
				founderFingerprint = fp
				workspaceID = decoded.WorkspaceID
				join = true
			}
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
			pinnedFounder := false
			if founderCode.DeviceID != "" {
				// The fingerprint binds ONLY the two keys (it must match part
				// 1's `devices recipient --fingerprint`), so the other carried
				// fields are shown here for the operator to eyeball: a
				// tampered workspace/device id cannot forge trust (signatures
				// from the fingerprinted keys won't match a fake device id,
				// and a wrong workspace id is doctor-detected), but it CAN
				// break convergence visibly — surface it at decision time.
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"Pairing code carries: workspace %s, founder device %s (%q, %s/%s).\nCross-check the workspace id against `devstrap status` on the founding device.\n",
					founderCode.WorkspaceID, founderCode.DeviceID, founderCode.Name, founderCode.OS, founderCode.Arch)
				approved, err := confirmFounderFromPairingCode(cmd, founderCode, founderFingerprint, fingerprint)
				if err != nil {
					return err
				}
				trustState := "pending"
				if approved {
					trustState = "approved"
				}
				if err := store.UpsertDevice(cmd.Context(), state.Device{
					ID:               founderCode.DeviceID,
					Name:             founderCode.Name,
					OS:               founderCode.OS,
					Arch:             founderCode.Arch,
					PublicKey:        founderCode.AgeRecipient,
					SigningPublicKey: founderCode.SigningPublicKey,
					TrustState:       trustState,
				}); err != nil {
					return err
				}
				if approved {
					replayQuarantinedEvents(cmd.Context(), cmd.ErrOrStderr(), opts, store, founderCode.DeviceID)
					pinnedFounder = true
				} else {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: founder not pinned (no TTY for fingerprint confirmation). Verify the fingerprint out-of-band, then run: devstrap devices approve %s --fingerprint %s\n", founderCode.DeviceID, founderFingerprint)
				}
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
				var steps []string
				if founderCode.DeviceID != "" {
					steps = []string{
						"on the founding device: devstrap devices pairing-code           # copy the code + read its fingerprint aloud",
						"(this init already adopted the workspace id and pinned the founder when you passed --code)",
						"devstrap devices pairing-code                                   # now generate THIS device's code; paste it on the founder",
						"on the founding device: devstrap devices enroll --code '<code>' --approve --fingerprint <this device's fingerprint>",
						"set 'hub: r2://<bucket>' in ~/.devstrap/config.yaml, then devstrap sync",
					}
					if !pinnedFounder {
						steps[1] = "(this init already adopted the workspace id; pin the founder with the warning command before your first sync)"
					}
				} else {
					steps = []string{
						"pin the founder — and every other existing device — BEFORE your first sync: devstrap devices enroll <device-id> --name <n> --os <os> --arch <arch> --age-recipient <rec> --signing-public-key <sig> --approve --fingerprint <fp>  # closes the TOFU window (P4-SEC-04); events from devices you have not pinned yet quarantine and replay once approved",
						"devstrap devices recipient              # copy this device's age recipient",
						"devstrap devices recipient --signing    # and its signing key",
						"devstrap devices recipient --fingerprint  # and its fingerprint to compare out-of-band during approval",
						"on an approved device: devstrap devices enroll <id> --age-recipient <rec> --signing-public-key <sig> --approve --fingerprint <fp>  # compare the fingerprint against 'devstrap devices recipient --fingerprint' here before approving",
						"set 'hub: r2://<bucket>' in ~/.devstrap/config.yaml, then devstrap sync  # ingests the grant, then pushes your projects",
					}
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
	cmd.Flags().StringVar(&codeBlob, "code", "", "one-paste pairing code from the founding device; implies --join")
	cmd.Flags().StringVar(&fingerprint, "fingerprint", "", "with --code: the founding device fingerprint confirmed out-of-band; skips the interactive prompt")
	return cmd
}

func confirmFounderFromPairingCode(cmd *cobra.Command, code pairing.Code, expected, flagFP string) (bool, error) {
	if strings.TrimSpace(flagFP) != "" {
		return true, nil
	}
	stderr := cmd.ErrOrStderr()
	if f, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		_, _ = fmt.Fprintf(stderr, "Founding device %s fingerprint:\n  %s\n\n", code.DeviceID, expected)
		_, _ = fmt.Fprint(stderr, "Type 'yes' to approve: ")
		reader := bufio.NewReader(cmd.InOrStdin())
		line, _ := reader.ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			return false, appError{code: exitInvalidConfig, err: fmt.Errorf("approval of founding device %s refused: fingerprint not confirmed", code.DeviceID)}
		}
		return true, nil
	}
	return false, nil
}

func ensureLocalDeviceIdentity(ctx context.Context, paths config.Paths, store *state.Store, device state.Device) error {
	if err := recordKeyCustodyAtInit(ctx, paths, store, device); err != nil {
		return err
	}
	keyStore, err := resolveKeyStore(ctx, paths, store)
	if err != nil {
		return err
	}
	if device.PublicKey != "" {
		identity, err := keyStore.Read(ctx, device.ID)
		if err != nil {
			return fmt.Errorf("read local device identity: %w", err)
		}
		if identity.Recipient != device.PublicKey {
			return fmt.Errorf("local device identity does not match stored public key")
		}
	} else {
		identity, _, err := keyStore.Ensure(ctx, device.ID, device.PublicKey)
		if err != nil {
			return fmt.Errorf("ensure local device identity: %w", err)
		}
		if err := store.SetDevicePublicKey(ctx, device.ID, identity.Recipient); err != nil {
			return err
		}
	}
	signingIdentity, _, err := keyStore.EnsureSigning(ctx, device.ID, device.SigningPublicKey)
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

// resolveKeyStore builds the device key custody store stamped with this
// machine's recorded custody backend (P6-XP-04). It is side-effect-free: it
// reads the recorded decision and applies the DEVSTRAP_NO_KEYCHAIN override, but
// never probes or records — recording is done exactly once, at init, by
// recordKeyCustodyAtInit. An unrecorded (pre-P6-XP-04) store resolves to
// CustodyUnset, i.e. legacy hybrid behavior, which the mint guard still
// protects. Every path that mints, reads, or reports device/workspace keys goes
// through this so custody is honored consistently process-wide.
func resolveKeyStore(ctx context.Context, paths config.Paths, store *state.Store) (devicekeys.HybridStore, error) {
	base := devicekeys.NewHybridStore(paths.KeyDir(), platform.Detect().Keychain)
	custody, err := store.KeyCustody(ctx)
	if err != nil {
		return devicekeys.HybridStore{}, err
	}
	return base.WithCustody(state.EffectiveKeyCustody(custody)), nil
}

// recordKeyCustodyAtInit records the key-custody decision exactly once, at init,
// and only from safe evidence (P6-XP-04). A recorded decision is never
// rewritten. When none is recorded yet, it records:
//
//   - file, if DEVSTRAP_NO_KEYCHAIN=1 (explicit operator choice); else
//   - keychain, if the probe positively finds the keychain reachable (safe
//     regardless of whether secrets already exist); else
//   - file, ONLY for a genuine first init — a brand-new device with no
//     already-published keys, so there are no keychain secrets to strand.
//
// Crucially, it does NOT record file from a merely-unreachable probe on an
// already-initialized store (a pre-00016 store whose secrets live only in the
// keychain, first run headless after upgrade): that would permanently route
// later desktop runs away from the keychain where the real secrets are. Such a
// store stays CustodyUnset (legacy hybrid + the mint guard) until the keychain
// is seen reachable or the operator sets DEVSTRAP_NO_KEYCHAIN=1.
func recordKeyCustodyAtInit(ctx context.Context, paths config.Paths, store *state.Store, device state.Device) error {
	recorded, err := store.KeyCustody(ctx)
	if err != nil {
		return err
	}
	if recorded != devicekeys.CustodyUnset {
		return nil // already decided; honored, never rewritten
	}
	if os.Getenv(platform.NoKeychainEnv) == "1" {
		return store.RecordKeyCustody(ctx, devicekeys.CustodyFile)
	}
	base := devicekeys.NewHybridStore(paths.KeyDir(), platform.Detect().Keychain)
	switch base.Probe(ctx) {
	case devicekeys.CustodyKeychain:
		return store.RecordKeyCustody(ctx, devicekeys.CustodyKeychain)
	case devicekeys.CustodyFile:
		// Keychain unreachable: only safe to record file for a genuine first
		// init (no already-published keys to strand).
		if device.PublicKey == "" && device.SigningPublicKey == "" {
			return store.RecordKeyCustody(ctx, devicekeys.CustodyFile)
		}
		return nil
	default:
		return nil
	}
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
