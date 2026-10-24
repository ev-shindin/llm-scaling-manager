# Shared rules for the analyzers' learners

- **Review:** Reviewed in five rounds by four reviewers; all four: good enough
  (round 5).
- **Status:** proposed, revision 6. No code changed. It describes main
  73a464475 (with #272 and #281) plus the heads of #273 (3e611e3fb), #282
  (c130c24ca, round 4) and #283 (40762ccd1). Line numbers are #283's for code
  on main or #283. Code that exists only on #273 or #282 (#273's rows, rows 5
  and 11, [the mu choice](#the-mu-choice): `siblingYield`,
  `MaxTokensOverCompletions`) is cited at that PR's head. #282's
  round-5 fix (the sibling yield reads every Ready full replica, queued ones
  included, and only when at least one is unqueued) was not pushed when this
  was written. Rows 5 and 11 and [the mu choice](#the-mu-choice) are re-checked
  against #282's merge commit before this proposal merges.
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
| Presence and idle | A known bit per gauge; idle is a two-gauge rule | ITL presence is three-state. Idle needs **positive** evidence, so two failed queries do not restart the settle fleet-wide | Idle evidence is role-aware: a prefill pod never has an ITL, so its idle reads the request or prompt-token rate. KV means peak KV. A listed row with unknown gauges is a sighting, and an idle run spans at most 1 min | Prefill idle reads the request rate for both entering and leaving idle (the prompt-token rate is absent on SGLang). The brownout trade is stated: an idle-and-return inside a long brownout keeps its settle (cost side); PR 1 may end the settle after more than 2 min of unknown rows, and the section 8 brownout case pins the choice | [4.3](#43-what-the-tracker-owns) |
| Property tests | Twelve classes, thresholds enumerated, findings mapped | A known-failures ledger; a mutant per mapping row; estimands declared, so section 5's estimators can pass; time base is an input family | A section-5 estimator that over-prices when one pod is genuinely slow declares where it starts to, and the case is in the ledger, owned by PR 3 | - | [2.6](#26-property-tests) |
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
| **One reading dominates.** | Least squares pulled by three joined-pod readings out of sixteen (#273: now dropped by state, then a k-weighted mean). The saturated throughput sample was the hottest replica (#282: now a lower median, bounded by the served rate). One slow replica set the fleet's mu (#282 round 4: now yields to its full siblings). |
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

| # | quantity | where | admission | fit | memory and forgetting | persisted |
| - | --- | --- | --- | --- | --- | --- |
| 1 | ITL line `ITL(k) = A·k + B`, saturation analyzer | `internal/signals/itl/`, wired in `D/analyze.go` | Ready, not a bridge, finite ITL > 0, k > 0. Each reading goes to the main window, the main and steady windows, or nowhere (see [Admission shapes](#33-admission-shapes)). A reading taken while its replica settles (joined, or load returned after idle, under 2 min) is stamped and kept out of the pinned fit | OLS on the steady window at ≥ 10 rows spread ≥ 0.30. Otherwise A = k-weighted mean of `ITL − B` over unstamped readings, B pinned. Fewer than 3 such readings, or fewer than 10 in the window at all (light or overloaded load ages them out): the last A is held (`SlopeHeld`, not persisted), up to `BucketRetention` after its fit and under the same args; a held line is not republished to row 2. A < 0 refused | Main 20 rows / 30 min. Steady 40 rows, carried 5 min past the main window. Swept 24 h unseen. Args change stashes B | B and its stash |
| 2 | Shared and borrowed ITL line | `D/analyzer.go` `publishLine`/`borrowLine` | `EnableLearnedStateReuse` (off), same fingerprint | none | Ages on last write. May hold a fleet, never order one | yes |
| 3 | k2, KV at saturation | `D/replica_capacity.go` `k2Chain`, `D/k2_settle.go`, `D/keys.go` | Queue ≥ threshold; downstream not saturated; not a bridge; not mid-rollout; under the physical ceiling. Joined under 1 min: refused. When queue × footprint exceeds what the busiest rank holds, also: rate ≥ 0.7 of mu on record **and** residency ≥ 0.7 of k2 on record, **or** residency within ±25 % of the reading 1 min back with the queue standing on every cycle | Mean of a 10-sample window. Admitted readings raise it as well as lower it. The first admitted reading prices at once | Key: the fleet's tracked output length for decode and aggregated, prefill's own average. 24 h write gap starts a new episode. Evicted 7 d after last read | yes, with args tags and stash |
| 4 | Borrowed k2 (lower bound) | `D/k2_borrow.go` | Only when lower than the variant's own k2. KV total within 3 % | none | Held 30 min, then relaxed **upward** over 6 h of serving time. Re-arms on an args change or a figure ≥ 20 % lower | yes |
| 5 | Measured mu | `D/throughput_floor.go`, `D/throughput_admission.go` | Only on a P1-obs k2 write, so it inherits k2's joined test. RequestRate > 0. Not filling: generated tokens at most 1.15x completions × output length (`MaxTokensOverCompletions`, round 4). Not under half the highest rate a Ready replica of the variant served, its own included (`servedBounds`). Not under 0.5 of derived, unless the same pod read that low on every saturated cycle for 2 min | One sample a minute. Within the minute a reading rewrites the last sample: max per pod, lower median across pods | See [the mu choice](#the-mu-choice) below. Evicted 7 d after last read | window yes, choice no |
| 6 | Derived mu | `D/mu_from_itl.go` `deriveMu` | none: a formula on #1 per replica | none | As fresh as the line | no |
| 7 | Prefill throughput bound | `D/prefill_bound.go` | `EnablePrefillThroughputBound`. Saturated, serving rows. No joined test, no rate check, no band | Window max. It writes through #5's fold with pod `""`, so it kept its max only because every prefill pod folds under one empty key | **Only rises.** The prior max is read before the write and put back with `RaiseLast`, so it survives the 24 h episode reset. Touched on every read, so never ages while pricing. Cleared only by an args change or a forget | with #5 |
| 8 | Fleet shape and shape-change hold | `D/shape_change.go`, `D/fleet_shape.go` | Tolerance band on the router's prompt length | Rate-weighted mean | Hold up to 5 min | no |
| 9 | Decode-saturation memory | `D/roles.go` `roleSaturated` | One full decode pod gates prefill learning (the #282 fix that changed this was reverted, 7ab00fa8c). Bridges count | none | 1 min | no |
| 10 | Idle room | `D/idle_room.go` | Decode only. Idle on every reading for 2 min, no gap > 1 min, not a bridge | none | Run resets on a non-idle reading | no |
| 11 | Floor arrival rate, trust and backlog memory | `D/throughput_floor.go`, `internal/signals/floor/` | none | Blend from 0.5 to 0.8 of resident demand. A queue counts as grown past a noise band over a minute. An engine's own queue is capped at what it drains (mu × T) **only while it stands**; while the role's backlog grows it is charged in full (round 4) | Last cycle's backlog per role; `backlogGrew` remembers 1 min, so the full charge outlives growth by up to a minute (an over-order) | no |
| 12 | Replica start time | `D/start_est.go` | Each pod once, not a bridge, 0–3600 s. A sample over 3x the estimate is refused until 3 such, counted per variant **per rejected sample**: three outlier pods in one cycle exhaust the guard, and one pod repeating passes on its fourth cycle | EWMA α 0.3. The first sample replaces the default | Seen pods not persisted: after a restart every running pod is folded again. The floor reads it as a min per role, share pacing as a max | yes |
| 13 | SLO analyzer line | `internal/engines/analyzers/slo/` | Its own ITL tracker, same rules as #1 | Same as #1 | 24 h unseen | no |
| 14 | Share need history | `internal/engines/allocation/utilization_share_ledger.go` `RecordNeeds`, `PeakNeed`, `PeakKnown` | A peak needs two readings in a row. After a restart a full swing window before a peak is known | Mean over the swing window. Step detector `ShareStepSpread` 0.25 | Trimmed to the window | yes, `wva-utilization-share-history` |
| 15 | Share release time | same file, `MeasuredRelease` | Completed releases | 90th percentile of recent releases, needs ≥ 3 | Recent releases only | no |
| 16 | ITL line, throughput analyzer | `internal/engines/analyzers/throughput/analyzer.go` `resolveITLModel` | The window's k band only | OLS, else B pinned to `lastFittedB` | Per variant | no |

#### The mu choice

A measured mu and a derived mu are both offered every cycle, and
`measuredYieldsToDerived` picks one. It keeps the measured window either way.

| rule | when |
| --- | --- |
| Yields to derived **at once** | the measurement is under derived and a Ready replica (its own included) serves more than 2x it |
| Yields to the full siblings (round 4, `siblingYield`) | the measurement is under 0.8 of the lower median served by Ready replicas with a full cache and no queue. Logged as `saturated-throughput-yields-to-siblings`, bucket `served-siblings`. Remembered in `muChoice` until the measurement comes within 0.8 or the window starts over. Round 5 (landing): the median is over every Ready full replica, queued ones included, and the yield applies only when at least one of them is unqueued |
| Yields when stale | freshness is the last saturated reading that **agreed** with it within 2x. Above derived: after 5 min. Below derived: after 30 min |
| Latch | out at 2.0, back at 1.6 |

**Named, not fixed here:** the latch is not persisted. After a restart a
measured mu between 1.6x and 2x **above** derived comes back unlatched and
prices from the **higher** measured figure, the SLO side. PR 3 either persists
the choice or restores it to the safe side ([3.5](#35-hysteresis)). A
one-replica variant with no line has nothing to yield to.

Also learned and not tabled: the hot-rank hysteresis memo and the share swing,
which stabilize the controller's own decisions rather than learn from replicas.

The snapshot carries #1–#5 and #12, plus the accelerators, the forgotten set,
`K2BorrowedSince`, `PriorK2` (read by rollback builds) and both stashes. A
restore never makes a figure younger, and an unknown age resolves to the safe
side ([3.4](#34-which-side-breaks-the-slo)).

Row 16 has none of #273's fixes (steady gate, settling stamp, k > 0 guard). It
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

Only `KvUsageInstantKnown` exists (`internal/domain/replica_metrics.go`). The
collector turns NaN, Inf and absent series into 0 (`collector/extract.go`). PR
0 adds a `...Known` bit beside each gauge a learner reads: peak KV, ITL
(`AvgITL`), the engine queue, request rate, generation-token rate and
prompt-token rate. A bit is true when the series was present and finite.

**ITL is three-state:** finite, empty window, or absent. ITL is
`rate(sum[1m]) / rate(count[1m])`
(`internal/collector/registration/queueing_model.go`), so an idle replica
reads 0/0 = NaN with the series present ("NaN is the ordinary case here",
`recent_window_extract_test.go`). A failed or timed-out query reads absent. A
single bool would merge the idle signal with a failed query, so ITL carries
`AvgITLKnown` and `AvgITLEmpty`.

- The recorder writes the bits. It never encodes "missing" as NaN:
  `encoding/json` refuses NaN and would drop the whole line.
- 0a's test: the collector sets each bit on a present, NaN, Inf and absent
  series, and ITL's three states on a live, an empty and a failed query.
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
| `Analyze` | the full `AnalyzerInput`: every `ReplicaMetrics` field with its presence bits (instant and peak KV, ReadySince, `Revision`, the warm-pool flag, per-replica generation, completion and prompt rates, queue, ITL, KV capacity); per-variant GPU count, role, accelerator (`VariantStates`); each variant's `capacity.Record`; the scheduler queue; the arrival rate; the policy; for the SLO analyzer, the router latency; and the `AnalyzerResult` it returned, minus `AnalyzedAt`, or its error. Written whether or not it errs |
| store mutations, engine-side | the exported values method behind `LoadFromScaleTarget` (the parsed `EngineParams`, accelerator, GPU count, `RollingOut`, `now`), `EvictStale` (the timeout and `now`; what it removed, as a check that fails only the fidelity run) |
| settle observation, engine-side (from PR 1) | the model and the engine's own `now`, read where the engine observes the tracker; the rows are the next `Analyze` event's ([4.2](#42-one-owner)) |
| `RestoreLearnedState`, `ExportLearnedState` | the document in or out |
| `ForgetVariant`, `ForgetLearnedState` | the variant, key and cause (annotation, model gone) |
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
| collection | `testing.md` says how to copy the files out (`kubectl cp`) before teardown; the benchmark harness does it on every exit path |
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
`AnalyzedAt` is excluded.

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

**Per-path counters must be above zero.** P1-obs admitted, mu samples written,
mu yields (served, sibling, stale), OLS cycles, pinned cycles, instant-KV
rows, input rows with a known ReadySince, and from PR 1 on the settle
tracker's verdicts per reason. A replay that touched none of a PR's paths
fails rather than passes. A counter counts only where its path can run on the
PR's base: a counter that is zero by construction there (joined refusals with
a known ReadySince, while `replicaReadySince` returns zero until PR 2) is not
coverage, and is required from the PR that makes it non-zero.

**The closed-loop probe** is new work, not a commit of an existing test. The
probes #282's model reviews wrote hold the fleet fixed: they compute the
implied replicas and never add one. A closed-loop probe needs a fleet
simulator: the order becomes replicas after the start time S, a new replica
is Ready after the settle, load splits equally across Ready replicas or by a
recorded imbalance, and the queue grows by λ minus what was served. It lands
in **PR 3**, the first PR that needs it, as `D/closed_loop_probe_test.go`,
with one known-failing case as its negative control (one slow pod's confirmed
low rate, #282 round 3 H1). Holds, latches, the sibling yield and the
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
| Contamination **paired with** heterogeneity | a **state-flagged** contaminant is dropped, and a genuinely slower replica with no flag moves the estimate by its weight under the declared estimand. A learner using a value estimator allowed by [section 5](#5-fits-exclude-by-state-then-fit-untrimmed) (mu's lower median across pods, half the served max, 0.8 of the siblings' median) runs the heterogeneity case with the deviation it declares, on the side it declares. Where that side is the risky one (mu's lower median against one slow pod at N ≥ 3, for example {1, 1, 1, 0.5} prices 14 % above the mean; half the served max; the sibling yield), the learner declares the deviation at which it starts to over-price, and the case goes into the known-failures ledger with PR 3 as its owner |
| Redistribution invariance | the same fleet load moved between replicas leaves fleet demand and learned figures within a declared tolerance |
| N-dependence | under iid noise, an estimate's expectation does not depend on N in {1, 2, 4, 16} beyond a declared bias (catches the max across replicas, +13.6 % at 16) |
| Known truth | with a true mu and k2, the priced capacity is at most truth × (1 + ε) under every family, idle replicas included. At the documented target the steady order equals λ / (mu · target) |
| Scale | block size, GPUs per replica, output length and cycle interval varied one at a time: priced replicas are invariant, and each figure scales by its unit |
| Liveness | from clean identifying data each learner reaches truth within a stated time, **per topology**: N ≥ 2, P/D, flow control on and off, 8-GPU DP ranks |
| Monotonicity | along a trajectory, not only between two static inputs: demand does not fall as lambda or the queue rises, and stays bounded by what the fleet can drain |
| Edges by enumeration | the operating point is put on **each** threshold below, with noise of the gauges' real autocorrelation. Between declared latched switches the output is Lipschitz in the input, and it reverses at most once per excursion, an excursion being one crossing of the threshold by the **noise-free** generator path |

**Known-failures ledger.** Several classes fail on today's code by design: known
truth on the time-base family (A5, an open defect), first sample on k2 and
start time, N-dependence on the prefill bound's max and on mu's lower median,
edges on the k2 key boundary, and heterogeneity and known truth on mu with one
genuinely slow pod at N in {4, 16} (owner PR 3; #282 is evaluating a fold to
the mean of admitted replicas, the likely resolution). 0b commits a ledger of (class, learner, finding,
measured result, the PR that clears it or "open"). A listed case runs and must
still fail; a listed case that starts passing fails the build until its row is
removed. A PR that fixes one removes its row. Nothing is skipped and no
tolerance is widened to pass a defect.

**Each mapping row is demonstrated.** For each row of
[2.7](#27-past-findings-against-the-test-classes), 0b includes a mutant that
reintroduces the finding: the pre-fix commit's function, or a toy learner with
the defect. The class must fail on the mutant, and the PR pastes the failure.
A class with no failing mutant does not count as catching anything.

Thresholds the enumeration covers (each PR adds any it introduces):

| learner | thresholds |
| --- | --- |
| ITL | OLS at 10 rows and spread 0.30; slope held under 3 readings; settle 2 min; 20 / 40 rows; 30 min window, 5 min carry; the 0.30 idle line |
| k2 | queue threshold; joined 1 min; 0.7 of mu; 0.7 of k2; ±25 % at 1 min back; window 10; episode gap 24 h; eviction 7 d; the key boundaries |
| borrowed k2 | 3 % KV; 30 min hold; 6 h relaxation; 20 % re-arm |
| mu | half of served; 0.5 of derived; 2 min confirm; 1.15 tokens over completions; 0.8 of siblings; the sibling yield's "full cache" line; the median's 50 % mass point; 2x stale agreement; 5 / 30 min staleness; latch 2.0 / 1.6 |
| floor | blend 0.5–0.8; growth band max(2, 5 %) over 1 min; the standing-queue cap |
| shape, idle, decode memory | shape tolerance band and 5 min hold; idle 2 min with 1 min gap; decode memory 1 min |
| start time | 3x outlier, 3 confirmations, 0–3600 s, α 0.3 |
| share (#14, #15) | step spread 0.25; p90 at ≥ 3 releases (listed for later; see 2.7) |

### 2.7 Past findings against the test classes

| finding | caught by |
| --- | --- |
| GPS ratio first raw; pinned B from unsteady readings; k2 at 8 % behind a step | first sample, transient |
| Full fit never runs at N ≥ 2, B stays 6 ms (#273 A3) | stale value at N ≥ 2 |
| k == 0 unsettles; router queue without engine metrics | missing values |
| Instant k against a rate[1m] ITL (#273 A5, in the ledger: open) | known truth, on the time-base family |
| Sum of per-pod one-minute peaks | redistribution invariance, on the time-base family |
| Settle refuses one cycle of a fill (#282 F1) | transient, on the time-base family |
| k2 key flips on the output-length boundary; measured/derived flips near 2x; the 0.30 idle line; `backlogGrew` | edges by enumeration (the operating point on the boundary, or the ratio built at 2) |
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
| Restart fix stubbed | per-path counter "joined refusals with a known ReadySince", required from PR 2 |
| "For good" self-sealing low mu | closed-loop probe (PR 3) |

Every row above has a mutant ([2.6](#26-property-tests)); a row whose class
does not fail its mutant is moved to "still escaping".

**Still escaping:**

| finding | why |
| --- | --- |
| Share swing and cancel (#272 F1/F2 and the d3 reviews) | the share ledger (#14, #15) is not recorded or replayed. Out of scope for PR 0 |
| Alert debounce and sustain (#283 M2/M3) | not a learner |
| Speculative decoding; shape dependence of the ITL line (#274) | no generator models them |
| A self-sealing error in a PR that does not touch a hold | the probe runs only when a PR names a closed-loop change |

### 2.8 Recordings each PR needs

None exists yet: no recorder has run. No recorded run has ever reached OLS, and
none carries ReadySince until #283 merges.

| PR | recording needed | counters that must be above zero | synthetic stand-in |
| --- | --- | --- | --- |
| 0a | none: unit tests and negative controls | — | — |
| 0b | the E1 scenario re-run with the recorder on, from a build with #283 and 0a merged. 0b's goldens commit with it | instant-KV rows, input rows with a known ReadySince | — |
| 1 | the 0b recording | the settle tracker's verdicts per reason | the settle suite (section 8); the balance tracker's unit test |
| 2 | flow control off; a P/D run; a pod restarted under the same name | P1-obs admitted, settle refusals per reason, joined refusals with a known ReadySince | restart suite: same-name restart, LWS group with mixed Ready times, bridge lend, node clock 90 s behind, controller restart. 8-GPU DP: `D/dp_ranks_test.go` |
| 3 | flow control off (mu is written only beside P1-obs) | mu samples written, mu yields | closed-loop probe (lands here) |
| 4a | a run that reaches OLS: stepped concurrency giving ≥ 10 steady rows spread ≥ 0.30, with the SLO analyzer recorded | OLS cycles, pinned cycles | restart suite (PR 2) and the settle suite's one-missing-row case, both through ITL, for its two named changes; the closed-loop probe through a controller restart |
| 4b | a throughput-analyzer run with its recorder on | its OLS and pinned cycles | negative control on the parent |
| 5 | a controller restart with the snapshot on | restored keys, start samples | restart suite plus restore-then-run |

LWS and SGLang have no recording. Their synthetic suites stand in until one
exists, and the PR says so.

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

- An unknown age resolves to the safe side. (`measuredYieldsToDerived` treats
  a missing `SampledAt` as "as stale as can be", which skips the 30 min low
  hold. That is the risky move; PR 3 fixes it.)
- An exact repeat of a float (`rate == lastRead`) is evidence of use, not of
  saturation: it is the signature of a frozen series. Today mu counts it as a
  write and keeps its episode alive; PR 3 fixes it.

| learner | side that breaks the SLO | moves toward it without new evidence, today |
| --- | --- | --- |
| k2 | too high | none. The window mean rises on admitted readings only. A too-low k2 is **permanent** on a serving variant: touched on every read, never evicted, and its episode resets only on a write, which needs the saturation a low k2 prevents. The only exit is `llm-d.ai/forget-learned-state`; the runbook says so |
| borrowed k2 | too high | the upward relaxation over 6 h of serving time |
| measured mu | too high | the stale yield to a higher derived figure after 30 min (5 min toward a lower one). The restart that drops the latch (see [the mu choice](#the-mu-choice)). An unknown age (above). Backed by new evidence, so allowed: the **immediate** yield when a Ready replica serves more than 2x it, and the sibling yield |
| ITL line and derived mu | ITL(kPrice) too low, that is, derived mu too high (with B pinned low, A comes out too high and the prediction is still low) | the 6 ms default B or a stashed B. With B pinned below the true intercept, the pinned A absorbs the error and the prediction at kPrice is low by (B_true − B_pin)(1 − kPrice / k̄): **risky whenever the fleet runs above its pricing point**, the under-provisioned fleet that most needs a correct line |
| prefill bound | too high | its window max only rises, survives the episode reset through `RaiseLast`, and never ages while read. Because `RaiseLast` carries the max past every reset, it is a running max over the bound's life: about 1.5σ above the mean after 10 readings, about 3.2σ after a day at one a minute. Unreviewed |
| start time | **both** | too low under-orders a fleet that is behind (λ > mu · ready). Too high, in `internal/signals/floor/floor.go`: `DrainHorizon` under-prices the backlog whenever S exceeds 60 s, `startingCredit` credits a stuck pod for up to 2S, and a fleet with spare projects more drain and orders less. Share pacing errs the other way: too low moves GPUs for bursts that end first. PR 5 bounds both sides |
| share need history (#14) | too low for the role, too high for its neighbours | `PeakNeed` is a max of adjacent-pair minima, low by design; trimming to the swing window ages a peak out with no new evidence |
| share release time (#15) | too low | p90 over ≥ 3 releases is the max at n = 3 |

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
| restart | an unpersisted latch restores to the safe-side choice. The mu latch does not today ([the mu choice](#the-mu-choice)) |
| tuning | dwell is tuned on noise with the gauges' real autocorrelation, not iid noise, which over-counts flips |

### 3.6 Clocks

The clock fix itself is [2.2](#22-one-clock). The clocks in use:

| clock | used by |
| --- | --- |
| **write** | ITL line age, the shared line |
| **use** | k2 and mu eviction, 7 d after the last read; the prefill bound's `Touch` |
| **write gap** | k2's and mu's new episode after 24 h without a write, on the same window as the use clock. The prefill bound defeats it (`RaiseLast`) |
| **agreement** | mu freshness: the last saturated reading within 2x. Refused readings set it too |
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

| | ITL steady trace (#273) | k2 settle and first sight (#282 `D/k2_settle.go`) | `noteReady` (#282 `D/throughput_admission.go`) | balance tracker (#283 `internal/signals/balance/tracker.go`) |
| --- | --- | --- | --- | --- |
| joined wait | 2 min | 1 min lookback, queue standing | 1 min | 2 min |
| ready time | first sight | first sight | Pod Ready time (`replicaReadySince`, from `ReplicaMetrics.ReadySince`) | later of Pod Ready and first sight of that Ready time |
| restart events | joined, load back after idle | joined | joined | joined, warm-pool lend |
| key includes variant and bridge | no | no | no | yes: `(pod, variant, bridge)` |
| first cycle after a controller restart | taken as settled | taken as established | taken as established | waits again |

### 4.2 One owner

One tracker in `internal/signals/settle` replaces them.

| question | answer |
| --- | --- |
| owner | **the engine**, one tracker per model, swept with the model. It observes this cycle's rows into the tracker at the top of `runAnalyzers`, before `observeLoadBalance` (`engine_v2.go:156`) and before any analyzer. An analyzer-owned tracker would be written after the balance tracker reads it (`observeLoadBalance` runs before `runV2AnalysisOnly`), one cycle late on every join |
| readers | the balance tracker (#283), the saturation, SLO and throughput analyzers, each through a **read-only view**. None writes. The engine builds the saturation and SLO analyzers (`steadystate/engine.go:335, 404`) and passes the view at construction. The throughput analyzer is built in `cmd/main.go:916` and added by `RegisterAnalyzer` after the engine exists, so it takes the view at registration through an optional interface (`settleViewTaker`), as the engine's other capabilities are reached |
| replay | the tracker is a function of the rows and the engine's clock read only. The engine's observation is an engine-side event in the stream ([2.4](#24-the-recorder)): the model and the `now` the engine read, which is earlier than the `a.now()` each analyzer reads inside `Analyze`. Replay applies it as the engine did, at that instant, with the next `Analyze` event's rows. It reads nothing from the store, so a rollout is detected from the rows (4.3) |
| concurrency | the tracker holds its own lock; one writer per model per cycle, any number of readers |
| key | (model, pod, variant, bridge). A warm-pool lend keeps the pool's Ready time but changes variant and bridge, so it is a new key and a new settle. A lend is detected by the key change, not by absence |
| prerequisite | #283 merges before PR 1 |

**Difference to reconcile with #283.** #283 rebuilds its sightings from this
cycle's rows, so one missing row forgets a replica. The shared tracker keeps a
sighting across an absence up to one rule period (1 min): a single missing row
is a missed scrape, and restarting a busy replica's settle for k2, mu and ITL
would re-open #273's A4. When the balance tracker moves onto the view in PR 1,
its imbalance timers take this rule. That is a named change for #283's metrics,
and it is outside the analyzer replay (the engine owns the timers and
publishes them as metrics), so PR 1 proves it with a unit test on the balance
tracker: one missing row keeps the timer, two rows past the period restart
it. The test fails on the parent.

### 4.3 What the tracker owns

- **A stamp per reading, taken when observed.** The learner stores it with the
  reading, because a window outlives the settle.
- **The events that start a settle:** a replica joins, load returns after idle,
  the engine restarts (an engine or LWS group restart under the same name gives
  a new Ready time), a warm-pool lend, a rollout. A rollout is detected from
  the rows (a pod with a new `Revision`, or a new pod), not from
  `capacity.Record.RollingOut`: `LoadFromScaleTarget` files that bit later in
  the cycle than the engine observes the tracker. PR 2 names which pod's Ready
  time stands for an LWS group.
- **The ready time:** the later of the Pod's Ready time and the controller's
  first sight of it, as #283 does. A node clock behind the controller's, or a
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
  intervals and reads known-empty on every row. Two counting rows in a row make
  the replica idle (#273's `idleRows`), and "in a row" spans at most the 1 min
  gap: two idle rows further apart than that, with only unknown rows between,
  do not make the replica idle. A row where a gauge it needs is
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
  brownout longer than 1 min does not restart every replica's settle. A rule
  compares against the reading one period back, not one cycle back:
  cycle-to-cycle one-minute peaks share 45 s.
- **The brownout trade.** A run of unknown rows keeps the settle. So a replica
  that goes idle and takes load again inside a long brownout keeps its settled
  state, and its first known rows, a fill transient, are admitted. That errs
  on the cost side: a fill reading has a standing queue and low residency, so
  it lowers k2 and over-orders, and k2's own transient checks (0.7 of mu, 0.7
  of k2) catch most such readings. PR 1 may instead end the settled state
  after more than 2 min of unknown rows, at the price of 2 min of refused
  admissions per replica. The brownout case in [section 8](#8-risks-and-limits)
  pins whichever PR 1 picks.

Each learner keeps its own predicate and thresholds over the trace the tracker
returns. k2's rule needs a second gauge, a precondition and a cross-learner
read, and stays k2's. Before PR 1 merges, all four rules above are written
against the API (the review sketch already writes k2's against `Back` and
`Trace`).

### 4.4 The cost after a controller restart

Under the shared tracker every replica waits the window again after a
controller restart. This is **on the SLO side**: capacity is `min(k1, k2)`, and
with the snapshot off (the default) every k2 window is empty, so a saturated
fleet prices from k1, the high figure, for 1 min longer than today. ITL does
not refit for 2 min (a named change in PR 4a). #273 chose the current behaviour to avoid exactly this
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

So: with the snapshot on, first sight is persisted (PR 5) and the wait applies
only to replicas with no record. With it off, the wait is accepted, logged on
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

**`learner-write`**, at the shipped default verbosity:

| item | rule |
| --- | --- |
| when | at most **one line per key per cycle**, written after the last replica of the cycle is priced |
| trigger | the value differs from the **last logged** value (not last cycle's) by more than the learner's band, or is set for the first time. Against the last logged value a slow drift still logs, and noise under the band does not |
| band | per learner, stated in its PR, and above its measured cycle-to-cycle noise (mu's must exceed the 4 % that flipped it 11 times in 20 cycles) |
| heartbeat | a key that priced or was written in the last 10 min, and has not logged for 10 min, logs once |
| source change | a mu choice flip (measured, derived, siblings) is a `learner-write` with `source` changing, so latch flaps count from one line |
| fields | `learner`, `modelID`, `namespace`, `variant`, `role`, `key`, `value`, `previousValue`, `source`, `samples`, `pods` (each contributing pod with its reading and time base), and the admission tests passed with their margins |
| ceiling | keys × 1 per cycle. 100 variants × about 9 keys = 900 lines per cycle (about 450 KB), reached once after a snapshot-off restart when every key is set for the first time. Steady state, with the heartbeat: up to keys / 40 per cycle, about 90 lines a minute at 900 keys |

E1's 0.0995 req/s was **admitted**; this is the line that would have shown it.

**Decision lines name their learner key and source.**
`throughput-demand-floor` gains `muKey`, `muSource`, `startEstimateSeconds` and
`startSource` (measured or seed).

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
| k2 | yes | yes, as `P2-hist` (or `P2-prior`) |
| ITL | B and its stash | no: the line is not usable until refit |
| mu | window yes, choice no | the window yes; the choice starts unlatched |
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
shared-cache decision line. A PR that changes any line a tool in
`hack/benchmark/` parses either keeps its fields (adding is fine) or bumps a
`v` field and updates the tools in the same PR. The tools fail when a run has
zero `k2-decision` or zero `throughput-demand-floor` lines, or zero lines of a
message the scenario declares (as `post_run_analyze.sh` already does for
`k2-decision`); an aggregated run legitimately has no prefill lines. The P-code legend in `dump_k2_decisions.py` stays P-codes
only; `restored` is rendered as a separate column.

### 6.2 Metrics

- Labels: `namespace`, `model_name`, `variant_name`, `role`, and
  `controller_instance` when set. No pod, no bucket. The vocabulary is
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
  the new key; when no key remains (every key forgotten), the variant's
  learner series are deleted.
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
| **0a** | One clock (2.2), including the capacity store and the engine's calls into it; presence bits with ITL's three states (2.3). Unit tests, the `EpisodeGap` negative control, the one-cycle boundary test | none needed | Nothing, beyond 2.2's one-cycle boundary. Metrics: none |
| **0b** | The recorder at the input boundary in each analyzer and the store (2.4), with each `Analyze` result recorded and the test that a replay at the recording's commit reproduces it byte for byte; the store split into parsing and one shared method; one switch hook per package; replay with goldens and the golden rule (2.5), per-path counters, property tests with the known-failures ledger and a mutant per mapping row (2.6, 2.7) | E1 re-run from a build with #283 and 0a (no) | Nothing. A recorder flag and overlay in `testing.md`. Metrics: none |
| **1** | `internal/signals/settle`, written by the engine at the top of `runAnalyzers`, read-only views for the balance tracker and the analyzers. #283's one-missing-row forget becomes the gap rule, a named change proved by a unit test on the balance tracker (4.2). 2.3's A4 and 273-r3 #2 negative controls | 0b's (no) | The imbalance timers no longer restart on one missed scrape |
| **2** | k2 (#3, #4) onto the tracker. The Pod Ready time switched on. The LWS rule named. `learn` lands. `learner-write` for k2. Restart suite | flow control off, P/D, same-name restart (no) | A pod back from a restart under the same name waits again: more `P1-obs-unsettled`. After a controller restart every replica waits once: `P1-obs-unsettled` across the fleet for 1 min, and with the snapshot off k2-less fleets price at k1 for that minute (SLO side). `learner-start` and k2 writes are visible |
| **3** | Mu (#5, #6). Its joined test reads k2's stamp. Exact repeats count as use. Unknown age resolves safe. The latch persisted or restored safe-side. Two prefill pods in one spacing pin #7's max before the fold is touched. The closed-loop probe and its fleet simulator land here (2.5) | flow control off (no) | `learner-write` for mu, including choice flips. `throughput-demand-floor` names `muKey`, `muSource`, `startEstimateSeconds`, `startSource`. Measured mu waits out the post-restart settle |
| **4a** | ITL #1 and #13 onto the tracker. Neutral on the recording except two named changes, each behind its own switch: ITL waits the settle after a controller restart (today it takes the first cycle as settled), and one missing row no longer restarts a settle (the 1 min absence rule, 4.2). Neither occurs in the OLS recording, so the restart suite (PR 2) and the settle suite's one-missing-row case, both run through ITL, cover them, each with a negative control on the parent. The closed-loop probe runs through a controller restart (4.4) | a run that reaches OLS, SLO analyzer recorded with its router latency (no) | No ITL refit for 2 min after a controller restart; with the snapshot off the throughput floor is silent for about 2.75 min (SLO side, masked by the default 300 s scale-down stabilization as long as the controller is back within about 2 min). A missed scrape no longer restarts ITL's settle |
| **4b** | ITL #16 (throughput analyzer) gains #273's fixes: a named change behind a switch, with its own fixture, a negative control on the parent and the probe for the slope hold | a throughput-analyzer recording (no) | `learner-write` when A or B moves. Throughput-analyzer fleets may change, as named |
| **5** | Start time (#12) and snapshot. Confirmations counted by distinct pod. The first-sample rule, a named change that bounds **both** sides (for example: adopt a first sample only within [S0/3, 3·S0] of the default, or blend toward it until N distinct pods agree). First sight persisted. The writer stays central with per-learner adapters, and reads a v1 document from the previous release. Restart suite plus restore-then-run | controller restart with the snapshot on (no) | Start estimates no longer take one pod's first sample raw. `restored: true` on restored lines. With the snapshot on, only replicas with no record wait after a restart |

Later, each after a recorded run that covers it: idle room and the decode gate
(#9, #10) after a P/D run; the prefill bound (#7), with a double-scrape fixture
and its own-reading witness. Out of scope: drift actions, fit changes, #8, #11,
#14, #15, and the share planner's holds (why a second transfer waited), which
belong to the utilization-share work.

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
  [4.3](#43-what-the-tracker-owns)), a restart under the same name, an LWS group with mixed Ready times,
  a missed cycle, a slow fill, a just-Ready pod, a bridge lend, clock skew.
- **A shared write path already changed a learner silently.** #282 turned #5's
  in-spacing fold from a max into a lower median. #7 kept its max only through
  an empty pod name.
- **Self-sealing errors.** Each capacity learner is witnessed only when
  saturated, and the error that over-orders stops saturation. A too-low k2 or
  mu removes its own witness; for k2 it is permanent until forgotten
  ([3.4](#34-which-side-breaks-the-slo)). Validation residuals are one-sided by
  construction, and must not be read as two-sided.
- **The prefill bound** is reviewed before it moves: a double scrape once
  doubled its input, its max kept it, and `RaiseLast` keeps it past a reset.
- **The recorder can evict the controller** if pointed at `/tmp`. It is off by
  default and has its own volume and cap.
- **Open defects this does not fix:** instantaneous k paired with a rate[1m]
  ITL; speculative decoding; shape dependence of the ITL line (#274).

## Deferred to the implementing PRs

Round-3 lows, each settled in the PR named.

| item | PR |
| --- | --- |
| Determinism beyond the clock: sorted output, a fixed float format, no state write that depends on map order (`roleDemand`, `pendingRestore`, `eppByRole`); the acceptance replay runs three times in one process as well as 30 days apart | 0a, 0b |
| The recorder volume's `sizeLimit` is the per-volume ceiling (two recordings at the cap) plus headroom for the export. The writer stops each recording at the cap and deletes the oldest closed recording before the ceiling | 0b |
| The export also pseudonymises args and config strings and borrowed-line donors, with a per-export key that is not committed | 0b |
| `learner-write`'s `pods` lists at most 16, lowest reading first, with `podsOmitted` | 2 |
| Forget reaches every learner through a plain list of learner names in `learn`, not a registry; it is a named change of its own; when no key remains, the variant's learner series are deleted | 2 |
| Each operator-column cell names its metric series, or "metrics: none" | each PR |
| A witness column per learner in 3.4 ("witnessed when", "non-saturating witness"), which also tells the probe which side to drive | 3 |
| 3.6's cycle-count clocks become durations, or record the cycle interval in the threshold | each learner's PR |
| The snapshot gains a last-admitted stamp for start time and B; a v1 document's figure publishes 0 (as old as can be) | 5, or the first PR that publishes a start-time or B age, whichever is earlier |

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
