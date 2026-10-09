# What sharing one quota did, measured

[← Share one GPU quota between models whose peaks do not coincide](README.md)

Two models behind one gateway take turns at bursting, under **one** namespace
quota that is smaller than their two peaks together. The same traffic ran
three times, and only the policy's `optimizer:` block differed between the
runs:

- `today`: no block, so today's optimizer runs;
- `shadow`: the utilization-share optimizer computing and moving nothing;
- `share`: the same optimizer acting.

There was one run of each arm on H200 nodes, on 2026-10-08. The image was built
from `259c52f8` and deployed by digest. The scenario and the guards that keep
the arms comparable are in the
[benchmarking runbook](../../guides/benchmarking/two-model-utilization-share.md).
The tables below are that report's, unedited, except where a row says it came
from the controller log.

## The setup

| | |
| --- | --- |
| models | `unsloth/Meta-Llama-3.1-8B-Instruct` (A), `Qwen/Qwen3-8B` (B). Each has its own InferencePool and EPP, and each replica gets 1 × H200 |
| quota | `H200: 4`, one namespace quota for both models (`WVA_LIMITER=quota`) |
| bounds | each model 1–3 replicas in every arm. The two peaks together want 6, so the quota is binding |
| load | inference-perf, Poisson open-loop, 1000 in / 500 out tokens, synthetic prompts |
| rates | 3 rps between bursts. Bursts reach 9 rps for A and 8 rps for B, and A and B never burst at once |
| schedule | 120 s lead-in, B bursts for 1200 s, a 90 s band, then A bursts for 1200 s. That is 2610 s and 28 860 requests per arm |
| HPA | the shipped defaults: a 300 s scale-down window |

## What each arm held, through each rise

The fleet sampler records every replica and what its Deployment asked for every
5 s. Times are from the start of the rise.

| arm | Qwen (B) rises at 120 s | Llama (A) rises at 1410 s |
| --- | --- | --- |
| `today` | 1 at the rise; asked for 2 at +43 s, ready +127 s; 3 ready +257 s | asked for 2 at **+57 s**, ready +151 s |
| `shadow` | 1 at the rise; asked for 2 at +56 s, ready +138 s; 3 ready +239 s | asked for 2 at **+55 s**, ready +137 s |
| `share` | **2 at the rise**; 3 ready +501 s | asked for 2 at **+390 s**, ready +478 s; 3 ready +555 s |

Two mechanisms show here.

**Idle headroom goes to whoever rises first.** At idle, `share` hands the
quota's spare GPUs out as headroom, by weight. It raised both models from 1 to
2 before any burst began, so Qwen met its burst with a second replica already
serving. `today` started that burst from one replica and paid for two cold
starts while it was underway.

**At the swap, headroom has to be released before it can move.** By the time
Llama rose, `today` had already scaled Qwen down to what its load needed, so
Llama took a free GPU at once. `share` had kept Qwen at 3 as headroom, so Llama
could grow only through a transfer from Qwen. A transfer's release goes through
the donor's scale-down window. Llama started growing about **5.5 minutes later**
than under `today`.

## Time to first token

![p95 (bar) and p50 (tick) TTFT per rise window, per arm](figures/ttft-rises.svg)

Per rise, in ms. Each row is ONE scale-up event, so the requests inside a
window are not independent samples of it:

| model | rise at | arm | served | failed | p50 | p95 | max |
| --- | ---: | --- | ---: | ---: | ---: | ---: | ---: |
| Llama | 1410 s | today | 2160 | 0 | 60 | 1379 | 2419 |
| Llama | 1410 s | shadow | 2160 | 0 | 2526 | 10231 | 11354 |
| Llama | 1410 s | share | 2160 | 0 | 3817 | 8665 | 10559 |
| Qwen | 120 s | today | 1920 | 0 | 1417 | 7732 | 8697 |
| Qwen | 120 s | shadow | 1920 | 0 | 135 | 4796 | 5499 |
| Qwen | 120 s | share | 1920 | 0 | 73 | 126 | 208 |

**Read `shadow` before `share`.** A shadow arm moves nothing, so it makes the
same decisions as `today`. On Llama's rise it held exactly the replicas `today`
held, at the same times, yet its p95 is 8.9 s higher. That spread comes from
serving, not from scaling, and it is larger than the difference between
`share` and either other arm. **This run does not say whether `share` helped or
hurt Llama's latency.** The table above, which shows when the replicas came,
is the evidence for that rise.

Qwen's rise is different. `share` came in 4.7 s below `shadow` and 7.6 s below
`today`, both wider than the 2.9 s between `today` and `shadow`. The mechanism
is also visible: there was a second replica already serving when the burst
began.

## Accelerators

![GPU-seconds per arm](figures/gpu-seconds.svg)

| arm | GPU-seconds | peak GPUs |
| --- | ---: | ---: |
| `today` | 8311 | 4 |
| `shadow` | 8328 | 4 |
| `share` | 10308 | 4 |

`share` spent **24% more** GPU-seconds. That is the documented cost of the mode
and the reason not to take it where idle GPUs could be given back: it holds the
whole quota, idle or not.

![replicas per model over the whole run, per arm](figures/fleet-timeline.svg)

## What the optimizer did

From the controller log of the `share` arm. Every transfer ended `done`: no
abort, fill timeout or wrong pod.

| what | detail |
| --- | --- |
| idle fills | at the start, one replica each to Qwen and Llama from spare quota |
| Llama → Qwen | when Qwen rose; released after about 6 min, then Qwen raised |
| Qwen → Llama | two concurrent transfers when Llama rose, both released after about 5 min 45 s |
| Qwen ← Llama | one more started as Llama's burst ended, evening out headroom again |

Events on both Deployments named the pod that went and the reason, for
example *"giving one replica (1 GPUs) to model Qwen/Qwen3-8B (decode) to even
out headroom by weight; … goes"*. The two concurrent transfers from one donor
are the case a fix in this branch covers (the second mark must not rank below
the first). It ran correctly here on real hardware.

The report's own transfer count, from `increase()` on
`wva_utilization_share_transfers_total`, read 2 where the log shows 3 transfers
plus the idle fills: `increase()` does not count a counter's first increment.
Count transfers from the log or the Events. The `model-s held back` column is
also `share`-only by construction: today's optimizer emits no blocked reason
when the quota is simply spent.

## What this says, and how far it goes

- **The mechanics work on real hardware.** Release came before the fill every
  time. The marked pods were the ones removed, and the Events said what
  happened.
- **Headroom by weight is insurance for the first rise, and a delay at the
  swap.** Here the delay was one donor scale-down window, 300 s, because
  headroom held by the falling model is only released through a transfer.
  Today's optimizer does not hold that headroom, so at the swap it was faster.
  A short scale-down window for urgent receivers is designed but not built (the
  proposal's stage 3), and it would address exactly this delay.
- **It costs GPU-seconds**: here 24%.
- One run, two rises per arm, no repetition. The `today` and `shadow` arms
  disagree by up to 9 s p95 on identical decisions. A latency difference smaller
  than that is not a result. The replica timelines are.

## P/D: not measured

The same three arms were attempted on one P/D-disaggregated model
(`Qwen/Qwen3-0.6B`, prefill and decode each 1–4 replicas, a quota of 5). The
idea was for prefill and decode to compete for the quota through alternating
prompt-heavy and decode-heavy phases. In the `today` arm, decode scaled 1 → 4
on the decode-heavy phase. **Prefill never left one replica**, including through
20 minutes of 15 000-token prompts at 12 rps. With no prefill demand there is
no prefill-versus-decode competition for the optimizer to resolve, so the
comparison would only have re-measured decode. It was not run.

Prefill does queue under such load, but the scaling manager reads that pressure
on decode, not as prefill demand. Until prefill has a demand signal of its own,
a P/D quota-sharing benchmark cannot exercise transfers between the two roles.
The attempt also found two problems in the benchmark tooling, both open:

- The standup rendered the scenario's Qwen3-32B resource requests (32 CPU /
  128 GiB per engine) despite logging the Qwen3-0.6B override. They were set
  by hand.
- The decode-heavy loader (4000-token outputs) wrote no results after its
  load ended, within the 1200 s grace.
