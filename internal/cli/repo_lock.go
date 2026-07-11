package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Reederey87/DevStrap/internal/platform"
)

var (
	repoLockStaleAfter   = 30 * time.Minute
	repoLockProcessAlive = processAlive
	processStartTime     = platform.ProcessStartTime
)

type repoLockInfo struct {
	PID        int    `json:"pid"`
	StartedAt  int64  `json:"started_at,omitempty"`
	Hostname   string `json:"hostname"`
	AcquiredAt string `json:"acquired_at"`
}

func repoLockDir(home string) string { return filepath.Join(home, "locks") }

func repoLockPath(home, projectID string) string {
	return filepath.Join(repoLockDir(home), projectID+".lock")
}

// readRepoLock reports the current lock holder for a project, whether a lock
// file exists, and whether it is stale (dead holder or past the age window).
func readRepoLock(home, projectID string) (repoLockInfo, bool, bool, error) {
	lockPath := repoLockPath(home, projectID)
	//nolint:gosec // lockPath is built under the DevStrap home locks directory.
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return repoLockInfo{}, false, false, nil
		}
		return repoLockInfo{}, false, false, fmt.Errorf("read repo lock: %w", err)
	}
	var info repoLockInfo
	_ = json.Unmarshal(raw, &info)
	stale, err := repoLockIsStale(lockPath)
	if err != nil {
		return info, true, false, err
	}
	return info, true, stale, nil
}

// clearRepoLock removes a project's repo lock. It refuses to clear a live
// (non-stale) lock unless force is set. It returns whether a lock was cleared.
func clearRepoLock(home, projectID string, force bool) (bool, error) {
	_, exists, stale, err := readRepoLock(home, projectID)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if !stale && !force {
		return false, appError{code: exitConflict, err: fmt.Errorf("repo lock for %s is held by a live process; pass --force to clear", projectID)}
	}
	if err := os.Remove(repoLockPath(home, projectID)); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("remove repo lock: %w", err)
	}
	return true, nil
}

func acquireRepoLock(home, projectID string) (func(), error) {
	lockDir := repoLockDir(home)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	lockPath := repoLockPath(home, projectID)
	unlock, err := tryAcquireRepoLock(lockPath)
	if err == nil {
		return unlock, nil
	}
	if !os.IsExist(err) {
		return nil, fmt.Errorf("create repo lock: %w", err)
	}
	stale, staleErr := repoLockIsStale(lockPath)
	if staleErr != nil {
		return nil, staleErr
	}
	if !stale {
		return nil, appError{code: exitConflict, err: fmt.Errorf("repo operation already in progress: %s", projectID)}
	}
	if err := removeStaleRepoLock(lockPath); err != nil {
		return nil, err
	}
	unlock, err = tryAcquireRepoLock(lockPath)
	if err != nil {
		if os.IsExist(err) {
			return nil, appError{code: exitConflict, err: fmt.Errorf("repo operation already in progress: %s", projectID)}
		}
		return nil, fmt.Errorf("create repo lock: %w", err)
	}
	return unlock, nil
}

func tryAcquireRepoLock(lockPath string) (func(), error) {
	//nolint:gosec // lockPath is built by acquireRepoLock under the DevStrap home locks directory.
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	startedAt, _ := processStartTime(os.Getpid())
	info := repoLockInfo{
		PID:        os.Getpid(),
		StartedAt:  startedAt,
		Hostname:   hostname(),
		AcquiredAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.MarshalIndent(info, "", "  ")
	if err == nil {
		_, err = file.Write(append(raw, '\n'))
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(lockPath)
		if err != nil {
			return nil, fmt.Errorf("write repo lock: %w", err)
		}
		return nil, fmt.Errorf("close repo lock: %w", closeErr)
	}
	return func() { _ = os.Remove(lockPath) }, nil
}

func repoLockIsStale(lockPath string) (bool, error) {
	//nolint:gosec // lockPath is built by acquireRepoLock under the DevStrap home locks directory.
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, fmt.Errorf("read repo lock: %w", err)
	}
	var info repoLockInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		stat, statErr := os.Stat(lockPath)
		if statErr != nil {
			return false, fmt.Errorf("stat malformed repo lock: %w", statErr)
		}
		return time.Since(stat.ModTime()) > repoLockStaleAfter, nil
	}
	acquiredAt, err := time.Parse(time.RFC3339Nano, info.AcquiredAt)
	if err != nil {
		return false, fmt.Errorf("parse repo lock acquired_at: %w", err)
	}
	// GIT-01/P7-GIT-03: same-host liveness is authoritative over age, but a
	// recorded process start-time identity must also match so PID reuse cannot
	// keep a crashed holder's lock alive forever. The age-based fallback applies
	// only when liveness is indeterminate (cross-host) or the PID is confirmed
	// dead.
	if info.Hostname == hostname() && info.PID > 0 {
		return !processIdentityAlive(info.PID, info.StartedAt), nil
	}
	return time.Since(acquiredAt) > repoLockStaleAfter, nil
}

func removeStaleRepoLock(lockPath string) error {
	//nolint:gosec // lockPath is built by acquireRepoLock under the DevStrap home locks directory.
	before, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read stale repo lock: %w", err)
	}
	stale, err := repoLockIsStale(lockPath)
	if err != nil {
		return err
	}
	if !stale {
		return nil
	}
	//nolint:gosec // lockPath is built by acquireRepoLock under the DevStrap home locks directory.
	after, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reread stale repo lock: %w", err)
	}
	if !bytes.Equal(before, after) {
		return nil
	}
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale repo lock: %w", err)
	}
	return nil
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// processIdentityAlive reports whether pid is alive AND, when a start-time
// identity was recorded, still the same process (PID-reuse guard, P7-GIT-03).
// It is the single identity check behind both repo-lock staleness and the
// agent-run sweep; liveness routes through the repoLockProcessAlive seam so
// tests can fake it in one place.
func processIdentityAlive(pid int, startedAt int64) bool {
	if !repoLockProcessAlive(pid) {
		return false
	}
	if startedAt == 0 {
		return true
	}
	current, err := processStartTime(pid)
	if err != nil {
		return true // Indeterminate: never reclaim a holder we cannot disprove.
	}
	return current == startedAt
}

func hostname() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "unknown"
	}
	return host
}
