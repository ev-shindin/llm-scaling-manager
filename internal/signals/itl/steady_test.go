package itl

import (
	"fmt"
	"math"
	"math/rand/v2"
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

	// A heavier shape at the same fill: k holds at 0.6 while A steps from 0.08
	// to 0.12, so every reading stays steady. The pinned fit prices every
	// multi-replica fleet and should follow on the main window's 20 readings;
	// fitted on the carried steady window (40 readings, 5 minutes) it takes
	// about twice as long. The k-weighted mean takes 16, 8, 4 and 2 cycles
	// here, as least squares did (17, 8, 4, 2). The weighted median took 10,
	// 5, 2 and 1 and the interquartile mean 13, 6, 3 and 1, both by trimming
	// the new level until it held most of the weight, and the median then
	// answered the new A whole on one cycle, the same jump that made it flap
	// between two shapes served side by side.
	DescribeTable("follows a shape change at the same fill on the main window, not the carry",
		func(replicas, cycles int) {
			fleet := func(itl float64) []domain.ReplicaMetrics {
				rows := make([]domain.ReplicaMetrics, replicas)
				for i := range rows {
					rows[i] = row(string(rune('a'+i)), 0.6, itl)
				}
				return rows
			}
			before, after := Model{A: 0.08, B: DefaultBaselineSec}, Model{A: 0.12, B: DefaultBaselineSec}
			var r Reading
			for range 30 {
				r = observe(fleet(before.ITLAt(0.6))...)
			}
			Expect(r.Model.A).To(BeNumerically("~", before.A, 1e-9))
			for c := range 30 {
				r = observe(fleet(after.ITLAt(0.6))...)
				Expect(r.Tier).To(Equal(TierPinnedB))
				if r.Model.A > 0.95*after.A {
					Expect(c).To(Equal(cycles), "%d replicas: cycles to 95%% of the new A", replicas)
					return
				}
			}
			Fail(fmt.Sprintf("%d replicas: A never reached 95%% of %.2f", replicas, after.A))
		},
		Entry("1 replica", 1, 16), Entry("2 replicas", 2, 8),
		Entry("4 replicas", 4, 4), Entry("8 replicas", 8, 2),
	)

	// The round-2 statistics review's probe: a stationary fleet whose
	// replicas serve two shapes at the same fill (a prefix-aware router keeps
	// similar prompts together), half at slope 0.08 and half at 0.12, k
	// jittering by +-0.02 and 1 ms of ITL noise. The weighted median jumped
	// between the two groups as k jitter moved their weight across one half:
	// A ranged 0.081 to 0.119, 36% of its mean, with 15 cycle-to-cycle jumps
	// over 10% in 50 cycles. Least squares held to 2%, with none.
	DescribeTable("prices a fleet of two shapes at their blend, without flapping",
		func(replicas int) {
			rng := rand.New(rand.NewPCG(1, 2))
			lo, hi, sum, n, prev := math.Inf(1), math.Inf(-1), 0.0, 0, 0.0
			for c := range 80 {
				rows := make([]domain.ReplicaMetrics, replicas)
				for i := range rows {
					slope := 0.08
					if i%2 == 1 {
						slope = 0.12
					}
					k := 0.6 + 0.04*(rng.Float64()-0.5)
					rows[i] = row(fmt.Sprintf("p%d", i), k, DefaultBaselineSec+slope*k+0.001*rng.NormFloat64())
				}
				r := observe(rows...)
				if c < 30 {
					continue
				}
				Expect(r.Tier).To(Equal(TierPinnedB))
				if prev > 0 {
					Expect(math.Abs(r.Model.A-prev)).To(BeNumerically("<=", 0.10*prev),
						"cycle %d: A from %.4f to %.4f", c, prev, r.Model.A)
				}
				prev = r.Model.A
				lo, hi, sum, n = math.Min(lo, r.Model.A), math.Max(hi, r.Model.A), sum+r.Model.A, n+1
			}
			mean := sum / float64(n)
			Expect(mean).To(BeNumerically("~", 0.10, 0.005), "the blend of 0.08 and 0.12")
			Expect((hi-lo)/mean).To(BeNumerically("<", 0.10), "A from %.4f to %.4f", lo, hi)
		},
		Entry("2 replicas", 2), Entry("4 replicas", 4),
	)

	// One replica serving a heavier shape among replicas at 0.08, all steady
	// at one fill. The interquartile mean set it aside entirely from four
	// replicas up (0.080 for four), and the weighted median from two.
	DescribeTable("prices one slower replica in, not out, whatever the fleet's size",
		func(replicas int) {
			fleet := make([]domain.ReplicaMetrics, replicas)
			fleet[0] = row("slow", 0.6, DefaultBaselineSec+0.12*0.6)
			for i := 1; i < replicas; i++ {
				fleet[i] = row(fmt.Sprintf("p%d", i), 0.6, DefaultBaselineSec+0.08*0.6)
			}
			var r Reading
			for range 40 {
				r = observe(fleet...)
			}
			// The window holds the newest 20 readings in replica order, so
			// the slow replica's share of them is not exactly 1/replicas.
			var slow, all float64
			for i := range DefaultWindowMaxSize {
				all++
				if (40*replicas-DefaultWindowMaxSize+i)%replicas == 0 {
					slow++
				}
			}
			Expect(r.Tier).To(Equal(TierPinnedB))
			Expect(r.Model.A).To(BeNumerically("~", (0.12*slow+0.08*(all-slow))/all, 1e-9))
		},
		Entry("1 of 3", 3), Entry("1 of 4", 4), Entry("1 of 5", 5), Entry("1 of 8", 8),
	)

	It("fits the pinned line on the readings of replicas past their settle while too few are steady", func() {
		observe(row("p0", 0.40, 0.04))
		rows := make([]domain.ReplicaMetrics, 0, 11)
		rows = append(rows, row("p0", 0.40, 0.04))
		for i := range 10 {
			rows = append(rows, row(string(rune('a'+i)), 0.40, 0.04))
		}
		r := observe(rows...)
		Expect(r.SteadyHeld).To(Equal(2), "the newcomers wait out DefaultSteadyFor")
		Expect(r.Held).To(Equal(12))
		Expect(r.Settling).To(Equal(10))
		Expect(r.Tier).To(BeEmpty(), "two readings of p0 are too few, and there is no A to hold")

		for c := 1; c < int(DefaultSteadyFor/cycle); c++ {
			r = observe(rows...)
			Expect(r.Settling).To(Equal(10))
			Expect(r.Tier).To(BeEmpty(), "cycle %d: the window is the newcomers' but for p0", c)
		}
		r = observe(rows...)
		Expect(r.Settling).To(BeZero(), "the newcomers are past their settle")
		Expect(r.Tier).To(Equal(TierPinnedB))
		Expect(r.Model.A).To(BeNumerically("~", (0.04-DefaultBaselineSec)/0.40, 1e-9))
	})

	// The round-3 statistics review's probe, on the fixture of "prices a
	// saturated fleet from its replicas at work": a scale-up whose joiners'
	// first readings are warm-ups (k 0.46 at 0.5 s, slopes near 1.0 against
	// 0.1). Taking every reading, the interquartile mean set them aside only
	// while they carried less than a quarter of the weight: a fleet of three
	// growing to six read A = 0.127, and mu about 20% low. Set aside by state,
	// they never reach the fit, however many there are, and while the window
	// holds too few readings of replicas past their settle the last A holds.
	DescribeTable("prices a scale-up from the replicas past their settle, however many join",
		func(joiners int) {
			line := Model{A: 0.1, B: DefaultBaselineSec}
			// The replicas already serving swing in k by more than
			// DefaultMaxKDrift from cycle to cycle, as a saturated fleet's do
			// between above the band and its dips, so almost nothing is steady
			// and the pinned fit reads every reading but the settling ones.
			cycle := 0
			fleet := func(extra func(i int, k float64) (float64, float64)) []domain.ReplicaMetrics {
				k := 0.6 + 0.15*float64(cycle%2)
				cycle++
				rows := make([]domain.ReplicaMetrics, 0, 3+joiners)
				for _, pod := range []string{"a", "b", "c"} {
					rows = append(rows, row(pod, k, line.ITLAt(k)))
				}
				for i := range joiners {
					kj, itl := extra(i, k)
					rows = append(rows, row(fmt.Sprintf("j%d", i), kj, itl))
				}
				return rows
			}
			for range 10 {
				observe(fleet(func(int, float64) (float64, float64) { return 0, 0 })[:3]...)
			}
			// Three cycles of warm-ups, then the joiners at work.
			for c := range 16 {
				r := observe(fleet(func(i int, k float64) (float64, float64) {
					if c < 3 {
						kw := 0.46 + 0.01*float64(i)
						return kw, DefaultBaselineSec + 1.0*kw
					}
					return k, line.ITLAt(k)
				})...)
				Expect(r.Tier).To(Equal(TierPinnedB))
				Expect(r.SteadyHeld).To(BeNumerically("<", DefaultMinSamples))
				Expect(r.Model.A).To(BeNumerically("~", line.A, 1e-9), "cycle %d", c)
			}
		},
		Entry("3 join 3", 3), Entry("6 join 3", 6), Entry("12 join 3", 12),
	)

	It("holds the last A while every reading in the window is a settling replica's", func() {
		line := Model{A: 0.1, B: DefaultBaselineSec}
		for c := range 12 {
			k := 0.6 + 0.15*float64(c%2)
			observe(row("a", k, line.ITLAt(k)))
		}
		// "a" leaves; four replicas join and warm up, and fill the window.
		var r Reading
		for c := range 6 {
			rows := make([]domain.ReplicaMetrics, 4)
			for i := range rows {
				rows[i] = row(fmt.Sprintf("j%d", i), 0.5, DefaultBaselineSec+0.9*0.5)
			}
			r = observe(rows...)
			Expect(r.Tier).To(Equal(TierPinnedB), "cycle %d", c)
			Expect(r.Model.A).To(BeNumerically("~", line.A, 1e-9), "cycle %d", c)
		}
		Expect(r.SlopeHeld).To(BeTrue(), "the window holds the joiners' readings alone")
	})

	// A fleet-wide shape alternating every P cycles between slopes 0.08 and
	// 0.12, two replicas at one fill. A period shorter than the window is
	// priced near the blend; the interquartile mean swung +-8% around it at
	// periods 4 and 6, against +-4% here.
	DescribeTable("prices a shape that alternates faster than the window near its blend",
		func(period int) {
			for c := range 200 {
				slope := 0.08
				if (c/period)%2 == 1 {
					slope = 0.12
				}
				r := observe(row("a", 0.6, DefaultBaselineSec+slope*0.6), row("b", 0.6, DefaultBaselineSec+slope*0.6))
				if c >= 60 {
					Expect(r.Model.A).To(BeNumerically("~", 0.10, 0.005), "cycle %d", c)
				}
			}
		},
		Entry("period 4", 4), Entry("period 6", 6),
	)

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

	// The round-2 statistics review's probe. p1 serves at k 0.35; p2 is Ready
	// but idle from the key's first Observe (k 0 and no ITL: nothing running).
	// The load rises: p1 steps to 0.70, and p2's first reading is k 0.55 at
	// 0.45 s, the warm-up reading a two-namespace run logged at 435 to 496 ms.
	// Then both sit on the true line at 0.70. With nothing in p2's trace to
	// judge it by, the warm-up reading was steady, and the full fit through it
	// stored B = 77 ms against a true 12 ms: 31 of 40 cycles were priced more
	// than 10% off at ITL(0.85), the worst +236%.
	It("holds a replica the load comes back to out until it has served DefaultSteadyFor", func() {
		for range 16 {
			observe(row("p1", 0.35, trueLine.ITLAt(0.35)), row("p2", 0, 0))
		}
		r := observe(row("p1", 0.70, trueLine.ITLAt(0.70)), row("p2", 0.55, 0.45))
		Expect(r.Unsteady).To(Equal(2), "p1 stepped, and p2 just started serving")
		full := 0
		for c := range 40 {
			r = observe(row("p1", 0.70, trueLine.ITLAt(0.70)), row("p2", 0.70, trueLine.ITLAt(0.70)))
			if r.Tier == TierOLS {
				full++
				Expect(r.Model.B).To(BeNumerically("~", trueLine.B, 1e-6), "cycle %d: the full fit's B", c)
			}
			// Before the first full fit the pinned fit holds B at the 6 ms
			// default, and through the old level that reads +9% here.
			Expect(r.Model.ITLAt(0.85)).To(BeNumerically("~", trueLine.ITLAt(0.85), 0.10*trueLine.ITLAt(0.85)),
				"cycle %d: %s A=%.4f B=%.4f", c, r.Tier, r.Model.A, r.Model.B)
		}
		Expect(full).To(BeNumerically(">", 0), "the step gives the fleet its spread")
		b, ok := t.Baseline("key")
		Expect(ok).To(BeTrue())
		Expect(b).To(BeNumerically("~", trueLine.B, 1e-6), "remembered B")
	})

	It("holds a lone replica out after it idled, and learns B from the levels it settled at", func() {
		for range 4 {
			observe(row("p", 0, 0))
		}
		Expect(observe(row("p", 0.30, 0.27)).Unsteady).To(Equal(1), "the warm-up reading")
		for range 12 {
			observe(row("p", 0.35, trueLine.ITLAt(0.35)))
		}
		for range 16 {
			observe(row("p", 0.70, trueLine.ITLAt(0.70)))
		}
		b, ok := t.Baseline("key")
		Expect(ok).To(BeTrue())
		Expect(b).To(BeNumerically("~", trueLine.B, 1e-6), "against 0.23 through the warm-up reading")
	})

	It("does not take one scrape that read nothing on a serving replica as idling", func() {
		for range 12 {
			observe(row("p", 0.60, 0.05))
		}
		r := observe(row("p", 0, 0))
		Expect(r.NoITL).To(Equal(1))
		Expect(observe(row("p", 0.60, 0.05)).Unsteady).To(BeZero(), "one gap is a missed scrape")

		By("two in a row are the replica idling")
		observe(row("p", 0, 0))
		observe(row("p", 0, 0))
		Expect(observe(row("p", 0.60, 0.05)).Unsteady).To(Equal(1))
	})

	// The round-3 statistics review's probe: the avg_itl query fails on
	// consecutive cycles while kv_usage_instant is read (the collector drops
	// a failed query's values whole). The replicas never stopped serving --
	// their k says so -- and counted idle on the missing ITL alone, all four
	// restarted as joiners: 32 of the next 48 readings unsteady, and 8 cycles
	// with the whole fleet held out.
	DescribeTable("does not take an ITL the collector did not read on a busy replica as idling",
		func(gap int) {
			fleet := func(itl float64) []domain.ReplicaMetrics {
				rows := make([]domain.ReplicaMetrics, 4)
				for i := range rows {
					rows[i] = row(fmt.Sprintf("p%d", i), 0.6, itl)
				}
				return rows
			}
			for range 20 {
				observe(fleet(trueLine.ITLAt(0.6))...)
			}
			for range gap {
				Expect(observe(fleet(0)...).NoITL).To(Equal(4))
			}
			for c := range 12 {
				r := observe(fleet(trueLine.ITLAt(0.6))...)
				Expect(r.Unsteady).To(BeZero(), "cycle %d after a gap of %d", c, gap)
			}
		},
		Entry("two cycles", 2), Entry("three cycles", 3),
	)

	It("forgets a stale key's steady readings, traces and held slope with its window", func() {
		for range DefaultMinSamples {
			observe(row("p", 0.5, 0.06))
		}
		Expect(t.slopes).To(HaveKey("key"))
		t.Sweep(now, func(string) bool { return true })
		Expect(t.steady).To(BeEmpty())
		Expect(t.traces).To(BeEmpty())
		Expect(t.slopes).To(BeEmpty())
	})
})
