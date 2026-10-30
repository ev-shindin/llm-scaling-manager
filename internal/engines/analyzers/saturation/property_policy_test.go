package saturation

import (
	"fmt"
	"slices"
	"testing"
)

// policyKeys is every policy key a learner reads (2.6: "property tests on
// defaults alone do not cover a key"). Each must be run at a value other than
// its default by at least one property case (propCase.keys). The scaling
// policy's other keys are not read by a learner: notLearnerKeys says why.
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
	// The scaling policy's own keys a learner reads.
	"kvCacheThreshold", "queueLengthThreshold", keyShapeHoldSeconds, keyDisableShapeHold, keyReuse,
	keyDisableK2Borrow, keyPrefillBound,
}

// notLearnerKeys are policy keys no learner here reads, and why.
var notLearnerKeys = map[string]string{
	"loadImbalance.idleAfterReady": "read by the load-balance diagnostic (internal/signals/balance), not a learner",
	"loadImbalance.sustainFor":     "read by the load-balance diagnostic, not a learner",
	"loadImbalance.clearAfter":     "read by the load-balance diagnostic, not a learner",
}

// checkPolicyKeys holds the cases to keys both ways: each key has a case at
// a non-default value, and each key a case names is listed.
func checkPolicyKeys(keys []string, cases []propCase) []error {
	covered := map[string]bool{}
	var errs []error
	for _, c := range cases {
		for _, k := range c.keys {
			covered[k] = true
			if !slices.Contains(keys, k) {
				errs = append(errs, fmt.Errorf("case %s runs %s at a non-default value, and policyKeys does not list it", c.id(), k))
			}
			if c.tuning == tunedDefault {
				errs = append(errs, fmt.Errorf("case %s names key %s and says it runs the default tuning", c.id(), k))
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

// Negative controls: a key no case runs, a case naming an unlisted key, and
// a key named on a default-tuning case each break the check.
func TestPropertyPolicyKeysNegativeControls(t *testing.T) {
	c := propCase{class: classEdges, learner: "x", name: "y", tuning: tunedNonDefault, keys: []string{"a"}}
	if errs := checkPolicyKeys([]string{"a"}, []propCase{c}); len(errs) != 0 {
		t.Fatalf("a covered key is clean: %v", errs)
	}
	for name, tc := range map[string]struct {
		keys  []string
		cases []propCase
	}{
		"a key no case runs":        {[]string{"a", "b"}, []propCase{c}},
		"a case naming an unlisted": {nil, []propCase{c}},
		"a key on a default case":   {[]string{"a"}, []propCase{{class: classEdges, learner: "x", name: "y", tuning: tunedDefault, keys: []string{"a"}}}},
		"a key also exempted":       {[]string{"a", "loadImbalance.sustainFor"}, []propCase{c, {tuning: tunedNonDefault, keys: []string{"loadImbalance.sustainFor"}}}},
	} {
		if len(checkPolicyKeys(tc.keys, tc.cases)) == 0 {
			t.Errorf("%s: the check passed", name)
		}
	}
}
