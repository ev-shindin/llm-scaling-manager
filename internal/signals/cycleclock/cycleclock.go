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
		c.at = c.base.Now()
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
// reading otherwise.
func (c *Clock) Now() time.Time {
	if c == nil {
		return clock.RealClock{}.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.depth > 0 {
		return c.at
	}
	return c.base.Now()
}

// Since returns how long before Now t was.
func (c *Clock) Since(t time.Time) time.Duration {
	return c.Now().Sub(t)
}

var _ clock.PassiveClock = (*Clock)(nil)
