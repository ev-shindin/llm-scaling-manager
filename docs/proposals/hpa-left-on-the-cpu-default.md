# An HPA left on the Kubernetes CPU default

**Status:** proposed, nothing built. Describes a live failure mode and asks for
one decision: whether the scaling manager should look at the HorizontalPodAutoscaler
KEDA builds from it.

## The failure

KEDA asks the external scaler for a metric specification when it reconciles a
ScaledObject, and writes the answer into the HPA's `spec.metrics`. Get no answer
and it creates the HPA with an **empty metrics list**. Kubernetes defaults an
empty list to `Resource/cpu`, and the workload then scales on a CPU metric that
a cluster without metrics-server cannot evaluate at all.

Nothing reports this. Observed in CI:

```
HPA:  spec.metrics [Resource], CurrentMetrics one all-nil entry,
      ScalingActive=False  FailedGetResourceMetric x41
SO:   Ready=True  "ScaledObject is defined correctly and is ready for scaling"
      Active=True
WVA:  EmitReplicaMetrics completed  desiredReplicas: 2   (x40, correct)
```

Every component reports health. The ScaledObject says it is ready, the scaling
manager publishes the right replica count every cycle, and the workload does not
move. The only object that knows is the HPA, and the manager does not read it.

**KEDA never re-asks.** It derives HPA metrics on a ScaledObject **spec** change;
an annotation change does not count, and nothing else triggers it. So a single
refused call at creation leaves that HPA unwired for the life of the
ScaledObject.

### The cause that is already mitigated

A standby replica refusing because it does not hold the lease. `Server`
deliberately reports `NeedLeaderElection() == false` and runs in two phases: a
standby answers `GetMetricSpec` before the lease is held, refuses only what
needs a decision, and cycles client connections so a client that landed on a
standby re-dials and reaches the leader. That is a real fix and it is in place.

### The cause that is not

Transport and timing. Measured from the KEDA operator log of the CI failure:

```
10:54:00  external_scaler  dial tcp 10.96.148.208:9090: connect: connection refused
10:54:05  (x2)             ... connection refused
10:54:20  external_scaler  dial tcp 10.96.148.208:9090: i/o timeout
10:54:20  scale_handler     error getting metric spec for the scaler: got empty metric spec
```

The manager was restarting. Its pod was not yet in the Service KEDA dials, and
the ScaledObject for the next spec was reconciled in that window. Within five
seconds the manager was answering `IsActive` normally and publishing decisions —
too late, because the HPA had already been built. No scaling-manager code runs
in the failing window, so no amount of care inside it closes this.

This cost **two multi-day CI investigations**. PR #124 made it visible in the
e2e suites and PR #125 made it visible in the manager's own log; neither fixed
it, and both say so.

## Why this is not already solved

- **The manager reads no HPAs.** There is no `horizontalpodautoscalers` rule in
  `config/` at all, so it cannot see the object that holds the evidence.
- **The only mitigation that works today lives in test code on an unmerged
  branch.** `waitForKEDAExternalMetricWired` — wait 45s, recreate the
  ScaledObject once, log it — is on the utilization-share branch and exists so
  the e2e suites stop flaking. Production has no equivalent.
- **Recreating the ScaledObject is the only known recovery**, because a spec
  change is the only thing that makes KEDA re-derive. An annotation nudge was
  tried during the #124 investigation and measured: 35 nudges over 210 seconds,
  and the HPA never left `[Resource]`.

## What to decide

Three options, in increasing order of what the manager takes on.

### A. Report only

Add read access to HorizontalPodAutoscalers, and for each managed scale target
compare the HPA's metrics against what the manager advertises. On a mismatch,
publish `wva_model_scaling_blocked{reason="hpa-on-cpu-default"}` and log it once
per occurrence.

The reason-label convention is already established —
`internal/constants/metrics.go` has `ScalingBlockedReasonsPolicy` and
`ScalingBlockedReasonsWake`, each owned and cleared by one writer — so this
needs a third owned set rather than a new gauge.

Cost: one watch, one RBAC rule, no new authority. The operator still has to act,
but they find out, and an alert becomes possible.

### B. Report and recover

As A, plus: on a confirmed mismatch, touch the ScaledObject's **spec** so KEDA
re-derives. The smallest real spec change needs identifying — the measurement
above shows an annotation is not enough.

Cost: write access to ScaledObjects, which the manager already has for parking.
The risk is a loop — a manager that recreates a ScaledObject KEDA then rebuilds
wrong recreates it again — so this needs a bounded attempt count and a reason
value for "recovery attempted and did not take".

### C. Do not build it

Accept that an HPA created during a manager restart may be dead, and document
the symptom and the manual recovery (`kubectl` edit the ScaledObject spec) in
the troubleshooting reference.

Defensible on one ground: the window is small and a restart is rare. Against it:
the window is **exactly** when a fleet is most likely to be reconciling
ScaledObjects, because an upgrade rolls the manager and the workloads together,
and the failure is permanent and silent once it happens.

## Recommendation

**A, then measure before B.** Reporting is cheap, needs no new authority, and
turns a silent permanent failure into an alertable one. Whether B is worth its
loop risk depends on how often A actually fires on a real fleet, which is not
currently known — nobody has counted, because nothing has been able to see it.

## What this does not cover

- **Why the dial failed** is upstream of the manager. Readiness gating the
  Service endpoint, or KEDA retrying a failed `GetMetricSpec`, would both
  prevent it; neither is this project's code.
- **The e2e flake is separate.** The suites need the recreate helper whatever is
  decided here, and it should be merged on its own rather than waiting on this.

## Open questions

1. Which ScaledObject spec mutation is the minimal one that makes KEDA
   re-derive? Required for B, and measurable on a kind cluster.
2. Should detection be a watch on HPAs, or a read during the optimize cycle for
   the targets the manager already touches? The second needs no informer and no
   cluster-wide list, which this project has deliberately avoided before (see
   `Server.Client`'s uncached-reader requirement).
3. Does the comparison belong in the scaler package, which knows what it
   advertises, or in the engine, which knows the managed targets?
4. Is one reason value enough, or should "no metrics at all" and "metrics that
   are not ours" be distinguished? They have different causes.
