package scalefromzero

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// The last successful constraints check a wake only while young. Past
// lastConstraintsMaxAge the wake goes unchecked, as it always did: a stale
// bound refuses wakes for a reason that may no longer hold.
func TestGPUConstraintsSnapshotAgeBound(t *testing.T) {
	ctx := context.Background()
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 1}, nil)

	last := []*allocation.ResourceConstraints{{ProviderName: "gpu-limiter",
		Pools: map[string]allocation.ResourcePool{"H100": {Limit: 8}}}}

	for _, tc := range []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"just under the bound is used", lastConstraintsMaxAge - 10*time.Second, true},
		{"at the bound is not", lastConstraintsMaxAge, false},
		{"well past the bound is not", 2 * lastConstraintsMaxAge, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{gpuLimiter: failingProvider{name: "gpu-limiter"}}
			e.rememberConstraints("chat", last, time.Now().Add(-tc.age))
			got := e.gpuConstraints(ctx, "chat")
			if (got != nil) != tc.want {
				t.Fatalf("snapshot %s old: gpuConstraints = %+v, want used=%t", tc.age, got, tc.want)
			}
		})
	}
}

// Review finding: lastGood kept one entry per namespace forever and survived
// a limiter rebuild. An expired entry is dropped when read or when another is
// remembered, and a rebuild forgets them all.
func TestGPUConstraintsSnapshotsAreEvicted(t *testing.T) {
	last := []*allocation.ResourceConstraints{{ProviderName: "gpu-limiter"}}
	now := time.Now()
	e := &Engine{}
	e.rememberConstraints("old", last, now.Add(-2*lastConstraintsMaxAge))
	e.rememberConstraints("read", last, now.Add(-2*lastConstraintsMaxAge))
	if _, _, ok := e.lastConstraints("read", now); ok {
		t.Fatal("an expired snapshot was used")
	}
	if _, kept := e.lastGood["read"]; kept {
		t.Error("an expired snapshot was kept after it was read")
	}
	e.rememberConstraints("fresh", last, now)
	if _, kept := e.lastGood["old"]; kept {
		t.Error("an expired snapshot of a namespace never read again was kept")
	}

	e.config = config.NewTestConfig()
	e.limiterBuilder = func() (allocation.Limiter, error) { return failingProvider{name: "gpu-limiter"}, nil }
	e.limiterSig = "an older configuration"
	e.refreshLimiter(context.Background())
	if len(e.lastGood) != 0 {
		t.Errorf("snapshots of the replaced limiter survived the rebuild: %v", e.lastGood)
	}
}

// With every provider unavailable and no young snapshot, the unchecked report
// names each provider and why, so the cause is readable from one line.
func TestDescribeSkippedNamesEveryProvider(t *testing.T) {
	got := describeSkipped([]allocation.SkippedProvider{
		{Name: "gpu-limiter", Reason: "no cluster GPU-usage snapshot has been published yet"},
		{Name: "tenant-quota", Reason: "could not compute constraints: forbidden"},
	})
	for _, want := range []string{`"gpu-limiter"`, "no cluster GPU-usage snapshot", `"tenant-quota"`, "forbidden"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeSkipped = %q, missing %q", got, want)
		}
	}
}
