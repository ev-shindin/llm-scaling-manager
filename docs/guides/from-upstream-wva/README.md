# Coming from llm-d's workload-variant-autoscaler

## Overview

This guide moves a cluster from llm-d's workload-variant-autoscaler (WVA, up to
its `v0.9.x` releases) to the scaling manager (llm-scaling-manager).

The scaling manager is an independently developed fork of WVA. It is not
affiliated with or endorsed by llm-d. It was forked from upstream commit
`9906dac` (1 August 2026); upstream's `v0.9.0` tag is the next commit. A
`v0.9.0` install is therefore the closest starting point. Older releases need
the extra steps marked *up to v0.8*.

You keep your model servers, KEDA and Prometheus. You replace the controller,
and point each workload at the new one.

**What changes for a workload.** The scaling manager is a KEDA external
scaler. A workload is registered by a KEDA ScaledObject whose `external-push`
trigger names the scaler. The `llm-d.ai/managed`, `llm-d.ai/model-id` and
`llm-d.ai/variant-cost` annotations are not read, and there is no HPA +
Prometheus Adapter path. The rest of this guide converts what you have.

## Prerequisites

- [ ] **KEDA** on the cluster ([supported versions](../../reference/keda.md)).
  Workloads that upstream scaled through a plain HPA move to KEDA too.
- [ ] **Prometheus** with the Prometheus Operator, as upstream needed.
- [ ] **A cluster admin** for the one-time setup, as in the [install
  guides](#4-install-the-scaling-manager).
- [ ] **A short window.** Between steps 3 and 5 no controller sizes your
  workloads; frozen ones hold their replica count.

Find the upstream install and what it scales:

<!-- guide:prerequisites start -->
```bash
# Find the upstream controller. ARGS without --watch-namespace means it manages every namespace.
kubectl get deploy -A -l app.kubernetes.io/name=workload-variant-autoscaler,control-plane=controller-manager -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,IMAGE:.spec.template.spec.containers[0].image,ARGS:.spec.template.spec.containers[0].args

# Installed with Helm? A release with a workload-variant-autoscaler chart means yes.
helm list -A | grep workload-variant-autoscaler

# List what it scales. MANAGED true marks the ones upstream discovered. OWNER ScaledObject marks an HPA that KEDA owns.
kubectl get scaledobject -A -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,MANAGED:.metadata.annotations.llm-d\.ai/managed,TRIGGERS:.spec.triggers[*].type,MIN:.spec.minReplicaCount'
kubectl get hpa -A -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,MANAGED:.metadata.annotations.llm-d\.ai/managed,OWNER:.metadata.ownerReferences[0].kind'
```
<!-- guide:prerequisites end -->

Note the upstream namespace (`<upstream-namespace>` below), and which
workloads are scaled by a ScaledObject and which by a plain HPA (an HPA with no
`ScaledObject` owner). *Up to v0.8*, also list `kubectl get variantautoscalings -A`.

## Installation Instructions

### 1. Save the upstream settings

<!-- guide:migrate.save start -->
```bash
# Save the upstream ConfigMaps. Your scaling settings are in the one named *saturation-scaling-config.
kubectl get configmap -n <upstream-namespace> -o yaml > upstream-configmaps.yaml
```
<!-- guide:migrate.save end -->

### 2. Freeze the ScaledObjects upstream scales

Do this for each ScaledObject with a `prometheus` trigger that reads
`wva_desired_replicas`:

<!-- guide:migrate.freeze start -->
```bash
# Freeze every ScaledObject with a prometheus trigger that upstream scales. It holds its current replica count.
kubectl annotate scaledobject <name> -n <namespace> autoscaling.keda.sh/paused=true
```
<!-- guide:migrate.freeze end -->

Do not skip it. Once the upstream controller stops, the query soon returns
nothing, and KEDA's Prometheus trigger reads an empty result as `0` unless the
trigger sets `ignoreNullValues: "false"`. An unfrozen workload then scales down
to its `minReplicaCount`, which is `0` when the ScaledObject does not set it.

Workloads scaled by a plain HPA need nothing here; you replace the HPA in step 5.

### 3. Remove the upstream controller

<!-- guide:migrate.remove start -->
```bash
# Stop the upstream controller. Your workloads, KEDA and Prometheus stay.
kubectl delete deployment wva-controller-manager -n <upstream-namespace>
```
<!-- guide:migrate.remove end -->

*Up to v0.8*, installed with Helm:

<!-- guide:migrate.removehelm start -->
```bash
# Installed with Helm? Remove the release instead.
helm uninstall <release> -n <upstream-namespace>
```
<!-- guide:migrate.removehelm end -->

Why first: the scaling manager's installer refuses to install beside a
controller that manages every namespace. Both would also publish the same
`wva_` series.

### 4. Install the scaling manager

<!-- guide:migrate.checkout start -->
```bash
# Get a release of this project. The install guides run from this checkout.
git clone https://github.com/ev-shindin/llm-scaling-manager.git
cd llm-scaling-manager
git checkout v0.10.0
```
<!-- guide:migrate.checkout end -->

Then follow one install guide up to, not including, its "Register the
workloads" step:

- [Install in a namespace](../install-in-namespace/): one namespace. The
  default scope; `NAMESPACE` is required.
- [Install for the whole cluster](../install-cluster-wide/): every namespace,
  like an upstream install with no `--watch-namespace`.

The namespace guide runs `make check-prereqs`, `make setup-prereqs` (cluster
admin) and `make deploy-wva`. The cluster-wide guide runs
`make deploy-wva SCOPE=cluster`, which does both. Pin the release as [Pin the
version](../../reference/upgrade-and-uninstall.md#pin-the-version) shows.

Then carry over the settings you tuned, from `upstream-configmaps.yaml`:

<!-- guide:migrate.policy start -->
```bash
# Carry over the settings you tuned. Edit the new ConfigMap, not the old one.
kubectl edit configmap wva-scaling-policy-config -n <controller-namespace>
```
<!-- guide:migrate.policy end -->

- Thresholds (`scaleUpThreshold`, `scaleDownBoundary`, `kvCacheThreshold`,
  `queueLengthThreshold`), `analyzers`, tiers and per-model entries keep their
  keys.
- `enableLimiter: true` turns nothing on. Declare the limiter in the `default`
  entry's `limiters:` list ([limiters](../../reference/scaling-policy.md#limiters-cluster-default-only-live)).
- `kvSpareTrigger` and `queueSpareTrigger` are gone. Delete them.
- A key the scaling manager does not know is ignored and reported, not
  rejected.
- Re-running `make deploy-wva` resets the `default` entry. Keep lasting
  changes in a tier or a per-model entry ([Changing policy
  safely](../../reference/operations.md#changing-policy-safely)).

### 5. Register the workloads

<!-- guide:migrate.register start -->
```bash
# Write the plan, edit it, apply it. Cluster-wide install: add SCOPE=cluster to both.
make scaledobjects-plan WVA_DEFAULT_SO_PLAN=wva-plan.yaml
# edit wva-plan.yaml: for each upstream ScaledObject, change apply: no to apply: adopt
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```
<!-- guide:migrate.register end -->

In `wva-plan.yaml`:

- **Upstream ScaledObjects** appear as `apply: no`, with a note naming the
  ScaledObject. Change it to `apply: adopt`. Adopt replaces the `prometheus`
  trigger with the scaler's trigger and keeps the polling interval, cooldown,
  `fallback` and `behavior` ([what adopt
  keeps](../../reference/configuration.md#default-scaledobjects)).
- **Check `minReplicas` on each adopt entry.** It is read from the
  ScaledObject, and one without `minReplicaCount` reads as `0`.
- **Plain HPAs**: delete the HPA first (`kubectl delete hpa <name> -n
  <namespace>`), then set the workload's entry to `apply: yes`. Two autoscalers
  on one workload fight over its replica count.

Then unfreeze each adopted ScaledObject:

<!-- guide:migrate.unfreeze start -->
```bash
# Unfreeze each adopted ScaledObject. The scaling manager now sizes it.
kubectl annotate scaledobject <name> -n <namespace> autoscaling.keda.sh/paused-
```
<!-- guide:migrate.unfreeze end -->

## Verification

<!-- guide:verify.deployment start -->
```bash
# Check the controller and the ScaledObjects. Use the NAMESPACE you installed with.
make verify-deployment
make verify-scaledobjects
```
<!-- guide:verify.deployment end -->

<!-- guide:verify.rewire start -->
```bash
# A ScaledObject whose HPA shows only cpu is unwired. Re-wire it.
make scaledobjects-rewire
```
<!-- guide:verify.rewire end -->

Then check that decisions arrive, as in [Check KEDA received the
decision](../install-in-namespace/README.md#1-check-keda-received-the-decision).

## Cleanup

Remove what is left of the upstream install. If the upstream namespace held
nothing else, and the scaling manager does not run there, delete the namespace.

<!-- guide:cleanup.clusterrbac start -->
```bash
# Remove the upstream cluster-wide RBAC. These names have no suffix; this project's own all do.
kubectl delete clusterrolebinding wva-manager-rolebinding wva-metrics-auth-rolebinding \
  wva-metrics-reader-rolebinding wva-epp-metrics-reader-role-binding --ignore-not-found
kubectl delete clusterrole wva-manager-role wva-metrics-auth-role \
  wva-metrics-reader wva-epp-metrics-reader-role --ignore-not-found
```
<!-- guide:cleanup.clusterrbac end -->

- Skip it while another upstream install on the cluster still runs: they share
  these names.
- On OpenShift, upstream's bindings carry a suffix, the same one the scaling
  manager uses for its own. Delete them by hand only if the scaling manager does
  not run in the upstream namespace.

*Up to v0.8*:

<!-- guide:cleanup.crd start -->
```bash
# Releases up to v0.8 installed the VariantAutoscaling CRD. This project does not read it.
kubectl delete crd variantautoscalings.llmd.ai --ignore-not-found
```
<!-- guide:cleanup.crd end -->

## What stays the same

- **Object names**: the controller Deployment `wva-controller-manager`, its
  Service, ServiceMonitor and ServiceAccount, and the `wva-manager-config`
  ConfigMap. Cluster-wide RBAC gains a per-install suffix.
- **Metric names**: every `wva_` series upstream `v0.9.0` publishes, with the
  same labels on the replica gauges, except `wva_config_kv_spare_threshold`
  and `wva_config_queue_spare_threshold`, which are removed. `variant_name` is
  the ScaledObject's name. [All metrics](../../reference/prometheus.md).
- **Alerts**: the five upstream alerts keep their names.
  `DEPLOY_ALERTING_RULES=true` installs them, as before.
- **Defaults**: the cluster-scope controller namespace is still
  `workload-variant-autoscaler-system`.

## What is different

| | upstream `v0.9.0` | the scaling manager |
| --- | --- | --- |
| How a workload is registered | `llm-d.ai/managed` annotation on a ScaledObject or HPA | a ScaledObject `external-push` trigger ([trigger keys](../../reference/scaledobject-trigger.md)) |
| Actuation | KEDA `prometheus` trigger, or HPA through Prometheus Adapter | KEDA only |
| Install scope | cluster-wide on Kubernetes, one namespace on OpenShift | one namespace unless `SCOPE=cluster` ([which namespace is which](../../reference/configuration.md#which-namespace-is-which)) |
| Scaling-policy ConfigMap | `wva-saturation-scaling-config` | `wva-scaling-policy-config`. The old name is read only while the new one does not exist; the installer creates the new one |
| Turning on a limiter | `enableLimiter: true` | a `limiters:` entry |
| Prometheus TLS | not verified (`PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "true"`) | verified, except for a Prometheus the installer deploys ([TLS](../../reference/prometheus.md#tls-configuration)) |
| Scaler port | none | `:9090`, mutual TLS on a new install ([external scaler TLS](../../reference/configuration.md#external-scaler-mutual-tls)) |
| Image | `ghcr.io/llm-d/llm-d-workload-variant-autoscaler` | `ghcr.io/ev-shindin/llm-scaling-manager` |

New, and off until you turn it on: a [warm pool](../warm-pool/), [Kueue
borrowing](../kueue-borrowing/), the [utilization-share
optimizer](../utilization-share/), and [learned state across a
restart](../../reference/configuration.md#learned-state-across-a-restart).

## Next

- [After the install](../../reference/operations.md)
- [Upgrade and uninstall](../../reference/upgrade-and-uninstall.md)
- [Bound every install by real GPUs](../admin-gpu-bounding/)
