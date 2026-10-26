# Security model

What the scaling manager is trusted with, what it reads and writes, what it
exposes on the network, and which failures leave scaling bounded or unbounded.
For three readers: the **cluster admin** who grants its permissions, the
**namespace tenant** who runs it with namespace rights only, and the **security
reviewer** who signs off on both.

It is taken from the rendered overlays in `config/overlays/`, the installer in
`deploy/lib/`, and the controller source. Known gaps are named, with the
workaround.

## Trust boundaries

| Boundary | Who is on each side | What crosses it |
| --- | --- | --- |
| Kubernetes API | controller ServiceAccount ↔ API server | reads of pods, workloads, nodes, ConfigMaps, InferencePools, ScaledObjects, Kueue queues; pod patches, Events, Leases, two ConfigMaps of its own |
| KEDA → controller | KEDA operator ↔ gRPC on `:9090` | scale target names, trigger metadata (`modelID`, `variantCost`, ...); replica counts back |
| Prometheus → controller | the scraping Prometheus ↔ `:8443` | the controller's own `wva_*` metrics |
| Controller → Prometheus / Thanos | controller ↔ your Prometheus | PromQL queries, carrying the controller's own ServiceAccount token |
| Controller → EPP and model-server pods | controller ↔ pod `/metrics` | metric scrapes, with the `epp-metrics` token |
| Controller → warm-pool pods (only with a warm pool) | controller ↔ the pool's supervisor and engine ports | plain-HTTP commands to start, sleep, wake and stop engine instances |
| Cluster admin ↔ tenant | whoever owns the controller's Deployment ↔ whoever is bounded by it | the scaling policy, and the GPU budget in it |

The last row decides whether a GPU bound holds. Whoever can edit the
controller's Deployment owns its image, args and env, so **nothing carried on
the controller can bound the person who can edit it**. See
[Multi-tenancy](#multi-tenancy).

## Identities

| Identity | Created by | Used for |
| --- | --- | --- |
| ServiceAccount `wva-controller-manager` | the prereqs phase (`make setup-prereqs`) | everything the controller does against the API server, and its queries to Prometheus (`PROMETHEUS_TOKEN_PATH` points at this SA's projected token) |
| ServiceAccount `wva-epp-metrics-reader` | the prereqs phase | scraping EPP `/metrics`. Bound cluster-wide to `get` on the non-resource URL `/metrics` |
| Secret `wva-epp-metrics-token` | the prereqs phase | a **legacy, non-expiring** `kubernetes.io/service-account-token` for `wva-epp-metrics-reader`, mounted into the controller at `/var/run/secrets/epp-metrics` |
| ServiceAccount `wva-metrics-scraper` | the prereqs phase, **OpenShift only** | the identity user-workload Prometheus scrapes the controller's `:8443` with. Its only grant is `get` on `/metrics` (the repointed `metrics-reader`) |
| Secret `wva-metrics-scraper-token` | the prereqs phase, **OpenShift only** | a **legacy, non-expiring** token for `wva-metrics-scraper`, referenced by the ServiceMonitor's `bearerTokenSecret` (the prometheus-operator can only read a Secret) |

About the token Secrets:

- They do not expire and are not rotated. To rotate one, delete the Secret and
  re-run the prereqs phase.
- Anyone who can read Secrets in the controller's namespace holds them. On a
  default namespace-scoped install that is the tenant, who then holds a token
  that can `get /metrics` on every component that authorizes `/metrics` through
  the API server, the API server included.

**On OpenShift the scrape token is not the controller's identity.** It used to
be (`wva-controller-manager-token`, a non-expiring token for the controller's
own ServiceAccount); the installer deletes that Secret on upgrade and on
uninstall. Whoever can read Secrets in the controller's namespace still holds
`wva-epp-metrics-token` and `wva-metrics-scraper-token`, both `get /metrics`
only. The controller's own token stays projected and short-lived, and it
carries `cluster-monitoring-view`, which lets it query the platform
Prometheus / Thanos for **every namespace's** series. Keep the controller
[out of the tenant's reach](../guides/admin-cluster-setup/README.md#keeping-the-controller-out-of-the-tenants-reach)
on a shared OpenShift cluster.

## Permissions

All names below carry the `wva-` prefix, and every ClusterRole and
ClusterRoleBinding the installer applies is renamed with an 8-character hash of
the controller's namespace (`deploy/lib/common.sh`, `wva_ns_suffix`), so two
installs never share or overwrite each other's grants. `kubectl kustomize` on an
overlay shows the unsuffixed names.

### What each overlay creates, by count

| Overlay | ClusterRoles | ClusterRoleBindings | Roles | RoleBindings |
| --- | --- | --- | --- | --- |
| `namespace-scoped/kubernetes` | 6 | 6 | 4 | 4 |
| `namespace-scoped/openshift` | 6 | 7 (adds `cluster-monitoring-view`) | 4 | 4 |
| `cluster-scoped/kubernetes` | 4 | 4 | 3 | 3 |
| `cluster-scoped/openshift` | 4 | 5 (adds `cluster-monitoring-view`) | 3 | 3 |

Namespace-scoped ClusterRoles: `metrics-auth`, `metrics-reader`,
`epp-metrics-reader`, `node-reader`, `kueue-reader`, `policy-namespace-reader`.
Roles: `manager`, `leader-election`, `controller-status`,
`utilization-share-history`. Cluster-scoped has `manager` as a ClusterRole and no
`node-reader`, `kueue-reader` or `policy-namespace-reader` (the manager role
covers them).

So **every shipped overlay has cluster-scoped objects**. A cluster admin creates
them once with `make setup-prereqs`; a namespace admin then installs the
controller.

- Adding `config/components/unauthenticated-metrics` removes the metrics-auth,
  `/metrics` and EPP-scrape grants, but **not** `node-reader`, `kueue-reader` or
  `policy-namespace-reader`.
- The only composition with nothing cluster-scoped is the test overlay
  `test/e2e/testdata/secondary-controller` (base + `namespace-scoped` +
  `tenant-installable` + `unauthenticated-metrics`). It gives up authenticated
  metrics, the EPP scrape grant, the `gpu-inventory` limiter, the Kueue quota
  source and finding a published cluster policy.

### Namespace-scoped, Kubernetes

Granted to `wva-controller-manager` unless noted. "Own" is the controller's
namespace; "managed" is the namespace it manages (the same one unless
`WVA_WATCH_NS` is set, in which case the prereqs phase puts a copy of the
`manager-role` Role and its binding in the managed namespace as well; the one
in the controller's own namespace stays, so the controller reads ConfigMaps
in both).

| Verbs | Resource | Scope | Why |
| --- | --- | --- | --- |
| get, list, watch | configmaps, services | managed namespace (`manager-role`) | the scaling-policy and manager ConfigMaps; resolving Services. The grant is not narrowed by name: it reads **every** ConfigMap in the managed namespace |
| get | configmaps/status | managed namespace | |
| get, list, watch, **patch** | pods | managed namespace | reading replicas and their node; patching annotations and warm-pool labels — see [What it writes](#what-it-writes) |
| create, patch, get, list | events | managed namespace | decisions and warnings on ScaledObjects; reading the cluster autoscaler's `TriggeredScaleUp` for borrowing |
| get, list, watch | deployments, replicasets, statefulsets (apps) | managed namespace | scale targets and their pods |
| get | deployments/scale | managed namespace | current replica count |
| get, list | resourcequotas | managed namespace | a GPU ResourceQuota as the bound for cohort borrowing |
| get | jobs (batch) | managed namespace | the owner of a Kueue-managed GPU pod, for cohort borrowing |
| get, list, watch | inferencepools (both InferencePool API groups) | managed namespace | which EPP queue a workload sits behind |
| get, **patch** | scaledobjects (keda.sh) | managed namespace | the scale target behind a KEDA call; re-wiring one whose HPA KEDA left without the scaler's metric — see [What it writes](#what-it-writes) |
| get | horizontalpodautoscalers (autoscaling) | managed namespace | whether KEDA put the scaler's External metric on a ScaledObject's HPA |
| get, list | localqueues (kueue.x-k8s.io) | managed namespace | the Kueue quota source |
| get, list, watch | workloads (kueue.x-k8s.io) | managed namespace | cohort borrowing; granted cluster-wide below only when a policy counts borrowing |
| get, list, watch | leaderworkersets; get leaderworkersets/scale | managed namespace | LWS scale targets |
| get, list, watch | servicemonitors | managed namespace | preflight of what is scraped |
| get, list, watch, create, update, patch, delete | leases | own namespace (`leader-election-role`) | leader election |
| create, patch | events | own namespace (`leader-election-role`) | |
| get, update | configmaps named `wva-utilization-share-history` | own namespace (`utilization-share-history-role`) | the utilization-share optimizer's need history |
| get, update | configmaps named `wva-controller-status` | own namespace (`controller-status-role`) | publishing the policy version, limiter and optimizer mode the controller runs with ([status ConfigMap](operations.md#what-the-controller-is-running-with)) |
| get, update | configmaps named `wva-learned-state` | own namespace (`learned-state-role`) | learned state across a restart, when `LEARNED_STATE_SNAPSHOT` is on |
| create | configmaps (any name — `create` cannot be narrowed by name) | own namespace (`utilization-share-history-role`, `controller-status-role`, `learned-state-role`) | creating the history, status and learned-state ConfigMaps the first time |
| get, list, watch | nodes, nodes/status | **cluster** (`node-reader-role`) | resolving a variant's accelerator; required by the `gpu-inventory` limiter |
| get, list | clusterqueues, cohorts, resourceflavors | **cluster** (`kueue-reader-role`) | the Kueue quota source; cohorts for cohort borrowing. Inert on a cluster without Kueue. Not created when the install sets `WVA_LIMITER=none` and no policy the controller reads uses Kueue |
| get | namespaces, by name: the managed one only | **cluster** (`kueue-reader-role`) | its labels, to evaluate a ClusterQueue `namespaceSelector` |
| get, list, watch on workloads; get, list on localqueues (kueue.x-k8s.io) | **cluster** (`wva-kueue-borrow-reader-role-<suffix>`) | cohort borrowing: a lender's pending Workload can be in any namespace. Reaches **every tenant's** Kueue Workloads, pod templates included (see [Kueue borrowing](#kueue-borrowing)). **Not rendered.** The installer grants it only while a limiter policy of the install counts borrowing, and revokes it otherwise |
| get | the `kueue-controller-manager` Deployment and the Lease `c1f6bfd2.kueue.x-k8s.io`, by name | Kueue namespace (Role `wva-kueue-controller-reader-<suffix>`, labelled with the suffix) | the Kueue version, and whether Kueue runs. Made by the prereqs phase in `KUEUE_NAMESPACE` (default `kueue-system`); removed by `make undeploy-wva` and by `make disable-limiter WVA_LIMITER_TYPE=quota` |
| get | namespaces, by name: its own, the managed one, `wva-policy` and the namespace the managed one is labelled with | **cluster** (`policy-namespace-reader-role`) | finding a published cluster policy — see [Multi-tenancy](#multi-tenancy) |
| create | tokenreviews, subjectaccessreviews | **cluster** (`metrics-auth-role`) | authenticating and authorizing scrapes of `:8443` |
| get | non-resource URL `/metrics` | **cluster** (`metrics-reader`) | bound to the Prometheus that scrapes the controller, **not** to the controller — see below |
| get | non-resource URL `/metrics` | **cluster** (`epp-metrics-reader-role`) | bound to `wva-epp-metrics-reader`; the EPP scrape |

The two `get namespaces` grants are limited by `resourceNames` to the Namespaces
the controller reads; neither lists or watches. An install whose admin has not
re-run `make setup-prereqs` since they were added lacks them, which matters for
policy resolution — see [Fail-open and fail-closed](#fail-open-and-fail-closed).

**Who `metrics-reader` is bound to.** On Kubernetes the installer binds it to
one ServiceAccount, the first of:

1. `PROMETHEUS_SA`, when set;
2. the one non-default account the Prometheus CRs agree on, in the namespace an
   explicitly set `PROMETHEUS_URL` addresses;
3. the kube-prometheus-stack this install deploys (`DEPLOY_PROMETHEUS=true`,
   with the URL in `MONITORING_NAMESPACE`).

A detected URL never creates it. With none of those, **no binding** is created,
the installer warns, and `make check-prereqs` lists it as optional. It never
binds a guessed account or `default`: a grant to an account nobody chose waits
for whoever creates it. Pass `PROMETHEUS_SA=<namespace>/<serviceaccount>` to
grant your Prometheus the scrape ([configuration](configuration.md)).

Only those sources write or repoint it. An admin re-run of `make setup-prereqs`
with none of them **keeps** an existing binding and warns, naming its account
and how to repoint it (`PROMETHEUS_SA=<namespace>/<serviceaccount>`) or remove
it (`kubectl delete clusterrolebinding <name>`). A binding it cannot read is left
alone and reported as unknown. The tenant phase (`make deploy-wva`) never reads,
reports or patches it.

### Namespace-scoped, OpenShift

Everything in the Kubernetes table, plus:

| Verbs | Resource | Scope | Why |
| --- | --- | --- | --- |
| (OpenShift's role) | ClusterRoleBinding to `cluster-monitoring-view` | **cluster** | querying Thanos Querier with the controller's own token — which reads the platform Prometheus series of **every** namespace, not just the managed one. That role is reported to carry `get` on namespaces, which would let the controller read Namespace objects on OpenShift; nothing in this repository pins the role's contents, so verify on your cluster with `kubectl auth can-i get namespaces --as=system:serviceaccount:<controller namespace>:wva-controller-manager` |
| get | non-resource URL `/metrics` | **cluster** (`metrics-reader`) | repointed at `wva-metrics-scraper`, whose token Secret the ServiceMonitor uses to scrape `:8443`. Not the controller: its token no longer sits in a Secret |

Because of `cluster-monitoring-view`, a namespace-scoped install on OpenShift
always needs a cluster admin, and the installer's preflight says so.

### Cluster-scoped (both platforms)

The namespaced rules above become one ClusterRole, `manager-role`, bound
cluster-wide. The differences that matter for review:

| Verbs | Resource | Scope | Note |
| --- | --- | --- | --- |
| get, list, watch | **configmaps** | **every namespace** | the controller's cache filters ConfigMaps by label, but the grant does not: it can read any ConfigMap in the cluster |
| get, list, watch | **pods** | **every namespace** | read only. `patch` is granted per namespace by Role `wva-pod-writer`, made where it is used (see [Grants made after install](#grants-made-after-install)) |
| get, list, watch | namespaces, nodes, nodes/status | cluster | |
| get, list | clusterqueues, cohorts, localqueues, resourceflavors | cluster | Kueue's queues and flavors |
| get, list, watch | workloads | cluster | reaches **every namespace's** Kueue Workloads: their pod sets (full pod templates), priority and admission, not only the managed models'. Watched through an informer that holds them in memory, started on the first borrowing check. At this scope the read is part of `manager-role` and is held whether or not borrowing is counted |
| get, list | **events** | **every namespace** | read for the cluster autoscaler's `TriggeredScaleUp` on a borrowed pod; the grant reaches every Event in the cluster, whose messages can name objects and carry error text |
| get, list | resourcequotas | **every namespace** | the GPU bound Kueue borrowing checks in each managed namespace |
| get, list, watch | **deployments**, replicasets, statefulsets | **every namespace** | scale targets, the owners of GPU pods for the Kueue managed check, and the `kueue-controller-manager` Deployment (Kueue's version and whether it runs) |
| get, **patch** | **scaledobjects** | **every namespace** | `patch` re-wires a ScaledObject KEDA left without the scaler's metric. The grant is not narrowed to that one key: it can change any ScaledObject's spec. `get` on horizontalpodautoscalers comes with it, read only |
| get | leases, by name: `c1f6bfd2.kueue.x-k8s.io` | **every namespace** | Kueue's leader-election Lease, whether Kueue runs. `resourceNames` narrows the name, not the namespace |

`node-reader` and `kueue-reader` are not rendered at this scope; `manager-role`
already covers them. Leases, the leader-election Events and the history and
status ConfigMaps stay namespaced in the controller's own namespace. On OpenShift the `cluster-monitoring-view`
binding and the `metrics-reader` repoint are added as above.

### Grants made after install

| Command | Grants | To whom |
| --- | --- | --- |
| `deploy/warmpool.sh create`, `make utilization-share-activate`, and `make deploy-wva` for a cluster-scoped install in the `all` or `prereqs` phase only (where a warm pool or utilization-share marks of this controller are, and every namespace of its models when the optimizer is active) | Role `wva-pod-writer` (patch on pods) and a RoleBinding, in the pool's or the models' namespace only. `deploy/warmpool.sh create` and `make utilization-share-activate` skip it where the controller can already patch pods | `wva-controller-manager` of the controller namespace given. Needed by a cluster-scoped controller; a namespace-scoped one already has it in its Role. `make undeploy-wva` removes them |
| `make enable-physical-limiter` (gpu-inventory) | ClusterRole + binding: get, list, watch on nodes | the ServiceAccount of each targeted controller |
| `make enable-physical-limiter WVA_LIMITER_TYPE=quota WVA_QUOTAS='<type>=-1' WVA_QUOTA_KUEUE=true` | ClusterRole + binding: get, list on clusterqueues, cohorts, resourceflavors; get on Namespaces, by name: the namespace a namespace-scoped controller (tenant-owned or out of reach) manages, the same grant `make setup-prereqs` renders. Every Namespace only for a cluster-scoped controller whose ServiceAccount already reads every Namespace (an installer-made one does, through its manager ClusterRole) or whose namespace `WVA_LIMITER_TARGETS` names; a controller that looks cluster-scoped without that read is warned about, with the `make setup-prereqs` run for that install | the same |
| `make setup-prereqs` (namespace scope) and `make enable-physical-limiter WVA_LIMITER_TYPE=quota`, when a limiter policy the controller reads counts cohort borrowing (a quota entry with `kueue.enabled` and `countBorrowable` not `false`) | ClusterRole `wva-kueue-borrow-reader-role-<suffix>` + binding: get, list, watch on Kueue Workloads and get, list on LocalQueues, in **every namespace**. Revoked by the same commands when no policy counts borrowing (`WVA_QUOTA_KUEUE_BORROW=false`, a kept `countBorrowable: false`, a quota without Kueue), by `make disable-limiter WVA_LIMITER_TYPE=quota` and by `make undeploy-wva`. After a revoke, `make setup-prereqs` and `make enable-physical-limiter` say to restart the controller: until then it keeps the Workloads it already cached. Once the controller reads a policy namespace, that policy alone decides; the controller's own ConfigMap, which a tenant may write, does not. Where none resolves, the tenant's own ConfigMap never decides it alone: this run's `WVA_LIMITER=quota` with `WVA_QUOTA_KUEUE=true` grants it, `WVA_QUOTA_KUEUE_BORROW=false` with them revokes it, and a run without `WVA_LIMITER` leaves it as it is. Left as it is, with a warning, when an admin-owned policy cannot be read; a tenant's own policy that cannot be read or parsed counts as not borrowing. Both commands warn, naming them, when a controller they grant it to shares a Kueue cohort with another borrower, granted in the same run or an earlier one | the install's controller (`make setup-prereqs`), or each targeted controller (`make enable-physical-limiter`) |
| `make enable-physical-limiter`, and `make setup-prereqs` when the policy namespace already exists | Role `wva-policy-reader` in the policy namespace (`wva-policy`, or the one a `wva.llmd.ai/policy-namespace` label names): get, list, watch on **all** ConfigMaps there, plus RoleBinding `wva-policy-reader-<suffix>` | each controller that reads policy from that namespace. `make setup-prereqs` grants it only to a controller that manages its own namespace. `make undeploy-wva` leaves both ([cleanup](upgrade-and-uninstall.md#policy-namespace-and-limiter-leftovers-cluster-admin)) |
| `make dashboard` (OpenShift only; **not** part of a default install) | ServiceAccount `wva-grafana-datasource` with a **third legacy, non-expiring** token Secret (`wva-grafana-datasource-token`); RoleBinding to `view` on the namespace; a Role with get, list, **create** on `metrics.k8s.io` pods (the Thanos tenancy port checks `create` because Grafana POSTs) | the Grafana datasource, in the namespace the dashboard is created in. Whoever reads Secrets there can query that namespace's metrics and `view` its objects |

"Targeted" is discovered by label unless you set `WVA_LIMITER_TARGETS` — see
[Multi-tenancy](#multi-tenancy). `make disable-limiter` leaves the node,
cluster-scoped Kueue (`kueue-reader`) and policy reads in place. With
`WVA_LIMITER_TYPE=quota` or `all` it revokes the two Kueue reads that go with
a quota: the Kueue controller read and the cluster-wide borrowing read.

## What it writes

The controller **never writes Deployments, StatefulSets, LeaderWorkerSets
or HPAs**, and writes one trigger metadata key on ScaledObjects. KEDA does the
scaling. What it does write:

| Object | What | When |
| --- | --- | --- |
| ScaledObjects (trigger metadata) | the key `wvaRewire`, on a trigger that calls this scaler, guarded by a test of that trigger's `scalerAddress` | when KEDA left the ScaledObject's HPA without the scaler's External metric; at most once per backoff (1 minute, doubling to 30). `--scaledobject-rewire=false` turns it off |
| Pods (annotation) | `controller.kubernetes.io/pod-deletion-cost` | the utilization-share optimizer, to steer which pod a scale-down removes; and the warm pool, on pods it is lending |
| Pods (annotation) | `llm-d.ai/utilization-share-transfer` — a transfer mark, restoring the previous deletion cost when removed | utilization-share optimizer only |
| Pods (labels) | the labels in an **InferencePool's selector**, added to and removed from warm-pool pods | warm pool only. **This moves live traffic**: a labelled pool pod joins the InferencePool and the EPP routes requests to it. The selector is the tenant's; the controller refuses to write the three labels that make a pod part of its own Deployment and its NetworkPolicy (`app.kubernetes.io/component`, `app.kubernetes.io/name`, `control-plane`) |
| Events | create, patch | warnings and decisions, mostly on ScaledObjects |
| Leases | leader election | own namespace |
| ConfigMap `wva-utilization-share-history` | the optimizer's need history | own namespace, utilization-share only |
| ConfigMap `wva-controller-status` | the policy version, limiter and optimizer mode the controller runs with | own namespace, on every policy load |
| ConfigMap `wva-learned-state` | the saturation analyzer's learned state | own namespace, `LEARNED_STATE_SNAPSHOT` only |
| Warm-pool supervisor (HTTP, not the API) | start, sleep, wake and stop engine instances | warm pool only |

A cluster-scoped grant to patch pods is therefore a grant to change which pods
an InferencePool serves from, wherever warm pools exist.

**Residual risk of the per-namespace grant.** `patch` on pods can change a
running pod's container image. So whoever holds the controller's token can run
code as any ServiceAccount used by a pod in a namespace with
`wva-pod-writer`. The grant is limited to those namespaces, not removed. Grant
it only where a warm pool or the utilization-share optimizer needs it, and
delete it when they go
([leftover pod-write grants](upgrade-and-uninstall.md#leftover-pod-write-grants)).

## Network exposure

| Port | Protocol | Exposed by | Authentication | Notes |
| --- | --- | --- | --- | --- |
| `9090` | gRPC over **mutual TLS** when the install can turn it on, else plaintext ([when](configuration.md#external-scaler-mutual-tls)) | Service `wva-external-scaler` (ClusterIP); NetworkPolicy `wva-external-scaler` admits only KEDA's namespace, where the admin phase wrote it (below) | with TLS, only a client certificate the install's CA signed for `wva-keda-client`; in plaintext, **none** | the KEDA external scaler |
| `8443` | HTTPS | Service `wva-controller-manager-metrics-service` (ClusterIP) | TokenReview + SubjectAccessReview on `get /metrics` | self-signed certificate unless `--metrics-cert-path` is set (OpenShift sets it, from the service CA) |
| `8081` | HTTP | no Service; the kubelet's probes | none | `/healthz`, `/readyz` |
| `8001` (warm pool only) | HTTP, **plaintext** | the warm-pool pod's launcher (supervisor); also `8002` (proxy control) and `9001`-`9016` (engine instances) | **none** | the supervisor starts engine processes with **caller-supplied argv and env**, in a pod that mounts the shared model cache (`model-pvc`) **read-write**. The only protection is the pool's NetworkPolicy, see below |

**The warm-pool NetworkPolicy.** `deploy/warmpool.sh` renders one per pool,
`wva-warm-pool-<pool>`, in the pool's namespace:

- `:8000` (serving) is admitted from **every pod in the pool's namespace**, and
  from `MONITORING_NAMESPACE` when one is named.
- `:8001`, `:8002` and `:9001`-`:9016` (control) are admitted only from
  controller pods (`app.kubernetes.io/name=workload-variant-autoscaler`,
  `control-plane=controller-manager`) in the namespace given as
  `--wva-namespace`. Anyone who can start a pod with those labels there passes.
- A pool whose unit spans several pods (`GROUP_SIZE` > 1) also admits **all
  ports, `:8001` included,** from any pod carrying that pool's labels, so the
  ranks can reach each other.

It holds only where the network plugin enforces NetworkPolicy. The static sample
in `config/warmpool/` names `workload-variant-autoscaler-system` as the
controller namespace; for a controller anywhere else, including the out-of-reach
`wva-<namespace>`, that sample blocks the controller. Use `deploy/warmpool.sh`, or
edit the namespace.

**The gRPC port is the one to close.** Without the TLS flags it has no
authentication, and every call registers the ScaledObject it names with the
trigger metadata the caller sends. So any pod that can reach `:9090` can
register a workload, overwrite a registered one's `modelID` or `variantCost`
until KEDA's next call replaces it, and read the controller's decisions for any
workload it names.

The NetworkPolicy and mutual TLS below prove the caller is **KEDA**. Nothing
checks which namespace a call is for: the scaler registers whatever namespace
and name KEDA sends (`internal/scaler/externalscaler.go`, `registry.Observe`).
So any tenant can write a ScaledObject whose `scalerAddress` names another
install's scaler, and KEDA relays its calls there. Per-namespace authorization
does not exist yet.

Every shipped overlay includes a NetworkPolicy
(`config/components/external-scaler-networkpolicy`) that admits `:9090` only from
KEDA's namespace and leaves `:8443` and `:8081` open. The admin phase
(`make setup-prereqs`) writes it only when it knows KEDA's namespace. Otherwise
**no policy is applied**, and both the installer and `make verify-deployment`
warn that `:9090` is open to every Pod. Close it with `KEDA_NAMESPACE=<keda ns>`
on `make setup-prereqs`, or with the mutual-TLS flags below. How the namespace
is chosen, and which policies the installer leaves alone:
[the external-scaler NetworkPolicy](upgrade-and-uninstall.md#the-external-scaler-networkpolicy).

A wrong namespace fails closed: KEDA is refused and nothing scales. The policy
holds only where the network plugin enforces NetworkPolicy; elsewhere, serve the
scaler over TLS and require KEDA's client certificate
([configuration](configuration.md#advanced)). The shipped rule is equivalent to:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: wva-external-scaler
  namespace: <controller namespace>
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: workload-variant-autoscaler
      control-plane: controller-manager
  policyTypes: [Ingress]
  ingress:
  - ports:                                     # metrics and probes, from anywhere
    - {protocol: TCP, port: 8443}
    - {protocol: TCP, port: 8081}
  - ports:
    - {protocol: TCP, port: 9090}
    from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: keda-system   # your KEDA operator's namespace
```

- `keda-system` is the shipped default. The Custom Metrics Autoscaler on
  OpenShift usually runs in `openshift-keda`, which the OpenShift overlays name.
  Applying an overlay by hand with KEDA elsewhere needs the `namespaceSelector`
  edited.
- `wva-external-scaler-from-keda`, a policy applied by hand from an earlier
  version of this page, adds to the shipped one rather than replacing it. Delete
  it ([upgrade](upgrade-and-uninstall.md#the-external-scaler-networkpolicy)).
- `8443` is already authenticated; narrow its rule to your Prometheus's
  namespace if you want it closed too.
- `8081` is admitted from every source, because kubelet probes come from the
  node, which no `from:` selector can name.

### Scaler mutual TLS

The NetworkPolicy holds only on a network plugin that enforces it. Mutual TLS
holds on any network. It is on by default, once KEDA's side is in place
([when it stays off](configuration.md#external-scaler-mutual-tls)):

- The installer issues a serving certificate for the scaler Service and a
  client certificate for KEDA, from cert-manager when it is installed and from
  a private CA of its own otherwise (on OpenShift the serving certificate comes
  from the service CA). Details: [configuration](configuration.md#external-scaler-mutual-tls).
- The controller then accepts only a client certificate that CA signed **and**
  that names `wva-keda-client`. Anything else is refused in the TLS handshake,
  before any call can register a workload or read a decision.
- KEDA presents its certificate through a ClusterTriggerAuthentication that
  reads a copy of the client Secret in KEDA's own namespace. The admin phase
  writes both; no model namespace gets a copy.
- It protects the port only where the tenant cannot edit the controller's
  Deployment (the controller in an admin-owned namespace): whoever owns the
  Deployment owns its flags.
- It authenticates KEDA, not whoever wrote a ScaledObject. Anyone who may
  create a ScaledObject can give it this install's `authenticationRef`, and
  KEDA then calls the scaler for them
  ([what a ScaledObject needs](configuration.md#external-scaler-mutual-tls)).
  Who may create ScaledObjects is still your RBAC.
- A ScaledObject on the TLS address **without** the `authenticationRef` gets no
  access, but can cost everyone scaling. KEDA shares one connection per
  address; after a KEDA restart, if that ScaledObject connects first, the
  connection opens without KEDA's certificate, and every ScaledObject on that
  address stops scaling until it is fixed and KEDA is restarted
  ([multi-tenancy](#multi-tenancy)).

These Secrets are as sensitive as the port. Anyone who can read one can call
the scaler as KEDA:

- `wva-external-scaler-client-tls` in the controller's namespace, and its copy
  `wva-external-scaler-<controller namespace>` in KEDA's: KEDA's client key.
- `wva-external-scaler-ca` in the controller's namespace: the CA's private key,
  from cert-manager or the installer alike. With it, anyone can sign a client
  certificate of their own.

A tenant who can read Secrets in the controller's namespace holds the client
key and the CA key.

## Data handled

Metrics only. The controller reads Prometheus series, EPP and model-server
`/metrics`, and Kubernetes object metadata and status. It sends no inference
requests and reads no prompts or completions. With a warm pool it calls only the
engines' control endpoints (`/sleep`, `/wake_up`, `/is_sleeping`, `/health`).
The warm-pool **proxy**, a separate image inside pool pods, does forward live
requests; it is part of the data plane, not the controller.

The controller sends its own ServiceAccount token to Prometheus on every query.
The shipped config verifies the Prometheus certificate. The installer turns
verification off only for the kube-prometheus-stack it deploys on Kubernetes,
which has no CA to give the controller, or, on an upgrade, a Prometheus the
controller already ran unverified with no CA. There the token goes to whatever
answers at that address. Set `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` to
`false` and give a CA with `PROMETHEUS_CA_CERT_PATH`. See
[TLS configuration](prometheus.md#tls-configuration).

## Supply chain

| Image | Default | Recommendation |
| --- | --- | --- |
| Controller | from a checkout of `main`: `ghcr.io/ev-shindin/llm-scaling-manager:main`, a **floating** tag rebuilt on every push to main. The installer sets `imagePullPolicy: Always` for it; a kustomize or GitOps render keeps the manifest's `IfNotPresent`, so a node keeps whichever `:main` it pulled first. From a checkout of a release tag: that release's image, signed with cosign | install a release: `git checkout v0.10.0`, which deploys `:v0.10.0`, or pin its digest from the [release notes](https://github.com/ev-shindin/llm-scaling-manager/releases), `IMG=ghcr.io/ev-shindin/llm-scaling-manager@sha256:<digest>` (the digest form has not been verified end to end through the installer yet) ([upgrade and uninstall](upgrade-and-uninstall.md#pin-the-version)). A floating tag means any pod restart can run different code. A registry host with a port (`host:5000/...`) is not parsed correctly by the installer yet; use a Kustomize overlay for that until it is fixed |
| Warm-pool launcher | `ghcr.io/llm-d-incubation/llm-d-fast-model-actuation/launcher:v0.6.4@sha256:...` | pinned by digest in `config/warmpool/` |
| Warm-pool proxy | `ghcr.io/ev-shindin/llm-scaler/warmpool-proxy:v11@sha256:...` | pinned by digest; a fork-built image |

`WVA_IMAGE_PULL_SECRET` names an existing pull Secret for a private registry; the
installer never handles registry credentials itself.

## Multi-tenancy

**Where policy comes from.**

- **The GPU budget** (limiters and the optimizer selection) comes from the
  policy namespace when one resolves, otherwise from the controller's own
  `wva-scaling-policy-config`. It is never taken from a namespace-local map.
- **Thresholds, tiers, per-model entries and utilization-share weights** come
  from the controller's own ConfigMap, **unless the managed namespace has its
  own** `wva-scaling-policy-config`. The controller reads that as a
  namespace-local map that **replaces** its own map for that namespace's models
  (`internal/controller/predicates.go`, `configmap_reconciler.go`,
  `internal/config/config.go`).

The budget's source is resolved once, at startup
(`internal/controller/policy_namespace.go`):

| Controller shape | Policy source |
| --- | --- |
| cluster-scoped | its own ConfigMap |
| namespace-scoped, out of reach (`WVA_NS` ≠ `WVA_WATCH_NS`) | its own ConfigMap, which lives in the admin's namespace |
| namespace-scoped in the namespace it manages, can read Namespaces (on Kubernetes, the grant `make setup-prereqs` creates) | the `wva.llmd.ai/policy-namespace` label on its namespace, else `wva-policy` if it exists, else its own ConfigMap |
| namespace-scoped in the namespace it manages, **cannot** read Namespaces — on **Kubernetes**, an install whose admin has not run `make setup-prereqs` since the grant was added | **its own ConfigMap** — the label and `wva-policy` are not seen; logged at Info with the fix, and `wva_policy_namespace_unverified` = 1 |

The startup log says which applied:

```bash
kubectl logs -n <controller namespace> deploy/wva-controller-manager \
  | grep -E 'Cluster policy comes from|Could not read the managed Namespace|Cannot tell whether'
```

| log line | policy source |
| --- | --- |
| `Cluster policy comes from the namespace this one is labelled with` | the admin's, from the labelled namespace |
| `Cluster policy comes from the well-known namespace` | the admin's, from `wva-policy` |
| `Cluster policy comes from this controller's own namespace` | its own ConfigMap; no admin policy found. Logged at V(1), which the default `-v=2` shows |
| `Cannot tell whether a cluster admin published policy` | its own ConfigMap, because Namespaces could not be read; `wva_policy_namespace_unverified` = 1 |

A cluster-scoped or out-of-reach controller always logs the own-namespace line.

**What a tenant can influence**, beyond their own workloads:

| Lever | Effect | Status |
| --- | --- | --- |
| Owning the controller's Deployment (tenant-owned shape) | image, args, env and therefore every bound; and the pod runs as an SA with cluster-wide node read, Kueue read and the EPP token | by design — use the [out-of-reach shape](../guides/admin-cluster-setup/README.md#keeping-the-controller-out-of-the-tenants-reach) where the bound must hold |
| The controller's own `wva-scaling-policy-config`, in the tenant-owned shape | thresholds, tiers, per-model entries; **and the limiters**, wherever no policy namespace resolves (including a namespace-scoped Kubernetes install without the Namespace read, which `wva_policy_namespace_unverified` flags) | by design where no policy is published; on Kubernetes a cluster admin re-runs `make setup-prereqs` so a published one is seen |
| A `wva-scaling-policy-config` in their own namespace, **in any shape** — out-of-reach included | read as a namespace-local map that replaces the controller's thresholds, tiers, per-model entries and weights for their models. It cannot set limiters or the optimizer | by design — the out-of-reach shape protects the budget, not the per-model tuning |
| `weightClass` / `weight` on their models' entries | under a shared utilization-share budget, a model set to `critical` (weight 4 in the default classes) takes **4x** the headroom of a `standard` one, but only from models planned in the **same group**. A model whose namespace has its own `wva-scaling-policy-config` is kept out of the cluster-wide group and planned only in its namespace's quota group, so a weight there reorders that tenant's own models. A weight in the controller's own ConfigMap applies in the cluster-wide group too | by design — keep weights in admin-owned policy; a tenant-written namespace-local weight moves only that tenant's budget |
| `minReplicaCount` on their ScaledObjects | under a shared utilization-share budget, a replica floor is held first, so a higher floor takes from the other models' share | by design |
| `maxReplicaCount`, or a second KEDA trigger | bypasses the limiter: the HPA takes the maximum across triggers | by design — a `ResourceQuota` is the bound that holds |
| A LocalQueue pointing at any ClusterQueue | the controller grants their namespace that ClusterQueue's quota only when its `namespaceSelector` admits the namespace, with Kueue's semantics (none admits nothing, `{}` admits all); a Namespace whose labels cannot be read gets nothing from a selector-bearing queue | closed by the selector. An install whose Kueue reader lacks `get namespaces` (set up before it was added) withholds every selector-bearing queue until a cluster admin re-runs `make setup-prereqs` |
| An InferencePool selector | decides which labels the controller writes onto warm-pool pods in that namespace | by design, minus the guarded identity labels |
| A Deployment labelled `app.kubernetes.io/name=workload-variant-autoscaler` | `make enable-physical-limiter` treats it as a controller only if it runs as the controller ServiceAccount (`WVA_CONTROLLER_SA`) and has a `manager` container from a controller image repository (`WVA_CONTROLLER_IMAGE_REPOS`; a namespace named in `WVA_LIMITER_TARGETS` relaxes the image check). Anything else is `SKIPPED`, granted nothing, and listed as NOT bound under the banner. A verified one gets its ServiceAccount node read (and Kueue read), a `kubectl rollout restart` of that named Deployment when it must re-resolve its policy namespace (it could not read its Namespace, or started before the policy namespace existed), and `limiters` in the ConfigMap its policy resolves to, the `wva.llmd.ai/policy-namespace` label honoured | signals, not proof: any tenant can satisfy them. Where tenants are not trusted, run their controllers out of reach and name exactly those namespaces in `WVA_LIMITER_TARGETS` |
| Reaching `:9090` | register or overwrite trigger metadata; read decisions | closed by the shipped NetworkPolicy, above, where it was applied and the network plugin enforces it. Scaler mutual TLS closes it on any network only with `--external-scaler-client-san` pinning KEDA's identity, and only where an admin owns the controller's Deployment: in the tenant-owned shape the tenant owns its args, and so the TLS flags |
| A ScaledObject whose `scalerAddress` names another install's scaler | KEDA relays its calls, and that controller registers and sizes the workload | **open**: both controls above authenticate KEDA, not the tenant. No per-namespace authorization exists |
| A ScaledObject on a shared controller's TLS address **without** its `authenticationRef` (or with a CA-only TriggerAuthentication) | **availability, not access.** The scaler still refuses it in the TLS handshake. But KEDA keeps one connection per address: after a KEDA operator restart, if this ScaledObject connects first, that connection opens without KEDA's certificate, and every ScaledObject on that address, in every namespace, stops scaling until it is fixed and KEDA is restarted | **open**: neither the NetworkPolicy nor the address limits it. KEDA connects from its own namespace, which the policy admits, and the address is predictable. `make verify-deployment` names such ScaledObjects in the namespaces it watches. To close it, limit who may create ScaledObjects, or add an admission policy that requires the `authenticationRef` on triggers naming that address |

**A shared Prometheus shows every tenant's series.** Every controller's
metrics carry the model namespace (`exported_namespace`), model and variant
names, budgets and refusal reasons. Anyone who can query that Prometheus
reads them for every tenant. Where that matters, give each tenant its own
Prometheus, or put a label-enforcing proxy in front of a shared one.

**Budgets are per controller.** Each controller charges a static quota only
with what its own models hold. A `scope: cluster` quota published in a shared
policy therefore lets N controllers take N times the figure, and a
namespace-scoped controller under `gpu-inventory` without a cluster-wide Pod
list sees only its own namespace's GPUs. A bound that must hold across
tenants needs one controller for it, or a `ResourceQuota`.

### Kueue borrowing

- **What borrowing reads beyond the managed namespace.** Kueue Workloads in
  every namespace, at every install scope: a lender's pending Workload can be
  anywhere. A Workload carries its pod templates whole: other teams' images,
  commands, arguments, inline environment values, requests and priorities.
  A cluster-scoped install also reads Events in every namespace. Through a
  Role in the Kueue namespace, it reads the `kueue-controller-manager`
  Deployment and its leader-election Lease. All read-only.
- **Who gets the cluster-wide Workload read.** A cluster-scoped install always
  has it, in its manager ClusterRole. A namespace-scoped install gets it only
  while one of its limiter policies counts borrowing: `make setup-prereqs` and
  `make enable-physical-limiter WVA_LIMITER_TYPE=quota` grant it as its own
  ClusterRole, `wva-kueue-borrow-reader-role-<suffix>`, and revoke it when no
  policy counts borrowing. A controller that reads a policy namespace is
  judged by that policy alone, so a tenant cannot earn the read by editing its
  own ConfigMap. Where no policy namespace resolves, the controller's policy
  is the tenant's own ConfigMap; there `make setup-prereqs` grants the read
  only when the admin sets `WVA_LIMITER=quota` and `WVA_QUOTA_KUEUE=true` for
  that run, never for the ConfigMap alone, and warns when the ConfigMap counts
  borrowing without them. A run without `WVA_LIMITER` leaves the read as it
  is: it neither grants it nor revokes one an earlier run granted.
  `WVA_LIMITER=none`, a quota without Kueue and
  `countBorrowable: false` (`WVA_QUOTA_KUEUE_BORROW=false`) all go without
  it. Without the read, borrowing is withheld (`lenders-unreadable`) and the
  nominal cap applies.
- **The grant follows the commands, not the ConfigMap.** It is decided when
  `make setup-prereqs` or `make enable-physical-limiter` runs. Editing the
  policy ConfigMap by hand to `countBorrowable: false` afterwards stops the
  borrowing, but the cluster-wide Workload read stays granted until one of
  those commands runs again. To take the read away, re-run
  `make setup-prereqs` with the install's variables, or
  `make disable-limiter WVA_LIMITER_TYPE=quota`.
- **The cluster autoscaler's Events are trusted.** A `TriggeredScaleUp` Event
  on an unplaced pod stretches its placement timeout up to three times. The
  Event's source is not verified, so anyone who may create Events in the
  namespace can write one. The most it buys is a borrowed replica held
  unplaced for three placement timeouts (15 minutes at the default) before it
  is given back. Do not grant tenants `create` on Events where that matters.
- **The managed check is a heuristic, not a boundary.** Borrowing is counted
  for a namespace only when its GPU pods are Kueue-managed, each backed by a
  Workload with quota reserved that requests a GPU and is owned by a kind a
  Kueue integration owns Workloads by (Pod, Job, JobSet, LeaderWorkerSet,
  StatefulSet), so a tenant's own small Workload owned by its Deployment or
  ReplicaSet does not pass; the reserved Workloads must also cover the GPUs
  the running pods of their owners request, each Workload counted once, so a
  1-GPU Workload owned by a 16-GPU LeaderWorkerSet, or one 8-GPU Workload
  naming ten 8-GPU pods, does not pass either. That keeps the figures honest; it does
  not stop a tenant from running GPU work outside Kueue. A `ResourceQuota`
  does. Do not grant tenants write on Kueue `Workloads` in namespaces that
  count borrowing, or set `countBorrowable: false` there.
- **Kueue's pod condition is trusted.** A terminating pod carrying Kueue's
  `TerminationTarget` condition is skipped by the managed check, and the
  preemption watch reads a preemption from it. Writing that condition needs
  `pods/status`, which the built-in `edit` and `admin` roles do not grant. Do
  not grant tenants `pods/status` write in namespaces that count borrowing.
- **An evicted pod keeps its GPUs until it is gone.** A pod Kueue evicted
  with a long `terminationGracePeriodSeconds` still holds its GPUs while the
  managed check skips it. A replica ordered into those GPUs waits unplaced
  until they free; the placement timeout gives it back if that takes too long.
- **A `Never` lender's capacity is not borrowed.** With `reclaimWithinCohort:
  Never`, Kueue never takes capacity back, so the scaling manager does not
  count what such a lender lends (`lender-cannot-reclaim`). A `LowerPriority`
  lender is counted only for a borrowing model whose own priority is below
  the lender's. The priorities are read from the Workloads, which a tenant
  sets through its own priority classes: a tenant that raises its serving
  Workloads' priority above a lender's makes that lender's capacity
  uncountable for itself, never the reverse. A priority class is read at the
  highest `spec.priority` any GPU Workload of that class carries, cluster-wide.
  So a tenant that runs one high-priority Workload under a class other
  tenants also use closes `LowerPriority` lending to their models too. It can
  only deny borrowing, never open it.
- **In a cohort shared between tenants**, give every lender that must get
  capacity back from serving `reclaimWithinCohort: Any`.

**For a tenant you do not trust, bound the namespace at admission**: a
`ResourceQuota` on `requests.nvidia.com/gpu`. The limiter bounds what the
scaling manager asks for; the quota bounds what the cluster admits
([how](../guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)).

## Fail-open and fail-closed

| Condition | Behaviour | Direction |
| --- | --- | --- |
| No limiter declared (the shipped policy) | scaling is bounded only by each workload's `maxReplicaCount`; scale-from-zero wakes without a capacity check | **open** |
| A limiter entry that fails validation, at startup | the whole `default` entry is rejected, and the controller refuses to start (`refusing to start unbounded`) | closed |
| A limiter entry that fails validation, while running | the whole `default` entry is rejected and the previous one stays in force. A `ConfigEntryRejected` event and an ERROR log say the controller will refuse to start on its next restart until the entry is fixed | held |
| Policy namespace resolved, reading its ConfigMap fails with any error other than NotFound | the controller refuses to start (`refusing to start unbounded`) | closed |
| Policy namespace resolved, no ConfigMap in it | empty policy: no limiters | **open** |
| Cannot tell whether `wva-policy` exists (an API error other than NotFound or Forbidden) | refuses to start | closed |
| Forbidden reading Namespaces (namespace-scoped, Kubernetes, no `make setup-prereqs` grant) | not treated as "no policy namespace": it uses its own ConfigMap, logs `Cannot tell whether a cluster admin published policy` with the fix at Info, and sets `wva_policy_namespace_unverified` = 1 (alert `WVAPolicyNamespaceUnverified`, installed only with `DEPLOY_ALERTING_RULES=true`) | **open**, and reported |
| A ClusterQueue with a `namespaceSelector`, the managed Namespace's labels unreadable | that queue's grant is withheld from the namespace | closed |
| `gpu-inventory` declared beside a quota, node read denied | Ready; the physical limiter is skipped and the quota bounds; `wva_constraint_provider_skipped{reason="node_read_denied"}` = 1 | held |
| `gpu-inventory` declared alone, node read denied | NotReady via the `gpu-policy` readiness check; `wva_node_access_denied` = 1 (alert `WVANodeAccessDenied` with `DEPLOY_ALERTING_RULES=true`) | closed |
| `gpu-inventory` on a namespace-scoped controller that cannot list Pods in every namespace (no command grants it) | GPU usage counts only its own namespace, so GPUs other namespaces hold on the same nodes read as free. An error logged once, `wva_pod_access_denied` = 1, `podListDenied` in `wva-controller-status`; `make setup-prereqs` and `make enable-physical-limiter` warn | **open**, and reported |
| A `scope: cluster` quota read by a namespace-scoped controller | it is charged only its own variants' GPUs, so the quota caps each such controller, not the cluster. Logged when the limiter is built; `clusterQuotaPerController` in `wva-controller-status` | **open**, and reported |
| `utilizationShare.physicalGroups` on a namespace-scoped controller | ignored: the optimizer runs without physical groups. Under `gpu-inventory` alone a warning is logged and `utilizationShareNote` is set in `wva-controller-status`; under a quota it changes nothing. `utilization-share.sh shadow --physical-groups` refuses such a controller | closed |
| Kueue borrowing granted to two controllers in one cohort | each counts only its own borrowing in flight, so both may spend the same free GPUs. `make setup-prereqs` and `make enable-physical-limiter` warn, naming them. Sibling cohorts under one parent count as one; a parent Cohort that cannot be read is not followed | **open**, and warned |
| Quota entry with no entry for an accelerator type | that type is denied | closed |
| Kueue unreadable, never read since start | the static quota entry alone; with `-1` that is no cap | **open** |
| Kueue unreadable after a good read | the last good snapshot stays in force | held |
| Two limiter kinds declared | both are built; a decision must fit both | closed |
| One limiter unavailable in a cycle (usage not observed yet, or an error) | skipped, logged when the skip starts and ends, and `wva_constraint_provider_skipped` = 1; the others still bound. Only when every one is unavailable is a decision unchecked; scale-from-zero then uses its last constraints if under 5 minutes old, debiting each wake from them (best-effort: per namespace and accelerator name), and counts a wake with no check in `wva_scale_from_zero_wake_unchecked_total` | held |
| The installer cannot tell where KEDA runs | no external-scaler NetworkPolicy is applied; the installer warns and `make verify-deployment` reports `:9090` open | **open**, and reported |
| The external-scaler NetworkPolicy names a namespace KEDA does not run in | KEDA's calls are dropped; ScaledObjects go not-Ready and nothing scales | closed |
| Scaler TLS on, a ScaledObject on the plaintext address or without KEDA's `authenticationRef` | the scaler refuses it in the handshake and logs the refusal. On the TLS address it may still work, riding the connection an authenticated ScaledObject opened; after a KEDA restart, if it connects first, every ScaledObject on that address stops scaling. `make verify-deployment` names it | closed to access; **open** to that outage |
| TLS is the default, but KEDA's side is missing or ScaledObjects would be left behind | the install stays on plaintext and says why; `WVA_SCALER_TLS=true` refuses instead | **open**, and reported |
| KEDA's copy of the client certificate expired (no `make setup-prereqs` since) | every handshake fails; nothing scales. `make verify-deployment` warns 30 days ahead | closed |
| Scaler TLS on, the serving certificate expired (no cert-manager, no re-install) | every handshake fails; nothing scales. `make verify-deployment` warns 30 days ahead | closed |
| The installer finds a Secret, Issuer or Certificate it did not create, or cannot read one, in the controller namespace or on KEDA's side (KEDA's copy, the ClusterTriggerAuthentication, the `wva-scaler-keda-auth` ConfigMap) | it refuses to install TLS and changes nothing: every one is checked before the first write | closed |
| TLS is the default, but cert-manager does not accept or issue the certificates, or you may not read or create them | the install stays on plaintext and says why; `WVA_SCALER_TLS=true`, or a controller that already has TLS, refuses instead | **open**, and reported |
| KEDA's certificate carries another client identity than the controller pins (`WVA_SCALER_CLIENT_SAN` changed on a split install) | the default stays on plaintext, `WVA_SCALER_TLS=true` refuses, and a controller that has TLS keeps it with a warning; the fix is `make setup-prereqs` and a KEDA restart | closed |

## Production hardening checklist

- [ ] Prometheus TLS verified: `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "false"` and `PROMETHEUS_CA_CERT_PATH` set (the controller sends its token there).
- [ ] A real certificate for `:8443`: `--metrics-cert-path` (OpenShift does this; on Kubernetes the shipped ServiceMonitor also skips verification).
- [ ] Controller image pinned to a release tag (`v0.10.0` or later) or its digest, not `:main`, and deployed from a checkout of the same release.
- [ ] The shipped external-scaler NetworkPolicy in place (`WVA_SCALER_NETWORKPOLICY` not `false`), naming the KEDA operator's namespace (`make verify-deployment` prints no NetworkPolicy warning), on a network plugin that enforces it — or scaler mutual TLS (on by default; `make verify-deployment` says "mutual TLS" and names no ScaledObject left on plaintext) on a controller Deployment an admin owns. `make verify-deployment` reports which, and the certificate's expiry.
- [ ] A `ResourceQuota` on the GPU resource in every tenant namespace — the real bound.
- [ ] The controller out of the tenant's reach (`WVA_NS` admin-owned, `WVA_WATCH_NS` the tenant's) wherever a bound must hold.
- [ ] `make enable-physical-limiter` run with an explicit `WVA_LIMITER_TARGETS`.
- [ ] The `metrics-reader` binding names your Prometheus's ServiceAccount (`PROMETHEUS_SA`), or the installer reported that it created none.
- [ ] No `components/unauthenticated-metrics` in production overlays.
- [ ] The controller container's hardening kept in any overlay you write: it runs as non-root, with a read-only root filesystem (`readOnlyRootFilesystem: true`; `/tmp` is an `emptyDir`), `RuntimeDefault` seccomp, no privilege escalation and all capabilities dropped.

## Reporting a vulnerability

Report privately through GitHub security advisories on
[ev-shindin/llm-scaling-manager](https://github.com/ev-shindin/llm-scaling-manager/security/advisories/new),
not in a public issue.

## Related

- [Cluster-admin setup](../guides/admin-cluster-setup/) — the objects a cluster admin creates, and why
- [The GPU limiter](gpu-limiter.md) — where policy lives and who may edit it
- [Configuration](configuration.md) — every installer variable
