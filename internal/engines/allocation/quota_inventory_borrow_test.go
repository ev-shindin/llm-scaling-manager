package allocation

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

var _ = Describe("QuotaInventory borrowing log (read-only)", func() {
	const borrowMsg = "Kueue borrowing read and NOT applied"
	observed := time.Unix(1000, 0)

	entry := func(countBorrowable *bool) config.QuotaLimiterConfig {
		return config.QuotaLimiterConfig{
			Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
			NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
			Exclude:         []string{"ops"},
			Kueue:           &config.KueueQuotaSource{Enabled: true, CountBorrowable: countBorrowable},
		}
	}
	row := config.ExternalBorrow{
		Queue: "team-a-cq", Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
		Accelerator: "H100", Nominal: 9, Guaranteed: 0, Borrowable: 10, Reason: "nominal-lent-out",
	}
	snapshot := config.ExternalQuotas{
		Namespace: map[string]config.ExternalCaps{
			"team-a": {ByType: map[string]int{"H100": 9}},
			"ops":    {ByType: map[string]int{"H100": 4}},
		},
		Borrow: map[string][]config.ExternalBorrow{
			"team-a": {row},
			"ops":    {row},
		},
		ObservedAt: observed,
	}

	// capture returns a context whose logger appends every line to lines, as
	// JSON, the controller's production encoding.
	capture := func(lines *[]string) context.Context {
		return logr.NewContext(context.Background(), funcr.NewJSON(func(obj string) {
			*lines = append(*lines, obj)
		}, funcr.Options{}))
	}
	borrowLines := func(lines []string) []string {
		var out []string
		for _, l := range lines {
			if strings.Contains(l, borrowMsg) {
				out = append(out, l)
			}
		}
		return out
	}

	It("logs what borrowing would add, once per change, and applies none of it", func() {
		var lines []string
		ctx := capture(&lines)
		src := &fakeQuotaSource{}
		src.push(snapshot, nil)
		inv := NewQuotaInventoryWithSource(entry(nil), src)

		Expect(inv.Refresh(ctx)).To(Succeed())
		got := borrowLines(lines)
		Expect(got).To(HaveLen(1))
		Expect(got[0]).To(ContainSubstring(`"team-a"`))
		Expect(got[0]).To(ContainSubstring(`"borrowable":10`))
		Expect(got[0]).To(ContainSubstring(`"guaranteed":0`))
		Expect(got[0]).To(ContainSubstring(`"kueueBorrowed":0`))
		Expect(got[0]).To(ContainSubstring(`"reason":"nominal-lent-out"`))
		Expect(got[0]).NotTo(ContainSubstring(`"ops"`), "an excluded namespace is not governed by the entry")

		// Not applied: the bound is the nominal figure, exactly as without rows.
		Expect(inv.EffectiveConfig().NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 9}))

		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(borrowLines(lines)).To(HaveLen(1), "unchanged figures are not logged again")
	})

	It("logs a stale snapshot as withheld, back at nominal", func() {
		var lines []string
		ctx := capture(&lines)
		src := &fakeQuotaSource{}
		src.push(snapshot, nil)
		src.push(snapshot, errors.New("api down"))
		inv := NewQuotaInventoryWithSource(entry(nil), src)

		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(inv.Refresh(ctx)).To(Succeed())
		got := borrowLines(lines)
		Expect(got).To(HaveLen(2))
		Expect(got[1]).To(ContainSubstring(`"reason":"stale"`))
		Expect(got[1]).To(ContainSubstring(`"guaranteed":9`))
		Expect(got[1]).To(ContainSubstring(`"borrowable":0`))
		Expect(inv.EffectiveConfig().NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 9}))
	})

	It("logs nothing about borrowing with countBorrowable: false", func() {
		var lines []string
		ctx := capture(&lines)
		src := &fakeQuotaSource{}
		src.push(snapshot, nil)
		off := false
		inv := NewQuotaInventoryWithSource(entry(&off), src)

		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(borrowLines(lines)).To(BeEmpty())
		Expect(inv.EffectiveConfig().NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 9}))
	})

	It("keeps the external-caps line on its own fingerprint: borrowing churn does not repeat it", func() {
		var lines []string
		ctx := capture(&lines)
		src := &fakeQuotaSource{}
		src.push(snapshot, nil)
		churned := snapshot
		churned.Borrow = map[string][]config.ExternalBorrow{"team-a": {row.Withheld("no-status")}}
		src.push(churned, nil)
		inv := NewQuotaInventoryWithSource(entry(nil), src)

		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(inv.Refresh(ctx)).To(Succeed())
		capsLines := 0
		for _, l := range lines {
			if strings.Contains(l, "quota entry bounded by external caps") {
				capsLines++
			}
		}
		Expect(capsLines).To(Equal(1))
		Expect(borrowLines(lines)).To(HaveLen(2))
	})
})
