package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The usage refresher's timer runs on UtilizationShareActive: getting it wrong
// leaves a quota-only install without node information, and node-aware
// planning silently off.
var _ = Describe("UtilizationShareActive", func() {
	DescribeTable("is true only when the optimizer is selected and not in shadow mode",
		func(doc string, want bool) {
			c := NewTestConfig()
			c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(quotaLimiters + doc)})
			Expect(c.UtilizationShareActive()).To(Equal(want))
		},
		Entry("active", "optimizer:\n  type: utilizationShare\n", true),
		Entry("shadow", "optimizer:\n  type: utilizationShare\n  utilizationShare:\n    shadow: true\n", false),
		Entry("not selected", "", false),
	)
})
