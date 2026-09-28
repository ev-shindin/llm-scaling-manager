# llm-scaling-manager: project proposal

## Summary

llm-scaling-manager is an autoscaler for llm-d inference. It works out how many
replicas each model and each variant needs, decides for all of them together
against a single GPU budget, and does it from a capacity model instead of a
threshold on a raw metric. Around that decision it carries the things the
decision needs in order to be useful: a shared warm pool that covers the minutes
a new replica spends loading, scale-to-zero, and separate handling for prefill
and decode.

We built it because the signals a general-purpose autoscaler can act on do not
tell you what you need to know. Request rate, queue depth and cache occupancy
all sound like measures of load, and none of them is. We have a benchmark where
all three said the fleet was fine and the right answer was to triple it. This
document sets out that problem and three others, what we do about each, and the
measurements behind the claims.

The project started as a fork of llm-d's Workload Variant Autoscaler
(`llm-d/llm-d-workload-variant-autoscaler`, still the module path in this tree's
`go.mod`) and has moved a long way from it since. There is a shorter version of
this argument written for people outside the repository:
[Sub-second scale-ups on llm-d](blog/sub-second-scale-ups-on-llm-d.md).

## What is already measured

Everything claimed below comes from a run on real hardware, and the runs are
recorded in this repository. The detail sits with each part of the proposal; the
table is here so the evidence is easy to find and easy to check.

| Claim | Measured | Where |
| --- | --- | --- |
| The usual signals mislead | At a constant 6 req/s with the request shape changed halfway: rate flat, no queue, KV at 5–15 %. All of it pointed at one replica when three were needed | [P/D path](well-lit-paths/pd-disaggregation/) |
| A shape change is caught without a queue | Third replica ordered at +1344 s, in the same cycle the new shape's completion rate first came on record. No queue formed at any point; p95 TTFT settled at 0.04–0.07 s | [P/D path](well-lit-paths/pd-disaggregation/) |
| Getting that wrong has a price | A cold controller sized the same window from occupancy, for want of a completion rate, and paid 4.2 s p95 | [P/D path](well-lit-paths/pd-disaggregation/) |
| The decision comes before the queue | Second replica ordered at +53 s, from the load, before the first tipped into preemption | [P/D path](well-lit-paths/pd-disaggregation/) |
| Roles scale on their own bottleneck | Decode went 1 → 2 → 3; prefill was never ordered | [P/D path](well-lit-paths/pd-disaggregation/) |
| The warm pool covers the rise | p95 TTFT per rise falls from 5.1–8.8 s to 0.11–0.83 s | [measured.md](well-lit-paths/warm-pool-bridge/measured.md) |
| It is cheaper than the floor it replaces | 14 138 GPU-seconds against 16 080, a 12 % saving at the same latency | [measured.md](well-lit-paths/warm-pool-bridge/measured.md) |
| And here is what it costs | 17 % more than holding nothing at all, which came in at 12 129 | [measured.md](well-lit-paths/warm-pool-bridge/measured.md) |
| A warm Pod switches models quickly | 437 ms, against roughly 41 s for a cold start, on a Pod serving real gateway traffic | [fast model loading](proposals/fast-model-loading.md) |
| Cold start is mostly not the weights | An 8B server takes ~41 s. GLM-5.2-FP8 takes 192 s, of which the weights are 40 s, and 463 s if the JIT cache is cold | [weight transfer](proposals/warm-pool-weight-transfer.md) |
| The figures repeat | The P/D pair was run twice, a day apart, and landed within a tenth of every number | [P/D path](well-lit-paths/pd-disaggregation/) |

The limits are worth stating alongside them. The warm-pool benchmark is four
scale-up events per arm, one run of each, with no confidence intervals. The
direction held across all four rises, but the margins come from a single run.

## Motivation

KEDA and the HPA do their job well: they turn a signal into a replica count,
quickly and safely. The trouble is that LLM inference breaks several of the
assumptions underneath that job.

### The available signals do not mean anything stable

Request rate, queue length and request latency are what KEDA gives you to
trigger on. None of them has a fixed relationship to how loaded a GPU is,
because the cost of a request depends on its shape, meaning how many tokens go
in and how many come out, and on how the server is configured. Two requests to
the same model can differ by a factor of a thousand in compute. A threshold
tuned for one traffic pattern is wrong for the next one, and nothing in the
signal tells you it has changed.

We can show this rather than assert it. We ran a trace at a constant 6 req/s
where the only thing that changed was the request shape, from 6000 tokens in and
1000 out to 1000 in and 4000 out. A single decode replica sustains about
5.4 req/s at the first shape and about 2.5 at the second, so the fleet should
have gone from two replicas to three. Here is what the usual signals were saying
while that happened:

| signal | what it showed | what it implies |
| --- | --- | --- |
| request rate | constant at 6 req/s throughout | nothing to do |
| gateway queue depth | flat, no queue ever formed | nothing to do |
| KV-cache occupancy | 5–15 % through the steady first phase | scale down to one |

All three are wrong, and the third is worse than useless: acting on it would
have cut the fleet to a single replica just before the work per request went up
fourfold.

### Reacting after saturation is already too late

A reactive autoscaler is fine when a pod starts in a second. An LLM replica is
not ready when it is scheduled. It is ready when the model has loaded and the
kernels have compiled, and that takes a while:

| | |
| --- | ---: |
| 8B model server, from not running to first request served | ~41 s |
| GLM-5.2-FP8 (744B MoE), warm node, weights from local NVMe | 192 s |
| of which the weights account for | 40 s |
| the same start on a node with a cold JIT cache | 463 s |

The third row is the interesting one. Weights are only about a fifth of a GLM
start, so putting them on faster storage does not fix this. The rest of the time
goes on process spawn, imports, memory profiling, kernel warmup and CUDA-graph
capture.

### The unit of waste is a GPU, and the models are competing for it

An accelerator costs $2–4 an hour, so being wrong is expensive in both
directions: an unnecessary replica burns real money, and a missing one breaks an
SLO. On top of that, a general-purpose autoscaler decides one Deployment at a
time. It has no way of knowing that scaling one model up might mean scaling
another down, or that both are drawing from the same finite pool of GPUs.

### One model is several workloads, and they scale differently

Production deployments are not one Deployment per model. The same model usually
runs as several variants, differing in accelerator, tensor-parallel width or
quantization. And where prefill and decode are disaggregated, the two halves are
separate workloads with opposite bottlenecks: prefill is compute-bound, decode
is memory-bandwidth-bound. Scaling one without the other just moves the queue
somewhere else.

### Goals

1. Scale on something that stays meaningful when the traffic shape changes,
   without per-model or per-workload tuning.
2. Decide before a queue forms, and cover whatever load time is left.
3. Decide for the whole fleet against one GPU budget rather than per Deployment.
4. Treat variants and P/D roles as scaling units in their own right.
5. Publish what each mechanism costs, including the cases where it loses.

### Non-Goals

We do not forecast. The model is closed-form, which trades anticipation for a
decision an operator can read and disagree with.

We also leave alone the things other components already own: node provisioning
belongs to the cluster autoscaler, quota between models to Kueue, tenant quota
to the gateway, routing and scheduling to the gateway and EPP. The scale
operation itself belongs to KEDA and the HPA. We make the decision; Kubernetes
carries it out.

## Proposal

### A universal threshold, on a quantity that is already normalised

We scale on fleet utilization measured against a fixed threshold, scaling up
above 0.85 and releasing below 0.70. The utilization comes from a capacity model
rather than from a gauge. Each role's demand is floored at what the load
actually requires in throughput terms, which is the arrival rate divided by the
completion rate one replica sustained the last time it was seen saturated.

That ratio depends on the request shape by construction. When the shape changes,
the price changes with it, and nobody has to re-tune a threshold. There is
nothing per-model or per-traffic-pattern to configure.

In the run described above, the third replica was ordered at +1344 s, in the
same cycle that the new shape's completion rate first came on record, and no
queue formed at any point during the second phase. Once each phase settled, p95
TTFT sat at 0.04–0.07 s. The cold controller, which had no completion rate on
record yet and fell back to sizing by occupancy, paid 4.2 s p95 over the same
window. That number is what the ordinary signal is worth, measured. The whole
pair was run twice, a day apart, and landed within a tenth of every figure.
See [Scale a P/D-disaggregated model](well-lit-paths/pd-disaggregation/).

### Order earlier, then bridge whatever is left

Because demand is priced from throughput rather than read off a queue, the order
goes in before the queue exists. In the same run, the second replica was ordered
at +53 s, from the load, and before the first replica tipped into preemption.

That still leaves the load time itself, which is what the warm pool is for. A
pool Pod holds an accelerator with models already resident and lends it to a
model that is scaling up, so that model serves while its own replica starts. We
benchmarked this with two models bursting out of phase:

| arm | p95 TTFT per rise | GPU-seconds |
| --- | --- | ---: |
| autoscaling alone | 5.1 – 8.8 s | 12 129 |
| plus a one-Pod warm pool | 0.11 – 0.83 s | 14 138 (+17 %) |
| a floor of 2 replicas per model | 0.09 – 0.13 s | 16 080 (+33 %) |

On a pool Pod serving real gateway traffic, a model switch took 437 ms against
roughly 41 s for a cold start. Full method and results in
[what a warm pool buys](well-lit-paths/warm-pool-bridge/measured.md).

### One decision, for the whole fleet, inside one budget

Every model and every variant is solved together each cycle, against a single
GPU budget, and a declared limiter keeps the result inside whatever the
namespace is entitled to.

The warm pool is the same idea expressed in hardware. One held accelerator
insures several models, so the more models share it the better it looks, which
is the opposite of how a per-model reserve behaves.

On cost, the pool comes in 12 % below the floor it replaces, at the same
latency. It is worth being blunt about the other comparison too: autoscaling on
its own is the cheapest arm of the three. If you hold nothing today and can live
with rises of several seconds, keep holding nothing. The pool is for people who
would otherwise be holding a floor. See
[GPU capacity accounting](concepts/gpu-capacity-accounting.md) for what the
budget means and three ways a naive reading overstates free capacity.

### The variant is the unit of scaling

A variant is one ScaledObject and the workload it scales. Variants whose
triggers name the same model are solved as a group, with each role carrying its
own target and its own bottleneck, and the cost-aware optimizer picks among
accelerator variants instead of assuming a single GPU type.

In the P/D run this showed up clearly: decode scaled 1 → 2 → 3 while prefill was
never ordered at all, because prompts waiting at the scheduler are no longer
charged to it as resident KV. See
[accelerator variants](well-lit-paths/accelerator-variants/).

### Also shipped

Scale-to-zero and wake, over KEDA's push path rather than a poll
([scale-to-zero](well-lit-paths/scale-to-zero/)). Quota-bounded scaling, against
a GPU budget and against Kueue quota where that is the real boundary
([bound by GPUs](well-lit-paths/bound-by-gpus/),
[Kueue-bounded quotas](well-lit-paths/kueue-bounded-quotas/)). And workload
classes, so interactive and batch tiers can share a fleet under named policies
([workload classes](well-lit-paths/workload-classes/)).

## Design details

Rather than summarise the decision path here, it is documented properly
elsewhere. [The steady-state engine](concepts/steady-state-engine.md) covers
what gets measured and how a measurement becomes a replica count.
[Modeling and optimization](concepts/modeling-and-optimization.md) covers the
queueing model and the optimization itself.
[GPU capacity accounting](concepts/gpu-capacity-accounting.md) covers the budget.

Actuation goes through the KEDA external-scaler gRPC contract, with KEDA owning
the HPA and writing the scale subresource. Workloads are discovered from the
KEDA calls themselves, so there is no watch, no listing and no opt-in
annotation: a newly registered workload is managed from its first call. That
matters for self-service platforms, where a tenant or a model can appear without
anyone telling the autoscaler.

## Alternatives considered

We tried or measured each of these rather than ruling them out on paper.

**Threshold autoscaling on the signals KEDA already has.** The shape-swap run
settles this one. At a constant rate, with no queue anywhere, KV occupancy
pointed at one replica when the answer was three.

**Holding a floor of replicas.** This works, and on latency it is the closest
thing to the warm pool, within tens of milliseconds. It loses on cost: 33 % more
GPU-seconds than autoscaling alone, against the pool's 17 %, which is where the
12 % saving comes from. A floor is still the better choice if one model is
always bursting and the fleet never comes back down, because then the pool's
accelerator is held without ever being the thing that saves you.

**Faster storage, a bigger page cache, or peer-to-peer weight transfer.** None
of these fixes start latency, and the measurement says why: the weights are 40 s
of a 192 s GLM start, so even a perfect transfer only gets you to about 152 s. A
weights PVC measured 430 MB/s, which avoids re-downloading but does nothing for
a cold start. Peer transfer is still worth having, and we have it working
byte-identical at around 9× the speed of reloading from storage, but it does not
solve this problem.

**Process snapshots.** These would remove the ~152 s that no transfer can touch,
which is why we built them. They do not work yet. On a single process everything
is clean: the GPU is released to 0 MiB, the process dumps, restores, and returns
an identical checksum. On a real multi-rank engine it hangs, with the driver's
own thread spinning after the data movement has finished. Fifteen experiments
narrowed that down to NVIDIA's checkpoint path rather than anything the
inference server can release.

**A warm launcher that holds no GPU.** Appealing, because the accelerator would
be free while the launcher waits. It does not work: an unbound sleeper often
cannot wake at all. Holding the accelerator is what makes the wake possible.

## What is still open

**Prediction.** A replica becomes ready minutes after the decision is made, so
the question that matters is where load will be then, not where it is now. We do
not answer it. Against a 152 s floor on a large model, that is a real gap and
not only a matter of taste.

**SLA-target-driven scaling.** Scaling on whether a batch tier is going to miss
its deadline, rather than on whether utilization is above a threshold, is not
built. Named policy tiers approximate it coarsely. As far as we know nobody
upstream has solved this either.

**Maintenance pre-scaling and failure replacement.** Not built.

**A shape the controller has never seen saturated** gets sized from occupancy
until the first saturated reading arrives. The 4.2 s cold-pass window above is
exactly that cost, and it is the honest weak point of the approach.

## Status

Running on CoreWeave H200s and on OpenShift. The scaling path, the warm pool,
scale-to-zero and quota limiting are built and verified on a cluster. Replica
reallocation across priorities is designed but not built, and P/D role switching
is still experimental. Apache 2.0, and the Go module path has not changed, so
imports do not move.
