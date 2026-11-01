package saturation

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// policyKeys is every policy key a learner reads (2.6: "property tests on
// defaults alone do not cover a key"). Each must be run at a value other than
// its default by at least one property case: a case's policy sets it
// (propCase.policy, nonDefaultKeys). The analyzerTuning keys are named
// without their "analyzerTuning." prefix, the loadImbalance keys with theirs.
// Every key of the scaling policy is here or in notLearnerKeys
// (TestPropertyPolicyKeysCoverThePolicy).
var policyKeys = []string{
	// analyzerTuning.
	"itlBand.minObservableK", "itlBand.maxObservableK", "itlBand.capMaxPreemptionRate",
	"steadyGate.steadyFor", "steadyGate.maxKDrift", "steadyGate.carry", "steadyGate.idleRows",
	"steadyGate.minSettledSamples",
	"k2Settle.rateFraction", "k2Settle.maxRise",
	"muChoice.minMeasuredOfDerived", "muChoice.maxRatio", "muChoice.returnRatio", "muChoice.freshFor",
	"muChoice.lowHoldFor",
	"admission.maxTokensOverCompletions", "admission.siblingMuTolerance",
	"floorTrust.lagFraction", "floorTrust.trustFraction",
	"backlogGrowth.minRequests", "backlogGrowth.fraction",
	// loadImbalance.
	"loadImbalance.idleKvUsage",
	// The scaling policy's own keys a learner reads. scaleUpThreshold sets the
	// k derived mu is priced at, with kvCacheThreshold (pricingK).
	"kvCacheThreshold", "queueLengthThreshold", "scaleUpThreshold", keyShapeHoldSeconds, keyDisableShapeHold,
	keyReuse, keyDisableK2Borrow, keyPrefillBound, keyBatchCapLearning,
}

// notLearnerKeys are the scaling policy's keys no learner here reads, and
// why. With policyKeys it is every key of the policy, top level and nested
// (TestPropertyPolicyKeysCoverThePolicy): a key added to the policy must be
// placed in one of the two.
var notLearnerKeys = map[string]string{
	"loadImbalance.idleAfterReady": "read by the load-balance diagnostic (internal/signals/balance), not a learner",
	"loadImbalance.sustainFor":     "read by the load-balance diagnostic, not a learner",
	"loadImbalance.clearAfter":     "read by the load-balance diagnostic, not a learner",
	"model_id":                     "selects the policy entry a model takes",
	"namespace":                    "selects the policy entry a namespace takes",
	"enableRescale":                "an actuation switch of the engine, read after the analyzers",
	"analyzerName":                 "names the analyzer an entry configures",
	"scaleDownBoundary":            "the release threshold the composite applies to a figure; the shape hold floors demand at it, and learns nothing from it",
	"priority":                     "the optimizer's ordering between models",
	"analyzers":                    "which analyzers run, and their weights",
	"slo":                          "the SLO analyzer's targets, which it prices against and learns nothing from",
	"scaleToZero":                  "the scale-to-zero envelope, read by the engine",
	"scaleFromZero":                "the scale-from-zero envelope, read by the engine",
	"analyzerDefinitions":          "external analyzers' queries",
	"defaultPolicy":                "names the default entry",
	"limiters":                     "the quota limiters, read by the optimizer",
	"optimizer":                    "the optimizer's configuration",
	"weightClass":                  "the share optimizer's weight class",
	"weight":                       "the share optimizer's weight",
}

// tuningPolicy is a policy that sets t and nothing else.
func tuningPolicy(t config.AnalyzerTuning) *config.ScalingPolicy {
	return &config.ScalingPolicy{AnalyzerTuning: t}
}

// yamlName is a struct field's yaml key, and whether the field has a yaml
// tag at all. A tag of "-" is a field kept out of the yaml on purpose, and
// has no key.
func yamlName(f reflect.StructField) (string, bool) {
	tag, tagged := f.Tag.Lookup("yaml")
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return "", true
	}
	return name, tagged && name != ""
}

// policyKeyPrefix is the prefix a policy key under a nested field takes:
// none under analyzerTuning, "loadImbalance." under loadImbalance. Other
// fields are keys of their own.
func policyKeyPrefix(name string) (string, bool) {
	switch name {
	case "analyzerTuning":
		return "", true
	case "loadImbalance":
		return "loadImbalance.", true
	}
	return "", false
}

// policyLeaves walks v, a ScalingPolicy or one of its tuning structs, and
// calls fn with each leaf key and its value. It returns the exported fields
// it met with no yaml key: a field the walk cannot name is a key no test
// would see, so the caller fails on it rather than skip it.
func policyLeaves(v reflect.Value, prefix string, top bool, fn func(key string, v reflect.Value)) (untagged []string) {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, tagged := yamlName(f)
		if !tagged {
			untagged = append(untagged, prefix+f.Name)
			continue
		}
		if name == "" {
			continue // yaml:"-"
		}
		fv := v.Field(i)
		if top {
			if p, nested := policyKeyPrefix(name); nested {
				untagged = append(untagged, policyLeaves(fv, p, false, fn)...)
				continue
			}
		}
		if !top && fv.Kind() == reflect.Struct {
			untagged = append(untagged, policyLeaves(fv, prefix+name+".", false, fn)...)
			continue
		}
		fn(prefix+name, fv)
	}
	return untagged
}

// nonDefaultKeys is the keys p sets away from their defaults: the fields it
// sets at all, since every unset field takes its default.
func nonDefaultKeys(p *config.ScalingPolicy) []string {
	if p == nil {
		return nil
	}
	var out []string
	policyLeaves(reflect.ValueOf(*p), "", true, func(key string, v reflect.Value) {
		if !v.IsZero() {
			out = append(out, key)
		}
	})
	return out
}

// policyKeysOf is every key of the scaling policy type t, top level and
// nested, and the exported fields it found with no yaml key.
func policyKeysOf(t reflect.Type) (keys, untagged []string) {
	untagged = policyLeaves(reflect.New(t).Elem(), "", true, func(key string, _ reflect.Value) {
		keys = append(keys, key)
	})
	return keys, untagged
}

// checkPolicyCoverage holds a policy's keys to the two lists: every key is a
// learner's (policyKeys) or says why not (notLearnerKeys), every listed key
// is a key of the policy, and no exported field lacks a yaml key.
func checkPolicyCoverage(all, untagged, learner []string, not map[string]string) []error {
	var errs []error
	for _, f := range untagged {
		errs = append(errs, fmt.Errorf("%s has no yaml key: name it, or tag it yaml:\"-\"", f))
	}
	for _, k := range all {
		_, exempt := not[k]
		if !slices.Contains(learner, k) && !exempt {
			errs = append(errs, fmt.Errorf("%s is neither in policyKeys nor in notLearnerKeys", k))
		}
	}
	for _, k := range learner {
		if !slices.Contains(all, k) {
			errs = append(errs, fmt.Errorf("policyKeys lists %s, which is no key of the scaling policy", k))
		}
	}
	for k := range not {
		if !slices.Contains(all, k) {
			errs = append(errs, fmt.Errorf("notLearnerKeys lists %s, which is no key of the scaling policy", k))
		}
	}
	return errs
}

// checkPolicyKeys holds the cases to keys both ways: each key has a case
// whose policy sets it, and each key a case's policy sets is listed. A case
// with a policy says it runs a non-default tuning.
func checkPolicyKeys(keys []string, cases []propCase) []error {
	covered := map[string]bool{}
	var errs []error
	for _, c := range cases {
		set := nonDefaultKeys(c.policy)
		if c.policy != nil && len(set) == 0 {
			errs = append(errs, fmt.Errorf("case %s has a policy that sets no key", c.id()))
		}
		for _, k := range set {
			covered[k] = true
			if !slices.Contains(keys, k) {
				errs = append(errs, fmt.Errorf("case %s runs %s at a non-default value, and policyKeys does not list it", c.id(), k))
			}
			if c.tuning == tunedDefault {
				errs = append(errs, fmt.Errorf("case %s sets key %s and says it runs the default tuning", c.id(), k))
			}
		}
	}
	for _, k := range keys {
		if _, exempt := notLearnerKeys[k]; exempt {
			errs = append(errs, fmt.Errorf("%s is listed both as a learner's key and as no learner's", k))
		}
		if !covered[k] {
			errs = append(errs, fmt.Errorf("policy key %s: no property case runs it at a non-default value", k))
		}
	}
	return errs
}

func TestPropertyPolicyKeysEachRunNonDefault(t *testing.T) {
	for _, e := range checkPolicyKeys(policyKeys, propertyCases()) {
		t.Error(e)
	}
}

// Every key of the scaling policy, top level and nested, read from the yaml
// tags, is a learner's key or says why it is not; every listed key is a key
// of the policy; and every exported field has a yaml key. A key added to the
// policy cannot be left out of the property tests silently.
func TestPropertyPolicyKeysCoverThePolicy(t *testing.T) {
	all, untagged := policyKeysOf(reflect.TypeFor[config.ScalingPolicy]())
	if len(all) < 40 {
		t.Fatalf("the walk found %d policy keys: %v", len(all), all)
	}
	for _, e := range checkPolicyCoverage(all, untagged, policyKeys, notLearnerKeys) {
		t.Error(e)
	}
}

// The coverage check's negative controls, on types shaped like the policy:
// a scalar placed directly in analyzerTuning, a new top-level field, and an
// exported field with no yaml tag are each reported.
func TestPropertyPolicyCoverageNegativeControls(t *testing.T) {
	type tuningWithScalar struct {
		Probe float64         `yaml:"probeBar"`
		Band  struct{ A int } `yaml:"band"`
	}
	type policy struct {
		Kv     float64          `yaml:"kv"`
		Tuning tuningWithScalar `yaml:"analyzerTuning"`
		Top    float64          `yaml:"probeTop"`
		Bare   float64
		Hidden float64 `yaml:"-"`
	}
	all, untagged := policyKeysOf(reflect.TypeFor[policy]())
	slices.Sort(all)
	if want := []string{"kv", "probeBar", "probeTop"}; !slices.Equal(all, want) {
		t.Fatalf("keys %v, want %v", all, want)
	}
	errs := fmt.Sprint(checkPolicyCoverage(all, untagged, []string{"kv"}, map[string]string{}))
	for _, want := range []string{"probeBar is neither", "probeTop is neither", "Bare has no yaml key", "band.A has no yaml key"} {
		if !strings.Contains(errs, want) {
			t.Errorf("not reported: %s (got %s)", want, errs)
		}
	}
	if strings.Contains(errs, "Hidden") {
		t.Errorf("a field tagged yaml:\"-\" was reported: %s", errs)
	}
	if errs := checkPolicyCoverage([]string{"kv"}, nil, []string{"kv"}, map[string]string{}); len(errs) != 0 {
		t.Errorf("a covered policy is clean: %v", errs)
	}
}

// Negative controls: a key no case runs, a case setting an unlisted key, a
// key set on a default-tuning case, a key both listed and exempted, and a
// policy that sets nothing each break the check; and nonDefaultKeys reads
// what a policy sets, nested or not.
func TestPropertyPolicyKeysNegativeControls(t *testing.T) {
	pol := &config.ScalingPolicy{KvCacheThreshold: 0.9}
	c := propCase{class: classEdges, learner: "x", name: "y", tuning: tunedPolicy, policy: pol}
	if errs := checkPolicyKeys([]string{"kvCacheThreshold"}, []propCase{c}); len(errs) != 0 {
		t.Fatalf("a covered key is clean: %v", errs)
	}
	sustain := &config.ScalingPolicy{LoadImbalance: config.LoadImbalance{SustainFor: 1}}
	for name, tc := range map[string]struct {
		keys  []string
		cases []propCase
	}{
		"a key no case runs":         {[]string{"kvCacheThreshold", "queueLengthThreshold"}, []propCase{c}},
		"a case setting an unlisted": {nil, []propCase{c}},
		"a key on a default case":    {[]string{"kvCacheThreshold"}, []propCase{{class: classEdges, learner: "x", name: "y", tuning: tunedDefault, policy: pol}}},
		"a key also exempted":        {[]string{"kvCacheThreshold", "loadImbalance.sustainFor"}, []propCase{c, {tuning: tunedPolicy, policy: sustain}}},
		"a policy that sets no key":  {[]string{"kvCacheThreshold"}, []propCase{c, {tuning: tunedPolicy, policy: &config.ScalingPolicy{}}}},
	} {
		if len(checkPolicyKeys(tc.keys, tc.cases)) == 0 {
			t.Errorf("%s: the check passed", name)
		}
	}
	capRate := 0.0
	got := nonDefaultKeys(&config.ScalingPolicy{ScaleUpThreshold: 0.9,
		AnalyzerTuning: config.AnalyzerTuning{SteadyGate: config.SteadyGateTuning{IdleRows: 5},
			ITLBand: config.ITLBandTuning{CapMaxPreemptionRate: &capRate}},
		LoadImbalance: config.LoadImbalance{IdleKvUsage: 0.2}})
	want := []string{"scaleUpThreshold", "itlBand.capMaxPreemptionRate", "steadyGate.idleRows", "loadImbalance.idleKvUsage"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("nonDefaultKeys: %v, want %v", got, want)
	}
}
