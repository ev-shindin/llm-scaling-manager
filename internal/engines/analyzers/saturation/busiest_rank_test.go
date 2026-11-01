package saturation

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Issue #150, the saturation guards. A pod of eight data-parallel ranks is
// priced on what the pod holds -- its ranks' queues summed, their utilization
// averaged -- but queueLengthThreshold and kvCacheThreshold describe ONE
// engine. So whether a replica is saturated is judged on its busiest rank,
// and a replica with a full rank is not released on its mean.
//
// The figures: eight ranks of 640,000 tokens (5,120,000 a pod), k1 = 0.8 of
// that = 4,096,000, a derived k2 of 384,000 per engine (3,072,000 a pod),
// queueLengthThreshold 5, scale-up 0.85, scale-down 0.70.

const (
	brRankKv   = 640_000
	brRanks    = 8
	brVariant  = "dp-v"
	brPodKv    = brRankKv * brRanks
	brPodK1    = brPodKv * 8 / 10
	brPodK2    = 384_000 * brRanks
	brScaleUp  = 0.85
	brScaleDn  = 0.70
	brQueueThr = 5
)

// brPod is one pod's ranks.
type brPod struct {
	name  string
	kv    []float64
	queue []int
}

func brUniform(name string, kv float64, q int) brPod {
	p := brPod{name: name, kv: make([]float64, brRanks), queue: make([]int, brRanks)}
	for i := range p.kv {
		p.kv[i], p.queue[i] = kv, q
	}
	return p
}

// brHot is a pod whose last rank is at hot with hotQ waiting, the rest at base.
func brHot(name string, base, hot float64, hotQ int) brPod {
	p := brUniform(name, base, 0)
	p.kv[brRanks-1], p.queue[brRanks-1] = hot, hotQ
	return p
}

// brRow is the pod as the collector reports it: the sum and the mean, the
// busiest rank, and the ranks counted.
func brRow(p brPod) domain.ReplicaMetrics {
	var kvSum, kvMax, rate float64
	qSum, qMax := 0, 0
	for i, k := range p.kv {
		kvSum += k
		kvMax = math.Max(kvMax, k)
		qSum += p.queue[i]
		qMax = max(qMax, p.queue[i])
		if k > 0 {
			rate++
		}
	}
	kv := kvSum / float64(len(p.kv))
	rm := makeReplicaMetrics(p.name, brVariant, int64(math.Round(kv*brPodKv)), brPodKv, qSum, 1000, 1000)
	rm.EngineRanks = len(p.kv)
	rm.MaxRankQueueLength = qMax
	rm.MaxRankKvCacheUsage = kvMax
	rm.KvUsageInstant = kv
	rm.RequestRate = rate
	rm.GenerationTokenRate = rate * 1000
	return rm
}

// brCycle is what one Analyze call decided for the variant.
type brCycle struct {
	perReplica, demand float64
	replicas           int
	busiestQueueLoad   float64
	k2                 []string
	hotRankHeld        bool
}

func (c brCycle) supply() float64 { return float64(c.replicas) * c.perReplica }

// spareReplicas is how many replicas the spare would release: SC over P, as
// the engine computes SC (applyUniversalThreshold).
func (c brCycle) spareReplicas() float64 {
	return math.Max(0, c.supply()-c.demand/brScaleDn) / c.perReplica
}

// needed is the replicas the demand needs at the scale-up threshold.
func (c brCycle) needed() float64 { return math.Ceil(c.demand / brScaleUp / c.perReplica) }

func brRun(t *testing.T, role string, cycles ...[]brPod) []brCycle {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", brVariant, capacity.Record{
		EngineParams: &capacity.EngineParams{EffectiveMaxBatchedTokens: 8192, MaxNumSeqs: 256},
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	out := make([]brCycle, 0, len(cycles))
	for _, pods := range cycles {
		ctx, logs := observedCtx(t)
		rows := make([]domain.ReplicaMetrics, 0, len(pods))
		for _, p := range pods {
			rows = append(rows, brRow(p))
		}
		res, err := a.Analyze(ctx, makeAnalyzerInput(rows, []domain.VariantReplicaState{{
			VariantName: brVariant, AcceleratorName: "H200", CurrentReplicas: len(rows), GPUsPerReplica: 8, Role: role,
		}}))
		require.NoError(t, err)
		c := brCycle{demand: res.TotalDemand, busiestQueueLoad: res.VariantBusiestQueueLoad[brVariant],
			hotRankHeld: logs.FilterMessage("hot-rank-hold").Len() > 0}
		for _, vc := range res.VariantCapacities {
			if vc.VariantName == brVariant {
				c.perReplica, c.replicas = vc.PerReplicaCapacity, vc.ReplicaCount
			}
		}
		for _, e := range logs.FilterMessage("k2-decision").All() {
			if f := e.ContextMap(); f["variant"] == brVariant {
				c.k2 = append(c.k2, fmt.Sprint(f["priority"]))
			}
		}
		out = append(out, c)
	}
	return out
}

func brRepeat(n int, pods ...brPod) [][]brPod {
	out := make([][]brPod, n)
	for i := range out {
		out[i] = pods
	}
	return out
}

var brRoles = []string{"", domain.RoleDecode}

// S1. One rank full and queued, seven idle. The rank is saturated and its
// reading is recorded -- but as one engine's saturation point scaled to the
// pod, 5,120,000, not as the 640,000 tokens the pod held. Recorded as 640,000,
// the window priced the pod at one rank's tokens for as long as it lived:
// once the load spread evenly at 0.3, the pod read as three replicas' worth
// where it is one.
func TestHotRankRecordsK2ScaledToThePod(t *testing.T) {
	for _, role := range brRoles {
		t.Run("role="+role, func(t *testing.T) {
			cycles := make([][]brPod, 0, 6)
			for _, q := range []int{5, 10, 20} {
				cycles = append(cycles, []brPod{brHot("pod-a", 0, 1.0, q)})
			}
			cycles = append(cycles, brRepeat(3, brUniform("pod-a", 0.3, 0))...)
			got := brRun(t, role, cycles...)

			// The window's first reading is held for the next
			// (k2_confirm.go): the first cycle is priced from the derived
			// figure.
			assert.Equal(t, []string{k2ReasonObsUnconfirmed, capacity.K2SrcDerived.String()}, got[0].k2,
				"cycle 0: the busiest rank is full and queued: a saturated reading, held")
			for i := 1; i < 3; i++ {
				assert.Equal(t, []string{capacity.K2SrcObserved.String()}, got[i].k2,
					"cycle %d: the busiest rank is full and queued: a saturated reading", i)
				assert.Equal(t, float64(brPodK1), got[i].perReplica,
					"cycle %d: k2 = 5,120,000 does not bind under k1", i)
			}
			after := got[len(got)-1]
			assert.Equal(t, []string{capacity.K2SrcHistorical.String()}, after.k2)
			assert.Equal(t, float64(brPodK1), after.perReplica,
				"the window holds 5,120,000, so k1 prices the pod; a k2 of one rank's 640,000 priced it at that")
			assert.Equal(t, 1.0, after.needed(), "the evenly loaded pod at 0.3 is one replica, not three")
		})
	}
}

// S3. Every rank at 0.3 with one request waiting: the queue sums to eight,
// over the threshold of five, but no engine has more than one waiting and
// none is full. Not a saturated reading. Recorded, it priced the pod at the
// 1,536,000 tokens it held -- half its derived capacity -- and ordered two
// replicas where one serves.
func TestQueueSpreadOverRanksIsNotSaturation(t *testing.T) {
	for _, role := range brRoles {
		t.Run("role="+role, func(t *testing.T) {
			cycles := brRepeat(4, brUniform("pod-a", 0.3, 1))
			cycles = append(cycles, brRepeat(3, brUniform("pod-a", 0.5, 0))...)
			got := brRun(t, role, cycles...)

			for i, c := range got {
				assert.Equal(t, []string{capacity.K2SrcDerived.String()}, c.k2,
					"cycle %d: one waiting per rank is not saturation; the chain reaches the derived k2", i)
				assert.Equal(t, float64(brPodK2), c.perReplica, "cycle %d: eight engines' derived k2", i)
			}
			wantLoad := 0.2 // the busiest engine's one waiting request of five, not the pod's eight
			if role == domain.RoleDecode {
				wantLoad = 0 // and decode counts a queue only on a full cache
			}
			assert.InDelta(t, wantLoad, got[0].busiestQueueLoad, 1e-12)
			assert.Equal(t, 1.0, got[len(got)-1].needed(), "the pod at 0.5 is one replica")
		})
	}
}

// S2. Two replicas. pod-a has one rank at its ceiling with three waiting
// (under the queue threshold) over ranks at 0.1; pod-b is at 0.1 on every
// rank. On their means the two read as 26% used, with more than a replica
// spare: one would be released, and it may be pod-a. A replica with a full
// rank is kept: only the other replicas' spare may go, and pod-b alone at 0.1
// does not spare a whole replica. The busiest queue the release guards read is
// pod-a's full rank's three of five.
func TestHotRankHoldsItsReplica(t *testing.T) {
	for _, role := range brRoles {
		t.Run("role="+role, func(t *testing.T) {
			got := brRun(t, role, []brPod{brHot("pod-a", 0.1, 1.0, 3), brUniform("pod-b", 0.1, 0)})[0]
			require.Equal(t, 2, got.replicas)
			require.Equal(t, float64(brPodK2), got.perReplica)
			assert.True(t, got.hotRankHeld, "the hold is logged")
			assert.Less(t, got.spareReplicas(), 1.0, "no replica's worth spare: pod-a's capacity is kept")
			// pod-b's 512,000 plus pod-a held at the scale-down boundary.
			assert.InDelta(t, 512_000+brScaleDn*brPodK2, got.demand, 1)
			assert.Equal(t, 2.0, got.needed(), "kept, not ordered: the hold stays under the scale-up threshold")
			assert.InDelta(t, 0.6, got.busiestQueueLoad, 1e-12,
				"decode too: the full rank's queue of three, as a fraction of five")
		})
	}
}

// S2 with more replicas, and the release the engine makes of them, are in
// steadystate (hot_rank_hold_test.go): the spare is the engine's to compute.

// The same pods with no full rank are not held: the hold is the full rank.
func TestNoHotRankNoHold(t *testing.T) {
	for _, role := range brRoles {
		t.Run("role="+role, func(t *testing.T) {
			got := brRun(t, role, []brPod{brHot("pod-a", 0.1, 0.5, 3), brUniform("pod-b", 0.1, 0)})[0]
			assert.False(t, got.hotRankHeld)
			assert.GreaterOrEqual(t, got.spareReplicas(), 1.0, "two pods near 0.1 spare a replica")
		})
	}
}

// A replica of ONE rank reads exactly as before: its own queue and KV are its
// busiest rank's, a full one among idle peers is not held (its demand already
// shows it), and the busiest-rank fields, whatever they hold, change nothing.
func TestSingleRankReplicaUnchangedByBusiestRank(t *testing.T) {
	for _, role := range brRoles {
		t.Run("role="+role, func(t *testing.T) {
			run := func(stray bool) brCycle {
				store := capacity.NewStore()
				store.Update("test-ns", "test-model", brVariant, capacity.Record{
					EngineParams: &capacity.EngineParams{EffectiveMaxBatchedTokens: 8192, MaxNumSeqs: 256},
				}, time.Now())
				a := NewSaturationAnalyzer(store)
				full := makeReplicaMetrics("pod-a", brVariant, brRankKv, brRankKv, 3, 1000, 1000)
				idle := makeReplicaMetrics("pod-b", brVariant, brRankKv/10, brRankKv, 0, 1000, 1000)
				idle2 := makeReplicaMetrics("pod-c", brVariant, brRankKv/10, brRankKv, 0, 1000, 1000)
				rows := []domain.ReplicaMetrics{full, idle, idle2}
				if stray {
					for i := range rows {
						rows[i].EngineRanks = 1
						rows[i].MaxRankQueueLength = 99
						rows[i].MaxRankKvCacheUsage = 0.01
					}
				}
				ctx, logs := observedCtx(t)
				res, err := a.Analyze(ctx, makeAnalyzerInput(rows, []domain.VariantReplicaState{{
					VariantName: brVariant, AcceleratorName: "H200", CurrentReplicas: 3, GPUsPerReplica: 1, Role: role,
				}}))
				require.NoError(t, err)
				c := brCycle{demand: res.TotalDemand, busiestQueueLoad: res.VariantBusiestQueueLoad[brVariant],
					hotRankHeld: logs.FilterMessage("hot-rank-hold").Len() > 0}
				for _, vc := range res.VariantCapacities {
					if vc.VariantName == brVariant {
						c.perReplica, c.replicas = vc.PerReplicaCapacity, vc.ReplicaCount
					}
				}
				return c
			}
			plain, stray := run(false), run(true)
			assert.False(t, plain.hotRankHeld, "one rank: no hold")
			assert.Equal(t, plain, stray, "the busiest-rank fields of a one-rank row are ignored")
			assert.InDelta(t, 0.6, plain.busiestQueueLoad, 1e-12, "its own queue of three")
		})
	}
}

// Disaggregated: a replica with a full rank is kept, decode's and, while
// decode is not saturated, prefill's. The raise is reported apart
// (HotRankDemand), never as a hold of the role (HeldRoleDemand): a held role
// releases nothing, and a role with a hot replica still releases the others.
func TestHotRankHoldOnPD(t *testing.T) {
	store := capacity.NewStore()
	for _, v := range []string{"dec", "pre"} {
		store.Update("test-ns", "test-model", v, capacity.Record{
			EngineParams: &capacity.EngineParams{EffectiveMaxBatchedTokens: 8192, MaxNumSeqs: 256},
		}, time.Now())
	}
	a := NewSaturationAnalyzer(store)
	rows := make([]domain.ReplicaMetrics, 0, 4)
	for _, v := range []string{"dec", "pre"} {
		for _, p := range []brPod{brHot(v+"-a", 0.1, 1.0, 3), brUniform(v+"-b", 0.1, 0)} {
			rm := brRow(p)
			rm.VariantName = v
			rows = append(rows, rm)
		}
	}
	ctx, logs := observedCtx(t)
	res, err := a.Analyze(ctx, makeAnalyzerInput(rows, []domain.VariantReplicaState{
		{VariantName: "dec", AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 8, Role: domain.RoleDecode},
		{VariantName: "pre", AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 8, Role: domain.RolePrefill},
	}))
	require.NoError(t, err)
	require.Equal(t, 1, logs.FilterMessage("hot-rank-hold").Len())
	assert.Empty(t, res.HeldRoleDemand, "no role is held")
	for _, role := range []string{domain.RoleDecode, domain.RolePrefill} {
		assert.Positive(t, res.HotRankDemand[role], "%s's hot replica is kept", role)
	}
}

// While decode is saturated, prefill's hot rank is decode's backlog, and
// holdPrefillDemand decides prefill: the hot-rank hold leaves it alone.
func TestHotRankHoldLeavesPrefillToThePrefillHold(t *testing.T) {
	store := capacity.NewStore()
	for _, v := range []string{"dec", "pre"} {
		store.Update("test-ns", "test-model", v, capacity.Record{
			EngineParams: &capacity.EngineParams{EffectiveMaxBatchedTokens: 8192, MaxNumSeqs: 256},
		}, time.Now())
	}
	a := NewSaturationAnalyzer(store)
	rows := make([]domain.ReplicaMetrics, 0, 4)
	for _, p := range []brPod{brHot("dec-a", 0.1, 1.0, brQueueThr), brUniform("dec-b", 0.1, 0)} {
		rm := brRow(p)
		rm.VariantName = "dec"
		rows = append(rows, rm)
	}
	for _, p := range []brPod{brHot("pre-a", 0.1, 1.0, 3), brUniform("pre-b", 0.1, 0)} {
		rm := brRow(p)
		rm.VariantName = "pre"
		rows = append(rows, rm)
	}
	ctx, _ := observedCtx(t)
	res, err := a.Analyze(ctx, makeAnalyzerInput(rows, []domain.VariantReplicaState{
		{VariantName: "dec", AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 8, Role: domain.RoleDecode},
		{VariantName: "pre", AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 8, Role: domain.RolePrefill},
	}))
	require.NoError(t, err)
	_, prefill := res.HotRankDemand[domain.RolePrefill]
	assert.False(t, prefill, "decode is saturated: prefill is the prefill hold's")
}
