package steadystate

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controllerstatus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

var _ = Describe("restoring persisted pins after a restart", func() {
	ctx := context.Background()
	now := time.Unix(50000, 0)

	// Review finding (bug hunter + maintainer): restoreHeldReleases marked the
	// check done before the ledger read, so one transient ConfigMap read
	// failure was never retried: the persisted pins were lost for as long as
	// borrowing stayed off.
	It("reads the ledger again next cycle when the first read fails", func() {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		fail := true
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: controllerstatus.ConfigMapName, Namespace: config.SystemNamespace()},
				Data: map[string]string{controllerstatus.KeyBorrowReleaseLedger: `{"entries":[],"timeoutHeld":{"b/v":2}}`}}).
			WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if fail {
					return errors.New("etcdserver: request timed out")
				}
				return cl.Get(ctx, key, obj, opts...)
			}}).Build()
		e := &Engine{Config: config.NewTestConfig(), client: c, Status: controllerstatus.New(c, c, config.SystemNamespace())}
		reqs := []allocation.ModelScalingRequest{{ModelID: "m", Namespace: "b", VariantStates: []domain.VariantReplicaState{
			{VariantName: "v", CurrentReplicas: 3, GPUsPerReplica: 8, AcceleratorName: "H100"}}}}
		decisions := func() []domain.VariantDecision {
			return []domain.VariantDecision{{Namespace: "b", ModelID: "m", VariantName: "v", AcceleratorName: "H100",
				CurrentReplicas: 3, TargetReplicas: 4, GPUsPerReplica: 8}}
		}

		ds := decisions()
		e.releaseBorrowed(ctx, reqs, ds, nil, now)
		Expect(ds[0].TargetReplicas).To(Equal(4), "the ledger could not be read: nothing to pin yet")
		Expect(e.borrowing.governor).To(BeNil())

		fail = false
		ds = decisions()
		e.releaseBorrowed(ctx, reqs, ds, nil, now.Add(30*time.Second))
		Expect(ds[0].TargetReplicas).To(Equal(2), "read on the next cycle: the persisted hold is in force")
	})

	// Review finding (upgrade/rollback): a ledger a newer version wrote was
	// logged as an Error on every restart. It is discarded with one warning,
	// and the next persist replaces it, so the next restart reads it cleanly.
	It("discards a ledger of an unknown version with one warning, and replaces it", func() {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: controllerstatus.ConfigMapName, Namespace: config.SystemNamespace()},
				Data: map[string]string{controllerstatus.KeyBorrowReleaseLedger: `{"version":99,"entries":[],"timeoutHeld":{"b/v":2}}`}}).Build()
		var infos, errs []string
		logCtx := logr.NewContext(ctx, funcr.New(func(prefix, args string) {
			if strings.Contains(args, `"error"`) || strings.Contains(prefix, "error") {
				errs = append(errs, args)
				return
			}
			infos = append(infos, args)
		}, funcr.Options{}))
		pub := controllerstatus.New(c, c, config.SystemNamespace())
		e := &Engine{Config: config.NewTestConfig(), client: c, Status: pub}
		e.borrowing.governor = allocation.NewBorrowGovernor(allocation.DefaultBorrowSettings())
		e.loadBorrowLedger(logCtx)
		Expect(errs).To(BeEmpty(), "not an Error")
		Expect(infos).To(HaveLen(1))
		Expect(infos[0]).To(ContainSubstring("Warning: discarding a borrowing release ledger written by a newer version"))
		Expect(e.borrowing.governor.Ledger().TimeoutHeld).To(BeEmpty(), "nothing of it is restored")

		e.persistBorrowLedger(logCtx)
		Expect(pub.Data()[controllerstatus.KeyBorrowReleaseLedger]).To(HavePrefix(`{"version":1,`),
			"the next persist replaces it, so the next restart does not warn again")
	})
})
