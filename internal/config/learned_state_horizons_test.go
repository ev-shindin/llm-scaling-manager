package config

import (
	"os"
	"testing"
	"time"
)

// Does setting LEARNED_STATE_* reach Config at all?
//
// The analyzer's own specs prove that a horizon handed to WithHorizons decides
// what the sweep and the write path do. They cannot prove the operator can get
// a value that far: between the environment and WithHorizons sit three viper
// keys, and a typo in any of them is silent -- viper answers an unknown key
// with the zero value, which this code reads as "not configured" and replaces
// with the default. The knob would then be inert in exactly the way the
// analyzer specs were written to rule out, and every one of them would still
// pass. So the keys are asserted by name, from both sources viper reads.
func TestLoad_LearnedStateHorizons(t *testing.T) {
	t.Run("from the config file", func(t *testing.T) {
		t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
		configFile := writeTestConfigFile(t, `
LEARNED_STATE_EPISODE_GAP: "90m"
LEARNED_STATE_TIMEOUT: "72h"
LEARNED_STATE_RETENTION: "240h"
`)
		cfg, err := Load(nil, configFile)
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		gap, variant, bucket := cfg.LearnedStateHorizons()
		if gap != 90*time.Minute {
			t.Errorf("LEARNED_STATE_EPISODE_GAP: want 90m, got %v", gap)
		}
		if variant != 72*time.Hour {
			t.Errorf("LEARNED_STATE_TIMEOUT: want 72h, got %v", variant)
		}
		if bucket != 240*time.Hour {
			t.Errorf("LEARNED_STATE_RETENTION: want 240h, got %v", bucket)
		}
	})

	t.Run("from the environment", func(t *testing.T) {
		t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
		t.Setenv("LEARNED_STATE_EPISODE_GAP", "5m")
		t.Setenv("LEARNED_STATE_TIMEOUT", "10m")
		t.Setenv("LEARNED_STATE_RETENTION", "30m")

		cfg, err := Load(nil, "")
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		gap, variant, bucket := cfg.LearnedStateHorizons()
		if gap != 5*time.Minute || variant != 10*time.Minute || bucket != 30*time.Minute {
			t.Errorf("env horizons not read: got %v / %v / %v, want 5m / 10m / 30m",
				gap, variant, bucket)
		}
	})

	t.Run("unset is zero, which the analyzer reads as the default", func(t *testing.T) {
		// Not "unset is 24h": Config deliberately does not know the defaults.
		// Sanitizing in one place -- capacity.Horizons -- is what keeps the
		// floor from being enforced twice and disagreeing.
		for _, k := range []string{"LEARNED_STATE_EPISODE_GAP", "LEARNED_STATE_TIMEOUT", "LEARNED_STATE_RETENTION"} {
			if v, ok := os.LookupEnv(k); ok {
				t.Fatalf("precondition: %s is set to %q in this process", k, v)
			}
		}
		t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")

		cfg, err := Load(nil, "")
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		gap, variant, bucket := cfg.LearnedStateHorizons()
		if gap != 0 || variant != 0 || bucket != 0 {
			t.Errorf("unset should be zero, got %v / %v / %v", gap, variant, bucket)
		}
	})

	t.Run("an unusable value is passed through, not corrected here", func(t *testing.T) {
		// "24" is 24 nanoseconds to viper. Config hands it on exactly as
		// written; capacity.Horizons.Sanitized is what floors it, and it is
		// the only thing that logs the replacement. A silent correction here
		// would make that log line a lie.
		t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
		configFile := writeTestConfigFile(t, `LEARNED_STATE_TIMEOUT: "24"`)

		cfg, err := Load(nil, configFile)
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		if _, variant, _ := cfg.LearnedStateHorizons(); variant != 24*time.Nanosecond {
			t.Errorf("want the unusable value passed through as 24ns, got %v", variant)
		}
	})
}
