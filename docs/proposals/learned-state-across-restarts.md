# Learned state across a restart, and what it should be keyed by

**Status:** proposed, nothing built. Two changes that only make sense together:
persisting what the analyzer learns, and fixing what it is filed under. Either
alone is worth less than half.

This **revises [`signals-as-metrics.md`](signals-as-metrics.md)**, which said
publishing these signals was observability only and that a restart gap was
"honest rather than fixable". The first half stands; the second was wrong, and
the reason is below.

## The problem, measured

Nothing the analyzer learns survives a restart. Confirmed rather than assumed:
there is no persistence anywhere in `internal/signals/capacity` or
`internal/engines/analyzers/saturation` — `capacity.Store` is a plain
`map[string]*Record` behind a mutex, and every window is in memory on the
analyzer.

What that costs, from this repository's own measurements:

| lost | cost of relearning |
| --- | --- |
| the ITL line `(A, B)` per variant | **51 cycles** with `itlZero: true` before a fit exists — needs 10 samples and a `k` spread of 0.30 |
| the derived mu | nothing until the line exists, so **no demand floor at all** for that window |
| `k2` per shape bucket | falls back to `k1` alone, so capacity is memory-bound until a saturated cycle is observed |
| the saturated throughput window | `MinThroughputSamplesToOrder` = 2 readings a minute apart before the floor may order |
| the stable shape | the queue is priced at `DefaultExpectedOutputTokens` = 512, which **under-states a 6000-token workload twelvefold** |
| the replica start estimate | the floor's horizon `T` falls back to configuration |

Measured end to end on run QT this month: **~8 minutes** between load starting
and the first `throughput-demand-floor` line, with 51 `itlZero: true` cycles in
between. A restart during a traffic ramp therefore removes the floor for the
window the floor exists for — and the floor is what stops occupancy
under-sizing a fleet that is keeping up.

A rollout restarts the controller. So does a node drain, an OOM, a leader
election. This is not a rare path.

## Part 1 — what it is keyed by

Take this first, because persisting state keyed on the wrong thing makes the
problem worse rather than better: it carries a figure forward into a situation
it does not describe.

### What the keys are today

| state | key | variant name in it? | engine config in it? |
| --- | --- | --- | --- |
| k2 history (`historyKey`) | model, accelerator, gpus, role, output bucket, queue threshold | **no** | **no** |
| throughput window (`throughputKey`) | the same + input bucket | **no** | **no** |
| ITL window (`itlWindowKey`) | namespace, model, **variant**, accelerator, gpus | **yes** | **no** |
| `capacity.Store` record | namespace, model, **variant** | **yes** | — (it *holds* the params) |
| shape tracker | namespace, model | no | no |
| accelerator memo | namespace, **variant** | yes | no |

`throughputKey` and `historyKey` take `variantName` as a parameter and do not
put it in the key — it is used only to resolve `stableAccelerator`. So those
two are already keyed on hardware and traffic shape rather than on identity.
The ITL window and the capacity store are not.

**Neither includes the engine configuration.** That is the defect, and it cuts
both ways:

- Two variants of one model on the same accelerator and GPU count, with
  **different `--max-num-batched-tokens`**, share a k2 history window. Their
  capacity genuinely differs; the window averages them together.
- One variant **renamed**, or a second identical variant added, gets a fresh
  ITL window and pays the 51 cycles again — even though `ITL(k)` is a property
  of the engine build and the hardware, not of a name.

### The relation already exists

`capacity.EngineParams.IsCapacityCompatible` is exactly the equality this
wants: an eight-field predicate over `Engine`, `GpuMemoryUtilization`,
`BlockSize`, `KvCacheDtype`, `TensorParallelSize`, `NumGpuBlocksOverride`,
`TotalKvTokensOverride`, `EffectiveMaxBatchedTokens`. It is used only as a
*fallback scan* in `Store.FindCompatible`, for sizing a zero-replica variant.

**The proposal is to promote that relation from a fallback to the key.**

```
engineFingerprint = hash(
    Engine, GpuMemoryUtilization, BlockSize, KvCacheDtype,
    TensorParallelSize, NumGpuBlocksOverride, TotalKvTokensOverride,
    EffectiveMaxBatchedTokens,
    MaxNumSeqs,                    // see the defect below
    acceleratorName, gpusPerReplica,
)
```

Then key each piece of learned state by what it is actually a function of,
which is not the same thing for all of them:

| state | should be keyed by | why not more |
| --- | --- | --- |
| ITL line `(A, B)` | fingerprint | it is physics: the same build on the same hardware has the same line **whatever shape is arriving**. That is precisely why the derived mu can price a shape the fleet has never saturated under. |
| k2 | fingerprint + role + output bucket | capacity is config **and** shape: `k2 = N_steady x (I + O/2)` |
| throughput window (mu) | fingerprint + role + input and output buckets | a completion rate is per shape |
| `capacity.Store` record | fingerprint | the record *is* the config plus what was measured for it |
| stable shape | namespace + model | traffic, not configuration. Unchanged. |
| accelerator memo | should disappear — the accelerator is **in** the fingerprint |

Dropping `namespace` from the physics-keyed entries is deliberate and is the
main prize: the same engine build on the same GPU in a different namespace has
the same ITL line, and today it relearns it.

### A defect this exposes

`IsCapacityCompatible` does **not** compare `MaxNumSeqs`. But `S` caps
`N_steady` in k2 (`replica_capacity.go:705`) and caps `seqs` in the derived mu
(`mu_from_itl.go:218`). Two engines differing only in `--max-num-seqs` have
different capacity *and* a different mu, and the predicate calls them
compatible.

Today that only mis-sizes a zero-replica variant through `FindCompatible`.
Promoted to a key it would pool two genuinely different engines into one
window. `MaxNumSeqs` must join the comparison, and that is worth doing
**whether or not** the rest of this proposal happens.

`EnforceEager` is a second candidate — no CUDA graphs changes ITL, so it
belongs in the fingerprint for the ITL line even if KV capacity is unaffected.
Needs a measurement before being added.

## Part 2 — persisting it

### What has to survive, and its shape

Every piece is a handful of scalars per key. Nothing needs a blob:

| state | what to store | per key |
| --- | --- | --- |
| ITL line | `A`, `B`, sample count, `k` spread | 4 floats |
| k2 | the value, and which priority produced it | 1 float + 1 label |
| throughput window | the value, the sample count | 2 floats |
| stable shape | `I`, `O` | 2 floats |
| start estimate | seconds | 1 float (**already published** as `wva_replica_start_seconds_estimate`) |

Crucially, the ITL line is rehydrated **as a model, not as observations**. The
20 `(k, ITL)` pairs do not need to survive; `(A, B)` plus the sample count and
spread is enough to resume, and is four numbers instead of forty.

### Why metrics, and not a ConfigMap or a CR status

I argued against this in the earlier proposal and was wrong. The decisive
argument is write amplification:

| | metrics | ConfigMap / CR status |
| --- | --- | --- |
| write cost | in-process, no etcd | **an etcd write per change, per model** |
| size limit | none | **1,048,576 bytes**, enforced; this project has already hit it |
| history | free, and queryable | last value only |
| new API surface | none | a schema to version and migrate |
| availability at startup | needs Prometheus **reachable** | needs only the API server |
| correctness | a **feedback loop** — reads its own output back | authoritative |

A snapshot written to etcd on every cycle, per model, is not acceptable churn
for a controller that runs every few seconds. Throttling it means the snapshot
is stale exactly when a crash loses the most. Metrics have neither problem, and
the signals are already time series conceptually.

The two columns metrics lose on are real, and the design has to answer them.

### The guards, which are the whole design

**Rehydration is an optimisation and never a dependency.** If Prometheus is
unreachable, the series are absent, or anything fails to parse, the controller
starts cold exactly as it does now. There is no configuration in which WVA
requires a query engine to start.

**Validate on ingest with the live path's own validators.** A rehydrated ITL
line goes through `itl.ValidModel` — finite, `A > 1e-12`, `A·0.85 + B > 0` —
and a rehydrated k2 through the same positivity checks. This is what bounds the
feedback loop: a bug that writes a wrong `A` cannot survive a restart if the
wrong `A` is one the live path would itself reject. It does not bound a wrong
*plausible* value, which is the residual risk and should be stated as such.

**Carry the original timestamp, not `now()`.** Prometheus returns the sample's
own time; rehydrated entries enter with it and are subject to the existing
`EvictStaleHistory`. A figure learned six hours ago ages out on the same rule
as one learned in-process.

**Cap the age** at something short — a scrape-interval multiple, not hours —
with one knob, `rehydrateMaxAge`. Past it, start cold. A shape bucket nobody is
serving simply never gets read, because the key will not match.

**Query a range, not an instant.** `wva_` series go stale roughly five minutes
after the controller stops. An instant query at startup returns nothing if the
pod was down longer than that. The rehydration must ask for a range and take
the last non-stale sample.

**One query, at startup, once.** Not a per-cycle read. The analyzer's live path
never reads Prometheus for its own state.

**Log every rehydrated entry at `logging.DEFAULT`.** A decision made on a
figure the process did not measure must be visible as such, with its age and
its key — otherwise the first confusing scale-up after a restart is
undiagnosable. A `rehydrated` flag belongs on the `derived-mu` and
`replica-capacity-decision` records too.

### The metric shape

The fingerprint is the key label; its components live in a companion info
metric, which is the standard way to keep a hash debuggable:

```
wva_engine_config_info{exported_namespace,model,variant,fingerprint,
                       engine,tp,block_size,kv_dtype,max_num_seqs,
                       max_batched_tokens,accelerator,gpus}          1

wva_learned_itl_slope{fingerprint}                                   A
wva_learned_itl_intercept{fingerprint}                               B
wva_learned_itl_samples{fingerprint}                                 n
wva_learned_itl_k_spread{fingerprint}                                spread

wva_learned_k2_tokens{fingerprint,role,out_bucket,source}            k2
wva_learned_throughput{fingerprint,role,in_bucket,out_bucket}        mu
wva_learned_throughput_samples{fingerprint,role,in_bucket,out_bucket} n
wva_learned_stable_shape_tokens{exported_namespace,model,axis}       I or O
```

Label vocabulary is [`signals-as-metrics.md`](signals-as-metrics.md)'s —
`exported_namespace` and not `namespace`, for the reason stated there. Note the
physics-keyed families carry **no namespace**, which is the point of part 1.

Cardinality is bounded by distinct engine configurations times shape buckets,
not by replicas: a fleet scaling 1 → 10 adds no series.

## What this does not solve

- **It does not make a cold start fast.** A genuinely new configuration still
  pays the 51 cycles. This removes the relearning after a *restart*, not the
  learning.
- **It does not validate plausible-but-wrong values.** The ingest validators
  reject impossible figures, not mistaken ones.
- **It does not survive a Prometheus wipe or a retention expiry**, by design —
  those start cold.
- **It is not actuation.** Nothing here is a scaling input for KEDA or an HPA;
  that remains the rejected "metric shop" design, and
  `wva_learned_*` are internal state made visible, not targets.

## Order

1. **`MaxNumSeqs` into `IsCapacityCompatible`.** A one-line correctness fix
   with a test, independent of everything else, and wrong today.
2. **The fingerprint**, computed and published as `wva_engine_config_info`, and
   used for nothing yet. Observable before it is load-bearing.
3. **Re-key the ITL window and `capacity.Store`** onto it, dropping variant
   name and namespace. This alone makes a rename and a second identical variant
   free, with no persistence involved — and it is the step whose behaviour
   change needs the most care, since it pools windows that are separate today.
4. **Publish `wva_learned_*`.** Still no read-back; the series become the
   record of what the controller knows.
5. **Rehydrate at startup**, behind a flag, default off, with every guard
   above. Default on only after a measured restart-during-ramp comparison.

Steps 1-3 are worth doing even if 4 and 5 are never built, which is the test of
whether the ordering is honest.

## How to know it worked

The measurement already exists: restart the controller mid-ramp on the
shape-swap trace and compare the time from load start to the first
`throughput-demand-floor` line against the ~8 minutes and 51 `itlZero` cycles
of run QT. Cold-to-cold, same trace, binary the only variable — the method
[`analyzer-evidence.md`](../developer-guide/analyzer-evidence.md) uses
throughout.
