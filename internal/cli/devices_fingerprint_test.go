package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/state"
)

// pendingDeviceWithKeys inits a founder store and inserts a pending device row
// carrying both keys, returning the store, the device's fingerprint, and a
// close func. The store is left open for trust-state assertions.
func pendingDeviceWithKeys(t *testing.T, deviceID string) (home, root, fp string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), ".devstrap")
	root = filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	store, err := state.Open(context.Background(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	id, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDevice(context.Background(), state.Device{
		ID: deviceID, Name: "remote", OS: "linux", Arch: "arm64",
		PublicKey: id.Recipient, SigningPublicKey: sig.Public, TrustState: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	fp, err = devicekeys.Fingerprint(sig.Public, id.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	return home, root, fp
}

func TestApproveWithMatchingFingerprintSucceeds(t *testing.T) {
	home, root, fp := pendingDeviceWithKeys(t, "dev_match")
	if _, stderr, err := executeForTest("--home", home, "--root", root,
		"devices", "approve", "dev_match", "--fingerprint", fp); err != nil {
		t.Fatalf("approve stderr = %q err = %v", stderr, err)
	}
	if got := trustStateOf(t, home, "dev_match"); got != "approved" {
		t.Fatalf("trust state = %q, want approved", got)
	}
}

func TestApproveWithMismatchedFingerprintRefusesNoWrite(t *testing.T) {
	home, root, fp := pendingDeviceWithKeys(t, "dev_mismatch")
	// Flip the first group so the value is well-formed but wrong.
	wrong := "0000" + fp[4:]
	_, stderr, err := executeForTest("--home", home, "--root", root,
		"devices", "approve", "dev_mismatch", "--fingerprint", wrong)
	if err == nil {
		t.Fatal("approve with mismatched fingerprint succeeded, want refusal")
	}
	if !strings.Contains(stderr, "fingerprint mismatch") {
		t.Fatalf("stderr = %q, want fingerprint mismatch", stderr)
	}
	if got := trustStateOf(t, home, "dev_mismatch"); got != "pending" {
		t.Fatalf("trust state = %q, want pending (no DB write on mismatch)", got)
	}
}

func TestApproveNonTTYWithoutFlagRefusesWithRemedy(t *testing.T) {
	home, root, fp := pendingDeviceWithKeys(t, "dev_notty")
	_, stderr, err := executeForTest("--home", home, "--root", root,
		"devices", "approve", "dev_notty")
	if err == nil {
		t.Fatal("approve without fingerprint on non-TTY succeeded, want refusal")
	}
	if !strings.Contains(stderr, "--fingerprint "+devicekeys.NormalizeFingerprint(fp)) &&
		!strings.Contains(stderr, "--fingerprint "+fp) {
		t.Fatalf("stderr = %q, want remedy embedding computed fingerprint %q", stderr, fp)
	}
	if got := trustStateOf(t, home, "dev_notty"); got != "pending" {
		t.Fatalf("trust state = %q, want pending", got)
	}
}

// SECU-05 tightening: a bare placeholder row (no keys) must refuse approval.
func TestApproveKeylessPlaceholderRefuses(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	store, err := state.Open(context.Background(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	if err := store.UpsertDevice(context.Background(), state.Device{
		ID: "dev_bare", Name: "bare", OS: "linux", Arch: "arm64", TrustState: "pending",
	}); err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	closeStore(store)

	_, stderr, err := executeForTest("--home", home, "--root", root,
		"devices", "approve", "dev_bare", "--fingerprint", "whatever")
	if err == nil {
		t.Fatal("approving a keyless placeholder succeeded, want refusal")
	}
	if !strings.Contains(stderr, "cannot be approved") || !strings.Contains(stderr, "re-enroll") && !strings.Contains(stderr, "Re-enroll") {
		t.Fatalf("stderr = %q, want keyless refusal with re-enroll remedy", stderr)
	}
	if got := trustStateOf(t, home, "dev_bare"); got != "pending" {
		t.Fatalf("trust state = %q, want pending", got)
	}
}

func TestRecipientFingerprintPrintsLocalFingerprint(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	rec, _, err := executeForTest("--home", home, "--root", root, "devices", "recipient")
	if err != nil {
		t.Fatal(err)
	}
	sig, _, err := executeForTest("--home", home, "--root", root, "devices", "recipient", "--signing")
	if err != nil {
		t.Fatal(err)
	}
	want, err := devicekeys.Fingerprint(strings.TrimSpace(sig), strings.TrimSpace(rec))
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := executeForTest("--home", home, "--root", root, "devices", "recipient", "--fingerprint")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != want {
		t.Fatalf("recipient --fingerprint = %q, want %q", strings.TrimSpace(got), want)
	}
	// Mutually exclusive with --signing.
	if _, _, err := executeForTest("--home", home, "--root", root, "devices", "recipient", "--fingerprint", "--signing"); err == nil {
		t.Fatal("recipient --fingerprint --signing succeeded, want mutual-exclusion error")
	}
}

func TestDevicesListHasFingerprintColumn(t *testing.T) {
	home, root, fp := pendingDeviceWithKeys(t, "dev_listed")
	out, _, err := executeForTest("--home", home, "--root", root, "devices", "list")
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "dev_listed\t") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("dev_listed not in list output:\n%s", out)
	}
	fields := strings.Split(row, "\t")
	if got := fields[len(fields)-1]; got != fp {
		t.Fatalf("last column = %q, want fingerprint %q", got, fp)
	}
	// The local device row has both keys too, so its last column is a real
	// fingerprint, not the "-" placeholder.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "\tlocal\t") {
			fields := strings.Split(line, "\t")
			if fields[len(fields)-1] == "-" {
				t.Fatalf("local device row has no fingerprint: %q", line)
			}
		}
	}
}
