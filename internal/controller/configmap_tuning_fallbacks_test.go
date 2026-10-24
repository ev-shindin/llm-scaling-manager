package controller

import (
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
)

// A tuning key an entry sets alone, past its partner's default, is accepted
// and then not used: the resolved policy puts the crossed pair back to both
// defaults. That is said once when the ConfigMap is read, naming the entry,
// the pair, the values and the defaults used, and so is an idle bound held
// under kvCacheThreshold. An entry that runs as written says nothing.
func TestParseWarnsAboutTuningFallbacks(t *testing.T) {
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) }, funcr.Options{})
	data := map[string]string{
		"default": "kvCacheThreshold: 0.3\nanalyzerTuning:\n  muChoice: {maxRatio: 3, returnRatio: 2.5}\n",
		"slow":    "analyzerTuning:\n  steadyGate: {steadyFor: 10m}\n  itlBand: {minObservableK: 0.85}\n",
		"ratio":   "analyzerTuning:\n  muChoice: {maxRatio: 2}\n",
		"clean":   "kvCacheThreshold: 0.8\nanalyzerTuning:\n  steadyGate: {steadyFor: 10m, carry: 15m}\n",
	}
	configs, count, rejected, _ := parseScalingPolicyConfigRejects(data, logger)
	if count != 4 || len(configs) != 4 || len(rejected) != 0 {
		t.Fatalf("kept %d, rejected %v: every entry is valid alone", count, rejected)
	}
	warnings := map[string][]string{}
	for _, l := range lines {
		if !strings.Contains(l, "WARNING: tuning figure not used") {
			continue
		}
		for _, key := range []string{"default", "slow", "ratio", "clean"} {
			if strings.Contains(l, `"key"="`+key+`"`) {
				warnings[key] = append(warnings[key], l)
			}
		}
	}
	want := map[string][]string{
		"default": {"loadImbalance.idleKvUsage = 0.3 is not below kvCacheThreshold 0.3"},
		"slow": {
			"analyzerTuning.itlBand.minObservableK/maxObservableK = 0.85/0.8", "using the defaults 0.15/0.8",
			"analyzerTuning.steadyGate.steadyFor/carry = 10m0s/5m0s", "using the defaults 2m0s/5m0s",
			// Over the default entry's kvCacheThreshold of 0.3.
			"loadImbalance.idleKvUsage = 0.3 is not below kvCacheThreshold 0.3",
		},
		"ratio": {"analyzerTuning.muChoice.maxRatio/returnRatio = 2/2.5", "using the defaults 2/1.6"},
	}
	for key, parts := range want {
		all := strings.Join(warnings[key], "\n")
		for _, p := range parts {
			if !strings.Contains(all, p) {
				t.Errorf("%s: no warning containing %q; got:\n%s", key, p, all)
			}
		}
	}
	if len(warnings["slow"]) != 3 || len(warnings["ratio"]) != 2 {
		t.Errorf("slow: %d warnings, want 3; ratio: %d, want 2 (one per pair, plus the idle bound over default):\n%s\n%s",
			len(warnings["slow"]), len(warnings["ratio"]),
			strings.Join(warnings["slow"], "\n"), strings.Join(warnings["ratio"], "\n"))
	}
	if len(warnings["clean"]) != 0 {
		t.Errorf("clean entry warned: %v", warnings["clean"])
	}
}
