package allocation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// recordingEligibility is fakeEligibility that also records which namespaces
// it was asked about.
type recordingEligibility struct {
	fakeEligibility
	asked []string
}

func (r *recordingEligibility) Evaluate(ctx context.Context, namespaces []string) map[string]kueue.NamespaceEligibility {
	r.asked = append(r.asked, namespaces...)
	return r.fakeEligibility.Evaluate(ctx, namespaces)
}

// A pilot: kueue.borrowNamespaces limits counting borrowing to the namespaces
// it lists. The entry is parsed from YAML, as an operator writes it.
var _ = Describe("QuotaInventory borrowNamespaces pilot", func() {
	observed := time.Unix(1000, 0)
	ctx := context.Background()

	entryYAML := func(pilot string) config.QuotaLimiterConfig {
		var e config.QuotaLimiterConfig
		src := "name: namespace-quota\ntype: quota\nscope: namespace\n" +
			"namespaceQuotas:\n  default:\n    H100: -1\n" +
			"kueue:\n  enabled: true\n" + pilot
		Expect(yaml.Unmarshal([]byte(src), &e)).To(Succeed())
		return e
	}
	row := func(queue string) config.ExternalBorrow {
		return config.ExternalBorrow{
			Queue: queue, Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
			Accelerator: "H100", Nominal: 8, Guaranteed: 0, Borrowable: 16, BorrowCap: 8, WithinNominal: 8,
			Reason: "nominal-lent-out",
		}
	}
	snapshot := func() config.ExternalQuotas {
		caps := func(n int) config.ExternalCaps { return config.ExternalCaps{ByType: map[string]int{"H100": n}} }
		return config.ExternalQuotas{
			Namespace:              map[string]config.ExternalCaps{"team-a": caps(8), "team-b": caps(8)},
			NamespaceGuaranteed:    map[string]config.ExternalCaps{"team-a": caps(0), "team-b": caps(0)},
			NamespaceWithinNominal: map[string]config.ExternalCaps{"team-a": caps(8), "team-b": caps(8)},
			Borrow:                 map[string][]config.ExternalBorrow{"team-a": {row("a-cq")}, "team-b": {row("b-cq")}},
			SharedPools: []config.ExternalSharedPool{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
				Accelerator: "H100", Free: 16}},
			ObservedAt: observed,
		}
	}
	run := func(e config.QuotaLimiterConfig) (*ResourceConstraints, *recordingEligibility) {
		src := &fakeQuotaSource{}
		src.push(snapshot(), nil)
		inv := NewQuotaInventoryWithSource(e, src)
		inv.now = func() time.Time { return observed.Add(10 * time.Second) }
		elig := &recordingEligibility{}
		inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: elig})
		c, err := NewDefaultLimiter(e.Name, inv).ComputeConstraints(ctx, nil,
			map[string]map[string]int{"team-a": {}, "team-b": {}})
		Expect(err).NotTo(HaveOccurred())
		return c, elig
	}

	It("counts every governed namespace without the list (the control)", func() {
		c, elig := run(entryYAML(""))
		Expect(c.NamespaceBorrow).To(HaveKey("team-a"))
		Expect(c.NamespaceBorrow).To(HaveKey("team-b"))
		Expect(elig.asked).To(ConsistOf("team-a", "team-b"))
	})

	It("counts only the listed namespaces and withholds the others as not-selected, unread", func() {
		c, elig := run(entryYAML("  borrowNamespaces: [team-b]\n"))
		Expect(c.NamespaceBorrow).NotTo(HaveKey("team-a"))
		Expect(c.NamespaceBorrow).To(HaveKey("team-b"))
		Expect(c.BorrowWithheld).To(HaveKeyWithValue("team-a", "not-selected"))
		Expect(elig.asked).To(Equal([]string{"team-b"}), "a namespace outside the pilot costs no eligibility read")
	})
})

var _ = Describe("kueue.borrowNamespaces validation", func() {
	validate := func(pilot string) error {
		var p config.QuotaLimiterEntries
		src := "limiters:\n- name: ns\n  type: quota\n  scope: namespace\n" +
			"  namespaceQuotas:\n    default:\n      H100: -1\n" +
			"  kueue:\n    enabled: true\n    borrowNamespaces: " + pilot + "\n"
		Expect(yaml.Unmarshal([]byte(src), &p)).To(Succeed())
		_, err := p.Validate()
		return err
	}

	It("accepts namespace names", func() {
		Expect(validate("[team-a, team-b]")).To(Succeed())
	})

	It("rejects a name that is no namespace, and a duplicate", func() {
		Expect(validate("[Team_A]")).To(MatchError(ContainSubstring("kueue.borrowNamespaces[0]")))
		Expect(validate("[team-a, team-a]")).To(MatchError(ContainSubstring("listed twice")))
	})
})
