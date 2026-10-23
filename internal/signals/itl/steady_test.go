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
	//
	// The first replica did settle at 0.65 before the split, and both settle
	// at 0.33 after it, so the steady readings span two real levels: a full
	// fit through them alone is the true line.
	It("learns the intercept from the levels either side of a transient, not from the transient", func() {
		for range 8 {
			observe(row("p1", 0.65, trueLine.ITLAt(0.65)))
		}
		for j := range 4 {
			f := float64(j+1) / 4
			lagging := trueLine.ITLAt(0.65)*(1-f) + trueLine.ITLAt(0.33)*f
			r := observe(row("p1", 0.33, lagging), row("p2", 0.33, 0.06))
			Expect(r.Unsteady).To(Equal(2), "both replicas' load has just moved")
			Expect(r.Tier).NotTo(Equal(TierOLS))
		}
		for j := range 12 {
			r := observe(row("p1", 0.33, trueLine.ITLAt(0.33)), row("p2", 0.33, trueLine.ITLAt(0.33)))
			if r.Tier == TierOLS {
				Expect(r.Model.B).To(BeNumerically("~", trueLine.B, 1e-9),
					"cycle %d: a full fit through pairs whose halves describe different moments", j)
			}
		}
		b, ok := t.Baseline("key")
		Expect(ok).To(BeTrue())
		Expect(b).To(BeNumerically("~", trueLine.B, 1e-9), "remembered B, against the transient's 0.024")
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

	// A balanced fleet through one load step: k 0.35 for ten minutes, then
	// 0.70 for ten, with the one-minute ITL a minute behind k. The main window
	// holds 20 readings, so from two replicas on the step's unsteady readings
	// fill it before the new level is steady; the old level's steady readings
	// have to outlive it until then, or the fleet never learns B (stats review
	// probe: 0 full fits for 2-8 replicas, against B = 12 ms learned before the
	// steady gate).
	DescribeTable("learns B from a load step, whatever the fleet's size",
		func(replicas int) {
			line := Model{A: 0.020, B: 0.012}
			fleet := func(k, itl float64) []domain.ReplicaMetrics {
				rows := make([]domain.ReplicaMetrics, replicas)
				for i := range rows {
					rows[i] = row(string(rune('a'+i)), k, itl)
				}
				return rows
			}
			full := 0
			for c := range 80 {
				k, itl := 0.35, line.ITLAt(0.35)
				if c >= 40 {
					f := min(float64(c-40+1)/4, 1)
					k, itl = 0.70, line.ITLAt(0.35)*(1-f)+line.ITLAt(0.70)*f
				}
				if observe(fleet(k, itl)...).Tier == TierOLS {
					full++
				}
			}
			b, ok := t.Baseline("key")
			Expect(ok).To(BeTrue(), "%d replicas: no full fit in %d cycles", replicas, 80)
			Expect(b).To(BeNumerically("~", line.B, 1e-6))
			GinkgoWriter.Printf("replicas=%d fullFits=%d B=%.6f\n", replicas, full, b)
		},
		Entry("1 replica", 1), Entry("2 replicas", 2), Entry("3 replicas", 3),
		Entry("4 replicas", 4), Entry("5 replicas", 5), Entry("6 replicas", 6),
		Entry("7 replicas", 7), Entry("8 replicas", 8), Entry("16 replicas", 16),
	)

	// The spread is there only while the load moves: a two-replica fleet at
	// k 0.33 takes a one-minute burst to 0.65 and comes back, its ITL a
	// minute behind k both ways. Before the steady gate the full fit ran
	// across the burst and kept its intercept.
	It("learns no intercept from a burst it never settled at", func() {
		lagging := func(from, to float64, j int) float64 {
			f := min(float64(j+1)/4, 1)
			return trueLine.ITLAt(from)*(1-f) + trueLine.ITLAt(to)*f
		}
		for range 20 {
			observe(row("p1", 0.33, trueLine.ITLAt(0.33)), row("p2", 0.33, trueLine.ITLAt(0.33)))
		}
		for j := range 4 {
			itl := lagging(0.33, 0.65, j)
			observe(row("p1", 0.65, itl), row("p2", 0.65, itl))
		}
		for j := range 20 {
			itl := lagging(0.65, 0.33, j)
			r := observe(row("p1", 0.33, itl), row("p2", 0.33, itl))
			Expect(r.Tier).NotTo(Equal(TierOLS), "cycle %d after the burst", j)
		}
		b, ok := t.Baseline("key")
		Expect(ok).To(BeFalse(), "remembered B = %v, learned from the burst", b)
	})

	// The adversarial review's probe: a balanced two-replica fleet steady at
	// k 0.30, then the load steps to 0.55 while the one-minute ITL still
	// averages the prefill burst of the step, twice the line. A balanced fleet
	// is priced from the pinned fit, and taking those readings it walked A to
	// 1.95x the true slope within six cycles (the rerun: A 0.077 -> 0.207, mu
	// 6.4 -> 2.65 req/s for two and a half minutes).
	It("keeps the pinned fit on the steady readings through a load step", func() {
		line := Model{A: 0.08, B: DefaultBaselineSec}
		var r Reading
		for range 12 {
			r = observe(row("p1", 0.30, line.ITLAt(0.30)), row("p2", 0.30, line.ITLAt(0.30)))
		}
		Expect(r.Tier).To(Equal(TierPinnedB))
		Expect(r.Model.A).To(BeNumerically("~", line.A, 1e-9))
		for j := range 6 {
			r = observe(row("p1", 0.55, 2*line.ITLAt(0.55)), row("p2", 0.55, 2*line.ITLAt(0.55)))
			Expect(r.Unsteady).To(Equal(2), "cycle %d: both replicas just moved", j)
			Expect(r.Tier).To(Equal(TierPinnedB))
			Expect(r.Model.A).To(BeNumerically("~", line.A, 0.02*line.A),
				"cycle %d after the step: A from the transient", j)
		}
	})

	It("fits the pinned line on every reading while too few are steady", func() {
		observe(row("p0", 0.40, 0.04))
		rows := make([]domain.ReplicaMetrics, 0, 11)
		rows = append(rows, row("p0", 0.40, 0.04))
		for i := range 10 {
			rows = append(rows, row(string(rune('a'+i)), 0.40, 0.04))
		}
		r := observe(rows...)
		Expect(r.SteadyHeld).To(Equal(2), "the newcomers wait out DefaultSteadyFor")
		Expect(r.Held).To(Equal(12))
		Expect(r.Tier).To(Equal(TierPinnedB), "a fleet that has not settled still gets a line")
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

	It("does not take a k the collector did not read as a step", func() {
		for range 12 {
			observe(row("p", 0.60, 0.05))
		}
		// One scrape without a k: the zero value, which Observe counts as NoK.
		r := observe(row("p", 0, 0.05))
		Expect(r.NoK).To(Equal(1))
		r = observe(row("p", 0.60, 0.05))
		Expect(r.Unsteady).To(BeZero(), "the replica's load never moved")
		Expect(r.SteadyHeld).To(Equal(13))
	})

	It("forgets a stale key's steady readings and traces with its window", func() {
		observe(row("p", 0.5, 0.06))
		t.Sweep(now, func(string) bool { return true })
		Expect(t.steady).To(BeEmpty())
		Expect(t.traces).To(BeEmpty())
	})
})
