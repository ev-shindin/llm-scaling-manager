# Coming from llm-d's workload-variant-autoscaler

## Overview

If you run llm-d's workload-variant-autoscaler (WVA), this guide moves your
cluster to the scaling manager (llm-scaling-manager). It was checked against
WVA's `v0.8.0` and `v0.9.0` tags.

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
- [ ] **`yq`** (v4), only if you need [Going back](#going-back).

Find the upstream install and what it scales:

<!-- guide:prerequisites start -->
```bash
# Find the upstream controller. ARGS without --watch-namespace means it manages every namespace.
kubectl get deploy -A -l app.kubernetes.io/name=workload-variant-autoscaler,control-plane=controller-manager -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,IMAGE:.spec.template.spec.containers[0].image,ARGS:.spec.template.spec.containers[0].args'

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

### 1. Save what you will change

<!-- guide:migrate.save start -->
```bash
# Save what step 3 removes and step 5 changes. Going back needs these files.
kubectl get configmap -n <upstream-namespace> -o yaml > upstream-configmaps.yaml
kubectl get deployment wva-controller-manager -n <upstream-namespace> -o yaml > upstream-controller.yaml
kubectl get scaledobject,hpa -A -o yaml > upstream-autoscalers.yaml
# Installed with Helm: save the release, its values and the chart version instead of the Deployment
#   helm get values <release> -n <upstream-namespace> > upstream-values.yaml
#   helm list -A | grep workload-variant-autoscaler > upstream-release.txt
#   helm list does not record where the chart came from: add its repo or path to upstream-release.txt
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
Keep `upstream-autoscalers.yaml` from step 1: adopting replaces each trigger, so
it is the only copy of the upstream ones.

### 3. Remove the upstream controller

<!-- guide:migrate.remove start -->
```bash
# Stop the upstream controller. Your workloads, KEDA and Prometheus stay.
kubectl delete deployment wva-controller-manager -n <upstream-namespace>
```
<!-- guide:migrate.remove end -->

*Up to v0.8*, installed with Helm. The uninstall also deletes the chart's
model-server Service and ServiceMonitor (`vllmService`, on by default), the
scrape the controller reads engine metrics from. Save them first:

<!-- guide:migrate.savescrape start -->
```bash
# Helm only. Save the chart's model-server Service and ServiceMonitor; the uninstall deletes them.
kubectl get service,servicemonitor -n <model-namespace> -l app.kubernetes.io/instance=<release> -o yaml > upstream-scrape.yaml
```
<!-- guide:migrate.savescrape end -->

<!-- guide:migrate.removehelm start -->
```bash
# Installed with Helm? Remove the release instead.
helm uninstall <release> -n <upstream-namespace>
```
<!-- guide:migrate.removehelm end -->

Then decide who scrapes the model server:

- **The pod template carries `llm-d.ai/role=decode` (or `prefill`) and names a
  container port `modelserver`**: let the chart's scrape go. `make
  setup-prereqs` in step 4 adds PodMonitor `wva-modelserver-metrics`, which
  selects exactly those pods and that port.
- **Otherwise**: re-create the chart's scrape now. Do not add the role label to
  the pod template later while it exists: the setup would add its own PodMonitor
  as well, and every series would be counted twice.

<!-- guide:migrate.restorescrape start -->
```bash
# Helm only, pods without the role label and modelserver port. Re-create the saved scrape; yq drops what the API server set.
yq '.items[] | del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.generation, .metadata.managedFields, .status, .spec.clusterIP, .spec.clusterIPs, .spec.ports[].nodePort) | split_doc' upstream-scrape.yaml | kubectl apply -f -
```
<!-- guide:migrate.restorescrape end -->

The chart's HPA (`hpa`, on by default) goes with the release too. The workload
keeps its replica count, and step 5 has no HPA to delete for it.

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

To run a cluster-wide install in the upstream namespace, name it:
`make deploy-wva SCOPE=cluster WVA_NS=<upstream-namespace>`. Then
`<controller-namespace>` below is `<upstream-namespace>`: pass
`SCOPE=cluster WVA_NS=<upstream-namespace>` to every `make` command; without
`WVA_NS` they look in `workload-variant-autoscaler-system`. A namespace-scope
install there also works:
`NAMESPACE=<model-namespace> WVA_NS=<upstream-namespace> WVA_WATCH_NS=<model-namespace>`.

Then carry over the settings you tuned, from `upstream-configmaps.yaml`:

<!-- guide:migrate.policy start -->
```bash
# Carry over the settings you tuned. Edit the new ConfigMap, not the old one.
kubectl edit configmap wva-scaling-policy-config -n <controller-namespace>
```
<!-- guide:migrate.policy end -->

- Thresholds (`scaleUpThreshold`, `scaleDownBoundary`, `kvCacheThreshold`,
  `queueLengthThreshold`), `analyzers` and per-model entries keep their keys.
- `enableLimiter: true` turns nothing on. Declare the limiter in the `default`
  entry's `limiters:` list ([limiters](../../reference/scaling-policy.md#limiters-cluster-default-only-live)).
- `kvSpareTrigger` and `queueSpareTrigger` are gone. Delete them.
- A key the scaling manager does not know is ignored and reported, not
  rejected.
- Re-running `make deploy-wva` resets the `default` entry. Keep lasting
  changes in a tier or a per-model entry ([re-running the installer resets the
  default entry](../../reference/scaling-policy.md#re-running-the-installer-resets-the-default-entry)).
- A namespace-local `wva-saturation-scaling-config` in the namespace the
  scaling manager now runs in is ignored, because the install creates
  `wva-scaling-policy-config` there. Merge its entries into the new one.

Settings outside that ConfigMap do not carry over:

| upstream | here |
| --- | --- |
| `PROMETHEUS_BASE_URL` in `wva-manager-config` | `PROMETHEUS_URL=<url>` on the install ([Prometheus](../../reference/configuration.md#pointing-at-an-existing-prometheus)) |
| `WVA_SCALE_TO_ZERO` in `wva-manager-config` (`"false"`) | `ENABLE_SCALE_TO_ZERO` on the install, default `true` |
| `GLOBAL_OPT_INTERVAL` in `wva-manager-config` | edit it in `wva-manager-config` after each install, then restart the controller ([what a change needs](../../reference/configuration.md#what-a-change-needs-a-restart-or-not)) |
| `wva-model-scale-to-zero-config` | not read. Put `scaleToZero:` on the model's entry ([scaleToZero](../../reference/scaling-policy.md#scaletozero)) |

### 5. Register the workloads

<!-- guide:migrate.plan start -->
```bash
# Write the plan. It applies nothing. Cluster-wide install: add SCOPE=cluster WVA_NS=<controller-namespace>.
make scaledobjects-plan WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```
<!-- guide:migrate.plan end -->

Edit `wva-plan.yaml` before you apply anything:

- **Upstream ScaledObjects** appear as `apply: no`, with a note naming the
  ScaledObject. Change it to `apply: adopt`. Adopt replaces the `prometheus`
  trigger with the scaler's trigger and keeps the polling interval, cooldown,
  `fallback` and `behavior` ([what adopt
  keeps](../../reference/configuration.md#default-scaledobjects)).
- **Set `minReplicas` on each adopt entry before you apply.** It is read from
  the ScaledObject, and one without `minReplicaCount` reads as `0`. This
  install allows scale to zero by default, so an idle model would be parked,
  and without the EPP's `flowControl` gate it never wakes. Set
  `minReplicas: 1` unless you want that ([scale to zero](../scale-to-zero/)),
  or install with `ENABLE_SCALE_TO_ZERO=false`.
- **Plain HPAs**: the workload's entry is already `apply: yes`, with the plan's
  bounds (1 and 10), not the HPA's: copy the HPA's `minReplicas` and
  `maxReplicas` into the entry. Then delete the HPA
  (`kubectl delete hpa <name> -n <namespace>`) before you run
  `scaledobjects-apply`. Two autoscalers on one workload fight over its replica
  count. Its new ScaledObject is named `<workload>-wva`, so its
  `variant_name` series change from the HPA's name to that.
- **A workload missing from the plan** has no llm-d serving label
  (`llm-d.ai/role=decode` or `prefill`, or `llm-d.ai/inferenceServing=true`).
  Upstream did not need one. Label the Deployment and plan again, or write its
  trigger by hand ([trigger keys](../../reference/scaledobject-trigger.md)).
  Leave it frozen until it is registered. The object label is for the plan
  only; the scrape needs the pod-template label (step 3).

<!-- guide:migrate.unlisted start -->
```bash
# A workload missing from the plan has no llm-d serving label. Label the Deployment itself (its pods do not restart), then plan again.
kubectl label deployment <name> -n <namespace> llm-d.ai/role=decode
```
<!-- guide:migrate.unlisted end -->

Then apply the edited plan:

<!-- guide:migrate.apply start -->
```bash
# Only after editing the plan and deleting the plain HPAs. Cluster-wide install: add SCOPE=cluster WVA_NS=<controller-namespace>.
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```
<!-- guide:migrate.apply end -->

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
# Check the controller and the ScaledObjects, with the NAMESPACE you installed with.
make verify-deployment
make verify-scaledobjects
# cluster-wide install:
#   make verify-deployment SCOPE=cluster WVA_NS=<controller-namespace>
#   make verify-scaledobjects SCOPE=cluster WVA_NS=<controller-namespace>
```
<!-- guide:verify.deployment end -->

<!-- guide:verify.rewire start -->
```bash
# A ScaledObject whose HPA shows only cpu is unwired. Re-wire it.
make scaledobjects-rewire
# cluster-wide install:
#   make scaledobjects-rewire SCOPE=cluster WVA_NS=<controller-namespace>
```
<!-- guide:verify.rewire end -->

Then check that decisions arrive, as in [Check KEDA received the
decision](../install-in-namespace/README.md#1-check-keda-received-the-decision).

## Going back

The same steps work before and after step 5. Skip the ones marked *after step 5*
if you have not registered anything yet.

1. *After step 5*: freeze the adopted ScaledObjects.

<!-- guide:rollback.freeze start -->
```bash
# Freeze the adopted ScaledObjects again, so nothing scales while the controllers change.
kubectl annotate scaledobject <name> -n <namespace> autoscaling.keda.sh/paused=true --overwrite
```
<!-- guide:rollback.freeze end -->

2. Remove the scaling manager. Do this first: the upstream image must never run
   on the scaling manager's `wva-manager-config`.

<!-- guide:rollback.undeploy start -->
```bash
# Remove the scaling manager, with the same SCOPE and NAMESPACE (cluster-wide, WVA_NS) you installed with. It also deletes the ScaledObjects it created.
make undeploy-wva NAMESPACE=<namespace>
```
<!-- guide:rollback.undeploy end -->

   Cluster-wide install: `make undeploy-wva SCOPE=cluster WVA_NS=<controller-namespace>`.

3. Restore the upstream controller, one of three ways:
   - **The scaling manager ran in another namespace**: apply the saved
     Deployment.
   - **It ran in the upstream namespace**: the uninstall deleted the whole
     same-named install there (ConfigMaps except
     `wva-saturation-scaling-config`, ServiceAccounts, the token Secret,
     Service, ServiceMonitor, the leader-election Role and RoleBinding).
     Reinstall upstream from its release, then put back the ConfigMaps you
     tuned.
   - **Helm**: `helm install` the same chart and version, under the same
     release name (all three are in `upstream-release.txt`), with
     `-f upstream-values.yaml`. The chart renders its ConfigMaps from the
     values only, so changes made with `kubectl edit` are lost; put those back
     with the Helm ConfigMap step below. The next `helm upgrade` renders them
     from the values again, so also fold those changes into
     `upstream-values.yaml`.

   The other-namespace option needs neither ConfigMap step: upstream's
   ConfigMaps were never touched.

<!-- guide:rollback.controller start -->
```bash
# Restore the upstream controller. yq drops the fields the API server sets.
yq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.generation, .metadata.managedFields, .status)' upstream-controller.yaml | kubectl apply -f -
```
<!-- guide:rollback.controller end -->

<!-- guide:rollback.configmaps start -->
```bash
# After a reinstall from the release, put back the ConfigMaps you tuned, then restart the controller. It reads wva-manager-config only at startup.
yq '.items[] | select(.metadata.name | test("^wva-")) | del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.generation, .metadata.managedFields, .status) | split_doc' upstream-configmaps.yaml | kubectl apply -f -
kubectl rollout restart deployment wva-controller-manager -n <upstream-namespace>
```
<!-- guide:rollback.configmaps end -->

<!-- guide:rollback.configmapshelm start -->
```bash
# Helm only. The chart names everything after the release. Put back the ConfigMaps you changed with kubectl edit, then restart the controller.
yq '.items[] | select(.metadata.labels."app.kubernetes.io/instance" == "<release>") | del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.generation, .metadata.managedFields, .status) | split_doc' upstream-configmaps.yaml | kubectl apply -f -
kubectl rollout restart deployment -n <upstream-namespace> -l app.kubernetes.io/instance=<release>,control-plane=controller-manager
```
<!-- guide:rollback.configmapshelm end -->

4. *After step 5*: restore the saved ScaledObjects and plain HPAs. `error: no
   objects passed to apply` means there was nothing to restore. The restore
   puts the `prometheus` trigger back; a `minReplicaCount` or `fallback` that
   adopting added stays.

<!-- guide:rollback.autoscalers start -->
```bash
# Restore the saved ScaledObjects and plain HPAs. HPAs that KEDA owns are skipped; KEDA recreates them.
yq '.items[] | select(.metadata.ownerReferences == null) | del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.generation, .metadata.managedFields, .status) | split_doc' upstream-autoscalers.yaml | kubectl apply -f -
```
<!-- guide:rollback.autoscalers end -->

5. Unfreeze each ScaledObject once `wva_desired_replicas` for it is back in
   Prometheus. Before that, its trigger reads `0`.
   (`kubectl annotate scaledobject <name> -n <namespace> autoscaling.keda.sh/paused-`)

Do this before the cleanup below, which deletes what the upstream controller
needs.

## Cleanup

Remove what is left of the upstream install. If the upstream namespace held
nothing else, and the scaling manager does not run there, delete the namespace.
Installed with Helm? `helm uninstall` already removed the chart's cluster-wide
RBAC, whose names carry the release; the names below are the release install's.

<!-- guide:cleanup.clusterrbac start -->
```bash
# Remove the upstream cluster-wide RBAC. These names have no suffix; this project's own all do.
kubectl delete clusterrolebinding wva-manager-rolebinding wva-metrics-auth-rolebinding \
  wva-metrics-reader-rolebinding wva-epp-metrics-reader-role-binding --ignore-not-found
kubectl delete clusterrole wva-manager-role wva-metrics-auth-role \
  wva-metrics-reader wva-epp-metrics-reader-role --ignore-not-found
```
<!-- guide:cleanup.clusterrbac end -->

Skip it while another upstream install on the cluster still runs: they share
these names.

On OpenShift, upstream's six bindings carry a suffix, the same one the scaling
manager uses for its own:

<!-- guide:cleanup.openshift start -->
```bash
# OpenShift only. Upstream suffixed its bindings with a hash of its namespace. List them; delete them only if the scaling manager does not run in that namespace.
SUFFIX=$(printf '%s' "<upstream-namespace>" | sha256sum | cut -c1-8)
kubectl get clusterrolebinding -o name | grep -- "-${SUFFIX}$"
```
<!-- guide:cleanup.openshift end -->

- **The scaling manager runs in the upstream namespace**: five of them are now
  its own; keep them. Its installer removes the sixth,
  `wva-prometheus-cluster-monitoring-view-<suffix>`, when it may.
- **It runs elsewhere**: delete all six with `kubectl delete clusterrolebinding <name>`.

*Up to v0.8*:

<!-- guide:cleanup.crd start -->
```bash
# Releases up to v0.8 installed the VariantAutoscaling CRD. This project does not read it.
kubectl delete crd variantautoscalings.llmd.ai --ignore-not-found
```
<!-- guide:cleanup.crd end -->

## What stays the same

- **Object names** (as in upstream's release install; the Helm chart prefixes
  them with the release name): the controller Deployment `wva-controller-manager`, its
  Service, ServiceMonitor and ServiceAccount, and the `wva-manager-config`
  ConfigMap. Cluster-wide RBAC gains a per-install suffix.
- **Metric names**: every `wva_` series upstream `v0.9.0` publishes, with the
  same labels on the replica gauges, except `wva_config_kv_spare_threshold`
  and `wva_config_queue_spare_threshold`, which are removed. `variant_name` is
  the ScaledObject's name, as before. [All metrics](../../reference/prometheus.md).
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
| Scale to zero | off (`WVA_SCALE_TO_ZERO: "false"`) | allowed (`ENABLE_SCALE_TO_ZERO=true`); a model parks only if its `minReplicaCount` is `0` |
| Prometheus TLS | not verified on Kubernetes (`PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "true"`); verified on OpenShift | verified, except for a Prometheus the installer deploys ([TLS](../../reference/prometheus.md#tls-configuration)) |
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
