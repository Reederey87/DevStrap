package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTarget(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func journalTargets(doneDB bool) []restoreJournalTarget {
	targets := make([]restoreJournalTarget, 0, len(backupTargets))
	for _, name := range backupTargets {
		staged := name == backupEntryDB
		targets = append(targets, restoreJournalTarget{
			Name: name, Staged: staged, Existed: staged, Done: !staged || doneDB,
		})
	}
	return targets
}

func TestPromoteAllTargetsRollsBackAllTargets(t *testing.T) {
	home, stage := filepath.Join(t.TempDir(), "home"), filepath.Join(t.TempDir(), "stage")
	for _, name := range backupTargets {
		writeTarget(t, home, name, "old-"+name)
		writeTarget(t, stage, name, "new-"+name)
	}
	realRename := renameBackupTarget
	calls := 0
	renameBackupTarget = func(old, new string) error {
		calls++
		if calls == 7 {
			return errors.New("injected rename failure")
		}
		return os.Rename(old, new)
	}
	t.Cleanup(func() { renameBackupTarget = realRename })
	journalPath := restoreJournalPath(home)
	if err := promoteAllTargets(home, stage, journalPath); err == nil {
		t.Fatal("expected promotion failure")
	}
	for _, name := range backupTargets {
		got, err := os.ReadFile(filepath.Join(home, name))
		if err != nil || string(got) != "old-"+name {
			t.Fatalf("%s not rolled back: %q err=%v", name, got, err)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(home, "*.bak-*")); len(matches) != 0 {
		t.Fatalf("backup asides remain: %v", matches)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains: %v", err)
	}
}

func TestPromotionRollbackPreservesDanglingSymlinkTarget(t *testing.T) {
	home, stage := filepath.Join(t.TempDir(), "home"), filepath.Join(t.TempDir(), "stage")
	writeTarget(t, home, backupEntryDB, "old-db")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	const linkTarget = "missing-config-target"
	if err := os.Symlink(linkTarget, filepath.Join(home, backupEntryConfig)); err != nil {
		t.Fatal(err)
	}
	writeTarget(t, stage, backupEntryDB, "new-db")
	writeTarget(t, stage, backupEntryConfig, "new-config")
	writeTarget(t, stage, filepath.Join(backupDirBlobs, "blob.age"), "new-blob")

	realRename := renameBackupTarget
	renameBackupTarget = func(old, new string) error {
		if old == filepath.Join(stage, backupDirBlobs) {
			return errors.New("injected later promotion failure")
		}
		return os.Rename(old, new)
	}
	t.Cleanup(func() { renameBackupTarget = realRename })
	if err := promoteAllTargets(home, stage, restoreJournalPath(home)); err == nil {
		t.Fatal("expected promotion failure")
	}
	info, err := os.Lstat(filepath.Join(home, backupEntryConfig))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config target was not restored as symlink: mode=%v err=%v", info, err)
	}
	gotTarget, err := os.Readlink(filepath.Join(home, backupEntryConfig))
	if err != nil || gotTarget != linkTarget {
		t.Fatalf("config symlink target=%q err=%v want %q", gotTarget, err, linkTarget)
	}
	occupied, err := stateDirHasBackupTargets(home)
	if err != nil || !occupied {
		t.Fatalf("dangling restore target not treated as occupied: occupied=%t err=%v", occupied, err)
	}
}

func TestRecoverRestoreJournalRollsBackUntilEveryTargetDone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		done       bool
		want       string
		rolledBack bool
	}{
		{name: "incomplete rolls back", want: "old", rolledBack: true},
		{name: "all done rolls forward", done: true, want: "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			const suffix = ".bak-42-123"
			writeTarget(t, home, backupEntryDB, "new")
			writeTarget(t, home, backupEntryDB+suffix, "old")
			j := restoreJournal{Version: 1, PID: 42, AsideSuffix: suffix, Targets: journalTargets(tc.done)}
			if err := writeRestoreJournal(restoreJournalPath(home), j); err != nil {
				t.Fatal(err)
			}
			rolledBack, err := recoverRestoreJournal(home)
			if err != nil {
				t.Fatal(err)
			}
			if rolledBack != tc.rolledBack {
				t.Fatalf("rolledBack=%t want %t", rolledBack, tc.rolledBack)
			}
			got, err := os.ReadFile(filepath.Join(home, backupEntryDB))
			if err != nil || string(got) != tc.want {
				t.Fatalf("state.db=%q err=%v want %q", got, err, tc.want)
			}
			if _, err := os.Stat(restoreJournalPath(home)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal remains: %v", err)
			}
			if _, err := os.Stat(filepath.Join(home, backupEntryDB) + suffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("aside remains: %v", err)
			}
		})
	}
}

func TestRecoverRestoreJournalRetainsJournalOnInvariantDamage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		done     bool
		putDst   bool
		putAside bool
	}{
		{name: "partial promotion missing required aside", putDst: true},
		{name: "all done missing promoted destination", done: true, putAside: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			const suffix = ".bak-42-123"
			if tc.putDst {
				writeTarget(t, home, backupEntryDB, "new")
			}
			if tc.putAside {
				writeTarget(t, home, backupEntryDB+suffix, "old")
			}
			j := restoreJournal{Version: 1, PID: 42, AsideSuffix: suffix, Targets: journalTargets(tc.done)}
			// The partial case models a durably promoted target.
			if !tc.done {
				j.Targets[0].Done = true
				j.Targets[1].Staged = true
				j.Targets[1].Done = false
			}
			if err := writeRestoreJournal(restoreJournalPath(home), j); err != nil {
				t.Fatal(err)
			}
			if _, err := recoverRestoreJournal(home); err == nil || !strings.Contains(err.Error(), "invariant failed") {
				t.Fatalf("recovery err=%v", err)
			}
			if _, err := os.Stat(restoreJournalPath(home)); err != nil {
				t.Fatalf("damaged journal was removed: %v", err)
			}
		})
	}
}

func TestOpenStateAndDoctorReportRestoreJournal(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v %s", err, stderr)
	}
	if err := os.WriteFile(restoreJournalPath(home), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "status"); err == nil || !strings.Contains(stderr, "db restore --recover") {
		t.Fatalf("status err=%v stderr=%q", err, stderr)
	}
	stdout, _, err := executeForTest("--home", home, "--root", root, "doctor")
	if err == nil || !strings.Contains(stdout, "restore journal") || !strings.Contains(stdout, "db restore --recover") {
		t.Fatalf("doctor err=%v stdout=%q", err, stdout)
	}
}

func TestMaintenanceLockConflictsAndRunLoopModes(t *testing.T) {
	home := t.TempDir()
	unlock, err := acquireMaintenanceLock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	opts := testOptions(home, filepath.Join(t.TempDir(), "Code"))
	var stdout, stderr bytes.Buffer
	err = runLoopTick(t.Context(), &stdout, &stderr, opts, "", false, true)
	if err == nil || !strings.Contains(err.Error(), "maintenance operation") {
		t.Fatalf("--once tick err=%v", err)
	}
	stderr.Reset()
	if err := runLoopTick(t.Context(), &stdout, &stderr, opts, "", false, false); err != nil {
		t.Fatalf("loop tick should skip: %v", err)
	}
	if !strings.Contains(stderr.String(), "maintenance in progress; skipping this cycle") {
		t.Fatalf("skip notice=%q", stderr.String())
	}
}

func TestRestoreAndDBDownConflictWithHeldMaintenanceLock(t *testing.T) {
	_, root, archive := newFullBackupForTest(t)
	home := filepath.Join(t.TempDir(), "restore")
	unlock, err := acquireMaintenanceLock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, args := range [][]string{{"db", "restore", archive}, {"db", "down"}} {
		_, stderr, err := executeForTest(append([]string{"--home", home, "--root", root}, args...)...)
		if err == nil || ExitCodeWithWriter(err, io.Discard) != exitConflict || !strings.Contains(stderr, "maintenance operation") {
			t.Fatalf("%v err=%v stderr=%q", args, err, stderr)
		}
	}
}

func TestMaintenanceLockBreaksDeadPID(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(repoLockDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(repoLockInfo{PID: 999999, Hostname: hostname(), AcquiredAt: "2026-07-11T00:00:00Z"})
	if err := os.WriteFile(repoLockPath(home, "maintenance"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	realAlive := repoLockProcessAlive
	repoLockProcessAlive = func(int) bool { return false }
	t.Cleanup(func() { repoLockProcessAlive = realAlive })
	unlock, err := acquireMaintenanceLock(home)
	if err != nil {
		t.Fatalf("stale maintenance lock was not broken: %v", err)
	}
	unlock()
}

func TestPlainRestoreAutoRecoversExistingJournal(t *testing.T) {
	_, root, archive := newFullBackupForTest(t)
	home := filepath.Join(t.TempDir(), "restore")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	const suffix = ".bak-42-123"
	writeTarget(t, home, backupEntryDB, "partial-new")
	writeTarget(t, home, backupEntryDB+suffix, "old")
	j := restoreJournal{Version: 1, PID: 42, AsideSuffix: suffix, Targets: journalTargets(false)}
	if err := writeRestoreJournal(restoreJournalPath(home), j); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := executeForTest("--home", home, "--root", root, "db", "restore", archive, "--force")
	if err != nil {
		t.Fatalf("plain restore did not auto-recover: %v stderr=%q", err, stderr)
	}
	if _, err := os.Stat(restoreJournalPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains: %v", err)
	}
}

func TestRecoverJSONIsSingleDocument(t *testing.T) {
	home := t.TempDir()
	stdout, stderr, err := executeForTest("--home", home, "--json", "db", "restore", "--recover")
	if err != nil {
		t.Fatalf("recover: %v stderr=%q", err, stderr)
	}
	var got restoreRecoveryResult
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode stdout %q: %v", stdout, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout has trailing content %q: %v", stdout, err)
	}
}

func TestFullBackupConflictsWithHeldMaintenanceLock(t *testing.T) {
	home, root, _ := newFullBackupForTest(t)
	unlock, err := acquireMaintenanceLock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	out := filepath.Join(t.TempDir(), "blocked.tar")
	_, stderr, err := executeForTest("--home", home, "--root", root, "db", "backup", "--full", out)
	if err == nil || ExitCodeWithWriter(err, io.Discard) != exitConflict || !strings.Contains(stderr, "maintenance operation") {
		t.Fatalf("full backup err=%v stderr=%q", err, stderr)
	}
}

func TestRecoverRejectsUnsafeJournalWithoutMutation(t *testing.T) {
	home := t.TempDir()
	outside := filepath.Join(filepath.Dir(home), "outside-target")
	writeTarget(t, filepath.Dir(outside), filepath.Base(outside), "keep")
	targets := journalTargets(false)
	targets[0].Name = "../outside-target"
	j := restoreJournal{Version: 1, PID: 7, AsideSuffix: ".bak-7-1", Targets: targets}
	if err := writeRestoreJournal(restoreJournalPath(home), j); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverRestoreJournal(home); err == nil || !strings.Contains(err.Error(), "cannot parse restore journal") {
		t.Fatalf("unsafe journal err=%v", err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "keep" {
		t.Fatalf("outside target changed: %q err=%v", got, err)
	}
	if _, err := os.Stat(restoreJournalPath(home)); err != nil {
		t.Fatalf("unsafe journal removed: %v", err)
	}
}

func TestPromotionRollbackFailureRetainsJournal(t *testing.T) {
	home, stage := filepath.Join(t.TempDir(), "home"), filepath.Join(t.TempDir(), "stage")
	for _, name := range backupTargets {
		writeTarget(t, home, name, "old-"+name)
		writeTarget(t, stage, name, "new-"+name)
	}
	realRename := renameBackupTarget
	calls := 0
	renameBackupTarget = func(old, new string) error {
		calls++
		if calls == 7 || calls == 8 {
			return errors.New("injected forward/rollback failure")
		}
		return os.Rename(old, new)
	}
	t.Cleanup(func() { renameBackupTarget = realRename })
	journalPath := restoreJournalPath(home)
	if err := promoteAllTargets(home, stage, journalPath); err == nil || !strings.Contains(err.Error(), "journal retained") {
		t.Fatalf("promotion err=%v", err)
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("journal not retained: %v", err)
	}
}
