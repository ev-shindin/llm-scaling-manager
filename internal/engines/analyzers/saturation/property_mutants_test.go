package saturation

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// The 2.7 accounting and its mutants. Every finding 2.7 lists is exactly one
// of: caught (a mutant puts the defect back and its class fails), or still
// escaping (with the reason).
//
// A mutant puts its defect back in one of five ways, named by its level:
//   - policy: a policy key set where the guard (the fix) stops working, on
//     the production learner;
//   - rows: the production learner handed rows the way the defect read them;
//   - stub: the production learner with one fix stubbed through a seam the
//     analyzer already has for tests (its readySince func, or a replay
//     switch turned off: SetSwitchesForReplay's fields);
//   - toy-learner: a small learner with the pre-fix estimator, run through the
//     same check. It proves the check is sensitive to the defect, not that
//     the production learner would be caught;
//   - toy-output: the production learner's output, edited the way the defect
//     edited it. The weakest: it proves only that the check reads that output.

// findingStatus is where a 2.7 finding stands in 0b.
type findingStatus string

const (
	statusCaught findingStatus = "caught"
	// statusCaughtToy is a row whose every mutant is a toy (toy-learner or
	// toy-output): the check is sensitive to the defect, and no mutant shows
	// the production learner would be caught.
	statusCaughtToy findingStatus = "caught (toy)"
	statusEscaping  findingStatus = "still escaping"
)

// finding is one row of 2.7, with its status and, unless caught, why.
type finding struct {
	row    string
	status findingStatus
	why    string
}

// findings is 2.7's table, row by row; a row listing several findings is
// split where they stand differently.
var findings = []finding{
	{row: "GPS ratio first raw", status: statusCaught},
	{row: "pinned B from unsteady readings", status: statusCaught},
	{row: "k2 at 8 % behind a step", status: statusCaught},
	{row: "Full fit never runs at N >= 2, B stays 6 ms (#273 A3)", status: statusCaught},
	{row: "k == 0 unsettles (#273 A4)", status: statusEscaping, why: "a k == 0 row restarting a settle is a settle-tracker defect; 2.3 puts its negative control in PR 1, the first PR with a tracker that reads the presence bits"},
	{row: "router queue without engine metrics", status: statusEscaping, why: "missing-duplicated/k2/unknown-gauges passes on today's code, and no row edit reproduces the pre-fix reading of a router queue as the engine's"},
	{row: "Instant k against a rate[1m] ITL (#273 A5)", status: statusCaught},
	{row: "Sum of per-pod one-minute peaks", status: statusCaught},
	{row: "Settle refuses one cycle of a fill (#282 F1)", status: statusCaught},
	{row: "k2 key flips on the output-length boundary", status: statusCaught},
	{row: "mu key flips on the output-length boundary", status: statusCaught},
	{row: "measured/derived flips near 2x", status: statusCaught},
	{row: "the 0.30 idle line", status: statusCaught},
	{row: "backlogGrew", status: statusCaught},
	{row: "Headroom counted twice (+17.6 %)", status: statusCaughtToy},
	{row: "idle room counted twice", status: statusCaughtToy},
	{row: "the rules together always pick the higher mu", status: statusCaughtToy},
	{row: "KV and throughput units mixed", status: statusCaught},
	{row: "Max across replicas (+13.6 % at 16)", status: statusCaughtToy},
	{row: "Prefill double scrape", status: statusCaught},
	{row: "Demand falls when lambda lags a step", status: statusEscaping, why: "the monotonicity trajectory passes on today's code and no pre-fix floor is reproduced"},
	{row: "local queue priced as drainable", status: statusCaughtToy},
	{row: "Weighted median flapping between modes", status: statusCaughtToy},
	{row: "IQM breaks at 25 % contamination", status: statusCaughtToy},
	{row: "A genuinely slow replica trimmed away (#273 r3, #282 r3)", status: statusCaughtToy},
	{row: "No floor under flow control (#144)", status: statusEscaping, why: "liveness/k2/flow-control-on fails on today's code (a ledger row, #394); no case drives mu or the floor under flow control"},
	{row: "prefill never learns (#144)", status: statusCaught},
	{row: "Imbalance audit (load moved between replicas)", status: statusCaught},
	{row: "Settle holes: restart", status: statusCaught},
	{row: "Settle holes: fall, rate escape, idle return, lend", status: statusEscaping, why: "only the restart and joiner events have a case; the others need the settle tracker (PR 1)"},
	{row: "Restart fix stubbed", status: statusCaught},
	{row: "k2 adopts its first reading (#392)", status: statusCaught},
	{row: "'For good' self-sealing low mu", status: statusEscaping, why: "the closed-loop probe lands in PR 3"},
}

// Mutant levels.
const (
	levelPolicy     = "policy"
	levelRows       = "rows"
	levelStub       = "stub"
	levelToyLearner = "toy-learner"
	levelToyOutput  = "toy-output"
)

type propMutant struct {
	// row is the 2.7 finding the mutant puts back.
	row string
	// caseID is the property case it runs (class/learner/name).
	caseID string
	level  string
	how    string
	check  func(seed uint64) error
}

func tuningWith(edit func(*config.AnalyzerTuning)) config.AnalyzerTuning {
	var t config.AnalyzerTuning
	edit(&t)
	return t
}

// settleOff turns k2's settle tests off: every reading passes the rate,
// residency and rise checks (the joined test still applies).
func settleOff(p *config.ScalingPolicy) {
	p.AnalyzerTuning.K2Settle = config.K2SettleTuning{RateFraction: 1e-9, MaxRise: 1e9}
}

// orderEdit is a toy-output mutant of the floor: its order, edited.
func orderEdit(edit func(n, mu float64) float64) floorOrder {
	return func(rows []domain.ReplicaMetrics, lambda float64) (float64, float64, error) {
		n, mu, err := floorImplied(rows, lambda)
		return edit(n, mu), mu, err
	}
}

func propertyMutants() []propMutant {
	steadyOff := steadinessOf(tuningWith(func(t *config.AnalyzerTuning) {
		t.SteadyGate.SteadyFor = config.PolicyDuration(time.Nanosecond)
	}))
	neverSteady := steadinessOf(tuningWith(func(t *config.AnalyzerTuning) { t.SteadyGate.MaxKDrift = 1e-12 }))
	return []propMutant{
		{row: "GPS ratio first raw", caseID: "first-sample/mu/low-first-reading", level: levelPolicy,
			how:   "muChoice.minMeasuredOfDerived 1e-9: the low-first-reading guard off",
			check: checkMuFirstSample(tuningWith(func(t *config.AnalyzerTuning) { t.MuChoice.MinMeasuredOfDerived = 1e-9 }))},
		{row: "pinned B from unsteady readings", caseID: "transient/itl/joiner-warmup", level: levelPolicy,
			how:   "steadyGate.steadyFor 1ns: no reading is stamped settling",
			check: checkITLJoiner(func() itlLearner { return newTrackerITL(steadyOff) }, 1, 20*time.Minute)},
		{row: "k2 at 8 % behind a step", caseID: "transient/k2/load-step", level: levelPolicy,
			how:   "k2Settle.rateFraction 1e-9, maxRise 1e9: the settle tests off",
			check: checkK2LoadStep(settleOff)},
		{row: "Full fit never runs at N >= 2, B stays 6 ms (#273 A3)", caseID: "stale-value/itl/b-converges-n2", level: levelPolicy,
			how:   "steadyGate.maxKDrift 1e-12: no reading is ever steady, so the full fit never runs",
			check: checkITLStaleB(func() itlLearner { return newTrackerITL(neverSteady) }, 2, 10*time.Minute)},
		{row: "Instant k against a rate[1m] ITL (#273 A5)", caseID: "known-truth/itl/line-at-kprice-timebase", level: levelRows,
			how: "rows with KvUsageMeanKnown cleared: the tracker pairs the instant k with the rate[1m] ITL, as before #393",
			check: checkITLKnownTruthTimeBase(func() itlLearner { return newTrackerITL(steadinessOf(tuningDefault)) },
				func(rm *domain.ReplicaMetrics) { rm.KvUsageMeanKnown = false })},
		{row: "Sum of per-pod one-minute peaks", caseID: "redistribution/demand/alternating-pods", level: levelRows,
			how:   "rows with KvUsageInstantKnown cleared: demand sums each pod's one-minute peak",
			check: checkDemandAlternating(func(rm *domain.ReplicaMetrics) { rm.KvUsageInstantKnown = false })},
		{row: "Settle refuses one cycle of a fill (#282 F1)", caseID: "transient/k2/slow-fill", level: levelPolicy,
			how:   "k2Settle.rateFraction 1e-9, maxRise 1e9: the rise is not compared a window back (broader than F1)",
			check: checkK2Fill(settleOff)},
		{row: "Settle refuses one cycle of a fill (#282 F1)", caseID: "transient/k2/slow-fill", level: levelToyLearner,
			how:   "a settle that refuses exactly one cycle of a fill suspect (queue deeper than its batch) and admits the next",
			check: checkK2FillOn(func() k2FillLearner { return &toyF1Settle{threshold: 5} })},
		{row: "measured/derived flips near 2x", caseID: "edges/mu/latch-at-2x", level: levelPolicy,
			how:   "muChoice.returnRatio = maxRatio = 2: the latch's hysteresis band removed",
			check: checkMuLatch(tuningWith(func(t *config.AnalyzerTuning) { t.MuChoice.MaxRatio, t.MuChoice.ReturnRatio = 2, 2 }))},
		{row: "backlogGrew", caseID: "edges/floor/backlog-grew", level: levelPolicy,
			how: "backlogGrowth.minRequests and fraction 1e-9: grown on any rise, as before e7d9f0ff1",
			check: checkBacklogEdge(tuningWith(func(t *config.AnalyzerTuning) {
				t.BacklogGrowth.MinRequests, t.BacklogGrowth.Fraction = 1e-9, 1e-9
			}))},
		{row: "Headroom counted twice (+17.6 %)", caseID: "known-truth/floor/steady-order", level: levelToyOutput,
			how:   "the floor's order divided by the 0.85 target a second time",
			check: checkFloorTruth(orderEdit(func(n, _ float64) float64 { return n / floorTarget }))},
		{row: "idle room counted twice", caseID: "known-truth/floor/steady-order", level: levelToyOutput,
			how:   "the floor's order less the two idle replicas' room a second time (4 of 6 replicas)",
			check: checkFloorTruth(orderEdit(func(n, _ float64) float64 { return n * 4 / 6 }))},
		{row: "the rules together always pick the higher mu", caseID: "known-truth/mu/choice-priced", level: levelToyLearner,
			how:   "a chooser that prices the higher of measured and derived",
			check: checkMuChoiceTruth(func(m, d float64, _, _ time.Time) float64 { return math.Max(m, d) })},
		{row: "KV and throughput units mixed", caseID: "scale/k2/block-size-and-cycle", level: levelRows,
			how:   "rows with TokensInUse in blocks, not tokens",
			check: checkK2Scale(func(rm *domain.ReplicaMetrics) { rm.TokensInUse /= rm.BlockSize })},
		{row: "Max across replicas (+13.6 % at 16)", caseID: "n-dependence/mu/iid-fold", level: levelToyLearner,
			how:   "mu's write path folding pods by the max within a minute",
			check: checkMuNDependence(func() muLearner { return newToyMu(foldMax) }, false)},
		{row: "local queue priced as drainable", caseID: "monotonicity/floor/standing-local-queue", level: levelToyOutput,
			how: "the floor's order plus the pinned queue's part past mu x T, charged as if added replicas drained it",
			check: checkFloorDrainBound(orderEdit(func(n, mu float64) float64 {
				return n + (300-mu*floorDrainT)/floorDrainT/(mu*floorTarget)
			}))},
		{row: "Weighted median flapping between modes", caseID: "edges/itl/bimodal-fit", level: levelToyLearner,
			how:   "the k-weighted median of the readings' slopes as the pinned fit",
			check: checkITLBimodal(fitWeightedMedian)},
		{row: "IQM breaks at 25 % contamination", caseID: "contamination-heterogeneity/itl/flagged-contamination-to-50pct", level: levelToyLearner,
			how:   "every reading, flags ignored, the interquartile mean of the slopes",
			check: checkITLFlagged(func() itlLearner { return &toyITL{fit: fitIQM} })},
		{row: "A genuinely slow replica trimmed away (#273 r3, #282 r3)", caseID: "contamination-heterogeneity/itl/slow-replica-fit", level: levelToyLearner,
			how:   "#273's interquartile mean of the slopes as the pinned fit",
			check: checkITLSlowReplica(fitIQM)},
		{row: "A genuinely slow replica trimmed away (#273 r3, #282 r3)", caseID: "contamination-heterogeneity/mu/one-slow-pod-n4", level: levelToyLearner,
			how:   "#282's lower median across pods, the fold before the mean",
			check: checkMuOneSlowPod(func() muLearner { return newToyMu(foldLowerMedian) }, 4)},
		{row: "Prefill double scrape", caseID: "contamination-heterogeneity/prefill-bound/one-pod-doubled", level: levelRows,
			how:   "prefill rows with no pod name: the bound folds every pod as one and keeps the fastest (#395)",
			check: checkMuDoubled(func() muLearner { return newBoundMu(func(rm *domain.ReplicaMetrics) { rm.PodName = "" }) })},
		{row: "Imbalance audit (load moved between replicas)", caseID: "redistribution/k2/queue-moved", level: levelRows,
			how:   "rows all read queued: k2 learned from replicas the load moved off",
			check: checkK2Redistribution(func(rm *domain.ReplicaMetrics) { rm.QueueLength = max(rm.QueueLength, 10) })},
		{row: "Settle holes: restart", caseID: "transient/k2/restart-same-name", level: levelRows,
			how:   "rows with ReadySince cleared: a restart under the same name is not seen",
			check: checkK2Restart(func(rm *domain.ReplicaMetrics) { rm.ReadySince = time.Time{} })},
		{row: "prefill never learns (#144)", caseID: "liveness/prefill-bound/behind-full-decode", level: levelPolicy,
			how:   "enablePrefillThroughputBound off, its default: behind a full decode computeK2 discards every prefill reading, and prefill learns no mu at all",
			check: checkBoundAnalyzeOn(1, true, &config.ScalingPolicy{})},
		{row: "k2 key flips on the output-length boundary", caseID: "edges/k2/key-near-output-boundary", level: levelStub,
			how:   "SwitchK2KeyBucketHeld off: the key reads the tracked length, with no band or dwell of its own (#396)",
			check: checkK2KeyEdgeOn(nil, func(a *SaturationAnalyzer) { a.k2KeyBucketHeld = false })},
		{row: "mu key flips on the output-length boundary", caseID: "edges/mu/key-near-output-boundary", level: levelStub,
			how:   "SwitchMuKeyBucketHeld off: the throughput key reads the tracked length, not the k2 key's band (#415)",
			check: checkMuKeyEdgeOn(func(a *SaturationAnalyzer) { a.muKeyBucketHeld = false })},
		{row: "the 0.30 idle line", caseID: "edges/idle/idle-line-030", level: levelStub,
			how: "SwitchIdleBand off: a replica in an idle run is held to the line it entered under (#396)",
			check: checkIdleEdge(func(a *SaturationAnalyzer, frac float64, now time.Time) bool {
				a.idleBand = false
				return realIdleVerdict(a, frac, now)
			})},
		{row: "Restart fix stubbed", caseID: "transient/k2/restart-same-name", level: levelStub,
			how:   "the restart fix stubbed: the analyzer's readySince seam reads no Ready time, so the joined test counts from first sight alone",
			check: checkK2RestartOn(nil, stubRestartFix)},
		{row: "k2 adopts its first reading (#392)", caseID: "first-sample/k2/second-reading", level: levelStub,
			how:   "SwitchK2FirstReadingWaits off: a window's first reading is written, and prices, at once",
			check: checkK2FirstSampleOn(func(a *SaturationAnalyzer) { a.k2FirstReadingWaits = false })},
	}
}

// stubRestartFix stubs the restart fix (#288) on a through its readySince
// seam: no Pod Ready time reaches noteReady, so a restart under the same name
// reads as the replica it replaced. Nothing is added to production code.
func stubRestartFix(a *SaturationAnalyzer) {
	a.readySince = func(domain.ReplicaMetrics) time.Time { return time.Time{} }
}

// The restart fix's per-path counter (2.5, "joined refusals with a known
// ReadySince") counts on the restart case, and drops to zero once the fix is
// stubbed.
func TestPropertyRestartStubZeroesTheJoinedCounter(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*SaturationAnalyzer)
		zero bool
	}{{"the fix", nil, false}, {"the fix stubbed", stubRestartFix, true}} {
		var joined int64
		count := func(c map[string]int64) { joined += c[PathJoinedRefusedReadySince] }
		if _, err := runCase(checkK2RestartOn(nil, tc.edit, count), []uint64{1, 2, 3}); err != nil {
			t.Fatal(err)
		}
		if (joined == 0) != tc.zero {
			t.Errorf("%s: %d joined refusals decided by a Ready time; want zero: %v", tc.name, joined, tc.zero)
		}
		t.Logf("%s: %d joined refusals decided by a Ready time", tc.name, joined)
	}
}

// The joined-refusal counter counts a refusal only when the Pod's Ready time
// decided it. A replica first seen this cycle is refused as a joiner whatever
// its Ready time says, and is not counted; one whose Ready time changed under
// its name -- a restart the first sight could not reveal -- is.
func TestJoinedCounterCountsOnlyARefusalTheReadyTimeDecided(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
		ready   func(now time.Time) time.Time
		want    int64
	}{
		{"no Ready time, first seen now", false, func(time.Time) time.Time { return time.Time{} }, 0},
		{"Ready 10 s ago, first seen now", false, func(now time.Time) time.Time { return now.Add(-10 * time.Second) }, 0},
		{"a restart under its name, Ready 10 s ago", true, func(now time.Time) time.Time { return now.Add(-10 * time.Second) }, 1},
	} {
		rig := newPropRig()
		idle := satRow("r1", 100_000, 0, 1)
		idle.ReadySince = propStart.Add(-time.Hour)
		rows := []domain.ReplicaMetrics{satRow("r0", 100_000, 0, 1)}
		if tc.restart {
			rows = append(rows, idle)
		}
		for range 2 {
			if _, err := rig.analyze(rows, decodeStates(2), 6, propCycle); err != nil {
				t.Fatal(err)
			}
		}
		rm := satRow("r1", int64(float64(propKv)*0.9), 10, 6)
		rm.ReadySince = tc.ready(rig.clock)
		if _, err := rig.analyze([]domain.ReplicaMetrics{satRow("r0", 100_000, 0, 1), rm}, decodeStates(2), 6, propCycle); err != nil {
			t.Fatal(err)
		}
		if got := rig.a.PathCounts()[PathJoinedRefusedReadySince]; got != tc.want {
			t.Errorf("%s: %d counted, want %d", tc.name, got, tc.want)
		}
		if k := rig.k2(""); k > 0 {
			t.Errorf("%s: the joiner was not refused (k2 %.0f)", tc.name, k)
		}
	}
}

// citedCase matches a property case id cited in a finding's reason.
var citedCase = regexp.MustCompile(`\b[a-z0-9-]+/[a-z0-9-]+/[a-z0-9-]+\b`)

// checkAccounting holds the mutants to the 2.7 table fs: every row is listed
// once; every mutant's row is a caught finding, every caught finding has a
// mutant, and every other finding says why; no mutant runs a ledgered case (a
// ledgered case fails without the defect, so it would prove nothing); a case a
// reason cites exists, and is a ledger row when the reason says it is one.
func checkAccounting(fs []finding, muts []propMutant, cases map[string]bool, ledger map[string]*ledgerRow) []string {
	var errs []string
	status := map[string]finding{}
	for _, f := range fs {
		if _, dup := status[f.row]; dup {
			errs = append(errs, f.row+": 2.7 row listed twice")
		}
		status[f.row] = f
		if f.status != statusCaught && f.status != statusCaughtToy && f.why == "" {
			errs = append(errs, f.row+": "+string(f.status)+" needs a reason")
		}
		for _, id := range citedCase.FindAllString(f.why, -1) {
			switch {
			case !cases[id]:
				errs = append(errs, f.row+": the reason cites "+id+", which is no property case")
			case strings.Contains(f.why, "ledger row") && ledger[id] == nil:
				errs = append(errs, f.row+": the reason calls "+id+" a ledger row, and the ledger does not list it")
			}
		}
	}
	mutated, production := map[string]bool{}, map[string]bool{}
	for _, m := range muts {
		f, ok := status[m.row]
		switch {
		case !ok:
			errs = append(errs, m.row+": a mutant for a row 2.7 does not list")
		case f.status != statusCaught && f.status != statusCaughtToy:
			errs = append(errs, m.row+" has a mutant and is "+string(f.status))
		}
		if m.level != levelToyLearner && m.level != levelToyOutput {
			production[m.row] = true
		}
		if !cases[m.caseID] {
			errs = append(errs, m.row+": mutant names no property case "+m.caseID)
		}
		if ledger[m.caseID] != nil {
			errs = append(errs, m.row+": mutant runs "+m.caseID+", a ledger row: it fails without the defect")
		}
		mutated[m.row] = true
	}
	for _, f := range fs {
		switch {
		case (f.status == statusCaught || f.status == statusCaughtToy) && !mutated[f.row]:
			errs = append(errs, f.row+" is marked caught and has no mutant")
		case f.status == statusCaught && !production[f.row]:
			errs = append(errs, f.row+" is marked caught and only a toy catches it: mark it caught (toy)")
		case f.status == statusCaughtToy && production[f.row]:
			errs = append(errs, f.row+" is marked caught (toy) and a production-level mutant catches it: mark it caught")
		}
	}
	return errs
}

// TestPropertyMutants runs every mutant and requires its class to fail on it,
// with a violation, not an error running the case.
func TestPropertyMutants(t *testing.T) {
	cases := map[string]bool{}
	for _, c := range propertyCases() {
		cases[c.id()] = true
	}
	muts := propertyMutants()
	for _, e := range checkAccounting(findings, muts, cases, loadLedger(t)) {
		t.Error(e)
	}
	for _, f := range findings {
		if f.status != statusCaught {
			t.Logf("%s: %s: %s", f.status, f.row, f.why)
		}
	}
	seeds := make([]uint64, defaultSeeds)
	for i := range seeds {
		seeds[i] = uint64(i + 1)
	}
	for _, m := range muts {
		t.Run(m.row+"/"+m.caseID, func(t *testing.T) {
			run, err := runCase(m.check, seeds)
			if err != nil {
				t.Fatal(err)
			}
			if len(run.failures) == 0 {
				t.Errorf("%s passes on the mutant (%s: %s). The check no longer detects this defect: if a production change weakened the learner or the case, fix that; do not move the row",
					m.caseID, m.level, m.how)
				return
			}
			t.Logf("caught [%s]: %s fails on the mutant (%s): %s", m.level, m.caseID, m.how, run)
		})
	}
}

// The accounting's negative controls.
func TestPropertyAccountingNegativeControls(t *testing.T) {
	const joiner = "transient/itl/joiner-warmup"
	cases := map[string]bool{joiner: true}
	caught := finding{row: "pinned B from unsteady readings", status: statusCaught}
	escaping := finding{row: "the 0.30 idle line", status: statusEscaping, why: "no case"}
	fs := []finding{caught, escaping}
	good := []propMutant{{row: caught.row, caseID: joiner}}
	if errs := checkAccounting(fs, good, cases, nil); len(errs) != 0 {
		t.Fatalf("a matching table is clean: %v", errs)
	}
	if errs := checkAccounting(fs, nil, cases, nil); !containsPrefix(errs, caught.row+" is marked caught") {
		t.Errorf("a caught finding with no mutant was not reported: %v", errs)
	}
	toy := []propMutant{{row: caught.row, caseID: joiner, level: levelToyLearner}}
	if errs := checkAccounting(fs, toy, cases, nil); !containsPrefix(errs, caught.row+" is marked caught and only a toy") {
		t.Errorf("a caught finding only a toy catches was not reported: %v", errs)
	}
	toyRow := []finding{{row: caught.row, status: statusCaughtToy}, escaping}
	if errs := checkAccounting(toyRow, toy, cases, nil); len(errs) != 0 {
		t.Errorf("a caught (toy) row with a toy mutant is clean: %v", errs)
	}
	if errs := checkAccounting(toyRow, good, cases, nil); !containsPrefix(errs, caught.row+" is marked caught (toy) and a production") {
		t.Errorf("a caught (toy) row with a production mutant was not reported: %v", errs)
	}
	for name, m := range map[string]propMutant{
		"a mutant for an escaping row": {row: escaping.row, caseID: joiner},
		"a mutant for no 2.7 row":      {row: "no such finding", caseID: joiner},
		"a mutant naming no case":      {row: caught.row, caseID: "gone"},
	} {
		if errs := checkAccounting(fs, append([]propMutant{m}, good...), cases, nil); !containsPrefix(errs, m.row) {
			t.Errorf("%s: not reported: %v", name, errs)
		}
	}
	if errs := checkAccounting(fs, good, cases, map[string]*ledgerRow{joiner: {}}); !containsPrefix(errs, caught.row+": mutant runs") {
		t.Errorf("a mutant on a ledgered case was not reported: %v", errs)
	}
	for name, tc := range map[string]struct {
		f      finding
		prefix string
	}{
		"a row listed twice":                {escaping, escaping.row + ": 2.7 row listed twice"},
		"an escaping row with no reason":    {finding{row: "x", status: statusEscaping}, "x: still escaping needs a reason"},
		"a reason citing no case":           {finding{row: "y", status: statusEscaping, why: "edges/k2/gone fails"}, "y: the reason cites"},
		"a reason citing an unledgered row": {finding{row: "z", status: statusEscaping, why: joiner + " fails on today's code (a ledger row, open)"}, "z: the reason calls"},
	} {
		if errs := checkAccounting(append(append([]finding{}, fs...), tc.f), good, cases, nil); !containsPrefix(errs, tc.prefix) {
			t.Errorf("%s: not reported: %v", name, errs)
		}
	}
}

func containsPrefix(errs []string, prefix string) bool {
	for _, e := range errs {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// ----- toy learners with the pre-fix estimators -----

// toyMu is mu's write path with another fold: samples a minute apart, pods
// folded within the minute by fold; the figure is the samples' median, as
// the window's.
type toyMu struct {
	fold    func([]podRate) float64
	reads   []podRate
	samples []float64
}

type podRate struct {
	pod  string
	rate float64
}

func newToyMu(fold func([]podRate) float64) *toyMu { return &toyMu{fold: fold} }

func (m *toyMu) read(pod string, rate float64) { m.reads = append(m.reads, podRate{pod, rate}) }
func (m *toyMu) next() {
	if len(m.reads) > 0 {
		m.samples = append(m.samples, m.fold(m.reads))
	}
	m.reads = nil
}
func (m *toyMu) figure() float64 {
	m.next()
	s := append([]float64(nil), m.samples...)
	sort.Float64s(s)
	if len(s) == 0 {
		return 0
	}
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func foldMax(r []podRate) float64 {
	m := 0.0
	for _, p := range r {
		m = math.Max(m, p.rate)
	}
	return m
}

func foldLowerMedian(r []podRate) float64 {
	v := make([]float64, len(r))
	for i, p := range r {
		v[i] = p.rate
	}
	sort.Float64s(v)
	return v[(len(v)-1)/2]
}

// slopes are the readings' own slopes (ITL - b) / k, sorted, with k weights.
func slopes(obs []itl.Observation, b float64) (s, w []float64) {
	idx := make([]int, len(obs))
	for i := range idx {
		idx[i] = i
	}
	sl := func(i int) float64 { return (obs[i].ITLSec - b) / obs[i].K }
	sort.Slice(idx, func(x, y int) bool { return sl(idx[x]) < sl(idx[y]) })
	for _, i := range idx {
		s, w = append(s, sl(i)), append(w, obs[i].K)
	}
	return s, w
}

func fitWeightedMedian(obs []itl.Observation, b float64) (itl.Model, bool) {
	s, w := slopes(obs, b)
	if len(s) == 0 {
		return itl.Model{}, false
	}
	total := 0.0
	for _, x := range w {
		total += x
	}
	acc := 0.0
	for i := range s {
		acc += w[i]
		if acc >= total/2 {
			return itl.Model{A: s[i], B: b}, true
		}
	}
	return itl.Model{A: s[len(s)-1], B: b}, true
}

func fitIQM(obs []itl.Observation, b float64) (itl.Model, bool) {
	s, w := slopes(obs, b)
	if len(s) < 4 {
		return itl.Model{}, false
	}
	lo, hi := len(s)/4, len(s)-len(s)/4
	var num, den float64
	for i := lo; i < hi; i++ {
		num += s[i] * w[i]
		den += w[i]
	}
	return itl.Model{A: num / den, B: b}, true
}

// toyITL keeps the last 20 readings of every Ready row, flags ignored, and fits
// them with fit at the default intercept.
type toyITL struct {
	fit fitLearner
	obs []itl.Observation
}

func (l *toyITL) observe(rows []domain.ReplicaMetrics, now time.Time) itl.Reading {
	for _, rm := range rows {
		if rm.Ready && rm.KvUsageInstant > 0 && rm.AvgITL > 0 {
			l.obs = append(l.obs, itl.Observation{K: rm.KvUsageInstant, ITLSec: rm.AvgITL, Timestamp: now})
		}
	}
	if len(l.obs) > itl.DefaultWindowMaxSize {
		l.obs = l.obs[len(l.obs)-itl.DefaultWindowMaxSize:]
	}
	if len(l.obs) < itl.DefaultMinSamples {
		return itl.Reading{}
	}
	m, ok := l.fit(l.obs, itl.DefaultBaselineSec)
	if !ok {
		return itl.Reading{}
	}
	return itl.Reading{Model: m, Tier: itl.TierPinnedB}
}

func (l *toyITL) baseline() (float64, bool) { return 0, false }
