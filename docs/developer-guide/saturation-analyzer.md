# The saturation analyzer

How `internal/engines/analyzers/saturation` decides, with every formula it
uses and where each one lives.

The package is laid out by what each file computes: `analyze.go` runs one
cycle, `replica_capacity.go` sizes a replica, `fleet_shape.go` averages the
fleet, `queue_demand.go` prices unstarted work, `keys.go` files readings,
`aggregate.go` sums them, `roles.go` holds the P/D predicates, and
`analyzer.go` is the analyzer itself.

This is the component page. Two neighbours cover parts of it in more depth and
are not repeated here:

- [The throughput floor](saturation-demand-floor.md) — why the floor exists and
  what it is not invariant to.
- [Analyzer evidence](analyzer-evidence.md) — the measured runs behind the hold
  rules, the windows and the arrival rate.
- [Which component owns which number](signal-dataflow.md) — the same formulas
  arranged by provenance: which component produces each parameter and which
  consumes it, with the component diagram.

`throughput-analyzer.md` is a **different** analyzer
(`internal/engines/analyzers/throughput`). It shares vocabulary and almost no
code.

## What the analyzer produces

One `*allocation.NamedAnalyzerResult` per model per cycle. The optimizer reads
four numbers from it, and everything below exists to compute them:

| field | meaning |
| --- | --- |
| `Result.TotalDemand` | tokens of work the model is being asked to hold |
| `TotalSupply` | tokens its **ready** replicas can hold |
| `TotalAnticipatedSupply` | tokens its ready **and starting** replicas can hold |
| `RoleCapacities[role]` | the same three, per P/D role |

`RequiredCapacity` and `SpareCapacity` are **not** the analyzer's. They are
recalibrated downstream — see [RC and SC](#rc-and-sc).

## One cycle, end to end

`SaturationAnalyzer.Analyze` (`analyze.go`):

1. Average the fleet's shape — `fleetOutputLength`, `servedPromptLength`,
   `fleetPrefixHitRate`.
2. Note the shape against the per-model tracker; decide whether a change is
   outstanding ([Shape change](#shape-change)).
3. Resolve the generation length the queue is priced at, and the shape `mu` is
   priced at ([The service rate](#the-service-rate-mu)).
4. Fit one ITL line per decode variant ([The ITL line](#the-itl-line)).
5. Per replica: compute `k1`, `k2`, the effective capacity, and `mu`
   ([Per-replica capacity](#per-replica-capacity)).
6. Price the scheduler queue and attribute it to a role
   ([Demand](#demand)).
7. Aggregate to variant, role and model totals ([Aggregation](#aggregation)).
8. Apply the throughput floor ([The throughput floor](#the-throughput-floor)).

## The shape

`internal/signals/shape`. A workload's shape is the pair (input length, output
length) plus the prefix-cache hit rate, and two derived figures.

```
hitRate  = clamp(hitRate, 0, 1)        // NaN -> 0
ILeff    = IL x (1 - hitRate)
KVreq    = ILeff + OL/2
```

`ILeff` is the prompt the engine must actually compute — a prefix-cache hit
costs nothing. `KVreq` is the KV footprint of **one resident request**, averaged
over its life: it holds the whole prompt throughout, and its generation grows
from nothing to `OL`, so the generation contributes its mean, `OL/2`.

`KVreq` is the single most load-bearing quantity in the analyzer. Everything
that asks "how many requests fit" divides by it.

Two shapes are the same shape when both axes are within tolerance:

```
Within(a, b, tol)  =  |a.IL - b.IL| / b.IL <= tol
                  and |a.OL - b.OL| / b.OL <= tol
```

`shape.DefaultChangeTolerance = 0.20`.

## Per-replica capacity

`computeReplicaCapacity` in `replica_capacity.go`. A replica's capacity is a number of
**tokens**, and it is the smaller of two bounds.

### k1 — memory bound

```
k1 = floor(rm.TotalKvCapacityTokens x kvCacheThreshold)
```

`memoryBound()`. The usable fraction of the KV cache; `kvCacheThreshold`
defaults to `0.80`. The remaining fifth is headroom — an engine driven to a
full cache preempts and thrashes.

### k2 — compute bound

`estimateCapacityFromParams()` in `replica_capacity.go`, from the engine's own parameters
(`capacity.EngineParams`, parsed from the deployment):

```
B = EffectiveMaxBatchedTokens
S = MaxNumSeqs
I = avgInputTokens
O = avgOutputTokens

N_steady = min(B x O / (I + O), S)
k2       = floor(N_steady x (I + O/2))
```

`N_steady` is how many requests a continuous-batching engine holds in steady
state: each step spends `B` token-slots, a request needs `I` of prefill once
and `O` decode steps, so the fraction of a request's life that is decode is
`O/(I+O)` and the batch sustains `B·O/(I+O)` of them — capped by the engine's
own `--max-num-seqs`. Multiplying by `(I + O/2)` converts a count of requests
into the tokens they occupy, the same per-request footprint as `KVreq`.

`B` and `S` bound one engine. A replica running several data-parallel ranks
runs that many engines, so `computeK2` multiplies the derived k2 by the ranks
the row reports (`ReplicaMetrics.Ranks`), and `deriveMu` caps the sequences at
the ranks times `S`. k1 needs no scaling: the replica's KV is already all of
its ranks'. See
[Data-parallel pods](signal-dataflow.md#data-parallel-pods).

`computeK2` has a priority order (observed, historical, derived, fallback);
`k2Source` in the `replica-capacity-decision` log line says which answered.
k2's persistence is deliberate and load-bearing — see
[analyzer-evidence.md](analyzer-evidence.md).

Because a recorded k2 persists, the observed priority admits a reading only
when it is a replica at its limit (`k2Unsettled` in `k2_settle.go`, the strict
admission `docs/plans/analyzers/k2-capacity-model.md` calls for). A reading is
suspect when the busiest rank's queue, at the request's footprint, is larger
than everything the rank holds:

```
joined   = Pod Ready (else first seen Ready) under JoinedReplicaWindow (1m) ago
suspect  = queue x (I [+ O unless prefill]) > tokens / ranks
admitted = !joined && (!suspect
        || (rate >= K2SettleRateFraction x rateOnRecord             // 0.7
            && tokens >= K2SettleRateFraction x k2OnRecord)         // k2OnRecord > 0
        || settledOverWindow: the newest earlier reading at least
             K2SettleLookback (1m) old, it and every reading since queued
             with no gap over 1m, and
             (1 - K2SettleMaxRise) x tokens then
                 <= tokens <= (1 + K2SettleMaxRise) x tokens then)   // 0.25
```

A replica that joined the fleet under a rate window ago is filling, whatever
its queue: its one-minute gauges still hold the minute before it was routed
to. On the E1 run (2026-10-23) one just joined read 84 waiting on 174,327
resident, the queue no deeper than its batch, and was recorded on sight; the
window settled at 264,265 while every replica then held 331-351k with no queue.
The age is read from when the Pod last became Ready, as the collector reports
it in `ReplicaMetrics.ReadySince` (`replicaReadySince`, read through
`SaturationAnalyzer.readySince` so tests stand in for the collector). A pod
back from an engine or LWS group restart under the same name has a new Ready
time: in review one absent for 75 s kept its old first-seen time and its
filling reading (174,327 tokens, 1.0 req/s) became k2 and mu
(`TestReadySince_ARejoiningPodIsAsFreshAsANewOne`).
Where the time is unknown, `readySince` in the settle memo is when the analyzer
first saw the replica Ready. A replica already Ready on the memo's first cycle
then has an age the analyzer cannot know and is taken as established, so a
restart does not hold every reading back; one unseen for over `K2SettleMaxGap`
has rejoined.

`I` and `O` are the replica's own lengths, or the fleet's where it has none. A
replica that has completed nothing reports no lengths (the per-pod mean is
0/0), and a footprint of zero made every queue on it the overflow of a batch
"at least as large", so the test admitted exactly the replica it is for.

The band is two-sided. A fall is not settling: a replica full and queued at
270,539 that next reads 31,713 under 160 queued has had its traffic moved off
and handed back, and admitted on the rise test alone it took the window to
222,774 (imbalance audit, 2026-10-23).

The band is measured a window back, not a cycle back. Readings are one-minute
peaks every 15 s, so consecutive ones share 45 s and a 25 % band per cycle
allows 2.4x a minute. A batch capped by `max_num_seqs` on long outputs fills
for minutes: replaying the statistics review's simulated traces
(`testdata/k2-fill`), the cycle-back test first recorded 0.624 of the steady
figure on 1000/4000 and 0.489 on 1000/6000 at +37 s; a window back records
1.042 at +82 s and 1.053 at +127 s. The cost is refusals at a true limit with
a noisy gauge: 0.8 / 18 / 52 / 67 % at CV 0.1 / 0.2 / 0.5 / 0.8, and the first
k2 still comes within 13 / 24 / 40 cycles at worst for CV 0.2 / 0.5 / 0.8
(`TestK2Settle_ANoisyLimitStillLearns`).

The rate path needs residency too. A rate shows a replica is busy, not that it
is at its limit: a sibling already serving 1.5 req/s against 2.0 on record,
handed the hot queue (25 waiting on 160,000 resident), passed on rate alone
and pulled the k2 mean 250,000 -> 232,000. With no k2 on record only the
window test admits.

`rate` is the replica's own `saturatedCompletionRate`; `rateOnRecord` is the
bucket's measured saturated throughput, read without touching the window, or
the derived mu; `k2OnRecord` is the bucket's k2 window mean, also read without
touching it. A refused reading logs `P1-obs-unsettled` and writes neither the
k2 window nor the throughput window. Each replica's readings over
`K2SettleMaxGap` (2m) live in `k2Settle`, one memo per `namespace|model`, swept
beside `hotRankHeld`. A refusal can only keep k2 where it was; nothing here
raises a recorded k2.

### Which bound applies

```
effectiveCapacity = k2  if k2 < k1
                    k1  otherwise
```

Logged as `boundBy: "k1-memory"` or `"k2-compute"`.

## The ITL line

`internal/signals/itl`. Inter-token latency rises with KV occupancy, and over
the observable range it rises linearly:

```
ITL(k) = A x k + B
```

where `k` is KV usage (0–1) averaged over the same minute as the ITL rate
(`rm.KvUsageForITL()`: `KvUsageMean`, from `avg_over_time[1m]`, or
`KvUsageInstant` when that was not read; issue #393). Fitted by ordinary least
squares over a rolling window of `(k, ITL)` observations:

```
A = (n x SUM(k x itl) - SUM(k) x SUM(itl)) / (n x SUM(k^2) - SUM(k)^2)
B = (SUM(itl) - A x SUM(k)) / n
```

A fit is refused unless it is usable (`ValidModel`): `A` and `B` finite,
`A > 1e-12` (a flat or falling line is not this physics), and
`A x 0.85 + B > 0`.

The window and the fit's rules live in `itl.Tracker`, which the slo analyzer
uses too ([the slo analyzer](slo-analyzer.md#itltracker)). A full fit needs
evidence:

| constant | value | why |
| --- | --- | --- |
| `DefaultMinSamples` | 10 | a line through noise is not a line |
| `DefaultMinKSpread` | 0.30 | samples clustered at one `k` fix an intercept, not a slope |
| `DefaultWindowMaxSize` | 20 | |
| `DefaultObservationMaxAge` | 30m | |
| `DefaultMinObservableK` | 0.15 | below this the engine is not batching |
| `DefaultMaxObservableK` | 0.80 | above it, preemption makes ITL non-linear; a replica at its sequence cap is still read (below) |
| `DefaultSteadyFor` | 2m | a replica's `k` must have held still this long |
| `DefaultMaxKDrift` | 0.10 | how far it may have moved and still count as still |
| `DefaultSteadyCarry` | 5m | how long a steady reading may outlive the main window's oldest |
| `DefaultSteadyWindowMaxSize` | 40 | steady readings kept per key |

The full fit, and so the remembered `B`, uses **steady** readings only: a
replica whose `k` stayed within `DefaultMaxKDrift` over the last
`DefaultSteadyFor`, and that has served at least that long if it joined after
the key was first observed. `k` is instantaneous and ITL is a `rate[1m]` mean,
so across a step in load the two describe different moments; in a balanced
fleet that step is the only spread a window ever holds. Before this gate, two
identical models in two namespaces remembered `B` = 4.5 ms and 14.8 ms from
whichever transient each window caught, and priced the same replica at 5.3 and
7.7 requests/s (issue #270). `itl-window` reports `unsteady` and `steadyHeld`.

The pinned fit, which prices a balanced fleet, takes the steady readings too
whenever there are at least `DefaultMinSamples` of them, and every reading only
below that, so a fleet that has not settled still gets a line. Taking every
reading, it took each load step whole: on the issue #270 rerun `A` went from
0.077 to 0.207 on the cycle the load stepped, and the derived mu fell from 6.4
to 2.65 requests/s for two and a half minutes.

A saturated fleet has few steady readings: `k` swings between above the band
and its dips into it by more than `DefaultMaxKDrift`, and on a two-namespace
run the steady readings numbered 0 on almost every cycle. So the pinned fit
takes every reading, and a replica's first readings after it starts serving (a
pod that joined, or one the load came back to) sit many times above the line.
Three such readings of sixteen, at about 0.46 of `k` and 435 to 496 ms, moved
least squares' `A` from 0.101 to 0.170, and the derived mu read 3.1 to 4.2
requests/s against the 6.1 to 6.2 each replica served, for a whole
twenty-minute phase.

Those readings are left out by the replica's state, not by their value: a
reading taken while its replica is inside its settle (it joined, or the load
came back to it, less than `DefaultSteadyFor` ago) is marked when it is taken,
and the pinned fit skips it. When fewer than `DefaultMinSettledSamples` (3)
readings are left, the key's last `A` holds (`itl-fit` logs `slopeHeld`), and a
key with no `A` yet gets no line. `itl-window` reports the marked readings as
`settling`.

The last `A` also holds when the window itself runs short: fewer than
`DefaultMinSamples` readings left after `DefaultObservationMaxAge`. A replica
below `analyzerTuning.itlBand.minObservableK` offers no reading, so a fleet that learned its line at
load and then served light load for half an hour used to lose the line, the
derived mu and the throughput floor with it, and was sized by KV occupancy
alone. At light load that reads far below the throughput share served: one H200
replica read 0.064 while serving 25% of its saturated rate (issue #284). The same
applies above the band, for a replica not at its sequence cap.

The hold is bounded. `itl.Admission.SlopeHoldFor` is the analyzer's
`BucketRetention` (the horizon `itlLines` ages a finished fit on), and
`Admission.Args` the variant's `windowArgs`: a slope older than the one or
fitted under other arguments is not held. `itlUnderArgs` also sweeps the key,
slope included, on an arguments change. `slopes` is not in the snapshot, so a
restart ends the hold too. A held line is the variant's own: it may order, and
a variant woken from zero inside the hold is priced from it on sight and can
settle a shape-change hold, as its fresh fit would have. It is **not**
republished to `itlLines`: `publishLine` stamps `learnedAt` with now, so a
held line republished every cycle would never age out and would overwrite a
sibling's fresh line on the same configuration. `itl-fit` logs `slopeHeld`
and `slopeAge`, `derived-mu` logs `lineHeld`.

A key
that never fitted, and has no sibling's line to borrow, still has no line;
`itl-line-none` says so each cycle (`derivedMu=false`), so `itl-window`'s
`offered: 0` beside `belowBand` reads as light load, not a broken window.

`FitPinnedB` then takes `A` as the `k`-weighted mean of the readings' own
slopes, `sum(ITL_i - B) / sum(k_i)`, with nothing trimmed. A trim by value
cannot tell a warm-up from a replica that is really slower, since both sit
above the line:

- The interquartile mean set aside one replica at slope 0.12 among four at
  0.08 entirely, pricing the fleet's capacity 8.5% high. It also let the
  warm-ups through once they carried more than a quarter of the weight: a
  fleet of three growing to six read `A` = 0.127, with mu about 20% low.
- The weighted median jumped from one group of slopes to the other as their
  weight crossed one half. A fleet serving two shapes side by side (0.08 and
  0.12, half each) had `A` swing 36% of its mean.

The mean blends a slow replica in at its weight, holds that two-shape fleet to
2%, and on the same fixture with three extra joiners reads 0.097. Replayed over
the two-namespace runs, the pinned `A` has a median of 0.097 and 0.101 for the
first run's two namespaces (worst 0.102) and 0.079 for the second.

`itl-window` counts every row under one reason (`offered`, `notReady`,
`noITL`, `noK`, `belowBand`, `aboveBand`), so they add up to `replicas`, and
reports `steadySpread` beside `minKSpread`, the other half of `ready`.

A reading above `DefaultMaxObservableK` is kept when its replica is at its
sequence cap (`itl.Admission.atSequenceCap`): `RunningRequests`
(`vllm:num_requests_running`) is exactly `max_num_seqs` times the replica's
ranks, `max_num_seqs` was read from the engine's flag
(`EngineParams.ExplicitMaxNumSeqs`; the parser's default of 256 is not vLLM
V1's 1024 on large cards), requests are waiting, and `PreemptionRate`
(`rate(vllm:num_preemptions_total[ITLWindow])`, the ITL mean's own window)
was read and is at most `analyzerTuning.itlBand.capMaxPreemptionRate` (default 0). The band
edges are the policy's `analyzerTuning.itlBand.minObservableK` and `analyzerTuning.itlBand.maxObservableK`
(`ScalingPolicy.ITLBand`); `Observe` takes them, the cap and the allowance as
one `itl.Admission`, built alike by both analyzers. `pricingK`, and the slo
analyzer's fill, clamp to the same band. Such a replica is not short of memory, so the reason for the band does
not apply to it. On one run a decode replica held 256 running and 768 waiting
at `k` 0.85-0.89 for 20 minutes while three siblings sat below 0.15, and no line
was ever fitted (issue #287). `itl-window` counts these readings as `capped`,
a part of `offered`, and logs the `batchCap` it judged them by (`--max-num-seqs`,
lowered by `--max-num-active-seqs`). An unknown
cap or preemption rate (SGLang publishes no preemption counter) keeps the band
closed.

The steady readings are kept apart from the main window, and outlive its oldest
reading by up to `DefaultSteadyCarry`. A load step is what gives a balanced
fleet its spread, and it fills the 20-reading main window with unsteady
readings: every replica is held out for `DefaultSteadyFor`, so N replicas add
8N of them at 15 s cycles. When the steady readings were trimmed to the main
window, the old level's were gone before the new level's were steady, and no
fleet of two or more replicas ever had the spread for a full fit: on the issue
#270 rerun, `itl-window` was `ready` on 0 of 294 multi-replica cycles, and `B`
stayed at the 6 ms default. Carried, the old level is still there when the new
one settles: on a step from `k` 0.35 to 0.70 (`steady_test.go`), every fleet of
1 to 16 replicas learns the true `B`. The carry is the full fit's only: the
pinned fit reads the steady readings the main window spans, reaching into the
carry only for the `DefaultMinSamples` it needs through a step. `A` moves with
the workload, and fitted on the carried window it followed a heavier shape at
the same fill twice as slowly (two replicas: 17 cycles to the new `A`, against
8). A level the fleet only passed through
never settles, so it never reaches the steady readings and teaches nothing.
A reading with no `k` (the collector's zero value) is not traced, so one
missed scrape does not make its replica unsteady. A replica the load comes back
to after two or more rows with neither an ITL nor a `k` starts over as a joiner
and waits out `DefaultSteadyFor`. A row with a `k` and no ITL is a busy replica
whose ITL was not read (the collector drops a failed query's values whole), and
does not count toward idling: counted, an `avg_itl` query that failed on two
cycles running restarted every replica of a busy fleet. An idle replica leaves nothing in its trace, so its first
reading, which carries the warm-up ITL, was otherwise steady: on a load step
that brought an idle replica back, the full fit stored `B` = 77 ms against a
true 12 ms. One such row between two readings is a missed scrape.

From the line, two closed forms:

```
Sequences(k, C, KVreq) = k x C / KVreq          // resident requests at occupancy k
TokenRate(m, k, C, KVreq) = Sequences / ITL(k)  // generated tokens/second
```

`GPSErrorPct` compares `TokenRate` against the observed
`rm.GenerationTokenRate`. It **reports and does not act** — `noteLineMismatch`
logs `itl-gps-mismatch` with `gates: false`.

## The service rate (mu)

`mu` is requests completed per second by one saturated replica. There are two
ways to get it. The derived one can be priced for a shape the fleet has never
run; the measured one wins where the fleet has measured this shape itself
(`useDerived`: an own window with at least `MinThroughputSamplesToOrder`
samples), until it is stale and sits more than `MeasuredDerivedMaxRatio` (2x)
above the derived figure (`measuredYieldsToDerived`, `muChoice`):

- **Fresh** is the bucket reading saturated, within `MeasuredMuFreshFor` (5m),
  at a rate within 2x of the window's median, whether that reading was
  recorded or refused (`noteSaturatedReading`; the newest sample stands in
  only where no such reading is on record). A fresh measurement wins either
  way. A replica queued every minute at 1.2 req/s used to have every reading
  refused under a derived 3.6, went stale by construction, and was priced at
  3.6 (`TestMuChoice_ContinuousSaturationKeepsItsMeasurement`).
- **Stale and disagreeing** prices the **lower** figure, which over-orders: a
  measurement far under derived keeps pricing, one far over it yields.
- **For at most `MeasuredLowHoldFor` (30m).** A measurement more than 2x
  under derived yields once its bucket has not read saturated in agreement
  for that long. A too-high derived figure corrects itself (the fleet
  saturates and re-freshens the measurement); a too-low measurement does not,
  because an over-ordered fleet never saturates. In review 1.2 req/s held for
  20 unsaturated minutes against a derived 3.585, three times the replicas
  the line priced, with no end date
  (`TestMuChoice_AStaleLowMeasurementHoldsForAtMostMeasuredLowHoldFor`). If
  the measurement was right, the fleet saturates and takes it back: at worst
  one saturated episode per 30 minutes.
- **A served rate contradicts it.** A measurement under derived yields at
  once, fresh or not, while a Ready replica of the variant serves more than
  2x it this cycle (latched down to 1.6x): that replica sustains what it
  serves (`servedBounds`,
  `TestMuChoice_ALowMeasurementAServedRateContradictsYields`). With no line
  there is nothing to yield to, and the measurement stands.
- **Its full siblings serve more.** A measurement under
  `SiblingMuTolerance` (0.8) of the lower median rate served by the
  variant's Ready replicas with a full cache, queued ones included, yields
  to that median when at least one of them is unqueued, line or no line, logged as `saturated-throughput-yields-to-siblings` with the
  bucket `served-siblings` (`siblingYield`). A replica at 0.55x its siblings
  passes the 2x served bound and, as the only queued replica, is the window.
  In review it priced the fleet at 1.8x the replicas for as long as it
  stayed slow. Neither the bound nor the 30-minute hold released it
  (`TestMuChoice_AModeratelySlowReplicaYieldsToItsFullSiblings`). The
  yield is remembered after the larger fleet stops filling the siblings,
  until newer sibling evidence moves it or the window starts over. A median
  read from queued replicas alone never opens a yield, so a slow replica
  queued alone is never its own sibling; it moves one already open, and
  ends it once the measurement is no longer under it. The queued replicas
  count because the router steers away from them: read from the unqueued
  alone, three replicas at 2.0 beside one at 3.6 priced every replica at
  3.6 and ordered 5.71 replicas for a load needing 7.84
  (`TestMuChoice_AMostlySlowFleetIsNotPricedAtItsFastReplica`,
  `TestMuChoice_ASiblingYieldEndsWhenItsSiblingsReadTheMeasurement`). 0.8 because equally saturated replicas at one shape read within
  about 10 % of each other (0.90-1.07 on E1). It bounds what one slow
  replica can over-order to 1.25x
  (`TestMuChoice_AMeasurementNearItsFullSiblingsStands`).
- **Latched.** Once a measurement has yielded it stays yielded until it comes
  back within `MeasuredDerivedReturnRatio` (1.6x) of derived, or the bucket
  reads saturated in agreement again. Derived mu is recomputed every cycle
  from the fleet's output length; with a hard edge at 2x a measurement near it
  flipped the priced mu 8-11 times in 20 cycles under 4 % noise on the length
  (`TestMuChoice_TheYieldDoesNotFlipNearTheRatio`).

Under EPP flow control the queue that writes a sample rarely builds again,
and a stale high reading must not under-order the fleet. Within 2x the
measurement keeps winning however old it is: run R's 1.27 req/s against a
derived 0.67 is the case it is there for.

### The pricing point

```
kPrice = clamp(kvCacheThreshold x scaleUpThreshold,
               DefaultMinObservableK, DefaultMaxObservableK)
```

`pricingK()`. With the shipped defaults that is `0.80 x 0.85 = 0.68`, **not**
`0.85`. This composition is easy to get wrong: `k_sat` (0.85) is where the ITL
line is validated, while 0.68 is the occupancy the autoscaler actually targets.

### The derived mu

`deriveMu()` in `mu_from_itl.go`:

```
C        = capacityTokensFor(params, rm.TotalKvCapacityTokens)
seqs     = min(Sequences(kPrice, C, KVreq), MaxNumSeqs)
tokenSec = seqs / ITL(kPrice)
mu       = tokenSec / avgOutput
```

It declines (`ok: false`, and the floor then emits nothing for that role) when
the model is unfitted, `avgOutput <= 0`, `KVreq <= 0`, `seqs <= 0`,
`ITL(kPrice) <= 0`, or `kPrice` is outside `(0, 1]`.

Note `avgOutput` is a **separate parameter** from `fleet.AvgOutputTokens`. That
separation is deliberate and is the subject of the next section.

### Both halves of the shape, one window

`avgOutput` divides, and `KVreq` — inside the shape — decides how many requests
fit. During a shape change the divisor follows a short `[1m]` window so the old
shape's long stragglers cannot dominate it. **The shape must follow with it.**

`analyze.go` resolves the pair together:

```
muDivisor = ExpectedOutputTokens(fleetOutput, stableOutput, 512)
muInput   = fleetInput
if shapeChangedWithin(window):
    if recentOut := fleetOutputLengthRecent(...); recentOut > 0:
        muDivisor = recentOut
        if recentIn := servedPromptLengthRecent(...); recentIn > 0:
            muInput, muShortWindow = recentIn, true

muShape = shape.New(muInput, muDivisor, hitRate)  if muShortWindow
          fleetShape                               otherwise
```

Measured on a 6000/1000 → 1000/4000 swap: with only the divisor on the short
window, a request priced at **7017 tokens** — larger than either real shape
(6500 then 3000) — halved `mu` to 1.57 req/s against a settled 2.76, and the
floor ordered 7 decode replicas where 3 was right, on a router queue that was
zero throughout. Priced as a pair the same cycle reads `KVreq` 3000 and `mu`
3.20. Both halves or neither: if the engine publishes the short-window output
but not the short-window prompt, the divisor still moves alone, because the
alternative is the straggler bug the short window exists for and that error is
an order of magnitude larger.

`ExpectedOutputTokens` (in `internal/config`) is first-positive-of:
the fleet's own reading, the last stable shape, the ScaledObject's
`defaultOutputTokens` trigger metadata, then `DefaultExpectedOutputTokens`
(512).

### The measured mu

`saturatedCompletionRate` in `shape_change.go`, used when a replica is
saturated. For a generating role it is priced from tokens, not completions —
completions are bursty and a max window keeps the burst. For prefill:

```
mu_prefill = rm.PrefillComputedTokenRate / ILeff
```

`PrefillComputedTokenRate` is **summed** across a pod's engines, not averaged:
each computes part of the prefill and their rates add.

The window (`recordSaturatedThroughputFrom`) takes one sample per
`ThroughputSampleSpacing`. Readings inside the spacing fold into the last
sample: by their **max** for re-reads of one replica (the same rate window read
again) and by their **mean** across replicas (independent readings,
`throughputPodReads`). The mean because a fleet serves the sum of its
replicas' rates: a lower median ignored a slow replica once three read, and
four at 1, 1, 1 and 0.5 priced 1.0 against the 0.875 they serve
(`TestMeasuredMu_ASlowReplicaCountsInTheSample`). The readings that reach the
fold have passed the settle, joined, filling and served-bound tests. Only replicas that are themselves admitted saturated
readings (`P1-obs`, after the settle test) read into a sample at all, so on a
fleet the router loads unevenly the idle replicas neither enter nor dilute it
(`TestMeasuredMuAndK2_IgnoreTheIdleReplicasOfAnImbalancedFleet`: 1 of 4
saturated learns the balanced fleet's mu and k2). With a max across replicas the sample was the hottest of
N readings, and the window's median rose with the number of saturated replicas:
with 6 % reading noise on a true 6.0 req/s, +5.4 % at one replica and +13.6 %
at sixteen. Per replica it stays at the one-replica +5.4-5.5 % at any size
(`TestMeasuredMu_DoesNotGrowWithTheSaturatedFleet`).

An admitted k2 reading is not always a service rate. `throughputRefusal`
(`throughput_admission.go`) also refuses, logging
`saturated-throughput-refused`, a reading from a replica that completed no
request over the window (`RequestRate` 0: every sequence is still growing),
one whose generated tokens run more than `MaxTokensOverCompletions` (1.15x)
ahead of what its completions account for (`tokensOutrunCompletions`, the
same fill a minute on; not judged for prefill),
one under half of what a Ready replica of the variant served this cycle
(`servedBounds`, logged as `servedBound`: a served rate is a lower bound on
`mu`, so this is one slow replica), and one under `MinMeasuredOfDerived`
(0.5) of the derived mu for the same line. Only queued replicas are read, so
one slow replica -- a degraded GPU, a CPU-starved pod -- is often the only
reading: in review one queued at 0.3 req/s beside three serving 3.6 unqueued
was confirmed (below) and priced the fleet at 45 replicas for a load four
serve, and 55.5 for 30 minutes after it recovered; it now reads 3.8 and 4.0
(`TestMuChoice_OneSlowReplicaIsNotTheFleetsMu`). The bound is 2x, not 1x:
equally saturated replicas read a few percent apart. On
E1 a replica just joined, 84 waiting and no completion, generated 49 tokens/s:
0.0995 req/s became the bucket's mu against a derived 3.1-5.7 and ~6.2 served,
and priced 2 req/s as 12-20 replicas for the rest of the run
(`TestE1Replay_AJoiningReplicaIsNotAThroughputSample` replays it). A filling
replica reads low for a minute or so; one replica that reads under half the
derived mu on every saturated cycle for `MeasuredLowConfirmFor` (2m, no gap over
a minute) is a real low rate, the line the wrong figure, and its readings are
recorded from then on (`saturated-throughput-low-confirmed`,
`TestMuChoice_ALowRateThatRepeatsIsRecorded`). The runs live in
`muChoice.low`. A reading the served rate contradicts is never confirmed.

A filling replica can read high as well. On the E1 rerun two replicas that
had joined a minute before, still queued, read 6.70 and 6.99 req/s from
their token rate while they completed 5.08 and 4.86. Their batches were
still growing, so tokens went to sequences that had not completed yet. The
two readings became the window's median, and sg1 was priced at 6.70 against
the 6.0-6.1 its saturated replicas completed. That is 11 % high, which
under-orders. Full replicas past their fill read 0.90-1.07x tokens over
completions across 24 scrape intervals of the same run. The filling
readings read 1.32x and 1.44x, so a reading over 1.15x is refused. Such a
replica is also left out of `servedBounds`
(`TestE1_AFillingReplicaDoesNotRaiseMu`, which read 6.70 before).

## Demand

Two sources, added.

### A replica's own demand

```
replicaDemand = residentCharge(rm, rm.TokensInUse, capacity, routing) + waitingQueueDemand(rm, role)
```

`residentCharge` is `rm.TokensInUse`, the collector's one-minute peak, except
for a Ready replica with nothing waiting that is idle NOW (`KvUsageInstant`
under `idleKvUsage`, with `KvUsageInstantKnown` set) while a replica of its
role is busy now (`cycle.busyNowRoles`, from `rolesBusyNow`): that one is
charged `KvUsageInstant x capacity`, never above the peak
(`replicaRouting.kvFromInstant`). Summed per pod, peaks count a load the router
moved within the minute once on every pod it visited: four pods each full for
15 s, one full now, read 4,416,512 tokens for a load of 1,173,863. A replica
with requests waiting keeps its peak: its instant KV dips between prefill
batches and on preemption, and charged at the dip a fleet queued everywhere
ordered nothing (instants 0.9/0.2/0.2/0.2 priced 3,161,293 against 4,616,512).
A missing instant series is not a reading of zero: the collector sets
`KvUsageInstantKnown` only when the series was read (per pod, only when every
rank reported one), and without it the peak stands. With no replica busy now
every peak stands. Only demand reads this; `kvFull`, k2 and its admission keep
the peak.

This rule counts one load once in time; it is not a routing compensation and
moves no demand between replicas. The role's charge never falls under the sum
of what its replicas hold now: a replica busy now keeps its peak, one idle now
is charged its instant reading, and no queue is discounted.

The waiting queue is charged in full, on whichever replica it stands. Demand
and supply are both sums over the role's replicas, so a queue on the wrong
replica is in the sum either way, and taking it out can only hide demand. An
earlier revision moved the share a settled-idle sibling had room for out of
`LocalQueueDemand` and out of the floor's backlog, and charged it nowhere: in
review a two-replica variant at 0.93 of its supply read 0.50, three replicas
queued 60 each beside a fourth never routed to implied 5.88 replicas against
the 9.41 the fleet needed, and 439 prompts on one of four prefill pods priced
2,169,104 tokens against the 2,866,448 resident plus queued
(`idle_room_test.go`, `imbalance_test.go` pin the full charge). Uneven routing
is reported by the load-balance diagnostic, not compensated here; where it
moves demand it raises it.

`settledIdleDecode` (`idle_room.go`) still reads settled idleness, for one
purpose: a decode replica idle for `IdleSettledFor` (2m) relaxes the hold on
prefill's demand while decode is saturated (`applyFloorAndHolds`), which can
only raise prefill's order. Idle is `pricedIdle`: `replicaIdle` (Ready,
busiest-rank KV under `idleKvUsage` = 0.30 of the cache, nothing waiting) AND
busiest-rank tokens under 0.30 x `EffectiveCapacity` (min(k1, k2)), on every
cycle for `IdleSettledFor`, no two readings over `IdleMaxGap` (1m) apart, own
replicas only. The runs live in `settleMemo.idle`.

### The scheduler queue

`estimateSchedulerQueueDemand`. The router's queue holds requests no pod has
started, so it is charged separately:

```
tokensFromBytes = QueueBytes / BytesPerToken        // BytesPerToken = 4
tokensFromCount = QueueSize x avgInput
inputTokensRaw  = max(tokensFromBytes, tokensFromCount)

inputTokens        = inputTokensRaw x (1 - avgHitRate)
prefillInputTokens = inputTokensRaw x (1 - prefillHitRate)
outputTokens       = QueueSize x avgOutput
```

Attribution depends on the fleet's shape, because on a P/D split the two roles
owe different halves of the same request:

| fleet | prefill is charged | decode is charged |
| --- | --- | --- |
| P/D split | `prefillInputTokens` | `outputTokens` |
| single role | — | `inputTokens + outputTokens` |

The two hit rates are not the same figure: prefill is discounted at **its own**
role's hit rate, because prefill is where the prefix cache lives.

`withExpectedOutputTokens` fills `avgOutput` for output-generating replicas
reporting none, so a cold fleet does not price its queue at zero — `mu` divides
by `avgOutput`, and a fleet that has completed nothing would otherwise divide by
zero and emit no floor at all, in the one window the projection exists for.

With no replica reporting a shape at all (parked at zero, or Ready without
engine series), `estimateSchedulerQueueDemandAt` falls back for each half on
its own: `avgInput` to the fleet's tracked prompt length (`stableInput`; the
bytes still win when larger) and `avgOutput` to the same expected output
length, unless prefill is the only active role. Before, 100 queued requests
were worth 0, 100 000 or 51 200 tokens depending on whether there were zero
replicas or one without series and whether the queue reported bytes.

## Aggregation

`internal/engines/aggregation`, over `[]domain.VariantCapacity`:

```
perReplica(vc)         = vc.PerReplicaCapacity   (0 if non-positive or +Inf)
TotalSupply            = SUM vc.ReplicaCount x perReplica(vc)
TotalAnticipatedSupply = SUM (vc.ReplicaCount + starting(vc)) x perReplica(vc)
TotalDemand            = SUM vc.TotalDemand
```

`AggregateByRole` computes all three per role. The asymmetry is the point:
starting replicas count toward supply (so the engine does not double-order
while pods launch) but not toward removable capacity.

## RC and SC

Not the analyzer's own. `applyUniversalThreshold` in
`internal/engines/steadystate/engine_v2.go` recalibrates them at every scope
from one formula:

```
RC = max(0, TotalDemand / scaleUp   - TotalAnticipatedSupply)
SC = max(0, TotalSupply  - TotalDemand / scaleDown)
```

`scaleUp` defaults to `0.85`, `scaleDown` to `0.70`. Between
`scaleDown x supply` and `scaleUp x anticipated` the engine neither orders nor
releases.

`atLeastZero` floors these, and rejects NaN and `+Inf` explicitly: demand is
deliberately not sanitised on the way in, so this is where a non-finite figure
has to fail closed. `v < 0` would not do it — every comparison against NaN is
false.

## The throughput floor

`internal/signals/floor`. Occupancy alone under-sizes a fleet that is keeping
up, because a fleet exactly keeping pace looks half-empty. The floor sizes the
role from arrival rate and backlog instead. Per role:

```
cost   = median over the role's own replicas of (P / mu)   // tokens per unit arrival rate
         x scaleUp for a derived mu                        // replicaCost
mu     = median over the role's own mus
horizon = max(drainSeconds, startSeconds[role])            // BacklogDrainSeconds = 60
b      = backlogAtLanding(observed, lambda, mu, startSeconds, ready, starting)
rate   = lambda + b / horizon
floor  = rate x cost
replicasImplied = rate / mu
```

Medians, not maxima: one replica draining a batch must not set the role's price.

The `x scaleUp` on a derived mu is not extra headroom; it removes a second
copy. The engine scales out at `scaleUp` of what a replica is worth
(`RC = D / scaleUp - anticipated`). A measured mu is the rate at saturation, so
one `scaleUp` is right for it. A derived mu is already the rate at `kPrice`,
the target fill, so the engine's division applied the headroom twice: on the
defaults the floor scaled out at 85 % of the rate at 0.68 fill, which with
throughput concave in fill is about 0.32 fill (97 sequences on the run that
found it, against 205 at 0.68). With the factor, a replica at `kPrice` serving
`mu` arrivals reads `RC = 0` from occupancy and from the floor alike
(`TestThroughputFloor_AgreesWithOccupancyAtTheTarget`).

The projection is what makes a backlog an *order* rather than a residency:

```
arrived   = lambda x T                                     // T = startSeconds
credit    = SUM startingCredit(T, pending, ages)
served    = mu x (ready x T + credit)
projected = max(0, backlog + arrived - served)
```

A replica ordered now lands in `T` seconds; by then the queue has grown by
`lambda x T` and the ready fleet has served `mu x ready x T`. A replica already
starting is credited for the part of `T` it has left — with no age information,
`pending x T / 2`.

Two hold rules gate ordering, both measured rather than reasoned:

- `MinThroughputSamplesToOrder = 2` — two readings a
  `ThroughputSampleSpacing` (1m) apart, so the second cannot come from the same
  drain as the first.
- A **derived** mu may order immediately: it is priced for the shape that
  changed to, which is the whole reason it exists.

Both are covered in [analyzer-evidence.md](analyzer-evidence.md).

## Shape change

`shape_change.go`. One `shapeMemo` per `namespace|modelID`, guarded by
`SaturationAnalyzer.mu`.

- `noteFleetShape` records this cycle's shape and declares a change when it
  falls outside `Within(tolerance)` of the stable one. It stamps `changedAt`
  (the outstanding hold) and `lastChangedAt` (the fact of the change).
- `settleFleetShape` clears the hold once the fleet has measured itself.
- `shapeChangedWithin(window)` answers `now - lastChangedAt < window`.

The two timestamps are deliberately separate. `DisableShapeChangeHold` leaves
`changedAt` unset for ever, so anything keyed on the hold would see one cycle
of shape change and then none — putting the mu divisor back on the `[5m]` mean
for the rest of the straggler window, silently, through a flag documented as
only turning off the fleet hold.

`ShapeChangeHoldMax = 5m`, overridable per policy via `ShapeChangeWindow`.

### Output buckets

Throughput readings are keyed by output length so a reading taken under one
shape cannot price another:

| bucket | upper bound (tokens) |
| --- | --- |
| short | 100 |
| medium | 500 |
| long | 1500 |
| extra long | 3000 |
| very long | 6000 |

Prefill is keyed `prefillOutputBucket = "noout"` instead: it emits about one
token per request, so its output length is not a property of its work.

The k2 history is keyed the same way (`k2KeyOutput` in `keys.go`): by the
fleet's **tracked** output length, which moves only when the shape changes by
more than `DefaultChangeTolerance`, not by the replica's own average. A
workload at `max_tokens=500` reads per-replica averages of 482-582 and, keyed
on those, alternated between the `medium` and `long` windows cycle by cycle,
each holding a different k2 (260 622 and 187 458 on the run that showed it).
Prefill keeps its own average, which is always `short`. The key strings are
unchanged, so stored windows are still found.

## Constants

| constant | value | package |
| --- | --- | --- |
| `DefaultKvCacheThreshold` | 0.80 | `internal/config` |
| `DefaultScaleUpThreshold` | 0.85 | `internal/config` |
| `DefaultScaleDownBoundary` | 0.70 | `internal/config` |
| `DefaultQueueLengthThreshold` | 5 | `internal/config` |
| `DefaultChangeTolerance` | 0.20 | `internal/signals/shape` |
| `DefaultKSat` | 0.85 | `internal/signals/itl` |
| `DefaultMinObservableK` | 0.15 | `internal/signals/itl` |
| `DefaultMaxObservableK` | 0.80 | `internal/signals/itl` |
| `DefaultMinSamples` | 10 | `internal/signals/itl` |
| `DefaultMinKSpread` | 0.30 | `internal/signals/itl` |
| `DefaultSteadyFor` | 2m | `internal/signals/itl` |
| `DefaultMaxKDrift` | 0.10 | `internal/signals/itl` |
| `BacklogDrainSeconds` | 60 | `internal/signals/floor` |
| `MinThroughputSamplesToOrder` | 2 | `internal/signals/floor` |
| `ShapeChangeHoldMax` | 5m | `saturation` |
| `DecodeSaturationMemory` | 1m | `saturation` |
| `K2SettleRateFraction` | 0.7 | `saturation` |
| `K2SettleMaxRise` | 0.25 | `saturation` |
| `K2SettleLookback` | 1m | `saturation` |
| `K2SettleMaxGap` | 2m | `saturation` |
| `balance.IdleKvUsage` (as `tuning.idleKvUsage`) | 0.30 | `internal/signals/balance` |
| `IdleSettledFor` | 2m | `saturation` |
| `IdleMaxGap` | 1m | `saturation` |
| `JoinedReplicaWindow` | 1m | `saturation` |
| `MinMeasuredOfDerived` | 0.5 | `saturation` |
| `MaxTokensOverCompletions` | 1.15 | `saturation` |
| `MeasuredLowConfirmFor` | 2m | `saturation` |
| `MeasuredDerivedMaxRatio` | 2.0 | `saturation` |
| `MeasuredDerivedReturnRatio` | 1.6 | `saturation` |
| `MeasuredMuFreshFor` | 5m | `saturation` |
| `MeasuredLowHoldFor` | 30m | `saturation` |
| `SiblingMuTolerance` | 0.8 | `saturation` |
| `FloorLagFraction` | 0.5 | `saturation` |
| `FloorTrustFraction` | 0.8 | `saturation` |
| `BacklogGrowthLookback` | 1m | `saturation` |
| `BacklogGrowthMinRequests` | 2 | `saturation` |
| `BacklogGrowthFraction` | 0.10 | `saturation` |
| `ThroughputSampleSpacing` | 1m | `saturation` |
| `DefaultExpectedOutputTokens` | 512 | `saturation` |
| `BytesPerToken` | 4 | `saturation` |
| `EpisodeGap` | 24h | `internal/signals/capacity` |
| `HistoryEvictionTimeout` | 24h | `internal/signals/capacity` |
| `HistoryRetention` | 7d | `internal/signals/capacity` |
| `EvictionTimeout` | 7d | `internal/signals/capacity` |

Most of the `saturation`, `itl` steady-gate and `balance` constants above are
**defaults of policy keys**, `analyzerTuning` and `loadImbalance`
(`internal/config/analyzer_tuning.go`; operator docs in
[Advanced tuning](../reference/scaling-policy.md#advanced-tuning-analyzertuning-and-loadimbalance)).
The `saturation` ones alias the `config.Default*` values, so they cannot drift.
The values in force are read once per cycle: `tuningOf(cfg)` (`saturation/tuning.go`)
fills the resolved policy's unset fields and is kept on `cycle.tune`, which
`computeReplicaCapacity` is handed; `applyThroughputFloor`, called once per
cycle, builds it from the `cfg` it is handed. The judgements take it as a
receiver or an argument, never from a package variable. The ITL gate reaches
`itl.Tracker.Observe` as `itl.Admission.Steady`, an `itl.Steadiness` mapped from
the keys by one constructor, `config.SteadyGateTuning.Steadiness`, in both the
saturation and the slo analyzer (`ScalingPolicy.ITLSteadiness`). The
diagnostic gets `balance.Thresholds` from `steadystate.loadBalanceThresholds`;
`Tracker.Measure` keeps them per model for `Observe` and `Failed`. Window-tied
values (`K2SettleLookback`, `K2SettleMaxGap`, `IdleSettledFor`, `IdleMaxGap`)
are multiples of `ThroughputSampleSpacing`, and `balance.StaleAfter` is the
model's `sustainFor`; none has a key of its own.
`TestTuningDefaultsAreTheCalibratedConstants` pins every default to the value
it had before it was configurable.

The first three are **defaults, not the values in force**. They are assembled into a
`capacity.Horizons` and handed to the analyzer by `WithHorizons`, which the engine
fills from `Config.LearnedStateHorizons()` — `LEARNED_STATE_EPISODE_GAP`,
`LEARNED_STATE_VARIANT_TIMEOUT`, `LEARNED_STATE_BUCKET_RETENTION`. `Horizons.Sanitized` floors each
field at `MinHorizon` (1m) independently and names the ones it replaced, so a single
unusable value does not discard the other two. A zero field takes its default without
being named, so an unconfigured controller logs no error. Zero is also what viper
returns for a value that was set and does not parse (`7d`) or was set to `0`; the
loader reports those itself (`learnedHorizon` in `internal/config/loader.go`), where
the value as written is still known. Read the pair in force off
`a.horizons`; `EvictStaleHistory()` takes no arguments, because a horizon passed per
sweep is a horizon no operator can reach.

### How long learned state lives

A sweep runs once per cycle from the engine, at the top, before any capacity is
computed. Three horizons govern it, and the differences are load-bearing —
getting any of them wrong silently changes replica counts.

**Per-variant state** — the fitted ITL window and its baseline, the replica
start estimate and its outlier counter — ages on `HistoryEvictionTimeout`
measured from when the variant was last *reported*, or last found to exist:
the engine stamps every active and every parked variant just before the sweep
(`notePresentVariants` → `NoteVariantsPresent`). It is a liveness stamp,
because an empty ITL window is not evidence that a variant is gone: a healthy
fleet below k=0.15 offers readings every cycle and holds none, so sweeping on
emptiness deleted the state of exactly the fleets that were doing fine.

**Bucket-keyed windows** — the k2 history and the mu windows the demand floor
prices from — are keyed by workload bucket as well as by variant, and a bucket
the variant has stopped serving must expire while the variant lives, so they
do not use that stamp. They are kept for `HistoryRetention` (7d) from
when they were last **read**: `Touch()` on the read path is what makes "last
read" the measure. Their only writer needs a saturated queue, so aged on last
*write* they would expire on exactly the fleets that are coping, and the figure
a decision depends on every cycle would vanish because the fleet was healthy.

**There is deliberately no trust horizon on a retained measurement.** However
old the figure is, while the window exists it answers. This follows from
`effectiveCapacity = min(k1, k2)`: a measured k2 can only ever pull capacity
*below* k1, so it is the most conservative figure available for its bucket, and
everything it could fall through to — the derived figure, or k1 itself — is
greater than or equal to it. Refusing a measurement can therefore only raise
capacity and order fewer replicas. An intermediate revision of this code added
a 24h trust horizon and fell through to k1; measured across k1 regimes, that
cost between 1.8× and 48× the replicas. Priority 1 replaces the figure on the
first saturated cycle regardless.

**Two timestamps, not one.** `RollingAverage` tracks `lastUsed` and
`lastWritten` separately, because three different questions were being asked of
one field:

| question | field | asked by |
| --- | --- | --- |
| keep this entry? | `lastUsed` | the sweep, via `StaleAt` |
| is a new observation part of this window, or a new episode? | `lastWritten` | the write paths, via `WriteGapExceedsAt` |

Neither is ever read against the wall clock. Every method takes `now` from its
caller, and the analyzer passes its own clock, which the engine pins once per
cycle (`internal/signals/cycleclock`): every stamp in one cycle is the same
instant, and a test that moves the analyzer's fake clock past `EpisodeGap`
sees the window reset. `TestNoWallClockInLearnedState` fails on a `time.Now`,
`time.Since` or `time.Until` in the capacity package or the saturation,
throughput and slo analyzers.

Collapsing them is a live defect: reads happen every cycle, so one field made
every read look like a write, and the write path's "a long gap means a new
episode, so reset rather than blend" check could never fire for any bucket
still being served. Two saturation episodes weeks apart then blended into one
rolling average — measured at 3.85× for k2 and 7.9× for mu, both in the
fewer-replicas direction.

**Two memos age on the *bucket* horizon despite being per variant**, because
each one's value resolves into a bucket key: `lastAccelerator` (into the k2
history key, the throughput key and the ITL window key) and `fleetShape` (into
the throughput key). Expiring either ahead of the window it keys moves the key
to `unresolved` or to a different bucket and makes a retained measurement
unreachable — the measurement is still in the map, and nothing will ever ask for
it again. Both are also stamped when they are **read**, not only when written,
for the same reason the windows are: the read path is the one that runs every
cycle. Those two are the whole set; a new memo whose value reaches a bucket key
has to join them.

**Capacity records** age on `EvictionTimeout` (7d), so a variant parked at zero
over a weekend keeps its engine params and capacity record. The store has no
trust horizon on reads either; `Store.IsStale` and `StalenessTimeout` (30 min)
exist but have no caller on the reconcile path.

Net: at the defaults, a variant that is *deleted* loses its fitted per-variant
figures 24h later. One that is merely parked keeps them, because
`notePresentVariants` stamps every variant that exists just before the sweep,
including on the cycle a long-parked fleet wakes. Seven days without a read costs the bucket's measurement, parked or
not, and seven days costs the capacity record.

**The horizons tick on cycles, not on wall clock.** The sweep is called from
`optimizeV2`, and `optimize` returns early on "No active VariantAutoscalings
found" — so a fleet with *every* variant parked stops sweeping until something
wakes. A variant's state therefore ages out only while some variant is still
live, which is the normal case. This is not a second leak: a parked fleet
learns nothing new, so what it holds is bounded by what it had when it parked.
It does mean that a test which parks the whole fleet and waits measures
nothing — park one variant and leave another serving.

## Reading a decision from the logs

Three lines carry the whole chain. `derived-mu` and `replica-capacity-decision`
are written once per replica, so they sit at `perReplicaLogLevel`
(`logging.VERBOSE`, `--v=3`; `replica_log.go`); at the shipped `logging.DEFAULT`
the per-variant `replica-capacity-summary` stands in for them.
`throughput-demand-floor` is at Info:

- `derived-mu` — every term of the mu derivation, because a mu wrong by 8x
  looks identical in the log whether the fault is the output length, the
  sequence count, the ITL line or the pricing point. `kvReqPerSeq` is the
  priced shape and `kvReqFleet` the fleet's own; `muShortWindow` says whether
  the pair moved to the short window.
  It, and every ITL line (`itl-window`, `itl-fit`, `itl-fit-declined`,
  `itl-line-borrowed`, `itl-line-none`, `itl-gps-mismatch`), carries `namespace` beside
  `variant`: one Deployment applied to two namespaces logs the same variant
  name in both, and only the namespace tells their lines apart.
- `replica-capacity-decision` — `k1MemoryBound`, `k2ComputeBound`, `k2Source`,
  `boundBy`, `saturatedThroughput`, `saturatedThroughputBucket`.
- `throughput-demand-floor` — `arrivalRate`, `backlogRequests`,
  `saturatedThroughput`, `perReplicaCapacity`, `replicasImplied`,
  `heldAtFleet`, `heldWhy`.
