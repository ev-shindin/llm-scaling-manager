package config

import (
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// PolicyDuration is a time.Duration read from a Go duration string ("2m",
// "90s"). A bare number is refused rather than read as nanoseconds. Zero means
// unset: the key takes its default.
type PolicyDuration time.Duration

// Duration returns d as a time.Duration.
func (d PolicyDuration) Duration() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses a Go duration string.
func (d *PolicyDuration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if node.Tag != "!!str" || node.Decode(&s) != nil {
		return fmt.Errorf("%q must be a duration string such as \"2m\"", node.Value)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration (use a unit, e.g. \"2m\" or \"90s\"): %w", s, err)
	}
	*d = PolicyDuration(parsed)
	return nil
}

// MarshalYAML writes the duration back as a Go duration string.
func (d PolicyDuration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// AnalyzerTuning holds the saturation analyzer's advanced tuning: the figures
// behind its admission and pricing judgements. Every field is optional; zero
// or absent takes the default, which is the value the analyzer was calibrated
// with. Merged field by field, so a per-model entry can change one figure and
// inherit the rest.
//
// None of these keys raises, floors, ages or resets k2. The k2 settle figures
// only decide which saturated readings k2 may learn from.
type AnalyzerTuning struct {
	// ITLBand is the KV band inside which a replica's (k, ITL) readings teach
	// the ITL line, and the sequence-cap exception above it.
	ITLBand ITLBandTuning `yaml:"itlBand,omitempty"`
	// SteadyGate decides which (k, ITL) readings the ITL line is fitted from.
	SteadyGate SteadyGateTuning `yaml:"steadyGate,omitempty"`
	// K2Settle decides when a saturated reading whose queue is deeper than its
	// whole resident batch is a replica at its limit, not a load step.
	K2Settle K2SettleTuning `yaml:"k2Settle,omitempty"`
	// MuChoice decides between the measured and the derived service rate.
	MuChoice MuChoiceTuning `yaml:"muChoice,omitempty"`
	// Admission decides which saturated readings are service-rate samples.
	Admission AdmissionTuning `yaml:"admission,omitempty"`
	// FloorTrust decides how far the throughput floor's drain price may
	// replace the queues' residency charge.
	FloorTrust FloorTrustTuning `yaml:"floorTrust,omitempty"`
	// BacklogGrowth is the noise band a role's backlog must grow past to
	// count as growing.
	BacklogGrowth BacklogGrowthTuning `yaml:"backlogGrowth,omitempty"`
}

// ITLBandTuning is the KV band the ITL line is fitted over
// (internal/signals/itl, Admission).
type ITLBandTuning struct {
	// MinObservableK and MaxObservableK are the band's edges, as fractions of
	// a replica's cache. Below the lower edge the engine is barely batching
	// and its latency is noise. Above the upper edge an engine short of
	// memory preempts, and its latency leaves the line: on a benchmark run,
	// fitting up to 0.85 instead of 0.80 pushed the intercept below zero
	// (itl.DefaultMaxObservableK has the table). Validated 0 < min < max < 1.
	MinObservableK float64 `yaml:"minObservableK,omitempty"`
	MaxObservableK float64 `yaml:"maxObservableK,omitempty"`
	// CapMaxPreemptionRate is the most preemptions per second, over the
	// inter-token latency's window, a replica at its sequence cap may show
	// and still have its reading above MaxObservableK teach the line (issue
	// #287). Nil takes DefaultITLCapMaxPreemptionRate, zero: no preemption at
	// all. A pointer, because zero is the default and a meaningful setting:
	// an entry must be able to set 0 over a tier that raised it.
	CapMaxPreemptionRate *float64 `yaml:"capMaxPreemptionRate,omitempty"`
}

// SteadyGateTuning is the ITL steady-state gate (internal/signals/itl).
type SteadyGateTuning struct {
	// SteadyFor is how long a replica's KV fill must have held still before
	// its (k, ITL) pair may teach the full fit.
	SteadyFor PolicyDuration `yaml:"steadyFor,omitempty"`
	// MaxKDrift is how far a replica's KV fill may move within SteadyFor and
	// its reading still count as steady.
	MaxKDrift float64 `yaml:"maxKDrift,omitempty"`
	// Carry is how long a steady reading may outlive the oldest reading of
	// the main window, so a load step's old level is still there when the new
	// one settles.
	Carry PolicyDuration `yaml:"carry,omitempty"`
	// IdleRows is how many rows in a row with neither an ITL nor a KV fill
	// make a replica idle rather than one that missed a scrape.
	IdleRows int `yaml:"idleRows,omitempty"`
	// MinSettledSamples is the fewest readings from replicas past their
	// settle the pinned fit is taken from; below it the last slope holds.
	MinSettledSamples int `yaml:"minSettledSamples,omitempty"`
}

// K2SettleTuning is the k2 settle test (saturation/k2_settle.go). Its window
// is the collector's rate window and is not configured.
type K2SettleTuning struct {
	// RateFraction is how close to the rate and the k2 on record a suspect
	// reading must be to count.
	RateFraction float64 `yaml:"rateFraction,omitempty"`
	// MaxRise is how much a suspect reading's resident KV may have moved, up
	// or down, over one rate window and still count as settled.
	MaxRise float64 `yaml:"maxRise,omitempty"`
}

// MuChoiceTuning chooses between the measured and the derived mu
// (saturation/throughput_admission.go).
type MuChoiceTuning struct {
	// MinMeasuredOfDerived is the smallest fraction of the derived mu a
	// saturated reading may be and still be recorded at once.
	MinMeasuredOfDerived float64 `yaml:"minMeasuredOfDerived,omitempty"`
	// MaxRatio is how far a measured mu may disagree with the derived one, or
	// sit under what a replica served, before it gives way.
	MaxRatio float64 `yaml:"maxRatio,omitempty"`
	// ReturnRatio is how close a measured mu that gave way must come back
	// before it prices again: the hysteresis band under MaxRatio.
	ReturnRatio float64 `yaml:"returnRatio,omitempty"`
	// FreshFor is how long after its bucket last read saturated in agreement
	// with it a measured mu outranks a derived one it disagrees with.
	FreshFor PolicyDuration `yaml:"freshFor,omitempty"`
	// LowHoldFor is how long a measured mu more than MaxRatio under the
	// derived one keeps pricing after its bucket last agreed with it.
	LowHoldFor PolicyDuration `yaml:"lowHoldFor,omitempty"`
}

// AdmissionTuning is the throughput-sample admission
// (saturation/throughput_admission.go).
type AdmissionTuning struct {
	// MaxTokensOverCompletions is the most a replica's generated-token rate
	// may be over the tokens its completions account for, for its rate to be
	// a service rate rather than a batch filling.
	MaxTokensOverCompletions float64 `yaml:"maxTokensOverCompletions,omitempty"`
	// SiblingMuTolerance is how far under what its full siblings serve a
	// measured mu may sit before it yields to them.
	SiblingMuTolerance float64 `yaml:"siblingMuTolerance,omitempty"`
}

// FloorTrustTuning is the throughput floor's trust ramp
// (saturation/throughput_floor.go).
type FloorTrustTuning struct {
	// LagFraction is the share of the resident KV the arrival rate must
	// account for before the floor may lower a role's demand at all.
	LagFraction float64 `yaml:"lagFraction,omitempty"`
	// TrustFraction is the share at which the floor's drain price is trusted
	// in full. Between the two the prices are blended.
	TrustFraction float64 `yaml:"trustFraction,omitempty"`
}

// BacklogGrowthTuning is the band a backlog must grow past, over one rate
// window, to count as growing: max(MinRequests, Fraction of the backlog).
type BacklogGrowthTuning struct {
	// MinRequests is the least growth, in requests, that counts.
	MinRequests float64 `yaml:"minRequests,omitempty"`
	// Fraction is the least growth, as a share of the earlier backlog, that
	// counts.
	Fraction float64 `yaml:"fraction,omitempty"`
}

// LoadImbalance holds the load-imbalance diagnostic's tuning
// (internal/signals/balance). IdleKvUsage is also the saturation analyzer's
// idle test, so the two cannot disagree on what idle means. A collection that
// fails for SustainFor drops the model's load-distribution series.
type LoadImbalance struct {
	// IdleKvUsage is the busiest-rank KV utilization below which a Ready
	// replica with nothing waiting is idle.
	IdleKvUsage float64 `yaml:"idleKvUsage,omitempty"`
	// IdleAfterReady is how long a replica must have been Ready before it can
	// read idle.
	IdleAfterReady PolicyDuration `yaml:"idleAfterReady,omitempty"`
	// SustainFor is how long a role must read imbalanced, on every cycle,
	// before it is reported.
	SustainFor PolicyDuration `yaml:"sustainFor,omitempty"`
	// ClearAfter is how long a reported role must read balanced, on every
	// cycle, before the report clears.
	ClearAfter PolicyDuration `yaml:"clearAfter,omitempty"`
}

// Defaults of AnalyzerTuning and LoadImbalance: the values the analyzer and
// the diagnostic were calibrated with. The packages that read them keep the
// same figures as their own constants, and a test in the saturation package
// checks the two agree.
const (
	// The ITL band equals itl.DefaultMinObservableK and
	// itl.DefaultMaxObservableK; a test holds them equal.
	DefaultITLMinObservableK        = 0.15
	DefaultITLMaxObservableK        = 0.80
	DefaultITLCapMaxPreemptionRate  = 0.0
	DefaultSteadyFor                = PolicyDuration(2 * time.Minute)
	DefaultMaxKDrift                = 0.10
	DefaultSteadyCarry              = PolicyDuration(5 * time.Minute)
	DefaultIdleRows                 = 2
	DefaultMinSettledSamples        = 3
	DefaultK2SettleRateFraction     = 0.7
	DefaultK2SettleMaxRise          = 0.25
	DefaultMinMeasuredOfDerived     = 0.5
	DefaultMeasuredDerivedMaxRatio  = 2.0
	DefaultMeasuredDerivedReturn    = 1.6
	DefaultMeasuredMuFreshFor       = PolicyDuration(5 * time.Minute)
	DefaultMeasuredLowHoldFor       = PolicyDuration(30 * time.Minute)
	DefaultMaxTokensOverCompletions = 1.15
	DefaultSiblingMuTolerance       = 0.8
	DefaultFloorLagFraction         = 0.5
	DefaultFloorTrustFraction       = 0.8
	DefaultBacklogGrowthMinRequests = 2.0
	DefaultBacklogGrowthFraction    = 0.10
	DefaultIdleKvUsage              = 0.30
	DefaultIdleAfterReady           = PolicyDuration(2 * time.Minute)
	DefaultImbalanceSustainFor      = PolicyDuration(2 * time.Minute)
	DefaultImbalanceClearAfter      = PolicyDuration(2 * time.Minute)
)

// Bounds that are not a fraction's natural (0, 1).
const (
	// MinSteadyFor is the shortest steadyFor: the ITL rate window, which the
	// steady gate exists to wait out.
	MinSteadyFor = PolicyDuration(time.Minute)
	// MinImbalanceWindow is the shortest idleAfterReady and sustainFor: the
	// one-minute window the KV and queue peaks are read over.
	MinImbalanceWindow = PolicyDuration(time.Minute)
	// MaxTuningDuration bounds every tuning duration, so a typo of hours does
	// not hold a gate for a day.
	MaxTuningDuration = PolicyDuration(24 * time.Hour)
	// MaxTuningCount bounds idleRows and minSettledSamples.
	MaxTuningCount = 1000
	// MaxTokensOverCompletionsLimit bounds maxTokensOverCompletions.
	MaxTokensOverCompletionsLimit = 10.0
	// MaxMeasuredDerivedRatio bounds maxRatio.
	MaxMeasuredDerivedRatio = 100.0
	// MaxBacklogGrowthMinRequests bounds backlogGrowth.minRequests: a band of
	// a million requests is a typo, not a noise floor.
	MaxBacklogGrowthMinRequests = 1e6
)

func defaultFloat(v *float64, d float64) {
	if *v == 0 {
		*v = d
	}
}

func defaultInt(v *int, d int) {
	if *v == 0 {
		*v = d
	}
}

func defaultDuration(v *PolicyDuration, d PolicyDuration) {
	if *v == 0 {
		*v = d
	}
}

// WithDefaults returns t with every unset field at its default.
func (t AnalyzerTuning) WithDefaults() AnalyzerTuning {
	b := &t.ITLBand
	defaultFloat(&b.MinObservableK, DefaultITLMinObservableK)
	defaultFloat(&b.MaxObservableK, DefaultITLMaxObservableK)
	// Each edge is checked alone, so a merge can invert the pair; an
	// inverted or out-of-range band reads as the defaults, never as a band
	// that admits nothing. CapMaxPreemptionRate stays nil when unset
	// (ITLCapPreemptionRate).
	if !(b.MinObservableK > 0 && b.MinObservableK < b.MaxObservableK && b.MaxObservableK < 1) {
		b.MinObservableK, b.MaxObservableK = DefaultITLMinObservableK, DefaultITLMaxObservableK
	}
	g := &t.SteadyGate
	defaultDuration(&g.SteadyFor, DefaultSteadyFor)
	defaultFloat(&g.MaxKDrift, DefaultMaxKDrift)
	defaultDuration(&g.Carry, DefaultSteadyCarry)
	defaultInt(&g.IdleRows, DefaultIdleRows)
	defaultInt(&g.MinSettledSamples, DefaultMinSettledSamples)
	defaultFloat(&t.K2Settle.RateFraction, DefaultK2SettleRateFraction)
	defaultFloat(&t.K2Settle.MaxRise, DefaultK2SettleMaxRise)
	m := &t.MuChoice
	defaultFloat(&m.MinMeasuredOfDerived, DefaultMinMeasuredOfDerived)
	defaultFloat(&m.MaxRatio, DefaultMeasuredDerivedMaxRatio)
	defaultFloat(&m.ReturnRatio, DefaultMeasuredDerivedReturn)
	defaultDuration(&m.FreshFor, DefaultMeasuredMuFreshFor)
	defaultDuration(&m.LowHoldFor, DefaultMeasuredLowHoldFor)
	defaultFloat(&t.Admission.MaxTokensOverCompletions, DefaultMaxTokensOverCompletions)
	defaultFloat(&t.Admission.SiblingMuTolerance, DefaultSiblingMuTolerance)
	defaultFloat(&t.FloorTrust.LagFraction, DefaultFloorLagFraction)
	defaultFloat(&t.FloorTrust.TrustFraction, DefaultFloorTrustFraction)
	defaultFloat(&t.BacklogGrowth.MinRequests, DefaultBacklogGrowthMinRequests)
	defaultFloat(&t.BacklogGrowth.Fraction, DefaultBacklogGrowthFraction)
	t.repairCrossedPairs()
	return t
}

// repairCrossedPairs puts each pair whose two keys are crossed back to both
// defaults, as the ITL band is. An entry's validate checks a pair only where
// it sets both keys, so a key set alone, or merged over a partner from
// another layer, can cross it; no consumer may see a crossed pair:
//
//   - floorTrust: trustFraction at or under lagFraction, which the ramp
//     divides by the difference of;
//   - steadyGate: carry under steadyFor, so a steady reading would age out
//     before it could be steady;
//   - muChoice: returnRatio over maxRatio, which removes the hysteresis band
//     the latch exists for, so the choice flips with derived's noise;
//   - muChoice: lowHoldFor under freshFor, so a low measurement would stop
//     pricing before it is even stale.
func (t *AnalyzerTuning) repairCrossedPairs() {
	if !(t.FloorTrust.TrustFraction > t.FloorTrust.LagFraction) {
		t.FloorTrust.LagFraction, t.FloorTrust.TrustFraction = DefaultFloorLagFraction, DefaultFloorTrustFraction
	}
	if g := &t.SteadyGate; g.Carry < g.SteadyFor {
		g.SteadyFor, g.Carry = DefaultSteadyFor, DefaultSteadyCarry
	}
	if m := &t.MuChoice; m.ReturnRatio > m.MaxRatio {
		m.MaxRatio, m.ReturnRatio = DefaultMeasuredDerivedMaxRatio, DefaultMeasuredDerivedReturn
	}
	if m := &t.MuChoice; m.LowHoldFor < m.FreshFor {
		m.FreshFor, m.LowHoldFor = DefaultMeasuredMuFreshFor, DefaultMeasuredLowHoldFor
	}
}

// WithDefaults returns l with every unset field at its default.
func (l LoadImbalance) WithDefaults() LoadImbalance {
	defaultFloat(&l.IdleKvUsage, DefaultIdleKvUsage)
	defaultDuration(&l.IdleAfterReady, DefaultIdleAfterReady)
	defaultDuration(&l.SustainFor, DefaultImbalanceSustainFor)
	defaultDuration(&l.ClearAfter, DefaultImbalanceClearAfter)
	return l
}

func mergeFloat(dst *float64, v float64) {
	if v != 0 {
		*dst = v
	}
}

func mergeInt(dst *int, v int) {
	if v != 0 {
		*dst = v
	}
}

func mergeDuration(dst *PolicyDuration, v PolicyDuration) {
	if v != 0 {
		*dst = v
	}
}

// merge overlays the fields o sets onto t.
func (t *AnalyzerTuning) merge(o AnalyzerTuning) {
	mergeFloat(&t.ITLBand.MinObservableK, o.ITLBand.MinObservableK)
	mergeFloat(&t.ITLBand.MaxObservableK, o.ITLBand.MaxObservableK)
	if o.ITLBand.CapMaxPreemptionRate != nil {
		v := *o.ITLBand.CapMaxPreemptionRate
		t.ITLBand.CapMaxPreemptionRate = &v
	}
	mergeDuration(&t.SteadyGate.SteadyFor, o.SteadyGate.SteadyFor)
	mergeFloat(&t.SteadyGate.MaxKDrift, o.SteadyGate.MaxKDrift)
	mergeDuration(&t.SteadyGate.Carry, o.SteadyGate.Carry)
	mergeInt(&t.SteadyGate.IdleRows, o.SteadyGate.IdleRows)
	mergeInt(&t.SteadyGate.MinSettledSamples, o.SteadyGate.MinSettledSamples)
	mergeFloat(&t.K2Settle.RateFraction, o.K2Settle.RateFraction)
	mergeFloat(&t.K2Settle.MaxRise, o.K2Settle.MaxRise)
	mergeFloat(&t.MuChoice.MinMeasuredOfDerived, o.MuChoice.MinMeasuredOfDerived)
	mergeFloat(&t.MuChoice.MaxRatio, o.MuChoice.MaxRatio)
	mergeFloat(&t.MuChoice.ReturnRatio, o.MuChoice.ReturnRatio)
	mergeDuration(&t.MuChoice.FreshFor, o.MuChoice.FreshFor)
	mergeDuration(&t.MuChoice.LowHoldFor, o.MuChoice.LowHoldFor)
	mergeFloat(&t.Admission.MaxTokensOverCompletions, o.Admission.MaxTokensOverCompletions)
	mergeFloat(&t.Admission.SiblingMuTolerance, o.Admission.SiblingMuTolerance)
	mergeFloat(&t.FloorTrust.LagFraction, o.FloorTrust.LagFraction)
	mergeFloat(&t.FloorTrust.TrustFraction, o.FloorTrust.TrustFraction)
	mergeFloat(&t.BacklogGrowth.MinRequests, o.BacklogGrowth.MinRequests)
	mergeFloat(&t.BacklogGrowth.Fraction, o.BacklogGrowth.Fraction)
}

// merge overlays the fields o sets onto l.
func (l *LoadImbalance) merge(o LoadImbalance) {
	mergeFloat(&l.IdleKvUsage, o.IdleKvUsage)
	mergeDuration(&l.IdleAfterReady, o.IdleAfterReady)
	mergeDuration(&l.SustainFor, o.SustainFor)
	mergeDuration(&l.ClearAfter, o.ClearAfter)
}

// fraction checks a set value is in (0, 1), or (0, 1] when one is allowed.
func fraction(key string, v float64, oneAllowed bool) error {
	if v == 0 {
		return nil
	}
	if v < 0 || v > 1 || (v == 1 && !oneAllowed) {
		if oneAllowed {
			return fmt.Errorf("%s must be in (0, 1], got %g", key, v)
		}
		return fmt.Errorf("%s must be in (0, 1), got %g", key, v)
	}
	return nil
}

// within checks a set value is in [lo, hi].
func within(key string, v, lo, hi float64) error {
	if v != 0 && (v < lo || v > hi) {
		return fmt.Errorf("%s must be in [%g, %g], got %g", key, lo, hi, v)
	}
	return nil
}

// durationWithin checks a set duration is in [lo, MaxTuningDuration].
func durationWithin(key string, v, lo PolicyDuration) error {
	if v != 0 && (v < lo || v > MaxTuningDuration) {
		return fmt.Errorf("%s must be in [%v, %v], got %v", key, lo.Duration(), MaxTuningDuration.Duration(), v.Duration())
	}
	return nil
}

// countWithin checks a set count is in [1, MaxTuningCount].
func countWithin(key string, v int) error {
	if v != 0 && (v < 1 || v > MaxTuningCount) {
		return fmt.Errorf("%s must be in [1, %d], got %d", key, MaxTuningCount, v)
	}
	return nil
}

// validate checks each field the entry sets, and each pair rule where the
// entry sets both keys of the pair.
func (t AnalyzerTuning) validate() error {
	const p = "analyzerTuning."
	g, m := t.SteadyGate, t.MuChoice
	var capRate error
	if r := t.ITLBand.CapMaxPreemptionRate; r != nil && *r < 0 {
		capRate = fmt.Errorf("%sitlBand.capMaxPreemptionRate must be >= 0, got %g", p, *r)
	}
	err := errors.Join(
		fraction(p+"itlBand.minObservableK", t.ITLBand.MinObservableK, false),
		fraction(p+"itlBand.maxObservableK", t.ITLBand.MaxObservableK, false),
		capRate,
		durationWithin(p+"steadyGate.steadyFor", g.SteadyFor, MinSteadyFor),
		fraction(p+"steadyGate.maxKDrift", g.MaxKDrift, false),
		durationWithin(p+"steadyGate.carry", g.Carry, MinSteadyFor),
		countWithin(p+"steadyGate.idleRows", g.IdleRows),
		countWithin(p+"steadyGate.minSettledSamples", g.MinSettledSamples),
		fraction(p+"k2Settle.rateFraction", t.K2Settle.RateFraction, true),
		fraction(p+"k2Settle.maxRise", t.K2Settle.MaxRise, false),
		fraction(p+"muChoice.minMeasuredOfDerived", m.MinMeasuredOfDerived, true),
		within(p+"muChoice.maxRatio", m.MaxRatio, 1, MaxMeasuredDerivedRatio),
		within(p+"muChoice.returnRatio", m.ReturnRatio, 1, MaxMeasuredDerivedRatio),
		durationWithin(p+"muChoice.freshFor", m.FreshFor, PolicyDuration(time.Second)),
		durationWithin(p+"muChoice.lowHoldFor", m.LowHoldFor, PolicyDuration(time.Second)),
		within(p+"admission.maxTokensOverCompletions", t.Admission.MaxTokensOverCompletions, 1, MaxTokensOverCompletionsLimit),
		fraction(p+"admission.siblingMuTolerance", t.Admission.SiblingMuTolerance, true),
		fraction(p+"floorTrust.lagFraction", t.FloorTrust.LagFraction, false),
		fraction(p+"floorTrust.trustFraction", t.FloorTrust.TrustFraction, true),
		within(p+"backlogGrowth.minRequests", t.BacklogGrowth.MinRequests, 0, MaxBacklogGrowthMinRequests),
		fraction(p+"backlogGrowth.fraction", t.BacklogGrowth.Fraction, false),
	)
	if err != nil {
		return err
	}
	// Relations, only where the entry sets both keys of the pair. One key set
	// alone meets its partner from the layer below, which this entry cannot
	// see; the resolved policy repairs a pair the merge crossed
	// (repairCrossedPairs).
	b, f := t.ITLBand, t.FloorTrust
	switch {
	case b.MinObservableK != 0 && b.MaxObservableK != 0 && b.MinObservableK >= b.MaxObservableK:
		return fmt.Errorf("%sitlBand.minObservableK (%g) must be below itlBand.maxObservableK (%g)",
			p, b.MinObservableK, b.MaxObservableK)
	case g.SteadyFor != 0 && g.Carry != 0 && g.Carry < g.SteadyFor:
		return fmt.Errorf("%ssteadyGate.carry (%v) must be at least steadyGate.steadyFor (%v)",
			p, g.Carry.Duration(), g.SteadyFor.Duration())
	case m.MaxRatio != 0 && m.ReturnRatio != 0 && m.ReturnRatio > m.MaxRatio:
		return fmt.Errorf("%smuChoice.returnRatio (%g) must be at most muChoice.maxRatio (%g)",
			p, m.ReturnRatio, m.MaxRatio)
	case m.FreshFor != 0 && m.LowHoldFor != 0 && m.LowHoldFor < m.FreshFor:
		return fmt.Errorf("%smuChoice.lowHoldFor (%v) must be at least muChoice.freshFor (%v)",
			p, m.LowHoldFor.Duration(), m.FreshFor.Duration())
	case f.LagFraction != 0 && f.TrustFraction != 0 && !(f.TrustFraction > f.LagFraction):
		return fmt.Errorf("%sfloorTrust.trustFraction (%g) must be above floorTrust.lagFraction (%g)",
			p, f.TrustFraction, f.LagFraction)
	}
	return nil
}

// validate checks each field the entry sets. kvCacheThreshold is the entry's,
// zero when it sets none. idleKvUsage is checked against it only when the
// entry sets idleKvUsage: an entry that leaves it unset may lower
// kvCacheThreshold to or under the default idle figure, as it could before
// the key existed, and ScalingPolicy.IdleKvUsage keeps the idle bound under
// the threshold at run time.
func (l LoadImbalance) validate(kvCacheThreshold float64) error {
	const p = "loadImbalance."
	err := errors.Join(
		fraction(p+"idleKvUsage", l.IdleKvUsage, false),
		durationWithin(p+"idleAfterReady", l.IdleAfterReady, MinImbalanceWindow),
		durationWithin(p+"sustainFor", l.SustainFor, MinImbalanceWindow),
		durationWithin(p+"clearAfter", l.ClearAfter, PolicyDuration(time.Second)),
	)
	if err != nil {
		return err
	}
	if l.IdleKvUsage == 0 {
		return nil
	}
	if kvCacheThreshold == 0 {
		kvCacheThreshold = DefaultKvCacheThreshold
	}
	if l.IdleKvUsage >= kvCacheThreshold {
		return fmt.Errorf("%sidleKvUsage (%g) must be below kvCacheThreshold (%g): a replica cannot be idle and full at once",
			p, l.IdleKvUsage, kvCacheThreshold)
	}
	return nil
}

// IdleKvUsage is the KV utilization below which a Ready replica with nothing
// waiting reads idle: loadImbalance.idleKvUsage, or its default, and never
// above kvCacheThreshold, so a replica cannot be idle and full at once. An
// entry may lower kvCacheThreshold to or under the default idle figure
// without setting idleKvUsage; the bound then is the threshold. A nil policy
// reads as the defaults.
func (c *ScalingPolicy) IdleKvUsage() float64 {
	if c == nil {
		return DefaultIdleKvUsage
	}
	kv := c.KvCacheThreshold
	if kv == 0 {
		kv = DefaultKvCacheThreshold
	}
	return min(c.LoadImbalance.WithDefaults().IdleKvUsage, kv)
}

// resetNonFinite clears each tuning number that is NaN or infinite, through
// the caller's nonFinite and nonFinitePtr.
func (t *AnalyzerTuning) resetNonFinite(nonFinite func(field string, v *float64),
	nonFinitePtr func(field string, v **float64)) {
	const p = "analyzerTuning."
	nonFinite(p+"itlBand.minObservableK", &t.ITLBand.MinObservableK)
	nonFinite(p+"itlBand.maxObservableK", &t.ITLBand.MaxObservableK)
	nonFinitePtr(p+"itlBand.capMaxPreemptionRate", &t.ITLBand.CapMaxPreemptionRate)
	nonFinite(p+"steadyGate.maxKDrift", &t.SteadyGate.MaxKDrift)
	nonFinite(p+"k2Settle.rateFraction", &t.K2Settle.RateFraction)
	nonFinite(p+"k2Settle.maxRise", &t.K2Settle.MaxRise)
	nonFinite(p+"muChoice.minMeasuredOfDerived", &t.MuChoice.MinMeasuredOfDerived)
	nonFinite(p+"muChoice.maxRatio", &t.MuChoice.MaxRatio)
	nonFinite(p+"muChoice.returnRatio", &t.MuChoice.ReturnRatio)
	nonFinite(p+"admission.maxTokensOverCompletions", &t.Admission.MaxTokensOverCompletions)
	nonFinite(p+"admission.siblingMuTolerance", &t.Admission.SiblingMuTolerance)
	nonFinite(p+"floorTrust.lagFraction", &t.FloorTrust.LagFraction)
	nonFinite(p+"floorTrust.trustFraction", &t.FloorTrust.TrustFraction)
	nonFinite(p+"backlogGrowth.minRequests", &t.BacklogGrowth.MinRequests)
	nonFinite(p+"backlogGrowth.fraction", &t.BacklogGrowth.Fraction)
}

// ITLBand is the KV band the ITL fit reads inside (analyzerTuning.itlBand),
// with the defaults for an edge left unset. An inverted or out-of-range pair
// reads as the defaults, never as a band that admits nothing. A nil policy
// reads as the defaults.
func (c *ScalingPolicy) ITLBand() (lo, hi float64) {
	if c == nil {
		return DefaultITLMinObservableK, DefaultITLMaxObservableK
	}
	b := c.AnalyzerTuning.WithDefaults().ITLBand
	return b.MinObservableK, b.MaxObservableK
}

// Steadiness is g as the ITL tracker takes it (itl.Admission.Steady). The one
// place the two are mapped, so both analyzers that fit ITL lines build the
// same gate from the same keys.
func (g SteadyGateTuning) Steadiness() itl.Steadiness {
	return itl.Steadiness{
		For: g.SteadyFor.Duration(), MaxKDrift: g.MaxKDrift, Carry: g.Carry.Duration(),
		IdleRows: g.IdleRows, MinSettledSamples: g.MinSettledSamples,
	}
}

// ITLSteadiness is the ITL steady-state gate the policy sets
// (analyzerTuning.steadyGate), defaults filled in and a crossed pair
// repaired. A nil policy reads as the defaults.
func (c *ScalingPolicy) ITLSteadiness() itl.Steadiness {
	var t AnalyzerTuning
	if c != nil {
		t = c.AnalyzerTuning
	}
	return t.WithDefaults().SteadyGate.Steadiness()
}

// ITLCapPreemptionRate is analyzerTuning.itlBand.capMaxPreemptionRate, or its
// default when unset or the policy is nil.
func (c *ScalingPolicy) ITLCapPreemptionRate() float64 {
	if c == nil {
		return DefaultITLCapMaxPreemptionRate
	}
	r := c.AnalyzerTuning.ITLBand.CapMaxPreemptionRate
	if r == nil || !(*r >= 0) || isNonFinite(*r) {
		return DefaultITLCapMaxPreemptionRate
	}
	return *r
}
