# Learned state across a restart, and what it should be keyed by

**Status:** steps 1-5 built; 6 and 7 proposed. Revised twice, substantially:
once after an adversarial review against the code, and once after the
measurement in Part 2, which **removed the store from the design entirely**.
The findings are folded in where they land rather than appended.

Two changes that only make sense together: persisting what the analyzer learns,
and fixing what it is filed under. Either alone is worth less than half —
persisting state keyed on the wrong thing carries a figure forward into a
situation it does not describe.

This **revises [`signals-as-metrics.md`](signals-as-metrics.md)**, which said
publishing these signals was observability only and that a restart gap was
"honest rather than fixable". The first half stands. The second was wrong, but
not in the way the first draft of this document claimed: the gap is fixable,
and the fix is neither reading WVA's own metrics back nor storing a snapshot.
It is to refit the line from the **engines'** own series, which Prometheus
already holds. See
[there is no store](#measured-and-it-settles-the-mechanism-there-is-no-store).

## The problem, measured

Nothing the analyzer learns survives a restart. Confirmed rather than assumed:
there is no persistence anywhere in `internal/signals/capacity` or
`internal/engines/analyzers/saturation` — `capacity.Store` is a plain
`map[string]*Record` behind a mutex, and every window is in memory on the
analyzer.

What that costs:

| lost | cost of relearning |
| --- | --- |
| the ITL line `(A, B)` per variant | **51 cycles** with `itlZero: true` before a fit exists — 10 samples **and** a `k` spread of 0.30, within an observable band of `[0.15, 0.80]` |
| the derived mu | the floor may **hold the fleet but not grow it** until two own readings exist |
| `k2` per shape bucket | falls back to `k1` alone, so capacity is memory-bound until a saturated cycle is observed |
| the saturated throughput window | `MinThroughputSamplesToOrder` = 2 readings before the floor may order |
| the stable shape | the queue is priced at `DefaultExpectedOutputTokens` = 512, which **under-states a 6000-token workload twelvefold** |
| the replica start estimate | the floor's horizon `T` falls back to configuration |

Measured end to end on run QT this month: **~8 minutes** between load starting
and the first `throughput-demand-floor` line, with 51 `itlZero: true` cycles in
between.

Two corrections to how this was first stated, because both change what the
proposal is worth:

**The floor is not absent — it is capped.** `useDerived` consults the derived mu
only when a role has no own measured reading, and `floor.go` lets a role order
on `SaturatedThroughputDerived || (samples >= 2 && !staleShape)`. With no
derived mu, two own saturated readings still produce a floor. What is actually
lost is the first two readings' worth of ordering plus the thin-window cap —
"may hold the fleet but not grow it". Only a role **never seen saturated** gets
no floor at all. That is still the window the floor exists for, and a restart
mid-ramp still lands in it; it is a smaller claim than "no floor", and it is the
true one.

**Which gate cost the 51 cycles is an open question, and it decides the prize.**
At one reading per replica per cycle, 10 samples is ~10 cycles. 51 means the
binding constraint was the **`k` spread** (0.30) or the observable band, not the
sample count — and `noteITL` additionally discards `!Ready`, `FromWarmPool`,
`AvgITL <= 0` and `KvUsageInstant <= 0` readings. This matters directly:
if those 51 cycles were spent waiting for the fleet's load to *spread* `k`,
rehydrating `(A, B)` buys all 51; if they were spent collecting samples, it buys
about 10. **Settle this from the run-QT scrapes before building step 6** — the
data is already on the PVC. The table above says 51 because that is what was
measured; the attribution is not yet evidence.

A rollout restarts the controller. So does a node drain and an OOM. Leader
election is **off by default** (`--leader-elect=false`), so election churn is
not routine — but when it is enabled a handoff is a worse case than a restart,
not a better one, and [that gets its own section](#more-than-one-controller).

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
hardware and traffic shape rather than on identity. The ITL window and the
capacity store are not.

**Neither includes the engine configuration.** That is the defect, and it cuts
both ways:

- Two variants of one model on the same accelerator and GPU count with
  **different `--max-num-batched-tokens`** share a k2 history window. Their
  capacity genuinely differs; the window averages them together.
- One variant **renamed**, or a second identical variant added, gets a fresh
  ITL window and pays the relearning again — though `ITL(k)` is a property of
  the weights, the engine build and the hardware, not of a name.

### The relation already exists

`capacity.EngineParams.IsCapacityCompatible` is close to the equality this
wants: an eight-field predicate over `Engine`, `GpuMemoryUtilization`,
`BlockSize`, `KvCacheDtype`, `TensorParallelSize`, `NumGpuBlocksOverride`,
`TotalKvTokensOverride`, `EffectiveMaxBatchedTokens`. It is used only as a
*fallback scan* in `Store.FindCompatible`, for sizing a zero-replica variant.

The proposal promotes that relation from a fallback to a key — but it is **not
sufficient as written**, and the next two sections are the corrections.

```
engineFingerprint = hash(
    fingerprintVersion,            // see versioning, below
    weightDtype, quantization,     // NOT in EngineParams today
    Engine, GpuMemoryUtilization, BlockSize, KvCacheDtype,
    TensorParallelSize, NumGpuBlocksOverride, TotalKvTokensOverride,
    EffectiveMaxBatchedTokens,
    MaxNumSeqs, MaxModelLen,       // see the defects below
)

learnedStateKey = (modelID, acceleratorName, gpusPerReplica, engineFingerprint)
```

### The hash covers the flags; the axes people query by stay labels

`modelID`, `acceleratorName` and `gpusPerReplica` are **part of the key and
not part of the hash**. The key is the tuple above, composed in one key
function, and the fingerprint is only its last component.

This corrects an earlier draft, which hashed all three in. Three reasons the
split is better, none of which weakens the identity:

- **It is directly queryable.** `wva_learned_itl_slope{model="..."}` answers
  "what has this model learned" with no join, and `{accelerator="H200"}`
  answers the comparison an operator actually makes. Hashed in, every such
  question becomes a join against `wva_engine_config_info`.
- **It removes a many-to-one inconsistency.** The info metric has to carry
  the identity somewhere. If `model` is also hashed, there are N info series
  per fingerprint and no way to tell which one a learned line came from. With
  `model` on the key itself, the two agree by construction.
- **It costs no cardinality.** The number of distinct
  `(model, accelerator, gpus, fingerprint)` combinations is the same either
  way. Only the representation changes.

What this must not become is "the fingerprint alone identifies the line" --
exactly the error the first draft made, and it would pool a 0.6B and a 32B
model onto one ITL line. In Go the key is a composed string or a struct
either way, so the discipline lives in the key function; a hash is not a
substitute for writing that function once and using it everywhere.

`weightDtype` and `quantization` stay hashed. They are launch flags like the
rest, they are already labels on the info metric for readability, and nobody
asks an autoscaler's internal state to list every model served at FP8.

### `EngineParams` says nothing about the weights

Stated prominently because the first draft got it wrong, and the error would
have been the worst one this design could make. `EngineParams` holds `Engine`,
`GpuMemoryUtilization`, `BlockSize`, `KvCacheDtype`, `TensorParallelSize`,
`NumGpuBlocksOverride`, `MaxNumBatchedTokens`, `MaxNumSeqs`, `MaxModelLen`,
`EnforceEager`, `IsV1Engine`, `ChunkedPrefillEnabled`, `TotalKvTokensOverride`
and `EffectiveMaxBatchedTokens` — and **nothing that identifies the weights**.
`KvCacheDtype` is the KV cache's dtype, not the model's; `MaxModelLen` is a
context-length limit.

A fingerprint built from `EngineParams` alone gives a 0.6B model and a 32B model
the same identity whenever their launch flags and hardware match, and therefore
the same ITL line — a quantity that differs between them by an order of
magnitude. ITL's slope and intercept are dominated by parameter count and weight
dtype, which is exactly what the struct cannot see. Two variants deployed from
one manifest template is the *common* case, not a contrived collision.

So `modelID` is part of the KEY, as a label beside the fingerprint rather
than hashed into it (see above). And that is still not enough:
`applyParam` parses `gpu_memory_utilization`, `block_size`, `kv_cache_dtype`,
`tensor_parallel_size`, `num_gpu_blocks_override`, `max_num_batched_tokens`,
`max_num_seq(s)`, `max_model_len`, `enforce_eager` and
`enable_chunked_prefill` — **no `--dtype`, no `--quantization`**. An FP8 and a
BF16 serving of the same model collide. **Teaching the parser those two flags is
therefore a prerequisite, not a refinement**, and it is step 2 of the order.

A served model path plus revision would be stricter still, since two `modelID`s
can point at different weights and a retune changes ITL without changing the
name. `modelID` plus dtype and quantization is the floor, not the goal.

The general lesson, which applies to every field: the fingerprint is a
**deliberate list of what capacity and latency are a function of**, not
"whatever `EngineParams` happens to carry". Build it by naming the physics and
then checking the struct can supply it — the opposite order from the one that
produced the error above.

### Two defects this exposes, both worth fixing alone

**`IsCapacityCompatible` does not compare `MaxNumSeqs`.** But `S` caps
`N_steady` in k2 (`replica_capacity.go:705`) and caps `seqs` in the derived mu
(`mu_from_itl.go:218`). Two engines differing only in `--max-num-seqs` have
different capacity *and* a different mu, and the predicate calls them
compatible. Today that only mis-sizes a zero-replica variant through
`FindCompatible`; promoted to a key it would pool two genuinely different
engines.

**`MaxModelLen` is absent too**, from the predicate and from the first draft's
fingerprint. It changes KV per sequence, and with chunked prefill off it changes
`EffectiveMaxBatchedTokens` through `resolveEffectiveMaxBatchedTokens`. Both
join the comparison, and that is **step 1, worth doing whether or not the rest
of this proposal happens.**

`EnforceEager` is a third candidate — no CUDA graphs changes ITL, so it belongs
in the fingerprint for the line even if KV capacity is unaffected. It needs a
measurement first, and adding it later is a **fingerprint version bump**, not a
free change.

### What gets re-keyed, and what must not

The first draft said "re-key the ITL window and `capacity.Store`". Both halves
were wrong in ways the review caught.

`itlWindowKey` keys **three** maps, not one: `itlWindows`, `itlBaseline` (the
last `B` an OLS fit produced, which `FitPinnedB` pins) and `startSeconds`. The
code is explicit about why the latter two die with the window:

> The learned baseline dies with the window that produced it. Kept, it would
> grow one float per variant/accelerator ever seen and, worse, pin a hardware
> floor measured before a redeploy onto different hardware into every later fit
> for that key. / And the start estimate, for the same reason.

**Replica start time is not a function of the engine configuration.** It is
image pull, node, PVC and weight-download size — precisely what the fingerprint
cannot see. Worse, `noteReplicaStart` runs *before* the decode guard that keeps
prefill out of the ITL window, so prefill and decode variants with matching
flags would land `startSeconds` under one fingerprint key, and the estimator
takes the **minimum** per role: the fastest-starting pod anywhere sharing the
fingerprint would set the horizon `T` for everyone, under-projecting the backlog
and under-ordering. A step presented as behaviour-preserving would have been a
regression.

So: **re-key `itlWindows` only.** `itlBaseline` and `startSeconds` stay on the
variant key — two keys computed per cycle instead of one, which is cheap.

`capacity.Store` is **not re-keyed at all**, for three reasons:

- It is circular. The fingerprint comes from `Record.EngineParams`, and the
  Record is populated by `LoadFromScaleTarget(namespace, modelID, variantName)`.
  Looking up by fingerprint requires the params you only have after looking up
  by name, so it needs an extra name→fingerprint index with its own staleness —
  a net *increase* in keyed state.
- `Get(namespace, modelID, variantName)` has six non-test callers, and several
  genuinely ask "what did *this variant* measure"; one reads
  `existing.EngineParams` to decide whether to preserve them, which becomes
  self-referential under a fingerprint key.
- `Record.EffectiveCapacity` is `min(k1, k2)`, and k2 is **shape-dependent** —
  which is why `historyKey` buckets it by output length. Two variants with one
  fingerprint serving different shapes would overwrite each other on every
  `Update`.

`FindCompatible` therefore stays as it is, and gets the two fixed fields.

**Keep `queueThreshold` in the k2 and throughput keys.** Both end in `q%g`
today, with a measurement in the comment: a k2 of 2 learned under a low
threshold kept a variant at utilization 1.0 under a threshold of 100, where P1
could not fire at all. It is the *only* invalidator for a policy retune —
persisting k2 without it would re-seed an operator who retunes
`queueLengthThreshold` and restarts with the pre-retune value, removing the
escape hatch the key change provides.

**Keep the accelerator memo.** The first draft said it "should disappear — the
accelerator is in the fingerprint". Backwards: the memo exists because the
observed accelerator changes with pod placement and can read unresolved, and it
was added to stop a measured k1↔k2 oscillation on a heterogeneous cluster. The
fingerprint takes `acceleratorName` as an **input**, so an unresolved
accelerator makes the fingerprint itself oscillate. The memo must survive and
must feed the fingerprint.

So the corrected key table is:

| state | should be keyed by | why not more |
| --- | --- | --- |
| ITL line `(A, B)` | model + accelerator + gpus + fingerprint | the same weights, engine build and hardware give the same line **whatever shape is arriving** — which is why the derived mu can price a shape the fleet has never saturated under. `noteITL` deliberately does *not* clear the window on a shape change, while the throughput analyzer's window *is* cleared; that asymmetry is the existing evidence for this claim. |
| k2 | fingerprint + role + output bucket + **queue threshold** | capacity is config **and** shape: `k2 = N_steady x (I + O/2)` |
| throughput window (mu) | fingerprint + role + input and output buckets + **queue threshold** | a completion rate is per shape |
| `capacity.Store` record | **unchanged** (namespace, model, variant) | circular, and `EffectiveCapacity` is shape-dependent |
| `itlBaseline`, `startSeconds` | **unchanged** (variant key) | a hardware floor and a start time are not functions of the config |
| stable shape | namespace + model | traffic, not configuration |
| accelerator memo | **unchanged**, and it feeds the fingerprint | it exists because the accelerator reading is unstable |

What is dropped from the ITL line's key is `namespace` and the variant name —
not the model. The prize is correspondingly smaller than the first draft sold:
**a rename is free, and a second identical variant is free.** The cross-namespace
claim survives only for genuinely identical deployments.

### What pooling the ITL window actually changes

Five effects, since this is the step whose behaviour change needs the most care:

1. Two variants of one model with identical flags — a canary, or the decode
   halves of a P/D pair — now share observations. This is the prize.
2. Observations from differently-loaded fleets merge, which **increases**
   `KSpread()` and makes `Ready()` fire *sooner*: a fleet stuck at one `k` can
   borrow spread from a sibling. A real benefit, and also the mechanism by which
   a mis-keyed pool produces a confidently-fitted *wrong* line faster than
   today — which is why the fingerprint has to be right first.
3. `itlBaseline` would pool, pinning one variant's measured `B` into every
   sibling's fit. Prevented by leaving it on the variant key, above.
4. `startSeconds` would pool. Same.
5. **The window's 20 slots are now shared.** `Add` evicts the oldest at
   `DefaultWindowMaxSize` regardless of how many fleets feed it, so with four
   contributing variants each one's history is five cycles deep and
   `DefaultObservationMaxAge` becomes irrelevant — eviction by size dominates.
   **`DefaultWindowMaxSize` must scale with the number of contributors**, or
   pooling makes the window shorter. This is the most mechanical of the five and
   the easiest to miss.

## Part 2 — carrying it across a restart

### MEASURED, and it settles the mechanism: there is no store

This section replaced its own conclusion once the measurement below was taken.
Everything after it that argues for a ConfigMap is kept as the reasoning that
led here, and is **superseded**.

From run QT's controller log, 244 decode `itl-window` cycles, classified by the
line's own `held` and `ready` fields:

| what blocked the ITL fit | cycles | share |
| --- | --- | --- |
| the engine reported **no ITL at all** (window empty) | 35 | 14% |
| the **sample count** (held 1-9) | 28 | 12% |
| the **k-spread** (held >= 10, still not ready) | 127 | 52% |
| ready, a fit was available | 54 | 22% |

Cold start: the first observation lands at **t+8.8 min (cycle 36)** and the
first OLS fit at **t+15.8 min (cycle 64)**. Between those two, 28 cycles were
blocked on the sample count and **zero** on the spread.

Two corrections this forces on the rest of this document. The gap is ~63 cycles
and ~16 minutes, not the "51 cycles, ~8 minutes" quoted above — 8.8 minutes is
when the first OBSERVATION arrives, not the first fit. And `itlZero` does not
appear in these logs at all; the evidence is `held` and `ready` on the
`itl-window` line, and any claim keyed on `itlZero` should be distrusted.

**The conclusion: refit from the engines' own series, and store nothing.**

If the fingerprint matches, it is by construction the same engine build on the
same hardware serving the same model. Prometheus already holds that engine's
`QueryAvgITL` and `QueryKvUsageInstant` series from before the restart — the
same two series the live path fits the line from, every cycle. A **range**
query at startup reconstructs the line from those real observations, so a
stored copy adds nothing that the source does not already have.

Every objection that killed the metrics read-back was about WVA reading back
what **it** had published about itself: no writer identity behind the
ServiceMonitor's `labeldrop`, an apparent age that resets on every republish,
an unauthenticated input to a scaling decision, a feedback loop. **None of them
apply to the engines' series.** That is an independent measurement, not WVA's
own claim, and it is already trusted on the live path.

So this drops, in full: the ConfigMap, its `schemaVersion`, the RBAC increase,
the migration story, the flush story, and the "a poisoned figure now survives
the restart that used to clear it" problem — because nothing is stored and
every start re-derives.

It also adds no dependency. Prometheus is already mandatory: `cmd/main.go`
exits 1 when `PROMETHEUS_BASE_URL` is unset, so a controller that cannot reach
it does not run at all.

What the refit does **not** cover, stated rather than hidden:

- **k2-observed.** It depends on WVA's own judgement that a replica was
  saturated, not on a raw engine series. Re-deriving it means replaying that
  judgement over historical rows, which is a bigger piece of work and is not
  part of this.
- **The replica start estimate.** Not a function of the engine configuration
  (see the re-key section), so it stays per variant and is not carried.
- **Retention.** Where Prometheus does not hold the window, the refit finds
  nothing and the controller starts cold, exactly as today.

### A second finding, possibly the more valuable half

52% of **all** cycles could not fit an OLS line because of the k-spread and
fell back to the pinned-B path. That is not a cold-start problem: it is steady
state. The live window holds 20 observations over 30 minutes, and a balanced
router keeps every replica at nearly the same `k`, so the window often cannot
span the 0.30 of `k` a two-parameter fit needs.

A historical range spans more varied load, so the same query may produce a
genuine OLS fit where the live window cannot — improving the line in **normal
operation**, not only after a restart. That is a hypothesis and is written here
as one: it needs the comparison in *How to know it worked* before anything is
claimed for it. If it holds, it is worth more than the restart case.

### What in the rest of Part 2 still stands

The sections below were written for a stored snapshot. Read them with this
split in mind, because most of the reasoning survives the store being dropped:

**Superseded.** *The store: a ConfigMap written on change* and its scoring
table — kept because the four counts on which metrics-as-a-store lose are the
reason the engines' series are read instead, and that argument is still load
bearing. *Recovery* goes with it: there is nothing stored to flush, and
restarting the controller clears the figure again, as it did before.
*More than one controller* loses its ConfigMap half; the leader-election
timing in it is why step 7 refits on leadership rather than at boot.

**Still stands, unchanged.** *The guards* — rehydration is an optimisation and
never a dependency, ingest runs `itl.ValidModel`, and the cross-validation
against the first live observation is what bounds a plausible-but-wrong line.
*Fingerprint versioning*, because the hash's input set is a compatibility
promise whether or not anything stores it. *The metric shape*, since
publishing `wva_learned_*` is still step 6. *The warm pool* and *SGLang*.

**Three mechanisms the first draft assumed** also still stands in full, and one
of its three is now a prerequisite that has already landed: `EvictStaleHistory`
had no caller.

### What has to survive

The first draft's inventory was incomplete. The analyzer's struct also holds
`itlBaseline`, `startSeconds`, `startOutliers`, `startSeenPods`,
`decodeSaturatedAt`, `throughputSampledAt`, `throughputLastRead` and
`lastAccelerator`.

| state | what to store | per key | persist? |
| --- | --- | --- | --- |
| ITL line | `A`, `B`, sample count, `k` spread, **`learnedAt`** | 5 scalars | **yes** |
| k2 | the value, the sample count, which priority produced it, `learnedAt` | 3 + 1 label | **yes** |
| throughput window | the value, sample count, `learnedAt` | 3 scalars | **yes** |
| stable shape | `I`, `O` | 2 floats | **yes** |
| start estimate | seconds | 1 float | **no** — not a function of the fingerprint, and it already publishes as `wva_replica_start_seconds_estimate` |
| `itlBaseline` | — | — | **no** — dies with its window, deliberately |
| accelerator memo, `decodeSaturatedAt`, scrape bookkeeping | — | — | **no** — process-local |

The ITL line is rehydrated **as a model, not as observations**: the 20
`(k, ITL)` pairs do not need to survive.

### Three mechanisms the first draft assumed and the code does not have

**`EvictStaleHistory` has no caller.** It is defined on the analyzer and called
only from tests — the codebase already says so, in `RollingAverage.Stale`'s own
doc comment: "but it has no caller on the reconcile path, so a window can
outlive the behaviour it describes." So `itlWindows`, `itlBaseline`,
`startSeconds`, `lastAccelerator` and `decodeSaturatedAt` are **never swept in
production**, and the "one window per variant that has EVER been seen" leak the
comment warns about is live today. The first draft's central age guard —
"rehydrated entries are subject to the existing `EvictStaleHistory`" — was
built on dead code. **Wiring it up is an independent bug fix and a prerequisite:
step 3.** The real read-time guards are `RollingAverage.Stale(HistoryEvictionTimeout)`
at **24 hours**, not a scrape-interval multiple.

**A rehydrated value cannot carry its original age through the types it lands
in.** `RollingAverage.lastUpdated` is unexported and stamped `time.Now()` on
creation and on every `Add`, and its own doc says a window created and never
added to is "fresh for one timeout" — so a rehydrated k2 or mu window is
24h-fresh regardless of the age of the number in it, the opposite of the first
draft's claim. `itlBaseline` and `startSeconds` are bare `map[string]float64`
with no timestamp slot. Hence `learnedAt` is stored as **an explicit field and
compared against `time.Now()`**, never inferred from a sample's own timestamp.

**`itl.Window` has nowhere to put a model.** It holds `observations` plus six
config scalars; `Ready()` is `len(observations) >= minSamples && KSpread() >= minKSpread`,
both computed from the slice. Rehydrating `(A, B)` needs three new fields and a
precedence rule, and the first draft specified neither. The rules:

- **Handover.** A rehydrated line is **provisional**. It is used, and it is
  replaced by the first genuine fit — not held until `Ready()`, which would
  reintroduce the full exposure to a possibly-wrong line. A one-sample
  `FitPinnedB` counts as a genuine fit, so the gain in the worst case is one
  cycle; the gain in the expected case is every cycle until the spread gate
  opens, which is the quantity [the open question above](#the-problem-measured)
  has to settle.
- **Eviction.** A rehydrated-model-only window has `Len() == 0`, and the
  eviction pass deletes windows at `Len() == 0`. Once that pass is wired up
  (step 3) it would delete the rehydrated line immediately; while it is dead,
  the line is immortal for the process lifetime. Both are wrong, so the window
  needs an explicit "model without observations" state that eviction ages by
  `learnedAt` instead of by `Len()`.
- **Spread.** A stored spread cannot be combined with live observations —
  `KSpread()` over one real observation is 0. The stored spread is **metadata
  about the rehydrated model only**, and goes away with it at handover.

**`RollingAverage` has no weight.** Seeding one value and claiming `n = 10` is
not expressible: the next `Add` makes the average `(v + new)/2`, not
`(10v + new)/11`, giving the rehydrated figure five times the influence it
should have. `Median()` over a one-element seed *is* that element, so the first
real reading flips mu wholesale. The choice taken here is to **seed `n`
identical values** — defensible, stated explicitly, and the alternative is a
weighted form that is a larger change than the feature.

### The store: a ConfigMap written on change, not the metrics

**This reverses the first draft's headline choice, and the reversal is the
review's most useful outcome.** The first draft dismissed a ConfigMap on write
amplification. That argument attacks a design nobody proposed.

The first draft also claimed the 1 MiB limit is one "this project has already
hit". **That claim is unsupported and is withdrawn** — the only 1 MiB reference
in the repository is a *guidellm trace* ConfigMap in the P/D well-lit path
("1MiB cap; the trace is 900KB"), a benchmark artifact and not controller state.
Asserting an incident that did not happen, as confirmation of the very thing
being argued, is the failure mode this project has a rule about.

Scored against the variant that was actually available — **one** ConfigMap for
the whole controller, written only when a hash of the serialised state moves,
with a floor on write frequency:

| | metrics read-back | one ConfigMap, write-on-change |
| --- | --- | --- |
| write cost | none | a write when a figure moves, floored at ~30s; k2 and mu move on *saturated* cycles, and a quiet fleet writes nothing. The controller already writes Events on the reconcile path. |
| size | n/a | a few hundred fingerprint×bucket entries of scalars is tens of kB, three orders of magnitude inside the limit |
| history | free, and queryable | last value only |
| new API surface | none | one object and a schema |
| availability at startup | needs Prometheus reachable **and** scraping WVA | needs only the API server, which the controller already requires |
| **writer identity** | **none** — see below | RBAC |
| **concurrent writers** | last scrape wins, silently | CAS on `resourceVersion` |
| **honest age** | **impossible** — see below | a `learnedAt` field |
| **authority** | a feedback loop over an unauthenticated input | authoritative |
| **flush** | delete nothing; restart re-reads it | `kubectl delete configmap` |

Four of those rows are decisive, and three are new since the first draft:

**Metrics have no writer identity, by deliberate configuration.** The
ServiceMonitor drops `instance`, `pod`, `container` and `node`, with the comment
that this keeps "a single series per (variant_name, namespace) regardless of how
many controller pods are running. Without this, rolling updates produce multiple
series and break KEDA's scalar expectation." So `wva_learned_itl_slope{fingerprint}`
is **one series shared by every controller pod**. During a rollout the old and
new pod post to the same identity, and which value a range query returns is a
race — the new pod can read back the cold state it just published itself.
Keeping `pod` would fix the race and break the invariant KEDA depends on.

**An age cap is unenforceable across more than one restart.** Prometheus stamps
samples at scrape time and `client_golang`'s `GaugeVec` has no timestamp API, so
the moment a rehydrated value is republished its apparent age resets to zero. A
crashlooping controller would carry a figure **forever**, with `rehydrateMaxAge`
never firing and no observation ever re-validating it. That is a ratchet: it
turns the acknowledged "plausible but wrong" risk from transient into permanent.
A `learnedAt` *field* has none of this; a `learnedAt` *series* would have to be
published as a value and would still be republished by every hop.

**Prometheus becomes an unauthenticated input to a scaling decision.** Anything
that can write `wva_learned_itl_slope{fingerprint=...}` into the configured
Prometheus can size someone else's fleet: a co-tenant exposing a metric of that
name, a misconfigured `remote_write`, a federation or Thanos endpoint
aggregating another cluster, a recording rule. There is no writer identity (the
labeldrop removed it), no signature, and **no namespace scoping** — the physics
keys deliberately drop namespace, so a tenant in one namespace can poison a
fingerprint serving another. This is a genuine trust-boundary change, it was
absent from the first draft, and on its own it settles the mechanism.

**Metrics make the feature silently inert.** Reading back requires the operator
to have installed `config/base/monitoring/servicemonitor.yaml` *and* pointed
`PROMETHEUS_BASE_URL` at the same Prometheus that scrapes WVA. Nothing checks
that, and the failure mode — "starts cold" — is also the correct-and-expected
log line.

So the split is:

- **Publish `wva_learned_*` as metrics.** Worth doing for its own sake: it is
  the [`signals-as-metrics.md`](signals-as-metrics.md) work, it makes a
  post-restart decision explicable on a dashboard, and it needs no trust.
- **Read back from the ConfigMap.** It has RBAC, CAS, an honest timestamp, a
  flush, and no dependency on a query engine.

What metrics genuinely win — free history and queryability — is retained by
publishing; it was never the read path that needed them.

### The guards, which are the whole design

**Rehydration is an optimisation and never a dependency.** If the ConfigMap is
absent, unreadable or unparseable, the controller starts cold exactly as it does
now. There is no configuration in which WVA requires anything beyond the API
server to start.

**Validate on ingest with the live path's own validators.** A rehydrated ITL
line goes through `itl.ValidModel` — finite, `A > 1e-12`, `A·0.85 + B > 0` — and
a rehydrated k2 through the same positivity checks. A bug that writes a wrong
`A` cannot survive a restart if the live path would itself reject it.

**Cross-validate against the first live observation.** This is strictly stronger
than the validators and was missing from the first draft. `noteLineMismatch`
already compares the observed generation-token rate against the line as a
diagnostic. For rehydrated entries, promote it: admit `(A, B)` as provisional,
use it, and on the first cycle with a real `(k, ITL)` reading check
`|ITL_obs − (A·k + B)| / ITL_obs` against a tolerance. Fail it and discard, log
it, start cold. **This bounds exactly the class `ValidModel` cannot** —
plausible but wrong — and costs one cycle rather than 51. The ITL case is the
easy one because a single observation falsifies a line; k2 and mu need the same
treatment through the P1-observed path.

**Cap the age** on the stored `learnedAt` against `time.Now()`, with
`rehydrateMaxAge`. Past it, start cold.

**Rehydrate on becoming leader, not at process start.** With leader election
enabled those are different moments: a standby elected two hours later would
otherwise hold data read at boot, or nothing at all. It is a leader-elected
`Runnable`.

**Log every rehydrated entry at `logging.DEFAULT`**, with its age and its key,
and carry a `rehydrated` flag on the `derived-mu` and
`replica-capacity-decision` records. A decision made on a figure the process did
not measure has to be visible as such.

**And publish a metric for the rehydration itself** — this project's own rule is
that a diagnostic nobody can read is the problem it was written to solve:

```
wva_rehydrate_entries{family,outcome}   counter
```

with `outcome` in `applied`, `rejected_invalid`, `rejected_stale`,
`rejected_mismatch`, `absent`. Without it, "is this feature doing anything" is
unanswerable from a dashboard.

### Recovery

Today, when a learned figure is wrong in a way the validators pass, the
operator's recourse **is** restarting the controller. This feature removes that,
so it has to replace it. `kubectl delete configmap wva-learned-state` is the
documented flush, `rehydrateMaxAge=0` disables the read path, and both belong in
the troubleshooting reference rather than only here.

### Fingerprint versioning

The hash's **input set is a wire format** the moment anything stores it, and the
breakage is silent rather than a missing series: adding `EnforceEager` later
makes every stored entry stop matching, with no error. So
`fingerprintVersion` is hashed in, the ConfigMap carries a `schemaVersion`, and
a version bump means "start cold for everything", which is correct and cheap.

### The metric shape

The label set is the key: `model`, `accelerator` and `gpus` beside the
`fingerprint`, which stands for the engine flags alone. The flags themselves
live in a companion info metric carrying exactly the hashed inputs, so it is
one series per fingerprint rather than one per fingerprint and model. The
`variant` name mapping is a third series, because a variant name is not part
of any key here.

```
# the flags behind one fingerprint: exactly the hashed inputs, so 1:1
wva_engine_config_info{fingerprint,fingerprint_version,engine,weight_dtype,
                       quantization,tp,block_size,kv_dtype,max_num_seqs,
                       max_model_len,max_batched_tokens}              1
# which deployed variants currently run that configuration
wva_engine_config_variant{exported_namespace,variant,model,accelerator,
                          gpus,fingerprint}                           1

# KEY = model + accelerator + gpus + fingerprint, on every learned family
wva_learned_itl_slope{model,accelerator,gpus,fingerprint}             A
wva_learned_itl_intercept{model,accelerator,gpus,fingerprint}         B
wva_learned_itl_samples{model,accelerator,gpus,fingerprint}           n
wva_learned_itl_learned_at_seconds{model,accelerator,gpus,
                                   fingerprint}                      unix
wva_learned_k2_tokens{model,accelerator,gpus,fingerprint,
                      role,out_bucket,q,source}                       k2
wva_learned_throughput{model,accelerator,gpus,fingerprint,
                       role,in_bucket,out_bucket,q}                   mu
wva_learned_throughput_samples{...}                                   n
wva_learned_stable_shape_tokens{exported_namespace,model,axis}        I or O
```

Label vocabulary is [`signals-as-metrics.md`](signals-as-metrics.md)'s —
`exported_namespace` and not `namespace`, for the reason stated there.

One unresolved tension, named rather than hidden: `source` on
`wva_learned_k2_tokens` should distinguish a rehydrated figure from an observed
one, but adding `source="rehydrated"` **changes series identity**. Since the
read path is the ConfigMap, this is now cosmetic rather than load-bearing — but
if metrics read-back is ever revisited, it is a contradiction, not a detail.

**Cardinality, computed rather than asserted.** `outputBuckets` has 6 members
plus `prefillOutputBucket`, and `classifyInputLength` delegates to
`classifyOutputLength`, so inputs have 6. `wva_learned_throughput` is therefore
fingerprints × 3 roles × 6 in × 7 out ≈ **126 series per fingerprint**, doubled
by the `_samples` family, times the number of distinct queue thresholds in use
(1, in practice). Bounded by distinct engine configurations rather than by
replicas — a fleet scaling 1 → 10 adds no series — but it is per fingerprint,
not per fleet, and that multiplication belongs in the open rather than behind
the word "bounded".

### More than one controller

With `--leader-elect=false` (the default) a rollout briefly runs two pods, both
scraped, both writing. The ConfigMap's CAS makes the loser retry rather than
clobber. With leader election enabled, `LeaderElectionReleaseOnCancel: true`
means a graceful step-down releases the lease; the new leader rehydrates on
acquisition, which is the correct moment. A flap A→B→A is the case that breaks
the metrics path (B publishes cold state; A rehydrates B's garbage) and is
harmless against a store only the leader writes.

### The warm pool

`noteITL` excludes `FromWarmPool` replicas because a borrowed pod runs the
pool's own engine settings — that is, **a different fingerprint**. If the pool
publishes one, the controller would learn an ITL line for a configuration
nothing serves. The pool's variants are excluded from publishing as well as from
learning.

### SGLang

`sglang_parser.go` populates a narrower subset of `EngineParams` than the vLLM
parser, so SGLang fingerprints discriminate less. `Engine` is hashed, so the two
never cross — but the within-SGLang collision rate is higher, and until the
parser is extended the honest statement is that this feature is better tested on
vLLM.

## What this does not solve

- **It does not make a cold start fast.** A genuinely new configuration still
  pays the full relearning. This removes relearning after a *restart*, not
  learning.
- **It does not eliminate plausible-but-wrong values** — it bounds them, with
  the cross-validation guard, to one cycle of exposure plus whatever a single
  observation cannot falsify.
- **It does not survive a Prometheus retention gap**, by design: where the
  series are not held, the refit finds nothing and the start is cold.
- **It is not actuation.** Nothing here is a scaling input for KEDA or an HPA;
  that remains the rejected "metric shop" design.

## Order

Revised twice: the first draft put an unsound step third, and the measurement
in Part 2 replaced the store in steps 6 and 7.

1. **`MaxNumSeqs` and `MaxModelLen` into `IsCapacityCompatible`**, with tests.
   Correctness fix, wrong today, independent of everything else.
2. **Teach the parser `--dtype` and `--quantization`.** Without them the
   fingerprint pools an FP8 and a BF16 serving of one model. Prerequisite for
   step 4, useful on its own for `FindCompatible`.
3. **Wire up `EvictStaleHistory`.** It has no production caller, which is a live
   leak the code already documents, and the eviction rules below depend on it.
   Independent bug fix.
4. **The fingerprint**, computed, versioned and published as
   `wva_engine_config_info`, used for nothing. Observable before it is
   load-bearing.
5. **Re-key `itlWindows` only** onto it — not `itlBaseline`, not `startSeconds`,
   not `capacity.Store` — keeping `queueThreshold` in the k2 and mu keys and
   scaling `DefaultWindowMaxSize` with the number of contributors. This alone
   makes a rename and a second identical variant free, with no persistence
   involved.
6. **Publish `wva_learned_*`.** Worth doing on its own: it makes a
   post-restart decision explicable on a dashboard, and it needs no trust from
   anyone. No store, and nothing reads it back.
7. **Refit the ITL line at startup from the engines' own series.** A range
   query over `QueryAvgITL` and `QueryKvUsageInstant` for the fingerprint's
   model and hardware, fitted with the same `itl.Fit` the live path uses, and
   admitted through the same `itl.ValidModel`. Behind a flag, default off,
   with the cross-validation guard: the refitted line is provisional until the
   first live observation either confirms it within a tolerance or discards it.
   Default on only after the measured comparison below.

   On becoming leader rather than at process start, because with leader
   election enabled those are different moments and a standby elected later
   would otherwise hold a line fitted from a range it read at boot.

Steps 1-5 are worth doing even if 6 and 7 are never built, which is the test of
whether the ordering is honest. Under the first draft's order, step 3 failed
that test — it moved `startSeconds` onto a key that does not describe it.

Three knobs, not one: `refitFromHistory` (default off), the range the query
looks back over, and the mismatch tolerance for cross-validation.

## Testing

- **Unit.** The ingest path against a table of stored entries: valid, invalid
  `A`, stale `learnedAt`, unknown `schemaVersion`, mismatched fingerprint
  version, and a line that fails cross-validation on the first observation.
- **Envtest** for the leader-elected `Runnable`: that it rehydrates on
  acquisition and not at process start, and that a lost lease stops writes.
  `internal/collector/source/prometheus` and the scale-to-zero suite already
  have the harness patterns.
- **Negative control, per this project's rule.** Every one of these tests run
  against the pre-change binary, with the failure pasted — in particular that
  the re-key test fails on the parent commit.

## How to know it worked

Restart the controller mid-ramp on the shape-swap trace and compare the time
from load start to the first `throughput-demand-floor` line, and to the first
`itl-fit`, against run QT's measured baseline: first observation t+8.8 min
(cycle 36), first OLS fit t+15.8 min (cycle 64).

And measure the second finding separately, because it is the one that pays in
steady state: the share of cycles that reach an OLS fit rather than pinned-B,
against run QT's 22%.

With thresholds, because the first draft named a measurement and no number:

- **Success** is the first floor line inside **90 seconds** of load start, and
  fewer than **10** `itlZero` cycles.
- **The cold control matters as much as the treatment.** Phase-1 timings on this
  trace are noisy enough that identical work has produced 28 s, 59 s and 256 s
  medians across three runs, so this is **three runs each way**, compared on the
  median, cold-to-cold, binary the only variable — the method
  [`analyzer-evidence.md`](../developer-guide/analyzer-evidence.md) uses
  throughout.
- **A result that does not clear its floor by a wide margin is a negative
  result**, and the feature should stay default-off.
