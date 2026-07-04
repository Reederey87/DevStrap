package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Reederey87/DevStrap/internal/logging"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// defaultSweepLockTTL bounds how long a sweep lock is honored before a
// cooperating client may break it as stale (P4-HUB-12). A sweeper that crashes
// without releasing its lock cannot wedge the object plane forever.
const defaultSweepLockTTL = time.Hour

// hubSweepLock acquires the advisory sweep lock over the hub object plane so the
// destructive passes (`hub gc`, `hub compact`, `hub migrate-events`) run by
// COOPERATING clients do not interleave (P4-HUB-12). It is best-effort and
// ADVISORY: it serializes cooperating clients only, not a hostile writer, which
// is out of scope (spec/15).
//
// It creates the lock with a create-only conditional PUT. On a live conflict it
// refuses with the holder id (exitConflict). A lock older than its TTL — judged
// by the object's backend mtime, never its self-reported acquisition time, so a
// clock-skewed or hostile writer cannot extend it — is broken (deleted) and
// re-acquired ONCE. The returned release func deletes the lock and MUST be
// deferred by the caller on every path (success and error) so the lock is not
// leaked.
func hubSweepLock(ctx context.Context, store *state.Store, hub dssync.Hub, ttl time.Duration) (func(), error) {
	if ttl <= 0 {
		ttl = defaultSweepLockTTL
	}
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		return nil, err
	}
	hlc, err := store.CurrentHLC(ctx)
	if err != nil {
		return nil, err
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, fmt.Errorf("generate sweep-lock nonce: %w", err)
	}
	body, err := dssync.MarshalSweepLock(dssync.SweepLock{
		HolderDevice:  device.ID,
		AcquiredAtHLC: hlc,
		TTLSeconds:    int64(ttl / time.Second),
		Nonce:         hex.EncodeToString(nonceBytes),
	})
	if err != nil {
		return nil, err
	}
	// release deletes the lock ONLY when it still carries our exact body (nonce
	// included). If this sweeper overran its TTL and a successor stale-broke and
	// re-acquired, the successor's body differs, so we leave it alone rather than
	// delete a live lock. The GET→DELETE window is a narrow TOCTOU, acceptable
	// for an advisory lock (a successor that acquires in that sliver loses its
	// lock but only serializes cooperating sweepers — no correctness impact).
	release := func() {
		cur, _, gerr := hub.GetSweepLock(ctx)
		if gerr != nil {
			if !errors.Is(gerr, dssync.ErrSweepLockNotFound) {
				logging.Logger(ctx).Warn("hub sweep lock: could not read lock to release; a later sweeper will break it after its TTL",
					"err", gerr.Error())
			}
			return // already gone, or unreadable — nothing we can safely delete
		}
		if !bytes.Equal(cur, body) {
			logging.Logger(ctx).Warn("hub sweep lock: lock no longer ours (a successor stale-broke it); not releasing")
			return
		}
		if derr := hub.DeleteSweepLock(ctx); derr != nil {
			logging.Logger(ctx).Warn("hub sweep lock: release failed; a later sweeper will break it after its TTL",
				"err", derr.Error())
		}
	}
	// Fast path: create-only acquire.
	err = hub.PutSweepLock(ctx, body)
	if err == nil {
		return release, nil
	}
	if !errors.Is(err, dssync.ErrSweepLockHeld) {
		return nil, appError{code: exitNetwork, err: fmt.Errorf("acquire sweep lock: %w", err)}
	}
	// Conflict: inspect the current lock.
	raw, lastModified, gerr := hub.GetSweepLock(ctx)
	if errors.Is(gerr, dssync.ErrSweepLockNotFound) {
		// Released between our create and our read: retry the create ONCE.
		if perr := hub.PutSweepLock(ctx, body); perr != nil {
			return nil, sweepRaceRefusal(perr)
		}
		return release, nil
	}
	if gerr != nil {
		return nil, appError{code: exitNetwork, err: fmt.Errorf("read sweep lock: %w", gerr)}
	}
	held, _ := dssync.ParseSweepLock(raw)
	if !sweepLockStale(held, lastModified, time.Now()) {
		return nil, appError{code: exitConflict, err: fmt.Errorf(
			"another sweep is in progress (lock held by device %s); re-run once it finishes, or wait for the %s TTL to expire",
			held.HolderDevice, held.TTL())}
	}
	// Stale: break it and retry the create ONCE.
	logging.Logger(ctx).Warn("hub sweep lock: breaking a stale lock",
		"holder", held.HolderDevice, "ttl", held.TTL().String())
	if derr := hub.DeleteSweepLock(ctx); derr != nil {
		return nil, appError{code: exitNetwork, err: fmt.Errorf("break stale sweep lock: %w", derr)}
	}
	if perr := hub.PutSweepLock(ctx, body); perr != nil {
		return nil, sweepRaceRefusal(perr)
	}
	return release, nil
}

// sweepRaceRefusal maps a lost re-acquire race (another cooperating sweeper
// grabbed the lock between our break/observe and our retry) to a conflict
// refusal, and any other error to a network error.
func sweepRaceRefusal(err error) error {
	if errors.Is(err, dssync.ErrSweepLockHeld) {
		return appError{code: exitConflict, err: fmt.Errorf(
			"another sweep acquired the lock while we were breaking a stale one; re-run later")}
	}
	return appError{code: exitNetwork, err: fmt.Errorf("acquire sweep lock: %w", err)}
}

// sweepLockStale reports whether a held lock has outlived its TTL. Staleness is
// judged by the object's backend mtime (lastModified); the body's self-reported
// AcquiredAt is used only as a fallback when the backend cannot report an mtime,
// and a lock with no age signal at all is never broken (fail safe).
func sweepLockStale(held dssync.SweepLock, lastModified, now time.Time) bool {
	ttl := held.TTL()
	if ttl <= 0 {
		ttl = defaultSweepLockTTL
	}
	ref := lastModified
	if ref.IsZero() {
		ref = held.AcquiredAt()
	}
	if ref.IsZero() {
		return false
	}
	return now.Sub(ref) > ttl
}
