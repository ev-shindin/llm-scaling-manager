package cycleclock

import (
	"sync"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"
)

var t0 = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func TestUnpinnedReadsTheBaseClock(t *testing.T) {
	base := clocktesting.NewFakePassiveClock(t0)
	c := New(base)
	if got := c.Now(); !got.Equal(t0) {
		t.Fatalf("Now = %v, want %v", got, t0)
	}
	base.SetTime(t0.Add(time.Second))
	if got := c.Now(); !got.Equal(t0.Add(time.Second)) {
		t.Fatalf("Now after the base moved = %v, want %v", got, t0.Add(time.Second))
	}
	if got := c.Since(t0); got != time.Second {
		t.Fatalf("Since = %v, want 1s", got)
	}
}

func TestPinHoldsOneReading(t *testing.T) {
	base := clocktesting.NewFakePassiveClock(t0)
	c := New(base)
	release := c.Pin()
	base.SetTime(t0.Add(time.Minute))
	if got := c.Now(); !got.Equal(t0) {
		t.Fatalf("pinned Now = %v, want the reading at Pin, %v", got, t0)
	}
	if got := c.Since(t0); got != 0 {
		t.Fatalf("pinned Since(pin) = %v, want 0", got)
	}
	release()
	if got := c.Now(); !got.Equal(t0.Add(time.Minute)) {
		t.Fatalf("Now after release = %v, want the base clock, %v", got, t0.Add(time.Minute))
	}
}

func TestNestedPinSharesTheOuterReading(t *testing.T) {
	base := clocktesting.NewFakePassiveClock(t0)
	c := New(base)
	outer := c.Pin()
	base.SetTime(t0.Add(time.Minute))
	inner := c.Pin()
	if got := c.Now(); !got.Equal(t0) {
		t.Fatalf("nested pin Now = %v, want the outer reading %v", got, t0)
	}
	inner()
	inner() // a second release of the same pin releases nothing
	if got := c.Now(); !got.Equal(t0) {
		t.Fatalf("Now with the outer pin still held = %v, want %v", got, t0)
	}
	outer()
	if got := c.Now(); !got.Equal(t0.Add(time.Minute)) {
		t.Fatalf("Now after the last release = %v, want %v", got, t0.Add(time.Minute))
	}
}

func TestEachPinTakesAFreshReading(t *testing.T) {
	base := clocktesting.NewFakePassiveClock(t0)
	c := New(base)
	c.Pin()()
	base.SetTime(t0.Add(time.Hour))
	release := c.Pin()
	defer release()
	if got := c.Now(); !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("second cycle's Now = %v, want %v", got, t0.Add(time.Hour))
	}
}

func TestNilClockReadsTheWallClock(t *testing.T) {
	var c *Clock
	c.Pin()()
	before := time.Now()
	got := c.Now()
	if got.Before(before) || got.After(time.Now()) {
		t.Fatalf("nil Clock Now = %v, not between the wall readings around it", got)
	}
}

func TestNewNilBaseIsTheWallClock(t *testing.T) {
	c := New(nil)
	before := time.Now()
	got := c.Now()
	if got.Before(before) || got.After(time.Now()) {
		t.Fatalf("New(nil).Now = %v, not between the wall readings around it", got)
	}
}

func TestConcurrentReadersSeeThePin(t *testing.T) {
	base := clocktesting.NewFakePassiveClock(t0)
	c := New(base)
	release := c.Pin()
	defer release()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if got := c.Now(); !got.Equal(t0) {
					t.Errorf("concurrent Now = %v, want %v", got, t0)
					return
				}
			}
		}()
	}
	for i := range 100 {
		base.SetTime(t0.Add(time.Duration(i) * time.Second))
	}
	wg.Wait()
}
