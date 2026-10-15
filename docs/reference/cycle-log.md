# llm-scaling-manager Cycle Log

Each reconcile cycle, the saturation engine writes two INFO lines per model.
They show what the analyzer and optimizer computed, with no debug logging needed.

The log uses zap's development **console** format, not JSON: timestamp, level,
caller, message, then the fields as one JSON object. A line looks like this
(wrapped here; it is one line in the log):

```text
2026-10-14T09:12:03.512Z  INFO  steadystate/engine_v2.go:1246  analyzer-result
  {"modelID": "my-model", "namespace": "default", "analyzer": "saturation", ...}
```

Top-level fields are written `"key": value`, with a space after the colon.
Nested values (`variants`, `decisions`) are compact JSON with no spaces. The
greps below rely on both.

---

## Log lines

### `analyzer-result`

One line per analyzer that ran for a model, written right after the threshold
post-step. The values are what the optimizer receives.

```text
2026-10-14T09:12:03.512Z  INFO  analyzer-result  {"modelID": "my-model", "namespace": "default",
  "analyzer": "saturation", "supply": 2362173, "demand": 2240000, "util": 0.95, "rc": 273121,
  "sc": 0, "scaleUpThreshold": 0.85, "scaleDownBoundary": 0.7,
  "variants": [{"name":"primary","prc":1152000,"role":"both","reason":"P3-k2","observed":1},
               {"name":"v2","prc":403391,"role":"both","reason":"P1-obs","observed":3}]}
```

A P/D model also carries `roleRC` and `roleSC`: the per-role required and spare
capacity the optimizer branches on.

| Field | Description |
|---|---|
| `modelID` | the scaling manager model ID (unique within a namespace) |
| `namespace` | Kubernetes namespace |
| `analyzer` | Analyzer name, e.g. `"saturation"`, `"throughput"` |
| `supply` | Total token supply across ready replicas (readyCount × perReplicaCapacity) |
| `demand` | Total token demand. For saturation V2 it is the sum of three terms: resident KV tokens; a role-aware projection of requests waiting in each replica's engine queue; and a model-level, prefix-cache-discounted projection of requests still queued in llm-d flow control (`SchedulerQueue`, not attributed to any variant). See [scaling-policy-config.md](scaling-policy.md) |
| `util` | `demand / supply`; > 1.0 means the model is over capacity |
| `rc` | Required capacity (post-threshold): > 0 triggers scale-up |
| `sc` | Spare capacity (post-threshold): > 0 permits scale-down |
| `scaleUpThreshold` | Scale-up threshold for this analyzer (from config) |
| `scaleDownBoundary` | Scale-down boundary for this analyzer (from config) |
| `variants[].name` | Variant name |
| `variants[].prc` | Per-replica capacity in analyzer units (tokens for saturation) |
| `variants[].role` | P/D role: `prefill`, `decode`, or `both`. It reads `both` when the scale target has no `llm-d.ai/role` label, and when the analyzer does not set a role. Saturation V2 charges waiting requests by this role, so you need it to read `demand` |
| `variants[].reason` | How the variant's capacity was computed (see below) |
| `variants[].observed` | Replicas that reported metrics this cycle, before any cap; compare with the target's ready replicas |
| `roleRC`, `roleSC` | Per-role `rc` and `sc`; P/D models only. A role over its own threshold keeps the model scaling up even when the model-level `rc` is 0 |

An analyzer that computes no per-variant capacity writes an empty `variants`
array. With several analyzers enabled there are several `analyzer-result` lines,
each with the same `modelID`/`namespace` and its own `analyzer`.

### `scaling-decision`

One line per model, after the optimizer chose each variant's replica target.

```text
2026-10-14T09:12:03.540Z  INFO  scaling-decision  {"modelID": "my-model", "namespace": "default",
  "decisions": [{"name":"primary","curr":1,"tgt":2,"action":"scale-up"},
                {"name":"v2","curr":3,"tgt":3,"action":"no-change","atMax":true}]}
```

| Field | Description |
|---|---|
| `modelID` | the scaling manager model ID |
| `namespace` | Kubernetes namespace |
| `decisions[].name` | Variant name |
| `decisions[].curr` | Replica count at this cycle |
| `decisions[].tgt` | Target replica count chosen by the optimizer |
| `decisions[].action` | `scale-up`, `scale-down`, or `no-change` |
| `decisions[].atMax` | `true` when the target is the variant's `maxReplicaCount`; absent otherwise. Same as `wva_variant_at_max_replicas` |

---

## Reason values (`reason` field)

Each analyzer sets `reason` to say how it computed a variant's per-replica
capacity. It is free text. The saturation V2 analyzer uses:

| Reason | Meaning |
|---|---|
| `P0-store` | from the **capacity store** (no live replicas) |
| `P1-obs` | k2 from **observed** tokens-in-use (queue saturated), blended into the same rolling average `P2-hist` reads, so one noisy cycle gets 1/N weight |
| `P2-hist` | k2 from the **historical** rolling average. The window is kept for 7 days from when it was last *read*, not written, so a bucket priced from it every cycle keeps its value however long the fleet goes without saturating. While kept it is used: with `min(k1, k2)`, it is the most conservative number available. After 7 days unread the bucket is forgotten and the next decision falls to `P3-k2` or `P4-k1` |
| `P2-borrow` | No k2 history of its own for this traffic shape, so k2 from a **sibling variant**: same namespace, model, accelerator, GPU count, role, output-length bucket and queue threshold, and capacity-compatible engine parameters (same `--max-num-seqs`, batch-token budget, KV settings and quantization). Used only when **lower** than the variant's own capacity without it (`P3-k2` or `P4-k1`, capped at k1), so it never orders fewer replicas. A k2 that is too low never corrects itself (light load never saturates, and only saturation measures k2), so the borrow holds 30 minutes, then relaxes linearly to the variant's own figure over 6 hours. The clock is per output-length bucket, and counts only cycles where a Ready replica of the variant's own serves that bucket: it stops while the variant is parked, and a warm-pool bridge or a Pod that is not Ready reads the borrow without starting or moving the clock. A donor that leaves and returns does not restart it. Once relaxed, the borrow stays spent: it starts again only when the donor's figure has moved by more than 20%, or the variant's engine configuration has changed. The clock lasts as long as the variant; a controller restart keeps it only through the learned-state snapshot (`LEARNED_STATE_SNAPSHOT`). The `k2-decision` line names the `donor`, its `donorK2`, this variant's `ownCapacity` (its own k2 or k1, whichever is lower), and how far it has `relaxed`. The variant's first own saturated reading replaces it |
| `P3-k2` | k2 **derived** from deployment parameters (vLLM model args). Never for a **prefill** variant: the formula needs a real output length, and prefill's avgOutput (~0-1, it hands off before generating) reduces it to the batch-token budget |
| `P4-k1` | k2 unavailable; **fell back** to k1 (memory-bound capacity). Normal for prefill, not degraded; see `P3-k2` |

These appear in the `k2-decision` line's `priority` field but never as a
variant's `reason`, because no capacity comes from them:

| value | meaning |
| --- | --- |
| `P1-obs-invalid` | an observation above the KV cache's **physical** ceiling was discarded as a scrape artifact. The bound is the ceiling, not k1: k1 is the ceiling times `kvCacheThreshold` (0.80 by default), so occupancy between the two is kept. The analyzer falls through to the next priority |
| `P1-obs-downstream` | a **prefill** replica's saturated queue was not recorded because a **decode** replica was full and queued (resident KV at its k1, queue at the threshold) in the same cycle or the last minute (the collector's row window). Prefill finishes a request only when decode pulls its KV, so with decode saturated, prefill's KV, queue and completion rate measure decode, not prefill. Neither k2 nor the saturated throughput (`throughput-demand-floor`) is recorded; the analyzer falls through, and prefill's demand is held for the cycle, neither ordering nor releasing (`prefill-demand-held`). A decode queue of only KV transfers in flight (KV under k1) does not count. Both roles saturate at once only when decode is short at prefill's completion rate; prefill's reading then waits for decode to recover |
| `no-data` | no ready replicas, no stored record, no compatible variant: capacity is 0 this cycle (normal for a new variant) |
| `error` | K2 priority not in the known set: an unlabelled code path. Should not occur |

`P1-obs` to `P4-k1` describe the variant's representative replica: the one whose
effective capacity is the lower median of all ready replicas (the one that set
`prc`). `P1-obs` means live data and a high-confidence estimate; `P4-k1` means
no replica had a compute-bound signal, and the estimate is conservative.

The throughput analyzer uses:

| Reason | Meaning |
|---|---|
| `T1-ols` | capacity from Tier-1 OLS fit (observation window ready) |
| `T2-pinned` | capacity from Tier-2 constrained OLS with a prior fitted B |
| `T2-default` | capacity from Tier-2 constrained OLS with the default baseline B (cold start) |
| `T2-failed` | both tiers failed (all replicas idle or no usable ITL signal); variant skipped this cycle |

Other analyzers may set their own values or leave `reason` empty.

---

## Grep patterns

```bash
LOGS="kubectl logs -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler --tail=-1"

# All analyzer results for a specific model
$LOGS | grep analyzer-result | grep '"modelID": "my-model"'

# Saturation analyzer only
$LOGS | grep analyzer-result | grep '"analyzer": "saturation"'

# Scale-up decisions only
$LOGS | grep scaling-decision | grep '"action":"scale-up"'

# Full cycle for one model (both lines)
$LOGS | grep -E 'analyzer-result|scaling-decision' | grep '"modelID": "my-model"'
```

`modelID` is unique only within a namespace. Add `| grep '"namespace": "<ns>"'`
when two namespaces serve the same model.

---

## Ordering and timing

In one reconcile cycle, for one model:

1. One `analyzer-result` line per enabled analyzer (saturation first, then the
   others in registration order).
2. One `scaling-decision` line, after the optimizer has processed every model in
   the cycle.

The two are not adjacent in the log: other models' lines can fall between them.
Filter by `modelID` and `namespace` to match them up.

---

## Enabling the log

Both lines are INFO and appear in the default controller log. Nothing to enable.
