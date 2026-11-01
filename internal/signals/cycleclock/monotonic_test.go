package cycleclock

import (
	"strings"
	"testing"
	"time"
)

// hasMonotonic reports whether t carries a monotonic clock reading, which
// time.Time.String prints as an "m=" suffix.
func hasMonotonic(t time.Time) bool {
	return strings.Contains(t.String(), " m=")
}

// A recording keeps wall times only, so a replay subtracts wall times. A
// reading that kept the monotonic clock was subtracted by it live, and an age
// taken live then differed from the replayed one by the nanoseconds between
// time.Now's separate wall and monotonic readings, or by a wall-clock step --
// the K2Age a replay could not match. Every reading
// this clock hands out must be wall-only, from the real clock as from any
// other.
func TestReadingsCarryNoMonotonicClock(t *testing.T) {
	if !hasMonotonic(time.Now()) {
		t.Fatal("the premise: time.Now carries a monotonic reading on this platform")
	}

	c := New(nil)
	if got := c.Now(); hasMonotonic(got) {
		t.Errorf("unpinned Now carries a monotonic reading: %v", got)
	}

	release := c.Pin()
	pinned := c.Now()
	if hasMonotonic(pinned) {
		t.Errorf("pinned Now carries a monotonic reading: %v", pinned)
	}
	release()

	var nilClock *Clock
	if got := nilClock.Now(); hasMonotonic(got) {
		t.Errorf("a nil Clock's Now carries a monotonic reading: %v", got)
	}
}
