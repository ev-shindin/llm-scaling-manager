package config

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

const quotaLimiters = `
limiters:
  - type: quota
    name: team-quota
    scope: cluster
    quotas:
      H100: 8
`

func parsePolicy(doc string) ScalingPolicy {
	var p ScalingPolicy
	ExpectWithOffset(1, yaml.Unmarshal([]byte(doc), &p)).To(Succeed())
	return p
}

var _ = Describe("ResolveUtilizationShare", func() {
	It("resolves an empty block to every default", func() {
		us, err := ResolveUtilizationShare(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(us.Tolerance).To(Equal(DefaultUtilizationShareTolerance))
		Expect(us.ReserveGPUs).To(BeZero())
		Expect(us.Shadow).To(BeFalse())
		Expect(us.PhysicalGroups).To(BeFalse())
		Expect(us.Classes).To(Equal(DefaultWeightClasses))
		Expect(us.DefaultClass).To(Equal("standard"))
		Expect(us.MinWeight).To(Equal(0.5))
		Expect(us.MaxWeight).To(Equal(4.0))
	})

	It("does not alias the default classes", func() {
		us, err := ResolveUtilizationShare(nil)
		Expect(err).NotTo(HaveOccurred())
		us.Classes["standard"] = 99
		Expect(DefaultWeightClasses["standard"]).To(Equal(1.0))
	})

	DescribeTable("rejects invalid settings",
		func(cfg UtilizationShareConfig, msg string) {
			_, err := ResolveUtilizationShare(&cfg)
			Expect(err).To(MatchError(ContainSubstring(msg)))
		},
		Entry("tolerance 1", UtilizationShareConfig{Tolerance: 1}, "tolerance"),
		Entry("negative tolerance", UtilizationShareConfig{Tolerance: -0.1}, "tolerance"),
		Entry("NaN tolerance", UtilizationShareConfig{Tolerance: math.NaN()}, "tolerance"),
		Entry("negative reserve", UtilizationShareConfig{ReserveGPUs: -1}, "reserveGPUs"),
		Entry("zero class weight", UtilizationShareConfig{WeightClasses: map[string]float64{"a": 1, "b": 0}}, `"b"`),
		Entry("NaN class weight", UtilizationShareConfig{WeightClasses: map[string]float64{"a": 1, "b": math.NaN()}}, `"b"`),
		Entry("infinite class weight", UtilizationShareConfig{WeightClasses: map[string]float64{"a": 1, "b": math.Inf(1)}}, `"b"`),
		Entry("no class of weight 1", UtilizationShareConfig{WeightClasses: map[string]float64{"a": 2}}, "exactly one class of weight 1"),
		Entry("two classes of weight 1", UtilizationShareConfig{WeightClasses: map[string]float64{"a": 1, "b": 1}}, "exactly one class of weight 1"),
	)

	It("derives the clamp and the default class from the classes", func() {
		us, err := ResolveUtilizationShare(&UtilizationShareConfig{
			WeightClasses: map[string]float64{"low": 0.25, "normal": 1, "top": 8},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(us.DefaultClass).To(Equal("normal"))
		Expect(us.MinWeight).To(Equal(0.25))
		Expect(us.MaxWeight).To(Equal(8.0))
	})

	It("disables only the namespaces that say enabled: false", func() {
		off, on := false, true
		us, err := ResolveUtilizationShare(&UtilizationShareConfig{
			Namespaces: map[string]UtilizationShareNamespace{
				"research": {Enabled: &off},
				"prod":     {Enabled: &on},
				"batch":    {},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(us.EnabledForNamespace("research")).To(BeFalse())
		Expect(us.EnabledForNamespace("prod")).To(BeTrue())
		Expect(us.EnabledForNamespace("batch")).To(BeTrue())
		Expect(us.EnabledForNamespace("unlisted")).To(BeTrue())
		Expect(us.DisabledNamespaces()).To(Equal([]string{"research"}))
	})
})

var _ = Describe("UtilizationShare.Weight", func() {
	var us UtilizationShare
	BeforeEach(func() {
		var err error
		us, err = ResolveUtilizationShare(nil)
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTable("resolves a model's weight",
		func(class string, weight ModelWeight, want float64, wantErr string) {
			got, err := us.Weight(class, weight)
			Expect(got).To(Equal(want))
			if wantErr == "" {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(MatchError(ContainSubstring(wantErr)))
			}
		},
		Entry("a class", "critical", ModelWeight{}, 4.0, ""),
		Entry("neither: the default class", "", ModelWeight{}, 1.0, ""),
		Entry("a number inside the range", "", NewModelWeight(2.5), 2.5, ""),
		Entry("a number above the range is clamped", "", NewModelWeight(1e6), 4.0, ""),
		Entry("a number below the range is clamped", "", NewModelWeight(0.01), 0.5, ""),
		Entry("an unknown class falls back and reports", "gold", ModelWeight{}, 1.0, "unknown weightClass"),
		Entry("both set falls back and reports", "critical", NewModelWeight(2), 1.0, "mutually exclusive"),
		Entry("a negative number falls back and reports", "", NewModelWeight(-1), 1.0, "finite number > 0"),
		Entry("NaN falls back and reports", "", NewModelWeight(math.NaN()), 1.0, "finite number > 0"),
		Entry("infinity falls back and reports", "", NewModelWeight(math.Inf(1)), 1.0, "finite number > 0"),
		Entry("zero falls back and reports", "", NewModelWeight(0), 1.0, "finite number > 0"),
	)

	It("reports a weight that did not decode, and falls back", func() {
		p := parsePolicy("weight: high\n")
		Expect(p.Weight.Err()).To(HaveOccurred())
		got, err := us.Weight(p.WeightClass, p.Weight)
		Expect(got).To(Equal(1.0))
		Expect(err).To(MatchError(ContainSubstring("weight")))
	})
})

var _ = Describe("ScalingPolicy weight fields", func() {
	// §8.3: the "default" entry carries the limiters, so no weight problem may
	// fail it. Negative control: before ModelWeight, `weight: high` failed the
	// whole entry's decode, and both-set or a negative weight failed Validate.
	DescribeTable("never fail the entry, so the limiters survive",
		func(fields string) {
			var p ScalingPolicy
			Expect(yaml.Unmarshal([]byte(quotaLimiters+fields), &p)).To(Succeed())
			p.ApplyDefaults()
			Expect(p.Validate()).To(Succeed())
			Expect(p.Limiters).To(HaveLen(1))
		},
		Entry("a type error", "weight: high\n"),
		Entry("both set", "weight: 2\nweightClass: critical\n"),
		Entry("a negative weight", "weight: -1\n"),
		Entry("an unknown class", "weightClass: gold\n"),
	)

	It("decodes a numeric weight and leaves an absent one unset", func() {
		Expect(parsePolicy("weight: 2.5\n").Weight).To(Equal(NewModelWeight(2.5)))
		Expect(parsePolicy("priority: 2\n").Weight.IsZero()).To(BeTrue())
	})

	It("lets an override's class replace an inherited number, not join it", func() {
		base := ScalingPolicy{Weight: NewModelWeight(3)}
		base.Merge(ScalingPolicy{WeightClass: "critical"})
		Expect(base.WeightClass).To(Equal("critical"))
		Expect(base.Weight.IsZero()).To(BeTrue())
	})

	It("lets an override's number replace an inherited class", func() {
		base := ScalingPolicy{WeightClass: "critical"}
		base.Merge(ScalingPolicy{Weight: NewModelWeight(2)})
		Expect(base.WeightClass).To(BeEmpty())
		Expect(base.Weight).To(Equal(NewModelWeight(2)))
	})

	It("keeps the inherited weight when the override states none", func() {
		base := ScalingPolicy{WeightClass: "important"}
		base.Merge(ScalingPolicy{Priority: 2})
		Expect(base.WeightClass).To(Equal("important"))
	})

	It("does not merge the optimizer block from an override", func() {
		base := ScalingPolicy{}
		base.Merge(ScalingPolicy{Optimizer: &OptimizerConfig{Type: OptimizerTypeUtilizationShare}})
		Expect(base.Optimizer).To(BeNil())
	})
})

var _ = Describe("Config.UtilizationShare", func() {
	var c *Config
	BeforeEach(func() { c = NewTestConfig() })

	It("is not selected when no optimizer block is declared", func() {
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(quotaLimiters)})
		_, selected, err := c.UtilizationShare()
		Expect(err).NotTo(HaveOccurred())
		Expect(selected).To(BeFalse())
	})

	It("is selected by type alone, with every default", func() {
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(quotaLimiters + `
optimizer:
  type: utilizationShare
`)})
		us, selected, err := c.UtilizationShare()
		Expect(err).NotTo(HaveOccurred())
		Expect(selected).To(BeTrue())
		Expect(us.Tolerance).To(Equal(DefaultUtilizationShareTolerance))
	})

	It("reads the separated cluster policy, not the global map", func() {
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(quotaLimiters)})
		policy := parsePolicy(quotaLimiters + `
optimizer:
  type: utilizationShare
  utilizationShare:
    shadow: true
`)
		c.UpdateClusterPolicy(&policy)
		us, selected, err := c.UtilizationShare()
		Expect(err).NotTo(HaveOccurred())
		Expect(selected).To(BeTrue())
		Expect(us.Shadow).To(BeTrue())
	})

	It("refuses to run without limiters: there is no budget to share", func() {
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(`
optimizer:
  type: utilizationShare
`)})
		_, selected, err := c.UtilizationShare()
		Expect(selected).To(BeFalse())
		Expect(err).To(MatchError(ContainSubstring("limiters")))
	})

	It("reports an unknown optimizer type", func() {
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(quotaLimiters + `
optimizer:
  type: bogus
`)})
		_, selected, err := c.UtilizationShare()
		Expect(selected).To(BeFalse())
		Expect(err).To(MatchError(ContainSubstring("bogus")))
	})

	// The proposal's §8.3: an invalid optimizer block must not cost the limiters.
	// Negative control: a misspelled key anywhere else in the entry is accepted
	// silently by yaml.v3, and a type error fails the whole entry -- which is
	// exactly what would drop the limiters if the optimizer block decoded the
	// ordinary way.
	DescribeTable("an invalid block disables the optimizer and keeps the limiters",
		func(block, msg string) {
			doc := quotaLimiters + block
			var p ScalingPolicy
			Expect(yaml.Unmarshal([]byte(doc), &p)).To(Succeed(), "the entry itself must still decode")
			p.ApplyDefaults()
			Expect(p.Validate()).To(Succeed(), "the entry must still validate")
			c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: p})

			_, selected, err := c.UtilizationShare()
			Expect(selected).To(BeFalse())
			Expect(err).To(MatchError(ContainSubstring(msg)))
			Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeQuota))
			Expect(c.EffectiveQuotaEntries()).To(HaveLen(1))
		},
		Entry("a type error", `
optimizer:
  type: utilizationShare
  utilizationShare:
    tolerance: high
`, "optimizer"),
		Entry("a misspelled key", `
optimizer:
  type: utilizationShare
  utilizationShare:
    toleranceBand: 0.2
`, "toleranceBand"),
		Entry("an out-of-range value", `
optimizer:
  type: utilizationShare
  utilizationShare:
    tolerance: 1.5
`, "tolerance"),
		Entry("classes without a weight-1 class", `
optimizer:
  type: utilizationShare
  utilizationShare:
    weightClasses:
      low: 0.5
      high: 2
`, "weight 1"),
	)

	It("ignores, and names, a namespace-local optimizer block", func() {
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(quotaLimiters)})
		c.UpdateScalingPolicyConfigForNamespace("tenant-a", map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(`
optimizer:
  type: utilizationShare
`)})
		_, selected, err := c.UtilizationShare()
		Expect(err).NotTo(HaveOccurred())
		Expect(selected).To(BeFalse())
		Expect(c.IgnoredOptimizerNamespaces()).To(Equal([]string{"tenant-a"}))
	})
})

var _ = Describe("Config.UtilizationShare edge cases", func() {
	var c *Config
	BeforeEach(func() { c = NewTestConfig() })

	It("is not selected by a block without a type, and is not an error", func() {
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(quotaLimiters + `
optimizer:
  utilizationShare:
    shadow: true
`)})
		_, selected, err := c.UtilizationShare()
		Expect(err).NotTo(HaveOccurred())
		Expect(selected).To(BeFalse())
	})

	It("reports a block that is not a mapping, and keeps the limiters", func() {
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(quotaLimiters + "optimizer: utilizationShare\n")})
		_, selected, err := c.UtilizationShare()
		Expect(selected).To(BeFalse())
		Expect(err).To(MatchError(ContainSubstring("optimizer")))
		Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeQuota))
	})

	It("ignores the global map's optimizer once a separated cluster policy has none", func() {
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(quotaLimiters + `
optimizer:
  type: utilizationShare
`)})
		policy := parsePolicy(quotaLimiters)
		c.UpdateClusterPolicy(&policy)
		_, selected, err := c.UtilizationShare()
		Expect(err).NotTo(HaveOccurred())
		Expect(selected).To(BeFalse())
	})
})

var _ = Describe("Config.NamespaceHasLocalPolicy", func() {
	It("is true only for a namespace with its own scaling-policy map", func() {
		c := NewTestConfig()
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(quotaLimiters)})
		c.UpdateScalingPolicyConfigForNamespace("tenant-a", map[string]ScalingPolicy{GlobalDefaultsKey: {WeightClass: "critical"}})
		Expect(c.NamespaceHasLocalPolicy("tenant-a")).To(BeTrue())
		Expect(c.NamespaceHasLocalPolicy("tenant-b")).To(BeFalse())
	})
})
