package capacity

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The two clocks, tested on the type that owns them.
//
// Every assertion about these was previously indirect, through the saturation
// analyzer's specs: in this package Stale had no direct test at all, and
// Touch, ObservedAt and WriteGapExceeds had none either. That is how the
// collapsed-clock defect survived -- the type's contract was only ever
// exercised in the combinations the analyzer happened to produce, so a change
// that broke the contract but kept those combinations working would pass.
//
// The contract in one sentence: a READ keeps a window alive without making its
// figure newer. Everything below is a corner of that.
var _ = Describe("RollingAverage's two clocks", func() {
	const (
		gap   = time.Hour
		short = time.Millisecond
	)

	It("starts both clocks together, so a new window is neither stale nor gapped", func() {
		ra := NewRollingAverage(10)
		Expect(ra.Stale(short)).To(BeFalse())
		Expect(ra.WriteGapExceeds(short)).To(BeFalse())
	})

	It("fails safe on a zero value, which the constructor is the only way to avoid", func() {
		// Nothing constructs one this way today (NewRollingAverage is the only
		// construction site). If something ever does, both predicates must
		// report the CONSERVATIVE answer -- evict it, and treat the next
		// observation as a new episode -- rather than silently keeping a
		// window whose timestamps are the zero time.
		var ra RollingAverage
		Expect(ra.Stale(gap)).To(BeTrue())
		Expect(ra.WriteGapExceeds(gap)).To(BeTrue())
	})

	DescribeTable("what each mutator advances",
		func(apply func(*RollingAverage), advancesWrite bool) {
			ra := NewRollingAverage(10)
			ra.Add(1) // so RaiseLast has a value to raise
			// Age BOTH clocks, then apply the mutator and see which moved.
			ra.ObservedAt(time.Now().Add(-2 * gap))
			Expect(ra.Stale(gap)).To(BeTrue(), "precondition: both clocks are old")
			Expect(ra.WriteGapExceeds(gap)).To(BeTrue())

			apply(ra)

			Expect(ra.Stale(gap)).To(BeFalse(),
				"every mutator counts as USE: anything that touches a window means a "+
					"decision still depends on it, and the sweep must not take it")
			Expect(ra.WriteGapExceeds(gap)).To(Equal(!advancesWrite))
		},
		Entry("Add is a write", func(ra *RollingAverage) { ra.Add(2) }, true),
		Entry("RaiseLast is a write", func(ra *RollingAverage) { ra.RaiseLast(99) }, true),
		Entry("Observe is a write with no new value", func(ra *RollingAverage) { ra.Observe() }, true),
		Entry("Touch is NOT a write", func(ra *RollingAverage) { ra.Touch() }, false),
	)

	It("keeps a read-only window alive forever without making its figure newer", func() {
		// The whole point, and the case the collapsed clock got wrong. A
		// window written once and then only READ -- which is what a bucket on
		// a fleet that has stopped saturating looks like -- must never expire,
		// and must still report the write gap so a new observation resets it
		// instead of blending into a figure from before the gap.
		ra := NewRollingAverage(10)
		ra.Add(50000)
		ra.ObservedAt(time.Now().Add(-30 * 24 * time.Hour))

		for i := 0; i < 50; i++ {
			ra.Touch()
		}

		Expect(ra.Stale(gap)).To(BeFalse(), "reads keep it; the sweep must not take it")
		Expect(ra.WriteGapExceeds(gap)).To(BeTrue(),
			"but nothing has been WRITTEN for a month, and the producer has to be able "+
				"to see that -- collapsed onto one field, the reads hid it and two "+
				"episodes a month apart were averaged together")
		Expect(ra.WriteAge()).To(BeNumerically(">", 29*24*time.Hour),
			"and the age must be reportable, since nothing refuses a window on age any more")
	})

	It("reports an age that tracks the last write, not the last read", func() {
		ra := NewRollingAverage(10)
		ra.Add(1)
		ra.ObservedAt(time.Now().Add(-3 * gap))
		ra.Touch()
		Expect(ra.WriteAge()).To(BeNumerically("~", 3*gap, time.Minute),
			"a read must not reset the reported age, or the field is a lie in the log line")
	})

	It("does not disturb the stored values when only the clocks move", func() {
		ra := NewRollingAverage(10)
		ra.Add(10)
		ra.Add(20)
		before, n := ra.Average(), ra.Len()

		ra.Touch()
		ra.Observe()
		ra.ObservedAt(time.Now().Add(-gap))
		ra.TouchAt(time.Now().Add(-gap))

		Expect(ra.Average()).To(Equal(before))
		Expect(ra.Len()).To(Equal(n))
	})
})
