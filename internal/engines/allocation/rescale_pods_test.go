package allocation

import (
	"cmp"
	"context"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// TestFillRoleExactFindsTheSum: on 6- and 4-GPU pods, +8 is two 4s; the
// greedy pass took the cheaper 6 and stopped.
func TestFillRoleExactFindsTheSum(t *testing.T) {
	req := podRequest(nil, []podVariant{
		{name: "d6", role: domain.RoleDecode, gpus: 6, cost: 5, perGPU: 100},
		{name: "d4", role: domain.RoleDecode, gpus: 4, cost: 4, perGPU: 100},
	})
	records := buildVariantRecords(req, req.CompositeSignal.Result)
	stateMap := buildStateMap(req.VariantStates)
	greedy := initTargets(req.VariantStates)
	assert.Equal(t, 6, fillRole(records, domain.RoleDecode, stateMap, greedy, 8))
	exact := initTargets(req.VariantStates)
	assert.Equal(t, 8, fillRoleExact(records, domain.RoleDecode, stateMap, exact, 8))
	assert.Equal(t, map[string]int{"d6": 0, "d4": 2}, exact)
	exact = initTargets(req.VariantStates)
	assert.Equal(t, 10, fillRoleExact(records, domain.RoleDecode, stateMap, exact, 11))
	assert.Equal(t, map[string]int{"d6": 1, "d4": 1}, exact, "the cheaper pod first, as greedy prefers it")
}

// TestPodSearchPrimitives pins the set arithmetic the pod search runs on.
func TestPodSearchPrimitives(t *testing.T) {
	b := zeroPodBits(200)
	b.orShifted(b, 70)
	b.orShifted(b, 3)
	for _, i := range []int{0, 3, 70, 73} {
		assert.True(t, b.has(i), "%d", i)
	}
	assert.False(t, b.has(4))
	assert.Equal(t, 73, b.maxAtMost(200))
	assert.Equal(t, 70, b.maxAtMost(72))
	assert.Equal(t, 3, b.maxAtMost(69))
	assert.Equal(t, 0, b.maxAtMost(2))

	items := []podItem{{name: "a", size: 2, count: 2}, {name: "b", size: 4, count: 3}}
	sums := podSums(items, 100)
	for s := 0; s <= 17; s++ {
		assert.Equal(t, s%2 == 0 && s <= 16, sums.has(s), "%d", s)
	}
	assert.Equal(t, []int{2, 2}, composePods(items, 12), "the first item first")
	assert.Equal(t, []int{1, 3}, composePods(items, 14))

	// The issue's fill: decode {0,2,4,6} under its cap of 6, prefill {0,8,16};
	// a growth of 9 is one prefill pod.
	dec, pre := podSums([]podItem{{size: 2, count: 1}, {size: 4, count: 9}}, 6), podSums([]podItem{{size: 8, count: 2}}, 16)
	assert.Equal(t, []int{0, 8}, jointPods([]podBits{dec, pre}, []int{6, 16}, 9, []int{2, 7}))
	assert.Equal(t, []int{2, 8}, jointPods([]podBits{dec, pre}, []int{6, 16}, 10, []int{2, 8}))
}

// TestPodSearchIsBounded: a role on 1- and 2-GPU pods wanting more than
// the search bound grows by the bound this cycle, and at the bound's cost.
func TestPodSearchIsBounded(t *testing.T) {
	req := podRequest(nil, []podVariant{
		{name: "d1", role: domain.RoleDecode, gpus: 1, cost: 1, perGPU: 100},
		{name: "d2", role: domain.RoleDecode, gpus: 2, cost: 2.2, perGPU: 100},
	})
	records := buildVariantRecords(req, req.CompositeSignal.Result)
	targets := initTargets(req.VariantStates)
	assert.Equal(t, podSearchMaxUnits, fillRoleExact(records, domain.RoleDecode, buildStateMap(req.VariantStates), targets, 1<<30))
}

// legacyRoundFillsToReplicas is roundFillsToReplicas before #219, the oracle
// a model of uniform replica sizes must still match.
func legacyRoundFillsToReplicas(records []variantRecord, stateMap map[string]domain.VariantReplicaState,
	roles []string, tgt, cur, dem, targets map[string]int) {
	trial := maps.Clone(targets)
	cut := make(map[string]int, len(roles))
	leftover := 0
	for _, role := range roles {
		want := tgt[role] - cur[role]
		if want <= 0 {
			continue
		}
		placed := fillRole(records, role, stateMap, trial, want)
		cut[role] = want - placed
		tgt[role] -= cut[role]
		leftover += cut[role]
	}
	order := slices.Clone(roles)
	slices.SortStableFunc(order, func(a, b string) int { return cmp.Compare(cut[b], cut[a]) })
	for _, role := range order {
		if leftover <= 0 {
			break
		}
		limit := max(dem[role]-tgt[role], ceilToReplica(cut[role], roleReplicaStepGPUs(records, role, stateMap)))
		extra := fillRole(records, role, stateMap, trial, min(leftover, limit))
		tgt[role] += extra
		leftover -= extra
	}
}

// legacyReclaimWholeReplicas is reclaimWholeReplicas before #219.
func legacyReclaimWholeReplicas(ctx context.Context, e NamedAnalyzerResult, records []variantRecord,
	stateMap map[string]domain.VariantReplicaState, roles []string, tgt, cur, targets map[string]int) {
	cut := make(map[string]int, len(roles))
	total := 0
	trialCtx := ctrl.LoggerInto(ctx, logr.Discard())
	for _, role := range roles {
		if tgt[role] < cur[role] && !e.Holds[role].NoScaleDown {
			cut[role] = cur[role] - tgt[role]
			keep := 0
			if tgt[role] > 0 {
				keep = max(roleReplicaStepGPUs(records, role, stateMap), 1)
			}
			can := reclaimRole(trialCtx, e, records, role, stateMap, maps.Clone(targets), max(cur[role]-keep, 0))
			total += min(cut[role], can)
		}
	}
	if total == 0 {
		return
	}
	shed := make(map[string]int, len(roles))
	for _, role := range roles {
		if cut[role] > 0 {
			shed[role] = reclaimRole(ctx, e, records, role, stateMap, targets, cut[role])
			total -= shed[role]
		}
	}
	for _, role := range roles {
		if cut[role] > 0 && !uniformReplicaGPUs(records, role, stateMap) {
			return
		}
	}
	order := slices.Clone(roles)
	slices.SortStableFunc(order, func(a, b string) int {
		return cmp.Compare(cut[b]-shed[b], cut[a]-shed[a])
	})
	for _, role := range order {
		left := cut[role] - shed[role]
		if total <= 0 || left <= 0 {
			continue
		}
		limit := ceilToReplica(left, roleReplicaStepGPUs(records, role, stateMap))
		n := reclaimRole(ctx, e, records, role, stateMap, targets, min(total, limit))
		shed[role] += n
		total -= n
	}
}

// TestRescaleUniformSizesUnchanged: with one replica size across the model
// (1-GPU pods through whole-node and two-node pods) the rounding is the
// pre-#219 one, output for output, over a grid of shares, holds and floors.
func TestRescaleUniformSizesUnchanged(t *testing.T) {
	r := rand.New(rand.NewPCG(219, 2))
	ctx := context.Background()
	compared := 0
	for range 1500 {
		req := randomPodModel(r, false, false)
		records := buildVariantRecords(req, req.CompositeSignal.Result)
		stateMap := buildStateMap(req.VariantStates)
		roles := modelRolesOnType(records, "A100")
		require.True(t, uniformPods(records, stateMap))
		e := req.CompositeSignal
		if r.IntN(4) == 0 {
			e.Holds = map[string]RoleHold{roles[r.IntN(len(roles))]: {NoScaleDown: true}}
		}
		cur, dem := map[string]int{}, map[string]int{}
		for _, role := range roles {
			cur[role] = roleCurrentGPUs(records, stateMap, "A100", role)
			dem[role] = roleDemandGPUs(e, records, stateMap, "A100", role)
		}
		for range 20 {
			tgt := map[string]int{}
			for _, role := range roles {
				tgt[role] = max(cur[role]+r.IntN(49)-24, 0)
			}
			oldTgt, newTgt := maps.Clone(tgt), maps.Clone(tgt)
			oldT, newT := initTargets(req.VariantStates), initTargets(req.VariantStates)
			legacyRoundFillsToReplicas(records, stateMap, roles, oldTgt, cur, dem, oldT)
			roundFillsToReplicas(records, stateMap, roles, newTgt, cur, dem, newT)
			require.Equal(t, oldTgt, newTgt, "fill shares, %v from %v", tgt, cur)
			require.Equal(t, oldT, newT)
			legacyReclaimWholeReplicas(ctx, e, records, stateMap, roles, oldTgt, cur, oldT)
			reclaimWholeReplicas(ctx, e, records, stateMap, roles, newTgt, cur, newT)
			require.Equal(t, oldT, newT, "reclaim, %v from %v", tgt, cur)
			for _, role := range roles {
				if want := newTgt[role] - cur[role]; want > 0 {
					a, b := maps.Clone(newT), maps.Clone(newT)
					require.Equal(t, fillRole(records, role, stateMap, a, want), fillRoleExact(records, role, stateMap, b, want))
					require.Equal(t, a, b)
				}
			}
			compared++
		}
	}
	assert.Equal(t, 30000, compared)
}

// BenchmarkPodSearchAtTheBound times the joint search at its worst case:
// two roles of 1- and 2-GPU pods, every bound at podSearchMaxUnits.
func BenchmarkPodSearchAtTheBound(b *testing.B) {
	items := []podItem{{size: 1, count: 1 << 20}, {size: 2, count: 1 << 20}}
	sets := []podBits{podSums(items, podSearchMaxUnits), podSums(items, podSearchMaxUnits)}
	caps := []int{podSearchMaxUnits, podSearchMaxUnits}
	for b.Loop() {
		jointPods(sets, caps, podSearchMaxUnits, []int{podSearchMaxUnits / 2, podSearchMaxUnits / 2})
	}
}
