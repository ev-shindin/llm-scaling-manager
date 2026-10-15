# Install the scaling manager in a namespace

## Overview

This guide installs the scaling manager (llm-scaling-manager) into one namespace.
It sizes the llm-d model servers in that namespace and nothing else. It decides
how many replicas each variant needs and hands that number to KEDA, which does
the scaling.

A cluster admin runs one command once ([Cluster-admin setup](../admin-cluster-setup/)).
After that, the namespace owner installs and upgrades the controller.

## Prerequisites

- [ ] **llm-d model servers in the namespace.** The install refuses an empty
  namespace. No model yet? [Install a small llm-d model](../install-small-model/).
- [ ] **KEDA.** On OpenShift, install the Custom Metrics Autoscaler operator. On
  Kubernetes, `KEDA_HELM_INSTALL=true make setup-prereqs` installs it if missing.
- [ ] **A Prometheus** that scrapes the model servers and their EPP.
- [ ] **The EPP's `flowControl` feature gate.** Add `featureGates: [flowControl]`
  to the `EndpointPickerConfig` and restart the EPP. The install refuses without it.
- [ ] **A cluster admin has run step 1**, or you are a cluster admin.

<!-- guide:prerequisites start -->
```bash
# The namespace your model servers run in.
export NAMESPACE=<your-namespace>

# Read-only. Reports the namespace, its model servers and the Prometheus it found.
make check-prereqs
```
<!-- guide:prerequisites end -->

If `check-prereqs` says the namespace is empty or missing, fix `NAMESPACE`
before going on. Each refusal and its fix is in [First-line
troubleshooting](../../reference/operations.md#first-line-troubleshooting).
Step 3 checks two more things the model servers need: a model cache and a drain
hook.

## Installation Instructions

### 1. Cluster admin: prepare the namespace

**Needs cluster-admin rights.** Nothing after this step does.

<!-- guide:deploy.prereqs start -->
```bash
make setup-prereqs
```
<!-- guide:deploy.prereqs end -->

Run it once per namespace, and again when a release adds a prerequisite
([upgrading](../../reference/upgrade-and-uninstall.md#upgrading-to-this-release)).
Your namespace must already exist. Two optional settings:

- `KEDA_NAMESPACE=<ns>` when the command says it cannot find KEDA. Without it no
  NetworkPolicy is written and the scaler port `:9090` is open to every Pod.
- `PROMETHEUS_SA=<namespace>/<serviceaccount>` to let your Prometheus scrape the
  controller's `/metrics`.

If KEDA is missing it stops and tells you what to do. What it creates, and why
each object needs an admin, is in [Cluster-admin setup](../admin-cluster-setup/).

On a **shared cluster** your admin should run the controller
[out of your reach](../admin-cluster-setup/README.md#keeping-the-controller-out-of-the-tenants-reach),
in a namespace they own (for example `wva-${NAMESPACE}`). Then the admin does
steps 1 and 2, and you start at step 3.

### 2. Install the controller

<!-- guide:deploy.controller start -->
```bash
make deploy-wva
```
<!-- guide:deploy.controller end -->

It works out what is left to do and says what it chose. Run by you after step 1,
it installs the controller. Run by a cluster admin before step 1, it does both
steps. If step 1 is missing and you cannot do it, it stops and names what an
admin must create.

### What you own, and what your admin owns

| What | Owner | Notes |
| --- | --- | --- |
| The controller Deployment, its Services, `wva-manager-config` | you | install and upgrade with namespace rights |
| `wva-scaling-policy-config` in your namespace | you | the controller's **global** map: its `default` entry applies to every model. You set thresholds, tiers, per-model entries, and the limiters unless a [cluster policy](#is-a-cluster-policy-in-force) governs the controller |
| ServiceAccounts, Roles, RoleBindings, the ServiceMonitor, token Secrets | your admin | created by step 1 in your namespace |
| Hashed ClusterRoles and ClusterRoleBindings | your admin | cluster-scoped |
| The cluster policy namespace (`wva-policy`) | your admin | holds a GPU budget when the admin publishes one |
| **Out-of-reach shape:** everything above except your policy map | your admin | the controller runs in `wva-${NAMESPACE}`. There is no `wva-scaling-policy-config` in your namespace until you create one. If you do, it [replaces the admin's map whole](../../reference/scaling-policy.md#namespace-local-scaling-policy-map) for your models, so its `default` must carry every threshold. It cannot set `limiters:` or `optimizer:` |

#### Is a cluster policy in force?

The controller publishes the answer in its `wva-controller-status` ConfigMap.
Use `-n` for the namespace the **controller** runs in: `${NAMESPACE}` here, or
`wva-${NAMESPACE}` in the out-of-reach shape.

```bash
# Print the policy namespace and the limiter in force
kubectl get cm wva-controller-status -n ${NAMESPACE} \
  -o jsonpath='{.data.policyNamespace} {.data.limiter}{"\n"}'
```

- `policyNamespace` is the controller's own namespace: your ConfigMap's
  `limiters:` are in force.
- Any other value (such as `wva-policy`): the limiters come from there and yours
  are ignored. `limiter` names those in force: their types joined by `+`, then
  every entry's name, such as `quota/install-quota` or
  `inventory+quota/gpu-inventory,install-quota`.
- **Forbidden** (normal in the out-of-reach shape): you cannot read the
  controller's namespace. Ask your admin, or read the Events on your
  ScaledObjects. A limiter holding a scale-up shows as `ResourceConstrained`:

  ```bash
  # Scaling Events the controller recorded on your ScaledObjects
  kubectl get events -n ${NAMESPACE} --field-selector involvedObject.kind=ScaledObject
  ```

- **NotFound**: the controller is older than this ConfigMap. See
  [Details](#reading-the-policy-from-the-controller-log).

### 3. Make the workloads safe to scale

<!-- guide:deploy.readiness start -->
```bash
# Report what the model servers still need. Changes nothing.
make workload-patch NAMESPACE=${NAMESPACE}
```
<!-- guide:deploy.readiness end -->

It checks two things:

- **A drain hook (preStop).** Without one, every scale-down cuts the responses a
  replica is still writing. llm-d does not set one.
- **A model cache.** Without one, every scale-up downloads the weights again.
  llm-d mounts one by default.

Both live in the pod spec, which your modelservice chart owns, so it writes a
patch file rather than changing your workloads. See [Writing the
patch](../../reference/workload-preparation.md#writing-the-patch-make-workload-patch),
including `make model-cache` when the cache is missing.

### 4. Register the workloads

Nothing scales until a ScaledObject exists.

<!-- guide:deploy.register start -->
```bash
make scaledobjects-plan WVA_DEFAULT_SO_PLAN=wva-plan.yaml
# edit wva-plan.yaml: apply: yes|no|adopt, the modelID, the replica bounds
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```
<!-- guide:deploy.register end -->

In the out-of-reach shape, `scaledobjects-apply` finds the admin's controller
from the RoleBinding step 1 wrote in your namespace. If it says it cannot, name
it: add `WVA_NS=wva-${NAMESPACE} WVA_WATCH_NS=${NAMESPACE}` to both commands.

`scaledobjects-plan` writes one entry per model server and applies nothing.
Lines starting with `#` are information only and are never read back:

```yaml
plan:

  - apply: "yes"  # yes | no
    namespace: llm-d-sim
    kind: Deployment
    name: dev-model-decode
    modelID: "meta/llama"
    minReplicas: 1
    maxReplicas: 10
    variantCost: "10.0"
    # scalingPolicy: "standard"
    # inferencePool: optimized-baseline
```

Edit it, then `scaledobjects-apply` does exactly what the file says. The
comments at the top of the file explain every field.

- `apply: adopt` appears only when something already scales the workload. It
  points that ScaledObject at the scaling manager instead of adding a second one.
- A workload whose model could not be read is written as `apply: "no"` with a
  `# note:` saying why.
- Below the entries, a `warmPools:` section suggests [warm pools](../warm-pool/)
  for workloads that share an accelerator. All are written `apply: "no"`, because
  a pool holds its GPUs for as long as it exists.

> **Running Fast Model Actuation?** See [the FMA
> post-mortem](../../proposals/fma-post-mortem.md) for the extra plan entry.

## Verification

### 1. Check KEDA received the decision

<!-- guide:verify.objects start -->
```bash
kubectl get scaledobject,hpa -n ${NAMESPACE}
```
<!-- guide:verify.objects end -->

```text
NAME                                        SCALETARGETKIND      SCALETARGETNAME    MIN  MAX  READY  ACTIVE  TRIGGERS
scaledobject.keda.sh/dev-model-decode-wva   apps/v1.Deployment   dev-model-decode   1    10   True   True    external-push

NAME                                                  REFERENCE                     TARGETS     MINPODS  MAXPODS  REPLICAS
horizontalpodautoscaler.autoscaling/keda-hpa-dev-...  Deployment/dev-model-decode   1/1 (avg)   1        10       1
```

- **`READY True`**: KEDA reached the scaling manager.
- **`TARGETS` shows a number**: decisions are flowing. `1/1 (avg)` means one
  replica is the right size.
- **`ACTIVE True`**: there is traffic. `Unknown` with no traffic is normal.

**`TARGETS` reads `cpu: <unknown>/80%`** (with `READY False`): KEDA cannot reach
the scaling manager, so nothing scales this workload. Usually the ScaledObject
names a scaler that is not there, such as a shipped sample naming the default
namespace. Point it at the running install:

```bash
# Point ScaledObjects at the running install's scaler
make scaledobjects-repoint
```

It changes `scalerAddress` only. In the out-of-reach shape, run
`make scaledobjects-repoint WVA_NS=wva-${NAMESPACE}`. If your token cannot read
that namespace, it warns and repoints as given. A ScaledObject that names a
namespace it cannot check is left alone; ask your admin to run the repoint, or
re-apply that plan entry with `WVA_NS=wva-${NAMESPACE}`. Other causes: [First-line
troubleshooting](../../reference/operations.md#first-line-troubleshooting).

### 2. Read the decisions

**Wait a few minutes first.** The scaling manager decides on a timer, so a new
ScaledObject has no decisions yet. An empty result straight after
`scaledobjects-apply` is normal.

<!-- guide:verify.decisions start -->
```bash
# Needs read access to the controller's logs.
kubectl logs -n ${NAMESPACE} deploy/wva-controller-manager --tail=-1 | grep scaling-decision
```
<!-- guide:verify.decisions end -->

```text
scaling-decision {"modelID":"meta/llama","decisions":[{"name":"llama-decode-wva","curr":1,"tgt":3,"action":"scale-up"}]}
```

The log is the admin's in the out-of-reach shape. Without log access, watch the
`TARGETS` column of `kubectl get hpa -n ${NAMESPACE}` change, and read
`ScaledUp` / `ScaledDown` Events with the `kubectl get events` command
[above](#is-a-cluster-policy-in-force).

### 3. (Optional) See it in a dashboard

<!-- guide:visualize.dashboard start -->
```bash
make dashboard
```
<!-- guide:visualize.dashboard end -->

**OpenShift only**, and it needs grafana-operator's CRDs (a cluster-admin
install). It creates a Grafana private to this namespace with the operational
dashboard, and prints the URL and admin password on every run. On Kubernetes,
import `deploy/grafana/operational-dashboard.json` into your own Grafana. See
[Details](#the-dashboard) for what it creates.

### 4. (Optional) Drive some load

<!-- guide:visualize.smoke start -->
```bash
make benchmark-smoke NAMESPACE=${NAMESPACE}
```
<!-- guide:visualize.smoke end -->

Five minutes of 1024-in/1024-out requests at 15 req/s, then a dashboard
snapshot of that window. It answers one question: does the scaling manager scale
this workload? It needs a GPU cluster and is not a benchmark; for that, see
[Benchmark the scaling manager](../benchmarking/).

## Cleanup

<!-- guide:cleanup.uninstall start -->
```bash
make undeploy-wva
```
<!-- guide:cleanup.uninstall end -->

It removes the ScaledObjects it created, and KEDA returns each workload to its
earlier replica count. ScaledObjects you **adopted** are left and listed by name.
Repoint or remove them yourself: their trigger now calls a scaler that is gone,
and a workload parked at zero can never wake. `UNDEPLOY_SCALEDOBJECTS=false`
keeps every ScaledObject, for reinstalling in place.

The model servers, EPP and InferencePools keep running. A drain hook you applied
and a model cache from `make model-cache` also stay. If you deployed the model
with [Install a small llm-d model](../install-small-model/), remove it **after**
`make undeploy-wva`:

<!-- guide:cleanup.model start -->
```bash
make benchmark-teardown BENCHMARK_NAMESPACE=${NAMESPACE}
```
<!-- guide:cleanup.model end -->

### What an uninstall run by you leaves behind

You cannot delete cluster-scoped RBAC. `make undeploy-wva` removes everything
you may delete, then lists the `Forbidden` objects for a cluster admin. Deleting
the namespace leaves them too. Send your admin this:

```bash
# Run by a cluster admin: list, then remove, this install's hashed cluster RBAC
SUFFIX=$(printf '%s' "<the namespace>" | sha256sum | cut -c1-8)   # shasum -a 256 on macOS
kubectl get clusterrole,clusterrolebinding -o name | grep -- "-${SUFFIX}$"
NAMESPACE=<the namespace> make undeploy-wva
```

## Configuration

`NAMESPACE` is required. Everything else is detected; set a value only to
override what `check-prereqs` reported.

| Parameter | Default | Example |
| --- | --- | --- |
| `NAMESPACE` | none, **required** | `llm-d-optimized-baseline` |
| `WVA_NS` | `NAMESPACE`. Where the **controller** runs. It differs from `NAMESPACE` in the cluster-wide and out-of-reach shapes | `wva-llm-d-optimized-baseline` |
| `WVA_WATCH_NS` | `WVA_NS`. The namespace a controller in `WVA_NS` manages (out-of-reach shape) | `llm-d-optimized-baseline` |
| `IMG` | the image built from main, a **floating** tag ([pin the version](../../reference/upgrade-and-uninstall.md#pin-the-version)) | `ghcr.io/you/wva:dev` |
| `PROMETHEUS_URL` | detected; Thanos on OpenShift | `https://prom.monitoring.svc:9090` |
| `KEDA_HELM_INSTALL` | `false` on Kubernetes | `true` |
| `KEDA_NAMESPACE` | detected (admin step) | `keda` |
| `PROMETHEUS_SA` | none (admin step) | `monitoring/prometheus-k8s` |
| `SKIP_CHECKS` | `false`. `true` turns refusals into warnings ([what it does not skip](../../reference/configuration.md#deployment-flags)) | `true` |

Full list: [Configuration reference](../../reference/configuration.md).

## Details

### Why the prerequisites matter

- **No model servers:** the install refuses rather than leave a healthy
  controller with nothing to scale. A controller pointed at the wrong namespace
  installs cleanly, reports healthy and scales nothing.
- **`flowControl`:** it publishes the router queue the scaling manager reads as
  demand. It is the only signal at zero replicas. EPPs from the llm-d guides have
  it off.
- **Model cache:** the cache removes the download, not the load. See [Weights and
  the model cache](../../reference/workload-preparation.md#weights-and-the-model-cache).
  `scaledobjects-plan` also flags a workload without one.

### Step 1 settings

- `KEDA_NAMESPACE`: found from the KEDA-served external-metrics APIService; else,
  with `KEDA_HELM_INSTALL=true` and that APIService NotFound, the namespace the
  install puts KEDA in; else an existing scaler NetworkPolicy. With none of
  these, no NetworkPolicy is written.
- `PROMETHEUS_SA`: the `metrics-reader` binding goes only to a Prometheus you
  name. A re-run without it keeps the binding an earlier run made.
- Step 1 also creates the controller's namespace when `WVA_NS` names one apart
  from yours.

### Reading the policy from the controller log

When `wva-controller-status` is NotFound, read the startup log. It needs
`pods/log` in the controller's namespace, so in the out-of-reach shape it is
your admin's check.

```bash
# Startup lines that say where cluster policy comes from
kubectl logs -n ${NAMESPACE} deploy/wva-controller-manager --tail=-1 \
  | grep -E 'Cluster policy comes from|Could not read the managed Namespace|Cannot tell whether'
```

- `Cluster policy comes from this controller's own namespace`: your limiters are
  in force. (It logs at V(1), which the default `-v=2` shows.)
- `Cluster policy comes from the namespace this one is labelled with` or `...from
  the well-known namespace`: the admin's limiters are in force.
- `Could not read the managed Namespace` or `Cannot tell whether a cluster admin
  published policy`: the controller lacks `get` on Namespaces, so it cannot see a
  published `wva-policy`. It also sets `wva_policy_namespace_unverified` to 1.
  A cluster admin re-runs `NAMESPACE=${NAMESPACE} make setup-prereqs`, then the
  controller restarts.

Every permission and its reason is in the [security
model](../../reference/security-model.md).

### Adopt and repoint

- `apply: adopt` exists because two ScaledObjects on one target are two HPAs
  writing the same replica count.
- `scaledobjects-repoint` finds the running install itself. It leaves `modelID`,
  `variantCost` and the bounds alone, and is safe to re-run. It never repoints
  a ScaledObject that names another live install.

### The dashboard

`make dashboard` reads Thanos through the platform's tenancy port and publishes
through a Route; neither exists on plain Kubernetes, so it checks and says so.
It reads Thanos through a namespaced role, with no cluster-scoped grant. It
refuses a namespace with no model servers, because the dashboard pins its
namespace variable to it. Its datasource ServiceAccount,
`wva-grafana-datasource`, gets a **non-expiring** token Secret, `view` on the
namespace and `create` on `metrics.k8s.io` pods. Anyone who can read Secrets in
the namespace holds that token; see the [security
model](../../reference/security-model.md#grants-made-after-install).

## Next

- [After the install](../../reference/operations.md)
- [Bound every install by real GPUs](../admin-gpu-bounding/). Without it, only
  each workload's `maxReplicaCount` bounds scaling.
- [Bridge a scale-up with a warm pool](../warm-pool/)
- [Scaling policy](../../reference/scaling-policy.md)
