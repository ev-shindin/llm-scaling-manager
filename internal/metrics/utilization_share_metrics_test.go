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
})
