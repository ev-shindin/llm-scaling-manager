# Shared rules for the analyzers' learners

- **Review:** Revision 6 was reviewed in five rounds by four reviewers; all
  four: good enough (round 5).
- **Status:** built. The PRs of [section 7](#7-migration) have merged and #341 is
  closed. What this document asks for and main does not do yet is listed in
  #495. The notes below are each PR's decisions, as it made them, and read in
  the future tense where the PR came first. Revision 6 merged as
  #286. Revision 7 re-checked it against main 3515c6ccd, after #282, #283,
  #288, #293, #294, #298, #302, #303, #306, #329, #331 and #361 merged. Line
  numbers are main's at that commit.
  [Shipped behaviour each PR must keep](#shipped-behaviour-each-pr-must-keep)
  lists what those PRs fixed that a later step could undo.
- **Since revision 7:** #345 step 1 landed: one analyzer registry, and one
  `AnalyzerInput` per model built before any `Analyze` (`buildAnalyzerInput`).
  It runs after `observeLoadBalance`, as the scale-target load did. PR 1's
  settle tracker must observe before the balance tracker reads (4.2), so it
  goes at the top of `runAnalyzers`, ahead of both (decided on #345; the PR 1
  row says so). The engine now reaches
  saturation's capabilities through typed fields resolved at registration
  (`saturationCapabilities`), not by assertion at each call; a wrapper around
  the analyzer would still hide them (2.4). `runV2AnalysisOnly` is now
  `buildAnalyzerInput` followed by `runSaturation`.
- **PR 1's decisions.** The brownout trade (4.3): a run of unknown rows keeps
  the settle, and the PR does not end it after 2 min; an idle run that a
  brownout interrupts for more than the tolerance is no return of load
  (`TestABrownoutKeepsTheSettle`). A brownout that drops the rows altogether
  (a failed collection, or one that returns no series) keeps it too: the
  engine holds the model's sightings on such a cycle
  (`TestSettle_AFailedCollectionKeepsTheSighting`,
  `TestSettle_OptimizeV2HoldsOnEveryPathWithNoRows`), for up to 5 min
  without an observation (or the tolerance, if longer), after which the
  model is forgotten. A hold leaves idle runs to the tolerance, as missing
  rows do (`TestHoldLeavesIdleRunsToTheTolerance`). The absence tolerance
  (4.2) is 1 min or 2.5 optimize intervals, whichever is longer, so a long
  interval does not make every cycle an absence
  (`TestTrackerEndsTheGraceAtLongIntervals`). The re-lend (4.2): the warm pool's lend
  time rides on each bridge row (`ReplicaMetrics.LentSince`), and a new one
  under the same key is a lend event (`TestTrackerRestartsTheGraceOfAPodLentAgain`).
  Checked in the code, not on a run: a lend does not touch the Pod's
  readiness, so ReadySince does not change on a re-lend, and a re-lend to
  the same variant keeps the key.
- **PR 2's decisions.** Where an LWS group's Ready time comes from (4.3):
  the collector builds one row per group, the leader's; a worker's instance
  is dropped before its row is built (`isLWSWorker`), and the row carries
  the latest Ready time over the leader and its live, Ready workers,
  matched by the LWS set-name and group-index labels (`groupReadyTimes`,
  `replicaReadySince`; `TestLWSGroupReadyTimeIsTheLatestOfItsPods`). Under
  LWS's default `restartPolicy`, `RecreateGroupOnPodRestart`, any worker's
  restart deletes the leader, so the recreated leader has a new Ready time
  and the group waits again. Under `restartPolicy: None` a worker-only
  restart leaves the leader Ready, and the restarted worker's Ready time
  makes the group wait. A worker missing or not Ready adds nothing, so it
  never makes the group's time zero, and the group waits from the worker's
  own once it is Ready. The time is not monotonic: a worker that drops while
  it holds the latest time moves the group back to the next latest Pod's,
  which the tracker reads as a restart, so the group waits from the drop and
  again from the return (cautious, accepted). A not-Ready leader still makes
  the group's Ready time zero. Checked in the code (the collector and lws
  v0.8.0's pod controller), not on a run. k2's joined
  test reads the tracker, keeping #288's rule, behind two replay switches:
  `k2-reads-the-settle-tracker` (a replica unseen past the tracker's
  absence tolerance waits again, where the memo held it 2 min; and a Ready
  row whose Ready time reads zero keeps the settle, where the memo waited
  twice; no committed golden moves with it alone) and `k2-waits-after-controller-restart`
  (every replica waits once after a restart; moves mu-paths only). An
  analyzer given no view, as unit tests build one, observes a tracker of
  its own and takes its first cycle's replicas as already watched; a
  controller restart is the engine's tracker's first cycle. `learn` holds
  `Report`, `Forgotten` and the list `Learners` (`k2`); k2's
  `learner-write` band is 2 % of the last logged value, not measured on a
  run. The counter `k2-unsettled-window` is required since the
  flow-control-off recording landed as the `pd-restart` golden.
  Forget through `learn.Learners` drops the same entries, counted the same,
  so it is behaviour-neutral and has no switch; the clause "when no key
  remains, the variant's learner series are deleted" has nothing to apply
  to in PR 2, which adds no learner series, and waits for the first PR that
  publishes one. The switch runs are a committed test
  (`TestGoldens_TheSwitchRuns`).
- **PR 3's decisions.** Mu (rows 5 and 6) has no joined test of its own: a
  sample is written only beside a k2 P1-obs write, so it inherits k2's test
  on the tracker, and with it the wait after a controller restart (PR 2's
  `k2-waits-after-controller-restart`; no switch of mu's). Each admitted
  reading carries k2's stamp (how long the replica had been settled, and
  which settle test passed it), and mu's `learner-write` shows it
  (`TestMuLearner_AMeasuredMuWaitsOutThePostRestartSettleAndLogsItsFirstSample`).
  The fold across pods stays the mean. Three named changes, each behind a
  replay switch, none of which moves a committed golden, so each names the
  unit tests that hold it (`heldBy`). `mu-exact-repeat-is-use`: a repeat
  past the spacing touches the use clock only, so it does not keep the
  episode (write clock) alive, and is judged before the episode, so a frozen
  series past `EpisodeGap` starts no new window with its frozen value. Its
  effect on freshness (agreement clock) is by side: over the derived figure
  it does not refresh it, so the measurement yields to the lower figure;
  under it, or with no derived figure, it still counts as agreement, since a
  real low rate that repeats would otherwise give way to the higher derived
  figure after the 30 min hold. The prefill bound (row 7) keeps the old
  rule, a write. A window installed by a restore, or brought back from the
  stash, marks any choice memo made before it unknown, so the latch rule
  below sees it. `mu-unknown-age-resolves-safe`: over the
  derived figure an unknown age is still stale; under it the low hold counts
  from the window's own last write, never earlier than its newest sample, so
  it holds at least as long as a known sample time would, and with no write
  time it holds. `mu-latch-restores-safe-side`: the latch is restored, not
  persisted (the snapshot is PR 5's); a window on record with no choice memo
  (restored, or brought back) has an unknown latch, which resolves latched
  over the derived figure and unlatched under it, and its first choice is
  evidence. Before the fold was touched, two prefill pods in one spacing pin
  row 7 (`prefill_bound_pin_test.go`): since #395 the bound folds by pod
  name, so the sample is the pods' mean and the bound the max across
  minutes. Mu's `learner-write` band is 5 %, over the 4 % noise that flipped
  the choice, not measured on a run; a source flip (measured, derived,
  served-siblings, neighbour, borrowed-line, lower-bound) always logs, with
  `previousSource`. `throughput-demand-floor` names `muKey` and `muSource`
  of the role's variant nearest the term's mu, and `startEstimateSeconds`
  with `startSource` `measured`, or `none` where no start is measured: the
  floor is given no seed today, so `seed` waits for PR 5 to wire one. The
  closed-loop probe and its fleet simulator land in
  `D/closed_loop_probe_test.go`; its known-failing case is one slow replica
  at 0.55 of its siblings, inside the 2x served bound with no full sibling,
  which seals the fleet over-ordered for good (28 replicas against a need of
  18), and the test asserts the probe sees it. Mu is not on
  `learn.Learners` yet: Forget waits for PRs 4a and 4b, with the SLO and
  throughput lines. The mu counters are re-measured on the flow-control-off
  P/D recording when it lands.
- **PR 4a's decisions.** The ITL line (rows 1 and 13: saturation's and the
  slo analyzer's) reads a replica's settle from the tracker, behind six
  switches each (`itl-…`, and `slo-itl-…` in the slo analyzer):
  `reads-the-settle-tracker` (the move), `restart-under-its-name-settles`
  (a new Ready time under the same Pod name is a restart, `settle.Restarted`,
  with or without a not-Ready row between), `idle-needs-evidence` (a scrape
  that read nothing is not idle), `waits-after-controller-restart`,
  `keeps-the-settle-through-a-missing-row` and `reads-the-finite-pod-mean`
  (a Pod with an Absent rank is no reading). A settle starts at the latest
  of the tracker's `Since`, the last return of load after `idleRows` idle
  rows (`ReturnedAfter`), and the first Ready row after one that was not
  (`ReadyAgain`, now in `settlerow` and shared with k2). The key stamps the
  cycle it first read a start, so a joiner waits from the analyzer's clock
  as before, and takes a start only when it is later than the one it holds:
  the tracker keeps rows for 5 min and its last eight returns, and a start
  that falls back when one ages out is no event (review round 1 found the
  line settling again 5 min after a readiness blip). A start first read on
  the key's own first cycle (a controller restart, or a key made anew by a
  forget or new engine arguments) is a settle only with the wait, and then
  runs from the tracker's start, so a key made anew does not make
  long-settled replicas wait; on the key's own trace (the move off) the
  wait reads as "every replica on the key's first cycle waits", which only
  a replay runs. An analyzer with no engine tracker never waits. The
  acceptance and engine goldens were taken again: their generator stamped
  a Ready time 30 min before each cycle, a restart on every cycle to the
  tracker, and no line of theirs settled; it now keeps a Ready time for the
  day. The move then moves only those two goldens' lines on the cycles
  after a day-long clock jump: 25 hours without a cycle is an absence past
  the tolerance (`settle.Rejoined`), where the line's own trace counted
  absences only by cycles that missed a replica. The wait after a restart
  moves the in-process goldens and `ols-sweep-slo-restart`, where the first
  saturation line comes 75 s later (14:07:17 against 14:06:02) and the
  floor-held decode demand is 32 to 37 % lower for those five cycles. The
  other four switches move no golden and are held by their unit tests.
  `learn.ITL` joins `learn.Learners` (forget neutral: the same windows and
  intercepts, counted once); mu stays off it for 4b. The ITL
  `learner-write` band is 5 % on A or B, and a change of tier always logs.
  The closed-loop probe through a restart, on a decode fleet stepping from
  2.5 to 12 requests/s 15 s after the restart: snapshot off, under need
  for 3m30s against 15 s without the restart, inside the 5 min bound, and
  settles at need; the ITL wait adds nothing to that, k2's and mu's minute
  is what the step waits on. Snapshot on: as without the restart. A 120 s
  stabilization changes nothing in the probe: its fleet orders at need on
  occupancy once nothing queues, so the restart's gap shows on the way up,
  where stabilization does not enter. The probe does not reproduce the
  unmasked case 4.4 names, so no known-failing row was added for it; on
  the recorded restart the dip is 75 s, so a scale-down stabilization
  under about 90 s would let it through (configuration.md says so).
- **Problem:** the analyzers learn about fifteen quantities. Each one accepts
  readings, fits, forgets and logs in its own way. The same defects were fixed
  in one learner after another, because no learner could reuse another's fix.
- **Decision:**
  1. Build the test harness first, and make it deterministic: PR 0 carries the
     clock fix, the presence bits and the recorder. Every later step is judged
     by it.
  2. Share one **settle tracker** (the engine writes it), one **vocabulary**,
     and one **set of observability rules**. Do not share a loop that drives
     the learners, a direction enum, or a robust-fit library.
  3. Each behaviour change ships behind its own switch, on in production, so a
     replay proves that nothing else moved.
- **Shape:** eight PRs (0a, 0b, 1, 2, 3, 4a, 4b, 5). Each says what an operator
  sees and which recording it needs.

## Revision notes

Four reviews each of revisions 2 to 5 (architect, statistician, SRE,
skeptic). Round 1 is summarised at the end. A dash means the row was not a
blocker that round, and nothing in it changed.

| blocker | round 2 | round 3 | round 4 | round 5 (lows only) | where |
| --- | --- | --- | --- | --- | --- |
| The harness ran on two clocks | Every wall-clock read in the analyzers moves onto one injected clock **in PR 0**. Two replays at different wall times must give identical output | `store.go`'s five wall-clock sites are listed, and the engine's calls into the store take the same clock | - | - | [2.2](#22-one-clock) |
| The recorder sees too little | The recorder lives **inside** each analyzer; a test fails when an exported method goes unrecorded | It records at the **input boundary**: each variant's `capacity.Record` and the SLO analyzer's router latency go into the `Analyze` event, the engine's store mutations are events in the same stream, and `*capacity.Store` is in the coverage test. Nested exported calls record once. The test proves recording, not only classification | **The store events record what the engine supplies, not what the store ends up holding.** A live record carries analyzer state (`EffectiveCapacity`, `PriorLiveCapacity`, the carried k1), so `LoadFromScaleTarget` records the parsed `EngineParams`, accelerator, GPU count, `RollingOut` and `now`, and replay runs the one shared store method on its own store. `EvictStale` records the timeout and `now`; replay recomputes what goes. Each `Analyze` result is recorded, and 0b proves a replay at the recording's commit reproduces it byte for byte. `Judging()` is in `start`. The `With...` builders are a coverage bucket of their own | The exported values method (`FileScaleTarget`) is the recorded one; `LoadFromScaleTarget` parses, calls it and records nothing (a parsing-wrapper bucket). An `Analyze` that errs is still recorded, with the error in place of the result | [2.4](#24-the-recorder) |
| A mid-run snapshot is not the analyzer's state | - | A recording starts only at a controller start, and keeps its head at the cap. No mid-run snapshot. The claim that `ExportLearnedState` touches `LastUsed` was false and is removed | The cap is per recording, with a per-volume ceiling that holds two, so the recording after a container restart has room | The volume's `sizeLimit` is that per-volume ceiling plus headroom, not one cap (the deferred row now says so) | [2.4](#24-the-recorder) |
| Goldens without a stated rule | Change behind a switch; off equals the parent, on moves only named outputs | One switch per named change, set on by the constructor. Committed goldens are the all-on run; the off-run compares against the base commit's goldens. Runs: all off, each alone, all on. New recordings land with base goldens first. One golden file per output. The check re-runs on PR edits | The full store-record check runs in the all-off run only; the other runs compare the engine-supplied fields, and the analyzer-written store fields are a golden output. "All off" means this PR's switches. Replay runs every recorded analyzer together, or checks only the fields one analyzer reads. Each package exposes its switches through one exported test hook, a seam in the coverage test | The whole-record check runs only in the fidelity run (replay at the recording's own commit); all off compares the analyzer-written fields through their golden against `$BASE_SHA`, since a reused recording holds an earlier PR's k2. Elsewhere the engine-supplied fields are compared by the store's own rule (`EngineParams` by `ArgsFingerprint` and `Image`, hardware by `sameHardware`) and only where both records share `LearnedFrom`. `EvictStale`'s removed set fails only the fidelity run; elsewhere it is reported | [2.4](#24-the-recorder), [2.5](#25-replay-and-the-golden-rule) |
| Presence and idle | A known bit per gauge; idle is a two-gauge rule | ITL presence is three-state. Idle needs **positive** evidence, so two failed queries do not restart the settle fleet-wide | Idle evidence is role-aware: a prefill pod never has an ITL, so its idle reads the request or prompt-token rate. KV means peak KV. A listed row with unknown gauges is a sighting, and an idle run spans at most 1 min (revision 7: per gap, see [4.3](#43-what-the-tracker-owns)) | Prefill idle reads the request rate for both entering and leaving idle (the prompt-token rate is absent on SGLang). The brownout trade is stated: an idle-and-return inside a long brownout keeps its settle (cost side); PR 1 may end the settle after more than 2 min of unknown rows, and the section 8 brownout case pins the choice | [4.3](#43-what-the-tracker-owns) |
| Property tests | Twelve classes, thresholds enumerated, findings mapped | A known-failures ledger; a mutant per mapping row; estimands declared, so section 5's estimators can pass; time base is an input family | A section-5 estimator that over-prices when one pod is genuinely slow declares where it starts to, and the case is in the ledger, owned by PR 3. Both mu rows (`half-served-max-slow-pod`, `sibling-yield-slow-pod`) pass since #495 and are out of the ledger ([one slow pod, as built](#one-slow-pod-as-built-495)) | - | [2.6](#26-property-tests) |
| Settle tracker ownership | The saturation analyzer owns the one tracker | The **engine** owns it and observes rows at the top of `runAnalyzers`, before the balance tracker and every analyzer. The throughput analyzer takes the view at registration | The engine's observation is a recorded event with the engine's own clock read. A rollout is detected from the rows' `Revision`, not from `capacity.Record.RollingOut`, which is filed later in the cycle | - | [4.2](#42-one-owner), [4.3](#43-what-the-tracker-owns) |
| PR 4a was called neutral | - | Its two behaviour changes are named, each behind a switch, tested by the restart suite with a negative control | 4.4 states what an operator sees during the ITL wait (the floor is silent for about 2.75 min, the SLO side, masked by the 300 s scale-down stabilization). 4a runs the closed-loop probe through a restart, and the missing-row case runs through ITL | The masking holds "as long as the controller is back within about 2 min" | [4.4](#44-the-cost-after-a-controller-restart), [7](#7-migration) |

Also in round 4: the start-time and B last-admitted stamp is due in PR 5 or the
first PR that publishes either age. 6.2 no longer contradicts the deferred
Forget row. The status line says which branch each cited line is from.

Also in round 3: the closed-loop probe is a fleet simulator and moves to PR 3,
its first user. PR 0 splits into 0a and 0b. Counters that are zero by
construction move to the PR that makes them non-zero. PR 1's named change is
proved by a unit test on the balance tracker. Hysteresis covers the k2 key and
two-sided learners. Smaller items are in
[Deferred to the implementing PRs](#deferred-to-the-implementing-prs).

Round 2 also wrote in: `restored: true` is a field, not a source value.
`learner-write` is at most one line per key per cycle, against the last logged
value, with a heartbeat. Per-key series deletion is gone. One start line per
model. Start time has two risky sides. PR 4 is split. Each PR names its
recording.

## Why

The two-namespace runs of October 2026 (issue #270, its rerun, E1 and E2) and
the reviews of #273 and #282 found six kinds of defect:

| defect class | where it hit |
| --- | --- |
| **A transient admitted as a reading.** A replica that just joined, the first minute of a load step, a slow fill. | ITL intercept learned from a ramp (#273). k2 taken at 8 % KV behind a 160-request step (#282). k2 recorded mid-fill on long outputs (#282). A replica just Ready counted as idle room (#282). A filling replica's token rate taken as mu, 11 % high (#282 round 4). |
| **The first sample adopted raw.** | A fresh pod's 0.0995 req/s became the bucket's mu (E1, #282). k2 prices its first admitted reading at once. The start-time estimator's first measurement replaces the 70 s default. |
| **A stale value never refreshed.** | A measured mu outranked a derived one for the rest of a run (#282). The 6 ms default intercept stayed on every multi-replica fleet (#273). |
| **A missing reading read as a value.** | k == 0 meant "not collected" and was traced as zero load (#273). A NaN KV read as 0, and so as idle (#283 review). |
| **One reading dominates.** | Least squares pulled by three joined-pod readings out of sixteen (#273: now dropped by state, then a k-weighted mean). The saturated throughput sample was the hottest replica (#282: now the mean across replicas, bounded by the served rate). One slow replica set the fleet's mu (#282 round 4: now yields to its full siblings). |
| **A hard edge with no hysteresis.** The most frequent class. | The k2 key boundary swung capacity 39 % at constant load. Demand fell 48 % for a 2 % rise in lambda. Mu flipped between measured and derived 11 times in 20 cycles under 4 % noise. `backlogGrew` swung demand 1.54x on one request (fixed on #282 by e7d9f0ff1). |

The joined-replica test now exists four times, with four sets of rules (see
[Settled replicas](#4-settled-replicas)): the case for one settle tracker. The
last class no shared code fixes. Only a test that perturbs the inputs finds it,
so the harness comes first.

**Errors are asymmetric.** A capacity that is too high breaks the SLO. One that
is too low costs money. [The k2 design](../plans/analyzers/k2-capacity-model.md)
draws its rule from that: nothing outside admission raises k2. No floor, no
age-out, no reset. That rule stays.

**What this answers for an operator,** after PR 5:

| question | answered by |
| --- | --- |
| Why did a light model run at 0.95 utilisation? | `throughput-demand-floor` names `muKey` and `muSource` (PR 3); mu's `learner-write` shows the first sample, its pod and its joined age. The `[1m]` peak after a swap belongs to no learner and stays open |
| Why did a fleet hold 3 replicas? | `heldAtFleet`/`heldWhy` (exist), `muSource`, `startEstimateSeconds` and `startSource` (PR 3), `k2-decision`'s P-code |
| Why did the second GPU transfer wait? | **Not answered here.** The share planner's holds are out of scope; they belong to the utilization-share work (#272 and its follow-ups) |

## 1. Inventory

D = `internal/engines/analyzers/saturation`. "Persisted" means it is in the
learned-state snapshot (`D/learned_snapshot.go`, ConfigMap `wva-learned-state`,
off by default).

Since #298 most admission and settle figures below are operator policy keys:
`analyzerTuning` and `loadImbalance` in a scaling policy
(`internal/config/analyzer_tuning.go`, documented in
[scaling policy](../reference/scaling-policy.md#advanced-tuning-analyzertuning-and-loadimbalance)).
The numbers in this document are their defaults.

| # | quantity | where | admission | fit | memory and forgetting | persisted |
| - | --- | --- | --- | --- | --- | --- |
| 1 | ITL line `ITL(k) = A·k + B`, saturation analyzer | `internal/signals/itl/`, wired in `D/analyze.go` | Ready, not a bridge, finite ITL > 0, k > 0. k inside the policy KV band (`analyzerTuning.itlBand`), or above it when the replica's batch is at its sequence cap (`max_num_seqs`, lowered by `--max-num-active-seqs`, #331) with preemption at most `itlBand.capMaxPreemptionRate` (`internal/signals/itl/admission.go`, #293). Each reading goes to the main window, the main and steady windows, or nowhere (see [Admission shapes](#33-admission-shapes)). A reading taken while its replica settles (joined, or load returned after idle, under `steadyGate.steadyFor`, 2 min) is stamped and kept out of the pinned fit | OLS on the steady window at ≥ 10 rows spread ≥ 0.30. Otherwise A = k-weighted mean of `ITL − B` over unstamped readings, B pinned. Fewer than `steadyGate.minSettledSamples` (3) such readings, or fewer than 10 in the window at all (light or overloaded load ages them out): the last A is held (`SlopeHeld`), up to `BucketRetention` after its fit and under the same args; a held line is not republished to row 2. A < 0 refused | Main 20 rows / 30 min. Steady 40 rows, carried `steadyGate.carry` (5 min) past the main window. Swept 24 h unseen. Args change stashes B | B, its stash, and the held slope (`ITLSlopes`, #329): without it a restart under light load had no line and sized by KV occupancy alone (issue #316) |
| 2 | Shared and borrowed ITL line | `D/analyzer.go` `publishLine`/`borrowLine` | `EnableLearnedStateReuse` (off), same fingerprint | none | Ages on last write. May hold a fleet, never order one | yes |
| 3 | k2, KV at saturation | `D/replica_capacity.go` `k2Chain`, `D/k2_settle.go`, `D/keys.go` | Queue ≥ threshold; downstream not saturated; not a bridge; not mid-rollout; under the physical ceiling. Joined under 1 min, counted from the later of the Pod's Ready time and the first sight of that Ready time (`k2Joined`, `D/settle_view.go`, on the settle tracker; #288): refused. When queue × footprint exceeds what the busiest rank holds, also: rate ≥ `k2Settle.rateFraction` (0.7) of mu on record **and** residency ≥ 0.7 of k2 on record, **or** residency within ±`k2Settle.maxRise` (25 %) of the reading 1 min back with the queue standing on every cycle. On decode of a P/D model, the queue k2 reads is the engine's own deferred queue, rewritten before k2 reads it (`D/pd_decode_queue.go`, #294, #306) | Mean of a 10-sample window. Admitted readings raise it as well as lower it. The first admitted reading of an empty window waits for a second, from a later cycle (`D/k2_confirm.go`, #392): it is held aside, labelled `P1-obs-unconfirmed`, and its cycle is priced at most at the reading. A window that holds readings, or was restored, takes each at once | Key: the fleet's output length for decode and aggregated, its bucket held by a band (smoothed over 10 min, 3 % past the boundary, or 20 % past it for 2 min; #396); prefill's own average. 24 h write gap starts a new episode. Evicted 7 d after last read | yes, with args tags and stash |
| 4 | Borrowed k2 (lower bound) | `D/k2_borrow.go` | Only when lower than the variant's own k2. KV total within 3 % | none | Held 30 min, then relaxed **upward** over 6 h of serving time. Re-arms on an args change or a figure ≥ 20 % lower | yes |
| 5 | Measured mu | `D/throughput_floor.go`, `D/throughput_admission.go` | Only on a P1-obs k2 write, so it inherits k2's joined test. RequestRate > 0. Not filling: generated tokens at most `admission.maxTokensOverCompletions` (1.15x) completions × output length. Not under half the highest rate a Ready replica of the variant served, its own included (`servedBounds`). Not under `muChoice.minMeasuredOfDerived` (0.5) of derived, unless the same pod read that low on every saturated cycle for 2 min | One sample a minute. Within the minute a reading rewrites the last sample: max per pod, **mean** across pods (`meanOf`, `D/throughput_floor.go`, #282): a fleet serves the sum of its replicas' rates | See [the mu choice](#the-mu-choice) below. Evicted 7 d after last read | window yes, choice no |
| 6 | Derived mu | `D/mu_from_itl.go` `deriveMu` | none: a formula on #1 per replica | none | As fresh as the line | no |
| 7 | Prefill throughput bound | `D/prefill_bound.go` | `EnablePrefillThroughputBound`. Saturated, serving rows. No joined test, no rate check, no band | Window max. It writes through #5's fold with pod `""`, so it kept its max only because every prefill pod folds under one empty key | **Only rises.** The prior max is read before the write and put back with `RaiseLast`, so it survives the 24 h episode reset. Touched on every read, so never ages while pricing. Cleared only by an args change or a forget | with #5 |
| 8 | Fleet shape and shape-change hold | `D/shape_change.go`, `D/fleet_shape.go` | Tolerance band on the router's prompt length | Rate-weighted mean | Hold up to 5 min | no |
| 9 | Decode-saturation memory | `D/roles.go` `roleSaturated` | One full decode pod gates prefill learning (the #282 fix that changed this was reverted, 7ab00fa8c). Bridges count | none | 1 min | no |
| 10 | Idle room | `D/idle_room.go` | Decode only. Idle on every reading for 2 min, no gap > 1 min, not a bridge. A run starts under the idle line and goes on under 1.4 times it (#396) | none | Run resets on a non-idle reading | no |
| 11 | Floor arrival rate, trust and backlog memory | `D/throughput_floor.go`, `internal/signals/floor/` | none | Blend from 0.5 to 0.8 of resident demand. A queue counts as grown past a noise band over a minute. An engine's own queue is capped at what it drains (mu × T) **only while it stands**; while the role's backlog grows it is charged in full (round 4) | Last cycle's backlog per role; `backlogGrew` remembers 1 min, so the full charge outlives growth by up to a minute (an over-order) | no |
| 12 | Replica start time | `D/start_est.go` | Each pod once, not a bridge, 0–3600 s. A sample over 3x the estimate is refused until 3 distinct Pods have reported one (`startConfirmPods`, 5b), so one Pod repeating never opens the guard | EWMA α 0.3, and the first sample replaces the prior. The prior is the ScaledObject's `replicaStartSeconds` seed (5c, [trigger reference](../reference/scaledobject-trigger.md)), else the 70 s default (`DefaultReplicaStartSeconds`). With a seed and no sample, sizing reads the seed; with neither, the floor does not project. Under 3 counted Pods the published figure blends the prior with their mean, held in half to twice the seed, and sizing reads the largest of the blend, the mean and the EWMA | Counted Pods' Ready times are persisted (`StartCounted`, 5d): after a restart a Pod with the same Ready time is not folded again, and any other is. The floor reads it as a min per role, share pacing as a max | yes |
| 13 | SLO analyzer line | `internal/engines/analyzers/slo/` | Its own ITL tracker, same rules as #1 | Same as #1 | 24 h unseen | no |
| 14 | Share need history | `internal/engines/allocation/utilization_share_ledger.go` `RecordNeeds`, `PeakNeed`, `PeakKnown` | A peak needs two readings in a row. After a restart a full swing window before a peak is known | Mean over the swing window. Step detector `ShareStepSpread` 0.25 | Trimmed to the window | yes, `wva-utilization-share-history` |
| 15 | Share release time | same file, `MeasuredRelease` | Completed releases | 90th percentile of recent releases, needs ≥ 3 | Recent releases only | no |
| 16 | ITL line, throughput analyzer | `internal/engines/analyzers/throughput/analyzer.go` `resolveITLModel` | The policy KV band only (`ITLBand()`, the same `analyzerTuning.itlBand` keys as row 1, `throughput/analyzer.go:270-275`). No cap admission, no steady gate | OLS, else B pinned to `lastFittedB` | Per variant | no |

#### The mu choice

A measured mu and a derived mu are both offered every cycle, and
`measuredYieldsToDerived` picks one. It keeps the measured window either way.

| rule | when |
| --- | --- |
| Yields to derived **at once** | the measurement is under derived and a Ready replica (its own included) serves more than `muChoice.maxRatio` (2x) it |
| Yields to the full siblings (`siblingYield`, #282) | the measurement is under `admission.siblingMuTolerance` (0.8) of the lower median served by every Ready replica with a full cache, queued ones included. A yield opens only when at least one of them is unqueued. Logged as `saturated-throughput-yields-to-siblings`, bucket `served-siblings`. Remembered in `muChoice` until the measurement comes within 0.8 or the window starts over |
| Yields when stale | freshness is the last saturated reading that **agreed** with it within 2x. Above derived: after `muChoice.freshFor` (5 min). Below derived: after `muChoice.lowHoldFor` (30 min) |
| Latch | out at `muChoice.maxRatio` (2.0), back at `muChoice.returnRatio` (1.6) |

**Restored to the safe side (PR 3):** the latch is not persisted. Before PR
3, after a restart a measured mu between 1.6x and 2x **above** derived came
back unlatched and priced from the **higher** measured figure, the SLO side.
A latch with no evidence behind it now restores latched over the derived
figure and unlatched under it ([3.5](#35-hysteresis);
`mu-latch-restores-safe-side`). A one-replica variant with no line has
nothing to yield to.

Also learned and not tabled: the hot-rank hysteresis memo and the share swing,
which stabilize the controller's own decisions rather than learn from replicas.

The snapshot carries #1 (B, its stash and the held slope `ITLSlopes`),
#2–#5 and #12, plus the accelerators, the forgotten set, both stashes, and
`K2BorrowedSince` and `PriorK2`, which are written beside `LowerBounds` so a
rollback build still reads them. A restore never makes a figure younger, and an
unknown age resolves to the safe side ([3.4](#34-which-side-breaks-the-slo)).

Row 16 has none of #273's fixes (steady gate, settling stamp, k > 0 guard) and
none of row 1's cap admission. It reads the same policy band as row 1. It
moves in PR 4b.

## 2. Harness first (PR 0)

### 2.1 What the recorded runs can and cannot replay

The E1, E2 and #270-rerun logs were the planned fixtures. They cannot carry
most of the migration:

- **No instant KV, no ReadySince, no warm-pool flag** on any line. The
  instant-KV paths and the Ready-time rule never run in replay.
- **k is the one-minute peak** in 973 of 1257 reconstructed E1 rows. Only 39
  are raw.
- **No per-replica generation or completion rate.** No measured mu is ever
  written, so a mu replay compares nothing.
- **OLS is never reached.** B is the 6 ms default on every cycle, so "same A
  and B" says nothing about B, its stash or the carry.
- E2's replay CSV holds one namespace. The rerun has none. `D/testdata/k2-fill/`
  is simulated, and `TestE1Replay_*` is about 20 hand-copied rows.

What they can test: the ITL pinned tier, on reconstructed k.

No recorded run covers: **P/D**, **whole-node 8-GPU and DP ranks**,
**multi-node LWS**, **SGLang**, **EPP flow control off** (where P1-obs fires
most, so k2's main path), **shape changes**, **controller restarts and the
snapshot**, **warm-pool bridges**, and **borrowed lines** (reuse on). All three
runs are one model, decode only, one shape, 1 GPU per replica.

PR 0 has five parts: one clock, presence bits, the recorder, replay with
goldens, and property tests. It ships as two PRs. **0a** is the clock and the
presence bits, with their unit tests and negative controls. **0b** is the
recorder, replay, goldens, the CI check, the property tests and their ledger,
and the first recording, taken from a build with 0a merged. 0a changes no
output except as [2.2](#22-one-clock) states; 0b changes nothing in
production.

### 2.2 One clock

A replay drives `a.now()` from the recording. Every other read of wall time
then stamps replay time, so goldens differ from run to run, and an episode
reset or an eviction depends on when the replay ran. Each site below takes
`now` from its caller, or from the one clock injected into the analyzer.

| site | what reads wall time today |
| --- | --- |
| `internal/signals/capacity/rolling_average.go` (the k2 and mu windows) | `NewRollingAverage`, `Add`, `Observe`, `Touch` (the use clock, also called by the prefill bound), `Stale`, `WriteGapExceeds`, `WriteAge`. Each gains an `...At` form |
| `internal/signals/capacity/store.go` (the capacity store) | five sites: `LearnedAt` in `Update` (`:121`) and in `LoadFromScaleTarget` (`:289`); `IsStale` (`:187`); the `PriorLiveAt` window in `LoadFromScaleTarget` (`:268`); `EvictStale` (`:339`). Each takes `now` from its caller |
| `internal/engines/steadystate/engine_v2.go` (the engine's store calls) | `LoadFromScaleTarget` (`:75`) and `EvictStale` (`:606`) pass the same injected clock the analyzers use; the engine holds it |
| `D/analyze.go` | `AnalyzedAt` (also excluded from goldens) |
| `D/analyzer.go` | the accelerator memo touch; the pending-restore sweep `alive(a.now(), time.Now())` and `pruneWindows(time.Now())` |
| `D/roles.go` | the accelerator memo |
| `D/learned_snapshot.go` | the snapshot writer and restore stamps |
| `internal/engines/analyzers/throughput/analyzer.go` | `now := time.Now()` inline in `Analyze`, no seam at all |
| `internal/engines/analyzers/slo/` | already has a `now` field; wired to the same clock |

The snapshot compares persisted wall stamps on restore, so in production the
injected clock **is** wall time. In replay it is the recorded clock.

**Acceptance:**

- The same recording replayed twice, with the wall clock 30 days apart, gives
  byte-identical outputs.
- A test fails on any `time.Now()` in these packages outside the injected clock.
- Negative control: a fake clock that jumps past `EpisodeGap` resets the k2
  window. It fails on the parent, where the window stamps wall time.
- One clock reading per cycle stamps every `Add` of that cycle at the same
  instant, where today each call reads its own `time.Now()`. A comparison at
  the one-minute mu spacing or the 1 min joined lookback can move by one
  cycle. That is 0a's only output change; a unit test pins it at the
  boundary.

Tests can already age a window with `ObservedAt` (`sweep_keying_test.go`,
`eviction_regression_test.go`). What they cannot do is age it **on the same
fake clock** that `a.now()` uses. That is the defect.

### 2.3 Presence bits

Three bits exist (`internal/domain/replica_metrics.go`):
`KvUsageInstantKnown`, `CapacityQueueKnown` (#294, the engine's own queue)
and `PreemptionRateKnown` (#293). The collector turns NaN, Inf and absent
series into 0 (`collector/extract.go`). PR 0a reuses the three bits and adds a
`...Known` bit beside each other gauge a learner reads: peak KV, ITL
(`AvgITL`), request rate, generation-token rate, prompt-token rate, and
`RunningRequests`. The engine-queue bit is `CapacityQueueKnown`; 0a does not
add a second one. A bit is true when the series was present, finite and inside
its block's range. A read zero is known (no work), except for ITL: ITL is
`ITLFinite` only for a finite value above 0; a NaN is `ITLEmpty`; ±Inf, a
value at or below 0, or a failed query is `ITLAbsent`. A not-Ready Pod clears
only its ITL bits. Ranks merge by AND; for ITL, Finite needs one Finite rank
and none Absent; Empty needs every rank Empty and none that generated tokens.
The Pod's mean is generation-weighted over its Finite ranks only (#341 4a's
named change, made in the collector ahead of 4a): an Empty rank that generated
tokens (max_tokens=1) has no interval, so it neither dilutes the mean nor
blocks Finite. The per-pod query divides by inter-token intervals instead
(generated tokens less one per request). The merge does not weigh by
intervals: that would need tokens less the request rate, which counts finished
requests and bursts on a drain, so a busy rank would drop out of the mean for a
cycle, and a slow rank dropping out reads ITL low. Token weights are smooth.
Where they differ from the per-interval mean they favour the rank serving
short requests, usually the slower one, so the Pod reads higher: the safe side,
though not a guarantee (a hot rank of long requests reads slightly low). The k
paired with
that ITL is still capacity-weighted over every rank, Empty ones included, so a
pod with a busy Empty rank pairs the Finite ranks' ITL with a lower k. That
fits ITL(k) high and mu low, the side that over-orders; it is left as is and
must not be "fixed" by putting the Empty ranks back into the ITL mean. A
prefill-role Pod reads Empty permanently, so readers gate by role.

`RunningRequestsKnown` is a prerequisite of #277 (reading
`num_requests_running` as resident load). `RunningRequests` is "zero when the
metric is unavailable" today, so without the bit a missing series would read
as 0 resident. #277 reads it as that: where the running requests are read, the derived mu
prices a resident request at the KV they hold, between the hit rate's discount
and none; a row without the bit is no reading, and with no reading the
discount stands (behind the switch `kv-per-request-hits-resident`).

**ITL is three-state:** finite, empty window, or absent. ITL is
`rate(sum[1m]) / rate(count[1m])`
(`internal/collector/registration/queueing_model.go`), so an idle replica
reads 0/0 = NaN with the series present ("NaN is the ordinary case here",
`recent_window_extract_test.go`). A failed or timed-out query reads absent. A
single bool would merge the idle signal with a failed query, so ITL carries
`AvgITLKnown` and `AvgITLEmpty`.

- The recorder writes the bits. It never encodes "missing" as NaN:
  `encoding/json` refuses NaN and would drop the whole line.
- 0a's test: the collector sets each new bit on a present, NaN, Inf and absent
  series, and ITL's three states on a live, an empty and a failed query. The
  three existing bits keep their tests.
- Negative control, in PR 1 (the first PR with a tracker to read the bits):
  #273's A4 case (k == 0 unsettles a replica) and 273-r3 #2 (a missed scrape
  restarts a settle) fail on the parent.
- Until a learner moves onto the tracker it ignores the bits, so this part
  changes no output.

### 2.4 The recorder

A wrapper around `domain.Analyzer` is ruled out. The engine reaches six
capabilities by type assertion (`variantForgetter`, `staleHistoryEvictor`,
`learnedStater`, `acceleratorStabiliser`, `presenceNoter`, `sloStateKeeper`).
A wrapper hides them: with the flag on, forget, eviction, snapshot restore and
export, and accelerator stabilisation would silently stop.

So the recorder is a field inside each analyzer (`a.rec`, nil when off). The
saturation, SLO and throughput analyzers each get one, and so does the shared
`*capacity.Store`. All write to one sink with one `seq`.

The recorder owns its schema. 0b lands before #345 step 1 creates
`AnalyzerInput`, so the `Analyze` event mirrors the input shape that
engine-structure stage 3 plans: the input, each variant's capacity record,
`VariantMetadata`, and the merged policy including `analyzerTuning`.
Recordings survive step 1, and step 1's "zero golden change" is measured on
them.
The store's scale-target split is the seam stage 3 moves into the input
builder.

**It records at the input boundary,** not only at the analyzer's methods. An
input is anything an analyzer reads that it did not compute. Two inputs reach
the analyzers without passing an analyzer method:

- **The capacity store.** `NewSaturationAnalyzer(capacityStore)` and
  `slo.New(replicaCollector, capacityStore)` share one `*capacity.Store`
  (`steadystate/engine.go:334-335, 404`). The engine writes it before every
  `Analyze`: `LoadFromScaleTarget` per variant (`engine_v2.go:75`) files
  `EngineParams`, `NumGpuBlocks`, `EffectiveCapacity`, `RollingOut` and
  `PriorLiveCapacity`, and `EvictStale` (`:606`) sweeps it. Pricing reads it:
  `deriveMu` and `MaxNumSeqs`, the line fingerprint that keys sharing and
  borrowing, the borrow compatibility test, the per-replica capacity
  fallback, restore install (`installRestored` leaves a restore pending while
  the record has no `EngineParams`), and the args tags that decide a stash.
  Replayed without it, every `engineParamsFor` returns nil: no fingerprint, no
  reuse, no stash, every restore pending, and a rollout admitted that the run
  refused. The goldens would describe a controller that never ran.
- **Router latency.** The SLO analyzer calls
  `a.router.CollectRouterLatency(...)` inside `Analyze`
  (`slo/analyzer.go:382`), a Prometheus query.

So the store's recorder logs each **engine-side** mutation in the same
stream, as **what the engine supplied**, never as the record the store ended
up holding. For a live variant the filed record is partly analyzer state: with
the same arguments and hardware the live record is kept and only
`EngineParams` and `RollingOut` are swapped (`store.go:245-255`); on an
arguments change `PriorLiveCapacity` and `PriorLiveAt` come from the run's
last `EffectiveCapacity` and `LearnedAt` (`:264-266`); and the carried k1 is
the run's measured `TotalKvCapacityTokens` (`:276-278`). All of these were
written by the saturation analyzer's `Update` (`replica_capacity.go:371`). A
replay that copied them back would price every stored-figure reader (the
zero-replica estimate, the sibling clamp, the compatible lookup) from the
run's k2, not its own, and a PR that moves k2 would report those outputs as
unmoved.

- **`LoadFromScaleTarget`** records the parsed `EngineParams`, the
  accelerator, the GPU count, `RollingOut` and `now`. The function splits
  into parsing the scale target and one store method that takes those values;
  production and replay call the same method, and replay calls it on its own
  store, so the kept live record and `PriorLiveCapacity` come from the
  replay's own `Update`s. Replay lives outside `capacity`, so that method is
  exported (for example `FileScaleTarget(namespace, model, variant,
  accelerator, gpuCount, params, rolling, now)`), and **it** is the recorded
  method. `LoadFromScaleTarget` parses and then calls it, records nothing of
  its own, and the coverage test lists it as a parsing wrapper. The
  accelerator in this event is itself an analyzer answer (the engine passes
  `StableAcceleratorFor`'s result, `engine_v2.go:72-76`), so replay applies
  the recorded value and does not recompute it.
- **`EvictStale`** records the timeout and `now`. Replay recomputes what it
  removes; what the run removed is recorded as a check, not applied. A
  mismatch fails only the fidelity run; in every other run it is reported,
  because `LearnedAt` follows the replay's own `Update`s.
- The store's other mutators, `Update` (`replica_capacity.go:371`) and
  `Delete` (`forget_variant.go:76`), are called by the saturation analyzer,
  so replay reproduces them from the analyzer events; recording them too
  would apply them twice.

The `Analyze` event carries each variant's `capacity.Record` as read at the
top of `Analyze`, and the SLO event carries the router latency. Replay checks
its own store against those records before each call:

| run | store check before each `Analyze` |
| --- | --- |
| fidelity (0b's acceptance: replay at the recording's own commit) | the whole record. A mismatch fails the replay |
| every replay run (all off, each alone, all on) | the engine-supplied fields, by the store method's own rule, not by field equality: `EngineParams` by `ArgsFingerprint` plus `Image`, the hardware (accelerator, GPU count) by `sameHardware`, and `RollingOut` exactly; and only where the replay's record and the recorded one share `LearnedFrom`. A mismatch fails the replay |

The analyzer-written fields (`EffectiveCapacity`, `TotalKvCapacityTokens`,
`PriorLiveCapacity`, `PriorLiveAt`, `LearnedFrom`) are a golden output, one
file under [2.5](#25-replay-and-the-golden-rule), so a named k2 change shows
where it reaches the store. All off compares them through that golden file
against `$BASE_SHA`, as it does every other output, and not against the
recording: a recording is reused across PRs, so it holds the fields as the
recording's commit computed them, while all off reproduces the base, which
already carries every earlier PR's switches (PR 3's all off reproduces PR 2's
k2).

Why the engine-supplied check is scoped: on a live record with the same
arguments and hardware, `LoadFromScaleTarget` keeps the record
(`store.go:250-258`). The accelerator and GPU count are then the ones the
saturation analyzer's `Update` filed (`replica_capacity.go:371-374`), and
`EngineParams` is carried forward, swapped only on an image or `RollingOut`
change, so it can differ from this cycle's parse in fields the fingerprint
ignores. A switch that changes when, or whether, a variant first files live
moves these fields with no defect present. Where `LearnedFrom` differs, they
are covered by the golden output instead. Latency is served from the
recording; replay never queries Prometheus.

**A replay runs every recorded analyzer together,** in `seq` order. The SLO
analyzer's `Analyze` runs after the saturation analyzer's `Update` has
rewritten the record, so an SLO-only replay has nothing to reproduce those
writes. A replay of one analyzer alone therefore checks only the record fields
that analyzer reads, listed beside its recorder (the SLO analyzer reads only
`EngineParams`, `slo/analyzer.go:433`).

**Each result is recorded.** The `Analyze` event also records its
`AnalyzerResult` (minus `AnalyzedAt`), and each store event what it returned.
An `Analyze` event is written whether or not it errs, with the error in place
of the result: `runV2AnalysisOnly` returns on a saturation error
(`engine_v2.go:101-104`) after the engine has observed the tracker, and that
cycle's settle observation needs its rows.
Recording inputs only, a replay that misses an input still gives stable
goldens; the recorded result is what catches it. 0b's acceptance: a replay at
the recording's own commit reproduces every recorded result byte for byte. A
recording that fails this is not committed as a fixture.

**Nested calls record once.** `Analyze` calls `a.StableAcceleratorFor` per
variant and per replica, and the throughput `Analyze` calls its own
`Observe` (`throughput/analyzer.go:254`). Each exported method records and
then calls an unexported implementation; internal callers use the
implementation. A replay therefore applies each event once.

**What it records,** one recording per controller start, all models and
analyzers in call order (borrowed lines cross models), each event with `seq`
and `now`:

| event | content |
| --- | --- |
| `start` (first line) | cold start, or the restored document as `RestoreLearnedState` received it; the store's records; the configuration the `With...` builders set (`WithHorizons`, `WithOptimizationInterval`, `WithStateTimeout`); whether the SLO analyzer had a router (`Judging()`), so its replay builds a recorded-latency router exactly when the run had one. Replay refuses a recording without it |
| `Analyze` | the full `AnalyzerInput`: every `ReplicaMetrics` field with its presence bits (instant and peak KV, ReadySince, `Revision`, the warm-pool flag, per-replica generation, completion and prompt rates, queue, ITL, KV capacity); per-variant GPU count, role, accelerator (`VariantStates`); each variant's `capacity.Record`; the scheduler queue; the arrival rate; the policy, with its merged `analyzerTuning` and `loadImbalance`, every cycle; for the SLO analyzer, the router latency; and the `AnalyzerResult` it returned, minus `AnalyzedAt`, or its error. Written whether or not it errs. The rows are recorded **raw**, as `Analyze` received them: the P/D decode deferred-queue rewrite (`decodeCapacityView`, `D/pd_decode_queue.go:63-71`, `QueueLength` rewritten at 69) changes `MaxRankQueueLength` and `QueueLength` inside the analyzer, before k2 reads them, and is skipped when `!CapacityQueueKnown` or at the batch cap, so replay must run it again |
| store mutations, engine-side | the exported values method behind `LoadFromScaleTarget` (the parsed `EngineParams`, accelerator, GPU count, `RollingOut`, `now`), `EvictStale` (the timeout and `now`; what it removed, as a check that fails only the fidelity run) |
| settle observation, engine-side (from PR 1) | the model and the engine's own `now`, read where the engine observes the tracker; the rows are the next `Analyze` event's ([4.2](#42-one-owner)) |
| `RestoreLearnedState`, `ExportLearnedState` | the document in or out |
| `ForgetVariant`, `ForgetLearnedState`; SLO `ForgetVariant` (#431) | the variant, key and cause (annotation, model gone) |
| `EvictStaleHistory`, `NoteVariantsPresent`, SLO `BeginCycle`, `EvictStale`, `NoteVariantsPresent`, throughput `Observe` (engine-called only) | the arguments |
| `HoldLowerBounds` | the flag |
| `StableAcceleratorFor` | the arguments and the answer, when the engine calls it (it moves the memo) |
| `TakeRestoreOutcomes` | that it ran: it drains a map no output reads |
| borrowed lines | `publishLine` and `borrowLine` across models, as they happen |

A note on `LastUsed`: `ExportLearnedState` copies under `a.mu` and touches
nothing (`learned_snapshot.go:467`). The only `LastUsed = time.Now()` is in
`notePendingPresent` (`:1392`), reached from `NoteVariantsPresent`, which is
recorded. Revision 3 said otherwise; it was wrong.

**Coverage test,** in two parts, run over `*SaturationAnalyzer`,
`*slo.Analyzer`, `*ThroughputAnalyzer` and `*capacity.Store`:

1. By reflection, the exported method set equals the union of six reviewed
   buckets, and a new method fails until it is classified:
   - the recorded methods (for the store, the exported values method and
     `EvictStale`);
   - parsing wrappers, which call a recorded method and record nothing of
     their own (`LoadFromScaleTarget`);
   - pure readers (`Name`, `Horizons`, `ForgottenToken`, `BorrowTickGap`,
     `StateTimeout`, `Judging`, `Get`, `FindCompatible`, `FindClampBound`,
     `IsStale`, `VariantState`);
   - configuration builders, captured in `start` (`WithHorizons`,
     `WithOptimizationInterval`, `WithStateTimeout`);
   - for the store, the mutators named as analyzer-internal with the analyzer
     event that reproduces each (`Update`, `Delete`);
   - test seams: the one switch hook per package
     ([2.5](#25-replay-and-the-golden-rule)). A test asserts no production
     path calls it.
2. A table test calls each recorded method once with a recorder and asserts
   exactly one event of its kind. One `Analyze` emits exactly one `Analyze`
   event, whatever it calls inside.

A third test asserts the engine's six type assertions still hold with
recording on.

**A recording starts only at a controller start,** cold or restored. There is
no mid-run snapshot: the learned-state document is not the analyzer's state.
It omits `decodeSaturatedAt`, the hot-rank memo, `fleetShape`, the ITL
windows, `itlArgs`, `throughputHeld`, `boundsHeld`, `startSeenPods`,
`startOutliers`, the mu-choice latch and the settle tracker, and restoring it
stages windows rather than installing them. A whole-state debug dump (a
field-coverage test over each analyzer's struct and a dump, load, dump
round trip) would allow one, and is a PR of its own if a recording ever needs
to start mid-run. None of the PRs here needs it.

**Where recordings go:**

| item | rule |
| --- | --- |
| default | off |
| volume | a separate `emptyDir` named by a recorder overlay (to be added under `config/`). Never `/tmp`, the controller's 64Mi `emptyDir`: the kubelet evicts the pod when it fills |
| size | about 1–1.5 KB per replica per cycle. At a 15 s cycle: 8 replicas fill 64Mi in about 33 h, 50 replicas in about 6 h; 100 replicas write about 0.5 MB/min |
| cap | a byte cap **per recording** (default 256Mi), and a per-volume ceiling that holds two recordings at the cap. Files are named by start time. At the cap the recorder keeps the head, stops writing, and counts and logs the lines it drops. A controller restart starts a new recording; a container restart (crash, OOM) keeps the `emptyDir`, so the recording after it has room of its own (PR 5's restart fixture is that recording). At the volume ceiling the oldest closed recording is deleted first. No recording starts mid-run |
| external sink | instead of the volume, a file a log shipper reads. Never the controller log: the kubelet rotates it, and it interleaves |
| collection | `testing.md` says how to copy the files out (`kubectl cp`) before teardown; the benchmark harness does it on every exit path. Status: built (`hack/benchmark/learner_recordings.sh`, `hack/check-learner-recordings.sh`): `benchmark-run` copies during the run and at its end and files them under `learner-recordings/`; `benchmark-teardown`, `benchmark-restart-controller`, `benchmark-run-all` and the two-model arms copy before the Pod goes; an interrupted run's copy is finished by the teardown |
| content | inputs, plus each `Analyze` result as a fidelity check. No prompts, payloads or headers: the collector never sees them |
| before commit | an export step maps namespace, model, pod, variant and `Metadata` values to stable pseudonyms, drops cluster names, and gzips. A committed fixture stays under a stated size limit |

### 2.5 Replay and the golden rule

A replay drives the analyzers from a recording and writes its outputs to golden
CSV committed beside the fixture, **one file per output**, so a file diff
names the output.

**What it compares:** analyzer outputs only. Role demand; variant capacities
with their sources and keys; priced figures; per-key window contents; the
learner reports. Required and spare capacity come from the engine's threshold
post-step, not from `Analyze`, so an analyzer replay does not compare them.
`AnalyzedAt` is excluded. 0b records the analyzer boundary only. An
engine-level golden layer (the composite, its holds and their reasons,
`ReleaseBound`/`OtherFrames`, the warm-pool supply and pressure, the
optimizer's decisions) is #345's and outside this plan: #345 step 0 adds its
own recorder events (composite, optimizer and warm-pool inputs) to 0b's sink
and builds its goldens on them. Status: the composite and the warm pool's
figures are recorded and replayed (`engine.Composite`, the `engine` golden;
[testing](../developer-guide/testing.md#the-engine-layer)); the optimizer's
decisions get their own PR, before #345 step 3.

**The rule.**

- **One switch per named change.** Each is an unexported field that the
  constructor sets **on**, so production runs the new behaviour in the PR that
  adds it. It is a test seam, never a feature flag: no option, flag or
  ConfigMap key reaches it.
- **One exported hook per package.** The replay drives the saturation, SLO
  and throughput analyzers and the store together, four packages, and a test
  in one package cannot set another's unexported field. So each package
  exposes its switches to the replay through one exported test hook (for
  example `SetSwitchesForReplay(map[string]bool)`). The coverage test
  ([2.4](#24-the-recorder)) lists the hook as a seam, and a test asserts no
  production path calls it.
- **A switch is not a policy key.** Switches are internal, exist for replay,
  and are deleted with their seam. Policy keys (`analyzerTuning`,
  `loadImbalance`) are for operators and are documented in
  [scaling policy](../reference/scaling-policy.md). A switch never reads a
  policy key, and no policy key turns a switch off. A PR that moves a
  threshold an operator key sets changes the key's default or meaning, not a
  constant: see [4.3](#43-what-the-tracker-owns).
- **The committed goldens are the all-on run.**
- **The replay runs all off, each switch on alone, and all on.** "All off"
  means **this PR's** switches off. A seam an earlier PR left in the package
  stays at its constructor default (on), because the base goldens are the
  base's all-on run.

| run | must hold |
| --- | --- |
| all off | identical to the **base commit's** goldens (`git show $BASE_SHA:<golden>`), every cycle of every output, for the whole recording. A divergence is a bug, and the PR does not merge with one |
| each on alone | the replay reports, per output and per key, the first diverging cycle, the number of changed rows, and the sign and size of the change. Only the outputs that change names may move |
| all on | equals the goldens the PR commits. An output that moves here and in no single-switch run is an interaction, and the PR names it |

- **A new recording lands with goldens generated at the base,** in its own
  commit before the change, so the all-off run has something to compare
  against.
- **A seam is deleted,** with zero golden diff, by the next PR that touches the
  same package, or by a cleanup PR. It never rides in an unrelated learner's
  diff.

The replay is open loop: recorded inputs do not depend on outputs, so later
cycles stay comparable after a divergence. What the fleet would then have done
is the closed-loop probe's question.

**Goldens change only with a named change.** The PR body has a `goldens:`
section listing each output it expects to move and in which direction, and
pastes the replay's per-output summary (never the CSV). CI fails when a golden
file changes and that section is missing, or when an output moves that the
section does not name. The check also runs on the PR's `edited` event, so
correcting the `goldens:` section re-runs it. Goldens are written only by the
`make` target, never by hand.

As built (#495), the check is `.github/workflows/ci-goldens-gate.yaml`, which runs
`deploy/ci-pr-checks/goldens-section.sh`. `hack/check-goldens-section.sh` tests it
on fixture PRs from `make lint-deploy-scripts`. The section is a strict line
format, set out in [testing](../developer-guide/testing.md#the-goldens-section),
not Markdown: two reviews found paths through a Markdown parser written in bash,
and the check is for honest mistakes, since a PR runs its own copy of it. So the
pasted summary goes under the next heading, not in the section. The check watches
more than this rule's replay outputs: also the `cmd/whatif` goldens and schema,
`goldens.yaml`, and each `recording.jsonl.gz`, so a retaken recording or a new
fixture is named too. It compares file names only. The direction and the pasted
summary stay with the reviewer, and nothing machine-checks them. Run on the 77
PRs merged since #388, with their real bodies and changed paths, the 46 that moved
no golden pass. The 31 that moved one fail, because their bodies predate the
format, so that count says nothing about the format.

**Per-path counters must be above zero.** P1-obs admitted, mu samples written,
mu yields (served, sibling, stale), OLS cycles, pinned cycles, instant-KV
rows, input rows with a known ReadySince, and from PR 1 on the settle
tracker's verdicts per reason. A replay that touched none of a PR's paths
fails rather than passes. A counter counts only where its path can run on the
PR's base: a counter that is zero by construction there is not coverage, and is
required from the PR that makes it non-zero. Joined refusals with a known
ReadySince are not such a counter: the collector reports ReadySince, and k2 and
mu read it through `noteReady` since #288, so it is required from 0b.

**The closed-loop probe** is new work, not a commit of an existing test. The
probes #282's model reviews wrote hold the fleet fixed: they compute the
implied replicas and never add one. A closed-loop probe needs a fleet
simulator: the order becomes replicas after the start time S, a new replica
is Ready after the settle, load splits equally across Ready replicas or by a
recorded imbalance, and the queue grows by λ minus what was served. It lands
in **PR 3**, the first PR that needs it, as `D/closed_loop_probe_test.go`,
with one known-failing case as its negative control (one slow pod's confirmed
low rate, #282 round 3 H1; a passing case since #495, whose switches off
are now its negative control). Holds, latches, the sibling yield and the
self-sealing low mu are closed-loop behaviours; a PR that changes one runs the
probe and pastes the result. PRs 3, 4a, 4b and 5 must; 4a runs it through a
controller restart ([4.4](#44-the-cost-after-a-controller-restart)).

### 2.6 Property tests

Each class is a synthetic case, run against every learner it applies to.
Each learner **declares its estimand**: the floor's is the arithmetic mean of
mu_i (fleet capacity is their sum), A's the k-weighted ratio, k2's per
replica. A class has no target without it.

**Time base is an input family, not a class.** Inputs come from a simulated
per-second path, then pass through rate[1m], max_over_time[1m] and instant
sampling at a 15 s cycle; consecutive one-minute windows share 45 s. Every
class below runs on it as well as on direct inputs.

| class | what it asserts |
| --- | --- |
| Transient | a joiner, a load step, a slow fill and a filling token rate are each refused or stamped, per event |
| First sample | one reading does not set a figure that a second, identifying reading would move by more than the band |
| Stale value | from identifying data **at N ≥ 2**, the figure converges (catches A3: B stuck at 6 ms) |
| Missing and duplicated values | a missing gauge (presence false), a single missed row, and two rows for one pod each leave the figure where it was. A series doubled inside PromQL reaches the analyzer as one value at 2x: that is the heterogeneity case here, and a collector test |
| Contamination **paired with** heterogeneity | a **state-flagged** contaminant is dropped, and a genuinely slower replica with no flag moves the estimate by its weight under the declared estimand. A learner using a value estimator allowed by [section 5](#5-fits-exclude-by-state-then-fit-untrimmed) (half the served max, `siblingMuTolerance` of the siblings' lower median) runs the heterogeneity case with the deviation it declares, on the side it declares. Where that side is the risky one (half the served max; the sibling yield), the learner declares the deviation at which it starts to over-price, and the case goes into the known-failures ledger with an issue as its owner (#495). Since #495 mu uses neither: the slow reading counts by its weight, and the sibling yield never prices above the measurement it replaces, so both mu cases pass ([one slow pod, as built](#one-slow-pod-as-built-495)). Mu's fold across pods is the mean (#282), the declared estimand, so one slow pod at N in {4, 16} (for example {1, 1, 1, 0.5}) is a **passing** case: 0b commits it as a guard, not a ledger row |
| Redistribution invariance | the same fleet load moved between replicas leaves fleet demand and learned figures within a declared tolerance |
| N-dependence | under iid noise, an estimate's expectation does not depend on N in {1, 2, 4, 16} beyond a declared bias (catches the max across replicas, +13.6 % at 16) |
| Known truth | with a true mu and k2, the priced capacity is at most truth × (1 + ε) under every family, idle replicas included. At the documented target the steady order equals λ / (mu · target) |
| Scale | block size, GPUs per replica, output length and cycle interval varied one at a time: priced replicas are invariant, and each figure scales by its unit |
| Liveness | from clean identifying data each learner reaches truth within a stated time, **per topology**: N ≥ 2, P/D, flow control on and off, 8-GPU DP ranks |
| Monotonicity | along a trajectory, not only between two static inputs: demand does not fall as lambda or the queue rises, and stays bounded by what the fleet can drain |
| Edges by enumeration | the operating point is put on **each** threshold below, with noise of the gauges' real autocorrelation. Between declared latched switches the output is Lipschitz in the input, and it reverses at most once per excursion, an excursion being one crossing of the threshold by the **noise-free** generator path |

**Known-failures ledger.** Some classes fail on today's code by design. Those
listed when 0b landed that have since been cleared are known truth on the
time-base family (A5, #393), first sample on k2 (#392), first sample on start
time (`first-sample/start-time/between-sample-and-default`, 86f9cb5f0), edges
on the k2 key boundary (#396), and heterogeneity on mu
(`half-served-max-slow-pod` and `sibling-yield-slow-pod`, #495). 0b commits a ledger of (class, learner, finding,
measured result, the issue that clears it or "open"), and `CheckLedger` holds
the owner to that form. It reads no tracker, so a closed issue passes. A listed
case runs and must
still fail; a listed case that starts passing fails the build until its row is
removed. A PR that fixes one removes its row. Nothing is skipped and no
tolerance is widened to pass a defect.

**Each mapping row is demonstrated.** For each row of
[2.7](#27-past-findings-against-the-test-classes), 0b includes a mutant that
reintroduces the finding: the pre-fix commit's function, or a toy learner with
the defect. The class must fail on the mutant, and the PR pastes the failure.
A class with no failing mutant does not count as catching anything.

Thresholds the enumeration covers (each PR adds any it introduces). Where a
threshold is an operator policy key, the enumeration runs at its default and at
least one non-default value an operator may set (for example `idleRows: 5`,
`steadyFor: 10m`, `idleAfterReady: 5m`); property tests on defaults alone do
not cover a key.

| learner | thresholds |
| --- | --- |
| ITL | OLS at 10 rows and spread 0.30; slope held under `minSettledSamples`; `steadyFor`; `idleRows`; `carry`; the KV band edges; the sequence cap and `itlBand.capMaxPreemptionRate`; `steadyGate.maxKDrift` (0.10); 20 / 40 rows (`edges/itl/steady-window-40-rows`); 30 min window |
| k2 | queue threshold; joined 1 min; `rateFraction` of mu and of k2; `maxRise` at 1 min back; window 10; episode gap 24 h; eviction 7 d (`edges/k2/eviction-7d`, `eviction-nondefault`); the key boundaries |
| borrowed k2 | 3 % KV; 30 min hold; 6 h relaxation; 20 % re-arm |
| mu | half of served; `minMeasuredOfDerived`; 2 min confirm; `maxTokensOverCompletions`; `siblingMuTolerance`; the sibling yield's "full cache" line; the siblings' lower median's 50 % mass point; 2x stale agreement; `freshFor` / `lowHoldFor`; latch `maxRatio` / `returnRatio` |
| floor | the trust blend between `floorTrust.lagFraction` (0.5) and `floorTrust.trustFraction` (0.8); growth band max(`backlogGrowth.minRequests` (2), `backlogGrowth.fraction` (0.10) of the backlog) over 1 min; the standing-queue cap (`edges/floor/standing-queue-cap`, and `standing-queue-cap-start-over-drain` for `floor.DrainHorizon` taking a start over the drain time; the start `applyThroughputFloor` passes is not held) |
| shape, idle, decode memory | shape tolerance band and 5 min hold; idle 2 min with 1 min gap; the 0.30 idle line (`loadImbalance.idleKvUsage`, saturation's idle test: `D/roles.go:123`, `D/idle_room.go:88`); `loadImbalance.idleAfterReady`; decode memory 1 min |
| start time | 3x outlier, 3 confirmations, 0–3600 s (`collector.TestPodStartSecondsCeilingEdge`), α 0.3 |
| share (#14, #15) | step spread 0.25; p90 at ≥ 3 releases (listed for later; see 2.7) |

### 2.7 Past findings against the test classes

| finding | caught by |
| --- | --- |
| GPS ratio first raw; pinned B from unsteady readings; k2 at 8 % behind a step | first sample, transient |
| Full fit never runs at N ≥ 2, B stays 6 ms (#273 A3) | stale value at N ≥ 2 |
| k == 0 unsettles; router queue without engine metrics | missing values |
| Instant k against a rate[1m] ITL (#273 A5, caught since #393) | known truth, on the time-base family |
| Sum of per-pod one-minute peaks | redistribution invariance, on the time-base family |
| Settle refuses one cycle of a fill (#282 F1) | transient, on the time-base family |
| k2 key flips on the output-length boundary; mu key flips on the output-length boundary; measured/derived flips near 2x; the 0.30 idle line; `backlogGrew` | edges by enumeration (the operating point on the boundary, or the ratio built at 2) |
| Headroom counted twice (+17.6 %); idle room counted twice; the rules together always pick the higher mu | known truth |
| KV and throughput units mixed | scale |
| Max across replicas (+13.6 % at 16) | N-dependence |
| Prefill double scrape | heterogeneity at the analyzer (one value at 2x); the doubling itself is a collector test |
| Demand falls when λ lags a step; local queue priced as drainable | monotonicity along a trajectory |
| Weighted median flapping between modes | edges, on a **bimodal** fleet |
| IQM breaks at 25 % contamination | **flagged** contamination swept to 50 %; unflagged contamination only up to a section-5 estimator's declared breakdown, so the sweep does not reward the median |
| A genuinely slow replica trimmed away (#273 r3, #282 r3) | contamination paired with heterogeneity (the old "one reading dominates" test rewarded the trim) |
| No floor under flow control; prefill never learns (#144) | liveness per topology |
| Imbalance audit (load moved between replicas) | redistribution invariance |
| Settle holes: fall, rate escape, restart, idle return, lend | transient, one case per event |
| Restart fix stubbed | per-path counter "joined refusals with a known ReadySince", required from 0b (the Ready time is on since #288) |
| "For good" self-sealing low mu | closed-loop probe |
| k2 adopts its first reading (#392) | first sample |
| Speculative decoding prices the ITL step as a token (#274) | known truth |
| KV per request assumes one output length (#276) | known truth |

**Where each row stands after 0b.** The code table is `findings` in
`D/property_mutants_test.go`, and a test holds it to the mutants. Each
finding above is in exactly one state:

- **Caught:** 34 findings. For each, a mutant puts the defect back, and its
  class fails on that mutant. For 26 a mutant runs the production learner
  (a policy key, rows, or a stub); 8 are caught only by a toy, and the code
  table marks them "caught (toy)". "Restart fix stubbed" is caught: its
  mutant stubs the Ready time through the analyzer's `readySince` seam, the
  restart case fails on it, and the per-path counter "joined refusals with a
  known ReadySince" drops to zero. "Prefill never learns (#144)" is caught
  too: behind a full decode the prefill bound learns a mu within 3 min, and
  with `enablePrefillThroughputBound` off, its default, prefill learns
  none. "Instant k against a rate[1m] ITL (#273 A5)" is caught since #393:
  the ITL line pairs each rate[1m] ITL with the KV fill averaged over the
  same minute, and a rows mutant that withholds that average fails known
  truth on the time base on 15 of 100 seeds. "k2 key flips on the
  output-length boundary" and "The 0.30 idle line" are caught since #396:
  a stub mutant turns each band's replay switch off, and its edge case
  fails (13 of 50 seeds, and 98 of 100). "k2 adopts its first reading" is caught since
  #392: a stub mutant turns the hold's replay switch off, and
  first-sample/k2/second-reading fails (20 of 20 seeds). "Speculative
  decoding prices the ITL step as a token" is caught since #274: ten vLLM
  replicas each accepting 0.5-2 draft tokens a step read ITL per step, and
  with the correction's replay switch off
  known-truth/itl/speculative-accepted-tokens fails (20 of 20 seeds).  "KV per request
  assumes one output length" is caught since #276: a bimodal workload with
  lengths on vLLM's bucket bounds, read through the histogram's bound on
  E[O^2], and with the spread's replay switch off known-truth/k2/output-spread
  and known-truth/mu/output-spread each fail (19 of 20 seeds). "k == 0 unsettles"
  and the settle holes "fall", "rate escape", "idle return" and "lend" are
  caught since #495, each on 20 of 20 seeds. A stub mutant turns
  `Admission.IdleOnEvidence` off, and a scrape that read nothing counts as idle.
  `k2Settle.maxRise` 1e9 admits a fall, `k2Settle.rateFraction` 1e-9 admits a
  rate alone, and `steadyGate.idleRows` 1000 leaves load returning with no
  settle. A rows mutant clears `LentSince`. A lend is held at the tracker
  (transient/settle/lent-again): no learner reads a bridge, so the balance
  tracker is its only reader. The replay counts that counter and the others of
  2.5, and the golden check fails when a counter is zero over every committed
  golden.
- **Still escaping:** the rest, listed below. A row is escaping either because
  no mutant reproduces it, or because today's code already fails its class,
  so the defect is a known-failures ledger row (`D/testdata/properties/known-failures.yaml`)
  rather than something a mutant can put back.

**Still escaping:**

| finding | why |
| --- | --- |
| Router queue without engine metrics | the missing-values case passes, and no row edit reproduces the pre-fix reading |
| Demand falls when λ lags a step | the trajectory passes; no pre-fix floor is reproduced |
| No floor under flow control (#144) | a ledger row for k2; no case drives mu or the floor under flow control |
| "For good" self-sealing low mu | fixed on #495, but no property class runs a closed loop, so none catches it. The probe holds it: `TestClosedLoopProbe_ASlowReplicaInsideTheServedBoundStaysUnderTheBound` passes, and with the unread replica's credit off it is over-ordered for good again |
| Share swing and cancel (#272 F1/F2 and the d3 reviews) | the share ledger (#14, #15) is not recorded or replayed. Out of scope for PR 0 |
| Alert debounce and sustain (#283 M2/M3) | not a learner |
| Shape dependence of the ITL line (#275) | no generator models it |
| A self-sealing error in a PR that does not touch a hold | the probe runs only when a PR names a closed-loop change |

### 2.8 Recordings each PR needs

None exists yet: no recorder has run. No recorded run has ever reached OLS.
#283 and #288 have merged, so a run recorded on main carries ReadySince; none
of the older runs does.

| PR | recording needed | counters that must be above zero | synthetic stand-in |
| --- | --- | --- | --- |
| 0a | none: unit tests and negative controls | — | — |
| 0b | the E1 scenario re-run with the recorder on, from a build with 0a merged; and one recording, or a replay of it, under a non-default `analyzerTuning` (at least `idleRows`, `steadyFor` and `loadImbalance.idleAfterReady` off their defaults). 0b's goldens commit with them | instant-KV rows, input rows with a known ReadySince, joined refusals with a known ReadySince | — |
| 1 | the 0b recordings, the non-default one included | the settle tracker's verdicts per reason | the settle suite (section 8); the balance tracker's unit tests, a re-lend within 1 min among them |
| 2 | flow control off; a P/D run; a pod restarted under the same name | P1-obs admitted, settle refusals per reason, joined refusals with a known ReadySince | restart suite: same-name restart, LWS group with mixed Ready times, bridge lend, node clock 90 s behind, controller restart. 8-GPU DP: `D/dp_ranks_test.go` |
| 3 | flow control off (mu is written only beside P1-obs) | mu samples written, mu yields | closed-loop probe (lands here) |
| 4a | a run that reaches OLS: stepped concurrency giving ≥ 10 steady rows spread ≥ 0.30, with the SLO analyzer recorded | OLS cycles, pinned cycles | restart suite (PR 2) and the settle suite's one-missing-row case, both through ITL, for its two named changes; the closed-loop probe through a controller restart; a replica at its sequence cap, admitted above the band |
| 4b | a throughput-analyzer run with its recorder on: the `ols-sweep` run with throughput listed and a controller restart mid-run, two recordings ([PR 4b](#pr-4b-the-throughput-analyzers-itl-line)) | its OLS and pinned cycles; settling readings; at-cap readings (the restart recording) | negative control on the parent; the slope hold through 30 min of light load |
| 5 | a controller restart with the snapshot on | restored keys, start samples | restart suite plus restore-then-run; a restore under light load that keeps the ITL line; v0.10.0 and v0.10.1 v1 documents |

LWS and SGLang have no recording. Their synthetic suites stand in until one
exists, and the PR says so.

### 2.9 Recorder fields for TTFT, and capture

[What-if capacity](slo-capacity-planner.md) and TTFT pricing in the SLO analyzer
([SLO tiers](slo-tiers.md#phase-2-ttft), phase 2) both learn TTFT from
recordings, and today's recordings carry too little of it. Both PRs here land
before any recording either of them depends on is made.

**What is recorded today.** Every `Analyze` event's `learnrec.Input` carries,
per pod, `AvgTTFT`, `AvgServiceTime`, `PrefillComputedTokenRate`,
`QueueLength`, the length means and the KV fields, and per model
`SchedulerQueue` (flow-control queue size and bytes) and `ArrivalRate`.
`slo.Analyze`'s `AnalyzeEvent.RouterLatency` carries the router's TTFT
histogram as per-second bucket rates (streaming only, 5-minute window) and the
TPOT sum and count rates. It is present only when the SLO analyzer is listed,
the policy has an SLO (`slo.Empty()` is false) and it judged (`RouterCalled`).

**R1: collect and record**, each field behind a presence bit, so a recording
made before R1 replays unchanged:

| Level | Field | vLLM source (SGLang) |
| --- | --- | --- |
| Per pod | Presence bits for `AvgTTFT`, `AvgServiceTime`, `QueueLength`, which have none today | — |
| Per pod | Prompt- and output-length histograms | `request_prompt_tokens_bucket`, `request_generation_tokens_bucket` (`prompt_tokens_histogram`, `generation_tokens_histogram`) |
| Per pod | Engine TTFT histogram | `time_to_first_token_seconds_bucket` (same name) |
| Per pod | Queue-time mean and histogram | `request_queue_time_seconds` (`queue_time_seconds`) |
| Per pod | Prefill-time mean | `request_prefill_time_seconds` (vLLM only) |
| Per pod | KV transfer time mean | `nixl_xfer_time_seconds` (vLLM only) |
| Per pod | Computed-prefill-token histogram, added by Q1 | `vllm:request_prefill_kv_computed_tokens_bucket`, over `[1m]` like the computed-token rate (vLLM only) |
| Per model | Router TTFT and TPOT every cycle | a new `AnalyzerInput.RouterLatency`, collected by the engine every cycle while recording is on |
| Per model | Router per-request TPOT histogram | `llm_d_epp_request_streaming_tpot_seconds_bucket`, as bucket rates beside the TTFT ones. `RouterLatency` keeps only the TPOT `_sum` and `_count` rates today, and the collector queries only those |
| Per pod | Engine per-request TPOT histogram | `vllm:request_time_per_output_token_seconds_bucket`, the fallback source for the TPOT ratio (vLLM only) |
| Per pod | Queue minimum over the minute, `QueueMin` with `QueueMinKnown`: above zero, the engine had work waiting the whole minute (the busy minutes TTFT P1 reads its `V_P` from) | `min_over_time(num_requests_waiting[1m])` (`num_queue_reqs`). Each rank's least is summed over the pod's ranks, as the queue length sums their peaks: a floor under the pod's own least, and tighter than the largest rank's |
| Per model | Flow-control wait histogram, dispatched requests only (`outcome="Dispatched"`): only a dispatched request's wait is part of a TTFT | `flow_control_request_queue_duration_seconds`, which carries `model_name` and `target_model_name` as well as the pool, so it is read per model with the router latency (`RouterLatency.FlowControlWaitBuckets`), not through the pool join |
| Per pool | Pool saturation by stage, outcomes | `flow_control_pool_saturation` (stages `prefill`, `decode` and `effective`, the larger of the two, which gates dispatch; a pool with no endpoint reads 1.0) and `flow_control_requests_total`. They carry `inference_pool`, not a model, so they are recorded per pool and joined to a model only when the pool serves one |

**Cost.** Q1 changed this for three of them: the queue minimum, the
computed-token histogram and the KV transfer time also run every cycle in a
namespace with a TTFT target ([as built](slo-tiers.md#as-built-q1)), and fail
under `query_type="ttft_inputs"`. Q1 adds the histogram to the figures below:
17 queries, not 16, and 9 per pod, not 8. As R1 shipped it, every query above
ran only while a learner recording was on: the
engine tells the collector at the start of each cycle whether its recorder
is writing (`Engine.beginCollectorCycle`, collector `SetRecordTTFTInputs`),
and with recording off a cycle issues exactly the queries it issued before
R1, the slo analyzer's router queries included, for the models that list it.
With recording on, a namespace of vLLM models costs up to 16 more queries
per cycle: 8 per pod, 5 router queries (the slo analyzer's 3, for models
that do not list it, and the 2 histograms) and 3 per-pool queries. An SGLang
or mixed namespace adds the SGLang variants of the 5 engine-specific per-pod
queries. Each runs once per namespace, shared through the cycle memo, and a
failed one is counted once, under `query_type="learner_recording"`.

`learnrec.Input` mirrors `domain.AnalyzerInput`, so the router field is new on
both. Replay keeps serving `AnalyzeEvent.RouterLatency` to the SLO analyzer, for
fidelity; the new field is read only by the planner and by the SLO analyzer's
TTFT pricing. Histograms are bucket rates over the collector's window, as
`RouterLatency.TTFTBuckets` already is. They hold numbers only, so the export
needs no new pseudonymisation rule, and its leak and coverage tests
(`learnreplay/coverage_test.go`, `export_leak*_test.go`) are extended. The
goldens are regenerated with `learnergolden update`: values unchanged, new
fields absent. No switch: nothing reads the fields in R1. R1 also updates the
comment on `constants.VLLMRequestPrefillTimeSeconds`, which then becomes
collected.

**Later: write each cycle's input once.** Each `Analyze` event carries its
whole input, so a model listing three analyzers writes its rows three times
a cycle. With R1's fields that is an estimated 4 KB more per replica per
`Analyze`, about 3.3 times the measured 1.7 KB: at a 15 s cycle with 100
replicas and three analyzers listed, the 256Mi cap fills in about 40 minutes
instead of 2.1 hours. A later PR writes each model's cycle input once, as its
own event, and has each `Analyze` event name that event's sequence number
instead of carrying a copy; replay resolves the reference. That brings the
cost back to one input per model per cycle, whatever the policy lists, and
needs no change to what a replay reads.

**R2: `whatif capture` and a `Generate` replay mode.**

```
whatif capture --prometheus URL --namespace NS (--kube-state-metrics | --role-of ROLE=SELECTOR ...) \
    --from T0 --to T1 [--step 15s] [--policy FILE] -o run.jsonl.gz
```

A file of inputs alone cannot be replayed: `Replay` needs a `start` line, and
both of its modes apply the engine's own events, per analyzer, with the store
records each one read. So capture writes **cycle inputs** (one per model per
step, a `learnrec.Input` built by `internal/whatifsource` from the collector's
query templates; R2 creates that package, and the planner's standalone mode
extends it). A new third mode, **`Generate`**, writes the `start` line
(`learnrec.Start`: the saturation horizons including `BorrowTickGap`, the SLO
configuration and `Judging`, `Throughput`, from the policy file or the
defaults), then plays each cycle in the order a cycle calls them
(`engine.go`, `engine_v2.go`):

1. `slo.BeginCycle`, called by the cycle before `optimizeV2`
   (`engine.go:881`); the SLO analyzer counts its judging windows by it;
2. `retainSettle` on the models with a variant that has replicas
   (`engine_v2.go:786`, from `optimizeV2`'s prune): a model parked whole is
   in no model group, and its settle record is forgotten. A step with no
   such model stops here, as the engine's cycle with no active variant
   returns (`evictAllAnalyzerSeries`, `retainSettle(nil)`);
3. `NoteVariantsPresent` on saturation and slo, including parked variants when
   the capture can see them (`engine_v2.go:482-514`, `:519` onwards);
4. the three sweeps, saturation's `EvictStaleHistory`, the store's
   `EvictStale` and slo's `EvictStale` (`engine.go:1478-1479`,
   `engine_v2.go:561-616`), before anything stamps state this cycle;

then, **per model**:

5. `observeSettle` (`engine_v2.go:187`; "the settle tracker first"), or
   `holdSettle` for a model with no rows or no policy;
6. per variant, in key order, `StableAcceleratorFor` and then
   `FileScaleTarget` through `LoadFromScaleTarget` (`engine_v2.go:74-101`,
   reached from `buildAnalyzerInput` at `:199`). `forgetOnRequest` is left out:
   a capture has no annotation history, so it writes no `ForgetVariant` events;
7. `Analyze`: saturation first (`:203`), then the listed analyzers. With slo
   listed, its router source serves the captured router latency.

The variant states come from the role map, and the policy from `--policy` or the
defaults. The output is an ordinary recording that carries the commit of the
build that generated it, so `learnergolden add` (which replays at the recorded
commit) accepts it. Its start line carries the capture's flags
(`learnrec.Start.Caveats`), so a golden made from it says what the capture
could not see.

**Replay retains the settle tracker too.** The engine records no event for
`retainSettle`, so a replay (`learnreplay/settle.go`) derives it: the
engine observes or holds every model it reconciles once a cycle, and every
cycle opens with `slo.BeginCycle`. A model with no observation or hold for
a whole cycle was parked whole, and its record is forgotten before its next
observation or hold applies, which is before anything reads it. Two
BeginCycles under half an interval apart are one cycle (the catch-up after
failed cycles, which comes on one clock reading). Not a whole interval:
real cycles come an interval apart, and a wall clock stepped back a few
milliseconds would merge two of them. A recording with no observation events, or no slo analyzer,
forgets nothing. It moves no golden: no committed recording parks a model
whole (the acceptance and engine runs' model-02 leaves for good, and is
never observed again), so it has no switch.
`TestReplay_ForgetsTheSettleRecordOfAModelParkedWhole` holds it, and
`TestWhatifCapture_*` replay the engine's own recording of a model parked
whole to the engine's verdicts.

**Roles and readiness for a past window** cannot come from a kubeconfig, which
sees today's pods: a scaled-out run's pods are gone. They come from
kube-state-metrics in the same Prometheus, with three limits:

- `kube_pod_labels` exports no `label_*` by default since kube-state-metrics
  v2; the role and variant labels need
  `--metric-labels-allowlist=pods=[...]`. Without them, `--role-of` selectors on
  pod names give the role map, flagged as inferred; capture does not refuse.
- `kube_pod_status_ready` and `kube_pod_status_ready_time` give readiness and
  `ReadySince`; `kube_deployment_spec_replicas` gives replica counts for
  Deployments only. A LeaderWorkerSet needs a custom-resource-state
  configuration, or `--role-of`.
- **Engine parameters are not in kube-state-metrics.** `FileScaleTarget` parses
  the container's arguments (batch cap, `max-num-batched-tokens`, ranks), which
  it does not export; the `llm-d.ai/max-num-seqs` annotation appears only
  through `kube_deployment_annotations` with an allowlist. Only the GPU count
  (`kube_pod_container_resource_requests`) and the accelerator (`kube_pod_info`
  joined to `kube_node_labels`) are derivable. The rest come from flags
  (`--batch-cap`, ...) or the current Deployment, flagged when it may differ
  from the window's. The output is pseudonymised by the
same export as `learnergolden export`. A capture needs Prometheus to have kept
the series for the window; `_bucket` series are often kept for days, not weeks,
so a window worth keeping is preserved first as a Prometheus snapshot (the TSDB
snapshot API, or a `promtool tsdb` dump) and captured from it later.

**What `ols-sweep-slo` gives this work**: flow control off, the SLO analyzer
listed, streamed load; router TTFT, and per pod `AvgTTFT`, `AvgServiceTime`,
`QueueLength` and `PrefillComputedTokenRate`. It lacks the histograms, queue and
prefill time, transfer time, any flow-control series and fixed replicas. A
capture of its window adds the histograms if Prometheus still holds them.

## 3. A shared vocabulary, not a shared loop

### 3.1 The terms

Docs, logs and reviews describe every learner in the same steps: **observe**,
**admit** or refuse, **fit**, **predict** with a source and an age,
**validate** against a witness, then **keep, refit, forget or hold**. No code
drives learners through them. The learners read each other mid-cycle in a
fixed order, and a generic loop would change it.

### 3.2 The order the learners depend on

`Analyze` runs eight stages: `installRestored` → `observeFleetShape` →
`resolvePricing` → `fitLines` → `priceReplicas` (once per replica) →
`settleShape` → `priceDemand` → `applyFloorAndHolds`.

| edge | what reads what | which value |
| --- | --- | --- |
| restore → fit | `installRestored` installs windows and P2-prior bounds | before `fitLines` |
| shape → k2 key | the shape tracker sets the output length that keys k2 (row 3) | before pricing |
| ITL → derived mu → mu admission | mu's 0.5-of-derived test reads the line | this cycle's line (fitted first) |
| k2 → mu observation | a mu sample is written only beside a P1-obs k2 write | the same replica, same cycle |
| mu → k2 settle | k2's rate path reads mu on record | as that replica is priced, earlier replicas of this cycle included |
| k2 window → later replicas | each replica's P1-obs writes the window, the next replica is priced from it | then the median across replicas |
| served → mu choice | `servedBounds` (max and full-sibling median) | this cycle's rows, before pricing |
| mu choice in the floor | measured, derived or siblings, picked after every replica is priced | this cycle's figures |
| settle shape → holds | `settleShape` clears `shapeChanged` | read by `applyFloorAndHolds` |

Edges that read the **previous** cycle. A reorder breaks these silently:

| state | read by |
| --- | --- |
| mu-choice latch and sibling memory (`muChoice`) | the next cycle's mu choice |
| decode-saturation memory (#9) | prefill learning |
| last cycle's backlog and `backlogGrew` (#11) | the floor's growth test and the full queue charge |
| hot-rank memo | the hot-rank hold |

A PR that moves a learner keeps this order, or names the change.

### 3.3 Admission shapes

Admission is not a yes or no per reading:

- **ITL** is three-way: main and steady windows, main only (unsteady or
  settling), or refused. Every reading updates the replica's trace either way.
- **Mu** refuses a low reading, then accepts it once the same pod has repeated
  it for 2 min. Within the one-minute spacing a reading rewrites the last sample
  instead of adding one. "Admitted" and "became a sample" are different events.
- **k2's** settle test applies only when the queue overflows the rank, has two
  alternative tests, and one refusal reads another role's state.

So each learner counts its own outcomes. The shared code formats and exports
the counts; it does not call a per-reading admit.

### 3.4 Which side breaks the SLO

Each learner declares which side of its error breaks the SLO. **Any move toward
that side that is not driven by a new admitted reading needs a reason in this
table**, inside a fit or out: carries, `RaiseLast`, defaults, stashes,
restores, exact repeats and unknown ages. Two rules apply to all:

- An unknown age resolves to the safe side. (`measuredYieldsToDerived`
  treated a missing `SampledAt` as "as stale as can be", which skipped the
  30 min low hold. That was the risky move; PR 3 fixed it,
  `mu-unknown-age-resolves-safe`.)
- An exact repeat of a float (`rate == lastRead`) is evidence of use, not of
  saturation: it is the signature of a frozen series. Mu counted it as a
  write and kept its episode alive; PR 3 fixed it, `mu-exact-repeat-is-use`.

| learner | side that breaks the SLO | moves toward it without new evidence, today |
| --- | --- | --- |
| k2 | too high | none. The window mean rises on admitted readings only. A too-low k2 is **permanent** on a serving variant: touched on every read, never evicted, and its episode resets only on a write, which needs the saturation a low k2 prevents. The only exit is `llm-d.ai/forget-learned-state`; the runbook says so |
| borrowed k2 | too high | the upward relaxation over 6 h of serving time |
| measured mu | too high | the stale yield to a higher derived figure after 30 min (5 min toward a lower one). The restart that drops the latch (see [the mu choice](#the-mu-choice); restored to the safe side since PR 3). An unknown age (above; safe side since PR 3). An exact repeat: since PR 3 it refreshes freshness only under the derived figure, where losing it would let a repeating low rate yield to the higher figure, and not over it, where staleness yields to the lower one. Backed by new evidence, so allowed: the **immediate** yield when a Ready replica serves more than 2x it. The sibling yield was on this list; since #495 it is capped at the measurement (`mu-yield-capped-at-measurement`), so it no longer moves mu up: what the siblings serve reaches mu through the sample instead. Since #495, with no line, a figure a minority of the fleet wrote, under what the fleet's Ready replicas serve, gives way to that served bound after `lowHoldFor` with no saturated reading agreeing with it (`mu-minority-figure-expires`). The reason is the low hold's: an over-ordered fleet never saturates to correct a low figure. The bound is a lower bound, so the move stops at what the replicas showed they sustain, never at no mu. A figure at or over the bound, the whole fleet's, a borrowed one, or one whose writers are unknown (a restore), stands. And an unread replica under the KV line counts at what it serves over the scale-up target (`mu-unread-replica-at-target`): at most 1 / target over what it served unqueued, so the floor orders the fleet in which it serves that again, and no more ([one slow pod, as built](#one-slow-pod-as-built-495)) |
| ITL line and derived mu | ITL(kPrice) too low, that is, derived mu too high (with B pinned low, A comes out too high and the prediction is still low) | the 6 ms default B or a stashed B. With B pinned below the true intercept, the pinned A absorbs the error and the prediction at kPrice is low by (B_true − B_pin)(1 − kPrice / k̄): **risky whenever the fleet runs above its pricing point**, the under-provisioned fleet that most needs a correct line |
| prefill bound | too high | its window max only rises, survives the episode reset through `RaiseLast`, and never ages while read. Because `RaiseLast` carries the max past every reset, it is a running max over the bound's life: about 1.5σ above the mean after 10 readings, about 3.2σ after a day at one a minute. Unreviewed |
| start time | **both** | too low under-orders a fleet that is behind (λ > mu · ready). Too high, in `internal/signals/floor/floor.go`: `DrainHorizon` under-prices the backlog whenever S exceeds 60 s, `startingCredit` credits a stuck pod for up to 2S, and a fleet with spare projects more drain and orders less. Share pacing errs the other way: too low moves GPUs for bursts that end first. PR 5 bounds both sides |
| share need history (#14) | too low for the role, too high for its neighbours | `PeakNeed` is a max of adjacent-pair minima, low by design; trimming to the swing window ages a peak out with no new evidence |
| share release time (#15) | too low | p90 over ≥ 3 releases is the max at n = 3 |
| first-read mu, `RunningRequests / AvgServiceTime` on a saturated row (#520) | too high | its one-minute service-time window still holds completions from before the cap: +5 % to +15 % on the first saturated row of three recordings. Priced at 0.85 of the reading before two mu readings, and that price held until every replica ordered under it is Ready; while the backlog grows, the lower of it and the floor's mu ([as built](#queued-work-priced-by-drain-time-as-built-520)) |

A clamp that stops k2's fit from rising would turn it into a running minimum:
one admitted low reading would then price the fleet for good.

### 3.5 Hysteresis

Every threshold that switches a source, a **key** (the k2 key boundary on
output length, which swung capacity 39 % at constant load), a role verdict or
a demand term declares:

| item | rule |
| --- | --- |
| set, clear, dwell | three values, stated in the code comment and the developer guide |
| safe side | which side is safe. The clear direction is never faster toward the risky side. For a two-sided learner (start time) set and clear are symmetric, and the dwell is at least the learner's own noise correlation time |
| restart | an unpersisted latch restores to the safe-side choice. The mu latch does since PR 3 ([the mu choice](#the-mu-choice)) |
| tuning | dwell is tuned on noise with the gauges' real autocorrelation, not iid noise, which over-counts flips |

Declared rows:

| Choice | Set | Clear | Dwell | Safe side | Restart |
| --- | --- | --- | --- | --- | --- |
| The TTFT decode term: a fit of D, and of R, against its fall-back (`prefill.fitLatch`) | 30 cycles in a row of a fit that stands (`FitSetCycles`): 7.5 min at 15 s | 15 cycles in a row of refusals (`FitClearCycles`), holding the last fit until then | 30 cycles on the fall-back, since one refused cycle restarts the set | The fall-back, the higher-priced source on every case read. The old sum floors either side | Not persisted: comes up on the fall-back, as does a sweep on a change of engine |

The term's latch is tuned on AR(1) noise at phi 0.9 around a crossing, one reversal
per excursion at a noise of 0.3 of the excursion (`TestLatch_OneReversalPerExcursion`,
each edge), about 9 flips with none. At 0.5 the simulation reads a mean of 2.0 flips
and a worst of 4. **The noise is assumed:** phi 0.9 is not measured on the fit's own
series.
This latch is its own: the ITL tier's (#495) is private to its tracker.

The rows as built are in the developer guide's
[set, clear and dwell table](../developer-guide/saturation-analyzer.md#set-clear-and-dwell).

#### The ITL tier, as built (#495)

`internal/signals/itl/tier_latch.go`, on the saturation, slo and throughput
lines, one named change with one `SwitchITLTierHysteresis` per analyzer.

- **Set:** 10 steady readings, spread 0.30, as before. **Clear:** under 8, or
  spread 0.18. **Dwell:** 10 min after the last change.
- **Safe side:** the line that prices more replicas, from either tier, by what it
  sustains where the analyzer prices it (`Admission.PricedRate`). Inside the dwell
  the tier moves only for a line that prices 3 % more, so a held line prices at
  most 3 % under the line it refused (`tier-safe-margin`,
  `TestDwellTier_AClampedFillStillComparesTheRate`).
- **Resting:** between the clear and set values the full fit prices its own key,
  and teaches nothing: no `B`, no held slope, and no publish, so the prior on `B`,
  a borrow, the shape points and the snapshot do not read it
  (`TestITLResting_TheFitPricesItsKeyAndPublishesNothing`). It leaves at once for a
  pinned fit that prices 3 % more.
- **Evidence:** an emptied steady window, a gap past the steady carry and every
  forget put the key back at the pinned tier.
- **Restart:** the latch is not persisted. A key with a restored slope stands at
  the pinned fit, the rule's safe choice (`TestDwellTier_ARestoredKeyStartsOnTheSafeSide`).
- **Not held:** a key's first full fit, since no `B` is learned without it, and a
  tier change once the dwell has run out, which may still price less in one cycle.
  A key with a full fit behind it dwells even if that fit's `B` was at or under
  zero and so was not kept.

Measured before the change, main at f7130a9cb, every golden replayed, the
staircase recordings of #492 included: 78 analyzer-key series, 7758 cycles.
Recorded fleets hold 1 or 2 decode replicas.

| line | tier flips | back within 1 h | largest move of ITL at k 0.68 at a flip |
| --- | --- | --- | --- |
| saturation | 57 | 47 | x0.853, x1.249 |
| slo | 19 | 12 | x0.941, x1.249 |
| throughput | 7 | 2 | x0.803, x1.005 |

Flips are real and fast: 20 in 44 min on `ols-sweep`. They came from load steps
ageing out of the steady window, not from noise. Leaving the full fit moved the
line by at most 2.8 % on the saturation line, as a remembered `B` now pins the
same line. The run in which a flip halved ITL does not reproduce on these
recordings: its pinned fit went through the default `B`, and a full fit now leaves
its own. The largest moves were on entering the full fit. No flip changed a
replica answer on these fleets.

After it, on the same replays:

| line | tier flips | back within 1 h | moves at a flip |
| --- | --- | --- | --- |
| saturation | 21 | 11 | x0.909, x1.249 |
| slo | 9 | 2 | x0.944, x1.249 |
| throughput | 5 | 1 | x0.803, x1.000 |

The x1.249 is a key's first full fit, on a staircase, which no dwell delays. The
mu falls on 222 of 298 moved cycles and rises by at most 3.0 %. Decode demand
moves x0.933 to x1.120. In whole replicas of supply the decode requirement is
higher on one cycle and lower on none. The cost: 250 of 2274 cycles refused a tier
change.

The two lowest cycles of decode demand (x0.933 and x0.937, throughput-sweep-restart
22:14:20 and 22:14:35) have the mu **lower** than main's and the demand lower too.
That is the floor's trust blend, not the line. Main's line stepped to a full fit,
the mu rose 10.67 to 12.52, and the floor's arrival demand fell 105 381 to 90 123.
Against the 122 872 resident that is a ratio of 0.73, under `trustFraction` 0.8, so
trust fell to 0.78 and the demand stopped at 131 574, keeping part of the queue's
charge of 162 108. With the step held the mu is 10.74 and 11.20, the ratio stays
over 0.8, trust is 1, and the demand is 123 282 and 122 872. So between
`lagFraction` and `trustFraction` the floor's demand rises with the mu. Main has
that today, and this change only moved these two cycles out of the band. Both
readings are two replicas of supply.

Variant `TotalDemand` falls to x0.890 where main held a shape change and this does
not. `holdFleetFloor` raises a role to `scaleDown` of its supply, 73 400 here, and
writes the variants' share. With the role already at 79 557 there is nothing to
raise, so the variant keeps its raw 66 113. The floor writes no variant figure, so
the variant reads 0.63 of supply while the role reads 0.76. Role demand is what
scales, and it is up x1.06 to x1.08 on those cycles.

**`SpareFraction`** is `1 - Utilization` of those variants, up to x1.256 on the same
cycles. Only a retained pool reads it (`warmpool.chooseAwake`, through
`decision.PressureFor`), to pick which model is awake. Nothing that sizes a fleet
or lends a replica reads it. `NeedsScaleUp`, from the same figure, does not change
on any golden cycle. But more spare can change a retained pool's choice, away from
the safe side: with `warmPoolSwitchSpareThreshold` between 30 and 38, the model
reads comfortable, not low on spare, so the pool may not wake it, and in a tie on
urgency the other model wins. That needs a pool with that threshold during a shape
change. No golden runs one, so its size is not measured. The gap between the
variant's figure and the role's is main's too, on any cycle the floor raises a role.

**The reversal trade: 2.6's "at most one reversal per excursion" does not hold near
the set value.** Decided on review of #498: pricing safety over flip count. The
measured bound is at most 16 reversals in 4 h on flat paths at spreads 0.26 to
0.30, 4 at 0.32, and 6 on a two-crossing ramp (`ols-spread-reversals-bounded`).
All of this noise is synthetic: rate[1m] noise, 40 seeds of 4 h from a cold key for
reversals, and 20 seeds of a key that entered at spread 0.36 and rests for the
under-the-truth share (cycles more than 3 % under).

| spread, pods | off: reversals (median, max) | built | no resting rule (first build) |
| --- | --- | --- | --- |
| 0.26, 4 | 44, 60 | 9, 13 | 1, 1 |
| 0.28, 4 | 56, 76 | 4, 10 | 1, 1 |
| 0.30, 4 | 26, 47 | 0, 3 | 0, 1 |
| 0.26, 10 | 38, 58 | 7, 13 | 1, 1 |
| 0.28, 10 | 86, 106 | 7, 11 | 1, 1 |
| 0.30, 10 | 78, 98 | 2, 5 | 0, 1 |
| ramp, two crossings, 4 and 10 | 8 to 12, 12 to 20 | 2, 4 | not run |

| resting at, pods | off: under the truth | built | no resting rule | clear 0.21 |
| --- | --- | --- | --- | --- |
| 0.22, 4 | 2.37 % | 2.41 % | 7.06 % | 2.29 % |
| 0.26, 4 | 4.82 % | 3.20 % | 4.94 % | 3.20 % |
| 0.22, 10 | 1.82 % | 1.19 % | 3.72 % | 1.55 % |
| 0.26, 10 | 1.90 % | 1.68 % | 2.88 % | 1.66 % |

What moved the error was the `B`, not the clear value. With the band teaching
`B`, a clear of 0.24 gave 18.8 % at 0.22 with 4 pods, since that `B` pins every
later fit. With it taught nothing, 0.21 is no better than 0.18. So the clear stays
at 0.18, the band teaches nothing, and the 3 % rule keeps a resting line on the
safe side, at the price of some reversals,
each a move to the safe side and back no faster than the spread sets again. A
margin of 6 % or a 4 min confirmation gives median reversals near 0 and loses most
of the gain (5.06 % and 5.53 % at 0.22 with 4 pods). A 10 % margin is no rule at
all. `ols-spread-reversals-bounded` holds at most 16 in 4 h on the flat paths,
and 6 on the ramp.

What is assumed: the 3 % comes from the 2.8 % above, and 10 min is twice the
steady carry. A 5 min dwell measured worse (25 flips and a x0.799 step, on the
first build). Neither value has been checked on a large fleet: every recording
here holds 1 or 2 decode replicas.

### 3.6 Clocks

The clock fix itself is [2.2](#22-one-clock). The clocks in use:

| clock | used by |
| --- | --- |
| **write** | ITL line age, the shared line |
| **use** | k2 and mu eviction, 7 d after the last read; the prefill bound's `Touch` |
| **write gap** | k2's and mu's new episode after 24 h without a write, on the same window as the use clock. The prefill bound defeats it (`RaiseLast`) |
| **agreement** | mu freshness: the last saturated reading within 2x. Refused readings set it too. An exact repeat sets it only while the measurement is under the derived figure (PR 3) |
| **serving** | the borrowed k2 relaxation, and the hold clocks that only a serving row advances |
| **existence** | `VariantTimeout` (last reported or found to exist), the ITL sweep at 24 h unseen, `evictStartSeenPods` |
| **since event** | the settle tracker: time since join, idle return, restart or lend |
| **cycle count** | start-time outliers, #273's `idleRows`, `PeakNeed`'s two in a row |

An age shown to an operator is the age of the last **admitted reading**, never
of the last use. A use-touched k2 otherwise reads young however old its
evidence is. Mu publishes its last admitted time; its freshness (agreement) is
a different clock and is logged, not published as an age.

## 4. Settled replicas

### 4.1 Four rules today

| | ITL steady trace (#273) | k2 settle (#282 `D/k2_settle.go`) | `noteReady` (#282, #288 `D/throughput_admission.go:176-210`) | balance tracker (#283 `internal/signals/balance/tracker.go`) |
| --- | --- | --- | --- | --- |
| joined wait | `steadyGate.steadyFor` (2 min) | 1 min lookback, queue standing | 1 min | `loadImbalance.idleAfterReady` (2 min) |
| ready time | first sight | later of Pod Ready and first sight of that Ready time, through `noteReady` (`k2_settle.go:197`) | later of Pod Ready (`replicaReadySince`, from `ReplicaMetrics.ReadySince`) and first sight of that Ready time | later of Pod Ready and first sight of that Ready time |
| restart events | joined, load back after idle (`steadyGate.idleRows` rows with no latency and no KV fill) | joined, a new Ready time under the same name | joined, a new Ready time under the same name | joined, warm-pool lend, any absence |
| key includes variant and bridge | no | no | no | yes: `(pod, variant, bridge)` |
| a missing row | restarts the settle (before PR 4a; kept up to the tracker's tolerance after it) | kept up to `K2SettleMaxGap` | kept with k2's trace; a row that is not Ready forgets it | forgotten (`tracker.go:117-141`) |
| first cycle after a controller restart | taken as settled (before PR 4a; waits `steadyFor` from first sight after it) | taken as established, unless the Pod's Ready time is under 1 min old | taken as established, unless the Pod's Ready time is under 1 min old | waits again |

### 4.2 One owner

One tracker in `internal/signals/settle` replaces them.

| question | answer |
| --- | --- |
| owner | **the engine**, one tracker per model, swept with the model. It observes this cycle's rows into the tracker at the top of `runAnalyzers`, before `observeLoadBalance` (`engine_v2.go:156`) and before any analyzer. An analyzer-owned tracker would be written after the balance tracker reads it (`observeLoadBalance` runs before `runV2AnalysisOnly`), one cycle late on every join |
| readers | the balance tracker (#283), the saturation, SLO and throughput analyzers, each through a **read-only view**. None writes. The engine builds the saturation and SLO analyzers (`steadystate/engine.go:335, 405`) and passes the view at construction. The throughput analyzer is built in `cmd/main.go:918` and added by `RegisterAnalyzer` after the engine exists, so it takes the view at registration through an optional interface (`settleViewTaker`), as the engine's other capabilities are reached |
| replay | the tracker is a function of the rows and the engine's clock read only. The engine's observation is an engine-side event in the stream ([2.4](#24-the-recorder)): the model and the `now` the engine read, which is earlier than the `a.now()` each analyzer reads inside `Analyze`. Replay applies it as the engine did, at that instant, with the next `Analyze` event's rows. It reads nothing from the store, so a rollout is detected from the rows (4.3) |
| concurrency | the tracker holds its own lock; one writer per model per cycle, any number of readers |
| key | (model, pod, variant, bridge). A warm-pool lend from the pool keeps the pool's Ready time but changes variant and bridge, so it is a new key and a new settle |
| prerequisite | #283 (merged) |

**Difference to reconcile with #283.** The balance tracker rebuilds its
sightings from this cycle's rows, so one missing row forgets a replica
(`tracker.go:117-141`). That forget is load-bearing: its comment says a
warm-pool Pod lent again "starts its grace again". The shared tracker keeps a
sighting across an absence up to one rule period (1 min; at an optimize
interval over 24 s, one missed cycle and half a cycle of slack, 2.5
intervals, so a long interval does not make every cycle an absence, PR 1): a
single missing row
is a missed scrape, and restarting a busy replica's settle for k2, mu and ITL
would re-open #273's A4. Under that rule a Pod lent again to the **same**
variant inside a minute keeps its key, (pod, variant, bridge = true), and so
would keep its grace. Not verified on a run: whether a re-lend keeps the key,
and whether ReadySince changes on a re-lend. PR 1 checks both, and keeps a
re-lend a new settle by an explicit lend event, not by absence.

When the balance tracker moves onto the view in PR 1, its imbalance timers
take the 1 min rule. That is a named change for #283's metrics and for the
`WVALoadImbalanced` alert, whose description says "Ready for at least 2
minutes" (`loadImbalance.idleAfterReady`). PR 1 updates the alert text and the
idle row in `docs/reference/prometheus.md` if their meaning moves. It is outside
the analyzer replay (the engine owns the timers and publishes them as metrics),
so PR 1 proves it with unit tests on the balance tracker: one missing row keeps
the timer, two rows past the period restart it, and a Pod lent again to the
same variant within 1 min restarts its grace. The first two fail on the parent;
the third passes on the parent and must keep passing.

### 4.3 What the tracker owns

- **A stamp per reading, taken when observed.** The learner stores it with the
  reading, because a window outlives the settle.
- **The events that start a settle:** a replica joins, load returns after idle,
  the engine restarts (an engine or LWS group restart under the same name gives
  a new Ready time), a warm-pool lend, a rollout. A rollout is detected from
  the rows (a pod with a new `Revision`, or a new pod), not from
  `capacity.Record.RollingOut`: `LoadFromScaleTarget` files that bit later in
  the cycle than the engine observes the tracker. An LWS group's row is its
  leader's, and its Ready time is the latest over the leader and its live,
  Ready workers (`groupReadyTimes` in
  `internal/collector/pod_serving_state.go`), so a group restart (#361) and a
  worker restarted alone under `restartPolicy: None` both give a new Ready
  time (the "PR 2's decisions" note at the top).
- **The ready time:** the later of the Pod's Ready time and the controller's
  first sight of it, as the balance tracker (#283) and `noteReady` (#288) do
  today. A node clock behind the controller's, or a
  lend that keeps the pool's Ready time, makes the Pod time too old.
- **Idle needs positive evidence, and the evidence is role-aware.** KV here
  is **peak** KV over the rule's window, not instant KV: prefill KV is freed
  once a prefix is handed to decode, so an instant 0 between requests is the
  ordinary case on a lightly loaded prefill pod.

  | role | a row counts toward idle when | load returns when |
  | --- | --- | --- |
  | decode, aggregated | peak KV known and 0, **and** either ITL is known-empty (series present, window empty) or the generation-token rate is known and 0 | a finite ITL, or the generation-token rate known and > 0 |
  | prefill | peak KV known and 0, **and** the request rate known and 0 | the request rate known and > 0 |

  Prefill uses the request rate, present on every engine, for both entering
  and leaving idle. `PrefillComputedTokenRate` is not a substitute: it is
  absent on SGLang. ITL-empty is not evidence on a prefill pod: the P/D sidecar rewrites prefill
  requests to `max_tokens=1`, so a busy prefill pod records no inter-token
  intervals and reads known-empty on every row. This table changes what
  `steadyGate.idleRows` counts on a prefill pod: scaling-policy.md defines an
  idle row as "no latency and no KV fill". That is a named change, and the PR
  that makes it rewrites the key's row there.

  `idleRows` counting rows in a row (default 2) make the replica idle. The ITL
  learner applies that count over the rows the tracker returns; the tracker
  does not fix it. "In a row" breaks on a gap: two counting rows more than
  1 min apart, with only unknown rows between, do not continue a run. The
  limit is on each gap, not on the run's span, so `idleRows: 5` (75 s at a
  15 s cycle) still makes a replica idle. A row where a gauge it needs is
  unknown neither counts toward idle nor breaks an idle run. So Prometheus
  timing out the KV and ITL queries for two cycles does not mark a busy fleet
  idle, and the next good row does not restart every replica's settle as
  "load returned after idle". A row with KV 0 and a finite ITL is a reading (a
  light or draining replica), not idle. A row with k > 0 and no ITL is a busy
  replica whose ITL is missing. A row whose bits are all false is a failed
  scrape: it neither counts toward idle nor restarts a settle.
- **Gaps and lookback.** A gap longer than the rule's period restarts the
  judgement. A gap is **missing rows**, not unknown gauges: a row is a
  sighting whenever the pod is listed, whatever its bits, so a Prometheus
  brownout longer than 1 min does not restart every replica's settle. A cycle
  that reads no rows of the model at all (its collection failed, or returned
  no series) is not a gap either: the engine holds the model's sightings
  through it (PR 1), for up to 5 min without an observation, or the
  tolerance if longer. A rule
  compares against the reading one period back, not one cycle back:
  cycle-to-cycle one-minute peaks share 45 s.
- **The brownout trade.** A run of unknown rows keeps the settle. So a replica
  that goes idle and takes load again inside a long brownout keeps its settled
  state, and its first known rows, a fill transient, are admitted. That errs
  on the cost side: a fill reading has a standing queue and low residency, so
  it lowers k2 and over-orders, and k2's own transient checks (0.7 of mu, 0.7
  of k2) catch most such readings. PR 1 could instead have ended the settled
  state after more than 2 min of unknown rows, at the price of 2 min of
  refused admissions per replica; it keeps the settle, and holds it through
  cycles with no rows at all as well. The brownout case in
  [section 8](#8-risks-and-limits) pins it.

Each learner keeps its own predicate and thresholds over the trace the tracker
returns. k2's rule needs a second gauge, a precondition and a cross-learner
read, and stays k2's. Before PR 1 merges, all four rules above are written
against the API (the review sketch already writes k2's against `Back` and
`Trace`).

**The tracker exposes event times; each learner keeps its policy.** The
tracker records when a replica joined, went idle, took load back, restarted or
was lent, and the time of each row. It does not decide how long a learner
waits. Each learner applies its own window, keyed by its policy. Examples, not a
complete list: `steadyGate.steadyFor`, `steadyGate.idleRows`,
`steadyGate.maxKDrift` and `steadyGate.minSettledSamples` for ITL, `k2Settle.*` for k2, `muChoice.*` and
`admission.*` for mu, `loadImbalance.idleAfterReady` for the balance tracker.
Every key in `internal/config/analyzer_tuning.go` stays honoured, with its
documented range, after PR 1 and 4a. A change to what a key means is a named
change, documented in [scaling policy](../reference/scaling-policy.md) in the
same PR. A key that can no longer be honoured is deprecated with a load-time
`WARNING`, as the crossed-pair repairs do today, never silently ignored.

### 4.4 The cost after a controller restart

Under the shared tracker every replica waits the window again after a
controller restart. This is **on the SLO side**: capacity is `min(k1, k2)`, and
with the snapshot off (the default) every k2 window is empty, so a saturated
fleet prices from k1, the high figure, for 1 min longer than today. ITL does
not refit for `steadyFor`, 2 min by default (a named change in PR 4a). #273 chose the current behaviour to avoid exactly this
delay.

What an operator sees during the ITL wait, and which side it errs on: with no
line there is no derived mu (`deriveMu` returns nothing on a zero model), and
with the snapshot off the measured-mu window is empty too, so the role has no
mu and the throughput floor is silent ("a role with no throughput on record is
silent"). Today that gap is the 3 unstamped readings a pinned A needs, about
45 s at a 15 s cycle. After 4a it is the 2 min settle plus those readings,
about 2.75 min, during which only occupancy holds the fleet: the **SLO side**.
On a default install it is masked by the 300 s scale-down stabilization
(`WVA_SO_SCALE_DOWN_STABILIZATION`, `deploy/lib/scaledobject.sh:3894`), as
long as the controller is back within about 2 min: the gap starts when the old
pod stops serving, so termination, image pull and leader election come on top
of the 2.75 min. An
operator who lowers that under about 3 min, or whose load rises during the
gap, gets an under-order. PR 4a runs the closed-loop probe through a
controller restart and pastes the result.

So: with the snapshot on, first sight is persisted (PR 5d) and the wait applies
only to replicas with no record, or whose Ready time moved. With it off, the
wait is accepted, logged on
the `learner-start` line ([6.1](#61-logs)), and named in the operator column of
PRs 2, 3 and 4a.

## 5. Fits: exclude by state, then fit untrimmed

The pinned ITL fit changed five times inside #273. A weighted median jumped
between two groups of slopes. A trimmed mean dropped a genuinely slower replica
and priced capacity 8.5 % high, on the SLO side. What holds is: stamp each
reading by the replica's state, drop the settling ones, and fit the rest
without trimming.

So there is no shared robust fit and no default fit. A value-based estimator
(median, trimmed or lower median) is allowed only where no state identifies the
contaminant, and the learner names the contaminant and the breakdown it needs.
Existing fits stay until a replay justifies a change.

## 6. Observability

### 6.1 Logs

**`learner-write`**, at `perReplicaLogLevel` (V(3)), one level above the
shipped `-v=2`. #303 moved the per-replica lines there because at the default
they were most of the controller's log, about 1.9 KB per replica per cycle;
`replica-capacity-summary` says what each replica was priced at, at the
default, split under the CRI line limit. A learner may log at the default only
if its lines pass #303's per-cycle budget test
(`TestCycleLogAtTheShippedVerbosityDoesNotGrowPerReplica`) with the first-write
burst after a restart included. Not verified: whether that test, as written,
fails on such a burst; the first PR that adds `learner-write` checks it, and
extends the test if it does not.

| item | rule |
| --- | --- |
| level | V(3) for per-key lines. At the default: `learner-start` (one per model) and nothing per key |
| when | at most **one line per key per cycle**, written after the last replica of the cycle is priced |
| trigger | the value differs from the **last logged** value (not last cycle's) by more than the learner's band, or is set for the first time. Against the last logged value a slow drift still logs, and noise under the band does not |
| band | per learner, stated in its PR, and above its measured cycle-to-cycle noise (mu's must exceed the 4 % that flipped it 11 times in 20 cycles) |
| heartbeat | a key that priced or was written in the last 10 min, and has not logged for 10 min, logs once |
| source change | a mu choice flip (measured, derived, siblings) is a `learner-write` with `source` changing, so latch flaps count from one line |
| fields | `learner`, `modelID`, `namespace`, `variant`, `role`, `key`, `value`, `previousValue`, `source`, `samples`, `pods` (each contributing pod with its reading and time base), and the admission tests passed with their margins |
| fields, ITL (as built, #495) | `a`, `b` for `value`. `pods`: the pods whose readings the line was fitted from this cycle, at most 16, lowest mean ITL first, each with `reading` (its mean ITL), `k`, `rows` and `timeBase`, and `podsOmitted`; none for a held slope. Margins: `kSpread` against `kSpreadThreshold` (0.30) and `steadyHeld` against `steadyHeldThreshold` (10), the full fit's gate, each with its `...OverThreshold` (negative when missed), and `fitRows`. The same fields on the saturation, throughput and slo lines. The tier latch's clear values (8 readings, 0.18) and the pinned fit's settled floor (3) are not logged |
| ceiling | at V(3), keys × 1 per cycle. 100 variants × about 9 keys = 900 lines per cycle (about 450 KB), reached once after a snapshot-off restart when every key is set for the first time. Steady state, with the heartbeat: up to keys / 40 per cycle, about 90 lines a minute at 900 keys. Each line stays under the CRI line limit (16 KiB); `pods` is bounded for that reason. At the default verbosity the per-cycle volume does not grow with keys or replicas |

E1's 0.0995 req/s was **admitted**; this is the line that would have shown it.

**Decision lines name their learner key and source.**
`throughput-demand-floor` gains `muKey`, `muSource`, `startEstimateSeconds` and
`startSource` (measured, or none until PR 5 wires a seed).

**Sources stay per learner,** in the vocabularies already shipped. k2 keeps
`P1-obs`, `P2-hist`, `P2-prior`, `P2-borrow`, `P3-k2`, `P4-k1`: the
troubleshooting runbook gives each a different remedy. Mu keeps measured,
derived, served-siblings, neighbour, borrowed-line. Start time keeps measured,
seed. **A restore is a field, not a source:** each line adds `restored: true`
until the learner's first admitted reading after a restart, and keeps its
source. No metric gains a `restored` label value; a restored figure shows as
`time() - ..._last_admitted_timestamp_seconds` older than the restart.

| learner | restored? | prices at once? |
| --- | --- | --- |
| k2 | yes, a held first reading no (#392) | a restored window yes, as `P2-hist` (or `P2-prior`); a reading held when the controller stopped is dropped, and the next one waits for a second |
| ITL | B, its stash and the held slope (`ITLSlopes`, #329) | yes, from the held slope, within `BucketRetention` and under the same args; otherwise not until refit |
| mu | window yes, choice no | the window yes; the choice restores to the safe side (PR 3) |
| start time | yes | yes |

**`learner-start`**, one line per model at the first cycle after a controller
start: restored keys per learner, cold keys, the oldest last-admitted age, and
`settleUntil`, the time the fleet-wide settle wait ends. It is the line that
logs "every replica waits again". There are no per-key restart lines, no count
lines and no "cleared" lines: counts are metrics only.

**Lines the tools parse stay stable, or are versioned.**
`hack/benchmark/dump_k2_decisions.py` parses `k2-decision`,
`replica-capacity-decision`, `scheduler-queue-demand`,
`throughput-demand-floor`, `scheduler-queue-prefill-share-dropped`,
`prefill-demand-held`, `hot-rank-hold`, `replica-capacity-skipped`,
`replica-capacity-store-fallback`, `variant-capacity-source`,
`zero-replica-capacity-estimate`, `itl-window`, `itl-fit`, `derived-mu` and the
shared-cache decision line. `replica-capacity-summary`, the default-level line
#303 added, is held to the same rule, and a tool that wants per-replica prices
at `-v=2` reads it. A PR that changes any line a tool in
`hack/benchmark/` parses either keeps its fields (adding is fine) or bumps a
`v` field and updates the tools in the same PR. `k2-decision`,
`replica-capacity-decision` and `derived-mu` are at V(3) since #303, and
`dump_k2_decisions.py` already says a run needs `--v=3`. So the zero-line guard
applies only to a run at `--v=3` or higher: there the tools fail when a run has
zero `k2-decision` or zero `throughput-demand-floor` lines, or zero lines of a
message the scenario declares (as `post_run_analyze.sh` already does for
`k2-decision`); an aggregated run legitimately has no prefill lines. At the
shipped `-v=2` the guard reads `replica-capacity-summary` instead. The P-code legend in `dump_k2_decisions.py` stays P-codes
only; `restored` is rendered as a separate column.

### 6.2 Metrics

- Labels: `namespace`, `model_name`, `variant_name`, `role`, and
  `controller_instance` when set; one metric takes `learner` rather than one
  name per learner ([as built](#reports-restored-and-mus-forget-as-built-495)).
  No pod, no bucket. The vocabulary is
  [Every signal as a metric](signals-as-metrics.md), within its budget of about
  60 series per model.
- Series are per variant. A gauge describes the key that priced the variant
  this cycle, and says so.
- Ages are published as `..._last_admitted_timestamp_seconds`, so that
  `time() - x` stays honest through a stalled analyzer.
- Each PR states its series count per variant, and a kept or replaced table
  for the `wva_learned_state_*` series it overlaps.
- **Deletion.** There are no per-key series, so nothing is deleted per key.
  A variant's learner series are deleted when the model is deleted, where the
  engine deletes the others. When the pricing key changes they are set from
  the new key; when the key last published is forgotten and no other prices,
  the variant's learner series are deleted. A cycle no key priced leaves the
  series, so the age stays visible through a stall.
- **Forget reaches every learner.** The `Report` carries `pricingKey` and
  `forgotten{key, by: operator | evicted}`; the engine applies both. Every
  learner implements `Forget(key, by)`. The `llm-d.ai/forget-learned-state`
  annotation calls it on all of them through a plain list of learner names in
  `learn`, not a registry, and a spec asserts that every learner on the list
  is reached.

### 6.3 Who does what

`internal/signals/learn` holds vocabulary types: `Report` (counts by reason,
source, samples, last admitted time, writes, `pricingKey`, `forgotten`),
`Source`, `Reason`. The learners fill the Report; `learn` never logs and never
registers a metric. The analyzer logs; the engine publishes. `learn` imports
only the standard library, and `settle` likewise, so `domain` and
`internal/metrics` can use their types without a cycle.

The engine takes reports through an optional interface,
`TakeLearnerReports() map[model][]learn.Report`, as it takes
`TakeRestoreOutcomes`. The map is overwritten per model each cycle, so it stays
bounded when metrics are off. `learn` lands with its first user (PR 2), not as
a standing package.

## 7. Migration

Each PR replays the fixtures under [the golden rule](#25-replay-and-the-golden-rule)
and says what an operator sees: log lines added or changed, metric series added
or deprecated, docs rows rewritten, tools updated.

| PR | change | recording (exists?) | what an operator sees |
| --- | --- | --- | --- |
| **0a** | One clock (2.2), including the capacity store and the engine's calls into it; presence bits with ITL's three states (2.3), reusing `KvUsageInstantKnown`, `CapacityQueueKnown` and `PreemptionRateKnown` and adding `RunningRequestsKnown` (a prerequisite of #277). Unit tests, the `EpisodeGap` negative control, the one-cycle boundary test | none needed | Nothing, beyond 2.2's one-cycle boundary. Metrics: none |
| **0b** | The recorder at the input boundary in each analyzer and the store (2.4), with each `Analyze` result recorded and the test that a replay at the recording's commit reproduces it byte for byte; raw rows and the merged policy recorded every cycle; the store split into parsing and one shared method; one switch hook per package; replay with goldens and the golden rule (2.5), per-path counters, property tests with the known-failures ledger and a mutant per mapping row (2.6, 2.7). Mu's mean fold and the sibling yield go into the base goldens, the one-slow-pod case as a passing guard | E1 re-run from a build with 0a, and one under a non-default `analyzerTuning` (no) | Nothing. A recorder flag and overlay in `testing.md`. Metrics: none |
| **1** | `internal/signals/settle`, written by the engine at the top of `runAnalyzers`, ahead of `observeLoadBalance` and of the input builder (`buildAnalyzerInput`, #345 step 1), as #345 decided; read-only views for the balance tracker and the analyzers. The tracker exposes event times; each learner keeps its policy-keyed window (4.3). #283's one-missing-row forget becomes the gap rule, a named change proved by unit tests on the balance tracker, with a re-lend within 1 min still restarting the grace (4.2): a row missing one cycle under the same lend keeps the grace, and a Pod lent again restarts it, told apart by a lend event, not by the absence (both restart on the parent). `idleRows`, `steadyFor` and `idleAfterReady` keep their documented meaning, or the PR names the change in scaling-policy.md. 2.3's A4 and 273-r3 #2 negative controls | 0b's, the non-default one included (no) | The imbalance timers no longer restart on one missed scrape. The `WVALoadImbalanced` alert text is checked against the new rule |
| **2** | k2 (#3, #4) onto the tracker. k2 keeps the Ready-time rule `noteReady` applies today (#288), now read from the tracker; nothing is switched on twice. First task: a fixture from #361's workload-patch group restart (leader and workers recreated, pod names kept, Ready times differ). It establishes where a group's Ready time comes from. Done: the latest Ready time over the leader and its Ready workers (`TestLWSGroupReadyTimeIsTheLatestOfItsPods`). The rule: each pod waits from the later of its own Ready time and first sight. `learn` lands. `learner-write` for k2, at V(3). Restart suite | flow control off, P/D, same-name restart (no) | After a controller restart every replica waits once: `P1-obs-unsettled` across the fleet for 1 min, and with the snapshot off k2-less fleets price at k1 for that minute (SLO side). A pod back from a restart under the same name already waits today (#288) and still does. `learner-start` at the default; k2 writes at `--v=3` |
| **3** | Mu (#5, #6). The fold across pods stays the mean (#282). Its joined test reads k2's stamp. Exact repeats count as use. Unknown age resolves safe. The latch persisted or restored safe-side. Two prefill pods in one spacing pin #7's max before the fold is touched. The closed-loop probe and its fleet simulator land here (2.5) | flow control off (no) | `learner-write` for mu, including choice flips. `throughput-demand-floor` names `muKey`, `muSource`, `startEstimateSeconds`, `startSource`. Measured mu waits out the post-restart settle |
| **4a** | ITL #1 and #13 onto the tracker. Cap admission above the band (#293, #331), the policy band, the steady-gate keys and the persisted held slope (`ITLSlopes`, #329) stay as they are. Neutral on the recording except two named changes, each behind its own switch: ITL waits the settle after a controller restart (today it takes the first cycle as settled), and one missing row no longer restarts a settle (the 1 min absence rule, 4.2). Neither occurs in the OLS recording, so 4a carries its own two tests through ITL, a controller restart and one missing row, each failing on 4a's parent; PR 2's restart suite runs as well. The closed-loop probe runs through a controller restart (4.4) | a run that reaches OLS, SLO analyzer recorded with its router latency (no) | No ITL refit for 2 min after a controller restart; with the snapshot off the throughput floor is silent for about 2.75 min (SLO side, masked by the default 300 s scale-down stabilization as long as the controller is back within about 2 min). A missed scrape no longer restarts ITL's settle |
| **4b** | ITL #16 (throughput analyzer) moves onto `itl.Tracker`, a tracker of its own, and gains #273's fixes and row 1's cap admission, still reading the policy band (`ITLBand()`). Five switches over four PRs (4b-0 to 4b-3): the steady gate (the move, with the settling stamp, the pinned fit over the window, positive B, +Inf refused and the held slope), the GPS-mismatch clear dropped, the settle tracker, the wait after a controller restart, and cap admission; each with a negative control on the parent, and the probe for the slope hold. Design: [PR 4b](#pr-4b-the-throughput-analyzers-itl-line) | `throughput-sweep` and `throughput-sweep-restart` (no) | `learner-write` when A or B moves; `Reason` `T2-held`; no throughput capacity for about 2 min after a controller restart. Throughput-analyzer fleets may change, as named: mostly fewer replicas where the throughput analyzer set the order (above-band rows and joiners' warm-up no longer steepen A), and the analyzer is silent, so out of the scale-down vote, more often. Metrics: none |
| **5** | Start time (#12) and snapshot. Confirmations counted by distinct pod. The first-sample rule, a named change that bounds **both** sides. PR 5 first wires the seed, a named change of its own: `replicaStartSeconds` is reserved and read by nothing today ([trigger reference](../reference/scaledobject-trigger.md)), so every estimate starts from 70 s. Once it is read, the first-sample bound is anchored to the seed when the ScaledObject sets one, not to the 70 s default. With no seed it blends toward the default until N distinct pods agree, rather than clamping: measured starts run from 15 s (snapshot restore) to 463 s (cold JIT cache) ([start time](../reference/start-time.md)), and a fixed band around 70 s would clamp both ends. Property cases: with a seed of 120 s, a first sample of 15 s gives an estimate in [lower bound, 120] and one of 460 s an estimate in [120, upper bound]; with no seed the estimate sits strictly between the sample and 70 s (never clamped to 70) and reaches the measured value after N distinct pods. First sight persisted. The writer stays central (`internal/engines/steadystate/learned_state_store.go`) with per-learner adapters. The document stays **v1 and additive**: new fields are `omitempty`, as #329's `itlSlopes` was, and PR 5 does not bump the version, which is part of the data key (a v2 would make every upgrade a cold start). `PriorK2` and `K2BorrowedSince` stay dual-written for rollback builds. Fixtures: a v0.10.0 v1 document (no `itlSlopes`) and a v0.10.1 v1 document both restore, and the previous release reads PR 5's document: a test pins a frozen copy of v0.10.1's `learnedStateDoc` and decodes PR 5's document with it. Restart suite plus restore-then-run, including a restore under light load that keeps the ITL line (issue #316). Design: [PR 5 design](#pr-5-design-start-time-and-the-snapshot) | controller restart with the snapshot on (no) | Start estimates no longer take one pod's first sample raw. `restored: true` on restored lines. With the snapshot on, only replicas with no record wait after a restart |

Later, each after a recorded run that covers it: idle room and the decode gate
(#9, #10) after a P/D run; the prefill bound (#7), with a double-scrape fixture
and its own-reading witness. Out of scope: drift actions, fit changes (one
named exception: PR 4b moves the throughput analyzer's pinned fit from least
squares through B to row 1's k-weighted mean, since that is the fit
`itl.Tracker` takes), #8, #11,
#14, #15, and the share planner's holds (why a second transfer waited), which
belong to the utilization-share work.

**Order with #345** (a composite that does not depend on saturation): 0a, 0b,
then #345's step 0 (the engine-level golden, with its own events in 0b's
sink) and its step 1 (one registry, one
`AnalyzerInput` built before `Analyze`, zero golden change), then PRs 1 to 5,
then #305 and #345's later steps. `AnalyzerInput.Capacity` is a read-only view
of the store for the cycle; learning capacity stays in the analyzers, as this
plan has it. #345's last step (saturation runs only when listed) needs an owner
for k2, mu, start time and the snapshot first.

### Shipped behaviour each PR must keep

These merged after revision 6. A step that follows the plan without them would
undo them. Each row is a test: an input, an assertion, and a negative control
that fails it. The owning PR adds the test, or keeps it green if it exists.

| behaviour (shipped in) | input | assertion | negative control | owner |
| --- | --- | --- | --- | --- |
| A restart under light load keeps the ITL line (issue #316, #329) | a v0.10.0 v1 document (no `itlSlopes`), a v0.10.1 v1 document, and PR 5's document; a restore under light load | both old documents restore and the held slope keeps the line. A frozen copy of v0.10.1's `learnedStateDoc` (`steadystate/learned_state_store.go:83`), pinned in the test, decodes PR 5's document with every field it knows intact | a PR 5 document that renames or drops a v0.10.1 field fails the frozen decode | 5. 4a keeps `TestARestoredSlopeIsInstalledOnlyWhenItCanBeTrusted` (`saturation/held_slope_restore_guard_test.go`) and `TestAHeldITLLineSurvivesARestart` green and does not drop `ITLSlopes` |
| A non-default `analyzerTuning` is honoured (#298) | `idleRows`, `steadyFor` and `idleAfterReady` all off their defaults | idle after exactly `idleRows` rows, steady after exactly `steadyFor`, grace over after exactly `idleAfterReady`; none of them one cycle earlier | a build that hard-codes the defaults fails it | 0b (records it), 1 and 4a (test it) |
| On P/D decode, k2 reads the engine's deferred queue, rewritten before k2 reads `MaxRankQueueLength` (#294, #306) | a P/D replay; raw rows recorded | replay runs `decodeCapacityView` again and matches the golden | the base goldens | 0b |
| ITL admits a replica at its sequence cap above the band (#293, #331) | running equal to `max_num_seqs` (from the flag, lowered by `--max-num-active-seqs`) on every rank, requests waiting, preemption read and under `capMaxPreemptionRate`, KV above the band, a valid ITL | the reading is admitted to the fit | admission removed (`itl/admission.go` `atSequenceCap`) fails it | 4a. 4b runs the same input through the throughput analyzer, where admission is new: a named change, refused on its parent |
| Mu folds across pods by the mean, and yields to its full siblings (#282; since #495 never above the measurement) | the base recordings; one slow pod | the goldens hold; one slow pod moves mu by its weight only | a fold by max or min fails the one-slow-pod case | 0b, 3 |
| The default verbosity does not grow per replica; no line passes the CRI limit (#303) | one cycle at 8 and at 64 replicas, the `learner-write` burst included | the same line count at the default verbosity for both, and every line under the CRI limit | a per-replica line at the default fails it | every PR that adds `learner-write` (2, 3, 4a, 4b, 5). Today the test lives only in the saturation package (`saturation/replica_log_test.go`: `TestCycleLogAtTheShippedVerbosityDoesNotGrowPerReplica` at 1 and 8 replicas, `TestSummaryStaysWithinTheCRILineLimit`); each other package that adds `learner-write` adds the same test (throughput in 4b) |
| A warm-pool Pod lent, and lent again, starts its grace again (#283) | balance-tracker rows: one Pod missing one cycle under the same lend; one Pod lent again to a variant within 1 min | the missing row keeps the grace; the re-lend restarts it. The tracker keys by {pod, variant, bridge} plus a changed ReadySince (`balance/tracker.go:117-141`), and a re-lend to the same variant keeps that key, so the discriminator is a lend event PR 1 adds (`ReplicaMetrics` carries only `FromWarmPool` today) | the parent restarts on both, so the first case fails there | 1 |
| A pod back from an engine or LWS group restart waits again, from the later of its Ready time and first sight (#288; #361 issues LWS group restarts) | PR 2's first task: a fixture from #361's workload-patch group restart, leader and workers recreated, pod names kept, Ready times differ. It establishes where a group's Ready time comes from. Done: the group's row is its leader's, with the latest Ready time over the leader and its Ready workers, so a worker restarted alone under `restartPolicy: None` makes the group wait (`TestLWSGroupReadyTimeIsTheLatestOfItsPods`) | each pod waits from the later of its own Ready time and first sight | on the parent, a pod whose Ready time the group restart does not change shows no wait | 2 |

### PR 4b: the throughput analyzer's ITL line

Line numbers are main's at 67790e633. T = `internal/engines/analyzers/throughput`,
I = `internal/signals/itl`, D as before.

#### Row 16 against row 1 today

| rule | row 1 (saturation) | row 16 (throughput) |
| --- | --- | --- |
| Band | policy band (`I/tracker.go:175,252-255`) | the same band, on the window only (`T/analyzer.go:294-301,189`). The pinned fit (tier 2) reads **every** healthy row with k > 0, no band: rows above 0.80, the thrashing ones the band exists to keep out, and rows near zero (`T/analyzer.go:637-643`) |
| Cap admission | above the band at the sequence cap (`I/admission.go:142-147`, `I/tracker.go:254`) | none. The analyzer reads no capacity store, so it knows no cap (`T/recording.go:26-27`, `T/analyzer.go:108-111`) |
| Ready, bridge | Ready and not lent (`I/tracker.go:212,244`) | bridges dropped (`T/analyzer.go:291,767-776`); Ready not read, the collector's zero ITL on a not-Ready pod stands in (`T/analyzer.go:353-357`) |
| ITL valid | > 0, not +Inf, Finite by presence bits (`I/settle.go:97-102`) | > 0 and not NaN (`T/sanity.go:69`, `I/window.go:130`). **+Inf is admitted** to the window, and poisons tier 1 for up to 20 rows or 30 min (every fit fails `ValidModel`); tier 2 fails the same cycle |
| k > 0 | `traceable`, a k of 0 is no reading and no trace (`I/steady.go:250-252`) | met for admission: the band's lower edge is > 0, tier 2 checks k > 0 (`T/analyzer.go:638`). There is no trace, so nothing for a k of 0 to unsettle. Not a change of its own: it comes with the trace |
| Steady gate | full fit on the steady window only, 40 rows, carry 5 min (`I/tracker.go:186-191,268-269,305-328`) | full fit on every windowed reading (`T/analyzer.go:605-607`) |
| Settle stamp | a reading taken in its replica's settle is stamped and kept out of the pinned fit (`I/tracker.go:260`, `pinnedReadings` `:428-447`) | none: a joiner's warm-up reading enters tier 2 on its first cycle |
| Settle source | the engine's settle tracker, 4a's six rules (`D/itl_settle.go:76-97`) | none. Registered without a settle view (`cmd/main.go:943`) |
| Pinned fit | k-weighted mean of (ITL - B) / k over the window (`I/fit_pinned.go:56-78`); at least 10 readings and 3 settled (`I/tracker.go:347-348`) | least squares through B, sum((ITL - B) k) / sum(k^2), over **this cycle's** rows (`T/analyzer.go:633-645`); one reading of one replica is enough |
| Remembered B | only a positive B from a steady full fit (`I/tracker.go:335-337`) | any tier-1 B (`T/analyzer.go:612`). `ValidModel` admits a negative B (`I/model.go:64`), and tier 2 then pins it |
| Held slope | last A held under too few settled readings or a short window, bounded by `BucketRetention` and the engine arguments (`I/tracker.go:375-383`, `I/admission.go:93-98`) | none. No line is `T2-failed` and the variant is skipped (`T/analyzer.go:367-374`) |
| Window clears | never on a shape change (`I/window.go:87-89`); args change stashes B (`D/mu_from_itl.go:286-307`); none mid-rollout (`D/analyze.go:511-513`) | cleared on a shape change (`T/analyzer.go:168-178`) and after 3 GPS mismatches (`:425-437`), B kept; no args rule; no rollout rule |
| Key | namespace, model, variant, accelerator, GPUs (`D/analyzer.go:1131-1134`) | namespace, model, variant (`T/analyzer.go:564-566`) |
| Forget | swept 24 h unseen (`D/analyzer.go:844`); on `learn.Learners` | the whole variant state after 1 h unseen (`T/analyzer.go:202-206`); no forget method, not on `learn.Learners` |
| Persisted, shared | B, stash, held slope; lines published and borrowed (`D/analyze.go:543-590`) | nothing |
| Report | `itl.Reading` counts, `learn.Report`, `learner-write` (`D/itl_learner.go`), path counters `itl-ols`/`itl-pinned` (`D/mu_from_itl.go:370-375`) | `Reason` `T1-ols`/`T2-default`/`T2-pinned`/`T2-failed` (`T/constants.go:75-79`), debug lines, `VariantState` (`T/types.go:63-96`) |

Persistence, sharing, the args stash, the rollout rule, speculative decoding
(#274) and the key are **not** in 4b: none is one of #273's fixes or the cap
admission.

#### Reuse

Row 16 moves onto `itl.Tracker`, `itl.Admission` and `itl.Settle`, with a
tracker **of its own**, as the slo analyzer did (row 13,
`slo/analyzer.go:504-538`). Sharing the saturation analyzer's instance is
out: both analyzers would add the same rows to one key in one cycle, and the
keys differ. What blocks the move today, each fixed in 4b-0:

1. **No settle view.** `cmd/main.go:943` passes only the clock. The engine
   hands the throughput analyzer its tracker as it does slo's
   (`steadystate/engine.go:335,408`), with `WithSettleView`,
   `WithOptimizationInterval` and `WithSlopeHoldFor` (`BucketRetention`).
   Without a view it observes a tracker of its own, as slo does
   (`slo/settle.go:94-109`).
2. **No engine parameters.** Cap admission needs the stated cap, and the
   held slope's bound needs `ArgsFingerprint`. The analyzer takes the
   capacity store read-only and reads `EngineParams` only, as slo's
   `admission` does (`slo/analyzer.go:504-517`). Replay already runs the
   analyzers on one store; the field check for a throughput-only replay is
   `EngineParams` (2.4).
3. **The two window clears.** The shape-change clear is kept, holding B and
   the last A. #453 added `Tracker.ClearWindow(key)` (`I/tracker.go:503`):
   the main and steady windows go; B, the held slope and the settle traces
   stay. The shape-change clear calls it from 4b-1. The GPS-mismatch clear
   is dropped in 4b-1 ([below](#the-gps-mismatch-clear-dropped-in-4b-1)).
4. **Config through an interface.** `internal/config`'s tests import this
   package (`T/analyzer.go:295-297`), so steadiness and the cap's preemption
   rate are read as `ITLBand` is: `ITLSteadiness() itl.Steadiness` and
   `ITLCapPreemptionRate() float64` (`config/analyzer_tuning.go:649,659`).
5. **Recorder.** `start.Throughput` is a bool (`learnreplay/replay.go:114`);
   it becomes the throughput analyzer's settings (slope hold, interval, a
   view or not), so a replay builds it as the run did.

The `Reason` values map from `itl.Reading`: `TierOLS` is `T1-ols`; pinned with
a learned B `T2-pinned`, else `T2-default`; no line `T2-failed`; a held slope
`T2-held` (new); the above-band fallback `T2-above-band` (new, added in
4b-1's review: [4b-1 as built](#4b-1-as-built)).

#### Named changes

Switches live in T and are set by `SetSwitchesForReplay`
(`T/recording.go:54`). Off is the parent's code, so all off reproduces the
base goldens.

| switch | changes | an operator sees | negative control (fails on the parent) |
| --- | --- | --- | --- |
| `throughput-itl-steady-gate` | The move onto the tracker, with the key's own trace, and #273's fixes with it: the steady gate on the full fit; the settling stamp; a pinned fit over the window's readings, in the band, k-weighted, with at least 10 readings and 3 settled; a positive B only; +Inf refused; the held slope. The shape-change clear through `ClearWindow`, which drops the held slope too; the above-band fallback for a key with no line and no slope to hold ([as built](#4b-1-as-built)) | `Reason` `T2-held` and `T2-above-band`. `learner-write` for the line when A or B moves (band 5 %, as 4a), and a tier change. Fewer `T2-*` lines in a variant's first cycles (10 readings first). Metrics: none | A joiner's warm-up reading (k 0.46, 450 ms against a line near 0.09) moves A on the parent and not on; one reading of one replica sets A on the parent and not on; an +Inf ITL leaves `T2-failed` for 20 rows on the parent; a step through the band teaches B on the parent from unsteady rows |
| `throughput-itl-keeps-the-window-on-gps-mismatch` | Three cycles of GPS mismatch no longer clear the window; the comparison stays a diagnostic, on the minute-mean k, one summary line per variant ([why](#the-gps-mismatch-clear-dropped-in-4b-1)) | `GPS mismatch persisted, clearing observation window` is gone; `GPS mismatch detected` moves to the debug level with a per-variant summary. More full fits on a P/D decode fleet | A correct line with the generation rate 30 % off for three cycles: the window empties on the parent, keeps every reading on |
| `throughput-itl-reads-the-settle-tracker` | A replica's settle start comes from the engine's tracker, with 4a's rules: restart under its name, idle on evidence, the absence tolerance, the Finite pod mean. Meaningful only with the gate on; on alone it moves nothing | None in logs; a missed scrape no longer restarts a replica's settle | A restart under the same Pod name with no not-Ready row between: settles on, not on the key's own trace |
| `throughput-itl-waits-after-controller-restart` | After a controller restart, replicas already serving wait their settle. With no snapshot of this line (none exists), the analyzer has no line for `steadyFor` | `T2-failed` and no throughput capacity for about 2 min after a restart: the analyzer is not live, so it neither orders nor vetoes a scale-down | The restart case of 4a, through the throughput analyzer: a line on the first cycle on the parent, none for `steadyFor` on |
| `throughput-itl-admits-at-sequence-cap` | Readings above the band from a replica at its stated cap, with preemption at most `itlBand.capMaxPreemptionRate`, enter the window | A line, and so throughput capacity, on a variant whose only replica in reach is at its cap (#287) | A replica at its cap above the band: refused on the parent, admitted on. With the gate on and this off, the variant has no line |

What can move the fleet size, and which way:

- **Down (TA orders fewer).** Tier 2 today takes above-band rows and every
  joiner's warm-up: both steepen A, cut supply (`N_sat / ITL(0.85)`), and
  order more, most during a scale-up, which feeds itself. Dropping them
  raises supply.
- **Silent more often.** The 10-reading floor and the restart wait leave
  variants with no line where tier 2 had one. A silent analyzer is out of the
  scale-down vote (`T/analyzer.go:488-493`), so the others can shrink the
  fleet where TA would have held it: the risky side, masked by the default
  300 s scale-down stabilization as in 4.4.
- **Either way.** B from steady fits only, and the held slope at light load
  in place of a tier-2 fit from k near zero, whose A is noise over a small k.
- **Up (live again).** Cap admission, and +Inf no longer blanking the line.

TA only drives a scale-up where it is the highest order; saturation still
votes. A policy that lists throughput alone has no such floor: a silent line
holds the role. 4b-1 says so in
[configuration.md](../reference/configuration.md#the-throughput-analyzers-line),
with how long the line is silent after a restart or a shape change, and the
above-band fallback's one-sided error.

#### Recordings

The in-process goldens carry throughput results (`acceptance` 52 rows, 19
`T1-ols`; `engine` 28, 7 `T1-ols`; `mu-paths` 192, all `T2-default`),
synthetic, so the switches move them but the physics is not in them.

**Taken:** the `ols-sweep` run of testing.md, with the throughput analyzer
on, as one run that gave two recordings: `throughput-sweep` and
`throughput-sweep-restart`, recorded at 59772fce2. As planned, except the
band and the load's tail, in the table.

| item | value |
| --- | --- |
| scenario | `hack/benchmark/scenarios/guides/pd-disaggregation.yaml`, `MODEL_ID=Qwen/Qwen3-0.6B`, EPP flow control off, decode KV 1024 blocks and `VLLM_MAX_NUM_SEQ=64`, decode 1-1, **prefill 1-1** (ols-sweep had 1-2) |
| analyzer config | the policy lists `saturation` and `throughput`, `analyzerTuning.itlBand` 0.12-0.70, the rest default. Set it **before** the recorder: the throughput analyzer registers only at controller start (`config/config.go:425-437`) |
| recorder | the `learner-recorder` patch as the last change before load; default cap (256Mi) |
| load | `hack/benchmark/closed_loop_sweep.py`, default schedule, then a 48 s rise to 80 in flight, 8 min at 80 (cap of 64, the rest waiting), 35 min at 2 in flight: 98 min |
| restart | the leader Pod deleted 90 s into level 5 (1200 s, 20 min in), snapshot off, as for the slo pair. The first recording is `throughput-sweep`, the second `throughput-sweep-restart`. The run is taken at a commit with 4b-0 merged; the recipe is [testing.md](../developer-guide/testing.md#learner-goldens) |
| GPUs | 2 H200 for 2 h including setup; torn down on every exit path |
| counters above zero | throughput OLS and pinned cycles and settling readings in both (the wait after a controller start: no joiner is recorded, the one decode pod never scales); at-cap readings in the restart recording |

A kind or simulator run is not enough: the simulator's ITL has no warm-up
and no thrashing above the band, which are the readings this PR removes,
and only a real decode engine has reached a full fit (2.8).

Measured, by the replay's counters:

| recording | OLS | pinned | settling | at cap | held | above band |
| --- | --- | --- | --- | --- | --- | --- |
| `throughput-sweep` | 18 | 9 | 5 | 0 | 0 | 0 |
| `throughput-sweep-restart` | 10 | 14 | 8 | 20 | 0 | 0 |

The cap was expected to stay synthetic; it did not. 80 in flight held the
decode pod at 64 running with requests waiting, at mean k 0.77 to 0.90
(`KvUsageMean`, the k the line reads), and the band's top of 0.70 put those
readings above it. The raw rows give the same 20. By peak `KvCacheUsage` 22
rows qualify: on the rise, the 22:11:50Z and 22:12:05Z rows read in the band
at mean k 0.50 and 0.60. The preemption total reached 37 and 4 decode rows of
the restart recording show preemption, none of them among the 20. All 14
pinned fits of the restart recording come from those readings: with
`throughput-itl-admits-at-sequence-cap` off, pinned falls to 0 and 7
above-band fits take their place.

The held slope stays synthetic (2.8): it ran 0 times in both. A shape change
drops the held slope with the window, and through the 35 min of light load
the line was silent with cause `shape-change` from 22:23:06Z to the end. A
recording of the hold probably needs light load of the shape of the last fit.

#### Tests

- **Property cases (2.6), run on the throughput line.** The generator moves
  from D's tests into a test package both can import, with D's `cases.txt`
  unchanged. Cases: transient (joiner warm-up, load step, controller
  restart); first sample (one reading); stale value at N = 2 and 4 (B
  converges); missing and duplicated (+Inf ITL, k of 0, a missed row);
  edges (band edges at default and non-default, the cap and its preemption
  rate, `steadyFor`, `minSettledSamples`, 10 rows and spread 0.30); known
  truth at k = 0.85 (the throughput analyzer prices at `DefaultKSat`);
  contamination paired with heterogeneity (a slow replica moves A by its
  weight). Each mutant is a switch off. Cases the parent fails go in the
  known-failures ledger with 4b as owner, and leave it as each lands.
- **Closed-loop probe (2.5),** on a fleet simulator driving the throughput
  analyzer (the D simulator, moved beside the generator): the slope hold
  (a line learned at load, 35 min below the band, then a step back to
  need); a scale-up whose joiners carry warm-up ITL; a controller restart
  before a load step (under need for at most 5 min); one replica at its
  cap with siblings below the band. Each pastes its result on and off.
- **Verbosity:** the throughput package gets `replica_log_test.go`'s two
  tests (shipped-behaviour table).
- **Replay switches** (`learnreplay/switch_runs_test.go`): the four go in
  `replaySwitches`, with `switchPackage` learning `PackageThroughput`, and the
  outputs each moves, measured: the steady gate is expected to move
  `results.csv` and `variant-capacities.csv` of `acceptance`, `engine` and
  `mu-paths`, the composite files of `engine`, and both new goldens; the
  restart wait `acceptance`, `engine` and `throughput-sweep-restart`. The
  settle-tracker and cap switches are expected to move none, and go in
  `heldBy` with their negative controls. `switchBase` takes the base
  digests of every output named. 4a's and earlier rows stay in
  `earlierSeams`; each 4b PR moves its predecessor's rows there.

#### Order

| PR | contains | goldens |
| --- | --- | --- |
| 4b-0 | the admission's inputs, wired and not yet used: settle view, slope hold and interval in the admission; the capacity store wired but not read until 4b-1 (`EngineParams` only, then), wired in main (`Engine.NewThroughputAnalyzer`) and in replay; the policy read through interfaces (`itlPolicyOf`); `start.ThroughputConfig`, so a recording states the analyzer's settings and a replay builds it as the run did. The window still reads only the band | none move: every golden replays byte for byte |
| 4b-1 | the two recordings with base goldens (own commit, taken after 4b-0 merged); the shared generator and simulator; the verbosity test; `throughput-itl-steady-gate`, `throughput-itl-keeps-the-window-on-gps-mismatch`, `learner-write` | as named above |
| 4b-2 | `throughput-itl-reads-the-settle-tracker`, `throughput-itl-waits-after-controller-restart` | the restart wait's |
| 4b-3 | `throughput-itl-admits-at-sequence-cap` ([as built](#4b-3-as-built)) | none expected; none measured |

4b-1 can land on the synthetic goldens alone if the recording is late; the
recordings then land in 4b-2 with their base goldens. Forget for this line
(and mu, which waits for it, 4a's decisions) is not in 4b: a small PR of its
own after 4b-3.

Decided on the design: the real 2-GPU recording; the cap stays synthetic;
the shape-change clear is kept, holding B (it held the last A too until
4b-1's review dropped that: see [4b-1 as built](#4b-1-as-built)) (the GPS-mismatch
clear, first kept too, is dropped: see below); the
analyzer reads `EngineParams` from the store now, not after #345;
`T2-held` is a new `Reason`; the k-weighted pinned fit is in scope (the
exception section 7 names); the four-PR split.

#### 4b-1 as built

Status: built on `feat/learner-4b1`, on main after 4b-0 (#455). The
`throughput-sweep` recordings are not in it; they landed later in their own
commit, with `TestGoldens_ThroughputSweepFloors` on them (see
[Recordings](#recordings)).

What each switch moves, measured on the in-process goldens (`acceptance`,
`engine`, `mu-paths`, the only fixtures that run the throughput analyzer),
each switch on alone against all off; base f7b4f7e47, the same tree as main's
69fb6395c, so `switchBase` is unchanged by the merge:

| switch | outputs it moves | how |
| --- | --- | --- |
| `throughput-itl-steady-gate` | `results.csv` and `variant-capacities.csv` of all three; `composite-holds.csv`, `composite-release.csv` and `composite-variants.csv` of `engine` | each variant's first cycles have no throughput capacity (10 readings first), and the line after them is the window's pinned or full fit, not one cycle's rows. Since the review's above-band fallback, one more `mu-paths` row: `variant-02` at 09:00:45 is priced from above the band (`T2-above-band`, `TotalDemand` 540 where it was 0), the fixture's only fallback cycle |
| `throughput-itl-keeps-the-window-on-gps-mismatch` | `variant-capacities.csv` of all three | the in-process rows trip the GPS check; kept, the window reaches a full fit where the clear had emptied it |

What else 4b-1 built:

- **Shared test helpers.** `internal/engines/analyzers/internal/learnertest`
  holds what the saturation and throughput tests share: the seeded
  generators and the time-base family, the property judgement against a
  known-failures ledger, and the closed-loop fleet simulator
  (`learnertest.Fleet`). The saturation tests run on it unchanged: every
  case id in `cases.txt` and every closed-loop probe's result is the same
  as before the move. A test holds that no non-test file imports it.
- **Property cases on the throughput line** (2.6;
  `throughput/property_test.go`, ledger and case list under
  `throughput/testdata/properties`): transient (joiner warm-up, load step
  with an ITL overshoot, controller restart); first sample; stale B at
  N = 2 and 4; missing and duplicated (+Inf ITL, k of 0, a missed row);
  contamination paired with heterogeneity (one slower replica moves A by its
  weight); known truth at `itl.DefaultKSat`, direct and on the time base;
  edges (10 readings, spread 0.30, the band at the default and at
  [0.05, 0.90], `steadyFor` at the default and at 10 min, the sequence cap);
  and the above-band fallback never pricing under the truth (below).
  The stale-B case's 15 % is its declared tolerance: on the tracker the
  converged B's error has mean about 0 and SD about 3.6 %, so 15 % is about
  4 SD. The mutant is the steady gate off: joiner warm-up, load step, one reading,
  +Inf ITL, ten readings and both `steadyFor` edges fail on the parent
  (`TestThroughputPropertyMutants`). One ledger row: the sequence cap, owner
  4b-3.
- **The above-band fallback** (part of `throughput-itl-steady-gate`;
  `itl.Admission.AboveBandFallback`, which only this line sets). A key with
  no line from the band and no slope it may hold -- one that never fitted,
  under load that holds every replica above the band -- fits A from its
  readings above the band at a k of at least `itl.DefaultKSat` (0.85), under
  the same steady gate, settle stamp and floors (10 readings, 3 settled),
  with B pinned at the least ITL among those readings (`Reason`
  `T2-above-band`). The band's first fit replaces it; it is never used while
  the band gives a line or a slope is held, and its A is never held nor its
  B stored as the key's intercept. Without it, a controller restart 15 s
  before a step into saturation left the restarted line silent for as long
  as the load stayed up, and with throughput alone the fleet stayed at 4
  against a need of 9.

  It is one-sided by construction: it never prices a higher mu than the
  truth. Every reading is ITL = B' + A k + noise with A >= 0 and k > 0, so
  the least ITL is at or above the true B'. On readings of the true line,
  the line through that B prices, at kSat, A kSat + B' + (B - B')(1 -
  kSat/kbar), kbar being the readings' mean k. Both factors of the last term
  are >= 0: the first because B >= B', the second because every reading's
  k is at least kSat. That second condition is why readings between the
  band's top (0.80) and 0.85 are not kept. Overloaded readings, which sit
  above the line, only raise the price. The strict minimum is used, not a
  low quantile: above the band the gauge's noise (about 1 ms) is small
  against A k (40 ms or more). Measured on the property case
  `known-truth/throughput-itl/above-band-fallback` over 100 seeds, the
  priced ITL is at least 2.5 % over the truth at a true B of 4 ms and 2.0 %
  over at 10 ms. Negative control: with B pinned at the 6 ms default, as the
  band's fit pins a key that never fitted, the 10 ms case prices under the
  truth on every seed.
- **A shape change drops the held slope.** The clear empties the window and
  now forgets the held A too (`itl.Tracker.ForgetSlope`), on this line only:
  shorter requests have a higher A, and the old shape's A held into them
  priced a lower ITL, so fewer replicas, until ten new readings arrived.
  Until the new shape fits, the line is the above-band fallback or silent.
  The saturation analyzer's clear is unchanged.
- **One summary line when the line goes silent or comes back,** at the
  default level, as the GPS summary: `throughput analyzer: ITL line` with
  `silent`, the `cause` of a silence (`no-healthy-rows`, `fit-declined`,
  `shape-change`, `all-above-band-below-ksat`, `restart`,
  `too-few-readings`, judged in that order), the `source`, and the cycle's
  rows in the band, above it, below it and settling, and what the band's
  and the above-band windows hold. A cycle with no healthy row reads
  nothing and has no line; before the round-2 review it priced from the last
  cycle's reading, for as long as the rows stayed unhealthy. The reference
  pages say what each `cause` means and what to do
  ([cycle log](../reference/cycle-log.md#reason-values-reason-field),
  [troubleshooting](../reference/troubleshooting.md#symptom-index)). A
  GPS mismatch the log opened is closed when the line goes silent or the
  variant has no shape, and a NaN GPS error (an infinite observed rate) is
  no mismatch.
- **Closed-loop probe** (`throughput/closed_loop_probe_test.go`), each
  case run on the tracker and on the parent's line: the slope hold through
  35 min of light load (on: settles at need, the line held 39 cycles;
  parent: runs to the replica cap, ends 6 against a need of 3); a scale-up
  whose joiners carry warm-up ITL (on: peak 6, need 6; parent: peak 33); a
  restart 2 min before a step (on: settles at need; parent: peak 40, ends
  over); a restart 15 s before a step that pushes every replica above the
  band (on: need 9, final 10, under need for 30 s, 7 cycles priced from
  above the band; with the fallback off, the negative control, final 4 and
  under need for 24m45s of the 25 min after the restart, as before the
  fallback). A replica at its cap with siblings below the band: the band
  still admits nothing (4b-3), and the fallback prices the variant from the
  capped replica's readings after its tenth.
- **Verbosity** (`throughput/replica_log_test.go`): the shipped-behaviour
  row's two tests. At the default level a run does not log more lines at 64
  replicas than at 8 (1 line at 64, the variant's GPS summary; 3 at 8,
  where the line is silent for its first cycles and logs that and its
  return); the parent's line logs 76 and 616. No line passes the CRI limit
  at the default or the verbose level.
- **Per-path counters** (`throughput.PathNames`): `throughput-itl-ols`,
  `throughput-itl-pinned`, `throughput-itl-held`,
  `throughput-itl-settling`, `throughput-itl-above-band` and
  `throughput-itl-at-cap`, the above-band path not required of any golden (a
  recording reaches it only by starting into saturated load). The golden
  check requires the first three (the in-process goldens reach each);
  settling and at-cap are zero there by construction and are required by
  `TestGoldens_ThroughputSweepFloors`: full, pinned and settling counts on
  both `throughput-sweep` recordings and at-cap counts on the restart
  recording, with the parent's line (the steady gate off) as its negative
  control. Held has no floor there: neither recording reaches it.

#### The GPS-mismatch clear: dropped in 4b-1

**Decision: the throughput line's window is no longer cleared on a GPS
mismatch.** The mismatch is still computed and logged, and the line keeps
its readings. It is a named change of its own in 4b-1, behind
`throughput-itl-keeps-the-window-on-gps-mismatch`, apart from the steady
gate. This revises the earlier "kept" for this clear; the shape-change
clear stays, through `Tracker.ClearWindow`.

**What it does today.** Each cycle `checkVariantGPSMismatch`
(`T/analyzer.go:855-939` at f7b4f7e47) compares, per replica at an instant k
of at least 0.30, the rate the line predicts, `k·C / KVreq / ITL(k)`
(`itl.TokenRate`), with the replica's `GenerationTokenRate`. Over 15 %
(`itl.DefaultGPSMismatchThresholdPct`) on any replica is a mismatch. Three
cycles in a row clear the variant's window (`consecutiveGPSMismatches`,
`T/analyzer.go:441-455`, `DefaultGPSMismatchClearThreshold`); B is kept.

**Why it existed, and why that is gone.** It came with the analyzer
(efca1b4cf, #1250). There a mismatch also suppressed spare capacity, and the
stale readings kept that suppression on for up to the window's 30 minutes;
the clear was added "to break calibration lock". The same PR then dropped
the suppression: `anyGPSMismatch` is a placeholder no output reads
(`T/analyzer.go:528-529`). Nothing is locked any more, so the clear only
throws readings away.

**It trips on a correct line.** On the committed recordings, the check was
run per Ready row with a known generation rate and ITL, at k >= 0.30, with
the replica's own observed ITL as the line: the best line there can be, so
every error left comes from the footprint model `k·C / KVreq` and the time
base, not from the fit. A clear is three such rows in a row on one pod.

| recording | rows | over 15 %, instant k (as today) | clears, instant k | over 15 %, minute-mean k | `k·C/KVreq` vs running, median / p90 |
| --- | --- | --- | --- | --- | --- |
| `e1` (aggregated decode, steady shape) | 596 | 7.4 % | 9 | 0.8 % | 2.5 % / 8.6 % |
| `e1-tuned` | 564 | 8.2 % | 10 | 1.4 % | 2.5 % / 8.5 % |
| `ols-sweep` (P/D decode, steps of load and shape) | 95 | 49.5 % | 13 | 36.6 % | 8.6 % / 246 % |
| `ols-sweep-slo` | 27 | 40.7 % | 2 | 7.1 % | 9.9 % / 24 % |
| `ols-sweep-slo-restart` | 66 | 53.0 % | 10 | 47.9 % | 26.7 % / 293 % |
| `pd-restart` (decode at its cap, queued) | 8 | 100 % | 2 | 100 % | 3069 % / 4283 % |

On a steady aggregated fleet a perfect line still clears the window about
ten times a run, from the instant k alone against a minute's rate. On a P/D
decode fleet the footprint model itself misses: across a step of load or
shape, and above all with requests whose KV landed and that wait to be
scheduled (`pd-restart`), the KV holds far more than the running sequences
`k·C / KVreq` assumes. A check that cannot pass on the true line is no test
of the line, and the clear it triggers is what kept the 4b-0 differential
test's window from filling until its rows dropped the generated-token rate.

**What 4b-1 does instead.** The comparison stays as a diagnostic, on the
minute-mean k where it was read (`KvUsageForITL`), and its per-pod INFO line
moves to the debug level with one summary per variant (the verbosity test,
shipped-behaviour table, would fail a per-replica line at the default
level). The window keeps its readings: a line that really moved is refitted
by the steady gate and the window's age and size, which is what row 1 does
and what no GPS signal ever told it. Kept in the switch-off path, the clear
is as today.

**Negative control.** A fleet whose generation rate disagrees with a
correct line by 30 % for three cycles: on the parent the window empties
(its sample count falls to 0); with the switch on it keeps every reading.
The step 4 count of the throughput recording (the recipe) is the run-level
check: with the switch on, the replay must keep at least as many full fits
as there were clears off.

The recipe keeps its step 4 grep: the recording is taken on 4b-0, where the
clear still runs, and its count of clears is the evidence above, measured
on the line itself.

#### 4b-2 as built

Status: built on `feat/learner-4b2`, on main after 4b-1 merged (#456,
1c9a34fe8). The line reads each replica's settle from the engine's
settle tracker (`T/settle.go`), which 4b-0 wired (`WithSettleView`) and
4b-1 withheld. An analyzer given no view -- a test that drives `Analyze`
alone, a replay of a recording with no `start.ThroughputConfig` -- observes
a tracker of its own, as the slo analyzer does, and stands for a controller
that has watched the fleet: nothing waits after a restart.

**The switches, split as 4a split them.** The table above names one switch
for the move, with "restart under its name, idle on evidence, the absence
tolerance, the Finite pod mean" inside it. Each of those moves the fleet on
its own, and 4a's review split them out for the saturation and slo lines
(`itl-restart-under-its-name-settles` and `itl-idle-needs-evidence` in
9b52db281). 4b-2 takes the same six, prefixed `throughput-`:

| switch | changes against 4b-1's own trace | which way the fleet can move |
| --- | --- | --- |
| `throughput-itl-reads-the-settle-tracker` | the settle start is the tracker's: joined, back past the tolerance (`settle.Rejoined`), lent, a new revision under the same name (`settle.RolledOut`), Ready again after a not-Ready row. Its own differences: a gap between two of the model's cycles past the tolerance (a paused controller, a clock jump) settles every replica, and a rollout that keeps the Pod's name settles; the own trace saw neither | fewer readings after a gap or an in-place rollout: **silent more often** (the risky side, 4.4), and a rollout's warm-up kept out: **down** |
| `throughput-itl-waits-after-controller-restart` | the engine's tracker is new with the controller and reads every replica on its first cycle as `settle.Joined`; on, those replicas wait `steadyFor` from that sight. Off, as 4b-1, they are taken as settled. Only with an engine view and on the tracker (the move on): the analyzer alone never waits, nor does the line's own trace. The above-band fallback's readings do not wait (round-1 review, below) | **silent** for `steadyFor` plus the ten readings in the band (cycle 8 on a three-replica variant at 15 s); a joiner whose warm-up straddles the restart is kept out: **down** against the parent's over-order |
| `throughput-itl-keeps-the-settle-through-a-missing-row` | one missing row keeps the settle up to the tolerance (2.5 intervals, at least 1 min). Off, as 4b-1, the replica starts over | more readings, **live more often**; a replica restarted inside the tolerance with nothing to show it (no not-Ready row, no new Ready time) is admitted while warm: **up**, the cost side |
| `throughput-itl-restart-under-its-name-settles` | a new Ready time under the same name settles. Off, only a not-Ready row shows the restart | a fast restart's warm-up kept out of A: **down** (the parent over-orders) |
| `throughput-itl-idle-needs-evidence` | idle only on evidence (KV known 0, and ITL known-empty or generation rate known 0; prefill by its request rate). Off, a row with neither ITL nor k is idle | a failed scrape no longer settles a busy replica: **live more often**; an idle replica whose gauges are unknown is not idle, so its return's warm-up is admitted: **up** |
| `throughput-itl-reads-the-finite-pod-mean` | a Pod with an Absent rank is no reading. Off, its Finite ranks' mean was one | fewer readings: **silent more often** on a fleet of such pods; and **either way** on the line: the pod's fill counts the Absent rank and its ITL does not, so the reading sat left of its line (an idle rank, A too steep, over-order) or right of it (a busy rank, A too flat, under-order) |

**A lend moves nothing on this line.** A warm-pool lend is a new key on the
tracker (`settle.Lent`), but the analyzer drops lent rows before the line
(`ownReplicasOnly`) and `itl.Tracker` refuses a `FromWarmPool` row, on either
path (`TestITLSettle_ALentPodIsNoReadingOnEitherPath`). **A rejoin** past
the tolerance settles on either path, as 4b-1's own trace with the absence
rule did (`TestITLSettle_AnAbsencePastTheToleranceStartsTheSettleAgain`);
the move's only rejoin of its own is the gap between cycles above.

**`throughput-itl-waits-after-controller-restart` is 4b-2's**, as the Order
table has it, not later: it is defined against the engine's view, which
4b-2 is the first to read, and with the view and the move on, leaving it
off would read a controller restart as a fleet that was watched.

Negative controls, each through `Analyze` on the engine's rig
(`T/settle_test.go`), run against 4b-1's tip with the switch names shimmed:
the restart, settling cycles `nil` against `[0..7]`; one missing row,
`[11..18]` against `[0..7]`; a new Ready time under the name, `nil` against
`[0..7, 10..17]`; a new revision under the name, the same; a 3 min gap
between cycles, the same; a scrape that read nothing, `[12..19]` against
`[0..7]`; a Pod with an Absent rank, 3 readings offered against 2. Each test
also runs its switch off on this tree and asserts the parent's result.

Property cases (`T/property_settle_test.go`, on the engine's rig, a new
tracker per controller): `transient/throughput-itl/controller-restart-with-a-joiner`
(a replica joins two cycles before a controller restart, its warm-up lasting
six cycles) fails on 100 of 100 seeds with the wait off and on 4b-1's tip
(seed 1: priced 0.2698 s against 0.0564 s, 378.6 %), and passes with it on;
`joiner-warm-up-on-the-engine-tracker` and
`missed-row-on-the-engine-tracker` pass on either path and are guards.

Closed-loop probe (`TestThroughputProbe_AControllerRestartOnTheEngineTrackerWaitsItsSettle`),
the restart case of 4b-1 on the engine's rig, need 9: a step 2 min after
the restart, final 10, under need for 30 s, silent 10 cycles (2.5 min)
from the restart on, against final 9, 0 s and 2 cycles with the wait off;
a step 15 s after, which pushes every replica above the band inside the
wait, final 10, under need for 30 s, silent 3 cycles (45 s), the same as
with the wait off. Both inside the probe's 5 min, on the SLO side as 4.4
says. Before the round-1 review the 15 s case was under need for 1m45s and
silent 8 cycles (2 min): the restart wait held the above-band fallback too.

**Goldens.** The in-process three predate `ThroughputConfig`, so they give
the throughput analyzer no engine view and the restart wait moves none of
them; its unit tests hold it (`heldBy`). The `throughput-sweep` recordings,
which landed later, have the view. There it moves no output file, but every
settling reading they hold rests on it: off, `throughput-itl-settling` falls
5 -> 0 and 8 -> 0, and `TestGoldens_ThroughputSweepFloors` replays that.
Measured after the round-1 review, each alone
against all off from main's goldens: none of the six moves a golden, and
all on reproduces main's goldens byte for byte, so each is held by its unit
tests. Before the review the move moved `acceptance/variant-capacities.csv`
(3 of 149 rows changed bytes) and `engine/variant-capacities.csv` (2 of
139, where this section said 1): on the cycles
after a clock jump past an hour, model-01, the first model analyzed, read
the gap on the analyzer's own tracker as an absence (`settle.Rejoined`) and
held its slope (`T2-held`) where the key's own trace refitted (`T1-ols`).
That was the eviction order, not the move (S3 below). The acceptance
fixture no longer reaches the held slope, which the engine fixture alone
reaches, as in 4b-1. The engine's own recording test
(`steadystate/learner_recording_test.go`), whose throughput analyzer has the
engine's view, prices 8 variants where 4b-1 priced 21: the band's line waits
after the controller start and after each day the clock jumps, as the slo
line does, and the 4 of the 8 that are pd-d's are priced from above the
band (`T2-above-band`) inside that wait. Before the review it priced 4.

**Round-1 review.**

- **S1: the above-band fallback does not wait after a restart.**
  Decision: the restart wait stamps only the band's readings. A reading
  the above-band window keeps is stamped as settling only for a settle that
  is not the restart's (`itl`'s `aboveBandSettling`): a joiner, a return
  after idling, or a replica that became Ready less than `steadyFor` before
  the cycle, by its Pod's Ready time, so a pod that just became Ready is
  still kept out. Why: the fallback pins B at the least ITL among its
  readings and prices at a k of at least 0.85, so it never prices a higher
  mu than the truth (4b-1 as built); a warm-up reading of a replica that was
  serving before the restart sits above the line and can only raise its
  price. Waiting `steadyFor` for it bought nothing and kept the line silent
  under need. Measured on the probe above, 15 s case: under need 1m45s and
  silent 8 cycles with the stamp, 30 s and 3 cycles without. Negative
  control: with the stamp put back, the probe returns to 1m45s and fails.
  The property case `known-truth/throughput-itl/above-band-fallback` (no
  engine view) still holds on 100 of 100 seeds; a new engine-rig case,
  `known-truth/throughput-itl/above-band-fallback-after-a-restart`, runs the
  same fleet from a restart, its first two cycles 30 % over the line, and
  holds the one-sided margin on 100 of 100 seeds with the fallback pricing
  inside the wait. With the stamp put back it fails on 100 of 100 (the
  fallback first prices on cycle 8). `itl`'s
  `TestTracker_AboveBandFallbackDoesNotWaitAfterARestart` holds both sides:
  cycle 3 with the replicas Ready an hour before, cycle 7 with them Ready
  15 s before the restart, on the key's own trace and on the tracker.
- **S3: the analyzer's own tracker after a long gap.** The own tracker
  forgets a model not analyzed for `ownSettleTimeout` (1 h), inside
  `observe`, after the model analyzed first that cycle has observed. So
  after a gap past an hour that model read the gap as an absence and
  settled, and every later model, evicted meanwhile, was first-seen and
  did not: the settle read depended on the order. `observeOwnSettle` now
  makes a model last seen more than `ownSettleTimeout` ago first-seen to its
  settle reading whatever the order: it forgets the model's observations, observes it backdated, and
  drops its line keys' replica traces (`itl.Tracker.ForgetTraces`), keeping
  their windows and held slope, as the eviction keeps them for that model.
  Part of the move: off, the line reads no tracker. Test:
  `TestITLSettle_AfterALongGapTheAnalyzerAloneSettlesEveryModelAlike`, two
  identical models and a 2 h gap; before the fix the first settled on
  cycles 8 to 15 (`pinned-B` for 11 cycles) and the second never. A no-op
  `observeOwnSettle` fails it and four other `TestITLSettle_` tests. The
  goldens came back to main's (above).
- **The slo analyzer had the same logic, fixed by issue #459** (`slo/settle.go`:
  first-seen only on a model's first sight, forgotten after `stateTimeout`).
  Its eviction runs only from `EvictStale`, which the engine and the replay
  call at the top of the cycle, so there every model reads a long gap
  alike. A caller that never calls it, or calls it between two models'
  `Analyze` calls, got the order or the gap read as an absence. Its
  `observeOwnSettle` now makes a model last seen past the timeout
  first-seen, as here, behind its own switch
  (`slo-itl-own-settle-reads-a-long-gap-as-first-seen`, read with the move
  on), since 4a's switches and goldens are already merged. It moves no
  golden. Only the settle reading is order-independent: the line's
  windows, intercepts and held slope are still swept by `EvictStale`, so
  the fit after a long gap still depends on whether and when it ran.
- **N1: the restart wait reads the tracker only.** `WaitsAfterRestart` was
  set with an engine view whatever the move said, and on the key's own
  trace it made every replica of a key made anew wait `steadyFor`, not only
  a controller restart. It is now set only with the move on
  (`throughput.settleAdmission`). Four `TestITLSettle_` tests whose switch-off
  runs read the own trace now expect no restart settle there, and
  `TestITLSettle_TheOwnTraceDoesNotWaitAfterARestart` fails with the old
  setting. The saturation and slo analyzers set it as before.
- **N2:** `WithOptimizationInterval` sets the interval of the analyzer's own
  tracker too, so its absence tolerance follows the controller's interval.

**Round-2 review.** The own-tracker path after a long gap now treats a
model the tracker does not hold (never seen, or evicted inside another
model's `observe`) the same as a stale one, and forgets the traces of
every line key of the model by prefix, absent variants included, all under
`a.mu` and all behind `throughput-itl-reads-the-settle-tracker`. The order
and absent-variant tests are guards: the round-1 code also passed them,
because `observe` evicts a variant state at the same timeout and the line's
keys with it; the new path does not rest on the two timeouts staying equal.
Mutants killed: the stale path with the switch off
(`TestITLSettle_AfterALongGapWithTheMoveOffTheOwnTraceIsKept`, an idle run
before the gap that 4b-1's own trace keeps), the eviction keeping the
model on the tracker or keeping its stamp
(`TestITLSettle_TheEvictionForgetsAModelOnTheOwnTracker`), and no
`ForgetTraces` (four tests). The probe figures the reference pages quote
are pinned exactly, the control included (`itl.Admission.AboveBandWaitsAfterRestart`,
set only by negative controls): in the band, 30 s under need and 10 silent
cycles; above it, 30 s and 3; the stamp put back, 1m45s and 8. On the
settle rig, three steady replicas price first on cycle 8 after a restart
and one replica on cycle 10, not 8 + 10: readings taken during the settle
count towards the ten, and only three must be settled.
### PR 5 design: start time and the snapshot

Design for the row 5 above, checked against main f82a55480. D is
`internal/engines/analyzers/saturation`. Numbers marked *unmeasured* are
choices, not results.

#### What exists today

| part | where | what it does |
| --- | --- | --- |
| Sample | `internal/collector/pod_serving_state.go:220-245` | Pod Ready `LastTransitionTime` minus creation, 0 when not Ready or outside (0, 3600] s. A readiness blip re-stamps the transition, so an old pod can report its age |
| Fold | `D/start_est.go:86-206`, called once per variant per cycle (`D/analyze.go:496`), after a restore is installed (`D/analyze.go:50`) | Each pod once (`startSeenPods`, key `itlKey\|namespace\|pod`, `:119`), bridges skipped. The first sample **replaces** the prior (`:171`); later ones fold by EWMA α 0.3 (`:42`, `:165`). A sample over 3x the estimate is refused (`:54`, `:151-158`) until 3 refusals, counted **per refusal**, not per pod (`startOutliers`, `:62`) |
| The 70 s | `DefaultReplicaStartSeconds`, `D/start_est.go:35`, applied at `:190-191` | Reaches **only** the gauge and the `replica-start-measured` log. Sizing never reads it: `startSecondsPerRole` skips a variant with no measurement (`:263-266`) |
| Floor | `D/throughput_floor.go:519-521`, `:556`, `:627`; `internal/signals/floor/floor.go:250`, `:664` | Min over the role's measured variants, only while the arrival rate is a true arrival rate. Projects the backlog over the start (`backlogAtLanding`) and caps engine queues over `DrainHorizon`. No measured variant: no projection, `startSource: none` (`:737-751`) |
| Share pacing | `D/analyze.go:142` (`RoleStartSeconds`, max per role), `steadystate/composite.go:146`, `allocation/utilization_share_groups.go:908`, `allocation/utilization_share_timing.go:49`, `:113` | Slowest measured start, capped at 30 min. None measured: `shareModelLoadEstimate`, 5 min (`:14`) |
| Warm pool | `internal/warmpool` | Does not read it. Bridges are excluded from the fold |
| Seed | `registry/scalermeta.go:102-122` | `replicaStartSeconds` is declared, not parsed by `ParseMeta` (`:192`), read by nothing |
| Persistence | `D/learned_snapshot.go:118` (`startSeconds`), export `:605`, restore `:1410-1413` | Estimate per key only, installed when the key has none. `startSeenPods` and `startOutliers` are working state (`D/learned_maps.go:100-101`): after a restart every running pod is folded again |
| Metrics | `internal/metrics/metrics.go:1203`, `:1218` | `wva_replica_start_seconds` histogram; `wva_replica_start_seconds_estimate{source=measured\|seed}`, where `seed` today means the 70 s default |

So the row's "every estimate starts from 70 s" is true of the gauge only. A
variant with no measured start is not projected at all.

#### The changes

Each is a named change behind its own replay switch (2.5), on in the
constructor. The prior P is the seed when the ScaledObject sets one, else 70 s.
N = 3 distinct pods, and a seed's bounds are [S/2, 2S]. Both are named
constants in `D/start_est.go` (`startConfirmPods`, `startSeedLowFactor`,
`startSeedHighFactor`) whose comments say they are *unmeasured*: N matches
today's 3-refusal guard and run T's 4-5 pod scale-ups. An LWS group counts
once, as its leader's row.

Decided (review of this design): N = 3 and [S/2, 2S]; unseeded, unmeasured
variants stay unprojected; the gauge's sources become `default`, `seed` and
`measured`; a bad seed is ignored and logged, never a frozen model; the frozen
decoder pins v0.10.1; sizing never uses a start shorter than measured (C); a
kind recording, a GPU witness only later and optional; the split below.

**A. Read the seed** (`start-reads-replica-start-seed`). `ParseMeta` parses
`replicaStartSeconds` as a number in (0, 3600]. A bad value is ignored, as if
unset, and logged once each time the value changes; it is never a trigger
error: today any value is accepted, and a rejected trigger freezes the model. The engine puts it on
`domain.VariantReplicaState` (`StartSeedSeconds`), as `modelOutputSeed`
(`steadystate/engine.go:1105`) does for output tokens, so the recorder carries
it. A seeded variant with no measurement now reaches the floor and share
pacing at the seed.
- Effect: a seeded, unmeasured role is projected over the seed (was: not at
  all), so the floor can order **more** during its first scale-up, and share
  pacing uses the seed instead of 5 min (shorter or longer). Unseeded
  variants do not move: with no seed and no measurement a role stays
  unprojected, and the 70 s stays out of sizing. `startSource: seed` on
  `throughput-demand-floor`.
- Gauge `source` on `wva_replica_start_seconds_estimate`: `default` (the
  70 s, which today is labelled `seed`), `seed` (the ScaledObject value) and
  `measured`. `seed` changes meaning, so the PR rewrites its row in
  [prometheus.md](../reference/prometheus.md) and adds an upgrade note to
  [upgrade and uninstall](../reference/upgrade-and-uninstall.md): a query on
  `source="seed"` now finds only ScaledObject seeds.
- Negative control: seed 120, no start measured, arrival rate > 0. The
  parent projects nothing (`startSource: none`); the PR projects over 120 s.

**B. Confirm by distinct pod** (`start-confirms-by-distinct-pod`). A refused
outlier is remembered per key as a run of pods. The guard opens when N
distinct pods across the run are over 3x; one pod repeating never opens it.
Pods that left stay in the run (as first written, the set was pruned with
the pods; review of 5b found that a variant with one or two replicas then
never learned a slowdown). The guard compares a sample with the measured
part of the estimate only, never with P. As built, see "5b as built".
- Effect, both directions: one pod with a readiness blip no longer lifts the
  estimate on its fourth cycle (**down**); a confirmed change installs the
  run's median rather than folding one pod at α (**up**, to the new level);
  and until N distinct slow pods are seen, a real slowdown is not followed
  at all, where the parent followed it after 3 refusals (**down**, by up to
  the change, for those starts). `replica-start-outlier-rejected` gains
  `distinctPods` and `needed`.
- Negative control: estimate 70, one pod reporting 2700 s on 4 cycles. The
  parent folds it on cycle 4 (about 860 s); the PR keeps 70.

**C. Blend the first samples** (`start-blends-first-samples`). With n < N
distinct pods measured, mean m:

    estimate = ((N - n) * P + n * m) / N

With a seed S it is also held in [S/2, 2S] while n < N. With no seed it is
never clamped. From N pods on, the estimate is m and later pods fold by the
EWMA as today. The cases (2.6):

| case | n | estimate | must hold |
| --- | --- | --- | --- |
| seed 120, first sample 15 | 1 | 85 | in [60, 120] |
| seed 120, first sample 460 | 1 | 233.3 | in [120, 240] |
| seed 120, first sample 3600 | 1 | 240 (held) | in [120, 240] |
| no seed, first sample 15 | 1 | 51.7 | strictly in (15, 70) |
| no seed, first sample 463 | 1 | 201 | strictly in (70, 463) |
| no seed, 3 pods at 463 | 3 | 463 | equals the measurement |

The blend is the **published** estimate: the gauge, the log and the cases
above. The readers that size, the floor's backlog projection and share
pacing, take **max(blend, m)** while n < N, so sizing never uses a start
shorter than one measured. From N pods on they read the estimate. A seeded
variant with nothing measured is sized at the seed.

| reader case | n | published | sized at |
| --- | --- | --- | --- |
| no seed, first sample 460 | 1 | 200 | 460 |
| no seed, first sample 15 | 1 | 51.7 | 51.7 |
| seed 120, first sample 460 | 1 | 233.3 | 460 |
| seed 120, first sample 15 | 1 | 85 | 85 |

- Effect: estimates no longer take one pod raw, and 5c can only
  **over-order**, during a variant's first N starts: a fast variant (15 s) is
  projected over the blend, longer than measured, so the floor orders more and
  share moves wait longer; a slow one is sized at its measurement, as on the
  parent. `replica-start-measured` gains `pods`, `prior`, `priorSource`
  (`default` or `seed`) and `sizedSeconds`.
- Negative controls: no seed, one sample of 15 s: the parent's estimate is
  15, the PR's 51.7. No seed, one sample of 460 s: a reader that takes the
  blend alone sizes at 200 and fails the reader case.

**D. Counted pods survive a restart** (`start-counted-pods-restored`). The
snapshot keeps, for each counted pod, its Ready time. After a restart a pod
whose row carries the same Ready time is counted already and is not folded
again. A pod with no Ready time, or another one, is folded as today.
- Effect, snapshot on only: the restored estimate is no longer pulled toward
  the running pods' starts at α 0.3 each, and a pod that blipped before the
  restart is not read as a new start. Small; either direction.
- Negative control: restored estimate 70, four running pods reporting 90 s
  with their stored Ready times. The parent ends near 85; the PR stays at 70.

**E. First sight survives a restart** (`settle-first-sight-restored`, the
settle package's first switch, with its own hook). The snapshot keeps the
settle tracker's sighting per model and replica: `FirstSeen`, `ReadySince`,
`ReadySeen` (`internal/signals/settle/settle.go:209-246`). On restore a
sighting is installed only when the replica's first row is Ready with the same
non-zero Ready time. An engine restart, an LWS group restart, or a new lend
changes it, and that replica waits as today. Idle runs are not kept: a return
of load during the restart is missed, as today.
- Effect, snapshot on only: after a controller restart only replicas with no
  record wait (k2 1 min, ITL `steadyFor`, mu behind them). The k1 pricing
  after a restart (4.4) ends sooner, so the fleet is sized **lower** than on
  the parent during that window. (Measured, that is one of two cases, and
  with no k2 on record it sizes higher: see "5d as built".) `learner-start`
  counts restored sightings, and `settleUntil` covers only the cold replicas.
- Negative control: four replicas Ready for 1 h, controller restart with the
  snapshot on. The parent refuses every k2 reading (`P1-obs-unsettled`) for
  1 min; the PR admits them on the first cycle.

All five: start time joins `learn` as learner `start`, with `learner-write`
at V(3) and `restored: true` until its first admitted sample after a restart
(6.1). The "Learned state: snapshot restored" line adds `startKeys` and
`sightings`. Metrics: no new series; the gauge's `source` values become
`default`, `seed` and `measured` (A).
The closed-loop probe runs C for true starts of 15 s and 463 s (the
`liveness/*/closed-loop-*-start-{15,90,463}s` property cases,
`closed_loop_probe_starts_test.go`), each held to the true start
published, a peak ceiling of 1.5x need plus one, and need at the horizon.
The probe reports the replicas still starting, as the engine does, so
the floor credits them. At 15 s and 90 s every case passes (the 90 s
decode step peaks at 24 for a need of 17). At 463 s four cases fail,
ledger rows owned by issue #520: the decode step, alone or through the
snapshot, holds 22 and ends at 21, over the 20.5 bound; after a restart,
before mu is back, it climbs to 29 and ends at 23; and the aggregated
load step peaks at 16 for a need of 9. It runs E through
a restart, and the PR pastes both.

#### Persistence and back-compat

The document stays v1. New fields, all `omitempty`:

| field | in | holds | an old document without it |
| --- | --- | --- | --- |
| `startSamples` | `LearnedState` | per key: n (capped at N) and mean m | n = N: the estimate keeps full authority, as before. No fleet moves on upgrade |
| `startAdmittedAt` | `LearnedState` | per key: last admitted start (the deferred last-admitted stamp) | zero: as old as can be |
| `itlBaselineAdmittedAt` | `LearnedState` | per ITL key: when its baseline B was last admitted (#495, [as built](#bs-last-admitted-stamp-as-built-495)) | zero: as old as can be |
| `startCounted` | `LearnedState` | per counted pod (`itlKey\|ns\|pod`): its Ready time | every running pod is folded again, as today |
| `settle` | `learnedStateDoc`, beside `state` | per model: each replica's key and its three sighting times | every replica waits, as today |

- `startSeconds`, `priorK2` and `k2BorrowedSince` keep their names and are
  still written. The seed is never stored: it is read from the ScaledObject.
- The per-pod fields are the only ones that grow with replicas (about 150 B
  each). They are spare entries in `learnedStateEntries`
  (`steadystate/learned_state_store.go:581`), dropped first at the budget:
  a dropped one costs one replica one wait. Their times are fixed per
  sighting, so they enter the digest without rewriting every minute.
- Forget drops the new maps with the rest (`D/learned_maps.go` lists them).
- Fixtures: a v0.10.0 document (no `itlSlopes`) and a v0.10.1 one restore
  through the current path; the light-load restore keeps the ITL line for the
  v0.10.1 one.
- Rollback: `json.Unmarshal` (`learned_state_store.go:243`) ignores unknown
  fields, so v0.10.x reads PR 5's document and drops the new ones.

**The frozen decoder.** v0.10.1 is commit 760f76d76; v0.10.2 has the same
blobs for every file below, so one copy covers both. Built from git alone:

    git show v0.10.1:internal/engines/steadystate/learned_state_store.go         # learnedStateDoc, learnedStateWriter (blob d1f8d339c7a0)
    git show v0.10.1:internal/engines/analyzers/saturation/learned_snapshot.go   # LearnedState and its *Snapshot types (b8277be9e29d)
    git show v0.10.1:internal/engines/analyzers/saturation/forget_variant.go     # ForgetMarkSnapshot (d10d6a82f7cf)
    git show v0.10.1:internal/signals/capacity/rolling_snapshot.go               # RollingAverageSnapshot (a3157ef25c84)

Copy the struct declarations only (fields and tags, no methods, the embedded
`RollingAverageSnapshot` kept embedded), with a `v0101` prefix, into
`steadystate/learned_state_v0101_test.go`, its comment naming the tag, the
commit and the blobs. The test encodes a PR 5 document with every field set
through the production encoder, decodes it into the frozen type, and asserts
every field the frozen type knows comes back equal. A second assertion pins
the frozen type's JSON tag set to a literal list, so an edit to the copy
fails. Negative control: renaming `startSeconds` or dropping the `priorK2`
write fails it.

#### Recording

Start time is a pod-lifecycle signal: Pod creation to Ready. kind with the
inference simulator gives real samples, so **no GPUs are needed**. The
measured GPU distribution (15 s to 463 s) is already in the start-time
reference and enters as property cases.

From a build with PR 5a merged (the seed is in the recorded input), on kind,
with the recorder on, `LEARNED_STATE_SNAPSHOT=true` and `--v=3`:

1. One model, two variants: `fast`, readiness at about 20 s; `slow`, about
   240 s, its ScaledObject setting `replicaStartSeconds: "120"`.
2. Load that takes each variant to at least 4 replicas: N + 1 distinct
   starts each. About 15 min.
3. Optional: restart the engine process in one long-running pod (a container
   restart under the same name, the blip case). Skip if the simulator image
   cannot; the synthetic suite covers it.
4. When the ConfigMap holds both keys, copy the recordings out, then delete
   the controller pod under load. `kubectl rollout restart` alone loses them:
   the recording volume is an `emptyDir`.
5. 5 min after the restart, drop to light load (issue #316) and restart again
   the same way.
6. One more scale-up per variant after the second restart.

Capture: every recording; the ConfigMap before and after each restart; each
controller pod's full log to a file; `kubectl get pods -o json` each minute
(the Pods' own creation and Ready times are the independent witness).
Counters above zero: start samples written, start keys restored, sightings
restored. About 45 min. Optional: run the v0.10.0 and v0.10.1 images on the
same cluster for 5 min each with the snapshot on, and keep their documents as
the back-compat fixtures; hand-built ones from the frozen types also do.

Later and optional, after 5d: a GPU witness. One small model, one variant,
1 GPU per replica, 1 to 4 replicas, one restart; about 30 min and 4 GPUs on a
shared cluster, torn down on every exit path. No PR waits for it.

#### Recording as run

Run on kind with main 59772fce2 (5a to 5c merged, 5d not): the goldens
`start-seed`, `start-seed-restart` and `start-seed-restart-2`, one per
controller start. Everything below is measured on that run. How to take it
again: [the start-seed goldens](../developer-guide/testing.md#the-start-seed-goldens).

Two things differ from the plan above. A cheaper fast variant at a ceiling of 5
made the slow one scale: the cost-aware optimizer adds to the cheapest variant
first, so without the ceiling the slow variant never scales. Step 3 (the
engine restart) was skipped; `pd-restart` and the synthetic suite cover it.

| measure | fast | slow |
| --- | --- | --- |
| Pods' own creation to Ready, 18 Pods from the per-minute dumps | 9 Pods, 20 to 22 s, mean 21.22 s | 9 Pods, 241 to 242 s, mean 241.44 s |
| Learned after the first scale-up | 21.66 s | 241.46 s |
| Learned at the end | 21.03 s | 241.29 s |
| `startSamples` in the ConfigMap, first scale-up | 1 Pod, mean 21 | 1 Pod, mean 241 |

- **Start samples written:** 8 of the first run's 10 exports hold
  `startSamples`; none does after the third Pod of each key, as designed.
- **Start keys restored:** 2 at each of the two restarts, in the recording's
  `saturation.RestoreLearnedState` and in the ConfigMap after it.
  `learner-start` does not count them yet (5d).
- **Sightings:** none. This build does not write them (5d; `start-restored` does,
  see "5d as built").
- **The seed:** `RoleStartSeconds` reads 120 on 12 cycles, then the first
  measured Pod. Published 160.3 s, sized 241 s, as `(2 x 120 + 241) / 3`
  predicts. Measured alone, `start-reads-replica-start-seed` moves 12 rows of
  `results.csv` from 21 to 120, and the blend moves them from 21 to 53.67.
- **The contradicted-seed line fired once**: 241 s is 1 s over 2 x 120. A seed
  half the true start sits at the edge of the band.
- **What 5d changes here:** after each restart the running Pods were folded
  again: slow went 241.46 to 241.66 at the first restart and 241.66 to 241.77
  at the second, and no `learner-write` carried `restored: true`, since
  the first cycle admits a sample.
- **Not reached:** the floor's projection logged `startSource: none` on all 31
  lines. The simulator run has no router, so no true arrival rate, and the
  floor projects only over one. No refused outlier either: no start was over
  three times the estimate.

#### Split and order

| PR | content | fleet sizing |
| --- | --- | --- |
| 5a | Parse the seed and carry it in the input, read by nothing. The frozen v0.10.1 decoder and the v0.10.0 and v0.10.1 fixtures. Zero golden diff | none |
| (recording) | the run above, from 5a, base goldens committed first | — |
| 5b | B | down on a pod that blips, and on a real slowdown until N distinct slow pods; up to the new level once they confirm |
| 5c | A and C together: the seed's bound is part of the blend, and the property cases need both. The gauge's source rename with its prometheus.md and upgrade notes. `startSamples`, `startAdmittedAt`, learner `start` | up only: during a variant's first N starts, and for a seeded, unmeasured role |
| 5d | D and E, snapshot on only. `startCounted`, `settle` | after a restart: k2's wait ends up to 1 min sooner, higher with no k2 on record and lower with a stale low one; ITL's up to `steadyFor` (2 min) sooner, so a line and the floor appear sooner ("5d as built") |
| later, optional | the GPU witness | — |

5c carries the most change, all of it on the cost side: sizing reads
max(blend, m), so it never under-orders. It goes after 5b so the
confirmation rule it leans on is already proven.

#### 5a as built

Built on main 35bd3dd88 (after #462, R1), not on f82a55480, so line numbers
above are f82a55480's.

- **The seed.** `registry.ParseMeta` (`internal/registry/scalermeta.go`)
  reads `replicaStartSeconds` as a plain decimal number of seconds from
  `MinReplicaStartSeconds` to `MaxReplicaStartSeconds`, 1 to 3600 (`"120"`,
  `"15.5"`, `"1e2"`). Any other value (under a second, over the hour, NaN,
  infinite, hex, a unit, surrounding space, not a number) leaves
  `Meta.ReplicaStartSeconds` 0 and is handed back in
  `Meta.ReplicaStartSecondsIgnored`; it is never a trigger error. An empty
  value is absent, as for the other keys.
- **Carried, not read.** `Engine.applyStartSeeds`
  (`steadystate/engine.go`), called in `prepareModelData` right after the
  states are projected from discovery, sets
  `domain.VariantReplicaState.StartSeedSeconds` and its presence bit
  `StartSeedSet`, from the variant's own registry entry. It runs only on
  cycles where the model has replica rows, so a model scaled to zero
  neither carries the seed nor logs a bad one. It is per variant:
  there is no largest-wins across a model's variants as for
  `defaultOutputTokens`; how roles combine is 5c's to decide. Both fields are
  tagged `learnrec:"omitzero"` (R1's tag), so a recording carries the seed
  in `Input.VariantStates` and an unseeded state is written as before.
  `ChangeReporter.ReportIgnoredStartSeed` logs "A variant's
  replicaStartSeconds was ignored" at Info, per variant, once each time the
  written value changes, and again if a fixed value turns bad again. The
  value is quoted and cut at 64 bytes. The log is the only place it shows.
  `TestPrepareModelData_CarriesTheStartSeed` drives `prepareModelData` with
  a fake client and a stub metrics source for a P/D model whose unseeded
  prefill comes first and whose two decode variants seed different figures,
  so the call is tested where it sits and a seed on the wrong variant fails.
- **Zero golden diff.** Every variant state in the nine goldens (4256) reads
  back and is written again byte for byte
  (`learnrec.TestStartSeed_APre5aRecordingIsWrittenAgainByteForByte`).
- **The frozen decoder** is `steadystate/learned_state_v0101_test.go`, from
  the commit and blobs above. Three tests: v0.10.1 reads a document with
  every current field set and gets every field it knows back, equal; a field
  v0.10.1 does not know is left out while empty, checked inside each entry
  type v0.10.1 knows as well as at the top; and the frozen tag set is pinned
  to a literal.
- **The fixtures are hand-built and frozen**: no capture of either release's
  ConfigMap exists. `TestLearnedStateFixtures_Generate` built them once,
  and runs only with `LEARNED_STATE_WRITE_FIXTURES=1`. The base is the last
  learned state the mu-paths golden exported (a real analyzer's state),
  put into the frozen types. Each map that export left empty got a
  hand-built entry in v0.10.1's key formats: argument tags, put-aside
  windows and baseline, an armed borrow and a previous figure dual-written
  to `k2BorrowedSince` and `priorK2`, an orphan `priorK2` entry, a forget
  mark. The document is encoded with `json.Marshal`, as v0.10.1 does; the
  v0.10.0 one drops `itlSlopes`, the one field v0.10.0 (823ddcd74) lacks.
  They live in `steadystate/testdata/learned-state/`, with a README saying
  they are frozen. Both restore through `restoreLearnedState` into a real
  analyzer with nothing aged, unusable or unattributable; the export holds
  every map's entries unchanged, the borrow comes back armed, and the
  orphan previous figure is installed as a lower bound.
  `saturation.TestLearnedStateKeyFormats_AreV0101s` pins the key builders to
  one sample key per map. The light-load restore that keeps the ITL
  line from the v0.10.1 fixture runs cycles, so it is left to 5d's
  restore-then-run. A document captured from a real controller may be added
  beside them.

#### 4b-3 as built

Status: built on `feat/learner-4b3`, on main after 4b-2 merged (#458,
891c071c4). One switch, `throughput-itl-admits-at-sequence-cap`
(`T/admission.go`), on by the constructor.

**What it does.** Row 1's cap admission, with row 1's semantics, on the
throughput line. A reading above the band is the band's when its replica's
batch is exactly at the cap its engine states, on every rank, with requests
waiting and a preemption rate read and at most
`itlBand.capMaxPreemptionRate`. The cap is `EngineParams.StatedBatchCap`:
the flags' `max_num_seqs`, lowered by `--max-num-active-seqs`, else the
`llm-d.ai/max-num-seqs` annotation; never the parser's default and never a
learned cap. The test itself is the shared one (`itl.Admission.atSequenceCap`,
which `itl.Admission.MaxNumSeqs` turns on). Until B is learned the line is
priced one-sided (`itl.Admission.OneSidedAboveKSat`, added in round 1:
[below](#4b-3-round-1-review)), the one change 4b-3 makes in I.

- **The store is read outside `a.mu`.** `engineOf` reads each variant's
  record once per cycle, before `observe` takes the analyzer's lock, and
  takes both the cap and `ArgsTag` from that one read. Since 4b-1 the
  arguments were read inside `lineAdmission`, under `a.mu`; they now come
  from the same read, so a held slope stays bounded by the same arguments.
- **Only on the tracker.** With `throughput-itl-steady-gate` off the
  parent's window reads the band alone, switch on or off
  (`TestITLCap_TheParentsWindowReadsNoCap`).
- **The summary line says so.** `throughput analyzer: ITL line` gains
  `atCap` (this cycle's readings admitted at the cap), `batchCap` and
  `batchCapSource` (`flags`, `declared` or `none`).

**Decision: a reading at the cap goes to the band's window, not the
above-band fallback's.** The order a variant's line is taken in is:

1. the band's window, readings at the cap included: the full fit, else the
   pinned fit;
2. the held slope;
3. the above-band fallback, from readings above the band that are **not**
   at the cap, at k of at least 0.85.

A reading at the cap enters one window only. The tracker already sorted rows
this way (`case k > hi && !adm.atSequenceCap(rm)` sends only uncapped rows
to the fallback); 4b-3 is what gives it a cap to read. Why the band's
window:

- A reading at the cap is a point on the line (#287): the batch cannot grow,
  and the fill above the band is its sequences' contexts, not memory
  pressure. Rows 1 and 13 fit it into their lines; the throughput line now
  reads the same rows the same way.
- The fallback is a stand-in, one-sided by construction: B pinned at the
  least ITL, so it prices over the truth (4b-1 as built). It never sets the
  held slope and never stores B. Priced from it, a variant at its cap
  could not hold its line through a dip below the band, and would never
  learn its slope there. On the probe below the fallback priced 0.0560 s at
  k 0.85 against a true 0.0525 s (6.7 % over). As first built, the band's
  line priced 0.0522 s there (0.6 % under, B pinned at the 6 ms default
  against a true 10 ms), and under by more for a higher B: round 1 made it
  one-sided until B is learned, and it now prices the same 0.0560 s
  ([round 1](#4b-3-round-1-review)).
- So the fallback stays for what it was built for: a key with no fit from
  the band (readings at the cap included) and no slope it may hold.

Tests: `TestITLCap_AVariantAtItsCapPricesFromTheBandNotTheFallback` holds a
capped replica at k 0.92, an uncapped one at 0.90 (its batch one short) and
one below the band. On, the band's line prices it from the capped replica's
tenth reading (T2-failed 9, T2-default 11, of 20 cycles) while the
fallback's window holds 20 readings it is never asked for. Off, the
fallback prices it (T2-failed 4, T2-above-band 16). With the capped
replica preempting at 0.5/s, nothing is the band's and the fallback prices
it, as it must. Mutant: a reading at the cap sent to the fallback's window
(`case k > hi:`) fails 4 tests and all 5 cap property cases (below).

**Which way the fleet moves.** Where a variant's only replica in reach sat
at its cap, the line is now its own: against 4b-2, **about the same**
where the above-band fallback priced it (until B is learned the line is
priced one-sided through the least ITL, as the fallback is, and on readings
at the cap alone gives the fallback's price: the round-1 bias table), and
**live again** where 4b-2 was silent: a capped replica at k between
0.80 and 0.85, which the fallback may not use (`cause`
`all-above-band-below-ksat`), or the cycles before the fallback had ten
readings. Live, the analyzer orders a scale-up and joins the scale-down
vote again. Readings at the cap also teach B and the held slope, as in the
band.

**Probe** (`TestThroughputProbe_AReplicaAtItsCapWithSiblingsBelowTheBandPricesFromTheBand`;
40 cycles, one replica at its declared cap of 64 at k 0.92 and two below
the band; truth at k 0.85 is 0.0525 s):

| run | reasons | band's window | priced at k 0.85 |
| --- | --- | --- | --- |
| main (891c071c4), the case as 4b-1 left it | T2-failed 9, T2-above-band 31 | 0 | 0.0560 s |
| on, as first built (B pinned at the default) | T2-failed 9, T2-default 31 | up to 20 | 0.0522 s (under) |
| on, after round 1 (one-sided until B is learned) | T2-failed 9, T2-default 31 | up to 20 | 0.0560 s |
| switch off (negative control) | T2-failed 9, T2-above-band 31 | 0 | 0.0560 s |
| the parent's line (gate off) | T2-default 40 | n/a | 0.0528 s |

**Ledger.** `edges/throughput-itl/sequence-cap`, owned by 4b-3, fails on
main on 1/1 seeds ("a replica at its sequence cap above the band was
refused: the window holds 0 readings") and passes on 1/1 here; the row is
removed and the ledger is empty.

**Property cases** (2.6; `T/property_cap_test.go`, each through
`Analyze`):

| case | here | main |
| --- | --- | --- |
| `edges/throughput-itl/sequence-cap` | passes 1/1 | fails 1/1: refused |
| `edges/throughput-itl/sequence-cap-edges`: at the cap admitted; one under, one over, no queue, preemption unread, preemption NaN, only the parser's default cap (256), two ranks at one rank's cap refused; two ranks both at the cap admitted | passes 1/1 | fails 1/1: at the cap refused |
| `edges/throughput-itl/cap-preemption-default`: 0/s admitted, 0.001/s refused | passes 1/1 | fails 1/1 |
| `edges/throughput-itl/cap-preemption-non-default`: `capMaxPreemptionRate: 0.5`, 0.5/s admitted, 0.501/s refused | passes 1/1 | fails 1/1 |
| `known-truth/throughput-itl/at-cap-with-siblings-below-the-band`: priced from the band's line on every cycle from the tenth reading, never the fallback, never under the truth by more than ε (since round 1: never under it at all without noise, and twelve more such cases on a grid of A, B and k) | passes 100/100 | fails 100/100: priced from the fallback |

The mutant is the switch off (`TestThroughputCapPropertyMutants`): each case
fails there with the failure main shows. The main column was run on
891c071c4 with the case file copied in (its own `sequence-cap` case renamed
apart).

**Mutants** (source edits, each alone, the throughput package run; all
killed):

| mutant | what fails |
| --- | --- |
| cap not read (`engineOf` reads no cap) | 4 tests (the stated-cap test, the fallback-order test, the wired-line test, the probe) and all 5 cap property cases |
| preemption check dropped in T (`CapMaxPreemptionRate` +Inf) | the fallback-order test and both `cap-preemption` edges |
| preemption check dropped in I (`atSequenceCap` without its rate clause) | the same |
| admitted when not at the cap: the parser's default cap read (`BatchCap`) | 3 tests (the annotation's stated-cap case, the fallback-order test, the probe) and all 5 cap property cases |
| admitted when not at the cap: every reading above the band once a cap is known | the fallback-order and wired-line tests, `sequence-cap-edges` and both `cap-preemption` edges |
| the switch ignored | 5 tests: the stated-cap test, the fallback-order test, the probe, `TestThroughputCapPropertyMutants` and `TestTheWiredAnalyzerReadsTheFleetAsABareOneDoes` |
| a reading at the cap sent to the fallback's window | 4 tests and all 5 cap property cases |

**A vacuous test removed.** 4b-0's `TestTheWindowStillReadsOnlyTheBand`
fed a row with `Ready` unset; since 4b-1 the tracker refuses such a row as
not Ready, so the test passed whatever the cap admission did. It is replaced
by `TestITLCap_AReplicaAtItsStatedCapIsAReading`, whose rows are Ready and
which asserts the reading's `Offered` and `Capped` counts on and off, for
each way a cap is stated. `TestTheWiredAnalyzerReadsTheFleetAsABareOneDoes`
turns the cap switch off with the settle switches: its capped replica is
the one row the wired analyzer now reads differently
(`TestITLCap_TheWiredLineAdmitsTheCappedReplica`).

**Goldens.** None move, as the order table expected. Measured
(`TestGoldens_TheSwitchRuns`): the switch alone against all off moves 0
outputs, and all on reproduces main's goldens byte for byte. By
construction: the in-process fixtures predate `start.ThroughputConfig`, so
the replay builds the analyzer with no capacity store, and with no store it
knows no cap. 4b-2's six switches move to `earlierSeams`
(`learner4b2Seams`), each still moving nothing off alone; the cap switch is
held by its unit tests (`heldBy`). `switchBase` is unchanged: main's
goldens are 4b-1's bytes.

#### 4b-3 round-1 review

**S1: the price at the cap was not one-sided.** With no learned B the
pinned fit pins `DefaultBaselineSec` (6 ms), and through readings at a k
above kSat (0.85) it prices, at kSat, A'kSat + B' + (B0 - B')(1 -
kSat/kbar): under the truth whenever the true B' is above 6 ms, so fewer
replicas than need. Readings at the cap sit there. Fixed in I, behind a
flag only this line sets (`itl.Admission.OneSidedAboveKSat`, set with the
cap under `throughput-itl-admits-at-sequence-cap`): while the key has no
learned B, the pinned fit is set against a line pinned at the least ITL of
the window's readings at k >= kSat, fitted over those readings by the
fallback's kernel (`fitAboveBand`), and the higher of the two at kSat is
the line (`itl.Reading.PinnedAtLeastITL`). The least ITL is at least B',
so that line errs only high (4b-1 as built). A learned B ends it. The A a
later hold holds is the one that prices the chosen line's ITL at kSat
through the default B, since a hold pairs its A with the baseline: holding
the flat line's own A (often 0) through 6 ms priced far under. The
saturation and slo lines also admit readings at the cap and pin the
default, with the same term; they do not set the flag, and that is a
follow-up of theirs.

Bias at kSat on the last of 24 cycles, one replica at its cap (k wandering
over k_cap ± 0.01) and two below the band (`TestITLCap_TheBiasTable`). Each
cell: no noise; then with the gauge's noise (2 %, autocorrelated) the mean
[least, most] over 100 seeds:

| A | B | k at cap | on | B pinned at the default only (as first built) | cap admission off (fallback) |
| --- | --- | --- | --- | --- | --- |
| 0.05 | 10 ms | 0.92 | +6.64 %; +6.33 [+4.27, +8.60] | -0.58 %; -0.64 [-2.61, +1.44] | +6.64 %; +6.33 [+4.27, +8.60] |
| 0.05 | 10 ms | 0.97 | +11.35 %; +10.87 [+8.78, +13.25] | -0.95 %; -1.01 [-2.96, +1.06] | the same as on |
| 0.05 | 20 ms | 0.92 | +5.58 %; +5.27 [+3.24, +7.51] | -1.72 %; -1.78 [-3.73, +0.28] | the same as on |
| 0.05 | 20 ms | 0.97 | +9.54 %; +9.06 [+7.00, +11.39] | -2.78 %; -2.84 [-4.76, -0.81] | the same as on |
| 0.05 | 30 ms | 0.92 | +4.81 %; +4.50 [+2.48, +6.72] | -2.54 %; -2.59 [-4.53, -0.55] | the same as on |
| 0.05 | 30 ms | 0.97 | +8.22 %; +7.74 [+5.72, +10.04] | -4.11 %; -4.17 [-6.06, -2.17] | the same as on |
| 0.01 | 10 ms | 0.92 | +3.77 %; +3.45 [+1.46, +5.65] | -1.66 %; -1.71 [-3.63, +0.31] | the same as on |
| 0.01 | 10 ms | 0.97 | +6.44 %; +5.96 [+3.96, +8.21] | -2.68 %; -2.74 [-4.61, -0.77] | the same as on |
| 0.01 | 20 ms | 0.92 | +2.45 %; +2.13 [+0.14, +4.29] | -3.76 %; -3.82 [-5.71, -1.82] | the same as on |
| 0.01 | 20 ms | 0.97 | +4.18 %; +3.69 [+1.71, +5.89] | -6.10 %; -6.16 [-7.98, -4.23] | the same as on |
| 0.01 | 30 ms | 0.92 | +1.81 %; +1.49 [-0.50, +3.63] | -4.77 %; -4.83 [-6.71, -2.85] | the same as on |
| 0.01 | 30 ms | 0.97 | +3.10 %; +2.60 [+0.63, +4.78] | -7.74 %; -7.80 [-9.60, -5.89] | the same as on |

On readings at the cap alone, the one-sided line and the fallback compute
the same thing, so they agree to the digit. Without noise the line on never
prices under the truth; with noise it is under on some seeds at a shallow A
and a cap near kSat (least -0.50 % at A 0.01, B 30 ms, k 0.92): the margin
(b - B')(1 - kSat/kbar) is under 1 ms there, and the noise on ten readings'
mean is of that size. The fallback, 4b-1's one-sided line, is under by the
same on the same seeds. So the property cases hold the line to the truth
without noise and to known truth's ε with it; strict on every noisy seed
is not met by either line and is not claimed. The default pin alone, as
first built, is under the truth without noise at every point of the grid,
by up to 7.7 %, beyond ε.

Tests: twelve known-truth cases, `known-truth/throughput-itl/at-cap-one-sided-a{0.05,0.01}-b{10,20,30}ms-k{0.92,0.97}`,
and `at-cap-with-siblings-below-the-band` on A 0.05, B 10 ms, k 0.89-0.91;
each through `Analyze` on 100 seeds, with no noise at or above the truth
on every cycle, with noise within ε. With the switch off each fails (the
fallback prices). `TestITLCap_TheBiasTable` holds, as the negative
control, that B pinned at the default alone prices under the truth without
noise at every point and on average at 30 ms. The probe now prices 0.0560 s
(truth 0.0525 s) and asserts at or above the truth; pinned at the default
alone it prices 0.0522 s and fails. `TestITLCap_ALearnedBEndsTheOneSidedPriceAndAForgetBringsItBack`
and `TestITLCap_AHeldSlopeKeepsTheOneSidedPrice` (held 0.05600 s, the
one-sided price) hold the rule's two edges; in I,
`TestTracker_OneSidedAboveKSatPricesAtOrAboveTheTruth`.

**S2: `atCap` was invisible while the line stayed live.** The summary line
logged only when the line went silent or came back. It now also logs once
when readings at the cap start (`atCap` above 0) or stop, or the cap's
source changes (`TestITLCap_TheSummaryLogsTheCapWhileTheLineIsLive`).

**Tables.** `mergedRows` panics on a switch in both `replaySwitches` and
`earlierSeams`, as `withSeams` does (`TestGoldens_ASwitchWithTwoRowsPanics`).
`replaySwitches` carries a note: #460 rewrites the start of that table too,
and 4b-2's six switches belong only in `learner4b2Seams`, whichever merges
second.

**Second tests.** Three mutants each fell to one test only; each now has a
second through `Analyze`: a two-rank replica at one rank's cap and a
replica at its cap with nothing waiting
(`TestITLCap_WhatIsNotAtTheCapIsNotAReading`), and the cap's source
(`batchCapSource` `declared` in the summary line test).

**Mutants, round 1** (each alone, the throughput package run; all killed):

| mutant | what fails |
| --- | --- |
| `AtBatchCap` reads one rank (`RunningRequests == maxNumSeqs`) | `TestITLCap_WhatIsNotAtTheCapIsNotAReading` (both two-rank cases), `sequence-cap-edges` |
| no queue needed (`atSequenceCap` without `QueueLength > 0`) | `TestITLCap_WhatIsNotAtTheCapIsNotAReading` (nothing waiting), `sequence-cap-edges` |
| the source always `flags` | the annotation's stated-cap case, the summary line test |
| one-sided rule off | all 13 at-cap known-truth cases, the bias table, the held-slope test, the probe |
| one-sided with a learned B | `TestITLCap_ALearnedBEndsTheOneSidedPriceAndAForgetBringsItBack` |
| the held A not adjusted | `TestITLCap_AHeldSlopeKeepsTheOneSidedPrice` |
| the summary ignores the cap | `TestITLCap_TheSummaryLogsTheCapWhileTheLineIsLive` |

**Follow-up, not in this PR: no rollout guard.** The saturation analyzer
empties a variant's ITL rows while its scale target rolls out
(`capacity.Record.InRollout`, `D/analyze.go`), so an old pod's reading is
not taken as the new engine's. The throughput line never had that guard,
before 4b or since; with the cap read from the store, a rollout that
changes `--max-num-seqs` can briefly judge old pods against the new cap.
A small PR of its own. (The forget for this line has landed:
[as built](#the-throughput-lines-forget-as-built), and so has the guard:
[as built](#rollouts-k2s-wait-and-the-lines-reports-as-built-481-495).)

#### 4b-3 round-2 review

No blockers; tests and docs tightened.

- **The kSat floor is pinned.** `atLeastITLLine` and the above-band
  window share one predicate (`itl.atOrPastKSat`). A window of capped
  readings at k 0.82 and 0.93 on a shallow line pins at the least ITL at
  0.93 and fits a flat line through it alone
  (`TestTracker_OneSidedReadsOnlyTheReadingsPastKSat`). The mutant that
  reads from 0.80 survived round 1, where every capped reading sat at 0.88
  or more; it is killed by this test.
- **The bias table asserts what this plan says**: the worst noisy seed
  within known truth's ε; the default pin's error equal to the derived
  term (B0 - B)(1 - kSat/kCap) over the truth, to 0.05 points, at every
  point of the grid; and the cap admission's columns equal to the
  fallback's (no noise, mean, least and most).
- **The rule both ways in one run**
  (`TestITLCap_ALearnedBEndsTheOneSidedPriceAndAForgetBringsItBack`):
  one-sided while B is unlearned, off once a full fit in the band learns
  it, and on again after the key is forgotten. The line's only forget today
  is the analyzer's eviction of a variant unseen for 2 x the window's 30
  min, which sweeps its key and its B.
- **The hold's expiry** (`TestITLCap_TheHeldOneSidedSlopeEndsWithTheHold`,
  hold 1 h): last fit at 32m30s, held through 1h32m30s, silent (T2-failed)
  from 1h32m45s and for the rest of the 2 h run. Nothing prices the
  variant from above the band after it: the fallback has no readings there.
- **The summary line logs `pinnedAtLeastITL`**, so an operator can tell a
  one-sided price from the default pin's. A cap that flaps logs once per
  flip; the reference pages say so.
- **`atLeastITLLine` may rest on one reading**: through one reading the line
  is flat at its ITL, which lies at or past kSat on the true line, so it
  errs the same way, only by more; and it only ever raises the price.
- **Docs qualify every "never under" and "never fewer"** as without noise;
  with 2 % gauge noise the one-sided lines were at most 0.5 % under in
  simulation, within the 5 % known-truth bound.

Mutants, round 2 (each alone, the throughput and itl packages run; all
killed):

| mutant | what fails |
| --- | --- |
| the pin reads from k 0.80 (M2) | `TestTracker_OneSidedReadsOnlyTheReadingsPastKSat` |
| the hold never expires | `TestITLCap_TheHeldOneSidedSlopeEndsWithTheHold` and four earlier hold tests |
| one-sided with a learned B | `TestITLCap_ALearnedBEndsTheOneSidedPriceAndAForgetBringsItBack` |
| the summary omits `pinnedAtLeastITL` | the probe |
| the negative control's knob ignored | the bias table and the probe |

#### The throughput line's forget, as built

The forget-learned-state annotation reaches this line, as #431 took it to
the slo line. One switch, on by the constructor:
`throughput-itl-forget-reaches-the-line` (`T/forget.go`). `forgetOnRequest`
calls `ThroughputAnalyzer.ForgetVariant` on the token the saturation analyzer
acts on, after the slo analyzer's, found among the built-ins because only the
saturation config registers it. The forget log line gains `throughputEntries`.

- **What goes.** What the tracker learned of the key, through
  `itl.Tracker.ForgetKeysKeepingTraces`: the window, the steady window, both
  above-band windows, B and the held slope. The settling stamps and the
  readings at the cap go with the windows. Also `itlLogged`, the parent's
  window and B, this cycle's reading and the figures priced from the line.
- **What stays.** The replicas' traces, which fit nothing. Unlike the
  saturation and slo forgets, which drop theirs and were left alone. Dropped,
  the steady gate went blind for the first cycle after a forget (a step of k
  +0.25 and ITL x3 read `unsteady` 0, where 3 with no forget;
  `TestForget_TheSteadyGateSeesTheFirstCycleAfter`). The settle holds through
  the trace, not through `ownSettle`, as this section first said: with the
  traces dropped it held only through the engine's view and the `waits` path,
  and was lost with no view. With the traces kept a replica inside its settle
  stays inside it on all three paths
  (`TestForget_AReplicaSettlingAtAForgetStillSettles`,
  `TestForget_AReplicaSettlingAtAForgetStillSettlesOnEveryPath`). Also the
  shape, the traffic now, and the log lines' memories. Two tests hold every
  field to a list
  (`TestForget_EveryFieldIsClassified`,
  `TestForget_EveryVariantStateFieldIsClassified`). The cap is read from the
  store each cycle, so with B gone a line at the cap prices one-sided again
  (`TestForget_AtTheCapALineIsPricedOneSidedAgain`).
- **Nothing refills it.** This analyzer has no snapshot and no restore, read
  from the code. `TestForget_NothingHereRestoresALine` checks method names
  only, as a tripwire. It borrows no line, and a key is one variant's. The
  tracker refuses a lent replica's rows; `ownReplicasOnly` drops them first on
  the `Analyze` path, as defence in depth, and that mutant survives alone
  (`TestForget_NoSiblingOrLentReplicaRefillsTheLine`, 16 cycles of a
  sibling's and lent rows leave the key empty).
- **Reuse.** `itl.Tracker.ForgetKeys` is the sweep and sorted key list that
  the saturation and slo forgets each wrote out, and now call.
  `logging.ForgottenKeys` is slo's per-key log line, moved: `learn` logs
  nothing and imports only the standard library, so it could not go there. It
  takes plain key strings, so `logging` stays a leaf (`learn.Keys` converts),
  and writes a key's NUL separators as `|`. Which learners an analyzer keeps
  stays each analyzer's own code, as `learn.Learners` says it should.
- **Learner series.** `learn.Report` has no consumer for this line and no
  learner series exists, so the deferred clause "when no key remains, the
  variant's learner series are deleted" still has nothing to apply to. Each
  key is logged instead.
- **Goldens.** None moves. No committed recording carries
  `throughput.ForgetVariant`. Measured on the `acceptance` and `engine` runs
  taken again: each carries one event (Dropped 1: a window, no intercept), the
  recording is otherwise line for line the one main writes, and all 19 outputs
  are byte for byte the same with the event and without it. So the unit tests
  hold the switch (`heldBy`), and the replay of the event is held by
  `TestReplay_TheThroughputForgetIsApplied`. Taking those two recordings again
  moves 9 outputs on main with no change at all; that drift is not this
  change's, and they were not retaken here.
- **After a forget.** `itl.DefaultMinSamples` readings are necessary and not
  a recovery time. Capacity returned on cycle 4 with 3 replicas and cycle 10
  with 1 (`TestForget_TheLineIsBackOnTheCycleItHoldsTenReadings`). A fleet
  offering no reading stays silent: over 40 cycles on three replicas, k 0.82
  with no stated cap priced 40/40 without a forget and 0/40 after
  (`all-above-band-below-ksat`), and k 0.10 likewise (`too-few-readings`;
  `TestForget_AFleetOfferingNoReadingStaysSilent`). So a policy listing
  throughput without saturation cannot scale up after a forget at k 0.80 to
  0.85 until k leaves that range. Once B is gone a balanced fleet pins
  `DefaultBaselineSec`, as a new variant does, and in the band that is not
  one-sided: capacity 1.20x the truth at true B 2 ms and k 0.30, 0.71x at
  20 ms (`TestForget_ABalancedFleetPricesEitherSideOfTheTruth`). The behaviour
  is left as it is: it is the new-variant state. `troubleshooting.md` states
  the holds and metrics an operator sees meanwhile.
- **Measured versus assumed.** The tests above are unit and replay tests on
  synthetic rows. It has not been run on a cluster.

#### 4b-3's follow-up: the saturation and slo lines at the cap (#467)

Built on main after #471 (602054e98), merged with main after #475
(0c2e4431a). Two switches, on by the constructors:
`saturation-itl-one-sided-at-cap` (`D/itl_one_sided.go`) and
`slo-itl-one-sided-at-cap` (`slo/one_sided.go`). Each turns on the shared
rule in I (`itl.Admission.OneSidedAboveKSat`) for its own line; no code is
copied.

**The rule: B bounded by a prior.** While B is unlearned and the window
holds readings at k >= kSat, a second line is formed (`itl.priorLine`):
intercept `b`, the prior capped at the window's least ITL, through an anchor
`(kbar, ITLbar)`, `A = (ITLbar - b)/kbar`. The line is priced on the higher
of it and the fit through the default B, where the line is priced. On
readings of the true line, at a fill k, it is off the truth by `(b - B)(1 -
k/kbar)`. That is the real rule: at or over the truth for `k <= kbar` while
the true B is at most the prior, over by the prior's error; under it past
`kbar`, by `(b - B)(k/kbar - 1)`. A true B above the prior is under-priced
by at most `(B - b)(1 - k/kbar)` for `k <= kbar`.

**The prior, round 1.** The first build of the prior read the largest
learned B across every key of the tracker. Measured intercepts run 1.5 to
15 ms, so one learned B anywhere replaced 30 ms: a whole-node variant (A
0.01, B 30 ms) priced -11.9, -17.4 and -19.5 % with siblings at 15, 8 and 3
ms, the bias #467 is about. Four rules replace it:

- **Scope.** A learned B counts only from the variant's physics
  (`itl.PriorScope`): a window key of the same model, accelerator and GPU
  count, last fitted under the same engine arguments, of a variant in the
  same role this cycle. For saturation, also the line published for the
  variant's own physics key (`itlPhysicsKey`: model, accelerator, GPUs,
  engine fingerprint; shared only with `enableLearnedStateReuse`). Another
  namespace is out of scope (its role is unknown there): the rule errs
  toward the floor.
- **Floor.** `itl.BPrior = max(DefaultBPrior, largest learned B in scope)`.
  A learned B only ever raises the prior, never lowers it below 30 ms.
- **Cap.** The prior is capped at the least ITL of any reading in the
  variant's own window (`itl.priorCap`): every reading is ITL >= B, so that
  cap is an upper bound on B the window proves. It bounds the over-price of
  an outlier fit (an 80 ms one, below), and makes a slo point feasible once
  any reading shows B is under the target.
- **Mean point.** The line passes through a mean of at-cap readings, not
  the least reading. The bound holds as before without noise; the noise
  falls by sqrt(n), and the minimum's selection bias (about -4 % at about 30
  readings) is gone. At B = prior with 2 % noise the worst of 20 seeds is
  now -1.10 % (mean -0.07 %), against -4.79 % (mean -2.37 %) through the
  least reading (`TestITLOneSided_TheLeastReadingLineFailsTheNoiseGates`:
  the gates, -0.5 % mean and -1.5 % worst, pass for the analyzer's line and
  fail for the least reading's; on slo, the noisiest fill 0.5105 and 0.5275
  against 0.5523 and 0.6573, gate 0.53).

**Round 2.** Four more rules:

- **Anchor at or past the priced fill.** The mean of every at-cap reading
  can sit under the fill priced (saturation at k 0.95 against readings at
  0.85 to 0.97, kbar 0.905: under by `(b - B)(1 - 0.95/0.905)`). The anchor
  is the mean of the at-cap readings at or past the priced fill when there
  are three, else the three of highest k (`Admission.priceAnchor`: the
  pricing k, or the fill where the default-B fit meets the TPOT target).
  Where even that mean is under the priced fill, `PriorBeyondReadings` is
  set and logged (`priorBeyondReadings`, `linePriorBeyondReadings`): the
  bound does not hold there (`TestBPrior_PricedAboveTheMeanFillIsNotUnder`,
  `TestBPrior_PricedPastEveryReadingIsFlagged`).
- **No flat line.** With a prior set, the line returned is always through
  the default B at the held A, also for an admission that names no price
  (slo with no TPOT target, `OneSidedAt` out of range): the prior's line can
  be flat (one reading; the cap's ITL at every reading), which `PriceTPOT`
  and `ValidModel` refuse (`TestBPrior_NoPriceStillReturnsALineThroughTheDefaultB`,
  `TestBPrior_OneReadingGivesAValidLine`).
- **Not lent.** A raised line is not published for siblings to borrow: it
  carries the donor's at-cap evidence and pricing k. While a variant is
  priced at the prior it publishes nothing (`TestITLOneSided_ARaisedLineIsNotLent`:
  on, the sibling borrows nothing; off, it borrows the donor's plain line).
  The what-if refit is gated on the recorded `PinnedAtBPrior`, not on the
  line's B being the default.
- **Evicted.** `itlOneSidedLast` goes with its variant in
  `EvictStaleHistory` (`TestITLOneSided_TheRecordIsEvictedWithItsVariant`).

The scope is built by one constructor, `itl.NewPriorScope`, reading each
analyzer's arguments lazily under its own lock.

`DefaultBPrior` is 30 ms, the top of the grid, so the grid's worst case is
exact. The ITL policy has no natural place for an intercept, so it is a
constant, not a knob.

**Where each line is priced.** `Admission.OneSidedAt` (saturation: its
pricing k, 0.68 by default) and `Admission.OneSidedTarget` (slo: its TPOT
target; each line judged at the fill where it meets it, and the lower fill
wins), with `Admission.BPrior`. When an admission names its price, the chosen
line is returned through the default B at the A that prices there what the
prior's line did, so `PriceTPOT`, `ValidModel` and line sharing read it as
any pinned fit, and the held A is that A. The throughput line sets none of
the three and is unchanged. Flag: `itl.Reading.PinnedAtBPrior`; `itl-fit`
logs `pinnedAtBPrior`, `slo-variant` logs `linePinnedAtBPrior`.

**Bias, saturation line at k 0.68** (`TestITLOneSided_TheBiasTableAtThePricingK`,
through `Analyze`, one subtest per case, three replicas at a cap of 64 at
k_cap ± 0.01, k_cap 0.97, 24 cycles; noise 2 %, autocorrelated, 20 seeds).
"Bound" is the uncapped prior's line worked by hand; "on" is held at or under
it (the cap only tightens) and at or above the truth while B <= prior:

| prior | A | B | off | on | bound | 2 % noise: mean [least, most] |
| --- | --- | --- | --- | --- | --- | --- |
| 30 ms | 0.05 | 10 ms | -2.72 % | +13.61 % | +13.62 % | +13.52 % [+12.42, +14.66] |
| 30 ms | 0.05 | 30 ms | -11.22 % | +0.00 % | +0.00 % | -0.07 % [-1.10, +0.98] |
| 30 ms | 0.01 | 20 ms | -15.64 % | +10.72 % | +11.94 % | +9.61 % [+8.08, +11.17] |
| 30 ms | 0.01 | 30 ms | -19.52 % | +0.00 % | +0.00 % | -0.07 % [-0.96, +0.86] |
| 30 ms | 0.01 | **40 ms** | -21.75 % | **-6.40 %** | -6.40 % | -6.46 % [-7.34, -5.55] |
| 45 ms scoped | 0.05 | 10 ms | -2.72 % | +23.81 % | +23.84 % | +23.73 % [+22.62, +24.87] |
| 45 ms scoped | 0.05 | 30 ms | -11.22 % | +7.02 % | +7.02 % | +6.94 % [+5.92, +7.99] |
| 45 ms scoped | 0.01 | 20 ms | -15.64 % | +10.72 % | +67.91 % | +9.61 % [+8.08, +11.17] |
| 45 ms scoped | 0.01 | 30 ms | -19.52 % | +7.81 % | +22.28 % | +6.70 % [+5.21, +8.24] |
| 45 ms on another accelerator, GPU count, namespace, engine args or role (one row each) | every point | | equal to the 30 ms rows to 1e-9 | | | |
| 80 ms outlier | 0.05 | 10 ms | -2.72 % | +32.65 % | +81.82 % | +31.49 % [+29.69, +33.19] |
| 80 ms outlier | 0.05 | 30 ms | -11.22 % | +22.45 % | +25.00 % | +21.32 % [+19.64, +22.95] |
| 80 ms outlier | 0.01 | 20 ms | -15.64 % | +10.72 % | +198.51 % | +9.61 % [+8.08, +11.17] |
| 80 ms outlier | 0.01 | 30 ms | -19.52 % | +7.81 % | +117.39 % | +6.70 % [+5.21, +8.24] |

"Off" equals the derived term `(B0 - B)(1 - k/kbar)` to 0.05 points on every
row. Without noise, no under-price anywhere B <= prior; the worst over-price
at the floor is +13.61 %. Each out-of-scope row puts a learned 45 ms, above
the floor, on a key that differs from the variant's physics in one dimension
alone: none moves the price. Where the window's cap binds (the 45 ms rows at
A 0.01, the 80 ms outlier), the price is the cap's line, far under the
uncapped bound. A B above the prior is under-priced by the bound, a third of
the default B alone. With noise, at B = prior, the mean is -0.07 % and the
worst seed -1.10 % (gates -0.5 % and -1.5 %).

**Bias, slo line at the target fill** (`TestOneSided_TheBiasTableAtTheTarget`,
through `Analyze`, one subtest per case, the line read from `slo-variant`
with its `linePinnedAtBPrior` key asserted; the target the true line meets
at 0.5; noise 2 %, 20 seeds):

| prior | A | B | fill off | fill on | the prior's line | 2 % noise [least, most] |
| --- | --- | --- | --- | --- | --- | --- |
| 30 ms | 0.05 | 10 ms | 0.5358 | 0.1701 | 0.1702 | [0.1662, 0.1747] |
| 30 ms | 0.05 | 30 ms | 0.6557 | 0.5000 | 0.5000 | [0.4910, 0.5105] |
| 30 ms | 0.01 | 20 ms | 0.7779 | infeasible | infeasible | infeasible 20 of 20 |
| 30 ms | 0.01 | 30 ms | 0.8000 | 0.5000 | 0.5000 | [0.4778, 0.5275] |
| 30 ms | 0.01 | **40 ms** | 0.8000 | **0.7388** | 0.7386 | [0.7182, 0.7633] |
| 45 ms scoped | 0.05 | 30 ms | 0.6557 | 0.2895 | 0.2896 | [0.2820, 0.2984] |
| 45 ms scoped | other three | | | infeasible | infeasible | infeasible |
| 80 ms outlier | every point | | | infeasible | infeasible | infeasible |
| 45 ms out of scope, one dimension per row | every point | | equal to the 30 ms rows | | | |

Off meets the target above the truth's fill on every point. On, the fill is
at or under the truth's for every B <= prior, without noise. Which points
abstain is asserted, case by case, so a NaN never passes an assertion
vacuously: the floor on A 0.01 B 20 ms only (its readings are all at the
cap, the least 29.6 ms, above the 25 ms target); the raised and outlier
priors where the cap does not bind, which is the cost of a prior that
learned a higher B. One reading under the target is enough to make a point
feasible (`TestOneSided_AReadingUnderTheTargetMakesItFeasible`), and the cap
brings an 80 ms outlier down to a feasible fill the same way
(`TestOneSided_TheCapBringsAnOutlierPriorToAFeasibleFill`). A B above the
prior meets the target at the prior's line's fill (0.7388 against 0.7386).
With noise at B = prior, the fill is at most 0.5275, under the gate of the
truth's plus 0.03.

**Tests.** I (`one_sided_at_test.go`): the saturation table at the floor,
"on" at or under the bound; a B above the prior, at the bound; noise within
ε; the slo target table, abstaining exactly where the capped prior's line
does; the held A; the lines compared at the pricing k where they cross
before kSat; `priorLine` through the mean point, capped, past kSat;
`BPrior` only raises; the window caps an 80 ms outlier; `PriorScope`
admits only the physics key; Refit prices the line Observe did. D and slo,
through `Analyze` with the switch off as the control: the tables above; a
published line on the physics key raises the saturation prior, one on
another physics key or through the default B does not (M4); a learned B
ends the rule and a forget brings it back; the hold keeps the price and
ends with it; `itl-fit` logs `pinnedAtBPrior`.

**The what-if gap, closed.** `itl.Refit` takes an `itl.Admission`, of which
it reads the one-sided fields. `whatif.Refit.OneSided` carries it
(`band.go`). `learnreplay` passes the admission the analyzer last priced the
key with (`SaturationAnalyzer.ITLOneSided`, per key in `itlOneSidedLast`,
since the prior now depends on the cycle's physics), for a pinned line
through the default B (`state.go`). On an at-cap window (A 0.05, B 10 ms) the
band prices the controller's 0.04999 s at k 0.68, against 0.04280 s, under
the truth's 0.04400 s, without it
(`TestBootstrapPricesTheOneSidedLineTheControllerDid`).

**Goldens.** The saturation switch alone, against all off, moves e1 and
e1-tuned: `variant-capacities.csv` (92 and 60 rows) and `results.csv` (70
and 32 rows); in `lines.csv` the 92 and 60 publishes of the raised line are
removed, since a raised line is no longer lent. Demand rises on 33 rows of e1 (by up to 5.50
%) and falls on 37 (by at most 0.05 %); on e1-tuned it rises on 1 row (11.93
%) and falls on 31 (by at most 0.08 %). Neither recording learns a B on the
physics, so the prior is the floor there. The per-replica capacity moves on
no row. The slo switch moves nothing (held by its tests, `heldBy`).

**Mutants** (each alone; all 43 killed, on a green base). The ones rounds 1
and 2 added:

| mutant | what fails |
| --- | --- |
| the line through the least at-cap reading, not the mean point | the I tables, the D and slo bias tables |
| no window cap | `TestBPrior_TheWindowCapsAnOutlierPrior`, the outlier rows, the slo feasibility test |
| the cap reads at-cap readings only | `TestBPrior_ThePriorLineIsThroughTheAnchor`, the slo feasibility test |
| a learned B may lower the prior (no floor) | `TestBPrior_ALearnedBOnlyRaisesThePrior`, the "3 ms elsewhere" rows |
| the scope fleet-wide; ignoring the arguments; the accelerator; the role | `TestBPrior_TheScopeIsThePhysicsKey`, the "3 ms elsewhere" rows |
| M4: published lines dropped from the saturation prior | `TestITLOneSided_APublishedLineOnThePhysicsKeyRaisesThePrior` |
| published lines of every physics key read | the same test (its other-physics case) |
| M12: the scope ignores the namespace | `TestBPrior_TheScopeIsThePhysicsKey`, the D and slo "namespace" rows |
| the anchor is every at-cap reading | `TestBPrior_PricedAboveTheMeanFillIsNotUnder`, `TestBPrior_ThePriorLineIsThroughTheAnchor` |
| under three at or past the fill, those alone | `TestBPrior_ThePriorLineIsThroughTheAnchor` |
| `PriorBeyondReadings` never set; set whenever past the mean | `TestBPrior_PricedPastEveryReadingIsFlagged`, the anchor tests |
| a prior with no named price returns the prior's own line | the I tables, `TestBPrior_NoPriceStillReturnsALineThroughTheDefaultB` |
| a raised line published for siblings | `TestITLOneSided_ARaisedLineIsNotLent` |
| the record not evicted | `TestITLOneSided_TheRecordIsEvictedWithItsVariant` |
| the raised flag never recorded; the record keeps the floor | `TestITLOneSided_TheWhatIfReadsTheScopedPrior`, `TestStateAt_TheBandBracketsTheControllersLineOnAScopedPrior` |
| StateAt gates on the default B, not the flag | `TestRefitFindsAOneSidedLine` |

**Not in this PR: the throughput line's rollout guard.** Still true after
4b-3: the saturation line empties a variant's rows while its scale target
rolls out (`rec.InRollout()`, `D/analyze.go`), and the throughput line has
no such guard. Left for an issue of its own, as the round-1 review planned
(#481, since [built](#rollouts-k2s-wait-and-the-lines-reports-as-built-481-495)).

#### 5b as built

Built on main 0fec87b18 (5a merged). Change B only, behind
`saturation.SwitchStartConfirmsByDistinctPod` (`start-confirms-by-distinct-pod`),
on from the constructor.

- **The rule.** `D/start_est.go` keeps, per start key, a run of the pods
  refused as over `maxStartOutlierFactor` (3) times the estimate
  (`startOutlierPods`, keyed like `startSeenPods`), each with its latest
  start. A refused pod is not counted as seen, as before, so it reports
  again next cycle; again it adds no pod to the run. When the run holds
  `startConfirmPods` (3, a named constant marked unmeasured) distinct pods,
  the guard opens: the estimate becomes the **median** of their starts, and
  the run's pods still running are counted as seen. The parent opened after
  three refusals and admitted the fourth sample, whoever sent them; one pod
  reporting its age on four cycles moved 70 s to 859 s.
- **Pods that left stay in the run.** As first built, a refused pod was
  dropped when it left. Review found the consequence: a variant never has
  three slow pods up at once if it runs one or two replicas, so it never
  learned a slowdown. A one-replica variant rolled ten times to a 300 s
  image stayed at 70 s (the parent reaches 293.5 s); two steady slow pods
  stayed at 70 s (the parent reaches 187.3 s). Nothing cleared it: the key
  carries no image digest, a live variant is never swept, and the estimate
  is in the snapshot. Whole-node 8-GPU variants are usually one or two
  replicas, and an estimate that is too short orders too little on a
  scale-up. Now the run counts across rollouts: the third slow pod opens it,
  up together or not.
- **The run ends** only on a start inside the bound from a pod not in the
  run. That is applied after every sample of the cycle is judged, so it does
  not depend on the order the pods are listed in (as first built, the refused
  pods re-joined the set after an in-range start earlier in the same cycle,
  and the reset depended on order). A run pod whose own start is now in range
  leaves the run without ending it. Samples are still compared, one by one,
  with the estimate as the cycle's in-range starts move it, as on the parent.
- **The median, not the EWMA.** All N pods were refused against the old
  estimate; that is the evidence it no longer describes the variant. Folding
  them at α keeps most of the weight on the figure they refute: 70 s to
  3000 s on three pods stalled near 949 s, and the pods, now counted, never
  moved it further. The median takes the level from the pods, and is robust
  to one of them being a blip: two pods at 300 s and one reporting 2700 s
  give 300 s, where the mean gives 1100 s.
- **Bounded.** Between cycles a run holds at most `startConfirmPods` - 1 = 2
  pods per key: the cycle that brings it to 3 opens the guard and empties it.
  It is working state (`learnedMap.working`): not in the snapshot, not
  cleared by a flush, dropped by a forget and by the variant sweep, which
  counts each key once in `Evicted.StartOutliers`. After a restart the run
  starts empty, and a running slow pod (not counted, since `startSeenPods` is
  not restored either) joins it again.
- **Limits.** Pods counted across a run can be far apart in time: three
  pods blipping hours apart, with no pod starting in range between them,
  open the guard at their median age (at most 3600 s, past which the
  collector reports no start). Until a third slow pod, a real slowdown is
  not followed at all, where the parent followed it after three refusals:
  for a one-replica variant the first two slow rollouts are sized at the old
  estimate. The operator's remedy is the `llm-d.ai/forget-learned-state`
  annotation, which drops the key's estimate so the running pods are measured
  afresh (troubleshooting, "Reset one variant's learned state"). The 5a
  seed is read by nothing until 5c.
- **"Never with P"**: in 5b `startSeconds` holds measured figures only, so
  the guard already compares with the measurement. 5c, which publishes a
  blend, must keep the guard on m.
- **Learned-state reuse** does not reach the start estimate: it lends k2
  and ITL lines. The only writers of `startSeconds` are the fold and the
  snapshot restore of the key's own estimate; a restored estimate is
  corrected by rollouts as a measured one is (tested).
- **The log.** `replica-start-outlier-rejected` has a neutral `reason`,
  `start-above-bound` (a blip is the usual cause, not the only one), gains
  `distinctPods` (the run's pods after the cycle: 0 on a cycle whose
  in-range start ended it) and `needed`, and is logged on a pod's first
  refusal in the run and again every `startOutlierRelog` (10 min, at exactly
  the interval) while it stays refused. With the switch off the line is the
  parent's byte for byte, reason included (a test pins it). A
  new line, `replica-start-change-confirmed`, says the guard opened, with
  `distinctPods`, `medianSeconds` and `previousEstimateSeconds`.
- **Zero golden diff.** Measured alone against all off, the switch moves no
  output of any recording (`TestGoldens_TheSwitchRuns`). Its row is in
  `replaySwitches` with no outputs, held by its unit tests (`heldBy`). 4b-2's
  six switches stay in that table, as main has them after #460 (P1, whose
  `SwitchPrefillCapacityObserve` row sits beside this one); #465 (4b-3)
  moves them to `earlierSeams`.
- **Tests** (`D/start_confirm_test.go`), each the figure with the switch on
  and, where it decides, off:

  | case | on | off (the parent) |
  | --- | --- | --- |
  | (a) one pod, age 2700 s growing 30 s a cycle | 70 after 60 cycles | 886 on cycle 4 |
  | (b) one replica, ten rollouts to a 300 s image | 70, 70, then 300 from the third | 139, 187.3, ... 293.5 |
  | (b) the same from a restored 70 s | 300 at the third pod | — |
  | (c) two steady slow pods, 50 cycles, then a third | 70, then 300 | 187.3 |
  | (d) 70 to 3000 s on three pods | 3000 on the first cycle | 949 on cycle 2, 1995.01 from then on (within 0.01) |

  Also: the median by a table (100/200/300 gives 200; 100/200/300/400 in
  one cycle gives 250), the median with a blip in the run, the bound (at 70
  s, 210 s admitted and 210.001 s refused), the re-log at exactly its
  interval, the reset in either order, a run ended this cycle logged as 0
  pods, a run pod back in range, the bound on the run's size, two keys of
  one variant kept apart, a controller restart dropping the run (the
  snapshot holds the estimate, not the run, so three fresh pods are needed
  again), (b) driven through `Analyze` (70, 70, 300 on; 187.3 off after
  three rollouts), the lines, and the sweep (counted once) and forget. The property edge
  `start-time/confirmations-3`, which pinned the parent's "one pod taken on
  cycle 4", is replaced by `one-pod-repeating` (not taken on cycle 3, 4 or
  20) and `distinct-pods-3` (taken at 3 pods, not 2). Twenty-three mutants,
  each killed, among them the code as first built (pods pruned as they
  left), the reset applied inside the loop, the EWMA, the mean, the lower
  or the upper middle instead of the median, the bound at `>=`, the re-log
  at `>`, the pod count read before the close, the off path's reason,
  `Analyze` not feeding the estimate, runs shared across a variant's keys,
  the sweep counting a key twice, and the parent (switch off everywhere).

#### 5c as built

Built on main 602054e98 (5b merged). Changes A and C, behind two switches,
both on from the constructor: `saturation.SwitchStartReadsSeed`
(`start-reads-replica-start-seed`) and
`saturation.SwitchStartBlendsFirstSamples` (`start-blends-first-samples`).

- **Three figures per key** (`startFiguresLocked`, `D/start_est.go`). E is
  the measured estimate the fold keeps (`startSeconds`: the first sample, the
  EWMA, or a 5b run's median). With n < N = 3 distinct Pods counted
  (`startSamples`) and m their plain mean:
  - published = ((N - n) * P + n * m) / N, held in [S/2, 2S] with a seed S;
  - sized = max(published, m, E);
  - from N Pods on, both are E.

  P is the seed when one is set, else 70 s. The 5b guard compares with E
  alone (`TestStartBlend_TheGuardReadsTheMeasurementNotTheBlend`).
- **Why E is in the max.** The design said max(blend, m). The fold is an
  EWMA, so with two Pods m can sit below E: 460 s then 15 s give m 237.5 s
  and E 326.5 s, and sizing at max(blend, m) read a shorter start than the
  parent did (`TestStartBlend_SizingNeverReadsShorterThanTheParent`).
- **A longer start can order less.** `backlogAtLanding` credits each Ready
  replica with mu x S of drain, and past the 60 s drain time `DrainHorizon`
  spreads the backlog over S. On a fleet that drains faster than work
  arrives, a longer S orders less. On the floor rig at λ 2.5, a first start
  of 15 s published as 51.7 s cut the order from 2.91M to 2.32M tokens. So
  the floor prices twice, at 5c's start and at 5b's (the fastest measured E,
  no seed), and each role keeps the larger order and names the start that
  priced it (`roleStarts`, `TestStartBlend_ThroughAnalyzeALongerStartNeverOrdersLess`).
  Where the two start maps are equal, it prices once.
- **The seed (A).** A role with no measured variant is sized at its
  fastest variant's seed: projected by the floor (`startSource: seed`) and
  read by share pacing. Pacing is the one reader a seed can move *down*: a
  seed under `shareModelLoadEstimate` (5 min) paces moves faster than main
  did. The timing source says `seed` (published as `scaledobject`), not
  `measured`, through `AnalyzerResult.RoleStartSeeded` (omitzero, so no
  recording changes). Beside a measured sibling, a seed never shortens the
  floor's start: the fastest *measured* variant wins. Share pacing takes the
  slowest variant as sized, seeds included. An unseeded, unmeasured role stays
  unprojected. A seed of 0 or less is no prior.
- **The gauge.** `wva_replica_start_seconds_estimate` publishes the published
  figure, with `source` `default`, `seed`, `blended` (fewer than three Pods
  measured, with a prior) or `measured`, and deletes the other children. With
  the seed switch off, the labels are the parent's (`seed` for the default,
  `measured` for a blend). Round 1 added `blended`: published as
  `measured`, a seed of 3600 s contradicted by a 70 s start read as a
  measurement while sizing held at 1800 s or more.
- **A contradicted seed warns.** While the measured mean lies outside
  [S/2, 2S], `A variant's replicaStartSeconds is contradicted by its
  measured starts` is logged at the default level, once per key and seed
  (working state, so again after a restart). It is the analyzer's line, not
  the engine's ignored-seed report: only the analyzer knows the measurement.
  A one-replica variant can stay there for days, and the count survives a
  restart.
- **Persistence.** Two new `omitempty` fields: `startSamples` (per key, pods
  and mean, only while fewer than N) and `startAdmittedAt`. The design had n
  and m in one field; it holds both. An estimate with no count has full
  authority, so an older document moves no fleet. A count no live key could
  hold (no Pods, N or more, a mean that is not a positive number) is not
  installed, and a restored mean over 3600 s (the collector's
  `maxCredibleStartSeconds`) is clamped to it. Both are exported beside a
  pending restore's estimate, dropped
  with the variant, and trimmed with its liveness in the store.
- **Learner `start`.** It is on `learn.Learners` (the slo analyzer holds
  none of it). Each cycle it files a `learn.Report` per variant, with
  `Samples` the Pods counted and `LastAdmitted` the stamp. It logs
  `learner-write` at V(3) when the published figure is first set, changes
  source, moves `StartWriteBand` (5 %, unmeasured) or reaches the heartbeat.
  The line carries `restored: true` until the key's first admitted start
  after a restore. A forget drops the estimate, count, stamp and log memo,
  and reports each key forgotten.
- **Not built here.** `learner-start` does not count start keys yet, and
  the "Learned state: snapshot restored" line does not gain `startKeys`; both
  are 5d's, with `sightings` (see "5d as built").
- **`replica-start-measured`** gains `pods`, `prior`, `priorSource` and
  `sizedSeconds` with the blend on. Off, it is the parent's line byte for byte.
- **Goldens.** Measured alone against all off, the seed switch moves nothing:
  no recording seeds a variant. The blend switch moves twelve outputs, every
  figure up and none down. `RoleStartSeconds` rises where a key has fewer
  than three Pods: 63 to 67.67 (ols-sweep, decode), 62 to 67.33 (prefill),
  60 to 66.67 (both ols-sweep-slo), 68 to 69.33 (pd-restart, prefill), and
  231.6 to 234 (e1-tuned). `learned-state.csv` gains a `StartAdmittedAt` row
  per key per export. No `TotalDemand` and no variant capacity moves. The
  property case `first-sample/start-time/between-sample-and-default` passes
  and leaves the known failures; `seed-bounds` is new.

#### 5d as built

Built on main 59772fce2 (5c and #482 merged). Changes D and E, snapshot on only,
behind two switches, both on from the constructor:
`saturation.SwitchStartCountedPodsRestored` (`start-counted-pods-restored`) and
`settle.SwitchFirstSightRestored` (`settle-first-sight-restored`), the settle
package's first switch, with its own hook (`Tracker.SetSwitchesForReplay`,
`learnreplay.PackageSettle`).

- **D, counted Pods.** The analyzer keeps each counted Pod's latest Ready time
  (`startCountedReady`, working state), and the snapshot stores it as
  `startCounted`, keyed `key|namespace|pod`. The latest, not the first: a counted
  Pod that blipped stays counted live, so the document holds the time it will
  report after a restart. On restore the times are installed beside an installed
  estimate only (`startCountedRestored`): a Pod counted into an estimate that was
  not installed has to be measured again. A row with the stored Ready time is
  counted and not folded. Another Ready time, or none with a start, is folded as
  today. A row with neither a Ready time nor a start says nothing, and the stored
  time waits for the next row. A refused Pod is not counted, so not stored.
- **E, first sight.** The engine owns the tracker, so `settle` sits in
  `learnedStateDoc` beside `state`: per model, each replica's Pod and variant and
  its `firstSeen`, `readySince` and `readySeen` (`settle.Sighting`).
  `Tracker.Restore` holds them until the replica's first row claims one. Only a
  Ready row with the same non-zero Ready time takes it. Any other first row loses
  it and the replica waits as one first seen. A sighting installed starts no
  settle and counts no verdict. Its settle is dated from the later of `firstSeen`
  and `readySeen`, all three times can say.
- **A departure from the design: a bridge is never kept.** The design says a new
  lend changes the Ready time. It does not: a lend keeps the Ready time the Pod
  had in the pool, and a Pod lent again to the same variant keeps its key
  (`settle.Key`, #283). The three stored times cannot tell a new lend from the old
  one, so every bridge waits after a restart, as with the snapshot off. That costs
  a bridge that was not re-lent one wait, on the SLO side
  (`TestRestore_ABridgeIsNotKeptSoANewLendStartsAgain`,
  `TestFirstSight_ANewLendIsABridgeFirstSeen`). Storing the lend time as a fourth
  field would keep nothing: `LentSince` comes from the process-local
  `decision.DefaultBridges`, whose `Publish` dates a Pod it has no previous entry
  for at this pass, so after a restart every lend reads a new time and a stored
  one could never match. Keeping a bridge would need `BridgeStore.since` itself
  persisted.
- **The age bound.** A restored sighting would otherwise have no age limit, where
  the same gap in one process gives `Rejoined`: with the controller down for hours
  and load back just before it returned, every replica would skip the wait in the
  one case it exists for. So a document older than `sightingsMaxAge`, the settle
  tolerance plus `learnedStateInterval`, gives back no sightings, and the log says
  why. That is 2 min at the default interval: section 4.4's controller back within
  about 2 min. A planned restart flushes on the way out, so its document is as old
  as the gap. A crash is not as lucky: a steady fleet's document is rewritten only
  by `learnedStateRefresh`, so it can be up to 15 min old, and then every replica
  waits, as on the parent. The bound errs that way on purpose
  (`TestLearnedStateSettle_ADocumentTooOldGivesBackNoSightings`, at 1, 2, 6 min,
  1 h and 72 h, with 2 min + 1 s refused). `startCounted` has no such bound: a Pod
  with the same Ready time has the same start, however long the gap.
- **Absent replicas.** One already absent past the tolerance when the document is
  written is not kept: back in this process it would settle again.
- **Unclaimed sightings.** One no row has claimed is written back with the next
  document, so a second restart inside the first loses nothing, and goes after
  the tolerance, as a replica missing that long would settle again. `Retain`
  leaves it, because the first cycles after a restart can see no active model. A
  replica this process already observed keeps what it saw: a read that succeeds
  late overwrites nothing.
- **A flush.** Deleting the snapshot drops the unclaimed sightings. The replicas
  the tracker already holds keep theirs, as they keep their settle: a sighting is
  something seen, not something learned, and the next document written holds it.
  A benchmark reset that deletes the snapshot and restarts starts cold, since the
  flush path writes nothing to a deleted store.
- **No off switch of its own.** The two switches are replay-only
  (`SetSwitchesForReplay`, which no production path calls). The way off is the
  snapshot: `LEARNED_STATE_SNAPSHOT` off stores and restores neither field.
- **Spare entries.** Both fields are spare at the size bound, oldest first: a
  dropped sighting costs one replica one wait, a dropped count one more fold.
  With 64 Pods in each field and long names the test measures 135 B an entry, at
  whole-second times. A live `firstSeen` or `readySeen` is the controller's clock
  and carries nanoseconds: the kind run's documents hold 138 B per counted Pod and
  207 B per sighting.
  The sightings are in the digest, and their times do not move, so a steady fleet
  does not rewrite the document (tested).
- **Recording.** `settle.KindRestore` carries the sightings with the namespace
  and model ID apart, not the engine's key, because a recording's export renames
  each whole. A counted Pod's key gets its own layout in the export (seven parts).
  A replay counts installed sightings as `settle-restored` in its paths, only when
  above zero, so no existing golden moves.
- **Logs.** `learner-start` gains `startKeys` and `sightings`.
  `settleUntil` covers the replicas that wait: a window from this cycle while any
  is cold, else the end of a restored wait under way, else this cycle. Its
  `reason` says when sightings came back. "Learned state: snapshot restored" gains
  `startKeys` and `sightings`.
- **Which way the fleet moves: both, not only lower.** The design says E sizes
  the fleet lower, and that is half of it. Measured on four saturated replicas
  (`first_sight_price_test.go`), the same rows and document, on against off:

  | case | on | off (the parent) | meet |
  | --- | --- | --- | --- |
  | no k2 on record | capacity 260000, 9.05 replicas from the first cycle | 301184 (k1), 7.81 replicas for 4 cycles | cycle 4 |
  | stored k2 200000 is stale, readings 260000 | 10.94, 9.84, then 9.05 replicas | 11.76 replicas for 4 cycles, then 10.94, 9.84, 9.05 | cycle 6 |

  With no k2 the wait holds k1, the high figure, so E orders **more**: the SLO side
  4.4 names, ended sooner. With a stale low k2 the readings the wait refused raise
  the window sooner, so E orders **less**. A third effect has no replica count of
  its own: with the ITL wait gone, a line, and the floor priced from it, can appear
  up to `steadyFor` (2 min by default) sooner.
- **What bounds the lower fleet.** The size has no ceiling. Against the parent's
  figure the order falls by demand x (1/stored - 1/min(k1, readings)), so it is as
  large as the stored k2 is wrong (`first_sight_floor_test.go`, readings 260000):

  | stored k2 | lower by, replicas |
  | --- | --- |
  | 200000 | 2.71 |
  | 100000 | 14.48 |
  | 50000 | 38.01 |
  | 25000 | 85.07 |
  | 10000 | 226.24 |

  The bound is the duration and the floor. E is ahead of the parent on 6 cycles,
  about 90 s at a 15 s cycle, and it stops at demand / min(k1, readings), never
  under: where the parent lands a minute later from the same readings. At a very
  stale k2 the parent's figure is the wrong one, a burst E avoids. E under-orders
  against true need only if the readings overstate capacity, and k1 caps those.

  It reaches only a replica whose document is young enough and whose Ready time is
  the same and non-zero. An engine restart that moves the Ready condition, an LWS
  group recreated with names kept, and a worker restarted alone (the group's row
  carries the latest Ready time) each wait
  (`TestFirstSight_AnEngineRestartUnderTheSameNameStillWaits`,
  `TestFirstSight_AnLWSGroupRestartStillWaits`). A wait begun by a join or a
  restart keeps its end (`TestFirstSight_AWaitInProgressKeepsItsEnd`). A reading
  past the wait still has to pass k2's admission: a queue deeper than the batch
  needs the rate or settled test, whose memo starts empty. With the snapshot off
  nothing is written or read (`TestLearnedStateSettle_SnapshotOffMovesNothing`).
  The default 300 s scale-down stabilization should hold the fleet over the gap,
  as 4.4 says of the wait; that is assumed here, not run.
- **What the check cannot see.**
  - Load that returned during the restart is missed: the idle run is not stored,
    and the parent's blanket wait covered it. The age bound limits how long a gap
    this can hide, not whether it happens inside 2 min.
  - An engine restart too fast to move the Pod's Ready condition is invisible, live
    as well, but the parent's wait covered it after a controller restart.
  - A rollout under a Pod's own name with no new Ready time. The tracker's `Since`
    for it is not stored, so a `RolledOut` wait under way when the controller
    restarted does not keep its end: the sighting's settle dates from `firstSeen`
    and `readySeen`.
- **The closed-loop probe sees neither.** Through a restart and a step, and through
  a restart under steady load, E moves no cycle of the probe's fleet (0 of the
  run's cycles differ, sightings restored 4 of 4): restored windows and the held
  line already price it. So the probe holds the outcome (need 17, final 17, under
  need 15 s, as with the snapshot alone) and is not the evidence for the bound.
- **The scripted engine run.** Its restart is 24 min after its last write, past the
  bound, so it restores no sightings and its pins are the parent's. A restart 1 min
  after the last write, the only change, prices more variants: slo 14 against 6,
  throughput 12 against 8 (`TestLearnerRecording_ARestartInsideTheBoundPricesSooner`).
  That is a time shift, not a ratio: E alone causes it, pricing starts about 120 to
  135 s sooner, and the fixture's 25 h clock jump cuts the run short, so the count
  is of cycles before the jump. With the snapshot off the run is byte-identical to
  the parent on all 12 cycles.
- **Goldens.** Measured alone against all off, D adds rows to three
  `learned-state.csv` (acceptance 103, engine 99, mu-paths 180), changes none,
  and moves no other output there: no recorded document holds `startCounted`. The
  three start-seed goldens gain the same rows (38, 80 and 51). On `start-restored`,
  whose document holds it, D also moves `results.csv` and `composite-results.csv`:
  off, the running Pods are folded again. E alone moves no output of any golden.
  On `start-restored` its restored windows and lines price the fleet either way,
  so it shows in the refusals, not in an order.
- **The real run.** `start-restored` is a kind run of a build of this branch
  (9c7aa470c), the steps of "Recording", with the snapshot on and two controller
  restarts. It is the recording between them: a real `settle.Restore`, and
  `startCounted` read back by a real restore.

  | restart | document age | `startKeys`, both lines | `sightings`, both lines | slow variant before and after, ConfigMap | fast |
  | --- | --- | --- | --- | --- | --- |
  | first, under load | 18 s | 2 | 10 | 242.147, 242.147 | 22, 22 |
  | second, light load | 15 s | 2 | 10 | 242.147, 242.147 | 22, 22 |

  Replayed, k2 refuses 0 readings as joined with E on and 17 with E off, and
  `learner-write` says `restored: true` with D on and never with D off, where the
  same recording folds the running Pods again
  (`TestGoldens_StartRestoredRestoresForReal`). Both restarts were well inside the
  bound, so the run does not exercise `too old`; a test does.
- **The start-seed goldens** predate 5d, so replayed as committed they still fold.
  `TestGoldens_StartSeedChainRestoresCountsAndFirstSight` splices in what each run
  stored on this build, Pods carried across by variant and Ready time: it gives
  241.4599 and 241.6593 with D on, against 241.6593 and 241.7678 off, the 241.66
  and 241.77 the cluster measured then. That test is spliced; `start-restored` is
  not.
- **Back-compat.** The document stays v1 and additive. v0.10.1's frozen decoder
  reads a document with both new fields with every field it knows intact
  (`TestV0101Decoder_ReadsADocumentWithSightings`). The v0.10.1 fixture
  restored under light load runs on its held slope, and the v0.10.0 one, with no
  slope, restores and has no line, as that release did
  (`TestFrozenDocuments_ALightLoadRestoreRunsOnTheHeldSlope`, the restore-then-run
  5a left here).
- **Tests.** The design's negative controls, each asserted both ways: restored 70
  and four Pods at 90 s stays 70 on and reaches 85.2 off; four replicas Ready 1 h
  are admitted on the first cycle on, and refused `P1-obs-unsettled` for 1 min off.
  Sixty-four mutants, each killed in a full run, among them
  the Ready check, the Ready-time equality, a bridge kept, the first row not
  claiming, the switch-off paths, each `settleUntil` branch, the age bound off, at
  `>=`, without the write interval and at the forget horizon. Mutants that survived
  an earlier pass showed an export filter and a clone were dead, both removed, and
  that the claim and the spare order were not reached, which tests now reach.

#### Reports, restored and mu's forget, as built (#495)

Built on main f7130a9cb. One replay switch, `saturation.SwitchMuForgetReported`
(`mu-forget-reported`), on from the constructor, for mu on the list; the series
and the `restored` field have none, because no replay output reads either.

- **Reports reach the engine (6.2, 6.3, 3.6).** `TakeLearnerReports` hands them
  over and keeps none, and the engine takes them at the end of each cycle.
  It is a capability (`learnerReporter`), as `TakeRestoreOutcomes` is.
  `LearnerReports` stays for the replay. The take is unrecorded: nothing the
  analyzer prices or stores reads the reports, so `coverage_test.go` files it
  with the readers.
- **One series.** `wva_learned_state_last_admitted_timestamp_seconds`, labels as
  6.2 plus `learner`, at most 4 per variant (k2, mu, itl, start), against the
  budget of about 60 per model. The `learner` label is bounded by
  `learn.Learners`, and a test holds a variant to one series through 1000 key
  changes. Only the saturation analyzer filed reports when this was built; the
  slo and throughput lines [do now](#rollouts-k2s-wait-and-the-lines-reports-as-built-481-495).

  | overlapping series | kept or replaced |
  | --- | --- |
  | `wva_learned_state_k2_age_seconds` | kept. It is the age of the k2 that priced the variant this cycle, the donor's for `P2-borrow`, and goes when nothing was priced. The new series is the variant's own key, and stays |
  | `wva_learned_state_k2_source` | kept: it says which source, which the new series does not |

- **`pricingKey` and `forgotten`, applied.** 6.2's "no key remains" cannot be read
  from a report: an empty `PricingKey` also means the variant priced from a key
  with no window while its other keys stand, and reports do not list them. So the
  metrics layer remembers the key behind each series, and deletes only when a
  report with no `PricingKey` names that key `Forgotten`. A report with no key
  leaves the series, and so does a model parked, or one whose `Analyze` failed.
  A key whose only window is in a stash is named too, on a forget and when the
  sweep evicts the stash, or the series of a key set aside by an args change
  would stand for good. The series also goes when the variant is deleted, which
  takes two tests: it is missing from `KnownVariantAutoscalings`, and a read of
  the scale target recorded at publish time says NotFound or finds it being
  deleted. The registry alone will not do: a KEDA outage past `DefaultTTL` empties
  it, and a trigger typo drops a variant from it. Any other read keeps the series,
  Forbidden included, and `learnerSeriesMissingMax` (24 h, an assumed figure, not
  measured) ends it whatever the reads say. Not where the engine deletes the
  analyzer series: those follow the active set, and a model at zero replicas is
  not in it. Nor where the
  k2-source series goes on a cycle with no replica rows: that one describes this
  cycle, this one the evidence. Reasons, writes and forgets are not published: 6.3
  lists them in the Report, and no alert or operator question needs them yet.
- **`restored` (6.1).** On `learner-write` for k2, mu and ITL, as start already
  had it, and on `k2-decision` at `P2-hist` for `dump_k2_decisions.py`'s Restored
  column. V(3) lines only: the default level gains nothing. The marks live on the
  figure (`RollingAverage.Restored`, `itl.Tracker.Restored`), so a stash, a forget
  and an eviction need no rule, and the snapshot is unchanged. Mu's exact repeat
  is use, not a write, so a fleet whose rate does not move stays restored while
  it serves, and its `LastAdmitted` stays old with it. That is what 6.1 says, and
  a test holds it. An ITL intercept put back from `itlBaselineStash` reads
  `false`, restored or not: the args change that stashes it sweeps the key.
- **Mu on Forget (6.2).** On `learn.Learners`, so the slo and throughput forgets
  each gained a `learn.Mu` case that drops nothing. `previousValue` was stale, by
  test, not only by reading: `muLogged` is `scopeShared`, so `dropVariantLocked`
  skipped it, and the first line after a forget read the value from before it.
  `forgetMuLocked` clears it, and counts the entries the variant's forget counted
  before. The prefill bound shares mu's maps: its keys still go with the variant,
  and are not reported. Eviction reports too, as k2's does.
- **Open.** Not measured on a cluster: no run has read this series. B's own
  last-admitted stamp, Deferred here, is
  [built since](#bs-last-admitted-stamp-as-built-495).

#### B's last-admitted stamp, as built (#495)

Built on main 1573829cd. One replay switch, `saturation.SwitchITLBaselineStamped`
(`itl-baseline-stamped`), on from the constructor.

- **The stamp.** `itl.Tracker` keeps, beside each intercept, when it was last
  admitted: the newest reading of the full fit that set it, not the cycle's
  clock, so a refit from the readings the steady window carries does not make B
  younger. `Tracker.Report` publishes it for a key with no reading in its window,
  no held slope and no above-band fit, where it published 0. A B put back from
  `itlBaselineStash` has none: the stash holds bare floats. The series reads a
  report only when it names a pricing key, and a B restored alone prices no line
  on an empty window, so no series changes. Nor does a live line: the stamp is
  read for a line with an empty window and no held slope, and `Observe` makes
  none, since both windows age out on `DefaultObservationMaxAge` and an empty
  main window leaves no steady reading to fit. What the stamp gives today is a
  correct `Report.LastAdmitted` for a restored B, for any reader of the report
  beyond the series.
- **The snapshot.** `itlBaselineAdmittedAt`, keyed like `itlBaseline`, additive
  and `omitempty`; the document stays v1. Exported beside a baseline only,
  pending with its variant, installed with B and never without it, and not in
  the future past `maxClockSkew` (one inside it is taken as now). A v1 document
  without it loads, and its B reports 0, as before. `dropVariant` and the size
  trim drop it with B; a stamp left with no B is spare and goes first. The store's
  digest leaves it out, as it leaves out a slope's `fittedAt`: a full fit moves it
  every cycle.
- **Goldens.** On, it moves `acceptance/learned-state.csv` and
  `mu-paths/learned-state.csv`, rows added and none changed (2 and 27): an
  `ITLBaselineAdmittedAt` row per baseline per export. No recorded document holds
  the field, so no restore reads it and no figure moves. The report's stamp has
  no switch: no replay output reads a report. Slo and throughput lines report
  the same way for their own intercepts.
- Not measured on a cluster.

#### Rollouts, k2's wait and the lines' reports, as built (#481, #495)

Eight replay switches, on from the constructors. None moves a replay golden: no
recording holds a rollout, a change of arguments or a new revision under a key,
and nothing priced reads a report. Their unit tests hold them. Nothing here has
run on a cluster.

| switch | what it changes |
| --- | --- |
| `throughput-itl-rollout-guard`, `slo-itl-rollout-guard` | mid-rollout (`Record.InRollout`, or two revisions in the rows, `domain.RevisionsMixed`) the line is offered the newest revision's rows alone, and the first of them clear the window. With no new pod serving it is offered none, and holds |
| `throughput-itl-forgets-on-new-arguments` | the throughput line forgets on a change of `ArgsTag`, keeping the traces. While the guard holds, the forget waits for a new pod |
| `saturation-itl-clears-on-a-new-revision` | the saturation line, which takes no rows mid-rollout, clears its window on the first rows of a new revision. The slo and throughput lines do it under their guards |
| `k2-waits-after-a-rollout` | `settle.RolledOut` starts k2's joined wait |
| `slo-itl-files-learner-reports` | the slo line's `learn.Report` and `learner-write` |
| `throughput-itl-files-learner-reports` | the throughput line's `learn.Report`; its `learner-write` stands as 4b-1 built it |

- **The guard (`itl.Tracker.RolloutRows`).** A key remembers the revision its
  readings are of, and the rows decide, not the status, which a plain scale trips
  too (`ObservedGeneration < Generation`). The pods cannot say which revision is
  new, so `domain.NewestRevision` takes the one whose oldest Ready pod is youngest:
  a rollout makes new pods, and an old pod restarted in place does not move its
  revision's oldest. Two revisions that cannot be ordered are treated as no new pod.
  With the guard off, a test holds the defect #481 names: an old pod above the
  band counted as at the new cap (`TestRollout_AnOldPodIsNotJudgedAgainstTheNewCap`).
- **A stalled rollout (round-one review).** As first built, an arguments change
  forgot the line on the rollout's first cycle and the guard then offered no rows,
  so both lines were silent for as long as the rollout ran: 720 of 720 cycles on a
  3 h stall. Now a line is offered the new pods' rows once one serves. With none
  serving, it holds what it has, whether or not the arguments changed, because the
  old pods are what serves and the old line prices them. The forget waits for the
  first new pod; the line stands under `Args` it was learned under meanwhile. It
  logs `rolloutHeld` (`throughput analyzer: ITL line`, and `lineRolloutHeld` on
  `slo-variant`). What prices is the window for 30 min, then the held slope.
  Measured on the 3 h stall of `TestRollout_AStalledRolloutDoesNotSilenceTheLine`,
  on both lines, at 15 s: no silent cycle with no new pod, none with a new pod and
  the same arguments, and at most 16 (4 min) with a new pod and new arguments.

  | rollout | what prices | silence |
  | --- | --- | --- |
  | no new pod, either way | the window, then the held slope | none until the hold ends: `steadyGate` window (30 min) plus `BucketRetention` (7 days by default) |
  | a new pod, same arguments | the held slope until 10 new readings | none |
  | a new pod, new arguments | nothing: B and the slope were the old engine's | until 10 settled readings: at most 16 cycles measured with one pod |

  Past the hold the line is silent with `cause` `rollout`, and the composite does
  not read that as no need. A silent line gives no capacity, so the analyzer is not
  eligible (`allocation.Eligible`): it neither sizes the role nor votes it down.
  With another analyzer listed that one decides. Listed alone, `CompositeHasSignal`
  is false and the role is held: it neither grows nor shrinks. Tests on both lines
  hold it (`TestRollout_PastTheHoldTheCompositeReadsNoNeed`).
  A controller restart mid-rollout loses the revision: with one revision in the
  rows the key cannot tell old from new, so it holds until the status clears or a
  second revision shows.

  A new pod that flaps does not move the key (round two). While it is not Ready
  the old revision is the only one Ready, and a key already on the new revision
  holds: no clear, no old rows, and the new pod's readings build up across the flap
  (`TestRollout_AFlappingNewPodMovesNothing`). Before, each flap cleared the window
  twice, so a crash-looping rollout never reached 10 readings.

  **Limits.** Each of these can mislabel the newest revision. Each is read from
  the code, not measured, and each is bounded by the window clearing again, so no
  fit spans two engines for longer than the next move:

  - a rollback mid-rollout;
  - a restarted pod that is the last Ready pod of its revision, since it makes that
    revision's oldest Ready pod young;
  - a worker flap in the last LeaderWorkerSet group, for the same reason.

  A stuck rollout's hold is log-only: `rolloutHeld` is on the summary line and
  `lineRolloutHeld` on `slo-variant`, and no metric or alert carries it.
- **The tier latch and B (#498, round-one review).** Resetting the latch alone kept
  nothing out: a full fit over both engines' readings rewrote the stored B for
  good. On an image-only rollout B went from .0100 to .0027 with one new pod, about
  47 % too much capacity with three. So the first rows of a real change of revision
  clear the window (`clearWindowLocked`), which keeps B and the held slope and puts
  the latch at the pinned tier: the line holds its slope until 10 new readings
  arrive, and a fit never spans two engines. The clear comes at the first new rows,
  not at the rollout's end: the review asked for the end, but on the slo and
  throughput lines the new pods teach during the rollout, and a clear at the end
  would drop what they taught. The saturation line takes no rows mid-rollout, so
  its first new rows are at the end. A cleared window also stops an idle fleet
  re-stamping the slope's `fittedAt`, so the hold ends on time. The tier stands
  through a hold, the window being frozen. A status with no change of revision
  clears nothing.
- **"Forget the line".** #481 lists it without saying when. Read here as the
  arguments rule the saturation and slo lines have, and the table in
  [PR 4b](#pr-4b-the-throughput-analyzers-itl-line) records this line lacking
  ("no args rule; no rollout rule"). Not a forget on every rollout: that would
  silence an image-only rollout, which the saturation line does not do. There is
  no B stash: a rollback relearns, as on the slo line. The saturation line's own
  arguments rule still forgets on the rollout's first cycle: it has k2 to price
  from, and this round leaves it. A forget files `learn.ByArguments`, so its series
  goes until the new line prices.
- **k2's wait.** Only a new revision that arrives with the Ready time unchanged
  or unread: a new Ready time waited already, and a new Pod is a join. Only
  `r.Reason == settle.RolledOut` is read, not `Since` for every reason, so
  `Rejoined` and load returning after idle keep what k2 does with them today.
  Mu's write sits behind the same joined test. A sighting in the snapshot carries
  no reason, so a controller restart that restores it ends the wait, and one with
  no snapshot starts every replica's wait anyway.
- **A parked variant.** The throughput analyzer now takes `NoteVariantsPresent`, as
  the slo analyzer does, or a parked variant lost its state and its series after
  1 h (`throughput-itl-keeps-present-variants`). The call is recorded, which moves
  `engine-run-event-counts.txt`: one more `throughput.NoteVariantsPresent` per
  noted model. A state kept across a long gap also holds its slope after it: in the
  recorded engine run, 2 more cycles price from pd-d's held slope after a day's
  clock jump.
- **The series.** The label is the report's `learner`, so the three lines need
  three names: `learn.SLO` (`slo`) and `learn.Throughput` (`throughput`), bounded
  by `learn.Reporters`. Neither is on `learn.Learners`: the annotation forgets both
  under `itl`. That takes the series to at most 6 per variant. `learner-write`
  keeps `learner` `itl` on all three lines, as the throughput line already
  logged it, and `analyzer` says which. The engine takes reports from every
  built-in analyzer with `TakeLearnerReports`, not the saturation analyzer alone.
- **Forgotten and evicted keys** go out as reports of their own ahead of the
  cycle's, so a key refilled on the same cycle stays published. They are held
  until the engine takes them, not until the model next reports, since a swept
  model reports again never. The slo line evicts through `ForgetKeys` to learn
  which keys went; the counts are `Sweep`'s.
- **A line above the band** has an empty main window. `Tracker.Report` stamps it
  with the newest reading there, or the throughput line's series would read 0.
- **`restoredKeys`** on `learner-start` gains `itl`: lines whose intercept or held
  slope came from the snapshot and have kept no reading (`Tracker.RestoredKeys`).
  One field on one log line, which no output reads, so it has no switch, as
  `restored` has none.

#### One slow pod, as built (#495)

Built on main 2f02cdbbe, merged with main 1879d7207. Mu's estimand is the fleet's mean (2.6), but a sample
reads only the replicas that queued, so what it meant depended on who queued.
Without the slow pod it read high: {1, 1, 1, 0.341} priced 1.000 against a mean
of 0.835, because half the served max refused the slow reading. With only the
slow pod it read low, and low seals: sixteen replicas, one at 0.55 for 10 min,
ordered 28 against a need of 18 and stayed there. Four replay switches, all on
from the constructor (`D/mu_slow_pod.go`). The first two move goldens, named
in `replaySwitches`: the weighing ten outputs of `mu-paths`,
`start-seed-restart-2` and `ttft-staircase-b-off` (a refused reading now a
sample, and samples counting their unread replicas: priced mu down 3.6 % to
5.7 % on those cycles), the cap `mu-paths` variant capacities (variant-02
priced at its measured 8.08 where it had yielded to its siblings' 12.12). The
last two move none, and `heldBy` names their tests.

How a lower mu moves demand, measured on those goldens. The floor's tokens
rise as mu falls, and `ttft-staircase-b-off`'s decode demand is up on four
cycles (to +7.7 %). It is down on four others (to -4.2 %), and
`start-seed-restart-2`'s total demand falls on 14 cycles (21:22:34: 1527 to
699, -54 %) with occupancy's per-variant demand unchanged. That is the floor's
trust blend (`flooredDemand`): it trusts the floor's drain price over the
queue's residency charge by lambda x P / mu against the resident KV, and a
lower mu raises that ratio, so more of the residency charge is swapped out.
The queue cap (`localQueueBacklog`) is not the term: the capped backlog is the
same either way (55.99 requests at 21:22:34), and in replicas the cap is
min(q / (mu x T), 1) per replica, which never falls as mu does.
`TestMuSlowPod_TheFloorTrustIsHowALowerMuLowersDemand` pins the cycle. Whether
a lower mu should lower the demand charged for a measured queue through the
trust blend is a question for the floor, not this change; it is filed apart.

- **The sample weighs the fleet (`mu-sample-weighs-the-fleet`).** No served
  rate refuses a reading. While the readers sit under `siblingMuTolerance` of
  what the unqueued replicas they did not read serve, those count in the sample
  at that rate. A served rate is a lower bound, so the sample errs low.
- **The yield is capped at the measurement
  (`mu-yield-capped-at-measurement`).** A sibling yield never prices mu above
  the measurement it replaces. It opens only while the measurement sits under
  `siblingMuTolerance` of what it yields to, so the cap refuses every yield,
  and what the siblings serve reaches mu only through the weighed sample.
  Uncapped, {1, 1, 1, 0.569} yielded to 1.000 against a mean of 0.892, and a
  build that yielded to the fleet's figure instead priced one `ttft-three-pods`
  cycle at 5.18 against a measured 3.39: both on the side that breaks the SLO.
  The cap was the user's decision on review.
- **The unread replica's credit (`mu-unread-replica-at-target`).** An unread
  replica under the KV line counts at served / the saturation analyzer's
  `scaleUpThreshold` (`config.AnalyzerThresholds`, a per-analyzer override
  where set, the target the floor charges). Its rate is
  what it was offered, and the floor charges the target on mu again: counted at
  what it serves, a lower bound alone cannot clear the probe (siblings showed
  0.674, so 12.8 / 0.667 / 0.85 = 23 against a bound of 21.7). The error is at
  most 1 / target over what it served unqueued, and it corrects itself: a
  replica priced too high queues and is read.
- **A minority's figure gives way to the fleet's served bound
  (`mu-minority-figure-expires`).** With no line, a figure whose newest sample
  was read by fewer than half of the readers and unqueued replicas, and that
  sits under what the Ready replicas serve weighed as a sample weighs them
  (queued at their rate, unqueued at the credit), gives way to that bound after
  `lowHoldFor` with no saturated reading agreeing with it. Never to no mu: a
  served rate does not overstate what a replica sustains, so the release only
  lifts mu to what the fleet showed. 3.4 has the reason. A fleet that saturated
  together keeps its figure, and so does a restored window (no writers on
  record: the safe side), a borrowed reading, and a figure at or over the
  bound: a minority that a steering router saturated first read what every
  replica does, and the fleet at a lower load serves less than that. It logs
  once per key, on the transition. Review decided both the bound and the gate.

Measured on the closed-loop probe (sixteen replicas at 12.8 req/s, one slow for
its first 10 min; 60 min, past `lowHoldFor` after the recovery and the 5 min
stabilization), need 18, bound 21.7:

| case | all on | credit off | expiry off | both off | before #495 |
| --- | --- | --- | --- | --- | --- |
| slow pod at 0.55 | 21 from 1.75 min, 20 from 16.25 to the end; peak 21 | 28 for good, peak 29 | as all on | 28 for good | 28 for good |
| slow pod at 0.3 | 24 at peak, 20 from 21.5 to the end | 23 for good, peak 27 | as all on | 23 for good | 18 from 27, peak 19 |

- **The minority rule does not act on the probe.** Both cases settle at 20, a
  fixed point: each of the twenty serves 0.64, which over the target is the
  figure on record, so the figure sits at the fleet's served bound and is kept.
  As first built, releasing to no mu without the gate, it brought both to 18;
  released to the bound, a fleet at 20 is priced at 20. It still releases a
  figure under the bound (`TestMuSlowPod_OneReplicasFigureExpiresAfterTheHold`).
  With the credit off it no longer rescues the original defect: the 0.55 figure
  sits over what 28 replicas serve (0.538), so it is kept.
- **A steering router.** Eighteen replicas, four of them sent twice the share,
  12 req/s, a lull at 2 req/s from 10 to 45 min, then 12 again
  (`TestClosedLoopProbe_ASteeringRouterKeepsARightMinorityFigure`): the four
  queue and read 1, the right figure, and the rule keeps it (released on no
  cycle); the fleet meets the return at need (17, final 19, never under need).
  As first built it released that figure on 45 cycles, and the fleet moved the
  same way: the first queued reading at the return writes it back within a
  cycle, and the simulator's floor never binds above occupancy without a queue.
- **The cost of the fix.** At 0.3 the old served bound refused the slow pod and
  the fleet peaked at 19. Now it counts by its weight, and while the pod is
  slow the fleet peaks at 24, over the bound, and settles at 20, inside it.
  That is the estimand at work on a lower bound: siblings unqueued show less
  than their limit. The weighing's hold is what keeps the peak at 24: read
  fresh it peaked at 28 and ended at 23.
- **Open.** Not measured on a cluster, and the simulator cannot price a right
  minority figure released: its floor never binds above occupancy without a
  queue.

#### Queued work priced by drain time before mu, design (#520 cause B)

Built on `fix/queue-priced-as-drain`, behind one replay switch,
`queue-priced-as-drain` (`SwitchQueuePricedAsDrain`, `queue_drain.go`), on
from the constructor. The design follows as written, with the decisions taken
on it (at the end) and what the build changed and measured
([as built](#queued-work-priced-by-drain-time-as-built-520)).

**What goes wrong.** Occupancy charges each queued request as if it were
resident: queue x (input + output) tokens (`waitingQueueDemand`,
`estimateSchedulerQueueDemand`). The floor swaps that charge for a drain price
only when its term may order (two mu readings) and the backlog is not growing
(`flooredDemand`, gated on `backlogGrew`). A load step hits both gates at once,
so the whole queue is priced as resident KV. On `pd-restart` (P/D decode,
`max_num_seqs` 16, about 4 to 14 req/s) that is 9.87 replicas at 12:28:02
against a need of 3: 130 queued x 2000 tokens is 260k, beside 130k resident. In
the closed-loop probe the same charge gives peaks of about 2x need at 15 s and
90 s starts under the shortest-queue router (#529), and long overshoot at 463 s.

**Already rejected.** Counting starting replicas against the queue charge. The
engine already counts them as anticipated supply, so the credit counts them
twice. On the 463 s probe it put restart-before-step under need for 12m0s, over
the 11m13s bound (#520).

##### The price

```
arrival  = max(lambda, sum over Ready replicas of RunningRequests / AvgServiceTime + dB/dt)
mu       = 0.85 x mu0                                 before two mu readings
         = min(floor's mu, mu0)                       while the backlog grows
N        = (arrival + B / T) / mu                     replicas at 100 %
T        = floor.DrainHorizon(60 s, start estimate)   = max(start, 60 s)
demand   = max(resident KV, N x P, floor's tokens)    tokens; the engine divides by 0.85 x P
```

- **`mu0`** is the first-read mu. It is the median, over the role's replicas
  that are saturated now, of `RunningRequests / AvgServiceTime` (Little's law).
  A replica counts only when it is at its stated batch cap
  (`ReplicaMetrics.AtBatchCap`). The design also took a row whose KV is at
  or over k1 with a capacity queue; review measured those 1.6x to 1.9x high
  (as built, review round), so they keep occupancy's pricing. A queue alone
  is not enough either. On `pd-restart` at 12:28:32
  the new decode replicas had requests waiting for their KV from prefill while
  running far under the cap, and read 3.0 req/s against a true 6.2.
  `AvgServiceTime` excludes queue wait (`request_inference_time_seconds`). At the
  cap the batch is fixed, so the service time is the replica's own for this
  shape. That is not true of the service time the retired Little's-law floor
  priced at whatever batch the replica ran
  ([why it was retired](../developer-guide/saturation-demand-floor.md#why-the-littles-law-floor-was-retired)).
- **`dB/dt`** is the signed growth rate of the role's uncapped backlog: the
  smaller of the slope over one cycle and the slope over `BacklogGrowthLookback`,
  read on every cycle. The design said the larger, floored at zero and counted
  only while `backlogGrew` is set; the build measured why not (as built). A
  noisy negative slope cannot price under lambda, through the max with it.
  `noteBacklogGrowth` already keeps the readings.
- **The sum of `RunningRequests / AvgServiceTime`** is what the replicas
  complete now. The one-minute completion rate and lambda both read a step a
  minute late. Adding `dB/dt` gives the arrivals the fleet is not serving. Both
  only raise the estimate, through the max with lambda.
- **`B`** is the backlog `localQueueBacklog` builds: each engine queue capped at
  what its own replica drains in `T` (`mu x T`), plus the scheduler's queue for
  the role that owns it. The cap stays on while the queue grows. Growth is
  priced through `dB/dt` and no longer through an uncapped queue. On a decode
  role with `CapacityQueueKnown`, only the capacity part counts. The rest waits
  for KV from prefill, and decode capacity does not drain it. Main already
  reads it so: `ReplicaCapacity.QueueLength` is the decode capacity view
  (`decodeCapacityView`), the full count at the sequence cap.
- **No starting-replica credit.** `B` is the queue that stands now, not
  `backlogAtLanding`. Each cycle prices the queue standing then, and the
  engine's anticipated-supply subtraction keeps an increment from being
  ordered twice.
- **The floor's own tokens** stay in the max, so the price is never under the
  term the floor would publish today.

##### Where it applies

Only on a role that generates output (decode or both), only with a backlog, and
only where an `mu0` or an orderable mu exists. Anywhere else nothing changes.

| region | condition | today | proposed |
| --- | --- | --- | --- |
| before two readings | `mayOrder` false (no reading, one reading, or `HeldWhy` single-sample) and the role has a saturated row | occupancy: resident + queue x (in + out) | the price above, mu = 0.85 x `mu0` |
| growing | `mayOrder` true and `backlogGrew` | `max(measured, floor)`: the queue as resident | the price above, mu = min(floor's mu, `mu0`) |
| floor | `mayOrder` true, not held, not growing | the swap, blended by `floorTrust` | unchanged |
| stale shape | `staleShape` | hold | unchanged while the shape-change hold stands, and every haircut hold of the model is dropped: the service-time window mixes two shapes, and the error is not bounded by the haircut. The design's further 60 s wait after the hold clears is not built |

##### Checked against `pd-restart`

Computed per cycle from the golden's recording and replayed capacities (decode,
start estimate 85.6 s, then 72.4 s after the landing). The decode role takes its
second mu reading at 12:28:02. "Today" is the replayed demand in replicas at the
scale-up target, with the role demand in brackets where it differs from the
variant's. Need is lambda / (0.85 x 6.2). All figures are replicas at 0.85.

| cycle | Ready / starting | lambda | sum of run/svc | B | dB/dt | arrival | mu priced | region | resident | proposed | today | need |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 12:26:47 | 2 / 0 | 0.46 | 7.13 | 3 | 0.20 | 7.33 | 6.06 | before two | 1.99 | 1.99 | 2.28 | 0.09 |
| 12:27:02 | 2 / 1 | 5.98 | 12.67 | 29 | 1.73 | 14.40 | 5.38 | before two | 3.17 | 3.22 | 6.01 | 1.13 |
| 12:27:17 | 2 / 2 | 8.68 | 12.55 | 40 | 0.73 | 13.28 | 5.33 | before two | 2.64 | 3.03 | 5.25 | 1.65 |
| 12:27:32 | 2 / 2 | 13.96 | 12.47 | 76 | 2.40 | 14.87 | 5.30 | before two | 2.69 | 3.50 | 7.24 | 2.65 |
| 12:27:47 | 2 / 2 | 13.62 | 12.42 | 84 | 1.35 | 13.77 | 5.28 | before two | 2.91 | 3.29 | 7.66 | 2.58 |
| 12:28:02 | 2 / 2 | 14.18 | 12.39 | 130 | 3.07 | 15.46 | 6.19 | growing | 3.29 | 3.29 | 9.87 | 2.69 |
| 12:28:17 | 4 / 0 | 13.08 | 12.40 | 161 | 2.07 | 14.47 | 6.20 | growing | 4.32 | 4.32 | 10.65 | 2.48 |
| 12:28:32 | 4 / 0 | 13.39 | 11.55 | 165 | 1.48 | 13.39 | 6.21 | growing | 2.04 | 2.97 | 9.06 | 2.54 |
| 12:28:47 | 4 / 0 | 13.56 | 13.27 | 165 | 1.35 | 14.62 | 6.21 | growing | 0.54 | 3.20 | 6.87 (4.88) | 2.57 |
| 12:29:02 | 4 / 0 | 13.47 | 14.31 | 122 | 0 | 14.31 | 6.21 | floor | 0.89 | unchanged | 5.66 (3.44) | 2.56 |
| 12:29:17 | 4 / 0 | 14.50 | 14.20 | 14 | 0 | 14.50 | 6.21 | floor | 2.15 | unchanged | 2.75 | 2.75 |

- **It never orders under need.** The lowest margin is 12:28:02, at 3.29
  against 2.69, and the queue's B / T adds 0.29 of it. The arrival estimate
  leads lambda by a minute: 14.40 at 12:27:02, where lambda read 5.98 and the
  completion rate 5.14.
- **The first read of mu was high.** At 12:26:47, `RunningRequests /
  AvgServiceTime` read 7.13 against the 6.2 it settled at a minute later
  (+15 %). The haircut prices it at 6.06. The same first read was +13 % on
  `throughput-sweep-restart` (14.29 against 12.6) and +5 % to +6 % on `e1` and
  `e1-tuned` (6.53 to 6.58 against 6.2). Every one settled within three
  cycles. The completion-rate mu errs the other way, at 2.84 and 3.66 on its
  first two cycles here.
- **The demand peak falls from 10.65 to 4.32.** The 4.32 at 12:28:17 is the
  resident term. The two new replicas took queued work as they landed, so all
  four held their batch. It lasts one cycle (see what this does not fix).
  At `maxReplicas` 4 the fleet orders 4 either way, so the live run would not
  have changed. The demand golden does change.
- **Expected golden move:** `pd-restart/results.csv`: decode demand down on 9
  cycles (12:26:47 to 12:28:47), by at most 67 %. No cycle goes up. With
  `B` on the capacity queue, 12:28:02 reads 95 queued, not 130, which lowers
  the price by about 0.07 replicas.

##### Checked against the probe, analytically

These are estimates for the build to measure, not measurements. The probe's need
is ceil(0.95 lambda / (0.68 mu)), with P at k1. A saturated row reads KV 0.95,
which is 1.40 replicas of resident at the target. The rig's rows carry neither
`RunningRequests` nor `AvgServiceTime`, so the build adds both, consistent with
Little's law, and a first window that mixes service times from before the
saturation.

- **load-step at 463 s** (need 9; two replicas, mu 1; 1.2 to 6 req/s; T 463 s).
  On the first saturated cycle the arrival estimate is 6 and B is 60. At
  mu 0.85: N = (6 + 60/463) / 0.85 = 7.21, which is 8.48 at the target, so the
  fleet orders 9. Those land about 7m58s after the step. That is the time under
  need, against an 11m13s bound; today it is 8m0s. **Without the haircut the
  formula under-orders here:** at mu 1 it orders 8, and the 9th waits until the
  queue passes 370 (93 s), so the fleet is under need for about 9m31s. That
  margin of 1m42s is too thin for slower CI runners, and it is why the haircut
  is in the design and not an option. The drain term peaks at (6 + 2) / 0.85 =
  9.41, which orders 10: two queued replicas, each worth at most one.
- **restart-before-step at 463 s** (need 17; four replicas, mu 1; 2.5 to 12
  req/s; no mu after the restart). On the first saturated cycle: N = (12 + 120/463)
  / 0.85 = 14.42, which is 16.97 at the target, so the fleet orders 17, at need.
  It is under need for about 8 min. Without the haircut it orders 15 and is under
  need for about 9m31s. The drain term peaks at (12 + 4) / 0.85 = 18.8, so it
  orders 19, against today's 29 (even split) and 44 (shortest queue). The
  ceiling is 26.5.
- **What the estimate cannot settle.** Under the shortest-queue router the
  replicas that land are filled to the queue level, and each reads KV 0.95.
  The resident term then reads 1.40 x the fleet, and can carry the peak to
  about 13 to 14 on load-step (ceiling 14.5) and about 24 on
  restart-before-step (ceiling 26.5). That is occupancy's resident term, which
  this design keeps (open question 6).

##### Which side each term errs to

| term | errs toward | when | guard |
| --- | --- | --- | --- |
| `mu0` | too high, which under-orders (the SLO side) | the one-minute service-time window still holds completions from before the cap, measured +5 % to +15 % on the first saturated row | 0.85 x `mu0` until two readings. While the backlog grows, the lower of `mu0` and the floor's mu. Under a shape change, no `mu0` until one rate window has passed. After the haircut every recorded first read lands at or under the settled figure (6.06 against 6.2; 12.15 against 12.6) |
| `mu0` | too low, which over-orders (spend) | preemption under KV pressure, or an inference time that includes the KV load on a P/D decode | none needed. It is bounded by the floor's own figure once two readings exist |
| haircut | spend | always, before two readings | at most 1 / 0.85 = 1.18x, for about the minute until the second reading |
| arrival: run/svc sum | spend | the service-time window reads short just after a step | it only raises the estimate (max with lambda) |
| arrival: `dB/dt` | spend on jitter; low if the growth sits in a queue it does not read | jitter is cut by the `backlogGrew` band (10 %, 2 requests). The scheduler's queue is in `B` | this term is what replaces `backlogGrew`'s protection |
| `B / T`, with T too long | under-orders the backlog (the SLO side) | a start estimate that is too high | never under the arrivals. PR 5c's two estimates both price, and the larger figure wins (`pickLargerFloor`), which is the shorter T |
| `B / T`, with T too short | spend | an unknown start: T = 60 s | none needed |
| `B` capped per replica | under-prices a queue pinned on one replica | an even-split router | by design: added replicas cannot drain another engine's queue. The arrivals that queue grows by come in through `dB/dt` |
| resident KV | spend | new replicas filled with queued work as they land | not changed here (open question 6) |
| no starting-replica credit | spend, compared with the floor's `backlogAtLanding` | while replicas start | none needed. Crediting them is the rejected option |

The SLO-side guards, in the order the brief asked for:

1. **A first mu read high.** The haircut, the min with the floor's mu while
   growing, and the shape-change wait. Without the haircut the 463 s probe
   cases come within 1m42s of their bound (above).
2. **What `backlogGrew` protects.** A step from 3 to 9 req/s on one replica of
   mu 6 (`TestThroughputFloor_AGrowingQueueIsPricedAtTheTrueArrivals`). The
   swap priced it at 280 704, against occupancy's 387 263. With arrival =
   max(lambda read, 6 + 3) = 9, the proposal prices the true arrivals plus the
   queue at T 60 s: 452 469, 485 046, 517 624 and 550 202 at 15, 30, 45 and
   60 s. That is above the 390 933 the arrivals alone need on every cycle, and
   above the pre-step 319 763. It is under occupancy at 45 s (-0.8 %) and 60 s
   (-6.6 %), so the test's second assertion moves from "not under occupancy"
   to "not under (true arrivals + B / T) x P / mu". That is a named change
   (open question 2). The mutant that drops `dB/dt` prices 15 s at
   (6 + 85/60) / 6 x P = 322 158, under 390 933, and the new assertion catches it.
3. **No double credit for starting replicas.** `B` is observed, not projected,
   and starting replicas earn nothing in the drain term.
4. **T is the knob between spend and how long TTFT breaches.** A request queued
   at the step waits about one start plus T. At a 463 s start, T = 463 s means
   up to about 15 min of breached TTFT for the queue that stood at the step,
   on the fewest extra replicas. A shorter T orders more replicas for the same
   queue (open question 4).

##### P/D roles

- **Decode** is the role this design is for. Its queue is the engines' own
  (`pd-restart` carries no scheduler queue). Only the capacity part of
  `B` counts. `mu0` reads the decode replicas at their batch cap.
- **Prefill** is unchanged. With no mu, the scheduler queue's prefill share is
  already dropped (`scheduler-queue-prefill-share-dropped`), and a prefill
  bound is held to `LowerBoundMaxGrowth`. Prefill's `RunningRequests /
  AvgServiceTime` is a ratio of a few requests to about 10 ms, too noisy to
  order on. The scheduler's queue stays with prefill, its owner
  (`queueOwner`), and decode does not take it.
- **Both** (aggregated) takes the decode treatment and the scheduler's queue
  in `B`.

##### Interactions

- **The floor.** In the growing region the floor's tokens stay in the max. In
  the floor region nothing changes. Leaving either drain region moves demand up
  or holds it: on `pd-restart` it goes from 3.20 to today's 5.66 (role 3.44) at
  12:29:02. Today the same edge is a fall by the whole queue charge (role demand
  9.06 to 4.88 a cycle earlier). Under the proposal, `backlogGrew`, which
  lingers a minute after the growth stops, moves demand only by the `dB/dt` and
  mu differences.
- **The start learner.** T reads PR 5c's start through `DrainHorizon`. An
  unknown start gives T = 60 s.
- **KEDA stabilization.** Scale-up has no window, so the first order lands as
  priced. Scale-down holds the largest desired count for 300 s. Once the
  haircut lifts, the price drops (17 to 16 on restart-before-step at about
  16:15). With a start longer than the window, the lower count could take back
  a replica that is still starting. The probe cannot show this: it removes
  pods only when none are starting (`learnertest.Fleet`, `starting == 0`).
  The build checks what the engine does with a desired count under the
  current count while replicas start (open question 1).
- **Hysteresis (3.5).** A declared row. Set: a backlog on a generating role,
  with a saturated row, in either drain region. Clear: two readings and
  `backlogGrew` clear. Dwell: `backlogGrew`'s minute. The safe side is the
  floor region's figure, and the clear goes toward it. Restart: not persisted.
  A restart without a snapshot comes back in the before-two-readings region,
  under the haircut. A restored snapshot brings its mu readings back, and with
  them the floor region.

##### Evidence plan

- **Probe.** Every #520 row in `known-failures.yaml` (load-step,
  decode-step, restart-before-step and restored-snapshot, at 15, 90 and 463 s)
  must clear the peak ceiling, the final bound and the under-need bound. That
  holds on main's even split and on #529's shortest-queue router, and the
  build removes each row that passes. The under-need margin is reported per
  case, not just pass or fail. The rig change is that rows carry
  `RunningRequests` and `AvgServiceTime`, with a first window that mixes
  service times from before the saturation.
- **`pd-restart` golden.** Decode demand down on 9 cycles, by at most 67 %,
  none up, never under resident or need (the table above). Every other golden
  is replayed. The expected direction is down only, and only on cycles with a
  backlog in a drain region. A move up, or on any other cycle, is a finding.
- **Property cases.**
  - Never under the resident KV, and never under (true arrivals) / mu.
  - In the growing region, never under (true arrivals + B / T) / mu while lambda
    lags. That is the rewritten step test above.
  - A standing queue with AR(1) noise (phi 0.9, as in 3.5) at 10 % and 15 % on
    the queue, `RunningRequests` and `AvgServiceTime`. Count region flips and
    the change in demand per cycle. At 15 % the noise crosses the 10 %
    `backlogGrew` band, so the case measures flip amplitude against today's,
    and does not assume no flips.
  - A first read of `mu0` at +15 % on the 463 s cases stays inside the
    under-need bound.
  - A decode replica whose waiting requests are all deferred (KV from prefill)
    and whose KV is low is not read as saturated.
- **Negative controls.**
  - Switch off: the goldens equal the parent's, and the ledgered rows fail as
    they do today. `pd-restart` reads 9.87 at 12:28:02.
  - Mutants, each of which must be caught:
    - `dB/dt` dropped (the step test).
    - The haircut dropped (the 463 s under-need margin. The build reports
      whether the bound catches it, and if it does not, the property case on
      the first read does).
    - Starting replicas credited (`TestQueueDrain_StartingReplicasEarnNoCredit`:
      the same cycle with five starting must price as with none).
    - T fixed at 60 s (`TestQueueDrain_TheHorizonIsTheStart`: 7.53 replicas
      over a 300 s start, 9.41 over 60 s).
    - The per-replica cap on `B` dropped (`TestUnevenRoutingAPinnedQueueIsPricedAtWhatItsReplicaDrains`:
      21.96 against 3.53).
    - The run/svc sum replaced by the one-minute completion rate (12:27:02's
      arrival estimate 5.98 against 14.40).
    - `mu0` taken from rows with a queue alone (12:28:32: 3.0 against 6.2).

##### What it does not fix

- **The resident term when replicas land.** New replicas take queued work at
  once, so the whole fleet reads full for a cycle or more: 4.32 at 12:28:17
  on `pd-restart`, 1.47x on `e1` for one cycle, and up to 1.40 x the fleet on
  the shortest-queue probe.
- **Cause C**, the queue left on the original replicas under an even split.
  That is the router, handled test-side in #529.
- **The release path.** `pd-restart`'s fleet held 4 until 12:33:02, about
  3.5 min after demand settled. That is scale-down stabilization, not demand.
- **A role with no saturated row and no mu.** Without `AvgServiceTime`
  (`AvgServiceTimeKnown` false) or a known batch cap and k1, the role keeps
  today's residency charge.
- **Prefill** (above), and k2 relearned during a step (23.9k to 55k on
  `pd-restart`). The drain term divides P out; the resident term does not.

##### Decisions

1. **The haircut holds until every replica ordered under it is Ready**, not
   until the second mu reading. While one is starting, demand is at least the
   haircut price at the cycle's arrivals and queue (`noteDrainHold`).
2. **The price may raise demand** above occupancy's.
3. **T stays `DrainHorizon`**: max(start, 60 s).
4. **The growing-queue test's second assertion is relaxed** to "not under
   the drain price at the true arrival rate". The price stays above the
   arrivals.
5. **Decode `B` is the capacity queue.** Main already reads it that way
   (`decodeCapacityView`).
6. **The resident spike on landing is the next step**, out of scope here.

#### Queued work priced by drain time, as built (#520)

Built on main 98ac6fc22 with #529 (the queue-aware probe router) merged in,
behind `queue-priced-as-drain` (`D/queue_drain.go`, wired in
`applyThroughputFloor`). The formula is the design's, with five changes
the build measured:

- **`dB/dt` is signed, and the smaller of the two slopes.** The design took
  the larger, floored at zero. The larger read growth for a minute after it
  stopped, because `backlogGrew` and the slope over its lookback both linger.
  On the probe's 90 s load step the first replicas landed at 3m45s with the
  queue down from 420 to 410, yet the arrivals read 9.38 for 6. The haircut
  price then ordered 16 for a need of 9. Floored at zero, a draining queue
  read its drain as arrivals: the replicas complete what arrives plus the
  backlog they drain. On `pd-restart` at 12:47:36 they completed 25.4 req/s
  against 20.7 arriving, and the same queue was priced twice, as arrivals and
  as B / T. Signed, completions plus `dB/dt` is the arrivals whichever way the
  queue moves. The max with lambda keeps a noisy negative slope from pricing
  under the router's rate.
- **The first read needs a majority.** `mu0` is read only when at least half
  the role's replicas are saturated. Read from a minority, the probe's slow
  pod (one replica at 0.3 of fifteen siblings) was the fleet's first read and
  ordered 58 replicas for a need of 18.
- **A held haircut keeps its first read.** While the hold lasts, a cycle with
  no majority read prices at the read the hold was taken at. Replicas that
  land under a hold start unsaturated, and without it the 463 s load step fell
  back to the residency charge the cycle its first order landed, peaking at 15
  against a ceiling of 14.5.
- **The probe rig reports `RunningRequests` and `AvgServiceTime`.** The
  service time is one minute's mean, and runs shorter on a smaller batch (0.6
  of the limit's at no load). The first saturated row therefore reads mu
  13.6 % high, inside the 5 % to 15 % the recordings showed.
- **The haircut is a field (`drainHaircut`)**, so a test can take it out.

**Probe, every start case, both routers, on against off** (peak / final /
time under need; bounds: ceiling 14.5 for need 9 and 26.5 for need 17,
under-need 3m45s, 5m0s and 11m13s at 15, 90 and 463 s;
`TestQueueDrain_StartProbesOnAndOff`):

| case | on | off |
| --- | --- | --- |
| load-step 15 s | 10 / 9 / 0s | 11 / 9 / 1m15s |
| load-step 15 s jsq | 12 / 9 / 0s | 17 / 9 / 1m15s, over |
| load-step 90 s | 11 / 9 / 0s | 12 / 9 / 3m30s |
| load-step 90 s jsq | 12 / 9 / 0s | 19 / 9 / 3m30s, over |
| load-step 463 s | 11 / 10 / 30s | 16 / 11 / 8m0s, over |
| load-step 463 s jsq | 14 / 9 / 30s | 23 / 9 / 8m0s, over |
| decode-step 15 s | 20 / 17 / 0s | 20 / 17 / 0s |
| decode-step 15 s jsq | 23 / 17 / 0s | 23 / 17 / 0s |
| decode-step 90 s | 24 / 17 / 0s | 24 / 17 / 0s |
| decode-step 90 s jsq | 24 / 17 / 0s | 25 / 17 / 0s |
| decode-step 463 s | 22 / 21 / 0s, over | 22 / 21 / 0s, over |
| decode-step 463 s jsq | 31 / 23 / 0s, over | 31 / 23 / 0s, over |
| restart-before-step 15 s | 21 / 17 / 0s | 22 / 17 / 1m15s |
| restart-before-step 15 s jsq | 23 / 17 / 0s | 34 / 17 / 1m15s, over |
| restart-before-step 90 s | 22 / 17 / 0s | 23 / 17 / 3m30s |
| restart-before-step 90 s jsq | 24 / 17 / 0s | 37 / 17 / 3m30s, over |
| restart-before-step 463 s | 23 / 21 / 15s, over | 29 / 23 / 8m0s, over |
| restart-before-step 463 s jsq | 28 / 23 / 15s, over | 44 / 23 / 8m0s, over |
| restored-snapshot 15 s | 20 / 17 / 0s | 20 / 17 / 0s |
| restored-snapshot 15 s jsq | 23 / 17 / 0s | 23 / 17 / 0s |
| restored-snapshot 90 s | 24 / 17 / 0s | 24 / 17 / 0s |
| restored-snapshot 90 s jsq | 24 / 17 / 0s | 25 / 17 / 0s |
| restored-snapshot 463 s | 22 / 21 / 0s, over | 22 / 21 / 0s, over |
| restored-snapshot 463 s jsq | 31 / 23 / 0s, over | 31 / 23 / 0s, over |

- **Six #520 ledger rows pass and are removed:** load-step 15 s jsq, 90 s
  jsq, 463 s and 463 s jsq; restart-before-step 15 s jsq and 90 s jsq.
- **No case breaks a kind it did not break off, and none is under need past
  its bound.** The longest time under need is 30 s, against 8m0s off.
- **The six rows left fail as they did, over-priced.** decode-step and
  restored-snapshot at 463 s are unchanged: a derived mu prices them, so the
  floor's swap runs and the queue charge was never theirs; the excess is the
  even split's queue (#520 cause C) and the resident term. restart-before-step
  at 463 s falls to 23 and 28 peaks but ends at 21 and 23, over the 20.5
  bound, for the same reason. Their `measured` fields say so.
- **What the haircut buys** (`TestQueueDrain_TheHaircutKeepsTheSlowStartsAtNeed`):
  at 463 s, 30 s and 15 s under need with it, 1m30s without it. With AR(1)
  noise on the service time (sigma 15 %, phi 0.9, eight seeds) the figures are
  2m30s and 45 s with it, 3m15s and 2m15s without. The design's analytic
  9m31s without the haircut did not show: the rig's first read is high, and
  the arrival term orders on its own. The bound does not catch taking the
  haircut out on these cases; what it buys is margin.
- **Property cases** `liveness/floor/first-read-noise-*` (12 cases: sigma
  10 % and 15 % at phi 0.3, and 15 % at phi 0.9; load-step and
  restart-before-step at 463 s, both routers; 100 seeds each) hold that the
  first read under noise never puts the fleet under need past its bound. All
  pass.

**`pd-restart` golden** (decode role demand, replicas at the target):

| cycle | today | as built |
| --- | --- | --- |
| 12:26:47 | 2.28 | 1.99 |
| 12:27:02 | 6.01 | 3.17 |
| 12:27:17 | 5.25 | 3.10 |
| 12:27:32 | 7.24 | 3.30 |
| 12:27:47 | 7.66 | 3.26 |
| 12:28:02 | 9.87 | 3.51 (haircut held) |
| 12:28:17 | 10.65 | 4.32 (resident) |
| 12:28:32 | 9.06 | 4.38 |

The second step (12:45:05 to 12:47:36, about 4 to 25 req/s on a fleet capped
at 4 with up to 1366 queued) moves too: 42.87 becomes 8.96 at 12:45:20, and
17.38 becomes 6.21 at 12:46:20. One cycle goes up, 12:47:36, from 6.55 to
7.10. The arrivals alone need at most 4.81 there, so no cycle prices under
them. The rows that moved: decode demand down on 17 cycles (to -79 %) and up
on 1 (+8.5 %).

**Other goldens** (role demand, cycles moved): e1 down 13 (to -30 %) and
up 22 (to +93.6 %); e1-tuned down 7 (to -26.5 %) and up 30 (to +20.1 %);
throughput-sweep-restart down 5 (to -25.8 %) and up 1 (+9.2 %);
ttft-staircase-b-off down 18 (to -70.2 %); ttft-staircase-c-on down 21 (to
-50.2 %) and up 1 (+22.2 %). acceptance, engine and mu-paths do not move: a
minority of their replicas queues. e1's +93.6 % is one cycle, 14:05:32,
1.48 to 2.86 replicas: a joiner landed and took 221 queued at once, which reads
as growth (the next cycle is 2.11). The four
start-seed and start-restored goldens fall about 99 % in tokens: their P is
6 tokens, so occupancy priced a queue of 50 at over a thousand replicas, and
the drain price reads about 18 at mu 0.3.

On the staircase the derived mu (26) reads over what the two saturated
replicas served (about 18 to 21 each, lambda less the queue's growth). In the
growing region the price takes the lower of the floor's mu and the first
read, and never goes under the floor's own figure. Its lowest moved cycle, 1.78
at 02:11:39, has lambda 14.3, which is 0.89 replicas at the served rate.

Each new test runs the switch on and off: `TestQueueDrain_*`, and
`TestThroughputFloor_AGrowingQueueIsPricedAtTheTrueArrivals` (on: not under
the true arrivals plus the queue over T; off: not under occupancy, which at
15 s is 387 263, under the arrivals alone at 390 933).

##### Review round (PR #533)

Each change runs the switch on and off in its test.

- **A lambda that is not finite prices nothing.** An +Inf `ArrivalRate`, or a
  NaN completion rate the arrival rate falls back on, passed the builtin max
  and published NaN or +Inf demand. The drain price now reads such a lambda
  as none, as `floor.Estimate` refuses it, and a non-finite price falls back
  to the figure without it (`TestQueueDrain_ANonFiniteLambdaPricesNothing`).
- **The first read comes only from rows at their stated batch cap.** A row
  saturated by its KV under the cap is still growing its batch. On
  `ttft-staircase-b-off` at 02:06:23, batches of 126 and 128 under a cap of
  256 read 31.3 req/s against the 16.7 to 19.3 the replicas served. A 1.45x
  first read priced 7.57 replicas against a need of 9.33, and the haircut
  does not bound that. Such rows give no read and keep occupancy's pricing:
  property case `edges/floor/drain-first-read-kv-saturated-1.45x` prices 11.80
  against 9.33, and its control, the same read at the cap, prices 7.57.
- **While the backlog grows, mu is also capped at what the queued replicas
  complete.** That is their median `RequestRate`, read only from a majority
  of the role's replicas and only at or over `MinMeasuredOfDerived` (0.5) of
  the floor's mu. A queue that grows shows a replica serves no more than
  that. The floor's derived mu on the staircase (26) is over what its
  replicas served (17 to 19). Without the 0.5 bar, a rate that had just begun
  to queue read a step late and priced `pd-restart` at 12:45:05 at 18.9
  replicas and `e1-tuned` at 16:24:13 at 35.
- **A haircut hold ends.**
  - It prices only while a backlog stands.
  - It counts starting replicas through one floor helper
    (`floor.FreshStarting`): pending less stuck, and ages within two starts.
    The floor's starting credit uses the same rule (`startingFresh`).
  - It never lasts past two drain horizons after it was set
    (`TestQueueDrain_AHoldEndsWithAReplicaThatNeverLands`: one replica that
    never lands, held for 9 cycles at a 60 s horizon).
  - A shape change drops every hold of the model.
  - The read, the decision and the update are one locked step
    (`drainHoldStep`).
- **Two mutants the review found surviving now die.**
  - T fixed at 60 s: `TestQueueDrain_TheHorizonIsTheStart`.
  - A starting-replica credit: `TestQueueDrain_StartingReplicasEarnNoCredit`.
- **The probe rig states a batch cap** of 1024 on every variant and runs at
  it at its limit. With the switch off every start case is unchanged.

The growing-queue test is renamed
`TestThroughputFloor_AGrowingQueueIsPricedAtTheTrueArrivals`.
`TestQueueDrain_AGrowingQueueIsPricedAtWhatTheReplicasServe`, adopted from
the review, steps four replicas of true mu 1 from 4 to 8 req/s with lambda
lagging and a one-cycle dip of the queue. With the floor's mu read 1.4, it
prices 11.27, 12.27, 9.33, 11.60 and then 12.27 to 13.60 replicas, against a
need of 9.67, 10.67, 10.33, 11.33 and then 12.00. The dip cycle is short,
at 0.90: lambda still reads 7 of 8, and a shrinking queue shows no arrivals
above it. The test holds the need from the cycle lambda reads the true rate,
and 0.85 of it before that.

**Probe after the review, both routers.** Every case is as in the table
above except load-step 15 s, 11 / 9 / 0s, and restart-before-step 463 s,
22 / 21 / 15s. No case fails a new kind, and none is under need past its
bound. The ledger is unchanged; only the restart row's `measured` moves.

**Goldens after the review, against main.** Role demand cycles moved:

| fixture | down | up |
| --- | --- | --- |
| e1 | 7, to -19.6 % | 16, to +21.1 % |
| e1-tuned | 5, to -25.3 % | 25, to +24.9 % |
| pd-restart | 17, to -79.1 % | 1, +8.5 % |
| start-restored | 9, to -99.1 % | none |
| start-seed | 3, to -99.2 % | none |
| start-seed-restart | 10, to -99.0 % | 1, +0.9 % |
| start-seed-restart-2 | 4, to -99.5 % | none |
| throughput-sweep-restart | 5, to -25.8 % | 1, +31.1 % |
| ttft-staircase-b-off | 18, to -66.6 % | none |
| ttft-staircase-c-on | 22, to -50.8 % | none |

`pd-restart`'s first step is as in its table above.

Against the need at each fixture's served mu, from the recorded inputs,
three sets of cycles price under it:

- **ttft-staircase-b-off** 02:06:38 to 02:07:08: 0.94 to 0.97 of the need
  at 18 req/s a replica.
- **ttft-staircase-c-on** 05:49:35 and 05:49:50: 0.91 and 0.92 at 18.
- **pd-restart** 12:47:21 and 12:47:36: 0.93 and 0.97 at 6.22.

On the staircase the price uses what the queued replicas served on those
cycles, 16.8 to 19.4 req/s, so it is at need at that rate; the 18 used for
the need is the reviewer's settled figure. On `pd-restart` the need charges
the whole queue of 1366, while the drain price caps each replica's queue at
what it drains in T (937 of it). A request waiting in one engine's queue is
served by that engine only (`localQueueBacklog`). `e1`, `e1-tuned` and
`throughput-sweep-restart` price no cycle under need.

Not run on a cluster.

## 8. Risks and limits

- **One shared bug.** A defect in the settle tracker reaches every learner at
  once. Its tests cover every case the four versions were fixed for: k == 0, KV
  zero with an ITL present, k > 0 with no ITL, one missing row on a busy
  replica, the KV and ITL queries both failing for two cycles on a busy fleet,
  an idle replica's empty ITL window, a P/D prefill pod under light load
  (instant KV 0 between requests, ITL always empty: it must not go idle while
  its request rate is above 0), a Prometheus brownout longer than 1 min with
  every pod still listed (including an idle-and-return inside it, with the
  expected behaviour being the trade PR 1 picks in
  [4.3](#43-what-the-tracker-owns)), the same brownout with no rows at all (a
  failed collection: held), an optimize interval over a minute, a restart under the same name, an LWS group with mixed Ready times,
  a missed cycle, a slow fill, a just-Ready pod, a bridge lend, clock skew.
- **A shared write path already changed a learner silently.** #282 turned #5's
  in-spacing fold from a max into a lower median, and then into the mean. #7 kept its max only through
  an empty pod name.
- **Self-sealing errors.** Each capacity learner is witnessed only when
  saturated, and the error that over-orders stops saturation. A too-low k2 or
  mu removes its own witness; for k2 it is permanent until forgotten
  ([3.4](#34-which-side-breaks-the-slo)). Validation residuals are one-sided by
  construction, and must not be read as two-sided.
- **Start time blends before it trusts (PR 5c).** For a variant's first N
  starts the published estimate leans toward the prior, but sizing reads
  max(blend, measured mean), so 5c can only over-order, and only then: a
  fast variant is projected over a longer start than it showed. It cannot
  size a slow variant shorter than measured
  ([PR 5 design](#pr-5-design-start-time-and-the-snapshot)).
- **The prefill bound** is reviewed before it moves: a double scrape once
  doubled its input, its max kept it, and `RaiseLast` keeps it past a reset.
- **The recorder can evict the controller** if pointed at `/tmp`. It is off by
  default and has its own volume and cap.
- **Open defects this does not fix:** shape dependence of the ITL line (#275).
  An instant k against a rate[1m] ITL is fixed since #393, and 2.7 holds it to
  a mutant. Speculative decoding is read
  per accepted token in the saturation analyzer since #274; the throughput
  and slo analyzers still read the step time.

## Deferred to the implementing PRs

Round-3 lows, each settled in the PR named.

| item | PR |
| --- | --- |
| Determinism beyond the clock: sorted output, a fixed float format, no state write that depends on map order (`roleDemand`, `pendingRestore`, `eppByRole`); the acceptance replay runs three times in one process as well as 30 days apart | 0a, 0b |
| The recorder volume's `sizeLimit` is the per-volume ceiling (two recordings at the cap) plus headroom for the export. The writer stops each recording at the cap and deletes the oldest closed recording before the ceiling | 0b |
| The export also pseudonymises args and config strings and borrowed-line donors, with a per-export key that is not committed | 0b |
| `learner-write`'s `pods` lists at most 16, lowest reading first, with `podsOmitted` | 2 |
| Forget reaches every learner through a plain list of learner names in `learn`, not a registry; it is a named change of its own; when no key remains, the variant's learner series are deleted. Built for the saturation analyzer (2), the slo line (#431) and the throughput line ([as built](#the-throughput-lines-forget-as-built)). The deletion is built with the first series ([as built](#reports-restored-and-mus-forget-as-built-495)) | 2 |
| Each operator-column cell names its metric series, or "metrics: none" | each PR |
| A witness column per learner in 3.4 ("witnessed when", "non-saturating witness"), which also tells the probe which side to drive | 3 |
| 3.6's cycle-count clocks become durations, or record the cycle interval in the threshold | each learner's PR |
| The snapshot gains a last-admitted stamp for start time and B, as an additive `omitempty` field of v1; a document without it publishes 0 (as old as can be) | Start time: 5c. B: #495, `itlBaselineAdmittedAt`; a line with an empty window publishes its held slope's fit, and an intercept with no slope its own stamp, 0 only when it has none ([as built](#bs-last-admitted-stamp-as-built-495)) |

## Review notes (round 1)

Four reviews of the first draft (architect, statistician, SRE, skeptic):

- **Direction enum dropped.** A sketch fed k2 readings 100k, 100k, 20k, then
  eleven at 100k. The window mean returns to 100 000. A clamped prediction
  stays at 73 333 for good.
- **No driver loop.** The k2 window changes between replicas of one cycle, and
  mu reads k2's write in the same cycle. A batch loop changes the median across
  replicas, and so the replica count.
- **Harness first.** The recorded runs lack instant KV, ReadySince and
  per-replica rates, and never reach OLS. The first draft's replay rule passed
  trivially on k2, mu and B.
- **Observability.** The first draft's per-key count line had no value, pod or
  previous value, and merged k2 sources that the runbook treats differently.
- **Corrections.** The decode-gate fix was reverted, the idle-room cut removed,
  and the pinned fit is a mean. The first draft missed hard edges, #15 and #16.
