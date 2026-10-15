package allocation

import (
	"maps"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// pdRole is one variant of a P/D fleet: its name, role and replicas.
type pdRole struct {
	name, role string
	n          int
}

// pdGPUs is the GPUs of every replica of a pdFleet: whole-node replicas.
const pdGPUs = 8

// pdFleet is a P/D model "pd" in ns "b" on H100, every replica of pdGPUs.
func pdFleet(roles ...pdRole) ModelScalingRequest {
	vcs := make([]vcFixture, 0, len(roles))
	sts := make([]domain.VariantReplicaState, 0, len(roles))
	for _, r := range roles {
		vcs = append(vcs, vcFixture{VariantName: r.name, AcceleratorName: "H100", Cost: 5, Role: r.role, ReplicaCount: r.n, PerReplicaCapacity: 10000})
		sts = append(sts, domain.VariantReplicaState{VariantName: r.name, CurrentReplicas: r.n, GPUsPerReplica: pdGPUs, Role: r.role, AcceleratorName: "H100"})
	}
	return withSatEntry(&satEntryFixture{VariantCapacities: vcs},
		ModelScalingRequest{ModelID: "pd", Namespace: "b", Disaggregated: true, Priority: 1, VariantStates: sts})
}

// unplacedVariant marks one replica of the named variant unplaced since t.
func unplacedVariant(r ModelScalingRequest, variant string, t time.Time) ModelScalingRequest {
	for i := range r.VariantStates {
		if r.VariantStates[i].VariantName == variant {
			r.VariantStates[i].UnplacedReplicas = 1
			r.VariantStates[i].UnplacedSince = t
		}
	}
	return r
}

// unplacedPods marks the named variant's pods unplaced at the given times, as
// discovery reports them: UnplacedSince is the newest.
func unplacedPods(r ModelScalingRequest, variant string, pods map[string]time.Time) ModelScalingRequest {
	for i := range r.VariantStates {
		s := &r.VariantStates[i]
		if s.VariantName != variant {
			continue
		}
		s.UnplacedReplicas = len(pods)
		s.UnplacedPodSince = pods
		for name, at := range pods {
			s.UnplacedPods = append(s.UnplacedPods, name)
			if at.After(s.UnplacedSince) {
				s.UnplacedSince = at
			}
		}
	}
	return r
}

func releasedGPUs(rel []BorrowRelease) int {
	n := 0
	for _, r := range rel {
		n += (r.From - r.To) * pdGPUs
	}
	return n
}

var _ = Describe("BorrowGovernor release units", func() {
	t0 := time.Unix(20000, 0)
	settings := DefaultBorrowSettings()

	// timeout runs one cycle past the placement timeout.
	timeout := func(g, used int, reqs ...ModelScalingRequest) ([]BorrowRelease, map[string]int) {
		gov := NewBorrowGovernor(settings)
		c := BorrowCycle{Now: t0.Add(settings.PlacementTimeout + time.Minute), Constraints: govWorld{g: g, used: used}.constraints(), Requests: reqs}
		gov.Observe(c)
		ds := decisionsOf(reqs)
		rel := gov.Releases(c, ds)
		return rel, targets(ds)
	}

	// The placement timeout's clock is per pod, from when it became unplaced
	// or from when its namespace went past guaranteed, whichever is later.
	Describe("placement timeout clock", func() {
		run := func(g *BorrowGovernor, at time.Time, world govWorld, req ModelScalingRequest) []BorrowRelease {
			c := BorrowCycle{Now: at, Constraints: world.constraints(), Requests: []ModelScalingRequest{req}}
			g.Observe(c)
			return g.Releases(c, decisionsOf(c.Requests))
		}

		// Review finding: with the newest unplaced pod as the clock, a variant
		// that keeps adding unplaced pods never lets its oldest time out.
		It("times out the oldest unplaced pod while the variant keeps adding new ones", func() {
			g := NewBorrowGovernor(settings)
			world := govWorld{used: 48}
			pods := map[string]time.Time{}
			for i, at := range []time.Duration{0, 2 * time.Minute, 4 * time.Minute} {
				pods[string(rune('a'+i))] = t0.Add(at)
				Expect(run(g, t0.Add(at), world, unplacedPods(govModel("x", 6, 8), "x-v", maps.Clone(pods)))).To(BeEmpty())
			}
			now := t0.Add(settings.PlacementTimeout + 10*time.Second)
			pods["d"] = t0.Add(settings.PlacementTimeout)
			rel := run(g, now, world, unplacedPods(govModel("x", 6, 8), "x-v", maps.Clone(pods)))
			Expect(rel).To(HaveLen(1), "pod a has been unplaced past the timeout, however many came after it")
			Expect(rel[0].From - rel[0].To).To(Equal(1), "one pod past the timeout: one replica")
		})

		// The round-1 finding stays fixed: a pod stuck since before the
		// namespace borrowed does not time out a replica borrowed a minute ago.
		It("does not time out a fresh borrowed replica because of a pod stuck since before the namespace borrowed", func() {
			g := NewBorrowGovernor(settings)
			old := map[string]time.Time{"old": t0.Add(-20 * time.Minute)}
			Expect(run(g, t0, govWorld{g: 16, used: 16}, unplacedPods(govModel("x", 2, 8), "x-v", maps.Clone(old)))).To(BeEmpty(),
				"within guaranteed")

			both := map[string]time.Time{"old": t0.Add(-20 * time.Minute), "new": t0.Add(30 * time.Second)}
			borrowed := govWorld{g: 16, used: 24}
			Expect(run(g, t0.Add(time.Minute), borrowed, unplacedPods(govModel("x", 3, 8), "x-v", maps.Clone(both)))).To(BeEmpty(),
				"the namespace went past guaranteed a moment ago")
			Expect(run(g, t0.Add(time.Minute+settings.PlacementTimeout-time.Second), borrowed,
				unplacedPods(govModel("x", 3, 8), "x-v", maps.Clone(both)))).To(BeEmpty())
			rel := run(g, t0.Add(time.Minute+settings.PlacementTimeout+time.Second), borrowed,
				unplacedPods(govModel("x", 3, 8), "x-v", maps.Clone(both)))
			Expect(rel).To(HaveLen(1), "a full timeout after the namespace borrowed")
			Expect(rel[0].From - rel[0].To).To(Equal(1), "bounded by the 8 borrowed GPUs")
		})
	})

	Describe("placement timeout on a P/D model", func() {
		It("1P:2D, 8 GPUs borrowed, a decode unplaced: releases that decode and keeps the pair (d:1, not 0/0)", func() {
			pd := unplacedVariant(pdFleet(pdRole{"p", "prefill", 1}, pdRole{"d", "decode", 2}), "d", t0)
			rel, tg := timeout(16, 24, pd)
			Expect(tg).To(Equal(map[string]int{"p": 1, "d": 1}))
			Expect(releasedGPUs(rel)).To(Equal(8))
		})

		It("1P:2D with minReplicas 1 on both roles still releases the unplaced borrowed decode", func() {
			pd := unplacedVariant(pdFleet(pdRole{"p", "prefill", 1}, pdRole{"d", "decode", 2}), "d", t0)
			one := 1
			for i := range pd.VariantStates {
				pd.VariantStates[i].MinReplicas = &one
			}
			_, tg := timeout(16, 24, pd)
			Expect(tg).To(Equal(map[string]int{"p": 1, "d": 1}))
		})

		It("2P:3D, 8 GPUs borrowed: releases one decode, not the model", func() {
			pd := unplacedVariant(pdFleet(pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 3}), "d", t0)
			rel, tg := timeout(32, 40, pd)
			Expect(tg).To(Equal(map[string]int{"p": 2, "d": 2}))
			Expect(releasedGPUs(rel)).To(Equal(8))
		})

		It("2P:4D, 32 borrowed over 16 guaranteed: releases at most one pair", func() {
			pd := unplacedVariant(pdFleet(pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 4}), "d", t0)
			rel, tg := timeout(16, 48, pd)
			Expect(releasedGPUs(rel)).To(BeNumerically(">", 0))
			Expect(releasedGPUs(rel)).To(BeNumerically("<=", 16))
			Expect(tg["p"]).To(BeNumerically(">=", 1))
			Expect(tg["d"]).To(BeNumerically(">=", 1))
		})

		// Review finding (whole-node): the borrowed prefill of a 2P:4D model
		// does not place, 8 GPUs borrowed. The ratio unit is {p:1, d:2}, 24
		// GPUs: two guaranteed decodes went with it.
		It("2P:4D, 8 GPUs borrowed, a prefill unplaced: releases that prefill alone, not two guaranteed decodes", func() {
			pd := unplacedVariant(pdFleet(pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 4}), "p", t0)
			rel, tg := timeout(40, 48, pd)
			Expect(tg).To(Equal(map[string]int{"p": 1, "d": 4}), "1P:4D still serves")
			Expect(releasedGPUs(rel)).To(Equal(8))
			Expect(rel).To(HaveLen(1))
			Expect(rel[0].GuaranteedGPUs).To(BeZero())
		})

		It("1P:2D, 8 GPUs borrowed, its only prefill unplaced: the whole pair goes, and says it went below guaranteed", func() {
			pd := unplacedVariant(pdFleet(pdRole{"p", "prefill", 1}, pdRole{"d", "decode", 2}), "p", t0)
			rel, tg := timeout(16, 24, pd)
			Expect(tg).To(Equal(map[string]int{"p": 0, "d": 0}), "no decode is left without its prefill")
			below := 0
			for _, r := range rel {
				below += r.GuaranteedGPUs
			}
			Expect(below).To(Equal(16))
		})

		It("counts a role split over two variants, and leaves no prefill without a decode", func() {
			pd := unplacedVariant(pdFleet(pdRole{"p1", "prefill", 1}, pdRole{"p2", "prefill", 1}, pdRole{"d", "decode", 1}), "d", t0)
			_, tg := timeout(0, 24, pd)
			Expect(tg).To(Equal(map[string]int{"p1": 0, "p2": 0, "d": 0}))
		})
	})

	It("takes each placement-timeout release off what the namespace holds past guaranteed", func() {
		// Guaranteed 2, held 3 (1 GPU each): m1 2 replicas, m2 1 replica,
		// each with one unplaced. Only one GPU is borrowed: one release.
		m1 := unplacedVariant(govModel("m1", 2, 1), "m1-v", t0)
		m2 := unplacedVariant(govModel("m2", 1, 1), "m2-v", t0)
		gov := NewBorrowGovernor(settings)
		c := BorrowCycle{Now: t0.Add(settings.PlacementTimeout + time.Minute),
			Constraints: []*ResourceConstraints{borrowQuota(0, nsBorrow{ns: "b", guaranteed: 2, used: 3, cap: config.QuotaUnlimited})},
			Requests:    []ModelScalingRequest{m1, m2}}
		gov.Observe(c)
		ds := decisionsOf(c.Requests)
		rel := gov.Releases(c, ds)
		Expect(rel).To(HaveLen(1))
		tg := targets(ds)
		Expect(tg["m1-v"]+tg["m2-v"]).To(Equal(2), "the namespace keeps its guaranteed 2")
	})

	Describe("the ledger", func() {
		// warmRelease takes a governor through the debounce until it released
		// one replica of a 4 x 8-GPU model to a lender waiting for 32.
		warmRelease := func() (*BorrowGovernor, govWorld, time.Time) {
			g := NewBorrowGovernor(settings)
			w := govWorld{lenders: []kueue.LenderState{never(32)}, used: 32}
			reqs := []ModelScalingRequest{govModel("m", 4, 8)}
			for _, d := range []time.Duration{0, 30 * time.Second, 61 * time.Second} {
				w.readAt = t0.Add(d)
				c := BorrowCycle{Now: t0.Add(d), Constraints: w.constraints(), Requests: reqs}
				g.Observe(c)
				g.Releases(c, decisionsOf(reqs))
			}
			Expect(g.Ledger().Entries).To(HaveLen(1))
			Expect(g.Ledger().Entries[0].Units).To(HaveLen(1))
			return g, w, t0.Add(61 * time.Second)
		}

		It("drops a release whose lender left the pool, so it stops pinning the variant", func() {
			g, w, now := warmRelease()
			w.lenders = nil // the lender's ClusterQueue was deleted
			reqs := []ModelScalingRequest{govModel("m", 4, 8)}
			var tg map[string]int
			for i := 0; i < 50; i++ {
				now = now.Add(30 * time.Second)
				w.readAt = now
				c := BorrowCycle{Now: now, Constraints: w.constraints(), Requests: reqs}
				g.Observe(c)
				ds := decisionsOf(reqs)
				g.Releases(c, ds)
				tg = targets(ds)
			}
			Expect(tg["m-v"]).To(Equal(4), "the pin is cleared")
			Expect(g.Ledger().Entries).To(BeEmpty())
			Expect(g.WaitingLenders(now)).To(BeEmpty())
		})

		It("keeps a persisted released figure through the first cycle after a restart", func() {
			g := NewBorrowGovernor(settings)
			g.Restore(BorrowLedger{Released: map[string]int{"c/h100/nvidia.com/gpu": 8}})
			w := govWorld{lenders: []kueue.LenderState{never(8)}, used: 32, readAt: t0}
			g.Observe(BorrowCycle{Now: t0, Constraints: w.constraints(), Requests: []ModelScalingRequest{govModel("m", 4, 8)}})
			Expect(g.Ledger().Released).To(HaveKeyWithValue("c/h100/nvidia.com/gpu", 8))

			w.lenders = []kueue.LenderState{{Queue: "lender", LentOut: 8}}
			w.readAt = t0.Add(30 * time.Second)
			g.Observe(BorrowCycle{Now: t0.Add(30 * time.Second), Constraints: w.constraints(), Requests: []ModelScalingRequest{govModel("m", 4, 8)}})
			Expect(g.Ledger().Released).To(BeEmpty(), "forgotten once a fresh read finds no lender waiting")
		})

		It("does not let a late Restore overwrite a newer entry", func() {
			g, _, _ := warmRelease()
			key := g.Ledger().Entries[0].LenderKey
			g.Restore(BorrowLedger{Entries: []ReleaseLedgerEntry{{LenderKey: key, ReleasedAt: t0, Unknown: true}}})
			Expect(g.Ledger().Entries).To(HaveLen(1))
			Expect(g.Ledger().Entries[0].Unknown).To(BeFalse())
			Expect(g.Ledger().Entries[0].Units).To(HaveLen(1))
		})

		It("drops what a restored ledger cannot use", func() {
			g := NewBorrowGovernor(settings)
			l, err := UnmarshalBorrowLedger(`{"entries":[` +
				`{"cohort":"c","lender":"","flavor":"h100","resource":"nvidia.com/gpu","releasedAt":"2026-01-01T00:00:00Z","unknown":true},` +
				`{"cohort":"c","lender":"l","flavor":"h100","resource":"nvidia.com/gpu","releasedAt":"2026-01-01T00:00:00Z"}],` +
				`"released":{"c/h100/nvidia.com/gpu":-5}}`)
			Expect(err).NotTo(HaveOccurred())
			g.Restore(l)
			Expect(g.Ledger().Entries).To(BeEmpty(), "no lender; neither assumed nor carrying a unit")
			Expect(g.Ledger().Released).To(BeEmpty())
		})
	})

	Describe("between cycles that count borrowing", func() {
		It("keeps releases in flight in force in a cycle that counts none, and drops them once borrowing stays off", func() {
			g := NewBorrowGovernor(settings)
			l, err := UnmarshalBorrowLedger(`{"entries":[{"cohort":"c","lender":"l","flavor":"h100","resource":"nvidia.com/gpu",` +
				`"releasedAt":"1970-01-01T05:33:20Z","units":[{"namespace":"b","variant":"m-v","to":3}]}],"timeoutHeld":{"b/x-v":1}}`)
			Expect(err).NotTo(HaveOccurred())
			g.Restore(l)
			reqs := []ModelScalingRequest{govModel("m", 4, 8), govModel("x", 2, 8)}
			ds := decisionsOf(reqs)
			ds[0].TargetReplicas, ds[1].TargetReplicas = 6, 3 // the demand floor asks for more
			g.HoldReleases(t0, reqs, ds)
			Expect(targets(ds)).To(Equal(map[string]int{"m-v": 3, "x-v": 1}))

			later := t0.Add(settings.Cooldown + settings.ReleaseActuation + time.Minute)
			ds = decisionsOf(reqs)
			g.HoldReleases(later, reqs, ds)
			Expect(targets(ds)).To(Equal(map[string]int{"m-v": 4, "x-v": 2}), "borrowing has stayed off: nothing is pinned")
			Expect(g.Ledger().Entries).To(BeEmpty())
		})

		It("persists the cooldowns and the placement-timeout holds, and restores them", func() {
			g := NewBorrowGovernor(settings)
			g.MarkPreempted("b/m", t0)
			pd := unplacedVariant(govModel("x", 2, 8), "x-v", t0)
			c := BorrowCycle{Now: t0.Add(settings.PlacementTimeout + time.Minute), Constraints: govWorld{used: 16}.constraints(),
				Requests: []ModelScalingRequest{pd}}
			g.Observe(c)
			g.Releases(c, decisionsOf(c.Requests))
			raw, err := g.MarshalLedger()
			Expect(err).NotTo(HaveOccurred())
			l, err := UnmarshalBorrowLedger(raw)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.ModelCooldown).To(HaveKey("b/m"))
			Expect(l.ModelCooldown).To(HaveKey("b/x"))
			Expect(l.TimeoutHeld).To(HaveKeyWithValue("b/x-v", 1))

			restarted := NewBorrowGovernor(settings)
			restarted.Restore(l)
			now := t0.Add(7 * time.Minute)
			Expect(restarted.BorrowBlocked("b", "m", "m-v", now)).To(BeTrue(), "the preemption cooldown survives the restart")
			held := []ModelScalingRequest{govModel("x", 2, 8)}
			ds := decisionsOf(held)
			restarted.HoldReleases(now, held, ds)
			Expect(targets(ds)["x-v"]).To(Equal(1))
		})

		// Review finding: a placement-timeout release 3 -> 2, then borrowing
		// switched off. The unplaced pod is gone, and demand asks 3 again,
		// within nominal now that borrowing is not counted. The hold must not
		// cap it at 2 for the cooldown plus the actuation window.
		It("drops a placement-timeout hold once it landed, in cycles that count no borrowing", func() {
			g := NewBorrowGovernor(settings)
			x := unplacedVariant(govModel("x", 3, 8), "x-v", t0)
			c := BorrowCycle{Now: t0.Add(settings.PlacementTimeout + time.Minute), Constraints: govWorld{used: 24}.constraints(),
				Requests: []ModelScalingRequest{x}}
			g.Observe(c)
			Expect(g.Releases(c, decisionsOf(c.Requests))).To(HaveLen(1))
			Expect(g.Ledger().TimeoutHeld).To(HaveKeyWithValue("b/x-v", 2))

			now := c.Now.Add(time.Minute)
			still := []ModelScalingRequest{govModel("x", 3, 8)}
			ds := decisionsOf(still)
			g.HoldReleases(now, still, ds)
			Expect(targets(ds)["x-v"]).To(Equal(2), "not landed yet: still pinned")

			landed := []ModelScalingRequest{govModel("x", 2, 8)}
			ds = decisionsOf(landed)
			ds[0].TargetReplicas = 3 // demand asks for 3, within nominal
			g.HoldReleases(now.Add(time.Minute), landed, ds)
			Expect(targets(ds)["x-v"]).To(Equal(3), "landed: the hold no longer caps growth")
			Expect(g.Ledger().TimeoutHeld).To(BeEmpty())
		})

		It("stops pinning a lender release's unit once it landed, and marks its pods gone", func() {
			g := NewBorrowGovernor(settings)
			l, err := UnmarshalBorrowLedger(`{"entries":[{"cohort":"c","lender":"l","flavor":"h100","resource":"nvidia.com/gpu",` +
				`"releasedAt":"1970-01-01T05:33:20Z","units":[{"namespace":"b","variant":"m-v","to":3}]}]}`)
			Expect(err).NotTo(HaveOccurred())
			g.Restore(l)
			landed := []ModelScalingRequest{govModel("m", 3, 8)}
			ds := decisionsOf(landed)
			ds[0].TargetReplicas = 5
			g.HoldReleases(t0, landed, ds)
			Expect(targets(ds)["m-v"]).To(Equal(5))
			Expect(g.Ledger().Entries).To(HaveLen(1), "kept until a fresh read, once borrowing is counted again")
			Expect(g.Ledger().Entries[0].PodsGoneAt).NotTo(BeNil())
		})

		It("persists when cycles stopped counting borrowing, so a restart does not restart the window", func() {
			g := NewBorrowGovernor(settings)
			g.Restore(BorrowLedger{TimeoutHeld: map[string]int{"b/x-v": 1}})
			held := []ModelScalingRequest{govModel("x", 2, 8)}
			g.HoldReleases(t0, held, decisionsOf(held))
			raw, err := g.MarshalLedger()
			Expect(err).NotTo(HaveOccurred())
			l, err := UnmarshalBorrowLedger(raw)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.IdleSince).NotTo(BeNil())
			Expect(l.IdleSince.Equal(t0)).To(BeTrue())

			restarted := NewBorrowGovernor(settings)
			restarted.Restore(l)
			ds := decisionsOf(held)
			ds[0].TargetReplicas = 3
			restarted.HoldReleases(t0.Add(settings.Cooldown+settings.ReleaseActuation+time.Minute), held, ds)
			Expect(targets(ds)["x-v"]).To(Equal(3), "the window ran out across the restart: nothing pinned")
		})

		It("prunes expired cooldowns and clamps one past the setting", func() {
			g := NewBorrowGovernor(settings)
			g.MarkPreempted("b/old", t0)
			g.Restore(BorrowLedger{ModelCooldown: map[string]time.Time{"b/far": t0.Add(24 * time.Hour)}})
			now := t0.Add(settings.Cooldown + time.Minute)
			g.Observe(BorrowCycle{Now: now})
			l := g.Ledger()
			Expect(l.ModelCooldown).NotTo(HaveKey("b/old"))
			Expect(l.ModelCooldown["b/far"]).To(BeTemporally("<=", now.Add(settings.Cooldown+time.Minute)))
		})

		It("blocks borrowed growth in a namespace whose preemptions could not be read", func() {
			g := NewBorrowGovernor(settings)
			g.Observe(BorrowCycle{Now: t0, PreemptionUnknown: map[string]bool{"b": true}})
			Expect(g.BorrowBlocked("b", "m", "m-v", t0)).To(BeTrue())
			Expect(g.BorrowBlocked("other", "m", "m-v", t0)).To(BeFalse())
			g.Observe(BorrowCycle{Now: t0.Add(30 * time.Second)})
			Expect(g.BorrowBlocked("b", "m", "m-v", t0.Add(30*time.Second))).To(BeFalse())
		})
	})

	Describe("a waiting lender that cannot reclaim, against a P/D borrower", func() {
		warm := func(w govWorld, reqs []ModelScalingRequest) []BorrowRelease {
			g := NewBorrowGovernor(settings)
			var rel []BorrowRelease
			for _, d := range []time.Duration{0, 30 * time.Second, 61 * time.Second} {
				w.readAt = t0.Add(d)
				c := BorrowCycle{Now: t0.Add(d), Constraints: w.constraints(), Requests: reqs}
				g.Observe(c)
				rel = g.Releases(c, decisionsOf(reqs))
			}
			return rel
		}

		It("2P:3D, 8 borrowed: releases exactly one unit", func() {
			rel := warm(govWorld{lenders: []kueue.LenderState{never(8)}, g: 32, used: 40},
				[]ModelScalingRequest{pdFleet(pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 3})})
			Expect(rel).To(HaveLen(1))
			Expect(rel[0].Variant).To(Equal("d"))
			Expect(rel[0].To).To(Equal(2))
		})

		It("1P:2D, 8 borrowed: releases one decode, not nothing", func() {
			rel := warm(govWorld{lenders: []kueue.LenderState{never(8)}, g: 16, used: 24},
				[]ModelScalingRequest{pdFleet(pdRole{"p", "prefill", 1}, pdRole{"d", "decode", 2})})
			Expect(rel).To(HaveLen(1))
			Expect(rel[0].Variant).To(Equal("d"))
		})

		It("2P:4D, 32 borrowed over 16 guaranteed: at most one pair", func() {
			rel := warm(govWorld{lenders: []kueue.LenderState{never(32)}, g: 16, used: 48},
				[]ModelScalingRequest{pdFleet(pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 4})})
			Expect(rel).NotTo(BeEmpty())
			Expect(releasedGPUs(rel)).To(BeNumerically("<=", 16))
		})
	})
})
