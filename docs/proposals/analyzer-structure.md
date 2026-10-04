# Structuring the analyzers

**Status:** proposed, nothing built. No behaviour change is intended by any
stage below; a stage that changes a decision has a bug in it.

The analyzers work. The problem is that reading them is expensive, and that
cost is now showing up as real defects: three doc comments silently attached to
the wrong function on one branch, a NaN hole in a shared helper that the
helper's own two callers each guarded against separately, and the same
request-rate-weighted mean written three times in two packages, with a comment
admitting the third.

This proposes what to change, in stages that each land on their own.

## What is actually wrong, measured

Taken at `00c105f4`, non-test Go only:

| file | lines | code | comment | top-level funcs |
| --- | --- | --- | --- | --- |
| `saturation_v2/analyzer.go` | 2441 | 1048 | 1283 (52%) | **35** |
| `steadystate/engine.go` | 2214 | 1264 | 761 (34%) | 43 |
| `steadystate/engine_v2.go` | 1365 | 755 | 534 (39%) | 35 |
| `saturation_v2/shape_change.go` | 624 | 228 | 378 (60%) | 11 |
| `signals/floor/floor.go` | 638 | 234 | 385 (60%) | — |
| `saturation_v2/throughput_floor.go` | 527 | 225 | 283 (53%) | 9 |

Four distinct problems, which want four different fixes:

**1. Two files are grab-bags.** `analyzer.go` holds 35 unrelated top-level
functions: the analyzer's lifecycle, the whole `Analyze` pipeline, per-replica
capacity, k2 estimation, key construction, variant aggregation, fleet-shape
averaging, scheduler-queue pricing, role predicates, and `median`. Nothing
tells you which of 2441 lines to open. `steadystate/engine.go` is the same
shape at 43 functions.

**2. One function is an outlier, and only one.** `Analyze` is 155 code lines
(406 counting comments). Worth stating clearly because it is the opposite of
what a size audit usually finds: of **550 non-test functions in the engine,
signals and collector packages, 6 exceed 60 code lines and exactly 1 exceeds
100**. The codebase does not have a long-function problem. It has one long
function, in the place that most needs to be skimmable.

**3. The same computation exists three times.** A request-rate-weighted mean
with a fallback to the plain mean: `fleetAverage` and `fleetPrefixHitRate` in
`saturation_v2`, and `averageShapeMetrics` in `analyzers/throughput`.
`fleetAverage`'s own comment says so — *"The throughput analyzer's
averageShapeMetrics is a third instance of it in another package, left alone
here."* The three have already diverged: only `fleetPrefixHitRate` treats a
measured `0` as a reading, and until recently only it rejected NaN.

**4. `saturation_v2` is a misleading name.** There is no `saturation` and no
`saturation_v1` — `internal/engines/analyzers/` contains `external/`,
`saturation_v2/` and `throughput/`. The `_v2` is vestigial, and the name breaks
the project's own stated convention twice over: package names should be "short,
lowercase, single-word", and this one carries an underscore and a version.

## What is NOT wrong, and must survive

**The comment volume is not the defect.** It is tempting to read "52% comment"
as bloat and delete. That would destroy the most valuable thing in the
repository. These comments carry *measurements* — which run, which figures,
what was tried and refuted — and they are the reason a wrong decision can be
diagnosed at all. The `withExpectedOutputTokens` comment carries the bisect
across five runs that chose its gate; the `AvgOutputTokensRecent` comment
carries the `3750 -> 2814 -> 2382 -> 250` decay that justifies a second
window; `fleetAverage`'s comment is what flagged the duplication above.

The problem is **placement**, not existence. A 30-line run narrative sitting
between two statements means the algorithm cannot be read in one screen. So:

> Move the narrative, keep the reason. A comment that says *why this is the
> way it is* stays next to the code. A comment that recounts *which run
> measured it and what the numbers were* moves to
> `docs/developer-guide/analyzer-evidence.md`, which exists for exactly this,
> and leaves a one-line pointer behind.

The test for whether a comment moves is not its length. It is whether someone
changing the line needs it, or whether someone doubting the line needs it. The
first stays.

## Proposal

### Stage 1 — split the grab-bags (mechanical, no behaviour change)

Split `saturation_v2/analyzer.go`'s 35 functions into files named after what
they compute. The grouping falls out of the existing code with no logic moved:

| new file | functions |
| --- | --- |
| `analyzer.go` | `NewSaturationAnalyzer`, `Name`, `EvictStaleHistory`, `rememberDecodeSaturation`, `engineParamsFor`, `itlWindowKey` |
| `analyze.go` | `Analyze` and its new stage functions (stage 2) |
| `replica_capacity.go` | `computeReplicaCapacity`, `computeReplicaCapacityFallback`, `useDerived`, `computeK2`, `estimateStoredCapacity`, `estimateCapacityFromParams`, `lookupCompatibleCapacity`, `memoryBound`, `k2SourceLabel` |
| `keys.go` | `throughputKey`, `historyKey` |
| `aggregate.go` | `aggregateByVariant`, `aggregateRoleDemand` |
| `fleet_shape.go` | `computeModelWorkloadAverages`, `fleetAverage`, `fleetOutputLength`, `fleetOutputLengthRecent`, `fleetPrefixHitRate`, `withExpectedOutputTokens` |
| `queue_demand.go` | `estimateSchedulerQueueDemand`, `waitingQueueDemand`, `holdPrefillDemand` |
| `roles.go` | `rolesFromStates`, `generatesOutput`, `canonicalRole`, `roleSaturated`, `stableAccelerator` |

No file over ~500 lines. `median` goes to stage 3.

Do the same to `steadystate/engine.go` as a separate change; it is a bigger job
and does not block this one.

**Verification:** `git diff --stat` shows only moves. Spec counts identical
(saturation_v2 279, allocation 272/273, config 166). `go build`, `go vet`,
`golangci-lint` clean. This is the stage where a reviewer should be able to
confirm "nothing changed" quickly, so it must not carry any other edit.

### Stage 2 — give `Analyze` named stages

`Analyze`'s 155 code lines are already a pipeline; it just has no names. Extract
the stages it already has, each returning a value the next consumes:

```go
func (a *SaturationAnalyzer) Analyze(ctx, input) (*Result, error) {
    fleet   := a.observeFleetShape(input)        // averages, hit rate, tracker
    pricing := a.resolvePricing(input, fleet)    // muDivisor, muInput, kPrice, ITL models
    caps    := a.priceReplicas(input, fleet, pricing)
    demand  := a.priceDemand(input, fleet, caps)
    return a.applyFloor(input, caps, demand)
}
```

The names are the point: `resolvePricing` is where the shape-window pairing
lives, and a reader looking for "why did mu collapse" should not have to find
it inside a 400-line function. Keep the `derived-mu`,
`replica-capacity-decision` and `throughput-demand-floor` log lines exactly
where they are — they are the debugging contract and
`analyzer_log_test.go` asserts them.

**Verification:** the log-contract tests already pin the fields. Add a golden
test that runs one `Analyze` over a fixed input and asserts the full emitted
field set, so a stage boundary that drops a field fails loudly.

### Stage 3 — one weighted mean, in `internal/signals`

Collapse the three implementations into one, in a package both analyzers
already import. The signature has to admit the difference that made them
diverge:

```go
// package signals/fleet
func WeightedMean(replicas []domain.ReplicaMetrics,
    value func(domain.ReplicaMetrics) float64,
    include func(domain.ReplicaMetrics) bool,
    opts ...Option) float64

// ZeroIsAReading: 0 is a measurement, not an absent value. Required for a
// prefix-cache hit rate; wrong for a token length.
```

This is the one stage with a real correctness payoff: the NaN hole fixed in
`08d8ad6c` existed in `fleetAverage` only, and `averageShapeMetrics` in the
throughput analyzer has not been audited for it. One implementation is one
place to get NaN right.

**Verification:** each call site keeps its current behaviour, proven by a
negative control per site — flip the option and the site's own test must fail.

### Stage 4 — rename `saturation_v2` to `saturation`

A pure rename, no `v1` to collide with. Touches every importer, so it lands
alone and last among the mechanical stages, when the diff is otherwise quiet.
`gofmt`-driven; the risk is entirely in merge conflicts with in-flight
branches, which argues for doing it immediately after a merge window rather
than before one.

### Stage 5 — move the run narratives

Per file, move measurement accounts into `analyzer-evidence.md` under a heading
that names the run, leaving the reason and a pointer:

```go
// The divisor follows the short window only while a shape change is
// outstanding: on a ramping fleet the short window reads below the length
// being served, which over-states mu and under-orders.
// Measured: analyzer-evidence.md#the-mu-divisor-window
```

Target: the analyzer files land under about 35% comment, with nothing of value
lost — `analyzer-evidence.md` is already 651 lines and is the right home.

This is last because it is the only stage that requires judgement per comment,
and because it is worthless until the files are small enough to see the effect.

### Stage 6 — a shared analyzer skeleton (optional, decide later)

Both analyzers independently implement: average the fleet, resolve a shape,
price per replica, aggregate by variant and role, hand back a
`NamedAnalyzerResult`. A shared skeleton would stop a third analyzer
re-deriving it.

Deliberately **last and optional**. There are two analyzers, and two instances
is thin evidence for an abstraction; a wrong skeleton is worse than a
duplicated pipeline because it makes the next analyzer fight it. Revisit when a
third exists, or drop it. `multi-analyzer-pipeline.md` already covers the
contract, which may be all the sharing that is wanted.

## What this costs

- **`git blame` on the moved lines** points at the move. Mitigated by keeping
  stages 1 and 4 pure, so `--follow` and `-C` work; not eliminated.
- **In-flight branches conflict.** Stage 1 and stage 4 touch every line's
  location in the two biggest files. Both want a quiet merge window.
- **Review burden.** A 2441-line file split reviews as a large diff even though
  nothing changed. The commit message has to say what to check — "only moves" —
  and the verification has to be mechanical enough to trust.
- **Stage 5 is unbounded** if allowed to be. Cap it: one file per change, and
  the mechanical stages must be done first.

## Order, and what is independent

```
stage 1 (split)  ->  stage 2 (stages)  ->  stage 5 (narratives)
stage 3 (one mean)   independent, do any time
stage 4 (rename)     after a merge window, alone
stage 6 (skeleton)   decide later, or never
```

Stage 3 is the only one with a correctness payoff and the smallest diff, so it
is the best first change if only one gets done.

## Out of scope

- The `internal/signals/*` packages. They are small, single-purpose and read
  well — `shape` is 167 lines, `itl` 437, `aggregation` 206. The structure
  being proposed here is roughly what they already are.
- Anything that changes a scaling decision. If a stage moves a number, it is
  wrong; revert and split the change.
- The comment *style*. The density is deliberate and it is why this system is
  debuggable. Only placement is in question.
- Test files. They have the same grab-bag tendency, and the same remedy, but
  production code first — the tests are what prove the moves are safe.
