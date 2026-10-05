# Retiring `kvCacheThreshold`

**Status:** Proposed — not scheduled. The field stays for now; the correctness
defects around it were fixed separately (see *Already fixed* below), so this is a
config-simplification proposal, not a bug fix.

## Summary

`kvCacheThreshold` and `scaleUpThreshold` are two knobs that do the same thing to
the scaling decision, and they multiply. Retiring `kvCacheThreshold` — treating a
replica's capacity as its physical KV budget — would leave one headroom control
instead of two compounding ones, without losing any expressible behaviour.

## What it does today

V2 treats it as a ceiling on usable KV:

```go
// internal/engines/analyzers/saturation/analyzer.go
k1 := int64(float64(rm.TotalKvCapacityTokens) * config.KvCacheThreshold)
```

Capacity is the physical KV budget scaled by the threshold, demand is measured
tokens, so `utilization == 1.0` exactly when KV occupancy reaches the ceiling. At
the default `0.80`, "100% utilized" means 80% of KV resident — the headroom notion
EPP saturation uses. That intent is sound and worth preserving; the question is
only *which knob expresses it*.

## Why retire it

**It is redundant with `scaleUpThreshold`.** Replica sizing is

```go
// internal/engines/allocation/greedy_score_optimizer.go
fairShareCap := int(math.Ceil(target / vc.PerReplicaCapacity))   // target = demand / scaleUpThreshold
```

and `PerReplicaCapacity ∝ kvCacheThreshold`. Halving either knob doubles the
resulting replica count: the same control, applied twice.

**They compound, probably unintentionally.** The default pair `kvCacheThreshold:
0.80` + `scaleUpThreshold: 0.85` scales out at `0.80 × 0.85 ≈ 0.68` of physical
KV. Neither number says 0.68, and an operator tuning one will not expect the other
to scale it.

**One of the two cannot be removed.** `scaleUpThreshold` and `scaleDownBoundary`
form the hysteresis band; without them there is no scale-down separation. So
`kvCacheThreshold` is the removable one.

**It reads better afterwards.** With capacity as the physical KV budget,
`utilization` means actual KV occupancy and `wva_analyzer_target` means actual
tokens per replica. "Saturated at 80% KV" becomes `scaleUpThreshold: 0.80` — the
EPP number, stated once.

## Migration

`kvCacheThreshold: K` with `scaleUpThreshold: S` is equivalent to
`scaleUpThreshold: K × S` once the multiplier is gone. Defaults `0.80 × 0.85`
become `scaleUpThreshold: 0.68`; `scaleDownBoundary` scales the same way
(`0.80 × 0.70 = 0.56`). Whether to ship those computed equivalents or re-anchor on
round numbers (`0.80` / `0.70`) is a product call — the former preserves behaviour
exactly, the latter is a deliberate (mild) retune.

Because the field is currently defaulted unconditionally in `ApplyDefaults`,
removal wants a deprecation window: accept and ignore it with a warning for one
release, then delete.

## Call sites to remove

- `internal/engines/analyzers/saturation/analyzer.go` — `k1` (~:155), the
  fallback (~:230/:238), `estimateStoredCapacity` (~:400, :489)
- `internal/config/saturation_scaling.go` — field, `DefaultKvCacheThreshold`,
  `ApplyDefaults`, `Merge`, and both `Validate` rules (range, and
  `>= kvSpareTrigger`)
- `config/` sample ConfigMaps and `docs/` references
- `test/e2e/saturation_analyzer_path_test.go` — the arcs are calibrated on it

**Behavioural consequence to measure first:** `k1` participates in
`min(k1, k2)` against the compute-bound estimate. Dropping the multiplier raises
`k1`, so `k2` will dominate more often. That shifts which bound decides capacity
and is the one part of this that is not a pure refactor.

## Also worth removing with it

`ReplicaCapacity.IsSaturated` (`types.go:47`, set at `analyzer.go:177,254`) is
computed and stored but never read in production — only asserted in tests. It is
the last remnant of V1's boolean saturation test and keeps that mental model
alive. It should go whether or not `kvCacheThreshold` does.

## Already fixed (do not re-litigate)

These were real defects around the same field, resolved independently:

1. **Field documented as a V1 boolean.** The doc comment said "Replica is
   saturated if KV cache utilization >= this threshold", and `ApplyDefaults`
   grouped it under "V1 thresholds". That implies the opposite tuning direction
   from what V2 does, and misled both a config and the e2e suite. Rewritten to
   describe the multiplier.
2. **The fallback path cancelled it.** `computeReplicaCapacityFallback` charged
   demand against the already-thresholded capacity, so the threshold appeared on
   both sides of demand/supply and utilization collapsed to `KvCacheUsage`
   regardless of configuration. Demand is now charged against the raw stored
   capacity, matching the main path (`utilization == KvCacheUsage /
   kvCacheThreshold`).
3. **Truncation could erase a replica.** `int64(capacity × threshold)` floored to
   zero for small products, and the zero-guard then dropped the replica entirely,
   leaving the engine with a shortfall it could not size. Now floored at one
   token, with a genuinely-zero stored capacity still returning nil.
4. **The unsizable state was silent.** `warnUnsizableShortfall` in
   `engine_v2.go` now logs when capacity is required but no variant has a
   per-replica capacity to supply it.

## Related

- `docs/plans/analyzers/analyzer-architecture-refactor.md`
- `issue-v2-kv-target-below-usage-blocks-scaleup.md` (the investigation)
