package itl

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("Tracker steady readings", func() {
	const cycle = 15 * time.Second
	var (
		t   *Tracker
		now time.Time
	)
	BeforeEach(func() {
		t = NewTracker()
		now = time.Date(2026, 10, 23, 7, 44, 0, 0, time.UTC)
	})
	row := func(pod string, k, itl float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, VariantName: "v", Ready: true, KvUsageInstant: k, AvgITL: itl}
	}
	// The line the engine really follows.
	trueLine := Model{A: 0.1, B: 0.012}
	observe := func(rows ...domain.ReplicaMetrics) Reading {
		r := t.Observe("key", "v", rows, now)
		now = now.Add(cycle)
		return r
	}

	// The shape of issue #270, cycle by cycle: one replica steady at k=0.65,
	// then a second joins and the load splits to k=0.33 each. For a minute the
	// first replica's ITL still averages the busier minute before, and the
	// newcomer's covers its own warm-up; then both settle on the line.
	//
	// Without the steady gate the window's spread comes from that transient,
	// the full fit walks its intercept down as the old readings leave, and
	// whatever it reads on the last cycle the spread lasts is remembered and
	// pinned into every fit after: 0.024 here against a true 0.012, the same
	// walk the run logged from 0.047 down to the 0.0045 it kept.
	It("learns no intercept from a transient", func() {
		for range 8 {
			observe(row("p1", 0.65, trueLine.ITLAt(0.65)))
		}
		for j := range 4 {
			f := float64(j+1) / 4
			lagging := trueLine.ITLAt(0.65)*(1-f) + trueLine.ITLAt(0.33)*f
			r := observe(row("p1", 0.33, lagging), row("p2", 0.33, 0.06))
			Expect(r.Unsteady).To(Equal(2), "both replicas' load has just moved")
		}
		for range 12 {
			r := observe(row("p1", 0.33, trueLine.ITLAt(0.33)), row("p2", 0.33, trueLine.ITLAt(0.33)))
			Expect(r.Tier).NotTo(Equal(TierOLS),
				"a full fit through pairs whose halves describe different moments")
		}
		b, ok := t.Baseline("key")
		Expect(ok).To(BeFalse(), "remembered B = %v, learned from the transient", b)
	})

	It("learns the true intercept from two levels the replica settled at", func() {
		for range 6 {
			observe(row("p", 0.20, trueLine.ITLAt(0.20)))
		}
		// The step: k moves at once, the ITL average follows a minute later.
		for j := range int(DefaultSteadyFor / cycle) {
			f := min(float64(j+1)/4, 1)
			observe(row("p", 0.65, trueLine.ITLAt(0.20)*(1-f)+trueLine.ITLAt(0.65)*f))
		}
		var last Reading
		for range 6 {
			last = observe(row("p", 0.65, trueLine.ITLAt(0.65)))
		}
		Expect(last.Tier).To(Equal(TierOLS))
		Expect(last.SteadyHeld).To(Equal(12), "six readings at each level; the step's eight are held out")
		b, ok := t.Baseline("key")
		Expect(ok).To(BeTrue())
		Expect(b).To(BeNumerically("~", trueLine.B, 1e-9))
	})

	It("takes the replicas it first sees as steady, so a restart does not delay the first full fit", func() {
		rows := make([]domain.ReplicaMetrics, 10)
		for i := range rows {
			k := 0.2 + 0.5*float64(i)/9
			rows[i] = row("p", k, trueLine.ITLAt(k))
		}
		r := observe(rows...)
		Expect(r.Unsteady).To(BeZero(), "rows of one Observe are not judged against each other")
		Expect(r.Tier).To(Equal(TierOLS))
	})

	It("holds a replica that joins out of the full fit until it has served DefaultSteadyFor", func() {
		observe(row("p1", 0.5, trueLine.ITLAt(0.5)))
		steadyAfter := int(DefaultSteadyFor / cycle)
		for i := range steadyAfter {
			r := observe(row("p1", 0.5, trueLine.ITLAt(0.5)), row("p2", 0.5, trueLine.ITLAt(0.5)))
			Expect(r.Unsteady).To(Equal(1), "cycle %d: the newcomer, and only it", i)
		}
		r := observe(row("p1", 0.5, trueLine.ITLAt(0.5)), row("p2", 0.5, trueLine.ITLAt(0.5)))
		Expect(r.Unsteady).To(BeZero())
		Expect(r.Held - r.SteadyHeld).To(Equal(steadyAfter))
	})

	It("holds a replica whose load moved out until it has held still for DefaultSteadyFor", func() {
		observe(row("p", 0.30, trueLine.ITLAt(0.30)))
		Expect(observe(row("p", 0.30+DefaultMaxKDrift/2, trueLine.ITLAt(0.30))).Unsteady).
			To(BeZero(), "drift inside DefaultMaxKDrift is still one load level")

		unsteady := 0
		for {
			r := observe(row("p", 0.60, trueLine.ITLAt(0.60)))
			if r.Unsteady == 0 {
				break
			}
			unsteady++
			Expect(unsteady).To(BeNumerically("<=", int(DefaultSteadyFor/cycle)+1))
		}
		Expect(unsteady).To(Equal(int(DefaultSteadyFor/cycle)),
			"held out until the step ages out of the trace, and no longer")
	})

	It("starts a replica over when it misses a cycle, and when it is not Ready", func() {
		observe(row("p1", 0.5, 0.06), row("p2", 0.5, 0.06))
		observe(row("p1", 0.5, 0.06))
		Expect(observe(row("p1", 0.5, 0.06), row("p2", 0.5, 0.06)).Unsteady).To(Equal(1),
			"p2 was gone for a cycle and is new again")

		notReady := row("p1", 0.5, 0.06)
		notReady.Ready = false
		observe(notReady, row("p2", 0.5, 0.06))
		Expect(observe(row("p1", 0.5, 0.06)).Unsteady).To(Equal(1))
	})

	It("forgets a stale key's steady readings and traces with its window", func() {
		observe(row("p", 0.5, 0.06))
		t.Sweep(now, func(string) bool { return true })
		Expect(t.steady).To(BeEmpty())
		Expect(t.traces).To(BeEmpty())
	})
})
