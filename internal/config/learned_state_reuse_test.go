package config

import "testing"

// TestMergeCarriesDisableLearnedStateReuse: Merge is the single overlay every
// per-entry setting resolves through, so a field it forgets is silently
// unconfigurable per model however correct the struct looks.
//
// This key was added without it, and a review caught that. It happened to work
// from a `default:` entry -- that one is taken wholesale as the base -- and
// silently did nothing in a named tier or a per-model override, which is the
// scope the key is actually for: the case it exists for is ONE variant being
// priced from a sibling it is not really like.
func TestMergeCarriesDisableLearnedStateReuse(t *testing.T) {
	t.Run("a per-model override turns reuse off", func(t *testing.T) {
		base := ScalingPolicy{}
		base.Merge(ScalingPolicy{DisableLearnedStateReuse: true})
		if !base.DisableLearnedStateReuse {
			t.Error("an override that disables reuse must survive Merge, or the key " +
				"does nothing outside a default: entry")
		}
	})

	t.Run("an override silent on it inherits the default", func(t *testing.T) {
		base := ScalingPolicy{DisableLearnedStateReuse: true}
		base.Merge(ScalingPolicy{ScaleUpThreshold: 0.9})
		if !base.DisableLearnedStateReuse {
			t.Error("an override that says nothing about reuse must not re-enable it")
		}
	})

	// THE LIMITATION, asserted so it is a recorded decision rather than a
	// surprise. Every override in Merge is one-way for a plain bool: a false is
	// indistinguishable from "not set", so a model cannot opt BACK IN where the
	// default opted out. ScaleToZeroEnvelope.Enabled is a *bool for exactly this
	// reason, and this field would have to become one too if re-enabling per
	// model is ever wanted.
	t.Run("but it cannot be turned back on per model", func(t *testing.T) {
		base := ScalingPolicy{DisableLearnedStateReuse: true}
		base.Merge(ScalingPolicy{DisableLearnedStateReuse: false})
		if !base.DisableLearnedStateReuse {
			t.Error("a false override is indistinguishable from unset in this merge " +
				"idiom; if this now passes, the field became a *bool and the doc " +
				"comment on Merge needs re-deriving")
		}
	})

	// And the default: reuse is ON unless an operator turns it off, because it
	// is what lets a scaled-out variant price its first decision from something
	// other than a guess. A zero value that disabled it would change every
	// existing deployment on upgrade.
	t.Run("the zero value leaves reuse enabled", func(t *testing.T) {
		var p ScalingPolicy
		if p.DisableLearnedStateReuse {
			t.Error("the zero value must leave reuse enabled")
		}
	})
}
