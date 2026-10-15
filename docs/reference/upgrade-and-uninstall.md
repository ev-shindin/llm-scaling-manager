# Upgrade and uninstall

How to move the controller to a new version without surprises, what a restart
costs, and what an uninstall removes and leaves behind. `$NS` is the namespace
the controller runs in.

> Part of the [deployment guide](../../deploy/). Day-to-day checks are in
> [After the install](operations.md); symptoms in [Troubleshooting](troubleshooting.md).

## Pin the version

The default image is `ghcr.io/ev-shindin/llm-scaling-manager:main`. `:main` is a
**floating** tag, rebuilt on every push to `main`, and the installer pulls it
with `imagePullPolicy: Always`. So every pod restart (a node drain, an eviction,
a leader change) can start different code than the one you tested. Do not run
production on it.

Pin a release tag or a digest, and deploy with the manifests from the **same**
release: an image older than the manifests crash-loops on flags it does not
know.

```bash
git checkout v0.9.0
make deploy-wva NAMESPACE=<namespace> IMG_TAG=v0.9.0
# or a digest
make deploy-wva NAMESPACE=<namespace> IMG=ghcr.io/ev-shindin/llm-scaling-manager@sha256:<digest>
```

`IMG_TAG` sets the tag of the default `IMG`; `IMG` replaces the whole reference.
The pull policy follows the reference: `IfNotPresent` for a digest or a `vX.Y.Z`
tag, `Always` for anything else, so any tag that is not a semver release is
pulled again on every pod start (`WVA_IMAGE_PULL_POLICY` overrides it).
Confirm what runs:

```bash
kubectl get deploy wva-controller-manager -n "$NS" \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"  "}{.spec.template.spec.containers[0].imagePullPolicy}{"\n"}'
```

Pass every install-time variable you used the first time (`SCOPE`,
`ENVIRONMENT`, `WVA_LIMITER`, `DEPLOY_ALERTING_RULES`, ...) on every upgrade.
An upgrade re-renders the install from them: `DEPLOY_ALERTING_RULES` left at its
default `false`, for example, deletes the alert rules. The full list is in
[Configuration](configuration.md).

## Upgrade

1. **Back up your policy.** The install owns `wva-scaling-policy-config`; keep a
   copy of what you tuned
   ([Changing policy safely](operations.md#changing-policy-safely)).
2. **Check prerequisites** against the new release. It is read-only:
   ```bash
   make check-prereqs NAMESPACE=<namespace>
   ```
   A new release can need a kind or a permission the old one did not. If it
   reports missing prerequisites, a cluster admin runs, once:
   ```bash
   NAMESPACE=<namespace> make setup-prereqs        # (cluster admin)
   ```
   A missing **optional** prerequisite does not stop the install: the controller
   runs without the feature that needs it, and says so. For this release that
   is not enough: see [Upgrading to this release](#upgrading-to-this-release).
3. **Deploy** the pinned image, as above.
4. **Verify**:
   ```bash
   make verify-deployment
   make verify-scaledobjects
   ```

### Upgrading to this release

This release adds cluster-scoped grants and moves the external-scaler
NetworkPolicy into the admin phase. `make check-prereqs` reports the new grants
(`wva-policy-namespace-reader-*`, `wva-kueue-reader-*`,
`wva-controller-status-*`) as **optional**, so a tenant's upgrade goes ahead
without them, and the controller then runs without what they enable. Do this,
in order:

1. **A cluster admin re-runs `make setup-prereqs` first**, for every
   namespace-scoped install, with the variables that install used, before the
   namespace owner runs `make deploy-wva`:
   ```bash
   NAMESPACE=<namespace> make setup-prereqs        # (cluster admin)
   # out-of-reach shape
   make setup-prereqs WVA_NS=wva-<namespace> WVA_WATCH_NS=<namespace>
   ```
   Add `KEDA_NAMESPACE=<namespace KEDA runs in>` unless the installer can find
   KEDA itself (the namespace serving the `v1beta1.external.metrics.k8s.io`
   APIService when that Service is KEDA's, or an existing `wva-external-scaler`
   policy, which is kept). When it cannot,
   **no** NetworkPolicy is applied and the installer warns that `:9090` is
   reachable from every Pod (see [the scaler NetworkPolicy](#the-external-scaler-networkpolicy)).
2. **Policy may move.** After its next restart, a controller that manages the
   namespace it runs in can read Namespaces, and so finds a published cluster
   policy: the namespace its `wva.llmd.ai/policy-namespace` label names, else
   `wva-policy` if it exists. Its limiters and optimizer selection then come
   from there, and the
   `limiters:` in its own `wva-scaling-policy-config` **stop applying**.
   Thresholds, tiers and per-model entries still come from its own ConfigMap.
3. **`make enable-physical-limiter` and `make disable-physical-limiter` restart
   every namespace-scoped controller whose policy source is outside the
   namespace it manages**, on this run and every later one: a self-managed one
   reading `wva-policy` (or its labelled namespace), and an out-of-reach one
   reading its own ConfigMap in `wva-<namespace>`. Such a controller caches only
   the namespace it manages and reads any other ConfigMap at startup, so the plan
   the command prints marks them `WILL BE RESTARTED`. Run it when a restart is
   acceptable ([what a restart costs](#after-every-upgrade-or-restart)).
4. **What now fails closed**:
   - A Kueue ClusterQueue with a `namespaceSelector` grants a namespace its
     quota only when the selector admits it (no selector admits nothing, `{}`
     admits every namespace). If the controller cannot read the namespace's
     labels, that queue's grant is withheld, so the namespace is bounded at
     nothing by it.
   - With a Kueue quota source configured, `make deploy-wva` (the controller-only
     phase) **refuses** when the controller cannot `get` its managed Namespace,
     and names the `make setup-prereqs` run that fixes it.
   - A NetworkPolicy that admits a namespace KEDA does not run in drops every
     KEDA call: every ScaledObject naming this scaler goes not-Ready and nothing
     scales, while the controller reports healthy.
5. **Check**:
   - `wva_policy_namespace_unverified` is `0`. `1` means the controller could
     not read its Namespace and runs on its own ConfigMap
     (alert `WVAPolicyNamespaceUnverified`).
   - `make verify-deployment` prints no NetworkPolicy banner: neither the one
     that says the policy admits `:9090` only from a namespace KEDA does not run
     in, nor the one that says there is no policy and the port is open.
   - The [`wva-controller-status` ConfigMap](operations.md#what-the-controller-is-running-with)
     names the policy namespace, limiter and acceptance the controller runs with:
     ```bash
     kubectl get cm wva-controller-status -n "$NS" \
       -o jsonpath='{.data.policyNamespace} {.data.limiter} {.data.policyAccepted}{"\n"}'
     ```

#### Limiters and Kueue borrowing

What changes without a config change:

- **A policy that declares both `gpu-inventory` and `quota` is bounded by
  both.** Before, the quota alone applied and the physical entry was never
  built. This can be tighter where other workloads hold GPUs; for one release
  the log says `Upgrade note (transitional)` for each variant whose target it
  changed.
- **A type one limiter entry denies stays denied** when another entry lists
  it. Before, entries listing different types allowed the union (bug fix).
- **A P/D pair commits only what fits together.** Before, a budget of 8 with
  two 8-GPU roles ordered 16 (bug fix).
- **A Kueue-bounded namespace quota counts cohort borrowing**, on by default,
  where it is safe; the scaling manager may then lower a borrowing model to
  give capacity back. To keep the nominal cap, re-run the install with
  `WVA_QUOTA_KUEUE_BORROW=false`, or set `countBorrowable: false` under the
  entry's `kueue:` block. See
  [the Kueue path](../well-lit-paths/kueue-bounded-quotas/#borrowing).

New reads, for borrowing: `cohorts` and `workloads` (Kueue), and in the
managed namespace `resourcequotas`, `events`, and the owners of GPU pods
(`replicasets`, `statefulsets`, `jobs`, `leaderworkersets`). A cluster-scoped
install gets them with the new manager ClusterRole. **A namespace-scoped
install needs a cluster admin to re-run `make setup-prereqs`** (step 1
above); until then borrowing is withheld (`hierarchy-unknown`,
`resourcequota-unreadable`) and the nominal cap applies.

Installer:

- `WVA_LIMITER` takes a list: `WVA_LIMITER=gpu-inventory,quota`.
- `WVA_QUOTA_KUEUE_BORROW=false`, beside `WVA_QUOTA_KUEUE=true`, keeps the
  Kueue cap at nominal (`countBorrowable: false`).
- With `WVA_QUOTA_KUEUE=true` on a cluster-scoped install, a `gpu-inventory`
  entry is added beside the quota; `WVA_LIMITER_PHYSICAL=false` leaves it out.
- `make disable-physical-limiter` removes one kind only: the physical
  limiter, or the quota with `WVA_LIMITER_TYPE=quota`. The other stays.

### The external-scaler NetworkPolicy

`wva-external-scaler` admits `:9090` only from KEDA's namespace. It is written in
the admin phase (`make setup-prereqs`), and only when the installer **knows**
where KEDA runs: `KEDA_NAMESPACE` set explicitly, the namespace serving the
external-metrics APIService when that Service is KEDA's, `KEDA_NAMESPACE` with
`KEDA_HELM_INSTALL=true` when that APIService does not exist yet (this install
puts KEDA there), or the namespace an existing `wva-external-scaler` policy
already admits. Otherwise none is written, the installer warns that `:9090` is
open, and `make verify-deployment` prints `No NetworkPolicy: scaler port open`.
`make check-prereqs` and the controller-only phase (`INSTALL_PHASE=wva`) list
the policy as optional, and that phase with `WVA_SCALER_NETWORKPOLICY=false`
leaves an existing policy in place; only the admin phase removes it. A tenant
usually cannot read the APIService, so on an upgrade:

- Pass `KEDA_NAMESPACE=<namespace>` to `make setup-prereqs` whenever KEDA is not
  in a namespace the installer can find
  (`KEDA_NAMESPACE=<namespace> NAMESPACE=<ns> make setup-prereqs`). An existing policy is kept as it is, so
  a policy an earlier install pointed at the wrong namespace stays wrong until
  `KEDA_NAMESPACE` replaces it.
- Run `make verify-deployment` and read its NetworkPolicy lines. A namespace you
  cannot see KEDA from is reported as not confirmed, not as correct.
- Earlier versions of these docs had you apply a policy by hand, named
  `wva-external-scaler-from-keda`. NetworkPolicies add up, so it still admits
  whatever namespace it names to `:9090`, and the installer neither reads nor
  updates it. Delete it once the shipped one is in place:
  ```bash
  kubectl delete networkpolicy wva-external-scaler-from-keda -n "$NS" --ignore-not-found
  ```

## After every upgrade or restart

A new controller pod starts with an empty in-memory decision. What happens
meanwhile:

- **Until its first cycle completes** (one `GLOBAL_OPT_INTERVAL`, 15 s by
  default, plus leader election), the scaler answers KEDA's `GetMetrics` with
  each target's current replica count, so the HPA holds and KEDA sees no
  error. `wva_scaler_undecided_answers_total{reason="no-decision"}` grows
  during that window and stops once the first cycle publishes. While the old
  pod is gone and the new one is not yet serving, KEDA cannot reach the
  scaler at all: that is the outage the generated fallback covers, applied
  only once more than `failureThreshold` reads (3 on generated ScaledObjects,
  about one read every 15 s) have failed in a row, and it too holds the
  current count. A short rollout may log a `KEDAScalerFailed` Event or two
  and never reach the fallback. See [before the first
  decision](scaledobject-trigger.md#before-the-first-decision-and-the-fallback).
- **Under the utilization-share optimizer**, every planned model holds what it
  runs for one fill timeout, about three minutes with default timings, and
  shows the blocked reason `quiet-period`. Expect it on every upgrade and leader
  change.

Do not judge an upgrade by the first few minutes of decisions.

## Uninstall

Pass the **same** `ENVIRONMENT`, `SCOPE` and namespace you installed with; the
uninstall renders the same overlay and deletes what it renders.

```bash
make undeploy-wva NAMESPACE=<namespace>
```

If the controller was installed in a namespace of its own (`WVA_NS`), as in the
out-of-reach shape where it runs in `wva-<namespace>` and manages `<namespace>`,
pass both, or the uninstall looks for the controller in the wrong place:

```bash
make undeploy-wva NAMESPACE=<namespace> WVA_NS=wva-<namespace>
```

### What it removes

| object | note |
| --- | --- |
| the controller Deployment, its ServiceAccounts, Services and ServiceMonitor | |
| `wva-manager-config` and `wva-scaling-policy-config` | back up your policy first |
| the install's ClusterRoles and ClusterRoleBindings | suffixed per install, so other installs keep theirs. Cluster-scoped: a namespace owner gets Forbidden on these, and the uninstall says which are left for a cluster admin |
| the controller's Role and RoleBinding in a namespace it managed from outside (`WVA_WATCH_NS`) | |
| `wva-utilization-share-history` | |
| the ScaledObjects **this installer created** | KEDA restores each workload to its pre-autoscaling replica count as they go. Keep them with `UNDEPLOY_SCALEDOBJECTS=false` |

### What it leaves

| left behind | why | clean up |
| --- | --- | --- |
| the namespace `$NS` | it may hold your model servers | `kubectl delete namespace "$NS"` only if nothing else lives there |
| ScaledObjects you **adopted** (written by you or GitOps, pointed at this scaler) | not the installer's to delete. Their trigger now calls a scaler that is gone, and **a model parked at zero never wakes**: activation only comes from the scaler | see below |
| Prometheus, KEDA, the EPP | shared; this install may not have created them | `UNDEPLOY_SHARED=true` removes them too, for everything on the cluster |
| the PrometheusRule `controller-manager-alerts` | the uninstall does not render the alerts component | `kubectl delete prometheusrule controller-manager-alerts -n "$NS"` |
| the dashboard ConfigMap `wva-operation-dashboard` | shared by every install on the cluster | (cluster admin, if it is in a shared monitoring namespace) `kubectl delete configmap wva-operation-dashboard -n <dashboard-namespace>` |
| cluster policy in the policy namespace (`wva-policy`, or the one a `wva.llmd.ai/policy-namespace` label names): the ConfigMap, Role `wva-policy-reader` and this install's RoleBinding | written by `make enable-physical-limiter`, for every controller it granted | (cluster admin) see below |
| ClusterRoles and bindings from an install older than per-install suffixes | they may be bound by another install | (cluster admin) delete by hand once no install uses them |
| utilization-share transfer marks on pods | written while the optimizer acted | [Transfer marks left on pods](troubleshooting.md#transfer-marks-left-on-pods-after-a-downgrade); set `shadow: true` and wait a cycle **before** uninstalling to avoid them |

### Adopted ScaledObjects

The uninstall lists them. To find them later:

```bash
kubectl get scaledobject -n <ns> \
  -o custom-columns=NAME:.metadata.name,ADDR:.spec.triggers[0].metadata.scalerAddress \
  | grep "wva-external-scaler.$NS"
```

Then, for each:

- **Another install serves the model**: `make scaledobjects-repoint` rewrites
  `scalerAddress` on objects that name a namespace where no scaler runs.
- **Going back to what scaled it before**: change its trigger, in GitOps if it
  is managed there.
- **No autoscaling any more**: delete it, then set the replica count you want.
  KEDA restores the original count only when the object has
  `advanced.restoreToOriginalReplicaCount: true`; otherwise the workload stays
  where it is, which for a parked model is zero.
  ```bash
  kubectl delete scaledobject <name> -n <ns>
  kubectl scale deployment <name> -n <ns> --replicas=1
  ```

### Policy namespace and limiter leftovers (cluster admin)

The per-install suffix is the first eight hex digits of the SHA-256 of the
controller's namespace (`wva-<namespace>` in the out-of-reach shape). The policy
namespace is the `policyNamespace` in the controller's `wva-controller-status`
ConfigMap, else `wva-policy`. The uninstall removes that ConfigMap, so read it
first; afterwards, the managed namespace's `wva.llmd.ai/policy-namespace` label
names it, if it is set:

```bash
POLICY_NS=$(kubectl get cm wva-controller-status -n "$NS" \
  -o jsonpath='{.data.policyNamespace}' 2>/dev/null)
POLICY_NS="${POLICY_NS:-wva-policy}"
SUFFIX=$(printf '%s' "$NS" | sha256sum | cut -c1-8)
kubectl delete rolebinding "wva-policy-reader-$SUFFIX" -n "$POLICY_NS"
kubectl get clusterrole,clusterrolebinding | grep -- "-$SUFFIX"   # anything the uninstall could not remove
```

Only when no scaling-manager install remains on the cluster:

```bash
make disable-physical-limiter   # removes the physical limiter (WVA_LIMITER_TYPE=quota: the quota) from wva-policy and every policy ConfigMap a controller reads; the other kind stays
kubectl delete namespace "$POLICY_NS"
```

`disable-physical-limiter` leaves the read-only RBAC in place on purpose; the
namespace delete takes it with it.

If the uninstall ran as a namespace owner and reported Forbidden objects, a
cluster admin re-runs the same `make undeploy-wva` with the same variables.
