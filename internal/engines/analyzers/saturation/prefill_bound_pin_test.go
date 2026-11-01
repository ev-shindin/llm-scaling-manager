package saturation

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The prefill bound (inventory row 7) writes through mu's fold
// (recordSaturatedThroughputFrom). Learner PR 3 changes that fold for mu
// (an exact repeat is use, not a write), and the bound is not mu: it moves in
// its own PR, after a double-scrape fixture (analyzer-learner.md, section 7).
// So two prefill pods in one spacing pin what the bound does before the fold
// is touched: the minute's sample is the pods' mean, the bound is the max over
// minutes, and an exact repeat of the last reading past the spacing is still a
// write of the bound's window, as it was.
var _ = Describe("the prefill bound with two pods in one spacing", func() {
	const (
		key    = "ns|prefill-v|m|H200|1|prefill|ihuge|noout|q5"
		prompt = 6000.0
	)
	var (
		analyzer *SaturationAnalyzer
		clock    time.Time
		cfg      *config.ScalingPolicy
	)
	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(capacity.NewStore())
		clock = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
		analyzer.now = func() time.Time { return clock }
		cfg = &config.ScalingPolicy{EnablePrefillThroughputBound: true, QueueLengthThreshold: 5}
	})
	// read reads pod saturated at computed prompt tokens/s, then moves the
	// clock a second, and returns the bound in prompt tokens/s.
	read := func(pod string, computed float64) float64 {
		rm := makeReplicaMetrics(pod, "prefill-v", 300_000, 1_149_312, 30, prompt, 1)
		rm.PrefillComputedTokenRate = computed
		got := analyzer.notePrefillBound(rm, cfg, domain.RolePrefill, key, "", prompt, admission{}, GinkgoLogr)
		clock = clock.Add(time.Second)
		return got.rate * prompt
	}
	window := func() *capacity.RollingAverage {
		analyzer.mu.Lock()
		defer analyzer.mu.Unlock()
		return analyzer.saturatedThroughput[prefillBoundKey(key)]
	}

	It("samples the two pods' mean in one minute, and keeps the max over minutes", func() {
		read("prefill-0", 6000)
		Expect(read("prefill-1", 4000)).To(BeNumerically("~", 5000, 1e-6))
		clock = clock.Add(ThroughputSampleSpacing)
		read("prefill-0", 3000)
		Expect(read("prefill-1", 3200)).To(BeNumerically("~", 5000, 1e-6),
			"a lower minute does not displace the bound's max")
		Expect(window().Len()).To(Equal(2))
	})
	for _, on := range []bool{true, false} {
		It(fmt.Sprintf("takes an exact repeat of the last reading past the spacing as a write of its window (%s %v)", SwitchMuRepeatIsUse, on), func() {
			Expect(analyzer.SetSwitchesForReplay(map[string]bool{SwitchMuRepeatIsUse: on})).To(Succeed())
			read("prefill-0", 6000)
			read("prefill-1", 4000)
			clock = clock.Add(ThroughputSampleSpacing)
			at := clock
			Expect(read("prefill-1", 4000)).To(BeNumerically("~", 5000, 1e-6))
			ra := window()
			Expect(ra.Len()).To(Equal(1), "a repeat starts no sample")
			Expect(ra.LastWritten()).To(Equal(at), "a repeat keeps the bound's episode alive, as it did")
		})
	}
})
