package scalefromzero

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// nodeSelectorTarget builds a Deployment accessor selecting GPUs by product
// label, the way a real workload pins itself to an accelerator type.
func nodeSelectorTarget(productLabelValue string) scaletarget.ScaleTargetAccessor {
	return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{"nvidia.com/gpu.product": productLabelValue},
				},
			},
		},
	})
}

// TestCandidateAcceleratorIsDeclaredVerbatim: the candidate keeps the name the
// workload declares. Reconciling it with pool keys happens at lookup time
// (allocation.FitsGPUBudget), because normalizing here mangles short names with a
// hyphen — NormalizeAcceleratorName("Gaudi-2") is "2", which matches no pool.
func TestCandidateAcceleratorIsDeclaredVerbatim(t *testing.T) {
	for _, declared := range []string{"NVIDIA-A100-PCIE-80GB", "A100", "Gaudi-2"} {
		t.Run(declared, func(t *testing.T) {
			got := candidateAccelerator(&wvav1alpha1.VariantAutoscaling{}, nodeSelectorTarget(declared))
			if got != declared {
				t.Fatalf("candidateAccelerator(%q) = %q, want it unchanged", declared, got)
			}
		})
	}
}

// TestCandidateAcceleratorKeepsTheUnresolvedSentinel: normalization must not
// mangle the placeholder, or demandOf's IsAcceleratorResolved check stops
// recognising it and unresolved variants get charged to a pool named "unknown"
// that no provider publishes — denying every one of them.
func TestCandidateAcceleratorKeepsTheUnresolvedSentinel(t *testing.T) {
	// No nodeSelector and no VA label: the resolver falls back to the sentinel.
	got := candidateAccelerator(&wvav1alpha1.VariantAutoscaling{},
		scaletarget.NewDeploymentAccessor(&appsv1.Deployment{}))
	if got != constants.DefaultAcceleratorName {
		t.Fatalf("candidateAccelerator = %q, want the %q sentinel", got, constants.DefaultAcceleratorName)
	}
	if constants.IsAcceleratorResolved(got) {
		t.Fatalf("%q must still read as unresolved after normalization", got)
	}
}

func TestResolveVariantCost(t *testing.T) {
	// The default must match what every other consumer assumes for an unset
	// cost; ranking such a variant last instead inverts the project default and
	// makes a fleet that never sets variantCost cost-order by nothing.
	tests := []struct {
		name string
		spec string
		want float64
	}{
		{name: "declared cost is used", spec: "12.5", want: 12.5},
		{name: "absent cost falls back to the default", spec: "", want: 10.0},
		{name: "unparseable cost falls back to the default", spec: "not-a-number", want: 10.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va := wvav1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{Name: "v"},
				Spec: wvav1alpha1.VariantAutoscalingSpec{
					VariantAutoscalingConfigSpec: wvav1alpha1.VariantAutoscalingConfigSpec{VariantCost: tt.spec},
				},
			}
			if got := resolveVariantCost(context.Background(), va); got != tt.want {
				t.Fatalf("resolveVariantCost(%q) = %v, want %v", tt.spec, got, tt.want)
			}
		})
	}
}

// failingProvider errors on every ComputeConstraints call.
type failingProvider struct{ name string }

func (f failingProvider) Name() string { return f.name }
func (f failingProvider) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	return nil, errors.New("cluster unreachable")
}

// okProvider returns a fixed constraint, and records the usage it was fed so a
// test can assert WHICH measure reached it. The zero basis is PhysicalUsage,
// which is what an undeclared provider gets.
type okProvider struct {
	name  string
	pools map[string]allocation.ResourcePool
	basis allocation.UsageBasis
	fed   *map[string]int // optional; set to capture the usage handed over
}

func (o okProvider) Name() string                      { return o.name }
func (o okProvider) UsageBasis() allocation.UsageBasis { return o.basis }
func (o okProvider) ComputeConstraints(_ context.Context, usageByType map[string]int, _ map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	if o.fed != nil {
		*o.fed = usageByType
	}
	return &allocation.ResourceConstraints{ProviderName: o.name, Pools: o.pools}, nil
}

// flakyProvider answers like okProvider until *fail is set, then errors.
type flakyProvider struct {
	name  string
	fail  *bool
	pools map[string]allocation.ResourcePool
}

func (f flakyProvider) Name() string { return f.name }
func (f flakyProvider) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	if *f.fail {
		return nil, errors.New("node API unreachable")
	}
	return &allocation.ResourceConstraints{ProviderName: f.name, Pools: f.pools}, nil
}

// TestGPUConstraintsPosture pins what a wake is checked against when inputs go
// missing: the providers that can answer, then the recent last constraints, and
// only then nothing at all. "Unknown" must still not become "denied".
func TestGPUConstraintsPosture(t *testing.T) {
	ctx := context.Background()
	healthy := okProvider{name: "gpu-limiter", pools: map[string]allocation.ResourcePool{"H100": {Limit: 8}}}

	t.Run("no limiter means no capacity check", func(t *testing.T) {
		e := &Engine{}
		if got := e.gpuConstraints(ctx, "chat"); got != nil {
			t.Fatalf("gpuConstraints = %v, want nil when no limiter is injected", got)
		}
	})

	t.Run("no usage snapshot means no capacity check", func(t *testing.T) {
		decision.DefaultGPUUsage.Reset() // nothing published
		e := &Engine{gpuLimiter: healthy}
		if got := e.gpuConstraints(ctx, "chat"); got != nil {
			t.Fatalf("gpuConstraints = %v, want nil before any usage snapshot exists", got)
		}
	})

	// One missing input never lifts the others. This used to return nil -- a
	// quota whose read failed lifted the physical bound beside it. Constraints
	// intersect, so the survivor denies only what the full set would deny too.
	t.Run("a failing provider is skipped and the healthy one still bounds", func(t *testing.T) {
		decision.DefaultGPUUsage.Reset()
		decision.PublishGPUUsage(map[string]int{"H100": 1}, nil)

		e := &Engine{gpuLimiter: allocation.NewCompositeLimiter("composite",
			[]allocation.Limiter{healthy, failingProvider{name: "quota-limiter"}})}

		got := e.gpuConstraints(ctx, "chat")
		if len(got) != 1 || got[0].ProviderName != "gpu-limiter" {
			t.Fatalf("gpuConstraints = %+v, want the healthy provider to still bound the wake", got)
		}
	})

	t.Run("an unobserved view skips only the provider that needs it", func(t *testing.T) {
		decision.DefaultGPUUsage.Reset() // no physical view
		decision.DefaultManagedGPUUsage.Reset()
		defer decision.DefaultManagedGPUUsage.Reset()
		decision.PublishManagedGPUUsage(map[string]int{"H100": 1}, nil)

		e := &Engine{gpuLimiter: allocation.NewCompositeLimiter("composite", []allocation.Limiter{
			healthy, // physical basis, unobserved
			okProvider{name: "tenant-quota", basis: allocation.ManagedUsage,
				pools: map[string]allocation.ResourcePool{"H100": {Limit: 4}}},
		})}

		got := e.gpuConstraints(ctx, "chat")
		if len(got) != 1 || got[0].ProviderName != "tenant-quota" {
			t.Fatalf("gpuConstraints = %+v, want the quota to bound while the physical view is missing", got)
		}
	})

	// Every provider unavailable: the last successful constraints, while young,
	// still check the wake. An API blip of a few seconds must not turn a
	// placement decision into none at all.
	t.Run("every provider unavailable falls back to the recent last constraints", func(t *testing.T) {
		decision.DefaultGPUUsage.Reset()
		decision.PublishGPUUsage(map[string]int{"H100": 1}, nil)

		fail := false
		e := &Engine{gpuLimiter: flakyProvider{name: "gpu-limiter", fail: &fail,
			pools: map[string]allocation.ResourcePool{"H100": {Limit: 8}}}}
		if got := e.gpuConstraints(ctx, "chat"); len(got) != 1 {
			t.Fatalf("setup: gpuConstraints = %+v, want one constraint while healthy", got)
		}
		fail = true
		got := e.gpuConstraints(ctx, "chat")
		if len(got) != 1 || got[0].Pools["H100"].Limit != 8 {
			t.Fatalf("gpuConstraints = %+v, want the last successful constraints (< 5m old)", got)
		}
		if other := e.gpuConstraints(ctx, "other-ns"); other != nil {
			t.Fatalf("gpuConstraints(other-ns) = %+v; the snapshot is per namespace and other-ns has none", other)
		}
	})

	t.Run("healthy providers yield constraints", func(t *testing.T) {
		decision.DefaultGPUUsage.Reset()
		decision.PublishGPUUsage(map[string]int{"H100": 1}, nil)

		e := &Engine{gpuLimiter: healthy}
		got := e.gpuConstraints(ctx, "chat")
		if len(got) != 1 || got[0].ProviderName != "gpu-limiter" {
			t.Fatalf("gpuConstraints = %+v, want the single healthy provider's constraints", got)
		}
	})
}

// TestGPUConstraintsFeedsEachProviderItsOwnUsageBasis pins the split that a
// single broadcast figure got wrong.
//
// A physical inventory must see every GPU held on the nodes, because the
// scheduler will not place a variant on a device something else already holds. A
// quota must see only what WVA's own variants hold, because that is all an
// allowance granted to WVA can be spent on — charged the physical figure, an
// unrelated job in the same namespace exhausts the operator's WVA quota without
// WVA having placed a single replica, and every wake is refused.
func TestGPUConstraintsFeedsEachProviderItsOwnUsageBasis(t *testing.T) {
	decision.DefaultGPUUsage.Reset()
	decision.DefaultManagedGPUUsage.Reset()
	defer func() {
		decision.DefaultGPUUsage.Reset()
		decision.DefaultManagedGPUUsage.Reset()
	}()

	// 6 GPUs held on the nodes; 2 of them by WVA's own variants.
	decision.PublishGPUUsage(map[string]int{"A100": 6}, map[string]map[string]int{"chat": {"A100": 6}})
	decision.PublishManagedGPUUsage(map[string]int{"A100": 2}, map[string]map[string]int{"chat": {"A100": 2}})

	var physicalFed, quotaFed map[string]int
	e := &Engine{gpuLimiter: allocation.NewCompositeLimiter("composite", []allocation.Limiter{
		okProvider{name: "gpu-limiter", basis: allocation.PhysicalUsage, fed: &physicalFed},
		okProvider{name: "quota-limiter", basis: allocation.ManagedUsage, fed: &quotaFed},
	})}

	if got := e.gpuConstraints(context.Background(), "chat"); len(got) != 2 {
		t.Fatalf("gpuConstraints returned %d constraint(s), want both providers consulted", len(got))
	}
	if physicalFed["A100"] != 6 {
		t.Errorf("physical provider was fed %v, want every GPU held on the nodes (6)", physicalFed)
	}
	if quotaFed["A100"] != 2 {
		t.Errorf("quota provider was fed %v, want only what WVA holds (2); charging a quota for "+
			"workloads it does not govern exhausts the allowance with somebody else's GPUs", quotaFed)
	}
}

// TestGPUConstraintsMaterializesTheAskedNamespace: a namespace-scoped quota is
// only applied to namespaces present in the active-usage map, and that map comes
// from ACTIVE variants — so every namespace this engine asks about would be
// absent and its quota simply would not apply. The wake would then be judged
// against the cluster aggregate: able to exceed its own cap, or refused because a
// different namespace is full.
func TestGPUConstraintsMaterializesTheAskedNamespace(t *testing.T) {
	decision.DefaultGPUUsage.Reset()
	// Another namespace is active; ours is entirely parked, so it is absent.
	decision.PublishGPUUsage(
		map[string]int{"A100": 4},
		map[string]map[string]int{"other-ns": {"A100": 4}},
	)

	var seen map[string]map[string]int
	e := &Engine{gpuLimiter: capturingProvider{fn: func(byNS map[string]map[string]int) { seen = byNS }}}

	e.gpuConstraints(context.Background(), "parked-ns")

	if _, ok := seen["parked-ns"]; !ok {
		t.Fatalf("active namespaces = %v; the namespace being placed must be present, or its "+
			"namespace-scoped quota is never materialised", seen)
	}
	if got := len(seen["parked-ns"]); got != 0 {
		t.Fatalf("parked namespace usage = %v, want empty (present with zero usage)", seen["parked-ns"])
	}
	if _, ok := seen["other-ns"]; !ok {
		t.Fatal("the existing active namespaces must be preserved")
	}
}

// capturingProvider records the namespace usage map it is handed.
type capturingProvider struct {
	fn func(map[string]map[string]int)
}

func (c capturingProvider) Name() string { return "capturing" }
func (c capturingProvider) ComputeConstraints(_ context.Context, _ map[string]int, byNS map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	c.fn(byNS)
	return &allocation.ResourceConstraints{ProviderName: "capturing"}, nil
}
