package cli

import (
	"math/rand/v2"
	"testing"
	"time"
)

// P5-QUAL-03: the jitter bound is clamped to >= 1 so rand.Int64N never panics
// on sub-10ns intervals, and the run loop never crashes computing jitter.
func TestRunLoopJitterBoundNeverPanics(t *testing.T) {
	for _, interval := range []time.Duration{1, 5, 9, 10, 100, time.Second, 5 * time.Minute} {
		bound := runLoopJitterBound(interval)
		if bound < 1 {
			t.Fatalf("runLoopJitterBound(%v) = %d, want >= 1", interval, bound)
		}
		// Must not panic for any bound.
		_ = rand.Int64N(bound)
	}
}
