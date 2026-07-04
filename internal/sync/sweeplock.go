package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrSweepLockNotFound signals that no sweep-lock object is present on the hub
// (nothing to break or read). It wraps the retention/blob not-found family only
// loosely — callers test it explicitly.
var ErrSweepLockNotFound = errors.New("sweep lock not found")

// ErrSweepLockHeld signals that a create-only sweep-lock PUT lost to an existing
// lock object (another sweeper, or a stale lock that must be broken first).
var ErrSweepLockHeld = errors.New("sweep lock already held")

// SweepLock is the advisory sweep-lock body serialized to
// meta/sweep.lock (P4-HUB-12). It coordinates the destructive hub passes
// (`hub gc`, `hub compact`, `hub migrate-events`) among COOPERATING clients so
// they do not interleave; it is not a defense against a hostile writer, which
// is out of scope (spec/15). Staleness is judged by the object's backend mtime
// (R2 ListObjectsV2 / FileHub file mtime), never by AcquiredAtHLC — a hostile
// or clock-skewed self-report must not extend a lock. AcquiredAtHLC is the
// fallback age source used only when a backend cannot report an object mtime.
type SweepLock struct {
	HolderDevice  string `json:"holder_device"`
	AcquiredAtHLC int64  `json:"acquired_at_hlc"`
	TTLSeconds    int64  `json:"ttl_seconds"`
	// Nonce is a per-acquire random token (crypto/rand hex). Release deletes the
	// lock object only when its bytes still match the exact body this acquire
	// wrote — so a sweeper that overran its TTL and had its lock stale-broken by
	// a successor cannot then delete the SUCCESSOR's lock (the nonce differs).
	Nonce string `json:"nonce"`
}

// MarshalSweepLock serializes a sweep-lock body.
func MarshalSweepLock(l SweepLock) ([]byte, error) {
	raw, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("marshal sweep lock: %w", err)
	}
	return raw, nil
}

// ParseSweepLock deserializes a sweep-lock body.
func ParseSweepLock(raw []byte) (SweepLock, error) {
	var l SweepLock
	if err := json.Unmarshal(raw, &l); err != nil {
		return SweepLock{}, fmt.Errorf("parse sweep lock: %w", err)
	}
	return l, nil
}

// TTL returns the lock's configured time-to-live.
func (l SweepLock) TTL() time.Duration {
	return time.Duration(l.TTLSeconds) * time.Second
}

// AcquiredAt derives a wall-clock acquisition time from the HLC physical
// component. It is the FALLBACK age source used only when a backend cannot
// report an object mtime; the primary staleness judgment uses the backend's
// LastModified.
func (l SweepLock) AcquiredAt() time.Time {
	physicalMs, _ := unpack(l.AcquiredAtHLC)
	return time.UnixMilli(physicalMs)
}
