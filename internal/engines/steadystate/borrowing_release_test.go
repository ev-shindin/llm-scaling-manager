package steadystate

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controllerstatus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

var _ = Describe("giving borrowed capacity back, engine side", func() {
	ctx := context.Background()
	now := time.Unix(50000, 0)

	It("checks that a LeaderWorkerSet's highest group index is its newest group", func() {
		group := func(idx string, age time.Duration) corev1.Pod {
			return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{lwsv1.GroupIndexLabelKey: idx},
				CreationTimestamp: metav1.NewTime(now.Add(-age))}}
		}
		_, _, ok := lwsHighestIsNewest([]corev1.Pod{group("0", time.Hour), group("1", time.Minute)})
		Expect(ok).To(BeTrue())
		highest, newest, ok := lwsHighestIsNewest([]corev1.Pod{group("0", time.Minute), group("1", time.Hour)})
		Expect(ok).To(BeFalse(), "group 0 was recreated after a preemption: scale-down removes group 1, not the borrowed one")
		Expect(highest).To(Equal(1))
		Expect(newest).To(Equal(0))
	})

	Describe("the cluster-autoscaler wait", func() {
		event := func(name, pod, reason string, at time.Time) *unstructured.Unstructured {
			u := &unstructured.Unstructured{Object: map[string]interface{}{
				"involvedObject": map[string]interface{}{"kind": "Pod", "name": pod},
				"reason":         reason,
				"lastTimestamp":  at.UTC().Format(time.RFC3339),
			}}
			u.SetAPIVersion("v1")
			u.SetKind("Event")
			u.SetNamespace("b")
			u.SetName(name)
			return u
		}
		reqs := []allocation.ModelScalingRequest{{ModelID: "m", Namespace: "b", VariantStates: []domain.VariantReplicaState{{
			VariantName: "v", UnplacedReplicas: 1, UnplacedSince: now.Add(-6 * time.Minute), UnplacedPods: []string{"v-1"}}}}}
		pending := func(objs ...client.Object) map[string]bool {
			scheme := runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			e := &Engine{Config: config.NewTestConfig(), client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()}
			return e.scaleUpPending(ctx, reqs, now)
		}

		It("waits while a TriggeredScaleUp is not followed by a NotTriggerScaleUp", func() {
			Expect(pending(event("e1", "v-1", eventTriggeredScaleUp, now.Add(-5*time.Minute)))).To(HaveKeyWithValue("b/v", true))
			Expect(pending(
				event("e1", "v-1", eventTriggeredScaleUp, now.Add(-5*time.Minute)),
				event("e2", "v-1", eventNotTriggerScaleUp, now.Add(-time.Minute)),
			)).To(BeEmpty(), "a later NotTriggerScaleUp ends the wait at once")
			Expect(pending()).To(BeEmpty(), "a scale-up never triggered does not pause the timeout")
		})
	})

	It("persists the ledger through the status publisher and reads it back on start", func() {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		pub := controllerstatus.New(c, c, config.SystemNamespace())
		e := &Engine{Config: config.NewTestConfig(), client: c, Status: pub}
		e.borrowing.governor = allocation.NewBorrowGovernor(allocation.DefaultBorrowSettings())
		e.borrowing.governor.Restore(allocation.BorrowLedger{Entries: []allocation.ReleaseLedgerEntry{{
			LenderKey: allocation.LenderKey{Cohort: "c", Lender: "l", Flavor: "h100", Resource: "nvidia.com/gpu"}, ReleasedAt: now}}})
		e.persistBorrowLedger()
		raw := pub.Data()[controllerstatus.KeyBorrowReleaseLedger]
		Expect(raw).To(ContainSubstring(`"lender":"l"`))

		Expect(c.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: controllerstatus.ConfigMapName, Namespace: config.SystemNamespace()},
			Data: map[string]string{controllerstatus.KeyBorrowReleaseLedger: raw}})).To(Succeed())
		restarted := &Engine{Config: config.NewTestConfig(), client: c}
		restarted.borrowing.governor = allocation.NewBorrowGovernor(allocation.DefaultBorrowSettings())
		restarted.loadBorrowLedger(ctx)
		Expect(restarted.borrowing.governor.Ledger().Entries).To(HaveLen(1))
	})
})
