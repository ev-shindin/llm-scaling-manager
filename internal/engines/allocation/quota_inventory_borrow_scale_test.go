package allocation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// Review finding (scale): the eligibility check ran for every namespace Kueue
// grants borrowing in -- every namespace with a LocalQueue on a cohort queue --
// though only the namespaces with managed variants are decided on.
var _ = Describe("QuotaInventory evaluates borrowing only where variants are managed", func() {
	observed := time.Unix(1000, 0)
	caps := func(n int) config.ExternalCaps { return config.ExternalCaps{ByType: map[string]int{"H100": n}} }
	row := config.ExternalBorrow{
		Queue: "q", Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
		Accelerator: "H100", Nominal: 8, Guaranteed: 0, Borrowable: 16, BorrowCap: 8, WithinNominal: 8,
		Reason: "nominal-lent-out",
	}
	snapshot := config.ExternalQuotas{
		Namespace:              map[string]config.ExternalCaps{"served": caps(8), "batch-1": caps(8), "batch-2": caps(8)},
		NamespaceGuaranteed:    map[string]config.ExternalCaps{"served": caps(0), "batch-1": caps(0), "batch-2": caps(0)},
		NamespaceWithinNominal: map[string]config.ExternalCaps{"served": caps(8), "batch-1": caps(8), "batch-2": caps(8)},
		Borrow:                 map[string][]config.ExternalBorrow{"served": {row}, "batch-1": {row}, "batch-2": {row}},
		SharedPools: []config.ExternalSharedPool{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
			Accelerator: "H100", Free: 16}},
		ObservedAt: observed,
	}
	entry := config.QuotaLimiterConfig{
		Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
		Kueue:           &config.KueueQuotaSource{Enabled: true},
	}

	It("asks the eligibility check about the namespaces of the cycle's variants only", func() {
		src := &fakeQuotaSource{}
		src.push(snapshot, nil)
		inv := NewQuotaInventoryWithSource(entry, src)
		inv.now = func() time.Time { return observed.Add(10 * time.Second) }
		elig := &recordingEligibility{}
		inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: elig})
		c, err := NewDefaultLimiter(entry.Name, inv).ComputeConstraints(context.Background(), nil,
			map[string]map[string]int{"served": {}})
		Expect(err).NotTo(HaveOccurred())
		Expect(elig.asked).To(Equal([]string{"served"}), "batch namespaces have no managed variant")
		Expect(c.NamespaceBorrow).To(HaveKey("served"))
	})
})
