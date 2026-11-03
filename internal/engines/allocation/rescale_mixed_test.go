package allocation

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// podVariant is one variant of a model built by podRequest.
type podVariant struct {
	name, role    string
	gpus, cur     int
	minR, maxR    int // 0 is unset
	cost, perGPU  float64
	noCapacityRdg bool
}

// podRequest is a model on A100 with variants vs and per-role demand dem,
// disaggregated unless every variant is RoleBoth.
func podRequest(dem map[string]float64, vs []podVariant) ModelScalingRequest {
	req := ModelScalingRequest{ModelID: "m", Namespace: "ns", Priority: 1}
	res := &domain.AnalyzerResult{ModelID: "m", Namespace: "ns", RoleDemand: map[string]float64{}}
	rcs := map[string]domain.RoleCapacity{}
	for _, v := range vs {
		prc := v.perGPU * float64(v.gpus)
		if v.noCapacityRdg {
			prc = 0
		}
		res.VariantCapacities = append(res.VariantCapacities, domain.VariantCapacity{
			VariantName: v.name, Role: v.role, ReplicaCount: v.cur, PerReplicaCapacity: prc})
		st := domain.VariantReplicaState{VariantName: v.name, Role: v.role, CurrentReplicas: v.cur, GPUsPerReplica: v.gpus}
		if v.minR > 0 {
			st.MinReplicas = intPtr(v.minR)
		}
		if v.maxR > 0 {
			st.MaxReplicas = intPtr(v.maxR)
		}
		req.VariantStates = append(req.VariantStates, st)
		req.Variants = append(req.Variants, domain.VariantMetadata{
			VariantName: v.name, Role: v.role, AcceleratorName: "A100", Cost: v.cost})
		rc := rcs[v.role]
		rc.Role, rc.TotalSupply = v.role, rc.TotalSupply+prc*float64(v.cur)
		rcs[v.role] = rc
		req.Disaggregated = req.Disaggregated || v.role != domain.RoleBoth
	}
	for role, d := range dem {
		res.RoleDemand[role] = d
		rc := rcs[role]
		rc.TotalDemand = d
		rcs[role] = rc
		res.TotalDemand += d
	}
	req.CompositeSignal = NamedAnalyzerResult{Name: CompositeSignalName, Live: true, Result: res, RoleCapacities: rcs}
	return req
}

// podGPUs is the GPUs a model ends with at targetGPUs, with free GPUs to spare.
func podGPUs(req ModelScalingRequest, targetGPUs int) int {
	got := rescaleTargets(req, targetGPUs, 1<<20)
	total := 0
	for _, st := range req.VariantStates {
		n, ok := got[st.VariantName]
		if !ok {
			n = st.CurrentReplicas
		}
		total += n * st.GPUsPerReplica
	}
	return total
}

// podFreedGPUs is the GPUs of the pods a model sheds at targetGPUs.
func podFreedGPUs(req ModelScalingRequest, targetGPUs int) int {
	got := rescaleTargets(req, targetGPUs, 1<<20)
	freed := 0
	for _, st := range req.VariantStates {
		if n, ok := got[st.VariantName]; ok && n < st.CurrentReplicas {
			freed += (st.CurrentReplicas - n) * st.GPUsPerReplica
		}
	}
	return freed
}

func podCurrentGPUs(req ModelScalingRequest) int {
	total := 0
	for _, st := range req.VariantStates {
		total += st.CurrentReplicas * st.GPUsPerReplica
	}
	return total
}

// TestRescaleMixedFillIsMonotone is the fill case of #219. Prefill has an
// 8-GPU variant at no pods and demand for two; decode a 2-GPU variant at one
// pod (max 2) and a 4-GPU variant, demand for 8 GPUs. Greedy, a target of 10
// placed 10 GPUs and 11 placed 4: decode's +2 fitted its share, and the 7
// prefill had left was short of a pod.
func TestRescaleMixedFillIsMonotone(t *testing.T) {
	req := podRequest(map[string]float64{domain.RolePrefill: 1600, domain.RoleDecode: 800}, []podVariant{
		{name: "p8", role: domain.RolePrefill, gpus: 8, cost: 8, perGPU: 100},
		{name: "d2", role: domain.RoleDecode, gpus: 2, cur: 1, maxR: 2, cost: 2, perGPU: 100},
		{name: "d4", role: domain.RoleDecode, gpus: 4, cost: 4.4, perGPU: 100},
	})
	at10, at11 := podGPUs(req, 10), podGPUs(req, 11)
	assert.Equal(t, 10, at10)
	assert.GreaterOrEqual(t, at11, at10, "a larger target places no fewer GPUs")
	for target := range 30 {
		assert.LessOrEqual(t, podGPUs(req, target), podGPUs(req, target+1), "target %d", target)
	}
}

// TestRescaleMixedReclaimFreesAPod is the reclaim case of #219. Prefill holds
// an 8- and a 4-GPU pod, decode two 8-GPU pods. Rounded per role, two cuts
// each short of a pod shed nothing, so the model kept GPUs the water-fill gave
// another model; summed, a cut of at least one pod frees one.
func TestRescaleMixedReclaimFreesAPod(t *testing.T) {
	req := podRequest(map[string]float64{domain.RolePrefill: 1000, domain.RoleDecode: 1000}, []podVariant{
		{name: "p8", role: domain.RolePrefill, gpus: 8, cur: 1, cost: 8, perGPU: 25},
		{name: "p4", role: domain.RolePrefill, gpus: 4, cur: 1, cost: 4, perGPU: 25},
		{name: "d8", role: domain.RoleDecode, gpus: 8, cur: 2, cost: 8, perGPU: 25},
	})
	cur := podCurrentGPUs(req)
	require.Equal(t, 28, cur)
	for target := 0; target <= cur-8; target++ {
		assert.Less(t, podGPUs(req, target), cur, "target %d: a cut of a pod or more frees one", target)
	}
	for target := 16; target < 28; target++ {
		assert.LessOrEqual(t, podGPUs(req, target), podGPUs(req, target+1), "target %d", target)
	}
}

// mixedSizes are per-role replica-size sets the grids draw from, whole-node
// 8-GPU pods and two-node 16-GPU pods among them.
var mixedSizes = [][]int{{8}, {4, 8}, {2, 8}, {8, 16}, {2, 4}, {1, 2}, {4}, {2, 4, 8}, {1, 8}, {6, 4}}

// randomPodModel draws a model whose roles take sizes from mixedSizes. With
// mixed, the variants that can be sized take more than one size, in one role
// or across roles; without, they all take one. With strict, no
// variant has a minimum or lacks a capacity reading and every role holds at
// least two pods, so every role can give a pod.
func randomPodModel(r *rand.Rand, mixed, strict bool) ModelScalingRequest {
	for {
		roles := []string{domain.RolePrefill, domain.RoleDecode}
		if r.IntN(4) == 0 {
			roles = []string{domain.RoleBoth}
		}
		vs := make([]podVariant, 0, 6)
		dem := map[string]float64{}
		sized := map[int]bool{}
		ok := true
		one := []int{1, 2, 4, 8, 16}[r.IntN(5)]
		for _, role := range roles {
			sizes := mixedSizes[r.IntN(len(mixedSizes))]
			if !mixed {
				sizes = []int{one}
				if r.IntN(2) == 0 {
					sizes = []int{one, one}
				}
			}
			pods := 0
			for i, g := range sizes {
				v := podVariant{
					name: fmt.Sprintf("%s-%d-%d", role, g, i), role: role, gpus: g,
					cur: r.IntN(4), cost: float64(g) * (1 + r.Float64()/2), perGPU: 50 + r.Float64()*100,
				}
				if r.IntN(3) == 0 {
					v.maxR = v.cur + r.IntN(3)
				}
				if !strict && r.IntN(5) == 0 {
					v.minR = min(v.cur, 1)
				}
				if !strict && r.IntN(12) == 0 {
					v.noCapacityRdg = true
				}
				if !v.noCapacityRdg {
					sized[v.gpus] = true
				}
				pods += v.cur
				vs = append(vs, v)
			}
			if strict && pods < 2 {
				ok = false
			}
			dem[role] = float64(r.IntN(30)) * 100
		}
		if ok && (len(sized) > 1) == mixed {
			return podRequest(dem, vs)
		}
	}
}

// TestRescaleMixedPodsGrid is #219's property over a grid of mixed replica
// sizes: a larger target never leaves a model fewer GPUs, and with every role
// able to give a pod, a target at least the largest pod below what the model
// holds frees one, though a role the split grows may take as many back. On
// the per-role rounding and greedy fill it replaces, both failed.
func TestRescaleMixedPodsGrid(t *testing.T) {
	r := rand.New(rand.NewPCG(219, 1))
	monotone, frees := 0, 0
	for range 1000 {
		req := randomPodModel(r, true, false)
		cur := podCurrentGPUs(req)
		prev := podGPUs(req, 0)
		for target := 1; target <= cur+40; target++ {
			got := podGPUs(req, target)
			if !assert.LessOrEqual(t, prev, got, "target %d leaves fewer GPUs than %d: %s", target, target-1, describePodModel(req, target-1, target)) {
				return
			}
			prev = got
			monotone++
		}
	}
	for range 1000 {
		req := randomPodModel(r, true, true)
		cur := podCurrentGPUs(req)
		largest := slices.MaxFunc(req.VariantStates, func(a, b domain.VariantReplicaState) int {
			return cmp.Compare(a.GPUsPerReplica, b.GPUsPerReplica)
		}).GPUsPerReplica
		for target := 0; target <= cur-largest; target++ {
			if !assert.Positive(t, podFreedGPUs(req, target), "target %d of %d frees no pod: %s", target, cur, describePodModel(req, target)) {
				return
			}
			frees++
		}
	}
	t.Logf("checked %d monotone steps and %d cuts of a pod or more", monotone, frees)
	assert.Greater(t, monotone, 60000)
	assert.Greater(t, frees, 20000)
}

// TestRescaleMixedHoldsAModelAtItsTarget: a model at its target keeps its
// pods. Prefill on two 8-GPU pods and decode on four 4-GPU pods, demand for
// 30 and 18 GPUs, hold 32 and are given 32: split 20 and 12, prefill cannot
// place its +4 as a pod, and decode was cut by 4. Rounded without netting,
// decode shed a pod, 32 went to 28, and the next cycle took it back.
func TestRescaleMixedHoldsAModelAtItsTarget(t *testing.T) {
	for _, decode := range [][]podVariant{
		{{name: "d4", role: domain.RoleDecode, gpus: 4, cur: 4, cost: 4, perGPU: 100}},
		{{name: "d4", role: domain.RoleDecode, gpus: 4, cur: 2, cost: 4, perGPU: 100},
			{name: "d8", role: domain.RoleDecode, gpus: 8, cur: 1, cost: 8.5, perGPU: 100}},
	} {
		req := podRequest(map[string]float64{domain.RolePrefill: 3000, domain.RoleDecode: 1800},
			append([]podVariant{{name: "p8", role: domain.RolePrefill, gpus: 8, cur: 2, cost: 8, perGPU: 100}}, decode...))
		require.Equal(t, 32, podCurrentGPUs(req))
		assert.Equal(t, 32, podGPUs(req, 32), "at its target the model keeps its pods: %s", describePodModel(req, 32))
		assert.Zero(t, podFreedGPUs(req, 32), "no pod is shed")
	}
}

// podApplied is req after a cycle at targetGPUs: each variant at the
// replicas that cycle set.
func podApplied(req ModelScalingRequest, targetGPUs int) ModelScalingRequest {
	got := rescaleTargets(req, targetGPUs, 1<<20)
	next := req
	next.VariantStates = slices.Clone(req.VariantStates)
	for i, st := range next.VariantStates {
		if n, ok := got[st.VariantName]; ok {
			next.VariantStates[i].CurrentReplicas = n
		}
	}
	res := *req.CompositeSignal.Result
	res.VariantCapacities = slices.Clone(res.VariantCapacities)
	for i, vc := range res.VariantCapacities {
		if n, ok := got[vc.VariantName]; ok {
			res.VariantCapacities[i].ReplicaCount = n
		}
	}
	next.CompositeSignal.Result = &res
	return next
}

// podRoleShares is the GPUs rescale's split gives each role at targetGPUs,
// before any rounding to pods.
func podRoleShares(req ModelScalingRequest, targetGPUs int) map[string]int {
	nr := req.CompositeSignal
	records := variantsOnType(buildVariantRecords(req, nr.Result), "A100")
	stateMap := buildStateMap(req.VariantStates)
	roles := modelRolesOnType(records, "A100")
	cur, dem, floor := map[string]int{}, map[string]int{}, map[string]int{}
	for _, role := range roles {
		cur[role] = roleCurrentGPUs(records, stateMap, "A100", role)
		dem[role] = roleDemandGPUs(nr, records, stateMap, "A100", role)
		floor[role] = roleFloorGPUs(records, stateMap, "A100", role)
	}
	return splitRoles(nr, records, stateMap, "A100", roles, targetGPUs, dem, cur, floor)
}

// TestRescaleMixedKeepsTheScaleDownRules: over a grid of mixed-size models
// with random holds and minReplicas, the pod search keeps every rule the
// greedy scale-down keeps. No variant goes below its minReplicas or above its
// maxReplicas, a role held against scale-down loses no pod and one held
// against scale-up gains none, and a role is emptied only when its share is
// zero and its cheapest variant holds no replica (the cheapest-at-one rule).
func TestRescaleMixedKeepsTheScaleDownRules(t *testing.T) {
	r := rand.New(rand.NewPCG(219, 4))
	points, emptied := 0, 0
	for range 1000 {
		req := randomPodModel(r, true, false)
		roles := modelRolesOnType(buildVariantRecords(req, req.CompositeSignal.Result), "A100")
		holds := map[string]RoleHold{}
		if r.IntN(3) == 0 {
			holds[roles[r.IntN(len(roles))]] = RoleHold{NoScaleDown: true}
		}
		if r.IntN(3) == 0 {
			holds[roles[r.IntN(len(roles))]] = RoleHold{NoScaleUp: true}
		}
		req.CompositeSignal.Holds = holds
		if pinnedModel(req.CompositeSignal) {
			continue
		}
		// The cheapest variant of each role, the one the cheapest-at-one
		// rule protects.
		records := buildVariantRecords(req, req.CompositeSignal.Result)
		cheapest := map[string]string{}
		for _, role := range roles {
			ordered := sortVariantsForScaleDown(req.CompositeSignal, variantsForRole(records, role))
			cheapest[role] = ordered[len(ordered)-1].VariantName
		}
		cur := podCurrentGPUs(req)
		for target := 0; target <= cur+24; target++ {
			points++
			got := rescaleTargets(req, target, 1<<20)
			share := podRoleShares(req, target)
			had, has := map[string]int{}, map[string]int{}
			for _, st := range req.VariantStates {
				n, ok := got[st.VariantName]
				if !ok {
					n = st.CurrentReplicas
				}
				had[st.Role] += st.CurrentReplicas
				has[st.Role] += n
				where := func() string { return describePodModel(req, target) }
				if st.MinReplicas != nil && n < *st.MinReplicas && n < st.CurrentReplicas {
					require.Fail(t, "below minReplicas", "%s: %s", st.VariantName, where())
				}
				if st.MaxReplicas != nil && *st.MaxReplicas > 0 && n > *st.MaxReplicas && n > st.CurrentReplicas {
					require.Fail(t, "above maxReplicas", "%s: %s", st.VariantName, where())
				}
				if holds[st.Role].NoScaleDown && n < st.CurrentReplicas {
					require.Fail(t, "a role held against scale-down shed", "%s: %s", st.VariantName, where())
				}
				if holds[st.Role].NoScaleUp && n > st.CurrentReplicas {
					require.Fail(t, "a role held against scale-up grew", "%s: %s", st.VariantName, where())
				}
			}
			for role, n := range had {
				if n == 0 || has[role] > 0 {
					continue
				}
				emptied++
				require.Zero(t, share[role], "role %s emptied with a share: %s", role, describePodModel(req, target))
				for _, st := range req.VariantStates {
					if st.VariantName == cheapest[role] {
						require.Zero(t, st.CurrentReplicas, "role %s emptied past its cheapest variant's last replica: %s",
							role, describePodModel(req, target))
					}
				}
			}
		}
	}
	t.Logf("checked %d points, %d roles emptied", points, emptied)
	assert.Greater(t, points, 30000)
	assert.Positive(t, emptied, "the grid empties some role, so the last rule is exercised")
}

// TestRescaleMixedRerunIsStable: run a cycle, take its result as what the
// model holds, and run the same target again, over targets within 16 GPUs
// of what the model holds. A model at its target does not move again, and no
// model goes back to where it started: that is a pod deleted and created
// again every cycle. A regression guard: rounded alone, 31 models at their
// target moved again and 86 went back, and the greedy rounding before #219
// moved 2 and sent 13 back. TestRescaleMixedHoldsAModelAtItsTarget pins the
// case that took a pod and gave it back; TestRescaleMixedRerunNeverPassesItsTarget
// pins the second step past the target of #421.
func TestRescaleMixedRerunIsStable(t *testing.T) {
	r := rand.New(rand.NewPCG(219, 3))
	points, moved, flapped, atTarget := 0, 0, 0, 0
	for range 1000 {
		req := randomPodModel(r, true, false)
		cur := podCurrentGPUs(req)
		for target := max(cur-16, 0); target <= cur+16; target++ {
			next := podApplied(req, target)
			first := podCurrentGPUs(next)
			second := podGPUs(next, target)
			points++
			if second != first {
				moved++
				if second == cur && first != cur {
					flapped++
				}
				if target == cur {
					atTarget++
				}
			}
		}
	}
	t.Logf("%d points: %d move again, %d go back to where they started, %d at their target", points, moved, flapped, atTarget)
	assert.Greater(t, points, 30000)
	assert.Zero(t, atTarget, "a model at its target moves again")
	assert.Zero(t, flapped, "a model goes back to where it started")
}

// TestRescaleMixedRerunStopsAtItsTarget is #421 on whole-node pods. Prefill
// holds one 8-GPU pod (16-GPU in the second case) and decode four pods half
// that size, with the same demand, and are given 40 GPUs (80): split evenly,
// prefill rounds its share up to a whole pod, takes two, and the model is at
// its target. On the next cycle at the same target prefill is cut by half a
// pod, which no pod meets, while decode still grows: rounded alone it took a
// pod, and the model ended at 44 GPUs (88), half a pod above its target.
func TestRescaleMixedRerunStopsAtItsTarget(t *testing.T) {
	for _, c := range []struct{ prefill, decode int }{{8, 4}, {16, 8}} {
		dem := 100 * float64(c.prefill)
		req := podRequest(map[string]float64{domain.RolePrefill: dem, domain.RoleDecode: dem}, []podVariant{
			{name: "p", role: domain.RolePrefill, gpus: c.prefill, cur: 1, cost: float64(c.prefill), perGPU: 100},
			{name: "d", role: domain.RoleDecode, gpus: c.decode, cur: 4, cost: float64(c.decode), perGPU: 100},
		})
		target := 5 * c.prefill
		require.Equal(t, map[string]int{domain.RolePrefill: target / 2, domain.RoleDecode: target / 2}, podRoleShares(req, target))
		first := podApplied(req, target)
		require.Equal(t, target, podCurrentGPUs(first), "the first cycle reaches the target: %s", describePodModel(req, target))
		require.Equal(t, 3, first.VariantStates[0].CurrentReplicas, "prefill rounds up to whole pods")
		assert.Equal(t, target, podGPUs(first, target), "%d- and %d-GPU pods: at its target the model keeps its pods: %s",
			c.prefill, c.decode, describePodModel(first, target))
	}
}

// TestRescaleMixedPlansAcrossLowerTargets: prefill holds one 12-GPU pod,
// decode three 8-GPU pods beside an empty 4-GPU variant. At a target of 41
// prefill grows a pod and decode, cut by 8, sheds one: 40 GPUs. At 42 decode
// is cut by 7, which no pod meets, and growing prefill alone passes the
// target, so the plan of 42 alone keeps 36, fewer than 41 leaves. The plan of
// 41 is still one 42 may take.
func TestRescaleMixedPlansAcrossLowerTargets(t *testing.T) {
	req := podRequest(map[string]float64{domain.RolePrefill: 2400, domain.RoleDecode: 1600}, []podVariant{
		{name: "p12", role: domain.RolePrefill, gpus: 12, cur: 1, cost: 12, perGPU: 100},
		{name: "d8", role: domain.RoleDecode, gpus: 8, cur: 3, cost: 8, perGPU: 100},
		{name: "d4", role: domain.RoleDecode, gpus: 4, cost: 4.4, perGPU: 100},
	})
	require.Equal(t, map[string]int{domain.RolePrefill: 25, domain.RoleDecode: 17}, podRoleShares(req, 42))
	assert.Equal(t, 40, podGPUs(req, 41), describePodModel(req, 41))
	assert.Equal(t, 40, podGPUs(req, 42), describePodModel(req, 42))
	for target := 30; target < 60; target++ {
		assert.LessOrEqual(t, podGPUs(req, target), podGPUs(req, target+1), "target %d: %s", target, describePodModel(req, target, target+1))
	}
}

// TestRescaleMixedGrowsWithTheTargetsSpareGPUs: prefill on 8-, 12- (at its
// maximum) and 16-GPU pods, decode on 2- (at its maximum) and 8-GPU pods,
// 122 GPUs in all. At a target of 162 decode's share is its ceiling of 30
// GPUs, 8 above what it holds, and prefill is cut by 7, which no pod meets.
// The shares sum to 123, so bounded by them the model could add one GPU and
// decode never got its pod, with 40 GPUs of its target unused. Bounded by
// the target it takes the pod, as rounding alone did.
func TestRescaleMixedGrowsWithTheTargetsSpareGPUs(t *testing.T) {
	req := podRequest(map[string]float64{domain.RolePrefill: 5400, domain.RoleDecode: 4000}, []podVariant{
		{name: "p8", role: domain.RolePrefill, gpus: 8, cur: 1, cost: 10.40, perGPU: 1182.4 / 8},
		{name: "p12", role: domain.RolePrefill, gpus: 12, cur: 5, maxR: 5, cost: 14.72, perGPU: 694.7 / 12},
		{name: "p16", role: domain.RolePrefill, gpus: 16, cur: 2, cost: 20.81, perGPU: 1802.5 / 16},
		{name: "d2", role: domain.RoleDecode, gpus: 2, cur: 3, maxR: 3, cost: 2.39, perGPU: 283.2 / 2},
		{name: "d8", role: domain.RoleDecode, gpus: 8, cur: 2, maxR: 3, cost: 8.89, perGPU: 825.9 / 8},
	})
	require.Equal(t, 122, podCurrentGPUs(req))
	require.Equal(t, map[string]int{domain.RolePrefill: 93, domain.RoleDecode: 30}, podRoleShares(req, 162))
	first := podApplied(req, 162)
	assert.Equal(t, 130, podCurrentGPUs(first), describePodModel(req, 162))
	assert.Equal(t, 130, podGPUs(first, 162), "the second cycle stays: %s", describePodModel(first, 162))
}

// TestRescaleMixedKeepingARoleCanCostGPUs pins the one case in which a larger
// target leaves a mixed-size model fewer GPUs. Prefill holds one 2-GPU pod
// and decode one 4-GPU pod. At a target of 12 prefill's share is zero, so
// its pod is shed and decode grows to 12. At 13 prefill has a share of one
// GPU and keeps its pod, and 2 + 8 is the most that does not pass 13; the
// next, 2 + 12, passes it, as rounding alone did. Taking 14 would keep the
// model monotone and step past its target, which is #421.
func TestRescaleMixedKeepingARoleCanCostGPUs(t *testing.T) {
	req := podRequest(map[string]float64{domain.RolePrefill: 400, domain.RoleDecode: 8500}, []podVariant{
		{name: "pa", role: domain.RolePrefill, gpus: 2, cur: 1, cost: 4, perGPU: 100},
		{name: "pb", role: domain.RolePrefill, gpus: 2, cost: 1, perGPU: 100},
		{name: "da", role: domain.RoleDecode, gpus: 4, cur: 1, cost: 7.15, perGPU: 100},
		{name: "db", role: domain.RoleDecode, gpus: 12, cost: 20.68, perGPU: 100},
	})
	require.Equal(t, map[string]int{domain.RolePrefill: 0, domain.RoleDecode: 12}, podRoleShares(req, 12))
	require.Equal(t, map[string]int{domain.RolePrefill: 1, domain.RoleDecode: 12}, podRoleShares(req, 13))
	assert.Equal(t, map[string]int{"pa": 0, "pb": 0, "da": 3, "db": 0}, rescaleTargets(req, 12, 1<<20))
	assert.Equal(t, map[string]int{"pa": 1, "pb": 0, "da": 2, "db": 0}, rescaleTargets(req, 13, 1<<20))
}

// TestRescaleMixedGrowsToFloorsAboveTheTarget: prefill must keep two 8-GPU
// pods (minReplicas) and holds one, beside an empty 4-GPU variant; decode
// holds one 4-GPU pod it may not give up (the cheapest-at-one rule). Given 8
// GPUs, less than prefill's floor of 16, the shares are the floors and sum
// above the target. The room is what the shares add, so prefill grows a
// 4-GPU pod toward its floor; bounded by the target alone it stayed short.
func TestRescaleMixedGrowsToFloorsAboveTheTarget(t *testing.T) {
	req := podRequest(map[string]float64{domain.RolePrefill: 800, domain.RoleDecode: 800}, []podVariant{
		{name: "p8", role: domain.RolePrefill, gpus: 8, cur: 1, minR: 2, cost: 8, perGPU: 100},
		{name: "p4", role: domain.RolePrefill, gpus: 4, cost: 4.4, perGPU: 100},
		{name: "d4", role: domain.RoleDecode, gpus: 4, cur: 1, cost: 4, perGPU: 100},
	})
	require.Equal(t, map[string]int{domain.RolePrefill: 16, domain.RoleDecode: 0}, podRoleShares(req, 8))
	assert.Equal(t, map[string]int{"p8": 1, "p4": 1, "d4": 1}, rescaleTargets(req, 8, 1<<20))
}

// heldDecodeModel is prefill on two 8-GPU pods beside an empty 4-GPU variant,
// and decode, held against scale-up, on an empty 8-GPU variant, with most of
// the demand on decode.
func heldDecodeModel() ModelScalingRequest {
	req := podRequest(map[string]float64{domain.RolePrefill: 300, domain.RoleDecode: 1500}, []podVariant{
		{name: "p4", role: domain.RolePrefill, gpus: 4, cost: 5.5, perGPU: 77},
		{name: "p8", role: domain.RolePrefill, gpus: 8, cur: 2, cost: 11, perGPU: 52.5},
		{name: "d8", role: domain.RoleDecode, gpus: 8, cost: 8.3, perGPU: 83.7},
	})
	req.CompositeSignal.Holds = map[string]RoleHold{domain.RoleDecode: {NoScaleUp: true}}
	return req
}

// TestRescaleMixedShedsNothingForAHeldFill: at a target of 16, what the model
// holds, decode's share is 14 and prefill's 2. Decode may not grow, so the
// fill leaves it out. Searched with it, a prefill pod was shed to pay for a
// decode pod the fill then skipped, and the model fell to 8 at its target.
func TestRescaleMixedShedsNothingForAHeldFill(t *testing.T) {
	req := heldDecodeModel()
	require.Equal(t, map[string]int{domain.RolePrefill: 2, domain.RoleDecode: 14}, podRoleShares(req, 16))
	assert.Equal(t, map[string]int{"p4": 0, "p8": 2, "d8": 0}, rescaleTargets(req, 16, 1<<20))
}

// TestRescaleMixedRoundsANegativeRoomDown: at a target of 13 the model, at 16,
// has a room of -3 GPUs, part of a 4-GPU unit. Rounded down it is a whole
// unit below zero, so prefill sheds a pod and the model ends at 8, at or
// below its target; rounded toward zero the room was 0 and the model stayed
// at 16, above it.
func TestRescaleMixedRoundsANegativeRoomDown(t *testing.T) {
	req := heldDecodeModel()
	require.Equal(t, map[string]int{domain.RolePrefill: 2, domain.RoleDecode: 11}, podRoleShares(req, 13))
	assert.Equal(t, map[string]int{"p4": 0, "p8": 1, "d8": 0}, rescaleTargets(req, 13, 1<<20))
}

// TestRescaleMixedEqualPlansTakeTheHighestTarget: prefill holds two 8-GPU pods
// and one 16-GPU pod, 32 GPUs, decode 20 on 2-, 4- and 8-GPU pods. At a target
// of 58 prefill's share is 25, a cut of 7 no pod meets, and the model grows a
// decode 2-GPU pod to 54. The plan of 57, where prefill is cut by 8, also
// reaches 54 by moving a prefill 8-GPU pod to decode. Among equal plans the
// highest target's is taken, so a pod this target's cut does not meet is
// kept.
func TestRescaleMixedEqualPlansTakeTheHighestTarget(t *testing.T) {
	req := podRequest(map[string]float64{domain.RolePrefill: 1200, domain.RoleDecode: 2500}, []podVariant{
		{name: "p8", role: domain.RolePrefill, gpus: 8, cur: 2, minR: 1, cost: 11.3, perGPU: 72.2},
		{name: "p16", role: domain.RolePrefill, gpus: 16, cur: 1, cost: 22, perGPU: 102.2},
		{name: "d2", role: domain.RoleDecode, gpus: 2, cur: 2, maxR: 3, cost: 2.15, perGPU: 89.6},
		{name: "d4", role: domain.RoleDecode, gpus: 4, cur: 2, minR: 1, cost: 5.2, perGPU: 100, noCapacityRdg: true},
		{name: "d8", role: domain.RoleDecode, gpus: 8, cur: 1, cost: 11.15, perGPU: 84.2},
	})
	require.Equal(t, map[string]int{domain.RolePrefill: 25, domain.RoleDecode: 33}, podRoleShares(req, 58))
	require.Equal(t, map[string]int{domain.RolePrefill: 24, domain.RoleDecode: 33}, podRoleShares(req, 57))
	assert.Equal(t, map[string]int{"p8": 2, "p16": 1, "d2": 3, "d4": 2, "d8": 1}, rescaleTargets(req, 58, 1<<20))
}

// TestRescaleMixedRerunNeverPassesItsTarget: over a grid of mixed-size
// models, a third of them with a role held against scale-up or scale-down,
// and targets within 16 GPUs of what each holds, no cycle ends above both
// its target and what the model held when it began, the first or the second
// at the same target. Before #421 was fixed, 4355 first cycles and 1575
// second ones did.
func TestRescaleMixedRerunNeverPassesItsTarget(t *testing.T) {
	r := rand.New(rand.NewPCG(421, 1))
	// Holds come from their own source, so the models are the same with or
	// without them.
	hr := rand.New(rand.NewPCG(421, 2))
	points, wholeNode, held, firstPast, secondPast := 0, 0, 0, 0, 0
	example := ""
	for range 1000 {
		req := randomPodModel(r, true, false)
		if hr.IntN(3) == 0 {
			roles := modelRolesOnType(buildVariantRecords(req, req.CompositeSignal.Result), "A100")
			hold := RoleHold{NoScaleDown: true}
			if hr.IntN(2) == 0 {
				hold = RoleHold{NoScaleUp: true}
			}
			req.CompositeSignal.Holds = map[string]RoleHold{roles[hr.IntN(len(roles))]: hold}
		}
		cur := podCurrentGPUs(req)
		whole := slices.ContainsFunc(req.VariantStates, func(st domain.VariantReplicaState) bool { return st.GPUsPerReplica == 8 })
		for target := max(cur-16, 0); target <= cur+16; target++ {
			next := podApplied(req, target)
			first := podCurrentGPUs(next)
			second := podGPUs(next, target)
			points++
			if whole {
				wholeNode++
			}
			if len(req.CompositeSignal.Holds) > 0 {
				held++
			}
			if first > max(target, cur) {
				firstPast++
			}
			if second > max(target, first) {
				secondPast++
				if example == "" {
					example = fmt.Sprintf("target %d: %d to %d to %d: %s", target, cur, first, second, describePodModel(next, target))
				}
			}
		}
	}
	t.Logf("%d points, %d on models with whole-node 8-GPU pods, %d with a hold: %d first and %d second cycles pass their target",
		points, wholeNode, held, firstPast, secondPast)
	assert.Greater(t, points, 30000)
	assert.Greater(t, wholeNode, 10000)
	assert.Greater(t, held, 5000)
	assert.Zero(t, firstPast, "a first cycle passes its target")
	assert.Zero(t, secondPast, "a second cycle passes its target, first at %s", example)
}

// describePodModel prints a model's variants and demand, and what each of
// targets splits to per role and ends at, for a readable failure.
func describePodModel(req ModelScalingRequest, targets ...int) string {
	var out strings.Builder
	fmt.Fprintf(&out, "demand %v;", req.CompositeSignal.Result.RoleDemand)
	for _, st := range req.VariantStates {
		fmt.Fprintf(&out, " %s x%d", st.VariantName, st.CurrentReplicas)
		if st.MinReplicas != nil {
			fmt.Fprintf(&out, " min %d", *st.MinReplicas)
		}
		if st.MaxReplicas != nil {
			fmt.Fprintf(&out, " max %d", *st.MaxReplicas)
		}
		out.WriteString(";")
	}
	for _, target := range targets {
		fmt.Fprintf(&out, " target %d -> %v;", target, rescaleTargets(req, target, 1<<20))
	}
	return out.String()
}

// TestRescaleWholeNodeMixedStress: whole-node 8-GPU pods beside 4- and
// 16-GPU ones, at tens of pods and targets in the hundreds of GPUs. A
// regression guard: the rounding before #219 passes it too.
func TestRescaleWholeNodeMixedStress(t *testing.T) {
	req := podRequest(map[string]float64{domain.RolePrefill: 30000, domain.RoleDecode: 50000}, []podVariant{
		{name: "p8", role: domain.RolePrefill, gpus: 8, cur: 12, cost: 8, perGPU: 100},
		{name: "p16", role: domain.RolePrefill, gpus: 16, cur: 3, cost: 17, perGPU: 100},
		{name: "d8", role: domain.RoleDecode, gpus: 8, cur: 20, cost: 8, perGPU: 100},
		{name: "d4", role: domain.RoleDecode, gpus: 4, cur: 5, cost: 4.2, perGPU: 100},
	})
	cur := podCurrentGPUs(req)
	prev := -1
	for target := 0; target <= cur+400; target += 3 {
		got := podGPUs(req, target)
		require.LessOrEqual(t, prev, got, "target %d", target)
		prev = got
		if target <= cur-16 {
			assert.Less(t, got, cur, "target %d", target)
		}
	}
}

// podSettle reruns req at targetGPUs, each cycle from what the last one set,
// for at most cycles cycles, and returns the model where it stopped and the
// cycles it took to stop moving, or -1 when it was still moving.
func podSettle(req ModelScalingRequest, targetGPUs, cycles int) (ModelScalingRequest, int) {
	for i := range cycles {
		next := podApplied(req, targetGPUs)
		if slices.EqualFunc(next.VariantStates, req.VariantStates, func(a, b domain.VariantReplicaState) bool {
			return a.CurrentReplicas == b.CurrentReplicas
		}) {
			return req, i
		}
		req = next
	}
	return req, -1
}

// podRoleGPUs is the GPUs role holds.
func podRoleGPUs(req ModelScalingRequest, role string) int {
	total := 0
	for _, st := range req.VariantStates {
		if st.Role == role {
			total += st.CurrentReplicas * st.GPUsPerReplica
		}
	}
	return total
}

// TestRescaleMixedHeldRoleSettles is #440. Prefill, held against scale-down,
// holds two 1-GPU pods and three 2-GPU pods (its maximum), 8 GPUs; decode one
// 8-GPU pod beside an empty 2-GPU variant. Demand is for 5 prefill and 18
// decode GPUs, and the target is 50. The split reserved prefill what it held
// and gave it a share of the rest on top, 17 GPUs: the pods it gained were
// next cycle's reserve, so every rerun at 50 moved decode 2-GPU pods to
// prefill 1-GPU pods, 16 GPUs to 22, 28, 32 and on to 42. Prefill's share is
// 11 of the 50; held, it may keep more than that, but not gain it.
func TestRescaleMixedHeldRoleSettles(t *testing.T) {
	req := podRequest(map[string]float64{domain.RolePrefill: 500, domain.RoleDecode: 1800}, []podVariant{
		{name: "p1", role: domain.RolePrefill, gpus: 1, cur: 2, cost: 1, perGPU: 100},
		{name: "p2", role: domain.RolePrefill, gpus: 2, cur: 3, maxR: 3, cost: 2, perGPU: 100},
		{name: "d2", role: domain.RoleDecode, gpus: 2, cost: 2, perGPU: 100},
		{name: "d8", role: domain.RoleDecode, gpus: 8, cur: 1, maxR: 3, cost: 8, perGPU: 100},
	})
	req.CompositeSignal.Holds = map[string]RoleHold{domain.RolePrefill: {NoScaleDown: true}}
	require.Equal(t, 16, podCurrentGPUs(req))
	assert.Equal(t, map[string]int{domain.RolePrefill: 11, domain.RoleDecode: 39}, podRoleShares(req, 50))

	got, cycles := podSettle(req, 50, 8)
	require.NotEqual(t, -1, cycles, "still moving after 8 cycles at the same target: %s", describePodModel(got, 50))
	assert.LessOrEqual(t, cycles, 2, "settles within two cycles: %s", describePodModel(got, 50))
	assert.Equal(t, 50, podCurrentGPUs(got), describePodModel(got, 50))
	assert.LessOrEqual(t, podRoleGPUs(got, domain.RolePrefill), 11,
		"the held role grows no further than its share: %s", describePodModel(got, 50))
}

// TestRescaleMixedIdleRoleSettles is #449. Prefill has no demand and holds two
// 1-GPU pods beside an empty 8-GPU variant. Decode holds two 8-GPU pods (min
// 1, max 4) with demand, but its variant has no capacity reading, so its
// demand sizes to no GPUs. With no role sized, the split went by what each
// role holds. Decode cannot fill a variant with no reading, so prefill took
// the spare share, and the pod it gained raised its weight on the next cycle:
// at a target of 34 it went 2, 3, 4, ... 9 GPUs and settled after 7 cycles.
// A role with no demand gets no share above its floor, so prefill never grows.
func TestRescaleMixedIdleRoleSettles(t *testing.T) {
	req := podRequest(map[string]float64{domain.RolePrefill: 0, domain.RoleDecode: 1700}, []podVariant{
		{name: "p1", role: domain.RolePrefill, gpus: 1, cur: 2, cost: 1.3, perGPU: 65},
		{name: "p8", role: domain.RolePrefill, gpus: 8, cost: 10, perGPU: 113},
		{name: "d8", role: domain.RoleDecode, gpus: 8, cur: 2, minR: 1, maxR: 4, cost: 10, perGPU: 100, noCapacityRdg: true},
	})
	require.Equal(t, 18, podCurrentGPUs(req))
	for target := 31; target <= 34; target++ {
		got, cycles := podSettle(req, target, 8)
		require.NotEqual(t, -1, cycles, "target %d: still moving after 8 cycles: %s", target, describePodModel(got, target))
		assert.LessOrEqual(t, cycles, 2, "target %d: settles within two cycles: %s", target, describePodModel(got, target))
		assert.LessOrEqual(t, podRoleGPUs(got, domain.RolePrefill), 2,
			"target %d: the role with no demand grows no further than what it held: %s", target, describePodModel(got, target))
	}
}

// TestRescaleMixedHeldRerunSettles: over a grid of mixed-size models, two
// thirds of them with a role held against scale-up or scale-down, and targets
// within 16 GPUs of what each holds, rerunning the same target reaches a cycle
// that moves nothing within 8 cycles, and a model with a role held against
// scale-down within 2. With the held role's pods reserved beneath a share of
// the rest (#440), 1 point was still moving after 8 cycles and 1780 held
// points took more than 2.
func TestRescaleMixedHeldRerunSettles(t *testing.T) {
	r := rand.New(rand.NewPCG(440, 1))
	hr := rand.New(rand.NewPCG(440, 2))
	points, held, moving, slow := 0, 0, 0, 0
	example := ""
	for range 3000 {
		req := randomPodModel(r, true, false)
		down := false
		if h := hr.IntN(3); h > 0 {
			roles := modelRolesOnType(buildVariantRecords(req, req.CompositeSignal.Result), "A100")
			down = h == 1
			req.CompositeSignal.Holds = map[string]RoleHold{roles[hr.IntN(len(roles))]: {NoScaleDown: down, NoScaleUp: !down}}
		}
		cur := podCurrentGPUs(req)
		for target := max(cur-16, 0); target <= cur+16; target++ {
			points++
			if down {
				held++
			}
			_, cycles := podSettle(req, target, 8)
			if cycles < 0 {
				moving++
			}
			if down && (cycles < 0 || cycles > 2) {
				slow++
				if example == "" {
					example = fmt.Sprintf("target %d, %d cycles: %s", target, cycles, describePodModel(req, target))
				}
			}
		}
	}
	t.Logf("%d points, %d held against scale-down: %d still moving after 8 cycles, %d held taking more than 2",
		points, held, moving, slow)
	assert.Greater(t, points, 90000)
	assert.Greater(t, held, 25000)
	assert.Zero(t, moving, "a rerun at the same target keeps moving")
	assert.Zero(t, slow, "a held model takes more than 2 cycles, first at %s", example)
}
