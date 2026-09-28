# llm-scaling-manager: project proposal

An analytical autoscaler for llm-d inference: **multi-model** — one GPU budget,
every model and variant, one joint decision — with **warm capacity**,
**scale-to-zero** and **P/D-aware** scaling on top.

It began as a fork of llm-d's Workload Variant Autoscaler
(`llm-d/llm-d-workload-variant-autoscaler`, the path still in this tree's
`go.mod`) and has diverged substantially since.

A shorter version of the argument, written for people outside this repository:
[Sub-second scale-ups on llm-d](blog/sub-second-scale-ups-on-llm-d.md).

---

## Why a general-purpose autoscaler is not enough

KEDA and the HPA are good at what they were built for: turn a signal into a
replica count, quickly and safely. LLM inference breaks that in four specific
ways, and each one is a problem this project exists to solve. Each is stated
below as **the problem**, then **what we do about it**, then **the evidence**
where we have it.

---

## Problem 1 — the signals a normal autoscaler has do not mean anything stable

Request rate, queue length and request latency are the levers KEDA gives you. In
LLM serving none of them has a fixed relationship to how loaded a GPU is,
because the cost of a request depends on its *shape* — how many tokens go in,
how many come out — and on the serving configuration. Two requests to the same
model can differ by 1000× in compute. A threshold that is right at one traffic
shape is wrong at the next, and nothing in the signal tells you it has moved.

This is not theoretical. We ran a trace at a **constant 6 req/s** where only the
request shape changed halfway through — 6000 in / 1000 out tokens, then
1000 in / 4000 out. One decode replica sustains ~5.4 req/s at the first shape and
~2.5 at the second, so the correct fleet is **two replicas, then three**. What
the ordinary signals said during that run:

| signal | what it showed | what it implies |
| --- | --- | --- |
| request rate | constant, 6 req/s throughout | no change — do nothing |
| gateway queue depth | **flat, no queue ever formed** | not saturated — do nothing |
| KV-cache occupancy | 5–15 % in the steady first phase | over-provisioned — **scale down to one** |

Every one of them is wrong, and the last is actively dangerous: read alone, KV
occupancy would have cut the fleet to a single replica just before the shape
changed and the work per request quadrupled.

### What we do about it

We scale on **fleet utilization against a universal threshold** — scale up above
0.85, release below 0.70 — where utilization is computed from a capacity model
rather than read off a gauge. Demand for each role is floored at what the load
requires *in throughput*: the arrival rate divided by the completion rate one
replica sustained when it was last seen saturated. That ratio is shape-dependent
by construction, so when the shape changes the price changes with it, and the
threshold does not have to be re-tuned.

The thresholds are universal precisely because the quantity they compare is
already normalised. There is nothing per-model or per-traffic-pattern to tune.

### The evidence

In that run the third replica was ordered at **+1344 s — the same cycle in which
the new shape's completion rate first came on record**, and **no queue formed at
all** through the entire second phase. p95 TTFT held at 0.04–0.07 s once each
phase settled. A cold controller, with no completion rate on record yet, sized
the same window by occupancy instead and paid **4.2 s p95** for it — which is
what the ordinary signal is worth, measured.

→ [Scale a P/D-disaggregated model](well-lit-paths/pd-disaggregation/),
"How it is benchmarked". The pair was run twice, a day apart, landing within a
tenth of every figure.

---

## Problem 2 — reacting after saturation is already too late

A general-purpose autoscaler is reactive by design, and that is fine when a pod
starts in a second. An LLM replica is not available when it is scheduled; it is
available when the model is loaded and the kernels are compiled.

| | |
| --- | ---: |
| 8B model server, not running → first request served | **~41 s** |
| GLM-5.2-FP8 (744B MoE), warm node, weights read from local NVMe | **192 s** |
| ...of which the weights are | **40 s** |
| ...the same start on a node with a cold JIT cache | **463 s** |

Note the third row. **The weights are a fifth of a GLM start**, so faster
storage, a bigger page cache and even a perfect peer-to-peer weight transfer
cannot fix this: the rest is process spawn, imports, memory profiling, kernel
warmup and CUDA-graph capture. A cold start cannot be made fast. It can only be
*not paid*.

### What we do about it

Two things. **Order earlier**: because demand is priced from throughput rather
than observed from a queue, the decision is made before the queue exists. And
**bridge the gap**: a shared warm pool holds an accelerator with models already
resident and lends it to a model that is scaling up, so it serves while its own
replica starts.

### The evidence

Ordering earlier, from the same P/D run: the second replica was ordered at
**+53 s — from the load, not from a queue, and before the first replica tipped
into preemption**.

Bridging, from a two-model benchmark with the models bursting out of phase:

| arm | p95 TTFT per rise | GPU-seconds |
| --- | --- | ---: |
| autoscaling alone | 5.1 – 8.8 s | **12 129** |
| + one-Pod warm pool | **0.11 – 0.83 s** | 14 138 (+17 %) |
| floor of 2 replicas per model | 0.09 – 0.13 s | 16 080 (+33 %) |

A pool Pod serving real gateway traffic switched models in **437 ms against a
~41 s cold start**.

→ [What a warm pool buys, measured](well-lit-paths/warm-pool-bridge/measured.md).

---

## Problem 3 — the unit of waste is a GPU, and the models are competing for it

At $2–4/hr per accelerator, the cost of being wrong is orders of magnitude
higher than in a CPU fleet, in both directions: an unnecessary replica burns
real money, a missing one breaks an SLO. And a general-purpose autoscaler
decides **one Deployment at a time** — it has no notion that scaling model A up
may mean scaling model B down, or that both are drawing on one finite pool of
GPUs.

### What we do about it

One joint decision across the whole fleet: every model, every variant, one GPU
budget, one allocation, re-solved each cycle. A declared limiter constrains every
decision against that budget, so the autoscaler cannot ask for capacity the
namespace is not entitled to.

The warm pool is the same idea expressed in hardware — **one** held accelerator
insuring **several** models, which is why its economics improve with every model
added to it rather than degrading.

### The evidence

From the table above: the pool costs **12 % less than the floor it replaces** at
the same latency, and that advantage grows with models per Pod, not with quiet
time. Two models and one Pod hold 3 accelerators in the quiet against a floor's
4, capping the advantage at 25 % for that shape; a third model sharing the same
Pod moves the ceiling.

We also publish the uncomfortable number: **autoscaling alone is the cheapest
arm**. If you hold nothing today and can live with multi-second rises, keep
holding nothing. The pool is for people who would otherwise hold a floor.

→ [GPU capacity accounting](concepts/gpu-capacity-accounting.md) — what the
budget means, and three ways a naive reading over-states free capacity.

---

## Problem 4 — one model is several workloads, and they scale differently

A production deployment is not one Deployment per model. The same model runs as
several **variants** — different accelerators, tensor-parallel widths,
quantizations — and under prefill/decode disaggregation the prefill and decode
halves are separate workloads with opposite bottlenecks: prefill is
compute-bound, decode is memory-bandwidth-bound. Scaling one without the other
just moves the queue.

A per-Deployment autoscaler cannot express this. Each ScaledObject sees its own
trigger and nothing else.

### What we do about it

A **variant** is the unit: one ScaledObject and the workload it scales. Variants
whose triggers name the same model are variants of one model, and the group is
solved together, each role carrying its own target and its own bottleneck. The
cost-aware optimizer chooses among accelerator variants rather than assuming one
GPU type.

### The evidence

Per-role targets are reported and actuated independently — in the P/D run,
decode scaled 1 → 2 → 3 while **prefill was never ordered at all**, because
prompts waiting at the scheduler are no longer charged to it as resident KV.

→ [Scale a P/D-disaggregated model](well-lit-paths/pd-disaggregation/)
· [Accelerator variants](well-lit-paths/accelerator-variants/)

---

## What else it does

- **Scale-to-zero and wake.** Idle models release their GPUs and are woken on
  demand, through KEDA's push path rather than a poll.
  → [scale-to-zero](well-lit-paths/scale-to-zero/)
- **Quota-bounded scaling.** Decisions are constrained by a GPU budget, and by
  Kueue quota where that is the boundary.
  → [bound by GPUs](well-lit-paths/bound-by-gpus/) ·
  [Kueue-bounded quotas](well-lit-paths/kueue-bounded-quotas/)
- **Workload classes.** Interactive and batch tiers share a fleet under named
  policies. → [workload classes](well-lit-paths/workload-classes/)
- **It decides; Kubernetes actuates.** We implement the KEDA external-scaler gRPC
  contract and KEDA owns the HPA. The decision is the hard part and it is ours;
  the actuation is the part Kubernetes already does well.

---

## What it does not do

Stated plainly, because a proposal that claims everything is worth less than one
that says where the edges are.

**Prediction.** A replica is ready minutes after the decision, so the right
question is where load will be *then*. We do not forecast — the model is
closed-form, trading anticipation for explainability. Against a 152 s
construction floor on a large model, that is a real gap.

**SLA-target-driven scaling.** Scaling on *"will this batch tier miss its
deadline"* rather than *"is utilization above threshold"* is not built. Named
policy tiers are a coarse approximation. Nobody upstream has solved this either.

**Maintenance pre-scaling and failure replacement.** Not built.

**Snapshots**, which would have removed the 152 s that no weight transfer can
touch. Built and does not work: clean on a single process, and it hangs on a
real multi-rank engine inside NVIDIA's checkpoint path. Fifteen experiments
narrowed it; the write-up is in the warm-pool weight-transfer proposal.

**Sizing a shape the controller has never seen saturated.** It is sized from
occupancy until the first saturated reading arrives — the 4.2 s cold-pass window
above is exactly that cost.

---

## Scope boundaries

Ours: horizontal replica decisions across models, variants and P/D roles; warm
capacity; scale-to-zero; GPU-budget constraints; the KEDA external-scaler
transport.

Not ours: node provisioning (cluster autoscaler), quota between models (Kueue),
tenant quota (the gateway), request routing and scheduling (the gateway and
EPP), and the scale operation itself (KEDA and the HPA).

## Status

Running on CoreWeave H200s and on OpenShift. The scaling path, warm pool,
scale-to-zero and quota limiting are built and cluster-verified; replica
reallocation across priorities is designed and not built; P/D role switching is
experimental. Apache 2.0, and the Go module path is unchanged, so imports do not
move.
