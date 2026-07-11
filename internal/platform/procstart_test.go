//go:build linux || darwin

package platform

import (
	"os"
	"testing"
)

func TestProcessStartTime(t *testing.T) {
	first, err := ProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("ProcessStartTime(own pid): %v", err)
	}
	if first <= 0 {
		t.Fatalf("ProcessStartTime(own pid) = %d, want > 0", first)
	}
	second, err := ProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("ProcessStartTime(own pid) second call: %v", err)
	}
	if second != first {
		t.Fatalf("ProcessStartTime(own pid) changed: %d -> %d", first, second)
	}
	if _, err := ProcessStartTime(1 << 30); err == nil {
		t.Fatal("ProcessStartTime(nonexistent pid) succeeded, want error")
	}
}
