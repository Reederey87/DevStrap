package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P4-SEC-07 pairing: `init --workspace-id <id>` adopts the founder's workspace
// id (implies --join), `status` surfaces the id for copying, and
// `devices recipient --workspace-id` prints it bare for scripts.

const testCLIWorkspaceID = "ws_0123456789abcdef0123456789abcdef"

func TestInitWorkspaceIDAdoptsIDAndImpliesJoin(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-id", testCLIWorkspaceID)
	if err != nil {
		t.Fatalf("init --workspace-id stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stdout, "Adopted workspace id "+testCLIWorkspaceID) {
		t.Fatalf("stdout = %q, want adopted-id line", stdout)
	}
	if !strings.Contains(stdout, "Joining an existing workspace") {
		t.Fatalf("stdout = %q, want join hint (--workspace-id implies --join)", stdout)
	}
	if strings.Contains(stderr, "without --workspace-id") {
		t.Fatalf("stderr = %q, want no bare-join warning when the id was supplied", stderr)
	}
	cfg := readConfig(t, home)
	if !strings.Contains(cfg, `role: "joiner"`) {
		t.Fatalf("config = %q, want role: joiner", cfg)
	}

	statusOut, _, err := executeForTest("--home", home, "--root", root, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusOut, "Workspace ID: "+testCLIWorkspaceID) {
		t.Fatalf("status = %q, want Workspace ID row with adopted id", statusOut)
	}
	jsonOut, _, err := executeForTest("--home", home, "--root", root, "--json", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonOut, `"workspace_id": "`+testCLIWorkspaceID+`"`) {
		t.Fatalf("status --json = %q, want workspace_id field", jsonOut)
	}
}

func TestInitWorkspaceIDRejectsInvalidShapeBeforeWriting(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	for _, bad := range []string{"ws_test", "ws_" + strings.Repeat("A", 32), "dev_0123456789abcdef0123456789abcdef", "ws_0123"} {
		_, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-id", bad)
		if err == nil {
			t.Fatalf("init --workspace-id %q succeeded, want shape refusal", bad)
		}
		if !strings.Contains(stderr, "invalid --workspace-id") {
			t.Fatalf("stderr = %q, want invalid --workspace-id message", stderr)
		}
		var app appError
		if !errors.As(err, &app) || app.code != exitInvalidConfig {
			t.Fatalf("err = %v, want appError exitInvalidConfig", err)
		}
	}
	// Shape validation runs before any MkdirAll: nothing was created.
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("state home %s exists after refused init", home)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("root %s exists after refused init", root)
	}
}

func TestInitRefusesDifferentWorkspaceIDOnReinit(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("founder init stderr = %q err = %v", stderr, err)
	}
	_, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-id", testCLIWorkspaceID)
	if err == nil {
		t.Fatal("re-init with a different workspace id succeeded, want refusal")
	}
	var app appError
	if !errors.As(err, &app) || app.code != exitInvalidConfig {
		t.Fatalf("err = %v, want appError exitInvalidConfig", err)
	}
	if !strings.Contains(stderr, "different workspace id") || !strings.Contains(stderr, "--workspace-id "+testCLIWorkspaceID) {
		t.Fatalf("stderr = %q, want mismatch remedy naming init --join --workspace-id", stderr)
	}
	// The founder's minted id survives the refused re-init.
	statusOut, _, err := executeForTest("--home", home, "--root", root, "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(statusOut, testCLIWorkspaceID) {
		t.Fatalf("status = %q, refused id must not be adopted", statusOut)
	}
}

func TestInitBareJoinWarnsAboutWorkspaceID(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "init", "--join")
	if err != nil {
		t.Fatalf("init --join stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stderr, "without --workspace-id") {
		t.Fatalf("stderr = %q, want non-fatal r2/s3 prefix warning", stderr)
	}
	if !strings.Contains(stdout, "devstrap init --join --workspace-id <id>") {
		t.Fatalf("stdout = %q, want copy-the-id step in the join hint", stdout)
	}
}

func TestInitDryRunPrintsWouldAdoptedID(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	stdout, _, err := executeForTest("--home", home, "--root", root, "init", "--dry-run", "--workspace-id", testCLIWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Would adopt workspace id "+testCLIWorkspaceID) {
		t.Fatalf("stdout = %q, want would-adopt line", stdout)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("dry-run created %s", home)
	}
}

func TestDevicesRecipientWorkspaceID(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-id", testCLIWorkspaceID); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	stdout, _, err := executeForTest("--home", home, "--root", root, "devices", "recipient", "--workspace-id")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != testCLIWorkspaceID {
		t.Fatalf("recipient --workspace-id = %q, want bare %q", stdout, testCLIWorkspaceID)
	}
	// The default recipient output stays frozen (scripts consume it bare).
	bare, _, err := executeForTest("--home", home, "--root", root, "devices", "recipient")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(bare), "age1") || strings.Contains(bare, testCLIWorkspaceID) {
		t.Fatalf("recipient = %q, want bare age recipient only", bare)
	}
	if _, _, err := executeForTest("--home", home, "--root", root, "devices", "recipient", "--workspace-id", "--signing"); err == nil {
		t.Fatal("recipient --workspace-id --signing succeeded, want mutual-exclusion error")
	}
}
