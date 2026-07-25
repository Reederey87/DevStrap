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

	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/redact"
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
	// Converger runs one convergence cycle. Optional: a daemon started without
	// one serves reads and reports its own health, but POST /v1/sync returns
	// 503 and no periodic convergence runs. Keeping it optional is what lets
	// the transport be tested without dragging the engine in.
	Converger Converger
	// Interval is the periodic convergence period. Zero disables periodic
	// convergence (on-demand /v1/sync still works when a Converger is set).
	Interval time.Duration
	// Jitter, when set, perturbs each periodic wait. The daemon uses it for the
	// same reason run-loop does: unjittered fleet-wide intervals stampede the hub.
	Jitter func(time.Duration) time.Duration
	// Watcher/WatchFallback/WatchSource enable the watch plane. All three must
	// be set; any missing one leaves the daemon periodic-only, which is a
	// supported configuration rather than an error — the watcher is an
	// optimization, never a correctness dependency.
	Watcher       platform.Watcher
	WatchFallback platform.Watcher
	WatchSource   WatchSource
	// Reader supplies GET /v1/status. Optional: without it that endpoint
	// answers 503, the same way /v1/sync does without a Converger.
	Reader Reader
}

// Server is the daemon's HTTP-over-Unix-socket control API.
type Server struct {
	socketPath string
	version    string
	logger     *slog.Logger
	uid        uint32
	startedAt  time.Time
	mux        *http.ServeMux
	scheduler  *scheduler
	interval   time.Duration
	jitter     func(time.Duration) time.Duration
	watch      *watchPlane
	reader     Reader
	events     *eventBus
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
		uid:      uint32(os.Geteuid()),
		mux:      http.NewServeMux(),
		interval: cfg.Interval,
		jitter:   cfg.Jitter,
		reader:   cfg.Reader,
		events:   newEventBus(),
	}
	if cfg.Converger != nil {
		s.scheduler = newScheduler(cfg.Converger)
		if cfg.Watcher != nil && cfg.WatchSource != nil {
			s.watch = newWatchPlane(cfg.Watcher, cfg.WatchFallback, cfg.WatchSource, s.scheduler, logger)
		}
	}
	s.routes()
	return s, nil
}

// Health is the /v1/health payload.
//
// OK is transport liveness — the daemon answered. Converging/Healthy describe
// the convergence plane separately, because a daemon whose syncs are failing is
// still correctly serving reads and must not report itself as down.
type Health struct {
	OK            bool `json:"ok"`
	UptimeSeconds int  `json:"uptime_seconds"`
	// Converging is false when this daemon has no Converger wired.
	Converging bool `json:"converging"`
	// Healthy is false once a convergence cycle has failed and not yet
	// succeeded; LastError carries the scrubbed reason.
	Healthy             bool   `json:"healthy"`
	LastError           string `json:"last_error,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	LastRunAt           string `json:"last_run_at,omitempty"`
	// Watch reports the filesystem-hint plane. A degraded watcher never makes
	// the daemon unhealthy — correctness rides on periodic convergence — but it
	// must be visible, or a user believes they have sub-interval convergence
	// when they do not.
	Watch WatchHealth `json:"watch"`
}

// WatchHealth is the watch plane's contribution to /v1/health.
type WatchHealth struct {
	Enabled  bool   `json:"enabled"`
	Backend  string `json:"backend,omitempty"`
	Degraded bool   `json:"degraded,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Roots    int    `json:"roots,omitempty"`
	Hints    uint64 `json:"hints,omitempty"`
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
	s.mux.HandleFunc("POST /v1/sync", s.handleSync)
	s.mux.HandleFunc("GET /v1/status", s.handleStatus)
	s.mux.HandleFunc("GET /v1/events", s.handleEvents)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	health := Health{
		OK:            true,
		UptimeSeconds: int(time.Since(s.startedAt).Seconds()),
		Healthy:       true,
	}
	if s.scheduler != nil {
		health.Converging = true
		ch := s.scheduler.snapshot()
		health.ConsecutiveFailures = ch.Failures
		if !ch.LastRunAt.IsZero() {
			health.LastRunAt = ch.LastRunAt.UTC().Format(time.RFC3339)
		}
		if ch.LastErr != nil {
			health.Healthy = false
			health.LastError = redact.Scrub(ch.LastErr.Error())
		}
	}
	if s.watch != nil {
		ws := s.watch.snapshot()
		health.Watch = WatchHealth{
			Enabled:  true,
			Backend:  ws.Backend,
			Degraded: ws.Degraded,
			Reason:   redact.Scrub(ws.Reason),
			Roots:    ws.Roots,
			Hints:    ws.Hints,
		}
	}
	writeJSON(w, http.StatusOK, health)
}

// handleSync triggers a convergence cycle, or joins one already running.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeError(w, http.StatusServiceUnavailable, "this daemon was started without a converger")
		return
	}
	s.events.publish(Event{Kind: EventConvergeStarted, At: time.Now()})
	result, err := s.scheduler.Converge(r.Context(), TickFull)
	if err != nil {
		s.events.publish(Event{Kind: EventConvergeFailed, At: time.Now(), Detail: redact.Scrub(err.Error())})
		s.logger.Warn("daemon: convergence failed", "error", redact.Scrub(err.Error()))
		writeError(w, http.StatusInternalServerError, "convergence failed: "+redact.Scrub(err.Error()))
		return
	}
	s.events.publish(Event{Kind: EventConvergeDone, At: time.Now()})
	writeJSON(w, http.StatusOK, result)
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

// ServeListener serves an already-bound listener until ctx is cancelled.
//
// Callers that must take an action only once the socket is genuinely theirs —
// `devstrap daemon start` writes its pid record, and must not touch a running
// daemon's record when its own bind loses — call Listen themselves, act on
// success, and then hand the listener here. Serve is the convenience wrapper
// that does both.
func (s *Server) ServeListener(ctx context.Context, listener net.Listener) error {
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

	if s.scheduler != nil && s.interval > 0 {
		go s.scheduler.runPeriodic(ctx, s.interval, s.jitter)
	}
	if s.watch != nil {
		go s.watch.run(ctx)
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
