package learnreplay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
)

// The switch runs of the golden rule (analyzer-learner.md 2.5), over every
// committed golden, for this branch's switches: all off, each alone, all on.
//
//   - All on is the committed goldens (the golden check holds that too).
//   - All off is the base commit's goldens: an output no switch names is the
//     committed file, and an output a switch names is the base's, by digest
//     (switchBase).
//   - Each switch alone moves, from all off, only the outputs it names.
//
// A PR that adds a switch adds it to replaySwitches, and the base digests of
// the outputs it names. An earlier PR's switch whose seam is still in the
// code moves to earlierSeams, and is replayed off alone from all on; the PR
// that deletes a seam deletes its row. Every name SetSwitchesForReplay
// accepts has a row in one of the two.

// replaySwitches is each replay switch of this PR, saturation's or slo's
// (switchPackage), and the golden outputs (fixture/file) it is named to move.
//
// #275 prices an aggregated variant's ITL line by request shape, in two
// switches. Its base is main after #432 (67790e633): #432's switch is an earlier
// seam here (earlierSeams), on in every run below. Measured, each alone
// against all off:
//
//   - SwitchITLAggregatedShapeTerm moves acceptance/learned-state.csv and
//     engine/learned-state.csv, one row each export (two per file): model-01's
//     |both line now carries the shape it was fitted at (ITLLineShapes, g
//     257.14 at the fixture's shape), with no prefill reading (sigmaOK false:
//     the recordings carry no running-request count or completion rate on
//     the aggregated rows) and no point (a point needs one). Nothing else
//     moves: with no prefill reading the reported figure is #432's, on the
//     same case (the shape never moves past the tolerance in either fixture).
//   - SwitchITLAggregatedSizes moves nothing: it lifts only a figure with a
//     prefill reading and a GPS check that agreed, and no fixture has
//     either for an aggregated variant. Its unit tests hold it (heldBy), as
//     the shape term's cases, c, the prefill correction and the cap at a
//     measured rate are held by theirs (itl_aggregated_shape_test.go).
//
// Both on moves nothing past the shape term's two files: no interaction.
//
// #438 prices a vLLM data-parallel decode or aggregated variant at zero
// replicas at every engine rank its leader pod runs, not one: the derived
// figure, and the stored budget it falls back to. It moves no golden: no
// recorded scale target states a data-parallel size above 1, so every
// zero-replica estimate the fixtures price is one rank's either way. Its unit
// tests hold it (heldBy).
//
// Earlier PRs' switches (learner PRs 2, 3 and 4a, #396's bands, #399's
// batch cap, #415's mu key, #392's first k2 reading, #420's batch-cap
// readers, #274's ITL per accepted token, #276's output spread, #427's
// zero-replica batch cap, #431's forget of the slo line, #277's resident
// prefix-cache hits, #432's aggregated line) are not here: "all off" is this
// PR's switches, and an earlier PR's stay at their constructor default, on,
// as the base goldens were written (analyzer-learner.md, 2.5).
//
// Since learner PR 4b-1 the rows above (#275's two switches, #438's) are
// earlier seams (earlierSeams); 4b-1's own two are learner4b1Seams.
//
// Since learner PR 4b-2 this PR's are the throughput analyzer's ITL line on
// the engine's settle tracker, six switches, as learner PR 4a split the
// saturation and slo lines' (throughput/settle.go). Its base is main after
// 4b-1 merged (1c9a34fe8, #456), whose goldens are 4b-1's all-on run. The
// fixtures that run the throughput analyzer are the in-process three
// (acceptance, engine, mu-paths), and none was recorded with the analyzer
// given the engine's view (start.ThroughputConfig is absent): the replay
// builds it with none, and its line reads the tracker the analyzer observes
// itself. Measured, each alone against all off:
//
//   - SwitchITLReadsSettle moves nothing. The PR first measured it moving
//     acceptance/variant-capacities.csv and engine/variant-capacities.csv
//     (3 and 2 rows changed bytes):
//     after a clock jump past an hour, the analyzer's own tracker read the
//     gap as an absence (settle.Rejoined) for model-01, the first model
//     analyzed, and its line settled, while every later model, evicted
//     meanwhile, was first-seen and did not. Its review made a model the
//     own tracker has not seen for ownSettleTimeout first-seen whatever the
//     order (throughput/settle.go, observeOwnSettle), and the goldens came
//     back byte for byte to main's. Its unit tests hold it.
//   - SwitchITLWaitsAfterRestart moves nothing: an analyzer with no engine
//     view never waits. On the throughput-sweep recordings, which have the
//     view, it moves no output file either, but every settling reading they
//     hold rests on it: off, throughput-itl-settling falls 5 -> 0 and 8 -> 0,
//     and TestGoldens_ThroughputSweepFloors replays that. Its unit tests hold
//     it.
//   - The other four move nothing, as their 4a twins moved nothing; their
//     unit tests hold them.
//
// Since learner PR 4b-3 those six are earlier seams (learner4b2Seams), and
// this PR's one switch is the throughput line's cap admission
// (throughput.SwitchITLAdmitsAtSequenceCap): a reading above the band from a
// replica at the sequence cap its engine states, with requests waiting and
// preemption at most itlBand.capMaxPreemptionRate, is the band's. Its base
// is main after 4b-2 merged (891c071c4, #458), whose goldens are 4b-1's
// all-on run byte for byte. On the three in-process fixtures that run the
// throughput analyzer it moves nothing, by construction: they predate
// start.ThroughputConfig, so the replay builds it with no capacity store,
// and with no store it knows no cap. Its unit tests hold it there (heldBy).
// The throughput-sweep recordings state the store, and the restart
// recording's load holds the decode pod at its cap of 64 for 20 admitted
// readings, so it moves that fixture's outputs, its row below.
//
// The what-if PR P1 (docs/proposals/slo-capacity-planner.md, the PR plan)
// adds one: the saturation analyzer's prefill observation
// (saturation.SwitchPrefillCapacityObserve), on 4b-1's base. It moves no
// golden in S1a: it only logs (prefill-capacity-observed), and writes no
// state an output reads there; S1b's TTFT pricing reads the estimate it
// stashes. Its unit tests hold it (heldBy).
var replaySwitches = withMoves(staircaseSwitchMoves, map[string][]string{
	saturation.SwitchPrefillCapacityObserve: nil,
	throughput.SwitchITLAdmitsAtSequenceCap: {
		"throughput-sweep-restart/composite-holds.csv",
		"throughput-sweep-restart/composite-release.csv",
		"throughput-sweep-restart/composite-results.csv",
		"throughput-sweep-restart/composite-roles.csv",
		"throughput-sweep-restart/composite-variants.csv",
		"throughput-sweep-restart/results.csv",
		"throughput-sweep-restart/variant-capacities.csv",
	},

	// The forget-learned-state annotation reaches the throughput analyzer's
	// line, one switch, recorded as an event of its own
	// (throughput.ForgetVariant). It moves no golden, measured: no committed
	// recording carries the event, and the acceptance and engine runs, taken
	// again, carry one each (Dropped 1: a window, no intercept) and give all
	// 19 outputs byte for byte with the event and without it. No cluster
	// recording forgets. Its unit tests hold it (heldBy). The steadystate
	// package's engine-run-event-counts.txt gains the event: that file is a
	// pin on the events the engine run records, not a replay output.
	throughput.SwitchITLForgetReachesTheLine: nil,

	// Learner PR 5b, on main after 5a (0fec87b18): the start guard opens on
	// distinct Pods. Measured alone against all off, it moves nothing: it
	// changes only when a start over three times the estimate is admitted,
	// and no output any recording replays differs on it. Its unit tests hold
	// it (heldBy).
	saturation.SwitchStartConfirmsByDistinctPod: nil,

	// P1's busy minutes (saturation.SwitchPrefillBusyMinutes), on P1's
	// observation: each prefill row's queue minimum goes to it, and a minute
	// whose queue never emptied is a busy minute, V_P's first source. It
	// moves no golden: no committed recording carries a queue minimum, and
	// the observation only logs. Its unit tests hold it (heldBy).
	saturation.SwitchPrefillBusyMinutes: nil,

	// P1's curved fit of a (saturation.SwitchPrefillAQuadratic), on main
	// after P1's busy minutes (c192d49a7): a is the intercept of the convex
	// curve through the low-load minutes' prompt clusters where four of
	// them span 3x, and of the line otherwise; an intercept extrapolated
	// from prompts all over 2500 tokens, or whose standard error is over
	// half of it, is refused. It moves no golden: the observation only
	// logs, and no output reads a (P3's adapter reads it, and no golden
	// runs P3). Its unit tests hold it (heldBy).
	saturation.SwitchPrefillAQuadratic: nil,
	// Issue #467, on main after #471 (602054e98): the saturation line priced
	// one-sided at the sequence cap while B is unlearned
	// (itl.Admission.OneSidedAboveKSat, at the line's pricing k), against the
	// line through the mean of the at-cap readings at or past that k with
	// intercept the prior on B (itl.Admission.BPrior: 30 ms, raised by B
	// learned on the physics, capped at the window's least ITL; neither
	// recording learns one). Measured alone against all off, it moves e1 and
	// e1-tuned, the two cluster recordings whose decode replica sits at its
	// stated cap above the band with no B learned: model-02's variant-01
	// line through the default 6 ms is raised where the prior's line is
	// higher at k 0.68 (first moved row: A 0.0856 -> 0.0874 on e1, 0.0889 ->
	// 0.1012 on e1-tuned; ITL at the pricing k 0.0642 -> 0.0654 s and 0.0665
	// -> 0.0748 s), so the derived mu falls (6.38 -> 6.26 and 6.17 -> 5.48
	// req/s). variant-capacities.csv carries the line and the rate (92 and
	// 60 rows) and results.csv the demand the floor prices from mu (70 and
	// 32 rows). In lines.csv the same 92 and 60 publishes are gone: a raised
	// line is not published for siblings to borrow. Nothing else moves.
	saturation.SwitchITLOneSidedAtCap: {
		"e1-tuned/lines.csv", "e1-tuned/results.csv", "e1-tuned/variant-capacities.csv",
		"e1/lines.csv", "e1/results.csv", "e1/variant-capacities.csv",
	},

	// Issue #467: the slo line priced one-sided at the sequence cap while B
	// is unlearned, at the fill where it meets the TPOT target, against the
	// line at the prior on B. It moves no
	// golden: no committed recording runs the slo analyzer on a replica at
	// a stated cap. Its unit tests hold it (heldBy).
	slo.SwitchITLOneSidedAtCap: nil,

	// S1 of the TTFT track (docs/proposals/slo-tiers.md, decision 4), on main
	// after #482 (59772fce2): each prefill variant of a model with a TTFT
	// target is priced against it, in shadow. It moves slo-published.csv in
	// the two goldens with a TTFT target and a prefill variant, and nothing
	// else: no result moves, so no replica count can. Alone from all off,
	// P1's observation is off too, so the reason reads no-estimate; with it
	// on, as committed, no-prefill-capacity
	// (TestGoldens_TTFTPriceReadsTheObservation holds the pair). The
	// engine golden's P/D model is TPOT-only, and the ttft-* recordings have
	// no slo.Analyze to replay.
	slo.SwitchTTFTPrice: {
		"ols-sweep-slo/slo-published.csv", "ols-sweep-slo-restart/slo-published.csv",
	},

	// Learner PR 5c, on main after 5b (602054e98): the replicaStartSeconds
	// seed is the prior (SwitchStartReadsSeed), and the first starts blend
	// with it (SwitchStartBlendsFirstSamples). Measured, each alone against
	// all off:
	//
	//   - SwitchStartReadsSeed moved nothing when 5c merged: no recording
	//     seeded a variant. The start-seed goldens do, so it now moves
	//     RoleStartSeconds on start-seed's first twelve cycles: 21 -> 120
	//     while the seeded variant has no measured start (share pacing reads
	//     the slowest variant, seeds included). Its unit tests hold what no
	//     golden reaches (heldBy).
	//   - SwitchStartBlendsFirstSamples moves the twelve outputs below, every
	//     figure up, none down. RoleStartSeconds (share pacing, the slowest
	//     variant as sized) rises where a key holds fewer than three Pods:
	//     sized at max(blend, mean, estimate), so 63 -> 67.67 and 62 ->
	//     67.33 (ols-sweep), 60 -> 66.67 (both ols-sweep-slo), 68 -> 69.33
	//     (pd-restart, prefill), 231.6 -> 234 (e1-tuned: the mean of two
	//     Pods over their EWMA). learned-state.csv gains a StartAdmittedAt
	//     row per start key per export (23, 23, 48). No TotalDemand and no
	//     variant capacity moves: the floor keeps the larger of its order at
	//     5c's start and at 5b's, and none of these fleets projects a backlog
	//     at a start the blend changed.
	//
	// Both on moves the blend's twelve and no more: no interaction. On
	// start-seed the blend's RoleStartSeconds rises 21 -> 53.67 alone, and
	// the two together give the seed's 120, then 241 (the measured Pod).
	saturation.SwitchStartReadsSeed: {
		"start-seed/composite-results.csv", "start-seed/results.csv",
	},
	saturation.SwitchStartBlendsFirstSamples: {
		"acceptance/learned-state.csv", "e1-tuned/results.csv", "engine/learned-state.csv",
		"mu-paths/learned-state.csv",
		"ols-sweep/composite-results.csv", "ols-sweep/results.csv",
		"ols-sweep-slo/composite-results.csv", "ols-sweep-slo/results.csv",
		"ols-sweep-slo-restart/composite-results.csv", "ols-sweep-slo-restart/results.csv",
		"pd-restart/composite-results.csv", "pd-restart/results.csv",
		// P2's ttft-saturated (#482), added before 5c merged: its decode
		// key holds two Pods, so RoleStartSeconds.decode rises, 98.3 ->
		// 100.5 on every cycle. No other TTFT golden moves.
		"ttft-saturated/composite-results.csv", "ttft-saturated/results.csv",
		// The start-seed goldens, recorded after 5c: RoleStartSeconds and
		// a StartAdmittedAt row per key per export.
		"start-seed/composite-results.csv", "start-seed/learned-state.csv", "start-seed/results.csv",
		"start-seed-restart/learned-state.csv", "start-seed-restart-2/learned-state.csv",
		// start-restored, recorded on a 5d build: a StartAdmittedAt row per
		// key per export.
		"start-restored/learned-state.csv",
		// The throughput-sweep goldens, added after 5c merged.
		"throughput-sweep-restart/composite-results.csv",
		"throughput-sweep-restart/results.csv",
		"throughput-sweep/composite-results.csv",
		"throughput-sweep/results.csv",
	},

	// Learner PR 5d, on main after 5c and #482 (59772fce2). Measured, each
	// alone against all off:
	//
	//   - SwitchStartCountedPodsRestored moves the six learned-state.csv
	//     below, rows added and none changed: each export gains a
	//     StartCounted row per counted Pod (103, 99, 180, 38, 80 and 51
	//     rows). No recorded document holds the field, so no restore in any
	//     recording reads it, and no figure moves. Its unit tests hold what it does
	//     after a restart (start_counted_test.go).
	//   - settle.SwitchFirstSightRestored moves nothing. start-restored
	//     holds a real settle.Restore, and E alone moves none of its outputs:
	//     its restored windows and lines price the fleet either way. Its unit
	//     tests hold it (heldBy), and
	//     TestGoldens_StartRestoredRestoresForReal asserts it on and off.
	saturation.SwitchStartCountedPodsRestored: {
		"acceptance/learned-state.csv", "engine/learned-state.csv", "mu-paths/learned-state.csv",
		// The start-seed goldens the same way. They predate 5d, so their
		// restores hold no startCounted and these replays still fold the
		// running Pods: golden_start_seed_chain_test.go shows the fold stop.
		"start-seed/learned-state.csv", "start-seed-restart/learned-state.csv", "start-seed-restart-2/learned-state.csv",
		// A real restart of a 5d build, whose document holds the field: off,
		// the running Pods are folded again, so RoleStartSeconds and the
		// estimate rows differ, beside the StartCounted rows.
		"start-restored/composite-results.csv", "start-restored/learned-state.csv", "start-restored/results.csv",
	},
	settle.SwitchFirstSightRestored: nil,
})

// learner4b2Seams is learner PR 4b-2's switches, earlier seams now (merged
// into earlierSeams): the throughput line on the engine's settle tracker,
// six switches. Measured each alone against all off from 1c9a34fe8 (4b-1
// merged), none moves a golden: the in-process fixtures give the analyzer
// no engine view. Each is held by its unit tests (heldBy).
var learner4b2Seams = map[string][]string{
	throughput.SwitchITLReadsSettle:                  nil,
	throughput.SwitchITLWaitsAfterRestart:            nil,
	throughput.SwitchITLKeepsSettleThroughMissingRow: nil,
	throughput.SwitchITLRestartUnderName:             nil,
	throughput.SwitchITLIdleOnEvidence:               nil,
	throughput.SwitchITLReadsFinitePodMean:           nil,
}

// learner4b1Seams is learner PR 4b-1's switches, earlier seams now (merged
// into earlierSeams), with the outputs 4b-1 measured each to move, each
// alone against all off from f7b4f7e47 (4b-0):
//
//   - The steady gate moves the throughput rows of results.csv and
//     variant-capacities.csv in all three, and, through the composite, the
//     engine fixture's composite-holds, composite-release and
//     composite-variants. Its line needs 10 readings in the window and a
//     settled replica's, where the parent priced the first cycle's rows: the
//     first cycles of each variant have no throughput capacity.
//   - The GPS-mismatch switch alone moves the throughput rows of
//     variant-capacities.csv in all three: the in-process rows trip the check,
//     and kept, the window reaches a full fit where the clear had emptied it.
var learner4b1Seams = map[string][]string{
	throughput.SwitchITLSteadyGate: {
		"acceptance/results.csv", "acceptance/variant-capacities.csv",
		"engine/composite-holds.csv", "engine/composite-release.csv", "engine/composite-variants.csv",
		"engine/results.csv", "engine/variant-capacities.csv",
		"mu-paths/results.csv", "mu-paths/variant-capacities.csv",
		"throughput-sweep-restart/composite-holds.csv",
		"throughput-sweep-restart/composite-release.csv",
		"throughput-sweep-restart/composite-results.csv",
		"throughput-sweep-restart/composite-roles.csv",
		"throughput-sweep-restart/composite-variants.csv",
		"throughput-sweep-restart/results.csv",
		"throughput-sweep-restart/variant-capacities.csv",
		"throughput-sweep-restart/warm-pool-pressure.csv",
		"throughput-sweep/composite-holds.csv",
		"throughput-sweep/composite-release.csv",
		"throughput-sweep/composite-results.csv",
		"throughput-sweep/composite-roles.csv",
		"throughput-sweep/composite-variants.csv",
		"throughput-sweep/results.csv",
		"throughput-sweep/variant-capacities.csv",
		"throughput-sweep/warm-pool-pressure.csv",
	},
	throughput.SwitchITLKeepsWindowOnGPSMismatch: {
		"acceptance/variant-capacities.csv", "engine/variant-capacities.csv", "mu-paths/variant-capacities.csv",
	},
}

// learner4aSeams is learner PR 4a's switches, earlier seams now (merged
// into earlierSeams): the ITL line, and the slo analyzer's line, on the
// settle tracker, six switches each, with the outputs 4a named each to
// move. The restart wait moves the in-process runs and
// ols-sweep-slo-restart; the move itself the in-process runs on the cycles
// after their clock jumps a day (settle.Rejoined); the other four move
// nothing and are held by their unit tests (heldBy).
var learner4aSeams = map[string][]string{
	// Since #432 the move reaches the aggregated line of the acceptance and
	// engine fixtures too: off, model-01's variant-01 does not settle in
	// time for its first fit at 09:32:00, so it publishes no |both line that
	// cycle, variant-02 borrows none, and their reported service rates and
	// the learned-state export lose them, as the slo analyzer's line does
	// under slo.SwitchITLReadsSettle.
	saturation.SwitchITLReadsSettle: {
		"acceptance/learned-state.csv", "acceptance/lines.csv", "acceptance/variant-capacities.csv",
		"engine/learned-state.csv", "engine/lines.csv", "engine/variant-capacities.csv",
	},
	saturation.SwitchITLWaitsAfterRestart: {
		"acceptance/learned-state.csv", "acceptance/lines.csv", "acceptance/variant-capacities.csv",
		"engine/composite-variants.csv", "engine/learned-state.csv", "engine/lines.csv", "engine/variant-capacities.csv",
		"mu-paths/learned-state.csv", "mu-paths/lines.csv", "mu-paths/variant-capacities.csv",
		"ols-sweep-slo-restart/composite-results.csv", "ols-sweep-slo-restart/lines.csv",
		"ols-sweep-slo-restart/results.csv", "ols-sweep-slo-restart/variant-capacities.csv",
		"throughput-sweep-restart/composite-results.csv",
		"throughput-sweep-restart/lines.csv",
		"throughput-sweep-restart/results.csv",
		"throughput-sweep-restart/variant-capacities.csv",
	},
	saturation.SwitchITLKeepsSettleThroughMissingRow: nil,
	saturation.SwitchITLReadsFinitePodMean:           nil,
	saturation.SwitchITLRestartUnderName:             nil,
	saturation.SwitchITLIdleOnEvidence:               nil,
	slo.SwitchITLReadsSettle: {
		"acceptance/slo-published.csv", "acceptance/variant-capacities.csv",
		"engine/slo-published.csv", "engine/variant-capacities.csv",
	},
	slo.SwitchITLWaitsAfterRestart: {
		"acceptance/slo-published.csv", "acceptance/variant-capacities.csv",
		"engine/slo-published.csv", "engine/variant-capacities.csv",
		"mu-paths/slo-published.csv", "mu-paths/variant-capacities.csv",
		"ols-sweep-slo-restart/slo-published.csv", "ols-sweep-slo-restart/variant-capacities.csv",
	},
	slo.SwitchITLKeepsSettleThroughMissingRow: nil,
	slo.SwitchITLReadsFinitePodMean:           nil,
	slo.SwitchITLRestartUnderName:             nil,
	slo.SwitchITLIdleOnEvidence:               nil,
}

// switchPackage is the package whose hook accepts the switch name.
func switchPackage(name string) string {
	if slices.Contains(slo.ReplaySwitchNames(), name) {
		return PackageSLO
	}
	if slices.Contains(throughput.ReplaySwitchNames(), name) {
		return PackageThroughput
	}
	if slices.Contains(settle.ReplaySwitchNames(), name) {
		return PackageSettle
	}
	return PackageSaturation
}

// earlierSeams is each switch an earlier learner PR added whose seam the
// code still has, and the golden outputs that PR named it to move. Each
// stays on, at its constructor default, in this PR's runs; off alone, from
// all on, it may move only what it named. A seam's row goes with the seam,
// in the PR, or cleanup PR, that deletes it with zero golden diff. Learner
// PR 4a's rows are learner4aSeams, 4b-1's learner4b1Seams and 4b-2's
// learner4b2Seams, merged in.
var earlierSeams = withMoves(staircaseSeamMoves, withMoves(startSeedSeamMoves, withSeams(learner4b2Seams, withSeams(learner4b1Seams, withSeams(learner4aSeams, map[string][]string{
	// #275 (two switches) and #438, this file's own rows before learner PR
	// 4b-1 (see replaySwitches).
	saturation.SwitchZeroReplicaDataParallel: nil,
	saturation.SwitchITLAggregatedShapeTerm:  {"acceptance/learned-state.csv", "engine/learned-state.csv"},
	saturation.SwitchITLAggregatedSizes:      nil,
	// The prefill-stress goldens (P2) were added after #392 and move under
	// it too: one decode cycle each in ttft-saturated and ttft-three-pods
	// takes k2's first reading, at the first queue (P1-obs in place of
	// P1-obs-unadmitted and P2-hist).
	saturation.SwitchK2FirstReadingWaits: {
		"e1-tuned/store-records.csv", "e1-tuned/variant-capacities.csv", "e1/variant-capacities.csv",
		"mu-paths/learned-state.csv", "mu-paths/store-records.csv", "mu-paths/variant-capacities.csv",
		"pd-restart/composite-results.csv", "pd-restart/composite-roles.csv", "pd-restart/composite-variants.csv",
		"pd-restart/store-records.csv", "pd-restart/variant-capacities.csv", "pd-restart/warm-pool-pressure.csv",
		"ttft-saturated/composite-variants.csv", "ttft-saturated/store-records.csv", "ttft-saturated/variant-capacities.csv",
		"ttft-three-pods/composite-variants.csv", "ttft-three-pods/store-records.csv", "ttft-three-pods/variant-capacities.csv",
		"throughput-sweep-restart/variant-capacities.csv",
	},
	saturation.SwitchK2ReadsSettle: nil,
	// No golden moved until the start-seed goldens (startSeedSeamMoves) and
	// throughput-sweep-restart. Its unit tests still hold it.
	saturation.SwitchMuKeyBucketHeld: {
		"throughput-sweep-restart/composite-results.csv",
		"throughput-sweep-restart/composite-roles.csv",
		"throughput-sweep-restart/results.csv",
		"throughput-sweep-restart/variant-capacities.csv",
	},
	saturation.SwitchMuMarginClamp: nil,
	saturation.SwitchMuSwapFreeze:  nil,
	saturation.SwitchK2WaitsAfterRestart: {
		"mu-paths/learned-state.csv", "mu-paths/store-records.csv", "mu-paths/variant-capacities.csv",
	},
	saturation.SwitchMuRepeatIsUse:       nil,
	saturation.SwitchMuUnknownAgeSafe:    nil,
	saturation.SwitchMuLatchRestoresSafe: nil,
	saturation.SwitchIdleBand:            nil,
	// As SwitchMuKeyBucketHeld: only the start-seed goldens
	// (startSeedSeamMoves) and throughput-sweep-restart move it.
	saturation.SwitchK2KeyBucketHeld: {
		"throughput-sweep-restart/variant-capacities.csv",
	},
	saturation.SwitchBatchCapDeclared:     nil,
	saturation.SwitchBatchCapLearned:      nil,
	saturation.SwitchBatchCapITLAdmission: nil,
	saturation.SwitchBatchCapDecodeView:   nil,
	saturation.SwitchBatchCapDerivedMu:    nil,
	slo.SwitchBatchCapAdmission:           nil,
	slo.SwitchBatchCapPerRank:             nil,
	saturation.SwitchITLPerToken:          nil,
	// #427's zero-replica estimate at the declared batch cap.
	saturation.SwitchBatchCapZeroReplica: nil,
	// #276's spread of output lengths in the KV per request. It moved no
	// golden until P2's ttft-three-pods, the first whose recorded output
	// histogram spreads (250 tokens out, three decode pods): there the
	// decode demand moves by a few parts in 100 million on about 80 cycles.
	saturation.SwitchKVPerRequestOutputDispersion: {
		"ttft-three-pods/composite-results.csv", "ttft-three-pods/composite-roles.csv",
		"ttft-three-pods/results.csv", "ttft-three-pods/variant-capacities.csv",
		"throughput-sweep-restart/composite-release.csv",
		"throughput-sweep-restart/composite-results.csv",
		"throughput-sweep-restart/composite-roles.csv",
		"throughput-sweep-restart/composite-variants.csv",
		"throughput-sweep-restart/results.csv",
		"throughput-sweep-restart/store-records.csv",
		"throughput-sweep-restart/variant-capacities.csv",
		"throughput-sweep-restart/warm-pool-pressure.csv",
		"throughput-sweep/composite-release.csv",
		"throughput-sweep/composite-results.csv",
		"throughput-sweep/composite-roles.csv",
		"throughput-sweep/composite-variants.csv",
		"throughput-sweep/results.csv",
		"throughput-sweep/variant-capacities.csv",
		"throughput-sweep/warm-pool-pressure.csv",
	},
	// Issue #431 routes the forget-learned-state annotation to the slo
	// analyzer's line, one switch, recorded as an event of its own
	// (slo.ForgetVariant). It moves no golden, measured: the acceptance and
	// engine recordings were taken again for it, and each carries one
	// slo.ForgetVariant (and nothing else new), with no output moved with the
	// switch on or off. Their forget lands at 09:31:30, on cycle 7, before the
	// slo line's first fit on cycle 9, so it drops a raw window and the
	// accelerator memo (Dropped 2, no intercept); two cycles later the clock
	// jumps a day and that window would have aged out anyway. No cluster
	// recording forgets. Its unit tests hold it (heldBy).
	slo.SwitchITLForgetReachesTheLine: nil,
	// #277 prices a resident request at the KV it is measured to hold,
	// between the prefix-cache hit rate's discount and none. It moves the
	// derived mu, and the decode demand the floor prices from it, in the two
	// fixtures whose derived mu the KV binds rather than the batch cap.
	//
	// Since #275 it moves the shape an aggregated line is fitted at too
	// (ITLLineShapes in the acceptance and engine exports): g is priced from
	// the KV a request holds, which #277 bounds (g 257.14 on, 281.25 off).
	saturation.SwitchKVPerRequestHitsResident: {
		"acceptance/learned-state.csv",
		"e1-tuned/results.csv", "e1-tuned/variant-capacities.csv",
		"e1/results.csv", "e1/variant-capacities.csv",
		"engine/learned-state.csv",
	},
	// #432 fits an ITL line for an aggregated (unlabeled, RoleBoth) variant,
	// as for decode, and reports the mu it derives without pricing it; its
	// line is shared only with other aggregated variants (a physics key
	// ending in |both). It moves the acceptance and engine fixtures, the two
	// whose models run aggregated variants (model-01's variant-01 and
	// variant-02, model-02's variant-03); no other fixture has one. As #432
	// measured it, all off against all on:
	//
	//   - lines.csv: model-01's variant-01 publishes a line under its |both
	//     key on its first fit (09:32:00), and variant-02, on the same engine
	//     configuration, borrows it on three cycles. No decode line moves.
	//   - learned-state.csv: that line, variant-01's intercept and its held
	//     slope join the two exports (three rows each); since #275 the line's
	//     shape (ITLLineShapes) too, which #275's switches need it on for.
	//   - variant-capacities.csv: 28 of 180 rows (acceptance) and 28 of 156
	//     (engine), all on the three aggregated variants: the ITL window, the
	//     line and a reported rate of 10.88/s, source derived-report-only.
	//     No demand, capacity, reason or borrow record moves.
	saturation.SwitchITLFitsAggregated: {
		"acceptance/learned-state.csv", "acceptance/lines.csv", "acceptance/variant-capacities.csv",
		"engine/learned-state.csv", "engine/lines.csv", "engine/variant-capacities.csv",
	},
})))))

// startSeedSeamMoves is what each earlier seam, off alone from all on, moves
// in the start-seed goldens, and in start-restored. Measured at 59772fce2, the
// commit that recorded the first three, and at 9c7aa470c, the fourth's. They are the first cluster goldens with k2, mu and ITL learning on a
// simulator's fleet, so seams that move no other recording move them.
var startSeedSeamMoves = map[string][]string{
	saturation.SwitchITLAggregatedShapeTerm: {
		"start-seed/variant-capacities.csv", "start-seed-restart/learned-state.csv", "start-seed-restart/lines.csv",
		"start-seed-restart/variant-capacities.csv", "start-seed-restart-2/variant-capacities.csv",
		"start-restored/learned-state.csv", "start-restored/lines.csv", "start-restored/variant-capacities.csv",
	},
	saturation.SwitchITLFitsAggregated: {
		"start-seed/variant-capacities.csv", "start-seed-restart/learned-state.csv", "start-seed-restart/lines.csv",
		"start-seed-restart/variant-capacities.csv", "start-seed-restart-2/variant-capacities.csv",
		"start-restored/learned-state.csv", "start-restored/lines.csv", "start-restored/variant-capacities.csv",
	},
	saturation.SwitchITLWaitsAfterRestart: {
		"start-seed-restart-2/learned-state.csv", "start-seed-restart-2/lines.csv", "start-seed-restart-2/variant-capacities.csv",
	},
	saturation.SwitchK2FirstReadingWaits: {
		"start-seed/composite-variants.csv", "start-seed/learned-state.csv", "start-seed/variant-capacities.csv",
		"start-seed-restart/learned-state.csv", "start-seed-restart/variant-capacities.csv",
		"start-restored/composite-results.csv", "start-restored/learned-state.csv", "start-restored/results.csv",
		"start-restored/variant-capacities.csv",
	},
	saturation.SwitchK2KeyBucketHeld: {
		"start-seed/learned-state.csv", "start-seed/variant-capacities.csv", "start-seed-restart-2/learned-state.csv",
	},
	saturation.SwitchKVPerRequestOutputDispersion: {"start-seed-restart/learned-state.csv", "start-restored/variant-capacities.csv"},
	saturation.SwitchMuKeyBucketHeld: {
		"start-seed/composite-results.csv", "start-seed/learned-state.csv", "start-seed/results.csv", "start-seed/variant-capacities.csv",
		"start-seed-restart-2/composite-results.csv", "start-seed-restart-2/learned-state.csv",
		"start-seed-restart-2/results.csv", "start-seed-restart-2/variant-capacities.csv",
	},
	saturation.SwitchMuMarginClamp: {
		"start-seed-restart-2/composite-results.csv", "start-seed-restart-2/learned-state.csv",
		"start-seed-restart-2/results.csv", "start-seed-restart-2/variant-capacities.csv",
	},
	saturation.SwitchMuSwapFreeze: {
		"start-seed/composite-results.csv", "start-seed/learned-state.csv", "start-seed/results.csv", "start-seed/variant-capacities.csv",
		"start-seed-restart-2/composite-results.csv", "start-seed-restart-2/learned-state.csv",
		"start-seed-restart-2/results.csv", "start-seed-restart-2/variant-capacities.csv",
	},
}

// withDigests is base with more's digests added.
func withDigests(more, base map[string]string) map[string]string {
	maps.Copy(base, more)
	return base
}

// withMoves is rows with more's outputs added to rows the same switch
// already has; a name with no row panics.
func withMoves(more, rows map[string][]string) map[string][]string {
	for name, outs := range more {
		if _, ok := rows[name]; !ok {
			panic("switch tables: " + name + " has no row to add to")
		}
		rows[name] = append(rows[name], outs...)
	}
	return rows
}

// withSeams is rows with more's rows added; a name in both is a row given
// twice, and panics.
func withSeams(more, rows map[string][]string) map[string][]string {
	for name, outs := range more {
		if _, dup := rows[name]; dup {
			panic("earlierSeams: " + name + " has two rows")
		}
		rows[name] = outs
	}
	return rows
}

// heldBy is, for a switch that names no output, the unit tests that hold it:
// no committed golden moves under it, so the runs below see nothing of it,
// and a switch whose off path were dead would pass them. Each test named
// must exist (by its func declaration, in the package directory given).
// The guard checks only that; each named test must also fail with its
// switch off (each of 4a's runs the switch on and off and asserts both). A
// test that holds the same on either side is a control, not a hold, and is
// not listed here.
var heldBy = map[string][]heldTest{
	// P1's prefill observation: it runs the switch on and off through
	// Analyze and asserts both (a line per cycle on, none off).
	saturation.SwitchPrefillCapacityObserve: {
		{"../saturation", "TestPrefillObserve_TheSwitchLogsTheObservation"},
		{"../saturation", "TestPrefillObserve_AnSGLangVariantIsRefusedVP"},
	},
	// P1's busy minutes: the first runs the switch on and off through
	// Analyze and asserts both (V_P from busy minutes on, the peak under a
	// queue off); the other three fail with it off (no busy minute, no
	// lower-bound reason, no KV-full pod passed).
	saturation.SwitchPrefillBusyMinutes: {
		{"../saturation", "TestPrefillBusy_BusyMinutesRecoverVP"},
		{"../saturation", "TestPrefillBusy_AnEmptiedOrUnreadMinuteIsNotBusy"},
		{"../saturation", "TestPrefillBusy_AKVFullMinuteIsNotBusy"},
		{"../saturation", "TestPrefillBusy_ADataParallelPodIsALowerBound"},
	},
	// P1's curved fit of a: each runs the switch on and off through Analyze
	// and asserts both. The first replays runs 3 and 4 of the TTFT
	// recordings (the curve's 7.95 ms on, the line's refusal off); the
	// second a synthetic sweep (2.8 ms on, refused off).
	saturation.SwitchPrefillAQuadratic: {
		{"../saturation", "TestPrefillAQuad_TheRecordingsThroughAnalyze"},
		{"../saturation", "TestPrefillAQuad_TheSwitchFitsTheCurve"},
	},
	// Issue #275's lift: an aggregated figure sizes once the GPS check has
	// agreed. Both run the switch on and fail with it off: the first checks
	// the lifted rate against a run with it off, the second needs the lift
	// before the shape moves.
	saturation.SwitchITLAggregatedSizes: {
		{"../saturation", "TestAggregatedShape_ALiftedLineSizesAtTheCorrectedRate"},
		{"../saturation", "TestAggregatedShape_AnOldLineDoesNotPriceAHeavierShape"},
	},
	// Issue #438's data-parallel ranks at zero replicas.
	saturation.SwitchZeroReplicaDataParallel: {
		{"../saturation", "TestZeroReplicaDataParallel_AnEightRankDecodeIsPricedAtEveryRank"},
		{"../saturation", "TestZeroReplicaDataParallel_AWholeNodePodWithNoDeclaredCap"},
		{"../saturation", "TestZeroReplicaDataParallel_TheClampsStillBound"},
		{"../saturation", "TestZeroReplicaDataParallel_AGroupIsPricedAtItsLeadersRanks"},
		{"../saturation", "TestZeroReplicaDataParallel_TheColdStartBudgetIsEveryRanks"},
		{"../saturation", "TestZeroReplicaDataParallel_UnknownPodsNeedTheGPUsToMatch"},
	},
	// The forget of the throughput line.
	throughput.SwitchITLForgetReachesTheLine: {
		{"../throughput", "TestForget_TheAnnotationForgetsTheLine"},
		{"../throughput", "TestForget_TakesTheHeldSlope"},
		{"../throughput", "TestForget_TakesTheAboveBandFallback"},
		{"../throughput", "TestForget_AtTheCapALineIsPricedOneSidedAgain"},
		{"../throughput", "TestForget_ReportsEachForgottenKey"},
	},
	// Issue #431's forget of the slo line.
	slo.SwitchITLForgetReachesTheLine: {
		{"../slo", "TestForget_TheAnnotationForgetsTheLine"},
		{"../slo", "TestForget_DropsTheAcceleratorMemo"},
		{"../slo", "TestForget_ReportsEachForgottenKey"},
	},
	// Learner PR 4b-2's throughput line on the engine's settle tracker: each
	// test runs the switch on and off through Analyze on the engine's rig.
	// The move names no output; its tests hold its two own rules directly
	// (a rollout under the same name, a gap between cycles).
	throughput.SwitchITLReadsSettle: {
		{"../throughput", "TestITLSettle_ANewRevisionUnderTheSameNameStartsTheSettle"},
		{"../throughput", "TestITLSettle_AGapBetweenCyclesPastTheToleranceSettlesAgain"},
	},
	throughput.SwitchITLWaitsAfterRestart: {
		{"../throughput", "TestITLSettle_AControllerRestartWaitsTheSettle"},
		{"../throughput", "TestITLSettle_AfterAControllerRestartTheLineIsSilentForTheSettle"},
		{"../throughput", "TestThroughputPropertySettleMutants"},
		{"../throughput", "TestThroughputProbe_AControllerRestartOnTheEngineTrackerWaitsItsSettle"},
	},
	throughput.SwitchITLKeepsSettleThroughMissingRow: {
		{"../throughput", "TestITLSettle_OneMissingRowKeepsTheSettle"},
	},
	throughput.SwitchITLRestartUnderName: {
		{"../throughput", "TestITLSettle_ANewReadyTimeUnderTheSameNameStartsTheSettle"},
	},
	throughput.SwitchITLIdleOnEvidence: {
		{"../throughput", "TestITLSettle_AScrapeThatReadNothingIsNotIdle"},
	},
	throughput.SwitchITLReadsFinitePodMean: {
		{"../throughput", "TestITLSettle_APodWithAnAbsentRankIsNoReading"},
	},
	// Learner PR 4b-3's cap admission on the throughput line: each runs the
	// switch on and off through Analyze (the property cases' mutant is the
	// switch off).
	throughput.SwitchITLAdmitsAtSequenceCap: {
		{"../throughput", "TestITLCap_AReplicaAtItsStatedCapIsAReading"},
		{"../throughput", "TestITLCap_AVariantAtItsCapPricesFromTheBandNotTheFallback"},
		{"../throughput", "TestITLCap_WhatIsNotAtTheCapIsNotAReading"},
		{"../throughput", "TestThroughputCapPropertyMutants"},
		{"../throughput", "TestThroughputProbe_AReplicaAtItsCapWithSiblingsBelowTheBandPricesFromTheBand"},
	},
	saturation.SwitchITLKeepsSettleThroughMissingRow: {
		{"../saturation", "TestITLSettle_OneMissingRowKeepsTheSettle"},
	},
	saturation.SwitchITLReadsFinitePodMean: {
		{"../saturation", "TestITLSettle_APodWithAnAbsentRankIsNoReading"},
	},
	slo.SwitchITLKeepsSettleThroughMissingRow: {
		{"../slo", "TestSettle_OneMissingRowKeepsTheSettle"},
	},
	slo.SwitchITLReadsFinitePodMean: {
		{"../slo", "TestSettle_APodWithAnAbsentRankIsNoReading"},
	},
	saturation.SwitchITLRestartUnderName: {
		{"../saturation", "TestITLSettle_ANewReadyTimeUnderTheSameNameStartsTheSettle"},
	},
	saturation.SwitchITLIdleOnEvidence: {
		{"../saturation", "TestITLSettle_AScrapeThatReadNothingIsNotIdle"},
	},
	slo.SwitchITLRestartUnderName: {
		{"../slo", "TestSettle_ANewReadyTimeUnderTheSameNameStartsTheSettle"},
	},
	slo.SwitchITLIdleOnEvidence: {
		{"../slo", "TestSettle_AScrapeThatReadNothingIsNotIdle"},
	},
	saturation.SwitchBatchCapZeroReplica: {
		{"../saturation", "TestBatchCapZeroReplica_TheEstimateIsPricedAtTheDeclaredCap"},
		{"../saturation", "TestBatchCapZeroReplica_ADeclaredCapAbove256RaisesTheEstimate"},
		{"../saturation", "TestBatchCapZeroReplica_TheFlagsWinAndTheDefaultIsNamed"},
		{"../saturation", "TestBatchCapZeroReplica_TheSwitchPicksTheSource"},
		{"../saturation", "TestBatchCapZeroReplica_APrefillVariantKeepsTheFlagsOrTheDefault"},
		{"../saturation", "TestBatchCapZeroReplica_ALearnedCapIsNeverRead"},
	},
	saturation.SwitchITLPerToken: {
		{"../saturation", "TestITLPerToken_TheLineIsFittedPerAcceptedToken"},
		{"../saturation", "TestITLPerToken_AMissingCounterUnderSpeculativeFlagsWithholdsTheReading"},
		{"../saturation", "TestITLPerToken_AStepTimeLineIsNotLentToASpeculatingVariant"},
		{"../saturation", "TestITLPerToken_AStepTimeSnapshotIsNotRestoredForASpeculatingVariant"},
		{"../saturation", "TestITLPerToken_AnAcceptedCountAboveTheDraftLengthIsWithheld"},
		{"../saturation", "TestITLPerToken_AWithheldReadingIsLoggedOncePerChange"},
	},
	saturation.SwitchKVPerRequestOutputDispersion: {
		{"../saturation", "TestKVOutputDispersion_TheDerivedK2HoldsTheSpread"},
		{"../saturation", "TestKVOutputDispersion_TheDerivedMuFitsFewerSequences"},
		{"../saturation", "TestKVOutputDispersion_AZeroReplicaVariantIsPricedWithTheSpread"},
		{"../saturation", "TestKVOutputDispersion_AZeroReplicaEstimateIsRequestWeighted"},
	},
	saturation.SwitchBatchCapITLAdmission: {
		{"../saturation", "TestBatchCapReaders_TheITLWindowTakesAReplicaAtItsDeclaredCap"},
		{"../saturation", "TestBatchCapReaders_ALearnedCapIsNotAnITLCap"},
		{"../saturation", "TestBatchCapReaders_TheITLAdmissionSwitchPicksTheSource"},
		{"../saturation", "TestBatchCapReaders_ADeclaredCapAbove256"},
	},
	saturation.SwitchBatchCapDecodeView: {
		{"../saturation", "TestBatchCapReaders_APDDecodeAtItsDeclaredCapKeepsTheFullQueue"},
		{"../saturation", "TestBatchCapReaders_TheDecodeViewSwitchPicksTheSource"},
		{"../saturation", "TestBatchCapReaders_ALearnedCapIsNotADecodeViewCap"},
	},
	saturation.SwitchBatchCapDerivedMu: {
		{"../saturation", "TestBatchCapReaders_TheDerivedMuIsCappedAtTheDeclaredCap"},
		{"../saturation", "TestBatchCapReaders_TheDerivedMuSwitchPicksTheSource"},
		{"../saturation", "TestBatchCapReaders_ALearnedCapLowersTheDerivedMu"},
		{"../saturation", "TestBatchCapReaders_ADeclaredCapAbove256"},
	},
	slo.SwitchBatchCapAdmission: {
		{"../slo", "TestBatchCap_TheLineTakesAReplicaAtItsDeclaredCap"},
		{"../slo", "TestBatchCap_TheAdmissionSwitchPicksTheSource"},
	},
	slo.SwitchBatchCapPerRank: {
		{"../slo", "TestBatchCap_AReplicaIsPricedAtItsDeclaredCap"},
		{"../slo", "TestBatchCap_ThePerRankSwitchPicksTheSource"},
		{"../slo", "TestBatchCap_TheLogNamesThePricedCap"},
		{"../slo", "TestBatchCap_ADeclaredCapAbove256RaisesThePrice"},
	},
	saturation.SwitchIdleBand: {
		{"../saturation", "TestIdleBand_ARunStartsUnderTheLineAndEndsAtTheTop"},
		{"../saturation", "TestIdleBand_TheSwitchOffHoldsEveryReadingToTheLine"},
	},
	saturation.SwitchK2KeyBucketHeld: {
		{"../saturation", "TestK2KeyBucket_TheSmoothedLengthLeavesPastTheMargin"},
		{"../saturation", "TestK2KeyBucket_ASwapMovesItTheSameWayBothWays"},
		{"../saturation", "TestK2KeyBucket_OneReadingNeverMovesIt"},
		{"../saturation", "TestK2KeyBucket_APendingSwapHoldsTheBucket"},
		{"../saturation", "TestK2KeyBucket_AMovedKeyIsPricedFromTheBucketItLeft"},
		{"../saturation", "TestK2KeyBucket_TheSwitchPicksTheLength"},
	},
	saturation.SwitchMuKeyBucketHeld: {
		{"../saturation", "TestMuKeyBucket_TheSwitchPicksTheLength"},
		{"../saturation", "TestMuKeyBucket_TheKeysShareABucket"},
		{"../saturation", "TestMuKeyBucket_AMovedKeyIsCarriedLowerOnly"},
		{"../saturation", "TestMuKeyBucket_TheCarryPricesThroughAnalyze"},
	},
	saturation.SwitchMuSwapFreeze: {
		{"../saturation", "TestMuKeyBucket_APendingSwapFreezesTheWindow"},
		{"../saturation", "TestMuKeyBucket_APendingSwapLeavesPrefillWriting"},
	},
	saturation.SwitchMuMarginClamp: {
		{"../saturation", "TestMuKeyBucket_AMarginMoveNeverRaisesTheWindow"},
		{"../saturation", "TestMuKeyBucket_TheClampWritesNothingToAnEmptyWindow"},
	},
	saturation.SwitchK2ReadsSettle: {
		{"../saturation", "TestSettleView_TheTrackerSwitchMovesTheAbsenceRule"},
		{"../saturation", "TestSettleView_TheTrackerSwitchKeepsTheSettleThroughAZeroReadyTime"},
	},
	saturation.SwitchMuRepeatIsUse: {
		{"../saturation", "TestMuRepeat_IsUseNotAWrite"},
		{"../saturation", "TestMuRepeat_AFrozenSeriesLetsTheEpisodeEnd"},
		{"../saturation", "TestMuRepeat_OverDerivedDoesNotKeepTheMeasurementFresh"},
	},
	saturation.SwitchMuUnknownAgeSafe: {
		{"../saturation", "TestMuUnknownAge_UnderDerivedKeepsTheLowHold"},
		{"../saturation", "TestMuUnknownAge_NoTimeAtAllIsHeld"},
	},
	saturation.SwitchBatchCapDeclared: {
		{"../saturation", "TestBatchCapSwitch_DeclaredOffPricesTheDefault"},
	},
	saturation.SwitchBatchCapLearned: {
		{"../saturation", "TestBatchCapSwitch_LearnedOffLearnsNothing"},
		{"../saturation", "TestBatchCap_SwitchesOffReproduceTheIssue"},
	},
	saturation.SwitchMuLatchRestoresSafe: {
		{"../saturation", "TestMuLatch_ARestoredMeasurementOverDerivedComesBackLatched"},
		{"../saturation", "TestMuLatch_TheRestoredLatchIsKnownAfterItsFirstChoice"},
		{"../saturation", "TestMuLatch_AWindowRestoredAfterARefusedReadingComesBackLatched"},
		{"../saturation", "TestMuLatch_AWindowBroughtBackFromTheStashComesBackLatched"},
		{"../saturation", "TestMuLatch_AWindowRestoredAsAPriorAfterARefusedReadingComesBackLatched"},
	},
	// Learner PR 5b's start guard on distinct Pods across a run: each runs
	// the switch on and fails with it off; (a) to (d) and the lines' also
	// run it off and assert the parent's figure.
	saturation.SwitchStartConfirmsByDistinctPod: {
		{"../saturation", "TestStartConfirm_OnePodRepeatingNeverOpensTheGuard"},
		{"../saturation", "TestStartConfirm_AOneReplicaVariantLearnsASlowerImage"},
		{"../saturation", "TestStartConfirm_ARestoredEstimateIsCorrectedByRollouts"},
		{"../saturation", "TestStartConfirm_TwoSteadyPodsAndALaterThirdOpenIt"},
		{"../saturation", "TestStartConfirm_ALargeStepReachesTheNewLevel"},
		{"../saturation", "TestStartConfirm_TheMedianIgnoresABlipInTheRun"},
		{"../saturation", "TestStartConfirm_ARunPodBackInRangeLeavesTheRunOpen"},
		{"../saturation", "TestStartConfirm_TheLinesCountDistinctPods"},
		{"../saturation", "TestStartConfirm_TheMedianOfTheRun"},
		{"../saturation", "TestStartConfirm_ARestartDropsTheRun"},
		{"../saturation", "TestStartConfirm_ThroughAnalyze"},
	},
	// Issue #467's slo switch: each runs it on and off through the line's
	// pricing (priceVariant) and asserts both (off, the line through the
	// default B, which meets the target above the truth's fill).
	slo.SwitchITLOneSidedAtCap: {
		{"../slo", "TestOneSided_TheBiasTableAtTheTarget"},
		{"../slo", "TestOneSided_TheHeldSlopeKeepsThePriceAndEndsWithTheHold"},
		{"../slo", "TestOneSided_TheSwitchPicksTheLine"},
	},
	// Learner PR 5d's first sight through a restart: no committed recording
	// holds a settle.Restore event, so no golden moves. The settle tests run
	// the tracker on and off; the saturation ones are the design's negative
	// control and the line's wait, each asserted on and off.
	settle.SwitchFirstSightRestored: {
		{"../../../signals/settle", "TestFirstSightSwitch_OffIsTheParent"},
		{"../saturation", "TestFirstSight_ARestartWithTheSnapshotDoesNotWait"},
		{"../saturation", "TestFirstSight_TheITLLineDoesNotWaitAgain"},
		{"../saturation", "TestFirstSight_TheLearnerStartLine"},
	},
	// Learner PR 5c's seed as the prior: each runs the switch on and off
	// and asserts both (the floor projects over the seed and share pacing
	// reads it on; nothing is projected off).
	saturation.SwitchStartReadsSeed: {
		{"../saturation", "TestStartSeed_AnUnmeasuredSeededVariantIsSizedAtTheSeed"},
		{"../saturation", "TestStartSeed_ThroughAnalyzeTheFloorProjectsOverTheSeed"},
		{"../saturation", "TestStartBlend_OffTheFirstSampleIsTheEstimate"},
		{"../saturation", "TestStartBlend_TheMeasuredLineNamesTheBlend"},
		{"../saturation", "TestStartSeed_TheGaugeNamesTheSource"},
	},
}

// Every switch the saturation and slo hooks accept has a row: this PR's, or
// an earlier PR's seam still in the code. A seam with no row is a switch no
// run drives.
func TestGoldens_EverySwitchHasARow(t *testing.T) {
	names := append(append(saturation.ReplaySwitchNames(), slo.ReplaySwitchNames()...), throughput.ReplaySwitchNames()...)
	names = append(names, settle.ReplaySwitchNames()...)
	for _, name := range names {
		_, mine := replaySwitches[name]
		_, earlier := earlierSeams[name]
		require.True(t, mine || earlier, "%s is accepted by SetSwitchesForReplay and is in neither replaySwitches nor earlierSeams", name)
	}
	for name := range mergedRows() {
		require.Contains(t, names, name, "%s's seam is gone: delete its row", name)
	}
}

// heldTest is a test function, and the package directory that declares it.
type heldTest struct{ dir, name string }

// A switch that names no golden output names the unit tests that hold it,
// and they exist.
func TestGoldens_ASwitchNoGoldenMovesIsHeldByItsTests(t *testing.T) {
	for name, outs := range mergedRows() {
		if len(outs) > 0 {
			continue
		}
		tests := heldBy[name]
		require.NotEmpty(t, tests, "%s moves no golden: name the unit tests that hold it in heldBy", name)
		for _, h := range tests {
			files, err := filepath.Glob(filepath.Join(h.dir, "*_test.go"))
			require.NoError(t, err)
			found := false
			for _, f := range files {
				b, err := os.ReadFile(f)
				require.NoError(t, err)
				if strings.Contains(string(b), "func "+h.name+"(t *testing.T)") {
					found = true
					break
				}
			}
			require.True(t, found, "%s is held by %s, which %s does not declare", name, h.name, h.dir)
		}
	}
}

// mergedRows is replaySwitches and earlierSeams together (mergeRows).
func mergedRows() map[string][]string {
	return mergeRows(replaySwitches, earlierSeams)
}

// mergeRows is mine and earlier together, on a copy; a name in both is a
// switch with two rows, this PR's and an earlier one's, and panics, as
// withSeams does: the row it kept would decide which outputs the switch may
// move.
func mergeRows(mine, earlier map[string][]string) map[string][]string {
	return withSeams(maps.Clone(mine), maps.Clone(earlier))
}

// A switch in both replaySwitches and earlierSeams panics, and the tables
// as they stand do not.
func TestGoldens_ASwitchWithTwoRowsPanics(t *testing.T) {
	require.Panics(t, func() { mergeRows(map[string][]string{"x": nil}, map[string][]string{"x": nil}) })
	require.NotPanics(t, func() { mergedRows() })
}

// switchBase is the sha256 of each output a switch of this PR names, as the
// base commit's goldens hold it (main at 891c071c4, learner PR 4b-2 merged;
// the same bytes as at 1c9a34fe8, since 4b-2 moved none). It also holds the
// base digests of the outputs a throughput switch could move that the
// expectation does not name, so the measured rows need no new lookup.
var switchBase = withDigests(staircaseBase, map[string]string{
	"acceptance/results.csv":            "7aef7e7391f761fe984f89720d61b0583048b330989c449098dce8ec2d285981",
	"acceptance/variant-capacities.csv": "47ad82ad3c017924b6cc5398d9578ddd28ab95fdc547adcec6decb69abcc91b3",
	"engine/composite-holds.csv":        "ce02c822639f90959858c2ddc1e4681dc5b65d281a13d033520cfaa9945d3e0c",
	"engine/composite-release.csv":      "f796638c7d8c1f0544cd3ab0d9850a206a470b166cad3b2318ea323f4ed03eaf",
	"engine/composite-variants.csv":     "9cb3149dc328096552a9b5aa640ffb2195e92f562e73266d5d89e92fd9e09828",
	"engine/results.csv":                "fdef165cd21f47deb81d73870af4953c05d9c303180975e713e39e4a138845e9",
	"engine/variant-capacities.csv":     "bded174c24f569874299866fb541e7f4ac4cc72294adbaf8e626f1e9ea06712f",
	"mu-paths/results.csv":              "83fca27f185344e2784706b0b6ae2e3ea8fe0ae70ecabe332862f04f4f4e951c",
	"mu-paths/variant-capacities.csv":   "dd5b3186f6fe513520ba6e0309d88520e668d60f84003326f57bd7dee0ca0275",

	// Issue #467's saturation switch, as main holds them at 602054e98.
	"e1/lines.csv":                    "d2691a9743301a74e2fdff1b301d2e7f5349edf987604ecadc52a8bddc44fd58",
	"e1/results.csv":                  "67b1a3af97f5951c65a8c8797a122428e85ad7aae624be4c33171b230651c2e4",
	"e1/variant-capacities.csv":       "5fc3ce5024a4e49d5712e20741a24d09dfe82d8270d8528f129294f618db13b8",
	"e1-tuned/lines.csv":              "eadedb4b3076f0c9ce1d998f6719004c26bf790155fe7bd5c390a85a959f507e",
	"e1-tuned/results.csv":            "9658cb3c3e302386715bfaac80ee245581eea3ec1bf6b528906a0756d1ef93fa",
	"e1-tuned/variant-capacities.csv": "919fd2193f111f1edc49b20a0db348a98355ecab7be221af5a35925a0d1ac3e4",

	// Learner PR 5c's outputs (SwitchStartBlendsFirstSamples), at main
	// 602054e98, 5b merged. e1-tuned/results.csv is #467's row above, the same digest.
	"acceptance/learned-state.csv":                "da4251071addee288ecbe495759e2af257fa54b3511bbb93c5eddf4517ecc7a9",
	"engine/learned-state.csv":                    "d823bbfe7f483ebca87d84224c0ae3faa5daef6546f14ffd5648937dc885dacf",
	"mu-paths/learned-state.csv":                  "97ca32de070b4879ac59a3171a6a9387d91e08cc42c7a267e82f2576e35fa621",
	"ols-sweep/composite-results.csv":             "d8a462aaa27a4c08077f8974172dd140d11a5ed2d675b932c632659d752db432",
	"ols-sweep/results.csv":                       "06edf09352f73434d7a519884cccd872f8a68152d83b7b926b78c937052482a5",
	"ols-sweep-slo/composite-results.csv":         "e7fbf20c0e73f463cb6c772129a2b97ed5664dc77103abef83a9b550ba660ce1",
	"ols-sweep-slo/results.csv":                   "5929aa0e195a10310472e0849337e1457f91814c1b8366efe542ea161986291c",
	"ols-sweep-slo-restart/composite-results.csv": "4ee9847180ddc44dfdefe11699931a5b0a224fa51cab950ed39c29937c3f5b75",
	"ols-sweep-slo-restart/results.csv":           "49a51c8b1bdbcbfd2401b05b383113727ec51384867dfa49c31e17349d60ae47",
	"pd-restart/composite-results.csv":            "7cd3bf0237b97c5ec7ec2a26e4c096701275ca79d622ad064c1ce06e3734b3f2",
	"pd-restart/results.csv":                      "1f7f9e64e4fa1cb7aa467e70358fd74778c45045f7f5460791bd5861e9234637",
	// ttft-saturated as #482 added it, before 5c (1ffdb6ad8).
	"ttft-saturated/composite-results.csv": "a94dd5a36fea549888a525795b152e7199969f46319e25d95bf2f5c8211688bd",
	"ttft-saturated/results.csv":           "4c1da5e9d1ecb5f7c084703c99c1907811de56e7974fc62b6d96ec965f05b7d7",

	// S1's outputs (slo.SwitchTTFTPrice), at main 59772fce2.
	"ols-sweep-slo/slo-published.csv":         "24b0954e3b5811850a2fcc460cf46b7790d478081cb9bee49db1e38847bb3446",
	"ols-sweep-slo-restart/slo-published.csv": "e9b48abe22e4d484e861290c5f417edb5f28b652ceeb21c89d8aaeb2dd516008",

	// The start-seed goldens have no base commit: each digest is the output
	// replayed with every switch of replaySwitches off, at 59772fce2.
	"start-seed/composite-results.csv":       "2c3b9c87e8540ac2281582ac49f0d740458109bca6c2f63f4b30c319fb4543dd",
	"start-seed/learned-state.csv":           "860fbdbd8559d29d4caf900c85802657bbacd811bf816175ae21c6d574f06346",
	"start-seed/results.csv":                 "84c0d2fee77414c2816bb49818aa0466ddbc51501e29dbc621430c7b21bb7265",
	"start-seed-restart/learned-state.csv":   "ae524f09cd431c1bd754cc903d0870d0d32e9b9b39b9a2a2999595e4cf7c4eb4",
	"start-seed-restart-2/learned-state.csv": "099f2c638296ba7f62820ed86fbfdcaf30dfcfc1e6d069bbc8b80080414a835d",

	// start-restored has no base commit either: every switch of
	// replaySwitches off, at 9c7aa470c.
	"start-restored/composite-results.csv": "9838f3fcbd4a6d6f1aa2786eb7c24ba97c1da456d976903eb9307c7f363eab9e",
	"start-restored/learned-state.csv":     "7bcb943f4f131f080ab70e27571c1dcffaad974d840ddb11954f9ef79d88afee",
	"start-restored/results.csv":           "6b7a8e062c42203b92eff8e2a1738a98aafb91c69a29c229dbcbfd3b5821cc0d",
	// The throughput-sweep goldens, replayed with this PR's switches all off.
	"throughput-sweep-restart/composite-holds.csv":    "e753e8d995badf84c780c9ae9e7f077ef0a6e0d738a1b6e46d330d10964d9092",
	"throughput-sweep-restart/composite-release.csv":  "25f7d0a50cd3b20f344b3e83aca36ffc9ac74713319ab1dd840ad3f3f7c4f128",
	"throughput-sweep-restart/composite-results.csv":  "16c8a61ca02f5a3778ad229b050c859e2890b3defeb148b3b7fa183025dfbd80",
	"throughput-sweep-restart/composite-roles.csv":    "59600bea0bc8d602c87e06e7578bcf9c5d24b6ee25eb160dd37c4662518e7e45",
	"throughput-sweep-restart/composite-variants.csv": "5c0e35f0a39c03ea40d80f241de739985e6f0b578cd98f9a045a0d56c234f7ae",
	"throughput-sweep-restart/results.csv":            "c76fd421028227104c9f50de695dc8fdda959958c2547b1919827157269153cf",
	"throughput-sweep-restart/variant-capacities.csv": "ff6bac8958d07f45c547edf83028dffac78aea3aeeadcd49eee719a7ef6c48ce",
	"throughput-sweep/composite-results.csv":          "dde3434231c8217840247e6b379be47cd730063d362a8d2d6eb93a926b48c68a",
	"throughput-sweep/results.csv":                    "95273482cb748258383bb0dfb1c1f8001eb6dcd3cc1248d62b7d840cef60f241",
})

// The slow tests of this package run in parallel with each other (each
// marked "reads the goldens, writes only to its own copies"): each replays
// with analyzers of its own and writes only under t.TempDir. The one test
// that swaps the registered engine replayer
// (TestEngineGoldens_NoEngineRegisteredIsAProblem) is not
// parallel, so it has restored it before any of them starts. Run in turn,
// the package took about 330 s with coverage, past the CI job's 10 min on a
// runner half as fast; this test alone took 139 s of it.
func TestGoldens_TheSwitchRuns(t *testing.T) {
	t.Parallel() // reads the goldens, writes nowhere
	m, err := ReadManifest(goldenDir)
	require.NoError(t, err)
	named := map[string]bool{}
	for _, outs := range replaySwitches {
		for _, o := range outs {
			named[o] = true
			require.Contains(t, switchBase, o, "an output a switch names needs its base digest")
		}
	}
	set := func(on ...string) map[string]bool {
		sw := map[string]bool{}
		for name := range replaySwitches {
			sw[name] = slices.Contains(on, name)
		}
		return sw
	}
	allOn := func() map[string]bool { return set(slices.Collect(maps.Keys(replaySwitches))...) }

	// Every run the rule asks for, replayed up front: all off, each of this
	// PR's switches alone, all on, and each earlier seam off alone from all
	// on. Each run is independent of the others (its own analyzers, store
	// and settle tracker), so the replays run in parallel, one per fixture
	// per run, bounded by GOMAXPROCS; the comparisons below then read them
	// in the order the rule states.
	mine := slices.Sorted(maps.Keys(replaySwitches))
	earlier := slices.Sorted(maps.Keys(earlierSeams))
	sets := make([]map[string]bool, 0, len(mine)+len(earlier)+2)
	sets = append(sets, set())
	for _, name := range mine {
		sets = append(sets, set(name))
	}
	sets = append(sets, allOn())
	for _, name := range earlier {
		sw := allOn()
		sw[name] = false
		sets = append(sets, sw)
	}
	runs := replaySets(t, m, sets)
	off, alone, on, earlierOff := runs[0], runs[1:1+len(mine)], runs[1+len(mine)], runs[2+len(mine):]

	// moved is the outputs that differ between two runs, sorted, each
	// logged with the replay's one-line summary of how.
	moved := func(name string, from, to map[string][]byte) []string {
		var out []string
		for f, b := range to {
			d, err := compareOutput(f, from[f], b)
			require.NoError(t, err)
			if d != nil {
				out = append(out, f)
				t.Logf("%s: %s", name, d)
			}
		}
		slices.Sort(out)
		return out
	}

	for f, b := range off {
		if named[f] {
			sum := sha256.Sum256(b)
			require.Equal(t, switchBase[f], hex.EncodeToString(sum[:]),
				"all off: %s is not the base commit's golden", f)
			continue
		}
		committed, err := os.ReadFile(filepath.Join(goldenDir, filepath.FromSlash(f)))
		require.NoError(t, err)
		d, err := compareOutput(f, committed, b)
		require.NoError(t, err)
		require.Nil(t, d, "all off: %s moved, and no switch names it", f)
	}
	for i, name := range mine {
		got := moved(name, off, alone[i])
		// Exactly what it names: a named output that does not move is an off
		// path that went dead.
		require.ElementsMatch(t, replaySwitches[name], got, "%s alone must move exactly the outputs it names", name)
		t.Logf("%s alone moves %d outputs: %v", name, len(got), got)
	}
	for f, b := range on {
		committed, err := os.ReadFile(filepath.Join(goldenDir, filepath.FromSlash(f)))
		require.NoError(t, err)
		d, err := compareOutput(f, committed, b)
		require.NoError(t, err)
		require.Nil(t, d, "all on: %s is not the committed golden", f)
	}
	for i, name := range earlier {
		got := moved(name, on, earlierOff[i])
		require.ElementsMatch(t, earlierSeams[name], got, "%s off alone must move exactly the outputs its PR named", name)
		t.Logf("%s off alone moves %d outputs: %v", name, len(got), got)
	}
}

// replaySets replays every fixture of m once per switch set, and returns,
// per set, its outputs by fixture/file. Each fixture's recording is read
// once; the replays run on GOMAXPROCS workers, since each builds its own
// analyzers and touches no state another shares.
func replaySets(t *testing.T, m *Manifest, sets []map[string]bool) []map[string][]byte {
	t.Helper()
	recs := make([][]byte, len(m.Goldens))
	for i, g := range m.Goldens {
		gz, err := os.ReadFile(filepath.Join(goldenDir, filepath.FromSlash(g.Recording)))
		require.NoError(t, err)
		recs[i], err = gunzip(gz)
		require.NoError(t, err)
	}
	type job struct{ set, golden int }
	type result struct {
		job
		outs map[string][]byte
		err  error
	}
	jobs := make(chan job)
	results := make(chan result)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				outs, err := replayGolden(recs[j.golden], m.Goldens[j.golden].Name, sets[j.set])
				results <- result{job: j, outs: outs, err: err}
			}
		}()
	}
	go func() {
		for s := range sets {
			for g := range m.Goldens {
				jobs <- job{set: s, golden: g}
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	out := make([]map[string][]byte, len(sets))
	for i := range out {
		out[i] = map[string][]byte{}
	}
	var errs []error
	for r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		for _, f := range m.Goldens[r.golden].Outputs {
			out[r.set][f] = r.outs[path.Base(f)]
		}
	}
	require.NoError(t, errors.Join(errs...))
	return out
}

// replayGolden replays one fixture's recording with the switches set, and
// returns its outputs by file.
func replayGolden(rec []byte, name string, sw map[string]bool) (map[string][]byte, error) {
	byPkg := map[string]map[string]bool{}
	for n, on := range sw {
		pkg := switchPackage(n)
		if byPkg[pkg] == nil {
			byPkg[pkg] = map[string]bool{}
		}
		byPkg[pkg][n] = on
	}
	// A run without the throughput analyzer has none of its switches: off
	// and on are the same run there.
	if _, ok := byPkg[PackageThroughput]; ok {
		read, err := learnrec.Read(rec)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		var start learnrec.Start
		if err := read.Events[0].DecodeData(&start); err != nil {
			return nil, fmt.Errorf("%s: start: %w", name, err)
		}
		if !start.Throughput {
			delete(byPkg, PackageThroughput)
		}
	}
	rep, err := Replay(context.Background(), rec, Options{Mode: Normal, Switches: byPkg})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if f := rep.Failed(); len(f) > 0 {
		return nil, fmt.Errorf("%s: the replay failed: %v", name, f)
	}
	return Outputs(rep.Recording)
}
