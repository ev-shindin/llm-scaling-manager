package allocation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Warm-pool headroom across namespace providers", func() {
	It("denies a type only some providers holding the namespace list", func() {
		constraints := []*ResourceConstraints{
			{ProviderName: "q1", NamespacePools: map[string]map[string]ResourcePool{"x": {"H100": {Limit: 4}}}},
			{ProviderName: "q2", NamespacePools: map[string]map[string]ResourcePool{"x": {"A100": {Limit: 4}}}},
		}
		Expect(namespaceHeadroom(constraints)).To(Equal(map[string]map[string]int{"x": {"H100": 0, "A100": 0}}),
			"the optimizer allows neither (mergeNamespaceConstraints); the pool must not grow on either")
	})

	It("keeps the tightest figure where every provider lists the type", func() {
		constraints := []*ResourceConstraints{
			{ProviderName: "q1", NamespacePools: map[string]map[string]ResourcePool{"x": {"H100": {Limit: 4, Used: 1}}}},
			{ProviderName: "q2", NamespacePools: map[string]map[string]ResourcePool{"x": {"H100": {Limit: -1}}}},
		}
		Expect(namespaceHeadroom(constraints)).To(Equal(map[string]map[string]int{"x": {"H100": 3}}))
	})

	It("matches a type across providers by accelerator identity", func() {
		constraints := []*ResourceConstraints{
			{ProviderName: "q1", NamespacePools: map[string]map[string]ResourcePool{"x": {"H100": {Limit: 4}}}},
			{ProviderName: "q2", NamespacePools: map[string]map[string]ResourcePool{"x": {"NVIDIA-H100-80GB-HBM3": {Limit: 2}}}},
		}
		Expect(namespaceHeadroom(constraints)).To(Equal(map[string]map[string]int{"x": {"H100": 2, "NVIDIA-H100-80GB-HBM3": 2}}),
			"two spellings of one GPU are one type: the tightest figure applies under either name")
	})
})
