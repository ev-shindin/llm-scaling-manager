package capacity

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// rolloutDeployment is a Deployment with args, mid-rollout when rolling.
func rolloutDeployment(rolling bool, args ...string) scaletarget.ScaleTargetAccessor {
	d := makeTestDeployment(args...)
	d.Generation, d.Status.ObservedGeneration = 2, 2
	d.Status.Replicas, d.Status.UpdatedReplicas = 4, 4
	if rolling {
		d.Status.UpdatedReplicas = 1
	}
	return scaletarget.NewDeploymentAccessor(d)
}

// The record says a rollout is in progress for exactly as long as the scale
// target does -- every rollout, an image-only one and an undo included --
// and remembers nothing about where one began, so a live record carries it
// and the cycle after the rollout ends drops it.
func TestARecordFollowsTheRolloutItsScaleTargetReports(t *testing.T) {
	for _, tc := range []struct {
		name string
		live bool
	}{{"from a record derived from the deployment", false}, {"from a live record", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			s.LoadFromScaleTarget("ns", "m", "v", "H100", 8, rolloutDeployment(false, "--max-num-seqs=256"), time.Now())
			if tc.live {
				r := s.Get("ns", "m", "v")
				s.Update("ns", "m", "v", Record{AcceleratorName: "H100", GpuCount: 8, EngineParams: r.EngineParams,
					TotalKvCapacityTokens: 16000, EffectiveCapacity: 12800, LearnedFrom: LearnedFromLive}, time.Now())
			}
			if s.Get("ns", "m", "v").InRollout() {
				t.Fatal("precondition: no rollout")
			}
			s.LoadFromScaleTarget("ns", "m", "v", "H100", 8, rolloutDeployment(true, "--max-num-seqs=256"), time.Now())
			if !s.Get("ns", "m", "v").InRollout() {
				t.Error("a rollout that leaves the arguments alone is a rollout too")
			}
			s.LoadFromScaleTarget("ns", "m", "v", "H100", 8, rolloutDeployment(true, "--max-num-seqs=64"), time.Now())
			if !s.Get("ns", "m", "v").InRollout() {
				t.Error("a rollout to other arguments")
			}
			if tc.live {
				r := s.Get("ns", "m", "v")
				s.Update("ns", "m", "v", Record{AcceleratorName: "H100", GpuCount: 8, EngineParams: r.EngineParams,
					RollingOut: r.RollingOut, LearnedFrom: LearnedFromLive}, time.Now())
				s.LoadFromScaleTarget("ns", "m", "v", "H100", 8, rolloutDeployment(true, "--max-num-seqs=64"), time.Now())
				if !s.Get("ns", "m", "v").InRollout() {
					t.Error("a live record lost the rollout it is in")
				}
			}
			s.LoadFromScaleTarget("ns", "m", "v", "H100", 8, rolloutDeployment(true, "--max-num-seqs=256"), time.Now())
			if !s.Get("ns", "m", "v").InRollout() {
				t.Error("an undo in mid-rollout read as no rollout while the other arguments' pods still serve")
			}
			s.LoadFromScaleTarget("ns", "m", "v", "H100", 8, rolloutDeployment(false, "--max-num-seqs=256"), time.Now())
			if s.Get("ns", "m", "v").InRollout() {
				t.Error("the rollout completed and is still reported")
			}
		})
	}
}

// A rollout first seen with no record -- a controller restart, a failover, a
// forget in mid-rollout -- is a rollout like any other: nothing about it is
// remembered, so nothing is missing.
func TestARolloutFirstSeenWithNoRecordIsOne(t *testing.T) {
	s := NewStore()
	s.LoadFromScaleTarget("ns", "m", "v", "H100", 8, rolloutDeployment(true, "--max-num-seqs=64"), time.Now())
	if !s.Get("ns", "m", "v").InRollout() {
		t.Error("a rollout first seen with no record was not taken as one")
	}
	s.LoadFromScaleTarget("ns", "m", "v", "H100", 8, rolloutDeployment(false, "--max-num-seqs=64"), time.Now())
	if s.Get("ns", "m", "v").InRollout() {
		t.Error("it completed and is still reported")
	}
}

// The record takes scaletarget.RollingOut, whose LeaderWorkerSet half is
// covered there against statuses built the way the LWS controller writes them
// (TestRollingOutReadsALeaderWorkerSetAsItsControllerWritesIt).
