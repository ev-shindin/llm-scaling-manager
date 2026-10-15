package metrics

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

var _ = Describe("Kueue borrowing series", func() {
	var registry *prometheus.Registry

	BeforeEach(func() {
		registry = prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
	})

	// rows is each series of name as "label=value,..." (sorted label order as
	// gathered) -> value.
	rows := func(name string, labels ...string) map[string]float64 {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		out := map[string]float64{}
		for _, mf := range mfs {
			if mf.GetName() != name {
				continue
			}
			for _, m := range mf.GetMetric() {
				parts := make([]string, 0, len(labels))
				for _, l := range labels {
					parts = append(parts, getLabelValue(m, l))
				}
				out[strings.Join(parts, ",")] = value(m)
			}
		}
		return out
	}

	It("publishes both tiers and the withheld reason, and clears what no longer holds", func() {
		PublishQuotaBudgets([]QuotaBudget{
			{Limiter: "kueue", Namespace: "a", Accelerator: "H100", Guaranteed: 8, Borrowable: 4},
			{Limiter: "kueue", Namespace: "b", Accelerator: "H100", Guaranteed: 0, Reason: "not-kueue-managed"},
			{Limiter: "kueue", Namespace: "c", Accelerator: "H100", Guaranteed: -1},
		})
		Expect(rows(constants.WVAQuotaLimitGPUs, constants.LabelNamespace, constants.LabelTier)).To(Equal(map[string]float64{
			"a,guaranteed": 8, "a,borrowable": 4, "b,guaranteed": 0, "b,borrowable": 0,
		}), "an unlimited guaranteed figure publishes nothing")
		Expect(rows(constants.WVAQuotaBorrowWithheld, constants.LabelLimiterName, constants.LabelNamespace,
			constants.LabelAccelerator, constants.LabelReason)).To(Equal(map[string]float64{
			"kueue,b,H100,not-kueue-managed": 1,
		}))

		// b's reason changes, a stops borrowing, and namespace c leaves.
		PublishQuotaBudgets([]QuotaBudget{
			{Limiter: "kueue", Namespace: "a", Accelerator: "H100", Guaranteed: 8, Reason: "cohort-pool-empty"},
			{Limiter: "kueue", Namespace: "b", Accelerator: "H100", Guaranteed: 2, Borrowable: 6},
		})
		Expect(rows(constants.WVAQuotaLimitGPUs, constants.LabelNamespace, constants.LabelTier)).To(Equal(map[string]float64{
			"a,guaranteed": 8, "a,borrowable": 0, "b,guaranteed": 2, "b,borrowable": 6,
		}))
		Expect(rows(constants.WVAQuotaBorrowWithheld, constants.LabelNamespace, constants.LabelReason)).To(Equal(map[string]float64{
			"a,cohort-pool-empty": 1,
		}), "the stale not-kueue-managed series is deleted, not left at 1")

		PublishQuotaBudgets(nil)
		Expect(rows(constants.WVAQuotaLimitGPUs, constants.LabelNamespace)).To(BeEmpty())
		Expect(rows(constants.WVAQuotaBorrowWithheld, constants.LabelNamespace)).To(BeEmpty())
	})

	It("publishes unplaced pods and waiting lenders only while they hold", func() {
		PublishKueueUnplacedPods([]UnplacedVariant{{Namespace: "a", Variant: "v1", Pods: 2}, {Namespace: "a", Variant: "v2"}})
		PublishKueueLenderWaiting([]WaitingLender{{Cohort: "c", ClusterQueue: "lender", Seconds: 700}})
		Expect(rows(constants.WVAKueueUnplacedPods, constants.LabelNamespace, constants.LabelVariantName)).To(Equal(map[string]float64{"a,v1": 2}))
		Expect(rows(constants.WVAKueueLenderWaitingSeconds, constants.LabelCohort, constants.LabelClusterQueue)).To(Equal(map[string]float64{"c,lender": 700}))

		PublishKueueUnplacedPods(nil)
		PublishKueueLenderWaiting(nil)
		Expect(rows(constants.WVAKueueUnplacedPods, constants.LabelNamespace)).To(BeEmpty())
		Expect(rows(constants.WVAKueueLenderWaitingSeconds, constants.LabelCohort)).To(BeEmpty())
	})

	It("counts preempted pods per model", func() {
		CountKueuePreemptedPods("a", "m", 2)
		CountKueuePreemptedPods("a", "m", 1)
		CountKueuePreemptedPods("a", "m", 0)
		Expect(rows(constants.WVAKueuePreemptedPodsTotal, constants.LabelNamespace, constants.LabelModelName)).To(Equal(map[string]float64{"a,m": 3}))
	})

	It("counts borrow releases per model and cause", func() {
		CountBorrowRelease("a", "m", "lender-waiting")
		CountBorrowRelease("a", "m", "lender-waiting")
		CountBorrowRelease("a", "m", "placement-timeout")
		Expect(rows(constants.WVABorrowReleasesTotal, constants.LabelNamespace, constants.LabelModelName, constants.LabelCause)).To(
			Equal(map[string]float64{"a,m,lender-waiting": 2, "a,m,placement-timeout": 1}))
	})

	It("registers the borrowing reasons as one producer's set", func() {
		Expect(constants.ScalingBlockedReasonsBorrowing).To(ConsistOf(
			"borrow-released", "lender-reclaimed", "borrow-unplaced", "nominal-lent-out"))
		for _, other := range [][]string{constants.ScalingBlockedReasonsPolicy, constants.ScalingBlockedReasonsWake,
			constants.ScalingBlockedReasonsUtilizationShare} {
			for _, r := range constants.ScalingBlockedReasonsBorrowing {
				Expect(other).NotTo(ContainElement(r), "a reason owned by two producers flaps")
			}
		}
	})
})

// value is a gauge's or a counter's value.
func value(m *dto.Metric) float64 {
	if m.GetCounter() != nil {
		return m.GetCounter().GetValue()
	}
	return m.GetGauge().GetValue()
}
