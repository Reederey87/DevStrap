package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

// versionHeader carries the daemon's build version on every response, so a
// client CAN detect skew against its own binary. It is a header rather than a
// body field so it is available on error responses too.
//
// Precisely: this is version ADVERTISEMENT. Nothing reads it yet — client-side
// skew handling arrives with a later slice — so do not describe it as
// negotiation until a consumer exists.
const versionHeader = "Devstrap-Daemon-Version"

const (
	// readHeaderTimeout bounds header reads (gosec G112: an unbounded header
	// read is a slowloris vector even on a local socket, since any process
	// running as this user can open one).
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 2 * time.Minute
	// shutdownTimeout bounds the graceful drain before in-flight requests are
	// cut off.
	shutdownTimeout = 5 * time.Second
)

// Config is the daemon's injected dependencies. Everything the package needs
// from the rest of DevStrap arrives here rather than through an import, so this
// package never reaches into command code.
type Config struct {
	// SocketPath is where the daemon listens. Required.
	SocketPath string
	// Version is the build version reported by /v1/version and the response
	// header. Injected so this package does not import the CLI.
	Version string
	// Logger receives operational logs. Defaults to slog.Default when nil.
	Logger *slog.Logger
}

// Server is the daemon's HTTP-over-Unix-socket control API.
type Server struct {
	socketPath string
	version    string
	logger     *slog.Logger
	uid        uint32
	startedAt  time.Time
	mux        *http.ServeMux
}

// New validates cfg and builds a Server. It does not touch the filesystem;
// the socket is created by Serve.
func New(cfg Config) (*Server, error) {
	if cfg.SocketPath == "" {
		return nil, errors.New("daemon: SocketPath is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	version := cfg.Version
	if version == "" {
		version = "unknown"
	}

	s := &Server{
		socketPath: cfg.SocketPath,
		version:    version,
		logger:     logger,
		// Geteuid, not Getuid: socket and file ownership follow the EFFECTIVE
		// uid, so that is what a peer's uid must match. The two are identical in
		// every supported deployment and diverge only under setuid, but matching
		// the ownership semantics is the correct comparison either way.
		//nolint:gosec // A uid; the conversion is not a truncation risk.
		uid: uint32(os.Geteuid()),
		mux: http.NewServeMux(),
	}
	s.routes()
	return s, nil
}

// Health is the /v1/health payload.
type Health struct {
	OK            bool `json:"ok"`
	UptimeSeconds int  `json:"uptime_seconds"`
}

// Version is the /v1/version payload.
type Version struct {
	Version string `json:"version"`
}

func (s *Server) routes() {
	// Go 1.22+ method-prefixed patterns give a 405 for a wrong method on a
	// registered path, rather than the 404 a bare path pattern would produce.
	s.mux.HandleFunc("GET /v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /v1/version", s.handleVersion)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Health{
		OK:            true,
		UptimeSeconds: int(time.Since(s.startedAt).Seconds()),
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Version{Version: s.version})
}

// Serve binds the socket and serves until ctx is cancelled, then drains
// in-flight requests and returns. The socket is unlinked on return.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := Listen(s.socketPath)
	if err != nil {
		return err
	}
	return s.serveListener(ctx, listener)
}

// serveListener is the testable core of Serve: it takes an already-bound
// listener so tests can drive the server without racing on socket creation.
func (s *Server) serveListener(ctx context.Context, listener net.Listener) error {
	s.startedAt = time.Now()
	srv := &http.Server{
		Handler:           s.guard(s.mux),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		ConnContext:       connContext,
		// The daemon's own logs go through slog; net/http's default error
		// logger would bypass that and write unstructured lines to stderr.
		ErrorLog: slog.NewLogLogger(s.logger.Handler(), slog.LevelDebug),
	}

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	s.logger.Info("daemon: listening", "socket", s.socketPath, "version", s.version)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	// Shutdown closes the listener, which unlinks the socket.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("daemon: shutdown: %w", err)
	}
	if err := <-serveErr; err != nil {
		return err
	}
	s.logger.Info("daemon: stopped")
	return nil
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeError returns a generic, non-revealing message to the caller; the
// specific reason is logged server-side instead.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorBody{Error: message})
}
