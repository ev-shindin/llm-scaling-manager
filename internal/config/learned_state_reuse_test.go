package config

import "testing"

// TestMergeCarriesEnableLearnedStateReuse: Merge is the single overlay every
// per-entry setting resolves through, so a field it forgets is silently
// unconfigurable per model however correct the struct looks.
//
// This key was added without it, and a review caught that. It happened to work
// from a `default:` entry -- that one is taken wholesale as the base -- and
// silently did nothing in a named tier or a per-model override, which is the
// scope the key is actually for.
//
// The field is `Enable`, not `Disable`, and the direction is the whole point.
// Every override in Merge is one-way for a plain bool: a false is
// indistinguishable from "not set". Spelled as a disable with a default of
// true, reuse could never have been turned back on by anybody -- not a tier,
// not a model -- which is a removed feature wearing a switch. Spelled as an
// enable with a default of false, the same one-way idiom runs in the direction
// that is actually wanted: off everywhere, on where someone asks.
func TestMergeCarriesEnableLearnedStateReuse(t *testing.T) {
	t.Run("a per-model override turns reuse on", func(t *testing.T) {
		base := ScalingPolicy{}
		base.Merge(ScalingPolicy{EnableLearnedStateReuse: true})
		if !base.EnableLearnedStateReuse {
			t.Error("an override that enables reuse must survive Merge, or the key " +
				"does nothing outside a default: entry")
		}
	})

	t.Run("an override silent on it inherits the default", func(t *testing.T) {
		base := ScalingPolicy{EnableLearnedStateReuse: true}
		base.Merge(ScalingPolicy{ScaleUpThreshold: 0.9})
		if !base.EnableLearnedStateReuse {
			t.Error("an override that says nothing about reuse must not re-disable it")
		}
	})

	// THE LIMITATION, asserted so it is a recorded decision rather than a
	// surprise. It is the mirror of the old one and it now points the harmless
	// way: a model cannot opt back OUT where a `default:` entry opted in. That
	// is recoverable by editing the default entry; the old direction was not
	// recoverable at all.
	t.Run("but it cannot be turned back off per model", func(t *testing.T) {
		base := ScalingPolicy{EnableLearnedStateReuse: true}
		base.Merge(ScalingPolicy{EnableLearnedStateReuse: false})
		if !base.EnableLearnedStateReuse {
			t.Error("a false override is indistinguishable from unset in this merge " +
				"idiom; if this now fails, the field became a *bool and the doc " +
				"comment on Merge needs re-deriving")
		}
	})

	// And the default. Reuse is OFF unless somebody asks for it, because the
	// equality that decides who may share is not sound: five adversarial
	// rounds found seven measured ways to produce a false match or destroy a
	// true one, and two attempts to close them inside the parser were reverted
	// for doing more harm than good. Everything that reads the digest ships;
	// only the act of pricing one variant from another's reading waits.
	t.Run("the zero value leaves reuse disabled", func(t *testing.T) {
		var p ScalingPolicy
		if p.EnableLearnedStateReuse {
			t.Error("the zero value must leave reuse disabled")
		}
	})
}

// TestMergeCarriesDisableK2Borrow: the k2 borrow's switch is for one model in
// an incident, so it must survive Merge from a per-model override -- the same
// trap TestMergeCarriesEnableLearnedStateReuse records -- and the borrow is on
// when nobody says otherwise.
func TestMergeCarriesDisableK2Borrow(t *testing.T) {
	t.Run("a per-model override turns the borrow off", func(t *testing.T) {
		base := ScalingPolicy{}
		base.Merge(ScalingPolicy{DisableK2Borrow: true})
		if !base.DisableK2Borrow {
			t.Error("an override that disables the borrow must survive Merge")
		}
	})
	t.Run("the zero value leaves the borrow on", func(t *testing.T) {
		var p ScalingPolicy
		if p.DisableK2Borrow {
			t.Error("the borrow is lower-only and on by default")
		}
	})
}

// TestMergeCarriesEnablePrefillThroughputBound: the prefill bound is off by
// default, and either a `default:` entry or a model's own entry turns it on.
func TestMergeCarriesEnablePrefillThroughputBound(t *testing.T) {
	t.Run("the zero value leaves the bound off", func(t *testing.T) {
		var p ScalingPolicy
		if p.EnablePrefillThroughputBound {
			t.Error("the prefill bound is opt-in")
		}
	})
	t.Run("a per-model override turns the bound on", func(t *testing.T) {
		base := ScalingPolicy{}
		base.Merge(ScalingPolicy{EnablePrefillThroughputBound: true})
		if !base.EnablePrefillThroughputBound {
			t.Error("an override that enables the prefill bound must survive Merge")
		}
	})
	t.Run("a default entry that turns it on reaches a model that says nothing", func(t *testing.T) {
		base := ScalingPolicy{EnablePrefillThroughputBound: true}
		base.Merge(ScalingPolicy{ScaleUpThreshold: 0.9})
		if !base.EnablePrefillThroughputBound {
			t.Error("an override silent on the bound must not turn it off")
		}
	})
}

// TestMergeCarriesEnableBatchCapLearning: batch-cap learning is off by
// default, and either a `default:` entry or a model's own entry turns it on.
func TestMergeCarriesEnableBatchCapLearning(t *testing.T) {
	var p ScalingPolicy
	if p.EnableBatchCapLearning {
		t.Error("batch-cap learning is opt-in")
	}
	base := ScalingPolicy{}
	base.Merge(ScalingPolicy{EnableBatchCapLearning: true})
	if !base.EnableBatchCapLearning {
		t.Error("an override that enables batch-cap learning must survive Merge")
	}
	base = ScalingPolicy{EnableBatchCapLearning: true}
	base.Merge(ScalingPolicy{ScaleUpThreshold: 0.9})
	if !base.EnableBatchCapLearning {
		t.Error("an override silent on batch-cap learning must not turn it off")
	}
}
