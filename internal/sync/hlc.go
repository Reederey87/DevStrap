package sync

import (
	"fmt"
	"sync"
	"time"
)

const (
	hlcLogicalBits = 16
	hlcLogicalMask = (1 << hlcLogicalBits) - 1
	defaultMaxSkew = 5 * time.Minute
)

type HLC struct {
	mu      sync.Mutex
	Last    int64
	Now     func() time.Time
	MaxSkew time.Duration
}

func (h *HLC) Send() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := h.physicalMillis()
	lastPhysical, lastLogical := unpack(h.Last)
	switch {
	case now > lastPhysical:
		h.Last = pack(now, 0)
	case lastLogical < hlcLogicalMask:
		h.Last = pack(lastPhysical, lastLogical+1)
	default:
		h.Last = pack(lastPhysical+1, 0)
	}
	return h.Last
}

func (h *HLC) Receive(remote int64) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := h.physicalMillis()
	maxSkew := h.MaxSkew
	if maxSkew == 0 {
		maxSkew = defaultMaxSkew
	}
	remotePhysical, remoteLogical := unpack(remote)
	if remotePhysical-now > maxSkew.Milliseconds() {
		return h.Last, fmt.Errorf("remote HLC is %s ahead of local clock, exceeds max skew %s", time.Duration(remotePhysical-now)*time.Millisecond, maxSkew)
	}
	localPhysical, localLogical := unpack(h.Last)
	maxPhysical := maxInt64(now, localPhysical, remotePhysical)
	var logical int64
	switch {
	case maxPhysical == localPhysical && maxPhysical == remotePhysical:
		logical = maxInt64(localLogical, remoteLogical) + 1
	case maxPhysical == localPhysical:
		logical = localLogical + 1
	case maxPhysical == remotePhysical:
		logical = remoteLogical + 1
	default:
		logical = 0
	}
	if logical > hlcLogicalMask {
		maxPhysical++
		logical = 0
	}
	h.Last = pack(maxPhysical, logical)
	return h.Last, nil
}

func (h *HLC) physicalMillis() int64 {
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	return now.UnixMilli()
}

func pack(physical, logical int64) int64 {
	return (physical << hlcLogicalBits) | logical
}

func unpack(value int64) (physical int64, logical int64) {
	return value >> hlcLogicalBits, value & hlcLogicalMask
}

func maxInt64(values ...int64) int64 {
	max := values[0]
	for _, value := range values[1:] {
		if value > max {
			max = value
		}
	}
	return max
}
