//go:build linux

package platform

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ProcessStartTime returns the process's raw /proc starttime identity (clock
// ticks since boot). The value is opaque and is only suitable for equality
// comparisons on the same host and boot.
func ProcessStartTime(pid int) (int64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("read process stat: %w", err)
	}
	// Field 2 (comm) is parenthesized but may itself contain spaces or
	// parentheses. Fields 3 onward therefore begin after the last ')'.
	commEnd := strings.LastIndexByte(string(raw), ')')
	if commEnd < 0 {
		return 0, fmt.Errorf("parse process stat: missing comm terminator")
	}
	fields := strings.Fields(string(raw[commEnd+1:]))
	if len(fields) <= 19 {
		return 0, fmt.Errorf("parse process stat: got %d post-comm fields, need at least 20", len(fields))
	}
	// ParseInt (not ParseUint+cast): starttime is a non-negative tick count
	// that fits int64 for any realistic uptime, and a signed parse keeps gosec
	// G115 out of the picture.
	startedAt, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process starttime: %w", err)
	}
	if startedAt < 0 {
		return 0, fmt.Errorf("parse process starttime: negative tick count %d", startedAt)
	}
	return startedAt, nil
}
