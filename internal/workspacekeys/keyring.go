// Package workspacekeys implements the Workspace Content Key (WCK) epoch
// keyring for envelope encryption of the namespace-map event log (P4-SEC-02 /
// P4-SEC-07).
//
// There is one 32-byte symmetric WCK per integer epoch. Events are
// envelope-encrypted under the current epoch's WCK; the WCK is age-wrapped to
// each approved device's X25519 recipient and published as a device.key.granted
// event. Adding a device only re-wraps the small WCK to the new recipient (one
// grant per held epoch), never bulk content, so a newly-approved device ingests
// every epoch's WCK on its first pull and decrypts the entire history. Revoking
// a device mints a fresh WCK at epoch+1 and wraps it to the remaining approved
// recipients, giving go-forward forward secrecy without re-encrypting past
// events (a revoked device keeps its already-downloaded history — the residual
// risk age's no-native-revocation model accepts, bounded by secret rotation).
//
// The secret WCK lives only in the OS keychain / 0600 file fallback
// (devicekeys.HybridStore). SQLite holds only non-secret metadata (which epochs
// this device holds, and a membership audit of grants). This package keeps
// keychain/platform/state deps out of internal/sync, which depends only on the
// WorkspaceKeyring interface defined in internal/sync/encryptedhub.go.
package workspacekeys

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"filippo.io/age"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// wckSize is the XChaCha20-Poly1305 key length (matches internal/sync.wckSize).
const wckSize = 32

// Keyring is the concrete WCK epoch keyring. It implements dssync.WorkspaceKeyring
// for the EncryptedHub decorator and exposes CLI-facing lifecycle methods
// (EnsureBootstrap / GrantAllEpochs / Rotate) for init and devices.
type Keyring struct {
	Store    *state.Store
	KeyStore devicekeys.HybridStore

	mu       sync.Mutex
	cache    map[int64][]byte
	resolved bool
	// resolved lazily from the store on first use
	workspaceID string
	recipient   string
	identity    string // local device age private identity
}

// New constructs a Keyring backed by the given store and key custody store.
func New(store *state.Store, keyStore devicekeys.HybridStore) *Keyring {
	return &Keyring{Store: store, KeyStore: keyStore}
}

// CurrentEpoch returns the highest WCK epoch this device holds, or 0 if none
// has been bootstrapped (P4-SEC-07).
func (k *Keyring) CurrentEpoch(ctx context.Context) (int64, error) {
	return k.Store.CurrentKeyEpoch(ctx)
}

// Prime loads every held epoch's WCK from the keychain into the in-memory cache
// so subsequent WCK lookups during a Pull are pure and context-free. The
// EncryptedHub decorator calls Prime before ingesting in-batch grants and
// decrypting.
func (k *Keyring) Prime(ctx context.Context) error {
	if err := k.resolve(ctx); err != nil {
		return err
	}
	epochs, err := k.Store.HeldKeyEpochs(ctx)
	if err != nil {
		return err
	}
	for _, epoch := range epochs {
		if _, ok := k.cached(epoch); ok {
			continue
		}
		wck, err := k.KeyStore.LoadWCK(ctx, k.workspaceID, epoch)
		if err != nil {
			return fmt.Errorf("load workspace key epoch %d: %w", epoch, err)
		}
		k.cacheWCK(epoch, wck)
	}
	return nil
}

// WCK returns the WCK for an epoch from the in-memory cache, or ok=false if this
// device does not hold it. Call Prime (and ingest any in-batch grants) first.
func (k *Keyring) WCK(epoch int64) ([]byte, bool) {
	return k.cached(epoch)
}

// EnsureBootstrap mints the first epoch's WCK if none exists, stores it in the
// keychain, and records the epoch locally. It returns the active epoch (the
// minted epoch 1, or the existing highest epoch). The bootstrap self-grant
// event is emitted separately by the init command via GrantAllEpochs so the WCK
// is recoverable from the hub and the grant log is complete.
func (k *Keyring) EnsureBootstrap(ctx context.Context) (int64, error) {
	epoch, err := k.Store.CurrentKeyEpoch(ctx)
	if err != nil {
		return 0, err
	}
	if epoch > 0 {
		return epoch, nil
	}
	if err := k.resolve(ctx); err != nil {
		return 0, err
	}
	const firstEpoch = int64(1)
	wck, err := dssync.NewWCK()
	if err != nil {
		return 0, err
	}
	if err := k.KeyStore.StoreWCK(ctx, k.workspaceID, firstEpoch, wck); err != nil {
		return 0, err
	}
	if err := k.Store.RecordKeyEpoch(ctx, firstEpoch); err != nil {
		return 0, err
	}
	k.cacheWCK(firstEpoch, wck)
	return firstEpoch, nil
}

// GrantAllEpochs wraps every held epoch's WCK to recipient and emits one
// device.key.granted event per epoch (P4-SEC-07). Used by `devices approve` so a
// newly-approved device can decrypt the entire namespace-map history on its
// first pull. Returns the emitted (stamped) events.
func (k *Keyring) GrantAllEpochs(ctx context.Context, recipient string) ([]state.Event, error) {
	if err := k.resolve(ctx); err != nil {
		return nil, err
	}
	if err := k.Prime(ctx); err != nil {
		return nil, err
	}
	epochs, err := k.Store.HeldKeyEpochs(ctx)
	if err != nil {
		return nil, err
	}
	events := make([]state.Event, 0, len(epochs))
	for _, epoch := range epochs {
		wck, ok := k.cached(epoch)
		if !ok {
			return nil, fmt.Errorf("grant: missing WCK for held epoch %d", epoch)
		}
		wrapped, err := wrapWCK(wck, recipient)
		if err != nil {
			return nil, fmt.Errorf("grant epoch %d: %w", epoch, err)
		}
		payload, err := json.Marshal(dssync.DeviceKeyGrant{Epoch: epoch, Recipient: recipient, WrappedKey: wrapped})
		if err != nil {
			return nil, fmt.Errorf("grant epoch %d: marshal payload: %w", epoch, err)
		}
		ev, err := k.Store.InsertLocalEvent(ctx, dssync.NewDeviceKeyGrantEvent(dssync.EventDeviceKeyGranted, string(payload)))
		if err != nil {
			return nil, fmt.Errorf("grant epoch %d: insert event: %w", epoch, err)
		}
		if err := k.Store.RecordKeyGrant(ctx, epoch, recipient, ev.ID, ev.HLC, ev.DeviceID); err != nil {
			return nil, fmt.Errorf("grant epoch %d: record audit: %w", epoch, err)
		}
		events = append(events, ev)
	}
	return events, nil
}

// Rotate mints a fresh WCK at epoch+1, stores it, and wraps it to every
// remaining approved recipient, emitting one device.key.granted event per
// recipient (P4-SEC-07). Used by `devices revoke`/`lost` for go-forward forward
// secrecy: the revoked device is excluded because its trust_state is already
// revoked when Rotate runs, so ApprovedRecipients no longer contains it.
// Returns the new epoch and the emitted grant events.
func (k *Keyring) Rotate(ctx context.Context) (int64, []state.Event, error) {
	if err := k.resolve(ctx); err != nil {
		return 0, nil, err
	}
	current, err := k.Store.CurrentKeyEpoch(ctx)
	if err != nil {
		return 0, nil, err
	}
	next := current + 1
	wck, err := dssync.NewWCK()
	if err != nil {
		return 0, nil, err
	}
	if err := k.KeyStore.StoreWCK(ctx, k.workspaceID, next, wck); err != nil {
		return 0, nil, err
	}
	if err := k.Store.RecordKeyEpoch(ctx, next); err != nil {
		return 0, nil, err
	}
	k.cacheWCK(next, wck)
	recipients, err := k.Store.ApprovedRecipients(ctx)
	if err != nil {
		return 0, nil, err
	}
	events := make([]state.Event, 0, len(recipients))
	for _, recipient := range recipients {
		wrapped, err := wrapWCK(wck, recipient)
		if err != nil {
			return 0, nil, fmt.Errorf("rotate: wrap WCK for %s: %w", recipient, err)
		}
		payload, err := json.Marshal(dssync.DeviceKeyGrant{Epoch: next, Recipient: recipient, WrappedKey: wrapped})
		if err != nil {
			return 0, nil, fmt.Errorf("rotate: marshal payload: %w", err)
		}
		ev, err := k.Store.InsertLocalEvent(ctx, dssync.NewDeviceKeyGrantEvent(dssync.EventDeviceKeyGranted, string(payload)))
		if err != nil {
			return 0, nil, fmt.Errorf("rotate: insert grant: %w", err)
		}
		if err := k.Store.RecordKeyGrant(ctx, next, recipient, ev.ID, ev.HLC, ev.DeviceID); err != nil {
			return 0, nil, fmt.Errorf("rotate: record audit: %w", err)
		}
		events = append(events, ev)
	}
	return next, events, nil
}

// IngestGrant unwraps a device.key.grant payload addressed to the local device
// and persists the WCK for its epoch (P4-SEC-07). Grants for other recipients
// are a no-op. The EncryptedHub decorator calls this for every grant seen
// during a Pull, in HLC order, before decrypting the rest of the batch, so a
// newly-approved device obtains its WCKs before decrypting history.
func (k *Keyring) IngestGrant(ctx context.Context, grant dssync.DeviceKeyGrant) error {
	if err := k.resolve(ctx); err != nil {
		return err
	}
	if grant.Recipient != k.recipient {
		return nil // not addressed to this device
	}
	wck, err := unwrapWCK(grant.WrappedKey, k.identity)
	if err != nil {
		return fmt.Errorf("ingest grant epoch %d: %w", grant.Epoch, err)
	}
	if len(wck) != wckSize {
		return fmt.Errorf("ingest grant epoch %d: unwrapped WCK length = %d, want %d", grant.Epoch, len(wck), wckSize)
	}
	if err := k.KeyStore.StoreWCK(ctx, k.workspaceID, grant.Epoch, wck); err != nil {
		return err
	}
	if err := k.Store.RecordKeyEpoch(ctx, grant.Epoch); err != nil {
		return err
	}
	k.cacheWCK(grant.Epoch, wck)
	return nil
}

func (k *Keyring) resolve(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.resolved {
		return nil
	}
	ws, err := k.Store.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	dev, err := k.Store.CurrentDevice(ctx)
	if err != nil {
		return err
	}
	id, err := k.KeyStore.Read(ctx, dev.ID)
	if err != nil {
		return fmt.Errorf("read local device identity: %w", err)
	}
	k.workspaceID = ws
	k.recipient = id.Recipient
	k.identity = id.Private
	k.resolved = true
	return nil
}

func (k *Keyring) cacheWCK(epoch int64, wck []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cache == nil {
		k.cache = map[int64][]byte{}
	}
	k.cache[epoch] = append([]byte(nil), wck...)
}

func (k *Keyring) cached(epoch int64) ([]byte, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	wck, ok := k.cache[epoch]
	return wck, ok
}

// wrapWCK age-encrypts a WCK to a single X25519 recipient and returns base64.
func wrapWCK(wck []byte, recipient string) (string, error) {
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return "", fmt.Errorf("parse age recipient: %w", err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		return "", fmt.Errorf("encrypt wck: %w", err)
	}
	if _, err := w.Write(wck); err != nil {
		return "", fmt.Errorf("write wck: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("close wck: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// unwrapWCK age-decrypts a base64-wrapped WCK with the local device identity.
func unwrapWCK(wrapped, identity string) ([]byte, error) {
	id, err := age.ParseX25519Identity(identity)
	if err != nil {
		return nil, fmt.Errorf("parse age identity: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil {
		return nil, fmt.Errorf("decode wrapped wck: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(raw), id)
	if err != nil {
		return nil, fmt.Errorf("decrypt wck: %w", err)
	}
	return io.ReadAll(r)
}
