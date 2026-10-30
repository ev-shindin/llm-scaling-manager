package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// RecordedValue and SetRecordedValue carry what the learner recorder cannot
// read from the unexported fields, the decode error included.
func TestModelWeight_RecordedValueRoundTrips(t *testing.T) {
	var failed ScalingPolicy
	if err := yaml.Unmarshal([]byte("weight: heavy\n"), &failed); err != nil {
		t.Fatal(err)
	}
	for name, w := range map[string]ModelWeight{"unset": {}, "set": NewModelWeight(2.5), "failed": failed.Weight} {
		var back ModelWeight
		if err := back.SetRecordedValue(w.RecordedValue().(RecordedWeight)); err != nil {
			t.Fatal(err)
		}
		if back.IsSet() != w.IsSet() || back.Value() != w.Value() || (back.Err() == nil) != (w.Err() == nil) {
			t.Errorf("%s: %+v came back as %+v", name, w, back)
		}
		if w.Err() != nil && back.Err().Error() != w.Err().Error() {
			t.Errorf("%s: error %q came back as %q", name, w.Err(), back.Err())
		}
	}
}

func TestOptimizerConfig_RecordedValueRoundTrips(t *testing.T) {
	var failed struct {
		Optimizer OptimizerConfig `yaml:"optimizer"`
	}
	if err := yaml.Unmarshal([]byte("optimizer:\n  nope: 1\n"), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Optimizer.decodeErr == nil {
		t.Fatal("precondition: the block did not decode")
	}
	set := OptimizerConfig{Type: "utilization-share", UtilizationShare: &UtilizationShareConfig{ReserveGPUs: 2}}
	for name, o := range map[string]OptimizerConfig{"zero": {}, "set": set, "failed": failed.Optimizer} {
		var back OptimizerConfig
		if err := back.SetRecordedValue(o.RecordedValue().(RecordedOptimizer)); err != nil {
			t.Fatal(err)
		}
		if back.Type != o.Type || (back.UtilizationShare == nil) != (o.UtilizationShare == nil) ||
			(back.decodeErr == nil) != (o.decodeErr == nil) {
			t.Errorf("%s: %+v came back as %+v", name, o, back)
		}
		if o.decodeErr != nil && back.decodeErr.Error() != o.decodeErr.Error() {
			t.Errorf("%s: error %q came back as %q", name, o.decodeErr, back.decodeErr)
		}
	}
}
