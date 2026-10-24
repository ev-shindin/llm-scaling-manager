package steadystate

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// setA4Ready sets the Ready condition of the fleet's pod A-v-4.
func setA4Ready(t *testing.T, c client.Client, ready bool) {
	t.Helper()
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "A-v-4"}, &p); err != nil {
		t.Fatal(err)
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
}

// A donor refused because one of its pods is not Ready is held only until the
// pod is Ready and its workload has read stable for the confirm cycles -- not
// for the whole release timeout, which held a donor whose idle-fill pod was
// Ready in four minutes for ten (issue #290). Every held cycle names the hold:
// donor-changing on the donor and on the short receiver, and a log line at the
// default level. With whole 8-GPU replicas as with 1-GPU ones.
func TestUtilizationShareChangingDonorIsReleasedOnceStable(t *testing.T) {
	const heldLine = "Utilization share: donor held while its workload is changing; it gives nothing until it is stable"
	for _, gpus := range []int{1, 8} {
		t.Run(fmt.Sprintf("%d GPUs per replica", gpus), func(t *testing.T) {
			reg := freshMetrics(t)
			f := newShareFleet()
			c := sharePods(t, f)
			setA4Ready(t, c, false)
			se := newShareEngine(t, f, c, time.Unix(0, 0))
			ctx, logs := observe(t)
			se.ctx = ctx
			quota := fullQuota()
			for _, rc := range quota {
				p := rc.Pools["A100"]
				p.Limit, p.Used = p.Limit*gpus, p.Used*gpus
				rc.Pools["A100"] = p
			}
			cycle := func() map[string]utilizationShareOverride {
				se.clock = se.clock.Add(30 * time.Second)
				reqs := f.requests()
				for i := range reqs {
					for j := range reqs[i].VariantStates {
						reqs[i].VariantStates[j].GPUsPerReplica = gpus
					}
				}
				return se.e.evaluateUtilizationShare(se.ctx, reqs, quota, f.scaleTargets())
			}
			ledger := func() *allocation.ShareLedger {
				for _, l := range se.e.utilizationShare.ledgers {
					return l
				}
				t.Fatal("no ledger")
				return nil
			}

			// Asked to give with a pod still starting: refused and held.
			held := false
			for range 20 {
				if a := targetOf(t, cycle(), "ns/A-v"); a != 9 {
					t.Fatalf("A lowered to %d although one of its pods is not Ready", a)
				}
				if len(ledger().ChangingHolds(se.clock)) > 0 {
					held = true
					break
				}
			}
			if !held {
				t.Fatal("setup: A was never refused as a changing donor in 20 cycles")
			}
			since := ledger().ChangingHolds(se.clock)[0].Since

			// Still not Ready: still held, and said every cycle.
			for i := range 4 {
				lines := logs.FilterMessage(heldLine).Len()
				if a := targetOf(t, cycle(), "ns/A-v"); a != 9 {
					t.Fatalf("A lowered to %d with its pod still not Ready", a)
				}
				if logs.FilterMessage(heldLine).Len() != lines+1 {
					t.Fatalf("held cycle %d logged no reason at the default level", i+1)
				}
				for _, m := range []string{"A", "B"} {
					if !slices.Contains(reasonModels(t, reg, constants.ScalingBlockedDonorChanging), m) {
						t.Fatalf("held cycle %d: %s does not carry %s (carrying it: %v)", i+1, m,
							constants.ScalingBlockedDonorChanging, reasonModels(t, reg, constants.ScalingBlockedDonorChanging))
					}
				}
			}

			// Ready for one cycle, then not Ready again: the stable count
			// starts over, and A is still held.
			setA4Ready(t, c, true)
			if a := targetOf(t, cycle(), "ns/A-v"); a != 9 {
				t.Fatalf("A lowered to %d after one stable cycle of two", a)
			}
			setA4Ready(t, c, false)
			for range 2 {
				if a := targetOf(t, cycle(), "ns/A-v"); a != 9 {
					t.Fatalf("A lowered to %d with its pod not Ready again", a)
				}
				if h := ledger().ChangingHolds(se.clock); len(h) != 1 || h[0].SteadyCycles != 0 {
					t.Fatalf("after the pod went not Ready again: holds %+v, want A held with the count reset", h)
				}
			}

			// Ready: released after the confirm cycles, far inside the
			// release timeout, and A gives.
			setA4Ready(t, c, true)
			ready := se.clock
			gave := false
			for range 6 {
				if targetOf(t, cycle(), "ns/A-v") < 9 {
					gave = true
					break
				}
			}
			if !gave {
				// Say when it does give, if ever: at the release timeout is
				// the fixed hold this test guards against.
				waited := se.clock.Sub(ready)
				for range 40 {
					if targetOf(t, cycle(), "ns/A-v") < 9 {
						t.Fatalf("A still not giving %s after its pod became Ready; it gave %s after, %s after the hold began",
							waited, se.clock.Sub(ready), se.clock.Sub(since))
					}
				}
				t.Fatalf("A still not giving %s after its pod became Ready, nor 20 minutes later", waited)
			}
			if got := se.clock.Sub(ready); got > 2*time.Minute {
				t.Fatalf("A gave %s after its pod became Ready, want within the confirm cycles", got)
			}
			t.Logf("A gave %s after its pod became Ready, held %s in all", se.clock.Sub(ready), se.clock.Sub(since))
			if got := reasonModels(t, reg, constants.ScalingBlockedDonorChanging); len(got) != 0 {
				t.Fatalf("%s still set on %v after the hold ended", constants.ScalingBlockedDonorChanging, got)
			}
		})
	}
}
