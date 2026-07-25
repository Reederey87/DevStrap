package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type stubReader struct {
	status Status
	err    error
}

func (s stubReader) Status(context.Context) (Status, error) { return s.status, s.err }

func startServerWith(t *testing.T, cfg Config) string {
	t.Helper()
	cfg.SocketPath = tempSocketPath(t)
	cfg.Logger = testLogger()
	if cfg.Version == "" {
		cfg.Version = "test"
	}
	server, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForSocket(t, cfg.SocketPath)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("timeout waiting for Serve to return")
		}
	})
	return cfg.SocketPath
}

func TestStatusEndpointServesTheWorkspaceSnapshot(t *testing.T) {
	want := Status{WorkspaceName: "personal", WorkspaceID: "ws_x", RootPath: "/Code", ProjectCount: 7, DeviceID: "dev_a"}
	socket := startServerWith(t, Config{Reader: stubReader{status: want}})

	got, err := NewClient(socket).Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != want {
		t.Fatalf("status = %+v, want %+v", got, want)
	}
}

// TestStatusEndpointWithoutReaderReports503 pins that a transport-only daemon
// says so rather than inventing an empty snapshot, which a caller could not
// distinguish from a genuinely empty workspace.
func TestStatusEndpointWithoutReaderReports503(t *testing.T) {
	socket := startServer(t, "test")
	client := rawClient(socket)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+socketHost+"/v1/status", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestStatusEndpointDoesNotLeakReaderErrors pins that a store error becomes a
// generic message to the caller — an error string from the store can carry a
// path or a DSN, and the client is not the place to surface it.
func TestStatusEndpointDoesNotLeakReaderErrors(t *testing.T) {
	socket := startServerWith(t, Config{
		Reader: stubReader{err: errors.New("open /Users/someone/.devstrap/state.db: permission denied")},
	})
	_, err := NewClient(socket).Status(t.Context())
	if err == nil {
		t.Fatal("Status succeeded despite a reader error")
	}
	if strings.Contains(err.Error(), "/Users/someone") {
		t.Fatalf("client error leaked the reader's message: %v", err)
	}
}

// TestEventsStreamDeliversPublishedEvents is the round trip a shell hook or
// editor depends on.
func TestEventsStreamDeliversPublishedEvents(t *testing.T) {
	fake := newFakeConverger()
	close(fake.release)
	socket := startServerWith(t, Config{Converger: fake})

	received := make(chan Event, 8)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	streamed := make(chan error, 1)
	go func() {
		streamed <- NewClient(socket).Events(ctx, func(e Event) { received <- e })
	}()

	// Give the subscription time to register before triggering work; otherwise
	// the publish races the subscribe and the stream legitimately misses it.
	waitForSubscriber(t, socket)

	client := rawClient(socket)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+socketHost+"/v1/sync", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	_ = resp.Body.Close()

	kinds := map[string]bool{}
	deadline := time.After(10 * time.Second)
	for len(kinds) < 2 {
		select {
		case event := <-received:
			kinds[event.Kind] = true
			if event.At.IsZero() {
				t.Fatalf("event %q has a zero timestamp", event.Kind)
			}
		case <-deadline:
			t.Fatalf("only saw %v, want both converge.started and converge.done", kinds)
		}
	}
	if !kinds[EventConvergeStarted] || !kinds[EventConvergeDone] {
		t.Fatalf("kinds = %v, want started and done", kinds)
	}

	cancel()
	select {
	case <-streamed:
	case <-time.After(5 * time.Second):
		t.Fatal("Events did not return after cancel")
	}
}

// TestEventBusDropsForSlowSubscriberRatherThanBlocking is the load-bearing
// contract: convergence must never be slowed by a reader. A subscriber that
// stops draining loses events; it does not become backpressure on the daemon.
func TestEventBusDropsForSlowSubscriberRatherThanBlocking(t *testing.T) {
	bus := newEventBus()
	_, slow := bus.subscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Publish far more than the buffer holds, with nobody draining.
		for range eventBufferSize * 4 {
			bus.publish(Event{Kind: EventConvergeDone, At: time.Now()})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publish blocked on a slow subscriber; convergence must never wait on a reader")
	}
	if got := len(slow); got != eventBufferSize {
		t.Fatalf("subscriber queue = %d, want it capped at %d", got, eventBufferSize)
	}
}

func TestEventBusUnsubscribeIsIdempotentAndClosesChannel(t *testing.T) {
	bus := newEventBus()
	id, ch := bus.subscribe()
	bus.unsubscribe(id)
	if _, ok := <-ch; ok {
		t.Fatal("channel not closed on unsubscribe")
	}
	bus.unsubscribe(id) // must not panic on a second call
	if got := bus.subscriberCount(); got != 0 {
		t.Fatalf("subscriberCount = %d, want 0", got)
	}
}

func TestEventBusFansOutToEverySubscriber(t *testing.T) {
	bus := newEventBus()
	const subs = 5
	chans := make([]<-chan Event, 0, subs)
	for range subs {
		_, ch := bus.subscribe()
		chans = append(chans, ch)
	}
	bus.publish(Event{Kind: EventWatchDegraded, At: time.Now()})

	var wg sync.WaitGroup
	for i, ch := range chans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case event := <-ch:
				if event.Kind != EventWatchDegraded {
					t.Errorf("subscriber %d got %q", i, event.Kind)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("subscriber %d received nothing", i)
			}
		}()
	}
	wg.Wait()
}

// waitForSubscriber blocks until the server has registered an SSE subscriber,
// so a test never races publish against subscribe.
func waitForSubscriber(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		health, err := NewClient(socket).Health(t.Context())
		if err == nil && health.OK {
			// The bus count is not exposed over HTTP; a successful health call
			// plus a short settle is the closest observable proxy.
			time.Sleep(150 * time.Millisecond)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon never became ready")
}
