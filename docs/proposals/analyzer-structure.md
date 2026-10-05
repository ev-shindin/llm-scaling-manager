# Structuring the analyzers

**Status:** stages 1-4 built, stage 5 concluded, stage 6 dropped. No stage
changed a decision; every one was verified against unchanged spec counts.

| stage | | |
| --- | --- | --- |
| 3 — one weighted mean | **built** | `internal/signals/fleet.Mean`, three copies collapsed |
| 1 — split the grab-bag | **built** | `analyzer.go` 2380 -> 259 lines, eight files |
| 2 — named stages | **built** | `Analyze` is fifteen lines over eight named stages |
| 4 — rename the package | **built** | `saturation_v2` -> `saturation` |
| 5 — move the run narratives | **concluded, mostly declined** | see below; the 35% target was wrong |
| 6 — shared analyzer skeleton | **dropped** | see below |

`steadystate/engine.go` (2214 lines, 43 declarations) was NOT split. It is the
same fault as stage 1 and wants the same treatment, as its own change.

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
| `saturation/analyzer.go` | 2441 | 1048 | 1283 (52%) | **35** |
| `steadystate/engine.go` | 2214 | 1264 | 761 (34%) | 43 |
| `steadystate/engine_v2.go` | 1365 | 755 | 534 (39%) | 35 |
| `saturation/shape_change.go` | 624 | 228 | 378 (60%) | 11 |
| `signals/floor/floor.go` | 638 | 234 | 385 (60%) | — |
| `saturation/throughput_floor.go` | 527 | 225 | 283 (53%) | 9 |

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
`saturation`, and `averageShapeMetrics` in `analyzers/throughput`.
`fleetAverage`'s own comment says so — *"The throughput analyzer's
averageShapeMetrics is a third instance of it in another package, left alone
here."* The three have already diverged: only `fleetPrefixHitRate` treats a
measured `0` as a reading, and until recently only it rejected NaN.

**4. `saturation` is a misleading name.** There is no `saturation` and no
`saturation_v1` — `internal/engines/analyzers/` contains `external/`,
`saturation/` and `throughput/`. The `_v2` is vestigial, and the name breaks
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

Split `saturation/analyzer.go`'s 35 functions into files named after what
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
(saturation 279, allocation 272/273, config 166). `go build`, `go vet`,
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

### Stage 4 — rename `saturation` to `saturation`

A pure rename, no `v1` to collide with. Touches every importer, so it lands
alone and last among the mechanical stages, when the diff is otherwise quiet.
`gofmt`-driven; the risk is entirely in merge conflicts with in-flight
branches, which argues for doing it immediately after a merge window rather
than before one.

### Stage 5 — move the run narratives (concluded: mostly declined)

The idea was that the 52-72% comment ratio was largely *narrative* that could
be relocated to `analyzer-evidence.md`, leaving the reason and a pointer, and
that the analyzer files would land near 35%.

**Measured, and the premise was mostly wrong.** One block was a genuine
narrative appendix: `resolvePricing`'s account of runs QM and QS and the
five-run bisect, which recounted the evidence *after* the rule had already been
stated. It moved cleanly — 82 comment lines out of the code, every figure
preserved in `analyzer-evidence.md` under two new headings — and bought
`analyze.go` four percentage points, 49% -> 45%.

The rest does not separate, and the two clearest cases say why:

- `shape_change.go`'s table of *which signal first reads the new shape* (+22 s,
  +1.5 min, +1.6 min and wrong) **is** the argument for the arriving-prompt
  signal existing. Move it and what remains is "this is the earliest signal",
  asserted.
- `mu_from_itl.go`'s "it reproduces a measurement it was never given, which is
  the reason to believe it: 1.487 where the fleet measured 1.4292 and 1.5429"
  **is** the justification for deriving mu at all.

These are reasoning *carried in* measurements, not narrative decorating it. The
test this proposal set — does someone CHANGING the line need it, or only
someone doubting it — answers "changing" for both. Relocating them would
convert justified claims into assertions, which is the failure this document
warned against two sections earlier.

So the scope narrows to its real extent: **a block that recounts a run after
the rule is already stated**, which was one block. The 35% target is withdrawn;
45-50% is what these files are when every load-bearing comment stays, and that
is not a defect to fix.

What remains available, and deliberately not done: `analyzer-evidence.md` is
now 740 lines and could take the measurement tables out of `shape_change.go`
and `mu_from_itl.go` **by reference** — the code keeping the figure and the doc
carrying the full run, so neither side is an assertion. That is duplication
rather than relocation, and it needs someone to decide whether two copies of a
number is better or worse than one in the wrong place.

### Stage 6 — a shared analyzer skeleton (dropped)

Both analyzers independently average the fleet, resolve a shape, price per
replica, aggregate by variant and role, and hand back a `NamedAnalyzerResult`.
A shared skeleton would stop a third re-deriving it.

**Dropped, not deferred.** The argument against it has not changed and has
gained a data point: stage 3 showed what the two analyzers actually share —
one weighted mean — and extracting precisely that, with the callers'
disagreement expressed as two options, was the whole of the real duplication.
What is left is a sequence of five calls in a shared order, which is a
*convention* and is now readable as one in each analyzer; a skeleton would make
the next analyzer fight it to differ anywhere.

`multi-analyzer-pipeline.md` already specifies the contract an analyzer must
meet, which is the sharing that has value. Revisit only if a third analyzer
appears and genuinely wants this shape; the decision recorded here is that two
instances were not evidence enough.

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
