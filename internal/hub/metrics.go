package hub

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// Metrics accumulates in-process operation and byte counters for a hub backend
// (P4-HUB-14). Every real backend (r2/s3, git carrier, folder) drives its
// object I/O through the S3Client boundary, so metering that one seam
// (meteredS3) captures op counts and transferred bytes for all of them without
// touching each of R2Hub's ~two dozen methods. The counters are process-local
// and best-effort observability, not a persisted metric; `doctor --remote`
// reports the snapshot accumulated during its probe. Safe for concurrent use.
type Metrics struct {
	mu        sync.Mutex
	ops       map[string]int64
	bytesUp   int64
	bytesDown int64
}

// NewMetrics returns an empty counter set.
func NewMetrics() *Metrics { return &Metrics{ops: map[string]int64{}} }

func (m *Metrics) op(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.ops == nil {
		m.ops = map[string]int64{}
	}
	m.ops[name]++
	m.mu.Unlock()
}

func (m *Metrics) up(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.mu.Lock()
	m.bytesUp += int64(n)
	m.mu.Unlock()
}

func (m *Metrics) down(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.mu.Lock()
	m.bytesDown += int64(n)
	m.mu.Unlock()
}

// MetricsSnapshot is an immutable copy of a Metrics for reporting.
type MetricsSnapshot struct {
	Ops       map[string]int64
	TotalOps  int64
	BytesUp   int64
	BytesDown int64
}

// Snapshot copies the current counters.
func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ops := make(map[string]int64, len(m.ops))
	var total int64
	for k, v := range m.ops {
		ops[k] = v
		total += v
	}
	return MetricsSnapshot{Ops: ops, TotalOps: total, BytesUp: m.bytesUp, BytesDown: m.bytesDown}
}

// String renders a stable one-line summary ("12 ops (get 6, list 4, put 2),
// up 4321 B, down 87654 B") for doctor and logs.
func (s MetricsSnapshot) String() string {
	names := make([]string, 0, len(s.Ops))
	for name := range s.Ops {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s %d", name, s.Ops[name]))
	}
	detail := ""
	if len(parts) > 0 {
		detail = " (" + strings.Join(parts, ", ") + ")"
	}
	return fmt.Sprintf("%d ops%s, up %d B, down %d B", s.TotalOps, detail, s.BytesUp, s.BytesDown)
}

// meteredS3 wraps an S3Client and records one op per call plus payload bytes on
// the transfers that carry them (put bodies count up, fetched bodies count
// down). It is the single instrumentation point for P4-HUB-14: every backend
// composes R2Hub over an S3Client, so wrapping the client here counts hub I/O
// for r2/s3, the git carrier, and the folder carrier alike.
type meteredS3 struct {
	inner S3Client
	m     *Metrics
}

// newMeteredS3 wraps inner so its calls accumulate into m.
func newMeteredS3(inner S3Client, m *Metrics) meteredS3 {
	return meteredS3{inner: inner, m: m}
}

func (s meteredS3) PutObject(ctx context.Context, key string, body []byte, ifNoneMatch bool) error {
	s.m.op("put")
	err := s.inner.PutObject(ctx, key, body, ifNoneMatch)
	if err == nil {
		s.m.up(len(body))
	}
	return err
}

func (s meteredS3) GetObject(ctx context.Context, key string) ([]byte, error) {
	s.m.op("get")
	data, err := s.inner.GetObject(ctx, key)
	s.m.down(len(data))
	return data, err
}

func (s meteredS3) ObjectExists(ctx context.Context, key string) (bool, error) {
	s.m.op("stat")
	return s.inner.ObjectExists(ctx, key)
}

func (s meteredS3) DeleteObject(ctx context.Context, key string) error {
	s.m.op("delete")
	return s.inner.DeleteObject(ctx, key)
}

func (s meteredS3) ListObjectsV2(ctx context.Context, prefix, startAfter string, maxKeys int) ([]dssync.BlobInfo, string, error) {
	s.m.op("list")
	return s.inner.ListObjectsV2(ctx, prefix, startAfter, maxKeys)
}

func (s meteredS3) ListCommonPrefixes(ctx context.Context, prefix, delimiter string) ([]string, error) {
	s.m.op("list")
	return s.inner.ListCommonPrefixes(ctx, prefix, delimiter)
}

func (s meteredS3) GetObjectWithETag(ctx context.Context, key string) ([]byte, string, error) {
	s.m.op("get")
	data, etag, err := s.inner.GetObjectWithETag(ctx, key)
	s.m.down(len(data))
	return data, etag, err
}

func (s meteredS3) PutObjectIfMatch(ctx context.Context, key string, body []byte, etag string) error {
	s.m.op("put")
	err := s.inner.PutObjectIfMatch(ctx, key, body, etag)
	if err == nil {
		s.m.up(len(body))
	}
	return err
}

func (s meteredS3) StatObject(ctx context.Context, key string) (dssync.BlobInfo, error) {
	s.m.op("stat")
	return s.inner.StatObject(ctx, key)
}
