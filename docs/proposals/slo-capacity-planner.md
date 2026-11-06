# What-if capacity: from an SLO to a rate, and from replicas to an SLO

**Status:** proposal, 2026-11-02. Nothing here is built. The decode half reuses
arithmetic the controller runs today. The TTFT model is on its own page,
[end-to-end TTFT](slo-capacity-planner-ttft.md). The recorder fields this work
needs are in [the learner rules](analyzer-learner.md#29-recorder-fields-for-ttft-and-capture),
and TTFT pricing in the SLO analyzer is [SLO tiers](slo-tiers.md) phase 2. This
page supersedes [prefill TTFT model](prefill-ttft-model.md) for how prefill
capacity is modelled.

**Audience:** contributors. Operators read [the scaling policy reference](../reference/scaling-policy.md)
for what runs today.

## The two questions

1. **"My SLO is a mean TPOT of 4 ms and an end-to-end TTFT of 200 ms at p95.
   What request rate can this fleet carry?"** Optionally per token bucket.
2. **"I run 1 prefill and 1 decode replica. At what rate do they saturate, and
   what SLO do they hold below that?"** The answer is one pair, TPOT and
   TTFT at percentile `q`, at one operating rate.

TPOT is the mean by default. A TPOT target may instead be a **percentile over
requests of each request's own TPOT**, `(e2e − TTFT)/(output tokens − 1)`, as
MLPerf Inference's LLM server scenario, vLLM's `benchmark_serving`, guidellm,
SLO-attainment work such as DistServe, and the QoS proposal state it:
`--tpot 4ms@p90`. A percentile over individual token gaps (ITL) is not
supported.

TTFT is end to end, as the client sees it: router queue, engine queue, prefill,
KV transfer, decode's admission and first step. Both questions are answered
from a recording, from a live Prometheus with no controller installed, or
(only if asked) inside the controller.

## At a glance

| | |
| --- | --- |
| **Decode** | Reuses the ITL line and the SLO analyzer's TPOT pricing unchanged, moved to a `signals` package so a CLI can call them. A per-request TPOT percentile is the mean times a ratio `R_q` learned from the router's per-request TPOT histogram. Decode admission is a birth-death chain over the batch cap's slots. |
| **TTFT** | Per-pod queue with the measured imbalance; the router's flow-control queue only above the rate where it starts holding requests; prefill capacity as a token throughput from sources that cannot over-state it; parts combined by convolution and an optimised union bound. See [end-to-end TTFT](slo-capacity-planner-ttft.md). |
| **Packaging** | `internal/signals/whatif` and `internal/signals/prefill`, an adapter `internal/whatifsource`, a CLI `cmd/whatif`. |
| **Validated** | Decode against the goldens; TTFT on a fixed-replica staircase, flow control off and on. TTFT prints UNVALIDATED until then. |
| **Refuses** | With no evidence for a term, rather than guessing. |

## A session

Proposed commands. The central values are the [worked example](#worked-example-ols-sweep):
the decode side from the `ols-sweep` golden's last ITL line, `V_P` assumed
because that golden holds no evidence of it. The bands are illustrative.

```
$ whatif replicas-to-slo --recording ols-sweep.jsonl.gz --cycle last \
    --model m --prefill 1 --decode 1 --ttft-percentile 90 --assume-vp 30000 \
    --tpot-percentile 90 --assume-tpot-ratio 1.3
knee            13.31 r/s   decode, batch cap binds at fill 0.732
operating rate  11.32 r/s   0.85 x knee (scaleUpThreshold)
mean TPOT       3.66 ms     [3.58 .. 3.79]   line ols, observed fill up to 0.62
TPOT p90        4.76 ms                      per request; R_q 1.3 ASSUMED (no TPOT histogram)
TTFT p90        160 ms      [146 .. 207]     end to end; UNVALIDATED; V_P ASSUMED
                (154 ms if prefill and decode waits were independent)
flags: flow control inferred off (no queue series); T_xfer omitted (not collected);
       prompt spread assumed (cv 1)
estimate from cycles 1-226, not a guarantee; see basis (-o json)

$ whatif slo-to-rate --recording ols-sweep.jsonl.gz --cycle last \
    --model m --prefill 1 --decode 1 --tpot 4ms --ttft 200ms@p95 --assume-vp 30000
role     est. rate at SLO   bound by                                basis
decode   11.32 r/s          operating-point cap (TPOT alone: 12.03)  ols [11.0 .. 11.5]
prefill  11.10 r/s          TTFT p95                                 UNVALIDATED; V_P ASSUMED
fleet    11.10 r/s          prefill TTFT p95
estimate from cycles 1-226, not a guarantee; see basis (-o json)
```

- **Knee**: the rate at which a role saturates; the fleet's is the lower.
- **Operating rate**: 0.85 of the knee, the autoscaler's headroom.
- **Fill**: how full a decode replica's KV cache is, 0 to 1.
- **Bound by**: the constraint that set the rate; on a tie all are named, the
  operating-point cap first.
- **Bands**: 10th to 90th percentile of a block bootstrap, so not symmetric.

These are the proposal's figures, at `cv` 1. [As built](#ttft-as-built) the CLI
assumes S1's `cv` 2, prints no TTFT band and adds a `prefill knee` row: the same
commands read 353 ms and 1.19 r/s.

Refusals, flags, the standalone mode and exit codes are in the
[CLI reference](#cli-reference).

## Terms

| Term | Meaning |
| --- | --- |
| ITL line | `ITL(k) = A·k + B`, inter-token latency against fill `k`, fitted per variant (`internal/signals/itl`) |
| Tier | Where a line came from: `ols` (full fit), `pinned-B` (slope fitted, intercept held from the last full fit, or 6 ms if none), `held` (last slope kept), `borrowed` (another variant's line) |
| Line check | The token rate the line predicts at a replica's fill against its reported `generation_tokens` rate (`itl.GPSErrorPct`, 15%; the analyzers call it the GPS check) |
| `KVreq` | KV tokens one in-flight request holds on average: `ILeff + OL/2`, `ILeff = IL·(1 − hit)` |
| `V_P` | A prefill pod's prompt-token throughput while it has work |
| `a` | The fixed latency one request pays on a prefill pod |
| Batch cap | Most sequences an engine runs: flags, else the `llm-d.ai/max-num-seqs` annotation, else learned (#422, #429) |
| Presence bit | A `...Known` flag beside a metric, so a missing reading is never read as zero |

## Decode: TPOT from the ITL line

**TPOT is a mean by default** (`whatif.PriceTPOT` in `internal/signals/whatif/decode.go`: "TPOT is a MEAN ... no tail
model"); a per-request percentile is supported [below](#tpot-as-a-percentile-over-requests).

A decode replica at fill `k` holds `n(k) = min(k·C/KVreq, batchCap·ranks)`
sequences, each emitting a token every `ITL(k)`. So TPOT is `ITL(k)` and the
replica's request rate is `λ_rep(k) = n(k)/(ITL(k)·OL)`: `whatif.ReplicaTokenRate/OL`
in the SLO analyzer, `whatif.DeriveMuAtCap` in saturation. For a given rate the fill is
closed form (Little's law, residence `OL·ITL(k)`):

    k(λ_rep) = λ_rep·OL·B / ( C/KVreq − λ_rep·OL·A )

valid while the denominator is positive and `n` is under the cap.

Corrections: TPOT per accepted token under speculative decoding (`ITLPerToken`,
#274, PR #430); `KVreq = ILeff + D·OL/2` with the output spread
`D = E[O²]/E[O]²` (#276, unmerged); `KVreq` assumes prompt and output length are
uncorrelated (the exact footprint is `E[IL·OL]/E[OL] + E[OL²]/(2·E[OL])`), so a
positive correlation makes the rate read high; and `whatif.ReachableFill` clamps `k`
to the batch-cap fill `batchCap·KVreq/C`.

### TPOT as a percentile over requests

**What it is.** Each request has its own TPOT,
`(e2e − TTFT)/(output tokens − 1)`, the average gap over its own generation. A
percentile target says that share `q` of requests must have their own TPOT at or
below the target. The router records exactly this, one observation per request,
in `llm_d_epp_request_streaming_tpot_seconds` (a histogram), for streaming
requests with at least 2 output tokens, and only when the response carries
`usage` (the client sends `stream_options.include_usage`, or vLLM runs with
`--enable-force-include-usage`). Without that, the series is empty even on a
router that publishes it.

**What is not supported: percentiles over token gaps** (ITL, every gap of every
request pooled). The ITL line models the mean step time against fill, and
nothing in the codebase models the spread of single steps: chunked prefill
landing in a step, a preemption, a speculative step accepting zero tokens. A
token-gap percentile also weights a long request by its length, so it is not a
per-request service level. It gives `TokenGapPercentile`.

**The model.** `TPOT_q = ITL(k) × R_q`, where `R_q` is the ratio of the `q`-th
percentile to the mean of per-request TPOT. `R_q` is above 1 because:

- a request's fill moves over its residence, so requests that live through the
  busy periods see a higher mean ITL;
- a short output averages few gaps, so its TPOT spreads more: `R_q` depends on
  output length, and is learned per output bucket where the data allows;
- speculative decoding accepts a variable number of tokens per step (#274);
- on aggregated pods, prefill chunks land in decode steps (#275).

**Learning `R_q`.** From the router's TPOT histogram, per window: the bucket
holding the `q` quantile, read at its **upper edge**, over the window's mean
(`_sum / _count`). `R_q` is the 90th percentile of those per-window ratios, and
never below 1. The histogram is model-wide, so it pools output lengths and
variants: a per-bucket answer uses the pooled `R_q` and is flagged, unless a
load generator's per-request log supplies the per-bucket ratio. The window mean
is request-weighted, while `ITL(k)` is a step-time mean, which is
token-weighted; multiplying one by a ratio over the other assumes the two
agree, the same assumption the mean TPOT target already makes.

**Resolution.** The router's default edges (`DefaultRequestTPOTBuckets`) are
0.5, 2.05, 5 and 10 ms, then coarser. At the worked example's 3.66 ms mean, the
p90 lands in `(2.05, 5]` or `(5, 10]`, so a window reads 1.37 or 2.73, whatever
the true spread: `R_q` is set by the grid, not the traffic. It stays
conservative (an upper edge never under-states), but coarse. So:

- the remedy is the router's `RequestTPOTBuckets` override, with edges a few
  percent apart around the target (for example every 0.25 ms from 2 to 8 ms
  for a 4 ms target);
- the basis prints the `q` bucket's width over the mean, and flags it above
  0.25;
- a `q` quantile in the `+Inf` bucket has no upper edge, and gives
  `TPOTQuantileUnbounded`.

The example's assumed 1.3 could not be learned from the default buckets: the
smallest ratio they can give there is 1.37.

**Sources.** The router histogram is the default, because it is what the client
sees and what the SLO is judged on. The engine's per-pod
`vllm:request_time_per_output_token_seconds` is the fallback: it is per variant,
needs no `usage` option and is read near the `k` it is applied at, but it misses
the router's own streaming path and exists for vLLM only. It is used, flagged,
when the router histogram is empty or its resolution is flagged. The SLO
analyzer's percentile ([SLO tiers](slo-tiers.md#later-a-tpot-percentile)) uses
the same rule and the same library.

**Without a histogram**, a percentile TPOT is refused (`NoTPOTHistogram`).
`--assume-tpot-ratio R` gives a flagged answer instead. Recommendation: refuse
by default, because no default ratio has been measured; a stated default would
be read as one.

**Inversion.** Question 1 inverts `ITL(k)·R_q = t`: `k* = (t/R_q − B)/A`, then
the same clamps and cap as the mean. Question 2 reports TPOT at both the mean
and percentile `q` (`--tpot-percentile`; else the mean only, since the scaling
policy has no TPOT percentile today: setting one is a validation error).

### Decode admission

A birth-death chain on the requests a pod holds, with
the line at each state's own fill; [end-to-end TTFT](slo-capacity-planner-ttft.md#decode-admission)
has it. Its wait is negligible until close to the knee, then rises fast: in the
worked example the wait probability is 0.032 at 0.85 of the knee and 0.131 at
0.90, with a p90 wait of 203 ms there. That is why question 1 caps decode at the
operating point.

## TTFT, in one paragraph

TTFT is the wait before prefill (router and engine), the request's own prefill
`a + I/V_P`, the KV transfer, and decode's admission wait and first step.
`V_P` comes from busy minutes, an observed peak under a queue or saturation's
back-pressure bound, else the tool refuses (or takes `--assume-vp`, flagged).
Each prefill pod is an M/G/1 queue on prefill work, fed its share of arrivals
with the measured imbalance. EPP flow control adds a router queue only above
the rate where its gate closes; below it nothing changes. The parts are
combined by convolution where independent and by a union bound, with an
optimised split, across the prefill and decode sides. The full model is
[end-to-end TTFT](slo-capacity-planner-ttft.md).

## Inverting the models

### Question 1: from an SLO to a rate

**Decode, TPOT `t`.** For the mean, `k* = (t − B)/A` (`whatif.PriceTPOT`); for a
per-request percentile, `k* = (t/R_q − B)/A`. `TargetBelowFloor` if
`ITL(band.lo)·R_q ≥ t` (`R_q` = 1 for the mean). Clamp to the band and the batch cap (`whatif.ReachableFill`).
Rate `N_d·λ_rep(k*)`, **capped at the operating point**, because past it the
admission wait is no longer small. `--headroom 1` lifts the cap.

**Prefill, TTFT `T` at percentile `q`.** TTFT rises with the rate, so bisect
below the lowest knee (with flow control on, also below the decode gate's
onset) for the largest rate whose union-bound TTFT is at most `T`, decode
admission priced at the same rate. When the prefill side alone would allow more,
the answer is the rate decode admission holds it to, flagged. If own prefill,
transfer and `ITL(band.lo)` alone exceed `T`, `TargetBelowFloor`.

**The fleet** carries the smaller of the two roles, then the operating-point cap.

### Question 2: from replicas to an SLO

1. **Knees.** Decode: `N_d·λ_rep(k_full)`, with `k_full` the lower of the
   policy's band top (`analyzerTuning.itlBand.maxObservableK`, 0.80 by default:
   the highest fill the line is fitted on, a policy knob, not a physical limit)
   and the batch-cap fill (physical). Prefill: `N_p·V_P/(f·E[I_eff])`. The
   binding role has the lower knee.
2. **Operating point.** `scaleUpThreshold` (0.85) times the knee, in rate,
   applied once (as `whatif.RealisedFill`), or `--rate`.
3. **Answer.** TPOT from the closed-form fill, as the mean and, when a TPOT
   percentile is asked, as `ITL(k)·R_q`; TTFT at percentile `q`
   (`--ttft-percentile`, else the policy's, else 90). Floors are printed: TPOT
   never beats `ITL(band.lo)` (times `R_q`); TTFT never beats
   `a + I_q/V_P + ITL(band.lo)`.

**Two headroom conventions.** The tool applies 0.85 in rate; saturation applies
it in fill (`whatif.PricingK` = `KvCacheThreshold × ScaleUpThreshold`, 0.68). The rate is
concave in fill, so a saturation-driven fleet runs hotter: `λ_rep(0.68)` is
12.99 r/s in the worked example, against 11.32. `--headroom-space fill` gives
the saturation convention.

### Token buckets and the current mix

A bucket is a range of prompt length, output length, or both, on
`classifyOutputLength`'s edges (under 100, 500, 1500, 3000, 6000, above), which
`classifyInputLength` reuses. A per-bucket answer means "if all traffic looked
like bucket `b`": `KVreq_b`, `OL_b`, and the prompt distribution restricted to
the bucket. An input-only bucket keeps the mix's output distribution. The spread
inside a bucket comes from the histogram restricted to it; without histograms it
is an assumed coefficient of variation of 1, flagged. The mix answer uses the
moments of the current distribution.

## Worked example: ols-sweep

From the `ols-sweep` golden: the last published line (`A` = 0.004438,
`B` = 0.001557 s), `C` = 131,072 tokens, batch cap 64, one rank, one prefill and
one decode pod. `IL` = `OL` = 1000, no prefix hits. `a` = 10.1 ms plus the
2.5 ms overhead, from the golden. (That 10.1 ms is an intercept fitted
against the long-window prompt mean; P1's fit against the effective prompt
gives 8.36 ms and the overhead 2.68 ms, so `a` is 11.04 ms, not 12.6
([end-to-end TTFT](slo-capacity-planner-ttft.md#what-is-derivable-today)).
The example is left as computed: `a` enters only the request's own
latency, not utilization or the knee, so the TTFT figures and the TTFT
floor below would each read about 1.6 ms lower.) **`V_P` = 30,000 tok/s is assumed**: the
golden's prefill pod never queued, so the tool refuses `V_P` from it. Prompt
spread: coefficient of variation 1, assumed. Flow control inferred off. The
figures are reproduced by two independent scripts.

| Step | Value |
| --- | --- |
| Batch-cap fill `64·1500/131072` | 0.732, below the band top 0.80: the cap binds |
| Decode knee `64/(ITL(0.732)·1000)`, `ITL` 4.81 ms | **13.31 r/s** |
| Prefill knee `30000/1000` | 30.0 r/s |
| Operating rate `0.85·13.31` | **11.32 r/s** |
| Decode fill at it | 0.474 (41.4 sequences) |
| Mean TPOT `ITL(0.474)` | **3.66 ms** |
| TPOT p90, per request | 4.76 ms, with `R_q` = 1.3 **assumed** and flagged: `ols-sweep` carries no TPOT histogram, and the router's default buckets could not give a ratio below 1.37 here |
| Decode wait probability (the chain) | 0.032 |
| Prefill `ρ`, `θ` | 0.377, 53.5 ms |
| **TTFT p90**, optimised union bound | **160 ms** (decode share of the tail 0.032; independent 154 ms) |
| TTFT p95, optimised union bound | 231 ms (independent 210 ms; adding the p95s would give 224 ms) |
| Floors | TPOT 2.22 ms; TTFT 91.6 ms |
| Flow-control onsets, if it were on | decode 12.50 r/s (0.94 of the knee), prefill 25.6 r/s: above every answer here, so nothing changes |

**Question 1 on the same fleet: mean TPOT 4 ms, TTFT 200 ms.**

| Step | p90 | p95 |
| --- | --- | --- |
| TPOT inverts to `k*` = 0.551 | 12.03 r/s | 12.03 r/s |
| TTFT bisected below the decode knee | 11.64 r/s | **11.10 r/s** |
| Operating-point cap | 11.32 r/s | 11.32 r/s |
| **Answer** | **11.32 r/s**, the cap | **11.10 r/s**, TTFT p95 |

The round trip, question 1 at the SLO question 2 returned, gives back 11.32 r/s
only because both sit at the cap. The property case therefore runs below it: a
TPOT of 3.0 ms inverts to 9.47 r/s, and the fill there gives back 3.00 ms.

## What exists

One inventory: each input, where it comes from, and whether each mode can read
it. "R1" is the [recorder PR](analyzer-learner.md#29-recorder-fields-for-ttft-and-capture).

| Input | Series or source | Collected | Recording | Snapshot | Live |
| --- | --- | --- | --- | --- | --- |
| `(k, ITL)` per pod | `QueryKvUsageMean`, `QueryAvgITL` | yes | yes | lines only | yes |
| KV capacity | `QueryCacheConfigInfo` | yes | yes (`TotalKvCapacityTokens`) | no | yes |
| Batch cap | Deployment flags, annotation, learned | yes | store records | no | needs the Deployment |
| Shape means | prompt/output length, prefix hits | yes | yes | no | yes |
| Length histograms | `request_prompt_tokens_bucket`, `request_generation_tokens_bucket` | no | after R1 | no | yes |
| Engine TTFT mean | `QueryAvgTTFT` (`AvgTTFT`); read by no analyzer | yes | yes | no | yes |
| Service time, queue excluded | `QueryAvgServiceTime`; SGLang: e2e minus `queue_time` | yes | yes | no | yes |
| Prefill tokens per second | `QueryPrefillComputedTokenRate`, vLLM only | yes | yes | lower bounds | yes |
| Pod queue length | `QueryQueueLength` | yes | yes | no | yes |
| Queue-time, prefill-time, engine TTFT histograms | `request_queue_time_seconds`, `request_prefill_time_seconds`, `time_to_first_token_seconds_bucket` | no | after R1 | no | yes |
| KV transfer | `nixl_xfer_time_seconds` | no | after R1 | no | yes |
| Router TTFT histogram, TPOT mean | `QuerySLORouterTTFTBuckets`; streaming TPOT `_sum`/`_count` | yes | only when the SLO analyzer judges; every cycle after R1 | no | yes |
| Router per-request TPOT histogram | `llm_d_epp_request_streaming_tpot_seconds_bucket` | no (only `_sum` and `_count` today) | after R1 | no | yes |
| Arrival rate | `QueryModelArrivalRate`: flow-control enqueues, else scheduler placements | yes | yes | no | yes |
| Flow-control queue size | `QuerySchedulerQueueSize` | yes | yes (`SchedulerQueue`) | no | yes |
| Flow-control wait, saturation, outcomes | `flow_control_request_queue_duration_seconds`, `_pool_saturation`, `_requests_total` (per pool) | no | after R1 | no | yes |

`AvgTTFT`, `AvgServiceTime` and `QueueLength` have no presence bit today; R1
adds them, because the TTFT model reads a missing value as 0 otherwise.

## Data per mode

**Recording.** The tool replays the recording through the real analyzers, stops
after the cycle named and reads the learned state: the answer is what the
controller knew then. Of the real goldens, `ols-sweep` answers decode and the
prefill `a`; `pd-restart` publishes no line; `e1` and `e1-tuned` list slo as
notRun; none has router TTFT. **Capture** (`whatif capture`) makes a recording
from Prometheus alone for a past window; it is specified with the recorder.

**Standalone.** The collector's query templates and
`prometheus.NewPrometheusSource` need only a Prometheus client;
`internal/whatifsource` builds `domain.ReplicaMetrics` from the series and a
pod-to-role map, refits the lines over the lookback, and takes every pod as
Ready without a kubeconfig (flagged "admitted without readiness"). It does not
apply saturation's P/D decode view. A snapshot (`wva-learned-state`) may supply
an ITL line when the lookback spans too little fill: `a, b`, the donor and the
age, no fit statistics, no learned batch cap.

**Controller.** A read-only handler on the metrics server, behind its existing
authentication and authorization filter, off by default; only if asked.

## Uncertainty

Every answer carries a **basis**: the tier of each term, its readings, range and
age. **Bands**: a block bootstrap, blocks of 4 cycles (15 s cycles over 1-minute
rates overlap by three quarters), admission and tier rerun per resample, 200
resamples, a fixed printed seed; "tier unstable" when more than 10% of resamples
change tier. `V_P` from a peak and `f` are extreme statistics, so they get a
±15% sensitivity sweep instead. **Extrapolation**: a fill or prompt length
outside the window's range is flagged with the distance; outside the band the
tool clamps, as `whatif.PricingK` and `whatif.PriceTPOT` do. **The line check** runs on every
answer and a mismatch above 15% away from a ramp is printed.

## Where decode is most wrong

TTFT's rows are on [its page](slo-capacity-planner-ttft.md#where-it-is-most-wrong).

| Limit | Direction | Issue |
| --- | --- | --- |
| Decode assumed balanced across `N_d` | Over-promises | — |
| Output-length spread ignored (`D` = 1) | Over-promises by 7-44% | #276 |
| Prefix-cache hits still occupy KV | Over-promises, up to about 4x under multi-turn reuse | #277 |
| Positive prompt/output correlation | Over-promises | — |
| ITL depends on shape on aggregated pods | Either way; about 40% of ITL at 1000/500. The saturation analyzer prices it ([aggregated ITL by shape](aggregated-itl-shape.md)); the slo analyzer does not yet | #275 |
| SLO analyzer reads step time, not per token | Under-promises on speculating vLLM by `1 + E` | #274 |
| No saturation line for an unlabeled aggregated variant | Line fitted (switch `itl-line-fits-aggregated`, its own key); sized from once priced by shape and checked (#275) | #432 |
| Residence exponential in the admission chain | Either way; a constant ITL would over-promise near the knee | — |
| `R_q` learned under one load and applied at another | Learned at lower load and applied near the knee, over-promises (the spread grows with load); the reverse under-promises | — |
| `R_q` pooled across output lengths, applied to a short-output bucket | Over-promises | — |
| `R_q` pooled across output lengths, applied to a long-output bucket | Under-promises | — |
| `R_q` read off coarse buckets | Under-promises (an upper edge over-states the ratio) | — |
| KV-bound admission approximated by slots | Either way | — |

## Packaging

| Package | Holds | Layer (`hack/check-import-direction.sh`) |
| --- | --- | --- |
| `internal/signals/itl` | unchanged | 30 |
| `internal/signals/prefill` (new) | `V_P`, `a`, `f`, their windows and admission | 30 |
| `internal/signals/whatif` (new) | decode pricing (moved), the closed form, the chain, the queues, the inversions, bands, basis, `Answer`/`Unanswerable` | 32, an arm before `internal/signals/*`, above `floor` (31) so an edge from `floor` is caught |
| `internal/whatifsource` (new) | Prometheus to `ReplicaMetrics`, the role map, readiness fallback, snapshot reader, capture | outside the pipeline, like `controller`; the only new importer of `collector/source` and a client |
| `cmd/whatif` | flags, output, exit codes | `make whatif`; not in the manager image |

**D1 moves** `priceTPOT`, `reachableFill`, `replicaTokenRate`, `realisedFill`,
`fillBand`, `pricing` (from `slo/price.go`) and `deriveMuAtCap`,
`capacityTokensFor`, `derivedMu` (from `saturation/mu_from_itl.go`); `pricingK`
takes its thresholds as numbers; the batch-cap order stays in the analyzers (its
readers are methods behind replay switches); the two copies of the sequence-cap
arithmetic become one. About 64 test references in 12 files move, and paths in
`docs/developer-guide/saturation-analyzer.md` and `slo-analyzer.md` change.
**Accessors**, each in the PR that first calls it: the tracker's observations
per key; read-only views of the analyzers' trackers, batch cap and capacity; one
"replay to cycle N, return the learned state" function (D3 also corrects
`learnreplay`'s stale "test support only" doc).

## Validation

**Against the goldens**: learn on the first 60% of a golden's cycles, freeze,
predict the rest; signed, out of sample. An error report with bounds, not the
pass mark (goldens scale out):

| Check | Measured | Bound |
| --- | --- | --- |
| Mean TPOT at the observed fill | engine ITL mean | ±10% median signed error |
| Prefill own latency at the observed prompt | `AvgServiceTime` | ±15%; SKIPPED where `V_P` is refused or assumed |
| Decode knee | saturated throughput windows, at least 5 | ±15% |

A check with no data is **SKIPPED with a reason**, never passed. TTFT's pass mark
is [the staircase](slo-capacity-planner-ttft.md#ground-truth-and-the-staircase).

**Property cases**, in `internal/signals/whatif` (`property_test.go`, in the
package so the chain's Erlang C case reaches its solver), each
mutation-checked:

- The rate never falls when a target loosens, never rises when prompts or
  answers lengthen.
- Round trip within 1e-6, with `--headroom 1`, at unclamped points only (fill
  inside the band, `n` under the cap, rate under the operating-point cap), for
  the mean TPOT and for a per-request percentile: question 1 at `ITL(k)·R_q`
  gives back the rate whose fill is `k`.
- `R_q` is never below 1, including from a histogram whose bucketed quantile
  falls below its mean.
- A known per-request TPOT distribution, bucketed on the router's default
  edges, gives an `R_q` at or above the true ratio; a quantile in `+Inf` gives
  `TPOTQuantileUnbounded`.
- The chain at constant ITL equals Erlang C; with `f` = 1 and one pod, the
  engine wait equals Pollaczek-Khinchine.
- The union bound is never below the independent figure; the optimised split is
  never above the even split; percentiles are never added.
- Below the flow-control onsets, flow control on equals off within the gate's
  closed fraction (or exactly, at `N_d` large enough that the mean score
  concentrates). Decode admission is counted once.
- Every `Unanswerable` reason has a case; NaN gives a reason, never 0 or +Inf.
- `V_P` is never above the busy-minute median on a synthetic pod.

## PR plan

Learner PRs follow [the learner rules](analyzer-learner.md): one behaviour change
per replay switch, goldens replayed on and off, a property case that fails on
`main` first. **The recorder lands before any recording this work depends on
is made.**

| PR | What | Goldens | Depends on |
| --- | --- | --- | --- |
| R1 | Recorder fields for TTFT: [the learner rules, 2.9](analyzer-learner.md#29-recorder-fields-for-ttft-and-capture) | regenerated, values unchanged | — |
| R2 | `whatif capture` and the `Generate` replay mode; creates `internal/whatifsource`, which D4 extends | none | R1 |
| Recordings | Prefill-stress (30k prompts, 3 prefill pods, decode below its ceiling, fixed replicas); the staircase warm-ups, flow control off and on | — | R1 |
| Capture | The `ols-sweep-slo` window, from its Prometheus snapshot | — | R2 |
| D1 | Move the decode pricing into `whatif` at layer 32. Proof: vectors captured from `main` before the move, frozen, asserted bit-identical; the fails-on-main rule is waived for a pure move | none move | — |
| D2 | Closed form, TPOT inversions, the admission chain, `Answer`/`Unanswerable`, property cases. Deletes D1's frozen vectors and `TestMovedPricingIsBitIdentical` if it changes a moved function (regenerable from bf493785a). Also D2: `PriceTPOT` with a NaN band edge returns `OK: true` with a NaN fill (only `PricingK` checks the band); `DerivedMu.CapAtMeasured`, set only by the saturation analyzer, and the string `Why` codes are candidates to move out or type | none | D1 |
| D3 | Accessors, the replay function, bands, basis, `cmd/whatif` from a recording or snapshot, footer, exit codes | none | D2 |
| D4 | Standalone answers from `--prometheus`; no switch (adapter only) | none | D3, R2 |
| P1 | `internal/signals/prefill`, observing: `V_P`, `a`, `f`; logs only; switch `prefill-capacity-observe`. On `ols-sweep` it fits `a` and refuses `V_P` and `f` | none | — |
| P2 | The prefill-stress recordings as goldens, and P3 checked against them: runs 1 to 4 ([the recordings](slo-tiers.md#the-p2-recordings)). [As built](slo-capacity-planner-ttft.md#as-built-p2): P3 under-predicts the router's TTFT at light load and at saturation | adds four | P1, R1, the runs |
| P3 | The TTFT model in `whatif`; UNVALIDATED; no switch. Built before P2, against closed-form and synthetic cases, because the prefill-stress runs were blocked on cluster access: [as built](slo-capacity-planner-ttft.md#as-built-p3) | none | P2 (to validate), D2 |
| TTFT CLI | `--ttft`, `--ttft-percentile`, `--assume-vp` and `--prefill` on both questions, from every source: [as built](#ttft-as-built) | none | P3, D4 |
| S1, S2 | The SLO analyzer prices TTFT, shadow then enforce, on the same `prefill` and `whatif` library: [SLO tiers, phase 2](slo-tiers.md#phase-2-ttft) | with its switch | S1: P3. S2: S1, Q1, M2 |
| M1 | Per-bucket and mix answers; aggregated TTFT | none | P3, R1, #276 |
| M2 | The staircase run; an operator page in `docs/reference/`; drops UNVALIDATED if it passes | — | D4, P3 |
| Q1 | Issue the queue-minimum query every cycle, not only while a recording is on, and add the per-request computed-prefill-token histogram (`vllm:request_prefill_kv_computed_tokens_bucket`, per instance), for namespaces holding a model whose policy sets a TTFT target: busy minutes fire in production, and the measured effective-prompt spread replaces S1's assumed `cv` 2. Before S2, not S1: [S1 decisions](slo-tiers.md#s1-decisions) | none for the queue minimum (recordings carry it); the histogram is new, so recordings made after Q1 carry it | P1's busy minutes (PR #470) |
| Only if asked | The controller endpoint and two gauges | none | P3 |
| Later | Prefill line and learned batch cap in the snapshot; the floor's prefill mu from `V_P`; a shared prefill/decode budget; #275, #277 | — | — |

**P1 and busy minutes.** `V_P`'s first source, busy minutes, needs each pod's
least waiting count over the minute (`min_over_time` of
`num_requests_waiting`), which no query reads today. P1 takes it as
`prefill.Cycle.QueueMin` and is fed none, so busy minutes never fire and
`V_P` comes only from the lower-bound sources (the peak under a queue, the
back-pressure bound). R1 adds a per-pod `QueueMin` with a presence bit
(`QueueMinKnown`); once R1 merges, a follow-up to P1 wires it into the
observation.

*As built (the P1 follow-up).* The saturation analyzer passes each prefill
row's `QueueMin` to the observation where `QueueMinKnown` is set
(`prefill.QueueMins`), behind its own replay switch,
`prefill-busy-minutes`. A minute is busy when that minimum is above zero
(finite), the queue's peak is above zero, the prefill pod's KV is not full
(`kvFull` at the policy's `kvCacheThreshold`), and decode is not
back-pressuring prefill. `V_P` is the median computed-token rate over the
busy minutes of the last 30 minutes (`prefill.BusyWindow`), from at least 5.
A zero, a minimum that was not read, NaN or `+Inf` is not busy.

**A busy minute reads at or below capacity, never above**: at it when the
pod ran flat out, under it when it did not. Data-parallel pods and stalls
under-read:

- On a data-parallel pod the minimum is each rank's least, summed. Above
  zero, the pod held work every sample of the minute, but one rank may
  have idled while another queued. When any pod behind the busy minutes
  has more than one rank, `V_P` is flagged a lower bound
  (`vpLowerBoundReason=DataParallelRanks`).
- A stall can leave the queue non-empty while throughput falls. A KV-full
  prefill pod is excluded, and so is a cycle that the saturation test reads
  as decode back-pressure. Back-pressure that test does not catch (decode's
  KV full with a short queue) still counts, and under-reads.

The window exists because `V_P` depends on the prompt lengths a pod is fed,
and a median over the observation's six hours would keep a load long gone
and win over the fresher sources. Thirty minutes is six times the 5 busy
minutes needed, so a pod saturated one minute in six keeps its source.
`busyMinutes` is counted over this window and `queuedMinutes` over the six
hours kept, so the two are not comparable; nothing compares them, and the
log line is their only reader. One busy minute from a multi-rank pod flags
the whole median a lower bound: the conservative side for an observe-only
signal.

The observation log carries:

- `busyMinutesOn`, the switch;
- `queueMinPods`, the pods whose row read a minimum this cycle, whether or
  not the switch feeds them;
- `kvFullPods`;
- `vpLowerBoundReason`.

Three limits:

- The collector issues the queue-minimum query only while a learner
  recording is on (`SetRecordTTFTInputs`), like every R1 field. With none
  on, no row carries one and no new busy minute is read: `queueMinPods`
  reads 0 with `busyMinutesOn=true`. Busy minutes already read stay for the
  window, so for up to 30 minutes after a recording stops the log can show
  `vpSource=busy-minutes` with `queueMinPods=0`. Issuing the query every
  cycle is a collector change, left for P3 or the first consumer that
  sizes from `V_P`.
- The KV-full test reads the row's KV at the cycle, a one-minute row, so a
  pod that filled and drained within the minute is not caught.
- No committed recording carries a queue minimum, so no golden moves; the
  switch is held by its unit tests (`heldBy`). The prefill-stress recording
  (P2) is the first that will exercise it.

**P3 and the bias of `f`.** P1's `f` is de-biased for Poisson arrivals, and
the de-bias errs both ways
([where it is most wrong](slo-capacity-planner-ttft.md#where-it-is-most-wrong)).
On a real imbalance it reads low: four pods at 2:1:1:1 (`f` 1.6) read about
1.50 at 100 requests a pod, about 6% low, and about 1.57 at 1500. A low `f`
over-promises TTFT. On equal pods with variable prompts it reads high, by
about `√(1+Cs²)` times the request count's excess less that excess: about
1.03 at 100 requests a pod with `Cs²` = 1. P1 corrects neither; P3 carries
both as stated limits.

The `ols-sweep-slo` run (flow control off, scales out, router TTFT recorded)
is worth preserving now, before any capture tool exists, as a Prometheus
snapshot (the TSDB snapshot API or a `promtool tsdb` dump), while Prometheus
still holds its `_bucket` series. R2 captures it from the snapshot later.

## Risks

- **The answer is read as a promise.** The footer, UNVALIDATED, the asymmetric
  staircase pass mark and the refusals exist for that.
- **Means in, percentiles out.** Recordings before R1 carry means, so tails rest
  on an assumed spread.
- **`f` moves** between windows; the sensitivity sweep shows the effect.
- **Drift from the analyzers.** D1 removes the copies first, and S1 uses the
  same library.

## Open questions

TTFT's are on [its page](slo-capacity-planner-ttft.md#open-questions).

1. **Persist the learned batch cap in the snapshot?** *Recommend:* yes, in a
   separate PR; standalone uses flags, the annotation or `--batch-cap` until
   then.
2. **Per-request logs for the joint length distribution?** *Recommend:* an
   optional input; otherwise assume independence and say so.
3. **ITL per token in the SLO analyzer (#430's known limit).** *Recommend:* a
   separate PR with its own switch, not D1.

## CLI reference

### Modes

| | Recording | Standalone | Controller (only if asked) |
| --- | --- | --- | --- |
| Input | A recording, `--cycle N` or `last` | Prometheus and a pod-to-role map | The live trackers |
| Must have | The file | `--prometheus`, `--namespace`, and `--kubeconfig` or `--role-of` | The flag that turns it on |
| Batch cap | Recorded store records | Flags or annotation through a kubeconfig, else `--batch-cap`; no learned cap | Live |
| Length histograms | After R1 | Yes | No |

**Standalone flags**: `--prometheus URL` with `--bearer-token-file`, `--ca-file`,
`--insecure-skip-verify`; `--namespace`; `--lookback` (1h) and `--step` (15s).
Flow control is inferred from its series; `--flow-control on|off` overrides;
`--fc-queue-threshold`, `--fc-kv-threshold`, `--fc-max-concurrency` give the
detector's settings. **Kubeconfig**: read-only `get`/`list` on Pods,
Deployments and LeaderWorkerSets in the namespace, for roles, readiness and the
batch cap; nothing is written. **`--role-of <role>=<selector>`**, repeated, for
example `--role-of prefill=app=m-prefill`.

```
$ whatif slo-to-rate --prometheus https://prom.example:9091 --namespace llm --model m --tpot 4ms
error: cannot tell prefill from decode pods: pass --kubeconfig, or --role-of <role>=<selector>
exit 2

$ whatif slo-to-rate --prometheus https://prom.example:9091 --namespace llm \
    --role-of prefill=app=m-prefill --role-of decode=app=m-decode --model m --tpot 4ms
decode   refused: NoBatchCap (no kubeconfig and no --batch-cap)
exit 3

$ whatif slo-to-rate --prometheus https://prom.example:9091 --namespace llm \
    --role-of prefill=app=m-prefill --role-of decode=app=m-decode \
    --batch-cap 64 --model m --tpot 4ms --input-bucket huge
decode   9.8 r/s   TPOT, fill 0.43   pinned-B (learned B), admitted without readiness [8.9 .. 10.4]
estimate from the last 1h, not a guarantee; see basis (-o json)
```

### Results and exit codes

Every answer is an `Answer{Value, Band, Basis, Flags}` or an
`Unanswerable{Reason}`; `-o json` carries a `schemaVersion`.

| Reason | When |
| --- | --- |
| `NoLine` | No ITL line, or an invalid fit |
| `PinnedDefaultB` | Only a `pinned-B` fit on the default 6 ms intercept |
| `NoCapacity` | No KV capacity reading or flag, or one whose band top holds no whole request |
| `NoBatchCap` | No batch cap known |
| `NoRoleMap` | Standalone with neither a kubeconfig nor `--role-of` |
| `NoTraffic` | No traffic in the window: an output length or KV footprint per request that is not positive |
| `NoPrefillCapacity` | No `V_P` from a conservative source (always on SGLang) |
| `NoFixedLatency` | No `a`: P1 refused its low-load fit, or the reading is not positive |
| `NoImbalance` | More than one prefill pod and no `f`, learned or assumed (`f` = 1 is never assumed silently: it over-promises) |
| `NoPrefillReplicas` | A TTFT question with no prefill pod to price |
| `TokenGapPercentile` | A percentile over token gaps (ITL); per-request TPOT percentiles are supported |
| `NoTPOTHistogram` | A per-request TPOT percentile with no router or engine TPOT histogram (the router's needs `usage` on responses) and no `--assume-tpot-ratio` |
| `TPOTQuantileUnbounded` | The TPOT `q` quantile falls in the histogram's `+Inf` bucket |
| `TargetBelowFloor` | The target is below what any number of replicas reaches |
| `FlowControlUnknown` | Flow control on and the detector's thresholds unknown |
| `AboveDecodeOnset` | A TTFT asked at or above the decode gate's onset with flow control on, where the gate is not work conserving and TTFT is not priced |
| `Saturated` | Question 2's operating rate (`--rate`) or the decode admission chain at or above the knee, where no fill or admission wait exists; both saturate at the same rate. A TTFT at or above the prefill knee too |
| `NoDecodeReplicas` | No decode replica read from the fleet |
| `MalformedHistogram` | A TPOT histogram whose data cannot be read: a NaN, non-positive or falling edge, a negative or non-finite count, observations with no sum. Counts that dip, as `rate()` leaves them, are made monotone, not refused |
| `TooManySlots` | More than 1,000,000 slots on one replica (a cap or capacity read wrongly); the chain holds one state per slot |
| `InvalidInput` | A usage or configuration error, exit `2`, not `3`: a NaN or infinite target, rate or assumed ratio, a percentile outside (0, 1), an assumed `R_q` below 1, a headroom outside (0, 1] (outside (0, 1) for question 2), both `--rate` and a headroom for question 2, an invalid band |

**Flags**, never refusals: extrapolation (with its distance), a borrowed, held
or snapshot line, a lower-bound or assumed `V_P`, no readiness, an unstable
tier, an assumed `R_q`, a pooled `R_q` on a bucket, a coarse TPOT bucket, the
engine TPOT histogram used in place of the router's, a rate held down by decode
admission, flow control inferred off, above a
flow-control onset, several priority bands, and UNVALIDATED on TTFT. The decode
library's flags today: `AssumedTPOTRatio`, `CoarseTPOTBucket`,
`BelowFittedBand` (a fill, or the knee, read at the band's bottom because it
sits below the band the line was fitted over) and `KVBoundSlots` (admission
slots set by the requests the band's top holds rather than the batch cap). The
TTFT model's (P3) are listed in
[its as-built section](slo-capacity-planner-ttft.md#as-built-p3).

Exit codes: `0` every role answered; `3` a role refused (output still printed);
`2` usage or input error; `1` internal error. `whatif.ExitCode` applies them:
`2` if any result is `InvalidInput`, else `3` if any is refused, else `0`.

### Recording mode, as built (D3)

`whatif replicas-to-slo` and `whatif slo-to-rate` answer for decode from a
recording, `--recording FILE --cycle N|last`. TTFT, prefill and the fleet row
came later: [TTFT, as built](#ttft-as-built).

- **Shape.** Each length is measured or assumed, and the output says which.
  Measured is the request-rate-weighted mean of the variant's last 20 cycles
  with traffic (5 minutes at a 15 s cycle), so a recording that ends idle is
  priced at the traffic it last served. `--assume-input-tokens` and
  `--assume-output-tokens` state a length instead, flagged `AssumedShape`;
  the worked example states IL = OL = 1000.
- **Bands.** Each resample is refitted by the tracker's own choice of fit
  (`itl.Refit`): the full fit only on steady readings that are ready, else
  pinned-B on the readings the pinned fit reads, else the held slope. An
  above-band line is refitted on the above-band readings. The bootstrap is
  stratified: cycles holding steady readings are drawn among themselves
  until the resample holds exactly the steady window's count, and the other
  cycles among themselves. Whether the tracker takes the full fit turns on
  that count as much as on the spread; drawn freely, a 15-reading window
  gave resamples of 8 to 20, windows the tracker never had. `--seed` sets
  the bootstrap's seed; the JSON names it.
- **Tier flags.** When more than 10% of the resamples land at another tier
  (either way, a hold or no fit included), the answer is flagged:
  `TierUnstable` when those resamples' median figure lies more than 5% from
  the answer, the tier moving the figure; `LineNearGate`, informational,
  when it does not. Neither changes the exit code. The text gives the count:
  `TierUnstable (n/200 resamples changed tier)`, `LineNearGate (n/200)`.
- **The worked example is flagged `LineNearGate (22/200)`.** Its line rests on
  15 steady readings spanning 0.322 of fill, against the tracker's 0.30
  gate, so a resample that narrows below the gate refits pinned-B; the
  figures those resamples price stay within 5% of the answer, and the mean
  TPOT's whole band, [3.65 .. 3.70] ms, is under 2% wide. Nothing to do: the flag
  says the line sits near the gate, not that the answer is in doubt.
- **Exit codes.** A recording file that is missing or unreadable, or is not
  a learner recording, is an input error: exit `2`, like a model or cycle
  the recording does not hold. A recording that fails inside the analyzers,
  or a cancelled run, is exit `1`.

### Prometheus mode, as built (D4)

`whatif replicas-to-slo` and `whatif slo-to-rate` also answer from a window of
Prometheus, in place of `--recording`:

```
whatif replicas-to-slo|slo-to-rate --prometheus URL --namespace NS
    (--kube-state-metrics | --role-of ROLE=SELECTOR ...) --from T0 --to T1
    [--step 15s] [--policy FILE] [--engine vllm|sglang] [--engine-args "ARGS"]
    [--batch-cap N] [--gpus-per-replica 1] [--accelerator NAME]
    [--bearer-token-file FILE] [--ca-file FILE] [--insecure-skip-verify]
    [--timeout 2m30s] [--force]
    [the question's flags: --cycle, --model, --decode, --tpot, -o json, ...]
```

- **What it is.** An adapter, with no switch and no pricing of its own. It
  captures the window into memory with `whatifsource.Capture`, the same call
  `whatif capture` makes, then plays the capture through the analyzers with
  the same `Generate` call `whatif generate` makes, then answers from that
  recording as `--recording` does (`learnreplay.StateAt` at `--cycle`, the
  last by default). The answer is the one the three commands give on files:

  ```
  whatif capture --prometheus URL --namespace NS --role-of ... --from T0 --to T1 -o w.capture.jsonl.gz
  whatif generate -in w.capture.jsonl.gz -o w.jsonl
  whatif slo-to-rate --recording w.jsonl --namespace NS --tpot 4ms
  ```

  `TestPrometheus_TheAnswerIsTheFilesAnswer` holds the two to the same
  output, byte for byte in text and in JSON, but for `source`, which names
  where the answer was read from.
- **The window's flags are capture's**, with the same meaning, defaults and
  checks: [What-if capture](../developer-guide/whatif-capture.md) lists
  them, and its safety holds here too (the timeout, the token sent over https
  to Prometheus' own host only, the CA file, the window and sample bounds
  that `--force` lifts, every sample on the step grid). Without
  `--prometheus` each of them is a usage error; `--recording` and
  `--prometheus` together are one too.
- **Memory.** Nothing is written to disk, so this mode holds more at once
  than `capture` does: the range queries' samples (as capture holds them,
  bounded by `whatifsource.MaxSamples` unless `--force`), then the gzipped
  capture, the capture unzipped, and the generated recording, all in RAM.
  `--force` lifts the bound on the first only; for a window near the bound,
  capture and generate to files instead.
- **`--namespace`** is required, and is both the namespace captured and the
  one answered for.
- **`--batch-cap`** is the engine's batch cap the window is captured with, as
  `capture --batch-cap`: the engine then prices at it, as a controller would
  have. It is not a second cap to price at. To price another cap, capture
  and generate the window once, then ask `--recording` with `--batch-cap`.
- **The standalone flags above that are not built yet** (`--kubeconfig`,
  `--lookback`, `--flow-control` and the `--fc-*` settings) are not in this
  mode. The role map is `--kube-state-metrics` or `--role-of`, as for
  capture.
- **Warnings** (`--insecure-skip-verify`, a batch cap given twice) and
  the capture's caveats, as `generate` prints them (`whatif: caveat: ...`),
  go to stderr, so that `-o json` is still one object.
- **The text answer** opens with the window it was answered from:
  `answered for NS/MODEL from URL, T0 to T1 every STEP (captured and
  generated in memory)`. The rest, the footer included, is the text
  `--recording` prints on the files.
- **Secrets in the URL.** The URL is printed, in the source line and in
  `source.window`, as its scheme and host alone. Its userinfo (a user and
  password), path (a gateway's `/t/TOKEN/`), query (`?token=...`) and
  fragment are dropped, not redacted value by value, since any of them can
  carry a secret. In an error message (capture's too, in text and in the
  `-o json` error object), they are replaced by `REDACTED`, in one pass,
  since the HTTP client quotes a request's whole URL when it cannot send it.
  The userinfo, the whole query and each `key=value` pair are replaced at
  any length. The path, the password and each query value on its own are
  replaced only when at least 4 characters long, so that a short value
  (`x=1`) does not rewrite every `1` in the message. Each is matched as
  written and in both escapings of a space (`+` and `%20`). The
  `--bearer-token-file` token is never printed: it travels in the
  `Authorization` header, over https, to Prometheus' own host only.
- **Exit codes** are capture's and the question's. `2`: a usage error, a
  window too large without `--force`, a role map that attributes no Pod, a
  model or cycle the window does not hold. `1`: a failed range query
  (`whatifsource.ErrQueryFailed`), or a replay that fails inside the
  analyzers. `3`: a role refused. With `-o json`, an error is one JSON error
  object, as in recording mode.
- **The JSON** names the window in `source.window` (`prometheus`, the URL's
  scheme and host alone, `from`, `to`, `stepSeconds` and `roles`), and
  `source.recording` is empty. The field is new and optional, absent in
  recording mode, so `schemaVersion` stays `1`.

### TTFT, as built

`whatif replicas-to-slo` and `whatif slo-to-rate` answer TTFT for one P/D
fleet, from `--recording`, from the files `capture` and `generate` write, and
from `--prometheus`. All three replay the same recording, so they give the
same answer: `TestPrometheus_TheTTFTAnswerIsTheFilesAnswer` holds them to it,
byte for byte, on both questions, answers and refusals. No flag about TTFT, no
TTFT: the decode answers are as they were.

| Flag | What it does |
| --- | --- |
| `--ttft 200ms@p95` | `slo-to-rate`: the end-to-end target, a mean (`200ms`) or a percentile. Without `--tpot`, only TTFT is asked |
| `--ttft-percentile 90` | `replicas-to-slo`: the percentile. Default: the policy's `slo.ttft`, else 90. On `slo-to-rate`, beside a plain `--ttft` |
| `--assume-vp 30000` | `V_P`, prompt tokens per second, in place of the learned one, as every `--assume-*` flag overrides the recording. Neither rescaled nor margined |
| `--prefill N` | Prefill pods to price; 0 takes the serving pods at the cycle |
| `--prefill-variant V` | The prefill variant to price, when the model has several: the CLI prices one fleet, and a usage error says so otherwise. It gets the whole rate, named as assumed |

**Where each input comes from.**

| Input | Source | With no source |
| --- | --- | --- |
| `V_P` | `--assume-vp`, flagged `AssumedVP` and printed `V_P ASSUMED`, with any learned figure beside it (`V_P 130000 ASSUMED, learned 115621`, and `vpLearned` in the JSON); else P1's estimate at the cycle, read through the replay | `NoPrefillCapacity`. It never defaults |
| `a` | P1's estimate at the cycle | `NoFixedLatency`. There is no `--assume-a`, and S1 has no assumption for it |
| `f` | P1's own at the pod count, `--prefill N` included: a reading of that count, so unflagged, where S1 clamps at any other count | One pod: none, exactly 1. Otherwise `min(N_p, max(1.5, f))`, `f` the learned figure at the recorded count or the raw share, flagged `AssumedImbalance` (S1, decision 1). The raw share is the largest over the last 5 cycles that read one, at the current pod count: a pod with no rate counts as one that computed nothing, and an idle tail forgets nothing, as in S1. It reads serving rows, not S1's settled ones: a pod warming up reads low, which raises the share, the permitted side |
| Prompt | `--assume-input-tokens`, else computed tokens over requests on the prefill rows, over the last 20 cycles with traffic | The decode variant's `IL·(1 − hit)`, named. Neither is `NoTraffic` |
| `cv` | No recording has one: it needs Q1's histogram | 2, flagged `AssumedPromptSpread` (S1) |
| Arrivals | None | Poisson, flagged `AssumedPoissonArrivals`, the S1 flag, kept in `cmd/whatif` because P3 has none |
| `T_xfer` | Decode rows' `AvgKVTransferTime` over the measured prompt | Left out, flagged `TransferOmitted` |
| Decode side | The decode variant's line, if `whatif.KneeRate` prices it | The decode engine's `AvgTTFT`, readings above 0 only: the largest of the last 5 cycles, with that cycle's prompt, flagged `DecodeEngineMeasured`. A cycle with no reading holds an empty slot, so the figure ages out of an idle tail. Else left out, flagged `DecodeSideOmitted` and `DecodeTTFTUnmeasured` |
| Flow control | On when the scheduler queue exists at the cycle, S1's rule (`domain.AnalyzerInput.FlowControlSeries`). A pool or a wait histogram does not count: both are filled only while recording, and wait buckets read at rate 0 within a run's first 3 to 4 minutes on runs `goldens.yaml` describes as off, cause not known (74 cycles across five committed recordings) | Off, flagged `FlowControlInferredOff`. On takes the upstream thresholds 5 and 0.8, and is named as assumed. On with no decode line is answered, flagged `DecodeOnsetUnchecked`; S1 refuses that case, a planner reads the flag. `--flow-control` is not built |

Every assumed input is named on the `assumed` row, and in `ttft.assumed` in the
JSON, the two whole-rate assumptions among them: `--prefill-variant` gives one
variant the whole rate, and with several decode variants each is priced at the
model's whole rate, as S1 prices decode. The assumed figures are S1's own: both
read them from `internal/signals/whatif/ttft_assumed.go`.

Two more flags are S1's (#486):

- **`RoutingBound`** is `f / N_p` of at least 0.8, sustained: a learned `f`, or a
  raw share at that level over all of a full 5-cycle window. **`ImbalanceNotSustained`**
  is that level not held: chance alone can read it at two pods. Either way the
  answer is priced at that `f`, the side that asks for more replicas, and the
  flag is read at the recorded count only.
- **`PromptSpreadAbovePeak`** is a `cv` above the peak of a mean-one gamma's
  quantile for the percentile: p70 0.79, p80 1.30, p85 1.66, p88 1.96, p90 2.22,
  p92 2.56, p93 2.78, p94 and up 3. Above it a larger `cv` can price a shorter
  floor and a higher rate. At the assumed 2 it is raised at p88 and below.

**Reading the output.** Question 2 adds the rows below. The operating rate is the
headroom times the lower knee, so the prefill knee can set it. The TPOT is then
read at that rate, and a figure read there rests on `V_P`: the rate and TPOT
rows carry the knee's flags and print the verdict, and the decode role's JSON
gives `inputs.rate` and `inputs.rateSource`. On `ols-sweep` at `--assume-vp 8000`
the rate is 6.80 r/s and the mean TPOT 2.38 ms, against 11.32 and 3.66 with the
decode knee the lower. That rate has no band: `V_P` has no resamples. Question 1
adds a `prefill` row, the rate at the target alone, and a `fleet` row, the
smallest of the roles' rates and the operating point of the lower knee, each
named under `bound by`.

```
prefill knee    30.00 r/s   N_p 1 x V_P 30000 / (f 1 x 1000 tokens); UNVALIDATED; V_P ASSUMED
TTFT p90        353.4 ms                     end to end; UNVALIDATED; V_P ASSUMED
                (323.4 ms if prefill and decode waits were independent)
                (floor 113.3 ms, pooled 311.4 ms, worst pod utilization 0.377)
ttft inputs     V_P 30000 ASSUMED; a 11.04 ms learned; f 1 (one pod); N_p 1 (--prefill); prompt 1000 tokens (assumed); decode by its line
assumed         V_P 30000 tok/s (--assume-vp); prompt 1000 tokens (--assume-input-tokens); prompt cv 2 (no computed-prompt histogram); flow control off (no queue series); Poisson arrivals
```

That is the worked example with the CLI's `cv` of 2, so 353 ms where the page,
at `cv` 1, has 160 ms; `a` is P1's 11.04 ms. Question 1 at 200 ms p95 reads
1.19 r/s where the page has 11.10.
`TestTTFT_TheWorkedExampleFromTheRecording` puts `cv` 1 back on the fleet the
CLI read, and gets the page's figures within 1 ms, 1.6 ms lower as the page
says. `TestTTFT_HandComputedAtCV2` holds a figure at `cv` 2 through the command
to a calculation that reads no Go: 4.752942 s on run 4 at `--assume-vp 130000`.
Both check the arithmetic. Neither validates the model. The `ttft-*.golden`
files are regression pins, read back from the code.

**Every TTFT output says what it is.** Each figure prints `UNVALIDATED`, and
`V_P ASSUMED` when `--assume-vp` supplied it. The closing line, and the JSON's
`note`, give the direction for what the CLI can print: "at the assumed prompt
cv 2 it read 1.1x to 4.1x the router's percentile on the P2 recordings; at the
sent spread (cv 0.06) up to 34% under it". At `cv` 2 the frozen table in
`ttft_validation_test.go` reads 13 of 14 levels over the 25% mark, none under,
and one pass; `TestTTFTFrozen_TheNotesFigures` holds the note to the table. With
a measured `cv` (Q1) the note keeps only the sent spread's half.

**Refusals.** A refusal is a row, not a number, and exit `3`, with the rest of
the output printed. A rate at or above the knee is `Saturated`. Aggregated TTFT
is `NoPrefillReplicas`, with a detail that names M1. `--input-bucket` is a usage
error that names M1. A bad flag is exit `2`, whatever the recording holds. TTFT
has no band: `V_P`, `a` and `f` have no resamples, and a band on the ITL line
alone would read narrower than the truth. A rate the prefill knee sets has none
either.

**What the committed recordings answer.** None of the four prefill-stress
goldens has both `V_P` and `a`, so none answers from learned figures alone:
`ttft-open-loop-short` learns `a` and answers with `--assume-vp`;
`ttft-saturated` learns `V_P`, and `ttft-three-pods` a lower bound on it, and
both refuse `NoFixedLatency`; `ttft-open-loop-long` refuses `V_P` and `a`. On
all four the decode side is the decode engine's TTFT, or the refusal comes first:
none prices a decode line. A fleet with both figures is tested on a built
state (`TestTTFT_VPOrder`), not on a recording.

**The JSON** adds roles `prefill` and, on question 1, `fleet`; `ttft` on the
prefill role, with each input and its source; `results.ttft`, with `parts`
beside the value; `basis.prefillOnlyRate`; and `inputs.rateSource` on the decode
role. A TTFT rate's `basis.floor` and `basis.kneeRate` are the TTFT's. Every key
is new and absent with no TTFT asked, so `schemaVersion` stays `1`, as for
`source.window`.

**Replay.** `learnreplay.State` gains `Prefill`, `FlowControlSeries` and each
variant's `DecodeTTFT` (`state_ttft.go`). They are read from the cycles' rows and
the saturation analyzer's stash, and nothing is fed back: no switch, no golden
moves.

### JSON output

`-o json` (or `--output json`) writes one object. Abridged from the worked
example (`cmd/whatif/testdata/replicas-to-slo-worked-json.golden`; of its
four results only `meanTPOT` is kept here):

```json
{
  "schemaVersion": 1,
  "question": "replicas-to-slo",
  "source": {
    "recording": "../../internal/engines/analyzers/learnreplay/testdata/goldens/ols-sweep/recording.jsonl.gz",
    "namespace": "ns-01",
    "model": "model-02",
    "firstCycle": 1,
    "lastCycle": 226,
    "totalCycles": 226,
    "firstAt": "2026-11-01T21:54:30.377845609Z",
    "lastAt": "2026-11-01T22:50:53.616794067Z"
  },
  "bands": {
    "method": "block bootstrap",
    "blockCycles": 4,
    "resamples": 200,
    "seed": 1,
    "loPercent": 10,
    "hiPercent": 90
  },
  "roles": [
    {
      "role": "decode",
      "variant": "variant-01",
      "inputs": {
        "line": {
          "a": 0.004437632383842851,
          "b": 0.0015568837184686396,
          "tier": "ols",
          "readings": 15,
          "observedFill": {
            "lo": 0.17953730856956665,
            "hi": 0.501466275659824
          },
          "fromCycle": 197,
          "toCycle": 220,
          "resamples": 200,
          "tierChanges": 22
        },
        "fillBand": {
          "lo": 0.15,
          "hi": 0.8
        },
        "capacityTokens": 131072,
        "kvPerRequest": 1500,
        "shape": {
          "inputTokens": 1000,
          "inputSource": "assumed",
          "outputTokens": 1000,
          "outputSource": "assumed",
          "prefixHitRate": 0,
          "fromCycle": 204,
          "toCycle": 223,
          "cycles": 20
        },
        "batchCap": 64,
        "batchCapSource": "flags",
        "ranks": 1,
        "replicas": 1,
        "replicasFrom": "recording",
        "headroom": 0.85,
        "tpot": {
          "percentile": 0.9,
          "ratio": 1.3,
          "ratioSource": "assumed",
          "windows": 0
        }
      },
      "results": {
        "meanTPOT": {
          "answer": {
            "value": 0.0036607509947803553,
            "unit": "s",
            "band": {
              "lo": 0.003646523903932082,
              "hi": 0.0036965490906630907
            },
            "basis": {
              "fill": 0.4740967917873882,
              "kneeRate": 13.313632625239457,
              "floor": 0.002222528576045067,
              "uncapped": 0,
              "boundBy": [],
              "tier": "ols"
            },
            "flags": [
              "LineNearGate",
              "AssumedShape"
            ]
          }
        }
      }
    }
  ],
  "exitCode": 0,
  "note": "estimate from 15 readings (cycles 197-220) of a 226-cycle recording, not a guarantee; see basis (-o json)"
}
```

| Field | Meaning |
| --- | --- |
| `schemaVersion` | The schema below. Removing, renaming or retyping a key, or changing its meaning, is a new version; a key absent unless a new flag asks for it is not. `cmd/whatif` is in no release. |
| `question` | `replicas-to-slo` or `slo-to-rate`. |
| `source` | The recording, the model, `firstCycle` (1), `lastCycle` (the cycle answered at) and `totalCycles` (the recording's), with their clocks. With `--prometheus`, `recording` is empty and `window` names the window: `prometheus` (the URL's scheme and host alone), `from`, `to`, `stepSeconds`, and `roles` (`kube-state-metrics` or `role-of`); absent in recording mode. Absent on an error object. |
| `bands` | How the bands were drawn: block bootstrap, `blockCycles`, `resamples`, `seed`, the percentiles. Absent on an error object. |
| `roles[]` | One per decode variant priced: `inputs` (what it was priced from) and `results`. |
| `inputs.line` | The ITL line (`a` in seconds per unit of fill, `b` in seconds), its `tier`, the `readings` it was fitted from, their `observedFill` range and `fromCycle`-`toCycle`, and of the bootstrap's `resamples` how many (`tierChanges`) landed at another tier. |
| `inputs.shape` | Lengths in tokens, each with its source, `measured` (over `fromCycle`-`toCycle`) or `assumed`. |
| `results` | `knee`, `operatingRate`, `meanTPOT`, `percentileTPOT` for question 2; `rate` for question 1. A question not asked has no key. |
| a result | Exactly one of `answer` and `refused`. |
| `answer` | `value` in `unit`: `r/s` (requests per second over the fleet) for a rate, `s` for a TPOT. `band`, when present, is in the same unit. `basis` is what it rests on (`fill`, `kneeRate` in r/s, `floor` in s, `uncapped` in r/s, `boundBy`, `tier`; a field that does not apply is 0). `flags` is always an array. |
| `refused` | `reason`, one of the reasons table above, and `detail`. |
| `inputs.tpot` | `targetSeconds` (question 1, seconds), `percentile` (0 to 1), `ratio` (R_q) and where it came from. |
| `exitCode` | The process's exit code, as above. |
| `error` | Present only when no answer was reached (exit `2` or `1`): `message`; the object then holds `schemaVersion`, `question`, `roles` (empty) and `exitCode` alone. |

To read a figure, for example the mean TPOT in milliseconds:

```
whatif replicas-to-slo --recording run.jsonl.gz -o json | jq '.roles[0].results.meanTPOT.answer.value * 1000'
```

Every key path, with its JSON type, for `schemaVersion` 1 (the `cmd/whatif`
tests hold the wire types to this list):

<!-- whatif-json-schema-v1: begin -->
```
bands object
bands.blockCycles number
bands.hiPercent number
bands.loPercent number
bands.method string
bands.resamples number
bands.seed number
error object
error.message string
exitCode number
note string
question string
roles array
roles[] object
roles[].inputs object
roles[].inputs.batchCap number
roles[].inputs.batchCapSource string
roles[].inputs.capacityTokens number
roles[].inputs.fillBand object
roles[].inputs.fillBand.hi number
roles[].inputs.fillBand.lo number
roles[].inputs.headroom number
roles[].inputs.kvPerRequest number
roles[].inputs.line object
roles[].inputs.line.a number
roles[].inputs.line.b number
roles[].inputs.line.borrowedFrom string
roles[].inputs.line.fromCycle number
roles[].inputs.line.observedFill object
roles[].inputs.line.observedFill.hi number
roles[].inputs.line.observedFill.lo number
roles[].inputs.line.readings number
roles[].inputs.line.resamples number
roles[].inputs.line.tier string
roles[].inputs.line.tierChanges number
roles[].inputs.line.toCycle number
roles[].inputs.ranks number
roles[].inputs.rate number
roles[].inputs.rateSource string
roles[].inputs.replicas number
roles[].inputs.replicasFrom string
roles[].inputs.shape object
roles[].inputs.shape.cycles number
roles[].inputs.shape.fromCycle number
roles[].inputs.shape.inputSource string
roles[].inputs.shape.inputTokens number
roles[].inputs.shape.outputSource string
roles[].inputs.shape.outputTokens number
roles[].inputs.shape.prefixHitRate number
roles[].inputs.shape.toCycle number
roles[].inputs.tpot object
roles[].inputs.tpot.percentile number
roles[].inputs.tpot.ratio number
roles[].inputs.tpot.ratioSource string
roles[].inputs.tpot.targetSeconds number
roles[].inputs.tpot.windows number
roles[].results object
roles[].results.knee object
roles[].results.knee.answer object
roles[].results.knee.answer.band object
roles[].results.knee.answer.band.hi number
roles[].results.knee.answer.band.lo number
roles[].results.knee.answer.basis object
roles[].results.knee.answer.basis.boundBy array
roles[].results.knee.answer.basis.boundBy[] string
roles[].results.knee.answer.basis.fill number
roles[].results.knee.answer.basis.floor number
roles[].results.knee.answer.basis.kneeRate number
roles[].results.knee.answer.basis.prefillOnlyRate number
roles[].results.knee.answer.basis.tier string
roles[].results.knee.answer.basis.uncapped number
roles[].results.knee.answer.flags array
roles[].results.knee.answer.flags[] string
roles[].results.knee.answer.unit string
roles[].results.knee.answer.value number
roles[].results.knee.refused object
roles[].results.knee.refused.detail string
roles[].results.knee.refused.reason string
roles[].results.meanTPOT object
roles[].results.meanTPOT.answer object
roles[].results.meanTPOT.answer.band object
roles[].results.meanTPOT.answer.band.hi number
roles[].results.meanTPOT.answer.band.lo number
roles[].results.meanTPOT.answer.basis object
roles[].results.meanTPOT.answer.basis.boundBy array
roles[].results.meanTPOT.answer.basis.boundBy[] string
roles[].results.meanTPOT.answer.basis.fill number
roles[].results.meanTPOT.answer.basis.floor number
roles[].results.meanTPOT.answer.basis.kneeRate number
roles[].results.meanTPOT.answer.basis.prefillOnlyRate number
roles[].results.meanTPOT.answer.basis.tier string
roles[].results.meanTPOT.answer.basis.uncapped number
roles[].results.meanTPOT.answer.flags array
roles[].results.meanTPOT.answer.flags[] string
roles[].results.meanTPOT.answer.unit string
roles[].results.meanTPOT.answer.value number
roles[].results.meanTPOT.refused object
roles[].results.meanTPOT.refused.detail string
roles[].results.meanTPOT.refused.reason string
roles[].results.operatingRate object
roles[].results.operatingRate.answer object
roles[].results.operatingRate.answer.band object
roles[].results.operatingRate.answer.band.hi number
roles[].results.operatingRate.answer.band.lo number
roles[].results.operatingRate.answer.basis object
roles[].results.operatingRate.answer.basis.boundBy array
roles[].results.operatingRate.answer.basis.boundBy[] string
roles[].results.operatingRate.answer.basis.fill number
roles[].results.operatingRate.answer.basis.floor number
roles[].results.operatingRate.answer.basis.kneeRate number
roles[].results.operatingRate.answer.basis.prefillOnlyRate number
roles[].results.operatingRate.answer.basis.tier string
roles[].results.operatingRate.answer.basis.uncapped number
roles[].results.operatingRate.answer.flags array
roles[].results.operatingRate.answer.flags[] string
roles[].results.operatingRate.answer.unit string
roles[].results.operatingRate.answer.value number
roles[].results.operatingRate.refused object
roles[].results.operatingRate.refused.detail string
roles[].results.operatingRate.refused.reason string
roles[].results.percentileTPOT object
roles[].results.percentileTPOT.answer object
roles[].results.percentileTPOT.answer.band object
roles[].results.percentileTPOT.answer.band.hi number
roles[].results.percentileTPOT.answer.band.lo number
roles[].results.percentileTPOT.answer.basis object
roles[].results.percentileTPOT.answer.basis.boundBy array
roles[].results.percentileTPOT.answer.basis.boundBy[] string
roles[].results.percentileTPOT.answer.basis.fill number
roles[].results.percentileTPOT.answer.basis.floor number
roles[].results.percentileTPOT.answer.basis.kneeRate number
roles[].results.percentileTPOT.answer.basis.prefillOnlyRate number
roles[].results.percentileTPOT.answer.basis.tier string
roles[].results.percentileTPOT.answer.basis.uncapped number
roles[].results.percentileTPOT.answer.flags array
roles[].results.percentileTPOT.answer.flags[] string
roles[].results.percentileTPOT.answer.unit string
roles[].results.percentileTPOT.answer.value number
roles[].results.percentileTPOT.refused object
roles[].results.percentileTPOT.refused.detail string
roles[].results.percentileTPOT.refused.reason string
roles[].results.rate object
roles[].results.rate.answer object
roles[].results.rate.answer.band object
roles[].results.rate.answer.band.hi number
roles[].results.rate.answer.band.lo number
roles[].results.rate.answer.basis object
roles[].results.rate.answer.basis.boundBy array
roles[].results.rate.answer.basis.boundBy[] string
roles[].results.rate.answer.basis.fill number
roles[].results.rate.answer.basis.floor number
roles[].results.rate.answer.basis.kneeRate number
roles[].results.rate.answer.basis.prefillOnlyRate number
roles[].results.rate.answer.basis.tier string
roles[].results.rate.answer.basis.uncapped number
roles[].results.rate.answer.flags array
roles[].results.rate.answer.flags[] string
roles[].results.rate.answer.unit string
roles[].results.rate.answer.value number
roles[].results.rate.refused object
roles[].results.rate.refused.detail string
roles[].results.rate.refused.reason string
roles[].results.ttft object
roles[].results.ttft.answer object
roles[].results.ttft.answer.basis object
roles[].results.ttft.answer.basis.boundBy array
roles[].results.ttft.answer.basis.boundBy[] string
roles[].results.ttft.answer.basis.fill number
roles[].results.ttft.answer.basis.floor number
roles[].results.ttft.answer.basis.kneeRate number
roles[].results.ttft.answer.basis.prefillOnlyRate number
roles[].results.ttft.answer.basis.tier string
roles[].results.ttft.answer.basis.uncapped number
roles[].results.ttft.answer.flags array
roles[].results.ttft.answer.flags[] string
roles[].results.ttft.answer.parts object
roles[].results.ttft.answer.parts.decodeEngine number
roles[].results.ttft.answer.parts.decodeKnee number
roles[].results.ttft.answer.parts.decodeShare number
roles[].results.ttft.answer.parts.decodeWait number
roles[].results.ttft.answer.parts.decodeWaitProbability number
roles[].results.ttft.answer.parts.engineWait number
roles[].results.ttft.answer.parts.firstStep number
roles[].results.ttft.answer.parts.independent number
roles[].results.ttft.answer.parts.onsets object
roles[].results.ttft.answer.parts.onsets.decode number
roles[].results.ttft.answer.parts.onsets.prefill number
roles[].results.ttft.answer.parts.ownPrefill number
roles[].results.ttft.answer.parts.pooled number
roles[].results.ttft.answer.parts.prefillKnee number
roles[].results.ttft.answer.parts.prefillUtilization number
roles[].results.ttft.answer.parts.vpMarginApplied number
roles[].results.ttft.answer.parts.waitOffset number
roles[].results.ttft.answer.parts.waitScale number
roles[].results.ttft.answer.unit string
roles[].results.ttft.answer.value number
roles[].results.ttft.refused object
roles[].results.ttft.refused.detail string
roles[].results.ttft.refused.reason string
roles[].role string
roles[].ttft object
roles[].ttft.arrivalsSource string
roles[].ttft.assumed array
roles[].ttft.assumed[] string
roles[].ttft.decodeSide string
roles[].ttft.decodeTTFTPromptTokens number
roles[].ttft.decodeTTFTSeconds number
roles[].ttft.decodeVariant string
roles[].ttft.fixedLatencySeconds number
roles[].ttft.fixedLatencySource string
roles[].ttft.flowControl string
roles[].ttft.imbalance number
roles[].ttft.imbalanceSource string
roles[].ttft.percentile number
roles[].ttft.promptCV number
roles[].ttft.promptCVSource string
roles[].ttft.promptFromCycle number
roles[].ttft.promptSource string
roles[].ttft.promptToCycle number
roles[].ttft.promptTokens number
roles[].ttft.rate number
roles[].ttft.rateSource string
roles[].ttft.replicas number
roles[].ttft.replicasFrom string
roles[].ttft.targetSeconds number
roles[].ttft.transferSecondsPerToken number
roles[].ttft.transferSource string
roles[].ttft.vp number
roles[].ttft.vpLearned number
roles[].ttft.vpOrigin string
roles[].ttft.vpPromptTokens number
roles[].ttft.vpSource string
roles[].variant string
schemaVersion number
source object
source.firstAt string
source.firstCycle number
source.lastAt string
source.lastCycle number
source.model string
source.namespace string
source.recording string
source.totalCycles number
source.window object
source.window.from string
source.window.prometheus string
source.window.roles string
source.window.stepSeconds number
source.window.to string
```
<!-- whatif-json-schema-v1: end -->

## See also

- [End-to-end TTFT](slo-capacity-planner-ttft.md): the TTFT model, flow control,
  percentiles and the staircase.
- [SLO tiers](slo-tiers.md): TPOT pricing, the router's TTFT, phase 2.
- [One learner lifecycle](analyzer-learner.md): the switch and golden rules, and
  the recorder.
- [Traffic shape shifts](shape-shift-treatment.md) and
  [a per-token economy](per-token-economy.md): the shape and units.
- [Capacity-planner positioning](capacity-planner-positioning.md): a planner at a
  different altitude, which would call this model.
