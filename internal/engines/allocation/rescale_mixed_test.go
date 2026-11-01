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
		if nr.Holds[role].NoScaleDown {
			floor[role] = max(floor[role], cur[role])
		}
	}
	uncapped := distributeGPUsByWeight(targetGPUs, roles, dem, cur, floor, nil)
	ceil := roleShareCeilings(records, stateMap, "A100", roles, dem, uncapped)
	return distributeGPUsByWeight(targetGPUs, roles, dem, cur, floor, ceil)
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
// moved 2 and sent 13 back. It passes without the shed-nothing plan of
// roundExact too; TestRescaleMixedHoldsAModelAtItsTarget pins that plan.
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
