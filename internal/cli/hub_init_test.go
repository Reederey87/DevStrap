package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/config"
)

func TestRewriteConfigHubReplacesTopLevelLine(t *testing.T) {
	home := t.TempDir()
	paths := config.Paths{Home: home}
	seed := "# keep\nroot: \"/code\"\nhub: \"git+file:///old.git\"\n  hub: nested\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := rewriteConfigHub(paths, "git+file:///new.git"); err != nil {
		t.Fatalf("rewriteConfigHub: %v", err)
	}

	want := "# keep\nroot: \"/code\"\nhub: \"git+file:///new.git\"\n  hub: nested\n"
	got := readConfig(t, home)
	if got != want {
		t.Fatalf("config mismatch:\ngot  %q\nwant %q", got, want)
	}
	if got := countTopLevelHubLines(got); got != 1 {
		t.Fatalf("top-level hub line count = %d, want 1", got)
	}
}

func TestRewriteConfigHubAppendsWhenAbsent(t *testing.T) {
	home := t.TempDir()
	paths := config.Paths{Home: home}
	seed := "# keep\nroot: \"/code\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := rewriteConfigHub(paths, "git+ssh://git@host/path.git?branch=work"); err != nil {
		t.Fatalf("rewriteConfigHub: %v", err)
	}

	want := "# keep\nroot: \"/code\"\nhub: \"git+ssh://git@host/path.git?branch=work\"\n"
	got := readConfig(t, home)
	if got != want {
		t.Fatalf("config mismatch:\ngot  %q\nwant %q", got, want)
	}
	if got := countTopLevelHubLines(got); got != 1 {
		t.Fatalf("top-level hub line count = %d, want 1", got)
	}
}

func TestHubInitUninitializedHomeRefusedWithRemedy(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")

	_, stderr, err := executeForTest("--home", home, "--root", root, "hub", "init", "git+file:///tmp/hub.git")
	if err == nil {
		t.Fatal("hub init succeeded without config.yaml, want usage error")
	}
	var app appError
	if !errors.As(err, &app) || app.code != exitUsage {
		t.Fatalf("err = %v, want appError exitUsage", err)
	}
	if !strings.Contains(stderr, "run `devstrap init` first") {
		t.Fatalf("stderr = %q, want init remedy", stderr)
	}
}

func TestHubInitSameURLNoOp(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubURI := "git+file://" + filepath.Join(t.TempDir(), "hub.git")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "hub", "init", "--no-probe", hubURI); err != nil {
		t.Fatalf("first hub init stderr = %q err = %v", stderr, err)
	}
	before := readConfig(t, home)

	stdout, stderr, err := executeForTest("--home", home, "hub", "init", "--no-probe", hubURI)
	if err != nil {
		t.Fatalf("same hub init stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "nothing to do") {
		t.Fatalf("stdout = %q, want no-op message", stdout)
	}
	if after := readConfig(t, home); after != before {
		t.Fatalf("config changed on same hub init:\nbefore=%q\nafter=%q", before, after)
	}
}

func TestHubInitConfirmationSurvivesQuiet(t *testing.T) {
	// P7-CLI-03: "Configured hub:" is a terminal confirmation and must print
	// under --quiet; printHubInitNextSteps ("Next:") stays gated.
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubURI := "git+file://" + filepath.Join(t.TempDir(), "hub.git")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--quiet", "hub", "init", "--no-probe", hubURI)
	if err != nil {
		t.Fatalf("hub init stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Configured hub:") {
		t.Fatalf("stdout = %q, want Configured hub: confirmation even under --quiet", stdout)
	}
	if strings.Contains(stdout, "Next:") {
		t.Fatalf("stdout = %q, want Next: hint suppressed under --quiet", stdout)
	}
}

func TestHubInitDifferentURLRefusedWithoutForce(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	oldHub := "git+file://" + filepath.Join(t.TempDir(), "old.git")
	newHub := "git+file://" + filepath.Join(t.TempDir(), "new.git")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "hub", "init", "--no-probe", oldHub); err != nil {
		t.Fatalf("first hub init stderr = %q err = %v", stderr, err)
	}

	_, stderr, err := executeForTest("--home", home, "hub", "init", "--no-probe", newHub)
	if err == nil {
		t.Fatal("different hub init succeeded without --force, want conflict")
	}
	var app appError
	if !errors.As(err, &app) || app.code != exitConflict {
		t.Fatalf("err = %v, want appError exitConflict", err)
	}
	if !strings.Contains(stderr, oldHub) || !strings.Contains(stderr, newHub) {
		t.Fatalf("stderr = %q, want both old %q and new %q", stderr, oldHub, newHub)
	}
	if cfg := readConfig(t, home); !strings.Contains(cfg, oldHub) || strings.Contains(cfg, newHub) {
		t.Fatalf("config = %q, want old hub preserved", cfg)
	}
}

func TestHubInitForceOverwritesDifferentURL(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	oldHub := "git+file://" + filepath.Join(t.TempDir(), "old.git")
	newHub := "git+file://" + filepath.Join(t.TempDir(), "new.git")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "hub", "init", "--no-probe", oldHub); err != nil {
		t.Fatalf("first hub init stderr = %q err = %v", stderr, err)
	}

	if _, stderr, err := executeForTest("--home", home, "hub", "init", "--no-probe", "--force", newHub); err != nil {
		t.Fatalf("forced hub init stderr = %q err = %v", stderr, err)
	}
	cfg := readConfig(t, home)
	if !strings.Contains(cfg, newHub) || strings.Contains(cfg, oldHub) {
		t.Fatalf("config = %q, want new hub only", cfg)
	}
	if got := countTopLevelHubLines(cfg); got != 1 {
		t.Fatalf("top-level hub line count = %d, want 1", got)
	}
}

func TestHubInitCredentialedURIRejectedWithoutEcho(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	const secret = "supersecret"
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	_, stderr, err := executeForTest("--home", home, "hub", "init", "--no-probe", "git+https://"+secret+"@host/path.git")
	if err == nil {
		t.Fatal("credentialed hub init succeeded, want error")
	}
	if strings.Contains(stderr, secret) {
		t.Fatalf("stderr leaked credential %q: %q", secret, stderr)
	}
	if !strings.Contains(stderr, "must not contain credentials") {
		t.Fatalf("stderr = %q, want credential refusal", stderr)
	}
}

func TestHubInitRejectsR2WithManualConfigMessage(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	_, stderr, err := executeForTest("--home", home, "hub", "init", "r2://bucket")
	if err == nil {
		t.Fatal("r2 hub init succeeded, want error")
	}
	if !strings.Contains(stderr, "only bootstraps git carriers") || !strings.Contains(stderr, "set `hub:` manually for r2/s3") {
		t.Fatalf("stderr = %q, want manual r2/s3 config message", stderr)
	}
}

func TestHubInitNoProbeSkipsGitRunner(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "probe-called")
	gitPath := filepath.Join(binDir, "git")
	script := "#!/bin/sh\nif [ \"$1\" = \"ls-remote\" ]; then echo called > " + marker + "; exit 42; fi\nexec /usr/bin/git \"$@\"\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	if _, stderr, err := executeForTest("--home", home, "hub", "init", "--no-probe", "git+file:///tmp/hub.git"); err != nil {
		t.Fatalf("hub init --no-probe stderr = %q err = %v", stderr, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("--no-probe ran git ls-remote; marker err = %v", err)
	}
}

// TestHubInitJSON pins the P5-CLI-01 part B --json shape for hub init.
func TestHubInitJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubURI := "git+file://" + filepath.Join(t.TempDir(), "hub.git") + "?branch=main"
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--json", "hub", "init", "--no-probe", hubURI)
	if err != nil {
		t.Fatalf("hub init --json stderr = %q err = %v", stderr, err)
	}
	var got hubInitResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("hub init --json is not a hubInitResult: %v\n%s", err, stdout)
	}
	if got.HubURI != hubURI {
		t.Errorf("hub_uri = %q, want %q", got.HubURI, hubURI)
	}
	if got.Branch != "main" {
		t.Errorf("branch = %q, want main", got.Branch)
	}
	if got.Remote == "" {
		t.Error("remote is empty")
	}
	if got.ReplacedPrevious {
		t.Error("replaced_previous = true on first init, want false")
	}
	if strings.Contains(stdout, "Configured hub:") {
		t.Fatalf("hub init --json leaked human confirmation: %s", stdout)
	}

	// Force-replace a different hub: replaced_previous must be true.
	newHub := "git+file://" + filepath.Join(t.TempDir(), "other.git")
	stdout, stderr, err = executeForTest("--home", home, "--json", "hub", "init", "--no-probe", "--force", newHub)
	if err != nil {
		t.Fatalf("forced hub init --json stderr = %q err = %v", stderr, err)
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("forced hub init --json: %v\n%s", err, stdout)
	}
	if !got.ReplacedPrevious || got.HubURI != newHub {
		t.Fatalf("force replace --json = %+v, want replaced_previous=true hub_uri=%s", got, newHub)
	}
}

func countTopLevelHubLines(content string) int {
	count := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "hub:") {
			count++
		}
	}
	return count
}
