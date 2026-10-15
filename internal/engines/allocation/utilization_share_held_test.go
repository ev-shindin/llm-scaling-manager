package allocation

import (
	"math"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// heldIdleGroup is two idle models, A and B, at four 1-GPU replicas each in a
// quota of 10, whose demand the analyzer holds at the bottom of the band
// (0.70 x supply) -- a shape-change hold -- with their demand before the hold
// at zero. Without the hold recorded (held false) the result looks like the
// analyzer's before HeldRoleDemand existed.
func heldIdleGroup(t *testing.T, held bool) ShareGroup {
	t.Helper()
	reqs := make([]ModelScalingRequest, 0, 2)
	for _, m := range []string{"A", "B"} {
		r := shareReq(m, "ns", 0.70*4*1000, 4)
		r.CompositeSignal.Result.RoleDemand = map[string]float64{domain.RoleBoth: 0.70 * 4 * 1000}
		if held {
			r.CompositeSignal.Result.HeldRoleDemand = map[string]float64{domain.RoleBoth: 0}
		}
		reqs = append(reqs, r)
	}
	groups := BuildShareGroups(reqs, []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 10, Used: 8}}}},
		ShareGroupOptions{ClusterIsQuota: true})
	if len(groups) != 1 || len(groups[0].Roles) != 2 {
		t.Fatalf("setup: groups %+v, want one group of two roles", groups)
	}
	return groups[0]
}

// heldIdleTargets plans the group the way actuation does and returns each
// role's whole-replica target -- what the idle fill raises it to.
func heldIdleTargets(g ShareGroup) map[string]int {
	p := PlanShareTransfers(NewShareLedger(), SharePlanInput{
		Roles: g.Roles, Held: g.Committed, Thresholds: g.Thresholds, DownThresholds: g.DownThresholds,
		Budget: g.Budget, Tolerance: 0.15, Give: g.Give, Grow: g.Grow, PhysicalFree: g.ClusterFree,
		Headroom: ShareHeadroom{Capped: true, Replicas: 1, ReturnLoad: 0.6}, Origins: g.Origins,
	}, time.Unix(100000, 0), simTimings())
	out := map[string]int{}
	for _, v := range p.Evaluation.Roles {
		out[v.Key] = v.Integer
	}
	return out
}

// A hold keeps a fleet where it is by raising its demand to the bottom of the
// band, and read as need that is 0.70/0.85 of what the role holds: plus a
// replica of headroom, every idle fill raised the next. On a cluster two idle
// models filled a quota of 10 from 1 and 1 to 5 and 4 before any load came. A
// held role is sized on its demand before the hold -- here none, so its floor
// plus a replica of headroom -- and gives nothing while the hold lasts.
func TestShareHeldDemandDoesNotRatchetTheIdleFill(t *testing.T) {
	g := heldIdleGroup(t, true)
	for _, r := range g.Roles {
		if r.Need != 0 {
			t.Errorf("%s needs %v, want 0: the demand before the hold", r.Key, r.Need)
		}
		if _, ok := g.Give[r.Key]; ok {
			t.Errorf("%s can give while its demand is held", r.Key)
		}
	}
	for k, n := range heldIdleTargets(g) {
		if n > g.Committed[k] {
			t.Errorf("%s, idle and held at %d GPUs, targets %d: the idle fill would raise it again", k, g.Committed[k], n)
		}
	}

	// Control: the held figure read as need is 3.5 GPUs, which with a replica
	// of headroom targets 5 -- one above the 4 the role holds.
	g = heldIdleGroup(t, false)
	for _, r := range g.Roles {
		if r.Need < 3.4 {
			t.Fatalf("control: %s needs %v, want the held 0.70 x 4000 / 0.8 / 1000 = 3.5", r.Key, r.Need)
		}
	}
	raised := false
	for k, n := range heldIdleTargets(g) {
		raised = raised || n > g.Committed[k]
	}
	if !raised {
		t.Fatal("control: on the held figure no role targets more than it holds; the test no longer shows the ratchet")
	}
}

// The group carries each role's measured replica start from its analyzer
// result, and the timings take the slowest: a whole-node model's 10 minutes,
// not the 2 of the small model beside it. A reading that is not positive is
// not carried.
func TestShareGroupCarriesTheMeasuredStart(t *testing.T) {
	reqs := make([]ModelScalingRequest, 0, 3)
	for m, s := range map[string]float64{"A": 600, "B": 120, "C": math.NaN()} {
		r := shareReq(m, "ns", 1000, 1)
		r.CompositeSignal.Result.RoleStartSeconds = map[string]float64{domain.RoleBoth: s}
		reqs = append(reqs, r)
	}
	groups := BuildShareGroups(reqs, []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 10, Used: 3}}}},
		ShareGroupOptions{ClusterIsQuota: true})
	if len(groups) != 1 || len(groups[0].Roles) != 3 {
		t.Fatalf("setup: groups %+v, want one group of three roles", groups)
	}
	g := groups[0]
	if len(g.StartSeconds) != 2 {
		t.Fatalf("StartSeconds %v, want A's and B's only", g.StartSeconds)
	}
	if d := ShareReplicaStart(g.StartSeconds); d != 10*time.Minute {
		t.Fatalf("the group's replica start is %v, want A's 10m", d)
	}
}
