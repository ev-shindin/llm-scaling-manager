package saturation

import (
	"testing"
)

// What the property cases cover, learner by learner (the inventory of
// docs/proposals/analyzer-learner.md, section 1), and what they do not.
// TestPropertyCoverage holds the cases to it both ways: a covered entry
// needs a case, and a case needs a covered entry.

type coverage struct {
	learner string
	// classes is the classes with a case. Empty for a learner with none.
	classes []propClass
	// gap says what is not covered, and why.
	gap string
}

var propCoverage = []coverage{
	{learner: "itl", classes: []propClass{classTransient, classFirstSample, classStale, classMissing, classHeterogeneity, classKnownTruth, classEdges},
		gap: "row 1 and row 13 (the SLO analyzer's line is the same itl.Tracker, so these cases cover its learner, not the SLO analyzer's admission wiring). Not covered: redistribution, N-dependence, scale, liveness, monotonicity; the sequence-cap admission, maxKDrift, carry, the 20/40 rows and 30 min window as edges"},
	{learner: "k2", classes: []propClass{classTransient, classFirstSample, classStale, classMissing, classRedistribution, classKnownTruth, classScale, classLiveness, classEdges},
		gap: "not covered: N-dependence, monotonicity; scale over GPUs per replica and output length; liveness for 8-GPU DP ranks; edges at the queue threshold, rateFraction, maxRise, the window of 10, the 24 h episode gap and 7 d eviction"},
	{learner: "demand", classes: []propClass{classRedistribution},
		gap: "the per-pod KV peak against instant KV only"},
	{learner: "mu", classes: []propClass{classTransient, classFirstSample, classMissing, classHeterogeneity, classNDependence, classKnownTruth, classLiveness, classEdges},
		gap: "not covered: stale value (freshFor / lowHoldFor), redistribution, scale, monotonicity; edges at minMeasuredOfDerived, the 2 min confirm, siblingMuTolerance and the full-cache line; mu under flow control"},
	{learner: "prefill-bound", classes: []propClass{classHeterogeneity, classNDependence},
		gap: "driven through its write path (recordSaturatedThroughput), not a P/D Analyze with EnablePrefillThroughputBound"},
	{learner: "floor", classes: []propClass{classKnownTruth, classMonotonicity, classEdges},
		gap: "not covered: the trust blend between lagFraction and trustFraction as an edge"},
	{learner: "idle", classes: []propClass{classEdges},
		gap: "the 0.30 line only; idleAfterReady is read by the balance tracker, not a learner here"},
	{learner: "start-time", classes: []propClass{classFirstSample, classStale, classEdges},
		gap: "not covered: the 3-confirmation count and the 0-3600 s range as edges; restart (seen pods are not persisted)"},
	{learner: "throughput-itl", gap: "row 16 (throughput/analyzer.go resolveITLModel) lives in another package and has none of #273's fixes; its cases come with PR 4b"},
	{learner: "shared-line", gap: "rows 2 and 4 (the shared and borrowed ITL line, borrowed k2): no case; reuse is off by default"},
	{learner: "shape-hold", gap: "row 8 (fleet shape and its hold): no case"},
	{learner: "decode-memory", gap: "row 9 (decode-saturation memory): no case"},
	{learner: "share", gap: "rows 14 and 15 (the share ledger): out of scope for PR 0"},
}

// timeBaseExempt is each class with no time-base case, and why.
var timeBaseExempt = map[propClass]string{
	classFirstSample:   "one or two readings: there is no one-minute window to pass them through",
	classMissing:       "a missing row is a cycle-level event; the time base changes no reading in it",
	classHeterogeneity: "the cases fold per-pod rates; the time base is the N-dependence case's",
	classScale:         "the cycle interval is one of the scaled quantities",
	classLiveness:      "not built: liveness on the time base waits for a P/D or flow-control recording",
	classMonotonicity:  "not built: the floor trajectory runs on direct inputs only",
}

func TestPropertyCoverage(t *testing.T) {
	type key struct {
		learner string
		class   propClass
	}
	declared := map[key]bool{}
	learners := map[string]bool{}
	for _, c := range propCoverage {
		if learners[c.learner] {
			t.Errorf("coverage lists %s twice", c.learner)
		}
		learners[c.learner] = true
		if c.gap == "" {
			t.Errorf("coverage for %s states no gap: say what is not covered, or \"none\"", c.learner)
		}
		for _, cl := range c.classes {
			declared[key{c.learner, cl}] = true
		}
	}
	have := map[key]bool{}
	timeBase := map[propClass]bool{}
	nonDefault := false
	for _, c := range propertyCases() {
		k := key{c.learner, c.class}
		have[k] = true
		if !declared[k] {
			t.Errorf("case %s: coverage does not list class %s for %s", c.id(), c.class, c.learner)
		}
		if c.family == familyTimeBase {
			timeBase[c.class] = true
		}
		if c.tuning != tunedDefault {
			nonDefault = true
		}
	}
	for k := range declared {
		if !have[k] {
			t.Errorf("coverage lists class %s for %s, and no case runs it", k.class, k.learner)
		}
	}
	for _, cl := range allPropClasses {
		why, exempt := timeBaseExempt[cl]
		switch {
		case timeBase[cl] && exempt:
			t.Errorf("class %s has a time-base case and an exemption (%s): drop the exemption", cl, why)
		case !timeBase[cl] && !exempt:
			t.Errorf("class %s has no time-base case and no exemption", cl)
		}
	}
	if !nonDefault {
		t.Error("no property case runs a non-default analyzerTuning")
	}
}
