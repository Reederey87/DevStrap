package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/pairing"
	"github.com/Reederey87/DevStrap/internal/state"
)

const (
	joinTestWorkspaceID = "ws_0123456789abcdef0123456789abcdef"
	joinTestFounderID   = "dev_0123456789abcdef0123456789abcdef"
)

// founderV2Code builds a v2 founder pairing code with fresh keys and (optionally)
// an embedded hub URI, returning the blob and the derived fingerprint.
func founderV2Code(t *testing.T, hubURI string) (blob, fingerprint string) {
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
		WorkspaceID:      joinTestWorkspaceID,
		DeviceID:         joinTestFounderID,
		Name:             "founder-laptop",
		OS:               "linux",
		Arch:             "arm64",
		AgeRecipient:     identity.Recipient,
		SigningPublicKey: signing.Public,
		HubURI:           hubURI,
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

// founderV1Code hand-builds an old-format v1 pairing code (no fp/hub fields).
func founderV1Code(t *testing.T) string {
	t.Helper()
	identity, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"v":    1,
		"ws":   joinTestWorkspaceID,
		"dev":  joinTestFounderID,
		"name": "founder-laptop",
		"os":   "linux",
		"arch": "arm64",
		"age":  identity.Recipient,
		"sig":  signing.Public,
	})
	if err != nil {
		t.Fatal(err)
	}
	return "devstrap-pair1:" + base64.RawURLEncoding.EncodeToString(raw)
}

// TestJoinV2AutoTrustsFingerprintAndConfiguresHub covers the headline path: a v2
// code with an embedded fingerprint and hub URI joins in one command with no
// prompt — the workspace id is adopted, the founder is pinned approved, the hub
// is configured from the embedded URI, and this device's own pairing code is
// printed for the founder to approve.
func TestJoinV2AutoTrustsFingerprintAndConfiguresHub(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubURI := "git@github.com:me/devstrap-hub.git"
	code, _ := founderV2Code(t, hubURI)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "join", code)
	if err != nil {
		t.Fatalf("join stderr = %q err = %v", stderr, err)
	}
	if strings.Contains(stderr, "not pinned") {
		t.Fatalf("join auto-trust printed a not-pinned warning: %q", stderr)
	}

	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	gotWS, err := store.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gotWS != joinTestWorkspaceID {
		t.Fatalf("workspace id = %q, want %q", gotWS, joinTestWorkspaceID)
	}
	founder, err := deviceByID(ctx, store, joinTestFounderID)
	if err != nil {
		t.Fatal(err)
	}
	if founder.TrustState != "approved" {
		t.Fatalf("founder trust = %q, want approved", founder.TrustState)
	}

	cfg := readConfig(t, home)
	if !strings.Contains(cfg, `hub: "`+hubURI+`"`) {
		t.Fatalf("config = %q, want hub %q", cfg, hubURI)
	}
	if !strings.Contains(stderr, "Configured hub: "+hubURI) {
		t.Fatalf("stderr = %q, want hub-configured note", stderr)
	}

	// stdout must carry this device's own decodable v2 pairing code.
	own, err := pairing.Decode(pairingLine(t, stdout))
	if err != nil {
		t.Fatalf("decode own code from stdout %q: %v", stdout, err)
	}
	dev, err := store.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if own.DeviceID != dev.ID || own.WorkspaceID != joinTestWorkspaceID {
		t.Fatalf("own code = %#v, want device %s workspace %s", own, dev.ID, joinTestWorkspaceID)
	}
	if own.DeviceID == joinTestFounderID {
		t.Fatalf("own code device id equals the founder's; join must mint its own")
	}
}

// TestJoinV2NoHubLeavesHubUnconfigured: a v2 code from a founder with no hub
// configured joins fine but leaves the hub unset with a clear next-step message.
func TestJoinV2NoHubLeavesHubUnconfigured(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	code, _ := founderV2Code(t, "")

	_, stderr, err := executeForTest("--home", home, "--root", root, "join", code)
	if err != nil {
		t.Fatalf("join stderr = %q err = %v", stderr, err)
	}
	cfg := readConfig(t, home)
	if strings.Contains(cfg, "hub:") {
		t.Fatalf("config = %q, want no hub line", cfg)
	}
	if !strings.Contains(stderr, "carried no hub") || !strings.Contains(stderr, "devstrap hub init") {
		t.Fatalf("stderr = %q, want no-hub guidance", stderr)
	}
}

// TestJoinV1FallsBackToManualFlow: a v1 code has no embedded fingerprint, so join
// falls back to init --join --code's non-TTY behavior — the founder is left
// pending with the approve follow-up command, exactly as today.
func TestJoinV1FallsBackToManualFlow(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	code := founderV1Code(t)

	_, stderr, err := executeForTest("--home", home, "--root", root, "join", code)
	if err != nil {
		t.Fatalf("join v1 stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stderr, "founder not pinned") ||
		!strings.Contains(stderr, "devstrap devices approve "+joinTestFounderID) {
		t.Fatalf("stderr = %q, want not-pinned warning + approve follow-up", stderr)
	}
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	founder, err := deviceByID(ctx, store, joinTestFounderID)
	if err != nil {
		t.Fatal(err)
	}
	if founder.TrustState != "pending" {
		t.Fatalf("founder trust = %q, want pending", founder.TrustState)
	}
}

// TestJoinFingerprintMismatchRefuses: --fingerprint enforces the high-assurance
// out-of-band compare — a wrong value refuses before any state is written.
func TestJoinFingerprintMismatchRefuses(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	code, _ := founderV2Code(t, "git@github.com:me/hub.git")

	_, stderr, err := executeForTest("--home", home, "--root", root, "join", code, "--fingerprint", "AAAA-BBBB-CCCC-DDDD")
	if err == nil {
		t.Fatalf("join with a mismatched --fingerprint succeeded, want refusal; stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "fingerprint mismatch") {
		t.Fatalf("stderr = %q, want fingerprint mismatch", stderr)
	}
}

// TestJoinFingerprintMatchApproves: passing the correct out-of-band fingerprint
// to a v2 code pins the founder approved (the high-assurance path succeeds).
func TestJoinFingerprintMatchApproves(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	code, fp := founderV2Code(t, "git@github.com:me/hub.git")

	_, stderr, err := executeForTest("--home", home, "--root", root, "join", code, "--fingerprint", fp)
	if err != nil {
		t.Fatalf("join --fingerprint stderr = %q err = %v", stderr, err)
	}
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	founder, err := deviceByID(ctx, store, joinTestFounderID)
	if err != nil {
		t.Fatal(err)
	}
	if founder.TrustState != "approved" {
		t.Fatalf("founder trust = %q, want approved", founder.TrustState)
	}
}

// pairingLine returns the single stdout line that is a pairing-code blob.
func pairingLine(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), pairing.Prefix) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("no %s line in stdout %q", pairing.Prefix, out)
	return ""
}
