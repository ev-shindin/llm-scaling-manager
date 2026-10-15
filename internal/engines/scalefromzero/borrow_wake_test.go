package scalefromzero

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// borrowingProvider is a namespace-scope quota counting cohort borrowing for
// "chat": the guaranteed and within-nominal H100 figures and usage given, no
// cap on borrowing, and a cohort pool of 8 free GPUs that idle lenders left.
type borrowingProvider struct{ guaranteed, withinNominal, used int }

func (borrowingProvider) Name() string { return "tenant-quota" }
func (p borrowingProvider) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	return &allocation.ResourceConstraints{
		ProviderName:   "tenant-quota",
		Pools:          map[string]allocation.ResourcePool{"H100": {Limit: config.QuotaUnlimited}},
		NamespacePools: map[string]map[string]allocation.ResourcePool{"chat": {"H100": {Limit: p.guaranteed, Used: p.used}}},
		NamespaceBorrow: map[string]map[string]allocation.NamespaceBorrow{
			"chat": {"H100": {Cap: config.QuotaUnlimited, WithinNominal: p.withinNominal}},
		},
		NamespaceCohorts: map[string][]string{"chat": {"c"}},
		SharedPools: []allocation.SharedPoolChunk{{
			Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", Free: 8,
			FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: "H100"},
			Lenders: []kueue.LenderState{
				{Queue: "idle-a", CanReclaim: true, LentOut: 4},
				{Queue: "idle-b", CanReclaim: false, LentOut: 4},
			},
		}},
	}, nil
}

// A wake spends within nominal only (B7). With the namespace's nominal spent,
// 8 GPUs free in its cohort because lenders are idle are borrowed capacity:
// a model woken onto them can be preempted before it serves, so it must not
// wake. The control, one GPU of nominal left, wakes.
func TestWakeDoesNotBorrowFromIdleLenders(t *testing.T) {
	ctx := context.Background()
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 4}, nil)

	for _, tc := range []struct {
		name string
		p    borrowingProvider
		want bool
	}{
		{"guaranteed and nominal full: no wake onto idle lenders' GPUs", borrowingProvider{4, 4, 4}, false},
		{"guaranteed full below a full nominal: no wake", borrowingProvider{2, 4, 4}, false},
		{"control: one GPU of nominal left wakes", borrowingProvider{4, 4, 3}, true},
		// Kueue's default (Never) gives an idle queue a guaranteed figure of 0;
		// its nominal must still wake it.
		{"control: guaranteed 0 with nominal room wakes", borrowingProvider{0, 4, 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{gpuLimiter: tc.p}
			c := e.gpuConstraints(ctx, "chat")
			if len(c) == 0 {
				t.Fatal("premise: the borrowing provider must bound the wake")
			}
			if got := allocation.FitsGPUBudget(c, "chat", map[string]int{"H100": 1}); got != tc.want {
				t.Errorf("wake fits = %t, want %t", got, tc.want)
			}
		})
	}
}

// reportUnchecked fires on a change of the reason, not on every tick, and not
// at all while a young snapshot still checks the wake.
func TestReportUncheckedThrottlesOnChange(t *testing.T) {
	var logged []string
	ctx := log.IntoContext(context.Background(),
		funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{Verbosity: 0}))
	count := func() int {
		n := 0
		for _, l := range logged {
			if strings.Contains(l, "WITHOUT a GPU capacity check") {
				n++
			}
		}
		return n
	}

	e := &Engine{}
	e.reportUnchecked(ctx, "chat", "reason one")
	e.reportUnchecked(ctx, "chat", "reason one")
	if got := count(); got != 1 {
		t.Errorf("the same reason twice logged %d lines, want 1", got)
	}
	e.reportUnchecked(ctx, "chat", "reason two")
	if got := count(); got != 2 {
		t.Errorf("a new reason logged %d lines in all, want 2", got)
	}
	e.reportUnchecked(ctx, "other", "reason two")
	if got := count(); got != 3 {
		t.Errorf("another namespace logged %d lines in all, want 3: the throttle is per namespace", got)
	}

	// Every provider failing, but a young snapshot checks the wake: no report.
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 1}, nil)
	fail := false
	f := &Engine{gpuLimiter: flakyProvider{name: "gpu-limiter", fail: &fail,
		pools: map[string]allocation.ResourcePool{"H100": {Limit: 8}}}}
	if got := f.gpuConstraints(ctx, "snap"); len(got) != 1 {
		t.Fatalf("setup: %+v", got)
	}
	fail = true
	before := count()
	if got := f.gpuConstraints(ctx, "snap"); len(got) != 1 {
		t.Fatalf("want the snapshot to check the wake, got %+v", got)
	}
	if count() != before {
		t.Error("a wake checked against the young snapshot was reported as unchecked")
	}
}
