package steadystate

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// The live-Kueue e2e found Kueue v0.20.0's preempted pod and its Workload both
// gone 1.4s after Kueue marked the pod, inside one 15s cycle, so listing
// either missed the preemption and its cooldown never started. The pod
// informer delivers every update and the final state on delete.
var _ = Describe("PreemptionWatch", func() {
	ctx := context.Background()
	now := time.Unix(90000, 0)
	at := metav1.NewTime(now.Add(-time.Minute))

	marked := func(name string, uid types.UID, labels map[string]string) *corev1.Pod {
		l := map[string]string{kueue.ManagedLabel: "true"}
		for k, v := range labels {
			l[k] = v
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "b", UID: uid, Labels: l,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "v-abc"}}},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: "TerminationTarget", Status: corev1.ConditionTrue,
				Reason: "WorkloadEvictedDueToPreempted", LastTransitionTime: at,
				Message: "Preempted to accommodate a workload due to reclamation within the cohort"}}},
		}
	}

	It("records a managed pod marked on an update", func() {
		w := NewPreemptionWatch(0)
		w.Handler().OnUpdate(nil, marked("v-abc-1", "pod-1", nil))
		got, dropped := w.drain()
		Expect(dropped).To(BeZero())
		Expect(got).To(HaveLen(1))
		Expect(got[0].Namespace).To(Equal("b"))
		Expect(got[0].UID).To(Equal(types.UID("pod-1")))
		Expect(got[0].At).To(BeTemporally("==", at.Time), "from when Kueue marked the pod")
		Expect(got[0].OwnerRefs).To(HaveLen(1))
		again, _ := w.drain()
		Expect(again).To(BeEmpty(), "a drain empties the buffer")
	})

	It("records a pod's final state on delete, and through a tombstone", func() {
		w := NewPreemptionWatch(0)
		w.Handler().OnDelete(marked("v-abc-1", "pod-1", nil))
		w.Handler().OnDelete(toolscache.DeletedFinalStateUnknown{Key: "b/v-abc-2", Obj: marked("v-abc-2", "pod-2", nil)})
		got, _ := w.drain()
		Expect(got).To(HaveLen(2))
		Expect(got[1].UID).To(Equal(types.UID("pod-2")))
	})

	It("ignores a pod Kueue does not manage, an unmarked pod, and a pod terminated for another reason", func() {
		w := NewPreemptionWatch(0)
		unmanaged := marked("x-1", "x-1", nil)
		delete(unmanaged.Labels, kueue.ManagedLabel)
		w.Handler().OnUpdate(nil, unmanaged)
		plain := marked("x-2", "x-2", nil)
		plain.Status.Conditions = nil
		w.Handler().OnDelete(plain)
		other := marked("x-3", "x-3", nil)
		other.Status.Conditions[0].Reason, other.Status.Conditions[0].Message = "StoppedByKueue", "stopped"
		w.Handler().OnUpdate(nil, other)
		w.Handler().OnDelete("not a pod")
		got, _ := w.drain()
		Expect(got).To(BeEmpty())
	})

	It("keeps one entry per pod, and drops the oldest past its bound", func() {
		w := NewPreemptionWatch(2)
		w.Handler().OnUpdate(nil, marked("p-1", "p-1", nil))
		w.Handler().OnUpdate(nil, marked("p-1", "p-1", nil))
		w.Handler().OnDelete(marked("p-1", "p-1", nil))
		w.Handler().OnUpdate(nil, marked("p-2", "p-2", nil))
		w.Handler().OnUpdate(nil, marked("p-3", "p-3", nil))
		got, dropped := w.drain()
		Expect(dropped).To(Equal(1))
		Expect([]types.UID{got[0].UID, got[1].UID}).To(Equal([]types.UID{"p-2", "p-3"}))
	})

	It("keys a pod group by its name, and a group's Workload the same way", func() {
		Expect(preemptionKey("b", "p-1", map[string]string{kueue.PodGroupNameLabel: "g"})).To(Equal("group/b/g"))
		Expect(preemptionKey("b", "p-1", nil)).To(Equal("pod/p-1"))
		Expect(workloadPreemptionKey("b", kueue.PreemptedWorkload{Name: "g", OwnerRefs: []metav1.OwnerReference{
			{Kind: "Pod", UID: "p-1"}, {Kind: "Pod", UID: "p-2"}}})).To(Equal("group/b/g"))
		Expect(workloadPreemptionKey("b", kueue.PreemptedWorkload{Name: "pod-x", OwnerRefs: []metav1.OwnerReference{
			{Kind: "Pod", UID: "p-1"}}})).To(Equal("pod/p-1"))
	})

	Describe("drained by the engine", func() {
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
		targets := map[string]scaletarget.ScaleTargetAccessor{"b/v": scaletarget.NewDeploymentAccessor(
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "b"}})}
		engine := func(w *PreemptionWatch) *Engine {
			scheme := runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: kueue.Group, Version: "v1beta2"}, {Version: "v1"}})
			mapper.Add(schema.GroupVersionKind{Group: kueue.Group, Version: "v1beta2", Kind: kueue.KindWorkload}, meta.RESTScopeNamespace)
			mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
			c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).Build()
			return &Engine{Config: config.NewTestConfig(), client: c, Recorder: record.NewFakeRecorder(10), Preemptions: w}
		}

		It("starts the cooldown from a preemption whose pod and Workload are both gone by the cycle", func() {
			w := NewPreemptionWatch(0)
			w.Handler().OnDelete(marked("v-abc-1", "pod-1", nil))
			e := engine(w)
			e.prepareBorrowing(ctx, borrowing(), reqs, targets, now)
			Expect(e.borrowing.governor.BorrowBlocked("b", "m", "v", now)).To(BeTrue(), "the cooldown started")
			Expect(e.borrowing.reclaimedUntil).To(HaveKey("b/m"))
			Expect(e.borrowing.governor.BorrowBlocked("b", "m", "v", now.Add(10*time.Minute))).To(BeFalse(),
				"from when Kueue preempted: one minute before now, so 9 minutes are left")
		})

		It("counts a pod seen by the watch and then again in a list once", func() {
			w := NewPreemptionWatch(0)
			w.Handler().OnUpdate(nil, marked("v-abc-1", "pod-1", nil))
			e := engine(w)
			e.prepareBorrowing(ctx, borrowing(), reqs, targets, now)
			Expect(e.borrowing.watchSeen).To(HaveKey(types.UID("pod-1")))
			w.Handler().OnDelete(marked("v-abc-1", "pod-1", nil))
			e.prepareBorrowing(ctx, borrowing(), reqs, targets, now.Add(15*time.Second))
			Expect(e.borrowing.preemptCounted).To(HaveLen(1))
		})

		// Live e2e: the preempted pod, terminating with its Workload gone,
		// withheld its own namespace (not-kueue-managed) for 30s. A preemption
		// drained in that window must still start the cooldown.
		It("starts the cooldown while the model's namespace is withheld this cycle", func() {
			w := NewPreemptionWatch(0)
			w.Handler().OnDelete(marked("v-abc-1", "pod-1", nil))
			e := engine(w)
			other := borrowing()
			other[0].NamespacePools = map[string]map[string]allocation.ResourcePool{"x": {"H100": {Limit: 0, Used: 16}}}
			other[0].NamespaceBorrow = map[string]map[string]allocation.NamespaceBorrow{"x": {"H100": {Cap: -1}}}
			other[0].NamespaceCohorts = map[string][]string{"x": {"c"}}
			e.prepareBorrowing(ctx, other, reqs, targets, now)
			Expect(e.borrowing.governor.BorrowBlocked("b", "m", "v", now)).To(BeTrue())
		})

		It("ignores a preempted pod of no known model", func() {
			w := NewPreemptionWatch(0)
			w.Handler().OnDelete(marked("other-abc-1", "pod-9", nil))
			e := engine(w)
			e.prepareBorrowing(ctx, borrowing(), reqs, targets, now)
			Expect(e.borrowing.governor.BorrowBlocked("b", "m", "v", now)).To(BeFalse())
		})
	})
})
