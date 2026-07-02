package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P6-SEC-02: `init` records role=founder and no longer mints a workspace key
// (founding is deferred to the first sync against an empty hub), while
// `init --join` records role=joiner and prints the approval-first next steps.
func TestInitWritesFounderRoleByDefault(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "init")
	if err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	cfg := readConfig(t, home)
	if !strings.Contains(cfg, `role: "founder"`) {
		t.Fatalf("config = %q, want role: founder", cfg)
	}
	if strings.Contains(stdout, "Joining an existing workspace") {
		t.Fatalf("default init printed a join hint: %q", stdout)
	}
}

func TestInitJoinWritesJoinerRoleAndHint(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "init", "--join")
	if err != nil {
		t.Fatalf("init --join stderr = %q err = %v", stderr, err)
	}
	cfg := readConfig(t, home)
	if !strings.Contains(cfg, `role: "joiner"`) {
		t.Fatalf("config = %q, want role: joiner", cfg)
	}
	if !strings.Contains(stdout, "Joining an existing workspace") {
		t.Fatalf("init --join stdout = %q, want join next-steps", stdout)
	}
	if !strings.Contains(stdout, "--approve") {
		t.Fatalf("init --join stdout = %q, want approval instruction", stdout)
	}
}

func readConfig(t *testing.T, home string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
