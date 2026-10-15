package steadystate

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
		sourced := func(name, pod, reason, component string, at time.Time) *unstructured.Unstructured {
			u := &unstructured.Unstructured{Object: map[string]interface{}{
				"involvedObject": map[string]interface{}{"kind": "Pod", "name": pod},
				"reason":         reason,
				"source":         map[string]interface{}{"component": component},
				"lastTimestamp":  at.UTC().Format(time.RFC3339),
			}}
			u.SetAPIVersion("v1")
			u.SetKind("Event")
			u.SetNamespace("b")
			u.SetName(name)
			return u
		}
		event := func(name, pod, reason string, at time.Time) *unstructured.Unstructured {
			return sourced(name, pod, reason, "cluster-autoscaler", at)
		}
		reqs := []allocation.ModelScalingRequest{{ModelID: "m", Namespace: "b", VariantStates: []domain.VariantReplicaState{{
			VariantName: "v", UnplacedReplicas: 1, UnplacedSince: now.Add(-6 * time.Minute), UnplacedPods: []string{"v-1"}}}}}
		var lists []*client.ListOptions
		var listErr error
		pending := func(objs ...client.Object) map[string]bool {
			scheme := runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			lists = nil
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
				WithIndex(&corev1.Event{}, "reason", func(o client.Object) []string { return []string{o.(*corev1.Event).Reason} }).
				WithIndex(&corev1.Event{}, "involvedObject.kind", func(o client.Object) []string {
					return []string{o.(*corev1.Event).InvolvedObject.Kind}
				}).
				WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					lo := &client.ListOptions{}
					lo.ApplyOptions(opts)
					lists = append(lists, lo)
					if listErr != nil {
						return listErr
					}
					return cl.List(ctx, list, opts...)
				}}).Build()
			e := &Engine{Config: config.NewTestConfig(), client: c}
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

		It("counts only the cluster autoscaler's Events, read by reason rather than the whole namespace", func() {
			Expect(pending(sourced("e1", "v-1", eventTriggeredScaleUp, "someone-else", now.Add(-5*time.Minute)))).To(BeEmpty(),
				"an Event another component wrote with the same reason does not pause the timeout")
			Expect(lists).NotTo(BeEmpty())
			for _, lo := range lists {
				Expect(lo.FieldSelector).NotTo(BeNil(), "every Event list is narrowed by a field selector")
				Expect(lo.FieldSelector.String()).To(ContainSubstring("involvedObject.kind=Pod"), "and to Events on pods")
			}
		})

		It("keeps the timeout running when the Events cannot be read", func() {
			listErr = errors.New("forbidden")
			defer func() { listErr = nil }()
			Expect(pending(event("e1", "v-1", eventTriggeredScaleUp, now.Add(-5*time.Minute)))).To(BeEmpty())
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
			LenderKey: allocation.LenderKey{Cohort: "c", Lender: "l", Flavor: "h100", Resource: "nvidia.com/gpu"}, ReleasedAt: now, Unknown: true}}})
		e.persistBorrowLedger(ctx)
		raw := pub.Data()[controllerstatus.KeyBorrowReleaseLedger]
		Expect(raw).To(ContainSubstring(`"lender":"l"`))

		Expect(c.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: controllerstatus.ConfigMapName, Namespace: config.SystemNamespace()},
			Data: map[string]string{controllerstatus.KeyBorrowReleaseLedger: raw}})).To(Succeed())
		restarted := &Engine{Config: config.NewTestConfig(), client: c}
		restarted.borrowing.governor = allocation.NewBorrowGovernor(allocation.DefaultBorrowSettings())
		restarted.loadBorrowLedger(ctx)
		Expect(restarted.borrowing.governor.Ledger().Entries).To(HaveLen(1))
	})

	// A restart while borrowing is not counted: the persisted pins hold, and
	// the persisted start of the no-borrowing window is not restarted.
	It("restores persisted pins in a first cycle that counts no borrowing, and only when there are some", func() {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		reqs := []allocation.ModelScalingRequest{{ModelID: "m", Namespace: "b", VariantStates: []domain.VariantReplicaState{
			{VariantName: "v", CurrentReplicas: 3, GPUsPerReplica: 8, AcceleratorName: "H100"}}}}
		decisions := func() []domain.VariantDecision {
			return []domain.VariantDecision{{Namespace: "b", ModelID: "m", VariantName: "v", AcceleratorName: "H100",
				CurrentReplicas: 3, TargetReplicas: 4, GPUsPerReplica: 8}}
		}
		start := func(ledger string) *Engine {
			b := fake.NewClientBuilder().WithScheme(scheme)
			if ledger != "" {
				b = b.WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: controllerstatus.ConfigMapName, Namespace: config.SystemNamespace()},
					Data: map[string]string{controllerstatus.KeyBorrowReleaseLedger: ledger}})
			}
			c := b.Build()
			return &Engine{Config: config.NewTestConfig(), client: c, Status: controllerstatus.New(c, c, config.SystemNamespace())}
		}

		e := start(`{"entries":[],"timeoutHeld":{"b/v":2},"idleSince":"` + now.Add(-5*time.Minute).UTC().Format(time.RFC3339) + `"}`)
		e.prepareBorrowing(ctx, nil, reqs, nil, now)
		ds := decisions()
		e.releaseBorrowed(ctx, reqs, ds, nil, now)
		Expect(ds[0].TargetReplicas).To(Equal(2), "the persisted placement-timeout hold survives the restart")

		window := allocation.DefaultBorrowSettings().Cooldown + allocation.DefaultBorrowSettings().ReleaseActuation
		later := now.Add(window - 4*time.Minute)
		ds = decisions()
		e.releaseBorrowed(ctx, reqs, ds, nil, later)
		Expect(ds[0].TargetReplicas).To(Equal(4), "the window counts from before the restart")

		none := start("")
		ds = decisions()
		none.releaseBorrowed(ctx, reqs, ds, nil, now)
		Expect(ds[0].TargetReplicas).To(Equal(4))
		Expect(none.borrowing.governor).To(BeNil(), "nothing persisted: no governor, the ledger left for the first borrowing cycle")
		Expect(none.borrowing.ledgerLoaded).To(BeFalse())
	})
})
