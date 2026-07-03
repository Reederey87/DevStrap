package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/state"
)

// WorkspaceKeyring is the key-epoch abstraction the EncryptedHub decorator uses
// to encrypt outgoing events and decrypt incoming ones (P4-SEC-07). It is
// defined here so the decorator depends on the interface, not the concrete
// keyring, keeping keychain/platform/state dependencies out of internal/sync.
// The concrete implementation lives in internal/workspacekeys.
type WorkspaceKeyring interface {
	// PushKey returns the key new outgoing events encrypt under: the active
	// epoch, its kid (KIDForWCK), and the WCK bytes. epoch == 0 with a nil
	// error means this device holds no workspace key yet (a joiner awaiting
	// its grant). When several keys coexist at the active epoch (P6-SEC-02
	// collision coexistence), the fleet key — grant origin — is preferred over
	// a local self-mint so a legacy joiner converges onto the founder's key.
	// Call Prime first.
	PushKey(ctx context.Context) (epoch int64, kid string, wck []byte, err error)
	// Prime loads every held key's WCK into memory so WCK lookups during a
	// Pull are pure and context-free. EncryptedHub.Pull calls Prime before
	// ingesting in-batch grants and decrypting.
	Prime(ctx context.Context) error
	// WCKCandidates returns the candidate WCKs for decrypting an envelope
	// addressed to (epoch, kid), from the in-memory cache. kid != "" selects
	// the exact key (zero or one candidates); kid == "" (a legacy pre-kid
	// envelope) returns every held key at the epoch, which the caller tries in
	// order — the AEAD authenticates, so a wrong candidate just fails. An
	// empty result means no candidate is held (the grant has not arrived).
	// Call Prime (and ingest in-batch grants) first.
	WCKCandidates(epoch int64, kid string) [][]byte
	// IngestGrant unwraps a device.key.grant addressed to the local device and
	// persists the WCK under its (epoch, kid). Grants for other recipients are
	// a no-op. A grant whose carried kid does not match its unwrapped bytes is
	// rejected, and an already-held (epoch, kid) is never overwritten
	// (P6-SEC-01b/c).
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
	// Stats, when non-nil, is populated by Pull with observability about the
	// raw batch (P6-SEC-02). RawSeen is the number of objects the backend
	// returned before any decrypt/skip/truncate — the founder/join gate uses
	// it to distinguish a genuinely empty hub (found here) from a populated
	// hub whose events this device cannot yet decrypt (a joiner awaiting its
	// grant, which must NOT self-found). It is also the seam later cursor and
	// GC work (P6-HUB-01/SEC-03/SYNC-02) will read.
	Stats *PullStats
	// MissingKeyWait records that an event sealed under (epoch, kid) was found
	// with no held key, and returns the STABLE first-seen time of that missing
	// key — the start of its grace window (P6-SEC-03). hubFromOptions wires it
	// to (*state.Store).NoteMissingKeyGrant. nil disables grace expiry
	// entirely: a missing key truncates forever (the pre-P6-SEC-03 behavior,
	// kept for unit tests that exercise the truncate contract in isolation).
	MissingKeyWait func(ctx context.Context, epoch int64, kid string) (time.Time, error)
	// GraceWindow bounds how long a missing (epoch, kid) may keep truncating
	// the pull before its events are handed to the undecryptable quarantine so
	// the cursor can advance (P6-SEC-03). Within the window the grant is
	// presumed in flight (truncate = retry next sync); past it the wedge is
	// treated as permanent-until-regrant: the still-encrypted carrier is
	// forwarded, ApplyEvents quarantines it, and a later grant recovers it via
	// ReplayUndecryptableConflicts. Zero means expire immediately (quarantine
	// on first sight); only meaningful when MissingKeyWait is non-nil.
	GraceWindow time.Duration
}

// PullStats reports what a single EncryptedHub.Pull observed. Fields are set
// only when EncryptedHub.Stats is non-nil, and reset at the start of every
// Pull so a caller always reads the latest cycle.
type PullStats struct {
	// RawSeen is the count of objects the backend returned for this pull,
	// before decryption, grant ingestion, skipping, or truncation.
	RawSeen int
	// Truncated is the count of raw events deferred by an epoch/kid truncate
	// (grant not yet held): the tail of the batch this device could not read
	// yet. Non-zero means this device's view of the log is INCOMPLETE, so
	// consumers deriving a mark set from local state (hub gc, P6-HUB-01) must
	// refuse to sweep.
	Truncated int
	// Skipped is the count of events dropped in the decrypt pass (malformed
	// envelope, retired enc.v1 traffic, anti-downgrade plaintext). Like
	// Truncated, non-zero means the local replica may be missing references
	// other devices still rely on.
	Skipped int
	// Undecryptable is the count of enc.v2 events that failed AEAD
	// authentication on every held candidate key (corruption, forgery, or a
	// hub-side carrier mutation — P6-SYNC-04). These are NOT dropped: the
	// still-encrypted carrier is forwarded in the returned batch so
	// ApplyEvents records a permanent undecryptable quarantine conflict and
	// the cursor advances past it (no silent loss, no wedge).
	Undecryptable int
}

// Push envelope-encrypts every non-grant event under the current epoch's WCK
// and forwards the carrier events to the backend. Grant events pass through
// unchanged. The carrier preserves ID/DeviceID/Seq/HLC/DeviceSig so hub
// ordering, dedup, and signature verification are byte-for-byte unchanged.
func (h EncryptedHub) Push(ctx context.Context, events []state.Event) error {
	// Prime the cache so PushKey can select from every held key. Prime is
	// idempotent and only loads held keys that are not yet cached.
	if err := h.Keyring.Prime(ctx); err != nil {
		return fmt.Errorf("encrypted hub push: prime keyring: %w", err)
	}
	epoch, _, wck, err := h.Keyring.PushKey(ctx)
	if err != nil {
		return fmt.Errorf("encrypted hub push: select push key: %w", err)
	}
	if epoch == 0 {
		return fmt.Errorf("%w: awaiting workspace key grant (approve this device from an existing device, or sync against an empty hub to found the workspace)", ErrMissingWorkspaceKey)
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
// in HLC order, then decrypts enc.v2 envelopes back to plaintext.
//
// The hub is untrusted (zero-knowledge), so a single non-conforming object must
// never be able to wedge sync. Pull therefore degrades instead of aborting the
// whole batch:
//
//   - Missing (epoch, kid) key: the grant for this event's key has not
//     propagated yet (a missing epoch, or a kid at a held epoch that this
//     device does not hold — the P6-SEC-02 collision case). Pull TRUNCATES the
//     batch here — it returns the decryptable prefix so it applies and the
//     caller advances the cursor up to (but not past) this event, then retries
//     from here on the next sync once the grant arrives.
//     Truncating (not skipping) is required so a legitimately-decryptable-later
//     event is never permanently stranded by the cursor jumping over it.
//   - Held-epoch AEAD failure (corruption, forgery, or a hub-side carrier
//     mutation — P6-SYNC-04): the event can never be decrypted by this device,
//     but silently skipping it would be silent permanent loss with no operator
//     signal. Pull FORWARDS the still-encrypted carrier so ApplyEvents records
//     a permanent undecryptable quarantine conflict (surfaced by `conflicts
//     list`, blocking `hub gc`) and advances the cursor past it — visible
//     refusal without a wedge.
//   - A malformed/unknown envelope, retired enc.v1 traffic, or a non-grant
//     plaintext event (a downgrade attempt or a pre-envelope legacy event):
//     Pull SKIPS it with a loud warning and continues (P6-SYNC-02 tracks
//     promoting these skip classes to first-class signals). The event is never
//     applied — no unauthenticated data enters the log — but one bad object
//     cannot brick the device.
func (h EncryptedHub) Pull(ctx context.Context, afterHLC int64) ([]state.Event, error) {
	raw, err := h.Hub.Pull(ctx, afterHLC)
	if err != nil {
		return nil, err
	}
	if h.Stats != nil {
		*h.Stats = PullStats{RawSeen: len(raw)}
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
	// Second pass: decrypt enc.v2, passthrough grants, forward undecryptable
	// carriers for quarantine, skip retired/malformed traffic, and truncate at
	// the first not-yet-granted epoch.
	out := make([]state.Event, 0, len(raw))
	for i, event := range raw {
		switch event.Type {
		case EventDeviceKeyGranted:
			out = append(out, event)
		case EventEncryptedV1:
			// Retired enc.v1 traffic: written before the enc.v2 wire break
			// (P6-SYNC-04) bound the full carrier into the AAD. There is no v1
			// decrypt path — the break was taken while every hub was a
			// disposable spike. Refuse loudly; the remedy is re-founding the
			// workspace on a fresh hub (or fresh bucket/prefix).
			logging.Logger(ctx).Warn("encrypted hub pull: skipping retired enc.v1 event; this hub predates the enc.v2 wire break — re-found the workspace on a fresh hub",
				"event_id", event.ID)
			if h.Stats != nil {
				h.Stats.Skipped++
			}
			continue
		case EventEncryptedV2:
			env, err := ParseEncryptedEnvelope(event)
			if err != nil {
				// Malformed envelope or unknown version: an untrusted hub can
				// serve junk, and a newer client may write a version this build
				// cannot read. Refuse it, but skip rather than wedge.
				logging.Logger(ctx).Warn("encrypted hub pull: skipping undecodable event",
					"event_id", event.ID, "err", err.Error())
				if h.Stats != nil {
					h.Stats.Skipped++
				}
				continue
			}
			// The envelope kid FIELD is an unauthenticated routing hint (the
			// sealing key's kid is bound into the AAD instead — enc.v2,
			// P6-SYNC-04), so it selects the candidate ORDER, never the
			// candidate SET: the exact match is tried first, then every other
			// held key at the epoch. Trusting the field to narrow the set
			// would let a hostile hub relabel a genuinely decryptable event's
			// kid to an unheld value and wedge the device forever on the
			// truncate below, even though it holds the decrypting key
			// (post-#33 review, fable-5). The AEAD authenticates under the
			// candidate key's own kid, so a wrong candidate just fails and
			// applying a kid-relabeled real event is safe (ContentHash/
			// DeviceSig are still verified in insertEvent).
			var exact [][]byte
			if env.KID != "" {
				exact = h.Keyring.WCKCandidates(env.Epoch, env.KID)
			}
			allHeld := h.Keyring.WCKCandidates(env.Epoch, "")
			if len(allHeld) == 0 {
				// No key at this epoch at all: the grant has not arrived.
				// Within the grace window, truncate: return the decryptable
				// prefix and stop, so the cursor advances only up to here and
				// the next sync retries from this event once granted. Past the
				// window (P6-SEC-03), forward the still-encrypted carrier for a
				// permanent undecryptable quarantine instead — the cursor can
				// then advance and later held-epoch events still apply, and a
				// grant that eventually arrives recovers the carrier via
				// ReplayUndecryptableConflicts.
				if h.missingKeyGraceExpired(ctx, env.Epoch, env.KID) {
					logging.Logger(ctx).Warn("encrypted hub pull: key grant grace expired; forwarding never-granted event for quarantine",
						"epoch", env.Epoch, "kid", env.KID, "event_id", event.ID)
					if h.Stats != nil {
						h.Stats.Undecryptable++
					}
					out = append(out, event)
					continue
				}
				logging.Logger(ctx).Info("encrypted hub pull: awaiting workspace key grant; deferring remaining events",
					"epoch", env.Epoch, "kid", env.KID, "event_id", event.ID)
				if h.Stats != nil {
					h.Stats.Truncated = len(raw) - i
				}
				return out, nil
			}
			var restored state.Event
			decErr := error(nil)
			decrypted := false
			for _, wck := range append(exact, allHeld...) {
				restored, decErr = DecryptEvent(event, wck)
				if decErr == nil {
					decrypted = true
					break
				}
			}
			if !decrypted {
				if env.KID != "" && len(exact) == 0 {
					// An unheld kid at a held epoch and none of our keys open
					// it: this is the fleet-key-vs-self-mint collision
					// (P6-SEC-02) — the grant for that key may still arrive.
					// Within the grace window, truncate (defer), never skip, so
					// the event is retried once granted instead of being
					// permanently jumped. Past the window (P6-SEC-03), hand it
					// to the quarantine like the AEAD-failure branch below —
					// this is also the forged-kid stall primitive (a hostile
					// hub naming a random kid at a held epoch), which the
					// grace bound turns from a forever-wedge into a bounded
					// delay. The wait clock is keyed per EPOCH (the store MINs
					// first-seen across kids), so relabeling the
					// unauthenticated kid hint on every pull cannot restart it.
					if h.missingKeyGraceExpired(ctx, env.Epoch, env.KID) {
						logging.Logger(ctx).Warn("encrypted hub pull: key grant grace expired; forwarding never-granted event for quarantine",
							"epoch", env.Epoch, "kid", env.KID, "event_id", event.ID)
						if h.Stats != nil {
							h.Stats.Undecryptable++
						}
						out = append(out, event)
						continue
					}
					logging.Logger(ctx).Info("encrypted hub pull: awaiting workspace key grant; deferring remaining events",
						"epoch", env.Epoch, "kid", env.KID, "event_id", event.ID)
					if h.Stats != nil {
						h.Stats.Truncated = len(raw) - i
					}
					return out, nil
				}
				// The envelope names a key we hold (or is a kid-less envelope)
				// and authentication failed on every held key: corruption,
				// forgery, or a hub-side carrier mutation (P6-SYNC-04). The
				// event can never be decrypted by this device — but silently
				// dropping it would be silent permanent loss. Forward the
				// still-encrypted carrier so ApplyEvents quarantines it as a
				// permanent undecryptable conflict: visible in `conflicts
				// list`, blocks `hub gc`, and the cursor advances past it so
				// one poisoned object cannot wedge sync. Unauthenticated data
				// still never enters the log (the carrier is quarantined, not
				// applied).
				logging.Logger(ctx).Warn("encrypted hub pull: forwarding undecryptable event for quarantine",
					"epoch", env.Epoch, "kid", env.KID, "event_id", event.ID, "err", decErr.Error())
				if h.Stats != nil {
					h.Stats.Undecryptable++
				}
				out = append(out, event)
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
			if h.Stats != nil {
				h.Stats.Skipped++
			}
			continue
		}
	}
	return out, nil
}

// missingKeyGraceExpired reports whether the grace window for a missing
// (epoch, kid) has run out (P6-SEC-03). It records the sighting through the
// MissingKeyWait seam (which returns the stable first-seen time) and compares
// against GraceWindow. Any error — and a nil seam — degrades to "not expired",
// i.e. the legacy truncate-and-retry behavior: failing toward the truncate is
// safe (no data is quarantined), just un-bounded until the store recovers.
func (h EncryptedHub) missingKeyGraceExpired(ctx context.Context, epoch int64, kid string) bool {
	if h.MissingKeyWait == nil {
		return false
	}
	firstSeen, err := h.MissingKeyWait(ctx, epoch, kid)
	if err != nil {
		logging.Logger(ctx).Warn("encrypted hub pull: could not record missing key grant wait; deferring instead",
			"epoch", epoch, "kid", kid, "err", err.Error())
		return false
	}
	return time.Since(firstSeen) >= h.GraceWindow
}

// TryDecrypt attempts to restore a single enc.v2 carrier with the keys held
// NOW — the exact-kid candidate first, then every held key at the epoch (the
// same candidate policy as Pull). It primes the keyring but never truncates,
// skips, or touches Stats: it exists for the undecryptable-conflict replay
// path (P6-SYNC-04 review fix), which re-attempts quarantined carriers after
// later grants arrive.
func (h EncryptedHub) TryDecrypt(ctx context.Context, event state.Event) (state.Event, error) {
	if err := h.Keyring.Prime(ctx); err != nil {
		return state.Event{}, fmt.Errorf("encrypted hub try-decrypt: prime keyring: %w", err)
	}
	env, err := ParseEncryptedEnvelope(event)
	if err != nil {
		return state.Event{}, err
	}
	var candidates [][]byte
	if env.KID != "" {
		candidates = h.Keyring.WCKCandidates(env.Epoch, env.KID)
	}
	candidates = append(candidates, h.Keyring.WCKCandidates(env.Epoch, "")...)
	if len(candidates) == 0 {
		return state.Event{}, fmt.Errorf("%w: epoch %d", ErrMissingWorkspaceKey, env.Epoch)
	}
	var lastErr error
	for _, wck := range candidates {
		restored, decErr := DecryptEvent(event, wck)
		if decErr == nil {
			return restored, nil
		}
		lastErr = decErr
	}
	return state.Event{}, lastErr
}

// ReplayUndecryptableConflicts re-attempts every open "undecryptable"
// quarantine conflict with the keys held now (P6-SYNC-04 review fix, gpt-5.5
// Major). Without this, a hostile hub could strip or relabel the untrusted
// envelope kid on an event whose grant had not arrived yet, steering the
// defer-vs-quarantine classification into permanent quarantine — and since
// the cursor advances past quarantined events, a later legitimate grant could
// never recover it. The quarantine preserves the full carrier, so each sync
// cycle replays it: once the grant lands, the carrier decrypts, the conflict
// auto-resolves, and the restored event applies through the normal verified
// path. Genuinely corrupt carriers keep failing and their conflicts stay open
// (visible, gc-blocking, deduped — no growth, no wedge).
//
// The replay is DELIBERATELY unconditional — it never skips a conflict as
// "hopeless" based on what the envelope kid named at quarantine time. Any
// such classification reads the same attacker-controlled field that caused
// the misroute: a hub that relabels a not-yet-granted event's kid to a HELD
// kid would get it marked permanently non-replayable and re-open the exact
// loss this fix closes (post-#44 dual-review analysis). A hopeless carrier
// just fails decryption again — cheap, bounded by open conflicts.
//
// The conflict is resolved AFTER a successful apply, so a transient failure
// (DB error) between decrypt and apply leaves the conflict open and the next
// cycle retries — the resolve-then-apply window would have been a narrow
// silent-loss path (post-#44 verify, gpt-5.5). A restored event that fails
// signature verification still records a FRESH "verification" conflict while
// the "undecryptable" row is open, because the conflict dedup keys on
// (event ID, kind). A restored event whose HLC is still beyond the trusted
// receive skew is left for a later cycle (applying it would land in the
// transient skew quarantine with no carrier left to retry).
func ReplayUndecryptableConflicts(ctx context.Context, st *state.Store, h EncryptedHub) (int, error) {
	conflicts, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil {
		return 0, err
	}
	replayed := 0
	for _, c := range conflicts {
		var details eventVerificationConflictDetails
		if json.Unmarshal([]byte(c.DetailsJSON), &details) != nil || details.Kind != EventConflictKindUndecryptable {
			continue
		}
		var carrier state.Event
		if err := json.Unmarshal([]byte(details.EventJSON), &carrier); err != nil {
			logging.Logger(ctx).Warn("undecryptable replay: conflict carries unparseable event JSON",
				"conflict_id", c.ID, "event_id", details.EventID, "err", err.Error())
			continue
		}
		restored, decErr := h.TryDecrypt(ctx, carrier)
		if decErr != nil {
			continue // still undecryptable; leave the conflict open for the next cycle
		}
		// Skew guard: a restored HLC still beyond the trusted skew would be
		// TRANSIENTLY skew-quarantined by ApplyEvents — but with the
		// undecryptable row resolved there would be no carrier left to
		// retry. Defer the whole replay to a later cycle instead.
		if physical := restored.HLC >> hlcLogicalBits; physical-time.Now().UnixMilli() > defaultReceiveMaxSkew.Milliseconds() {
			logging.Logger(ctx).Warn("undecryptable replay: restored event's clock is still beyond trusted skew; deferring",
				"event_id", details.EventID, "hlc", restored.HLC)
			continue
		}
		if _, _, err := ApplyEventsWithStats(ctx, st, []state.Event{restored}); err != nil {
			return replayed, err // conflict stays open; retried next cycle
		}
		if err := st.ResolveConflict(ctx, c.ID, `{"action":"auto","reason":"carrier decrypted after a later key grant (P6-SYNC-04 replay)"}`); err != nil {
			return replayed, err
		}
		logging.Logger(ctx).Info("undecryptable replay: quarantined carrier recovered after key grant",
			"event_id", details.EventID, "device_id", details.DeviceID)
		replayed++
	}
	return replayed, nil
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

func (h EncryptedHub) ListBlobs(ctx context.Context) ([]BlobInfo, error) {
	return h.Hub.ListBlobs(ctx)
}

// Compile-time assertion that EncryptedHub satisfies Hub.
var _ Hub = EncryptedHub{}
