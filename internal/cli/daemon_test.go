package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/daemon"
)

type daemonTestConverger struct{}

func (daemonTestConverger) Converge(_ context.Context, mode daemon.TickMode) (daemon.Result, error) {
	return daemon.Result{
		Mode:       mode,
		StartedAt:  time.Now(),
		DurationMS: 12,
	}, nil
}

// shortTestDir returns a state home short enough for a Unix socket path.
//
// t.TempDir() embeds the test name, which on darwin routinely pushes
// <home>/devstrapd.sock past sockaddr_un's 104-byte sun_path — and now that
// Listen validates the length up front, such a test fails on the guard rather
// than on a bare EINVAL. This is the same workaround the daemon package uses;
// it is a test-environment constraint, not a product one.
func shortTestDir(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "dh")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return home
}

func startDaemonForCLITest(t *testing.T, converger daemon.Converger, advertisedVersion ...string) string {
	t.Helper()
	home, err := os.MkdirTemp("", "dh")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	socket := filepath.Join(home, "devstrapd.sock")
	daemonVersion := "test"
	if len(advertisedVersion) > 0 {
		daemonVersion = advertisedVersion[0]
	}
	server, err := daemon.New(daemon.Config{SocketPath: socket, Version: daemonVersion, Converger: converger})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForFile(t, socket)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon Serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon Serve did not stop")
		}
	})
	return home
}

func withCLIVersion(t *testing.T, value string) {
	t.Helper()
	previous := version
	version = value
	t.Cleanup(func() { version = previous })
}

func TestDaemonCommandWarnsOnVersionSkew(t *testing.T) {
	withCLIVersion(t, "0.1.3")
	home := startDaemonForCLITest(t, daemonTestConverger{}, "0.1.2")

	_, stderr, err := executeForTest("--home", home, "daemon", "sync")
	if err != nil {
		t.Fatalf("daemon sync: %v", err)
	}
	if !strings.Contains(stderr, "daemon is version 0.1.2 but this CLI is 0.1.3") ||
		!strings.Contains(stderr, "devstrap daemon stop && devstrap daemon start") {
		t.Fatalf("stderr = %q, want both versions and restart remedy", stderr)
	}
}

// TestVersionsSkewSilenceRules pins the exact silence contract spec/13 commits
// to: "equal, empty, unknown, and development versions are silent". Only the
// equal case had coverage, and the untested ones are the dangerous ones — a
// daemon started without a version normalizes to "unknown", so treating that as
// a real version would warn on every single command.
func TestVersionsSkewSilenceRules(t *testing.T) {
	cases := []struct {
		daemon, cli string
		want        bool
	}{
		{"0.1.3", "0.1.3", false}, // equal
		{"", "0.1.3", false},      // no header advertised
		{"0.1.3", "", false},      // CLI built without a version
		{"unknown", "0.1.3", false},
		{"0.1.3", "unknown", false},
		{"dev", "0.1.3", false},
		{"0.1.3", "dev", false},
		{"0.1.2", "0.1.3", true}, // the only warning case
		{"0.1.3", "0.1.2", true}, // and it is symmetric: no ordering is implied
	}
	for _, tc := range cases {
		if got := versionsSkew(tc.daemon, tc.cli); got != tc.want {
			t.Errorf("versionsSkew(%q, %q) = %v, want %v", tc.daemon, tc.cli, got, tc.want)
		}
	}
}

// startForeignProtocolDaemon serves the daemon's read routes while advertising an
// api_version this CLI does not know. The real server cannot do that — it always
// reports its own compiled-in protocol — so testing the unsupported-protocol path
// needs a hand-rolled listener. Peer credentials are deliberately not enforced:
// the CLIENT is under test, and it does not check them.
func startForeignProtocolDaemon(t *testing.T, buildVersion, apiVersion string) string {
	t.Helper()
	home := shortTestDir(t)
	socket := filepath.Join(home, "devstrapd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen %s: %v", socket, err)
	}

	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Devstrap-Daemon-Version", buildVersion)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"uptime":"1m0s","converging":false}`)
	})
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"version":"`+buildVersion+`","api_version":"`+apiVersion+`"}`)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return home
}

// TestDaemonStatusReportsProtocolMismatchWithoutFailing pins the contract that
// `daemon status` exits 0 whatever the run state. A protocol the CLI cannot
// interpret is exactly when a user reaches for status — making it the one
// command that refuses to run would be backwards. The field is reported, not
// returned as an error.
//
// An earlier version of this test asserted `api_mismatch == ""` against a normal
// v1 daemon, so it could never have caught the regression it is named for.
func TestDaemonStatusReportsProtocolMismatchWithoutFailing(t *testing.T) {
	withCLIVersion(t, "0.1.3")
	home := startForeignProtocolDaemon(t, "0.9.9", "v2")

	stdout, stderr, err := executeForTest("--home", home, "--json", "daemon", "status")
	if err != nil {
		t.Fatalf("daemon status err = %v, want nil (status must exit 0 even here)", err)
	}
	var result daemonStatusResult
	if jerr := json.Unmarshal([]byte(stdout), &result); jerr != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", jerr, stdout)
	}
	if !result.Running {
		t.Fatal("running = false against a live daemon")
	}
	if result.APIMismatch != "v2" {
		t.Fatalf("api_mismatch = %q, want %q", result.APIMismatch, "v2")
	}
	// The build version rode the response HEADER even though the body was
	// uninterpretable, so neither the version nor the skew may go missing in
	// exactly the situation a user runs `status` to understand.
	if result.Version != "0.9.9" {
		t.Fatalf("version = %q, want the header-advertised %q", result.Version, "0.9.9")
	}
	if !result.VersionSkew {
		t.Fatal("version_skew = false for a 0.9.9 daemon against a 0.1.3 CLI")
	}
	// `status` IS the report: it renders the skew itself, so it must not ALSO
	// emit the stderr warning that exists for commands which would otherwise say
	// nothing. One condition, one place.
	if stderr != "" {
		t.Fatalf("stderr = %q, want silence: status reports the skew in its own output", stderr)
	}

	// And the human path must carry both facts, since suppressing the stderr
	// warning leaves this as the only place a non-JSON user sees them.
	human, _, herr := executeForTest("--home", home, "daemon", "status")
	if herr != nil {
		t.Fatalf("human daemon status err = %v, want nil", herr)
	}
	for _, want := range []string{"version skew: daemon 0.9.9, CLI 0.1.3", "protocol mismatch", "v2"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human status = %q, want %q", human, want)
		}
	}
}

// TestDaemonStatusReportsNoMismatchForItsOwnProtocol is the negative half: the
// field must stay absent rather than defaulting to something a consumer would
// read as a mismatch.
func TestDaemonStatusReportsNoMismatchForItsOwnProtocol(t *testing.T) {
	home := startDaemonForCLITest(t, daemonTestConverger{})

	stdout, _, err := executeForTest("--home", home, "--json", "daemon", "status")
	if err != nil {
		t.Fatalf("daemon status err = %v, want nil", err)
	}
	var result daemonStatusResult
	if jerr := json.Unmarshal([]byte(stdout), &result); jerr != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", jerr, stdout)
	}
	if result.APIMismatch != "" {
		t.Fatalf("api_mismatch = %q against a same-protocol daemon", result.APIMismatch)
	}
}

// TestDaemonSyncRefusesAnUnsupportedProtocol pins the other half of the
// reported-vs-returned split. A command that must actually SPEAK to the daemon
// cannot interpret a result document from a protocol it does not know, so it
// fails instead of reporting a cycle it cannot read — the distinction spec/13
// draws between `daemon status` and `daemon events`/`daemon sync`.
func TestDaemonSyncRefusesAnUnsupportedProtocol(t *testing.T) {
	home := startForeignProtocolDaemon(t, "0.9.9", "v2")

	_, _, err := executeForTest("--home", home, "daemon", "sync")
	if err == nil {
		t.Fatal("daemon sync succeeded against an uninterpretable protocol")
	}
	var apiErr *daemon.UnsupportedAPIVersionError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want UnsupportedAPIVersionError", err)
	}
	if apiErr.APIVersion != "v2" {
		t.Fatalf("APIVersion = %q, want %q", apiErr.APIVersion, "v2")
	}
}

func TestDaemonCommandSilentWhenVersionsMatch(t *testing.T) {
	withCLIVersion(t, "0.1.3")
	home := startDaemonForCLITest(t, daemonTestConverger{}, "0.1.3")

	_, stderr, err := executeForTest("--home", home, "daemon", "sync")
	if err != nil {
		t.Fatalf("daemon sync: %v", err)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want completely silent happy path", stderr)
	}
}

func TestDaemonStatusJSONCarriesSkew(t *testing.T) {
	withCLIVersion(t, "0.1.3")
	skewedHome := startDaemonForCLITest(t, nil, "0.1.2")
	stdout, stderr, err := executeForTest("--home", skewedHome, "--json", "daemon", "status")
	if err != nil {
		t.Fatalf("skewed daemon status: %v", err)
	}
	var skewed daemonStatusResult
	if err := json.Unmarshal([]byte(stdout), &skewed); err != nil {
		t.Fatalf("decode skewed status: %v", err)
	}
	if skewed.CLIVersion != "0.1.3" || !skewed.VersionSkew {
		t.Fatalf("skewed status = %+v, want cli_version and version_skew", skewed)
	}
	if strings.Contains(stdout, "warning:") {
		t.Fatalf("stdout = %q, warning must not corrupt JSON", stdout)
	}
	// `status` reports the skew in its own document rather than duplicating it as
	// a stderr warning; see TestDaemonStatusReportsProtocolMismatchWithoutFailing.
	if stderr != "" {
		t.Fatalf("stderr = %q, want status to report the skew only in its output", stderr)
	}

	matchedHome := startDaemonForCLITest(t, nil, "0.1.3")
	stdout, stderr, err = executeForTest("--home", matchedHome, "--json", "daemon", "status")
	if err != nil {
		t.Fatalf("matched daemon status: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("decode matched status: %v", err)
	}
	if _, ok := raw["cli_version"]; ok {
		t.Fatalf("matched JSON contains cli_version: %s", stdout)
	}
	if _, ok := raw["version_skew"]; ok {
		t.Fatalf("matched JSON contains version_skew: %s", stdout)
	}
	if stderr != "" {
		t.Fatalf("matched stderr = %q, want silent happy path", stderr)
	}
}

func TestDoctorWarnsOnDaemonVersionSkew(t *testing.T) {
	withCLIVersion(t, "0.1.3")
	home := startDaemonForCLITest(t, nil, "0.1.2")

	got := checkDaemonVersion(t.Context(), config.Paths{Home: home})
	if len(got) != 1 || got[0].Status != checkWarn ||
		!strings.Contains(got[0].Detail, "0.1.2") ||
		!strings.Contains(got[0].Detail, "0.1.3") ||
		got[0].Remedy != "devstrap daemon stop && devstrap daemon start" {
		t.Fatalf("checkDaemonVersion = %+v, want warning with both versions and exact remedy", got)
	}
}

func TestDoctorSkipsDaemonVersionWhenNotRunning(t *testing.T) {
	home := shortTestDir(t)
	got := checkDaemonVersion(t.Context(), config.Paths{Home: home})
	if len(got) != 1 || got[0].Status != checkOK || !strings.Contains(got[0].Detail, "skipped") {
		t.Fatalf("checkDaemonVersion = %+v, want OK/skipped", got)
	}
}

// TestDaemonStatusReportsNotRunning covers the common case — no daemon — and
// pins that it is reported as a fact rather than as an error. `daemon status`
// must exit 0 whatever the run state, the same contract `service status` has.
func TestDaemonStatusReportsNotRunning(t *testing.T) {
	home := shortTestDir(t)
	stdout, _, err := executeForTest("--home", home, "daemon", "status")
	if err != nil {
		t.Fatalf("daemon status err = %v, want nil (status must not fail when no daemon runs)", err)
	}
	if !strings.Contains(stdout, "not running") {
		t.Fatalf("stdout = %q, want it to report not running", stdout)
	}
}

func TestDaemonStatusJSONShape(t *testing.T) {
	home := shortTestDir(t)
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

// TestDaemonStartOnOverLongSocketPathIsConfigError pins that one condition has
// one exit class. Listen returns a plain error and runDaemonStart maps only
// ErrAlreadyRunning, so without an explicit check the same over-long path
// exits generic(1) here while exiting exitInvalidConfig(2) from every client
// command and from `service install --daemon`.
func TestDaemonStartOnOverLongSocketPathIsConfigError(t *testing.T) {
	home := filepath.Join(t.TempDir(), strings.Repeat("d", 120))
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, _, err := executeForTest("--home", home, "daemon", "start", "--hub-file", filepath.Join(t.TempDir(), "hub.json"))
	if err == nil {
		t.Fatal("daemon start succeeded with an unbindable socket path")
	}
	if got := ExitCode(err); got != exitInvalidConfig {
		t.Fatalf("exit code = %d, want %d (same class as the client commands)", got, exitInvalidConfig)
	}
}

func TestClientCommandOnOverLongSocketPathIsConfigError(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), strings.Repeat("h", 120))
	_, stderr, err := executeForTest("--home", home, "daemon", "status")
	if err == nil {
		t.Fatal("daemon status accepted an over-long socket path")
	}
	var app appError
	if !errors.As(err, &app) || app.code != exitInvalidConfig {
		t.Fatalf("err = %v, want appError code %d", err, exitInvalidConfig)
	}
	if strings.Contains(strings.ToLower(stderr), "invalid argument") {
		t.Fatalf("stderr = %q, want actionable validation rather than raw EINVAL", stderr)
	}
	if !strings.Contains(stderr, "choose a shorter state home") {
		t.Fatalf("stderr = %q, want shorter-home remedy", stderr)
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

// TestDaemonEventsWithoutDaemonReturnsExitCode3 pins the wave's first genuine
// use of the long-reserved exitDaemonUnavailable.
//
// It matters that this is `daemon events` and not `status` or `sync`: every
// other command has a local path that works without a daemon, so returning
// "daemon unavailable" for them would be a regression. A live event stream has
// no daemonless equivalent, which is exactly what makes exit code 3 meaningful
// here rather than merely available.
func TestDaemonEventsWithoutDaemonReturnsExitCode3(t *testing.T) {
	// Short path deliberately: this test's own name makes t.TempDir() long
	// enough to exceed sockaddr_un's 104-byte cap on darwin, and a too-long
	// address fails the dial with EINVAL rather than ENOENT — which
	// isUnavailable correctly does NOT treat as "no daemon", so the assertion
	// below would fail for a reason unrelated to what it is testing.
	home, err := os.MkdirTemp("", "dh")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	_, _, err = executeForTest("--home", home, "daemon", "events")
	if err == nil {
		t.Fatal("daemon events succeeded with no daemon running")
	}
	if got := ExitCode(err); got != exitDaemonUnavailable {
		t.Fatalf("exit code = %d, want %d (exitDaemonUnavailable)", got, exitDaemonUnavailable)
	}
}

func TestDaemonSyncAgainstRunningDaemon(t *testing.T) {
	home := startDaemonForCLITest(t, daemonTestConverger{})
	stdout, _, err := executeForTest("--home", home, "--json", "daemon", "sync", "--namespace-only")
	if err != nil {
		t.Fatalf("daemon sync: %v", err)
	}
	var result daemonSyncResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if result.Mode != daemon.TickNamespaceOnly {
		t.Fatalf("result = %+v, want namespace-only mode", result)
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &shape); err != nil {
		t.Fatalf("decode JSON shape: %v", err)
	}
	if _, ok := shape["duration_ms"]; !ok {
		t.Fatalf("JSON shape = %v, want duration_ms field", shape)
	}
}

func TestDaemonSyncWithoutDaemonReturnsExitCode3(t *testing.T) {
	home, err := os.MkdirTemp("", "dh")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	_, _, err = executeForTest("--home", home, "daemon", "sync")
	if err == nil {
		t.Fatal("daemon sync succeeded with no daemon running")
	}
	if got := ExitCode(err); got != exitDaemonUnavailable {
		t.Fatalf("exit code = %d, want %d", got, exitDaemonUnavailable)
	}
}

// blockingNamespaceConverger holds a namespace-only cycle open so a concurrent
// full request is forced to JOIN it, reproducing the coalescing case where the
// caller observes a weaker cycle than the one it asked for.
type blockingNamespaceConverger struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (c *blockingNamespaceConverger) Converge(ctx context.Context, mode daemon.TickMode) (daemon.Result, error) {
	c.once.Do(func() { close(c.entered) })
	select {
	case <-c.release:
	case <-ctx.Done():
		return daemon.Result{}, ctx.Err()
	}
	return daemon.Result{Mode: mode, StartedAt: time.Now(), DurationMS: 3}, nil
}

// TestDaemonSyncDoesNotClaimSuccessForWorkItDidNotDo is the honesty contract.
//
// A full `daemon sync` arriving while a namespace-only cycle is in flight joins
// that cycle and gets back mode=namespace-only, coalesced=true — no
// materialization happened. Exiting 0 with a plain "converged" line would tell
// a script its full sync ran when it did not.
func TestDaemonSyncDoesNotClaimSuccessForWorkItDidNotDo(t *testing.T) {
	c := &blockingNamespaceConverger{release: make(chan struct{}), entered: make(chan struct{})}
	home := startDaemonForCLITest(t, c)

	// Occupy the scheduler with a namespace-only cycle.
	go func() { _, _, _ = executeForTest("--home", home, "daemon", "sync", "--namespace-only") }()
	<-c.entered

	done := make(chan string, 1)
	go func() {
		stdout, _, err := executeForTest("--home", home, "--json", "daemon", "sync")
		if err != nil {
			t.Errorf("daemon sync: %v", err)
		}
		done <- stdout
	}()
	time.Sleep(150 * time.Millisecond)
	close(c.release)

	stdout := <-done
	var result daemonSyncResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if result.Mode == daemon.TickFull {
		// The retry claimed the next cycle, which is the good outcome.
		if result.Deferred {
			t.Fatal("reported deferred while also reporting a full cycle")
		}
		return
	}
	// Otherwise it MUST say so rather than implying the full sync ran.
	if !result.Deferred || result.RequestedMode != daemon.TickFull {
		t.Fatalf("observed a %s cycle for a full request but did not report it as deferred: %+v", result.Mode, result)
	}
}

func TestDaemonSyncAgainstConvergerlessDaemonIsNotUnavailable(t *testing.T) {
	home := startDaemonForCLITest(t, nil)
	_, _, err := executeForTest("--home", home, "daemon", "sync")
	if err == nil {
		t.Fatal("daemon sync succeeded against convergerless daemon")
	}
	if got := ExitCode(err); got == exitDaemonUnavailable {
		t.Fatalf("exit code = %d, must not report unavailable when daemon answered", got)
	}
	// The message must NOT prescribe a flag: `devstrap daemon start` always
	// wires a converger, so a convergerless daemon is never a user
	// misconfiguration and telling the user to restart with one would send
	// them chasing a setting that was never wrong.
	if !strings.Contains(err.Error(), "transport-only") {
		t.Fatalf("error = %q, want the transport-only explanation", err)
	}
	if strings.Contains(err.Error(), "--hub-file") {
		t.Fatalf("error = %q, must not prescribe a flag for a state the CLI cannot produce", err)
	}
}
