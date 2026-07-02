package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

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

func (r *recordingHub) Pull(_ context.Context, afterHLC int64) ([]state.Event, error) {
	var out []state.Event
	for _, e := range r.events {
		if e.HLC >= afterHLC {
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

func (r *recordingHub) ListBlobs(_ context.Context) ([]string, error) {
	out := make([]string, 0, len(r.blobs))
	for k := range r.blobs {
		out = append(out, k)
	}
	return out, nil
}

// fakeKeyring is a WorkspaceKeyring double with pre-set WCKs and an onIngest
// map that installs a WCK when a grant for that epoch is ingested (simulating a
// successful age-unwrap on the recipient device).
type fakeKeyring struct {
	epoch    int64
	keys     map[int64][]byte
	onIngest map[int64][]byte
	ingested []DeviceKeyGrant
}

func (f *fakeKeyring) CurrentEpoch(context.Context) (int64, error) { return f.epoch, nil }
func (f *fakeKeyring) Prime(context.Context) error                 { return nil }
func (f *fakeKeyring) WCK(epoch int64) ([]byte, bool)              { k, ok := f.keys[epoch]; return k, ok }
func (f *fakeKeyring) IngestGrant(_ context.Context, grant DeviceKeyGrant) error {
	f.ingested = append(f.ingested, grant)
	if wck, ok := f.onIngest[grant.Epoch]; ok {
		if f.keys == nil {
			f.keys = map[int64][]byte{}
		}
		f.keys[grant.Epoch] = wck
	}
	return nil
}

func newFakeKeyring(t *testing.T, epochs ...int64) *fakeKeyring {
	t.Helper()
	wck1, _ := NewWCK()
	keys := map[int64][]byte{epochs[0]: wck1}
	for _, e := range epochs[1:] {
		k, _ := NewWCK()
		keys[e] = k
	}
	return &fakeKeyring{epoch: epochs[len(epochs)-1], keys: keys}
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
	// The backend must store only the enc.v1 carrier, not plaintext.
	if len(back.events) != 1 {
		t.Fatalf("backend stored %d events, want 1", len(back.events))
	}
	stored := back.events[0]
	if stored.Type != EventEncryptedV1 {
		t.Errorf("stored Type = %q, want %q", stored.Type, EventEncryptedV1)
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

	got, err := hub.Pull(ctx, 0)
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
	got, err := hub.Pull(ctx, 0)
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

	got, err := hub.Pull(ctx, 0)
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

	got, err := hub.Pull(ctx, 0)
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

	got, err := hub.Pull(ctx, 0)
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
	kr := &fakeKeyring{epoch: 2, keys: map[int64][]byte{1: wck1}, onIngest: map[int64][]byte{2: wck2}}

	// Build a hub batch: epoch-1 event, epoch-2 grant, epoch-2 event (in HLC order).
	enc1, _ := EncryptEvent(state.Event{ID: "e1", DeviceID: "dev_a", HLC: 10, Type: EventProjectAdded, PayloadJSON: `{"path":"work/a"}`, ContentHash: state.ContentHash(`{"path":"work/a"}`)}, wck1, 1)
	grantPayload, _ := json.Marshal(DeviceKeyGrant{Epoch: 2, Recipient: "age1self", WrappedKey: "wrapped2"})
	grant := state.Event{ID: "g2", DeviceID: "dev_a", HLC: 15, Type: EventDeviceKeyGranted, PayloadJSON: string(grantPayload), ContentHash: state.ContentHash(string(grantPayload))}
	enc2, _ := EncryptEvent(state.Event{ID: "e2", DeviceID: "dev_a", HLC: 20, Type: EventProjectUpdated, PayloadJSON: `{"path":"work/b"}`, ContentHash: state.ContentHash(`{"path":"work/b"}`)}, wck2, 2)
	back := &recordingHub{events: []state.Event{enc1, grant, enc2}}
	hub := EncryptedHub{Hub: back, Keyring: kr}

	got, err := hub.Pull(ctx, 0)
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
	got, err := hub.Pull(ctx, 0)
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
	hub := EncryptedHub{Hub: back, Keyring: kr}
	got, err := hub.Pull(ctx, 0)
	if err != nil {
		t.Fatalf("Pull missing epoch: unexpected error %v", err)
	}
	if len(got) != 1 || got[0].ID != "e1" {
		t.Fatalf("Pull returned %+v, want the epoch-1 prefix only (epoch-5 event deferred)", got)
	}
}

// TestEncryptedHubUnknownVersion proves an envelope version this build cannot
// read is refused but skipped (not fatal), so a newer client's events cannot
// wedge an older client.
func TestEncryptedHubUnknownVersion(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := NewWCK()
	enc, _ := EncryptEvent(state.Event{ID: "ev", DeviceID: "dev_a", HLC: 1, Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}, wck1, 1)
	// Forge version 2.
	var env encryptedEnvelope
	_ = json.Unmarshal([]byte(enc.PayloadJSON), &env)
	env.Version = 2
	raw, _ := json.Marshal(env)
	enc.PayloadJSON = string(raw)
	back := &recordingHub{events: []state.Event{enc}}
	hub := EncryptedHub{Hub: back, Keyring: kr}
	got, err := hub.Pull(ctx, 0)
	if err != nil {
		t.Fatalf("Pull unknown version: unexpected error %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Pull returned %+v, want the unknown-version event skipped", got)
	}
}

// TestEncryptedHubPoisonEventDoesNotWedge is the core regression for the wedge
// bug: an event this device holds the epoch for but cannot authenticate (a
// wrong-key/cross-device epoch collision, corruption, or forgery) is skipped
// with the good events on either side still delivered — one bad object can no
// longer brick a device's sync by aborting the whole batch forever.
func TestEncryptedHubPoisonEventDoesNotWedge(t *testing.T) {
	ctx := context.Background()
	kr := newFakeKeyring(t, 1)
	wck1, _ := kr.WCK(1)
	before, _ := EncryptEvent(state.Event{ID: "before", DeviceID: "dev_a", HLC: 1, Type: EventProjectAdded, PayloadJSON: `{"path":"work/before"}`, ContentHash: state.ContentHash(`{"path":"work/before"}`)}, wck1, 1)
	// Poison: encrypted under a DIFFERENT key at the same epoch 1 (the P4-SEC-07
	// cross-device collision). This device's WCK(1) opens it and Open() fails.
	otherWCK1, _ := NewWCK()
	poison, _ := EncryptEvent(state.Event{ID: "poison", DeviceID: "dev_b", HLC: 2, Type: EventProjectAdded, PayloadJSON: `{"path":"work/poison"}`, ContentHash: state.ContentHash(`{"path":"work/poison"}`)}, otherWCK1, 1)
	after, _ := EncryptEvent(state.Event{ID: "after", DeviceID: "dev_a", HLC: 3, Type: EventProjectAdded, PayloadJSON: `{"path":"work/after"}`, ContentHash: state.ContentHash(`{"path":"work/after"}`)}, wck1, 1)
	back := &recordingHub{events: []state.Event{before, poison, after}}
	hub := EncryptedHub{Hub: back, Keyring: kr}
	got, err := hub.Pull(ctx, 0)
	if err != nil {
		t.Fatalf("Pull with poison event: unexpected error %v", err)
	}
	if len(got) != 2 || got[0].ID != "before" || got[1].ID != "after" {
		t.Fatalf("Pull returned %+v, want [before, after] with poison skipped", got)
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
	kr := &fakeKeyring{epoch: 0, keys: map[int64][]byte{}}
	hub := EncryptedHub{Hub: &recordingHub{}, Keyring: kr}
	err := hub.Push(ctx, []state.Event{{ID: "e", Type: EventProjectAdded, PayloadJSON: `{}`, ContentHash: state.ContentHash(`{}`)}})
	if !errors.Is(err, ErrMissingWorkspaceKey) {
		t.Fatalf("Push with no epoch: got %v, want ErrMissingWorkspaceKey", err)
	}
}
