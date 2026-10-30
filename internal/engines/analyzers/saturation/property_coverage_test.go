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
	{learner: "itl", classes: []propClass{classTransient, classFirstSample, classStale, classMissing, classHeterogeneity, classKnownTruth, classLiveness, classEdges},
		gap: "row 1. Not covered: redistribution, N-dependence, scale, monotonicity (the line is a per-key fit, and the cases move no load between keys); the carry (steadyGate.carry) as an edge of its own: it is run at a non-default value through the joiner and B cases, not held at the boundary"},
	{learner: "slo-itl", classes: []propClass{classTransient, classFirstSample, classStale, classMissing, classKnownTruth},
		gap: "row 13, through slo.Analyzer: its admission wiring (the policy's band and steady gate) on the shared tracker. The tracker's own edges are the itl learner's"},
	{learner: "k2", classes: []propClass{classTransient, classFirstSample, classStale, classMissing, classRedistribution, classKnownTruth, classScale, classLiveness, classEdges},
		gap: "not covered: N-dependence, monotonicity; the 7 d eviction (EvictStaleHistory, a sweep, not the learner's admission)"},
	{learner: "demand", classes: []propClass{classRedistribution},
		gap: "the per-pod KV peak against instant KV only"},
	{learner: "mu", classes: []propClass{classTransient, classFirstSample, classMissing, classHeterogeneity, classNDependence, classKnownTruth, classScale, classLiveness, classEdges},
		gap: "not covered: stale value (freshFor and lowHoldFor are held as edges), redistribution, monotonicity; the sibling yield's full-cache line and the lower median's mass point as edges; mu under flow control"},
	{learner: "prefill-bound", classes: []propClass{classHeterogeneity, classNDependence, classLiveness},
		gap: "the N-dependence case drives the write path; heterogeneity and liveness also run through a P/D Analyze with enablePrefillThroughputBound"},
	{learner: "floor", classes: []propClass{classKnownTruth, classMonotonicity, classEdges},
		gap: "the standing-queue cap is the monotonicity case's bound, not an edge of its own"},
	{learner: "idle", classes: []propClass{classEdges},
		gap: "edges only; idleAfterReady is read by the balance tracker, not a learner here"},
	{learner: "start-time", classes: []propClass{classFirstSample, classStale, classEdges},
		gap: "not covered: restart (seen pods are not persisted); the 0-3600 s range is the collector's (maxCredibleStartSeconds), and the learner's own bound, a positive sample, is the zero-sample edge"},
	{learner: "shared-line", classes: []propClass{classStale, classKnownTruth, classLiveness, classEdges},
		gap: "row 2. A line ages on its last write (the sweep), not covered"},
	{learner: "borrowed-k2", classes: []propClass{classMissing, classRedistribution, classKnownTruth, classLiveness, classMonotonicity, classEdges},
		gap: "row 4. Not covered: the bound across a controller restart (the snapshot's tests hold it)"},
	{learner: "shape-hold", classes: []propClass{classTransient, classFirstSample, classStale, classMissing, classEdges},
		gap: "row 8. The arriving-prompt axis (the scheduler queue) has no case; the backstop's cooldown is not an edge of its own"},
	{learner: "decode-memory", classes: []propClass{classTransient, classMissing, classRedistribution, classEdges},
		gap: "row 9"},
	{learner: "throughput-itl", gap: "row 16 (throughput/analyzer.go resolveITLModel) lives in another package and has none of #273's fixes; its cases come with PR 4b"},
	{learner: "share", gap: "rows 14 and 15 (the share ledger): out of scope for PR 0"},
}

// timeBaseExempt is each class with no time-base case, and why. Every class
// has one now.
var timeBaseExempt = map[propClass]string{}

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
