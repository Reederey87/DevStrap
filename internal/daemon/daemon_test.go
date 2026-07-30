package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/platform"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// tempSocketPath returns a socket path short enough to bind.
//
// A Unix socket address is capped by sockaddr_un.sun_path — 104 bytes on
// darwin, 108 on linux — and t.TempDir() embeds the test's name, so a
// descriptively-named test can push the path past the limit and fail at bind
// with a bare "invalid argument". os.MkdirTemp with a short prefix keeps the
// path independent of the test name.
func tempSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ds")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// startServer binds a socket and serves until the test ends, returning the
// socket path.
func startServer(t *testing.T, version string) string {
	t.Helper()
	socket := tempSocketPath(t)

	server, err := New(Config{SocketPath: socket, Version: version, Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()

	waitForSocket(t, socket)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("timeout waiting for Serve to return")
		}
	})
	return socket
}

// waitForSocket blocks until the daemon is accepting, so tests never race
// against bind.
func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon never started accepting on %s", socket)
}

func TestServerServesHealthAndVersion(t *testing.T) {
	socket := startServer(t, "v9.9.9-test")
	client := NewClient(socket)

	health, err := client.Health(t.Context())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.OK {
		t.Fatalf("health = %#v, want OK", health)
	}
	if health.UptimeSeconds < 0 {
		t.Fatalf("uptime = %d, want >= 0", health.UptimeSeconds)
	}

	version, err := client.Version(t.Context())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version.Version != "v9.9.9-test" {
		t.Fatalf("version = %q, want the injected build version", version.Version)
	}
}

func TestClientRecordsAdvertisedDaemonVersion(t *testing.T) {
	socket := startServer(t, "v9.9.9-test")

	client := NewClient(socket)
	if _, err := client.Health(t.Context()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got := client.DaemonVersion(); got != "v9.9.9-test" {
		t.Fatalf("DaemonVersion() = %q, want %q", got, "v9.9.9-test")
	}
}

func TestVersionEndpointCarriesAPIVersion(t *testing.T) {
	socket := startServer(t, "test")

	got, err := NewClient(socket).Version(t.Context())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got.APIVersion != "v1" {
		t.Fatalf("api_version = %q, want %q", got.APIVersion, "v1")
	}
}

// TestSocketAndDirectoryPermissions pins the layered access control: the 0700
// directory is the real gate (a peer that cannot traverse it never reaches the
// socket), and the 0600 socket is defense in depth.
func TestSocketAndDirectoryPermissions(t *testing.T) {
	socket := startServer(t, "test")

	info, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 0600", perm)
	}

	dirInfo, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		t.Fatalf("stat socket dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("socket dir mode = %o, want 0700", perm)
	}
}

// TestListenTakesOverStaleSocket covers the crash-recovery path: a socket file
// with nothing accepting on it is a leftover and must not block a restart.
func TestListenTakesOverStaleSocket(t *testing.T) {
	socket := tempSocketPath(t)

	// Create a real socket, then stop accepting on it without unlinking —
	// exactly what a SIGKILLed daemon leaves behind.
	stale, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	unixListener, ok := stale.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener is %T, want *net.UnixListener", stale)
	}
	unixListener.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatalf("close stale listener: %v", err)
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("stale socket should still exist: %v", err)
	}

	listener, err := Listen(socket)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
}

// TestListenRefusesLiveSocket is the other half: a running daemon must never be
// displaced by a second one starting up.
func TestListenRefusesLiveSocket(t *testing.T) {
	socket := startServer(t, "test")

	_, err := Listen(socket)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Listen err = %v, want ErrAlreadyRunning", err)
	}

	// The original daemon must be unharmed.
	if _, err := NewClient(socket).Health(t.Context()); err != nil {
		t.Fatalf("original daemon stopped serving after a refused takeover: %v", err)
	}
}

// TestListenRefusesNonSocketPath pins that Listen never deletes something it
// did not create.
func TestListenRefusesNonSocketPath(t *testing.T) {
	path := tempSocketPath(t)
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := Listen(path); err == nil {
		t.Fatal("Listen succeeded on a regular file, want an error")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Listen removed a file it did not create: %v", err)
	}
}

func TestListenConstantsAgree(t *testing.T) {
	atLimit := config.Paths{Home: strings.Repeat("h", maxUnixSocketPath-len("/devstrapd.sock"))}
	if got := len(atLimit.SocketPath()); got != maxUnixSocketPath {
		t.Fatalf("test path length = %d, want %d", got, maxUnixSocketPath)
	}
	if err := atLimit.ValidateSocketPath(); err != nil {
		t.Fatalf("config rejected daemon package's at-limit path: %v", err)
	}
	overLimit := config.Paths{Home: atLimit.Home + "h"}
	if err := overLimit.ValidateSocketPath(); err == nil {
		t.Fatal("config accepted daemon package's over-limit path")
	}
}

func TestConcurrentListenTakeoverElectsOneWinner(t *testing.T) {
	socket := tempSocketPath(t)
	stale, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	unixListener, ok := stale.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener is %T, want *net.UnixListener", stale)
	}
	unixListener.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatalf("close stale listener: %v", err)
	}

	const contenders = 8
	start := make(chan struct{})
	results := make(chan struct {
		listener net.Listener
		err      error
	}, contenders)
	var ready sync.WaitGroup
	ready.Add(contenders)
	for range contenders {
		go func() {
			ready.Done()
			<-start
			listener, listenErr := Listen(socket)
			results <- struct {
				listener net.Listener
				err      error
			}{listener: listener, err: listenErr}
		}()
	}
	ready.Wait()
	close(start)

	winners := 0
	for range contenders {
		result := <-results
		if result.err == nil {
			winners++
			t.Cleanup(func() { _ = result.listener.Close() })
			continue
		}
		if !errors.Is(result.err, ErrAlreadyRunning) {
			t.Errorf("losing Listen err = %v, want ErrAlreadyRunning", result.err)
		}
	}
	if winners != 1 {
		t.Fatalf("successful listeners = %d, want exactly 1", winners)
	}
}

// TestRequestHardening covers the header-level guards. A local API must not be
// drivable by a page the user happens to have open, and the fixed host closes a
// confused-deputy class aimed at loopback listeners.
func TestRequestHardening(t *testing.T) {
	socket := startServer(t, "test")
	client := rawClient(socket)

	tests := []struct {
		name    string
		host    string
		headers map[string]string
		want    int
	}{
		{name: "ok", host: socketHost, want: http.StatusOK},
		{name: "empty host allowed", host: "", want: http.StatusOK},
		{name: "origin rejected", host: socketHost, headers: map[string]string{"Origin": "http://evil.example"}, want: http.StatusForbidden},
		{name: "referer rejected", host: socketHost, headers: map[string]string{"Referer": "http://evil.example/page"}, want: http.StatusForbidden},
		{name: "foreign host rejected", host: "evil.example", want: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+socketHost+"/v1/health", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			if tc.host != "" {
				req.Host = tc.host
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if got := resp.Header.Get(versionHeader); got == "" {
				t.Fatalf("missing %s header; version negotiation must work on error responses too", versionHeader)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

func TestWrongMethodIsMethodNotAllowed(t *testing.T) {
	socket := startServer(t, "test")
	client := rawClient(socket)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+socketHost+"/v1/health", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (a wrong method on a real route must not look like a missing route)", resp.StatusCode)
	}
}

// TestShutdownUnlinksSocketAndClientReportsUnavailable covers the full lifecycle
// contract a supervisor depends on.
func TestShutdownUnlinksSocketAndClientReportsUnavailable(t *testing.T) {
	socket := tempSocketPath(t)
	server, err := New(Config{SocketPath: socket, Version: "test", Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForSocket(t, socket)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve err = %v, want nil on graceful shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for shutdown")
	}

	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still present after shutdown (stat err = %v); a stale file would force the next start through takeover", err)
	}
	if _, err := NewClient(socket).Health(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("client err = %v, want ErrUnavailable", err)
	}
}

func TestClientReportsUnavailableWhenNothingListens(t *testing.T) {
	socket := tempSocketPath(t)
	if _, err := NewClient(socket).Health(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestConcurrentClients(t *testing.T) {
	socket := startServer(t, "test")

	const clients = 32
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := NewClient(socket).Health(t.Context()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Health: %v", err)
	}
}

// TestAuthorizePeer covers the authorization decision directly. The
// different-uid cases cannot be exercised end-to-end without root (the test
// process can only connect as itself), so the decision function is unit-tested
// instead — including the case that matters most, root connecting to a socket it
// does not own.
func TestAuthorizePeer(t *testing.T) {
	tests := []struct {
		name      string
		auth      peerAuth
		serverUID uint32
		wantErr   bool
	}{
		{
			name:      "same uid allowed",
			auth:      peerAuth{identity: platform.PeerIdentity{UID: 501}},
			serverUID: 501,
		},
		{
			name:      "different uid refused",
			auth:      peerAuth{identity: platform.PeerIdentity{UID: 502}},
			serverUID: 501,
			wantErr:   true,
		},
		{
			name:      "root refused",
			auth:      peerAuth{identity: platform.PeerIdentity{UID: 0}},
			serverUID: 501,
			wantErr:   true,
		},
		{
			name:      "unresolved identity refused",
			auth:      peerAuth{err: errors.New("peercred failed")},
			serverUID: 501,
			wantErr:   true,
		},
		{
			name:      "unresolved identity refused even when uid happens to match",
			auth:      peerAuth{identity: platform.PeerIdentity{UID: 501}, err: errors.New("peercred failed")},
			serverUID: 501,
			wantErr:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := authorizePeer(tc.auth, tc.serverUID)
			if tc.wantErr && err == nil {
				t.Fatal("authorizePeer succeeded, want refusal")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("authorizePeer err = %v, want nil", err)
			}
		})
	}
}

func TestNewRequiresSocketPath(t *testing.T) {
	if _, err := New(Config{Version: "test"}); err == nil {
		t.Fatal("New succeeded without a socket path, want an error")
	}
}

// rawClient is an http.Client over the socket that does not go through Client,
// so a test can set arbitrary headers.
func rawClient(socket string) *http.Client {
	return &http.Client{
		Timeout: clientTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
	}
}
