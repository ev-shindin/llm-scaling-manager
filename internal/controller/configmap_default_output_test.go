package controller

import (
	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// defaultOutputTokens rides ScaledObject trigger metadata, not this ConfigMap.
// The risk worth a test is the one that is silent: yaml.Unmarshal is NOT strict
// here -- nothing in this repo calls KnownFields -- so an entry carrying the key
// parses, defaults, validates and logs exactly like one that does not, whichever
// way the field is tagged. Without these specs, re-adding a yaml tag would make
// a stale ConfigMap key quietly start overriding the trigger, and removing one
// would quietly stop a working config working.
var _ = Describe("defaultOutputTokens is not a ConfigMap key", func() {

	const body = `analyzers:
  - name: saturation
    score: 1.0
scaleUpThreshold: 0.85
scaleDownBoundary: 0.70
kvCacheThreshold: 0.80
queueLengthThreshold: 5
defaultOutputTokens: 6000
`

	It("parses the entry and ignores the key", func() {
		configs, count := parseScalingPolicyConfig(map[string]string{"default": body}, logr.Discard())
		Expect(count).To(Equal(1), "the unknown key must not cost the entry its parse")
		Expect(configs["default"].KvCacheThreshold).To(Equal(0.80), "its siblings still land")
		Expect(configs["default"].DefaultOutputTokens).To(BeZero(),
			"the ConfigMap is not the surface for this field")

		resolved := config.ResolveScalingPolicyForTier(configs, "Qwen/Qwen3-0.6B", "ns", "")
		Expect(resolved.DefaultOutputTokens).To(BeZero())
		Expect(resolved.ExpectedOutputTokens(0, 0, 512)).To(Equal(512.0),
			"with nothing seeded, the built-in net answers")
	})

	It("ignores it on a per-model override too", func() {
		data := map[string]string{
			"default":   body,
			"chatty#ns": "model_id: chatty\nnamespace: ns\ndefaultOutputTokens: 250\n",
		}
		configs, count := parseScalingPolicyConfig(data, logr.Discard())
		Expect(count).To(Equal(2))
		Expect(config.ResolveScalingPolicyForTier(configs, "chatty", "ns", "").DefaultOutputTokens).
			To(BeZero(), "an override cannot set it either; there is one surface, the trigger")
	})

	It("still accepts a figure folded in after resolution", func() {
		// What the engine does once it has read the model's triggers
		// (Engine.modelOutputSeed). The field is a resolved value, so it must
		// survive on a policy that came from a ConfigMap entry.
		configs, _ := parseScalingPolicyConfig(map[string]string{"default": body}, logr.Discard())
		resolved := config.ResolveScalingPolicyForTier(configs, "Qwen/Qwen3-0.6B", "ns", "")
		resolved.DefaultOutputTokens = 6000
		Expect(resolved.ExpectedOutputTokens(0, 0, 512)).To(Equal(6000.0))
		Expect(resolved.ExpectedOutputTokens(250, 0, 512)).To(Equal(250.0),
			"and a reading still beats it")
	})
})
