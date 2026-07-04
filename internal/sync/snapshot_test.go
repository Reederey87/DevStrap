package sync

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
)

func testSnapshot() Snapshot {
	return Snapshot{
		WorkspaceID: "ws_test",
		ProducedBy:  "dev_a",
		HLC:         1000,
		Floor:       Cursor{"dev_a": 5, "dev_b": 3},
		Anchors: []ChainAnchor{
			{DeviceID: "dev_a", Seq: 4, ContentHash: "hash-a4", HLC: 900},
			{DeviceID: "dev_b", Seq: 2, ContentHash: "hash-b2", HLC: 800},
		},
		Entries: []SnapshotEntry{{
			Path:                "work/api",
			PathKey:             "work/api",
			Type:                "git_repo",
			Status:              "active",
			SourceEventHLC:      700,
			SourceEventDeviceID: "dev_a",
			SourceEventID:       "evt_1",
			Git: &SnapshotGit{
				RemoteURL:     "git@github.com:acme/api.git",
				RemoteKey:     "github.com/acme/api",
				DefaultBranch: "main",
				LFSPolicy:     "auto",
			},
		}},
		Tombstones: []SnapshotTombstone{{
			PathKey:             "work/old",
			TombstoneHLC:        500,
			SourceEventDeviceID: "dev_b",
			SourceEventID:       "evt_2",
		}},
	}
}

func TestSealUnsealSnapshotRoundTrip(t *testing.T) {
	wck, err := NewWCK()
	if err != nil {
		t.Fatal(err)
	}
	snap := testSnapshot()
	obj, shaHex, err := SealSnapshot(snap, wck, 3)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(obj)
	if hex.EncodeToString(sum[:]) != shaHex {
		t.Fatalf("returned sha %s does not match object bytes", shaHex)
	}
	info, err := ParseSnapshotEnvelope(obj)
	if err != nil {
		t.Fatal(err)
	}
	if info.Epoch != 3 || info.KID != KIDForWCK(wck) || info.ProducedBy != "dev_a" || info.WorkspaceID != "ws_test" || info.HLC != 1000 {
		t.Fatalf("envelope info mismatch: %+v", info)
	}
	got, err := UnsealSnapshot(obj, wck)
	if err != nil {
		t.Fatal(err)
	}
	if got.V != snapshotVersion || got.Epoch != 3 || got.KID != KIDForWCK(wck) {
		t.Fatalf("unsealed version/epoch/kid mismatch: %+v", got)
	}
	if len(got.Entries) != 1 || got.Entries[0].Git == nil || got.Entries[0].Git.RemoteKey != "github.com/acme/api" {
		t.Fatalf("entries did not round-trip: %+v", got.Entries)
	}
	if len(got.Tombstones) != 1 || got.Tombstones[0].TombstoneHLC != 500 {
		t.Fatalf("tombstones did not round-trip: %+v", got.Tombstones)
	}
	if got.Floor.After("dev_b") != 3 || len(got.Anchors) != 2 {
		t.Fatalf("floor/anchors did not round-trip: %+v %+v", got.Floor, got.Anchors)
	}
}

func TestUnsealSnapshotWrongKeyFails(t *testing.T) {
	wck, _ := NewWCK()
	other, _ := NewWCK()
	obj, _, err := SealSnapshot(testSnapshot(), wck, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnsealSnapshot(obj, other); !errors.Is(err, ErrSnapshotVerification) {
		t.Fatalf("wrong key: got %v, want ErrSnapshotVerification", err)
	}
}

// TestUnsealSnapshotCarrierTamperMatrix mutates each plaintext carrier field
// of the sealed envelope and requires an AEAD authentication failure — the
// same posture as the enc.v2 event AAD (P6-SYNC-04).
func TestUnsealSnapshotCarrierTamperMatrix(t *testing.T) {
	wck, _ := NewWCK()
	obj, _, err := SealSnapshot(testSnapshot(), wck, 3)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(env map[string]any)) []byte {
		var env map[string]any
		if err := json.Unmarshal(obj, &env); err != nil {
			t.Fatal(err)
		}
		f(env)
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	cases := map[string]func(env map[string]any){
		"workspace_id": func(env map[string]any) { env["workspace_id"] = "ws_evil" },
		"produced_by":  func(env map[string]any) { env["produced_by"] = "dev_evil" },
		"hlc":          func(env map[string]any) { env["hlc"] = float64(9999) },
		"epoch":        func(env map[string]any) { env["epoch"] = float64(4) },
		"ct":           func(env map[string]any) { env["ct"] = base64.StdEncoding.EncodeToString(make([]byte, 64)) },
	}
	for name, f := range cases {
		if _, err := UnsealSnapshot(mutate(f), wck); !errors.Is(err, ErrSnapshotVerification) {
			t.Errorf("tampered %s: got %v, want ErrSnapshotVerification", name, err)
		}
	}
	// The kid FIELD is an unauthenticated routing hint (mirrors enc.v2): a
	// relabel must NOT break decryption under the true key.
	relabeled := mutate(func(env map[string]any) { env["kid"] = "0000" })
	if _, err := UnsealSnapshot(relabeled, wck); err != nil {
		t.Errorf("kid relabel must stay harmless, got %v", err)
	}
}

func TestSealSnapshotContentAddressIsStablePerObject(t *testing.T) {
	wck, _ := NewWCK()
	obj1, sha1, err := SealSnapshot(testSnapshot(), wck, 3)
	if err != nil {
		t.Fatal(err)
	}
	obj2, sha2, err := SealSnapshot(testSnapshot(), wck, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Random nonces mean two seals of the same document are different objects
	// with different addresses — that is the point of content addressing
	// (concurrent compactors can never clobber each other's keys).
	if sha1 == sha2 {
		t.Fatal("two seals produced the same content address (nonce reuse?)")
	}
	for _, pair := range []struct {
		obj []byte
		sha string
	}{{obj1, sha1}, {obj2, sha2}} {
		sum := sha256.Sum256(pair.obj)
		if hex.EncodeToString(sum[:]) != pair.sha {
			t.Fatal("content address does not match object bytes")
		}
	}
}

func testSigningKeys(t *testing.T) (private, public string) {
	t.Helper()
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return signing.Private, signing.Public
}

func TestRetentionManifestSignVerifyRoundTrip(t *testing.T) {
	private, public := testSigningKeys(t)
	m := RetentionManifest{
		WorkspaceID: "ws_test",
		Floors:      map[string]int64{"dev_a": 5, "dev_b": 3},
		Snapshot:    RetentionSnapshotRef{SHA256: "abc", Epoch: 3, KID: "kid", HLC: 1000, ProducedBy: "dev_a"},
		ProducedBy:  "dev_a",
		ProducedAt:  1000,
	}
	if err := SignRetentionManifest(&m, private); err != nil {
		t.Fatal(err)
	}
	if m.V != snapshotVersion || m.Sig == "" {
		t.Fatalf("sign did not stamp v/sig: %+v", m)
	}
	if err := VerifyRetentionManifest(m, public); err != nil {
		t.Fatal(err)
	}
	// JSON round-trip (what the hub stores) must still verify: the canonical
	// payload cannot depend on in-memory field order.
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRetentionManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRetentionManifest(parsed, public); err != nil {
		t.Fatalf("round-tripped manifest failed verification: %v", err)
	}
}

func TestRetentionManifestTamperFailsVerification(t *testing.T) {
	private, public := testSigningKeys(t)
	base := RetentionManifest{
		WorkspaceID: "ws_test",
		Floors:      map[string]int64{"dev_a": 5},
		Snapshot:    RetentionSnapshotRef{SHA256: "abc", Epoch: 3, KID: "kid", HLC: 1000, ProducedBy: "dev_a"},
		ProducedBy:  "dev_a",
		ProducedAt:  1000,
	}
	if err := SignRetentionManifest(&base, private); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(m *RetentionManifest){
		"floor raised":     func(m *RetentionManifest) { m.Floors["dev_a"] = 99 },
		"floor added":      func(m *RetentionManifest) { m.Floors["dev_evil"] = 1 },
		"snapshot swapped": func(m *RetentionManifest) { m.Snapshot.SHA256 = "evil" },
		"producer swapped": func(m *RetentionManifest) { m.ProducedBy = "dev_evil" },
		"prev unlinked":    func(m *RetentionManifest) { m.PrevSHA256 = "evil" },
		"sig stripped":     func(m *RetentionManifest) { m.Sig = "" },
	}
	for name, f := range mutations {
		m := base
		m.Floors = map[string]int64{}
		for k, v := range base.Floors {
			m.Floors[k] = v
		}
		f(&m)
		if err := VerifyRetentionManifest(m, public); !errors.Is(err, ErrSnapshotVerification) {
			t.Errorf("%s: got %v, want ErrSnapshotVerification", name, err)
		}
	}
}

func TestParseRetentionFloorsFailsClosedOnGarbage(t *testing.T) {
	if _, err := ParseRetentionFloors([]byte("not json")); err == nil {
		t.Fatal("garbled manifest must not parse as no-floor")
	}
}
