package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Reederey87/DevStrap/internal/daemon"
	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/platform"
)

// daemonPIDFile records the running daemon's identity next to its socket. The
// socket is the authoritative single-instance guard — only one process can bind
// it — so this file exists for reporting and for `daemon stop`, never as the
// lock itself.
const daemonPIDFile = "devstrapd.pid"

// stopPollInterval/stopTimeout bound how long `daemon stop` waits for the
// signalled process to actually exit before reporting failure.
const (
	stopPollInterval = 50 * time.Millisecond
	stopTimeout      = 15 * time.Second
)

// daemonRecord is the on-disk pid file. StartedAt is an opaque platform
// start-time identity, not a timestamp: it exists so a recycled PID is not
// mistaken for the daemon (the same PID-reuse guard `repo_lock.go` uses).
type daemonRecord struct {
	PID       int    `json:"pid"`
	StartedAt int64  `json:"started_at"`
	Socket    string `json:"socket"`
}

// daemonStatusResult is the `daemon status` --json shape.
type daemonStatusResult struct {
	Socket  string `json:"socket"`
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
	Version string `json:"version,omitempty"`
	Uptime  string `json:"uptime,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// Convergence health, distinct from Running. A supervised daemon never
	// exits on convergence failure — it backs off and keeps serving — so
	// "running" says nothing about whether the workspace is converging, and
	// the supervisor's restart count cannot be used as the failure signal the
	// way it can for run-loop. Without these fields the only way to see a
	// wedged daemon would be to curl the socket by hand.
	Healthy             *bool  `json:"healthy,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	LastRunAt           string `json:"last_run_at,omitempty"`
	LastSuccess         string `json:"last_success_at,omitempty"`
	// Watch reports the filesystem-hint plane. A degraded watcher is not an
	// unhealthy daemon (correctness rides on periodic convergence), but a
	// silently degraded one leaves the user believing they have sub-interval
	// convergence when they do not.
	WatchBackend  string `json:"watch_backend,omitempty"`
	WatchDegraded *bool  `json:"watch_degraded,omitempty"`
	WatchReason   string `json:"watch_reason,omitempty"`
	WatchRoots    int    `json:"watch_roots,omitempty"`
}

type daemonStartResult struct {
	Socket string `json:"socket"`
	PID    int    `json:"pid"`
}

type daemonStopResult struct {
	Socket  string `json:"socket"`
	PID     int    `json:"pid,omitempty"`
	Stopped bool   `json:"stopped"`
	Detail  string `json:"detail,omitempty"`
}

type daemonSyncResult struct {
	Mode       daemon.TickMode `json:"mode"`
	StartedAt  time.Time       `json:"started_at"`
	DurationMS int64           `json:"duration_ms"`
	Coalesced  bool            `json:"coalesced,omitempty"`
	// RequestedMode is set only when the observed cycle was WEAKER than what
	// this caller asked for, which happens when a full request joins an
	// in-flight namespace-only cycle. Without it a script sees mode
	// "namespace-only" and exit 0 for a request that asked to materialize, and
	// has no way to tell that apart from having asked for namespace-only.
	RequestedMode daemon.TickMode `json:"requested_mode,omitempty"`
	// Deferred reports that the requested work has NOT run yet. The scheduler
	// remembers the stronger mode and promotes the next cycle, so it is queued
	// rather than lost — but it has not happened when this command returns.
	Deferred bool `json:"deferred,omitempty"`
}

func newDaemonCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run and inspect the local DevStrap daemon",
		Long: "Run and inspect the local DevStrap daemon.\n\n" +
			"The daemon serves a local control API over a Unix socket at\n" +
			"~/.devstrap/devstrapd.sock. It is an optimization, never a correctness\n" +
			"dependency: every devstrap command works with the daemon absent, and\n" +
			"`devstrap run-loop` remains the portable daemonless convergence path.",
	}
	cmd.AddCommand(newDaemonStartCommand(stdout, opts))
	cmd.AddCommand(newDaemonStopCommand(stdout, opts))
	cmd.AddCommand(newDaemonStatusCommand(stdout, opts))
	cmd.AddCommand(newDaemonEventsCommand(stdout, opts))
	cmd.AddCommand(newDaemonSyncCommand(stdout, opts))
	return cmd
}

func newDaemonStartCommand(stdout io.Writer, opts *options) *cobra.Command {
	var hubFile string
	var interval time.Duration
	var namespaceOnly bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the daemon in the foreground",
		Long: "Start the daemon in the foreground.\n\n" +
			"The process runs until interrupted (Ctrl-C) or until its supervisor\n" +
			"stops it, which is what `devstrap service install` relies on: launchd\n" +
			"and systemd both supervise a foreground process rather than a\n" +
			"self-daemonizing one.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Fail fast on an unresolvable hub before binding the socket, the
			// same preflight run-loop does (and for the same reason: a daemon
			// that starts and then fails every tick is worse than one that
			// refuses with a clear message).
			if err := hubConfigured(opts, hubFile); err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			return runDaemonStart(cmd, stdout, opts, hubFile, interval, namespaceOnly)
		},
	}
	cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Minute, "time between convergence cycles (0 disables periodic convergence)")
	cmd.Flags().BoolVar(&namespaceOnly, "namespace-only", false, "sync namespace metadata only; skip materialization")
	return cmd
}

func runDaemonStart(cmd *cobra.Command, stdout io.Writer, opts *options, hubFile string, interval time.Duration, namespaceOnly bool) error {
	paths := opts.paths()
	socket := paths.SocketPath()
	stderr := cmd.ErrOrStderr()

	// Ctrl-C and a supervisor's SIGTERM both mean "shut down cleanly".
	//
	// This duplicates the handler cmd/devstrap/main.go already installs on the
	// root context, and is deliberately kept: the in-process test harness runs
	// the CLI inside `go test`, where `daemon stop` SIGTERMs the test binary
	// itself. Without a handler registered here at that instant the signal is
	// fatal and the whole internal/cli package dies with "signal: terminated",
	// an opaque failure with no visible link to whoever removed this line.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	converger := cliConverger{opts: opts, stdout: stdout, stderr: stderr, hubFile: hubFile}
	if namespaceOnly {
		// --namespace-only makes every periodic cycle skip materialization,
		// mirroring run-loop's flag of the same name.
		converger.forceNamespaceOnly = true
	}
	// The watcher is optional by construction: platform.Detect() always returns
	// an adapter, but if the watch plane cannot start the daemon stays
	// periodic-only rather than failing. Correctness never rides on it.
	adapters := platform.Detect()
	server, err := daemon.New(daemon.Config{
		SocketPath:    socket,
		Version:       version,
		Logger:        logging.Logger(cmd.Context()),
		Converger:     converger,
		Interval:      interval,
		Jitter:        daemonJitter,
		Watcher:       adapters.Watcher,
		WatchFallback: platform.PollWatcher{},
		WatchSource:   cliWatchSource{opts: opts},
		Reader:        cliReader{opts: opts},
	})
	if err != nil {
		return err
	}

	// Bind the socket BEFORE writing the pid record. The record must only ever
	// describe a process that actually owns the socket: writing first meant a
	// losing second start overwrote the RUNNING daemon's record and then its
	// deferred cleanup deleted it outright, leaving a live daemon with no
	// record at all — `daemon stop` would then report "no daemon is running"
	// while one was. Listening first makes that ordering impossible.
	listener, err := daemon.Listen(socket)
	if err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			return appError{code: exitConflict, err: fmt.Errorf(
				"another devstrap daemon is already listening on %s; stop it with `devstrap daemon stop`", socket)}
		}
		return err
	}

	startedAt, err := writeDaemonRecord(paths.Home, socket)
	if err != nil {
		_ = listener.Close()
		return err
	}
	// Remove only OUR record. An unconditional removal races a restart: this
	// daemon drains (up to shutdownTimeout) while a supervisor starts its
	// replacement, the replacement binds and writes its own record, and then
	// this deferred cleanup deletes the REPLACEMENT's record — the same
	// unstoppable-daemon state as writing the record before binding.
	defer func() { _ = removeDaemonRecordIfOwn(paths.Home, os.Getpid(), startedAt) }()

	if err := opts.render(stdout, func(w io.Writer) error {
		_, ferr := fmt.Fprintf(stderr, "devstrap daemon listening on %s\n", socket)
		return ferr
	}, daemonStartResult{Socket: socket, PID: os.Getpid()}); err != nil {
		return err
	}

	return server.ServeListener(ctx, listener)
}

func newDaemonStopCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonStop(cmd, stdout, opts)
		},
	}
}

func runDaemonStop(cmd *cobra.Command, stdout io.Writer, opts *options) error {
	paths := opts.paths()
	socket := paths.SocketPath()

	record, err := readDaemonRecord(paths.Home)
	if err != nil || record.PID <= 0 {
		// Nothing recorded: report the fact rather than failing. A stop that
		// finds nothing to stop has achieved its goal.
		return opts.render(stdout, func(w io.Writer) error {
			_, ferr := fmt.Fprintln(w, "no daemon is running")
			return ferr
		}, daemonStopResult{Socket: socket, Stopped: false, Detail: "no daemon is running"})
	}

	if !daemonRecordAlive(record) {
		// Ownership-guarded for the same reason `start`'s cleanup is: between
		// the read above and here, a new daemon can bind and write its own
		// record — and a lying-around stale record is precisely the situation
		// in which someone restarts, so the two are correlated, not independent.
		_ = removeDaemonRecordIfOwn(paths.Home, record.PID, record.StartedAt)
		return opts.render(stdout, func(w io.Writer) error {
			_, ferr := fmt.Fprintln(w, "no daemon is running (cleared a stale record)")
			return ferr
		}, daemonStopResult{Socket: socket, Stopped: false, Detail: "cleared a stale record"})
	}

	process, err := os.FindProcess(record.PID)
	if err != nil {
		return appError{code: exitGeneric, err: fmt.Errorf("find daemon process %d: %w", record.PID, err)}
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return appError{code: exitGeneric, err: fmt.Errorf("signal daemon %d: %w", record.PID, err)}
	}

	// Wait for the daemon to actually be down rather than reporting success on
	// the strength of a delivered signal.
	//
	// "Down" means the daemon RELEASED ITS RECORD, or its process is gone. The
	// record is deliberately the primary signal: `daemon start` removes it only
	// after ServeListener returns, i.e. after the graceful drain has finished
	// and every lock the daemon held is released.
	//
	// The socket is NOT a sufficient signal, though it is tempting. http.Server
	// .Shutdown closes listeners FIRST and unlinks the socket, then drains
	// in-flight requests for up to shutdownTimeout — so a socket check reports
	// "stopped" at the START of the drain. That is harmless while the surface is
	// read-only, but once a convergence tick holds the maintenance lock it would
	// make `devstrap daemon stop && devstrap db restore` fail with a conflict
	// from a daemon this command just declared stopped.
	deadline := time.Now().Add(stopTimeout)
	for time.Now().Before(deadline) {
		if daemonRecordReleased(paths.Home, record) || !daemonRecordAlive(record) {
			// Guarded, and this is the case where it matters most: we get here
			// when the record was RELEASED, which is either "gone" (removal is a
			// no-op) or "replaced by a new daemon's record" — the only case with
			// any effect, where an unguarded removal would delete the
			// REPLACEMENT's record and recreate the unstoppable-daemon state
			// this criterion exists to avoid.
			_ = removeDaemonRecordIfOwn(paths.Home, record.PID, record.StartedAt)
			return opts.render(stdout, func(w io.Writer) error {
				_, ferr := fmt.Fprintf(w, "stopped daemon (pid %d)\n", record.PID)
				return ferr
			}, daemonStopResult{Socket: socket, PID: record.PID, Stopped: true})
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(stopPollInterval):
		}
	}
	return appError{code: exitGeneric, err: fmt.Errorf(
		"daemon (pid %d) did not exit within %s of SIGTERM", record.PID, stopTimeout)}
}

func newDaemonStatusCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the daemon is running",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonStatus(cmd, stdout, opts)
		},
	}
}

func runDaemonStatus(cmd *cobra.Command, stdout io.Writer, opts *options) error {
	paths := opts.paths()
	client, socket := daemonClient(opts)
	result := daemonStatusResult{Socket: socket}

	// The socket is the source of truth: a daemon that answers is running,
	// whatever the pid file says. The pid file only enriches the report.
	health, err := client.Health(cmd.Context())
	switch {
	case err == nil:
		result.Running = true
		result.Uptime = (time.Duration(health.UptimeSeconds) * time.Second).String()
		// Only meaningful when a Converger is wired; a transport-only daemon
		// would otherwise report a permanently "unhealthy" convergence it does
		// not perform.
		if health.Converging {
			healthy := health.Healthy
			result.Healthy = &healthy
			result.LastError = health.LastError
			result.ConsecutiveFailures = health.ConsecutiveFailures
			result.LastRunAt = health.LastRunAt
			result.LastSuccess = health.LastSuccessAt
		}
		result.WatchBackend = health.Watch.Backend
		if health.Watch.Enabled {
			degraded := health.Watch.Degraded
			result.WatchDegraded = &degraded
			result.WatchReason = health.Watch.Reason
			result.WatchRoots = health.Watch.Roots
		}
		if v, verr := client.Version(cmd.Context()); verr == nil {
			result.Version = v.Version
		}
		if record, rerr := readDaemonRecord(paths.Home); rerr == nil {
			result.PID = record.PID
		}
	case errors.Is(err, daemon.ErrUnavailable):
		result.Detail = "not running"
	default:
		// The socket exists and answered, but not correctly — surface it rather
		// than reporting a clean "not running".
		result.Detail = err.Error()
	}

	return opts.render(stdout, func(w io.Writer) error {
		if !result.Running {
			_, ferr := fmt.Fprintf(w, "daemon: not running (%s)\n", result.Detail)
			return ferr
		}
		if _, ferr := fmt.Fprintf(w, "daemon: running (uptime %s", result.Uptime); ferr != nil {
			return ferr
		}
		if result.PID > 0 {
			if _, ferr := fmt.Fprintf(w, ", pid %d", result.PID); ferr != nil {
				return ferr
			}
		}
		if result.Version != "" {
			if _, ferr := fmt.Fprintf(w, ", version %s", result.Version); ferr != nil {
				return ferr
			}
		}
		if _, ferr := fmt.Fprintf(w, ")\nsocket: %s\n", result.Socket); ferr != nil {
			return ferr
		}
		if result.LastSuccess != "" {
			if _, ferr := fmt.Fprintf(w, "last successful convergence: %s\n", result.LastSuccess); ferr != nil {
				return ferr
			}
		}
		// Running and converging are different questions, and the supervisor
		// answers neither: it restarts only on a crash, so a daemon failing
		// every cycle looks "running" to launchd/systemd forever.
		if result.Healthy != nil {
			if *result.Healthy {
				if _, ferr := fmt.Fprintf(w, "converging: ok\n"); ferr != nil {
					return ferr
				}
			} else {
				if _, ferr := fmt.Fprintf(w, "converging: FAILING (%d consecutive; last error: %s)\n", result.ConsecutiveFailures, result.LastError); ferr != nil {
					return ferr
				}
			}
		}
		if result.WatchDegraded != nil && *result.WatchDegraded {
			if _, ferr := fmt.Fprintf(w, "watch: degraded (%s)\n", result.WatchReason); ferr != nil {
				return ferr
			}
		} else if result.WatchDegraded != nil {
			if _, ferr := fmt.Fprintf(w, "watch: %s, %d root(s)\n", result.WatchBackend, result.WatchRoots); ferr != nil {
				return ferr
			}
		}
		return nil
	}, result)
}

// daemonClient builds a client for this invocation's socket, returning the
// socket path so callers can name it in errors.
func daemonClient(opts *options) (*daemon.Client, string) {
	socket := opts.paths().SocketPath()
	return daemon.NewClient(socket), socket
}

// daemonUnavailable is the shared mapping for "no daemon is listening". Only
// daemon-ONLY commands use it: a command with a working local path must never
// report exit 3, because that would turn a fallback into a failure.
func daemonUnavailable(socket string) error {
	return appError{code: exitDaemonUnavailable, err: fmt.Errorf(
		"no daemon is running on %s; start one with `devstrap daemon start`", socket)}
}

func daemonRecordPath(home string) string {
	return filepath.Join(home, daemonPIDFile)
}

func writeDaemonRecord(home, socket string) (int64, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return 0, fmt.Errorf("create state home: %w", err)
	}
	// A failed start-time lookup is not fatal: the record degrades to
	// liveness-only checking, matching repo_lock.go's tolerance.
	startedAt, _ := processStartTime(os.Getpid())
	record := daemonRecord{
		PID:       os.Getpid(),
		StartedAt: startedAt,
		Socket:    socket,
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return 0, fmt.Errorf("encode daemon record: %w", err)
	}
	// Write atomically: daemonRecordReleased treats an unreadable record as
	// "released", so a reader observing a half-written file would declare a
	// still-draining daemon stopped. temp+rename matches how the service
	// adapters and the git carrier already write their state files.
	if err := writeDaemonRecordAtomic(daemonRecordPath(home), payload); err != nil {
		return 0, err
	}
	return startedAt, nil
}

// writeDaemonRecordAtomic writes the record via a temp file and rename, so a
// concurrent reader sees either the old record or the new one, never a torn one.
func writeDaemonRecordAtomic(path string, payload []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".devstrapd.pid.*")
	if err != nil {
		return fmt.Errorf("create daemon record temp: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()

	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write daemon record: %w", err)
	}
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("chmod daemon record: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close daemon record: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("promote daemon record: %w", err)
	}
	return nil
}

// removeDaemonRecordIfOwn deletes the record only when it still describes the
// given process, so a daemon shutting down never deletes its replacement's.
func removeDaemonRecordIfOwn(home string, pid int, startedAt int64) error {
	current, err := readDaemonRecord(home)
	if err != nil {
		return nil
	}
	if current.PID != pid || current.StartedAt != startedAt {
		return nil
	}
	return removeDaemonRecord(home)
}

func readDaemonRecord(home string) (daemonRecord, error) {
	var record daemonRecord
	payload, err := os.ReadFile(daemonRecordPath(home))
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(payload, &record); err != nil {
		return record, fmt.Errorf("parse daemon record: %w", err)
	}
	return record, nil
}

func removeDaemonRecord(home string) error {
	err := os.Remove(daemonRecordPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// daemonRecordAlive reports whether the recorded process is still the daemon.
// It delegates to the SAME helper repo_lock.go uses, rather than reimplementing
// it, so the PID-reuse guard cannot drift between the two callers — and so the
// repoLockProcessAlive seam keeps working for tests that fake liveness.
//
// Platform caveat: ProcessStartTime is implemented on darwin and linux only.
// Elsewhere StartedAt is 0 and this degrades to liveness alone, so the
// PID-reuse guard is inert there.
func daemonRecordAlive(record daemonRecord) bool {
	if record.PID <= 0 {
		return false
	}
	return processIdentityAlive(record.PID, record.StartedAt)
}

// daemonRecordReleased reports whether the daemon has cleaned up its own record
// — the signal that its drain finished and its locks are released. A record
// replaced by a DIFFERENT daemon also counts as released: ours is gone.
func daemonRecordReleased(home string, record daemonRecord) bool {
	current, err := readDaemonRecord(home)
	if err != nil {
		return true
	}
	return current.PID != record.PID || current.StartedAt != record.StartedAt
}

// daemonJitter perturbs each periodic wait by up to +10%, matching run-loop's
// bound. Unjittered fleet-wide intervals stampede the hub — every device
// installed on the same day would otherwise converge in lockstep.
func daemonJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + time.Duration(runLoopJitterBound(d))
}

// newDaemonEventsCommand tails the daemon's event stream.
//
// This is the wave's first genuinely daemon-ONLY command, and therefore the
// first caller to return the long-reserved exitDaemonUnavailable (3): every
// other command has a local path that works without a daemon, so returning "the
// daemon is unavailable" for them would be a regression, not a feature. A live
// event stream has no daemonless equivalent — that is the whole point of it.
func newDaemonEventsCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "events",
		Short: "Stream daemon events until interrupted (requires a running daemon)",
		Long: "Stream daemon events until interrupted.\n\n" +
			"The stream is deliberately LOSSY: the daemon drops events for a\n" +
			"subscriber that falls behind rather than slowing convergence. Treat it\n" +
			"as a notification channel, never as a log to reconstruct state from —\n" +
			"`devstrap status` remains the source of truth.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, socket := daemonClient(opts)
			err := client.Events(cmd.Context(), func(event daemon.Event) {
				line := fmt.Sprintf("%s  %s", event.At.Format(time.RFC3339), event.Kind)
				if event.Detail != "" {
					line += "  " + event.Detail
				}
				_, _ = fmt.Fprintln(stdout, line)
			})
			switch {
			case err == nil, errors.Is(err, context.Canceled):
				return nil
			case errors.Is(err, daemon.ErrUnavailable):
				return daemonUnavailable(socket)
			default:
				return err
			}
		},
	}
}

func newDaemonSyncCommand(stdout io.Writer, opts *options) *cobra.Command {
	var namespaceOnly bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Ask the running daemon to converge now (requires a running daemon)",
		Long: "Ask the running daemon to converge now.\n\n" +
			"`devstrap sync` runs a convergence cycle in this process and works\n" +
			"without a daemon. This daemon-namespaced command triggers a cycle in\n" +
			"the running daemon. The two are safe to overlap because the existing\n" +
			"SQLite and repository-operation locks serialize their work.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := daemon.TickFull
			if namespaceOnly {
				mode = daemon.TickNamespaceOnly
			}
			client, socket := daemonClient(opts)
			result, err := client.Sync(cmd.Context(), mode)
			// A full request that arrives mid-cycle JOINS the weaker cycle
			// already running and returns its result: coalesced, mode
			// namespace-only, nothing materialized. Reporting that as a plain
			// success would tell a caller its full sync happened when it did
			// not. The scheduler has recorded the promotion, so exactly ONE
			// retry is enough to claim the next cycle; a loop is not, because a
			// busy watcher could keep starting namespace-only cycles forever.
			if err == nil && result.Coalesced && mode == daemon.TickFull && result.Mode != daemon.TickFull {
				result, err = client.Sync(cmd.Context(), mode)
			}
			switch {
			case errors.Is(err, daemon.ErrUnavailable):
				return daemonUnavailable(socket)
			case errors.Is(err, daemon.ErrConvergerUnavailable):
				// `devstrap daemon start` ALWAYS wires a converger, so this is
				// not a user misconfiguration and must not prescribe a flag as
				// though it were. It means the daemon on this socket is
				// transport-only — a programmatically constructed one, which
				// today is only ever a test harness.
				return fmt.Errorf(
					"the daemon on %s is transport-only and cannot converge; "+
						"it was not started by `devstrap daemon start`", socket)
			case err != nil:
				return err
			}
			rendered := daemonSyncResult{
				Mode:       result.Mode,
				StartedAt:  result.StartedAt,
				DurationMS: result.DurationMS,
				Coalesced:  result.Coalesced,
			}
			deferred := result.Mode != mode
			if deferred {
				rendered.RequestedMode = mode
				rendered.Deferred = true
			}
			return opts.render(stdout, func(w io.Writer) error {
				duration := (time.Duration(result.DurationMS) * time.Millisecond).String()
				switch {
				case deferred:
					_, ferr := fmt.Fprintf(w,
						"joined a %s convergence already in progress, finished in %s\n"+
							"the %s sync you asked for has NOT run; it is queued and will run on the next cycle\n",
						result.Mode, duration, mode)
					return ferr
				case result.Coalesced:
					_, ferr := fmt.Fprintf(w, "joined a convergence already in progress (%s), finished in %s\n", result.Mode, duration)
					return ferr
				}
				_, ferr := fmt.Fprintf(w, "converged (%s) in %s\n", result.Mode, duration)
				return ferr
			}, rendered)
		},
	}
	cmd.Flags().BoolVar(&namespaceOnly, "namespace-only", false, "sync namespace metadata only; skip materialization")
	return cmd
}
