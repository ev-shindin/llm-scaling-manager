package controller

import (
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
)

// tuningWarnings parses data and returns, by entry key, the "tuning figure
// not used" warnings it logged.
func tuningWarnings(t *testing.T, data map[string]string) map[string][]string {
	t.Helper()
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) }, funcr.Options{})
	configs, count, rejected, _ := parseScalingPolicyConfigRejects(data, logger)
	if count != len(data) || len(configs) != len(data) || len(rejected) != 0 {
		t.Fatalf("kept %d of %d, rejected %v: every entry is valid alone", count, len(data), rejected)
	}
	warnings := map[string][]string{}
	for _, l := range lines {
		if !strings.Contains(l, "WARNING: tuning figure not used") {
			continue
		}
		for key := range data {
			if strings.Contains(l, `"key"="`+key+`"`) {
				warnings[key] = append(warnings[key], l)
			}
		}
	}
	return warnings
}

// expectWarnings checks each entry logged exactly the warnings want lists,
// one per substring, and every other entry none.
func expectWarnings(t *testing.T, data map[string]string, warnings, want map[string][]string) {
	t.Helper()
	for key := range data {
		got := strings.Join(warnings[key], "\n")
		if len(warnings[key]) != len(want[key]) {
			t.Errorf("%s: %d warnings, want %d:\n%s", key, len(warnings[key]), len(want[key]), got)
		}
		for _, p := range want[key] {
			if !strings.Contains(got, p) {
				t.Errorf("%s: no warning containing %q; got:\n%s", key, p, got)
			}
		}
	}
}

// A tuning key an entry sets alone, past its partner's default, is accepted
// and then not used: the resolved policy puts the crossed pair back to both
// defaults. That is said once when the ConfigMap is read, naming the entry,
// the pair, the values and the defaults used. An entry that runs as written
// says nothing, and so does a default entry whose kvCacheThreshold is under
// the default idle figure while no idleKvUsage is set: no setting of the
// operator's is ignored there.
func TestParseWarnsAboutCrossedTuningPairs(t *testing.T) {
	data := map[string]string{
		"default": "kvCacheThreshold: 0.3\nanalyzerTuning:\n  muChoice: {maxRatio: 3, returnRatio: 2.5}\n",
		"slow":    "analyzerTuning:\n  steadyGate: {steadyFor: 10m}\n  itlBand: {minObservableK: 0.85}\n",
		"ratio":   "analyzerTuning:\n  muChoice: {maxRatio: 2}\n",
		"clean":   "analyzerTuning:\n  steadyGate: {steadyFor: 10m, carry: 15m}\n",
	}
	expectWarnings(t, data, tuningWarnings(t, data), map[string][]string{
		"slow": {
			"analyzerTuning.itlBand.minObservableK/maxObservableK = 0.85/0.8: the minimum must be below the maximum; using the defaults 0.15/0.8",
			"analyzerTuning.steadyGate.steadyFor/carry = 10m0s/5m0s: carry must be at least steadyFor; using the defaults 2m0s/5m0s",
		},
		"ratio": {"analyzerTuning.muChoice.maxRatio/returnRatio = 2/2.5: returnRatio must be at most maxRatio; using the defaults 2/1.6"},
	})
}

// An idleKvUsage that is set, here on the default entry, and a
// kvCacheThreshold under it, here on another entry, merge into an idle bound
// held at the threshold: warned on the entry that resolves to it.
func TestParseWarnsAboutAClampedIdleKvUsage(t *testing.T) {
	data := map[string]string{
		"default": "loadImbalance: {idleKvUsage: 0.5}\n",
		"low-kv":  "kvCacheThreshold: 0.4\n",
	}
	expectWarnings(t, data, tuningWarnings(t, data), map[string][]string{
		"low-kv": {"loadImbalance.idleKvUsage = 0.5 is not below kvCacheThreshold 0.4: " +
			"a replica cannot be idle and full at once; using 0.4 as the idle bound instead"},
	})
}

// The S1 case: a default entry with a low kvCacheThreshold and its limiters,
// and no tuning key at all, loads silently.
func TestParseIsSilentForALowKvCacheThresholdAlone(t *testing.T) {
	data := map[string]string{"default": "kvCacheThreshold: 0.3\nlimiters:\n- type: gpu-inventory\n"}
	expectWarnings(t, data, tuningWarnings(t, data), nil)
}
