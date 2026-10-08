package warmpool

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// A pool publishes the part of its target it does not hold yet, which the
// utilization-share optimizer keeps free for it (proposal section 7.2): its
// size after the contention hold, before the headroom cap. A pool held at its
// size while a model is denied GPUs publishes nothing -- it yields to models.
func TestAPoolPublishesItsUnheldTarget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		contended bool
		want      int
	}{
		{"short of its target", false, 2},
		{"contended: asks for nothing more", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := decision.DefaultWarmPoolUnheld
			decision.DefaultWarmPoolUnheld = &decision.WarmPoolUnheldStore{}
			t.Cleanup(func() { decision.DefaultWarmPoolUnheld = saved })

			// One 2-GPU Pod held, a reserve of one: the pool wants two Pods,
			// four GPUs, and holds two.
			p := &fakePool{memberships: []pool.Membership{{Pod: podA(), State: pool.Absent, Pool: "sized",
				Capacity: pool.PodCapacity{GPUs: 2, Accelerator: "A100"}}}}
			cfg := testConfig()
			cfg.SleepMinSize = 1
			r := New(p, &staticDemand{}, cfg)
			r.Namespace = poolNamespace
			r.Pools = fakePools{{Name: "sized", Config: cfg, Replicas: 1, Deployment: "wva-warm-pool"}}
			r.PublishSize = func(string, string, int32) {}
			r.Contended = func(string, string) bool { return tc.contended }
			if _, err := r.Once(context.Background()); err != nil {
				t.Fatalf("Once: %v", err)
			}
			got := decision.DefaultWarmPoolUnheld.Latest(time.Hour, time.Now())[poolNamespace]["A100"]
			if got != tc.want {
				t.Fatalf("unheld A100 = %d, want %d", got, tc.want)
			}
		})
	}
}

// A pass with nothing short clears the namespace's figure, so GPUs stop being
// held free the moment the pool no longer wants them.
func TestAPoolAtItsTargetClearsItsCarveOut(t *testing.T) {
	saved := decision.DefaultWarmPoolUnheld
	decision.DefaultWarmPoolUnheld = &decision.WarmPoolUnheldStore{}
	t.Cleanup(func() { decision.DefaultWarmPoolUnheld = saved })
	decision.DefaultWarmPoolUnheld.Publish(poolNamespace, map[string]int{"A100": 8}, time.Now())

	p := &fakePool{memberships: []pool.Membership{
		{Pod: podA(), State: pool.Absent, Pool: "sized", Capacity: pool.PodCapacity{GPUs: 2, Accelerator: "A100"}},
		{Pod: types.NamespacedName{Namespace: podA().Namespace, Name: "pod-b"}, State: pool.Absent, Pool: "sized", Capacity: pool.PodCapacity{GPUs: 2, Accelerator: "A100"}},
	}}
	cfg := testConfig()
	cfg.SleepMinSize = 1
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{{Name: "sized", Config: cfg, Replicas: 2, Deployment: "wva-warm-pool"}}
	r.PublishSize = func(string, string, int32) {}
	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := decision.DefaultWarmPoolUnheld.Latest(time.Hour, time.Now()); len(got) != 0 {
		t.Fatalf("a pool holding its target still carves %v", got)
	}
}
