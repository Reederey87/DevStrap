package hub

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// memS3 is an in-memory S3Client double for the Hub conformance suite (HUB-02).
// It is NOT a production backend. It models PutObject with If-None-Match,
// GetObject, ObjectExists, and bounded ListObjectsV2 with start-after pagination.
type memS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemS3() *memS3 {
	return &memS3{objects: make(map[string][]byte)}
}

func (m *memS3) PutObject(_ context.Context, key string, body []byte, ifNoneMatch bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ifNoneMatch {
		if _, ok := m.objects[key]; ok {
			// HUB-09: surface a typed precondition error so R2Hub can classify
			// a duplicate conditional put as an idempotent no-op instead of a
			// hard failure.
			return ErrPreconditionFailed
		}
	}
	m.objects[key] = body
	return nil
}

func (m *memS3) GetObject(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", dssync.ErrBlobNotFound, key)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, nil
}

func (m *memS3) ObjectExists(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok, nil
}

// DeleteObject removes an object. A missing object is not an error (idempotent
// delete), matching the S3Client contract for HUB-12/SEC-01.
func (m *memS3) DeleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *memS3) ListObjectsV2(_ context.Context, prefix, startAfter string, maxKeys int) ([]string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) && k > startAfter {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if maxKeys <= 0 {
		maxKeys = 1000
	}
	if len(keys) <= maxKeys {
		return keys, "", nil
	}
	page := keys[:maxKeys]
	return page, page[len(page)-1], nil
}

// TestEvent constructs a state.Event for conformance tests.
func makeEvent(id, deviceID string, hlc int64, seq int64, typ, payload string) state.Event {
	return state.Event{
		ID:          id,
		DeviceID:    deviceID,
		HLC:         hlc,
		Seq:         seq,
		Type:        typ,
		PayloadJSON: payload,
		ContentHash: state.ContentHash(payload),
	}
}
