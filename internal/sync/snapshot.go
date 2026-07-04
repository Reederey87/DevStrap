// snapshot.go is the wire format for full-state snapshot exchange
// (P4-SYNC-02 / P4-HUB-11): a sealed, content-addressed snapshot object plus a
// signed retention manifest that names it and carries the hub's per-device
// retention floors (P6-HUB-04).
//
// Trust model: the snapshot OBJECT is not signed directly — one Ed25519
// signature on the retention MANIFEST covers both, because the manifest names
// the snapshot object by sha256 and the object's plaintext is bound to its
// carrier fields by the AEAD additional data. An importer therefore verifies,
// in order: (1) the manifest signature against a locally pinned approved
// device (fail-closed, no pre-enrollment window — a snapshot import is
// wholesale state replacement, so unlike event verification there is no
// bootstrap acceptance path); (2) the fetched object's sha256 against the
// manifest; (3) the AEAD open with the carrier-derived AAD. A malicious hub
// can withhold or garble either object (forced refusal, a DoS) but can never
// inject state.
//
// Encryption deliberately mirrors eventcrypt.go's enc.v2 (XChaCha20-Poly1305
// under the per-epoch Workspace Content Key) rather than the age blob plane:
// the snapshot has exactly the event log's sensitivity and audience (every
// WCK-granted device), WCK grants already solve group access with no per-device
// re-wrap on enrollment, and sealing under the CURRENT epoch makes each
// compaction a natural retirement boundary for old-epoch ciphertext (a fresh
// joiner never needs a retired epoch's key).
package sync

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
)

// snapshotVersion is the single supported snapshot document/envelope version.
const snapshotVersion = 1

// Signature domains for the snapshot-exchange plane. devstrap:snapshot:v1 is
// RESERVED and unused in v1 — snapshot-object integrity comes from the
// sha256-from-signed-manifest chain plus the AEAD — and is named here so the
// domain string can never be silently reused for something else.
const (
	// RetentionSignatureDomain signs the retention manifest (P6-HUB-04).
	RetentionSignatureDomain = "devstrap:retention:v1"
	// AckSignatureDomain signs per-device sync ack markers (P4-SYNC-06; the
	// marker struct ships with the tombstone-GC work).
	AckSignatureDomain = "devstrap:ack:v1"
	// SnapshotSignatureDomainReserved is reserved for a future directly-signed
	// snapshot object. Do not reuse for anything else.
	SnapshotSignatureDomainReserved = "devstrap:snapshot:v1"
)

// ErrSnapshotVerification signals that a retention manifest or snapshot object
// failed trust verification (unknown/unapproved producer, bad signature,
// sha256 mismatch, or AEAD failure on every held candidate key). Fail-closed:
// nothing is imported from an unverified snapshot; the caller keeps its
// current state and cursors.
var ErrSnapshotVerification = errors.New("snapshot verification failed")

// ErrRetentionNotFound signals that the hub has no retention manifest (no
// compaction has ever run). It wraps nothing deliberately — callers treat it
// as "no floor" on the pull path and "first write" on the compact path.
var ErrRetentionNotFound = errors.New("no retention manifest on hub")

// ErrRetentionConflict signals that a conditional PutRetention lost a
// compare-and-swap race: the manifest changed between the caller's read and
// its write. The caller must re-read, re-derive floors, and retry or refuse.
var ErrRetentionConflict = errors.New("retention manifest changed concurrently")

// ErrRetentionRollback signals that the hub served a retention manifest whose
// floor for some device is LOWER than a floor this device has already
// verified. Floors are monotonic by protocol; a rollback is a hub attempting
// to hide (or accidentally losing) its own truncation history.
var ErrRetentionRollback = errors.New("retention floor rollback")

// Snapshot is the plaintext full-state snapshot document (snapshot.v1): the
// derived namespace map with source-event coordinates for LWW convergence,
// surviving tombstones, per-device chain anchors, and the per-device floor the
// producing compactor is about to publish. Device-local state (conflicts,
// key_grant_waits, sync_skipped_events) is deliberately excluded, and grant
// events are NOT embedded — device approval re-grants all held epochs as fresh
// events, which always land above the floor.
type Snapshot struct {
	V           int                 `json:"v"`
	WorkspaceID string              `json:"workspace_id"`
	ProducedBy  string              `json:"produced_by"`
	HLC         int64               `json:"hlc"`
	Epoch       int64               `json:"epoch"`
	KID         string              `json:"kid"`
	Floor       Cursor              `json:"floor"`
	Anchors     []ChainAnchor       `json:"anchors,omitempty"`
	Entries     []SnapshotEntry     `json:"entries,omitempty"`
	Tombstones  []SnapshotTombstone `json:"tombstones,omitempty"`
}

// ChainAnchor is one origin device's hash-chain anchor: the content hash of
// the LAST event covered by the snapshot for that device (seq = floor-1).
// A snapshot-bootstrapped device has no event rows below the floor, so the
// prev-hash verification of the first post-floor event needs this anchor as
// its fallback predecessor.
type ChainAnchor struct {
	DeviceID    string `json:"device_id"`
	Seq         int64  `json:"seq"`
	ContentHash string `json:"content_hash"`
	HLC         int64  `json:"hlc"`
}

// SnapshotEntry is one active namespace-map row with the source-event
// coordinates (HLC, device, event id) that make import a pure LWW merge.
type SnapshotEntry struct {
	Path                  string         `json:"path"`
	PathKey               string         `json:"path_key"`
	Type                  string         `json:"type"`
	DisplayName           string         `json:"display_name,omitempty"`
	MaterializationPolicy string         `json:"materialization_policy,omitempty"`
	Status                string         `json:"status"`
	SourceEventHLC        int64          `json:"source_event_hlc"`
	SourceEventDeviceID   string         `json:"source_event_device_id"`
	SourceEventID         string         `json:"source_event_id"`
	Git                   *SnapshotGit   `json:"git,omitempty"`
	Draft                 *SnapshotDraft `json:"draft,omitempty"`
}

// SnapshotGit mirrors the git_repos row attached to a namespace entry.
type SnapshotGit struct {
	RemoteURL     string `json:"remote_url"`
	RemoteKey     string `json:"remote_key"`
	DefaultBranch string `json:"default_branch,omitempty"`
	CloneFilter   string `json:"clone_filter,omitempty"`
	SparseConfig  string `json:"sparse_config,omitempty"`
	LFSPolicy     string `json:"lfs_policy,omitempty"`
	ForgeKind     string `json:"forge_kind,omitempty"`
}

// SnapshotDraft is the LATEST draft-bundle pointer for a draft project, so a
// bootstrapping device can fetch the age-encrypted bundle blob and hub gc's
// mark set stays complete after the events that carried it are compacted.
type SnapshotDraft struct {
	BlobRef             string `json:"blob_ref"`
	ByteSize            int64  `json:"byte_size"`
	FileCount           int64  `json:"file_count"`
	SourceEventHLC      int64  `json:"source_event_hlc"`
	SourceEventDeviceID string `json:"source_event_device_id"`
	SourceEventID       string `json:"source_event_id"`
}

// SnapshotTombstone is one surviving deleted entry, kept so a stale add
// cannot resurrect a deleted path on a bootstrapped device.
type SnapshotTombstone struct {
	PathKey             string `json:"path_key"`
	TombstoneHLC        int64  `json:"tombstone_hlc"`
	SourceEventDeviceID string `json:"source_event_device_id,omitempty"`
	SourceEventID       string `json:"source_event_id,omitempty"`
}

// snapshotEnvelope is the sealed on-hub snapshot object: the AEAD ciphertext
// of the Snapshot JSON plus the plaintext carrier fields the AAD binds. Like
// enc.v2, the kid FIELD is an unauthenticated routing hint; the SEALING key's
// kid is bound into the AAD, so a hub-side relabel costs nothing while a
// carrier mutation is an authentication failure.
type snapshotEnvelope struct {
	V           int    `json:"v"`
	WorkspaceID string `json:"workspace_id"`
	ProducedBy  string `json:"produced_by"`
	HLC         int64  `json:"hlc"`
	Epoch       int64  `json:"epoch"`
	KID         string `json:"kid"`
	CT          string `json:"ct"`
}

// SnapshotEnvelopeInfo is the parsed plaintext carrier of a sealed snapshot
// object, exposed so the importer can select WCK candidates by (epoch, kid)
// before attempting to unseal.
type SnapshotEnvelopeInfo struct {
	WorkspaceID string
	ProducedBy  string
	HLC         int64
	Epoch       int64
	KID         string
}

// SealSnapshot seals a snapshot document under the WCK for the given epoch and
// returns the content-addressed object: the envelope bytes plus their sha256
// hex (the object key and the manifest's snapshot reference). The snapshot's
// own V/Epoch/KID are stamped here so document and envelope can never
// disagree.
func SealSnapshot(snap Snapshot, wck []byte, epoch int64) (obj []byte, sha256Hex string, err error) {
	if len(wck) != wckSize {
		return nil, "", fmt.Errorf("seal snapshot: wck length = %d, want %d", len(wck), wckSize)
	}
	kid := KIDForWCK(wck)
	snap.V = snapshotVersion
	snap.Epoch = epoch
	snap.KID = kid
	plaintext, err := json.Marshal(snap)
	if err != nil {
		return nil, "", fmt.Errorf("seal snapshot: marshal document: %w", err)
	}
	aead, err := aeadFor(wck)
	if err != nil {
		return nil, "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", fmt.Errorf("seal snapshot: generate nonce: %w", err)
	}
	aad := snapshotAAD(snap.WorkspaceID, snap.ProducedBy, kid, snap.HLC, epoch)
	ct := aead.Seal(nil, nonce, plaintext, aad)
	envelope := snapshotEnvelope{
		V:           snapshotVersion,
		WorkspaceID: snap.WorkspaceID,
		ProducedBy:  snap.ProducedBy,
		HLC:         snap.HLC,
		Epoch:       epoch,
		KID:         kid,
		CT:          base64.StdEncoding.EncodeToString(append(nonce, ct...)),
	}
	obj, err = json.Marshal(envelope)
	if err != nil {
		return nil, "", fmt.Errorf("seal snapshot: marshal envelope: %w", err)
	}
	sum := sha256.Sum256(obj)
	return obj, hex.EncodeToString(sum[:]), nil
}

// ParseSnapshotEnvelope decodes a sealed snapshot object's plaintext carrier
// without decrypting, so the importer can select WCK candidates by
// (epoch, kid) first. The version check is fail-closed.
func ParseSnapshotEnvelope(obj []byte) (SnapshotEnvelopeInfo, error) {
	var env snapshotEnvelope
	if err := json.Unmarshal(obj, &env); err != nil {
		return SnapshotEnvelopeInfo{}, fmt.Errorf("%w: parse snapshot envelope: %w", ErrSnapshotVerification, err)
	}
	if env.V != snapshotVersion {
		return SnapshotEnvelopeInfo{}, fmt.Errorf("%w: snapshot envelope version %d, want %d", ErrSnapshotVerification, env.V, snapshotVersion)
	}
	return SnapshotEnvelopeInfo{
		WorkspaceID: env.WorkspaceID,
		ProducedBy:  env.ProducedBy,
		HLC:         env.HLC,
		Epoch:       env.Epoch,
		KID:         env.KID,
	}, nil
}

// UnsealSnapshot opens a sealed snapshot object with one candidate WCK. The
// AAD is re-derived from the envelope's carrier fields and the CANDIDATE KEY's
// kid — never the envelope's unauthenticated kid hint — so a mutation of any
// carrier field, or a wrong candidate key, is an authentication failure. The
// caller loops held candidates for the envelope's (epoch, kid) exactly like
// EncryptedHub.Pull does for events.
func UnsealSnapshot(obj []byte, wck []byte) (Snapshot, error) {
	var env snapshotEnvelope
	if err := json.Unmarshal(obj, &env); err != nil {
		return Snapshot{}, fmt.Errorf("%w: parse snapshot envelope: %w", ErrSnapshotVerification, err)
	}
	if env.V != snapshotVersion {
		return Snapshot{}, fmt.Errorf("%w: snapshot envelope version %d, want %d", ErrSnapshotVerification, env.V, snapshotVersion)
	}
	if len(wck) != wckSize {
		return Snapshot{}, fmt.Errorf("unseal snapshot: wck length = %d, want %d", len(wck), wckSize)
	}
	aead, err := aeadFor(wck)
	if err != nil {
		return Snapshot{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(env.CT)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: decode snapshot ciphertext: %w", ErrSnapshotVerification, err)
	}
	nonceSize := aead.NonceSize()
	if len(raw) < nonceSize+aead.Overhead() {
		return Snapshot{}, fmt.Errorf("%w: snapshot ciphertext too short", ErrSnapshotVerification)
	}
	aad := snapshotAAD(env.WorkspaceID, env.ProducedBy, KIDForWCK(wck), env.HLC, env.Epoch)
	plaintext, err := aead.Open(nil, raw[:nonceSize], raw[nonceSize:], aad)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: snapshot aead open: %w", ErrSnapshotVerification, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(plaintext, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("%w: unmarshal snapshot document: %w", ErrSnapshotVerification, err)
	}
	if snap.V != snapshotVersion {
		return Snapshot{}, fmt.Errorf("%w: snapshot document version %d, want %d", ErrSnapshotVerification, snap.V, snapshotVersion)
	}
	return snap, nil
}

// snapshotAAD binds the sealed snapshot to its plaintext carrier: workspace,
// producing device, sealing key's kid, producer HLC, and epoch. Mirrors
// envelopeAAD's injective length-prefixed encoding (P6-SYNC-04 posture).
func snapshotAAD(workspaceID, producedBy, kid string, hlc, epoch int64) []byte {
	aad := make([]byte, 0, 12+len(workspaceID)+len(producedBy)+len(kid)+16)
	aad = appendLenPrefixed(aad, workspaceID)
	aad = appendLenPrefixed(aad, producedBy)
	aad = appendLenPrefixed(aad, kid)
	var nb [8]byte
	// HLC and epoch are always positive (enforced at stamping/bootstrap), so
	// the int64→uint64 casts cannot wrap.
	binary.BigEndian.PutUint64(nb[:], uint64(hlc)) //nolint:gosec // G115: hlc is always positive
	aad = append(aad, nb[:]...)
	binary.BigEndian.PutUint64(nb[:], uint64(epoch)) //nolint:gosec // G115: epoch is always positive
	aad = append(aad, nb[:]...)
	return aad
}

// RetentionSnapshotRef names the current snapshot object inside the manifest.
type RetentionSnapshotRef struct {
	// Fields are declared in alphabetical json-tag order so the marshaled
	// signature payload is canonical (matches eventSignaturePayloadV2 style).
	Epoch      int64  `json:"epoch"`
	HLC        int64  `json:"hlc"`
	KID        string `json:"kid"`
	ProducedBy string `json:"produced_by"`
	SHA256     string `json:"sha256"`
}

// RetentionManifest is the hub's single signed retention marker
// (workspaces/<ws>/meta/retention.json, P6-HUB-04): the per-device retention
// floors plus the snapshot object that covers everything below them. It is
// written with compare-and-swap so concurrent compactors cannot lose updates,
// and chained via PrevSHA256 for audit. One manifest with a floor MAP (rather
// than one marker per device stream) keeps the multi-device floor update
// atomic.
type RetentionManifest struct {
	V           int                  `json:"v"`
	WorkspaceID string               `json:"workspace_id"`
	Floors      map[string]int64     `json:"floors"`
	Snapshot    RetentionSnapshotRef `json:"snapshot"`
	ProducedBy  string               `json:"produced_by"`
	ProducedAt  int64                `json:"produced_at_hlc"`
	PrevSHA256  string               `json:"prev_sha256"`
	Sig         string               `json:"sig"`
}

// retentionSignaturePayload is the canonical signed form of a manifest:
// every field except Sig, declared in alphabetical json-tag order (Go
// marshals maps with sorted keys, so Floors is canonical too).
type retentionSignaturePayload struct {
	Floors      map[string]int64     `json:"floors"`
	PrevSHA256  string               `json:"prev_sha256"`
	ProducedAt  int64                `json:"produced_at_hlc"`
	ProducedBy  string               `json:"produced_by"`
	Snapshot    RetentionSnapshotRef `json:"snapshot"`
	V           int                  `json:"v"`
	WorkspaceID string               `json:"workspace_id"`
}

// RetentionSignaturePayload returns the canonical bytes a manifest signature
// covers.
func RetentionSignaturePayload(m RetentionManifest) []byte {
	raw, err := json.Marshal(retentionSignaturePayload{
		Floors:      m.Floors,
		PrevSHA256:  m.PrevSHA256,
		ProducedAt:  m.ProducedAt,
		ProducedBy:  m.ProducedBy,
		Snapshot:    m.Snapshot,
		V:           m.V,
		WorkspaceID: m.WorkspaceID,
	})
	if err != nil {
		// Marshaling a struct of strings/ints/maps cannot fail.
		panic(err)
	}
	return raw
}

// SignRetentionManifest stamps V and Sig on the manifest using the producing
// device's private signing key (domain devstrap:retention:v1).
func SignRetentionManifest(m *RetentionManifest, privateSigningKey string) error {
	m.V = snapshotVersion
	sig, err := devicekeys.Sign(privateSigningKey, RetentionSignatureDomain, RetentionSignaturePayload(*m))
	if err != nil {
		return fmt.Errorf("sign retention manifest: %w", err)
	}
	m.Sig = sig
	return nil
}

// VerifyRetentionManifest checks a manifest's signature against the claimed
// producer's public signing key. The caller is responsible for the TRUST
// decision (the key must belong to a locally pinned, approved device) —
// this only proves the manifest bytes were signed by that key.
func VerifyRetentionManifest(m RetentionManifest, publicSigningKey string) error {
	if m.V != snapshotVersion {
		return fmt.Errorf("%w: retention manifest version %d, want %d", ErrSnapshotVerification, m.V, snapshotVersion)
	}
	if m.Sig == "" {
		return fmt.Errorf("%w: retention manifest is unsigned", ErrSnapshotVerification)
	}
	if err := devicekeys.Verify(publicSigningKey, m.Sig, RetentionSignatureDomain, RetentionSignaturePayload(m)); err != nil {
		return fmt.Errorf("%w: retention manifest signature: %w", ErrSnapshotVerification, err)
	}
	return nil
}

// ParseRetentionManifest decodes raw manifest bytes WITHOUT verifying the
// signature. Backends use it on the pull path to read floors (they hold no
// device registry; an unverified floor can only FORCE the snapshot path,
// where the fail-closed verification lives), and the importer uses it before
// verification.
func ParseRetentionManifest(raw []byte) (RetentionManifest, error) {
	var m RetentionManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return RetentionManifest{}, fmt.Errorf("parse retention manifest: %w", err)
	}
	return m, nil
}

// ParseRetentionFloors extracts just the per-device floors from raw manifest
// bytes, tolerating nothing else: any parse failure returns an error so the
// pull path treats a garbled marker as a hard error rather than "no floor"
// (fail closed — a hub that can garble the marker into "no floor" could
// otherwise hide its own truncation).
func ParseRetentionFloors(raw []byte) (map[string]int64, error) {
	m, err := ParseRetentionManifest(raw)
	if err != nil {
		return nil, err
	}
	return m.Floors, nil
}
