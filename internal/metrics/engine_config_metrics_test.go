package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// flagValues builds a positional value list of the right length, so a test can
// vary one flag without restating all of them.
func flagValues(overrides map[string]string) []string {
	out := make([]string, len(constants.EngineConfigFlagLabels))
	for i, l := range constants.EngineConfigFlagLabels {
		if v, ok := overrides[l]; ok {
			out[i] = v
			continue
		}
		out[i] = "x"
	}
	return out
}

func engineConfigEmitter(t *testing.T) (*MetricsEmitter, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	return NewMetricsEmitter(), registry
}

func TestRecordEngineConfigPublishesOneSeriesPerVariant(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)

	emitter.RecordEngineConfig("ns", "m", "v1", "NVIDIA-H200", 1, "abc123", 1,
		flagValues(nil))
	if got := countSeries(t, registry, constants.WVAEngineConfig); got != 1 {
		t.Fatalf("%d series after one variant, want 1", got)
	}

	// A second variant is a second series; a second call for the SAME variant
	// is not, which is the property that keeps this bounded by configuration
	// rather than by how often a cycle runs.
	emitter.RecordEngineConfig("ns", "m", "v2", "NVIDIA-H200", 1, "abc123", 1,
		flagValues(nil))
	emitter.RecordEngineConfig("ns", "m", "v1", "NVIDIA-H200", 1, "abc123", 1,
		flagValues(nil))
	if got := countSeries(t, registry, constants.WVAEngineConfig); got != 2 {
		t.Fatalf("%d series after two variants and a re-record, want 2", got)
	}
}

// TestRecordEngineConfigReplacesAChangedConfiguration is the leak this guards.
// A flag label is part of the series identity, so a variant whose
// configuration changes would otherwise leave its old label combination
// published for ever -- with both rows reading as current.
func TestRecordEngineConfigReplacesAChangedConfiguration(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)

	emitter.RecordEngineConfig("ns", "m", "v1", "NVIDIA-H200", 1, "old-digest", 1,
		flagValues(map[string]string{"max_num_seqs": "256"}))
	emitter.RecordEngineConfig("ns", "m", "v1", "NVIDIA-H200", 1, "new-digest", 1,
		flagValues(map[string]string{"max_num_seqs": "512"}))

	if got := countSeries(t, registry, constants.WVAEngineConfig); got != 1 {
		t.Fatalf("%d series after a configuration change, want 1: the old label "+
			"combination is still published", got)
	}
	if !hasSeries(t, registry, constants.WVAEngineConfig, constants.LabelFingerprint, "new-digest") {
		t.Error("the new fingerprint is not published")
	}
	if hasSeries(t, registry, constants.WVAEngineConfig, constants.LabelFingerprint, "old-digest") {
		t.Error("the superseded fingerprint is still published")
	}
}

func TestRecordEngineConfigLabelsCarryTheKeyAndTheFlags(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)

	emitter.RecordEngineConfig("ns", "Qwen/Qwen3-8B", "v1", "NVIDIA-H200", 4, "digest", 7,
		flagValues(map[string]string{"engine": "vllm", "enforce_eager": "true"}))

	// The four components of the key, so a learned figure can be read back to
	// the thing it describes.
	for _, c := range []struct{ label, want string }{
		{constants.LabelNamespace, "ns"},
		{constants.LabelModelName, "Qwen/Qwen3-8B"},
		{constants.LabelVariantName, "v1"},
		{constants.LabelAccelerator, "NVIDIA-H200"},
		{constants.LabelGPUsPerReplica, "4"},
		{constants.LabelFingerprint, "digest"},
		{constants.LabelFingerprintVersion, "7"},
		{"engine", "vllm"},
		{"enforce_eager", "true"},
	} {
		if !hasSeries(t, registry, constants.WVAEngineConfig, c.label, c.want) {
			t.Errorf("no series with %s=%q", c.label, c.want)
		}
	}
}

// TestRecordEngineConfigDropsAMismatchedValueList pins the choice to publish
// nothing rather than a configuration described by the wrong labels. The
// labels and values are paired positionally, so a short list would shift every
// flag onto its neighbour's label.
func TestRecordEngineConfigDropsAMismatchedValueList(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)

	emitter.RecordEngineConfig("ns", "m", "v1", "NVIDIA-H200", 1, "digest", 1,
		[]string{"only", "two"})
	if got := countSeries(t, registry, constants.WVAEngineConfig); got != 0 {
		t.Fatalf("%d series from a mismatched value list, want 0", got)
	}

	// An empty fingerprint means the configuration could not be read, and an
	// absent series is the honest signal for that.
	emitter.RecordEngineConfig("ns", "m", "v1", "NVIDIA-H200", 1, "", 1, flagValues(nil))
	if got := countSeries(t, registry, constants.WVAEngineConfig); got != 0 {
		t.Fatalf("%d series from an empty fingerprint, want 0", got)
	}
}

func TestDeleteEngineConfig(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)

	emitter.RecordEngineConfig("ns", "m", "v1", "NVIDIA-H200", 1, "d1", 1, flagValues(nil))
	emitter.RecordEngineConfig("ns", "m", "v2", "NVIDIA-H200", 1, "d2", 1, flagValues(nil))
	emitter.RecordEngineConfig("other", "m2", "v3", "NVIDIA-H200", 1, "d3", 1, flagValues(nil))

	emitter.DeleteEngineConfig("ns", "v1")
	if got := countSeries(t, registry, constants.WVAEngineConfig); got != 2 {
		t.Fatalf("%d series after deleting one variant, want 2", got)
	}
	if hasSeries(t, registry, constants.WVAEngineConfig, constants.LabelVariantName, "v1") {
		t.Error("v1 is still published after DeleteEngineConfig")
	}

	emitter.DeleteEngineConfigForModel("ns", "m")
	if got := countSeries(t, registry, constants.WVAEngineConfig); got != 1 {
		t.Fatalf("%d series after deleting the model, want 1 (the other namespace)", got)
	}
	if !hasSeries(t, registry, constants.WVAEngineConfig, constants.LabelVariantName, "v3") {
		t.Error("another namespace's variant was deleted with this model's")
	}
}

// TestEngineConfigDeletesAreSafeBeforeInit covers the nil-vec guards: a delete
// on an uninitialised registry must no-op rather than panic, because the
// engine's eviction paths run regardless of whether metrics were set up.
func TestEngineConfigDeletesAreSafeBeforeInit(t *testing.T) {
	saved := engineConfig
	engineConfig = nil
	defer func() { engineConfig = saved }()

	emitter := NewMetricsEmitter()
	emitter.DeleteEngineConfig("ns", "v1")
	emitter.DeleteEngineConfigForModel("ns", "m")
	emitter.RecordEngineConfig("ns", "m", "v1", "a", 1, "d", 1, flagValues(nil))
}
