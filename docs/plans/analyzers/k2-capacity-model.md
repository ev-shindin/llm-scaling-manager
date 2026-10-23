# Design call: what `k2` measures, and why it has to persist

**Status:** Design call — consolidates `issue-v2-k2-rate-anchored.md`,
`k2-rate-solution-explained.md` and `issue-v2-k2-observed-pins-utilization-at-one.md`
into the decisions that need making. No code change proposed here.

## The one-line problem

`k2` is the compute bound in `effectiveCapacity = min(k1, k2)`. It is recorded as
`tokensInUse` at a moment the replica was seen queueing — a **KV stock** — while the
constraint it stands for is a **rate**. On prefill-heavy traffic those are unrelated:
the engine exhausts prompt-token throughput and queues while KV occupancy is still
low, so the analyzer reports abundant headroom on a replica that is already
dropping requests.

Measured: a sustained 1000-input/250-output run queueing and cycling replicas at
**16.2% average KV utilization**. Six threshold/window/policy legs failed to break
the cycling. It is not a tuning problem.

## Why persistence is not optional

This is the constraint that governs the whole design, and it is easy to get
backwards.

For a prefill-heavy workload the true compute bound is far below the memory bound.
`k2` is the only thing that carries that fact. If a learned `k2` is discarded —
aged out, floored upward, or otherwise relaxed toward `k1` — capacity reverts to the
memory bound, which for this workload is a large over-estimate. Then:

> over-stated capacity → utilization reads low → scale-down → requests queue →
> TTFT degrades → scale-up → **but P95/P99 TTFT are already broken**

Tail latency does not recover on the way back up. The replicas return; the damage is
already in the percentile window. Both faces of this are on record: the shed-to-one
at 100/1000 mid-experiment under unchanged load, and the cycling at 1000/250.

**The errors are asymmetric.** Under-estimating `k2` holds more replicas than
strictly needed — it costs money, and nothing is at risk. Over-estimating it breaks
the SLO the autoscaler exists to protect. Every default should therefore be biased
low: a running **minimum**, relaxed upward only slowly, never a floor that pushes
`k2` toward `k1`.

### Two recommendations this retracts

Both were proposed earlier in this repo's issue notes and are wrong for
prefill-heavy workloads. Recording them so they are not re-proposed:

- **"Floor `k2` relative to `k1`"** — raises `k2` toward the memory bound, which is
  precisely the over-estimate that sheds replicas. It would fix a degenerate sample
  by causing the failure the estimator exists to prevent.
- **"Age the history out"** — same direction. Ageing is only safe as the *slow
  upward relaxation* of a running minimum, never as eviction back to `k1`.

## The split the fix needs

```text
detector:    rates decide WHEN a replica is at its limit
measurement: tokens record WHAT that limit is
```

A replica is at its limit when it has a backlog at least `QueueLengthThreshold`
deep, **or** when its arrival rate has reached the service rate measured while it
*was* backlogged. At that moment its resident token count measures the limit —
including at 16% occupancy, which is the entire point. The measurement is stored per
workload bucket (model, accelerator, role, GPU count, request shape) as a running
minimum, and every replica of the bucket reads the same value.

### Constraints any implementation must respect

1. **Identical across replicas of a variant.** `aggregateByVariant` takes the median
   of per-replica capacities. A value that varies with each replica's own load is
   not commensurable across siblings — an idle replica's value blends with a
   backlogged one's and can lift variant capacity enough to turn a scale-up into a
   scale-down. A bucket ceiling makes the median a no-op.
2. **Must not move with the current cycle's load.** A capacity recomputed from this
   cycle's arrival rate changes every cycle — an oscillation waiting to happen.
3. **λ and μ are not on the same time base.** A completion happens one residence
   time after the arrival that caused it, so instantaneous λ against a
   completion-derived μ reads as saturation on a replica that is coping during a
   ramp. λ needs smoothing over `AvgTTFT + AvgOutputTokens × AvgITL`.
4. **Sample admission must be strict — because persistence makes bad samples
   permanent.** This constraint is new, and it comes from the degenerate case in
   `issue-v2-k2-observed-pins-utilization-at-one.md`: a replica whose capacity was
   frozen at **2 tokens**, after which utilization never fell below
   `scaleDownBoundary` and the model sat pinned at `maxReplicas`.

   Under a running-**minimum** scheme that failure gets *worse*, not better: a
   spuriously low sample can never be displaced by a higher observation, only by
   age. Queue-depth-alone is a weak detector — a replica that has barely started can
   have a deep queue and almost no residency, and it will happily volunteer a
   near-zero ceiling that then sticks. The rate-based detector is what rejects it:
   a just-started replica has not reached its measured service rate.

   So "persist the minimum" and "admit almost anything as a sample" cannot both
   hold. Strengthening the detector is the price of persistence.

## The safety net (independent of `k2`)

Scale-down asks a question the numbers cannot answer: *would N−1 replicas still
cope?* Demand is measured at the current replica count and does not survive the
change. Arrivals are the one quantity that does not move when replicas do:

```text
(replicas × μ)  must stay above  (arrivals ÷ scaleDownBoundary)
```

If removing a replica would break that, it is not removed. This depends on two
measured numbers and no cache, residence or ceiling — so it still holds when the
capacity estimate is wrong. Given the error asymmetry above, this net is arguably
more valuable than the ceiling itself and could ship first.

Two deliberate exceptions: GPU rebalancing ignores it (reclaiming a GPU for a
higher-priority model is exactly the argument the rule should not win), and it is
skipped entirely when any variant in a group has no measured μ, rather than acting
on half the picture.

## Decisions that need a call

1. **Ship order.** The safety net is small, independent, and directly prevents the
   TTFT-breaking shed. The rate-anchored ceiling is the larger correctness fix. Net
   first, ceiling second is the low-risk order — but the net alone leaves the
   estimator still reporting phantom headroom.
2. **Detector strictness.** Keep queue-depth as an admission path, or require the
   rate condition? Strict costs calibration opportunities on fleets that rarely
   queue; loose re-admits the degenerate-sample failure, permanently, under a
   running minimum.
   Resolved for the queue-depth path: a reading whose queue is larger than the
   replica's whole resident batch is admitted only when its service rate has
   reached the rate on record, or when the queue stood the previous cycle and
   the residency stopped rising (`k2_settle.go`, `P1-obs-unsettled`). Measured
   cause: a load step's first reading, 31 713 tokens under a queue of 160,
   pulled a 270 539 window to 151 126.
3. **Relaxation policy.** How does a bucket minimum ever move up when a workload
   genuinely gets cheaper? Slow age-based relaxation is the proposal; the rate and
   its interaction with the SLO asymmetry need choosing deliberately.
4. **Bucket key.** Was `model|accelerator|gpuCount|outputBucket` — no
   namespace, no variant — so one deployment could set another's ceiling.
   Resolved: the k2 and throughput keys now lead with `namespace|variant`, and
   starting one variant from another's figure is a read-side decision; see
   [learned-state sharing by similarity](../../proposals/learned-state-sharing-by-similarity.md).
5. **P/D disaggregation.** Not addressed by the direction above: μ from the
   generation-tokens histogram is decode-centric; a prefill pool needs
   `rate(vllm:request_prompt_tokens_count[1m])` as its μ.

## Non-goals / already settled

- **No new metrics required.** λ (`inference_extension_scheduler_attempts_total`),
  μ (`vllm:request_generation_tokens_count`), occupancy, queue depth and KV capacity
  are all collected today. Two are registered only when the throughput analyzer is
  enabled, which a fix must not depend on.
- **The ITL model does not cover this.** `ITL(k) = A·k + B` stays flat on
  prefill-heavy traffic while TTFT and the queue explode. It remains the better
  model for decode-bound workloads.
- **A fleet that never queues never calibrates** and falls back to the memory bound.
  Acceptable — nothing is at risk there — but it makes emitting the active
  estimator's source label mandatory rather than optional.

## Regression control

The 300/300 steady case must still hold flat at one replica. A more conservative
capacity estimate is exactly what could turn "correctly holds" into spurious
scale-up, so this is the control, not an afterthought.

## Related

- `issue-v2-k2-rate-anchored.md` — the units analysis and evidence
- `k2-rate-solution-explained.md` — prototype shape, per-path calculation, blockers
- `issue-v2-k2-observed-pins-utilization-at-one.md` — the degenerate-sample failure
- `docs/plans/analyzers/kvcachethreshold-retirement.md` — retiring `k1`'s multiplier
  makes `k2` bind more often, raising the stakes on all of the above
