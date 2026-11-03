package allocation

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// TestSplitRolesKeepsAHeldRoleAtWhatItHolds: splitRoles gives a role held
// against scale-down its share, and when that is below what it holds, exactly
// what it holds, with the rest split over the other roles. Each case is a
// P/D model on 1-GPU pods.
func TestSplitRolesKeepsAHeldRoleAtWhatItHolds(t *testing.T) {
	cases := []struct {
		name               string
		prefill, decode    podVariant
		demP, demD, target int // demand in GPUs
		holds              map[string]RoleHold
		want               map[string]int
	}{{
		// No demand: the split is by what the roles hold, 3 and 5 of 8, and
		// decode is capped at its maxReplicas of 4. Prefill is pinned at 6,
		// its ceiling too: given its maxReplicas of 10 as ceiling, it took
		// part of what decode could not, beyond what it holds.
		name:    "no demand, the other role at its ceiling",
		prefill: podVariant{name: "p", role: domain.RolePrefill, gpus: 1, cur: 6, maxR: 10, cost: 1, perGPU: 100},
		decode:  podVariant{name: "d", role: domain.RoleDecode, gpus: 1, cur: 10, maxR: 4, cost: 1, perGPU: 100},
		target:  8,
		holds:   map[string]RoleHold{domain.RolePrefill: {NoScaleDown: true}},
		want:    map[string]int{domain.RolePrefill: 6, domain.RoleDecode: 2},
	}, {
		// Prefill's share of 30 is 5 and it holds 20: it keeps 20 and decode
		// takes the other 10. Reserved beneath a share of the rest, it was
		// given 22.
		name:    "the held role holds more than its share",
		prefill: podVariant{name: "p", role: domain.RolePrefill, gpus: 1, cur: 20, cost: 1, perGPU: 100},
		decode:  podVariant{name: "d", role: domain.RoleDecode, gpus: 1, cur: 10, cost: 1, perGPU: 100},
		demP:    5, demD: 25, target: 30,
		holds: map[string]RoleHold{domain.RolePrefill: {NoScaleDown: true}},
		want:  map[string]int{domain.RolePrefill: 20, domain.RoleDecode: 10},
	}, {
		// Prefill's share is 5, so it is pinned at 20; decode then splits
		// the other 10, below its own 20, and is pinned on the second pass.
		// Stopped after the first, decode was given 10.
		name:    "two held roles, the second pinned by the re-split",
		prefill: podVariant{name: "p", role: domain.RolePrefill, gpus: 1, cur: 20, cost: 1, perGPU: 100},
		decode:  podVariant{name: "d", role: domain.RoleDecode, gpus: 1, cur: 20, cost: 1, perGPU: 100},
		demP:    5, demD: 25, target: 30,
		holds: map[string]RoleHold{domain.RolePrefill: {NoScaleDown: true}, domain.RoleDecode: {NoScaleDown: true}},
		want:  map[string]int{domain.RolePrefill: 20, domain.RoleDecode: 20},
	}, {
		// A held role whose share is above what it holds takes its share,
		// as an unheld one does.
		name:    "the held role's share is above what it holds",
		prefill: podVariant{name: "p", role: domain.RolePrefill, gpus: 1, cur: 4, cost: 1, perGPU: 100},
		decode:  podVariant{name: "d", role: domain.RoleDecode, gpus: 1, cur: 26, cost: 1, perGPU: 100},
		demP:    15, demD: 15, target: 30,
		holds: map[string]RoleHold{domain.RolePrefill: {NoScaleDown: true}},
		want:  map[string]int{domain.RolePrefill: 15, domain.RoleDecode: 15},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := podRequest(map[string]float64{
				domain.RolePrefill: float64(100 * c.demP), domain.RoleDecode: float64(100 * c.demD)},
				[]podVariant{c.prefill, c.decode})
			req.CompositeSignal.Holds = c.holds
			assert.Equal(t, c.want, podRoleShares(req, c.target))
		})
	}
}

// TestSplitRolesWithAnIdleRole is a regression guard for #449: every case
// passes on the code it was written against, and none of it is a fix.
// Prefill has no demand. When decode's demand sizes to GPUs, prefill gets
// nothing above its floor. When it does not (decode has no capacity reading,
// or its demand reads NaN), the split goes by what each role holds, so given
// a target above what the model holds, prefill would take a share it never
// had. applyRescale never gives such a model that target: rescaleInputsForGroup
// caps it at its floors plus what its held roles hold, the GPUs it is given
// below. Called on its own at a target of 34, the split gave prefill 3, and
// one more GPU each rerun (#449); that target never reaches it.
func TestSplitRolesWithAnIdleRole(t *testing.T) {
	unsized := podVariant{name: "d", role: domain.RoleDecode, gpus: 8, cur: 2, minR: 1, maxR: 4, cost: 8, perGPU: 100, noCapacityRdg: true}
	idle := podVariant{name: "p", role: domain.RolePrefill, gpus: 1, cur: 2, cost: 1, perGPU: 100}
	cases := []struct {
		name            string
		prefill, decode podVariant
		demP, demD      float64
		target          int
		holds           map[string]RoleHold
		want            map[string]int
	}{{
		// The model's cap is decode's floor, 8: prefill is cut, not grown.
		name:    "decode unsized, at the model's cap",
		prefill: idle, decode: unsized, demD: 1700, target: 8,
		want: map[string]int{domain.RolePrefill: 0, domain.RoleDecode: 8},
	}, {
		// Held, prefill raises the cap to 10 and keeps its 2.
		name:    "decode unsized, prefill held, at the model's cap",
		prefill: idle, decode: unsized, demD: 1700, target: 10,
		holds: map[string]RoleHold{domain.RolePrefill: {NoScaleDown: true}},
		want:  map[string]int{domain.RolePrefill: 2, domain.RoleDecode: 8},
	}, {
		// Held, decode raises the cap to 16 and keeps it all.
		name:    "decode unsized, decode held, at the model's cap",
		prefill: idle, decode: unsized, demD: 1700, target: 16,
		holds: map[string]RoleHold{domain.RoleDecode: {NoScaleDown: true}},
		want:  map[string]int{domain.RolePrefill: 0, domain.RoleDecode: 16},
	}, {
		// A NaN demand sizes to no GPUs, as an unsized role does.
		name:    "decode reads NaN, at the model's cap",
		prefill: idle, decode: podVariant{name: "d", role: domain.RoleDecode, gpus: 8, cur: 2, minR: 1, maxR: 4, cost: 8, perGPU: 100},
		demD: math.NaN(), target: 8,
		want: map[string]int{domain.RolePrefill: 0, domain.RoleDecode: 8},
	}, {
		// Decode at its maxReplicas leaves 14 of the 20 unassigned; prefill,
		// with no demand, takes none of it.
		name:    "decode sized and at its ceiling",
		prefill: podVariant{name: "p", role: domain.RolePrefill, gpus: 1, cur: 4, cost: 1, perGPU: 100},
		decode:  podVariant{name: "d", role: domain.RoleDecode, gpus: 1, cur: 4, maxR: 6, cost: 1, perGPU: 100},
		demD:    1000, target: 20,
		want: map[string]int{domain.RolePrefill: 0, domain.RoleDecode: 6},
	}, {
		// No role has demand: neither is idle, and the split is by what
		// each holds, 2 and 6 of 8.
		name:    "no role has demand",
		prefill: podVariant{name: "p", role: domain.RolePrefill, gpus: 1, cur: 2, cost: 1, perGPU: 100},
		decode:  podVariant{name: "d", role: domain.RoleDecode, gpus: 1, cur: 6, cost: 1, perGPU: 100},
		target:  16,
		want:    map[string]int{domain.RolePrefill: 4, domain.RoleDecode: 12},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := podRequest(map[string]float64{domain.RolePrefill: c.demP, domain.RoleDecode: c.demD},
				[]podVariant{c.prefill, c.decode})
			req.CompositeSignal.Holds = c.holds
			assert.Equal(t, c.want, podRoleShares(req, c.target))
		})
	}
}

// groupApplied is reqs after one applyRescale cycle on a budget of budget
// GPUs: each variant at the replicas that cycle set.
func groupApplied(t *testing.T, reqs []ModelScalingRequest, budget int) []ModelScalingRequest {
	used := 0
	for _, req := range reqs {
		used += podCurrentGPUs(req)
	}
	o := NewGreedyByScoreOptimizer()
	o.Rescale = RescaleFlags{Cluster: true}
	decisions, handled := o.applyRescale(context.Background(), reqs, nil, map[string]int{"A100": budget - used}, nil)
	require.Len(t, handled, len(reqs), "the group is contended")
	got := map[string]int{}
	for _, d := range decisions {
		got[d.VariantName] = d.TargetReplicas
	}
	out := make([]ModelScalingRequest, len(reqs))
	for i, req := range reqs {
		next := req
		next.VariantStates = append([]domain.VariantReplicaState(nil), req.VariantStates...)
		for j, st := range next.VariantStates {
			if n, ok := got[st.VariantName]; ok {
				next.VariantStates[j].CurrentReplicas = n
			}
		}
		res := *req.CompositeSignal.Result
		res.VariantCapacities = append([]domain.VariantCapacity(nil), res.VariantCapacities...)
		for j, vc := range res.VariantCapacities {
			if n, ok := got[vc.VariantName]; ok {
				res.VariantCapacities[j].ReplicaCount = n
			}
		}
		next.CompositeSignal.Result = &res
		out[i] = next
	}
	return out
}

// renamed is req as model id, its variants prefixed with id.
func renamed(req ModelScalingRequest, id string) ModelScalingRequest {
	req.ModelID = id
	res := *req.CompositeSignal.Result
	res.ModelID = id
	res.VariantCapacities = append([]domain.VariantCapacity(nil), res.VariantCapacities...)
	for i := range res.VariantCapacities {
		res.VariantCapacities[i].VariantName = id + "-" + res.VariantCapacities[i].VariantName
	}
	req.CompositeSignal.Result = &res
	req.VariantStates = append([]domain.VariantReplicaState(nil), req.VariantStates...)
	for i := range req.VariantStates {
		req.VariantStates[i].VariantName = id + "-" + req.VariantStates[i].VariantName
	}
	req.Variants = append([]domain.VariantMetadata(nil), req.Variants...)
	for i := range req.Variants {
		req.Variants[i].VariantName = id + "-" + req.Variants[i].VariantName
	}
	return req
}

// TestRescaleHeldRoleAcrossModels: two models of equal priority and demand
// share a budget of 100 GPUs on 1-GPU pods. Model a is P/D with prefill held
// against scale-down at 10 GPUs; model b serves both roles in one. The
// water-fill reserved a its held 10 GPUs and gave it half the rest on top, 55;
// the prefill it gained was the next cycle's reserve, and over five cycles a
// went to 67 and b to 33. Each takes 50 from the first cycle on.
func TestRescaleHeldRoleAcrossModels(t *testing.T) {
	a := renamed(podRequest(map[string]float64{domain.RolePrefill: 6000, domain.RoleDecode: 6000}, []podVariant{
		{name: "p", role: domain.RolePrefill, gpus: 1, cur: 10, cost: 1, perGPU: 100},
		{name: "d", role: domain.RoleDecode, gpus: 1, cur: 10, cost: 1, perGPU: 100},
	}), "a")
	a.CompositeSignal.Holds = map[string]RoleHold{domain.RolePrefill: {NoScaleDown: true}}
	b := renamed(podRequest(map[string]float64{domain.RoleBoth: 12000}, []podVariant{
		{name: "x", role: domain.RoleBoth, gpus: 1, cur: 40, cost: 1, perGPU: 100},
	}), "b")
	reqs := []ModelScalingRequest{a, b}
	for cycle := range 8 {
		reqs = groupApplied(t, reqs, 100)
		assert.Equal(t, []int{50, 50}, []int{podCurrentGPUs(reqs[0]), podCurrentGPUs(reqs[1])},
			"cycle %d: a prefill %d decode %d", cycle+1,
			podRoleGPUs(reqs[0], domain.RolePrefill), podRoleGPUs(reqs[0], domain.RoleDecode))
	}
}

// TestHeldRescaleTargets: a model keeps its HeldGPUs when its share is less,
// and the rest goes to the others; a model whose share is more takes its
// share. Reserved beneath a share of the rest, a held 60 was given 80.
func TestHeldRescaleTargets(t *testing.T) {
	for _, c := range []struct {
		held int
		want map[string]int
	}{
		{held: 60, want: map[string]int{"a": 60, "b": 40}},
		{held: 20, want: map[string]int{"a": 50, "b": 50}},
	} {
		got, over := heldRescaleTargets([]rescaleInput{
			{ID: "a", Priority: 1, Demand: 10, CapGPUs: 100, HeldGPUs: c.held},
			{ID: "b", Priority: 1, Demand: 10, CapGPUs: 100},
		}, 100)
		assert.False(t, over)
		assert.Equal(t, c.want, got, "held %d", c.held)
	}
	got, over := heldRescaleTargets([]rescaleInput{
		{ID: "a", Priority: 1, Demand: 10, CapGPUs: 100, HeldGPUs: 70},
		{ID: "b", Priority: 1, Demand: 10, CapGPUs: 100, HeldGPUs: 40},
	}, 100)
	assert.True(t, over, "what the models hold exceeds the budget")
	assert.Equal(t, map[string]int{"a": 70, "b": 40}, got)
}

// TestRescaleHeldRoleAboveItsModelsShare: model a holds 60 GPUs of prefill,
// held against scale-down, and 10 of decode; b holds 30. a's share of the 100
// GPUs is 37, below what its prefill holds, so a's target is 60 and b's the
// other 40. a sheds decode to its last pod, which it keeps, so a ends at 61
// and b grows into the 39 that frees.
// Reserved its 60 beneath a share of the rest, a was given 75, kept all 10
// decode pods, and b stayed at 30.
func TestRescaleHeldRoleAboveItsModelsShare(t *testing.T) {
	a := renamed(podRequest(map[string]float64{domain.RolePrefill: 1000, domain.RoleDecode: 6000}, []podVariant{
		{name: "p", role: domain.RolePrefill, gpus: 1, cur: 60, cost: 1, perGPU: 100},
		{name: "d", role: domain.RoleDecode, gpus: 1, cur: 10, cost: 1, perGPU: 100},
	}), "a")
	a.CompositeSignal.Holds = map[string]RoleHold{domain.RolePrefill: {NoScaleDown: true}}
	b := renamed(podRequest(map[string]float64{domain.RoleBoth: 12000}, []podVariant{
		{name: "x", role: domain.RoleBoth, gpus: 1, cur: 30, cost: 1, perGPU: 100},
	}), "b")
	reqs := groupApplied(t, groupApplied(t, []ModelScalingRequest{a, b}, 100), 100)
	assert.Equal(t, 60, podRoleGPUs(reqs[0], domain.RolePrefill), "the held role keeps what it holds")
	assert.Equal(t, 1, podRoleGPUs(reqs[0], domain.RoleDecode), "decode keeps its last pod")
	assert.Equal(t, 39, podCurrentGPUs(reqs[1]))
}

// TestRescaleSetsNoTargetPastWhatAHeldModelLeaves: model a, one role held
// against scale-down, holds 60 of 100 GPUs, above its share of 37; b holds the
// other 40. b's target is the 40 a leaves, so with no GPU free nothing is held
// back. Given its share of 63, b was marked held back by scarcity every cycle
// for GPUs a may not give up.
func TestRescaleSetsNoTargetPastWhatAHeldModelLeaves(t *testing.T) {
	a := renamed(podRequest(map[string]float64{domain.RoleBoth: 7000}, []podVariant{
		{name: "x", role: domain.RoleBoth, gpus: 1, cur: 60, cost: 1, perGPU: 100},
	}), "a")
	a.CompositeSignal.Holds = map[string]RoleHold{domain.RoleBoth: {NoScaleDown: true}}
	b := renamed(podRequest(map[string]float64{domain.RoleBoth: 12000}, []podVariant{
		{name: "x", role: domain.RoleBoth, gpus: 1, cur: 40, cost: 1, perGPU: 100},
	}), "b")
	o := NewGreedyByScoreOptimizer()
	o.Rescale = RescaleFlags{Cluster: true}
	decisions, handled := o.applyRescale(context.Background(), []ModelScalingRequest{a, b}, nil, map[string]int{"A100": 0}, nil)
	require.Len(t, handled, 2, "the group is contended")
	for _, d := range decisions {
		assert.False(t, d.WasLimited, "%s is marked held back by scarcity", d.VariantName)
	}
}
