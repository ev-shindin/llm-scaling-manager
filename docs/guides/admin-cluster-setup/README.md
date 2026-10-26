# Cluster-admin setup for a namespace

## Overview

For one namespace that already runs llm-d, this creates what a namespace admin
cannot: the cluster-scoped RBAC and the ServiceMonitor. After that, the
namespace owner installs and upgrades the controller without you, until a
release changes what it needs cluster-wide.

Every permission it grants, with verbs and reasons, is in the
[security model](../../reference/security-model.md#permissions).

## Prerequisites

- [ ] Cluster-admin rights.
- [ ] llm-d model servers **already running** in the namespace. Both commands
  refuse an empty or missing namespace, and run the same checks as the
  [install guide's prerequisites](../install-in-namespace/#prerequisites).

<!-- guide:prerequisites start -->
```bash
# The namespace you are preparing. It must already run llm-d.
export NAMESPACE=team-a

# Read-only. Checks the namespace, monitoring and the EPP.
make check-prereqs
```
<!-- guide:prerequisites end -->

## Installation Instructions

**Choose the shape first.**

- **Shared cluster:** use the [out-of-reach shape](#keeping-the-controller-out-of-the-tenants-reach).
  The controller runs in a namespace you own, so the GPU bound holds against
  the namespace owner.
- **One team, or owners you trust:** use the tenant-owned shape below. The
  controller runs in the namespace it manages, so its owner can edit the
  controller and every bound it carries.

Tenant-owned shape:

<!-- guide:deploy.prereqs start -->
```bash
make setup-prereqs
```
<!-- guide:deploy.prereqs end -->

Run it once per namespace, and again when a release adds a prerequisite
([upgrading](../../reference/upgrade-and-uninstall.md#upgrading-from-before-v0100-rc1)).
Pass the same variables as the first run. Add `KEDA_NAMESPACE=<ns>` or
`PROMETHEUS_SA=<ns>/<sa>` when the command says it cannot find KEDA or your
Prometheus.

### What it creates

Rendered from `config/overlays/namespace-scoped/<platform>`. Cluster-scoped
names carry a hash of the controller's namespace, so two installs never share
them and an uninstall removes only its own.

| Object | Why it needs a cluster admin |
| --- | --- |
| The controller's namespace, when `WVA_NS` names a separate one | cluster-scoped |
| `wva-metrics-auth-role` + binding | the metrics endpoint issues TokenReview and SubjectAccessReview |
| `wva-metrics-reader` + binding | `nonResourceURLs: /metrics`, for the Prometheus that scrapes the controller ([who gets it](#who-gets-the-metrics-reader-binding)) |
| `wva-epp-metrics-reader-role` + binding | `/metrics`, for the EPP scrape |
| `wva-node-reader-role` + binding | resolving a variant's accelerator reads Nodes |
| `wva-kueue-reader-role` + binding | Kueue ClusterQueues and ResourceFlavors are cluster-scoped. Inert without Kueue. Not created for `WVA_LIMITER=none` when no policy the controller reads uses Kueue |
| `wva-policy-namespace-reader-role` + binding | `get` on named Namespaces, to find the cluster policy |
| `wva-manager-cluster-monitoring-view` | **OpenShift only**: lets the controller query Thanos |
| ServiceAccounts `wva-controller-manager`, `wva-epp-metrics-reader` and, on OpenShift, `wva-metrics-scraper` | referenced by the RBAC and the ServiceMonitor above |
| Secret `wva-epp-metrics-token` | a **non-expiring** token the controller uses to scrape the EPP. On OpenShift also `wva-metrics-scraper-token`, which the ServiceMonitor scrapes with; it grants only `get /metrics` |
| ConfigMap `wva-metrics-server-ca` | **OpenShift only**: the CA the ServiceMonitor checks the controller's certificate with |
| The ServiceMonitor | the stock `admin` ClusterRole does not grant `monitoring.coreos.com` |
| Roles `wva-manager-role`, `wva-leader-election-role`, `wva-utilization-share-history-role`, `wva-controller-status-role`, `wva-learned-state-role` + RoleBindings | RBAC forbids granting what you do not hold |
| RoleBinding `wva-policy-reader-<hash>` in `wva-policy` | only when `wva-policy` exists; lets the controller read the cluster policy |
| NetworkPolicy `wva-external-scaler` | admits the scaler port `:9090` from KEDA's namespace only ([when it is written](#the-scaler-networkpolicy)) |
| Prometheus | Kubernetes only, and only if the cluster has none |
| KEDA | Kubernetes only, with `KEDA_HELM_INSTALL=true`. On OpenShift, install the Custom Metrics Autoscaler operator first. Versions: [KEDA reference](../../reference/keda.md) |

Whoever can read Secrets in the controller's namespace holds
`wva-epp-metrics-token`. In the tenant-owned shape that is the tenant, and the
token can `get /metrics` on anything that authorizes `/metrics` through the API
server. One more reason for the out-of-reach shape.

### Conditional grants

A tenant can forward this table to their admin. Each grant matters only under
the condition in the second column.

| Grant | Needed when | Created by | Without it |
| --- | --- | --- | --- |
| `wva-node-reader-role` (nodes: get, list, watch) | the governing policy declares the `gpu-inventory` limiter | `make setup-prereqs` (namespace scope); `make enable-physical-limiter` | with `gpu-inventory`, the controller goes **NotReady**. With no limiter it runs degraded: accelerators unresolved |
| list on pods in **every** namespace | a namespace-scoped controller under the `gpu-inventory` limiter | **no command**: grant it by hand. `make setup-prereqs` and `make enable-physical-limiter` warn when it is missing | the limiter counts only the GPUs the controller's own namespace holds; GPUs other namespaces hold on the same nodes read as free. Reported as `wva_pod_access_denied` = 1 and `podListDenied` in `wva-controller-status` |
| `wva-kueue-reader-role` (clusterqueues, cohorts, resourceflavors: get, list; the managed Namespace: get) | a quota limiter entry has `kueue.enabled: true` | `make setup-prereqs`; `make enable-physical-limiter WVA_LIMITER_TYPE=quota WVA_QUOTAS='<type>=-1' WVA_QUOTA_KUEUE=true` | logs `external quota source unreadable` every cycle and uses the static quota alone. [Details](#the-kueue-grant) |
| `wva-kueue-borrow-reader-role` (Kueue workloads in **every** namespace: get, list, watch; localqueues: get, list) | the limiter policy the controller reads **counts cohort borrowing**: a Kueue quota entry whose `countBorrowable` is not `false`. Once the controller reads a policy namespace, that policy alone decides, never its own ConfigMap. Where none resolves, only your `WVA_LIMITER=quota` and `WVA_QUOTA_KUEUE=true` for that run grant it, never the tenant's ConfigMap alone; a run without `WVA_LIMITER` leaves it as it is | `make setup-prereqs` (namespace scope), with the install's `WVA_LIMITER`/`WVA_QUOTA_KUEUE`; `make enable-physical-limiter WVA_LIMITER_TYPE=quota WVA_QUOTA_KUEUE=true`. Both revoke it when no policy counts borrowing, and warn when a controller they grant it to shares a cohort with another borrower, from this run or an earlier one | borrowing is withheld (`lenders-unreadable`); the nominal Kueue cap applies. [Details](#the-kueue-grant) |
| Role `wva-kueue-controller-reader-<hash>` in the Kueue namespace (the `kueue-controller-manager` Deployment and its leader Lease, get by name) | a policy counts cohort borrowing | `make setup-prereqs` (namespace scope); `make enable-physical-limiter WVA_LIMITER_TYPE=quota WVA_QUOTA_KUEUE=true`. In `KUEUE_NAMESPACE`, else the entry's `kueue.controllerNamespace`, else `kueue-system` | borrowing freezes (`kueue-controller-down`) |
| RoleBinding to `wva-policy-reader` in the policy namespace | a policy namespace exists and the controller reads policy from it | `make setup-prereqs` (tenant-owned shape, when it exists); `make enable-physical-limiter` | the controller **refuses to start** rather than run unbounded |
| `wva-epp-metrics-auth` (tokenreviews, subjectaccessreviews: create), bound to the ServiceAccount each managed EPP runs as | the EPP's ServiceAccount may not create them already. llm-d's router chart grants it; an EPP installed another way may not | `make setup-prereqs` (`WVA_EPP_METRICS_AUTH=false` skips it); `make epp-metrics-auth` on its own, `DRY_RUN=true` to see the plan | the EPP answers **500** to every `/metrics` scrape, from the controller and from Prometheus. [Details](../../reference/troubleshooting.md#epp-metrics-returns-500) |
| `wva-policy-namespace-reader-role` (namespaces: get, by name) | **Kubernetes**, for a controller that manages its own namespace | `make setup-prereqs` (namespace scope); `make enable-physical-limiter`; or [by hand](#grant-the-namespace-read-by-hand) | the controller cannot see `wva-policy` and uses its own ConfigMap. It logs `Could not read the managed Namespace` and sets `wva_policy_namespace_unverified` to 1 (alert `WVAPolicyNamespaceUnverified`) |

`WVA_LIMITER_TYPE=quota` is required for the Kueue grant, because
`enable-physical-limiter` defaults to `gpu-inventory`. `WVA_QUOTAS='<type>=-1'`
lets Kueue decide the cap.

### Keeping the controller out of the tenant's reach

In the tenant-owned shape the namespace admin can edit the controller, and
nothing carried on the controller can bound the person who edits it. They also
effectively own its ServiceAccount, with cluster-wide node and Kueue read and the
EPP token. On a shared cluster, run the controller in a namespace you own:

<!-- guide:deploy.out_of_reach start -->
```bash
# Run the controller in wva-NAMESPACE, which you own, managing NAMESPACE.
make setup-prereqs   WVA_NS=wva-${NAMESPACE} WVA_WATCH_NS=${NAMESPACE}
make deploy-wva      WVA_NS=wva-${NAMESPACE} WVA_WATCH_NS=${NAMESPACE}
```
<!-- guide:deploy.out_of_reach end -->

- Pass `WVA_WATCH_NS` to **both** commands. `setup-prereqs` writes the
  controller's Role into the tenant's namespace. Without it the controller
  cannot read that namespace and crash-loops.
- The limiters come from the ConfigMap in `wva-${NAMESPACE}`, which you own.
  `wva-policy` does not apply. Re-running `make deploy-wva` without `WVA_LIMITER`
  keeps the limiter already declared there, and says so. Pass
  `WVA_LIMITER=gpu-inventory|quota` to replace it, or `WVA_LIMITER=none` to
  remove it ([gpu-limiter](../../reference/gpu-limiter.md)).
- The tenant can still create a `wva-scaling-policy-config` in **their**
  namespace. It sets thresholds, tiers and utilization-share weights for their
  models, never the limiters ([security model](../../reference/security-model.md#multi-tenancy)).

### Bound the namespace's GPUs at admission

The limiter bounds what the scaling manager *asks* for. A `ResourceQuota`
bounds what the cluster *admits*, whoever owns the controller. On a shared
cluster it is the real bound. Create it in either shape:

<!-- guide:deploy.gpu_quota start -->
```bash
# Cap the namespace's GPUs at admission. Size it to the tenant's entitlement.
kubectl -n ${NAMESPACE} create quota wva-gpus --hard=requests.nvidia.com/gpu=8
kubectl -n ${NAMESPACE} describe quota wva-gpus
```
<!-- guide:deploy.gpu_quota end -->

Use the resource name your device plugin advertises.

## Verification

Check for a cluster-scoped controller before handing over. The tenant's
`make deploy-wva` cannot list Deployments cluster-wide, so it only warns if one
exists, and two controllers on the same workloads both spend the same free GPUs.
`make check-prereqs` run as you checks this too.

<!-- guide:verify.no_cluster_install start -->
```bash
# List every controller. A line without --watch-namespace in ARGS is cluster-scoped.
kubectl get deploy -A -l app.kubernetes.io/name=workload-variant-autoscaler,control-plane=controller-manager \
  -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,ARGS:.spec.template.spec.containers[0].args
```
<!-- guide:verify.no_cluster_install end -->

<!-- guide:verify.objects start -->
```bash
# Show this install's ServiceMonitor and hashed cluster RBAC (shasum -a 256 on macOS).
kubectl get servicemonitor -n ${NAMESPACE}
kubectl get clusterrole,clusterrolebinding | grep "$(printf '%s' "${NAMESPACE}" | sha256sum | cut -c1-8)"
```
<!-- guide:verify.objects end -->

The hash is of the **controller's** namespace. For the out-of-reach shape, use
`wva-${NAMESPACE}` in both commands. Once the objects exist, the namespace owner
can run `make deploy-wva` without you.

## Cleanup

Name the namespace the **controller** runs in. Tenant-owned shape:

<!-- guide:cleanup.undo start -->
```bash
make undeploy-wva WVA_NS=${NAMESPACE}
```
<!-- guide:cleanup.undo end -->

Out-of-reach shape:

<!-- guide:cleanup.out_of_reach start -->
```bash
make undeploy-wva WVA_NS=wva-${NAMESPACE}
```
<!-- guide:cleanup.out_of_reach end -->

The namespace, Prometheus, KEDA and the EPP stay, and so does the
`wva-policy-reader-<hash>` RoleBinding in `wva-policy`. Run by you, the uninstall
removes the hashed ClusterRoles and bindings. Run by the tenant, it cannot, and
it lists them for you.

## Configuration

| Parameter | Default | Example |
| --- | --- | --- |
| `NAMESPACE` | none, **required** | `team-a` |
| `WVA_NS` | `NAMESPACE` (the controller runs in the namespace it manages) | `wva-team-a` |
| `WVA_WATCH_NS` | `WVA_NS` | `team-a` |
| `KEDA_NAMESPACE` | detected | `keda` |
| `PROMETHEUS_SA` | none | `monitoring/prometheus-k8s` |

## Details

### Who gets the metrics-reader binding

On Kubernetes the `wva-metrics-reader` binding goes only to a Prometheus you
named, in this order:

1. `PROMETHEUS_SA`, when set.
2. The single non-default `serviceAccountName` of the Prometheus CRs in the
   namespace a `PROMETHEUS_URL` you set points at.
3. The kube-prometheus-stack this install deploys (`DEPLOY_PROMETHEUS=true`).

A Prometheus the installer only detected never gets it. With none of these, no
binding is made, the installer warns, and `make check-prereqs` lists it as
optional. A re-run with none of them **keeps** an existing binding and names its
account. The tenant's `make deploy-wva` never touches it. On OpenShift it is
bound to the controller's own ServiceAccount.

### The scaler NetworkPolicy

`wva-external-scaler` is written only when KEDA's namespace is known. The
installer takes the first of: `KEDA_NAMESPACE`; the KEDA-served
`v1beta1.external.metrics.k8s.io` APIService; `KEDA_NAMESPACE`'s default with
`KEDA_HELM_INSTALL=true` while that APIService does not exist yet; an existing
policy, kept as it is. Otherwise it writes none, warns that `:9090` is open to
every Pod, and prints the `KEDA_NAMESPACE=<keda ns> ... make setup-prereqs`
command for the install's shape. Mutual TLS on the scaler is the alternative.

An existing policy that cannot be read, or that admits `:9090` from anything
but exactly one namespace, is left alone and reported `state UNKNOWN`. Check it
with `kubectl get networkpolicy wva-external-scaler -n <ns> -o yaml`.

### The Kueue grant

- The borrowing read, Kueue Workloads in every namespace, reaches every
  tenant's Workloads, pod templates included. So it is granted only while a
  policy of the install counts borrowing, and revoked on
  `WVA_QUOTA_KUEUE_BORROW=false`, on `make disable-limiter
  WVA_LIMITER_TYPE=quota` and on uninstall. A tenant who turns borrowing on
  later asks an admin to re-run `make setup-prereqs` with the same
  `WVA_LIMITER` and `WVA_QUOTA_KUEUE`; `make deploy-wva` warns until then. A
  cluster-scoped install always has the read, in its manager ClusterRole.
- It reads `get` on the managed Namespace by name. A cluster-scoped controller
  the installer made already reads every Namespace, so nothing needs naming in
  `WVA_LIMITER_TARGETS`. `enable-physical-limiter` warns only when a controller
  that looks cluster-scoped really lacks that read.
- A role created before the Namespace read was added withholds every
  ClusterQueue with a `namespaceSelector`, so Kueue bounds nothing until a
  cluster admin re-runs `make setup-prereqs`. The tenant's `make deploy-wva`
  refuses in that state, and warns when it cannot tell.

### The Namespace read

`wva-policy-namespace-reader-role` names the controller's own namespace, the
managed one, `wva-policy` and the namespace the managed one is labelled with.
`make setup-prereqs` also adds `WVA_POLICY_NS` when it is exported. No
controller reads policy there unless a label names it.

#### Grant the namespace read by hand

**Run `make setup-prereqs` instead, if you can.** For a namespace-scoped install
it creates this grant, and the `wva-policy` read whenever that namespace exists.
An install set up before the grant existed lacks it: re-run
`NAMESPACE=${NAMESPACE} make setup-prereqs`, then restart the controller, which
resolves its policy once at startup.

Use the manifest below only when something else manages your RBAC, only for the
tenant-owned shape on Kubernetes, and only if `wva-policy` exists and should
govern this controller. It holds **both** grants. The Namespace read alone is
worse than neither: the controller finds `wva-policy`, cannot read its
ConfigMap, and refuses to start. For a policy namespace named by a
`wva.llmd.ai/policy-namespace` label, add that name to `resourceNames` and put
the Role and RoleBinding there.

<!-- guide:deploy.namespace_read start -->
```bash
# Grant the Namespace read and the wva-policy read by hand, then restart the controller.
kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: wva-namespace-reader-${NAMESPACE}
rules:
- apiGroups: [""]
  resources: ["namespaces"]
  resourceNames: ["${NAMESPACE}", "wva-policy"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: wva-namespace-reader-${NAMESPACE}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: wva-namespace-reader-${NAMESPACE}
subjects:
- kind: ServiceAccount
  name: wva-controller-manager
  namespace: ${NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: wva-policy-reader
  namespace: wva-policy
rules:
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: wva-policy-reader-${NAMESPACE}
  namespace: wva-policy
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: wva-policy-reader
subjects:
- kind: ServiceAccount
  name: wva-controller-manager
  namespace: ${NAMESPACE}
EOF
kubectl -n ${NAMESPACE} rollout restart deploy/wva-controller-manager
```
<!-- guide:deploy.namespace_read end -->

Restart the controller Deployment only, not the warm pool. Then check: the new
pod logs `Cluster policy comes from the well-known namespace` (or `... the
namespace this one is labelled with`), no longer logs `Could not read the
managed Namespace`, and `wva_policy_namespace_unverified` reads 0.

`make undeploy-wva` does not know about this manual grant. Delete
`clusterrole,clusterrolebinding wva-namespace-reader-${NAMESPACE}` and
`rolebinding wva-policy-reader-${NAMESPACE} -n wva-policy` yourself. Leave the
Role `wva-policy-reader`: every controller that reads `wva-policy` shares it.

## Next

- [Bounding GPU usage](../admin-gpu-bounding/)
- [Security model](../../reference/security-model.md): every identity,
  permission and port, and what fails open
- [Install the scaling manager in a namespace](../install-in-namespace/): what
  the namespace owner does next
