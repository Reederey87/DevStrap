//go:build linux

package platform

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLandlockCommandRejectsEmptyArgv(t *testing.T) {
	sc, err := LandlockSandbox{}.Command(context.Background(), SandboxSpec{}, nil)
	if err == nil || !strings.Contains(err.Error(), "empty argv") {
		t.Fatalf("Command() err = %v, want empty argv guard", err)
	}
	sc.Cleanup()
}

func TestLandlockCommandWrapsArgvWithHelper(t *testing.T) {
	sb := LandlockSandbox{}
	if err := sb.Available(); err != nil {
		t.Skipf("landlock unavailable: %v", err)
	}
	root := t.TempDir()
	worktree := filepath.Join(root, "wt")
	logs := filepath.Join(root, "logs")
	tmpDir := filepath.Join(root, "tmp")
	for _, dir := range []string{worktree, logs, tmpDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	worktreeLink := filepath.Join(root, "wt-link")
	if err := os.Symlink(worktree, worktreeLink); err != nil {
		t.Fatal(err)
	}
	resolvedWorktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{"/bin/true", "--flag"}
	sc, err := sb.Command(context.Background(), SandboxSpec{
		WorktreeDir: worktreeLink,
		TmpDir:      tmpDir,
		LogDir:      logs,
	}, argv)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Cleanup()
	wrapped := sc.Argv
	if wrapped[0] != self {
		t.Fatalf("wrapped[0] = %q, want %q", wrapped[0], self)
	}
	if wrapped[1] != SandboxHelperCommand || wrapped[2] != "--spec" || wrapped[4] != "--" {
		t.Fatalf("wrapped helper prefix = %v, want self, %q, --spec, <json>, --", wrapped[:5], SandboxHelperCommand)
	}
	if !slices.Equal(wrapped[5:], argv) {
		t.Fatalf("wrapped tail = %v, want %v", wrapped[5:], argv)
	}
	var gotSpec SandboxSpec
	if err := json.Unmarshal([]byte(wrapped[3]), &gotSpec); err != nil {
		t.Fatal(err)
	}
	if gotSpec.WorktreeDir != resolvedWorktree {
		t.Fatalf("WorktreeDir = %q, want resolved %q", gotSpec.WorktreeDir, resolvedWorktree)
	}
}

func TestExecSandboxHelperGuards(t *testing.T) {
	if err := ExecSandboxHelper(SandboxSpec{}, nil); err == nil || !strings.Contains(err.Error(), "empty argv") {
		t.Fatalf("ExecSandboxHelper empty argv err = %v, want empty argv guard", err)
	}
	if err := ExecSandboxHelper(SandboxSpec{WorktreeDir: "relative/wt"}, []string{"true"}); err == nil || !strings.Contains(err.Error(), "non-absolute") {
		t.Fatalf("ExecSandboxHelper relative worktree err = %v, want non-absolute guard", err)
	}
}
