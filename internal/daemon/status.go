package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Reader supplies the workspace snapshot served by GET /v1/status.
//
// Like Converger it is deliberately narrow: the daemon reports state, it does
// not become a query API over the store. A consumer that needs more than this
// should open the store itself — the daemon is not a database proxy.
type Reader interface {
	Status(ctx context.Context) (Status, error)
}

// Status is the /v1/status payload. Fields mirror the store summary the CLI
// already prints, in snake_case per spec/13's --json conventions.
type Status struct {
	WorkspaceName string `json:"workspace_name"`
	WorkspaceID   string `json:"workspace_id"`
	RootPath      string `json:"root_path"`
	ProjectCount  int    `json:"project_count"`
	DeviceID      string `json:"device_id,omitempty"`
}

// Event is one server-sent event on GET /v1/events.
//
// The stream exists so a shell hook, editor, or TUI can observe convergence
// without polling a database — the one thing no daemonless design can offer.
type Event struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	// Detail is scrubbed free text. No event carries secret material: kinds are
	// fixed strings and detail is redacted at publish time.
	Detail string `json:"detail,omitempty"`
}

// Event kinds. Kept a closed set so a consumer can switch on them, and so no
// caller-supplied string ever becomes an event name.
const (
	EventConvergeStarted = "converge.started"
	EventConvergeDone    = "converge.done"
	EventConvergeFailed  = "converge.failed"
	EventWatchDegraded   = "watch.degraded"
)

// eventBufferSize bounds each subscriber's queue. A subscriber that falls
// behind by more than this is dropped from rather than allowed to block the
// publisher — convergence must never be slowed by a slow reader, and the stream
// is explicitly lossy (it is a notification channel, not a log).
const eventBufferSize = 64

// eventBus fans events out to connected SSE subscribers.
type eventBus struct {
	mu   sync.Mutex
	next int
	subs map[int]chan Event
}

func newEventBus() *eventBus {
	return &eventBus{subs: make(map[int]chan Event)}
}

func (b *eventBus) subscribe() (int, <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Event, eventBufferSize)
	b.subs[id] = ch
	return id, ch
}

func (b *eventBus) unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// publish delivers to every subscriber with room, and DROPS for those without.
// Never blocks: the publisher is on the convergence path.
func (b *eventBus) publish(event Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- event:
		default:
			// Subscriber is behind. Dropping is the documented contract.
		}
	}
}

func (b *eventBus) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		writeError(w, http.StatusServiceUnavailable, "this daemon was started without a status reader")
		return
	}
	status, err := s.reader.Status(r.Context())
	if err != nil {
		s.logger.Warn("daemon: status read failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not read workspace status")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleEvents streams events until the client disconnects or the daemon stops.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	id, events := s.events.subscribe()
	defer s.events.unsubscribe(id)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// A heartbeat keeps an idle stream from looking hung and lets the handler
	// notice a client that went away without closing cleanly.
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, ok := <-events:
			if !ok {
				return
			}
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Kind, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
