package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
)

// recordingHub is an in-memory Hub double that records pushed events and blobs
// so the EncryptedHub decorator can be tested without filesystem I/O.
type recordingHub struct {
	events []state.Event
	blobs  map[string][]byte
}

func (r *recordingHub) Push(_ context.Context, events []state.Event) error {
	r.events = append(r.events, events...)
	return nil
}

func (r *recordingHub) Pull(_ context.Context, after Cursor) ([]state.Event, error) {
	var out []state.Event
	for _, e := range r.events {
		if e.Seq <= 0 || e.Seq > after.After(e.DeviceID) {
			out = append(out, e)
		}
	}
	sortEvents(out)
	return out, nil
}

func (r *recordingHub) PutBlob(_ context.Context, sha string, rr io.Reader) error {
	data, err := io.ReadAll(rr)
	if err != nil {
		return err
	}
	if r.blobs == nil {
		r.blobs = map[string][]byte{}
	}
	r.blobs[sha] = data
	return nil
}

func (r *recordingHub) GetBlob(_ context.Context, sha string) (io.ReadCloser, error) {
	data, ok := r.blobs[sha]
	if !ok {
		return nil, ErrBlobNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (r *recordingHub) DeleteBlob(_ context.Context, sha string) error {
	delete(r.blobs, sha)
	return nil
}

func (r *recordingHub) ListBlobs(_ context.Context) ([]BlobInfo, error) {
	out := make([]BlobInfo, 0, len(r.blobs))
	for k := range r.blobs {
		out = append(out, BlobInfo{Key: k})
	}
	return out, nil
}

// fakeKeyring is a WorkspaceKeyring double with pre-set WCKs (one per epoch)
// and an onIngest map that installs a WCK when a grant for that epoch is
// ingested (simulating a successful age-unwrap on the recipient device).
type fakeKeyring struct {
	epoch    int64
	keys     map[int64][][]byte // several keys may coexist at one epoch (P6-SEC-02)
	onIngest map[int64][]byte
	ingested []DeviceKeyGrant
}

func (f *fakeKeyring) PushKey(context.Context) (int64, string, []byte, error) {
	ks := f.keys[f.epoch]
	if len(ks) == 0 {
		return 0, "", nil, nil
	}
	return f.epoch, KIDForWCK(ks[0]), ks[0], nil
}
func (f *fakeKeyring) Prime(context.Context) error { return nil }
func (f *fakeKeyring) WCKCandidates(epoch int64, kid string) [][]byte {
	if kid == "" {
		return f.keys[epoch]
	}
	for _, k := range f.keys[epoch] {
		if kid == KIDForWCK(k) {
			return [][]byte{k}
		}
	}
	return nil
}

// WCK is a test-only accessor (not part of WorkspaceKeyring).
func (f *fakeKeyring) WCK(epoch int64) ([]byte, bool) {
	ks := f.keys[epoch]
	if len(ks) == 0 {
		return nil, false
	}
	return ks[0], true
}

// addKey installs an additional key at an epoch (simulating a later grant).
func (f *fakeKeyring) addKey(epoch int64, wck []byte) {
	if f.keys == nil {
		f.keys = map[int64][][]byte{}
	}
	f.keys[epoch] = append(f.keys[epoch], wck)
}

func (f *fakeKeyring) IngestGrant(_ context.Context, grant DeviceKeyGrant) error {
	f.ingested = append(f.ingested, grant)
	if wck, ok := f.onIngest[grant.Epoch]; ok {
		f.addKey(grant.Epoch, wck)
	}
	return nil
}

func newFakeKeyring(t *testing.T, epochs ...int64) *fakeKeyring {
	t.Helper()
	kr := &fakeKeyring{epoch: epochs[len(epochs)-1]}
	for _, e := range epochs {
		k, _ := NewWCK()
		kr.addKey(e, k)
	}
	return kr
}

func TestEncryptedHubRoundTrip(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	back := &recordingHub{}
	hub := EncryptedHub{Hub: back, Keyring: kr}

	original := state.Event{
		ID: "evt_rt", DeviceID: "dev_a", Seq: 1, HLC: 10,
		Type:        EventProjectAdded,
		PayloadJSON: `{"path":"work/nclh/foc-models","remote_key":"github.com/org/foc-models"}`,
		ContentHash: state.ContentHash(`{"path":"work/nclh/foc-models","remote_key":"github.com/org/foc-models"}`),
		DeviceSig:   "ed25519:sig",
	}
	if err := hub.Push(ctx, []state.Event{original}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	// The backend must store only the enc.v2 carrier, not plaintext.
	if len(back.events) != 1 {
		t.Fatalf("backend stored %d events, want 1", len(back.events))
	}
	stored := back.events[0]
	if stored.Type != EventEncryptedV2 {
		t.Errorf("stored Type = %q, want %q", stored.Type, EventEncryptedV2)
	}
	if stringContains(stored.PayloadJSON, "work/nclh/foc-models") || stringContains(stored.PayloadJSON, "github.com/org/foc-models") {
		t.Errorf("backend stored plaintext payload: %s", stored.PayloadJSON)
	}
	if stored.ContentHash != "" || stored.PrevEventHash != "" {
		t.Errorf("carrier must clear hashes, got %q/%q", stored.ContentHash, stored.PrevEventHash)
	}
	if stored.ID != original.ID || stored.DeviceSig != original.DeviceSig || stored.HLC != original.HLC {
		t.Errorf("carrier changed ID/Sig/HLC")
	}

	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 1 || got[0].Type != original.Type || got[0].PayloadJSON != original.PayloadJSON || got[0].ContentHash != original.ContentHash {
		t.Fatalf("Pull did not restore original: %+v", got)
	}
	if got[0].DeviceSig != original.DeviceSig {
		t.Errorf("Pull changed DeviceSig")
	}
}

func TestEncryptedHubGrantPassthrough(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	back := &recordingHub{}
	hub := EncryptedHub{Hub: back, Keyring: kr}

	grantPayload, _ := json.Marshal(DeviceKeyGrant{Epoch: 1, Recipient: "age1example", WrappedKey: "base64wrapped"})
	grant := state.Event{ID: "evt_grant", DeviceID: "dev_a", HLC: 5, Type: EventDeviceKeyGranted, PayloadJSON: string(grantPayload), ContentHash: state.ContentHash(string(grantPayload))}
	if err := hub.Push(ctx, []state.Event{grant}); err != nil {
		t.Fatalf("Push grant: %v", err)
	}
	if back.events[0].Type != EventDeviceKeyGranted {
		t.Fatalf("grant was envelope-encrypted on the wire: %q", back.events[0].Type)
	}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 1 || got[0].Type != EventDeviceKeyGranted || got[0].PayloadJSON != string(grantPayload) {
		t.Fatalf("grant did not pass through: %+v", got)
	}
	if len(kr.ingested) != 1 || kr.ingested[0].Epoch != 1 {
		t.Fatalf("grant not ingested: %+v", kr.ingested)
	}
}

func TestPullRefusesUnverifiedGrant(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	good, err := EncryptEvent(state.Event{
		ID: "evt_good", DeviceID: "dev_a", HLC: 20,
		Type:        EventProjectAdded,
		PayloadJSON: `{"path":"work/ok"}`,
		ContentHash: state.ContentHash(`{"path":"work/ok"}`),
	}, wck1, 1)
	if err != nil {
		t.Fatal(err)
	}
	grantPayload, _ := json.Marshal(DeviceKeyGrant{Epoch: 2, Recipient: "age1self", WrappedKey: "wrapped2"})
	grant := state.Event{
		ID: "evt_grant", DeviceID: "dev_attacker", HLC: 10,
		Type:        EventDeviceKeyGranted,
		PayloadJSON: string(grantPayload),
		ContentHash: state.ContentHash(string(grantPayload)),
	}
	back := &recordingHub{events: []state.Event{good, grant}}
	hub := EncryptedHub{
		Hub:     back,
		Keyring: kr,
		Verify: func(context.Context, state.Event) error {
			return errors.New("bad carrier")
		},
	}

	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(kr.ingested) != 0 {
		t.Fatalf("ingested grants = %+v, want none", kr.ingested)
	}
	if len(got) != 2 || got[0].ID != "evt_grant" || got[1].ID != "evt_good" {
		t.Fatalf("Pull returned %+v, want grant passthrough plus other events", got)
	}
}

func TestPullIngestsVerifiedGrant(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	grantPayload, _ := json.Marshal(DeviceKeyGrant{Epoch: 2, Recipient: "age1self", WrappedKey: "wrapped2"})
	grant := state.Event{
		ID: "evt_grant", DeviceID: "dev_a", HLC: 10,
		Type:        EventDeviceKeyGranted,
		PayloadJSON: string(grantPayload),
		ContentHash: state.ContentHash(string(grantPayload)),
	}
	hub := EncryptedHub{
		Hub:     &recordingHub{events: []state.Event{grant}},
		Keyring: kr,
		Verify: func(context.Context, state.Event) error {
			return nil
		},
	}

	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 1 || got[0].ID != "evt_grant" {
		t.Fatalf("Pull returned %+v, want grant", got)
	}
	if len(kr.ingested) != 1 || kr.ingested[0].Epoch != 2 {
		t.Fatalf("ingested grants = %+v, want epoch 2", kr.ingested)
	}
}

func TestPullNilVerifierBackcompat(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	grantPayload, _ := json.Marshal(DeviceKeyGrant{Epoch: 2, Recipient: "age1self", WrappedKey: "wrapped2"})
	grant := state.Event{
		ID: "evt_grant", DeviceID: "dev_a", HLC: 10,
		Type:        EventDeviceKeyGranted,
		PayloadJSON: string(grantPayload),
		ContentHash: state.ContentHash(string(grantPayload)),
	}
	hub := EncryptedHub{Hub: &recordingHub{events: []state.Event{grant}}, Keyring: kr}

	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 1 || got[0].ID != "evt_grant" {
		t.Fatalf("Pull returned %+v, want grant", got)
	}
	if len(kr.ingested) != 1 || kr.ingested[0].Epoch != 2 {
		t.Fatalf("ingested grants = %+v, want epoch 2", kr.ingested)
	}
}

// TestEncryptedHubIngestThenDecrypt proves the two-pass Pull ordering: a grant
// for an epoch the device does not yet hold is ingested before events encrypted
// under that epoch are decrypted, within the same batch.
func TestEncryptedHubIngestThenDecrypt(t *testing.T) {
	ctx := context.Background()
	wck1, _ := NewWCK()
	wck2, _ := NewWCK()
	// New device holds epoch 1 but not epoch 2; onIngest installs WCK(2) when
	// the epoch-2 grant arrives.
	kr := &fakeKeyring{epoch: 2, keys: map[int64][][]byte{1: {wck1}}, onIngest: map[int64][]byte{2: wck2}}

	// Build a hub batch: epoch-1 event, epoch-2 grant, epoch-2 event (in HLC order).
	enc1, _ := EncryptEvent(state.Event{ID: "e1", DeviceID: "dev_a", HLC: 10, Type: EventProjectAdded, PayloadJSON: `{"path":"work/a"}`, ContentHash: state.ContentHash(`{"path":"work/a"}`)}, wck1, 1)
	grantPayload, _ := json.Marshal(DeviceKeyGrant{Epoch: 2, Recipient: "age1self", WrappedKey: "wrapped2"})
	grant := state.Event{ID: "g2", DeviceID: "dev_a", HLC: 15, Type: EventDeviceKeyGranted, PayloadJSON: string(grantPayload), ContentHash: state.ContentHash(string(grantPayload))}
	enc2, _ := EncryptEvent(state.Event{ID: "e2", DeviceID: "dev_a", HLC: 20, Type: EventProjectUpdated, PayloadJSON: `{"path":"work/b"}`, ContentHash: state.ContentHash(`{"path":"work/b"}`)}, wck2, 2)
	back := &recordingHub{events: []state.Event{enc1, grant, enc2}}
	hub := EncryptedHub{Hub: back, Keyring: kr}

	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	if got[0].Type != EventProjectAdded || got[0].PayloadJSON != `{"path":"work/a"}` {
		t.Errorf("event 1 not decrypted: %+v", got[0])
	}
	if got[1].Type != EventDeviceKeyGranted {
		t.Errorf("grant not passed through: %+v", got[1])
	}
	if got[2].Type != EventProjectUpdated || got[2].PayloadJSON != `{"path":"work/b"}` {
		t.Errorf("event 2 not decrypted after ingest: %+v", got[2])
	}
	if _, ok := kr.WCK(2); !ok {
		t.Error("epoch-2 WCK not installed after ingest")
	}
}

// TestEncryptedHubAntiDowngrade proves a non-grant plaintext event (a downgrade
// attempt or a pre-envelope legacy event) is refused — never applied — but does
// NOT wedge the pull: it is skipped and the surrounding encrypted events still
// come through, so a hostile or stale hub cannot brick sync.
func TestEncryptedHubAntiDowngrade(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	good, _ := EncryptEvent(state.Event{ID: "good", DeviceID: "dev_a", HLC: 2, Type: EventProjectAdded, PayloadJSON: `{"path":"work/ok"}`, ContentHash: state.ContentHash(`{"path":"work/ok"}`)}, wck1, 1)
	// A plaintext project event on the hub is a downgrade.
	plain := state.Event{ID: "plain", DeviceID: "dev_a", HLC: 1, Type: EventProjectAdded, PayloadJSON: `{"path":"work/attacker"}`}
	back := &recordingHub{events: []state.Event{plain, good}}
	hub := EncryptedHub{Hub: back, Keyring: kr}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull plaintext: unexpected error %v", err)
	}
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("Pull returned %+v, want only the encrypted 'good' event (plaintext skipped)", got)
	}
	for _, e := range got {
		if e.ID == "plain" {
			t.Fatal("Pull applied a plaintext downgrade event")
		}
	}
}

// TestEncryptedHubMissingEpoch proves that an event whose epoch key has not yet
// been granted TRUNCATES the batch: the decryptable prefix is returned so it can
// apply, and the not-yet-decryptable event (and anything after it) is deferred
// to a later sync — the cursor never jumps over it.
func TestEncryptedHubMissingEpoch(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1) // holds epoch 1 only
	wck1, _ := kr.WCK(1)
	prefix, _ := EncryptEvent(state.Event{ID: "e1", DeviceID: "dev_a", HLC: 1, Type: EventProjectAdded, PayloadJSON: `{"path":"work/a"}`, ContentHash: state.ContentHash(`{"path":"work/a"}`)}, wck1, 1)
	wck5, _ := NewWCK()
	future, _ := EncryptEvent(state.Event{ID: "e5", DeviceID: "dev_a", HLC: 2, Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, wck5, 5)
	back := &recordingHub{events: []state.Event{prefix, future}}
	hub := EncryptedHub{Hub: back, Keyring: kr, Stats: &PullStats{}}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull missing epoch: unexpected error %v", err)
	}
	if len(got) != 1 || got[0].ID != "e1" {
		t.Fatalf("Pull returned %+v, want the epoch-1 prefix only (epoch-5 event deferred)", got)
	}
	// P6-HUB-01: the deferred tail must be visible to callers (gc gate).
	if hub.Stats.Truncated != 1 || hub.Stats.Skipped != 0 {
		t.Fatalf("Stats = %+v, want Truncated=1 Skipped=0", *hub.Stats)
	}
}

// skipRecorder is a NoteSkipped seam double: fixed first-seen, records
// (event id, reason) sightings.
type skipRecorder struct {
	firstSeen time.Time
	sightings [][2]string
}

func (r *skipRecorder) note(_ context.Context, ev state.Event, reason string) (time.Time, error) {
	r.sightings = append(r.sightings, [2]string{ev.ID, reason})
	return r.firstSeen, nil
}

// TestEncryptedHubUnknownVersionDefersWithinGrace (P6-SYNC-02): an envelope
// version this build cannot read is DECRYPTABLE AFTER UPGRADE, so within the
// grace window it defers its origin device's tail (per-device, like a missing
// grant — the seq gap holds that device's cursor) while other devices' events
// keep flowing, and the sighting is durably recorded through the NoteSkipped
// seam.
func TestEncryptedHubUnknownVersionDefersWithinGrace(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	future, _ := EncryptEvent(state.Event{ID: "ev_future", DeviceID: "dev_a", Seq: 1, HLC: 1, Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, wck1, 1)
	var env encryptedEnvelope
	_ = json.Unmarshal([]byte(future.PayloadJSON), &env)
	env.Version = 3 // a newer client's format
	raw, _ := json.Marshal(env)
	future.PayloadJSON = string(raw)
	// dev_a's later event rides the defer; dev_b's event keeps flowing.
	tailA, _ := EncryptEvent(state.Event{ID: "ev_tail", DeviceID: "dev_a", Seq: 2, HLC: 3, Type: EventProjectAdded, PayloadJSON: `{"path":"a"}`, ContentHash: state.ContentHash(`{"path":"a"}`)}, wck1, 1)
	okB, _ := EncryptEvent(state.Event{ID: "ev_b", DeviceID: "dev_b", Seq: 1, HLC: 2, Type: EventProjectAdded, PayloadJSON: `{"path":"b"}`, ContentHash: state.ContentHash(`{"path":"b"}`)}, wck1, 1)
	rec := &skipRecorder{firstSeen: time.Now()}
	back := &recordingHub{events: []state.Event{future, okB, tailA}}
	hub := EncryptedHub{Hub: back, Keyring: kr, Stats: &PullStats{}, NoteSkipped: rec.note, GraceWindow: time.Hour}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull unknown version: unexpected error %v", err)
	}
	if len(got) != 1 || got[0].ID != "ev_b" {
		t.Fatalf("Pull returned %+v, want only dev_b's event (dev_a deferred)", got)
	}
	if hub.Stats.Truncated != 2 || hub.Stats.Skipped != 0 {
		t.Fatalf("Stats = %+v, want Truncated=2 Skipped=0", *hub.Stats)
	}
	if len(rec.sightings) != 1 || rec.sightings[0] != [2]string{"ev_future", SkipReasonUnknownVersion} {
		t.Fatalf("sightings = %v, want one unknown-version record for ev_future", rec.sightings)
	}
}

// TestEncryptedHubUnknownVersionQuarantinesPastGrace (P6-SYNC-02): once the
// grace lapses, the carrier is handed to the undecryptable quarantine so an
// abandoned old client cannot wedge forever on a permanently-newer fleet; the
// post-upgrade replay recovers it.
func TestEncryptedHubUnknownVersionQuarantinesPastGrace(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	future, _ := EncryptEvent(state.Event{ID: "ev_future", DeviceID: "dev_a", Seq: 1, HLC: 1, Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, wck1, 1)
	var env encryptedEnvelope
	_ = json.Unmarshal([]byte(future.PayloadJSON), &env)
	env.Version = 3
	raw, _ := json.Marshal(env)
	future.PayloadJSON = string(raw)
	rec := &skipRecorder{firstSeen: time.Now().Add(-2 * time.Hour)}
	back := &recordingHub{events: []state.Event{future}}
	hub := EncryptedHub{Hub: back, Keyring: kr, Stats: &PullStats{}, NoteSkipped: rec.note, GraceWindow: time.Hour}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ev_future" || got[0].Type != EventEncryptedV2 {
		t.Fatalf("Pull returned %+v, want the still-encrypted carrier forwarded", got)
	}
	if hub.Stats.Undecryptable != 1 || hub.Stats.Truncated != 0 {
		t.Fatalf("Stats = %+v, want Undecryptable=1 Truncated=0", *hub.Stats)
	}
}

// TestEncryptedHubUnknownVersionNilSeamDefersForever pins the legacy contract:
// with no NoteSkipped seam wired, an unknown version defers (never
// quarantines) — unit-test isolation of the pull classification.
func TestEncryptedHubUnknownVersionNilSeamDefersForever(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	future, _ := EncryptEvent(state.Event{ID: "ev_future", DeviceID: "dev_a", Seq: 1, HLC: 1, Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, wck1, 1)
	var env encryptedEnvelope
	_ = json.Unmarshal([]byte(future.PayloadJSON), &env)
	env.Version = 3
	raw, _ := json.Marshal(env)
	future.PayloadJSON = string(raw)
	back := &recordingHub{events: []state.Event{future}}
	hub := EncryptedHub{Hub: back, Keyring: kr, Stats: &PullStats{}}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 0 || hub.Stats.Truncated != 1 {
		t.Fatalf("got %+v stats %+v, want deferred with Truncated=1", got, *hub.Stats)
	}
}

// TestEncryptedHubMalformedEnvelopeForwardsForQuarantine (P6-SYNC-02): junk
// that does not even parse as an envelope is permanently unreadable — it is
// FORWARDED so ApplyEvents records the durable undecryptable conflict and the
// slot is consumed, instead of a log-only drop.
func TestEncryptedHubMalformedEnvelopeForwardsForQuarantine(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	junk := state.Event{ID: "ev_junk", DeviceID: "dev_a", Seq: 1, HLC: 1, Type: EventEncryptedV2, PayloadJSON: `{not json`, ContentHash: "sha256:junk"}
	back := &recordingHub{events: []state.Event{junk}}
	hub := EncryptedHub{Hub: back, Keyring: kr, Stats: &PullStats{}}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ev_junk" {
		t.Fatalf("Pull returned %+v, want the malformed carrier forwarded", got)
	}
	if hub.Stats.Undecryptable != 1 || hub.Stats.Skipped != 0 {
		t.Fatalf("Stats = %+v, want Undecryptable=1 Skipped=0", *hub.Stats)
	}
}

// TestEncryptedHubPoisonEventDoesNotWedge is the core regression for the wedge
// bug, updated for P6-SYNC-04: an event whose envelope names a key this device
// holds but that cannot authenticate (corruption or a forged kid) is FORWARDED
// as its still-encrypted carrier — so ApplyEvents can quarantine it visibly —
// while the good events on either side still decrypt. One bad object can
// neither brick a device's sync nor vanish silently.
func TestEncryptedHubPoisonEventDoesNotWedge(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	before, _ := EncryptEvent(state.Event{ID: "before", DeviceID: "dev_a", HLC: 1, Type: EventProjectAdded, PayloadJSON: `{"path":"work/before"}`, ContentHash: state.ContentHash(`{"path":"work/before"}`)}, wck1, 1)
	// Poison: encrypted under a DIFFERENT key at the same epoch 1, with the
	// envelope's kid forged to the held key's kid so the candidate is found and
	// AEAD authentication fails (a wrong-key ciphertext claiming our kid).
	otherWCK1, _ := NewWCK()
	poison, _ := EncryptEvent(state.Event{ID: "poison", DeviceID: "dev_b", HLC: 2, Type: EventProjectAdded, PayloadJSON: `{"path":"work/poison"}`, ContentHash: state.ContentHash(`{"path":"work/poison"}`)}, otherWCK1, 1)
	poison = rewriteEnvelopeKID(t, poison, KIDForWCK(wck1))
	// Kid-less poison: same wrong-key ciphertext with the kid stripped, so
	// every held key at the epoch is tried and all fail.
	kidlessPoison, _ := EncryptEvent(state.Event{ID: "kidless_poison", DeviceID: "dev_b", HLC: 3, Type: EventProjectAdded, PayloadJSON: `{"path":"work/poison2"}`, ContentHash: state.ContentHash(`{"path":"work/poison2"}`)}, otherWCK1, 1)
	kidlessPoison = rewriteEnvelopeKID(t, kidlessPoison, "")
	after, _ := EncryptEvent(state.Event{ID: "after", DeviceID: "dev_a", HLC: 4, Type: EventProjectAdded, PayloadJSON: `{"path":"work/after"}`, ContentHash: state.ContentHash(`{"path":"work/after"}`)}, wck1, 1)
	back := &recordingHub{events: []state.Event{before, poison, kidlessPoison, after}}
	hub := EncryptedHub{Hub: back, Keyring: kr, Stats: &PullStats{}}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull with poison event: unexpected error %v", err)
	}
	if len(got) != 4 || got[0].ID != "before" || got[1].ID != "poison" || got[2].ID != "kidless_poison" || got[3].ID != "after" {
		t.Fatalf("Pull returned %+v, want [before, poison(carrier), kidless_poison(carrier), after]", got)
	}
	// The good events decrypt; the poisons stay encrypted carriers for
	// ApplyEvents to quarantine (never applied as plaintext).
	if got[0].Type != EventProjectAdded || got[3].Type != EventProjectAdded {
		t.Fatalf("good events not decrypted: %+v", got)
	}
	if got[1].Type != EventEncryptedV2 || got[2].Type != EventEncryptedV2 {
		t.Fatalf("poison events must be forwarded as enc.v2 carriers, got types %q/%q", got[1].Type, got[2].Type)
	}
	if hub.Stats.Undecryptable != 2 || hub.Stats.Skipped != 0 || hub.Stats.Truncated != 0 {
		t.Fatalf("Stats = %+v, want Undecryptable=2 Skipped=0 Truncated=0", *hub.Stats)
	}
}

// TestEncryptedHubRetiredV1Skipped proves retired enc.v1 traffic (written
// before the P6-SYNC-04 wire break) is refused loudly but skipped, not fatal:
// the remedy is re-founding the hub, and the surrounding enc.v2 events still
// come through.
func TestEncryptedHubRetiredV1Skipped(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	good, _ := EncryptEvent(state.Event{ID: "good", DeviceID: "dev_a", HLC: 2, Type: EventProjectAdded, PayloadJSON: `{"path":"work/ok"}`, ContentHash: state.ContentHash(`{"path":"work/ok"}`)}, wck1, 1)
	legacy := state.Event{ID: "legacy", DeviceID: "dev_a", HLC: 1, Type: EventEncryptedV1, PayloadJSON: `{"v":1,"epoch":1,"ct":"aGVsbG8="}`}
	back := &recordingHub{events: []state.Event{legacy, good}}
	hub := EncryptedHub{Hub: back, Keyring: kr, Stats: &PullStats{}}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull with retired v1 event: unexpected error %v", err)
	}
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("Pull returned %+v, want only the enc.v2 'good' event (v1 skipped)", got)
	}
	if hub.Stats.Skipped != 1 || hub.Stats.Undecryptable != 0 {
		t.Fatalf("Stats = %+v, want Skipped=1 Undecryptable=0", *hub.Stats)
	}
}

// rewriteEnvelopeKID rewrites a carrier's envelope kid in place — forging a
// kid (or stripping it). The kid FIELD is outside the AAD (the sealing key's
// kid is bound via the candidate on decrypt), so this is exactly what a
// hostile hub can produce.
func rewriteEnvelopeKID(t *testing.T, carrier state.Event, kid string) state.Event {
	t.Helper()
	var env encryptedEnvelope
	if err := json.Unmarshal([]byte(carrier.PayloadJSON), &env); err != nil {
		t.Fatalf("rewrite kid: %v", err)
	}
	env.KID = kid
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("rewrite kid: %v", err)
	}
	carrier.PayloadJSON = string(raw)
	return carrier
}

// TestEncryptedHubUnheldKidDefersOnlyThatDevice pins the P6-SEC-02 durability
// behavior under the P5-SYNC-01 per-device defer: an event encrypted under a
// kid this device does NOT hold at an epoch it DOES hold (the fleet key vs. a
// legacy self-mint collision) must DEFER — never skip — so the event is
// retried once its grant arrives instead of being permanently jumped by the
// cursor. The defer is scoped to the offending ORIGIN device: its batch tail
// (grants' key material was already ingested in the first pass) is dropped so
// its Seq cursor holds, while other devices' later events keep flowing.
func TestEncryptedHubUnheldKidDefersOnlyThatDevice(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1) // holds one key at epoch 1
	wck1, _ := kr.WCK(1)
	prefix, _ := EncryptEvent(state.Event{ID: "mine", DeviceID: "dev_a", Seq: 1, HLC: 1, Type: EventProjectAdded, PayloadJSON: `{"path":"work/mine"}`, ContentHash: state.ContentHash(`{"path":"work/mine"}`)}, wck1, 1)
	fleetWCK, _ := NewWCK() // the founder's key at the SAME epoch, not yet granted
	fleet, _ := EncryptEvent(state.Event{ID: "fleet", DeviceID: "dev_b", Seq: 1, HLC: 2, Type: EventProjectAdded, PayloadJSON: `{"path":"work/fleet"}`, ContentHash: state.ContentHash(`{"path":"work/fleet"}`)}, fleetWCK, 1)
	// dev_b's SUCCESSOR (also decryptable) must ride the defer with it — it
	// could not chain-apply and would only churn hash-chain conflicts.
	fleetNext, _ := EncryptEvent(state.Event{ID: "fleet_next", DeviceID: "dev_b", Seq: 2, HLC: 4, Type: EventProjectAdded, PayloadJSON: `{"path":"work/fleet2"}`, ContentHash: state.ContentHash(`{"path":"work/fleet2"}`)}, wck1, 1)
	// dev_a's later event keeps flowing (the old whole-batch truncate held it).
	trailing, _ := EncryptEvent(state.Event{ID: "trailing", DeviceID: "dev_a", Seq: 2, HLC: 3, Type: EventProjectAdded, PayloadJSON: `{"path":"work/trailing"}`, ContentHash: state.ContentHash(`{"path":"work/trailing"}`)}, wck1, 1)
	back := &recordingHub{events: []state.Event{prefix, fleet, trailing, fleetNext}}
	hub := EncryptedHub{Hub: back, Keyring: kr, Stats: &PullStats{}}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull with unheld kid: unexpected error %v", err)
	}
	if len(got) != 2 || got[0].ID != "mine" || got[1].ID != "trailing" {
		t.Fatalf("Pull returned %+v, want [mine trailing] (dev_b deferred, dev_a unaffected)", got)
	}
	// P6-HUB-01: both deferred dev_b events count as truncated (gc refusal).
	if hub.Stats.Truncated != 2 || hub.Stats.Skipped != 0 {
		t.Fatalf("Stats = %+v, want Truncated=2 Skipped=0", *hub.Stats)
	}
}

func TestEncryptedHubBlobPassthrough(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	back := &recordingHub{}
	hub := EncryptedHub{Hub: back, Keyring: kr}
	payload := []byte("ciphertext-blob")
	if err := hub.PutBlob(ctx, "aaaa", bytes.NewReader(payload)); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	r, err := hub.GetBlob(ctx, "aaaa")
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if string(got) != string(payload) {
		t.Fatalf("blob passthrough = %q, want %q", got, payload)
	}
}

func TestEncryptedHubPushNoEpochFails(t *testing.T) {
	ctx := context.Background()
	kr := &fakeKeyring{epoch: 0, keys: map[int64][][]byte{}}
	hub := EncryptedHub{Hub: &recordingHub{}, Keyring: kr}
	err := hub.Push(ctx, []state.Event{{ID: "e", Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}})
	if !errors.Is(err, ErrMissingWorkspaceKey) {
		t.Fatalf("Push with no epoch: got %v, want ErrMissingWorkspaceKey", err)
	}
}

// TestEncryptedHubRelabeledKidStillDecrypts pins the post-#33 review fix
// (fable-5, Major): the envelope kid is an unauthenticated routing hint, so a
// hostile hub relabeling a genuinely decryptable event's kid — to an unheld
// value or to a different held kid — must not wedge (truncate) or lose (skip)
// the event. All held keys at the epoch are tried; the AEAD picks the truth.
func TestEncryptedHubRelabeledKidStillDecrypts(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	genuine, _ := EncryptEvent(state.Event{ID: "genuine", DeviceID: "dev_a", HLC: 1, Type: EventProjectAdded, PayloadJSON: `{"path":"work/genuine"}`, ContentHash: state.ContentHash(`{"path":"work/genuine"}`)}, wck1, 1)
	// Relabel to an unheld, well-formed kid: pre-fix this truncated forever.
	unheldKID := KIDForWCK([]byte("0123456789abcdef0123456789abcdef"))
	relabeled := rewriteEnvelopeKID(t, genuine, unheldKID)
	trailing, _ := EncryptEvent(state.Event{ID: "trailing", DeviceID: "dev_a", HLC: 2, Type: EventProjectAdded, PayloadJSON: `{"path":"work/trailing"}`, ContentHash: state.ContentHash(`{"path":"work/trailing"}`)}, wck1, 1)
	back := &recordingHub{events: []state.Event{relabeled, trailing}}
	hub := EncryptedHub{Hub: back, Keyring: kr}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull with relabeled kid: unexpected error %v", err)
	}
	if len(got) != 2 || got[0].ID != "genuine" || got[1].ID != "trailing" {
		t.Fatalf("Pull returned %+v, want the relabeled event decrypted and the batch intact", got)
	}
	if got[0].PayloadJSON != `{"path":"work/genuine"}` {
		t.Fatalf("relabeled event not restored: %+v", got[0])
	}
}

// TestReplayRecoversKidStrippedEventAfterGrant pins the P6-SYNC-04 review fix
// (gpt-5.5 Major): a hostile hub strips the untrusted kid hint from an event
// whose key this device has NOT been granted yet, while the device holds a
// different key at the same epoch. The AEAD failure steers the carrier into
// the permanent undecryptable quarantine (the defer heuristic keys on the
// attacker-controlled kid, so it cannot be trusted to defer) — but the
// quarantine preserves the carrier, and once the real grant arrives,
// ReplayUndecryptableConflicts decrypts it, applies it, and auto-resolves the
// conflict. The hub can delay a not-yet-granted event; it can no longer
// destroy it.
func TestReplayRecoversKidStrippedEventAfterGrant(t *testing.T) {
	ctx := context.Background()
	st, device := newSyncStore(t)
	kr := newFakeKeyring(t, 1) // the device's own key at epoch 1
	fleetWCK, _ := NewWCK()    // the not-yet-granted key, same epoch

	sealed, err := EncryptEvent(state.Event{
		ID: "evt_stripped", DeviceID: device.ID, Seq: 0, HLC: 20 << hlcLogicalBits,
		Type:        EventProjectAdded,
		PayloadJSON: `{"path":"work/stripped","type":"git_repo","remote_key":"github.com/org/stripped"}`,
		ContentHash: state.ContentHash(`{"path":"work/stripped","type":"git_repo","remote_key":"github.com/org/stripped"}`),
	}, fleetWCK, 1)
	if err != nil {
		t.Fatal(err)
	}
	stripped := rewriteEnvelopeKID(t, sealed, "") // the hostile-hub mutation

	back := &recordingHub{events: []state.Event{stripped}}
	hub := EncryptedHub{Hub: back, Keyring: kr, Stats: &PullStats{}}

	// Pull: the kid-stripped carrier cannot be classified as deferrable, so it
	// is forwarded and quarantined permanently by ApplyEvents.
	pulled, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pulled) != 1 || pulled[0].Type != EventEncryptedV2 {
		t.Fatalf("Pull = %+v, want the forwarded carrier", pulled)
	}
	if _, stats, err := ApplyEventsWithStats(ctx, st, pulled, nil); err != nil || stats.Quarantined != 1 {
		t.Fatalf("apply: stats=%+v err=%v, want Quarantined=1", stats, err)
	}

	// Replay before the grant: still undecryptable, conflict stays open.
	if n, err := ReplayUndecryptableConflicts(ctx, st, hub); err != nil || n != 0 {
		t.Fatalf("pre-grant replay = (%d, %v), want (0, nil)", n, err)
	}
	open, err := st.OpenConflictsByType(ctx, ConflictEventVerification)
	if err != nil || len(open) != 1 {
		t.Fatalf("open conflicts = %d (%v), want 1", len(open), err)
	}

	// The grant arrives (simulated: the keyring now holds the fleet key).
	kr.addKey(1, fleetWCK)

	replayed, err := ReplayUndecryptableConflicts(ctx, st, hub)
	if err != nil || replayed != 1 {
		t.Fatalf("post-grant replay = (%d, %v), want (1, nil)", replayed, err)
	}
	// The event applied and the quarantine auto-resolved.
	if open, _ := st.OpenConflictsByType(ctx, ConflictEventVerification); len(open) != 0 {
		t.Fatalf("conflict still open after recovery: %+v", open)
	}
	if projection := projectionOf(t, st); projection["work/stripped"] != "github.com/org/stripped" {
		t.Fatalf("recovered event did not apply: %+v", projection)
	}
	if ev, err := st.EventByID(ctx, "evt_stripped"); err != nil || ev.Type != EventProjectAdded {
		t.Fatalf("recovered event not in log as plaintext: %+v err=%v", ev, err)
	}
}

// TestReplayRecoveryUnblocksHashChainSuccessor (review refinement, opus-4.8):
// the kid-stripped event E is quarantined; its origin-device successor E2
// (whose prev_event_hash names E) hits ErrEventHashChain — a TRANSIENT
// quarantine that holds the cursor, so E2 is re-delivered. Once E's grant
// arrives, the replay recovers E, and the re-delivered E2 then chains onto it
// cleanly: the whole origin-device tail converges instead of wedging.
func TestReplayRecoveryUnblocksHashChainSuccessor(t *testing.T) {
	ctx := context.Background()
	st, _ := newSyncStore(t)
	kr := newFakeKeyring(t, 1)
	heldWCK, _ := kr.WCK(1)
	fleetWCK, _ := NewWCK() // not yet granted

	const dev = "dev_remote_chain"
	e1Payload := `{"path":"work/chain-a","type":"git_repo","remote_key":"github.com/org/chain-a"}`
	e1 := state.Event{
		ID: "evt_chain_1", DeviceID: dev, Seq: 1, HLC: 20 << hlcLogicalBits,
		Type: EventProjectAdded, PayloadJSON: e1Payload, ContentHash: state.ContentHash(e1Payload),
	}
	e2Payload := `{"path":"work/chain-b","type":"git_repo","remote_key":"github.com/org/chain-b"}`
	e2 := state.Event{
		ID: "evt_chain_2", DeviceID: dev, Seq: 2, HLC: 30 << hlcLogicalBits,
		Type: EventProjectAdded, PayloadJSON: e2Payload, ContentHash: state.ContentHash(e2Payload),
		PrevEventHash: e1.ContentHash,
	}
	sealed1, err := EncryptEvent(e1, fleetWCK, 1) // sealed under the ungranted key
	if err != nil {
		t.Fatal(err)
	}
	stripped1 := rewriteEnvelopeKID(t, sealed1, "") // hostile hub strips the hint
	sealed2, err := EncryptEvent(e2, heldWCK, 1)    // successor decrypts fine
	if err != nil {
		t.Fatal(err)
	}

	back := &recordingHub{events: []state.Event{stripped1, sealed2}}
	hub := EncryptedHub{Hub: back, Keyring: kr, Stats: &PullStats{}}

	pulled, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	safe, stats, err := ApplyEventsWithStats(ctx, st, pulled, nil)
	if err != nil {
		t.Fatal(err)
	}
	// E1 is quarantined (permanent class — consumes seq 1); E2 is
	// hash-chain-held (transient — stops the contiguous run at seq 2): dev's
	// cursor lands on 1, below E2, so E2 will be re-delivered.
	if stats.Quarantined != 2 || !stats.CursorHeld {
		t.Fatalf("stats = %+v, want Quarantined=2 CursorHeld=true", stats)
	}
	if safe.After(dev) != 1 {
		t.Fatalf("safe = %v, want %s:1 (past the consumed E1, below the held E2)", safe, dev)
	}

	// The grant arrives; the replay recovers E1.
	kr.addKey(1, fleetWCK)
	if n, err := ReplayUndecryptableConflicts(ctx, st, hub); err != nil || n != 1 {
		t.Fatalf("replay = (%d, %v), want (1, nil)", n, err)
	}
	// The next pull re-delivers E2 (cursor was held below it); it now chains
	// onto the recovered E1 and applies.
	pulled, err = hub.Pull(ctx, safe)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ApplyEventsWithStats(ctx, st, pulled, nil); err != nil {
		t.Fatal(err)
	}
	projection := projectionOf(t, st)
	if projection["work/chain-a"] != "github.com/org/chain-a" || projection["work/chain-b"] != "github.com/org/chain-b" {
		t.Fatalf("origin-device tail did not converge: %+v", projection)
	}
}

// --- P6-SEC-03: grace-bounded quarantine for never-granted keys ---

// grantWaitRecorder is a MissingKeyWait seam double: it returns a fixed
// first-seen time and records every (epoch, kid) sighting.
type grantWaitRecorder struct {
	firstSeen time.Time
	sightings [][2]interface{}
}

func (g *grantWaitRecorder) note(_ context.Context, epoch int64, kid string) (time.Time, error) {
	g.sightings = append(g.sightings, [2]interface{}{epoch, kid})
	return g.firstSeen, nil
}

// TestEncryptedHubMissingEpochWithinGraceTruncates pins that a missing epoch
// still truncates (defer + retry) while its grace window is open, and that the
// sighting is recorded through the seam so the window has a stable start.
func TestEncryptedHubMissingEpochWithinGraceTruncates(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	prefix, _ := EncryptEvent(state.Event{ID: "e1", DeviceID: "dev_a", HLC: 1, Type: EventProjectAdded, PayloadJSON: `{"path":"work/a"}`, ContentHash: state.ContentHash(`{"path":"work/a"}`)}, wck1, 1)
	wck5, _ := NewWCK()
	future, _ := EncryptEvent(state.Event{ID: "e5", DeviceID: "dev_a", HLC: 2, Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, wck5, 5)
	rec := &grantWaitRecorder{firstSeen: time.Now()}
	hub := EncryptedHub{
		Hub: &recordingHub{events: []state.Event{prefix, future}}, Keyring: kr,
		Stats: &PullStats{}, MissingKeyWait: rec.note, GraceWindow: time.Hour,
	}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 1 || got[0].ID != "e1" {
		t.Fatalf("Pull returned %+v, want the epoch-1 prefix only (epoch-5 deferred within grace)", got)
	}
	if hub.Stats.Truncated != 1 || hub.Stats.Undecryptable != 0 {
		t.Fatalf("Stats = %+v, want Truncated=1 Undecryptable=0", *hub.Stats)
	}
	// The wait must be recorded EPOCH-LEVEL: with no key at the epoch the
	// envelope kid is an unauthenticated hint, and persisting it would leave a
	// phantom kid-specific row the real grant never clears (post-#55 review).
	if len(rec.sightings) != 1 || rec.sightings[0][0].(int64) != 5 || rec.sightings[0][1].(string) != "" {
		t.Fatalf("sightings = %+v, want one epoch-5 sighting with kid \"\"", rec.sightings)
	}
}

// TestEncryptedHubMalformedKidQuarantinesImmediately: a kid that is not shaped
// like hex(sha256) can never be granted, so at a held epoch it skips the grace
// wait entirely — immediate quarantine, and NO wait row for a hostile hub to
// pin a phantom "awaiting key grants" warning with.
func TestEncryptedHubMalformedKidQuarantinesImmediately(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	prefix, _ := EncryptEvent(state.Event{ID: "mine", DeviceID: "dev_a", HLC: 1, Type: EventProjectAdded, PayloadJSON: `{"path":"work/mine"}`, ContentHash: state.ContentHash(`{"path":"work/mine"}`)}, wck1, 1)
	otherWCK, _ := NewWCK()
	garbage, _ := EncryptEvent(state.Event{ID: "garbage", DeviceID: "dev_b", HLC: 2, Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, otherWCK, 1)
	garbage = rewriteEnvelopeKID(t, garbage, "ab") // hostile short label
	rec := &grantWaitRecorder{firstSeen: time.Now()}
	hub := EncryptedHub{
		Hub: &recordingHub{events: []state.Event{prefix, garbage}}, Keyring: kr,
		Stats: &PullStats{}, MissingKeyWait: rec.note, GraceWindow: time.Hour,
	}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 2 || got[1].ID != "garbage" || got[1].Type != EventEncryptedV2 {
		t.Fatalf("Pull returned %+v, want the malformed-kid event forwarded as a carrier immediately", got)
	}
	if hub.Stats.Undecryptable != 1 || hub.Stats.Truncated != 0 {
		t.Fatalf("Stats = %+v, want Undecryptable=1 Truncated=0", *hub.Stats)
	}
	if len(rec.sightings) != 0 {
		t.Fatalf("sightings = %+v, want none (never-grantable kid must not open a wait)", rec.sightings)
	}
}

// TestEncryptedHubMissingEpochGraceExpiredQuarantines is the P6-SEC-03 core
// fix: once the grace window for a never-granted epoch runs out, its events
// are forwarded as still-encrypted carriers (quarantine path) instead of
// truncating forever — and LATER events at held epochs still decrypt, so one
// never-granted epoch no longer wedges all sync behind it.
func TestEncryptedHubMissingEpochGraceExpiredQuarantines(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	prefix, _ := EncryptEvent(state.Event{ID: "e1", DeviceID: "dev_a", HLC: 1, Type: EventProjectAdded, PayloadJSON: `{"path":"work/a"}`, ContentHash: state.ContentHash(`{"path":"work/a"}`)}, wck1, 1)
	wck5, _ := NewWCK()
	sealed, _ := EncryptEvent(state.Event{ID: "e5", DeviceID: "dev_b", HLC: 2, Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, wck5, 5)
	trailing, _ := EncryptEvent(state.Event{ID: "e1b", DeviceID: "dev_a", HLC: 3, Type: EventProjectAdded, PayloadJSON: `{"path":"work/b"}`, ContentHash: state.ContentHash(`{"path":"work/b"}`)}, wck1, 1)
	rec := &grantWaitRecorder{firstSeen: time.Now().Add(-2 * time.Hour)}
	hub := EncryptedHub{
		Hub: &recordingHub{events: []state.Event{prefix, sealed, trailing}}, Keyring: kr,
		Stats: &PullStats{}, MissingKeyWait: rec.note, GraceWindow: time.Hour,
	}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 3 || got[0].ID != "e1" || got[1].ID != "e5" || got[2].ID != "e1b" {
		t.Fatalf("Pull returned %+v, want prefix + forwarded carrier + trailing", got)
	}
	if got[1].Type != EventEncryptedV2 {
		t.Fatalf("expired-grace event forwarded as %q, want the still-encrypted %q carrier", got[1].Type, EventEncryptedV2)
	}
	if got[2].Type != EventProjectAdded {
		t.Fatalf("trailing held-epoch event = %q, want decrypted %q (later events must still apply)", got[2].Type, EventProjectAdded)
	}
	if hub.Stats.Truncated != 0 || hub.Stats.Undecryptable != 1 {
		t.Fatalf("Stats = %+v, want Truncated=0 Undecryptable=1", *hub.Stats)
	}
}

// TestEncryptedHubUnheldKidGraceExpiredQuarantines covers the second truncate
// site (an unheld kid at a HELD epoch — the P6-SEC-02 collision and the
// forged-kid stall primitive): past the grace window the event quarantines
// instead of deferring, and the batch tail still decrypts.
func TestEncryptedHubUnheldKidGraceExpiredQuarantines(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	prefix, _ := EncryptEvent(state.Event{ID: "mine", DeviceID: "dev_a", HLC: 1, Type: EventProjectAdded, PayloadJSON: `{"path":"work/mine"}`, ContentHash: state.ContentHash(`{"path":"work/mine"}`)}, wck1, 1)
	fleetWCK, _ := NewWCK()
	fleet, _ := EncryptEvent(state.Event{ID: "fleet", DeviceID: "dev_b", HLC: 2, Type: EventProjectAdded, PayloadJSON: `{"path":"work/fleet"}`, ContentHash: state.ContentHash(`{"path":"work/fleet"}`)}, fleetWCK, 1)
	trailing, _ := EncryptEvent(state.Event{ID: "trailing", DeviceID: "dev_a", HLC: 3, Type: EventProjectAdded, PayloadJSON: `{"path":"work/trailing"}`, ContentHash: state.ContentHash(`{"path":"work/trailing"}`)}, wck1, 1)
	rec := &grantWaitRecorder{firstSeen: time.Now().Add(-2 * time.Hour)}
	hub := EncryptedHub{
		Hub: &recordingHub{events: []state.Event{prefix, fleet, trailing}}, Keyring: kr,
		Stats: &PullStats{}, MissingKeyWait: rec.note, GraceWindow: time.Hour,
	}
	got, err := hub.Pull(ctx, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(got) != 3 || got[1].ID != "fleet" || got[1].Type != EventEncryptedV2 {
		t.Fatalf("Pull returned %+v, want the fleet-kid event forwarded as a carrier", got)
	}
	if got[2].ID != "trailing" || got[2].Type != EventProjectAdded {
		t.Fatalf("trailing event %+v, want it decrypted (tail unwedged)", got[2])
	}
	if hub.Stats.Truncated != 0 || hub.Stats.Undecryptable != 1 {
		t.Fatalf("Stats = %+v, want Truncated=0 Undecryptable=1", *hub.Stats)
	}
	if len(rec.sightings) != 1 || rec.sightings[0][0].(int64) != 1 || rec.sightings[0][1].(string) == "" {
		t.Fatalf("sightings = %+v, want one epoch-1 sighting carrying the unheld kid", rec.sightings)
	}
}

// TestEncryptedHubNilWaitSeamTruncatesForever pins the nil-seam legacy
// contract explicitly: without MissingKeyWait there is no grace clock and a
// missing epoch defers indefinitely (the unit-test-only configuration).
func TestEncryptedHubNilWaitSeamTruncatesForever(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck5, _ := NewWCK()
	sealed, _ := EncryptEvent(state.Event{ID: "e5", DeviceID: "dev_b", HLC: 2, Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, wck5, 5)
	hub := EncryptedHub{Hub: &recordingHub{events: []state.Event{sealed}}, Keyring: kr, Stats: &PullStats{}, GraceWindow: 0}
	for i := 0; i < 2; i++ {
		got, err := hub.Pull(ctx, nil)
		if err != nil {
			t.Fatalf("Pull #%d: %v", i, err)
		}
		if len(got) != 0 || hub.Stats.Truncated != 1 {
			t.Fatalf("Pull #%d returned %+v (stats %+v), want empty + Truncated=1 forever", i, got, *hub.Stats)
		}
	}
}
