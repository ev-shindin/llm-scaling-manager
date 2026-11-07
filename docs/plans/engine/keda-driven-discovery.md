# KEDA-driven discovery

Status: increments 1-4 landed; one informer left (see "What still watches")
Branch: `feat/wva-external-scaler`

## What changes

WVA used to discover the workloads it manages by listing the cluster and filtering
on an annotation. This inverts that: **the KEDA external-scaler call is the discovery
event.** WVA stops looking for variants and lets them announce themselves.

Before:

```text
timer ──► List HPAs (cluster-wide)  ──┐
     ──► List ScaledObjects        ──┴─► filter llm-d.ai/managed
                                       ──► synthesize VariantAutoscaling from
                                           llm-d.ai/model-id, llm-d.ai/variant-cost
                                       ──► collector ──► optimizer ──► decision.Set

KEDA ──► GetMetrics ──────────────────► read decision store
```

After:

```text
KEDA ──► IsActive / StreamIsActive / GetMetricSpec / GetMetrics
           │  ScaledObjectRef{namespace, name, scalerMetadata}
           ├─► registry.Observe(...)   ◄── THIS is discovery
           └─► read decision store     ◄── the answer

timer ──► registry.Snapshot() ──► collector ──► optimizer ──► decision.Set
```

Three consequences:

- **No listing for discovery.** A ScaledObject WVA has never been called about
  does not exist as far as WVA is concerned. RBAC narrows to `get` by name.
- **No WVA annotations.** Being called *is* being managed, so `llm-d.ai/managed`
  has nothing left to say. Identity comes from the trigger's `metadata`, which
  KEDA already delivers verbatim as `scalerMetadata` on every call.
- **The HPA path goes.** It has no equivalent of "KEDA calls us", so it cannot be
  driven this way. It returns later as an external-metrics API server (see
  [HPA-only clusters](#hpa-only-clusters)).

The deleted reconcilers matter less than the deleted **indexes**. `IndexField`
starts a cluster-wide LIST+WATCH informer for its kind at startup whether or not
any code calls `List`, so removing the `List` calls alone would have moved the
listing rather than removed it. For the same reason WVA's per-entry ScaledObject
read uses `GetAPIReader`: a *cached* Get is served by that informer.

## What the call drives, and what it does not

The call drives **membership, not cadence.**

KEDA calls once per ScaledObject. The optimizer is inherently cross-variant — one
GPU budget, fair share across models, a limiter that has to see the whole fleet
at once — so answering each call by optimizing that one variant in isolation
would throw away the only view in which those decisions are meaningful. The
engines therefore keep their own cadence (saturation every `GLOBAL_OPT_INTERVAL`,
scale-from-zero every 100ms) and iterate the registry snapshot where they used to
iterate a List result.

So the call makes a variant *exist*; the timer decides what it *gets*.

## Identity: trigger metadata replaces annotations

KEDA passes the trigger's `metadata` map to every RPC as `scalerMetadata`. That is
the whole configuration surface.

| was | becomes | required |
|---|---|---|
| `llm-d.ai/managed: "true"` | — registration replaces it | — |
| `llm-d.ai/model-id` | `modelID` | yes |
| `llm-d.ai/variant-cost` | `variantCost` | no (defaults) |

```yaml
triggers:
- type: external-push
  name: wva-external-scaler
  metadata:
    scalerAddress: wva-external-scaler.<ns>.svc.cluster.local:9090
    modelID: default/default
    variantCost: "10.0"
```

Reserved for later increments, parsed but not yet consumed: `inferencePool`,
`engine`, `scalingPolicy`, `wvaOwnership` (see [named policy tiers](../../reference/scaling-policy.md#3-named-policy-tiers)).

`minReplicaCount` / `maxReplicaCount` / `advanced.horizontalPodAutoscalerConfig`
are NOT metadata — they stay on the ScaledObject, where KEDA needs them anyway,
and WVA reads them with a targeted `Get` of the object it was just called about.

## Liveness

An entry is live while KEDA is still interested in it. Two independent signals,
because at zero replicas only one of them exists:

- **A recent call.** Any RPC refreshes `LastSeen`. A `type: external` trigger
  polls `IsActive` every `pollingInterval`, so this alone keeps a parked workload
  live.
- **An open stream.** A `type: external-push` trigger calls `StreamIsActive` once
  and holds it. At zero replicas nothing else is called at all — the HPA does not
  query metrics for a workload it is not scaling — so the open stream must count
  as liveness on its own, for as long as it is open. It is released when the
  stream closes, after which the TTL takes over.

TTL defaults to 5 minutes: comfortably above KEDA's 30s default
`pollingInterval` and the 15s optimize interval, so a slow poll never evicts a
live workload, while an abandoned entry does not linger for the process lifetime.

## Why not watch ScaledObjects?

The obvious alternative is to subscribe: watch ScaledObjects and update the
registry the moment one changes. It is rejected, because **a watch is a list.**
A controller-runtime informer does an initial cluster-wide LIST and then holds a
WATCH, keeping every ScaledObject in the cluster in memory — including the ones
WVA does not manage — and needs `list;watch` cluster-wide in RBAC. That is
precisely the thing this design removes. (Note this is also why WVA's per-entry
read uses `GetAPIReader`: a *cached* Get is served by that same informer, so
reading through `GetClient()` would reinstate the watch without anyone writing
the word "watch".)

The gap a watch would close is narrower than it looks, because **KEDA already
notifies us.** When a ScaledObject's generation changes, KEDA rebuilds its scaler
cache: it re-issues `GetMetricSpec` and closes and re-opens `StreamIsActive`. So
a change to the trigger — which is where WVA's own configuration lives — is
pushed to WVA for free, on the object's own edit.

What KEDA does not forward is the rest of the object: `scaleTargetRef`,
`minReplicaCount`, `maxReplicaCount`. So:

Two signals invalidate the entry's cached target read, and between them they
cover both shapes of edit:

- **Metadata that differs from what is stored.** KEDA only re-sends a trigger it
  has rebuilt, so different metadata means the object changed.
- **A stream opening.** KEDA re-opens `StreamIsActive` when it rebuilds its
  scaler cache, which it does on a generation change — so a fresh stream is
  evidence of an edit even when the trigger is untouched. This is what catches
  `scaleTargetRef` and min/max, which metadata comparison cannot see because they
  are not carried in the trigger.

Both zero the read's DATE, not the target itself: the last known envelope keeps
serving until the fresh read lands, so an edit never drops a variant out of the
fleet mid-cycle.

Stream-open over-fires — KEDA also re-opens on its own restart or a dropped
connection. That is the deliberate trade: one uncached GET per reconnect per
workload, against a watch on every ScaledObject in the cluster. A KEDA restart
re-opens every stream at once, so the next pass re-reads the whole fleet, bounded
at one GET per variant and swept serially.

What remains uncovered is an edit to a workload on a POLL trigger (`type:
external`), which has no stream to re-open — that still waits out the freshness
window (≤30s). Shortening `DefaultTargetMaxAge` is the lever there.

### Restart

The registry is in-memory and starts empty. After a WVA restart, KEDA re-opens its
streams and resumes polling, so the registry refills within one `pollingInterval`.
Until it does, the engines see no variants and publish no decisions — which is
exactly the state WVA is already in before its first optimization cycle:
`GetMetrics` returns 0 and HPA holds the target at `minReplicaCount`. No new
failure mode, and no persistence needed.

> **Changed since:** `GetMetrics` no longer returns 0 before the first decision;
> it answers the target's current replica count from the `scale` subresource,
> so the HPA holds the target where it is. See
> [before the first decision](../../reference/scaledobject-trigger.md#before-the-first-decision-and-the-fallback).

## Consequence: the Prometheus-trigger delivery mode is gone

WVA could be delivered two ways: KEDA calls WVA's external scaler over gRPC, or
WVA publishes a `wva_desired_replicas` gauge and KEDA reads it with a plain
`prometheus` trigger. The second was the documented escape hatch.

Call-driven discovery removes it, and not by choice — **a `prometheus` trigger
never contacts WVA at all.** KEDA talks to Prometheus, not to us. There is no call,
so there is no registration, so the workload is never discovered, so WVA never
publishes the gauge KEDA is waiting for. The two halves deadlock.

It fails quietly, which is the dangerous part: nothing errors, the workload simply
sits at its replica count. The e2e fixture default was moved to the external
trigger for exactly this reason (`fixtures.SetExternalScalerAddress`), so a suite
cannot forget it and then time out with no explanation.

If the metric-shop mode is ever wanted back, it needs its own registration
channel — a config-listed set of workloads, or the external-metrics API server
below, whose request would register the object the same way a gRPC call does. An
annotation would work too, but that is what this design removed.

## HPA-only clusters

Killed now, restored later as a separate path: WVA serves
`external.metrics.k8s.io` so an HPA's `External` metric source reads
`wva-desired-replicas` the same way the KEDA trigger does. The registry then has a
second feeder — the metrics API request — with the same shape as the gRPC call:
the request names an object, and naming it registers it. That symmetry is the
reason to build the registry as a feeder-agnostic component now rather than
folding it into the gRPC handler.

Not in this plan. Next step after it.

## Increments — all landed

1. **Registry, fed by the scaler.** `internal/registry`. Every RPC upserts;
   `StreamIsActive` holds.
2. **Engines read the registry.** `utils.{Active,Inactive}VariantAutoscaling`
   build from a snapshot plus the enrichment cached on each entry. The
   synthesized `VariantAutoscaling` stays as the internal carrier; only where it
   comes from changed.
3. **Removed the HPA path**, and the Coordinator with it — a second writer on the
   scale subresource whose discovery was a cluster-wide listing of the two kinds
   this work removes from WVA's read path.
4. **Retired the annotations.** `llm-d.ai/managed`, `model-id` and
   `variant-cost` are gone, along with the annotation predicate and the index
   filter. Only `llm-d.ai/synthetic` survives, and only as the guard that keeps an
   in-memory object from reaching the API server.

## What still watches: nothing

No informer, index, or watch on ScaledObjects or HPAs remains. Three separate
things held the last one, and removing any single one would have left it in place:

- **the field index** — `IndexField` starts a LIST+WATCH for its kind at startup
  whether or not anything calls `List`, so deleting the index's callers was never
  going to be enough;
- **the ScaledObject reconciler's watch**, which existed only to track which
  namespaces hold managed workloads. The registry knows that for a better reason,
  so `Enricher.Refresh` reconciles the datastore's tracked set each pass;
- **the external scaler's own reads** — `h.client` was `mgr.GetClient()`, and a
  *cached* Get lazily starts the informer on first use. It would have appeared the
  moment KEDA made its first call, with nothing in the code saying "watch".

Pod attribution — the reason the index existed — now resolves against the
registry, which already holds ScaledObject → scale-target for every managed
workload. `targetName` consults the registry before falling back to an uncached
read, because uncached is mandatory here: without that hop, every KEDA poll of
every workload would be a real API request.

Still listed, deliberately, and none of it discovery: node inventory (cluster GPU
capacity), the locator's pod reads (uncached, per owner-chain walk), and the
ConfigMap namespace list at bootstrap.

