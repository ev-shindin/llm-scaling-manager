package allocation

import (
	"math"
	"math/rand"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The scenarios of hack/utilization-share-oscillation-sim.py, run against the
// real ledger and planner (proposal §6.7, §12). The cluster model is the
// simulator's: a 30 s cycle; a transfer's donor replica leaves RELEASE after
// it starts; the receiver's pods are scheduled the cycle after the release and
// serve FILL later; demand is seen LAG late, averaged over LAG.

const (
	simCycle   = 30 * time.Second
	simRelease = 360 * time.Second
	simFill    = 180 * time.Second
	simLag     = 60 * time.Second
	simK       = 0.8
)

func simTimings() ShareTimings {
	latency := 2*simLag + ShareConfirmCycles*simCycle + simRelease + simFill
	return ShareTimings{
		Window:         300 * time.Second,
		ReleaseTimeout: simRelease + 4*simCycle,
		FillTimeout:    4 * simCycle,
		ReversalHold:   2 * simRelease,
		SwingWindow:    8 * latency,
	}
}

type simRole struct {
	key     string
	weight  float64
	gpus    int
	floor   int
	initial int
	demand  func(t time.Duration, rnd *rand.Rand) float64 // GPU-equivalents at 100 %
}

type simResult struct {
	started, cancelled, wasted int
	shortfall                  float64
	final                      map[string]int
}

func runShareSim(roles []simRole, budget int, seed int64, tm ShareTimings) simResult {
	const dur = 6 * time.Hour
	rnd := rand.New(rand.NewSource(seed))
	l := NewShareLedger()
	t0 := time.Unix(0, 0)
	held, serving := map[string]int{}, map[string]int{}
	for _, r := range roles {
		held[r.key], serving[r.key] = r.initial, r.initial
	}
	type pending struct {
		at   time.Duration
		role string
		g    int
	}
	var arriving []pending
	released := map[string]bool{}
	granted := map[string]bool{}
	type landing struct {
		at           time.Duration
		donor, recvr string
	}
	var landed []landing
	samples := map[string][]float64{}
	var res simResult
	var short, total float64

	for t := time.Duration(0); t < dur; t += simCycle {
		now := t0.Add(t)
		// The cluster: donors release RELEASE after start; receivers are
		// scheduled the cycle after they are raised, and serve FILL later.
		for _, tr := range l.Transfers() {
			switch {
			case tr.State == ShareReleasing && !released[tr.ID] && now.Sub(tr.Started) >= simRelease:
				held[tr.Donor] -= tr.DonorGPUs
				serving[tr.Donor] -= tr.DonorGPUs
				released[tr.ID] = true
			case tr.State == ShareFilling && !granted[tr.ID]:
				held[tr.Receiver] += tr.GPUs
				arriving = append(arriving, pending{t + simFill, tr.Receiver, tr.GPUs})
				granted[tr.ID] = true
			}
		}
		keep := arriving[:0]
		for _, p := range arriving {
			if p.at <= t {
				serving[p.role] += p.g
			} else {
				keep = append(keep, p)
			}
		}
		arriving = keep
		for _, e := range l.Observe(held, now, tm) {
			if e.Outcome == ShareOutcomeDone {
				landed = append(landed, landing{t, e.Transfer.Donor, e.Transfer.Receiver})
			}
		}

		in := SharePlanInput{Held: held, Thresholds: map[string]float64{}, Budget: budget, Tolerance: 0.15}
		for _, r := range roles {
			d := r.demand(t, rnd)
			inst := d / simK
			short += math.Max(0, inst-float64(serving[r.key]))
			total += inst
			samples[r.key] = append(samples[r.key], inst)
			// lagged view: the mean of the samples from 2·LAG to LAG ago
			lag := int(simLag / simCycle)
			s := samples[r.key]
			lo, hi := max(0, len(s)-1-2*lag), max(0, len(s)-1-lag)
			sum := 0.0
			for _, v := range s[lo : hi+1] {
				sum += v
			}
			in.Roles = append(in.Roles, ShareRole{
				Key: r.key, Weight: r.weight, Need: sum / float64(hi-lo+1),
				Floor: r.floor, ReplicaGPUs: r.gpus,
			})
			in.Thresholds[r.key] = simK
		}
		plan := PlanShareTransfers(l, in, now, tm)
		res.started += len(plan.Started)
		res.cancelled += len(plan.Cancelled)
	}
	for i, a := range landed {
		for _, b := range landed[i+1:] {
			if b.at-a.at <= simRelease && (b.recvr == a.donor || b.donor == a.recvr) {
				res.wasted++
				break
			}
		}
	}
	res.shortfall = short / total
	res.final = held
	return res
}

func steady(level float64) func(time.Duration, *rand.Rand) float64 {
	return func(_ time.Duration, rnd *rand.Rand) float64 { return level * (1 + rnd.Float64()*0.2 - 0.1) }
}

func stepAt(before, after float64, at time.Duration) func(time.Duration, *rand.Rand) float64 {
	return func(t time.Duration, rnd *rand.Rand) float64 {
		v := before
		if t >= at {
			v = after
		}
		return v * (1 + rnd.Float64()*0.1 - 0.05)
	}
}

// swing is demand swinging ±40 % around 3.5 GPU-equivalents, the simulator's
// swap scenario.
func swing(period time.Duration, phase float64) func(time.Duration, *rand.Rand) float64 {
	const mean, amp = 3.5, 0.4
	return func(t time.Duration, rnd *rand.Rand) float64 {
		v := mean * (1 + amp*math.Sin(2*math.Pi*t.Seconds()/period.Seconds()+phase))
		return v * (1 + rnd.Float64()*0.1 - 0.05)
	}
}

func threeSim(a, b, c func(time.Duration, *rand.Rand) float64, initial ...int) []simRole {
	return []simRole{
		{key: "A", weight: 2, gpus: 1, floor: 1, initial: initial[0], demand: a},
		{key: "B", weight: 1, gpus: 1, floor: 1, initial: initial[1], demand: b},
		{key: "C", weight: 1, gpus: 1, floor: 1, initial: initial[2], demand: c},
	}
}

var _ = Describe("PlanShareTransfers against the §6.7 scenarios", func() {
	seeds := []int64{1, 2, 3, 4, 5}

	It("plans no transfer at all under steady, noisy load", func() {
		for _, seed := range seeds {
			r := runShareSim(threeSim(steady(4), steady(3), steady(1), 9, 5, 2), 16, seed, simTimings())
			Expect(r.started).To(BeZero(), "seed %d", seed)
		}
	})

	It("plans no transfer when two models are tied", func() {
		for _, seed := range seeds {
			r := runShareSim(threeSim(steady(3.2), steady(3.2), steady(1), 8, 6, 2), 16, seed, simTimings())
			Expect(r.started).To(BeZero(), "seed %d", seed)
		}
	})

	It("follows a demand step in the minimum number of moves, none wasted (§5.7)", func() {
		for _, seed := range seeds {
			r := runShareSim(threeSim(steady(4), stepAt(3, 6, time.Hour), steady(1), 9, 5, 2), 16, seed, simTimings())
			Expect(r.started).To(Equal(3), "seed %d", seed)
			Expect(r.cancelled).To(BeZero(), "seed %d", seed)
			Expect(r.wasted).To(BeZero(), "seed %d", seed)
			Expect(r.final).To(Equal(map[string]int{"A": 6, "B": 8, "C": 2}), "seed %d", seed)
		}
	})

	It("wastes no transfer under load swinging at any period", func() {
		for _, period := range []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 3 * time.Hour} {
			for _, seed := range seeds {
				r := runShareSim(threeSim(swing(period, 0), swing(period, math.Pi), steady(1), 6, 5, 1),
					12, seed, simTimings())
				Expect(r.wasted).To(BeZero(), "period %v seed %d", period, seed)
			}
		}
	})

	// The resonance of §6.7: without the swing rule a 30-minute swing ran at 2.5x
	// the shortfall of standing still. The rules must keep it well below that.
	It("keeps the 30-minute resonance bounded", func() {
		var with, still float64
		for _, seed := range seeds {
			roles := threeSim(swing(30*time.Minute, 0), swing(30*time.Minute, math.Pi), steady(1), 6, 5, 1)
			with += runShareSim(roles, 12, seed, simTimings()).shortfall
			frozen := simTimings()
			frozen.ReleaseTimeout = 0 // every transfer aborts at once: a fleet that never moves
			still += runShareSim(roles, 12, seed, frozen).shortfall
		}
		Expect(with).To(BeNumerically("<", 2.5*still))
	})

	// Negative control for the reversal hold (§6.7 rule 4): without it the
	// planner reverses transfers under fast swings.
	It("wastes transfers without the reversal hold (negative control)", func() {
		noHold := simTimings()
		noHold.ReversalHold = 0
		wasted := 0
		for _, seed := range seeds {
			r := runShareSim(threeSim(swing(15*time.Minute, 0), swing(15*time.Minute, math.Pi), steady(1), 6, 5, 1),
				12, seed, noHold)
			wasted += r.wasted
		}
		Expect(wasted).To(BeNumerically(">", 0))
	})
})

var _ = Describe("Pod shape in transfers (§6.5)", func() {
	DescribeTable("ShareCovers",
		func(donor, receiver ShareVariant, want bool) {
			Expect(ShareCovers(donor, receiver)).To(Equal(want))
		},
		Entry("an 8-GPU pod funds a 4-GPU pod", ShareVariant{GPUs: 8, PodGPUs: []int{8}}, ShareVariant{GPUs: 4, PodGPUs: []int{4}}, true),
		Entry("two 4-GPU pods do not fund one 8-GPU pod: nothing shows they share a node",
			ShareVariant{GPUs: 8, PodGPUs: []int{4, 4}}, ShareVariant{GPUs: 8, PodGPUs: []int{8}}, false),
		Entry("one 8-GPU pod does not fund two 4-GPU pods", ShareVariant{GPUs: 8, PodGPUs: []int{8}}, ShareVariant{GPUs: 8, PodGPUs: []int{4, 4}}, false),
		Entry("a 2x8 group funds a 2x4 group", ShareVariant{GPUs: 16, PodGPUs: []int{8, 8}}, ShareVariant{GPUs: 8, PodGPUs: []int{4, 4}}, true),
		Entry("a CPU leader is matched last", ShareVariant{GPUs: 16, PodGPUs: []int{0, 8, 8}}, ShareVariant{GPUs: 8, PodGPUs: []int{8}}, true),
		Entry("unknown shape falls back to totals", ShareVariant{GPUs: 8}, ShareVariant{GPUs: 8, PodGPUs: []int{8}}, true),
		Entry("unknown shape, smaller total", ShareVariant{GPUs: 4}, ShareVariant{GPUs: 8}, false),
	)

	// A is over-provisioned and B short, at equal weights; one replica of
	// either is 8 GPUs. Only the pod shape decides whether A can fund B.
	plan := func(donorPods []int) int {
		roles := []ShareRole{
			{Key: "A", Weight: 1, Need: 8, Ceiling: 64, ReplicaGPUs: 8},
			{Key: "B", Weight: 1, Need: 24, Ceiling: 64, ReplicaGPUs: 8},
		}
		in := SharePlanInput{
			Roles: roles, Held: map[string]int{"A": 24, "B": 8}, Thresholds: map[string]float64{"A": 0.8, "B": 0.8},
			Budget: 32, Tolerance: 0.15,
			Give: map[string]ShareVariant{"A": {Name: "a", GPUs: 8, PodGPUs: donorPods}},
			Grow: map[string]ShareVariant{"B": {Name: "b", GPUs: 8, PodGPUs: []int{8}}},
		}
		l := NewShareLedger()
		started := 0
		for i := range 3 {
			started += len(PlanShareTransfers(l, in, time.Unix(int64(30*i), 0), simTimings()).Started)
		}
		return started
	}

	It("moves a replica when a donor pod can host the receiver's pod", func() {
		Expect(plan([]int{8})).To(BeNumerically(">", 0))
	})

	It("moves nothing when only smaller donor pods add up to the receiver's (negative control above)", func() {
		Expect(plan([]int{4, 4})).To(BeZero())
	})
})

var _ = Describe("Reserve refill (§6.2)", func() {
	// A is well above its need, B at it; together they hold 10 of a budget
	// that, net of the reserve, is 8. The two over are reserve a wake spent.
	roles := []ShareRole{
		{Key: "A", Weight: 1, Need: 2, Floor: 1, Ceiling: 64, ReplicaGPUs: 1},
		{Key: "B", Weight: 1, Need: 4, Floor: 1, Ceiling: 64, ReplicaGPUs: 1},
	}
	plan := func(budget int) (SharePlan, *ShareLedger) {
		l := NewShareLedger()
		in := SharePlanInput{Roles: roles, Held: map[string]int{"A": 6, "B": 4},
			Thresholds: map[string]float64{"A": 0.8, "B": 0.8}, Budget: budget, Tolerance: 0.15}
		return PlanShareTransfers(l, in, time.Unix(0, 0), simTimings()), l
	}

	It("pays the debt back from the best-off donor, raising nobody, without waiting for confirmation", func() {
		p, l := plan(8)
		Expect(p.ReserveDebt).To(Equal(2))
		Expect(p.Refills).To(Equal(2))
		for _, t := range p.Started {
			Expect(t.Donor).To(Equal("A"))
			Expect(t.Receiver).To(BeEmpty())
			Expect(t.Entitled).To(BeTrue())
		}
		Expect(l.Committed(map[string]int{"A": 6, "B": 4})).To(Equal(map[string]int{"A": 4, "B": 4}),
			"a refill books its GPUs to no receiver")
	})

	It("plans no refill when the group is within its budget (control)", func() {
		p, _ := plan(10)
		Expect(p.ReserveDebt).To(BeZero())
		Expect(p.Refills).To(BeZero())
	})

	It("completes a refill at release", func() {
		_, l := plan(8)
		ends := l.Observe(map[string]int{"A": 4, "B": 4}, time.Unix(60, 0), simTimings())
		Expect(ends).To(HaveLen(2))
		for _, e := range ends {
			Expect(e.Outcome).To(Equal(ShareOutcomeDone))
		}
		Expect(l.Promised()).To(BeZero())
	})
})

var _ = Describe("Why a receiver is not funded (§9)", func() {
	// B is far short; A is the only other role. Over three cycles B is
	// confirmed actionable, so a receiver that is still unfunded says why.
	run := func(aFloor int, give, grow map[string]ShareVariant) SharePlan {
		roles := []ShareRole{
			{Key: "A", Weight: 1, Need: 8, Floor: aFloor, Ceiling: 64, ReplicaGPUs: 8},
			{Key: "B", Weight: 1, Need: 24, Ceiling: 64, ReplicaGPUs: 8},
		}
		in := SharePlanInput{Roles: roles, Held: map[string]int{"A": 24, "B": 8},
			Thresholds: map[string]float64{"A": 0.8, "B": 0.8}, Budget: 32, Tolerance: 0.15, Give: give, Grow: grow}
		l := NewShareLedger()
		var p SharePlan
		for i := range 3 {
			p = PlanShareTransfers(l, in, time.Unix(int64(30*i), 0), simTimings())
			if len(p.Started) > 0 {
				return p
			}
		}
		return p
	}
	grow := map[string]ShareVariant{"B": {Name: "b", GPUs: 8, PodGPUs: []int{8}}}

	It("names no-compatible-donor when no donor's pods fit the receiver's", func() {
		p := run(0, map[string]ShareVariant{"A": {Name: "a", GPUs: 8, PodGPUs: []int{4, 4}}}, grow)
		Expect(p.Started).To(BeEmpty())
		Expect(p.Unfunded).To(HaveKeyWithValue("B", ShareUnfundedNoCompatibleDonor))
	})

	It("names nothing when the receiver is funded (control)", func() {
		p := run(0, map[string]ShareVariant{"A": {Name: "a", GPUs: 8, PodGPUs: []int{8}}}, grow)
		Expect(p.Started).NotTo(BeEmpty())
		Expect(p.Unfunded).To(BeEmpty())
	})
})
