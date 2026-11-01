// Package cycleclock is the one clock an optimize cycle reads.
//
// The engine pins it once per cycle, and every stamp and every age taken
// inside the cycle -- by the analyzers, by the capacity store, by the windows
// they keep -- reads that one instant. Outside a pin it reads its base clock.
//
// One reading per cycle is what makes a cycle replayable: a replay drives the
// base clock from the recording, and with no other read of wall time in the
// learned state, the same recording gives the same state whenever it is
// replayed. It also means two stamps inside one cycle never differ by the few
// milliseconds the cycle took to run.
package cycleclock

import (
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// Clock is a clock.PassiveClock that holds one reading of its base clock while
// pinned. Safe for concurrent use.
//
// A nil *Clock reads the wall clock and pins nothing, so a zero-valued engine
// built by a test behaves as one built before this type existed.
type Clock struct {
	base clock.PassiveClock

	mu sync.Mutex
	// depth counts the pins not yet released; at is the reading they share.
	depth int
	at    time.Time
}

// New returns a Clock over base, or over the wall clock when base is nil.
func New(base clock.PassiveClock) *Clock {
	if base == nil {
		base = clock.RealClock{}
	}
	return &Clock{base: base}
}

// Pin reads the base clock once and holds that reading until the returned
// release is called: every Now and Since in between answers from it.
//
// A Pin taken while another is held shares the outer reading rather than
// taking a new one, so a nested caller cannot split one cycle into two
// instants. The reading is dropped when the last pin is released. Calling a
// release more than once releases once.
//
// Pin has one caller, the engine's optimize loop, which runs one cycle at a
// time; readers on other goroutines are safe and see the pinned instant. Two
// loops pinning one Clock would share a reading, so each loop needs its own.
func (c *Clock) Pin() (release func()) {
	if c == nil {
		return func() {}
	}
	c.mu.Lock()
	if c.depth == 0 {
		c.at = wall(c.base.Now())
	}
	c.depth++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.depth--
			c.mu.Unlock()
		})
	}
}

// Now returns the pinned reading while a pin is held, and the base clock's
// reading otherwise. Either way it carries no monotonic reading (wall).
func (c *Clock) Now() time.Time {
	if c == nil {
		return wall(clock.RealClock{}.Now())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.depth > 0 {
		return c.at
	}
	return wall(c.base.Now())
}

// wall strips t's monotonic clock reading, keeping its wall time.
//
// A recording keeps only wall times, so a replay subtracts wall times. A live
// time.Now also carries a monotonic reading, and Sub between two that both do
// uses the monotonic one. time.Now takes the wall and the monotonic readings
// separately, so the gap between them varies by nanoseconds from one call to
// the next, and a wall-clock step moves one and not the other. An age taken
// live then differed from the same age replayed, and the replay of a cycle
// could not match the recording of it. Every stamp and age read from this
// clock is wall-only, live and replayed alike.
//
// The cost: ages now follow wall-clock steps, as learned state restored after
// a restart already did. A forward step ages everything at once, and a step
// longer than a timeout evicts or expires what it guards. A backward step
// gives a negative age, which holds until the wall clock catches up: a
// timeout is extended, and a wait for an age to pass (sample spacing,
// settling) waits longer. Only a step does this, not a gradual NTP
// adjustment.
func wall(t time.Time) time.Time {
	return t.Round(0)
}

// Since returns how long before Now t was. It is negative when t is after
// Now, as after a backward wall-clock step (see wall).
func (c *Clock) Since(t time.Time) time.Duration {
	return c.Now().Sub(t)
}

var _ clock.PassiveClock = (*Clock)(nil)
