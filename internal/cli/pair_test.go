package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/pairing"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

// pairJoinerDeviceID is a fixed, distinct joiner device id for the pair tests
// (must differ from the founder's minted local id).
const pairJoinerDeviceID = "dev_11111111111111111111111111111111"

// forceTTY overrides the stdin-terminal seam so a piped-stdin test exercises the
// interactive path, restoring the real detector on cleanup.
func forceTTY(t *testing.T) {
	t.Helper()
	prev := stdinIsTerminal
	stdinIsTerminal = func(*cobra.Command) bool { return true }
	t.Cleanup(func() { stdinIsTerminal = prev })
}

// upFounder founds a workspace via `devstrap up` against a file hub and returns
// the home, root, hub path, and the founder's workspace id.
func upFounder(t *testing.T) (home, root, hubPath, workspaceID string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), ".devstrap")
	root = filepath.Join(t.TempDir(), "Code")
	hubPath = filepath.Join(t.TempDir(), "hub.json")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "up", "--hub", "file:"+hubPath); err != nil {
		t.Fatalf("up: %v\nstderr=%s", err, stderr)
	}
	store, err := state.Open(context.Background(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	workspaceID, err = store.WorkspaceID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return home, root, hubPath, workspaceID
}

// joinerPairCode builds a fresh-keys joiner v2 pairing code for the given
// workspace and returns the blob and its fingerprint.
func joinerPairCode(t *testing.T, workspaceID string) (blob, fingerprint string) {
	t.Helper()
	identity, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	blob, err = pairing.Encode(pairing.Code{
		WorkspaceID:      workspaceID,
		DeviceID:         pairJoinerDeviceID,
		Name:             "joiner-laptop",
		OS:               "linux",
		Arch:             "arm64",
		AgeRecipient:     identity.Recipient,
		SigningPublicKey: signing.Public,
	})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err = devicekeys.Fingerprint(signing.Public, identity.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	return blob, fingerprint
}

// TestPairApprovesPastedJoinerCodeAndSyncs is the happy path: the founder runs
// `pair`, a joiner code is piped in (followed by the "yes" confirmation), the
// joiner ends up approved, and the founder's grant-publishing sync runs.
func TestPairApprovesPastedJoinerCodeAndSyncs(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	forceTTY(t)
	ctx := context.Background()

	home, root, hubPath, ws := upFounder(t)
	joinerCode, _ := joinerPairCode(t, ws)

	// stdin: the pasted joiner code, then "yes" for the fingerprint confirmation.
	stdin := strings.NewReader(joinerCode + "\nyes\n")
	stdout, stderr, err := executeForTestWithStdin(stdin, "--home", home, "--root", root, "pair")
	if err != nil {
		t.Fatalf("pair: %v\nstderr=%s", err, stderr)
	}

	// The joiner is approved.
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	joiner, err := deviceByID(ctx, store, pairJoinerDeviceID)
	if err != nil {
		t.Fatalf("joiner not enrolled: %v", err)
	}
	if joiner.TrustState != "approved" {
		t.Fatalf("joiner trust = %q, want approved", joiner.TrustState)
	}

	// The founder's own code printed to stdout, and the summary confirms the sync.
	if _, derr := pairing.Decode(pairingLine(t, stdout)); derr != nil {
		t.Fatalf("decode founder code from stdout %q: %v", stdout, derr)
	}
	if !strings.Contains(stdout, "Paired: device "+pairJoinerDeviceID) {
		t.Fatalf("stdout = %q, want the paired summary", stdout)
	}
	if !strings.Contains(stdout, "must now run 'devstrap sync'") {
		t.Fatalf("stdout = %q, want the joiner-still-needs-to-sync reminder", stdout)
	}

	// The grant was published to the hub (proves the founder's sync ran).
	raw, err := os.ReadFile(hubPath)
	if err != nil {
		t.Fatal(err)
	}
	var hubEvents []state.Event
	if err := json.Unmarshal(raw, &hubEvents); err != nil {
		t.Fatal(err)
	}
	grantFound := false
	for _, e := range hubEvents {
		if e.Type == dssync.EventDeviceKeyGranted {
			grantFound = true
		}
	}
	if !grantFound {
		t.Fatalf("hub carries no device.key.granted after pair's sync (%d events)", len(hubEvents))
	}
}

// TestPairBlankOrEOFExitsCleanly: a blank line or bare EOF (no code) exits 0 with
// the manual follow-up, approving nothing.
func TestPairBlankOrEOFExitsCleanly(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	forceTTY(t)

	for name, in := range map[string]string{"blank-line": "\n", "eof": ""} {
		t.Run(name, func(t *testing.T) {
			home, root, _, _ := upFounder(t)
			stdout, stderr, err := executeForTestWithStdin(strings.NewReader(in), "--home", home, "--root", root, "pair")
			if err != nil {
				t.Fatalf("pair should exit cleanly on %s: %v\nstderr=%s", name, err, stderr)
			}
			if !strings.Contains(stderr, "Finish the founder side by hand") {
				t.Fatalf("stderr = %q, want the manual follow-up", stderr)
			}
			// Still prints this device's own code to stdout.
			if _, derr := pairing.Decode(pairingLine(t, stdout)); derr != nil {
				t.Fatalf("decode founder code from stdout %q: %v", stdout, derr)
			}
			if strings.Contains(stdout, "Paired: device") {
				t.Fatalf("stdout = %q, must not claim a pairing on a blank/EOF input", stdout)
			}
		})
	}
}

// TestPairNonTTYFailsFast: without a terminal, pair refuses immediately (never
// blocking on input that will not arrive) and points at the manual flow.
func TestPairNonTTYFailsFast(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	// No forceTTY: under go test the piped stdin is not a *os.File terminal.

	home, root, _, ws := upFounder(t)
	joinerCode, _ := joinerPairCode(t, ws)
	_, stderr, err := executeForTestWithStdin(strings.NewReader(joinerCode+"\nyes\n"), "--home", home, "--root", root, "pair")
	if err == nil {
		t.Fatalf("pair without a TTY succeeded, want fast fail; stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "needs an interactive terminal") {
		t.Fatalf("stderr = %q, want the non-TTY manual-flow message", stderr)
	}
}

// TestPairRefusesOwnCode proves the fix for a review finding (PR #202): pasting
// this device's OWN pairing code back (a plausible fat-finger, since it's
// printed directly above the paste prompt) must be refused with a clear
// message, not silently re-approve/re-grant the device to itself.
func TestPairRefusesOwnCode(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	forceTTY(t)
	ctx := context.Background()

	home, root, _, _ := upFounder(t)
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closeStore(store)
	ownCode, _, err := executeForTest("--home", home, "--root", root, "devices", "pairing-code")
	if err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := executeForTestWithStdin(strings.NewReader(pairingLine(t, ownCode)+"\n"), "--home", home, "--root", root, "pair")
	if err == nil {
		t.Fatalf("pair pasted its own code and succeeded, want refusal; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "OWN pairing code") {
		t.Fatalf("stderr = %q, want the own-code refusal", stderr)
	}

	// Nothing about the local device changed.
	store2, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store2)
	after, err := store2.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.TrustState != before.TrustState {
		t.Fatalf("local device trust state changed from %q to %q after a refused self-paste", before.TrustState, after.TrustState)
	}
}

// TestPairRejectsNegativeTimeout: --timeout must be >= 0; a negative value is a
// usage error, not another way to spell "wait indefinitely" (0 already means
// that) (review finding, PR #202).
func TestPairRejectsNegativeTimeout(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home, root, _, _ := upFounder(t)
	_, stderr, err := executeForTest("--home", home, "--root", root, "pair", "--timeout", "-1s")
	if err == nil {
		t.Fatalf("pair --timeout -1s succeeded, want a usage refusal; stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "--timeout must be >= 0") {
		t.Fatalf("stderr = %q, want the negative-timeout usage message", stderr)
	}
}

// TestPairTimesOutOnNoPaste exercises the ACTUAL timeout branch (not the
// blank-line/EOF branch, which is a distinct code path): stdin blocks forever
// (an unclosed pipe, simulating an operator who never pastes anything and
// never hits Ctrl-C or EOF either), and a short --timeout must still return
// cleanly with the manual follow-up rather than hanging.
func TestPairTimesOutOnNoPaste(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	forceTTY(t)
	home, root, _, _ := upFounder(t)

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })

	stdout, stderr, err := executeForTestWithStdin(pr, "--home", home, "--root", root, "pair", "--timeout", "50ms")
	if err != nil {
		t.Fatalf("pair should time out cleanly, not error: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stderr, "Timed out waiting") {
		t.Fatalf("stderr = %q, want the timeout message (not the blank/EOF message)", stderr)
	}
	if !strings.Contains(stderr, "Finish the founder side by hand") {
		t.Fatalf("stderr = %q, want the manual follow-up", stderr)
	}
	if strings.Contains(stdout, "Paired: device") {
		t.Fatalf("stdout = %q, must not claim a pairing after a timeout", stdout)
	}
}

// TestPairRefusesUninitializedWorkspace: pair on a home that was never founded
// refuses cleanly, pointing at `up`/`init`.
func TestPairRefusesUninitializedWorkspace(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	_, stderr, err := executeForTest("--home", home, "--root", root, "pair")
	if err == nil {
		t.Fatalf("pair on an uninitialized home succeeded, want refusal; stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "no workspace here yet") {
		t.Fatalf("stderr = %q, want the uninitialized refusal", stderr)
	}
}

// TestPairRefusesJoinerRole: pair on a joiner device refuses, pointing at
// `devstrap join` instead (pair is the founder-only wizard).
func TestPairRefusesJoinerRole(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--join"); err != nil {
		t.Fatalf("init --join: %v\nstderr=%s", err, stderr)
	}
	_, stderr, err := executeForTest("--home", home, "--root", root, "pair")
	if err == nil {
		t.Fatalf("pair on a joiner succeeded, want refusal; stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "role: joiner") || !strings.Contains(stderr, "devstrap join") {
		t.Fatalf("stderr = %q, want the joiner-role refusal pointing at devstrap join", stderr)
	}
}
