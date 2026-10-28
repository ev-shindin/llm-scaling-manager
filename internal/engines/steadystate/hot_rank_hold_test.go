package steadystate

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Issue #150, the hot-rank hold, through the engine: the saturation analyzer
// prices the pods, and buildNamedResult turns its demand into the required
// and spare capacity the optimizer acts on. A replica whose busiest engine
// rank is full keeps its capacity; the other replicas' spare is still
// released.
//
// The figures: eight ranks of 640,000 tokens a pod, k1 = 0.8, a derived
// decode capacity of 3,072,000 a pod, prefill priced at k1 = 4,096,000,
// scale-up 0.85, scale-down 0.70. A pod at 0.1 on every rank holds 512,000.
// A hot replica is held at 0.70 x its capacity, so the spare the engine
// reads is the spare of the other replicas alone: two pods at 0.1 on
// 3,072,000 spare 1.524 replicas, four spare 3.048.

const (
	hrRankKv    = 640_000
	hrRanks     = 8
	hrScaleUp   = 0.85
	hrScaleDown = 0.70
)

// hrPod is one pod of eight ranks at base, its last rank at hot with
// hotQueue waiting, as the collector reports it.
func hrPod(name, variant string, base, hot float64, hotQueue int) domain.ReplicaMetrics {
	podKv := int64(hrRankKv * hrRanks)
	mean := (base*(hrRanks-1) + hot) / hrRanks
	return domain.ReplicaMetrics{
		PodName: name, VariantName: variant, ModelID: "test-model", Namespace: "test-ns", Ready: true,
		KvCacheUsage: mean, KvUsageInstant: mean, QueueLength: hotQueue,
		NumGpuBlocks: podKv / 16, BlockSize: 16, TotalKvCapacityTokens: podKv,
		TokensInUse: int64(math.Round(mean * float64(podKv))), AvgInputTokens: 1000, AvgOutputTokens: 1000,
		EngineRanks: hrRanks, MaxRankQueueLength: hotQueue, MaxRankKvCacheUsage: math.Max(base, hot),
		RequestRate: hrRanks, GenerationTokenRate: hrRanks * 1000,
	}
}

// hrIdle is a pod at 0.1 on every rank.
func hrIdle(name, variant string) domain.ReplicaMetrics { return hrPod(name, variant, 0.1, 0.1, 0) }

// hrVariant is one variant's pods, and its scale target's replica count
// (the pods reporting, unless a scale-down is in flight).
type hrVariant struct {
	name, role string
	target     int
	rows       []domain.ReplicaMetrics
}

// hrEngine is one saturation analyzer, kept across cycles.
type hrEngine struct {
	a *saturation.SaturationAnalyzer
}

func newHREngine(variants ...string) *hrEngine {
	store := capacity.NewStore()
	for _, v := range variants {
		store.Update("test-ns", "test-model", v, capacity.Record{
			EngineParams: &capacity.EngineParams{EffectiveMaxBatchedTokens: 8192, MaxNumSeqs: 256},
		}, time.Now())
	}
	return &hrEngine{a: saturation.NewSaturationAnalyzer(store)}
}

// hrCycle is what one cycle decided, in replicas of each role's capacity.
type hrCycle struct {
	res              *domain.AnalyzerResult
	nr               allocation.NamedAnalyzerResult
	spare, required  map[string]float64
	modelRequiredCap float64
}

// cycle runs the analyzer and the engine's capacity build on vars. The
// analyzer is given the scale target's count, as the engine gives it
// (VariantMetadata.ToReplicaState).
func (e *hrEngine) cycle(t *testing.T, vars ...hrVariant) hrCycle {
	t.Helper()
	n := 0
	for _, v := range vars {
		n += len(v.rows)
	}
	rows := make([]domain.ReplicaMetrics, 0, n)
	meta := map[string]domain.VariantMetadata{}
	for _, v := range vars {
		rows = append(rows, v.rows...)
		role := v.role
		if role == "" {
			role = domain.RoleBoth
		}
		meta[v.name] = domain.VariantMetadata{VariantName: v.name, ModelID: "test-model", Namespace: "test-ns",
			Role: role, CurrentReplicas: v.target, GPUsPerReplica: hrRanks, AcceleratorName: "H200"}
	}
	states := make([]domain.VariantReplicaState, 0, len(vars))
	for _, v := range vars {
		s := meta[v.name].ToReplicaState()
		s.Role = v.role
		states = append(states, s)
	}
	in := domain.AnalyzerInput{ModelID: "test-model", Namespace: "test-ns", ReplicaMetrics: rows, VariantStates: states,
		Config: &config.ScalingPolicy{KvCacheThreshold: 0.8, QueueLengthThreshold: 5, AnalyzerName: "saturation",
			ScaleUpThreshold: hrScaleUp, ScaleDownBoundary: hrScaleDown}}
	ctx := logr.NewContext(context.Background(), logr.Discard())
	res, err := e.a.Analyze(ctx, in)
	require.NoError(t, err)
	nr := buildNamedResult(ctx, domain.SaturationAnalyzerName, res, config.ScalingPolicy{}, meta, hrScaleUp, hrScaleDown)

	per := map[string]float64{}
	for _, vc := range res.VariantCapacities {
		per[domain.RoleOfVC(vc)] = vc.PerReplicaCapacity
	}
	c := hrCycle{res: res, nr: nr, spare: map[string]float64{}, required: map[string]float64{},
		modelRequiredCap: nr.RequiredCapacity}
	if nr.RoleCapacities == nil {
		p := per[domain.RoleBoth]
		require.Positive(t, p)
		c.spare[domain.RoleBoth] = nr.SpareCapacity / p
		c.required[domain.RoleBoth] = nr.RequiredCapacity / p
		return c
	}
	for role, rc := range nr.RoleCapacities {
		p := per[role]
		require.Positive(t, p, "role %s is priced", role)
		c.spare[role] = rc.SpareCapacity / p
		c.required[role] = rc.RequiredCapacity / p
	}
	return c
}

// hrIdlePrefill is a P/D model's prefill: two pods at 0.1.
func hrIdlePrefill() hrVariant {
	return hrVariant{name: "pre", role: domain.RolePrefill, target: 2,
		rows: []domain.ReplicaMetrics{hrIdle("p-a", "pre"), hrIdle("p-b", "pre")}}
}

// hrHotFleet is n pods of one variant, the first with its busiest rank at
// hot and three waiting, the rest at 0.1.
func hrHotFleet(variant, role string, n int, hot float64) hrVariant {
	rows := []domain.ReplicaMetrics{hrPod(variant+"-a", variant, 0.1, hot, 3)}
	for i := 1; i < n; i++ {
		rows = append(rows, hrIdle(variant+"-"+string(rune('a'+i)), variant))
	}
	return hrVariant{name: variant, role: role, target: n, rows: rows}
}

// hrRoles is the two ways a decoding variant runs: aggregated, and as the
// decode role of a P/D model.
var hrRoles = []string{"", domain.RoleDecode}

// hrRun is one cycle of a fleet of one decoding variant (plus an idle
// prefill on P/D) on a fresh engine.
func hrRun(t *testing.T, role string, dec hrVariant) hrCycle {
	if role == "" {
		return newHREngine(dec.name).cycle(t, dec)
	}
	return newHREngine(dec.name, "pre").cycle(t, dec, hrIdlePrefill())
}

func hrKey(role string) string { return domain.CanonicalRole(role) }

// S2, three replicas. The hold keeps the replica with the full rank, not the
// fleet: the two idle replicas' spare, 1.524 replicas, is released. On P/D the
// hold used to be reported as a hold of the role (HeldRoleDemand), and the
// engine zeroes a held role's spare: decode released nothing.
func TestHotRankHoldReleasesTheOthers(t *testing.T) {
	for _, role := range hrRoles {
		t.Run("role="+role, func(t *testing.T) {
			got := hrRun(t, role, hrHotFleet("dec", role, 3, 1.0))
			k := hrKey(role)
			assert.InDelta(t, 1.524, got.spare[k], 0.001, "two idle replicas' spare; the hot one's is kept")
			assert.Zero(t, got.required[k], "kept, not ordered")
			_, held := got.res.HeldRoleDemand[k]
			assert.False(t, held, "a replica kept is not a role held")
		})
	}
}

// Five replicas, one hot, four idle: the four idle ones spare 3.048 replicas.
func TestHotRankHoldReleasesFourIdleReplicasSpare(t *testing.T) {
	for _, role := range hrRoles {
		t.Run("role="+role, func(t *testing.T) {
			got := hrRun(t, role, hrHotFleet("dec", role, 5, 1.0))
			assert.InDelta(t, 3.048, got.spare[hrKey(role)], 0.001)
			assert.Zero(t, got.required[hrKey(role)])
		})
	}
}

// A scale-down from three to two is in flight: the pod being removed still
// reports, and the analyzer prices three, but the engine counts the scale
// target's two. The hold's room was the analyzer's three; on the engine's
// two it ordered 0.392 of a replica, rounded up to one. The hold now never
// raises demand over what the engine's supply carries.
func TestHotRankHoldNeverOrdersWhileAScaleDownIsInFlight(t *testing.T) {
	for _, role := range hrRoles {
		t.Run("role="+role, func(t *testing.T) {
			dec := hrVariant{name: "dec", role: role, target: 2, rows: []domain.ReplicaMetrics{
				hrPod("dec-a", "dec", 0.1, 1.0, 3), hrPod("dec-b", "dec", 0.4, 0.4, 0), hrPod("dec-c", "dec", 0.4, 0.4, 0),
			}}
			got := hrRun(t, role, dec)
			assert.Zero(t, got.required[hrKey(role)], "the hold orders nothing")
			assert.Zero(t, got.modelRequiredCap, "nor does the model")
		})
	}
}

// The busiest rank hovering at k1 = 0.8 -- 0.81, 0.79, 0.81 -- does not turn
// the hold on and off every cycle: once held, a replica is let go when its
// busiest rank falls below 0.75. One that was never held is not held at
// 0.79.
func TestHotRankHoldDoesNotFlapAroundK1(t *testing.T) {
	for _, role := range hrRoles {
		t.Run("role="+role, func(t *testing.T) {
			names := []string{"dec"}
			if role != "" {
				names = append(names, "pre")
			}
			e := newHREngine(names...)
			cycle := func(hot float64) float64 {
				vars := []hrVariant{hrHotFleet("dec", role, 5, hot)}
				if role != "" {
					vars = append(vars, hrIdlePrefill())
				}
				return e.cycle(t, vars...).spare[hrKey(role)]
			}
			free := cycle(0.79)
			assert.Greater(t, free, 3.5, "never held: 0.79 is under k1")
			for i, hot := range []float64{0.81, 0.79, 0.81, 0.79, 0.76} {
				assert.InDelta(t, 3.048, cycle(hot), 0.001, "cycle %d at %.2f: held", i, hot)
			}
			assert.InDelta(t, free, cycle(0.74), 0.05, "below 0.75 the replica is let go")
			assert.Greater(t, cycle(0.79), 3.5, "and is not held again until it reaches k1")
		})
	}
}

// Prefill. While decode is not saturated, a full prefill rank is a prefill
// engine that cannot take the next prompt, and its replica is kept as
// decode's is: the two idle prefill replicas' spare is released, the hot
// one's is not.
func TestHotRankHoldKeepsAPrefillReplicaWhileDecodeIsNotSaturated(t *testing.T) {
	e := newHREngine("dec", "pre")
	dec := hrVariant{name: "dec", role: domain.RoleDecode, target: 2,
		rows: []domain.ReplicaMetrics{hrIdle("d-a", "dec"), hrIdle("d-b", "dec")}}
	pre := hrHotFleet("pre", domain.RolePrefill, 3, 1.0)
	got := e.cycle(t, dec, pre)
	idle := newHREngine("dec", "pre").cycle(t, dec, hrVariant{name: "pre", role: domain.RolePrefill, target: 2,
		rows: []domain.ReplicaMetrics{hrIdle("pre-b", "pre"), hrIdle("pre-c", "pre")}})
	assert.InDelta(t, idle.spare[domain.RolePrefill], got.spare[domain.RolePrefill], 0.001,
		"the spare is the two idle prefill replicas' alone")
	assert.Positive(t, got.spare[domain.RolePrefill])
	assert.Zero(t, got.required[domain.RolePrefill])
	_, held := got.res.HeldRoleDemand[domain.RolePrefill]
	assert.False(t, held)
}
