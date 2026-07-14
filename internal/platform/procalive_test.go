//go:build darwin || linux || windows

package platform

import (
	"io"
	"math"
	"os"
	"os/exec"
	"testing"
)

// These tests require a build with a real liveness-checking ProcessAlive
// (darwin/linux/windows); the conservative unsupported-platform fallback
// (procalive_other.go) cannot positively confirm a process dead — including
// after a helper process it spawned exits — so it cannot satisfy either
// assertion below (CodeRabbit review, PR #198).

const processAliveHelperEnv = "DEVSTRAP_WANT_PROCESS_ALIVE_HELPER"

func TestProcessAliveRunningAndExitedProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessAliveHelperProcess$", "--")
	cmd.Env = append(os.Environ(), processAliveHelperEnv+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("create helper stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	waited := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	pid := cmd.Process.Pid
	if !ProcessAlive(pid) {
		t.Fatalf("ProcessAlive(%d) = false for running helper", pid)
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("close helper stdin: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for helper process: %v", err)
	}
	waited = true
	if ProcessAlive(pid) {
		t.Fatalf("ProcessAlive(%d) = true after helper exited", pid)
	}
}

func TestProcessAliveRejectsNonexistentPID(t *testing.T) {
	// A process with MaxInt32 as its PID is theoretically possible but rare
	// enough for the same fake-PID convention used elsewhere in this suite.
	if ProcessAlive(math.MaxInt32) {
		t.Fatalf("ProcessAlive(%d) = true for nonexistent PID", math.MaxInt32)
	}
}

func TestProcessAliveHelperProcess(_ *testing.T) {
	if os.Getenv(processAliveHelperEnv) != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}
