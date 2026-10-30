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
// Every analyzerTuning and loadImbalance key is here or in notLearnerKeys
// (TestPropertyPolicyKeysCoverTheTuningStructs).
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
	keyReuse, keyDisableK2Borrow, keyPrefillBound,
}

// notLearnerKeys are the analyzerTuning and loadImbalance keys no learner
// here reads, and why.
var notLearnerKeys = map[string]string{
	"loadImbalance.idleAfterReady": "read by the load-balance diagnostic (internal/signals/balance), not a learner",
	"loadImbalance.sustainFor":     "read by the load-balance diagnostic, not a learner",
	"loadImbalance.clearAfter":     "read by the load-balance diagnostic, not a learner",
}

// tuningPolicy is a policy that sets t and nothing else.
func tuningPolicy(t config.AnalyzerTuning) *config.ScalingPolicy {
	return &config.ScalingPolicy{AnalyzerTuning: t}
}

// yamlName is a struct field's yaml key, "" when it has none.
func yamlName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if name == "-" {
		return ""
	}
	return name
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
// calls fn with each leaf key and its value.
func policyLeaves(v reflect.Value, prefix string, top bool, fn func(key string, v reflect.Value)) {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		name := yamlName(f)
		if name == "" || !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		if top {
			if p, nested := policyKeyPrefix(name); nested {
				policyLeaves(fv, p, false, fn)
				continue
			}
		}
		if !top && fv.Kind() == reflect.Struct {
			policyLeaves(fv, prefix+name+".", false, fn)
			continue
		}
		fn(prefix+name, fv)
	}
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

// tuningKeys is every analyzerTuning and loadImbalance key, by the structs'
// yaml tags.
func tuningKeys() []string {
	var out []string
	policyLeaves(reflect.ValueOf(config.ScalingPolicy{}), "", true, func(key string, _ reflect.Value) {
		out = append(out, key)
	})
	var tuned []string
	for _, k := range out {
		if strings.Contains(k, ".") {
			tuned = append(tuned, k)
		}
	}
	return tuned
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

// Every analyzerTuning and loadImbalance key, read from the structs' yaml
// tags, is a learner's key or says why it is not, and every key policyKeys
// lists is a key of the scaling policy: a key added to the tuning structs
// cannot be left out of the property tests silently.
func TestPropertyPolicyKeysCoverTheTuningStructs(t *testing.T) {
	tuned := tuningKeys()
	if len(tuned) < 20 {
		t.Fatalf("the walk found %d tuning keys: %v", len(tuned), tuned)
	}
	for _, k := range tuned {
		_, exempt := notLearnerKeys[k]
		if !slices.Contains(policyKeys, k) && !exempt {
			t.Errorf("%s is neither in policyKeys nor in notLearnerKeys", k)
		}
	}
	var all []string
	policyLeaves(reflect.ValueOf(config.ScalingPolicy{}), "", true, func(key string, _ reflect.Value) { all = append(all, key) })
	for _, k := range policyKeys {
		if !slices.Contains(all, k) {
			t.Errorf("policyKeys lists %s, which is no key of the scaling policy", k)
		}
	}
	for k := range notLearnerKeys {
		if !slices.Contains(tuned, k) {
			t.Errorf("notLearnerKeys lists %s, which is no tuning key", k)
		}
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
