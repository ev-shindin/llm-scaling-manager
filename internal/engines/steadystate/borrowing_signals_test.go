package steadystate

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

var _ = Describe("what on-call sees of borrowing, engine side", func() {
	ctx := context.Background()
	t0 := time.Unix(90000, 0)
	var registry *prometheus.Registry

	BeforeEach(func() {
		registry = prometheus.NewRegistry()
		Expect(metrics.InitMetrics(registry)).To(Succeed())
	})

	// series is each series of name as its labels' values joined by "," ->
	// value.
	series := func(name string, labels ...string) map[string]float64 {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		out := map[string]float64{}
		for _, mf := range mfs {
			if mf.GetName() != name {
				continue
			}
			for _, m := range mf.GetMetric() {
				vals := make([]string, 0, len(labels))
				for _, want := range labels {
					for _, l := range m.GetLabel() {
						if l.GetName() == want {
							vals = append(vals, l.GetValue())
						}
					}
				}
				v := m.GetGauge().GetValue()
				if m.GetCounter() != nil {
					v = m.GetCounter().GetValue()
				}
				out[strings.Join(vals, ",")] = v
			}
		}
		return out
	}
	reasons := func() map[string]float64 {
		return series(constants.WVAModelScalingBlocked, constants.LabelModelName, constants.LabelReason)
	}

	// world: namespace "a" guaranteed 0, holding used GPUs, borrowing without
	// a cap in cohort "c" (8 free), whose lenders are given.
	world := func(used int, readAt time.Time, lenders ...kueue.LenderState) []*allocation.ResourceConstraints {
		return []*allocation.ResourceConstraints{{
			ProviderName:     "kq",
			NamespacePools:   map[string]map[string]allocation.ResourcePool{"a": {"H100": {Limit: 0, Used: used}}},
			NamespaceBorrow:  map[string]map[string]allocation.NamespaceBorrow{"a": {"H100": {Cap: -1, NominalLentOut: 4}}},
			NamespaceCohorts: map[string][]string{"a": {"c"}},
			SharedPools: []allocation.SharedPoolChunk{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
				FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: "H100"}, Free: 8, Lenders: lenders, ReadAt: readAt}},
		}}
	}
	model := func(unplacedSince time.Time) []allocation.ModelScalingRequest {
		st := domain.VariantReplicaState{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 8, AcceleratorName: "H100"}
		if !unplacedSince.IsZero() {
			st.UnplacedReplicas, st.UnplacedSince, st.UnplacedPods = 1, unplacedSince, []string{"v-abc-1"}
		}
		return []allocation.ModelScalingRequest{{ModelID: "m", Namespace: "a", VariantStates: []domain.VariantReplicaState{st}}}
	}
	decisionsFor := func() []domain.VariantDecision {
		return []domain.VariantDecision{{Namespace: "a", ModelID: "m", VariantName: "v", AcceleratorName: "H100",
			CurrentReplicas: 2, TargetReplicas: 2, GPUsPerReplica: 8}}
	}
	targets := map[string]scaletarget.ScaleTargetAccessor{"a/v": scaletarget.NewDeploymentAccessor(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "a"}})}

	It("on a placement-timeout release: an Event on the scale target, borrow-unplaced, the unplaced gauge, the budget", func() {
		rec := record.NewFakeRecorder(10)
		e := &Engine{Config: config.NewTestConfig(), Recorder: rec}
		now := t0.Add(6 * time.Minute)
		reqs := model(t0)
		cs := e.prepareBorrowing(ctx, world(16, now), reqs, targets, now)
		ds := decisionsFor()
		e.releaseBorrowed(ctx, ds, targets, now)
		Expect(ds[0].TargetReplicas).To(Equal(1))
		var ev string
		Expect(rec.Events).To(Receive(&ev))
		Expect(ev).To(HavePrefix("Warning " + constants.K8SEventBorrowedCapacityReleased))
		Expect(ev).To(ContainSubstring("from 2 to 1"))
		Expect(ev).To(ContainSubstring("placement-timeout"))
		Expect(rec.Events).NotTo(Receive(), "one Event per release record")

		e.publishBorrowSignals(cs, reqs, ds, now)
		Expect(reasons()).To(Equal(map[string]float64{"m,borrow-unplaced": 1}))
		Expect(series(constants.WVAKueueUnplacedPods, constants.LabelNamespace, constants.LabelVariantName)).To(
			Equal(map[string]float64{"a,v": 1}))
		Expect(series(constants.WVAQuotaLimitGPUs, constants.LabelLimiterName, constants.LabelNamespace, constants.LabelTier)).To(
			Equal(map[string]float64{"kq,a,guaranteed": 0, "kq,a,borrowable": 8}))
		Expect(e.borrowing.budgets.Entries).To(HaveLen(1))
		Expect(e.borrowing.budgets.Entries[0].Used).To(Equal(16))
	})

	It("on a release to a waiting lender: the Event names the ClusterQueue, borrow-released, the lender's wait", func() {
		rec := record.NewFakeRecorder(10)
		e := &Engine{Config: config.NewTestConfig(), Recorder: rec}
		lender := kueue.LenderState{Queue: "team-b-cq", Waiting: true, LentOut: 8}
		reqs := model(time.Time{})
		var cs []*allocation.ResourceConstraints
		var ds []domain.VariantDecision
		now := t0
		for _, d := range []time.Duration{0, 30 * time.Second, 61 * time.Second} {
			now = t0.Add(d)
			cs = e.prepareBorrowing(ctx, world(16, now, lender), reqs, targets, now)
			ds = decisionsFor()
			e.releaseBorrowed(ctx, ds, targets, now)
		}
		var ev string
		Expect(rec.Events).To(Receive(&ev))
		Expect(ev).To(HavePrefix("Normal " + constants.K8SEventBorrowedCapacityReleased))
		Expect(ev).To(ContainSubstring("ClusterQueue team-b-cq in cohort c"))
		Expect(ev).To(ContainSubstring("lender-waiting"))

		e.publishBorrowSignals(cs, reqs, ds, now)
		Expect(reasons()).To(Equal(map[string]float64{"m,borrow-released": 1}))
		Expect(series(constants.WVAKueueLenderWaitingSeconds, constants.LabelCohort, constants.LabelClusterQueue)).To(
			Equal(map[string]float64{"c,team-b-cq": 61}))
		Expect(series(constants.WVAQuotaBorrowWithheld, constants.LabelNamespace, constants.LabelReason)).To(
			Equal(map[string]float64{"a,lender-waiting": 1}), "the cohort is closed to new borrowing")
	})

	It("counts a recent Kueue preemption and holds lender-reclaimed for the cooldown; an old one counts nothing", func() {
		e := &Engine{Config: config.NewTestConfig()}
		cs := e.prepareBorrowing(ctx, world(8, t0), model(time.Time{}), targets, t0)
		e.recordReclaim("a/m", kueue.PreemptedWorkload{Owners: []string{"v-abc-1", "v-abc-2"}, At: t0.Add(-time.Minute)}, t0)
		e.recordReclaim("a/old", kueue.PreemptedWorkload{Owners: []string{"x"}, At: t0.Add(-time.Hour)}, t0)
		Expect(series(constants.WVAKueuePreemptedPodsTotal, constants.LabelNamespace, constants.LabelModelName)).To(
			Equal(map[string]float64{"a,m": 2}))

		e.publishBorrowSignals(cs, model(time.Time{}), decisionsFor(), t0)
		Expect(reasons()).To(Equal(map[string]float64{"m,lender-reclaimed": 1}))
		e.publishBorrowSignals(cs, model(time.Time{}), decisionsFor(), t0.Add(10*time.Minute))
		Expect(reasons()).To(BeEmpty(), "the cooldown from the preemption ran out")
	})

	It("names nominal-lent-out only for a model the limiter capped", func() {
		e := &Engine{Config: config.NewTestConfig()}
		cs := e.prepareBorrowing(ctx, world(8, t0), model(time.Time{}), targets, t0)
		e.publishBorrowSignals(cs, model(time.Time{}), decisionsFor(), t0)
		Expect(reasons()).To(BeEmpty())
		ds := decisionsFor()
		ds[0].WasLimited = true
		e.publishBorrowSignals(cs, model(time.Time{}), ds, t0)
		Expect(reasons()).To(Equal(map[string]float64{"m,nominal-lent-out": 1}))
	})

	It("clears every borrowing series once the cycle borrows nothing", func() {
		e := &Engine{Config: config.NewTestConfig(), Recorder: record.NewFakeRecorder(10)}
		now := t0.Add(6 * time.Minute)
		reqs := model(t0)
		cs := e.prepareBorrowing(ctx, world(16, now), reqs, targets, now)
		ds := decisionsFor()
		e.releaseBorrowed(ctx, ds, targets, now)
		e.publishBorrowSignals(cs, reqs, ds, now)
		Expect(reasons()).NotTo(BeEmpty())

		plain := []*allocation.ResourceConstraints{{ProviderName: "gpu-limiter", PoolsClosed: true}}
		cs = e.prepareBorrowing(ctx, plain, reqs, targets, now)
		e.publishBorrowSignals(cs, reqs, decisionsFor(), now)
		Expect(reasons()).To(BeEmpty())
		Expect(series(constants.WVAKueueUnplacedPods, constants.LabelNamespace)).To(BeEmpty())
		Expect(series(constants.WVAQuotaLimitGPUs, constants.LabelNamespace)).To(BeEmpty())
		Expect(e.borrowing.budgets.Entries).To(BeEmpty())
	})
})
