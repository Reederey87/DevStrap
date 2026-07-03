// Package workspacekeys implements the Workspace Content Key (WCK) epoch
// keyring for envelope encryption of the namespace-map event log (P4-SEC-02 /
// P4-SEC-07).
//
// A WCK is a 32-byte symmetric key addressed by (epoch, kid), where kid =
// hex(sha256(wck)) (the full digest) names the specific key (P6-SEC-02). Keying by identity
// rather than bare epoch lets two keys minted independently at the same epoch —
// a legacy joiner's self-mint and the founder's fleet key — coexist instead of
// clobbering each other, which is what makes refusing key overwrites safe
// (P6-SEC-01b): nothing ever needs to displace an existing key, because a new
// key lands in its own (epoch, kid) slot. Events are envelope-encrypted under
// the push key (the fleet key at the highest epoch); the WCK is age-wrapped to
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
// (devicekeys.HybridStore). SQLite holds only non-secret metadata (which keys
// this device holds, each key's origin — 'self' mint, verified 'grant', or
// pre-kid 'legacy' backfill — and a membership audit of grants). Only three
// paths may write key metadata: founder bootstrap/rotate ('self'), verified
// grant ingestion ('grant'), and the one-time migration backfill ('legacy')
// (P6-SEC-01c). This package keeps keychain/platform/state deps out of
// internal/sync, which depends only on the WorkspaceKeyring interface defined
// in internal/sync/encryptedhub.go.
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

// Key origins recorded in workspace_keys.origin (P6-SEC-01c).
const (
	originSelf   = "self"   // founder bootstrap or rotate minted it locally
	originGrant  = "grant"  // ingested from a verified device.key.granted event
	originLegacy = "legacy" // pre-kid row backfilled by migration 00014
)

// keyID addresses one WCK in the in-memory cache.
type keyID struct {
	epoch int64
	kid   string
}

// cachedKey is a cache entry: the secret bytes plus the metadata origin used
// for push-key preference (fleet 'grant' keys beat local 'self' mints).
type cachedKey struct {
	wck    []byte
	origin string
}

// Keyring is the concrete WCK keyring. It implements dssync.WorkspaceKeyring
// for the EncryptedHub decorator and exposes CLI-facing lifecycle methods
// (EnsureBootstrap / GrantAllEpochs / Rotate) for init and devices.
type Keyring struct {
	Store    *state.Store
	KeyStore devicekeys.HybridStore

	mu       sync.Mutex
	cache    map[keyID]cachedKey
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

// Prime loads every held key's WCK from the keychain into the in-memory cache
// so subsequent WCK lookups during a Pull are pure and context-free. Legacy
// kid-less rows (pre-migration-00014 keys) are lazily upgraded here: the key is
// loaded from its legacy custody slot, its kid is computed from the bytes, the
// key is re-stored under the kid-aware slot (the legacy slot is left in place —
// a harmless duplicate), and the metadata row is rewritten with the real kid.
// The EncryptedHub decorator calls Prime before ingesting in-batch grants and
// decrypting.
func (k *Keyring) Prime(ctx context.Context) error {
	if err := k.resolve(ctx); err != nil {
		return err
	}
	held, err := k.Store.HeldKeys(ctx)
	if err != nil {
		return err
	}
	for _, hk := range held {
		if hk.KID != "" {
			if _, ok := k.cached(hk.Epoch, hk.KID); ok {
				continue
			}
			wck, err := k.KeyStore.LoadWCK(ctx, k.workspaceID, hk.Epoch, hk.KID)
			if err != nil {
				return fmt.Errorf("load workspace key epoch %d kid %s: %w", hk.Epoch, hk.KID, err)
			}
			k.cacheWCK(hk.Epoch, hk.KID, hk.Origin, wck)
			continue
		}
		// Legacy kid-less row: upgrade in place.
		wck, err := k.KeyStore.LoadWCK(ctx, k.workspaceID, hk.Epoch, "")
		if err != nil {
			return fmt.Errorf("load legacy workspace key epoch %d: %w", hk.Epoch, err)
		}
		kid := dssync.KIDForWCK(wck)
		// Same defense as IngestGrant (P6-SEC-01b): a custody slot's bytes are
		// content-bound to its kid, so the upgrade may never displace different
		// bytes already in the kid-aware slot — that would mean local
		// corruption or tampering, and overwriting would destroy a key.
		if existing, lerr := k.KeyStore.LoadWCK(ctx, k.workspaceID, hk.Epoch, kid); lerr == nil && !bytes.Equal(existing, wck) {
			return fmt.Errorf("upgrade legacy workspace key epoch %d: kid-aware slot %s holds different bytes (refusing custody overwrite)", hk.Epoch, kid)
		}
		if err := k.KeyStore.StoreWCK(ctx, k.workspaceID, hk.Epoch, kid, wck); err != nil {
			return fmt.Errorf("upgrade legacy workspace key epoch %d: %w", hk.Epoch, err)
		}
		if err := k.Store.UpdateKeyKid(ctx, hk.Epoch, kid); err != nil {
			return fmt.Errorf("upgrade legacy workspace key epoch %d: %w", hk.Epoch, err)
		}
		k.cacheWCK(hk.Epoch, kid, hk.Origin, wck)
	}
	return nil
}

// WCKCandidates returns the candidate WCKs for decrypting an envelope addressed
// to (epoch, kid), from the in-memory cache. kid != "" selects the exact key;
// kid == "" (a legacy pre-kid envelope) returns every held key at the epoch —
// the caller tries each, and the AEAD authenticates so a wrong candidate just
// fails. Empty means no candidate is held (the grant has not arrived). Call
// Prime (and ingest any in-batch grants) first.
func (k *Keyring) WCKCandidates(epoch int64, kid string) [][]byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	if kid != "" {
		if entry, ok := k.cache[keyID{epoch: epoch, kid: kid}]; ok {
			return [][]byte{entry.wck}
		}
		return nil
	}
	var out [][]byte
	for id, entry := range k.cache {
		if id.epoch == epoch {
			out = append(out, entry.wck)
		}
	}
	return out
}

// WCK returns the preferred WCK at an epoch from the in-memory cache (fleet
// 'grant' keys beat local 'self' mints beat 'legacy'), or ok=false if this
// device holds none. It is a convenience for callers addressing a whole epoch;
// decrypt paths use WCKCandidates.
func (k *Keyring) WCK(epoch int64) ([]byte, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	best := cachedKey{}
	found := false
	for id, entry := range k.cache {
		if id.epoch != epoch {
			continue
		}
		if !found || originRank(entry.origin) > originRank(best.origin) {
			best = entry
			found = true
		}
	}
	if !found {
		return nil, false
	}
	return best.wck, true
}

// PushKey returns the key new outgoing events encrypt under: the highest held
// epoch, and within it the fleet key — origin 'grant' is preferred over a local
// 'self' mint, then 'legacy' — so a legacy joiner that later receives the
// founder's grant at the same epoch converges onto the fleet key (P6-SEC-02).
// epoch == 0 with a nil error means this device holds no workspace key yet.
func (k *Keyring) PushKey(ctx context.Context) (int64, string, []byte, error) {
	if err := k.Prime(ctx); err != nil {
		return 0, "", nil, err
	}
	held, err := k.Store.HeldKeys(ctx)
	if err != nil {
		return 0, "", nil, err
	}
	best := state.HeldKey{}
	found := false
	for _, hk := range held {
		if !found || hk.Epoch > best.Epoch ||
			(hk.Epoch == best.Epoch && originRank(hk.Origin) > originRank(best.Origin)) {
			best = hk
			found = true
		}
	}
	if !found {
		return 0, "", nil, nil
	}
	wck, ok := k.cached(best.Epoch, best.KID)
	if !ok {
		return 0, "", nil, fmt.Errorf("push key: WCK for held key epoch %d kid %s not in cache", best.Epoch, best.KID)
	}
	return best.Epoch, best.KID, wck, nil
}

// originRank orders key origins for push preference: verified fleet grants
// beat local self-mints beat pre-kid legacy rows.
func originRank(origin string) int {
	switch origin {
	case originGrant:
		return 2
	case originSelf:
		return 1
	default:
		return 0
	}
}

// EnsureBootstrap mints the first epoch's WCK if none exists, stores it in the
// keychain under its (epoch, kid), and records the metadata row with origin
// 'self'. It returns the active epoch (the minted epoch 1, or the existing
// highest epoch). Only the workspace founder may reach this path (the sync
// founding gate and devices approve); a joiner waits for a grant (P6-SEC-02).
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
	kid := dssync.KIDForWCK(wck)
	if err := k.KeyStore.StoreWCK(ctx, k.workspaceID, firstEpoch, kid, wck); err != nil {
		return 0, err
	}
	if err := k.Store.RecordKeyEpoch(ctx, firstEpoch, kid, originSelf); err != nil {
		return 0, err
	}
	k.cacheWCK(firstEpoch, kid, originSelf, wck)
	return firstEpoch, nil
}

// GrantAllEpochs wraps the active key of every held epoch to recipient and
// emits one device.key.granted event per epoch (P4-SEC-07). When several keys
// coexist at an epoch (P6-SEC-02 collision), only the fleet key — the same
// preference PushKey uses — is granted, so the recipient converges onto the
// key the fleet encrypts under. Used by `devices approve` so a newly-approved
// device can decrypt the entire namespace-map history on its first pull.
// Returns the emitted (stamped) events.
func (k *Keyring) GrantAllEpochs(ctx context.Context, recipient string) ([]state.Event, error) {
	if err := k.resolve(ctx); err != nil {
		return nil, err
	}
	if err := k.Prime(ctx); err != nil {
		return nil, err
	}
	held, err := k.Store.HeldKeys(ctx)
	if err != nil {
		return nil, err
	}
	// Pick the active key per epoch, ascending epoch order (HeldKeys is sorted).
	bestByEpoch := map[int64]state.HeldKey{}
	var epochs []int64
	for _, hk := range held {
		best, ok := bestByEpoch[hk.Epoch]
		if !ok {
			epochs = append(epochs, hk.Epoch)
		}
		if !ok || originRank(hk.Origin) > originRank(best.Origin) {
			bestByEpoch[hk.Epoch] = hk
		}
	}
	events := make([]state.Event, 0, len(epochs))
	for _, epoch := range epochs {
		hk := bestByEpoch[epoch]
		wck, ok := k.cached(hk.Epoch, hk.KID)
		if !ok {
			return nil, fmt.Errorf("grant: missing WCK for held key epoch %d kid %s", hk.Epoch, hk.KID)
		}
		wrapped, err := wrapWCK(wck, recipient)
		if err != nil {
			return nil, fmt.Errorf("grant epoch %d: %w", epoch, err)
		}
		payload, err := json.Marshal(dssync.DeviceKeyGrant{Epoch: epoch, KID: hk.KID, Recipient: recipient, WrappedKey: wrapped})
		if err != nil {
			return nil, fmt.Errorf("grant epoch %d: marshal payload: %w", epoch, err)
		}
		var ev state.Event
		if err := k.Store.WithTx(ctx, func(tx *state.Tx) error {
			var err error
			ev, err = k.Store.InsertLocalEventTx(ctx, tx, dssync.NewDeviceKeyGrantEvent(dssync.EventDeviceKeyGranted, string(payload)))
			if err != nil {
				return fmt.Errorf("insert event: %w", err)
			}
			if err := tx.RecordKeyGrantTx(ctx, epoch, hk.KID, recipient, ev); err != nil {
				return fmt.Errorf("record audit: %w", err)
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("grant epoch %d: %w", epoch, err)
		}
		events = append(events, ev)
	}
	return events, nil
}

// Rotate mints a fresh WCK at epoch+1, stores it under its (epoch, kid) with
// origin 'self', and wraps it to every remaining approved recipient, emitting
// one device.key.granted event per recipient (P4-SEC-07). Used by `devices
// revoke`/`lost` for go-forward forward secrecy (the revoked device is
// excluded because its trust_state is already revoked when Rotate runs, so
// ApprovedRecipients no longer contains it) and by periodic rotation (`keys
// rotate` / the sync age trigger). Returns the new epoch and the emitted
// grant events.
//
// Every grant is WRAPPED BEFORE any state is written (post-#56 Codex review,
// P1): the by-far-likeliest mid-rotation failure is a malformed recipient on
// an approved device row, and failing after StoreWCK/RecordKeyEpoch would
// leave a half-minted active epoch — the caller's next push would seal events
// under a key whose grants never published, and the fresh created_at would
// keep the age trigger from retrying. With wrap-first ordering a bad
// recipient aborts before the epoch exists at all. The residual window is a
// DB/custody failure between RecordKeyEpoch and the last grant insert; callers
// treat any Rotate error as fatal for their cycle so it is at least loud.
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
	kid := dssync.KIDForWCK(wck)
	recipients, err := k.Store.ApprovedRecipients(ctx)
	if err != nil {
		return 0, nil, err
	}
	type pendingGrant struct {
		recipient string
		payload   string
	}
	pending := make([]pendingGrant, 0, len(recipients))
	for _, recipient := range recipients {
		wrapped, err := wrapWCK(wck, recipient)
		if err != nil {
			return 0, nil, fmt.Errorf("rotate: wrap WCK for %s: %w", recipient, err)
		}
		payload, err := json.Marshal(dssync.DeviceKeyGrant{Epoch: next, KID: kid, Recipient: recipient, WrappedKey: wrapped})
		if err != nil {
			return 0, nil, fmt.Errorf("rotate: marshal payload: %w", err)
		}
		pending = append(pending, pendingGrant{recipient: recipient, payload: string(payload)})
	}
	if err := k.KeyStore.StoreWCK(ctx, k.workspaceID, next, kid, wck); err != nil {
		return 0, nil, err
	}
	if err := k.Store.RecordKeyEpoch(ctx, next, kid, originSelf); err != nil {
		return 0, nil, err
	}
	k.cacheWCK(next, kid, originSelf, wck)
	events := make([]state.Event, 0, len(pending))
	for _, grant := range pending {
		var ev state.Event
		if err := k.Store.WithTx(ctx, func(tx *state.Tx) error {
			var err error
			ev, err = k.Store.InsertLocalEventTx(ctx, tx, dssync.NewDeviceKeyGrantEvent(dssync.EventDeviceKeyGranted, grant.payload))
			if err != nil {
				return fmt.Errorf("insert grant: %w", err)
			}
			if err := tx.RecordKeyGrantTx(ctx, next, kid, grant.recipient, ev); err != nil {
				return fmt.Errorf("record audit: %w", err)
			}
			return nil
		}); err != nil {
			return 0, nil, fmt.Errorf("rotate: %w", err)
		}
		events = append(events, ev)
	}
	return next, events, nil
}

// IngestGrant unwraps a device.key.grant payload addressed to the local device
// and persists the WCK under its (epoch, kid) with origin 'grant' (P4-SEC-07 /
// P6-SEC-02). Grants for other recipients are a no-op. The kid is computed
// from the unwrapped bytes; a grant whose carried kid disagrees is rejected
// (forged or corrupted). Because keys are addressed by (epoch, kid), ingesting
// never overwrites an existing key (P6-SEC-01b): the same key is idempotent,
// and a different key at the same epoch lands in its own slot, coexisting with
// any local self-mint. The EncryptedHub decorator calls this for every
// verified grant seen during a Pull, in HLC order, before decrypting the rest
// of the batch, so a newly-approved device obtains its WCKs before decrypting
// history.
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
	kid := dssync.KIDForWCK(wck)
	if grant.KID != "" && grant.KID != kid {
		return fmt.Errorf("ingest grant epoch %d: carried kid %q does not match unwrapped key kid %q (forged or corrupted grant)", grant.Epoch, grant.KID, kid)
	}
	// Defense in depth (P6-SEC-01b): a custody slot's bytes are content-bound to
	// its kid, so an existing (epoch, kid) slot may only ever be re-written with
	// identical bytes. With a full-digest kid a mismatch would take a sha256
	// collision; refusing is free.
	if existing, lerr := k.KeyStore.LoadWCK(ctx, k.workspaceID, grant.Epoch, kid); lerr == nil && !bytes.Equal(existing, wck) {
		return fmt.Errorf("ingest grant epoch %d: held key with kid %s has different bytes (refusing custody overwrite)", grant.Epoch, kid)
	}
	if err := k.KeyStore.StoreWCK(ctx, k.workspaceID, grant.Epoch, kid, wck); err != nil {
		return err
	}
	if err := k.Store.RecordKeyEpoch(ctx, grant.Epoch, kid, originGrant); err != nil {
		return err
	}
	k.cacheWCK(grant.Epoch, kid, originGrant, wck)
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

func (k *Keyring) cacheWCK(epoch int64, kid, origin string, wck []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cache == nil {
		k.cache = map[keyID]cachedKey{}
	}
	id := keyID{epoch: epoch, kid: kid}
	if existing, ok := k.cache[id]; ok {
		// Same (epoch, kid) is the same key bytes (kid is content-derived);
		// only upgrade the origin rank (e.g. a self-minted key later blessed
		// by a fleet grant). The DB row keeps its original origin (INSERT OR
		// IGNORE) — the divergence is intentional and unobservable, since it
		// only occurs for identical bytes, never between distinct keys.
		if originRank(origin) > originRank(existing.origin) {
			existing.origin = origin
			k.cache[id] = existing
		}
		return
	}
	k.cache[id] = cachedKey{wck: append([]byte(nil), wck...), origin: origin}
}

func (k *Keyring) cached(epoch int64, kid string) ([]byte, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	entry, ok := k.cache[keyID{epoch: epoch, kid: kid}]
	if !ok {
		return nil, false
	}
	return entry.wck, true
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
