# Learned state across a restart, and what it should be keyed by

**Status:** steps 1-5 built (PR #120); 6, 7 and 8 proposed. Revised **nine
times** against code and measurement — that is the count in this file's own
history, and an earlier header said five, which matched neither the history nor
the corrections below. **Six** of those revisions corrected a stated fact and
three of the six reversed a conclusion; those six are the ones recorded in
[Appendix C](#appendix-c--what-this-document-got-wrong), and the designs that
were tried and rejected are in Appendices A and B rather than inline, so
everything before the appendices describes what is built or proposed now.

The analyzer learns three things and keys them badly. This fixes the keying,
and then carries the one figure worth carrying across a restart — not by
storing it, but by refitting it from the engines' own measurements.

This **revises [`signals-as-metrics.md`](signals-as-metrics.md)**, which said
publishing these signals was observability only and that a restart gap was
"honest rather than fixable". The first half stands, and step 6 is that
document's work. The second was wrong — but not in the way an earlier revision
of *this* document claimed: the fix is neither reading WVA's own metrics back
nor storing a snapshot, it is refitting from the **engines'** series. The
sibling was corrected on this branch four commits before this sentence was
written, and already carries its own REVISED block; what was left in the old
vocabulary is one bullet, fixed here.

## The problem, measured

Nothing the analyzer learns survives a restart. Confirmed rather than assumed:
there is no persistence anywhere in `internal/signals/capacity` or
`internal/engines/analyzers/saturation` — `capacity.Store` is a plain
`map[string]*Record` behind a mutex, and every window is in memory.

### What a cold start costs, measured from load start

Run QT, the decode variant, 244 `itl-window` cycles at a uniform 15 s. The
harness starts at 14:52:38Z (`LLMDBENCH_HARNESS_START`), and **every figure
below is relative to that**, not to the controller's first cycle:

| | |
| --- | --- |
| first `(k, ITL)` observation held | **load + 22 s** |
| first OLS fit, and first `throughput-demand-floor` | **load + 7.4 min** |
| cycles between the two | **28, every one blocked on the sample count** |
| cycles blocked on the `k` spread before the first fit | **0** |

So the relearning cost is **28 cycles, about 7 minutes**. The window reports
the sample count as the blocker, but the sample count is not the *mechanism*,
and an earlier revision of this line said it was ("ten pairs at roughly one per
replica per cycle"). Measured over those 28 cycles:

| | run QT, decode |
| --- | --- |
| pairs OFFERED to the window | **38** |
| pairs the window HELD | **10** |
| cycles offering 2 / 1 / 0 | 15 / 8 / 5 |
| `Prune` drops in the interval | 0 (7 min against a 30-min bound) |
| cycles where `maxSize` bound | 0 (held never exceeded 10) |

Arrival was ~1.36 pairs a cycle and **admission 0.36**. The 28 missing pairs
were discarded inside `Window.Add` by the low-`k` floor,
`k < itl.DefaultMinObservableK` (0.15) — the fleet was ramping and most
replicas had not yet filled enough KV to be observable. At one admitted pair
per replica per cycle the first fit would have landed in ten cycles, not 28.

**Two things follow.** The cold-start cost is an *admission* cost, not an
arrival cost, so raising `DefaultWindowMaxSize` (step 7) does not touch it —
step 7 is a steady-state change and its own success criterion says so. And the
0.15 floor lives in `Window.Add`, not in `itl.Fit`, so step 8's "the same
`itl.Fit`" does **not** inherit it: a refit reading a range query directly
would admit points the live path rejects, and has to apply the band itself.

The classification over the whole run, with the idle period separated out
because it is not a cost:

| state of the window | cycles | note |
| --- | --- | --- |
| empty, **before any traffic existed** | 34 | idle; nothing to learn from, nothing to save |
| empty, after load started | 1 | the engine had not yet reported an ITL |
| blocked on the sample count | 28 | **this is the relearning cost** |
| blocked on the `k` spread | 127 | steady state, not cold start — see below |
| ready, a fit available | 54 | |

A rollout restarts the controller; so does a node drain, an OOM, and a leader
handoff. **Leader election is enabled in every shipped install** —
`config/base/manager/deployment.yaml` passes `--leader-elect=true` and no
overlay removes it — so a handoff is routine, not exotic. The Go flag's own
default is `false`, which is why an earlier revision of this document called
election churn rare; that was wrong.

### Scope

One decode variant, one run. The prefill variant never logged an
`itl-window` line, so "244 cycles" is one sample and not a population. That
matters most for the k-spread finding below, which is the one being promoted
as valuable.

## Part 1 — what it is keyed by

### What the keys are today

| state | key | variant name in it? | engine config in it? |
| --- | --- | --- | --- |
| k2 history (`historyKey`) | model, accelerator, gpus, role, output bucket, **queue threshold** | **no** | **no** |
| throughput window (`throughputKey`) | the same + input bucket | **no** | **no** |
| ITL window (`itlWindowKey`) | namespace, model, **variant**, accelerator, gpus | **yes** | **no** |
| `capacity.Store` record | namespace, model, **variant** | **yes** | — (it *holds* the params) |
| shape tracker | namespace, model | no | no |
| accelerator memo | namespace, **variant** | yes | no |

`throughputKey` and `historyKey` take `variantName` and do not put it in the
key — it only resolves `stableAccelerator`. So those two are already keyed on
hardware and traffic shape rather than identity. The ITL window and the
capacity store are not.

**Neither includes the engine configuration.** That is the defect, and it cuts
both ways:

- Two variants of one model on the same accelerator and GPU count with
  **different `--max-num-batched-tokens`** share a k2 history window. Their
  capacity genuinely differs; the window averages them together.
- One variant **renamed**, or a second identical variant added, gets a fresh
  ITL window and pays the 28 cycles again — though `ITL(k)` is a property of
  the weights, the engine build and the hardware, not of a name.

### The fingerprint

`capacity.EngineParams.IsCapacityCompatible` is close to the equality this
wants, and was used only as a *fallback scan* in `Store.FindCompatible`. As
built:

```
engineFingerprint = hash(
    fingerprintVersion,
    weightDtype, quantization,     // added to the parser; see below
    Engine, GpuMemoryUtilization, BlockSize, KvCacheDtype,
    TensorParallelSize, NumGpuBlocksOverride, TotalKvTokensOverride,
    EffectiveMaxBatchedTokens,
    MaxNumSeqs, MaxModelLen,
    EnforceEager,
)

learnedStateKey = (modelID, acceleratorName, gpusPerReplica, engineFingerprint)
```

`EnforceEager` **is** hashed. An earlier revision said it needed a measurement
first; it does not, and the asymmetry is deliberate. No CUDA graphs does not
change how much KV fits, so a capacity record survives it — but it does change
the inter-token latency, and this digest also keys a latency model. Including
it costs only that two engines differing in `--enforce-eager` each learn their
own line; excluding it would pool them onto one line describing neither. The
safe direction needs no measurement.

Three `EngineParams` fields are excluded on one rule: `MaxNumBatchedTokens`,
`IsV1Engine` and `ChunkedPrefillEnabled` exist only to resolve
`EffectiveMaxBatchedTokens`, which is hashed. Hashing an input beside the value
it produces splits a key on a distinction the engine has already collapsed.

### The hash covers the flags; the axes people query by stay labels

`modelID`, `acceleratorName` and `gpusPerReplica` are **part of the key and
not part of the hash**. The key is the tuple above, composed in one function.

- **Directly queryable.** `wva_engine_config{model="..."}` answers "what is
  this model running" with no join, and `{accelerator="H200"}` answers the
  comparison an operator actually makes.
- **No cardinality cost.** The number of distinct
  `(model, accelerator, gpus, fingerprint)` combinations is the same either
  way.

What this must not become is "the fingerprint alone identifies the line" —
that would pool a 0.6B and a 32B model. The key is the tuple, and the
discipline lives in the key function.

### `EngineParams` says nothing about the weights

`EngineParams` holds `Engine`, `GpuMemoryUtilization`, `BlockSize`,
`KvCacheDtype`, `TensorParallelSize`, `NumGpuBlocksOverride`,
`MaxNumBatchedTokens`, `MaxNumSeqs`, `MaxModelLen`, `EnforceEager`,
`IsV1Engine`, `ChunkedPrefillEnabled`, `TotalKvTokensOverride` and
`EffectiveMaxBatchedTokens` — and **nothing identifying the weights**.
`KvCacheDtype` is the KV cache's dtype; `MaxModelLen` a context limit.

A fingerprint from it alone would give a 0.6B and a 32B model one identity
whenever their flags and hardware matched, and ITL's slope is dominated by
parameter count and weight dtype. So `modelID` is in the key, and the parser
learned `--dtype` and `--quantization`, without which an FP8 and a BF16
serving of one model collide.

### Two defects this exposed, both fixed and both worth fixing alone

**`IsCapacityCompatible` compared neither `MaxNumSeqs` nor `MaxModelLen`.** `S`
caps `N_steady` in k2
(`internal/engines/analyzers/saturation/replica_capacity.go:718` — there are
two files of that name, and the one in `internal/signals/capacity` has no
`MaxNumSeqs` at all) and `seqs` in the derived mu
(`mu_from_itl.go:218`). `MaxModelLen` changes KV per sequence, and reaches
`EffectiveMaxBatchedTokens` only when chunked prefill is off — so on the V1
path, where the resolver returns a flat 8192, it was invisible to the predicate
entirely.

**`EvictStaleHistory` and `Store.EvictStale` had no production caller.** So the
ITL windows, the learned baseline, the start estimate, the accelerator memo and
the capacity records were never swept, and the "one window per variant that has
EVER been seen" leak the code's own comment warns about was live.

### What does NOT move

`capacity.Store` is **not re-keyed**: the fingerprint comes from
`Record.EngineParams`, which you only have after looking up by name, so a
fingerprint key needs an extra index; of its eight non-test callers — two
added by this work, `engineFingerprint` and `publishEngineConfig` — most
genuinely ask "what did *this variant* measure"; and
`Record.EffectiveCapacity` is `min(k1, k2)`,
which is shape-dependent.

**`queueThreshold` stays in the k2 and throughput keys.** Both end in `q%g`,
with a measurement in the comment: a k2 of 2 learned under a low threshold kept
a variant at utilization 1.0 under a threshold of 100. It is the only
invalidator for a policy retune.

**The accelerator memo stays, and feeds the fingerprint.** It exists because
the observed accelerator changes with pod placement and can read unresolved;
the fingerprint takes `acceleratorName` as an input, so an unresolved
accelerator would make the digest itself oscillate.

Two rows are marked **proposed**, and the distinction matters for step 6: as
built, `historyKey` is `modelID|accelerator|gpus|role|outBucket|q` and
`throughputKey` is `modelID|accelerator|gpus|role|i<in>|<out>|q`. Neither
contains the fingerprint — step 5 re-keyed the ITL line and nothing else. So a
`wva_learned_k2_tokens{…,fingerprint}` published today would split one
window's value across two series the moment a configuration changed, which is
the opposite of what the label is for. **Publishing a fingerprint label on k2
or throughput requires the re-key first.**

| state | keyed by | why not more |
| --- | --- | --- |
| ITL **window** | unchanged: namespace, model, variant, accelerator, gpus | the observations are this variant's own |
| ITL **line** (A, B), shared | model + accelerator + gpus + fingerprint | the same weights, build and hardware give **nearly** the same line — near enough to start from, not near enough to order on |
| k2 | **proposed:** fingerprint + role + output bucket + **queue threshold** | capacity is config **and** shape |
| throughput window (mu) | **proposed:** fingerprint + role + input and output buckets + **queue threshold** | a completion rate is per shape |
| `capacity.Store` record | **unchanged** | circular, and `EffectiveCapacity` is shape-dependent |
| `itlBaseline`, `startSeconds` | **unchanged** | a hardware floor and a start time are not functions of the config |
| stable shape | namespace + model | traffic, not configuration |
| accelerator memo | **unchanged**, and it feeds the fingerprint | the accelerator reading is unstable |

## Part 2 — sharing the fitted line (built)

The question that settled the mechanism: **what does a decision actually
read?**

**Only `A` and `B`.** One call site uses the ITL model —
`model.ITLAt(kPrice)` in `deriveMu` — plus `model.B` into `itlBaseline`.
Everything else that touches either is a log field. The observations exist only
to produce those two numbers and to gate their trust (`Ready()`, `Len()`,
`KSpread()`).

So the window is not shared. The **line** is:

- Each variant fits its own window, keyed exactly as before.
- A variant's own fit is published as the line for its engine configuration.
- A variant with no usable fit **borrows** that line, and the borrow is marked
  to the floor, which may **hold** the fleet on it and must not **grow** one.

That makes a rename, a second identical variant, and the same deployment in
another namespace free — the 28 cycles, on their first cycle instead.

### Why a borrowed line may not grow a fleet

`itlPhysicsKey` asserts ITL(k) is a function of the model, the hardware and the
flags. Not quite: at a fixed `k` the resident sequence count is `k·C/KVreq`,
and `KVreq` is the **traffic shape**. Two variants on one configuration serving
different shapes sit at different batch sizes at the same `k`, so on different
lines. (How far apart two real shapes sit is not measured here; the
1k/6000 → 8k/1000 swap this repository benchmarks moves `KVreq` by roughly 2x,
which is an order-of-magnitude statement and not a figure to rely on.) A
borrowed line is evidence about a *configuration*, not about this variant's
load.

**Gating this correctly took two attempts, and the first did nothing.** It is
documented here because the failure is easy to repeat: the gate was written as
`!LineBorrowed` added to one of two disjuncts, and the analyzer stamps
`MinDerivedThroughputSamples` — equal to `MinThroughputSamplesToOrder` by
construction — onto every derived figure, so the sample half re-admitted
exactly what the derived half excluded. Separately,
`fleetHasMeasuredItself` settled the shape-change hold on sight of any derived
figure, clearing the `staleShape` that was the only other brake, in the same
cycle. Measured by running the fixed and unfixed trees over one cycle of the
same input: the floor published **0.850 replicas** (790,323 tokens) with the
gate working and **13.433** (12,489,743 tokens) without it — **15.8x** past the
intended cap.

The two halves were fixed two different ways, and an earlier version of this
section reported only one of them.

**The floor's half: routing, not annotating.** A borrowed-line figure takes the
branch `floor.Estimate` already has for a reading borrowed from a neighbouring
shape bucket, which caps it, names it `borrowed-line`, and returns before
`mayOrder` is reached. Annotating the disjunction was what failed.

**The hold's half: annotating, in a conjunction.** `fleetHasMeasuredItself`
takes the same `!LineBorrowed` term that did nothing in the floor's
disjunction and it works there, because every term of a conjunction binds.

So the lesson is about **disjunctions**, not about annotation: a term added to
one of two disjuncts gates nothing, while the same term in a conjunction gates
everything. Stated as "routing, not annotating" it was over-general, and it
contradicted the fix sitting in the other file.

## Part 3 — carrying it across a restart: there is no store

If the fingerprint matches, it is by construction the same engine build on the
same hardware serving the same model. Prometheus already holds that engine's
measurements from before the restart — **the same two PromQL expressions the
live fit consumes every cycle**:

- `QueryKvUsageInstant` — a gauge,
  `max by (model_name, instance, pod)(vllm:kv_cache_usage_perc{…})`
- `QueryAvgITL` — *not* a single series:
  `max by (…)(rate(vllm:inter_token_latency_seconds_sum[1m]) / rate(…_count[1m]))`,
  a ratio of two rates over two histogram counters

Both land in `domain.ReplicaMetrics` straight from those queries and are added
as the pair `w.Add(rm.KvUsageInstant, rm.AvgITL, now)`, joined per pod. A
`query_range` over the same expressions reconstructs the line from the same
real observations, so a stored copy adds nothing its source does not have.

Every objection that killed reading WVA's **own** metrics back was about WVA
reading what it published about itself: no writer identity behind the
ServiceMonitor's `labeldrop`, an apparent age that resets on every republish,
an unauthenticated input to a scaling decision, a feedback loop. None of those
apply to the engines' series, which is an independent measurement already
trusted on the live path.

So this drops the ConfigMap, a `schemaVersion`, an RBAC increase, a migration
story, a flush story, and the problem that a poisoned figure would outlive the
restart that used to clear it. The reasoning, and the scoring against a
write-on-change ConfigMap, is in
[Appendix A](#appendix-a--rejected-storing-a-snapshot).

**What it costs, stated rather than implied.** There is no range-query path in
the tree today: `internal/prometheus/api.go` only calls
`promAPI.Query(ctx, q, time.Now())`, `QueryRange` appears nowhere outside
tests, and `parseMatrix` keeps only the newest sample per series. Step 8 needs
a matrix consumer that retains more than the last point. "A range query
reconstructs the line" is a mechanism, not a small change.

**What it does not cover.** k2-observed depends on WVA's own judgement that a
replica was saturated, not on a raw engine series. The replica start estimate
is not a function of the engine configuration and is not carried. And where
Prometheus does not retain the window, the refit finds nothing and the start is
cold — the honest failure mode, and indistinguishable in the log from "nothing
to find", which is the weakest point of this design.

`PROMETHEUS_BASE_URL` is already mandatory: `internal/config/loader.go` errors
without it and `cmd/main.go` exits 1, with a second check later and an exit on
an unreachable Prometheus. So the refit adds no dependency. It does depend on
the Prometheus Operator CRD being present for the ServiceMonitor, which
`config/base/kustomization.yaml` ships by default — the CRD, not the manifest,
is the external requirement.

## The second finding, and it is not about restarts

**52% of all 244 cycles** could not fit an OLS line for want of `k` spread and
fell back to pinned-B — 60% of the 210 post-load ones. Both bases appear in
this document and they answer different questions: the all-cycles figure is
what a run's log shows, the post-load figure is what the fix is measured
against, and the success criterion below uses the post-load one. That is
steady state either way. The cause is measured, and it is not the age bound:

| | run QT, decode |
| --- | --- |
| cycles at `held` = `DefaultWindowMaxSize` (20) | 129 of 244 (53%) |
| median observations offered per cycle | 2 |
| wall-clock a full 20-slot window spans | ~10 cycles, **~2.5 min** |
| `DefaultObservationMaxAge` it is allowed | 30 min |
| share of its own age bound actually used | **~8%** |
| of the 129 FULL windows, how many cleared the spread | **4 (3%)** |

The window is bounded by its **size**, not its age: a full window looks through
a two-and-a-half-minute keyhole and fails the spread gate 97% of the time. At
two observations per 15 s cycle, reaching the 30-minute bound needs about
**240 slots**, not 20.

So the cheapest fix is one constant, and it is a different lever from the
refit: raising `DefaultWindowMaxSize` addresses the 52%, while the range query
addresses the cold start where there is no live data to widen.

**Not claimed:** that a wider window actually spans 0.30 of `k`. If a balanced
router pins every replica to one `k` for half an hour, neither helps. The raw
pairs are not logged, so this needs a range query against a live fleet, and the
evidence above is structural — the window is far narrower than intended, not
that widening it clears the gate.

## Order

1. **`MaxNumSeqs` and `MaxModelLen` into `IsCapacityCompatible`**, with tests.
   Correctness fix, wrong today, independent of everything else. *(built)*
2. **Teach the parser `--dtype` and `--quantization`.** Without them the
   fingerprint pools an FP8 and a BF16 serving of one model. *(built)*
3. **Wire up `EvictStaleHistory` and `Store.EvictStale`.** Neither had a
   production caller. Independent bug fix — and not a neutral one: see
   Appendix C. *(built)*
4. **The fingerprint**, computed, versioned, published as `wva_engine_config`
   — one wide series per variant, load-bearing for nothing. *(built)*
5. **Share the fitted line.** Each variant keeps its own window; a variant with
   no usable fit borrows a sibling's `(A, B)`, marked borrowed. Nothing is
   re-keyed. *(built)*
6. **Publish `wva_learned_*`.** Worth doing on its own: it makes a
   post-restart decision explicable on a dashboard, and needs no trust from
   anyone. No store, nothing read back. The families, which an earlier
   revision dropped while leaving the cardinality arithmetic that depends on
   them:

   ```
   wva_learned_itl_slope{model,accelerator,gpus,fingerprint}              A
   wva_learned_itl_intercept{model,accelerator,gpus,fingerprint}          B
   wva_learned_itl_samples{model,accelerator,gpus,fingerprint}            n
   wva_learned_k2_tokens{...,role,out_bucket,q,source}                    k2
   wva_learned_throughput{...,role,in_bucket,out_bucket,q}                mu
   wva_learned_throughput_samples{...,role,in_bucket,out_bucket,q}        n
   wva_learned_stable_shape_tokens{exported_namespace,model,axis}         I or O
   ```

   Label vocabulary is [`signals-as-metrics.md`](signals-as-metrics.md)'s:
   **`exported_namespace`, never `namespace`**, because the controller's own
   namespace takes `namespace` through the scrape and alerts keyed on it
   silently matched nothing. The key labels are the tuple from Part 1, for the
   reason given there.

   **Three questions this list does not answer, and step 6 cannot be built
   until it does.**

   *It collides with the sibling's own families.* `signals-as-metrics.md`
   already specifies `wva_itl_line_slope/_intercept/_samples` keyed
   `{exported_namespace, model, variant}`, and
   `wva_shape_input_tokens`/`_output_tokens` keyed
   `{exported_namespace, model, role, window}`. The names above are different
   and the keys are incompatible — per-variant against per-configuration.
   Neither family is implemented, so this is two live proposals disagreeing,
   not a migration. **The per-configuration key is the right one for the ITL
   line** (it is what the line is now shared on; see Part 1), so step 6
   supersedes the sibling's three ITL families and the sibling's shape
   families stand as written. Step 6 must say so in the sibling too.

   *`source` is undefined here, and was flagged as a contradiction when it
   was defined.* It was meant to separate a refitted figure from an observed
   one — but a label is part of series identity, so `source="refit"` makes
   the refitted figure a *different series* rather than an annotation on the
   same one, and anything averaging over the family then double-counts. Carry
   it as an `_info`-style companion, or not at all. Not as a label on the
   value.

   *Two of the seven families need a re-key first*, per the table in Part 1:
   `wva_learned_k2_tokens` and `wva_learned_throughput` carry a `fingerprint`
   label over a window whose internal key has none.
7. **Raise `DefaultWindowMaxSize`** to about 240 so the live window reaches its
   own age bound instead of stopping at ~8% of it. One constant, independent of
   everything else, and the measured cause of the 52%. Gate it on the `k`-spread
   measurement above: a wider window that still cannot span 0.30 buys only
   memory.
8. **Refit the ITL line at startup from the engines' own expressions.** A range
   query fitted with the same `itl.Fit` and admitted through the same
   `itl.ValidModel`, behind a flag defaulting off, with the cross-validation
   guard below. On becoming **leader**, not at process start: election is on in
   every shipped install, so a standby elected later would otherwise hold a
   line fitted from a range it read at boot.

   **Whether a refitted line is its own or borrowed decides what step 8 can
   do, and this document did not say.** Part 2 routes a line a variant did not
   measure into the capped branch, where it may hold a fleet and not grow one.
   A refitted line is not measured by *this process* — but it can be measured
   by *this variant*, because the engine series carry `pod` and `instance` and
   the collector already joins on them.

   So the rule is by provenance, not by process:

   - Refitted from **this variant's own pods'** series — its own line. It may
     order, and the 90-second criterion below is the one that applies. This is
     the case a restart of a running fleet hits, and the case worth building.
   - Refitted from a **sibling's** pods, resolved only at fingerprint
     granularity — borrowed. It is stamped like any other borrowed line and
     may only hold, so step 8 cannot accelerate the first floor line at all in
     that case; it accelerates the *hold*, which is worth much less.

   A refit that cannot tell the two apart is the borrowed case by default, and
   is then not worth the range query. **Resolving provenance per pod is
   therefore a requirement of step 8, not a refinement of it.**

Steps 1-5 were worth doing without 6-8, which is the test of whether the
ordering is honest.

Three knobs: `refitFromHistory` (default off), the range it looks back over,
and the mismatch tolerance.

### The guards

**The refit is an optimisation, never a dependency.** No Prometheus, no series,
a bad parse — the controller starts cold exactly as it does now.

**Validate on ingest with the live path's own validator.** A refitted line goes
through `itl.ValidModel` — finite, `A > 1e-12`, `A·0.85 + B > 0`.

**Cross-validate against the first live observation.** `noteLineMismatch`
already compares an observed generation-token rate against the line as a
diagnostic. For a refitted line, promote it: admit the line as provisional, and
on the first real `(k, ITL)` reading check
`|ITL_obs − (A·k + B)| / ITL_obs` against a tolerance. Fail it and discard.
That bounds the one class `ValidModel` cannot — plausible but wrong — to a
single cycle.

**A refitted line needs somewhere to live that eviction will not sweep.**
`itl.Window` holds observations and six config scalars and has nowhere to put
a model, and `EvictStaleHistory` — which step 3 wired up, so this is live and
no longer hypothetical — deletes a window the cycle its `Len()` reaches zero.
A refitted-line-only window would be swept immediately. The place for it is
`itlLines`, which step 5 already added: it is keyed by `itlPhysicsKey`, carries
its own `learnedAt`, and ages by timeout rather than by a window's emptiness.
Two rules come with that:

- **Handover.** A refitted line is provisional and is replaced by the first
  genuine fit, including a one-sample `FitPinnedB`. Holding it until the
  window is `Ready()` would reintroduce the full exposure to a wrong line.
- **Spread.** A stored `k` spread is metadata about the refitted model only and
  goes away with it. It cannot be combined with live observations:
  `KSpread()` over one real observation is 0.

**Log every refitted line** with its range and its fit, and carry a flag on the
`derived-mu` and `replica-capacity-decision` records. A decision made on a
figure the process did not measure has to be visible as such — and `HeldWhy`
already carries `borrowed-line` for the same reason.

**Publish a refit outcome metric**, `wva_refit_entries{outcome}` over
`applied`, `rejected_invalid`, `rejected_mismatch`, `absent`. A diagnostic
nobody can read is the problem it was written to solve.

### The warm pool

`noteITL` excludes `FromWarmPool` replicas, because a borrowed pod runs the
pool's own engine settings — and that reason is **false**, which matters
because it is the reason the code gave too.

`warmpool.EngineOptionsFrom` derives a warm copy's command line from the
ordinary replicas' own PodSpec *precisely so the two match* (its comment: a
different `--gpu-memory-utilization` is a different torch.compile cache key),
and `warmableFlags` covers every flag this fingerprint hashes. A lent pod
therefore normally hashes to the **same** engine configuration. The
fingerprint is exactly what cannot tell it apart — which is why the exclusion
is load-bearing rather than redundant, and why it needed a reason that holds.

The reason that holds is what the pod is running: a pool Pod hosts one awake
engine **plus its sleepers**, each keeping ~1.4 GiB of GPU residue (`demand.go`
measured 4.4 GiB free at 0.95 on an 80 GiB card). Its ITL(k) is the latency of
an engine sharing a card, and its `k` is read against a KV budget the sleepers
have already eaten into. A pool that sets an explicit `GPUMemoryUtilization`
does also change the digest, but the exclusion cannot rest on that: zero
inherits the workload's value, and zero is the default.

The pool's variants are excluded from **publishing** as well as from learning,
for the same reason. It is the only safety property of line-sharing that was
implemented without being argued anywhere, which is why it is argued here.

### Cardinality of the learned families

`outputBuckets` has 6 members; `prefillOutputBucket` is deliberately not among
them; `classifyInputLength` delegates to `classifyOutputLength`, so the input
axis is the same 6 and never includes `noout`.

Role and out-bucket are **not independent**: `throughputKey` forces
`outBucket = prefillOutputBucket` for prefill and only for prefill. So the
reachable set is (decode+both) 2 × 6 × 6 = 72 plus prefill 1 × 6 × 1 = 6 →
**78 per fingerprint as a hard upper bound**, and realistically 36 for a
decode fingerprint and 6 for a prefill one, since a P/D fleet gives the two
roles different params and so different fingerprints. Doubled by the `_samples`
family, times the distinct queue thresholds in use (one, in practice).

**That is one family's axis, not the list's.** It is `wva_learned_throughput`'s
and its `_samples` twin. The other five:
`wva_learned_k2_tokens` is `role × out_bucket` = 2×6 + 1×1 = **13** per
fingerprint, not 78, times whatever `source` turns out to be (see the question
above — if it stays a label, multiply); the three ITL families are **1 each**
per fingerprint, since they carry no role or bucket; and
`wva_learned_stable_shape_tokens` is **2** per model, being keyed by axis
rather than by fingerprint at all. Total per fingerprint: 78 + 78 + 13 + 3,
plus 2 per model.

### What the digest discriminates less well on SGLang

One field: `NumGpuBlocksOverride` has no SGLang flag, so two SGLang engines
differing only in it hash the same. Everything else the digest hashes is
parsed off an SGLang Deployment, `EffectiveMaxBatchedTokens` through the same
helper as vLLM. An earlier revision made this a section and overstated it; it
is one field.

## Testing

- **Unit**, against the existing `mockPrometheusAPI`: a refitted line that is
  valid, one with an invalid `A`, one outside the retention window, and one
  that fails cross-validation on the first live observation.
- **Envtest** for the leader-elected runnable: that it refits on acquisition
  and not at process start, and that a lost lease stops it.
- **A negative control per fix**, per this project's rule — and gated on the
  control actually failing. **Eight** tests on this branch have now passed
  against the bug they named, each found by mutation and none by reading. The
  count is in this document because it is the branch's most reliable statistic
  and not a one-off: the recurring shapes are a fixture value production never
  produces, an assertion on an absence that holds because the subject never
  ran, and an assertion too broad to see its own boundary. The one that
  mattered is in Appendix C, because it certified a safety property that did
  not exist.

## How to know it worked

Restart the controller mid-ramp on the shape-swap trace, and measure **from
load start**, which is the correction Appendix C explains.

Run QT's baseline, re-derived: first observation **load + 22 s**, first OLS fit
and first floor line **load + 7.4 min**, with **28 cycles** between them.

- **Step 8 (the refit) succeeds** if the first floor line arrives inside
  **90 seconds** of load start, *and only if the refitted line counts as the
  variant's own* — see the provenance rule in step 8 of **Order** above, which
  is the condition the whole step rests on.

  A previous revision set this to 2 minutes and justified it by saying the
  floor "also needs `MinThroughputSamplesToOrder` saturated readings, so the
  ITL line is necessary and not sufficient". **That was wrong.** `floor.go`'s
  gate is `rc.SaturatedThroughputDerived || (samples >= … && !staleShape)`:
  the derived disjunct is unconditional, and the comment beside it says
  "Ordering on a derived figure does not wait for samples". Part 2 of this
  document says the same thing from the other direction. Run QT settles it —
  at 15:00:00Z the `itl-window`, `derived-mu` and `throughput-demand-floor`
  lines are all in the **same cycle**, with no measured saturated reading and
  `heldWhy` empty. The line was necessary *and sufficient*, so the figure to
  beat is the 22 seconds the first observation takes, not a prerequisite that
  does not exist.
- **Step 7 (the window size) succeeds** if the share of cycles reaching an OLS
  fit rises against run QT's **25.7%** — 54 of the **210 post-load** cycles,
  not 54 of all 244. The all-cycles figure includes the 34 idle cycles that
  "What a cold start costs" excludes as a cost, and comparing against it would
  be the same methodology error as the first correction in Appendix C. (A
  previous revision printed 25.8% = 54/209, which drops the one post-load
  empty cycle as well; the document's own classification table puts that cycle
  after load, so the denominator is 244 − 34 = 210.)
  Measured over a run with no restart at all: it is a steady-state change and
  must be measured as one.
- **Three runs each way, compared on the median.** Phase-1 timings on this
  trace have produced 28 s, 59 s and 256 s medians for identical work, and the
  run above is n=1 on one decode variant.
- A result that does not clear its target by a wide margin is a negative
  result, and the flag stays off.

## What this does not solve

- **It does not make a genuinely new configuration fast.** It removes
  relearning after a restart, not learning.
- **It does not eliminate plausible-but-wrong values** — the cross-validation
  guard bounds them to one cycle of exposure plus whatever a single observation
  cannot falsify.
- **It does not survive a retention gap**, by design.
- **It is not actuation.** Nothing here is a scaling input for KEDA or an HPA.

---

## Appendix A — rejected: storing a snapshot

**SUPERSEDED. Kept because the four counts on which metrics-as-a-store lose
are the reason the engines' series are read instead.**

Two candidates were worked through before the refit. Reading WVA's **own**
`wva_learned_*` metrics back, and writing one ConfigMap on change.

The metrics read-back loses on four counts, and all four are properties of the
shipped configuration rather than opinions:

- **No writer identity.** The ServiceMonitor drops
  `instance|pod|container|node` deliberately, so KEDA sees one scalar per
  series. That makes `wva_learned_itl_slope{fingerprint}` one series shared by
  every controller pod: during a rollout the new pod can read back the cold
  state it just published.
- **No honest age.** Prometheus stamps samples at scrape time and
  `client_golang`'s `GaugeVec` has no timestamp API, so a republished value's
  apparent age resets to zero. A crashlooping controller would carry a figure
  for ever.
- **No trust boundary.** Anything able to write that series name into the
  configured Prometheus could size someone else's fleet, and the physics keys
  deliberately drop the namespace.
- **Silently inert.** Read-back needs the ServiceMonitor present *and*
  `PROMETHEUS_BASE_URL` pointed at the Prometheus that scrapes WVA, and the
  failure mode is "starts cold", which is also the expected log line.

The ConfigMap answered all four — RBAC, CAS on `resourceVersion`, an honest
`learnedAt` field, and `kubectl delete` as a flush. It was rejected only once
the measurement showed there is nothing to store: the engines already hold the
observations, and refitting from them needs no object, no schema, no RBAC and
no migration.

One claim from that round is **withdrawn**: that the 1 MiB ConfigMap limit is
one "this project has already hit". The only such reference in the repository
is a *guidellm trace* ConfigMap in the P/D well-lit path — a benchmark
artifact, not controller state.

## Appendix B — rejected: pooling the ITL window

**SUPERSEDED by Part 2. Kept because four of its defects are easy to
reintroduce.**

The first design for sharing shared the *observations*: one window per
`itlPhysicsKey`, fed by every variant with that configuration. Review found
four defects, and the line form removes rather than mitigates each.

- **The blend described neither variant** whenever their shapes differed, for
  the `k·C/KVreq` reason in Part 2. The existing rule that the window is not
  cleared on a shape change is an approximation across *time* for one fleet,
  which 30-minute ageing corrects; pooling made it a permanent blend across
  *concurrent* fleets that never converges.
- **A variant could order on data it had never produced**, because a derived
  figure needs no sample count.
- **Separating `itlBaseline` was nominal**: the `B` written was the pooled
  fit's, so every contributor stored the same value under its own key.
- **Per-variant mu depended on Go map iteration order**, because each variant
  fitted over the shared window as it stood when its turn came.

It also needed machinery the line form does not: a contributor set with
per-contributor ageing, a `GrowMaxSize` on the window because pooling shortened
each contributor's history, a `variantSeenAt` stamp and the split eviction it
forced, and a two-key `noteITL`. Two bugs came with that machinery — the
contributor set reintroduced the "one entry per variant ever seen" leak in a
new map, and `MaxSize()` was read outside the lock on a now-shared window.

A re-key of the window would also have moved `itlBaseline` and `startSeconds`,
which share `itlWindowKey`. **Replica start time is not a function of the
engine configuration** — it is image pull, node, PVC and weight-download size —
and `startEstimates` takes the **minimum** per role, so the fastest-starting
pod anywhere sharing a fingerprint would have set the horizon `T` for everyone,
under-projecting the backlog and under-ordering.

## Appendix C — what this document got wrong

Six corrections, recorded because three of them reversed a conclusion and
because the method errors recur.

**The timeline was anchored to the wrong t0, twice.** Every figure was measured
from the controller's first analysis CYCLE, which is 8.4 minutes before the
harness
starts. So 34 of the 35 empty-window cycles were idle with no traffic to learn
from, the "first observation at t+8.8 min" was really load + 22 s, and the
first fit was load + 7.4 min rather than t+15.8. A revision that "corrected"
the cost *upward* to ~63 cycles and ~16 minutes was therefore wrong in the
wrong direction; the original ~8 minutes to the first floor line was right.
This is the second time in this work that a window was anchored to a log's
first line rather than to the event being measured.

**A retraction was itself wrong.** A revision claimed `itlZero` does not appear
in these logs. It appears 461 times on the `derived-mu` line — 79 true, 382
false — and the claim was made from *other* runs' logs and generalised without
grepping the one in question.

**Wiring up the evictors was presented as a pure memory fix and was not.** Two
read paths depended on entries the sweep now deletes: the k2 Priority-2
historical read had no staleness guard though the write path beside it did, and
`startSeenPods` was never re-stamped, so a Pod Ready longer than the timeout
had its start folded into the estimate again. Both are fixed; the lesson is
that "wire up a function that already exists" is a behaviour change until its
readers are checked.

**Leader election was called off by default.** The Go flag's default is
`false`, but `config/base/manager/deployment.yaml` passes
`--leader-elect=true` and no overlay removes it, so a handoff is routine in
every shipped install. The premise was used to downgrade the handoff case; it
upgraded it instead.

**`EnforceEager` was said to need a measurement before it could be hashed.**
It does not. Excluding it pools an eager and a graph-captured engine onto one
ITL line, which describes neither; including it costs only that each learns
its own. The safe direction needs no measurement, and the built fingerprint
hashes it.

**A gate was written that did nothing, and a test certified it.** The
borrowed-line gate added a term to one of two disjuncts while the other
disjunct re-admitted everything, and the spec that covered it built its fixture
with a sample count production never produces. Eight tests on this branch have
passed against the bug they were written for; this was the one that mattered,
because it certified a safety property that did not exist.
