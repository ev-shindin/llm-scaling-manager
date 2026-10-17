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

**The 2b design, not built.** Recorded so 2a's data can test it. **2b is
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
TTFT contributor in shadow, prefill role only, once 2a's data answers
[open question 5](#open-questions). **2c** enforces TTFT once 2b's shadow
predictions track observed attainment.

### Phase 3: the attainment trim

`P` is open-loop: a model of latency, trusted. Phase 3 adds a slow per-variant
trim `trim_v ∈ [0.5, 1.0]` on the priced fill, moved by confidence bounds on
attainment that account for bursty misses. It steps symmetrically, counts the
misses the router never records, and freezes when the SO is not SLO-decided,
held, or excluded. It corrects a drifted model over tens of minutes; it is not
llm-d's one-minute control law. **Designed later**, after phase 1 data.

### Later: a TPOT percentile

llm-d scores p90 TPOT. A percentile needs a tail ratio `q_p ÷ mean` from
`vllm:request_time_per_output_token_seconds`, measured near the `k` it is
applied at. Deferred until a user asks for it.

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
5. **What 2a must observe before 2b prices anything.** The x-axis: offered
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
