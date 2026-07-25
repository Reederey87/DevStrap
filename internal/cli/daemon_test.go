package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/daemon"
)

// TestDaemonStatusReportsNotRunning covers the common case — no daemon — and
// pins that it is reported as a fact rather than as an error. `daemon status`
// must exit 0 whatever the run state, the same contract `service status` has.
func TestDaemonStatusReportsNotRunning(t *testing.T) {
	home := t.TempDir()
	stdout, _, err := executeForTest("--home", home, "daemon", "status")
	if err != nil {
		t.Fatalf("daemon status err = %v, want nil (status must not fail when no daemon runs)", err)
	}
	if !strings.Contains(stdout, "not running") {
		t.Fatalf("stdout = %q, want it to report not running", stdout)
	}
}

func TestDaemonStatusJSONShape(t *testing.T) {
	home := t.TempDir()
	stdout, _, err := executeForTest("--home", home, "--json", "daemon", "status")
	if err != nil {
		t.Fatalf("daemon status --json err = %v", err)
	}
	var result daemonStatusResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if result.Running {
		t.Fatalf("running = true with no daemon started")
	}
	if result.Socket == "" {
		t.Fatal("socket path missing from the JSON payload")
	}
	if !strings.HasSuffix(result.Socket, "devstrapd.sock") {
		t.Fatalf("socket = %q, want it to end in devstrapd.sock", result.Socket)
	}
}

// TestDaemonStopWithNoDaemonSucceeds pins that stopping nothing is a success:
// a supervisor or uninstall path must be able to call it unconditionally.
func TestDaemonStopWithNoDaemonSucceeds(t *testing.T) {
	home := t.TempDir()
	stdout, _, err := executeForTest("--home", home, "daemon", "stop")
	if err != nil {
		t.Fatalf("daemon stop err = %v, want nil when nothing is running", err)
	}
	if !strings.Contains(stdout, "no daemon is running") {
		t.Fatalf("stdout = %q, want it to say no daemon is running", stdout)
	}
}

// TestDaemonStopClearsStaleRecord covers a crashed daemon: the pid file
// survives, but the process is gone. Stop must clear the record rather than
// signalling whatever now owns that PID.
func TestDaemonStopClearsStaleRecord(t *testing.T) {
	home := t.TempDir()
	// 999999999 cannot be a live pid: it exceeds Linux's pid_max ceiling (2^22)
	// and darwin's PID_MAX (99999), so kill(pid, 0) is always ESRCH. Using a
	// real-looking pid would race whatever actually owns that number.
	writeTestRecord(t, home, daemonRecord{PID: 999999999, StartedAt: 1, Socket: filepath.Join(home, "devstrapd.sock")})

	stdout, _, err := executeForTest("--home", home, "daemon", "stop")
	if err != nil {
		t.Fatalf("daemon stop err = %v", err)
	}
	if !strings.Contains(stdout, "stale") {
		t.Fatalf("stdout = %q, want it to report clearing a stale record", stdout)
	}
	if _, statErr := os.Stat(daemonRecordPath(home)); !os.IsNotExist(statErr) {
		t.Fatalf("stale record still present (stat err = %v)", statErr)
	}
}

// TestDaemonRecordAliveRejectsRecycledPID is the PID-reuse guard. A live PID
// whose start-time identity differs from the record is a different process that
// happens to have inherited the number — treating it as the daemon would mean
// `daemon stop` signalling an unrelated process.
func TestDaemonRecordAliveRejectsRecycledPID(t *testing.T) {
	self := os.Getpid()
	actual, err := processStartTime(self)
	if err != nil {
		t.Skipf("process start time unavailable on this platform: %v", err)
	}

	if !daemonRecordAlive(daemonRecord{PID: self, StartedAt: actual}) {
		t.Fatal("a record matching this live process was judged dead")
	}
	if daemonRecordAlive(daemonRecord{PID: self, StartedAt: actual + 1}) {
		t.Fatal("a live PID with a mismatched start identity was judged alive; a recycled PID would be signalled")
	}
	if daemonRecordAlive(daemonRecord{PID: 0}) {
		t.Fatal("PID 0 judged alive")
	}
	// A record with no recorded identity degrades to liveness-only rather than
	// being treated as dead.
	if !daemonRecordAlive(daemonRecord{PID: self, StartedAt: 0}) {
		t.Fatal("a record without a start identity should fall back to liveness")
	}
}

// TestDaemonStartServesAndStops drives the real command end to end: start it in
// the background, confirm status sees it over the socket, then stop it.
//
// The CLI runs in-process here, so the recorded PID is the test binary's and
// never exits. That is exactly why `daemon stop` treats "the control API stopped
// answering" as stopped in addition to process death — this test would hang on
// PID liveness alone.
func TestDaemonStartServesAndStops(t *testing.T) {
	// Keep the socket path short: sockaddr_un caps the address (104 bytes on
	// darwin), and t.TempDir() embeds the test name.
	home, err := os.MkdirTemp("", "dh")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// `daemon start` preflights the hub the same way run-loop does, so the test
	// supplies the file-backed test hub. Periodic convergence is disabled
	// (--interval 0) because this test is about the socket lifecycle, not
	// convergence; leaving it on would run real sync cycles against an
	// uninitialized store.
	hubFile := filepath.Join(home, "hub.json")
	started := make(chan error, 1)
	go func() {
		started <- executeForTestContext(ctx, "--home", home, "daemon", "start",
			"--hub-file", hubFile, "--interval", "0")
	}()

	socket := filepath.Join(home, "devstrapd.sock")
	waitForFile(t, socket)

	stdout, _, err := executeForTest("--home", home, "daemon", "status")
	if err != nil {
		t.Fatalf("daemon status err = %v", err)
	}
	if !strings.Contains(stdout, "running") || strings.Contains(stdout, "not running") {
		t.Fatalf("stdout = %q, want it to report a running daemon", stdout)
	}

	if _, _, err := executeForTest("--home", home, "daemon", "stop"); err != nil {
		t.Fatalf("daemon stop err = %v", err)
	}

	select {
	case err := <-started:
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("daemon start returned %v, want a clean shutdown", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("daemon did not exit after stop")
	}
}

// executeForTestContext runs the CLI with a caller-supplied context so a
// foreground command (daemon start) can be cancelled by the test.
func executeForTestContext(ctx context.Context, args ...string) error {
	var stdout, stderr bytes.Buffer
	cmd := NewRootCommand(&stdout, &stderr)
	cmd.SetArgs(args)
	return cmd.ExecuteContext(ctx)
}

func writeTestRecord(t *testing.T, home string, record daemonRecord) {
	t.Helper()
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if err := os.WriteFile(daemonRecordPath(home), payload, 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// TestSecondDaemonStartLeavesRunningDaemonsRecordIntact is a regression test for
// a confirmed bug found in review: `daemon start` used to write its pid record
// BEFORE binding the socket, and register the removal defer before the bind
// could fail. A losing second start therefore overwrote the running daemon's
// record and then deleted it outright, leaving a live daemon with no record —
// `daemon stop` reported "no daemon is running" while one was.
//
// Note what this test can and cannot see: the CLI runs in-process, so both
// "daemons" share a pid and the OVERWRITE half is invisible here. What it pins
// is the DELETION, which is the half that actually breaks `stop`. Mutation-
// checked against the original ordering.
func TestSecondDaemonStartLeavesRunningDaemonsRecordIntact(t *testing.T) {
	home, err := os.MkdirTemp("", "dh")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Same preflight/interval reasoning as TestDaemonStartServesAndStops: this
	// test is about the pid record, not convergence.
	hubFile := filepath.Join(home, "hub.json")
	started := make(chan error, 1)
	go func() {
		started <- executeForTestContext(ctx, "--home", home, "daemon", "start",
			"--hub-file", hubFile, "--interval", "0")
	}()
	waitForFile(t, filepath.Join(home, "devstrapd.sock"))

	first, err := readDaemonRecord(home)
	if err != nil {
		t.Fatalf("first daemon wrote no record: %v", err)
	}

	// A second start must lose, and must not disturb the first's record.
	if _, _, err := executeForTest("--home", home, "daemon", "start",
		"--hub-file", hubFile, "--interval", "0"); err == nil {
		t.Fatal("second daemon start succeeded, want a conflict")
	}

	after, err := readDaemonRecord(home)
	if err != nil {
		t.Fatalf("the running daemon's record was deleted by a losing second start: %v", err)
	}
	if after.PID != first.PID {
		t.Fatalf("record pid = %d, want the running daemon's %d — a losing start overwrote it", after.PID, first.PID)
	}

	// And stop must still find it.
	stdout, _, err := executeForTest("--home", home, "daemon", "stop")
	if err != nil {
		t.Fatalf("daemon stop: %v", err)
	}
	if strings.Contains(stdout, "no daemon is running") {
		t.Fatalf("stop reported no daemon while one was running: %q", stdout)
	}
	<-started
}

// TestLosingDaemonStartWritesNoJSONDocument pins the PROPERTY rather than the
// current implementation: a start that never bound the socket must not announce
// itself on stdout. Today that holds because the render sits after
// `daemon.Listen`, but nothing structurally prevents a future edit from moving a
// render back above the bind — which is exactly how the original bug shipped (a
// losing start printed a success document, with a pid about to exit, while
// stderr reported the conflict).
//
// It needs no second daemon: binding the socket directly is enough to make the
// CLI's start lose.
func TestLosingDaemonStartWritesNoJSONDocument(t *testing.T) {
	home, err := os.MkdirTemp("", "dh")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	listener, err := daemon.Listen(filepath.Join(home, "devstrapd.sock"))
	if err != nil {
		t.Fatalf("pre-bind socket: %v", err)
	}
	defer func() { _ = listener.Close() }()

	stdout, _, err := executeForTest("--home", home, "--json", "daemon", "start",
		"--hub-file", filepath.Join(home, "hub.json"), "--interval", "0")
	if err == nil {
		t.Fatal("daemon start succeeded against an occupied socket, want a conflict")
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("a start that never bound wrote to stdout: %q", stdout)
	}
	// And it must not have disturbed anything on disk either.
	if _, statErr := os.Stat(daemonRecordPath(home)); !os.IsNotExist(statErr) {
		t.Fatalf("a losing start left a pid record behind (stat err = %v)", statErr)
	}
}
