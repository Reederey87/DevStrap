package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"syscall"
	"time"
)

// ErrUnavailable reports that no daemon is reachable on the socket — it is
// absent, or nothing is accepting on it. Callers map this to the CLI's
// already-reserved exitDaemonUnavailable (3); every read path in DevStrap is
// expected to fall back to reading local state directly rather than fail.
var ErrUnavailable = errors.New("daemon: unavailable")

// clientTimeout bounds a single request. The daemon is a local process, so a
// request that has not completed in this long is wedged, not slow.
const clientTimeout = 10 * time.Second

// Client talks to a local daemon over its Unix domain socket.
type Client struct {
	http *http.Client
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
	err := c.get(ctx, "/v1/health", &out)
	return out, err
}

// Version reports the daemon's build version.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	err := c.get(ctx, "/v1/version", &out)
	return out, err
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	// The host is fixed so it matches the server's Host check; it is not used
	// for routing, since the transport always dials the socket.
	url := "http://" + socketHost + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("daemon: build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if isUnavailable(err) {
			return fmt.Errorf("%w: %s", ErrUnavailable, path)
		}
		return fmt.Errorf("daemon: request %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Bound the error body: a wedged or hostile peer must not be able to
		// make the client allocate without limit.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var parsed errorBody
		if json.Unmarshal(body, &parsed) == nil && parsed.Error != "" {
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
