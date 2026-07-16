package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// P5-CLI-01 part B: db migrate/status/backup (plain)/down --json shapes via the
// shared opts.render seam. Part A already covered db backup --full and db restore.

func TestDBMigrateJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "db", "migrate")
	if err != nil {
		t.Fatalf("db migrate --json stderr = %q err = %v", stderr, err)
	}

	var got dbMigrateResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("db migrate --json is not dbMigrateResult: %v\n%s", err, stdout)
	}
	if got.Version < 1 {
		t.Fatalf("version = %d, want >= 1", got.Version)
	}
}

func TestDBStatusJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "db", "status")
	if err != nil {
		t.Fatalf("db status --json stderr = %q err = %v", stderr, err)
	}

	var got dbStatusResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("db status --json is not dbStatusResult: %v\n%s", err, stdout)
	}
	if got.Version < 1 {
		t.Fatalf("version = %d, want >= 1", got.Version)
	}
	if got.QuickCheck != "ok" {
		t.Fatalf("quick_check = %q, want ok", got.QuickCheck)
	}
	if got.ForeignKeyCheck != "ok" {
		t.Fatalf("foreign_key_check = %q, want ok", got.ForeignKeyCheck)
	}
}

func TestDBBackupJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	outPath := filepath.Join(t.TempDir(), "state.db.bak")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "db", "backup", outPath)
	if err != nil {
		t.Fatalf("db backup --json stderr = %q err = %v", stderr, err)
	}

	var got dbBackupResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("db backup --json is not dbBackupResult: %v\n%s", err, stdout)
	}
	abs, err := filepath.Abs(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != abs {
		t.Fatalf("path = %q, want %q", got.Path, abs)
	}
	if _, err := os.Stat(got.Path); err != nil {
		t.Fatalf("backup file missing at %s: %v", got.Path, err)
	}
}

func TestDBDownJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	// Capture pre-down version so we can assert rollback moved version by one.
	statusOut, stderr, err := executeForTest("--home", home, "--root", root, "--json", "db", "status")
	if err != nil {
		t.Fatalf("db status --json stderr = %q err = %v", stderr, err)
	}
	var before dbStatusResult
	if err := json.Unmarshal([]byte(statusOut), &before); err != nil {
		t.Fatalf("pre-down status: %v\n%s", err, statusOut)
	}
	if before.Version < 1 {
		t.Fatalf("pre-down version = %d, want >= 1", before.Version)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "db", "down")
	if err != nil {
		t.Fatalf("db down --json stderr = %q err = %v", stderr, err)
	}

	var got dbDownResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("db down --json is not dbDownResult: %v\n%s", err, stdout)
	}
	if got.Version != before.Version-1 {
		t.Fatalf("version = %d, want %d (before-1)", got.Version, before.Version-1)
	}
}
