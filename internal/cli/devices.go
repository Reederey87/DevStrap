package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/pairing"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newDevicesCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "Manage device trust state",
	}
	cmd.AddCommand(newDevicesListCommand(stdout, opts))
	cmd.AddCommand(newDevicesPairingCodeCommand(stdout, opts))
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
	var allowEpochGap bool
	var fingerprint string
	var codeBlob string
	cmd := &cobra.Command{
		Use:   "enroll [device-id]",
		Short: "Enroll a remote device record",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deviceID := ""
			if len(args) == 1 {
				deviceID = args[0]
			}
			if strings.TrimSpace(codeBlob) != "" {
				if len(args) > 0 {
					return appError{code: exitUsage, err: fmt.Errorf("--code carries the device id; drop the positional argument")}
				}
				for _, flag := range []string{"name", "os", "arch", "age-recipient", "signing-public-key"} {
					if cmd.Flags().Changed(flag) {
						return appError{code: exitUsage, err: fmt.Errorf("--code is mutually exclusive with the manual enrollment flags (--name/--os/--arch/--age-recipient/--signing-public-key)")}
					}
				}
				decoded, err := pairing.Decode(codeBlob)
				if err != nil {
					return appError{code: exitInvalidConfig, err: err}
				}
				store, err := opts.openState(cmd.Context())
				if err != nil {
					return err
				}
				defer closeStore(store)
				localWorkspaceID, err := store.WorkspaceID(cmd.Context())
				if err != nil {
					return err
				}
				if decoded.WorkspaceID != localWorkspaceID {
					return appError{code: exitInvalidConfig, err: fmt.Errorf("pairing code is for workspace %s, but this store is %s; it was generated on a device from a different workspace", decoded.WorkspaceID, localWorkspaceID)}
				}
				deviceID = decoded.DeviceID
				name = decoded.Name
				osName = decoded.OS
				arch = decoded.Arch
				ageRecipient = decoded.AgeRecipient
				signingPublicKey = decoded.SigningPublicKey
				return runDeviceEnroll(cmd, stdout, opts, store, deviceID, name, osName, arch, ageRecipient, signingPublicKey, approve, allowEpochGap, fingerprint)
			}
			if len(args) == 0 {
				return appError{code: exitUsage, err: fmt.Errorf("device id argument required (or use --code)")}
			}
			if name == "" || osName == "" || arch == "" || ageRecipient == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--name, --os, --arch, and --age-recipient are required")}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			return runDeviceEnroll(cmd, stdout, opts, store, deviceID, name, osName, arch, ageRecipient, signingPublicKey, approve, allowEpochGap, fingerprint)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "device display name")
	cmd.Flags().StringVar(&osName, "os", "", "device operating system")
	cmd.Flags().StringVar(&arch, "arch", "", "device architecture")
	cmd.Flags().StringVar(&ageRecipient, "age-recipient", "", "device age recipient public key")
	cmd.Flags().StringVar(&signingPublicKey, "signing-public-key", "", "device Ed25519 signing public key")
	cmd.Flags().BoolVar(&approve, "approve", false, "mark the enrolled device approved immediately")
	cmd.Flags().BoolVar(&allowEpochGap, "allow-epoch-gap", false, "approve even though this device's workspace keys are incomplete (the enrolled device will quarantine events at the missing epochs — and its open quarantine conflicts keep 'hub gc' refused on it — until re-approved from a complete device)")
	cmd.Flags().StringVar(&fingerprint, "fingerprint", "", "with --approve: the device fingerprint confirmed out-of-band (see 'devstrap devices recipient --fingerprint' on that device); skips the interactive prompt")
	cmd.Flags().StringVar(&codeBlob, "code", "", "one-paste pairing code from 'devstrap devices pairing-code'; with --approve --fingerprint, completes founder-side enrollment")
	return cmd
}

func runDeviceEnroll(cmd *cobra.Command, stdout io.Writer, opts *options, store *state.Store, deviceID, name, osName, arch, ageRecipient, signingPublicKey string, approve, allowEpochGap bool, fingerprint string) error {
	// SECU-05: require a signing public key when --approve is set so an
	// approved device can never silently combine with the fail-open event
	// verification path (SECU-03).
	if approve && strings.TrimSpace(signingPublicKey) == "" {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("--approve requires --signing-public-key so the device's events can be signature-verified")}
	}
	trustState := "pending"
	if approve {
		trustState = "approved"
	}
	// P6-SEC-03: refuse to approve from an incomplete keyring — the grant set
	// would inherit the gap and wedge the new device. Runs BEFORE any trust
	// write so a refusal leaves no partial state, and BEFORE the fingerprint
	// prompt so the operator is never asked to confirm an approval that will be
	// refused anyway.
	if approve && !allowEpochGap {
		if err := checkEpochContiguity(cmd.Context(), store); err != nil {
			return err
		}
	}
	// P4-SEC-04: approving binds the device's keys into the trust set, so it
	// must be gated on out-of-band fingerprint confirmation. The fingerprint
	// is computed from the flag/code inputs (the keys being enrolled), never
	// from the local keystore.
	if approve {
		if err := confirmDeviceFingerprint(cmd, deviceID, signingPublicKey, ageRecipient, fingerprint); err != nil {
			return err
		}
	}
	device := state.Device{
		ID:               deviceID,
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
	// P4-SEC-07: when --approve is set, grant every held WCK epoch to the
	// newly-approved device so it can decrypt the namespace-map history on its
	// first pull.
	if approve {
		grantWorkspaceKeyToApprovedDevice(cmd.Context(), cmd.ErrOrStderr(), opts, store, deviceID)
		// Events that arrived before enrollment were quarantined (auto-created
		// pending placeholder / missing signing key); approving via enroll must
		// replay them just like `devices approve` does.
		replayQuarantinedEvents(cmd.Context(), cmd.ErrOrStderr(), opts, store, deviceID)
	}
	_, err := fmt.Fprintf(stdout, "Device %s enrolled as %s\n", deviceID, trustState)
	return err
}

func newDevicesPairingCodeCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "pairing-code",
		Short: "Print this device's one-paste pairing code",
		Args:  cobra.NoArgs,
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
			workspaceID, err := store.WorkspaceID(cmd.Context())
			if err != nil {
				return err
			}
			if dev.PublicKey == "" || dev.SigningPublicKey == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("local device is missing keys; run devstrap init")}
			}
			blob, err := pairing.Encode(pairing.Code{
				WorkspaceID:      workspaceID,
				DeviceID:         dev.ID,
				Name:             dev.Name,
				OS:               dev.OS,
				Arch:             dev.Arch,
				AgeRecipient:     dev.PublicKey,
				SigningPublicKey: dev.SigningPublicKey,
			})
			if err != nil {
				return err
			}
			fp, err := devicekeys.Fingerprint(dev.SigningPublicKey, dev.PublicKey)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(stdout, blob); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.ErrOrStderr(), "Pairing code printed to stdout. On the other device run:\n  devstrap init --join --code '<paste>'      # first device setup\n  devstrap devices enroll --code '<paste>' --approve --fingerprint <this device's fingerprint>\nThis device's fingerprint (read it aloud over a trusted channel; the approver must confirm it):\n  %s\n", fp)
			return err
		},
	}
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
				// P4-SEC-04: fingerprint is the LAST column so existing awk
				// scrapes of the earlier fields stay stable. A row missing
				// either key (a bare placeholder) has no bindable fingerprint.
				fp := "-"
				if device.SigningPublicKey != "" && device.PublicKey != "" {
					if computed, err := devicekeys.Fingerprint(device.SigningPublicKey, device.PublicKey); err == nil {
						fp = computed
					}
				}
				_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s/%s\t%s\n", device.ID, device.TrustState, device.Name, device.OS, device.Arch, fp)
			}
			return nil
		},
	}
}

func newDeviceTrustCommand(stdout io.Writer, opts *options, use, trustState string) *cobra.Command {
	var hubFile string
	var allowEpochGap bool
	var fingerprint string
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
			// P6-SEC-03: refuse to approve from an incomplete keyring — the
			// grant set would inherit the gap and wedge the approved device.
			// Runs BEFORE the trust write so a refusal leaves no partial state,
			// and BEFORE the fingerprint prompt so the operator is never asked
			// to confirm an approval that will be refused anyway.
			if trustState == "approved" && !allowEpochGap {
				if err := checkEpochContiguity(cmd.Context(), store); err != nil {
					return err
				}
			}
			// P4-SEC-04: approval binds the stored device's keys into the trust
			// set, so it is gated on out-of-band fingerprint confirmation BEFORE
			// any DB write. The fingerprint is computed from the STORED row, never
			// the local keystore. Revoke/lost are untouched.
			if use == "approve" {
				dev, err := deviceByID(cmd.Context(), store, args[0])
				if err != nil {
					return err
				}
				// SECU-05 tightening: a bare pending placeholder auto-created by
				// sync has no keys to bind — approving it would pin nothing and
				// re-open the fail-open verification path. Refuse with a
				// re-enroll remedy rather than approve a keyless row.
				if strings.TrimSpace(dev.SigningPublicKey) == "" || strings.TrimSpace(dev.PublicKey) == "" {
					return appError{code: exitInvalidConfig, err: fmt.Errorf(
						"device %s cannot be approved: it has no %s on record (a bare placeholder auto-created by sync). Re-enroll it with full keys: devstrap devices enroll %s --name <n> --os <os> --arch <arch> --age-recipient <rec> --signing-public-key <sig> --approve --fingerprint <fp>",
						args[0], missingDeviceKeyDesc(dev), args[0])}
				}
				if err := confirmDeviceFingerprint(cmd, args[0], dev.SigningPublicKey, dev.PublicKey, fingerprint); err != nil {
					return err
				}
			}
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
				replayQuarantinedEvents(cmd.Context(), stderr, opts, store, args[0])
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
	if use == "approve" {
		cmd.Flags().BoolVar(&allowEpochGap, "allow-epoch-gap", false, "approve even though this device's workspace keys are incomplete (the approved device will quarantine events at the missing epochs — and its open quarantine conflicts keep 'hub gc' refused on it — until re-approved from a complete device)")
		cmd.Flags().StringVar(&fingerprint, "fingerprint", "", "the device fingerprint confirmed out-of-band (see 'devstrap devices recipient --fingerprint' on that device); skips the interactive prompt")
	}
	return cmd
}

// checkEpochContiguity refuses an approval from a device whose own workspace
// keyring is demonstrably incomplete (P6-SEC-03). Approval grants exactly the
// approver's held epochs (GrantAllEpochs), so a gap in 1..max — or an open
// key-grant wait for ciphertext this device has seen but cannot decrypt —
// would be inherited by the approved device, which then wedges (now: grace-
// quarantines) on the missing epochs until someone re-approves it from a
// complete device. Passes trivially when NO keys are held: a keyless joiner
// grants nothing on approve — that approval is the P4-SEC-04 founder-pinning
// ceremony and must stay friction-free. --allow-epoch-gap overrides (the
// worktree-finalize --allow-stale-base precedent).
func checkEpochContiguity(ctx context.Context, store *state.Store) error {
	epochs, err := store.HeldKeyEpochs(ctx)
	if err != nil {
		return err
	}
	if len(epochs) == 0 {
		return nil
	}
	var missing []string
	expect := int64(1)
	for _, epoch := range epochs { // ascending
		for expect < epoch {
			missing = append(missing, strconv.FormatInt(expect, 10))
			expect++
		}
		expect = epoch + 1
	}
	waits, err := store.OpenKeyGrantWaits(ctx)
	if err != nil {
		return err
	}
	var waiting []string
	for _, w := range waits {
		label := strconv.FormatInt(w.Epoch, 10)
		if w.KID != "" {
			// %.8s: the kid rode an unauthenticated hub envelope, so its
			// length is hostile input — never slice it directly.
			label += fmt.Sprintf(" (kid %.8s…)", w.KID)
		}
		waiting = append(waiting, label)
	}
	if len(missing) == 0 && len(waiting) == 0 {
		return nil
	}
	var reasons []string
	if len(missing) > 0 {
		reasons = append(reasons, fmt.Sprintf("missing workspace key epoch(s) %s", strings.Join(missing, ", ")))
	}
	if len(waiting) > 0 {
		reasons = append(reasons, fmt.Sprintf("awaiting key grant(s) for epoch(s) %s", strings.Join(waiting, ", ")))
	}
	return appError{code: exitInvalidConfig, err: fmt.Errorf(
		"refusing to approve: this device's workspace keys are incomplete (%s), so the approval would grant an incomplete key set and strand the approved device on the gap; run 'devstrap sync' (or have a complete device re-approve THIS device) first, or pass --allow-epoch-gap to approve anyway — the approved device will then quarantine unreadable events until re-approved from a complete device",
		strings.Join(reasons, "; "))}
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

type eventVerificationConflictDetails struct {
	Kind      string `json:"kind"`
	DeviceID  string `json:"device_id"`
	EventJSON string `json:"event_json"`
}

type quarantinedEventReplay struct {
	conflict state.Conflict
	event    state.Event
}

func replayQuarantinedEvents(ctx context.Context, stderr io.Writer, opts *options, store *state.Store, deviceID string) {
	conflicts, err := store.OpenConflictsByType(ctx, dssync.ConflictEventVerification)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: could not inspect quarantined events for device %s: %v\n", deviceID, err)
		return
	}
	var replays []quarantinedEventReplay
	for _, conflict := range conflicts {
		var details eventVerificationConflictDetails
		if err := json.Unmarshal([]byte(conflict.DetailsJSON), &details); err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: could not decode quarantined event conflict %s: %v\n", conflict.ID, err)
			continue
		}
		if details.DeviceID != deviceID {
			continue
		}
		// Divergent-duplicate conflicts are data-integrity disputes with an
		// already-stored event of the same ID — approving the device does not
		// make them applicable, and a replay would "succeed" only because the
		// ORIGINAL event exists. Leave them open for manual resolution.
		if details.Kind != dssync.EventConflictKindVerification {
			continue
		}
		var event state.Event
		if err := json.Unmarshal([]byte(details.EventJSON), &event); err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: could not decode quarantined event for conflict %s: %v\n", conflict.ID, err)
			continue
		}
		replays = append(replays, quarantinedEventReplay{conflict: conflict, event: event})
	}
	sort.Slice(replays, func(i, j int) bool {
		if replays[i].event.HLC == replays[j].event.HLC {
			if replays[i].event.Seq == replays[j].event.Seq {
				return replays[i].event.ID < replays[j].event.ID
			}
			return replays[i].event.Seq < replays[j].event.Seq
		}
		return replays[i].event.HLC < replays[j].event.HLC
	})
	var replayed int
	for _, replay := range replays {
		if _, err := dssync.ApplyEvents(ctx, store, []state.Event{replay.event}); err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: could not replay quarantined event %s for device %s: %v\n", replay.event.ID, deviceID, err)
			continue
		}
		if _, err := store.EventByID(ctx, replay.event.ID); err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: quarantined event %s for device %s was not applied: %v\n", replay.event.ID, deviceID, err)
			continue
		}
		resolution, err := json.Marshal(map[string]string{
			"action":   "replayed-after-device-approval",
			"event_id": replay.event.ID,
		})
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: could not encode replay resolution for event %s: %v\n", replay.event.ID, err)
			continue
		}
		// A replayed device.key.granted only records the membership audit via
		// ApplyEvents; the WCK itself is ingested by EncryptedHub.Pull, which
		// already advanced past this event when it was quarantined and will
		// never re-pull it. Ingest it here so the granted (epoch, kid) is not
		// permanently lost to this device (post-#33 review finding): without
		// this, every fleet event sealed under that key would defer forever.
		if replay.event.Type == dssync.EventDeviceKeyGranted {
			var grant dssync.DeviceKeyGrant
			if err := json.Unmarshal([]byte(replay.event.PayloadJSON), &grant); err != nil {
				_, _ = fmt.Fprintf(stderr, "warning: could not decode replayed grant %s: %v\n", replay.event.ID, err)
			} else if err := buildKeyring(opts, store).IngestGrant(ctx, grant); err != nil {
				_, _ = fmt.Fprintf(stderr, "warning: could not ingest replayed grant %s (epoch %d): %v\n", replay.event.ID, grant.Epoch, err)
			}
		}
		if err := store.ResolveConflict(ctx, replay.conflict.ID, string(resolution)); err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: could not resolve quarantined event conflict %s: %v\n", replay.conflict.ID, err)
			continue
		}
		replayed++
	}
	if replayed > 0 {
		_, _ = fmt.Fprintf(stderr, "Replayed %d quarantined event(s) from device %s\n", replayed, deviceID)
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

// deviceByID returns the full stored device row by ID (P4-SEC-04).
func deviceByID(ctx context.Context, store *state.Store, deviceID string) (state.Device, error) {
	devices, err := store.ListDevices(ctx)
	if err != nil {
		return state.Device{}, err
	}
	for _, d := range devices {
		if d.ID == deviceID {
			return d, nil
		}
	}
	return state.Device{}, appError{code: exitInvalidConfig, err: fmt.Errorf("device %s not found", deviceID)}
}

// missingDeviceKeyDesc names which key(s) a placeholder row lacks, for the
// SECU-05 re-enroll remedy.
func missingDeviceKeyDesc(dev state.Device) string {
	noSign := strings.TrimSpace(dev.SigningPublicKey) == ""
	noRecip := strings.TrimSpace(dev.PublicKey) == ""
	switch {
	case noSign && noRecip:
		return "signing public key or age recipient"
	case noSign:
		return "signing public key"
	default:
		return "age recipient"
	}
}

// confirmDeviceFingerprint gates a device approval on out-of-band fingerprint
// confirmation (P4-SEC-04). expected is derived from the keys being approved
// (never from the local keystore). It returns nil to proceed and a non-nil
// appError to refuse WITHOUT any DB write:
//   - --fingerprint given: constant-time compare; mismatch refuses.
//   - no flag + TTY: prints the fingerprint and prompts; anything but "yes"
//     refuses.
//   - no flag + non-TTY: refuses with a copy-paste remedy embedding the
//     computed fingerprint.
func confirmDeviceFingerprint(cmd *cobra.Command, deviceID, signingPublicKey, ageRecipient, flagFP string) error {
	expected, err := devicekeys.Fingerprint(signingPublicKey, ageRecipient)
	if err != nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("cannot compute fingerprint for device %s: %w", deviceID, err)}
	}
	if strings.TrimSpace(flagFP) != "" {
		if devicekeys.FingerprintEqual(flagFP, expected) {
			return nil
		}
		return appError{code: exitInvalidConfig, err: fmt.Errorf(
			"fingerprint mismatch for device %s: the value you passed does not match this device's keys.\n  expected: %s\nCompare the full value out-of-band (e.g. over a call) before approving; no changes were made",
			deviceID, expected)}
	}
	stderr := cmd.ErrOrStderr()
	if f, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		_, _ = fmt.Fprintf(stderr,
			"Approving device %s. Verify its fingerprint out-of-band (it must match\n'devstrap devices recipient --fingerprint' on that device, character for character):\n\n  %s\n\n",
			deviceID, expected)
		_, _ = fmt.Fprint(stderr, "Type 'yes' to approve: ")
		reader := bufio.NewReader(cmd.InOrStdin())
		line, _ := reader.ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			return appError{code: exitInvalidConfig, err: fmt.Errorf("approval of device %s refused: fingerprint not confirmed", deviceID)}
		}
		return nil
	}
	return appError{code: exitInvalidConfig, err: fmt.Errorf(
		"approving device %s requires fingerprint confirmation, but stdin is not a terminal.\nVerify the fingerprint out-of-band against 'devstrap devices recipient --fingerprint' on that device, then re-run with:\n  --fingerprint %s",
		deviceID, expected)}
}

// newDeviceRecipientCommand implements `devstrap devices recipient`, a
// read-only helper that prints the local device's age recipient (or Ed25519
// signing public key with --signing) so it can be shared for out-of-band
// enrollment on another device (P4-SEC-07).
func newDeviceRecipientCommand(stdout io.Writer, opts *options) *cobra.Command {
	var signing bool
	var workspaceID bool
	var fingerprint bool
	cmd := &cobra.Command{
		Use:   "recipient",
		Short: "Print the local device's age recipient (or signing public key with --signing, workspace id with --workspace-id, fingerprint with --fingerprint)",
		RunE: func(cmd *cobra.Command, args []string) error {
			set := 0
			for _, b := range []bool{signing, workspaceID, fingerprint} {
				if b {
					set++
				}
			}
			if set > 1 {
				return appError{code: exitUsage, err: fmt.Errorf("--signing, --workspace-id, and --fingerprint are mutually exclusive")}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			// P4-SEC-07 pairing: print the workspace id alone so scripts can
			// thread it into `init --join --workspace-id` (the bare recipient
			// output is frozen — existing scripts consume it unadorned).
			if workspaceID {
				wsID, err := store.WorkspaceID(cmd.Context())
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(stdout, wsID)
				return err
			}
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
			// P4-SEC-04: print this device's fingerprint so it can be read aloud
			// on the untrusted pairing channel and compared during approval.
			if fingerprint {
				if dev.SigningPublicKey == "" || dev.PublicKey == "" {
					return appError{code: exitInvalidConfig, err: fmt.Errorf("local device is missing the keys needed for a fingerprint; run devstrap init")}
				}
				fp, err := devicekeys.Fingerprint(dev.SigningPublicKey, dev.PublicKey)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(stdout, fp)
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
	cmd.Flags().BoolVar(&workspaceID, "workspace-id", false, "print the workspace id instead of the age recipient (for init --join --workspace-id)")
	cmd.Flags().BoolVar(&fingerprint, "fingerprint", false, "print this device's fingerprint to compare out-of-band during approval (P4-SEC-04)")
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
		// P6-SEC-02: found defensively ONLY for a founder. A joining device that
		// has not yet been granted the fleet WCK must never self-mint one here —
		// doing so would let it push events under a key nobody else holds (the
		// same data loss the founder/join split closes), reachable via a joiner
		// approving another device before it is itself granted. A keyless joiner
		// simply has nothing to grant yet.
		if isJoiner(opts) {
			// P4-SEC-04 joiner half: this branch IS the founder-pinning
			// ceremony. Approving the founder on a keyless joiner grants
			// nothing (a joiner never self-mints), but the approved row pins
			// the founder's keys and flips verification fail-closed BEFORE
			// the joiner's first sync — so the wording must not read as if
			// the enrolled device were the one awaiting a grant.
			_, _ = fmt.Fprintf(stderr, "note: this joining device holds no workspace key, so nothing was granted to %s — the approval still pins that device's keys for fail-closed verification; this device receives its own key when an approved device approves it (then run 'devstrap sync')\n", deviceID)
			return
		}
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
