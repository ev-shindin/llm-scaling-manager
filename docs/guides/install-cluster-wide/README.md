# Install the scaling manager for the whole cluster

## Overview

This guide installs one scaling manager (llm-scaling-manager) that sizes the
llm-d model servers in **every** namespace. It decides how many replicas each
variant needs and hands that number to KEDA, which does the scaling.

Use it when one team runs the cluster. Where namespaces belong to different
teams, install [one controller per namespace](../install-in-namespace/) instead:
failure domains, policy and upgrades stay separate.

**This scope is work in progress**, and the install says so. It skips the
preflight's per-namespace checks, because the namespaces it will manage are
discovered and may not hold model servers yet.

## Prerequisites

- [ ] Cluster-admin rights.
- [ ] llm-d model servers somewhere on the cluster.
- [ ] **KEDA.** On Kubernetes, add `KEDA_HELM_INSTALL=true` to the deploy
  command to install it. On OpenShift, install the Custom Metrics Autoscaler
  operator first. KEDA 2.21 is recommended: older KEDA can leave a new
  ScaledObject [unwired](../../reference/keda.md#unwired-hpa-before-keda-220)
  after the controller restarts. Supported versions, and what happens with an
  existing KEDA: [KEDA reference](../../reference/keda.md).
- [ ] A Prometheus with the Prometheus Operator. Where nothing scrapes the
  model servers or the EPP yet, `make setup-prereqs` adds the monitors.
- [ ] **For scale-from-zero only: the `flowControl` feature gate on every
  EPP.** The check below finds every namespace with llm-d model servers or an
  InferencePool, and reports each one. Where the gate is off it warns and goes
  on. To turn it on, add this to the `EndpointPickerConfig` and restart the
  EPP:

  ```yaml
  featureGates:
  - flowControl
  ```

<!-- guide:prerequisites.check start -->
```bash
# Read-only. Reports what the install would need.
make check-prereqs SCOPE=cluster
```
<!-- guide:prerequisites.check end -->

No KEDA yet, and you will add `KEDA_HELM_INSTALL=true` to the install? Add it
to this check too. Without it the check stops at the missing KEDA.

## Installation Instructions

### 1. Install the scaling manager

<!-- guide:deploy.all start -->
```bash
make deploy-wva SCOPE=cluster
```
<!-- guide:deploy.all end -->

At this scope one person is both the cluster admin and the installer, so one
command runs [`setup-prereqs`](../admin-cluster-setup/) (cluster RBAC, the
namespace, the ServiceMonitor) and then installs the controller.

**Check the NetworkPolicy line it prints.** The scaler port `:9090` is limited
to KEDA's namespace only when the installer can find KEDA. If it warns that
`:9090` is open to every Pod, run the fix it prints:

```bash
# Limit the scaler port to KEDA's namespace
KEDA_NAMESPACE=<keda namespace> make setup-prereqs SCOPE=cluster WVA_NS=<controller namespace>
```

### 2. Register the workloads

Nothing scales until a ScaledObject exists. At this scope the plan covers every
namespace with model servers, so read it before applying it.

<!-- guide:deploy.register start -->
```bash
# Plan and apply ScaledObjects in every namespace with model servers. Keep SCOPE=cluster.
make scaledobjects-plan SCOPE=cluster WVA_DEFAULT_SO_PLAN=wva-plan.yaml
# edit wva-plan.yaml: apply: yes|no|adopt, the modelID, the replica bounds
make scaledobjects-apply SCOPE=cluster WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```
<!-- guide:deploy.register end -->

The plan has one entry per model server and applies nothing until you say so.
`apply: adopt` points an existing ScaledObject at the scaling manager instead of
adding a second. The file's own comments explain every field; there is an
example in [Install the scaling manager in a
namespace](../install-in-namespace/README.md#4-register-the-workloads).

Check the model servers are safe to scale with `make workload-patch`, again
with the scope:

```bash
# Report what the model servers need (drain hook, model cache), across the cluster
make workload-patch SCOPE=cluster
# or for one namespace
make workload-patch NAMESPACE=<ns>
```

See [Writing the
patch](../../reference/workload-preparation.md#writing-the-patch-make-workload-patch).

## Verification

<!-- guide:verify.objects start -->
```bash
kubectl get scaledobject,hpa -A
```
<!-- guide:verify.objects end -->

Look for **`READY True`** on the ScaledObject and a number in the HPA's
`TARGETS`. `1/1 (avg)` means one replica is the right size. Do not use
`CurrentMetrics` as the check ([why](#why-not-currentmetrics)).

`TARGETS` showing `<unknown>` means KEDA cannot reach the scaler. Usually the
ScaledObject names another install's namespace, for example after switching
between a namespace-scoped and a cluster-wide install. Repoint it:

<!-- guide:troubleshoot.repoint start -->
```bash
# Point ScaledObjects at the running install's scaler.
make scaledobjects-repoint
```
<!-- guide:troubleshoot.repoint end -->

`READY True` but `TARGETS` showing only `cpu` means KEDA's first call for the
metric failed and it does not retry. Re-wire it (`DRY_RUN=true` only lists
them):

```bash
make scaledobjects-rewire SCOPE=cluster
```

## Cleanup

<!-- guide:cleanup.uninstall start -->
```bash
make undeploy-wva SCOPE=cluster
```
<!-- guide:cleanup.uninstall end -->

**This touches every namespace:**

- **ScaledObjects the installer created are deleted** everywhere, and KEDA puts
  each workload back to its earlier replica count: a cluster-wide resize.
- **ScaledObjects you adopted are kept and listed.** They still call the removed
  scaler, and one parked at zero cannot wake. Repoint or delete them.

`UNDEPLOY_SCALEDOBJECTS=false` keeps the created ones too. Prometheus, KEDA and
the namespaces stay; `UNDEPLOY_SHARED=true` removes them as well. Model servers
keep serving in every namespace. If a namespace's model came from [Install a
small llm-d model](../install-small-model/), remove it per namespace:

<!-- guide:cleanup.model start -->
```bash
make benchmark-teardown BENCHMARK_NAMESPACE=<the model namespace>
```
<!-- guide:cleanup.model end -->

Full detail: [Upgrade and uninstall](../../reference/upgrade-and-uninstall.md).

## Configuration

| Parameter | Default | Example |
| --- | --- | --- |
| `SCOPE` | `namespace`. **Set `cluster` for this guide**, on every command | `cluster` |
| `WVA_NS` | `workload-variant-autoscaler-system`. Where the controller runs; differs from `NAMESPACE` at this scope | `wva-system` |
| `KEDA_NAMESPACE` | detected. Without it, no NetworkPolicy is written | `keda` |
| `KEDA_HELM_INSTALL` | `false` | `true` |
| `IMG` | the image built from main, a **floating** tag ([pin the version](../../reference/upgrade-and-uninstall.md#pin-the-version)) | `ghcr.io/you/wva:dev` |

A cluster-scoped install can read every ConfigMap and patch every pod in the
cluster; every permission is in the [security
model](../../reference/security-model.md#cluster-scoped-both-platforms). Full
list of settings: [Configuration reference](../../reference/configuration.md).

## Details

- **Why `SCOPE=cluster` on every command.** `SCOPE` defaults to `namespace`
  everywhere in this repo. Without it, `scaledobjects-plan` and
  `workload-patch` scan only the controller's own namespace, find nothing, and
  report `No model servers found in scope` or "Deploy them first".
  With `WVA_WORKLOAD_PATCH_APPLY=true`, `SCOPE=cluster` means a rolling restart
  of every model server on the cluster.
- **Why `flowControl`.** At zero replicas there are no model-server metrics, so
  the router queue the gate publishes is the only sign anyone wants the model.
  Without it scale-from-zero never fires. While running, flow control admits only
  what an engine can absorb, so `vllm:num_requests_waiting` can read 0 while
  requests wait at the router.
- **Why the NetworkPolicy matters.** Without it any Pod can call the scaler on
  `:9090`. How the namespace is found: [the scaler
  NetworkPolicy](../admin-cluster-setup/README.md#the-scaler-networkpolicy).

### Why not `CurrentMetrics`

On a ScaledObject whose trigger the scaling manager never answers, it still
shows an empty entry, `[{"type":""}]`, while `READY` is `False`, `TARGETS`
reads `<unknown>/1 (avg)` and nothing scales.

## Next

- `make benchmark-smoke NAMESPACE=<ns>`: drive load at one managed namespace
  and snapshot the dashboard
- [Bound every install by real GPUs](../admin-gpu-bounding/)
- [Share GPUs between models in different
  namespaces](../utilization-share-cluster-wide/): one budget across
  namespaces, with the utilization-share optimizer
- `deploy/warmpool.sh plan -n <ns>`: read-only check of whether a namespace has
  models that could share a [warm pool](../warm-pool/)
- [After the install](../../reference/operations.md)
- [Install methods](../../reference/install-methods.md): GitOps, direct
  Kustomize, and what the script does
