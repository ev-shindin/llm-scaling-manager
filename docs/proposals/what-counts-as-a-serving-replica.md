# What counts as a serving replica

WVA decides how much capacity a variant has by counting the Pods that reported
engine metrics. That count is used as though it meant "Pods that are taking
traffic right now". It does not, and the gap has produced three separate
symptoms that were each investigated as unrelated bugs.

This note states what the count means today, what it should mean, and what has
to change to close the difference. It covers ordinary replicas and warm pool
Pods together, because the same count is used for both and the answer differs
between them.

## What the count means today

A Pod contributes to a variant's measured supply when both of these hold:

1. a Prometheus query returned a series carrying its `pod`/`pod_name` label, and
2. `locator.Locate` resolved that Pod name to a managed scaler.

Neither condition says the Pod exists, is Ready, or is serving anything.
`internal/collector/replica_metrics.go` and `internal/collector/locator/` check
no Pod phase, no readiness condition and no `DeletionTimestamp`; and the
locator's pod→target resolution is deliberately cached "indefinitely", on the
sound reasoning that ownerReferences are immutable — which also means a Pod that
has been **deleted** still resolves for as long as the cache holds it.

So the count today includes:

- **Pods that are not yet Ready.** vLLM serves `/metrics` as soon as its HTTP
  server is up, which is before it passes readiness. The engine is scraping and
  the EPP is not yet routing to it.
- **Pods that are terminating.** A Pod with a `DeletionTimestamp` is still in the
  API and still scraped until it goes.
- **Pods that are already gone**, for as long as Prometheus retains their series
  (~5 minutes) — because the locator cache still resolves the name.

## The three symptoms

All measured on pokprod or in the e2e suite, 2026-08-30/31.

- **`ready 1, serving 4` with no bridge lent.** The scale target had one replica;
  three others had existed minutes earlier. The warm pool's return rule was
  reading a fleet four times the size of the one that existed.
- **`ready 3, serving 4` during a 1→4 scale-up.** Metrics led the Deployment's
  `Status.ReadyReplicas`. Benign in itself, but indistinguishable from the bug
  above without knowing which Pods were counted — which is why the e2e's
  `serving <= ready` assertion is only sound where the spec pins the fleet.
- **`scale_down_conceded_supply_test.go` fails about 1 run in 3**, and fails
  FAST (45 s against 370 s for a pass) with "WVA scaled below the target while an
  unowned replica was reporting: its capacity was counted as spare". A fast
  failure means the fleet was already below target when the spec began, which
  points at supply carried over from the previous run rather than at the
  behaviour under test.

The first and third are the same defect seen from two directions: capacity is
attributed to Pods that are not serving.

## What it should mean

The count has one consumer today — the warm pool's return rule, which asks: *have
the ordinary replicas arrived, so the Pod I lent can go home?* For that question
a Pod counts only if handing traffic back to it would actually be served. That is
three conditions, not one:

- it **exists and is not terminating** (`DeletionTimestamp == nil`),
- it is **Ready**, so the Service or EPP will route to it,
- it is **reporting**, so the engine has registered and is scraping.

Today only the third is checked. `ServingStore`'s own doc argues that reporting
is a better signal than the kubelet probe, and it is — but as an ADDITION to the
probe, not a replacement for it. A terminating Pod reports. A not-yet-Ready Pod
reports. A deleted Pod's series report for another five minutes.

### Warm pool Pods are a separate case

A pool Pod is not an ordinary replica and must not be counted as one. Two rules
already exist and should be stated together with the above:

- a Pod **lent** to a variant contributes that variant's DEMAND but never its
  SUPPLY (`publishServing` skips `FromWarmPool`; the analyzer splits
  `ownReplicas` from `warmPoolReplicas`);
- a pool Pod that is **awake but not lent** belongs to the pool's own scale
  target and must not reach any model's count at all.

Both hold today. What does not hold is the liveness and readiness of the Pods on
either side of that split.


## Where the fix can and cannot reach

The natural place for this is the BUILDER step, `buildInstanceKey`. Every series
of every query the collector runs passes through it, it already resolves pod
identity there, and a Pod it rejects can reach no downstream consumer. That
covers the analyzers that read `domain.ReplicaMetrics` -- `saturation` and
`throughput` both take `input.ReplicaMetrics` -- so one fix serves both without
either knowing about it.

**It does NOT cover external analyzers, and that asymmetry is structural.** An
external analyzer (`internal/engines/analyzers/external`) is config-driven
PromQL: it calls `source.Refresh` with the operator's own query body and reduces
the result to a single demand scalar D, then divides by a threshold P. It never
builds a per-Pod record, never calls the builder, and has no step at which a Pod
could be filtered. If the operator's query sums a per-Pod series, deleted Pods
inflate D for as long as Prometheus keeps them -- the same defect, arrived at by
a path the builder cannot see.

Three ways to answer that, and it is a design decision rather than a bug fix:

1. **The operator owns their query.** Honest, and it puts staleness handling in
   PromQL that is awkward to write and easy to omit.
2. **Push the hygiene into the metrics SOURCE**, so any query -- collector or
   external -- can be restricted to Pods that are live and Ready. This is the
   only option that makes "all analyzers are treated the same way" true rather
   than nearly true.
3. **Document the asymmetry** and accept that an external analyzer measures a
   different population than a built-in one.

**Option 2 was chosen and is implemented.** It turned out far smaller than
feared: an external analyzer does NOT push its reduction into Prometheus. It
asks the source for the query's series and sums them in Go, so the same Pod gate
can be applied to each series before it is added. `collector.SeriesPodIsGone`
exposes the builder's judgement for callers that hold raw series, and the engine
wires it into every external analyzer it constructs.

The one case it cannot judge is an ALREADY-AGGREGATED body -- `sum(...)` in the
operator's PromQL -- where Prometheus has done the reduction and no Pod label
survives. Those are summed untouched, which is the honest answer: there is no
per-Pod identity left to reject, and dropping such a series would silently zero
the demand of most useful analyzers.

## What has to change

1. ~~Filter the collector's rows by Pod state~~ DONE, in `buildInstanceKey`.
2. ~~Carry readiness onto the row~~ DONE: `domain.ReplicaMetrics.Ready`, required
   by `publishServing`. Deliberately NOT used by the analyzer -- a starting Pod
   holds its GPU and its KV cache is real, so it is still capacity.
3. ~~Bound the locator cache by Pod existence~~ NOT NEEDED. The builder now
   rejects a Pod the listing does not contain, so a stale cache entry can no
   longer resurrect one; invalidating the cache as well would buy nothing and
   would give up the property it exists for.
4. **Re-check the conceded-supply spec.** STILL OPEN, and now the measurement
   that says whether any of this worked: it failed 1 run in 3 before, and the
   leading explanation was supply carried over from the previous run.

### How Pod state is read

One `List` per namespace per cycle, memoized alongside the query results and
released by `EndCycle`, through the UNCACHED `apiReader`. Not the manager's
client: its cache holds no Pods, so reading Pods through it would start a Pod
informer -- namespace-wide on a scoped install, cluster-wide on a cluster-scoped
one. Not the locator's cache either: that is sound only because ownerReferences
cannot change, and readiness and deletion are exactly the mutable facts it must
never serve.

### Failing open is part of the design

A listing that SUCCEEDED and lacks a Pod says the Pod is gone. A listing that
FAILED says nothing. Conflating them would drop every series in the namespace on
an RBAC error or a moment of API unavailability, hand the analyzer zero supply,
and scale the fleet to its floor. So both helpers fail open, and under failure
WVA behaves exactly as it did before any of this existed.
