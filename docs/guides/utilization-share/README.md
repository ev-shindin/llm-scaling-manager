# Turn on the utilization-share optimizer, safely

## Overview

The utilization-share optimizer shares one GPU quota between the models under
it. Each model first gets the GPUs it needs, and the rest of the quota is shared
as headroom, by weight. When the shares drift, the optimizer moves GPUs from one
model to another: the donor scales down first, and the receiver is raised only
once the GPUs are free. Whether it suits your fleet, and what it costs, is on
the well-lit path:
[Share one GPU quota between models whose peaks do not coincide](../../well-lit-paths/utilization-share/).

This guide turns it on in steps: in **shadow**, where it computes and reports
but moves nothing; on a cluster quota, a **canary** that acts on a few
namespaces only; then **active**; then, if needed, back out. It is
one block in one ConfigMap, read live. The care is in the order: the
`shadow` key **defaults to `false`**, so a block written without it moves
replicas from the next cycle.

**Experimental.** It has been tested on kind with emulated GPUs, and
benchmarked in three runs on two aggregated models with 1 GPU per replica
([what they measured](../../well-lit-paths/utilization-share/measured.md)).
Two of them found a model held short for its whole burst; the fixes are built
and not yet measured. P/D, whole-node and multi-GPU pods have not been
measured.

## Prerequisites

A scaling-manager install with models registered, and a **quota limiter** in the
cluster default policy. [Cap what each tenant may take](../../well-lit-paths/tenant-gpu-quotas/)
declares one (`WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=<n>'`). The optimizer
shares the budget a quota gives, so without one it has nothing to share.

`<policy-namespace>` below is the namespace that holds the cluster policy:
`wva-policy` when an admin published one there, otherwise the controller's own
namespace ([where policy lives](../../reference/gpu-limiter.md#what-the-controller-does-with-that)).

<!-- guide:prerequisites.limiter start -->
```bash
# The optimizer shares the budget a QUOTA limiter gives, so the cluster
# default entry must already declare one. Without a limiters: list the
# optimizer stays off and reports mode "invalid".
# Read it from the namespace that holds the cluster policy: wva-policy when
# an admin published one there, otherwise the controller's own namespace.
# The last line says whether the controller ACCEPTED the entry: a malformed
# one is rejected on read and leaves NO limiter while the ConfigMap still
# looks healthy.
kubectl get configmap wva-scaling-policy-config -n <policy-namespace> \
  -o jsonpath='{.data.default}' | grep -A12 limiters
kubectl logs -n <wva-namespace> deploy/wva-controller-manager \
  | grep -E 'GPU limiter constructed|Invalid saturation scaling'
```
<!-- guide:prerequisites.limiter end -->

Then check the accelerators. A missing quota entry denies, so a quota keyed on
a name the controller never resolves is a quota of zero:

<!-- guide:prerequisites.accelerators start -->
```bash
# Every variant's accelerator must resolve, and the quota's type must be the
# name the controller resolves. A variant whose accelerator does not resolve
# gets no budget, and a quota key nobody resolves to is a quota of zero.
# The event hangs on the ScaledObject, in the MODEL's namespace -- hence -A.
kubectl get events -A --field-selector reason=AcceleratorNotResolved
```
<!-- guide:prerequisites.accelerators end -->

No output is the healthy answer.

Before the optimizer acts, the model owners need one thing. A transfer removes a
donor pod through an ordinary scale-down, so a request in flight on that pod
survives only if the model server drains: a `preStop` hook and a
`terminationGracePeriodSeconds` long enough to finish. If several controllers
share the cluster, each also needs its own `CONTROLLER_INSTANCE`. Otherwise one
controller's cleanup removes the other's live transfer marks.

## Installation Instructions

**Step one: shadow.**

<!-- guide:deploy.shadow start -->
```bash
# Add the optimizer block to the SAME default entry as limiters:, in
# shadow. shadow defaults to false, so a block without it ACTS from the
# next cycle -- write shadow: true explicitly.
# Read live on the next cycle. On a namespace-scoped install whose cluster
# policy lives in a separate policy namespace, the controller reads that
# policy only when it starts: restart it after the edit.
kubectl edit configmap wva-scaling-policy-config -n <policy-namespace>
# under data.default, beside limiters:
#   optimizer:
#     type: utilizationShare
#     utilizationShare:
#       shadow: true
#       # clusterNamespaces: [team-canary]   # cluster quota only: plan these namespaces alone
```
<!-- guide:deploy.shadow end -->

**Step two, on a cluster quota: a canary.** After reading what shadow would do
(below), act on a few namespaces before the whole cluster. `clusterNamespaces`
limits the cluster group to the models of the namespaces you list, and leaves
every other model on today's optimizer. On **namespace** quotas there is no
canary; keep a namespace out with `namespaces: {<ns>: {enabled: false}}`
instead, and skip to step three. Each key is described in
[the reference](../../reference/scaling-policy.md#optimizer-cluster-default-only-live).

<!-- guide:deploy.canary start -->
```bash
# Cluster quota only, and optional: act on a few namespaces first.
# clusterNamespaces limits the cluster group to the models of the
# namespaces listed; every other model stays on today's optimizer, and the
# GPUs it holds stay outside the group. Pick namespaces whose owners
# agreed, act on them alone, and read the active checks below before
# widening the list or removing it (empty plans every namespace).
# On namespace quotas there is no canary: keep a namespace out with
# namespaces: {<ns>: {enabled: false}} instead.
kubectl edit configmap wva-scaling-policy-config -n <policy-namespace>
#       shadow: false
#       clusterNamespaces: [team-canary]
```
<!-- guide:deploy.canary end -->

**Step three, after reading what shadow (and the canary) did: active.**

<!-- guide:deploy.active start -->
```bash
# Only after reading what shadow would do (and, on a cluster quota, what
# the canary did). From the first active cycle, each planned group holds
# what it runs for one fill timeout (about three minutes with default
# timings), nothing in it scales, and its models show the blocked reason
# quiet-period. Expect the same after every controller restart.
# After a canary, shadow is already false: widen or remove
# clusterNamespaces instead.
kubectl edit configmap wva-scaling-policy-config -n <policy-namespace>
#       shadow: false
#       # after a canary: clusterNamespaces: [team-canary, team-b], or remove the key
```
<!-- guide:deploy.active end -->

## Verification

**In shadow.** The mode comes first, then what the optimizer would do:

<!-- guide:verify.shadow start -->
```bash
# The mode first: exactly one series is 1. "invalid" means the block did not
# validate or there is no limiters: list -- the log line below says which.
# Then what it WOULD do: actionable roles, replicas_to_move per group, and
# headroom per role (negative = short). spare_gpus below 0 means the quota
# cannot cover every need, and no rebalance fixes that.
# The leader publishes the mode; with more than one controller replica,
# port-forward to the leader pod, or query Prometheus.
kubectl port-forward -n <wva-namespace> svc/wva-controller-manager-metrics-service 8443:8443 &
curl -sk https://localhost:8443/metrics | grep -E '^wva_utilization_share_(mode|actionable|replicas_to_move|headroom|spare_gpus)'
kubectl logs -n <wva-namespace> deploy/wva-controller-manager \
  | grep -E 'Utilization share: (would rebalance|invalid optimizer block)'
# The same in PromQL:
#   wva_utilization_share_mode == 1
#   wva_utilization_share_actionable == 1
#   wva_utilization_share_replicas_to_move
#   wva_utilization_share_headroom
```
<!-- guide:verify.shadow end -->

| what you read | what it says |
| --- | --- |
| `wva_utilization_share_mode{mode="shadow"} 1` | the block is read and valid. `invalid` means the log names the problem; `off` means the controller read no `optimizer:` block, or one with an empty `type` |
| `wva_utilization_share_actionable 1` | a role a move would fix now: outside its tolerance band, and off its whole-replica target |
| `wva_utilization_share_replicas_to_move` | per quota group: how many replicas it would move |
| `wva_utilization_share_headroom` | per role: the spike it absorbs before it must scale, as a fraction of need. Negative means the role is short |
| `wva_utilization_share_spare_gpus` below `0` | the quota cannot cover every need. Moving GPUs cannot fix that |

Leave it in shadow long enough to see each model's peak. Switch to active when
the roles it would move, and how often, are what you expect. A model it never
plans appears in the `frozen` field of the `would rebalance (shadow)` log line,
with the reason. A model whose variants span two accelerator types is never
planned at all.

**Active.** Transfers, the blocked reasons, and the Events on the scale targets:

<!-- guide:verify.active start -->
```bash
# Transfers that ended, by outcome: done is healthy; rising aborted,
# fill-timeout or wrong-pod means transfers are not landing. Every outcome
# is published at 0 while a group acts, so increase() counts the first.
# wrong-pod needs node information: a namespace-scoped install never
# reports it. A model held back shows a reason on wva_model_scaling_blocked.
# Every transfer is also an Event on the Deployment or LeaderWorkerSet it
# moved, in the model's namespace. Warnings are the ones to read.
curl -sk https://localhost:8443/metrics | grep -E '^wva_utilization_share_(mode|transfers_total|in_flight)'
curl -sk https://localhost:8443/metrics | grep '^wva_model_scaling_blocked'
kubectl get events -n <llmd-namespace> --field-selector reason=UtilizationShareGiving
kubectl get events -n <llmd-namespace> --field-selector reason=UtilizationShareReceived
kubectl get events -n <llmd-namespace> --field-selector type=Warning | grep UtilizationShare
# The same in PromQL:
#   sum by (outcome) (increase(wva_utilization_share_transfers_total[1h]))
```
<!-- guide:verify.active end -->

| Event reason | type | means |
| --- | --- | --- |
| `UtilizationShareGiving` | Normal | a donor gives one replica; names the pod that goes and the receiver |
| `UtilizationShareReceived` | Normal | the transfer landed: the receiver holds the GPUs. An idle fill says the GPUs came from the quota group's spare, as headroom by weight |
| `UtilizationShareReleaseAborted` | Warning | the donor did not release within the release timeout; its count is restored. Or its donor set could not complete; the Event then says so, and the donor is not held back |
| `UtilizationShareFillTimedOut` | Warning | the receiver's new replica did not take the released GPUs in time |
| `UtilizationShareWrongPod` | Warning | a pod other than the marked one was removed |
| `UtilizationShareDonorNotSteerable` | Warning | the pod that would go could not be chosen: a pod not Ready, or a rollout in progress |

Events expire, after an hour by default: read them soon after a transfer, and
use the counters for anything older.

The full list, with `Receiving`, `Cancelled` and `Redirected`, is in
[monitoring](../../reference/monitoring.md#events-on-the-models-scale-targets).
What each blocked reason means and what to do about it is in
[the metrics reference](../../reference/prometheus.md#wva_model_scaling_blocked-reasons-set-by-the-utilization-share-optimizer).

Expect nothing to scale in a group for about three minutes after the switch.
That is the freeze of one fill timeout described above, not a fault; its models
show the blocked reason `quiet-period` meanwhile. If it
still moves nothing after that, work down
[the troubleshooting list](../../reference/troubleshooting.md#the-optimizer-evaluates-but-never-moves-anything).

## Cleanup

Back to shadow, and confirm no transfer marks are left on any pod:

<!-- guide:cleanup.rollback start -->
```bash
# Back to shadow FIRST, and wait one optimization cycle. The controller then
# removes every transfer mark it wrote and restores each pod's previous
# deletion cost. Do this before a downgrade to a version without the
# optimizer too: that version leaves the marks behind.
# On a namespace-scoped install whose cluster policy lives in a separate
# policy namespace: edit, restart the controller, then wait a cycle.
# The last command must print nothing.
kubectl edit configmap wva-scaling-policy-config -n <policy-namespace>
#       shadow: true
# namespace-scoped install, separate policy namespace only:
#   kubectl rollout restart -n <wva-namespace> deploy/wva-controller-manager
kubectl get pods -A -o json | jq -r '.items[]
  | select(.metadata.annotations["llm-d.ai/utilization-share-transfer"] != null)
  | .metadata.namespace + "/" + .metadata.name'
```
<!-- guide:cleanup.rollback end -->

A mark is a `llm-d.ai/utilization-share-transfer` annotation and a low
`controller.kubernetes.io/pod-deletion-cost`, so the ReplicaSet removes that pod
first on any scale-down. A version without the optimizer does not remove them.
If some are left after a downgrade, or after a `CONTROLLER_INSTANCE` was
renamed, the cleanup is in
[troubleshooting](../../reference/troubleshooting.md#transfer-marks-left-on-pods-after-a-downgrade).

Then remove the block:

<!-- guide:cleanup.remove start -->
```bash
# Then delete the optimizer: block. The mode reads "off" and today's
# optimizer runs; the limiters are untouched. The transfer counters keep
# their last values, as counters do.
kubectl edit configmap wva-scaling-policy-config -n <policy-namespace>
```
<!-- guide:cleanup.remove end -->

## Monitoring

The alerts worth having once it is configured are in
[monitoring](../../reference/monitoring.md#the-metrics-that-answer-specific-questions):
a block that turned `invalid` or `off`, transfers that do not land, releases
close to their bound, and models held back by a condition that does not clear
by itself.

## Next

- [The well-lit path](../../well-lit-paths/utilization-share/): when not to
  take it, what it costs, and how it is tested
- [`optimizer` in the scaling policy](../../reference/scaling-policy.md#optimizer-cluster-default-only-live):
  every key, weights per model, and what it writes on pods
- [For model owners](../../reference/scaling-policy.md#for-model-owners-what-the-optimizer-may-take-and-how-to-protect-a-model):
  what can be taken from a model, and how to protect one
