package itl

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// GrowMaxSize exists because one window can be fed by several variants once
// windows are keyed by what ITL is a property of rather than by a variant
// name. Add evicts the oldest observation at capacity whoever contributed it,
// so without growing, N contributors hold DefaultWindowMaxSize/N cycles each
// and maxAge stops being what bounds the window.
var _ = Describe("Window.GrowMaxSize", func() {
	var (
		window *Window
		now    time.Time
	)

	BeforeEach(func() {
		now = time.Now()
		window = NewWindow(4, DefaultObservationMaxAge, DefaultMinSamples,
			DefaultMinKSpread, DefaultMinObservableK, DefaultMaxObservableK)
	})

	It("raises the capacity and reports the change", func() {
		Expect(window.MaxSize()).To(Equal(4))
		Expect(window.GrowMaxSize(8)).To(BeTrue())
		Expect(window.MaxSize()).To(Equal(8))
	})

	It("never lowers the capacity", func() {
		// A contributor going quiet must not discard the history of the ones
		// still reporting; the window ages by maxAge instead.
		Expect(window.GrowMaxSize(8)).To(BeTrue())
		Expect(window.GrowMaxSize(2)).To(BeFalse())
		Expect(window.MaxSize()).To(Equal(8),
			"shrinking would throw away observations that are still in age")
	})

	It("is a no-op at the same capacity", func() {
		Expect(window.GrowMaxSize(4)).To(BeFalse())
		Expect(window.MaxSize()).To(Equal(4))
	})

	It("keeps observations that the old capacity would have evicted", func() {
		// The behaviour the pooling depends on: at capacity 4 the window holds
		// the last four readings, and at 8 it holds eight.
		for i := 0; i < 6; i++ {
			k := 0.2 + float64(i)*0.05
			Expect(window.Add(k, 0.01, now.Add(time.Duration(i)*time.Second))).To(BeFalse())
		}
		Expect(window.Len()).To(Equal(4), "capacity 4 evicted the two oldest")

		window.GrowMaxSize(8)
		for i := 6; i < 10; i++ {
			k := 0.2 + float64(i)*0.05
			Expect(window.Add(k, 0.01, now.Add(time.Duration(i)*time.Second))).To(BeFalse())
		}
		Expect(window.Len()).To(Equal(8),
			"after growing, the window retains eight rather than four")
	})

	It("does not resurrect observations already evicted", func() {
		// Growing is not retroactive: what capacity 4 dropped is gone, which
		// is why the owner grows the window when it first sees a second
		// contributor rather than after a fit fails.
		for i := 0; i < 6; i++ {
			window.Add(0.2+float64(i)*0.05, 0.01, now)
		}
		Expect(window.Len()).To(Equal(4))
		window.GrowMaxSize(20)
		Expect(window.Len()).To(Equal(4))
	})
})
