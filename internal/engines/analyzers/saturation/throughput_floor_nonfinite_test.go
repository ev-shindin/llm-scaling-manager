package saturation

import (
	"math"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The window holds ten readings. A value that is not a rate must not take one of
// those slots from a real reading, and must not be logged as a saturated rate
// per replica per cycle -- which is the highest-volume log line in the
// controller.
//
// `!(rate > 0)` already rejected a NaN, because every comparison against NaN is
// false. It did NOT reject +Inf, which passes `rate > 0`, so the guard did not
// match the one the floor applies when it finally consumes the value.
var _ = Describe("recordSaturatedThroughput with a non-finite rate", func() {
	const key = "m|H200|1|decode|long|q5"

	newAnalyzer := func() (*SaturationAnalyzer, *time.Time) {
		a := NewSaturationAnalyzer(capacity.NewStore())
		now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
		a.now = func() time.Time { return now }
		return a, &now
	}

	for _, tc := range []struct {
		name string
		rate float64
	}{
		{"a NaN rate", math.NaN()},
		{"an infinite rate", math.Inf(1)},
		{"a negatively infinite rate", math.Inf(-1)},
		{"a zero rate", 0},
		{"a negative rate", -1},
	} {
		It(tc.name+" never enters the window", func() {
			a, _ := newAnalyzer()
			a.recordSaturatedThroughput(key, tc.rate)
			_, present := a.saturatedThroughput[tkey(a, key)]
			Expect(present).To(BeFalse(), "no window is even created for it")
		})
	}

	It("does not displace a real reading", func() {
		a, now := newAnalyzer()
		a.recordSaturatedThroughput(key, 5.4)
		*now = now.Add(ThroughputSampleSpacing)
		a.recordSaturatedThroughput(key, math.Inf(1))
		*now = now.Add(ThroughputSampleSpacing)
		a.recordSaturatedThroughput(key, math.NaN())

		Expect(a.saturatedThroughput[tkey(a, key)].Len()).To(Equal(1),
			"one real reading in, and neither non-finite value took a slot")
		Expect(a.saturatedThroughput[tkey(a, key)].Average()).To(BeNumerically("~", 5.4, 1e-9),
			"and the reading that is there is undisturbed")
	})

	It("still records an ordinary rate", func() {
		// The guard excludes non-finite values, not small or large finite ones.
		a, now := newAnalyzer()
		for _, r := range []float64{0.001, 5.4, 1e9} {
			a.recordSaturatedThroughput(key, r)
			*now = now.Add(ThroughputSampleSpacing)
		}
		Expect(a.saturatedThroughput[tkey(a, key)].Len()).To(Equal(3))
	})
})

// tkey resolves a throughput-window key written in the OLD shape
// (model|accel|gpus|role|outBucket|qN) to the real one, which carries an input
// bucket between the role and the output bucket. The specs name the parts they
// care about; the input bucket is a fixture detail they do not.
func tkey(a *SaturationAnalyzer, written string) string {
	wPrefix, wBucket, wSuffix, ok := splitHistoryKey(written)
	if !ok {
		return written
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.saturatedThroughput {
		p, b, sfx, ok := splitHistoryKey(k)
		if ok && b == wBucket && sfx == wSuffix && strings.HasPrefix(p, wPrefix) {
			return k
		}
	}
	return written
}
