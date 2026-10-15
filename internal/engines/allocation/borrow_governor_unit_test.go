package allocation

import (
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

// pdFleet is a P/D model "pd" in ns "b" on H100, every replica of gpr GPUs.
func pdFleet(gpr int, roles ...pdRole) ModelScalingRequest {
	var vcs []vcFixture
	var sts []domain.VariantReplicaState
	for _, r := range roles {
		vcs = append(vcs, vcFixture{VariantName: r.name, AcceleratorName: "H100", Cost: 5, Role: r.role, ReplicaCount: r.n, PerReplicaCapacity: 10000})
		sts = append(sts, domain.VariantReplicaState{VariantName: r.name, CurrentReplicas: r.n, GPUsPerReplica: gpr, Role: r.role, AcceleratorName: "H100"})
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

func releasedGPUs(rel []BorrowRelease, gpr int) int {
	n := 0
	for _, r := range rel {
		n += (r.From - r.To) * gpr
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

	Describe("placement timeout on a P/D model", func() {
		It("1P:2D, 8 GPUs borrowed, a decode unplaced: releases that decode and keeps the pair (d:1, not 0/0)", func() {
			pd := unplacedVariant(pdFleet(8, pdRole{"p", "prefill", 1}, pdRole{"d", "decode", 2}), "d", t0)
			rel, tg := timeout(16, 24, pd)
			Expect(tg).To(Equal(map[string]int{"p": 1, "d": 1}))
			Expect(releasedGPUs(rel, 8)).To(Equal(8))
		})

		It("1P:2D with minReplicas 1 on both roles still releases the unplaced borrowed decode", func() {
			pd := unplacedVariant(pdFleet(8, pdRole{"p", "prefill", 1}, pdRole{"d", "decode", 2}), "d", t0)
			one := 1
			for i := range pd.VariantStates {
				pd.VariantStates[i].MinReplicas = &one
			}
			_, tg := timeout(16, 24, pd)
			Expect(tg).To(Equal(map[string]int{"p": 1, "d": 1}))
		})

		It("2P:3D, 8 GPUs borrowed: releases one decode, not the model", func() {
			pd := unplacedVariant(pdFleet(8, pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 3}), "d", t0)
			rel, tg := timeout(32, 40, pd)
			Expect(tg).To(Equal(map[string]int{"p": 2, "d": 2}))
			Expect(releasedGPUs(rel, 8)).To(Equal(8))
		})

		It("2P:4D, 32 borrowed over 16 guaranteed: releases at most one pair", func() {
			pd := unplacedVariant(pdFleet(8, pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 4}), "d", t0)
			rel, tg := timeout(16, 48, pd)
			Expect(releasedGPUs(rel, 8)).To(BeNumerically(">", 0))
			Expect(releasedGPUs(rel, 8)).To(BeNumerically("<=", 16))
			Expect(tg["p"]).To(BeNumerically(">=", 1))
			Expect(tg["d"]).To(BeNumerically(">=", 1))
		})

		It("counts a role split over two variants, and leaves no prefill without a decode", func() {
			pd := unplacedVariant(pdFleet(8, pdRole{"p1", "prefill", 1}, pdRole{"p2", "prefill", 1}, pdRole{"d", "decode", 1}), "d", t0)
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
		Expect(tg["m1-v"] + tg["m2-v"]).To(Equal(2), "the namespace keeps its guaranteed 2")
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
			g.HoldReleases(t0, ds)
			Expect(targets(ds)).To(Equal(map[string]int{"m-v": 3, "x-v": 1}))

			later := t0.Add(settings.Cooldown + settings.ReleaseActuation + time.Minute)
			ds = decisionsOf(reqs)
			g.HoldReleases(later, ds)
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
			ds := decisionsOf([]ModelScalingRequest{govModel("x", 2, 8)})
			restarted.HoldReleases(now, ds)
			Expect(targets(ds)["x-v"]).To(Equal(1))
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
				[]ModelScalingRequest{pdFleet(8, pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 3})})
			Expect(rel).To(HaveLen(1))
			Expect(rel[0].Variant).To(Equal("d"))
			Expect(rel[0].To).To(Equal(2))
		})

		It("1P:2D, 8 borrowed: releases one decode, not nothing", func() {
			rel := warm(govWorld{lenders: []kueue.LenderState{never(8)}, g: 16, used: 24},
				[]ModelScalingRequest{pdFleet(8, pdRole{"p", "prefill", 1}, pdRole{"d", "decode", 2})})
			Expect(rel).To(HaveLen(1))
			Expect(rel[0].Variant).To(Equal("d"))
		})

		It("2P:4D, 32 borrowed over 16 guaranteed: at most one pair", func() {
			rel := warm(govWorld{lenders: []kueue.LenderState{never(32)}, g: 16, used: 48},
				[]ModelScalingRequest{pdFleet(8, pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 4})})
			Expect(rel).NotTo(BeEmpty())
			Expect(releasedGPUs(rel, 8)).To(BeNumerically("<=", 16))
		})
	})
})
