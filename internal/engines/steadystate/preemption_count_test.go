package steadystate

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// Review finding (SRE): wva_kueue_preempted_pods_total counted one per pod a
// preempted Workload names as its owner, so one preempted pod group -- a
// LeaderWorkerSet group -- added its size, and WVAKueueBorrowThrashing (more
// than 3 an hour) fired after two group preemptions. One preempted Workload
// counts once.
var _ = Describe("counting Kueue preemptions", func() {
	ctx := context.Background()
	now := time.Unix(90000, 0)
	var registry *prometheus.Registry

	BeforeEach(func() {
		registry = prometheus.NewRegistry()
		Expect(metrics.InitMetrics(registry)).To(Succeed())
	})

	preempted := func() map[string]float64 {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		out := map[string]float64{}
		for _, mf := range mfs {
			if mf.GetName() != constants.WVAKueuePreemptedPodsTotal {
				continue
			}
			for _, m := range mf.GetMetric() {
				key := ""
				for _, l := range m.GetLabel() {
					if l.GetName() == constants.LabelModelName {
						key = l.GetValue()
					}
				}
				out[key] = m.GetCounter().GetValue()
			}
		}
		return out
	}

	It("counts a preempted two-pod group once, however many pods its Workload names", func() {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: kueue.Group, Version: "v1beta2"}, {Version: "v1"}})
		mapper.Add(schema.GroupVersionKind{Group: kueue.Group, Version: "v1beta2", Kind: kueue.KindWorkload}, meta.RESTScopeNamespace)
		mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
		wl := &unstructured.Unstructured{Object: map[string]interface{}{
			"status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
				"type": "Evicted", "status": "True", "reason": "Preempted",
				"lastTransitionTime": now.Add(-time.Minute).UTC().Format(time.RFC3339),
			}}},
		}}
		wl.SetAPIVersion(kueue.Group + "/v1beta2")
		wl.SetKind(kueue.KindWorkload)
		wl.SetNamespace("b")
		wl.SetName("group-1")
		wl.SetUID(types.UID("wl-group-1"))
		wl.SetOwnerReferences([]metav1.OwnerReference{
			{APIVersion: "v1", Kind: "Pod", Name: "v-abc-1", UID: "pod-1"},
			{APIVersion: "v1", Kind: "Pod", Name: "v-abc-2", UID: "pod-2"},
		})
		c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(client.Object(wl)).Build()

		e := &Engine{Config: config.NewTestConfig(), client: c, Recorder: record.NewFakeRecorder(10)}
		borrowing := []*allocation.ResourceConstraints{{
			ProviderName:     "kq",
			NamespacePools:   map[string]map[string]allocation.ResourcePool{"b": {"H100": {Limit: 0, Used: 16}}},
			NamespaceBorrow:  map[string]map[string]allocation.NamespaceBorrow{"b": {"H100": {Cap: -1}}},
			NamespaceCohorts: map[string][]string{"b": {"c"}},
			SharedPools: []allocation.SharedPoolChunk{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
				FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: "H100"}, Free: 8, ReadAt: now}},
		}}
		reqs := []allocation.ModelScalingRequest{{ModelID: "m", Namespace: "b", VariantStates: []domain.VariantReplicaState{
			{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 8, AcceleratorName: "H100"}}}}
		targets := map[string]scaletarget.ScaleTargetAccessor{"b/v": scaletarget.NewDeploymentAccessor(
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "b"}})}

		e.prepareBorrowing(ctx, borrowing, reqs, targets, now)
		Expect(preempted()).To(Equal(map[string]float64{"m": 1}), "one preempted Workload, not one per pod")
		e.prepareBorrowing(ctx, borrowing, reqs, targets, now.Add(15*time.Second))
		Expect(preempted()).To(Equal(map[string]float64{"m": 1}), "the same Workload listed again counts nothing")
	})
})
