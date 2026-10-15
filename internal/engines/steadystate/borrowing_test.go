package steadystate

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

var _ = Describe("cohort borrowing in the steady-state cycle", func() {
	ctx := context.Background()
	now := time.Unix(5000, 0)

	physicalCfg := func() *config.Config {
		cfg := config.NewTestConfig()
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			"default": {Limiters: []config.QuotaLimiterConfig{{Type: "gpu-inventory", Name: "gpu"}}},
		})
		return cfg
	}
	borrowing := func(free int) []*allocation.ResourceConstraints {
		return []*allocation.ResourceConstraints{{
			ProviderName:     "namespace-quota",
			NamespacePools:   map[string]map[string]allocation.ResourcePool{"a": {"H100": {Limit: 0}}},
			NamespaceBorrow:  map[string]map[string]allocation.NamespaceBorrow{"a": {"H100": {Cap: -1}}},
			NamespaceCohorts: map[string][]string{"a": {"c"}},
			SharedPools: []allocation.SharedPoolChunk{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
				FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: "H100"}, Free: free}},
		}}
	}
	physicalBound := func() *allocation.ResourceConstraints {
		return &allocation.ResourceConstraints{ProviderName: allocation.PhysicalLimiterName,
			Pools: map[string]allocation.ResourcePool{"H100": {Limit: 8, Used: 2}}}
	}
	nodeStore := func() *decision.NodeGPUStore {
		s := &decision.NodeGPUStore{}
		s.Publish(map[string]decision.NodeGPU{"n1": {Accelerator: "H100", Capacity: 8, Used: 2}}, now)
		return s
	}

	It("builds one node view for the cycle, which the share idle fill then reads", func() {
		e := &Engine{Config: physicalCfg()}
		e.borrowing.nodeStore = nodeStore()
		e.prepareBorrowing(ctx, append(borrowing(8), physicalBound()), nil, nil, now)
		Expect(e.borrowing.nodes).NotTo(BeNil())
		Expect(e.shareNodes(now)).To(Equal(e.borrowing.nodes.Snapshot()))
	})

	It("starts no new borrowing while the declared physical limiter is skipped, and resumes when it bounds again", func() {
		e := &Engine{Config: physicalCfg()}
		e.borrowing.nodeStore = nodeStore()
		// The physical provider could not answer this cycle (node read
		// denied, or its view unobserved): only the quota's constraints reach
		// the engine, and nothing bounds a borrowed replica by the GPUs that
		// exist.
		e.prepareBorrowing(ctx, borrowing(4), nil, nil, now)
		Expect(e.borrowing.nodes).To(BeNil())
		blocked := e.borrowBlocked()
		Expect(blocked).NotTo(BeNil())
		Expect(blocked("a", "m", "v")).To(BeTrue(), "a borrowed replica would be bounded by no GPU count")

		e.prepareBorrowing(ctx, append(borrowing(8), physicalBound()), nil, nil, now)
		Expect(e.borrowing.nodes).NotTo(BeNil())
		if blocked := e.borrowBlocked(); blocked != nil {
			Expect(blocked("a", "m", "v")).To(BeFalse(), "the physical limiter bounds again")
		}
	})

	It("builds no node view without borrowing, leaving the share on the store as before", func() {
		e := &Engine{Config: physicalCfg()}
		e.borrowing.nodeStore = nodeStore()
		plain := []*allocation.ResourceConstraints{{ProviderName: "gpu-limiter"}}
		Expect(e.prepareBorrowing(ctx, plain, nil, nil, now)).To(Equal(plain))
		Expect(e.borrowing.nodes).To(BeNil())
	})

	It("takes gated managed pods out of the namespace's cohort pool", func() {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		gpus := corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("6")}
		gated := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "a"},
			Spec: corev1.PodSpec{
				SchedulingGates: []corev1.PodSchedulingGate{{Name: kueue.AdmissionGate}},
				Containers:      []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: gpus, Limits: gpus}}},
			},
		}
		e := &Engine{Config: config.NewTestConfig(), client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(gated).Build()}
		got := e.prepareBorrowing(ctx, borrowing(8), nil, nil, now)
		Expect(got[0].SharedPools[0].Free).To(Equal(2))
	})
})
