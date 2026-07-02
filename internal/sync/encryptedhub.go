package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/state"
)

// WorkspaceKeyring is the key-epoch abstraction the EncryptedHub decorator uses
// to encrypt outgoing events and decrypt incoming ones (P4-SEC-07). It is
// defined here so the decorator depends on the interface, not the concrete
// keyring, keeping keychain/platform/state dependencies out of internal/sync.
// The concrete implementation lives in internal/workspacekeys.
type WorkspaceKeyring interface {
	// CurrentEpoch returns the active epoch new events encrypt under, or 0 if
	// none has been bootstrapped.
	CurrentEpoch(ctx context.Context) (int64, error)
	// Prime loads every held epoch's WCK into memory so WCK lookups during a
	// Pull are pure and context-free. EncryptedHub.Pull calls Prime before
	// ingesting in-batch grants and decrypting.
	Prime(ctx context.Context) error
	// WCK returns the WCK for an epoch from the in-memory cache, or ok=false if
	// this device does not hold it. Call Prime (and ingest in-batch grants)
	// first.
	WCK(epoch int64) ([]byte, bool)
	// IngestGrant unwraps a device.key.grant addressed to the local device and
	// persists the WCK for its epoch. Grants for other recipients are a no-op.
	IngestGrant(ctx context.Context, grant DeviceKeyGrant) error
}

// EncryptedHub is a Hub decorator that envelope-encrypts the namespace-map event
// log at the hub boundary (P4-SEC-02 / P4-SEC-07). It wraps a backend Hub
// (FileHub or R2Hub) so the backend stores only ciphertext for the event log;
// the local SQLite keeps plaintext PayloadJSON, and the existing Ed25519
// signature and content/prev-hash verification run unchanged on the decrypted
// events (the carrier preserves ID/DeviceID/Seq/HLC/DeviceSig, and decryption
// restores Type/PayloadJSON/ContentHash/PrevEventHash before ApplyEvents). Blob
// operations pass through unchanged — blobs are already age-encrypted by the
// bundle layer, so the blob plane is already ciphertext.
//
// Grant events (device.key.granted) ride the hub as PLAINTEXT on both Push and
// Pull because their payload is itself age-wrapped (the hub cannot decrypt the
// WCK without the recipient's private key). On Pull, grants are ingested into
// the keyring in HLC order before the rest of the batch is decrypted, so a
// newly-approved device obtains its WCKs before decrypting history. If Verify
// is set, Pull verifies each grant's carrier event before ingesting its WCK so
// an untrusted hub cannot inject attacker-known workspace keys.
type EncryptedHub struct {
	Hub     Hub
	Keyring WorkspaceKeyring
	// Verify checks a grant carrier event's signature/trust before its WCK is
	// ingested (P6-SEC-01). nil disables the check (used by unit tests that
	// exercise decryption only). hubFromOptions wires it to
	// (*state.Store).VerifyRemoteEvent so the trust regime is identical to the
	// apply path.
	Verify func(ctx context.Context, ev state.Event) error
}

// Push envelope-encrypts every non-grant event under the current epoch's WCK
// and forwards the carrier events to the backend. Grant events pass through
// unchanged. The carrier preserves ID/DeviceID/Seq/HLC/DeviceSig so hub
// ordering, dedup, and signature verification are byte-for-byte unchanged.
func (h EncryptedHub) Push(ctx context.Context, events []state.Event) error {
	epoch, err := h.Keyring.CurrentEpoch(ctx)
	if err != nil {
		return fmt.Errorf("encrypted hub push: current epoch: %w", err)
	}
	if epoch == 0 {
		return fmt.Errorf("%w: no workspace key epoch bootstrapped (run devstrap init)", ErrMissingWorkspaceKey)
	}
	// Prime the cache so the WCK for the current epoch is in memory. Prime is
	// idempotent and only loads held epochs that are not yet cached.
	if err := h.Keyring.Prime(ctx); err != nil {
		return fmt.Errorf("encrypted hub push: prime keyring: %w", err)
	}
	wck, ok := h.Keyring.WCK(epoch)
	if !ok {
		return fmt.Errorf("%w: epoch %d not held locally", ErrMissingWorkspaceKey, epoch)
	}
	out := make([]state.Event, 0, len(events))
	for _, event := range events {
		if event.Type == EventDeviceKeyGranted {
			out = append(out, event)
			continue
		}
		enc, err := EncryptEvent(event, wck, epoch)
		if err != nil {
			return err
		}
		out = append(out, enc)
	}
	return h.Hub.Push(ctx, out)
}

// Pull fetches events from the backend, primes the keyring, verifies grant
// carrier events when a verifier is configured, ingests verified in-batch grants
// in HLC order, then decrypts enc.v1 envelopes back to plaintext.
//
// The hub is untrusted (zero-knowledge), so a single non-conforming object must
// never be able to wedge sync. Pull therefore degrades instead of aborting the
// whole batch:
//
//   - Missing epoch key: the grant for this event's epoch has not propagated
//     yet. Pull TRUNCATES the batch here — it returns the decryptable prefix so
//     it applies and the caller advances the cursor up to (but not past) this
//     event, then retries from here on the next sync once the grant arrives.
//     Truncating (not skipping) is required so a legitimately-decryptable-later
//     event is never permanently stranded by the cursor jumping over it.
//   - Held-epoch decrypt failure (corruption, forgery, or a cross-device
//     epoch-key collision), a malformed/unknown envelope, or a non-grant
//     plaintext event (a downgrade attempt or a pre-envelope legacy event):
//     the event can never be applied by this device, so Pull SKIPS it with a
//     loud warning and continues. The event is never applied (the security
//     property is preserved — no unauthenticated data enters the log), but one
//     bad object cannot brick the device. This routes bad events through the
//     same "refuse but keep going" posture the plaintext apply path already
//     relies on (ApplyEvents' quarantine + safe cursor).
func (h EncryptedHub) Pull(ctx context.Context, afterHLC int64) ([]state.Event, error) {
	raw, err := h.Hub.Pull(ctx, afterHLC)
	if err != nil {
		return nil, err
	}
	if err := h.Keyring.Prime(ctx); err != nil {
		return nil, fmt.Errorf("encrypted hub pull: prime keyring: %w", err)
	}
	// First pass: ingest grants in (HLC, device, id) order so the WCK for an
	// epoch is available before events encrypted under it are decrypted within
	// the same batch. The inner hub already returns events in that order. A
	// malformed or non-ingestable grant is skipped (logged) rather than aborting
	// the batch — the same untrusted-hub resilience as the second pass.
	for _, event := range raw {
		if event.Type != EventDeviceKeyGranted {
			continue
		}
		var grant DeviceKeyGrant
		if err := json.Unmarshal([]byte(event.PayloadJSON), &grant); err != nil {
			logging.Logger(ctx).Warn("encrypted hub pull: skipping undecodable grant event",
				"event_id", event.ID, "err", err.Error())
			continue
		}
		if h.Verify != nil {
			if err := h.Verify(ctx, event); err != nil {
				logging.Logger(ctx).Warn("encrypted hub pull: refusing unverified grant carrier",
					"event_id", event.ID, "device_id", event.DeviceID, "err", err.Error())
				continue
			}
		}
		if err := h.Keyring.IngestGrant(ctx, grant); err != nil {
			logging.Logger(ctx).Warn("encrypted hub pull: skipping ungrantable key event",
				"event_id", event.ID, "epoch", grant.Epoch, "err", err.Error())
			continue
		}
	}
	// Second pass: decrypt enc.v1, passthrough grants, skip anything this device
	// cannot apply, and truncate at the first not-yet-granted epoch.
	out := make([]state.Event, 0, len(raw))
	for _, event := range raw {
		switch event.Type {
		case EventDeviceKeyGranted:
			out = append(out, event)
		case EventEncryptedV1:
			env, err := ParseEncryptedEnvelope(event)
			if err != nil {
				// Malformed envelope or unknown version: an untrusted hub can
				// serve junk, and a newer client may write a version this build
				// cannot read. Refuse it, but skip rather than wedge.
				logging.Logger(ctx).Warn("encrypted hub pull: skipping undecodable event",
					"event_id", event.ID, "err", err.Error())
				continue
			}
			wck, ok := h.Keyring.WCK(env.Epoch)
			if !ok {
				// The grant for this epoch has not arrived. Truncate: return the
				// decryptable prefix and stop, so the cursor advances only up to
				// here and the next sync retries from this event once granted.
				logging.Logger(ctx).Info("encrypted hub pull: awaiting workspace key grant; deferring remaining events",
					"epoch", env.Epoch, "event_id", event.ID)
				return out, nil
			}
			restored, err := DecryptEvent(event, wck)
			if err != nil {
				// We hold the epoch key but authentication failed: corruption,
				// forgery, or a cross-device epoch-key collision (P4-SEC-07
				// pairing). The event can never be decrypted by this device, so
				// skip it — never apply unauthenticated data — and continue.
				logging.Logger(ctx).Warn("encrypted hub pull: skipping undecryptable event",
					"epoch", env.Epoch, "event_id", event.ID, "err", err.Error())
				continue
			}
			out = append(out, restored)
		default:
			// A non-grant plaintext event where ciphertext is required: a
			// downgrade attempt or a pre-envelope legacy event. Never apply it,
			// but skip (logged) rather than abort so a hostile or stale hub
			// cannot wedge sync.
			logging.Logger(ctx).Warn("encrypted hub pull: skipping non-encrypted event (anti-downgrade)",
				"event_id", event.ID, "type", event.Type)
			continue
		}
	}
	return out, nil
}

// Blob operations pass through. Blobs are age-encrypted by the bundle layer
// before they reach the hub, so the blob plane is already ciphertext; envelope
// encryption covers only the event-log plane.

func (h EncryptedHub) PutBlob(ctx context.Context, sha256Hex string, r io.Reader) error {
	return h.Hub.PutBlob(ctx, sha256Hex, r)
}

func (h EncryptedHub) GetBlob(ctx context.Context, sha256Hex string) (io.ReadCloser, error) {
	return h.Hub.GetBlob(ctx, sha256Hex)
}

func (h EncryptedHub) DeleteBlob(ctx context.Context, sha256Hex string) error {
	return h.Hub.DeleteBlob(ctx, sha256Hex)
}

func (h EncryptedHub) ListBlobs(ctx context.Context) ([]string, error) {
	return h.Hub.ListBlobs(ctx)
}

// Compile-time assertion that EncryptedHub satisfies Hub.
var _ Hub = EncryptedHub{}
