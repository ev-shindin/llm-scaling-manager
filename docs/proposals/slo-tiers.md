# SLO tiers: size to a TTFT/TPOT target, not only to a fill threshold

**Status:** proposal, 2026-10-15. **Phase 0 (the analyzer, observing) is
built** in `internal/engines/analyzers/slo` (PR #148); nothing that changes a
replica count is. Step 2a (the TTFT curve's inputs) is not built. The main
review results are under [Review findings](#review-findings). Tracks
[#141](https://github.com/ev-shindin/llm-scaling-manager/issues/141).

**Tags.** A behavioural claim is tagged **(built)** when the code on this
branch does it, **(phase N)** when it is planned for that phase, and **(not
built)** when no phase owns it yet.

## At a glance

**Decided**

- A separate `slo` analyzer, combined with saturation later by the composite.
- TPOT is a **mean** that inverts the ITL(k) line; TTFT is a **percentile** on
  the router's histogram.
- Observe is the default, forever; phase 1 needs `slo.mode: enforce`.
- Fill clamped to [0.15, 0.80]; an infeasible target abstains.
- Transients are excluded from judgement; unschedulable capacity is judged.

**Built** (phase 0): the `slo` block with validation, overlay and warnings;
TPOT pricing with the batch cap, and its source per variant (`declined`
included); router judgement with the transient exclusion; the `wva_slo_*`
metrics; alerts `WVAModelSLOAttainmentLow` (TTFT, request-weighted),
`WVAModelSLOMissed` (TPOT) and `WVAModelSLOUnjudged`.

**Built** (S1): each prefill variant of a P/D model priced for TTFT in shadow,
no replica count moved ([as built](#as-built-s1)).

**Not built**: step 2a. It was built and removed, because its output could not
answer [open question 5](#open-questions); it is re-specified as "observe what
open question 5 asks for". Until then, read the raw per-pod vLLM series.

**Open** ([Open questions](#open-questions)): the gates' measurable form;
pinned pricing in sizer mode; the simulator's load factor; what 2a must
observe; router-side limits; a growth cap, multi-model pools, data-parallel
replicas; scoring benchmark claims client-side over all requests (proposed).

**Gates**: phase 1 (TPOT enforce) needs the composite items, the estimator
fixes and a data gate; 2b (TTFT priced, in shadow) a 2a data gate; 2c (TTFT
enforce) its own. The data gates are open, and none is computable from
today's outputs (a phase-0 follow-up).

## The problem

A tier says how full a replica may run. It does not say how fast a request
must be served. An operator with a latency SLO has to turn it into
`kvCacheThreshold` and `scaleUpThreshold` by hand, and redo that whenever the
model, the accelerator or the request shape changes. The documented *Strict
(low latency SLO)* preset (`kvCacheThreshold 0.70`, `queueLengthThreshold 3`)
is one such guess, made once for every model.

llm-d's SLO-aware recipe takes the target directly. Its benchmark reports
**95.8 % combined attainment at 5.93 average replicas, where a static pool of 8
attains 100 %** (Qwen3-32B on H100; TTFT ≤ 3 s and TPOT ≤ 100 ms at p90;
**85.6 %** with a cold predictor). It needs no controller of its own and has a
measured result; that is the bar this proposal has to meet.

The recipe is scoped to *"one inference pool serving one model variant against
one SLO pair"*. [keda-alone](../comparison/keda-alone.md) lists this as a case
where KEDA alone is better today. The goal is to close that row **without
giving up what the recipe lacks**: several variants with cost-aware choice,
P/D pairing, a GPU budget, and scale to zero. The closest peer for that is
AIBrix's GPU optimizer, which works from offline profiles; we learn lines
online instead ([Alternatives](#alternatives-considered)).

## The two objectives are different statistics

- **TPOT is a mean.** It is the average time per output token, and the ITL
  line `ITL(k) = A·k + B` is fitted on a mean of nearly the same quantity:
  `vllm:inter_token_latency_seconds`, sum over count, per variant. A mean-TPOT
  target therefore **inverts the existing line**, with no tail model:

  ```
  k_tpot = (tpotTarget − B) ÷ A
  ```

  "Nearly", in two ways:
  - **Weighting.** The router's TPOT is weighted per request, the engine's ITL
    per output step (the clock-gap test of open question 1 measures the gap).
  - **Speculative decoding.** vLLM records one ITL per *step*, which can emit
    several tokens, so the line under-prices `k` (fixed before phase 1).

- **TTFT is a percentile**, dominated by queueing, with a long tail. It is read
  from the router's histogram and is the harder half (see [TTFT](#ttft-phase-2)).

A TPOT **percentile** is a [later](#later-a-tpot-percentile) extension.

## The design in one paragraph

A new **`slo` analyzer**, separate from saturation, owns latency: the router's
and engines' latency series, each variant's fill at the target, and
attainment. It returns an ordinary `(D, P)` per role. For decode: demand in
output tokens/s, and per variant the tokens/s one replica sustains **at the
fill where its mean ITL equals the TPOT target**. The **composite analyzer**
([appendix](#appendix-the-composite-rules)) takes, per ScaledObject, the max
of each eligible analyzer's `N = D ÷ P`. A tier chooses a mode:

- **Guard.** Both analyzers vote, and the SLO can only add replicas.
- **Sizer.** Saturation is explicitly disabled; the SLO decides. A role the
  SLO cannot price holds where it is: an unlisted saturation supplies units
  and variants, never a size.

There is no new actuation path. Until the composite lands, the analyzer only
observes (built).

## What llm-d's KEDA recipes read

At llm-d `7d5f86d` (`guides/workload-autoscaling/`): **`keda-epp`** scales on
the router's queue and running requests; **`keda-epp-token-aware`** on
in-flight tokens over a hand-calibrated tok/s. **`slo-aware`** scales on
`max(ttft/3.0, tpot/0.1)` from the predictor's p90s, for one pool and one
model, and scores attainment from `llm_d_epp_request_slo_violation_total`,
which needs the latency-predictor plugin.

## Which latency, measured where

| Interval | Router (EPP) | Engine (vLLM) | Client |
| --- | --- | --- | --- |
| proxy before the EPP's ext_proc | — | — | yes |
| **router flow-control queue** | yes | — | yes |
| tokenization + **engine scheduler queue** | yes | yes | yes |
| prefill | yes | yes | yes |

The router's `llm_d_epp_request_ttft_seconds` starts at `HandleRequestHeaders`,
before admission, and needs no plugin. The engine's clock starts at frontend
arrival; the client is inference-perf / guidellm. **The router's TTFT includes
queueing**; only the proxy hop is missing. The catches:

- **The predictor plugin uses a different clock.** Its histograms and
  `llm_d_epp_request_slo_violation_total` start **after** admission, so they
  exclude the router queue. Neither is a sizing input here.
- **Everything is recorded at end of stream.** A stream that never completes
  (abort, timeout, rejection) is never recorded, so attainment **reads high
  exactly when the SLO is being lost** (see [Attainment](#attainment)).
- **Some traffic is never judged** (open). A non-streaming request's TTFT is
  its whole duration, so only `streaming="true"` is read; TPOT without `usage`
  is not recorded. Nothing publishes the judged share of traffic.
- **Priority bands are pooled** (open). Flow control's deliberate queueing of
  a low-priority band counts as a miss, so phase 1 enforce would add replicas
  to remove it. Options: drop negative bands with `priority!~"-.*"` (labels
  are strings), or a per-objective selector.
- **LoRA adapters are counted under the adapter's name** (`target_model_name`),
  never the base model's (open: map adapters to their base model). A model
  whose traffic all goes to adapters reads as idle, and `WVAModelSLOUnjudged`
  stays silent.
- **An EPP scraped twice doubles counts**: the percentile holds, but the
  sample minimum is met with half the real requests.
- **Router per-request SLO headers** (latency-predictor plugin) are a second
  source of targets that nothing reconciles with the tier's. Pick one.

### Metrics this design reads

| Metric | Labels | Used for |
| --- | --- | --- |
| `llm_d_epp_request_ttft_seconds` | `model_name, target_model_name, streaming`, and `fairness_id, priority` (unverified); no pool or namespace (scrape-target `namespace`) | Per-model TTFT percentile and attainment: **the number the SLO is judged on**. 3 s and 0.1 s are bucket bounds |
| `llm_d_epp_request_streaming_tpot_seconds` | as above, no `streaming` label | Per-model mean TPOT as the router sees it. **Recorded only when the response carries `usage`**: the client must send `stream_options.include_usage`, or vLLM must run with `--enable-force-include-usage` |
| `vllm:inter_token_latency_seconds` | per pod | Already collected as a 1 m mean; the ITL line is fitted on it |
| `vllm:request_prefill_time_seconds`, `vllm:request_queue_time_seconds` | per pod | Not read today. The raw inputs to look at until step 2a is re-built. Coarse buckets |
| `vllm:time_to_first_token_seconds` | per pod | Diagnostic only, as today |

The router's request metrics have **no pool or namespace label** (#2309). The
analyzer scopes by the scrape target's `namespace`, as llm-d's keda-epp does.
That assumes the EPP runs in the model's namespace, which nothing enforces.

**The split:** the router's histograms say whether the SLO is met, per model.
The engines' series say which variant to price, and how full it may run.

## What exists today

From `main` at `fee45eb52`, where this work started, with what this branch changed:

| | Where | What it means here |
| --- | --- | --- |
| A tier resolves per **model** | `modelPolicy` in `steadystate/engine.go`; variants that disagree get the lexicographically smallest tier | The target is per model, and the fill is per variant |
| `analyzers:` is replaced whole by an overlay, and an absent name is disabled | `ScalingPolicy.Merge`; `ScalingPolicy.AnalyzerEnabled` returns false for an unlisted name | Dangerous for mode selection; see [Configuration](#configuration) |
| ~~An entry that lists `analyzers:` resets the scaling band~~ **Fixed in #154** | At `fee45eb52`, `ApplyDefaults` filled `0.85 / 0.70` into each stored V2-shaped entry and `Merge` copied it over `default`'s band. Since #154 a stored entry is validated as a defaulted copy (`ValidateEntry`) and left unset, so a tier inherits the band | A tier listing `slo` restates only saturation's analyzer entry, because the list is replaced whole |
| A `Validate` error drops the **whole tier entry** | `parseScalingPolicyConfigRejects` in `controller/configmap_helpers.go` | A bad `slo` block drops the tier, not one key |
| Every enabled analyzer runs; **only saturation's result is used** | `runAnalyzersAndScore`; `collectV2ModelRequest` sets `CompositeSignal: namedResults[0]` | The rest reach only `recordAnalyzerMetrics` (`wva_analyzer_*`), liveness and the cycle log. Observe mode is free |
| `AvgTTFT` is diagnostic by decision | `domain.ReplicaMetrics.AvgTTFT` | A `TTFT(T) = A·T + B` fit was built and removed: queue wait landed in the intercept (82 s) |
| No e2e spec asserts latency | The simulator runs constant latencies | An e2e can exercise only the fallback |

## Configuration

```yaml
data:
  interactive: |
    analyzers:
      - name: saturation
      - name: slo
    slo:
      tpot: {target: 100ms}                  # a mean
      ttft: {target: 3s, percentile: 90}     # a percentile; observed only (TTFT enforce is step 2c)
      flowControl: disabled                  # S1: the EPP's flow-control gate, declared
      mode: enforce                          # phase 1: not parsed yet, ignored today
```

**Observe is the default, and stays the default.** Enforcement needs
`slo.mode: enforce` (phase 1), so a phase-0 policy keeps observing when phase 1
ships. With `mode: enforce`, the `analyzers:` list picks the mode:

| `analyzers:` (with `mode: enforce`) | Mode | What decides |
| --- | --- | --- |
| `saturation` + `slo` | guard | per SO, the larger of the two replica counts |
| `saturation {enabled: false}` + `slo` | sizer | the SLO; a role the SLO cannot size holds where it is |

Without `mode: enforce`, any list that includes `slo` is observe.

- **Saturation is disabled only by an explicit `enabled: false`** (phase 1).
  Today an absent name is disabled and `Merge` replaces the list whole, so an
  override written to "add `slo`" (`analyzers: [{name: slo}]`) would silently
  switch the model to sizer mode.
- **`shadow: true`** is a generic composite field for an analyzer that runs
  without a vote (phase 1). The SLO's observe mode does not depend on it.
- **Half-configured is a warning; malformed is an error** (built). An `slo:`
  block on a tier that does not list `slo` is logged. A bad duration, a target
  ≤ 0, a TPOT percentile, or a TTFT percentile outside 50 to 99 drops the
  entry. The block is replaced whole by an overlay, like `analyzers:`.
- **Headroom is the tier's `scaleUpThreshold`**, in replicas: the fleet runs
  `N ÷ scaleUp`. At `scaleUp = 0.85` a replica priced at `k = 0.60` runs nearer
  `0.50`; the analyzer publishes the ITL predicted there (built). A
  per-analyzer threshold on `slo` is to be rejected (phase 1).

## Where it lives: an analyzer, combined by the composite

### Why a separate analyzer

Observe mode is free: the engine already runs every enabled analyzer in
isolation and discards all but saturation's result. Histogram queries and
attainment are too much code for `saturation/`
([analyzer-structure](analyzer-structure.md)), and the
[engine-structure](engine-structure.md) contract fits.

### What the SLO analyzer returns (decode and aggregated)

| | Value |
| --- | --- |
| `D[decode]` / `D[both]` | `ArrivalRate × O` output tok/s, plus `QueueSize × O ÷ 60 s` for the router backlog (built). Priced in the same unit, not through `signals/floor`, which works in saturation's per-replica units and already applies `scaleUpThreshold` |
| `P_v` | `min(itl.Sequences(k_v, C_v, KVreq), maxNumSeqs_v) ÷ ITL_v(k_v)`: the token rate at `k_v` with the engine's batch cap. When the cap binds, the cache never fills past `maxNumSeqs·KVreq ÷ C`, so the ITL is read there (built). `C_v` and `maxNumSeqs_v` come from the capacity store |
| unknown `O` | No demand, and every generating SO no-data (built): an output length nobody has reported is undefined, not a demand of zero |
| `k_v` | `clamp(k_tpot_v, 0.15, 0.80)`, the clamp `pricingK` uses (built) |
| prefill SOs | **Unmodeled until step 2b.** No `RoleDemand[prefill]`, so the SO is undefined: saturation decides prefill in guard mode (`C1`), and prefill holds in sizer mode, so sizer on P/D waits for step 2b. Prefill pods' TPOT is meaningless anyway (the P/D sidecar sends `max_tokens=1`) |

### Its own lines

Saturation fits ITL lines for the decode role only, so an aggregated variant
(the common deployment, and llm-d's benchmark) would never be priced from its
state. Both analyzers therefore use **one fitter, `itl.Tracker`** (built), so
a fix to its rules reaches both: Ready rows only, warm-pool rows out, readings
above 0.80 out; a full fit when the window spans 0.30 of `k`, otherwise `B`
pinned and only `A` fitted. The `slo` analyzer keeps **its own instance**, a
second 20-reading window per variant (built). Lines are keyed on the
variant's last resolved accelerator and evicted after
`LEARNED_STATE_VARIANT_TIMEOUT` unseen; a variant parked at zero keeps its
line. Intercepts are **not in the learned-state snapshot**, so after a restart
a line is `pinned-default` until the fleet spans `k` again.

The shape (`O`, prompt length, prefix hit rate) is read the same way as
saturation's, not shared. `O` scales both `D` and `P`, so it largely cancels in
`N`, except through `KVreq`.

### What the composite needs before phase 1

1. **The branch landed**, with the three fixes the engine-structure review
   found (the #82 quota-charge regression; the saturation fallback skipping
   the per-SO `Reason` filter; that guard tested on the real
   `buildComposite`). `HeldRoleDemand` is handled per role (built, #167): kept
   in saturation's frame, raised to the most demanding other analyzer's need,
   and dropped where another analyzer's frame decides the role.
2. **Saturation disabled only by an explicit `enabled: false`** (above).
3. **`shadow: true`** (above), also left out of `maxScore`, or a shadowed
   `score` still moves cross-model priority.
4. **Warm-pool pressure from the composite** (built, #167), not from
   saturation's result. The composite keeps saturation's PRC, so supply is the
   same in both; only demand differs.
5. **Size a role saturation sees as idle** (built, #167). The composite
   expresses a need as demand at saturation's PRC, `N_com × P_sat`, which is
   defined when `D_sat[role] = 0`.
6. **Hysteresis on the deciding source** (bound 6): per-SO memory of the last
   composite demand and decision path, which `buildComposite` does not keep.
7. **The SLO count is a floor for utilization-share.** In guard mode the
   [utilization-share](utilization-share-optimizer.md) pass must not hand away
   capacity below the SLO's count.

## Bounds

The tail of an over-estimate is the costliest failure here. Over-pricing `P`
is what breaks TTFT unrecoverably, so `P` is held to rules like k2's.

1. **The fill is clamped to [0.15, 0.80] of physical KV** (built), the range
   the line is fitted over. A bound on the highest fill observed ratchets
   ([Review findings](#review-findings)).
2. **A line with no information gives no fill.** The slope must pass
   `ValidModel`, or the SO reports no data. Pinned lines are priced in phase 0
   and labelled (built): `pinned` on an intercept this variant was measured at,
   `pinned-default` on the built-in 6 ms floor. The latter is not a
   measurement: on a large accelerator the real intercept is several times the
   floor, so the slope is too steep. A window the pinned fit refused is
   `declined` and not priced (built). In sizer mode `pinned-default` is no data
   (phase 1). Once seeding from a profile exists ([Before
   phase 1](#before-phase-1-prices-anything), item 9), `pinned-default` never
   prices in either mode. Whether `pinned` may price in sizer mode is open.
3. **A borrowed line never loosens** (phase 1). A sibling's line may price in
   guard mode only; in sizer mode it reports no data
   ([learned-state sharing](learned-state-sharing-by-similarity.md)).
4. **Infeasible means the SLO abstains** (built). If `tpotTarget ≤ B + A·0.15`,
   no fill in range meets the target. The SO reports no data with source
   `infeasible`; it never caps a variant and never votes 0. A `slo-infeasible`
   blocked reason is phase 1; steering growth away is open question 6.
5. **Undefined is never a number** (built). Every input, rate and quantile is
   checked for NaN/Inf before any clamp, max or division (`x <= 0` admits
   NaN). An invalid input reports `error` with source `input-invalid`.
6. **`P` drops at once and rises slowly** (phase 1). A whole-node replica is
   an 8-GPU node and minutes of start time, and a flipping source would flap
   `N_com`. `P_slo` may fall immediately, and rise only after the higher value
   has held for the scale-down stabilization window. The composite treats a
   switch of deciding source the same way (item 6 above).

## Before phase 1 prices anything

Phase 0 publishes `P_slo`; phase 1 lets it decide. These estimator errors do
not matter while nothing reads the number, and do once something does. Items
2 and 8 over-price `P`. Items 1 (speculative decoding) and 7 (a batch cap too
low per instance) under-price it. Items 3 and 6 can go either way, and so can
item 9 (a seed from a different vLLM version, config or shape); items 4 and 5
are about how far to trust the line. All are phase 1 except item 7, a
separate collector fix.

1. **Speculative decoding** (above): fit on ITL per token,
   `rate(vllm:inter_token_latency_seconds_sum) ÷ rate(vllm:generation_tokens_total)`.
   It also counts each request's first token: negligible for long outputs.
2. **Measure the per-request KV footprint.** `KVreq = ILeff + O ÷ 2`
   under-counts twice. A prefix hit on a block no running request holds still
   makes it resident (0.8 hits on a 4000-token prompt read 1050 tokens against
   about 4250 resident). And a heavy-tailed `O` pushes the footprint well past
   `O ÷ 2`; Dynamo's `context_length = ISL + OSL/2` has the same gap. Measure
   `KVreq_obs = k·C ÷ num_requests_running` per variant.
3. **Regress on a windowed `k`** (`avg_over_time(…kv_cache_usage_perc[1m])`).
   Noise in an instantaneous `k` flattens `A`. A flattened or pinned-`B` line
   rotates about approximately the window's centroid `(k̄, ȳ)` (a pinned fit
   pivots at the k-weighted `Σk² ÷ Σk = k̄(1 + CV²)`), so the pricing error's
   sign is `sign(T − ȳ) × sign(slope error)`: with a slope too flat, a target
   above `ȳ` over-prices and one below it under-prices.
4. **A confidence bound on `k`.** Price where the upper one-sided 90 % band of
   `ITL(k)` meets the target; it penalises extrapolation without a ratchet.
5. **Count cycles, not replica readings.** 20 readings turn over in two cycles
   on ten replicas.
6. **Every line is shape-specific.** On a `both` replica, ITL at a given `k`
   rises when prompts lengthen. A decode-only line moves too: the batch is
   `k·C ÷ KVreq`, so `A` moves with `KVreq` where per-sequence cost matters.
   The 20-reading window bounds this in practice. Reset on a shape change, or
   add prefill tok/s as a second regressor.
7. **The batch cap is per engine instance**, so a pod collapsed from several
   data-parallel ranks needs the cap times the ranks. Collector-wide. Done in
   issue #150: rows carry `EngineRanks`, and ranks behind one API server are
   folded in the per-pod queries.
8. **`max_num_batched_tokens` can bind first**: with speculative decoding the
   cap is `min(max_num_seqs, max_num_batched_tokens ÷ (1 + n_spec))`.
9. **Seed a line from a profile**, the direct remedy for `pinned-default`
   (bound 2). A measured or estimated profile gives a variant its `B` before
   the fleet spans `k`. A seeded `B` gets its own source label and goes
   through the same pinned-vs-own disagreement test (Y, open question 1) as a
   pinned one; `pinned-default` then never prices in either mode.

## TTFT (phase 2)

The least certain part: priced in shadow first (2b), enforced later (2c).

**Step 2a is not built: observe what open question 5 asks for.** A first 2a
published per-variant means of prefill time, queue wait, and utilization
against a learned ceiling. It was removed: means could not answer
[open question 5](#open-questions), and it had no x-axis for `both` variants.
The re-specified 2a observes per-replica points against offered prompt load.
Until then, read the raw per-pod series `vllm:request_prefill_time_seconds`
and `vllm:request_queue_time_seconds`, which hold the same data (vLLM only).

**The 2b design below is superseded** by
[end-to-end TTFT](slo-capacity-planner-ttft.md), which [phase 2](#phase-2-ttft)
now uses: the utilization-binned isotonic curve, the tail mapped through the
router's `q_p ÷ mean` ratio and "queue as zero" below a bin are replaced by a
per-pod queue on prefill work, a prefill throughput `V_P` and a union bound for
the percentile. What survives: the TTFT formula of parts; the blame rule (a miss
with decode at its ceiling is decode's), which the new model keeps as its
decode-back-pressure check; SGLang abstaining (the new model refuses `V_P` on
SGLang); and the transient being out of scope. Kept below for the record.

**The 2b design (superseded).** **2b is
prefill-role only** until an x-axis for aggregated variants is chosen (for
example, offered prompt load over a measured `both` ceiling).

```
TTFT_router  ≈  router queue  +  engine queue  +  prefill        (for means; quantiles do not add)
```

- **TTFT is its own contributor on the prompt-token axis** (a KV-fill curve
  would be confounded by shape). `D[prefill]` is `ArrivalRate × ILeff` input
  tok/s; `P_v` is the input tok/s per replica where the TTFT percentile meets
  the target.
- **The curve.** Per variant: points `(u, prefill, queue)` with `u` the
  prompt load over the variant's ceiling (the x-axis 2a must settle), in bins
  0.05 wide with an isotonic fit. A bin counts only with ≥ 20 cycles of
  effective sample size, read at its one-sided 90 % upper bound.
- **The tail.** The percentile is mapped from the mean by the router's own
  ratio `q_p ÷ mean`, binned by `u`, kept as the 80th percentile of per-window
  ratios. At low utilization the ratio is the service time's own q90 ÷ mean
  (about 2 for heavy-tailed prompt lengths); it moves toward ln 10 ≈ 2.3 at
  p90 as queueing dominates. The router histogram is model-wide, so with
  several prefill variants it cannot be binned per variant.
- **Queue as zero** below the bin where mean engine queue is under 5 % of the
  target: the engine's first queue bucket is 0.3 s.
- **Blame.** A TTFT miss with decode at its KV ceiling, or decode's queue above
  zero, is decode's: prefill queues on decode back-pressure (we measured 439
  queued with 3 of 4 prefill pods idle).
- **P/D transfer** is inside the router's tail ratio until
  `vllm:nixl_xfer_time_seconds` is collected. On **SGLang** the TTFT
  contributor abstains.
- **The transient is out of scope.** A step in arrival lands before a new
  replica is Ready; start time, the warm pool and
  [in-flight sizing](sizing-against-in-flight-capacity.md) are the levers.

## Attainment

From the router, per model and objective, over 5 minutes. All built.

- **The query.** `sum by (model_name, target_model_name, le)(rate(..._bucket{streaming="true"}[5m]))`:
  rate first, then sum, so a counter reset on one EPP replica is handled.
  Summed buckets are repaired to be monotone, as `histogram_quantile` does.
- **TTFT attainment** = `bucket(le=target) ÷ count`, exact when the target is a
  bucket bound (3 s is), interpolated otherwise. Where density falls within a
  tail bucket, linear interpolation over-reads the percentile and under-reads
  attainment, so the error is conservative. Under queueing the TTFT mode can
  sit in the target's bucket, and then it is not; choose a target on a bucket
  bound. Sampling error at the publish minimum is about ±3 points at p90
  (n = 100) and ±0.3 at p99 (n = 1000): 1σ binomial, assuming independent
  misses. A 95 % band is about twice that.
- **TPOT** is the router's **mean**, `rate(_sum) ÷ rate(_count)`, against the
  target. A mean is met or missed; it is not a share. Define `att_tpot` as the
  share of requests under target from the router's per-request TPOT
  histogram, published beside the mean as a diagnostic (not built).
- **No combined attainment.** The router publishes two marginal histograms, so
  joint goodput (both objectives met on one request) cannot be computed. The
  Fréchet lower bound `max(0, att_ttft + att_tpot − 1)` is a floor on it (not
  built). The two figures count different populations (TPOT only for
  responses that carry `usage`), so the bound holds only over that traffic.
- **No data instead of a guess.** Fewer than `max(50, 10 ÷ (1 − p))` requests
  (100 at p90, 1000 at p99) means no attainment, not 0 or 1. On a small
  whole-node fleet, p99 is usually not measurable in 5 minutes. A quantile in
  the `+Inf` bucket is published as `+Inf`.
- **It counts survivors.** Requests still queued, shed or abandoned are
  missing, so attainment reads high under overload. Before anything trims on
  attainment (phase 3), the router's old backlog and its rejected requests
  (`llm_d_epp_request_error_total`) are counted as misses.
- **Not comparable with llm-d's score.** llm-d scores post-admission, so with
  flow control on ours is harsher. Excluded windows bias ours the other way
  (see [below](#transient-exclusion)).

### Transient exclusion

A window is not judged while it may hold requests that waited out capacity no
steady-state sizing could have had ready: a wake, a replica still starting, or
a cycle the analyzer did not see. Stuck or unschedulable capacity is judged.
The rules are in [scaling-policy](../reference/scaling-policy.md), under
*`slo` (observe only)*. Why they are shaped this way:

- **A grace follows the window**, because a request queued through a wake is
  recorded only when its generation ends. One scale-up leaves an objective
  unjudged for the start time, plus the 5-minute window, plus a grace of 2 to
  30 minutes: 7 to 35 minutes after the transient. A fleet that scales more
  often is never judged; `WVAModelSLOUnjudged` says so. The grace is the
  longer of 120 s and 1.5 × the longest mean request duration, capped at 30
  minutes.
- **The grace uses the mean service time, but clearing is a tail question.**
  With exponential-like output lengths, about 22 % (e^−1.5) of requests queued
  through a wake finish after the grace and land in judged windows as false
  misses (conservative; speculative until measured on real traffic). The 22 %
  holds only when the grace is 1.5 × the mean (a mean above 80 s); backlog
  drain pushes it up, and using the longest mean pushes it down. A rough
  figure, not a bound. A high quantile of service time would fix it.
- **`unobserved` is counted in cycles, not timing**: a slow cycle is not a
  skipped one. A failed cycle and its retry count as one.
- **A rise is not a reason of its own**: excluding it after its replicas serve
  would drop the windows that test the new sizing.

**Judged attainment overstates what users saw.** The excluded windows are the
ones around scale-ups, which is when misses happen: the selection is on the
outcome. So `wva_slo_attainment_ratio` cannot be compared with llm-d's
all-requests "95.8 % combined". Benchmark claims for phase 1 are scored
client-side over all requests, from the guidellm or inference-perf per-request
records.

## Observability

The `wva_slo_*` metrics are listed in [prometheus](../reference/prometheus.md)
(*SLO Analyzer Metrics*), and the alerts are explained in
[troubleshooting](../reference/troubleshooting.md). The design choices behind
them:

- **Absent, not zero.** Too few requests, or an excluded window, publishes no
  attainment; `wva_slo_window_requests` tells "too few" from "nothing".
- **Alerts count judged minutes only**, so an exclusion neither fires nor
  resets `WVAModelSLOMissed`. Both gate on replicas with `max_over_time` over
  5 m, so a controller restart does not resolve and re-fire them.
  `WVAModelSLOUnjudged` fires only with a cause to act on. Its TPOT cause
  (streams sent without `include_usage`) is read from the router's own
  streaming request counter. On this release a miss means the
  tier's thresholds do not meet the SLO, not that the autoscaler failed.

## Phases

### Phase 0: the analyzer, observing (built)

Everything under **Built** at the top. It runs only for models whose policy
lists it, needs no restart, and **does not change the decision path**. It
answers "does my tier meet my SLO, and what would the SLO want?". The dormant
`ServiceClass` surface is left for a separate change. **Follow-up (not
built):** publish the line's ITL at the current fill, or `A` and `B`; until
then no gate is computable.

### Phase 1: TPOT enforce

`slo.mode: enforce`, guard and sizer, and bound 6. Needs the seven
[composite items](#what-the-composite-needs-before-phase-1), the nine
[estimator items](#before-phase-1-prices-anything), and open question 1.

### Phase 2: TTFT

**2a** (not built) observes what open question 5 asks for. **2b** runs the
TTFT contributor in shadow. [Open question 5](#open-questions) no longer gates
2b: its x-axis and bins belonged to the superseded binned curve, and the new
model needs none. 2b is gated instead by its recordings (the prefill-stress
golden) and, for 2c, by the staircase run. **2c** enforces TTFT once 2b's shadow
predictions track observed attainment.

**The TTFT model 2b uses** is the one in
[end-to-end TTFT](slo-capacity-planner-ttft.md): prefill capacity as a token
throughput, a per-pod queue with the measured imbalance, the router's
flow-control queue above its onset, and an optimised union bound for the
percentile. The SLO analyzer and the what-if tool use one library for it
(`internal/signals/prefill`, `internal/signals/whatif`), so they never disagree.
Two PRs, under [the learner rules](analyzer-learner.md):

| PR | What | Goldens | Depends on |
| --- | --- | --- | --- |
| S1 (2b) | Price each prefill variant of a P/D model against the TTFT target in shadow: the TTFT predicted at the current rate, and the rate per replica at which the bounded TTFT meets it. Aggregated variants wait for the what-if M1. Published as `wva_slo_*` series and in the cycle log; no replica count changes. Switch `slo-ttft-price`. Inputs and outputs: [S1 decisions](#s1-decisions) | `slo-published.csv` moves with the switch on | the what-if P3 (PR #471); a golden with router TTFT and the [recorder fields](analyzer-learner.md#29-recorder-fields-for-ttft-and-capture), which the prefill-stress recording provides |
| S2 (2c) | Enforce, once S1's shadow predictions pass their gate | moves | S1; Q1 (the measured `V_P` and prompt spread); M2, the what-if staircase run itself with its bursty bucket, not the per-bucket PR that precedes it in that plan |

**What S1 calls.** P3 is built, and is UNVALIDATED until P2
([as built](slo-capacity-planner-ttft.md#as-built-p3)). What S1 passes and
calls is set by [the S1 decisions](#s1-decisions); what P3 itself requires of
any caller:

- **Fleet size.** Price at the variant's actual `N_p`, with `f` held at that
  count. The rate per replica is `RateForTTFT` divided by `N_p`.
- **Imbalance.** P3 refuses `NoImbalance` for more than one pod with no
  `f`, and never assumes `f` = 1 there. The `f` S1 passes, learned or
  assumed and flagged `AssumedImbalance`, is
  [decision 1](#1-the-imbalance-f).
- **Decode role.** With flow control on, the decode role must be passed:
  without it the decode onset cannot be placed, and P3 answers flagged
  `DecodeOnsetUnchecked`, possibly above that onset (S1 does not price
  then: `no-decode-side`). With flow control off and no decode role,
  decode's part is the measured decode-engine TTFT, `TTFTFleet.DecodeTTFT`
  ([the decode engine's TTFT](#3-what-s1-passes-to-ttftat-and-rateforttft)),
  flagged `DecodeEngineMeasured`; unmeasured, it is left out and flagged
  `DecodeSideOmitted` and `DecodeTTFTUnmeasured`, which always coincide.

`ols-sweep-slo` gives S1 router TTFT, `AvgTTFT`, `AvgServiceTime` and the
arrival rate: enough to fit `a` and check a low-load mean, not to fit `V_P` (no
queue), the router queue (flow control off) or a tail (no histograms).

#### S1 decisions

Settled 2026-11-04, revised 2026-11-05 after two review rounds, before S1 is
built. The model is P3's ([end-to-end TTFT](slo-capacity-planner-ttft.md), and
its "As built (P3)" section on PR #471); this section says only what S1 feeds
it and what S1 does with the answer. One rule runs through it: **only err
toward more replicas.** Where an input is unknown, S1 assumes the value that
asks for more replicas and flags it; where a flag means the answer may
over-promise, S2 does not act on it.

**Scope: prefill variants only.** P3 prices a P/D fleet; aggregated TTFT is
the what-if M1, and P1 refuses `a` on an aggregated pod. S1 prices each
prefill variant of a P/D model and publishes nothing for an aggregated one,
as TPOT publishes nothing for a prefill one. Aggregated TTFT follows M1.

**No TTFT objective, no entry.** A model whose policy sets no `slo.ttft`
target gets no TTFT entry, no TTFT series and no `slo-ttft-variant` line,
whatever its roles. The `engine` golden's P/D model is TPOT-only, so it does
not move.

##### Where P1's figures come from

P1's observer lives in the saturation analyzer, which feeds it two inputs no
other analyzer has: decode back-pressure (the `computeK2` test) and the
back-pressure bound. S1 does not run a second observer: it would lack both
inputs and keep a second copy of six hours of samples.

- **The stash.** `observePrefill` stores each variant's latest
  `prefill.Estimate` in a new map on the saturation analyzer,
  `prefillEstimates map[string]stashedEstimate`, keyed
  `namespace|model|variant` (the shape of `prefillBoundStashKey`), with
  `stashedEstimate{est prefill.Estimate; at time.Time}`. It is written under
  the saturation analyzer's `mu`, which `observePrefill` takes for the write
  only (it does not hold it today; `takePrefillBounds` takes it the same
  way).
- **Stamped with the cycle.** `at` is the saturation analyzer's `now()` at
  the write. S1 refuses an entry whose `at` is not its own `now()` this
  cycle, with reason `no-estimate`. Both analyzers read one clock: the
  engine's per-cycle clock in production (`cycleclock`, passed to both in
  `steadystate/engine.go`) and the replay's clock in `learnreplay/replay.go`,
  so the times are equal within a cycle, not merely close. An entry left
  from a cycle in which the observation did not run is therefore never read.
- **Forgotten with the variant.** The entry is deleted in the saturation
  analyzer's `forgetVariant` (an operator's
  `llm-d.ai/forget-learned-state`) and swept in
  `evictPrefillObservationLocked` when older than
  `prefillBoundsStaleAfter`, like the bounds.
- **The handle.** S1 never rebuilds the observer's key
  (`prefillObserveKey`), which reads the accelerator memo under the
  saturation analyzer's lock. It reads the stash through an interface in
  package `slo`:

      type PrefillEstimates interface {
          PrefillEstimate(namespace, modelID, variant string) (prefill.Estimate, time.Time, bool)
      }

  `*saturation.SaturationAnalyzer` implements it. It is wired with
  `(*slo.Analyzer).WithPrefillEstimates(PrefillEstimates)` in both places
  that build the SLO analyzer: `steadystate/engine.go` (passed `satV2`) and
  `learnreplay/replay.go` (passed `r.sat`). `whatifsource/capture.go`
  builds one too and passes none, so it prices no TTFT.
- **Lock order:** the saturation analyzer's `mu`, then the observer's lock;
  the SLO analyzer's own `mu` is taken only after the read, never around
  it. S1 reads the estimate before taking `slo.mu`, so an `Analyze` path
  that holds `slo.mu` never calls the handle (`sync.Mutex` is not
  reentrant), and the saturation analyzer never calls back into `slo`, so a
  read from S1 cannot deadlock a saturation cycle.
- **The same cycle's estimate.** The engine runs saturation first, always
  (`runSaturation`, before `analyzerRunEntries` in `runAnalyzers`), so S1
  reads the estimate saturation made from the same cycle's rows. **Replay
  fidelity depends on this order:** a replay that ran `slo` before
  saturation would find no entry stamped with the cycle and read
  `no-estimate`, moving the golden. The order holds in sizer mode too:
  saturation still runs there, and `eligibleEntries` only drops its result
  from the composite.
- S1 therefore needs `prefill-capacity-observe` on; with it off, no entry is
  written and every prefill variant reads `no-estimate`.

##### 1. The imbalance `f`

P3 refuses `NoImbalance` with more than one prefill pod and no `f`, and P1
learns `f` only after 5 minutes at 100 requests a pod.

**At the current count `N_p`,** S1 takes, in order:

1. P1's learned `f` **at `N_p` pods**, from minutes taken at that count only.
   P1's `F` today is a median over every count the variant has had (its
   `fSample` carries no pod count; `FPodsLast` is only the last minute's), so
   a learned `f` from three pods would be read at five. **P1 gains a
   per-count `f` in the S1 PR:** `fSample` records the minute's pod count,
   and `prefill.Estimate` gains `FByPods map[int]float64` with
   `FSamplesByPods map[int]int`, each count's median held to the same
   `MinImbalanceMinutes`. `F` keeps its meaning for the log.
2. With one prefill pod, none: P3 prices one pod as `f` = 1, exactly.
3. Otherwise an assumed `f`, flagged `AssumedImbalance`:
   `f = min(N_p, max(1.5, f_raw))`.

**`f_raw`** is the busiest pod's share of the variant's computed-token rate
over the mean share, `max_i r_i ÷ mean_i r_i`, with `r_i` the row field
`PrefillComputedTokenRate` of each Ready, settled prefill row of the cycle.
It is the largest over the last 5 cycles at the current Ready count, and
**not de-biased**. The state lives in the SLO analyzer:
`imbalance map[string]*imbalanceWindow`, keyed `namespace|model|variant`,
with `imbalanceWindow{pods int; shares []float64}`, under the SLO analyzer's
`mu`. The window is reset when the Ready count changes, forgotten in the SLO
analyzer's `forgetVariant`, and evicted by its `evictStale` after the state
timeout, like its lines. With no computed-token rate on any row, `f` is 1.5.

Why:

- **Publishing nothing** leaves most multi-pod fleets with no figure, so
  shadow would collect no predictions on the fleets S2 most needs to size.
- **`f` = 1 over-promises**, which is why P3 refuses to assume it silently.
- **The raw share errs high.** At few requests, chance alone puts the busiest
  pod above its share; P1's de-bias exists to remove that excess. A high `f`
  lowers the rate per replica, so it asks for more replicas, the permitted
  side. The raw share also catches a real sticky-routing imbalance (one pod
  winning a whole refresh window: the 439 waiting with 3 of 4 pods idle),
  which a stated constant alone would miss.
- **The floor 1.5** covers minutes in which the shares happen to look even.
  It is P3's own synthetic multi-pod case, and near P1's 2:1:1:1 example
  (1.6). It is a constant, not a policy field, until a recording argues for
  another value ([open question 5](#s1-open-questions)).
- **Capped at `N_p`**, the most one pod can carry.
- **Deriving `f` from the router's configuration** (scorer weights) was
  rejected: no formula maps weights to a share.

**At another count `N'`** (S2's search), `f` is `min(N', max(1.5, f))`, with
`f` the figure above. A learned `f` is never carried unchanged to a new
count, and one pod's exact `f` = 1 never reaches two or more pods. At
`N'` = 1 the clamp gives 1, which is exact.

**Routing-bound.** When `f ÷ N_p` ≥ 0.8, one pod takes at least 80% of the
work, and more pods would mostly sit idle behind the router. The variant is
`routing-bound`: `predicted_ttft` is still published, the per-replica rate is
not, and it makes no TTFT contribution. The fix is the router's scoring, not
replicas.

##### 2. `V_P` in production

Busy minutes fire only while a learner recording is on, because the
queue-minimum query is record-only (#470). **Decision:** S1 takes P1's
estimate as it is: busy minutes when it has them, else the peak under a
queue, else the back-pressure bound, else `no-prefill-capacity`. S1 makes no
collector change. The collector change is its own PR, **Q1** in the
[what-if PR plan](slo-capacity-planner.md#pr-plan), after #470 merges and
before S2.

Why:

- **Every source reads at or below `V_P`** on the prompts it was learned on.
  Busy minutes read at it when the pod ran flat out and under it otherwise
  (#470: data-parallel pods and stalls under-read); the other two are lower
  bounds, flagged `VPLowerBound`. A low `V_P` asks for more replicas, so S1
  need not refuse the lower bounds. The one exception is the noise of the
  peak under a queue, a max of one-minute rates, which P1 states as a limit.
- **Refusing without busy minutes** was rejected: shadow would then mean
  "only while recording", and production would never accumulate predictions.
- **The lower bounds cost replicas, not safety.** A peak under a queue taken
  at moderate load reads well below `V_P` and over-provisions. That is why Q1
  comes before S2, which spends replicas, and not before S1, which spends
  none.
- **Q1 is its own PR** because it is a collector cost, reviewable alone. It
  issues three per-instance queries every cycle, not only while recording,
  for namespaces holding a model whose policy sets a TTFT target: the queue
  minimum (busy minutes), the per-request computed-prefill-token histogram
  (`vllm:request_prefill_kv_computed_tokens_bucket`, the
  [measured spread](#the-prompt-spread)), and the KV transfer time
  (`nixl_xfer_time_seconds`, the [transfer term](#3-what-s1-passes-to-ttftat-and-rateforttft)).
  The queue minimum and transfer time are R1 fields recordings already
  carry; the computed-token histogram is new. **Q1 is a hard gate for S2**:
  without it S2 sees only assumed spreads and omitted transfers, on neither
  of which it acts.
- **`V_P` depends on the prompts it was learned on.** Prefill throughput per
  token falls as prompts lengthen (attention grows with the prompt): run 2
  read 135k tok/s at 30k-token prompts and 118k at 37k
  ([the recordings](#the-p2-recordings)), 13% lower for 1.23 times the
  prompt. So a `V_P` learned on short prompts over-reads on long ones and
  over-promises. P1 records the mean effective prompt behind `V_P` in a new
  field, `prefill.Estimate.VPPromptTokens`: the mean of P1's
  `effectivePrompt` (`PrefillComputedTokenRate ÷ RequestRate`) over the
  minutes `V_P` rests on:
  - busy minutes: the busy minutes inside `BusyWindow` whose median is `VP`;
  - peak under a queue: the `PeakRun` minutes of the run whose least minute
    is the peak;
  - back-pressure bound: the bound's readings, through a new
    `prefill.Bound.PromptTokens` the saturation analyzer fills from the same
    rows. Until it does, the field is 0.

  When the current mean differs from `VPPromptTokens` by more than **1.2x**
  either way, or `VPPromptTokens` is 0, S1 flags `VPShapeMoved`. Published;
  S2 does not act on it. **Why 1.2x:** taking the per-token prefill time as
  linear in the prompt through run 2's two points (7.39 µs a token at 30k,
  8.50 µs at 37k: 135k and 118k tok/s, from the run's `levels.txt`, 4.51
  and 3.18 req/s with a queue on every sample), a prompt 1.5x longer than
  `V_P` was learned on reads `V_P` about 32% high inside the threshold
  (round 2 said a quarter, from the two points' ratio alone), and 1.2x
  longer about 13% high. The threshold is the largest over-read let through
  unflagged, so 1.2x accepts about 13% where 1.5x accepted about 32%. Two
  points fit no curve, and the linear extrapolation is an assumption; run 1
  adds points.
  A shorter prompt than `V_P` was learned on under-reads `V_P`, the safe
  side; it is flagged too, so a shape change is visible either way.

  **With a curve, P3 rescales instead of holding**
  ([P3 after P2](slo-capacity-planner-ttft.md#as-built-p3-after-p2)). When
  P1 took `a` from the quadratic (`AModel` quadratic) and `VPPromptTokens`
  is known, `PrefillFromEstimate` passes `b` and `c`, and P3 prices
  `V_P(I) = V_P(I_vp)·s(I_vp)/s(I)`, `s(I) = b + c·I`, for a longer prompt,
  flagged `VPScaledToPrompt`. It holds `V_P` for a shorter one, which is the
  safe side. The rescale reads at or under the true `V_P` only if P1's
  low-load `b` and `c` are the busy step's per-token cost, which is not
  measured. On run 2's 37k level it read 1.5% above what the pod served, and
  the margin covers that only from `ρ` 0.6. So `VPScaledToPrompt` is on
  S2's no-act list until a second measured point confirms the rescale. The
  1.2x rule
  is now P3's own fallback for when there is no curve: P3 raises
  `VPShapeMoved` itself (`whatif.MaxVPShapeRatio`), and S1 no longer
  computes it. S1 passes `VPPromptTokens` and the curve through
  `PrefillFromEstimate` and copies P3's flag. On run 2's 37k level the
  rescale reads 117,645 tok/s against the 115,953 served; held at 30k's
  figure, it read 134,775.
- **P1's estimate is not persisted** (the plan's "Later" row). After a
  controller restart every prefill variant reads `no-prefill-capacity` until
  a pod queues again, and makes no TTFT contribution.

##### 3. What S1 passes to `TTFTAt` and `RateForTTFT`

One `whatif.TTFTFleet` per prefill variant, from one pure function, so that
S2 calls it at other counts and the S1 tests call it directly:

    // internal/engines/analyzers/slo/ttft_fleet.go
    func ttftFleet(in ttftInputs, pods int) (whatif.TTFTFleet, []whatif.Flag)

`ttftInputs` holds the cycle's figures (the estimate, `f`'s state, the shape,
the decode variant, flow control); the returned flags are S1's own, added to
P3's on the answer.

| Input | From |
| --- | --- |
| `V_P`, `a` and their flags; `VPPromptTokens`, the curve's `b` and `c`, and the margin | P1's stashed estimate, through `whatif.PrefillFromEstimate`, which carries `VPPromptTokens` and, with the quadratic, `ASlope` and `ACurve`, and sets `VPMargin` to `DefaultVPMargin` |
| `f` | [Decision 1](#1-the-imbalance-f), at `pods` |
| `N_p` | `pods`: the variant's Ready replicas for S1, each `N'` for S2. Counted by `prefill.Serving` (Ready, not a warm-pool bridge), the rule P1 counts `FByPods` by, so `f` is read at the count it was learned at |
| `PromptTokens` | The effective prompt, `E[I_eff]` (prefix hits are not prefill work), as P1 reads it |
| `PromptCV` | Measured from the computed-prefill-token histogram where a row carries it (Q1); otherwise 2, flagged `AssumedPromptSpread` ([below](#the-prompt-spread)) |
| `ArrivalCV2` | 0, Poisson (P3's open question 5), flagged `AssumedPoissonArrivals`: an S1 flag, since P3 has none for it. Poisson over-promises on bursty traffic. Lifted by the staircase's bursty bucket ([which bucket lifts which flag](slo-capacity-planner-ttft.md#ground-truth-and-the-staircase)) |
| `TransferSecondsPerToken` | Measured: the mean of the R1 field `AvgKVTransferTime` over Ready decode rows with `AvgKVTransferTimeKnown`, divided by `E[I_eff]`. Run 2 recorded it on 374 of 386 decode rows. `TransferOmitted` (0 passed) only where no decode row carries it, which in production is until Q1 issues the query. With no decode line and `DecodeTTFT` measured, P3 does not price it (the decode engine's TTFT holds the pull) |
| `DecodeTTFT` | With no decode line only: the decode variant's measured `AvgTTFT` and the prompt it was read at ([below](#3-what-s1-passes-to-ttftat-and-rateforttft)); 0 when no row carries it (`DecodeTTFTUnmeasured`) |
| `Decode` | The model's decode variant, from the inputs S1 already prices TPOT with: its line, band, capacity tokens, `KVreq`, `O`, batch cap times ranks, and Ready replicas `N_d`. Only when the model has exactly one decode variant and its line prices (`own` or `pinned`; not `pinned-default`, `no-line` or `declined`) |
| `FlowControl` | The tier's `slo.flowControl` when it is set ([below](#the-flow-control-state)): `enabled` passes `FlowControlOn`, `disabled` passes `FlowControlOff`. Unset, it is inferred from the series: on when the router's flow-control queue series exists for the model, `FlowControlNoSeries` (flagged `FlowControlInferredOff`) otherwise. On always carries the upstream detector thresholds 5 and 0.8, which this repository's install leaves unchanged, so P3's `FlowControlUnknown` cannot arise |
| The rate | The model's arrival rate times the variant's share of the Ready prefill replicas |
| The target | `slo.ttft.target`, and `slo.ttft.percentile` ÷ 100 |

**The decode side.** With flow control on and no decode side, S1 does not
price, and the reason is `no-decode-side`: without decode, P3 cannot place
the decode onset, above which TTFT is not priced at all (P3's review). P3
answers such a fleet flagged `DecodeOnsetUnchecked`, and its "What S1 calls"
note says S1 must pass the decode role there; S1 never publishes such an
answer. With flow control off and no decode line, S1 prices prefill with
the decode engine's measured TTFT in place of decode's part, flagged
`DecodeEngineMeasured`: published. That flag is informational and not on
S2's no-act list; the answer leaves out `W_decode` and holds the decode
term at its mean and at the rate it was measured at, biases the TTFT page
states, and S2 is held back from it by `UnvalidatedTTFT` and the gate items
([6](#6-the-p2-recordings-and-what-they-must-show), item 4 fails today),
not by a flag. Unmeasured, it is flagged `DecodeSideOmitted` and
`DecodeTTFTUnmeasured`, which are no-act.

**The decode engine's TTFT** (`TTFTFleet.DecodeTTFT`, read by P3 only with no
decode line). The R1 field `AvgTTFT` on the decode variant's Ready decode
rows with `AvgTTFTKnown`, taken as the mean over the rows weighted by
`RequestRate`, is `DecodeTTFT.Seconds`. The rows' mean effective prompt
(the prefill variant's `E[I_eff]` in the same cycle) is
`DecodeTTFT.PromptTokens`. It is learned per decode variant in the SLO
analyzer, as `decodeTTFT map[string]decodeTTFTWindow` keyed
`namespace|model|variant` under its `mu`: the last 5 cycles' means, each
kept with its cycle's prompt, of which S1 passes the largest as a pair
(`Seconds` and `PromptTokens` from the same cycle, never the largest of
each). `PromptTokens` is `E[I_eff]`, the computed prompt, while the KV pull
moves the whole prompt, prefix hits included, so the term's scaling by
`I/I_D` tracks the pull only while the hit rate holds. The figure is held
at the rate it was measured at: `RateForTTFT` extrapolates to other rates
with it frozen. It is forgotten and evicted with the
variant's lines. `AvgTTFT` is a scaling query, collected every cycle, so
production has it. With no row carrying it, S1 passes 0, and P3 flags
`DecodeTTFTUnmeasured`, which is on S2's no-act list. On P/D the decode
engine's TTFT is its own clock, the KV pull and the first token, so it
replaces `T_xfer`: P3 does not price `TransferSecondsPerToken` beside it.

###### The flow-control state

P3 prices flow control on and off differently, and with the gate off its
series do not exist, so S1 cannot tell "off" from "not scraped". **Decision:**
a new tier field, `slo.flowControl`, `enabled` or `disabled`, is the positive
source when it is set:

```yaml
slo:
  ttft: {target: 3s, percentile: 90}
  flowControl: disabled        # the EPP's flowControl feature gate, as deployed
```

- **Set:** S1 passes what it declares, and no inference flag is raised.
- **Unset:** S1 infers the state from the series. An inferred "off" is
  flagged `FlowControlInferredOff`, which stays on S2's no-act list. **So on
  a default install S2 does not act until the operator declares the state.
  This is intended:** a scrape gap reads as "off", and pricing an on gate as
  off misses the decode onset, above which TTFT is not priced at all. The
  safe side is to wait for the operator.
- **Checked against the router.** Declared `disabled` while the router's
  flow-control queue series exist for the model is a contradiction: S1 logs
  a warning, prices as declared, and flags `FlowControlMismatch`, which is
  no-act. Declared `enabled` with no series for the model is treated as the
  same mismatch. That is the right reading once the model has had a request:
  an on gate then publishes the series at 0 while the queue is empty, on 652
  cycles of the staircase's on phases and with none missing, up to 82 s after
  the load ended; longer idle is not measured. Before the router's first
  request the series does not exist (4 cycles of each on phase), so a router
  with the gate on that has served nothing reads as this mismatch
  ([as built, M2](slo-capacity-planner-ttft.md#as-built-m2),
  `TestGoldens_TTFTStaircaseFlowControlSeries`).
- **Validation** is the `slo` block's: a value other than the two drops the
  tier entry, as a bad duration does. The field is in `config.SLOPolicy`,
  `FlowControl SLOFlowControl \`yaml:"flowControl,omitempty"\``, with
  `SLOFlowControlEnabled` and `SLOFlowControlDisabled`; empty is unset. It is
  replaced whole with the `slo` block by an overlay. The detector's
  thresholds stay at the upstream defaults; an override is open question 4.

**The transfer term** is taken as per request. If `nixl_xfer_time_seconds`
is per chunk (the TTFT page's open question 3), a request's transfer is
several of them and the mean under-reads, which over-promises; the staircase
settles it ([open question 8](#s1-open-questions)).

###### The prompt spread

**The spread P3 needs is the effective prompt's**, `I_eff`, the tokens a pod
computes after prefix hits, not the raw prompt's. The raw histogram is
`vllm:request_prompt_tokens`, per instance (the R1 field
`PromptTokensBuckets`).

**Decision:** Q1 measures `cv_eff` directly from vLLM's per-request
computed-prefill-token histogram, `vllm:request_prefill_kv_computed_tokens`,
per instance, the series P1 already reads `V_P` from (as `_sum`): it is
`I_eff` per request. An earlier revision derived `cv_eff = cv_raw ÷ (1 − h)`
from the raw histogram and the prefix hit rate `h`. That was dropped, because
hits that vary by request break it in the unsafe direction: 2000-token
prompts of which half hit 1800 tokens give `I_eff` of 2000 or 200, a `cv` of
0.82, while the raw `cv` is 0 and the formula gives 0.

**From coarse buckets:**

- The mean is exact, `_sum ÷ _count`.
- The second moment uses each bucket's **upper bound**, `Σ n_b·u_b² ÷ Σ n_b`,
  so `cv² = E_u[I²] ÷ mean² − 1` reads high, never low. vLLM's token buckets
  are coarse (1-2-5 steps), so it can read well high: constant 3000-token
  prompts read about `cv` 1.3 in the 2000-5000 bucket. That costs replicas,
  not safety.
- Mass in the `+Inf` bucket has no upper bound: `cv` is set to 3 and flagged
  `PromptSpreadClamped`. So is any measured `cv` above 3, where the p90
  stops being monotone (below).
- **Pods are pooled:** each bucket's rate is summed over the variant's Ready
  pods, and `cv` is computed once from the pooled counts. The worst pod's
  spread is not what P3's per-pod queue takes, but with routing that mixes
  prompts across pods the pooled figure is that spread, and `f` carries the
  imbalance.

**The assumption.** Without the histogram, S1 assumes `cv` = 2, not 1, flagged
`AssumedPromptSpread`. A gamma prompt at `cv` 1 is exponential; heavy-tailed
prompts (a few long documents among short chat turns) sit above it, and a
`cv` too low under-states both the queue (`Cs²` = `cv²`) and the request's
own prefill tail, so it over-promises TTFT and asks for too few replicas.

**Checked before relying on it:** does a larger `cv` always price a longer
TTFT at p90 and above? No test on #471 asserts it (its property cases vary
the rate and the replicas, not `cv`), so it was scanned on #471's round-1 tip
(`f12de0dcf`, `engineWait`): `TTFTAt` on the prefill side, mean prompt 1000
and 8000 tokens, `a` 5 and 30 ms, 1 and 4 pods, at 0.05, 0.3, 0.6 and 0.85 of
the knee, p90, p95 and p99, `cv` 0, 0.5, 1, 1.5, 2, 2.5, 3, 4, 5, 7 and 10:
96 cells, 1056 priced points.

- **Up to `cv` 3 it holds** in all 96 cells: TTFT rises with `cv` at every
  percentile and load, on that grid.
- **Above 3 it does not, at p90 and low load** (8 of the 96 cells, all of
  them p90 at 0.05 of the knee). There the p90 peaks near `cv` 3 and then
  falls (8000 tokens, `a` 5 ms: 1.131 s at `cv` 3, 1.118 at 4, 0.072 at 10).
  The request's own prefill dominates there, and a gamma of shape `1/cv²`
  puts ever more mass near zero, so its p90 shrinks. At 0.3 of the knee and
  above, and at p95 and p99 everywhere, it still rises.

So the claim is true only over a range, and the value is chosen as the one
that maximises the priced TTFT over the plausible range. Over [0, 3] that is
the range's top, because TTFT is monotone there. **The plausible top is taken
as 2**, not 3: it is the top of the spread P3's property cases draw, a gamma
at `cv` 2 already puts the p99 prompt near 10 times the mean, and `cv` 3 would
roughly double the predicted TTFT at 0.6 of the knee (0.47 s to 0.93 s at
1000 tokens, p90), spending replicas on a spread no measurement supports. The
2 is not measured: run 1's histograms are the first evidence, and Q1
replaces the assumption in production. No `cv`, assumed or measured, is
passed above 3.

**The scan's limits.** It called `TTFTAt` only, on the prefill side only (no
decode role, flow control off, Poisson arrivals, `f` = 1), at the grid points
listed. It did not call `RateForTTFT`, so the claim that a larger `cv` never
raises the rate at the target is inferred from TTFT rising at every rate,
not checked. It was a throwaway test, not committed; the S1 PR adds the
property case ([tests](#the-s1-prs-tests)).

**Two calls per variant, two roles:**

- `RateForTTFT(fleet, target)`, at the current count, **sets the reason**
  and the per-replica rate: the fleet's rate at the target ÷ `N_p`, with `f`
  held. Its refusals, its bound and its flags are what S2 reads.
- `TTFTAt(fleet, rate, q)`, at the current rate, is **the shadow
  prediction** scored against the router, and says whether the target is met
  now. A refusal here (`Saturated` at or over the prefill knee,
  `AboveDecodeOnset`) only blanks `predicted_ttft`: the current rate is past
  what the fleet serves at any TTFT, so the target is **unmet**, which is a
  reason to grow, not to hold. `AboveFlowControlOnset` on this call is also
  unmet, not a hold. S2 then grows from `RateForTTFT`'s answer, whose own
  flags decide whether it may.

**Decode-bound only when decode materially sets the rate.** P3 flags
`HeldByDecodeAdmission` whenever the prefill side alone allows a higher rate
than the full fleet, which any `W_decode` > 0 makes true. Read as a hold,
that would stop S2 growing prefill on almost every P/D fleet. So the variant
is `decode-bound` only when the rate answer is bound by the decode onset, or
the prefill side's own rate (`Basis.TTFT.PrefillOnlyRate`) exceeds the full
rate by more than 10%. Otherwise S1 publishes the per-replica rate as
TTFT-bound, at the full rate (the lower of the two), so decode's share of the
tail is still counted. When `decode-bound`, the per-replica figure is absent
and `predicted_ttft` stays: more prefill pods cannot raise a rate decode
sets, and dividing that fixed rate by more pods would ask for more pods every
cycle. This is the superseded design's blame rule (a miss with decode at its
ceiling is decode's), kept. The 10% is a stated threshold, not a measured
one.

With a decode side, the rate is not proportional to `N_p` (`W_decode` depends
on the whole rate), so the per-replica figure holds at the current count only.

##### 4. What S1 publishes

Behind the replay switch `slo.SwitchTTFTPrice = "slo-ttft-price"`, in
`internal/engines/analyzers/slo/ttft.go`, on by default like the other
observe switches.

**What keeps it out of a replica count.** The guard is the engine, not the
analyzer's result: `eligibleEntries` (`steadystate/engine_v2.go`) drops the
`slo` analyzer's result from the composite unconditionally, so nothing `slo`
returns decides a count today. S1 still leaves its result unchanged (a
prefill variant's `VariantCapacity` stays no-data, `RoleDemand` gains no
`prefill`), because that result is published as `wva_analyzer_*`
(`wva_analyzer_demand`, `wva_analyzer_target{analyzer_name="slo"}`) and
dashboards read it; a shadow figure there would read as a decision. **S2 needs
an enforce-path change:** `eligibleEntries` keeps `slo` under
`slo.mode: enforce`, and only then does the result carry prefill demand.

New series, all per prefill variant, labelled `namespace`, `model_name`,
`variant_name` and `statistic`. **One convention:** `statistic` (`p90`), as
the per-model series use it. No `objective` label: every series here is TTFT
by name.

| Metric | Labels beyond those | Meaning |
| --- | --- | --- |
| `wva_slo_predicted_ttft_seconds` | — | `TTFTAt` at the variant's current rate, at the objective's percentile. Present when priced and when `decode-bound` or `routing-bound`; absent when `TTFTAt` refuses (the target is then unmet, `wva_slo_ttft_unmet`). On P2's goldens it reads low at light load, up to about 34% under the router's p90: not a bound |
| `wva_slo_ttft_unmet` | — | `1` when the current rate misses the target: the prediction is above it, or `TTFTAt` refused at or over the prefill knee or the decode onset, or the current rate is above the prefill flow-control onset; `0` when met; absent when not priced |
| `wva_slo_ttft_replica_requests_per_second` | — | `RateForTTFT` ÷ `N_p` at the current count. Absent unless the reason is `priced` |
| `wva_slo_ttft_vp_source` | `vp_source` | `1` for where `V_P` came from (`busy-minutes`, `peak-under-queue`, `back-pressure-bound`, `none`), `0` for the others. Present whenever the variant is evaluated |
| `wva_slo_ttft_reason` | `reason` | `1` for the reason from `RateForTTFT` at the current count, `0` for the others. `priced`; S1's own: `no-estimate`, `no-decode-side`, `decode-bound`, `routing-bound`; and P3's refusals in kebab case: `no-prefill-capacity`, `no-fixed-latency`, `no-imbalance`, `no-prefill-replicas`, `no-decode-replicas`, `no-capacity`, `no-batch-cap`, `above-decode-onset`, `saturated`, `target-below-floor`, `no-traffic`, `too-many-slots`, `input-invalid`. `flow-control-unknown` is not among them: S1 always passes thresholds, so P3 cannot return it |
| `wva_slo_ttft_flag` | `flag` | `1` for each flag on the rate answer, `0` for the others |

The flag series zero-fills a fixed list, `constants.SLOTTFTFlags`, in this
order: P3's TTFT flags (`UnvalidatedTTFT`, `AssumedVP`, `VPLowerBound`,
`VPAboveSlopeBound`, `AssumedImbalance`, `AssumedPromptSpread`,
`TransferOmitted`, `DecodeSideOmitted`, `DecodeTTFTUnmeasured`, `DecodeEngineMeasured`, `VPScaledToPrompt`, `VPShapeMoved`, `FlowControlInferredOff`,
`AboveFlowControlOnset`, `HeldByDecodeAdmission`, `DecodeOnsetUnchecked`),
the decode flags a TTFT answer can carry (`BelowFittedBand`,
`KVBoundSlots`), and S1's (`AssumedPoissonArrivals`,
`PromptSpreadClamped`, `FlowControlMismatch`). `AssumedVP` and `DecodeOnsetUnchecked` always read 0
under S1, which assumes no `V_P` and publishes no unchecked answer; they are
listed so the series set does not change when that does. `UnvalidatedTTFT`
reads 1 until the staircase passes. `wva_slo_source` keeps its labels and
stays TPOT's.

**The operator join.** The prediction is per variant and the router's figure
per model. With one prefill variant per model, compare them on the model and
statistic:

    max by (exported_namespace, model_name, statistic) (wva_slo_predicted_ttft_seconds)
      / on (exported_namespace, model_name, statistic)
    max by (exported_namespace, model_name, statistic) (wva_slo_observed_seconds{objective="ttft"})

With several prefill variants, `max` takes the worst variant's prediction
against the model's mixed figure ([open question 6](#s1-open-questions)).

**The cycle log** gains one line per prefill variant, `slo-ttft-variant`, at
the verbose level: the variant, reason, target and statistic, the rate,
`N_p` and `unmet`; P3's figures (the bound, the independent and pooled
figures, the floor, `β`, `ρ_w`, `θ`, the fleet's rate, the per-replica rate,
the prefill side's own rate, what bound it, the knee, the onsets) and flags;
the inputs (`vp`, `vpSource`, `vpPromptTokens`, `estimateAt`, `a`, `f`,
`fSource` as `learned`, `one-pod` or `assumed`, `fRaw`, the prompt and its
`cv` with its source, the transfer per token, the decode variant or none, the
flow-control state); and both calls' refusals.

**The golden.** The snapshot gains one field:

    // internal/metrics/slo_metrics.go
    type SLOModelSnapshot struct {
        ...
        TTFT []SLOTTFTReading `learnrec:"omitzero"`
    }

    type SLOTTFTReading struct {
        Variant, Statistic   string
        Predicted, PerReplica float64 // NaN when absent
        Unmet                bool
        VPSource, Reason     string
        Flags                []string
    }

The recording codec (`internal/signals/learnrec`) reads its own tag, not
`json`: a field tagged `learnrec:"omitzero"` is left out while it holds its
zero value, and a nil slice is zero. S1 leaves `TTFT` nil with the switch
off, so `slo-published.csv` is byte identical off. With the switch on, it
moves only in goldens with a TTFT target and a prefill variant:
`ols-sweep-slo` and `ols-sweep-slo-restart`. The `engine` golden's P/D model
is TPOT-only and does not move. In the two that move:

- P1 refuses `V_P` (one prefill pod that never queued), so the prefill
  variant reads `reason="no-prefill-capacity"` and `vp_source="none"`.
- The back-pressure bound does not fire: both recordings run with
  `EnablePrefillThroughputBound: false` (166 and 286 recorded policies, all
  false).

P2's goldens are the first priced ones.

**The switch table** (`learnreplay`'s switch runs) declares:

| Switch | Moves | Note |
| --- | --- | --- |
| `slo-ttft-price` | `slo-published.csv` in `ols-sweep-slo` and `ols-sweep-slo-restart` | Off: byte identical |
| `prefill-capacity-observe`, with `slo-ttft-price` on | `slo-published.csv` in the same two | Off: no estimate is stashed, so every prefill variant reads `no-estimate` in place of `no-prefill-capacity` |

The second row is **documentation only** until the switch-run harness can run
"X off with Y on": today it turns one switch off with the others at their
defaults. The S1 PR adds that pairwise run, so the row is checked rather than
asserted.

##### 5. S2, as a sketch

- **The search.** S2 evaluates every `N'` from 1 upward to `N_p + cap`,
  with `cap = MaxReplicas − N_p` when the variant sets `MaxReplicas`
  (`domain.VariantMetadata`), else `4·N_p`. At `N'` = `N_p` it reuses S1's
  answer; at each other `N'` it calls `ttftFleet(in, N')`, which sets `f` to
  `min(N', max(1.5, f))`. It takes **the least `N'` whose `RateForTTFT` meets
  the variant's rate**, without assuming the rate rises with `N'`. It
  starts at 1, not `N_p`: a search from `N_p` would pin the TTFT floor at the
  current count while the target is met, and prefill could never scale in.
  The search stops at the first `N'` whose answer is `decode-bound`: that
  `N'` is taken if no smaller one met the rate, since more prefill pods
  cannot help. If no `N'` meets the rate, the answer is `N_p + cap`,
  flagged. **The cost:** at most `N_p + cap` `RateForTTFT` calls per variant
  per cycle, each a bisection over P3's model; S2's PR
  measures the time on its largest fleet.
- **With the demand floor.** The TTFT count is one more floor: the prefill
  role takes the larger of the floor's count and the TTFT count, never less.
  In guard mode the composite's larger-of rule does this, once
  `eligibleEntries` keeps `slo` under enforce
  ([decision 4](#4-what-s1-publishes)); the need reaches it as demand at
  saturation's per-replica capacity (composite item 5).
- **Where S2 does not act.** "Does not act" means **no TTFT contribution**
  that cycle: the TTFT count is absent, not held at the current count, so
  the demand floor and the other analyzers still size prefill, and it can
  still scale in. S2 does not act on a rate answer with:
  - a reason other than `priced`: `decode-bound`, `routing-bound`,
    `no-decode-side`, `no-estimate`, and every refusal;
  - `DecodeSideOmitted` and `DecodeTTFTUnmeasured`, which coincide (no
    measured decode-engine TTFT). `DecodeEngineMeasured` is not on the list;
    a measured term is gated by `UnvalidatedTTFT` and the gate items;
  - `AssumedPromptSpread` or `PromptSpreadClamped`, so Q1 is a hard gate;
  - `PromptSpreadAbovePeak`, a `cv` above the peak for the percentile, where a
    larger `cv` prices a higher rate ([as built](#as-built-s1));
  - `VPShapeMoved`, `VPAboveSlopeBound` or `VPScaledToPrompt` (the rescale
    read 1.5% above the served `V_P` on run 2 at 37k; lifted when a second
    measured point confirms it);
  - `TransferOmitted`, raised only when no transfer time was measured;
  - `AssumedPoissonArrivals`, until the staircase's bursty bucket passes;
  - `AboveFlowControlOnset` on the rate answer, where the per-pod model is
    not proven conservative;
  - `FlowControlInferredOff`, so S2 waits for a declared
    [`slo.flowControl`](#the-flow-control-state), and `FlowControlMismatch`.

  Which staircase bucket lifts which flag is one table, on the
  [TTFT page](slo-capacity-planner-ttft.md#ground-truth-and-the-staircase).
  `UnvalidatedTTFT` is not on the list only because S2 does not ship until
  the staircase removes it. A refused or flagged figure at the **current
  rate** is not on the list: it means the target is unmet, and S2 grows.
- **Bound 6** applies to the per-replica rate: it may fall at once, and
  rises only after holding for the scale-down stabilization window.

What S1 keeps stable for this: `ttftFleet` as a function of the pod count,
the series, reason and flag names, and the no-act list above.

##### 6. The P2 recordings, and what they must show

###### The P2 recordings

P2's goldens are three prefill-stress runs, all with a learner recording on a
build with the R1 fields:

| Run | Fleet | What it is for |
| --- | --- | --- |
| 1 | 3 prefill + 3 decode | R1 coverage: every TTFT recorder field, on several prefill pods; the source for `f` and for the first measured prompt spread |
| 2 | 1 prefill + 2 decode | Prefill-bound, so busy minutes fire. `V_P` read 135k tok/s at 30k-token prompts and 118k at 37k. `AvgKVTransferTime` on 374 of 386 decode rows |
| 3 | open loop, for `a` (running now) | Low-load minutes with spread prompts, which a prefill-bound run lacks, so P1 fits `a` |

Before S1 leaves shadow, replayed with `slo-ttft-price` on, they must show:

1. **The inputs learned.** `a` is fitted on run 3, which needs low-load
   minutes with prompts spanning 1.5x (P1's rule); a prefill-bound run reads
   `no-fixed-latency` throughout. On run 2, `V_P` comes from busy minutes,
   below P1's `slopeBound`, with `VPPromptTokens` tracking the prompt.
2. **The prefill side is what binds** on run 2: no `decode-bound` (the 10%
   rule of decision 3) at its rates, or it tests decode.
3. **The wait tail.** The per-pod `request_queue_time_seconds` histogram at
   or below P3's `ρ_w·e^(−t/θ)` at the target's tail (P3's check 2).
4. **The prediction against the router, in the TTFT domain.** The runs
   score `TTFTAt` at the observed rate, so the mark is on TTFT, not rate. In
   judged windows below the knee, at p90 and p95,
   `wva_slo_predicted_ttft_seconds` against `wva_slo_observed_seconds`: a
   prediction **more than 10% below** the observed TTFT fails (it
   over-promises); **more than 25% above** fails (it over-provisions). The
   staircase keeps its rate-domain mark (a predicted rate at the target more
   than 10% above the measured one fails, more than 25% below fails); the
   two have the same direction, since an under-predicted TTFT is an
   over-predicted rate. **It currently fails:** on P2's goldens 9 of 14
   light-load points at the sent spread are more than 10% under
   ([P3 after P2](slo-capacity-planner-ttft.md#as-built-p3-after-p2)).
5. **The mean residual.** The sum of the parts' means against the router's
   mean TTFT leaves a residual of at most **10 ms**. Source: none measured.
   The router's clock starts at the EPP's request headers, so the residual is
   the EPP's scheduling and the hop to the pod, not the proxy before the EPP.
   The only related figure in this repository is the engine-side overhead
   inside `a`, at least 2.5 ms on `ols-sweep`
   ([what is derivable](slo-capacity-planner-ttft.md#what-is-derivable-today)),
   which is already in the parts. 10 ms is a stated budget, four times that.
   Run 3 measures the real figure at low load (router mean TTFT less the
   engine's `AvgTTFT`), and the budget is replaced by it.
6. **`f` on run 1**: the per-count `f` at 3 pods, against the busiest pod's
   measured share, and whether the floor 1.5 holds.
7. **The goldens.** The three runs are committed as P2's goldens; run 2's
   `slo-published.csv` prices the prefill variant from `busy-minutes`.

**What they cannot show:** the flow-control onsets, bursty arrivals, and a
fleet resized under load. S2 stays gated on the staircase (`N_p` = 2, with
its bursty bucket), as the table says; P2 is necessary, not sufficient.

##### The S1 PR's tests

Each is a unit or property case that fails on `main` (where none of this
exists) and passes on the PR; each switch is also held by the golden runs.

1. The stash: an entry is written per prefill variant by `observePrefill`,
   stamped with the cycle; S1 refuses an entry from another cycle
   (`no-estimate`); `forgetVariant` and `evictPrefillObservationLocked`
   delete it.
2. Replay order: a replay with `slo` run before saturation reads
   `no-estimate` (a guard against reordering).
3. Lock order: a concurrent test runs saturation cycles and S1 reads, with
   no `slo.mu` held across the read, under `-race` without deadlock.
4. `f` per count: `FByPods` keeps counts apart; a learned `f` at 3 pods is
   not read at 4; at a count with too few minutes, the assumed `f` is used.
5. `f_raw`: the raw share, the 5-cycle max, the reset on a count change, the
   floor 1.5, the clamp to `N_p`, and one pod giving no `f`.
6. `f` at `N'`: `min(N', max(1.5, f))`, never 1 at `N'` ≥ 2.
7. `routing-bound` at `f ÷ N_p` ≥ 0.8, with `predicted_ttft` present and no
   per-replica rate.
8. `VPPromptTokens` per source, passed through `PrefillFromEstimate` with the curve; P3's `VPShapeMoved` (no curve, above 1.2x either way, not at or below it, and at 0) and `VPScaledToPrompt` copied to the flag series.
9. The measured `cv`: upper-bound second moment reads at or above the true
   `cv` on a bucketed sample; pooled over pods; `+Inf` mass and `cv` > 3
   give 3 and `PromptSpreadClamped`; the 2000/1800 counterexample reads 0.82
   or more.
10. The assumed `cv` 2 with `AssumedPromptSpread`.
11. Monotonicity in `cv` over [0, 3] for both `TTFTAt` (rising) and
    `RateForTTFT` (falling), as a property case on seeded fleets.
12. The transfer term from `AvgKVTransferTime`, and `TransferOmitted` only
    with none measured.
13. `slo.flowControl`: parsed and validated (a third value drops the
    entry); declared wins over the series; unset and no series flags
    `FlowControlInferredOff`; declared `disabled` with series, and
    `enabled` without, warn and flag `FlowControlMismatch`.
14. The decode side: `no-decode-side` with flow control on and no decode;
    with it off, `DecodeEngineMeasured` when the decode engine's TTFT is
    measured and `DecodeSideOmitted` with `DecodeTTFTUnmeasured` when it is
    not; one decode variant only.

15. `decode-bound` at more than 10% and not at 10% or less; at the decode
    onset.
16. Current-rate refusals (`Saturated`, `AboveDecodeOnset`) and
    `AboveFlowControlOnset` set `unmet` and blank only `predicted_ttft`; the
    reason comes from `RateForTTFT`.
17. The reason set: every listed reason reachable, `flow-control-unknown`
    not.
18. The flag series zero-fills `constants.SLOTTFTFlags` exactly.
19. No TTFT objective, no entry and no series.
20. The snapshot: `TTFT` nil with the switch off; the `learnrec` encoding of
    a snapshot is byte identical to `main`'s with it off.
21. S2's search (in S2's PR, listed so S1's `ttftFleet` serves it): from 1,
    the least `N'` meeting the rate below `N_p` when the target is met with
    slack, so the floor can fall.
22. The pairwise switch run: `prefill-capacity-observe` off with
    `slo-ttft-price` on, and the switch table rows held.

##### S1 open questions

1. **Several decode variants** behind one prefill variant: which decode side?
   *Recommend:* none, priced prefill alone with flow control off and
   `no-decode-side` with it on, until a fleet needs it.
2. **Burstiness.** Poisson over-promises on bursty traffic (P3's open
   question 5). Flagged `AssumedPoissonArrivals`; lifted by the staircase's
   bursty bucket. *Open:* estimate `Ca²` from arrivals instead, so the flag
   is not needed.
3. **Sizer mode: closed.** Saturation runs in sizer mode too
   (`runSaturation` is unconditional; `eligibleEntries` drops only its
   result), so P1's observer runs and the stash is written there.
4. **Flow control off is never known: closed.** The tier declares it,
   [`slo.flowControl`](#the-flow-control-state); unset, an inferred off is
   no-act, so S2 waits for the operator, by design. Still open: the
   detector's thresholds when an install overrides them, a further policy
   field or the EPP's configuration.
5. **The `f` floor 1.5** is not measured. *Recommend:* set it from run 1.
6. **Several prefill variants per model.** The router's TTFT is per model,
   so a per-variant prediction cannot be scored one to one. *Recommend:*
   score the request-weighted mixture of the variants' predicted tails.
7. **The `VPShapeMoved` threshold: closed, 1.2x** ([decision 2](#2-v_p-in-production)),
   and now P3's own fallback. With P1's curve (#475), P3 rescales `V_P`
   along it in place of flagging
   ([P3 after P2](slo-capacity-planner-ttft.md#as-built-p3-after-p2)); the
   flag stands only where there is no curve.
8. **Transfer per request or per chunk** (the TTFT page's open question 3):
   per chunk, the measured mean under-reads.
9. **Scale-in: closed.** S2 searches from 1 ([decision 5](#5-s2-as-a-sketch)),
   at most `N_p + cap` `RateForTTFT` calls per variant per cycle.

##### As built (S1)

Built in `internal/engines/analyzers/slo/ttft.go` and `ttft_fleet.go`, behind
`slo.SwitchTTFTPrice`, as the decisions above say. The analyzer's result is
what it was (`TestTTFT_TheResultDoesNotMove`), and `steadystate/runAnalyzers`
is unchanged. The prediction **is not a bound**, and which way it errs depends
on the prompt spread, on the 14 light-load points of [P3 after
P2](slo-capacity-planner-ttft.md#as-built-p3-after-p2), 7 each at p90 and p95:

| Spread | Result | Worst |
| --- | --- | --- |
| Assumed `cv` 2, all S1 has today | Reads **high**: over the mark at 13 of 14 (6 of 7 at p90, 7 of 7 at p95) | 3.15x at p90, 4.13x at p95 |
| The spread sent, `cv` 0.058 | Reads **low**: under the mark at 9 of 14 (5 of 7 at p90, 4 of 7 at p95) | 34% under, at p90 |

`constants.SLOTTFTPredictionCaveat` carries this into the metric's help, and
`TestS1_TheCaveatIsTheFrozenTable` holds its figures to the frozen table.

**What is measured:**

- `slo-published.csv` moves in `ols-sweep-slo` and `ols-sweep-slo-restart` only:
  83 and 143 readings, all `no-prefill-capacity` and `vp_source="none"`, as
  decision 4 predicted. With `prefill-capacity-observe` off they read
  `no-estimate`, held as the pairwise run
  (`TestGoldens_TTFTPriceReadsTheObservation`).
- The prefill-stress goldens have **no `slo.Analyze` events**: the analyzer was
  not listed when they were recorded, so their `slo-published.csv` stays empty
  whatever S1 does. `TestS1_OnThePrefillStressGoldensAsP1LearnsThem` runs the
  analyzer over their rows instead. No run alone prices: run 2 reads
  `no-fixed-latency` with `busy-minutes`, run 1 `no-fixed-latency` with
  `peak-under-queue`, runs 3 and 4 `no-prefill-capacity`.
- `TestS1_PricedOnRecordedRows` stitches `V_P` from run 2 and `a` from run 4,
  as P2's table does, and prices run 4's rows through `Analyze`. The published
  p90 reads 0.052, 0.087, 0.160 and 0.512 s against the router's 0.047, 0.048,
  0.092 and 0.316 s, 1.10 to 1.82 times over. That is P3's frozen `cv` 2 column
  (0.052, 0.088, 0.160, 0.523), to within 2%: the widest gap is 16k's 0.512
  against 0.523, one cycle's inputs against the level's means. So until Q1 the
  published figure usually reads **high**, which is a wrong spread hiding a
  missing term, not a margin. Items 1 to 6 are not scored by this PR.
- Stitched the same way, run 2 has 171 of 193 cycles past what the fleet
  serves: `reason="priced"` and `wva_slo_ttft_refused{reason="saturated"}` 1.

**What differs from the text above:**

1. **Each prefill variant is priced at the model's whole rate**, not its share,
   because the decode role takes the whole rate. Its share is expressed as the
   replicas that rate spreads over: `Prefill.Replicas` is every Ready prefill
   replica of the model, at this variant's `V_P` and `f`, which loads each of
   its pods as its share would. The per-replica rate is the whole rate over
   those replicas. `whatif` is unchanged. With one prefill variant nothing moves.
   Before this, two equal variants behind one decode replica read 0.065 s where
   the whole rate reads 0.90 s, and read a rate above the decode onset as met
   (`TestTTFT_TheDecodeSideTakesTheWholeRate`,
   `TestTTFT_TheRateIsTheModelsWithADecodeSide`).
2. **`routing-bound` must be sustained.** The raw share is a largest of 5
   cycles and chance alone can put it past 0.8, and withdrawing the rate errs
   toward fewer replicas. So a raw share past 0.8 is priced at that high `f`,
   flagged `ImbalanceNotSustained`, and is `routing-bound` only with every one
   of a full window past 0.8 (a learned `f` has already held). On run 1, 3 pods
   throughout, `routing-bound` read on 1 of 203 cycles before this and on 0
   after; the flag reads on that 1. That cycle does not contradict P2's "at most
   1.33", which is over loaded cycles: it carried 0.06 r/s, and all of it landed
   on one pod. On one pod `f ÷ N_p` is 1 by construction, so the rule needs two.
3. **A decode knee counts as `decode-bound`.** With the prefill side alone
   stopped at a decode knee below the prefill knee, `PrefillOnlyRate` cannot
   read 10% above the rate, though decode sets it.
4. **`wva_slo_ttft_refused{reason}`** is new: `saturated` and
   `above-decode-onset`, the current rate past what the fleet serves. The reason
   stays `RateForTTFT`'s, as test 16 says, so this is what names that state.
5. **The unmet contract.** `wva_slo_ttft_unmet` reads 1 for a refused rate and
   for `target-below-floor` at any rate, each with no prediction; 0 only with
   a prediction; absent otherwise. Every refusal reason blanks the prediction.
6. **Flow control is inferred from `SchedulerQueue` alone**, collected every
   cycle. `FlowControlPools` is filled only while a learner recording is on, so
   reading it would have made a replay differ from production
   (`TestTTFT_TheFlowControlStateDoesNotDependOnARecording`). The what-if CLI
   reads the same rule, `domain.AnalyzerInput.FlowControlSeries`. `AvgKVTransferTime`
   still differs that way: it is recorded and not collected, so `TransferOmitted`
   stands in production until Q1.
7. **Flags.** `PromptSpreadAbovePeak` and `ImbalanceNotSustained` join the list.
   With the rate refused the flags are `TTFTAt`'s where it answered, and none
   where neither call did, so `UnvalidatedTTFT` and `AssumedPoissonArrivals`
   read 0 on those cycles.
8. **`ComputedTokensBuckets`** is a new row field S1 reads for the measured
   `cv`. No collector fills it: that is Q1.
9. **`prefill.Bound.PromptTokens` is not added.** A back-pressure bound has no
   `VPPromptTokens`, so it reads `VPShapeMoved`, the safe side.

`no-imbalance`, and `saturated` and `above-decode-onset` as reasons, cannot fire
on this path: S1 always passes `f`, and `RateForTTFT` stays below both limits.

**Decision 3's monotonicity is narrower than stated.** The prompt's quantile
peaks below `cv` 3 up to p93: 0.79 at p70, 1.30 at p80, 1.66 at p85, 2.22 at
p90, 2.78 at p93 (`whatif.PromptCVPeak`, `TestTTFT_PromptCVPeakIsTheGammas`). At p90
it is 8% lower at `cv` 3 than at the peak. What holds, on seeded fleets at p90
to p93, p95 and p99 (`TestProperty_TTFTIsMonotoneInCV`), is that from `cv` 0.3
up to the peak a larger `cv` prices no shorter TTFT from 0.05 of the knee and no
higher rate at the target. Outside that (`TestTTFT_TheCVClaimsThatDoNotHold`):

- At the bottom of the range: `cv` 0.15 reads 5.2% under `cv` 0 at p90 and 0.11
  of the knee, on that test's fleet.
- Past the peak: just over the floor, at 1.001 times it, the rate at `cv` 3 is
  42.6, 16.2 and 5.2 times the rate at the peak at p90, p92 and p93 on the same
  fleet. A rate that high is a target at the floor, so the multiple depends on
  the fleet; it is not a typical error. On a low-rate fleet 1.1 times the floor
  and up, the rise was 5%.

`PromptSpreadAbovePeak` flags any `cv`, assumed or measured, above the peak for
the percentile. So at p85 and below the assumed 2 is flagged. The stop at 3
stays: it is where a measured `cv` is held, not the value that prices the most.

**Checked since:** an EPP with the gate on and no traffic yet publishes no queue
series, and one that has had traffic publishes it at 0: decision 3's "declared
`enabled` with no series" holds for the first, on the staircase's on phases
([as built, M2](slo-capacity-planner-ttft.md#as-built-m2)). They recorded no
priced cycle, so the flow-control paths still ran on synthetic fleets only. The `f` floor 1.5, the 0.8, the
10% and `cv` 2 remain stated, not measured.

**What this leaves for S2's gate:**

- **Priced coverage.** No committed recording prices end to end, so S2 needs one
  run with `V_P`, `a` and `slo` listed, and a decode line that prices. The
  staircase's six goldens do not: `slo` is listed on all 1414 cycles, and none
  holds `a` ([as built, M2](slo-capacity-planner-ttft.md#as-built-m2)).
- **The union bound is untested on a recording.** On runs 1 and 2 every answered
  cycle carries `DecodeEngineMeasured`, so item 2's "the prefill side binds" is
  vacuous there: no decode line priced. Run 2's decode rows never reached the
  band (0 of 386, KV at most 0.064). Run 1's did (393 of 609), but the line stayed
  `pinned-default` on all 145 cycles a TPOT target logged, which S1 refuses.
- **The cost.** One priced variant took 32 ms a cycle here with a decode line
  (64 slots, 4 replicas), and under 1 ms without one. The review measured 107 ms,
  on a fleet not reproduced here, so budget from the larger. It is paid once
  per variant per cycle today; S2's search multiplies it by `N_p + cap`.
- **The no-act list** gains `PromptSpreadAbovePeak`
  ([decision 5](#5-s2-as-a-sketch)).

### Phase 3: the attainment trim

`P` is open-loop: a model of latency, trusted. Phase 3 adds a slow per-variant
trim `trim_v ∈ [0.5, 1.0]` on the priced fill, moved by confidence bounds on
attainment that account for bursty misses. It steps symmetrically, counts the
misses the router never records, and freezes when the SO is not SLO-decided,
held, or excluded. It corrects a drifted model over tens of minutes; it is not
llm-d's one-minute control law. **Designed later**, after phase 1 data.

### Later: a TPOT percentile

llm-d scores p90 TPOT. The ratio `q_p ÷ mean` is now specified once, in
[what-if capacity](slo-capacity-planner.md#tpot-as-a-percentile-over-requests),
and the analyzer would use the same rule and library: the router's per-request
TPOT histogram by default, the engine's per-pod
`vllm:request_time_per_output_token_seconds` as a flagged fallback near the
`k` it is applied at. A TPOT percentile in the policy stays a validation error
until this is built. Deferred until a user asks for it.

## Tests

Unit and promtool tests for phase 0 are in PR #148. Phase 1 needs composite
tests: guard is `max(N_sat, N_slo)` per SO, the two edges, the rise-slowly
rule, and the closed loop of bound 1 (a loose SLO in guard mode never exceeds
saturation's `N`). An e2e spec where the SLO decides needs latency that rises
with load (open question 2).

**Benchmark.** The G1 replication in
[keda-recipe-comparison](https://github.com/ev-shindin/llm-scaling-manager/blob/bench/keda-recipe-arm/docs/plans/benchmark/keda-recipe-comparison.md)
needs its to-do attainment scorer, client-side over all requests
([why](#transient-exclusion)). Phase 0 records the SLO's `D ÷ P` beside the
actual count. Phase 1 adds **ours-slo** (sizer, mean TPOT 100 ms). Registered
before any run: TPOT does not bind on that load (a 32B replica at 0.68 holds a
batch of about 30), so **a tie is the expected result, not a failure**. The
phase-1 case is a TPOT-bound load, such as long outputs on a small
accelerator, in its own run.

## Alternatives considered

- **SLO logic inside saturation, or handing saturation a per-variant fill.**
  Loses the free shadow mode and isolation, and rebuilds what the composite
  gives. **Rescaling saturation's capacity** under-orders when loosening, by
  `ITL(k_slo) ÷ ITL(k_static)`.
- **Plain HPA/KEDA on a latency percentile** (llm-d's slo-aware recipe). No
  controller, and it takes the target directly. But it is reactive by design,
  has no variant choice, does not yet handle P/D, and a proportional law on a
  percentile oscillates around it.
- **Dynamo Planner's SLA planner: two modes, both SLA-driven.**
  - **Throughput-based** works from a profile (swept on GPUs or estimated by
    "AI Configurator"): ITL and TTFT against load and shape, so a shape change
    inside the profiled range is handled. Earlier releases corrected it online
    with actual-over-expected ratios. It is documented for prefill and decode.
  - **Load-based** needs no profile. Each engine emits per-iteration
    forward-pass metrics; the planner fits a linear regression of iteration
    wall time over a sliding window of them: decode on summed decode KV
    tokens, prefill on summed prefill tokens (TTFT estimated by simulating
    chunked prefill), aggregated on both (2D, TTFT and ITL together). It scales
    up when the estimate exceeds the SLA and down when it is below SLA × a
    sensitivity factor, ±1 replica per interval.
  - With both on, throughput planning sets a floor and load-based adjusts
    above it: structurally our guard mode.

  Our advantage: a replica count (`D ÷ P`) instead of ±1 steps, cost-aware
  choice among variants, P/D pairing, a shared GPU budget and scale to zero.
  Dynamo does estimate aggregated TTFT, which our 2b excludes. Seeding our
  line from a profile is [item 9](#before-phase-1-prices-anything).
- **AIBrix's GPU optimizer.** The closest peer for several variants with
  cost-aware choice. An ILP based on Melange picks an SLO- and cost-aware mix
  of heterogeneous GPUs from offline per-GPU, per-model profiles; its
  PodAutoscaler consumes the result. Experimental, off by default. We choose
  online lines: no profile to maintain, and the same line prices aggregated and
  P/D decode variants.
- **WVA's original queueing-model / `ServiceClass` sizing.** Needs
  per-accelerator parameters supplied and kept current by hand; the ITL line
  is already fitted online.
- **The predictor's histograms** need the plugin and exclude the router queue.
  **Static presets per SLO** are today's *Strict* preset.

## Open questions

1. **Phase gates (open).** Proposed measurable forms, with N, X, Y and the bin
   minimum to be chosen from shadow data:
   - **Phase 1:** over ≥ N hours of shadow on ≥ 2 variants or accelerators,
     at least one of them MoE (ITL is concave in batch at small batch on MoE,
     so a line over [0.15, 0.80] sits below the curve mid-range). Two tests,
     each within X %:
     - **The line test**, per variant: the line's ITL at a later fill against
       that variant's own engine ITL,
       `rate(vllm:inter_token_latency_seconds_sum) ÷ rate(…_count)`.
     - **The clock-gap test**, per single-variant model: router TPOT against
       engine ITL. The router TPOT is model-wide and mixes variants.

     Plus `pinned`-vs-`own` disagreement below Y, and a TPOT-bound benchmark
     with attainment at or above target and no more replicas than saturation
     alone. The error is **out of sample**: one cycle's prediction, scored at
     the fill reached later (a pinned line passes near its window's mean by
     construction). Scored **at the set point**, primarily by a forced-fill
     run: a fleet with `scaleUpThreshold` headroom runs below `wva_slo_fill`
     by design, so windows whose measured fill is within ±0.05 of it rarely
     occur by chance (count them, with a minimum per variant, when they do).
     Also score the published prediction `wva_slo_predicted_itl_seconds`, at
     the post-headroom fill, against the measured engine ITL there. Bucketed
     by how far the fill moved, **signed** (rising apart from falling,
     relative to the window centroid): a rotated
     line over-prices on one side and under-prices on the other ("Before
     phase 1", item 3), and an unsigned average lets them cancel. None of
     this is computable today: it needs the phase-0 follow-up.
   - **2b:** a minimum number of effective cycles per bin, plus a monotonicity
     or fit-residual criterion on the curve.
   - **2c:** to be set from 2b's shadow results.

   **Phase 1 does not ship** while any of these holds: the line test or the
   clock-gap test is outside X %; in guard mode on a loose target the SLO adds
   replicas saturation did not ask for; no TPOT-bound benchmark has been run
   and scored client-side; a gate variant can only be priced `pinned-default`
   (seeding, "Before phase 1" item 9, is the remedy); or any of "Before phase
   1 prices anything" items 1-4 is open.
2. **The simulator's load factor.** Without one, no e2e spec can have the SLO
   decide; whether v0.9.0 has one is unchecked.
3. **Pinned pricing.** Whether `pinned` may price in sizer mode, decided from
   how often `pinned` and `own` disagree.
4. **`vllm:nixl_xfer_time_seconds`**: per request or per chunk?
5. **What 2a would observe (no longer a gate for 2b).** 2b now uses the
   per-pod model of [end-to-end TTFT](slo-capacity-planner-ttft.md), which needs
   no utilization bins; 2a remains useful only as an observation of the TTFT
   curve, and is not required before 2b. Kept for the record. The x-axis: offered
   prompt load `ArrivalRate × ILeff` against the ceiling (served utilization
   pins at 1 under overload), and one for aggregated (`both`) variants.
   Per-replica points, not variant means (convexity biases pooled means up).
   Tokens/s normalised once, not by a ceiling that moves every cycle; a
   ceiling keyed by prompt-length bucket. A defined effective sample size; a
   saturation gate better than the 1-minute peak of the waiting gauge;
   preemptions flagged per point. And a check that router mean TTFT ≈ engine
   queue + prefill, or the residual hides in the tail ratio.
6. **A per-SO growth cap in the composite**, to steer growth away from an
   SLO-infeasible accelerator without capping below saturation's `N`: a hold,
   or a composite rule?
7. **`ArrivalRate` on a pool serving several models** is refused today. Should
   the SLO analyzer abstain there, or use `target_model_name` alone?
8. **Data-parallel replicas.** For LWS, a group is its leader's row: right for
   tensor parallelism, wrong where data-parallel ranks span nodes.
9. **The router-side limits**: priority bands, LoRA adapters, and the judged
   share of traffic ([Which latency](#which-latency-measured-where)).
10. **The docs to change with phase 1** in `docs/reference/scaling-policy.md`:
    `enabled` is no longer "reserved"; saturation no longer drives every
    decision; *Strict* stops being the low-latency SLO answer;
    `queueLengthThreshold` has no effect on an SLO-decided ScaledObject.

## Review findings

The full discussion is on PR #147 and PR #148. The design above already
reflects each of these: a fill bound on the observed maximum ratcheted the
fleet (to about 0.28 at `scaleUp = 0.85`), so it is a fixed clamp; holding an
infeasible variant froze single-variant P/D models, so it abstains; a TTFT
budget subtracted quantiles, which do not add; the first trim was a one-way
ratchet at p99; and step 2a's means could not answer open question 5.

## Appendix: the composite rules

These rules belong to the composite analyzer; this is what the SLO design
relies on. The design is on the
[composite-analyzer branch](https://github.com/deanlorenz/llm-scaler/tree/composite-analyzer)
(its spec §5.1), not merged.

```
N_i(SO)      = D_i[role(SO)] ÷ P_i(SO)                each eligible analyzer, in its own units, no threshold
N_com(SO)    = max over contributors of N_i(SO)        undefined skipped; P ≤ 0 or Reason no-data/error is undefined
               saturation contributes when listed; unlisted, a role nothing sized holds
frame(role)  = saturation's or the others', whichever has the lower Σ_SO replicas(SO) ÷ N(SO)
saturation's: D_com = D_sat[role], P_com = P_sat
the others':  D_com = max over SO of N_others(SO) × P_sat(SO);  P_com(SO) = D_com ÷ N_others(SO)
RC, SC       = applyUniversalThreshold on D_com, with the model-level scaleUp / scaleDown
```

With saturation eligible, the max lets the SLO only tighten (guard); with it
disabled, the result may come out below saturation alone (sizer). An SO the
SLO cannot price lands on `C1-single` (guard) or `C4-no-signal`, and its role
holds (sizer). A
defined `N_slo = 0` contributes zero demand, the right answer for an idle
variant; `D_sat = 0` is [item 5](#what-the-composite-needs-before-phase-1).
