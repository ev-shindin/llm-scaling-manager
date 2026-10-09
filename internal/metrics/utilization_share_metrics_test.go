package metrics

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

var _ = Describe("PublishUtilizationShare", func() {
	family := func(registry *prometheus.Registry, name string) []*dto.Metric {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		for _, mf := range mfs {
			if mf.GetName() == name {
				return mf.GetMetric()
			}
		}
		return nil
	}

	group := func(roles ...UtilizationShareRole) UtilizationShareGroup {
		return UtilizationShareGroup{
			AcceleratorType: "H200",
			Scope:           constants.UtilizationShareClusterScope,
			SpareGPUs:       6,
			ReplicasToMove:  3,
			Roles:           roles,
		}
	}

	It("publishes a role's headroom, target and actionable flag, and the group's spare", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		PublishUtilizationShare([]UtilizationShareGroup{group(
			UtilizationShareRole{Namespace: "ns", ModelName: "m", Role: "decode", Headroom: 0.4, TargetGPUs: 8.4, Actionable: true},
		)})

		h := family(registry, constants.WVAUtilizationShareHeadroom)
		Expect(h).To(HaveLen(1))
		Expect(h[0].GetGauge().GetValue()).To(Equal(0.4))
		Expect(getLabelValue(h[0], constants.LabelRole)).To(Equal("decode"))
		Expect(family(registry, constants.WVAUtilizationShareTargetGPUs)[0].GetGauge().GetValue()).To(Equal(8.4))
		Expect(family(registry, constants.WVAUtilizationShareActionable)[0].GetGauge().GetValue()).To(Equal(1.0))
		spare := family(registry, constants.WVAUtilizationShareSpareGPUs)
		Expect(spare[0].GetGauge().GetValue()).To(Equal(6.0))
		Expect(getLabelValue(spare[0], constants.LabelScope)).To(Equal("cluster"))
		Expect(family(registry, constants.WVAUtilizationShareReplicasToMove)[0].GetGauge().GetValue()).To(Equal(3.0))
	})

	It("publishes no headroom for a role with no demand", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		PublishUtilizationShare([]UtilizationShareGroup{group(
			UtilizationShareRole{Namespace: "ns", ModelName: "idle", Role: "both", Headroom: math.NaN(), TargetGPUs: 1},
		)})
		Expect(family(registry, constants.WVAUtilizationShareHeadroom)).To(BeEmpty())
		Expect(family(registry, constants.WVAUtilizationShareTargetGPUs)).To(HaveLen(1))
	})

	// A model that leaves must take its series with it, and a cycle that does not
	// run the optimizer must clear them all: a stale headroom reading would
	// describe a fleet nothing is sizing.
	// An active group publishes its actuation series; when the same group goes
	// back to shadow those series must go while the evaluation stays -- a
	// promised or swinging reading left behind describes transfers nothing runs.
	It("publishes the actuation series only while a group is active, and drops them on the flip", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		role := UtilizationShareRole{Namespace: "ns", ModelName: "m", Role: "both", Headroom: 0.4, TargetGPUs: 8,
			Swinging: true, Actual: 0.7, FloorExcess: 2}
		active := group(role)
		active.Active, active.PromisedGPUs, active.ReserveDebtGPUs = true, 3, 1
		active.Timings = []UtilizationShareTiming{{Param: "window", Source: "scaledObject", Seconds: 30}}
		PublishUtilizationShare([]UtilizationShareGroup{active})

		actuation := []string{
			constants.WVAUtilizationSharePromisedGPUs, constants.WVAUtilizationShareReserveDebtGPUs,
			constants.WVAUtilizationShareEffectiveSeconds, constants.WVAUtilizationShareSwinging,
		}
		for _, name := range actuation {
			Expect(family(registry, name)).To(HaveLen(1), name)
		}
		Expect(family(registry, constants.WVAUtilizationSharePromisedGPUs)[0].GetGauge().GetValue()).To(Equal(3.0))
		t := family(registry, constants.WVAUtilizationShareEffectiveSeconds)[0]
		Expect(t.GetGauge().GetValue()).To(Equal(30.0))

		PublishUtilizationShare([]UtilizationShareGroup{group(role)}) // the same group, in shadow
		for _, name := range actuation {
			Expect(family(registry, name)).To(BeEmpty(), name+" outlived the flip to shadow")
		}
		Expect(family(registry, constants.WVAUtilizationShareTargetGPUs)).To(HaveLen(1), "the evaluation stays")
		Expect(family(registry, constants.WVAUtilizationShareSpareGPUs)).To(HaveLen(1))
	})

	It("replaces every series each cycle, and clears them when called with none", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		PublishUtilizationShare([]UtilizationShareGroup{group(
			UtilizationShareRole{Namespace: "ns", ModelName: "a", Role: "both", Headroom: 0.2, TargetGPUs: 4},
			UtilizationShareRole{Namespace: "ns", ModelName: "b", Role: "both", Headroom: 0.1, TargetGPUs: 3},
		)})
		Expect(family(registry, constants.WVAUtilizationShareHeadroom)).To(HaveLen(2))

		PublishUtilizationShare([]UtilizationShareGroup{group(
			UtilizationShareRole{Namespace: "ns", ModelName: "a", Role: "both", Headroom: 0.3, TargetGPUs: 5},
		)})
		h := family(registry, constants.WVAUtilizationShareHeadroom)
		Expect(h).To(HaveLen(1))
		Expect(getLabelValue(h[0], constants.LabelModelName)).To(Equal("a"))

		PublishUtilizationShare(nil)
		for _, name := range []string{
			constants.WVAUtilizationShareHeadroom, constants.WVAUtilizationShareTargetGPUs,
			constants.WVAUtilizationShareActionable, constants.WVAUtilizationShareSpareGPUs,
			constants.WVAUtilizationShareReplicasToMove,
		} {
			Expect(family(registry, name)).To(BeEmpty(), name)
		}
	})

	// increase() cannot see a series that appears at 1, so the first abort or
	// wrong-pod would never alert: every outcome starts at 0 for an acting group.
	It("publishes every transfer outcome at 0 for an acting group, and counts from there", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		active := group()
		active.Active = true
		PublishUtilizationShare([]UtilizationShareGroup{active})

		series := family(registry, constants.WVAUtilizationShareTransfersTotal)
		Expect(series).To(HaveLen(2 * len(constants.UtilizationShareOutcomes)))
		for _, m := range series {
			Expect(m.GetCounter().GetValue()).To(BeZero())
		}

		CountUtilizationShareTransfer("H200", constants.UtilizationShareClusterScope, "aborted", false)
		PublishUtilizationShare([]UtilizationShareGroup{active}) // must not reset it
		var aborted float64
		for _, m := range family(registry, constants.WVAUtilizationShareTransfersTotal) {
			if getLabelValue(m, constants.LabelOutcome) == "aborted" && getLabelValue(m, constants.LabelUrgent) == "false" {
				aborted = m.GetCounter().GetValue()
			}
		}
		Expect(aborted).To(Equal(1.0))
	})

	It("publishes no transfer outcome for a group in shadow", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		PublishUtilizationShare([]UtilizationShareGroup{group()})
		Expect(family(registry, constants.WVAUtilizationShareTransfersTotal)).To(BeEmpty())
	})
})
