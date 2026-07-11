package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testFSLock(path string) fsLock {
	return fsLock{
		mu:        &sync.Mutex{},
		path:      path,
		wait:      20 * time.Millisecond,
		heartbeat: time.Hour,
		stale:     time.Second,
		sleep:     time.Sleep,
	}
}

func writeFSLockOwner(t *testing.T, path string, owner fsLockOwner) {
	t.Helper()
	raw, err := json.Marshal(owner)
	if err != nil {
		t.Fatalf("marshal owner: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write owner: %v", err)
	}
}

func localFSLockOwner(t *testing.T, pid int, nonce string) fsLockOwner {
	t.Helper()
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	return fsLockOwner{
		Version:    1,
		PID:        pid,
		Hostname:   hostname,
		Nonce:      nonce,
		AcquiredAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func TestFSLockOwnerRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.lock")
	lock := testFSLock(path)
	release, err := lock.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	owner, ok := readFSLockOwner(path)
	if !ok {
		t.Fatal("lock file did not contain a parseable owner")
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	if owner.Version != 1 || owner.PID != os.Getpid() || owner.Hostname != hostname {
		t.Fatalf("unexpected owner: %+v", owner)
	}
	if len(owner.Nonce) != 32 {
		t.Fatalf("nonce length = %d, want 32 hex characters", len(owner.Nonce))
	}
	for _, char := range owner.Nonce {
		if !strings.ContainsRune("0123456789abcdef", char) {
			t.Fatalf("nonce %q is not lowercase hex", owner.Nonce)
		}
	}
	if _, err := time.Parse(time.RFC3339, owner.AcquiredAt); err != nil {
		t.Fatalf("acquired_at is not RFC3339: %v", err)
	}

	release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("lock remains after release: %v", err)
	}
}

func TestFSLockSecondAcquireTimesOutNamesOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.lock")
	first := testFSLock(path)
	release, err := first.acquire()
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	second := testFSLock(path)
	_, err = second.acquire()
	if err == nil {
		t.Fatal("second acquire unexpectedly succeeded")
	}
	hostname, hostErr := os.Hostname()
	if hostErr != nil {
		t.Fatalf("hostname: %v", hostErr)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) || !strings.Contains(err.Error(), "on "+hostname) {
		t.Fatalf("timeout did not name owner: %v", err)
	}
}

func TestFSLockLegacyBarePIDFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.lock")
	if err := os.WriteFile(path, []byte("12345\n"), 0o600); err != nil {
		t.Fatalf("write legacy lock: %v", err)
	}
	lock := testFSLock(path)
	if _, err := lock.acquire(); err == nil {
		t.Fatal("fresh legacy lock was broken")
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("backdate legacy lock: %v", err)
	}
	release, err := lock.acquire()
	if err != nil {
		t.Fatalf("acquire over stale legacy lock: %v", err)
	}
	release()
}

func TestFSLockCorruptJSON(t *testing.T) {
	t.Run("backdated is broken", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hub.lock")
		if err := os.WriteFile(path, []byte("{garbage"), 0o600); err != nil {
			t.Fatalf("write corrupt lock: %v", err)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("backdate corrupt lock: %v", err)
		}
		lock := testFSLock(path)
		release, err := lock.acquire()
		if err != nil {
			t.Fatalf("acquire over stale corrupt lock: %v", err)
		}
		release()
	})

	t.Run("fresh times out", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hub.lock")
		if err := os.WriteFile(path, []byte("{garbage"), 0o600); err != nil {
			t.Fatalf("write corrupt lock: %v", err)
		}
		lock := testFSLock(path)
		if _, err := lock.acquire(); err == nil {
			t.Fatal("fresh corrupt lock was broken")
		}
	})
}

func TestFSLockDeadPIDBrokenImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.lock")
	writeFSLockOwner(t, path, localFSLockOwner(t, 999999999, strings.Repeat("a", 32)))
	oldAlive := hubProcessAlive
	hubProcessAlive = func(int) bool { return false }
	t.Cleanup(func() { hubProcessAlive = oldAlive })

	lock := testFSLock(path)
	release, err := lock.acquire()
	if err != nil {
		t.Fatalf("acquire over dead owner: %v", err)
	}
	release()
}

func TestFSLockHeartbeatStopsWhenLockVanishes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.lock")
	lock := testFSLock(path)
	lock.heartbeat = 5 * time.Millisecond
	release, err := lock.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Simulate a break: the lock file disappears and a successor appears.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // let the heartbeat observe ErrNotExist and stop
	writeFSLockOwner(t, path, localFSLockOwner(t, os.Getpid(), strings.Repeat("d", 32)))
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) < 30*time.Minute {
		t.Fatal("stopped heartbeat still refreshed the successor's mtime")
	}
	release() // must not remove the successor (foreign nonce)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("release after break removed the successor's lock: %v", err)
	}
}

func TestFSLockEmptyFileUsesTTLPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lock := testFSLock(path)
	// Fresh mtime: not stale, second acquire times out.
	if _, err := lock.acquire(); err == nil {
		t.Fatal("empty fresh lock file was broken before the TTL")
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	release, err := lock.acquire()
	if err != nil {
		t.Fatalf("acquire over expired empty lock: %v", err)
	}
	release()
}

// TestFSLockPartialOwnerRecordUsesTTLPath (CodeRabbit): a parseable but
// INCOMPLETE record (e.g. a crash left only {"hostname":...}, PID 0) must be
// judged by the mtime TTL like any corrupt lock — never insta-broken via the
// "PID 0 is dead" path, and never held forever.
// TestFSLockPublishedRecordIsAlwaysComplete (CodeRabbit): the lock file is
// link-published from a fully-written staged record, so a concurrent reader
// must NEVER observe an empty or torn owner record — the create-then-write
// shape this replaces had exactly that window.
func TestFSLockPublishedRecordIsAlwaysComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.lock")
	stopPoll := make(chan struct{})
	pollDone := make(chan struct{})
	var torn atomic.Int32
	go func() {
		defer close(pollDone)
		for {
			select {
			case <-stopPoll:
				return
			default:
			}
			raw, err := os.ReadFile(path)
			if err == nil {
				var owner fsLockOwner
				if len(raw) == 0 || json.Unmarshal(raw, &owner) != nil || !validFSLockOwner(owner) {
					torn.Add(1)
					return
				}
			}
		}
	}()
	for i := 0; i < 50; i++ {
		lock := testFSLock(path)
		release, err := lock.acquire()
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		release()
	}
	close(stopPoll)
	<-pollDone
	if torn.Load() != 0 {
		t.Fatal("a concurrent reader observed an empty or torn owner record")
	}
}

func TestFSLockPartialOwnerRecordUsesTTLPath(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hub.lock")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("{\"hostname\":%q}", hostname)), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := testFSLock(path)
	// Fresh mtime: NOT stale (would have been insta-broken via dead-PID-0).
	if _, err := lock.acquire(); err == nil {
		t.Fatal("fresh partial owner record was broken before the TTL")
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	release, err := lock.acquire()
	if err != nil {
		t.Fatalf("acquire over expired partial record: %v", err)
	}
	release()
}

func TestFSLockRecycledPIDBrokenImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.lock")
	owner := localFSLockOwner(t, os.Getpid(), strings.Repeat("c", 32))
	owner.StartedAt = 12345 // some other, long-dead process's identity
	writeFSLockOwner(t, path, owner)
	oldStart := hubProcessStartTime
	hubProcessStartTime = func(int) (int64, error) { return 67890, nil }
	t.Cleanup(func() { hubProcessStartTime = oldStart })

	// PID is alive (it is ours) but the start identity differs: the recorded
	// holder is dead and its PID was recycled — break immediately, fresh mtime.
	lock := testFSLock(path)
	release, err := lock.acquire()
	if err != nil {
		t.Fatalf("acquire over recycled-pid owner: %v", err)
	}
	release()
}

func TestFSLockLivePIDNeverStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.lock")
	writeFSLockOwner(t, path, localFSLockOwner(t, os.Getpid(), strings.Repeat("b", 32)))
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("backdate live lock: %v", err)
	}

	lock := testFSLock(path)
	if _, err := lock.acquire(); err == nil {
		t.Fatal("live same-host owner was broken despite stale mtime")
	}
	owner, ok := readFSLockOwner(path)
	if !ok || owner.Nonce != strings.Repeat("b", 32) {
		t.Fatalf("live owner's lock was changed: %+v, parseable=%v", owner, ok)
	}
}

func TestFSLockReleaseAfterBrokenLeavesSuccessor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.lock")
	lock := testFSLock(path)
	release, err := lock.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("simulate break: %v", err)
	}
	successor := localFSLockOwner(t, os.Getpid(), strings.Repeat("c", 32))
	writeFSLockOwner(t, path, successor)

	release()
	owner, ok := readFSLockOwner(path)
	if !ok || owner.Nonce != successor.Nonce {
		t.Fatalf("release removed or changed successor: %+v, parseable=%v", owner, ok)
	}
}

func TestFSLockBreakDoubleReadRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.lock")
	first := localFSLockOwner(t, 11111, strings.Repeat("d", 32))
	successor := localFSLockOwner(t, os.Getpid(), strings.Repeat("e", 32))
	writeFSLockOwner(t, path, first)

	oldAlive := hubProcessAlive
	calls := 0
	hubProcessAlive = func(int) bool {
		calls++
		if calls == 1 {
			writeFSLockOwner(t, path, successor)
			return false
		}
		return true
	}
	t.Cleanup(func() { hubProcessAlive = oldAlive })

	lock := testFSLock(path)
	if _, err := lock.acquire(); err == nil {
		t.Fatal("acquire unexpectedly broke the replacement owner")
	}
	owner, ok := readFSLockOwner(path)
	if !ok || owner.Nonce != successor.Nonce {
		t.Fatalf("double-read breaker removed successor: %+v, parseable=%v", owner, ok)
	}
}
