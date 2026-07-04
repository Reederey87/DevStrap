package sync

// hlc_property_test.go is the rapid property layer for the hybrid logical clock,
// complementing the fixed-example assertions in hlc_test.go. Time is injected
// through the HLC.Now seam and all wall-clock offsets are rapid-drawn; no
// math/rand and no real time.Now — clocks are frozen via time.UnixMilli exactly
// like hlc_test.go.

import (
	"fmt"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// TestHLCInterleavingMonotonic drives random interleavings of Send and Receive
// under a wall clock that steps both forward and backward. Invariant: every
// SUCCESSFUL Send or Receive strictly advances the clock (so timestamps are
// globally, strictly increasing regardless of wall-clock jumps), and a Receive
// beyond MaxSkew is rejected without mutating the clock.
func TestHLCInterleavingMonotonic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		const base = int64(1_000_000)
		now := base
		clock := &HLC{
			Now:     func() time.Time { return time.UnixMilli(now) },
			MaxSkew: time.Duration(rapid.IntRange(1, 600).Draw(t, "max_skew_s")) * time.Second,
		}
		maxSkewMS := clock.MaxSkew.Milliseconds()

		prev := clock.Last // 0
		ops := rapid.IntRange(1, 60).Draw(t, "ops")
		for i := 0; i < ops; i++ {
			// The wall clock may step forward or backward between operations.
			now += int64(rapid.IntRange(-2000, 2000).Draw(t, fmt.Sprintf("wall_%d", i)))

			if rapid.Bool().Draw(t, fmt.Sprintf("is_send_%d", i)) {
				got := clock.Send()
				if got <= prev {
					t.Fatalf("Send #%d regressed: got %d <= prev %d (now=%d)", i, got, prev, now)
				}
				if clock.Last != got {
					t.Fatalf("Send #%d: Last %d != returned %d", i, clock.Last, got)
				}
				prev = got
				continue
			}

			// Receive: the remote physical time is now+offset; offset may exceed
			// MaxSkew so the rejection path is exercised too.
			offset := int64(rapid.IntRange(-5000, int(maxSkewMS)+5000).Draw(t, fmt.Sprintf("recv_off_%d", i)))
			remoteLogical := int64(rapid.IntRange(0, hlcLogicalMask).Draw(t, fmt.Sprintf("recv_log_%d", i)))
			remote := pack(now+offset, remoteLogical)
			before := clock.Last
			got, err := clock.Receive(remote)
			if offset > maxSkewMS {
				if err == nil {
					t.Fatalf("Receive #%d accepted a beyond-skew offset %d > %d", i, offset, maxSkewMS)
				}
				if clock.Last != before || got != before {
					t.Fatalf("Receive #%d rejection mutated the clock: before=%d Last=%d got=%d", i, before, clock.Last, got)
				}
				continue
			}
			if err != nil {
				t.Fatalf("Receive #%d rejected a within-skew offset %d <= %d: %v", i, offset, maxSkewMS, err)
			}
			if got <= prev {
				t.Fatalf("Receive #%d regressed: got %d <= prev %d (remote=%d now=%d)", i, got, prev, remote, now)
			}
			if clock.Last != got {
				t.Fatalf("Receive #%d: Last %d != returned %d", i, clock.Last, got)
			}
			prev = got
		}
	})
}

// TestHLCReceiveSkewBoundary pins the accept/reject decision to the documented
// boundary. Invariant: Receive rejects a remote HLC EXACTLY when its physical
// component is strictly more than MaxSkew ahead of local time (offset ==
// MaxSkew is accepted; offset == MaxSkew+1 is rejected).
func TestHLCReceiveSkewBoundary(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nowMS := int64(rapid.IntRange(1, 1_000_000).Draw(t, "now"))
		skewMS := int64(rapid.IntRange(1, 600_000).Draw(t, "skew"))
		clock := &HLC{
			Now:     func() time.Time { return time.UnixMilli(nowMS) },
			MaxSkew: time.Duration(skewMS) * time.Millisecond,
		}
		// Straddle the boundary so both sides (and the exact edge) are hit.
		offset := int64(rapid.IntRange(int(skewMS)-3, int(skewMS)+3).Draw(t, "offset"))
		_, err := clock.Receive(pack(nowMS+offset, 0))
		if offset > skewMS {
			if err == nil {
				t.Fatalf("offset %d > skew %d must be rejected", offset, skewMS)
			}
			return
		}
		if err != nil {
			t.Fatalf("offset %d <= skew %d must be accepted: %v", offset, skewMS, err)
		}
	})
}

// TestHLCLogicalOverflowMonotonic exercises the logical-counter overflow path
// under a FROZEN wall clock, so every Send must advance the logical counter and
// roll the physical component when it saturates. Invariant: Send stays strictly
// increasing across the overflow boundary, and exhausting the logical space
// provably advances the physical component past the frozen wall clock.
func TestHLCLogicalOverflowMonotonic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		const nowMS = int64(1_000_000)
		clock := &HLC{Now: func() time.Time { return time.UnixMilli(nowMS) }}
		start := int64(rapid.IntRange(0, hlcLogicalMask).Draw(t, "start_logical"))
		clock.Last = pack(nowMS, start)

		sends := rapid.IntRange(1, 200).Draw(t, "sends")
		prev := clock.Last
		sawPhysicalAdvance := false
		for i := 0; i < sends; i++ {
			got := clock.Send()
			if got <= prev {
				t.Fatalf("frozen-clock Send #%d regressed: got %d <= prev %d", i, got, prev)
			}
			if physical, _ := unpack(got); physical > nowMS {
				sawPhysicalAdvance = true
			}
			prev = got
		}
		// If the draw exhausted the remaining logical headroom, the overflow
		// path MUST have carried into the physical component.
		if start+int64(sends) > hlcLogicalMask && !sawPhysicalAdvance {
			t.Fatalf("exhausted logical space (start=%d sends=%d) but physical never advanced past %d", start, sends, nowMS)
		}
	})
}
