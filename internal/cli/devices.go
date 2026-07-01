package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

func newDevicesCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "Manage device trust state",
	}
	cmd.AddCommand(newDevicesListCommand(stdout, opts))
	cmd.AddCommand(newDeviceEnrollCommand(stdout, opts))
	cmd.AddCommand(newDeviceTrustCommand(stdout, opts, "approve", "approved"))
	cmd.AddCommand(newDeviceTrustCommand(stdout, opts, "revoke", "revoked"))
	cmd.AddCommand(newDeviceTrustCommand(stdout, opts, "lost", "lost"))
	cmd.AddCommand(newDeviceRenameCommand(stdout, opts))
	cmd.AddCommand(newDeviceRecipientCommand(stdout, opts))
	return cmd
}

func newDeviceEnrollCommand(stdout io.Writer, opts *options) *cobra.Command {
	var name string
	var osName string
	var arch string
	var ageRecipient string
	var signingPublicKey string
	var approve bool
	cmd := &cobra.Command{
		Use:   "enroll <device-id>",
		Short: "Enroll a remote device record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" || osName == "" || arch == "" || ageRecipient == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--name, --os, --arch, and --age-recipient are required")}
			}
			// SECU-05: require a signing public key when --approve is set so
			// an approved device can never silently combine with the fail-open
			// event verification path (SECU-03).
			if approve && strings.TrimSpace(signingPublicKey) == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--approve requires --signing-public-key so the device's events can be signature-verified")}
			}
			trustState := "pending"
			if approve {
				trustState = "approved"
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			device := state.Device{
				ID:               args[0],
				Name:             name,
				OS:               osName,
				Arch:             arch,
				PublicKey:        strings.TrimSpace(ageRecipient),
				SigningPublicKey: strings.TrimSpace(signingPublicKey),
				TrustState:       trustState,
			}
			if err := store.UpsertDevice(cmd.Context(), device); err != nil {
				return err
			}
			// P4-SEC-07: when --approve is set, grant every held WCK epoch to
			// the newly-approved device so it can decrypt the namespace-map
			// history on its first pull.
			if approve {
				grantWorkspaceKeyToApprovedDevice(cmd.Context(), cmd.ErrOrStderr(), opts, store, args[0])
			}
			_, err = fmt.Fprintf(stdout, "Device %s enrolled as %s\n", args[0], trustState)
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "device display name")
	cmd.Flags().StringVar(&osName, "os", "", "device operating system")
	cmd.Flags().StringVar(&arch, "arch", "", "device architecture")
	cmd.Flags().StringVar(&ageRecipient, "age-recipient", "", "device age recipient public key")
	cmd.Flags().StringVar(&signingPublicKey, "signing-public-key", "", "device Ed25519 signing public key")
	cmd.Flags().BoolVar(&approve, "approve", false, "mark the enrolled device approved immediately")
	return cmd
}

func newDevicesListCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List devices",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			devices, err := store.ListDevices(cmd.Context())
			if err != nil {
				return err
			}
			if opts.v.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(devices)
			}
			for _, device := range devices {
				_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s/%s\n", device.ID, device.TrustState, device.Name, device.OS, device.Arch)
			}
			return nil
		},
	}
}

func newDeviceTrustCommand(stdout io.Writer, opts *options, use, trustState string) *cobra.Command {
	var hubFile string
	cmd := &cobra.Command{
		Use:   use + " <device-id>",
		Short: "Mark a device as " + trustState,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			// P5-CLI-05: progress/warnings go to stderr; stdout stays the result
			// stream.
			stderr := cmd.ErrOrStderr()
			if err := store.SetDeviceTrustState(cmd.Context(), args[0], trustState); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(stdout, "Device %s marked %s\n", args[0], trustState); err != nil {
				return err
			}
			// P4-SEC-07: approving a device grants every held WCK epoch to it
			// so it can decrypt the full namespace-map history on its first
			// pull. The grant events are local-origin; the next `devstrap sync`
			// publishes them to the hub.
			if trustState == "approved" {
				grantWorkspaceKeyToApprovedDevice(cmd.Context(), stderr, opts, store, args[0])
			}
			// Revoking or losing a device means it could decrypt any env bundle
			// it received; flag those values for source rotation (rewrapping
			// recipients does not revoke historical access).
			if trustState == "revoked" || trustState == "lost" {
				// P4-SEC-07: rotate the WCK epoch so go-forward events encrypt
				// under a key the revoked device does not hold. The revoked
				// device is already excluded from ApprovedRecipients (its
				// trust_state was just changed), so Rotate grants the new epoch
				// only to remaining approved devices. Skip silently if no epoch
				// was ever bootstrapped (pre-envelope workspace).
				rotateWorkspaceKeyOnRevoke(cmd.Context(), stderr, opts, store)
				flagged, err := store.MarkEncryptedBindingsNeedingRotation(cmd.Context())
				if err != nil {
					return err
				}
				if flagged > 0 {
					_, _ = fmt.Fprintf(stderr, "warning: %d secret value(s) must be rotated at their source (run 'devstrap env rotate'); rewrapping recipients does not revoke %s's historical access\n", flagged, args[0])
				}
				// P5-SEC-01/SEC-04/HUB-04: re-encrypt affected blobs to the
				// reduced recipient set. Env blobs are rewrapped locally only;
				// draft blobs emit a superseding event and (with a hub) delete
				// the old ciphertext only after the event + new blob are durably
				// pushed. age has no native revocation, so historical access is
				// irreversible — hence the mandatory rotation flag above.
				var hub dssync.Hub
				if hubFile != "" || strings.TrimSpace(opts.v.GetString("hub")) != "" {
					h, _, herr := hubFromOptions(cmd.Context(), opts, store, hubFile)
					if herr != nil {
						return appError{code: exitInvalidConfig, err: herr}
					}
					hub = h
				}
				rewrapped, err := rewrapBlobsOnRevoke(cmd.Context(), store, opts, hub)
				if err != nil {
					_, _ = fmt.Fprintf(stderr, "warning: blob re-encryption incomplete: %v\n", err)
				} else if rewrapped > 0 {
					_, _ = fmt.Fprintf(stderr, "Re-encrypted %d blob(s) to the reduced recipient set\n", rewrapped)
				}
				if hub == nil {
					// P5-PROD-02: the old draft ciphertext is queued (pending_hub_deletes)
					// and deleted on the next hub-enabled sync — state that plainly
					// instead of promising a cleanup that never ran.
					_, _ = fmt.Fprintf(stderr, "note: no hub configured; old draft ciphertext is queued and removed on the next 'devstrap sync --hub-file'. Rotate the affected secrets at their source regardless.\n")
				}
			}
			return nil
		},
	}
	// SEC-01: an optional hub path lets revoke/lost delete superseded
	// ciphertext from the hub immediately. Optional so revoke stays usable
	// without a hub configured.
	if trustState == "revoked" || trustState == "lost" {
		cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path; when set, old ciphertext is deleted from the hub on rewrap")
	}
	return cmd
}

func newDeviceRenameCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <device-id> <name>",
		Short: "Rename a device",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			if err := store.RenameDevice(cmd.Context(), args[0], args[1]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "Device %s renamed to %s\n", args[0], args[1])
			return err
		},
	}
}

// devicePublicKey looks up a device's age recipient by ID (P4-SEC-07).
func devicePublicKey(ctx context.Context, store *state.Store, deviceID string) (string, error) {
	devices, err := store.ListDevices(ctx)
	if err != nil {
		return "", err
	}
	for _, d := range devices {
		if d.ID == deviceID {
			return d.PublicKey, nil
		}
	}
	return "", fmt.Errorf("device %s not found", deviceID)
}

// newDeviceRecipientCommand implements `devstrap devices recipient`, a
// read-only helper that prints the local device's age recipient (or Ed25519
// signing public key with --signing) so it can be shared for out-of-band
// enrollment on another device (P4-SEC-07).
func newDeviceRecipientCommand(stdout io.Writer, opts *options) *cobra.Command {
	var signing bool
	cmd := &cobra.Command{
		Use:   "recipient",
		Short: "Print the local device's age recipient (or signing public key with --signing)",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			dev, err := store.CurrentDevice(cmd.Context())
			if err != nil {
				return err
			}
			if signing {
				if dev.SigningPublicKey == "" {
					return appError{code: exitInvalidConfig, err: fmt.Errorf("local device has no signing public key; run devstrap init")}
				}
				_, err = fmt.Fprintln(stdout, dev.SigningPublicKey)
				return err
			}
			if dev.PublicKey == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("local device has no age recipient; run devstrap init")}
			}
			_, err = fmt.Fprintln(stdout, dev.PublicKey)
			return err
		},
	}
	cmd.Flags().BoolVar(&signing, "signing", false, "print the Ed25519 signing public key instead of the age recipient")
	return cmd
}

// grantWorkspaceKeyToApprovedDevice grants every held WCK epoch to the
// newly-approved device so it can decrypt the full namespace-map history on its
// first pull (P4-SEC-07). It bootstraps epoch 1 if none exists (defensive for
// pre-envelope workspaces) and emits one device.key.granted event per epoch;
// the next sync publishes them. Warnings go to stderr; failures do not abort
// the trust-state change, which already succeeded.
func grantWorkspaceKeyToApprovedDevice(ctx context.Context, stderr io.Writer, opts *options, store *state.Store, deviceID string) {
	recipient, err := devicePublicKey(ctx, store, deviceID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: could not read device %s for workspace key grant: %v\n", deviceID, err)
		return
	}
	if recipient == "" {
		_, _ = fmt.Fprintf(stderr, "warning: device %s has no age recipient; cannot grant workspace key\n", deviceID)
		return
	}
	kr := buildKeyring(opts, store)
	if epoch, _ := kr.CurrentEpoch(ctx); epoch == 0 {
		if _, berr := kr.EnsureBootstrap(ctx); berr != nil {
			_, _ = fmt.Fprintf(stderr, "warning: workspace key bootstrap failed: %v\n", berr)
			return
		}
	}
	grants, gerr := kr.GrantAllEpochs(ctx, recipient)
	if gerr != nil {
		_, _ = fmt.Fprintf(stderr, "warning: workspace key grant to %s failed: %v\n", deviceID, gerr)
		return
	}
	if len(grants) > 0 {
		_, _ = fmt.Fprintf(stderr, "Granted %d workspace key epoch(s) to device %s; run 'devstrap sync' to publish\n", len(grants), deviceID)
	}
}

// rotateWorkspaceKeyOnRevoke mints a fresh WCK at epoch+1 and grants it to the
// remaining approved devices for go-forward forward secrecy (P4-SEC-07). The
// revoked device is excluded because its trust_state was just changed. Skipped
// silently if no epoch was ever bootstrapped. Warnings go to stderr.
func rotateWorkspaceKeyOnRevoke(ctx context.Context, stderr io.Writer, opts *options, store *state.Store) {
	kr := buildKeyring(opts, store)
	epoch, err := kr.CurrentEpoch(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: workspace key rotation skipped: %v\n", err)
		return
	}
	if epoch == 0 {
		return // pre-envelope workspace; nothing to rotate
	}
	newEpoch, grants, rerr := kr.Rotate(ctx)
	if rerr != nil {
		_, _ = fmt.Fprintf(stderr, "warning: workspace key rotation failed: %v\n", rerr)
		return
	}
	if len(grants) > 0 {
		_, _ = fmt.Fprintf(stderr, "Rotated workspace key to epoch %d; granted to %d remaining device(s); run 'devstrap sync' to publish\n", newEpoch, len(grants))
	}
}
