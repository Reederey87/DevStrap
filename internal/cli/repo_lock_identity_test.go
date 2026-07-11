package cli

import (
	"errors"
	"testing"
)

// TestProcessIdentityAliveMatrix pins the single identity check behind repo-lock
// staleness and the agent-run sweep (P7-GIT-03): dead PID reclaims, missing
// identity and indeterminate lookups keep the holder, mismatch reclaims.
func TestProcessIdentityAliveMatrix(t *testing.T) {
	oldAlive := repoLockProcessAlive
	oldStart := processStartTime
	t.Cleanup(func() {
		repoLockProcessAlive = oldAlive
		processStartTime = oldStart
	})

	repoLockProcessAlive = func(int) bool { return false }
	if processIdentityAlive(1, 222) {
		t.Fatal("dead PID must not be identity-alive")
	}

	repoLockProcessAlive = func(int) bool { return true }
	processStartTime = func(int) (int64, error) { return 222, nil }
	if !processIdentityAlive(1, 0) {
		t.Fatal("live PID with no recorded identity must stay alive")
	}
	if !processIdentityAlive(1, 222) {
		t.Fatal("live PID with matching identity must stay alive")
	}
	if processIdentityAlive(1, 111) {
		t.Fatal("live PID with mismatched identity must be reclaimed")
	}

	processStartTime = func(int) (int64, error) { return 0, errors.New("lookup failed") }
	if !processIdentityAlive(1, 222) {
		t.Fatal("indeterminate lookup must never reclaim a holder")
	}
}
