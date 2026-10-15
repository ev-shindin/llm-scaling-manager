package allocation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// govWorld is a cohort "c" with one h100 chunk read at readAt, the given
// lenders, and borrowing namespace "b" guaranteed g and using used.
type govWorld struct {
	readAt  time.Time
	stale   bool
	lenders []kueue.LenderState
	g, used int
	flavor  string
	// noOptIn leaves releaseToNonReclaimingLenders off (the default); the
	// release specs below opt in.
	noOptIn bool
}

func (w govWorld) constraints() []*ResourceConstraints {
	c := borrowQuota(0, nsBorrow{ns: "b", guaranteed: w.g, used: w.used, cap: config.QuotaUnlimited})
	flavor := w.flavor
	if flavor == "" {
		flavor = "h100"
	}
	c.SharedPools[0].Flavor = flavor
	if flavor == "a100" {
		c.SharedPools[0].AcceleratorType = "NVIDIA-A100-SXM4-80GB"
	}
	c.SharedPools[0].Lenders = w.lenders
	c.SharedPools[0].ReadAt = w.readAt
	c.SharedPools[0].Stale = w.stale
	c.SharedPools[0].ReleaseToNonReclaiming = !w.noOptIn
	return []*ResourceConstraints{c}
}

// never is a waiting lender that cannot reclaim, waiting for everything it
// lent out.
func never(lentOut int) kueue.LenderState {
	return kueue.LenderState{Queue: "lender", Waiting: true, LentOut: lentOut, PendingGPUs: lentOut}
}

// seenPast runs c once at an earlier time, so the namespaces c sees past
// guaranteed are known past it since then: a governor never told counts the
// placement timeout from its first cycle (observePast).
func seenPast(g *BorrowGovernor, c BorrowCycle, at time.Time) {
	c.Now = at
	g.Observe(c)
}

// decisionsOf are no-change decisions for every variant of the requests.
func decisionsOf(reqs []ModelScalingRequest) []domain.VariantDecision {
	out := make([]domain.VariantDecision, 0, len(reqs))
	for _, r := range reqs {
		for _, s := range r.VariantStates {
			out = append(out, domain.VariantDecision{Namespace: r.Namespace, ModelID: r.ModelID, VariantName: s.VariantName,
				CurrentReplicas: s.CurrentReplicas, TargetReplicas: s.CurrentReplicas, GPUsPerReplica: s.GPUsPerReplica})
		}
	}
	return out
}

func targets(ds []domain.VariantDecision) map[string]int {
	out := map[string]int{}
	for _, d := range ds {
		out[d.VariantName] = d.TargetReplicas
	}
	return out
}

// govModel is a single-variant H100 model in ns "b" at replicas of gpr GPUs.
func govModel(id string, replicas, gpr int) ModelScalingRequest {
	r := borrowModel("b", id, gpr, replicas, 0, nil)
	r.VariantStates[0].AcceleratorName = "H100"
	return r
}

// pdModel is a P/D model in ns "b": one prefill and one decode replica of
// gpr GPUs each.
func pdModel(gpr int) ModelScalingRequest {
	return withSatEntry(&satEntryFixture{
		VariantCapacities: []vcFixture{
			{VariantName: "p", AcceleratorName: "H100", Cost: 5, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: "d", AcceleratorName: "H100", Cost: 5, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
		},
	}, ModelScalingRequest{ModelID: "pd", Namespace: "b", Disaggregated: true, Priority: 1,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "p", CurrentReplicas: 1, GPUsPerReplica: gpr, Role: "prefill", AcceleratorName: "H100"},
			{VariantName: "d", CurrentReplicas: 1, GPUsPerReplica: gpr, Role: "decode", AcceleratorName: "H100"},
		}})
}

var _ = Describe("BorrowGovernor", func() {
	t0 := time.Unix(10000, 0)
	settings := DefaultBorrowSettings()

	// run feeds the governor one cycle and returns the releases and targets.
	run := func(g *BorrowGovernor, w govWorld, now time.Time, reqs []ModelScalingRequest) ([]BorrowRelease, map[string]int) {
		c := BorrowCycle{Now: now, Constraints: w.constraints(), Requests: reqs}
		g.Observe(c)
		ds := decisionsOf(reqs)
		rel := g.Releases(c, ds)
		return rel, targets(ds)
	}
	// warm takes a fresh governor past the restart entry and the debounce:
	// reads at t0, t0+30s, t0+61s, the lender waiting throughout.
	warm := func(w govWorld, reqs []ModelScalingRequest) (*BorrowGovernor, []BorrowRelease, time.Time) {
		g := NewBorrowGovernor(settings)
		var rel []BorrowRelease
		for _, d := range []time.Duration{0, 30 * time.Second, 61 * time.Second} {
			w.readAt = t0.Add(d)
			rel, _ = run(g, w, t0.Add(d), reqs)
		}
		return g, rel, t0.Add(61 * time.Second)
	}

	Describe("the debounced lender rule", func() {
		It("does not count one read, nor two inside the debounce, and counts two fresh reads 60s apart", func() {
			g := NewBorrowGovernor(settings)
			reqs := []ModelScalingRequest{govModel("m", 4, 8)}
			w := govWorld{lenders: []kueue.LenderState{never(8)}, used: 32}
			for _, d := range []time.Duration{0, 30 * time.Second} {
				w.readAt = t0.Add(d)
				rel, _ := run(g, w, t0.Add(d), reqs)
				Expect(rel).To(BeEmpty(), d.String())
			}
			w.readAt = t0.Add(61 * time.Second)
			rel, tg := run(g, w, t0.Add(61*time.Second), reqs)
			Expect(rel).To(HaveLen(1))
			Expect(tg["m-v"]).To(Equal(3))
		})

		It("ignores a lender waiting on another flavor", func() {
			_, rel, _ := warm(govWorld{lenders: []kueue.LenderState{never(8)}, used: 32, flavor: "a100"},
				[]ModelScalingRequest{govModel("m", 4, 8)})
			Expect(rel).To(BeEmpty(), "the H100 borrower holds nothing of the A100 lender's flavor")
		})

		It("by default releases nothing to a lender that cannot reclaim, and closes the cohort to new borrowing", func() {
			g, rel, now := warm(govWorld{lenders: []kueue.LenderState{never(8)}, used: 32, noOptIn: true},
				[]ModelScalingRequest{govModel("m", 4, 8)})
			Expect(rel).To(BeEmpty(), "releaseToNonReclaimingLenders is off")
			Expect(g.BlockedCohorts(now)).To(HaveKey("c"), "no new borrowing while the lender waits")
			Expect(g.WaitingLenders(now)).To(HaveLen(1), "the wait is reported")
			Expect(g.Ledger().Entries).To(BeEmpty(), "and nothing pins a variant")
		})

		It("releases nothing for a lender that can reclaim, and closes the cohort to new borrowing", func() {
			l := never(8)
			l.CanReclaim = true
			g, rel, now := warm(govWorld{lenders: []kueue.LenderState{l}, used: 32}, []ModelScalingRequest{govModel("m", 4, 8)})
			Expect(rel).To(BeEmpty(), "Kueue preempts for it")
			Expect(g.BlockedCohorts(now)).To(HaveKey("c"))
		})
	})

	It("releases a whole 8+8 pair against a 4-GPU shortfall, not nothing", func() {
		_, rel, _ := warm(govWorld{lenders: []kueue.LenderState{never(4)}, used: 16}, []ModelScalingRequest{pdModel(8)})
		Expect(rel).To(HaveLen(2))
		for _, r := range rel {
			Expect(r.To).To(Equal(0))
			Expect(r.Cause).To(Equal(ReleaseLenderWaiting))
		}
	})

	It("keeps one release in flight: short, lent out 32, a slow KEDA window -- exactly one unit", func() {
		reqs := []ModelScalingRequest{govModel("m", 4, 8)}
		w := govWorld{lenders: []kueue.LenderState{never(32)}, used: 32}
		g, rel, now := warm(w, reqs)
		Expect(rel).To(HaveLen(1))
		total := len(rel)
		for i := 1; i <= 20; i++ {
			now = now.Add(30 * time.Second)
			w.readAt = now // fresh reads; the pods stay (KEDA's window)
			r, tg := run(g, w, now, reqs)
			total += len(r)
			Expect(tg["m-v"]).To(Equal(3), "the release stays in force")
		}
		Expect(total).To(Equal(1))
		Expect(g.Ledger().Entries[0].Reason).To(Equal(ReasonReleaseNotActuated), "outlived 2 x grace + the KEDA window")
	})

	It("clears an entry once the pods are gone and a fresh read postdates that, then releases the next unit", func() {
		reqs := []ModelScalingRequest{govModel("m", 4, 8)}
		w := govWorld{lenders: []kueue.LenderState{never(32)}, used: 32}
		g, _, now := warm(w, reqs)
		gone := []ModelScalingRequest{govModel("m", 3, 8)}
		w.used = 24
		now = now.Add(15 * time.Second)
		r, _ := run(g, w, now, gone) // pods gone, no newer read yet
		Expect(r).To(BeEmpty())
		now = now.Add(15 * time.Second)
		w.readAt = now
		r, _ = run(g, w, now, gone)
		Expect(r).To(HaveLen(1))
		Expect(r[0].To).To(Equal(2))
	})

	It("bounds the total by the lent-out nominal summed over the waiting lenders on the flavor", func() {
		release := func(lenders ...kueue.LenderState) int {
			reqs := []ModelScalingRequest{govModel("m", 4, 8)}
			w := govWorld{lenders: lenders, used: 32}
			g, rel, now := warm(w, reqs)
			total := len(rel)
			for cur := 4 - len(rel); cur > 0 && now.Before(t0.Add(20*time.Minute)); {
				now = now.Add(30 * time.Second)
				w.readAt = now
				w.used = cur * 8
				r, _ := run(g, w, now, []ModelScalingRequest{govModel("m", cur, 8)})
				total += len(r)
				cur -= len(r)
			}
			return total
		}
		a, b := never(8), never(8)
		a.Queue, b.Queue = "lender-a", "lender-b"
		Expect(release(a)).To(Equal(1))
		Expect(release(a, b)).To(Equal(2))
	})

	It("bounds the release by what the lender waits for, rounded up to whole units, never past what it lent out", func() {
		release := func(l kueue.LenderState) int {
			reqs := []ModelScalingRequest{govModel("m", 4, 8)}
			w := govWorld{lenders: []kueue.LenderState{l}, used: 32}
			g, rel, now := warm(w, reqs)
			total := len(rel)
			for cur := 4 - len(rel); cur > 0 && now.Before(t0.Add(20*time.Minute)); {
				now = now.Add(30 * time.Second)
				w.readAt = now
				w.used = cur * 8
				r, _ := run(g, w, now, []ModelScalingRequest{govModel("m", cur, 8)})
				total += len(r)
				cur -= len(r)
			}
			return total
		}
		waits := func(lentOut, pending int) kueue.LenderState {
			l := never(lentOut)
			l.PendingGPUs = pending
			return l
		}
		Expect(release(waits(32, 4))).To(Equal(1), "4 GPUs pending, 32 lent out: exactly one 8-GPU unit")
		Expect(release(waits(32, 12))).To(Equal(2), "12 pending rounds up to two units")
		Expect(release(waits(32, 0))).To(Equal(0), "a lender waiting for nothing is handed nothing")
		Expect(release(waits(8, 40))).To(Equal(1), "never past what it lent out")
	})

	It("does not count gated pods as borrowed", func() {
		g := NewBorrowGovernor(settings)
		reqs := []ModelScalingRequest{govModel("m", 2, 8)}
		w := govWorld{lenders: []kueue.LenderState{never(8)}, used: 16, g: 8}
		var rel []BorrowRelease
		for _, d := range []time.Duration{0, 30 * time.Second, 61 * time.Second} {
			w.readAt = t0.Add(d)
			c := BorrowCycle{Now: t0.Add(d), Constraints: w.constraints(), Requests: reqs, Gated: map[string]map[string]int{"b": {"": 8}}}
			g.Observe(c)
			rel = g.Releases(c, decisionsOf(reqs))
		}
		Expect(rel).To(BeEmpty(), "the 8 past guaranteed are a gated pod, which holds no reservation")
	})

	It("never goes below the guaranteed figure or minReplicas", func() {
		_, rel, _ := warm(govWorld{lenders: []kueue.LenderState{never(8)}, used: 12, g: 8}, []ModelScalingRequest{govModel("m", 2, 6)})
		Expect(rel).To(BeEmpty(), "4 borrowed; a 6-GPU replica would take the namespace 2 below guaranteed")

		pinned := govModel("m", 4, 8)
		four := 4
		pinned.VariantStates[0].MinReplicas = &four
		_, rel, _ = warm(govWorld{lenders: []kueue.LenderState{never(8)}, used: 32}, []ModelScalingRequest{pinned})
		Expect(rel).To(BeEmpty())
	})

	It("overrides the demand floor while the release is in flight", func() {
		reqs := []ModelScalingRequest{govModel("m", 4, 8)}
		w := govWorld{lenders: []kueue.LenderState{never(32)}, used: 32}
		g, _, now := warm(w, reqs)
		now = now.Add(30 * time.Second)
		c := BorrowCycle{Now: now, Constraints: w.constraints(), Requests: reqs}
		g.Observe(c)
		ds := decisionsOf(reqs)
		ds[0].TargetReplicas = 6 // the floor asks for more
		g.Releases(c, ds)
		Expect(ds[0].TargetReplicas).To(Equal(3))
	})

	Describe("order", func() {
		It("releases the lowest priority first", func() {
			hi, lo := govModel("hi", 2, 8), govModel("lo", 2, 8)
			hi.Priority, lo.Priority = 2, 1
			_, rel, _ := warm(govWorld{lenders: []kueue.LenderState{never(32)}, used: 32}, []ModelScalingRequest{hi, lo})
			Expect(rel).To(HaveLen(1))
			Expect(rel[0].ModelID).To(Equal("lo"))
		})

		It("then the most recently grown, and after a restart the most expensive", func() {
			a, b := govModel("a", 2, 8), govModel("b", 2, 8)
			b.Variants[0].Cost = 50
			reqs := []ModelScalingRequest{a, b}
			_, rel, _ := warm(govWorld{lenders: []kueue.LenderState{never(32)}, used: 32}, reqs)
			Expect(rel[0].ModelID).To(Equal("b"), "growth unknown: the most expensive")

			g := NewBorrowGovernor(settings)
			w := govWorld{lenders: []kueue.LenderState{never(32)}, used: 32}
			for i, d := range []time.Duration{0, 30 * time.Second, 61 * time.Second} {
				w.readAt = t0.Add(d)
				c := BorrowCycle{Now: t0.Add(d), Constraints: w.constraints(), Requests: reqs}
				g.Observe(c)
				ds := decisionsOf(reqs)
				if i == 0 {
					ds[0].TargetReplicas = 3 // a grew in the first cycle
				}
				rel = g.Releases(c, ds)
			}
			Expect(rel[0].ModelID).To(Equal("a"), "the most recently grown")
		})
	})

	It("persists the ledger and, reloaded after a restart, keeps the release in flight", func() {
		reqs := []ModelScalingRequest{govModel("m", 4, 8)}
		w := govWorld{lenders: []kueue.LenderState{never(32)}, used: 32}
		g, rel, now := warm(w, reqs)
		Expect(rel).To(HaveLen(1))
		raw, err := g.MarshalLedger()
		Expect(err).NotTo(HaveOccurred())
		l, err := UnmarshalBorrowLedger(raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Entries).To(HaveLen(1))

		restarted := NewBorrowGovernor(settings)
		restarted.Restore(l)
		for i := 0; i < 4; i++ {
			now = now.Add(30 * time.Second)
			w.readAt = now
			r, tg := run(restarted, w, now, reqs)
			Expect(r).To(BeEmpty())
			Expect(tg["m-v"]).To(Equal(3))
		}
	})

	It("after a restart with nothing persisted, treats a waiting lender as in flight until the next fresh read", func() {
		g := NewBorrowGovernor(settings)
		w := govWorld{lenders: []kueue.LenderState{never(32)}, used: 32, readAt: t0}
		run(g, w, t0, []ModelScalingRequest{govModel("m", 4, 8)})
		Expect(g.Ledger().Entries).To(HaveLen(1))
		Expect(g.Ledger().Entries[0].Unknown).To(BeTrue())
	})

	It("freezes on a stale read, the ledger included", func() {
		reqs := []ModelScalingRequest{govModel("m", 4, 8)}
		w := govWorld{lenders: []kueue.LenderState{never(32)}, used: 32}
		g, _, now := warm(w, reqs)
		w.stale = true
		w.lenders = []kueue.LenderState{{Queue: "lender", LentOut: 32}} // the frozen read
		w.readAt = now.Add(time.Hour)
		for i := 0; i < 3; i++ {
			now = now.Add(30 * time.Second)
			r, tg := run(g, w, now, []ModelScalingRequest{govModel("m", 3, 8)})
			Expect(r).To(BeEmpty())
			Expect(tg["m-v"]).To(Equal(3))
		}
		Expect(g.Ledger().Entries).To(HaveLen(1), "neither reset nor cleared while stale")
	})

	Describe("cooldowns", func() {
		It("keeps the cohort closed for the cooldown after its lender stopped waiting", func() {
			w := govWorld{lenders: []kueue.LenderState{never(8)}, used: 32}
			g, _, now := warm(w, []ModelScalingRequest{govModel("m", 4, 8)})
			w.lenders = []kueue.LenderState{{Queue: "lender", LentOut: 0}}
			w.readAt = now.Add(30 * time.Second)
			run(g, w, now.Add(30*time.Second), []ModelScalingRequest{govModel("m", 3, 8)})
			Expect(g.Ledger().Entries).To(BeEmpty(), "reset when the lender stops waiting")
			Expect(g.BlockedCohorts(now.Add(9 * time.Minute))).To(HaveKey("c"))
			Expect(g.BlockedCohorts(now.Add(11 * time.Minute))).To(BeEmpty())
		})

		It("blocks a model's new borrowing for the cooldown after a preemption", func() {
			g := NewBorrowGovernor(settings)
			g.MarkPreempted("b/m", t0)
			Expect(g.BorrowBlocked("b", "m", "m-v", t0.Add(9*time.Minute))).To(BeTrue())
			Expect(g.BorrowBlocked("b", "m", "m-v", t0.Add(11*time.Minute))).To(BeFalse())
		})
	})

	Describe("placement", func() {
		unplaced := func(r ModelScalingRequest, variant int, since time.Time) ModelScalingRequest {
			r.VariantStates[variant].UnplacedReplicas = 1
			r.VariantStates[variant].UnplacedSince = since
			return r
		}

		It("holds new borrowed growth of a variant with unplaced replicas", func() {
			g := NewBorrowGovernor(settings)
			g.Observe(BorrowCycle{Now: t0, Requests: []ModelScalingRequest{unplaced(govModel("m", 2, 8), 0, t0)}})
			Expect(g.BorrowBlocked("b", "m", "m-v", t0)).To(BeTrue())
			Expect(g.BorrowBlocked("b", "other", "other-v", t0)).To(BeFalse())
		})

		It("releases a borrowed unit unplaced past the timeout with the partner that placed, and starts the cooldown", func() {
			g := NewBorrowGovernor(settings)
			pd := unplaced(pdModel(8), 1, t0) // decode unplaced, prefill placed
			c := BorrowCycle{Now: t0.Add(6 * time.Minute), Constraints: govWorld{used: 16}.constraints(), Requests: []ModelScalingRequest{pd}}
			seenPast(g, c, t0)
			g.Observe(c)
			ds := decisionsOf(c.Requests)
			rel := g.Releases(c, ds)
			Expect(rel).To(HaveLen(2))
			Expect(targets(ds)).To(Equal(map[string]int{"p": 0, "d": 0}))
			Expect(g.BorrowBlocked("b", "pd", "p", t0.Add(7*time.Minute))).To(BeTrue())
		})

		It("waits while the cluster autoscaler is scaling up, at most three times the timeout", func() {
			g := NewBorrowGovernor(settings)
			r := unplaced(govModel("m", 2, 8), 0, t0)
			pending := map[string]bool{"b/m-v": true}
			cycle := func(at time.Duration) []BorrowRelease {
				c := BorrowCycle{Now: t0.Add(at), Constraints: govWorld{used: 16}.constraints(), Requests: []ModelScalingRequest{r}, ScaleUpPending: pending}
				g.Observe(c)
				return g.Releases(c, decisionsOf(c.Requests))
			}
			Expect(cycle(0)).To(BeEmpty())
			Expect(cycle(6*time.Minute)).To(BeEmpty(), "TriggeredScaleUp: the timeout waits")
			Expect(cycle(16*time.Minute)).To(HaveLen(1), "capped at three times the timeout")

			g = NewBorrowGovernor(settings)
			pending = nil // a later NotTriggerScaleUp ends the wait
			Expect(cycle(0)).To(BeEmpty())
			Expect(cycle(6 * time.Minute)).To(HaveLen(1))
		})

		It("exempts growth within guaranteed, a reclaiming queue's growth below nominal included", func() {
			g := NewBorrowGovernor(settings)
			c := BorrowCycle{Now: t0.Add(time.Hour), Constraints: govWorld{used: 16, g: 16}.constraints(),
				Requests: []ModelScalingRequest{unplaced(govModel("m", 2, 8), 0, t0)}}
			g.Observe(c)
			Expect(g.Releases(c, decisionsOf(c.Requests))).To(BeEmpty(), "Kueue reclaims for it")
		})

		// The live-Kueue e2e (kueue_borrow_test.go, spec 4): nominal 2, queue
		// a reclaims (Any) and already reserves its whole nominal, and Kueue
		// preempted a borrowed pod -- the ReplicaSet's replacement is gated.
		// The namespace holds 3 replicas of 1 GPU, 1 of them gated: the gated
		// one is past guaranteed, Kueue has nothing to reclaim for it, and it
		// must go after the timeout.
		gatedReplacement := func(lentOut, used int) BorrowCycle {
			r := govModel("m", used, 1)
			r = unplaced(r, 0, t0)
			c := borrowQuota(0, nsBorrow{ns: "b", guaranteed: 2, used: used, cap: config.QuotaUnlimited, lentOut: lentOut})
			return BorrowCycle{Now: t0.Add(settings.PlacementTimeout + time.Second),
				Constraints: []*ResourceConstraints{c}, Requests: []ModelScalingRequest{r},
				Gated: map[string]map[string]int{"b": {"H100": 1}}}
		}

		It("releases a gated replica past guaranteed when its queue reserves its full nominal", func() {
			g := NewBorrowGovernor(settings)
			c := gatedReplacement(0, 3)
			seenPast(g, c, t0)
			g.Observe(c)
			ds := decisionsOf(c.Requests)
			rel := g.Releases(c, ds)
			Expect(rel).To(HaveLen(1), "the gated replacement of a preempted borrowed pod is borrowed")
			Expect(rel[0].Cause).To(Equal(ReleasePlacementTimeout))
			Expect(rel[0].From).To(Equal(3))
			Expect(rel[0].To).To(Equal(2))
			Expect(targets(ds)).To(Equal(map[string]int{"m-v": 2}))
			Expect(g.BorrowBlocked("b", "m", "m-v", c.Now.Add(time.Minute))).To(BeTrue(), "the model's cooldown starts")
		})

		It("exempts a gated replica within guaranteed while its reclaiming queue has nominal lent out", func() {
			g := NewBorrowGovernor(settings)
			c := gatedReplacement(1, 2) // holds 2 of 2, 1 gated: a peer holds 1 of a's nominal
			g.Observe(c)
			Expect(g.Releases(c, decisionsOf(c.Requests))).To(BeEmpty(), "Kueue reclaims the lent-out GPU for it")
		})
	})

	It("stops the greedy optimizer from borrowing for a blocked variant, not from growing within guaranteed", func() {
		o := NewGreedyByScoreOptimizer()
		o.BorrowBlocked = func(_, model, _ string) bool { return model == "mb" }
		got := addedGPUs(o.Optimize(context.Background(), []ModelScalingRequest{
			borrowModel("g", "mg", 8, 0, 1, nil),
			borrowModel("b", "mb", 8, 0, 1, nil),
		}, []*ResourceConstraints{physicalH100(64), borrowQuota(16,
			nsBorrow{ns: "g", guaranteed: 8, cap: config.QuotaUnlimited},
			nsBorrow{ns: "b", cap: config.QuotaUnlimited})}))
		Expect(got).To(Equal(map[string]int{"mg": 8}))
	})
})
