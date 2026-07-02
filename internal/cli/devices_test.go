package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/viper"
)

func TestReplayQuarantinedEventsAfterDeviceApproval(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}

	approvedSigning, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDevice(ctx, state.Device{
		ID:               "dev_approved",
		Name:             "approved",
		OS:               "linux",
		Arch:             "arm64",
		SigningPublicKey: approvedSigning.Public,
		TrustState:       "approved",
	}); err != nil {
		t.Fatal(err)
	}
	pendingSigning, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDevice(ctx, state.Device{
		ID:               "dev_pending",
		Name:             "pending",
		OS:               "linux",
		Arch:             "arm64",
		SigningPublicKey: pendingSigning.Public,
		TrustState:       "pending",
	}); err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(dssync.ProjectPayload{
		Path:      "work/acme/replayed",
		Type:      "git_repo",
		RemoteKey: "github.com/acme/replayed",
	})
	if err != nil {
		t.Fatal(err)
	}
	event := state.Event{
		ID:          "evt_pending_project",
		DeviceID:    "dev_pending",
		Seq:         1,
		HLC:         10 << 16,
		Type:        dssync.EventProjectAdded,
		PayloadJSON: string(payload),
		ContentHash: state.ContentHash(string(payload)),
	}
	sig, err := devicekeys.Sign(pendingSigning.Private, "devstrap:event:v1", state.EventSignaturePayload(event))
	if err != nil {
		t.Fatal(err)
	}
	event.DeviceSig = sig

	if _, err := dssync.ApplyEvents(ctx, store, []state.Event{event}); err != nil {
		t.Fatalf("quarantine pending event: %v", err)
	}
	open, err := store.OpenConflictsByType(ctx, dssync.ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open verification conflicts = %d, want 1", len(open))
	}

	if err := store.SetDeviceTrustState(ctx, "dev_pending", "approved"); err != nil {
		t.Fatal(err)
	}
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	var stderr bytes.Buffer
	replayQuarantinedEvents(ctx, &stderr, opts, store, "dev_pending")
	if !strings.Contains(stderr.String(), "Replayed 1 quarantined event") {
		t.Fatalf("stderr = %q, want replay summary", stderr.String())
	}
	if _, err := store.ProjectByPath(ctx, "work/acme/replayed"); err != nil {
		t.Fatalf("replayed project missing: %v", err)
	}
	got, err := store.ConflictByID(ctx, open[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "resolved" {
		t.Fatalf("conflict status = %q, want resolved", got.Status)
	}
}

// The realistic first-contact ordering: an unknown device's events arrive and
// are quarantined (auto-created pending placeholder, no signing key), THEN the
// user enrolls it with `devices enroll --signing-public-key ... --approve`.
// The enroll path must replay the quarantined events exactly like `approve`.
func TestEnrollApproveReplaysQuarantinedEvents(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		closeStore(store)
		t.Fatal(err)
	}

	approvedSigning, err := devicekeys.NewSigningIdentity()
	if err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	if err := store.UpsertDevice(ctx, state.Device{
		ID: "dev_approved", Name: "approved", OS: "linux", Arch: "arm64",
		SigningPublicKey: approvedSigning.Public, TrustState: "approved",
	}); err != nil {
		closeStore(store)
		t.Fatal(err)
	}

	newSigning, err := devicekeys.NewSigningIdentity()
	if err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	payload, err := json.Marshal(dssync.ProjectPayload{
		Path: "work/acme/first-contact", Type: "git_repo", RemoteKey: "github.com/acme/first-contact",
	})
	if err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	event := state.Event{
		ID:          "evt_first_contact",
		DeviceID:    "dev_unknown_yet",
		Seq:         1,
		HLC:         10 << 16,
		Type:        dssync.EventProjectAdded,
		PayloadJSON: string(payload),
		ContentHash: state.ContentHash(string(payload)),
	}
	sig, err := devicekeys.Sign(newSigning.Private, "devstrap:event:v1", state.EventSignaturePayload(event))
	if err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	event.DeviceSig = sig
	if _, err := dssync.ApplyEvents(ctx, store, []state.Event{event}); err != nil {
		closeStore(store)
		t.Fatalf("quarantine first-contact event: %v", err)
	}
	open, err := store.OpenConflictsByType(ctx, dssync.ConflictEventVerification)
	if err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	if len(open) != 1 {
		closeStore(store)
		t.Fatalf("open verification conflicts = %d, want 1", len(open))
	}
	closeStore(store)

	newIdentity, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root,
		"devices", "enroll", "dev_unknown_yet", "--name", "laptop", "--os", "linux", "--arch", "arm64",
		"--age-recipient", newIdentity.Recipient,
		"--signing-public-key", newSigning.Public, "--approve"); err != nil {
		t.Fatalf("enroll --approve stderr = %q err = %v", stderr, err)
	}

	store, err = state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	if _, err := store.ProjectByPath(ctx, "work/acme/first-contact"); err != nil {
		t.Fatalf("replayed project missing after enroll --approve: %v", err)
	}
	got, err := store.ConflictByID(ctx, open[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "resolved" {
		t.Fatalf("conflict status = %q, want resolved", got.Status)
	}
}

// Divergent-duplicate conflicts are data-integrity disputes, not trust
// failures: approving the device must NOT auto-resolve them (a replay would
// "succeed" only because the original event with different content exists).
func TestReplaySkipsDivergentConflicts(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}

	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDevice(ctx, state.Device{
		ID: "dev_x", Name: "x", OS: "linux", Arch: "arm64",
		SigningPublicKey: signing.Public, TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(dssync.ProjectPayload{
		Path: "work/acme/original", Type: "git_repo", RemoteKey: "github.com/acme/original",
	})
	if err != nil {
		t.Fatal(err)
	}
	event := state.Event{
		ID:          "evt_divergent",
		DeviceID:    "dev_x",
		Seq:         1,
		HLC:         10 << 16,
		Type:        dssync.EventProjectAdded,
		PayloadJSON: string(payload),
		ContentHash: state.ContentHash(string(payload)),
	}
	sig, err := devicekeys.Sign(signing.Private, "devstrap:event:v1", state.EventSignaturePayload(event))
	if err != nil {
		t.Fatal(err)
	}
	event.DeviceSig = sig
	if _, err := dssync.ApplyEvents(ctx, store, []state.Event{event}); err != nil {
		t.Fatalf("apply original: %v", err)
	}

	divergentPayload, err := json.Marshal(dssync.ProjectPayload{
		Path: "work/acme/tampered", Type: "git_repo", RemoteKey: "github.com/acme/tampered",
	})
	if err != nil {
		t.Fatal(err)
	}
	divergent := event
	divergent.PayloadJSON = string(divergentPayload)
	divergent.ContentHash = state.ContentHash(string(divergentPayload))
	divergentSig, err := devicekeys.Sign(signing.Private, "devstrap:event:v1", state.EventSignaturePayload(divergent))
	if err != nil {
		t.Fatal(err)
	}
	divergent.DeviceSig = divergentSig
	if _, err := dssync.ApplyEvents(ctx, store, []state.Event{divergent}); err != nil {
		t.Fatalf("quarantine divergent duplicate: %v", err)
	}
	open, err := store.OpenConflictsByType(ctx, dssync.ConflictEventVerification)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open verification conflicts = %d, want 1", len(open))
	}

	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	var stderr bytes.Buffer
	replayQuarantinedEvents(ctx, &stderr, opts, store, "dev_x")
	if strings.Contains(stderr.String(), "Replayed") {
		t.Fatalf("divergent conflict was replayed: %q", stderr.String())
	}
	got, err := store.ConflictByID(ctx, open[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "open" {
		t.Fatalf("divergent conflict status = %q, want open", got.Status)
	}
	stored, err := store.EventByID(ctx, "evt_divergent")
	if err != nil {
		t.Fatal(err)
	}
	if stored.PayloadJSON != string(payload) {
		t.Fatalf("stored event payload changed: %s", stored.PayloadJSON)
	}
}

// P6-SEC-02: a --join device that approves another device before it has been
// granted the fleet workspace key must NOT self-mint one (which would let it
// push events under a key nobody else holds — the data loss the split closes).
func TestJoinerApprovingAnotherDeviceDoesNotSelfMint(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--join"); err != nil {
		t.Fatalf("init --join stderr = %q err = %v", stderr, err)
	}

	remoteAge, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	remoteSigning, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	_, stderr, err := executeForTest("--home", home, "--root", root,
		"devices", "enroll", "dev_c", "--name", "c", "--os", "linux", "--arch", "arm64",
		"--age-recipient", remoteAge.Recipient, "--signing-public-key", remoteSigning.Public, "--approve")
	if err != nil {
		t.Fatalf("enroll --approve stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stderr, "holds no workspace key yet") {
		t.Fatalf("stderr = %q, want joiner-cannot-grant warning", stderr)
	}

	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	epoch, err := store.CurrentKeyEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if epoch != 0 {
		t.Fatalf("CurrentKeyEpoch = %d, want 0 (joiner must not self-mint on approve)", epoch)
	}
}
