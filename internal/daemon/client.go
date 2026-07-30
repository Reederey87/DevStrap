package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrUnavailable reports that no daemon is reachable on the socket — it is
// absent, or nothing is accepting on it. Callers map this to the CLI's
// already-reserved exitDaemonUnavailable (3); every read path in DevStrap is
// expected to fall back to reading local state directly rather than fail.
var ErrUnavailable = errors.New("daemon: unavailable")

// ErrConvergerUnavailable reports that a daemon is reachable, but it was
// started without a convergence engine.
var ErrConvergerUnavailable = errors.New("daemon: converger unavailable")

// clientTimeout bounds a single request. The daemon is a local process, so a
// request that has not completed in this long is wedged, not slow.
const clientTimeout = 10 * time.Second

// Client talks to a local daemon over its Unix domain socket.
type Client struct {
	http          *http.Client
	versionMu     sync.RWMutex
	daemonVersion string
}

// NewClient builds a client for the daemon at socketPath. It performs no I/O —
// the first request is what discovers whether a daemon is running.
func NewClient(socketPath string) *Client {
	return &Client{
		http: &http.Client{
			Timeout: clientTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// Health reports the daemon's liveness and uptime.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	err := c.request(ctx, c.http, http.MethodGet, "/v1/health", &out)
	return out, err
}

// Version reports the daemon's build version.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	err := c.request(ctx, c.http, http.MethodGet, "/v1/version", &out)
	if err == nil && out.APIVersion != "" && out.APIVersion != apiVersion {
		return Version{}, &UnsupportedAPIVersionError{APIVersion: out.APIVersion}
	}
	return out, err
}

// DaemonVersion reports the version the daemon advertised on the most recent
// response, or "" if no request has completed yet.
func (c *Client) DaemonVersion() string {
	c.versionMu.RLock()
	defer c.versionMu.RUnlock()
	return c.daemonVersion
}

// maxDaemonVersionLen bounds what is retained from the advertised header. A
// build version is a handful of characters; Go's transport would accept
// megabytes.
const maxDaemonVersionLen = 64

// recordDaemonVersion stores the advertised build version. A response without the
// header leaves the last known value alone rather than erasing it: the value is
// reported to the user, and forgetting it would silently switch a real skew
// warning off.
func (c *Client) recordDaemonVersion(version string) {
	version = sanitizeVersionHeader(version)
	if version == "" {
		return
	}
	c.versionMu.Lock()
	c.daemonVersion = version
	c.versionMu.Unlock()
}

// sanitizeVersionHeader clamps the header to something safe to print. The value
// is written straight to a terminal and into `--json`, and it arrives over the
// wire: a same-uid process squatting the socket could otherwise inject ANSI
// escapes into an operator's terminal. redact.Scrub does not cover this — it
// scrubs token shapes, not control bytes.
func sanitizeVersionHeader(version string) string {
	if len(version) > maxDaemonVersionLen {
		version = version[:maxDaemonVersionLen]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, version)
}

func (c *Client) request(ctx context.Context, requester *http.Client, method, path string, out any) error {
	// The host is fixed so it matches the server's Host check; it is not used
	// for routing, since the transport always dials the socket.
	url := "http://" + socketHost + path
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return fmt.Errorf("daemon: build request: %w", err)
	}

	resp, err := requester.Do(req)
	if err != nil {
		if isUnavailable(err) {
			return fmt.Errorf("%w: %s", ErrUnavailable, path)
		}
		return fmt.Errorf("daemon: request %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	c.recordDaemonVersion(resp.Header.Get(versionHeader))

	if resp.StatusCode != http.StatusOK {
		// Bound the error body: a wedged or hostile peer must not be able to
		// make the client allocate without limit.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var parsed errorBody
		hasDetail := json.Unmarshal(body, &parsed) == nil && parsed.Error != ""

		// Route on the ROUTE, not on the raw path: `path` carries a query
		// string, so a prefix match here would also catch a future
		// /v1/sync-state or /v1/syncthing and silently inherit this remap.
		route, _, _ := strings.Cut(path, "?")
		if route == syncRoute && resp.StatusCode == http.StatusServiceUnavailable {
			// Deliberately NOT nested under hasDetail: a 503 with an empty or
			// truncated body still means the daemon cannot converge, and
			// degrading to a bare status line would lose the curated
			// explanation exactly when the peer is misbehaving.
			if hasDetail {
				return fmt.Errorf("%w: %s", ErrConvergerUnavailable, parsed.Error)
			}
			return fmt.Errorf("%w: the daemon reported it cannot converge", ErrConvergerUnavailable)
		}
		if hasDetail {
			return fmt.Errorf("daemon: %s: %s (status %d)", path, parsed.Error, resp.StatusCode)
		}
		return fmt.Errorf("daemon: %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRequestBody)).Decode(out); err != nil {
		return fmt.Errorf("daemon: decode %s: %w", path, err)
	}
	return nil
}

// isUnavailable distinguishes "no daemon is there" from a real transport
// failure. A missing socket file surfaces as ENOENT and a socket nobody is
// accepting on as ECONNREFUSED; both mean the same thing to a caller.
func isUnavailable(err error) bool {
	// Deliberately narrow. Callers map ErrUnavailable to a SILENT fallback (read
	// local state instead of asking the daemon), so anything misclassified here
	// becomes a real failure the user never sees. Treating every dial-stage
	// error as "no daemon" would swallow: EMFILE/ENFILE in this process (out of
	// descriptors — every query would report no daemon), EACCES on a
	// permission-broken ~/.devstrap, a caller's own cancellation or deadline,
	// and a dial timeout against a daemon whose accept backlog is full. Only
	// these two mean "nothing is listening":
	//
	//   ENOENT        — no socket file at all
	//   ECONNREFUSED  — a socket file with no process accepting on it
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

// Status fetches the workspace snapshot without the caller opening the store.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var out Status
	err := c.request(ctx, c.http, http.MethodGet, "/v1/status", &out)
	return out, err
}

// syncRoute is the convergence endpoint's path without its query string.
const syncRoute = "/v1/sync"

// Sync asks the daemon to run (or join) one convergence cycle.
func (c *Client) Sync(ctx context.Context, mode TickMode) (Result, error) {
	path := syncRoute + "?" + url.Values{"mode": []string{string(mode)}}.Encode()
	var out Result

	// No client timeout on a convergence trigger: a cycle legitimately runs for
	// minutes (blobless clone + materialize), and the shared client's 10s bound
	// would cancel the request context and abort the cycle server-side. Bounded
	// only by the caller's context, exactly as Events is.
	syncer := &http.Client{Transport: c.http.Transport}
	err := c.request(ctx, syncer, http.MethodPost, path, &out)
	return out, err
}

// Events streams events until ctx is cancelled or the daemon stops, invoking fn
// for each. It returns ErrUnavailable when no daemon is reachable, so a caller
// can distinguish "nothing to watch" from a transport failure.
//
// The stream is explicitly LOSSY: the daemon drops events for a subscriber that
// falls behind rather than slowing convergence. Callers must treat it as a
// notification channel, never as a log they can reconstruct state from.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	url := "http://" + socketHost + "/v1/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("daemon: build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	// No client timeout on a stream: the shared c.http has one, so this uses a
	// dedicated client whose only bound is the caller's context.
	streamer := &http.Client{Transport: c.http.Transport}
	resp, err := streamer.Do(req)
	if err != nil {
		if isUnavailable(err) {
			return fmt.Errorf("%w: /v1/events", ErrUnavailable)
		}
		return fmt.Errorf("daemon: stream events: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	c.recordDaemonVersion(resp.Header.Get(versionHeader))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon: /v1/events: status %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 4096), maxRequestBody)
	for scanner.Scan() {
		line := scanner.Text()
		// SSE frames are "event: <kind>\ndata: <json>\n\n"; comments (": ...")
		// are heartbeats and carry nothing.
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		fn(event)
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("daemon: read event stream: %w", err)
	}
	return nil
}
