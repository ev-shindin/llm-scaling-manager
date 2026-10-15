package steadystate

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

var _ = Describe("borrowing governor, engine side", func() {
	ctx := context.Background()
	now := time.Unix(90000, 0)

	// borrowing is namespace "b" borrowing in cohort "c", nothing waiting.
	borrowing := func() []*allocation.ResourceConstraints {
		return []*allocation.ResourceConstraints{{
			ProviderName:     "kq",
			NamespacePools:   map[string]map[string]allocation.ResourcePool{"b": {"H100": {Limit: 0, Used: 16}}},
			NamespaceBorrow:  map[string]map[string]allocation.NamespaceBorrow{"b": {"H100": {Cap: -1}}},
			NamespaceCohorts: map[string][]string{"b": {"c"}},
			SharedPools: []allocation.SharedPoolChunk{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
				FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: "H100"}, Free: 8, ReadAt: now}},
		}}
	}
	reqs := []allocation.ModelScalingRequest{{ModelID: "m", Namespace: "b", VariantStates: []domain.VariantReplicaState{
		{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 8, AcceleratorName: "H100"}}}}

	newClient := func(failWorkloads bool, objs ...client.Object) client.Client {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: kueue.Group, Version: "v1beta2"}, {Version: "v1"}})
		mapper.Add(schema.GroupVersionKind{Group: kueue.Group, Version: "v1beta2", Kind: kueue.KindWorkload}, meta.RESTScopeNamespace)
		mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
		mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Event"}, meta.RESTScopeNamespace)
		b := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objs...)
		if failWorkloads {
			b = b.WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == kueue.KindWorkload+"List" {
					return errors.New(`workloads.kueue.x-k8s.io is forbidden`)
				}
				return cl.List(ctx, list, opts...)
			}})
		}
		return b.Build()
	}
	workload := func(name string, at time.Time, owners ...metav1.OwnerReference) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]interface{}{
			"status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
				"type": "Evicted", "status": "True", "reason": "Preempted",
				"lastTransitionTime": at.UTC().Format(time.RFC3339),
			}}},
		}}
		u.SetAPIVersion(kueue.Group + "/v1beta2")
		u.SetKind(kueue.KindWorkload)
		u.SetNamespace("b")
		u.SetName(name)
		u.SetUID(types.UID("wl-" + name))
		u.SetOwnerReferences(owners)
		return u
	}
	deployment := map[string]scaletarget.ScaleTargetAccessor{"b/v": scaletarget.NewDeploymentAccessor(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "b"}})}
	cycle := func(c client.Client, targets map[string]scaletarget.ScaleTargetAccessor) *Engine {
		e := &Engine{Config: config.NewTestConfig(), client: c, Recorder: record.NewFakeRecorder(10)}
		e.prepareBorrowing(ctx, borrowing(), reqs, targets, now)
		return e
	}

	Describe("Kueue preemptions", func() {
		It("matches a LeaderWorkerSet's Workload by the set's own name", func() {
			lws := scaletarget.NewLWSAccessor(&lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "b", UID: "lws-uid"}})
			e := cycle(newClient(false, workload("w1", now.Add(-time.Minute),
				metav1.OwnerReference{APIVersion: "leaderworkerset.x-k8s.io/v1", Kind: "LeaderWorkerSet", Name: "v", UID: "lws-uid"})),
				map[string]scaletarget.ScaleTargetAccessor{"b/v": lws})
			Expect(e.borrowing.governor.BorrowBlocked("b", "m", "v", now)).To(BeTrue())
		})

		It("sees a preempted pod through Kueue's TerminationTarget condition, its Workload already gone", func() {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "v-abc-1", Namespace: "b", UID: "pod-1"},
				Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: "TerminationTarget", Status: corev1.ConditionTrue,
					Reason: "WorkloadEvictedDueToPreempted", LastTransitionTime: metav1.NewTime(now.Add(-time.Minute)),
					Message: "Preempted to accommodate a workload due to reclamation within the cohort"}}}}
			e := cycle(newClient(false, pod), deployment)
			Expect(e.borrowing.governor.BorrowBlocked("b", "m", "v", now)).To(BeTrue())
			Expect(e.borrowing.reclaimedUntil).To(HaveKey("b/m"), "lender-reclaimed holds")
		})

		// Review finding: the namespace counted as read before its pod list,
		// so a failed pod list pruned a preemption seen only through its pod,
		// and the next cycle counted it again.
		It("keeps a preemption seen only through its pod when a later pod list fails", func() {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "v-abc-1", Namespace: "b", UID: "pod-1"},
				Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: "TerminationTarget", Status: corev1.ConditionTrue,
					Reason: "WorkloadEvictedDueToPreempted", LastTransitionTime: metav1.NewTime(now.Add(-time.Minute))}}}}
			failPods := false
			scheme := runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: kueue.Group, Version: "v1beta2"}, {Version: "v1"}})
			mapper.Add(schema.GroupVersionKind{Group: kueue.Group, Version: "v1beta2", Kind: kueue.KindWorkload}, meta.RESTScopeNamespace)
			mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
			c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(pod).
				WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.PodList); ok && failPods {
						return errors.New("pod cache not synced")
					}
					return cl.List(ctx, list, opts...)
				}}).Build()
			e := cycle(c, deployment)
			Expect(e.borrowing.preemptSeen).To(HaveKey(types.UID("pod-1")))

			failPods = true
			e.prepareBorrowing(ctx, borrowing(), reqs, deployment, now.Add(30*time.Second))
			Expect(e.borrowing.preemptSeen).To(HaveKey(types.UID("pod-1")),
				"not pruned on a cycle that could not list the pods: the next one would count it again")
		})

		It("cools down from when Kueue preempted, not from when the scaling manager noticed", func() {
			e := cycle(newClient(false, workload("w1", now.Add(-9*time.Minute),
				metav1.OwnerReference{APIVersion: "v1", Kind: "Pod", Name: "v-abc-1", UID: "pod-1"})), deployment)
			Expect(e.borrowing.governor.BorrowBlocked("b", "m", "v", now)).To(BeTrue())
			Expect(e.borrowing.governor.BorrowBlocked("b", "m", "v", now.Add(2*time.Minute))).To(BeFalse(),
				"preempted 9 minutes ago: one minute of a 10-minute cooldown is left")
		})

		It("withholds new borrowing in a namespace whose preemptions cannot be read", func() {
			e := cycle(newClient(true), deployment)
			Expect(e.borrowing.governor.BorrowBlocked("b", "m", "v", now)).To(BeTrue())
		})
	})

	It("keeps a release in force through a cycle that counts no borrowing", func() {
		e := &Engine{Config: config.NewTestConfig(), Recorder: record.NewFakeRecorder(10)}
		unplaced := []allocation.ModelScalingRequest{{ModelID: "m", Namespace: "b", VariantStates: []domain.VariantReplicaState{
			{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 8, AcceleratorName: "H100",
				UnplacedReplicas: 1, UnplacedSince: now.Add(-6 * time.Minute), UnplacedPods: []string{"v-abc-1"}}}}}
		decisions := func(target int) []domain.VariantDecision {
			return []domain.VariantDecision{{Namespace: "b", ModelID: "m", VariantName: "v", AcceleratorName: "H100",
				CurrentReplicas: 2, TargetReplicas: target, GPUsPerReplica: 8}}
		}
		before := now.Add(-6 * time.Minute) // the namespace is seen past guaranteed
		e.prepareBorrowing(ctx, borrowing(), unplaced, deployment, before)
		e.releaseBorrowed(ctx, unplaced, decisions(2), deployment, before)
		e.prepareBorrowing(ctx, borrowing(), unplaced, deployment, now)
		ds := decisions(2)
		e.releaseBorrowed(ctx, unplaced, ds, deployment, now)
		Expect(ds[0].TargetReplicas).To(Equal(1), "released after the placement timeout")

		later := now.Add(30 * time.Second)
		e.prepareBorrowing(ctx, nil, unplaced, deployment, later) // a Kueue read failed: no borrowing this cycle
		ds = decisions(3)                                         // the demand floor asks for more
		e.releaseBorrowed(ctx, unplaced, ds, deployment, later)
		Expect(ds[0].TargetReplicas).To(Equal(1), "the release stays in force")
	})
})
