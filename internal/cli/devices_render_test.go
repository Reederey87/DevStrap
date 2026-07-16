package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/pairing"
)

// P5-CLI-01 part B: devices * domain --json shapes via the shared opts.render seam
// (devices list was part A — covered in render_migration_test.go).

func TestDevicesEnrollJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	ageID, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	const deviceID = "dev_enroll_json_000000000000000000000001"
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"devices", "enroll", deviceID,
		"--name", "laptop", "--os", "linux", "--arch", "arm64",
		"--age-recipient", ageID.Recipient, "--signing-public-key", signing.Public)
	if err != nil {
		t.Fatalf("devices enroll --json stderr = %q err = %v", stderr, err)
	}
	var got deviceEnrollResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("devices enroll --json is not deviceEnrollResult: %v\n%s", err, stdout)
	}
	if got.DeviceID != deviceID || got.TrustState != "pending" {
		t.Fatalf("devices enroll --json = %+v, want device_id=%s trust_state=pending", got, deviceID)
	}
}

func TestDevicesEnrollCodeApproveJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	workspaceID, _, err := executeForTest("--home", home, "--root", root, "devices", "recipient", "--workspace-id")
	if err != nil {
		t.Fatal(err)
	}
	remote := testPairingCodeValue(t, strings.TrimSpace(workspaceID), "dev_33333333333333333333333333333333")
	blob, err := pairing.Encode(remote)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := devicekeys.Fingerprint(remote.SigningPublicKey, remote.AgeRecipient)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"devices", "enroll", "--code", blob, "--approve", "--fingerprint", fp)
	if err != nil {
		t.Fatalf("devices enroll --code --approve --json stderr = %q err = %v", stderr, err)
	}
	var got deviceEnrollResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("devices enroll --code --json is not deviceEnrollResult: %v\n%s", err, stdout)
	}
	if got.DeviceID != remote.DeviceID || got.TrustState != "approved" {
		t.Fatalf("devices enroll --code --json = %+v, want device_id=%s trust_state=approved", got, remote.DeviceID)
	}
}

func TestDevicesPairingCodeJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "devices", "pairing-code")
	if err != nil {
		t.Fatalf("devices pairing-code --json stderr = %q err = %v", stderr, err)
	}
	var got devicesPairingCodeResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("devices pairing-code --json is not devicesPairingCodeResult: %v\n%s", err, stdout)
	}
	if got.Code == "" || got.Fingerprint == "" {
		t.Fatalf("devices pairing-code --json = %+v, want non-empty code and fingerprint", got)
	}
	decoded, err := pairing.Decode(got.Code)
	if err != nil {
		t.Fatalf("pairing-code JSON code not decodable: %v", err)
	}
	wantFP, err := devicekeys.Fingerprint(decoded.SigningPublicKey, decoded.AgeRecipient)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != wantFP {
		t.Fatalf("fingerprint = %q, want %q", got.Fingerprint, wantFP)
	}
	// Guidance remains on stderr under --json (optional human help).
	if !strings.Contains(stderr, got.Fingerprint) {
		t.Fatalf("stderr guidance missing fingerprint; stderr=%q", stderr)
	}
}

func TestDevicesApproveJSON(t *testing.T) {
	home, root := initGuardHome(t)
	const id = "dev_approve_json_0000000000000000000001"
	fp := enrollPendingDevice(t, home, root, id)
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"devices", "approve", id, "--fingerprint", fp)
	if err != nil {
		t.Fatalf("devices approve --json stderr = %q err = %v", stderr, err)
	}
	var got deviceTrustResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("devices approve --json is not deviceTrustResult: %v\n%s", err, stdout)
	}
	if got.DeviceID != id || got.TrustState != "approved" {
		t.Fatalf("devices approve --json = %+v, want device_id=%s trust_state=approved", got, id)
	}
}

func TestDevicesRevokeJSON(t *testing.T) {
	home, root := initGuardHome(t)
	const id = "dev_revoke_json_0000000000000000000001"
	fp := enrollPendingDevice(t, home, root, id)
	if _, stderr, err := executeForTest("--home", home, "--root", root,
		"devices", "approve", id, "--fingerprint", fp); err != nil {
		t.Fatalf("approve first: %v (%s)", err, stderr)
	}
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"devices", "revoke", id)
	if err != nil {
		t.Fatalf("devices revoke --json stderr = %q err = %v", stderr, err)
	}
	var got deviceTrustResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("devices revoke --json is not deviceTrustResult: %v\n%s", err, stdout)
	}
	if got.DeviceID != id || got.TrustState != "revoked" {
		t.Fatalf("devices revoke --json = %+v, want device_id=%s trust_state=revoked", got, id)
	}
	// Side-effect notes stay on stderr (not folded into JSON Warnings).
	if strings.Contains(stdout, "note:") || strings.Contains(stdout, "warning:") {
		t.Fatalf("devices revoke --json leaked stderr diagnostics onto stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "revoked") && !strings.Contains(stderr, "note:") {
		// Propagation note is expected; don't hard-fail if wording drifts slightly
		// as long as stdout stayed pure JSON (asserted above).
		t.Logf("revoke stderr (diagnostics): %q", stderr)
	}
}

func TestDevicesLostJSON(t *testing.T) {
	home, root := initGuardHome(t)
	const id = "dev_lost_json_000000000000000000000001"
	fp := enrollPendingDevice(t, home, root, id)
	if _, stderr, err := executeForTest("--home", home, "--root", root,
		"devices", "approve", id, "--fingerprint", fp); err != nil {
		t.Fatalf("approve first: %v (%s)", err, stderr)
	}
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"devices", "lost", id)
	if err != nil {
		t.Fatalf("devices lost --json stderr = %q err = %v", stderr, err)
	}
	var got deviceTrustResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("devices lost --json is not deviceTrustResult: %v\n%s", err, stdout)
	}
	if got.DeviceID != id || got.TrustState != "lost" {
		t.Fatalf("devices lost --json = %+v, want device_id=%s trust_state=lost", got, id)
	}
}

func TestDevicesRenameJSON(t *testing.T) {
	home, root := initGuardHome(t)
	const id = "dev_rename_json_0000000000000000000001"
	_ = enrollPendingDevice(t, home, root, id)
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"devices", "rename", id, "new-name")
	if err != nil {
		t.Fatalf("devices rename --json stderr = %q err = %v", stderr, err)
	}
	var got deviceRenameResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("devices rename --json is not deviceRenameResult: %v\n%s", err, stdout)
	}
	if got.DeviceID != id || got.Name != "new-name" {
		t.Fatalf("devices rename --json = %+v, want device_id=%s name=new-name", got, id)
	}
}

func TestDevicesRecipientJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	// Human mode stays bare-value-on-a-line (frozen contract).
	humanOut, _, err := executeForTest("--home", home, "--root", root, "devices", "recipient")
	if err != nil {
		t.Fatalf("devices recipient human: %v", err)
	}
	humanVal := strings.TrimSpace(humanOut)
	if humanVal == "" || strings.Contains(humanOut, "{") {
		t.Fatalf("human recipient output must stay bare value, got %q", humanOut)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "devices", "recipient")
	if err != nil {
		t.Fatalf("devices recipient --json stderr = %q err = %v", stderr, err)
	}
	var got deviceRecipientResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("devices recipient --json is not deviceRecipientResult: %v\n%s", err, stdout)
	}
	if got.Kind != "recipient" || got.Value != humanVal {
		t.Fatalf("devices recipient --json = %+v, want kind=recipient value=%q", got, humanVal)
	}

	for _, tc := range []struct {
		flag string
		kind string
	}{
		{"--signing", "signing"},
		{"--workspace-id", "workspace_id"},
		{"--fingerprint", "fingerprint"},
	} {
		out, _, err := executeForTest("--home", home, "--root", root, "--json", "devices", "recipient", tc.flag)
		if err != nil {
			t.Fatalf("devices recipient %s --json: %v", tc.flag, err)
		}
		var mode deviceRecipientResult
		if err := json.Unmarshal([]byte(out), &mode); err != nil {
			t.Fatalf("devices recipient %s --json: %v\n%s", tc.flag, err, out)
		}
		if mode.Kind != tc.kind || mode.Value == "" {
			t.Fatalf("devices recipient %s --json = %+v, want kind=%s non-empty value", tc.flag, mode, tc.kind)
		}
	}
}
