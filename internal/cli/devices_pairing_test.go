package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/pairing"
	"github.com/Reederey87/DevStrap/internal/state"
)

func TestDevicesPairingCodePrintsDecodableLocalDevice(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "devices", "pairing-code")
	if err != nil {
		t.Fatalf("pairing-code stderr = %q err = %v", stderr, err)
	}
	code, err := pairing.Decode(stdout)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	dev, err := store.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, err := store.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if code.WorkspaceID != workspaceID || code.DeviceID != dev.ID || code.Name != dev.Name ||
		code.OS != dev.OS || code.Arch != dev.Arch || code.AgeRecipient != dev.PublicKey ||
		code.SigningPublicKey != dev.SigningPublicKey {
		t.Fatalf("pairing code = %#v, device = %#v workspace = %q", code, dev, workspaceID)
	}
	fp, err := devicekeys.Fingerprint(dev.SigningPublicKey, dev.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, fp) {
		t.Fatalf("stderr = %q, want fingerprint %q", stderr, fp)
	}
}

func TestDevicesEnrollCodeUsageErrorsAndWrongWorkspace(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	code := testPairingCode(t, "ws_11111111111111111111111111111111", "dev_11111111111111111111111111111111")

	_, stderr, err := executeForTest("--home", home, "--root", root, "devices", "enroll", "dev_22222222222222222222222222222222", "--code", code)
	if err == nil || !strings.Contains(stderr, "--code carries the device id; drop the positional argument") {
		t.Fatalf("enroll --code positional stderr = %q err = %v", stderr, err)
	}

	_, stderr, err = executeForTest("--home", home, "--root", root, "devices", "enroll", "--code", code, "--name", "remote")
	if err == nil || !strings.Contains(stderr, "--code is mutually exclusive with the manual enrollment flags") {
		t.Fatalf("enroll --code --name stderr = %q err = %v", stderr, err)
	}

	localWS, _, err := executeForTest("--home", home, "--root", root, "devices", "recipient", "--workspace-id")
	if err != nil {
		t.Fatal(err)
	}
	_, stderr, err = executeForTest("--home", home, "--root", root, "devices", "enroll", "--code", code)
	if err == nil || !strings.Contains(stderr, "pairing code is for workspace ws_11111111111111111111111111111111") ||
		!strings.Contains(stderr, "but this store is "+strings.TrimSpace(localWS)) {
		t.Fatalf("wrong workspace stderr = %q err = %v", stderr, err)
	}
}

func TestDevicesEnrollCodeApproveStoresDecodedFields(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	workspaceID, _, err := executeForTest("--home", home, "--root", root, "devices", "recipient", "--workspace-id")
	if err != nil {
		t.Fatal(err)
	}
	remote := testPairingCodeValue(t, strings.TrimSpace(workspaceID), "dev_22222222222222222222222222222222")
	blob, err := pairing.Encode(remote)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := devicekeys.Fingerprint(remote.SigningPublicKey, remote.AgeRecipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "devices", "enroll", "--code", blob, "--approve", "--fingerprint", fp); err != nil {
		t.Fatalf("enroll --code approve stderr = %q err = %v", stderr, err)
	}
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	dev, err := deviceByID(ctx, store, remote.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if dev.TrustState != "approved" || dev.Name != remote.Name || dev.OS != remote.OS ||
		dev.Arch != remote.Arch || dev.PublicKey != remote.AgeRecipient ||
		dev.SigningPublicKey != remote.SigningPublicKey {
		t.Fatalf("enrolled device = %#v, want approved fields from %#v", dev, remote)
	}
}

func TestInitJoinCodeApprovesOrLeavesFounderPending(t *testing.T) {
	ctx := context.Background()
	founderHome := filepath.Join(t.TempDir(), ".devstrap-founder")
	founderRoot := filepath.Join(t.TempDir(), "CodeFounder")
	if _, stderr, err := executeForTest("--home", founderHome, "--root", founderRoot, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("founder init stderr = %q err = %v", stderr, err)
	}
	codeOut, _, err := executeForTest("--home", founderHome, "--root", founderRoot, "devices", "pairing-code")
	if err != nil {
		t.Fatal(err)
	}
	code, err := pairing.Decode(codeOut)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := devicekeys.Fingerprint(code.SigningPublicKey, code.AgeRecipient)
	if err != nil {
		t.Fatal(err)
	}

	approvedHome := filepath.Join(t.TempDir(), ".devstrap-approved")
	approvedRoot := filepath.Join(t.TempDir(), "CodeApproved")
	if _, stderr, err := executeForTest("--home", approvedHome, "--root", approvedRoot, "init", "--join", "--code", codeOut, "--fingerprint", fp); err != nil {
		t.Fatalf("init --join --code --fingerprint stderr = %q err = %v", stderr, err)
	}
	approvedStore, err := state.Open(ctx, filepath.Join(approvedHome, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(approvedStore)
	gotWS, err := approvedStore.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gotWS != code.WorkspaceID {
		t.Fatalf("workspace id = %q, want %q", gotWS, code.WorkspaceID)
	}
	dev, err := deviceByID(ctx, approvedStore, code.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if dev.TrustState != "approved" {
		t.Fatalf("founder trust state = %q, want approved", dev.TrustState)
	}

	pendingHome := filepath.Join(t.TempDir(), ".devstrap-pending")
	pendingRoot := filepath.Join(t.TempDir(), "CodePending")
	_, stderr, err := executeForTest("--home", pendingHome, "--root", pendingRoot, "init", "--join", "--code", codeOut)
	if err != nil {
		t.Fatalf("init --join --code pending stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stderr, "warning: founder not pinned (no TTY for fingerprint confirmation)") ||
		!strings.Contains(stderr, "devstrap devices approve "+code.DeviceID+" --fingerprint "+fp) {
		t.Fatalf("pending stderr = %q, want warning and follow-up command", stderr)
	}
	pendingStore, err := state.Open(ctx, filepath.Join(pendingHome, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(pendingStore)
	dev, err = deviceByID(ctx, pendingStore, code.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if dev.TrustState != "pending" {
		t.Fatalf("founder trust state = %q, want pending", dev.TrustState)
	}
}

func testPairingCode(t *testing.T, workspaceID, deviceID string) string {
	t.Helper()
	blob, err := pairing.Encode(testPairingCodeValue(t, workspaceID, deviceID))
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func testPairingCodeValue(t *testing.T, workspaceID, deviceID string) pairing.Code {
	t.Helper()
	identity, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return pairing.Code{
		WorkspaceID:      workspaceID,
		DeviceID:         deviceID,
		Name:             "remote",
		OS:               "linux",
		Arch:             "arm64",
		AgeRecipient:     identity.Recipient,
		SigningPublicKey: signing.Public,
	}
}
