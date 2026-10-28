package capacity

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Rolling Average", func() {

	Describe("Add and Average", func() {
		It("should compute the mean of added values", func() {
			ra := NewRollingAverage(5, time.Now())
			ra.AddAt(10, time.Now())
			ra.AddAt(20, time.Now())
			ra.AddAt(30, time.Now())

			Expect(ra.Average()).To(Equal(20.0))
			Expect(ra.Len()).To(Equal(3))
		})
	})

	Describe("Window eviction", func() {
		It("should evict oldest values when window is full", func() {
			ra := NewRollingAverage(3, time.Now())
			ra.AddAt(1, time.Now())
			ra.AddAt(2, time.Now())
			ra.AddAt(3, time.Now())
			ra.AddAt(4, time.Now()) // evicts 1
			ra.AddAt(5, time.Now()) // evicts 2

			Expect(ra.Average()).To(Equal(4.0)) // (3+4+5)/3
			Expect(ra.Len()).To(Equal(3))
		})

		It("raises the last value, never a lower one, and reports it", func() {
			ra := NewRollingAverage(3, time.Now())
			Expect(ra.Last()).To(Equal(0.0), "empty")
			ra.AddAt(1.5, time.Now())
			ra.RaiseLastAt(1.2, time.Now())
			Expect(ra.Last()).To(Equal(1.5), "a lower reading leaves it")
			ra.RaiseLastAt(1.8, time.Now())
			Expect(ra.Last()).To(Equal(1.8))
			Expect(ra.Len()).To(Equal(1), "raising adds nothing")
			ra.AddAt(2, time.Now())
			Expect(ra.Last()).To(Equal(2.0))
			Expect(ra.Max()).To(Equal(2.0))
		})
	})

	Describe("Empty average", func() {
		It("should return 0 for an empty rolling average", func() {
			ra := NewRollingAverage(5, time.Now())
			Expect(ra.Average()).To(Equal(0.0))
			Expect(ra.Len()).To(Equal(0))
		})
	})

	Describe("Single value", func() {
		It("should return the value itself as the average", func() {
			ra := NewRollingAverage(5, time.Now())
			ra.AddAt(42, time.Now())

			Expect(ra.Average()).To(Equal(42.0))
			Expect(ra.Len()).To(Equal(1))
		})
	})

	Describe("Large window with overflow", func() {
		It("should retain only the last maxSize values", func() {
			ra := NewRollingAverage(RollingAverageWindowSize, time.Now())
			for i := 1; i <= 15; i++ {
				ra.AddAt(float64(i), time.Now())
			}

			Expect(ra.Len()).To(Equal(RollingAverageWindowSize))
			// Window contains 6..15, average = 10.5
			Expect(ra.Average()).To(BeNumerically("~", 10.5, 0.001))
		})
	})
})

var _ = Describe("RollingAverage.Median", func() {
	It("takes the middle value, and the lower of two on an even count", func() {
		r := NewRollingAverage(10, time.Now())
		for _, v := range []float64{5.4, 3.3, 3.5} {
			r.AddAt(v, time.Now())
		}
		Expect(r.Median()).To(Equal(3.5))
		Expect(r.Max()).To(Equal(5.4), "the read this replaced")

		r2 := NewRollingAverage(10, time.Now())
		r2.AddAt(4.0, time.Now())
		r2.AddAt(4.01, time.Now())
		Expect(r2.Median()).To(Equal(4.0), "the lower of the two middle values")
	})

	It("is not moved by a single burst, which is what Max was", func() {
		// The generation-token rate this window holds bursts rather than
		// under-reads: measured 2026-09-22, a per-replica rate of 4028 min,
		// 7548 median, 11663 max. Read with Max the window ratcheted 0.92 ->
		// 3.60 req/s and the demand floor asked for 1.6 replicas where about
		// eight were needed.
		r := NewRollingAverage(10, time.Now())
		for _, v := range []float64{0.90, 0.92, 0.88, 0.91, 3.60} {
			r.AddAt(v, time.Now())
		}
		Expect(r.Median()).To(BeNumerically("~", 0.91, 0.001))
		Expect(r.Max()).To(Equal(3.60))
	})

	It("is zero on an empty window", func() {
		Expect(NewRollingAverage(10, time.Now()).Median()).To(BeZero())
	})
})
