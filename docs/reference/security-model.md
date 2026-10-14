# Security model

What the scaling manager is trusted with, what it can read and write, what it
exposes on the network, and which failures leave scaling bounded or unbounded.
Written for three readers: the **cluster admin** who grants its permissions, the
**namespace tenant** who runs it with namespace rights only, and the **security
reviewer** who has to sign off on both.

Everything here is taken from the rendered overlays in `config/overlays/`, the
installer in `deploy/lib/`, and the controller source. Where the current
behaviour has a known gap, the page says so and gives the workaround.

## Trust boundaries

| Boundary | Who is on each side | What crosses it |
| --- | --- | --- |
| Kubernetes API | controller ServiceAccount ↔ API server | reads of pods, workloads, nodes, ConfigMaps, InferencePools, ScaledObjects, Kueue queues; pod patches, Events, Leases, one ConfigMap |
| KEDA → controller | KEDA operator ↔ gRPC on `:9090` | scale target names, trigger metadata (`modelID`, `variantCost`, ...); replica counts back |
| Prometheus → controller | the scraping Prometheus ↔ `:8443` | the controller's own `wva_*` metrics |
| Controller → Prometheus / Thanos | controller ↔ your Prometheus | PromQL queries, carrying the controller's own ServiceAccount token |
| Controller → EPP and model-server pods | controller ↔ pod `/metrics` | metric scrapes, with the `epp-metrics` token |
| Controller → warm-pool pods (only with a warm pool) | controller ↔ the pool's supervisor and engine ports | plain-HTTP commands to start, sleep, wake and stop engine instances |
| Cluster admin ↔ tenant | whoever owns the controller's Deployment ↔ whoever is bounded by it | the scaling policy, and the GPU budget in it |

The last row is the one that decides whether a GPU bound holds. Whoever can edit
the controller's Deployment owns its image, args and env, so **nothing carried
on the controller can bound the person who can edit it**. See
[Multi-tenancy](#multi-tenancy).

## Identities

| Identity | Created by | Used for |
| --- | --- | --- |
| ServiceAccount `wva-controller-manager` | the prereqs phase (`make setup-prereqs`) | everything the controller does against the API server, and its queries to Prometheus (`PROMETHEUS_TOKEN_PATH` points at this SA's projected token) |
| ServiceAccount `wva-epp-metrics-reader` | the prereqs phase | scraping EPP `/metrics`. Bound cluster-wide to `get` on the non-resource URL `/metrics` |
| Secret `wva-epp-metrics-token` | the prereqs phase | a **legacy, non-expiring** `kubernetes.io/service-account-token` for `wva-epp-metrics-reader`, mounted into the controller at `/var/run/secrets/epp-metrics` |
| Secret `wva-controller-manager-token` | the prereqs phase, **OpenShift only** | a **legacy, non-expiring** token for the controller SA, referenced by the ServiceMonitor's `bearerTokenSecret` so user-workload Prometheus can scrape `:8443` |

Two things to know about the token Secrets. They do not expire and are not
rotated: deleting the Secret and re-running the prereqs phase is the rotation.
And anyone who can read Secrets in the controller's namespace holds them — on a
default namespace-scoped install that is the tenant, so the tenant holds a token
that can `get /metrics` on every component in the cluster that authorizes
`/metrics` through the API server, including the API server's own.

**On OpenShift, `wva-controller-manager-token` is the controller's own identity.**
It is a non-expiring token for `wva-controller-manager`, so whoever can read
Secrets in the controller's namespace holds everything that ServiceAccount
holds: `patch` on pods and reads of every ConfigMap in the managed namespace,
cluster-wide node and Kueue read, `get /metrics` cluster-wide (the repointed
`metrics-reader`), and `cluster-monitoring-view` — which also lets it query the
platform Prometheus / Thanos for **every namespace's** series. In the
tenant-owned shape that reader is the tenant. Keep the controller
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
| `namespace-scoped/kubernetes` | 5 | 5 | 3 | 3 |
| `namespace-scoped/openshift` | 5 | 6 (adds `cluster-monitoring-view`) | 3 | 3 |
| `cluster-scoped/kubernetes` | 4 | 4 | 2 | 2 |
| `cluster-scoped/openshift` | 4 | 5 (adds `cluster-monitoring-view`) | 2 | 2 |

So **no shipped overlay is free of cluster-scoped objects**. A namespace admin
installs the controller; a cluster admin creates these first, once, with
`make setup-prereqs`. Adding `config/components/unauthenticated-metrics` to a
shipped overlay removes the metrics-auth, `/metrics` and EPP-scrape grants but **not**
`node-reader` or `kueue-reader`, so the result is still not free of
cluster-scoped objects. The one composition with nothing cluster-scoped is the
test overlay `test/e2e/testdata/secondary-controller` (base +
`namespace-scoped` + `tenant-installable` + `unauthenticated-metrics`, no
`node-reader`, no `kueue-reader`); a controller built that way gives up
authenticated metrics, the EPP scrape grant, the `gpu-inventory` limiter and
the Kueue quota source.

### Namespace-scoped, Kubernetes

Granted to `wva-controller-manager` unless noted. "Own" is the controller's
namespace; "managed" is the namespace it manages (the same one unless
`WVA_WATCH_NS` is set, in which case the prereqs phase puts the `manager-role`
Role and its binding in the managed namespace).

| Verbs | Resource | Scope | Why |
| --- | --- | --- | --- |
| get, list, watch | configmaps, services | managed namespace (`manager-role`) | the scaling-policy and manager ConfigMaps; resolving Services. The grant is not narrowed by name: it reads **every** ConfigMap in the managed namespace |
| get | configmaps/status | managed namespace | |
| get, list, watch, **patch** | pods | managed namespace | reading replicas and their node; patching annotations and warm-pool labels — see [What it writes](#what-it-writes) |
| create, patch | events | managed namespace | decisions and warnings on ScaledObjects |
| get, list, watch | deployments, replicasets, statefulsets (apps) | managed namespace | scale targets and their pods |
| get | deployments/scale | managed namespace | current replica count |
| get, list, watch | inferencepools (both InferencePool API groups) | managed namespace | which EPP queue a workload sits behind |
| get | scaledobjects (keda.sh) | managed namespace | the scale target behind a KEDA call |
| get, list | localqueues (kueue.x-k8s.io) | managed namespace | the Kueue quota source |
| get, list, watch | leaderworkersets; get leaderworkersets/scale | managed namespace | LWS scale targets |
| get, list, watch | servicemonitors | managed namespace | preflight of what is scraped |
| get, list, watch, create, update, patch, delete | leases | own namespace (`leader-election-role`) | leader election |
| create, patch | events | own namespace (`leader-election-role`) | |
| get, update | configmaps named `wva-utilization-share-history` | own namespace (`utilization-share-history-role`) | the utilization-share optimizer's need history |
| get, update | configmaps named `wva-controller-status` | own namespace (`controller-status-role`) | publishing the policy version, limiter and optimizer mode the controller runs with ([status ConfigMap](operations.md#what-the-controller-is-running-with)) |
| get, update | configmaps named `wva-learned-state` | own namespace (`learned-state-role`) | learned state across a restart, when `LEARNED_STATE_SNAPSHOT` is on |
| create | configmaps (any name — `create` cannot be narrowed by name) | own namespace (`utilization-share-history-role`, `controller-status-role`, `learned-state-role`) | creating the history, status and learned-state ConfigMaps the first time |
| get, list, watch | nodes, nodes/status | **cluster** (`node-reader-role`) | resolving a variant's accelerator; required by the `gpu-inventory` limiter |
| get, list | clusterqueues, resourceflavors | **cluster** (`kueue-reader-role`) | the Kueue quota source. Inert on a cluster without Kueue |
| get | namespaces, by name: the managed one only | **cluster** (`kueue-reader-role`) | its labels, to evaluate a ClusterQueue `namespaceSelector` |
| get | namespaces, by name: its own, the managed one, `wva-policy` and the namespace the managed one is labelled with | **cluster** (`policy-namespace-reader-role`) | finding a published cluster policy — see [Multi-tenancy](#multi-tenancy) |
| create | tokenreviews, subjectaccessreviews | **cluster** (`metrics-auth-role`) | authenticating and authorizing scrapes of `:8443` |
| get | non-resource URL `/metrics` | **cluster** (`metrics-reader`) | bound to the Prometheus that scrapes the controller, **not** to the controller — see below |
| get | non-resource URL `/metrics` | **cluster** (`epp-metrics-reader-role`) | bound to `wva-epp-metrics-reader`; the EPP scrape |

The two `get namespaces` grants are limited by `resourceNames` to the Namespaces
the controller reads; neither lists or watches. An install whose admin has not
re-run `make setup-prereqs` since they were added lacks them, which matters for
policy resolution — see [Fail-open and fail-closed](#fail-open-and-fail-closed).

**Who `metrics-reader` is bound to.** On Kubernetes the installer binds it to
one ServiceAccount, chosen in this order: `PROMETHEUS_SA` when set; else the
one non-default account the Prometheus CRs agree on in the namespace an
explicitly set `PROMETHEUS_URL` addresses; else the kube-prometheus-stack this
install deploys (`DEPLOY_PROMETHEUS=true`, with the URL in
`MONITORING_NAMESPACE`). A detected URL never creates it. With none of those,
**no binding** is created, the installer warns, and `make check-prereqs` lists
the binding as optional: it never binds a guessed account or `default`, because a grant to an account nobody
chose waits for whoever creates it. Pass `PROMETHEUS_SA=<namespace>/<serviceaccount>`
to have your Prometheus granted the scrape
([configuration](configuration.md)).

### Namespace-scoped, OpenShift

Everything in the Kubernetes table, plus:

| Verbs | Resource | Scope | Why |
| --- | --- | --- | --- |
| (OpenShift's role) | ClusterRoleBinding to `cluster-monitoring-view` | **cluster** | querying Thanos Querier with the controller's own token — which reads the platform Prometheus series of **every** namespace, not just the managed one. That role is reported to carry `get` on namespaces, which would let the controller read Namespace objects on OpenShift; nothing in this repository pins the role's contents, so verify on your cluster with `kubectl auth can-i get namespaces --as=system:serviceaccount:<controller namespace>:wva-controller-manager` |
| get | non-resource URL `/metrics` | **cluster** (`metrics-reader`) | repointed at the controller's own SA, whose token Secret the ServiceMonitor uses to scrape `:8443` |

Because of `cluster-monitoring-view`, a namespace-scoped install on OpenShift
always needs a cluster admin, and the installer's preflight says so.

### Cluster-scoped (both platforms)

The namespaced rules above become one ClusterRole, `manager-role`, bound
cluster-wide. The differences that matter for review:

| Verbs | Resource | Scope | Note |
| --- | --- | --- | --- |
| get, list, watch | **configmaps** | **every namespace** | the controller's cache filters ConfigMaps by label, but the grant does not: it can read any ConfigMap in the cluster |
| get, list, watch, **patch** | **pods** | **every namespace** | the code patches only pods of workloads it manages and warm-pool pods, but the grant reaches every pod, `kube-system` included |
| get, list, watch | namespaces, nodes, nodes/status | cluster | |
| get, list | clusterqueues, localqueues, resourceflavors | cluster | |

`node-reader` and `kueue-reader` are not rendered at this scope; `manager-role`
already covers them. Leases, Events and the history ConfigMap stay namespaced in
the controller's own namespace. On OpenShift the `cluster-monitoring-view`
binding and the `metrics-reader` repoint are added as above.

### Grants made after install

| Command | Grants | To whom |
| --- | --- | --- |
| `make enable-physical-limiter` (gpu-inventory) | ClusterRole + binding: get, list, watch on nodes | the ServiceAccount of each targeted controller |
| `make enable-physical-limiter WVA_QUOTA_KUEUE=true` (quota) | ClusterRole + binding: get, list on clusterqueues, resourceflavors; get on the controller's managed Namespace only, by name | the same |
| `make enable-physical-limiter` | Role `wva-policy-reader` in the policy namespace (`wva-policy`, or the one a `wva.llmd.ai/policy-namespace` label names): get, list, watch on **all** ConfigMaps there, plus a RoleBinding | each controller that reads policy from that namespace |
| `make dashboard` (OpenShift only; **not** part of a default install) | ServiceAccount `wva-grafana-datasource` with a **third legacy, non-expiring** token Secret (`wva-grafana-datasource-token`); RoleBinding to `view` on the namespace; a Role with get, list, **create** on `metrics.k8s.io` pods (the Thanos tenancy port checks `create` because Grafana POSTs) | the Grafana datasource, in the namespace the dashboard is created in. Whoever reads Secrets there can query that namespace's metrics and `view` its objects |

"Targeted" is discovered by label unless you set `WVA_LIMITER_TARGETS` — see
[Multi-tenancy](#multi-tenancy). `make disable-physical-limiter` leaves these
grants in place.

## What it writes

The controller **never writes Deployments, StatefulSets, LeaderWorkerSets,
ScaledObjects or HPAs**. KEDA does the scaling. What it does write:

| Object | What | When |
| --- | --- | --- |
| Pods (annotation) | `controller.kubernetes.io/pod-deletion-cost` | the utilization-share optimizer, to steer which pod a scale-down removes; and the warm pool, on pods it is lending |
| Pods (annotation) | `llm-d.ai/utilization-share-transfer` — a transfer mark, restoring the previous deletion cost when removed | utilization-share optimizer only |
| Pods (labels) | the labels in an **InferencePool's selector**, added to and removed from warm-pool pods | warm pool only. **This moves live traffic**: a labelled pool pod joins the InferencePool and the EPP routes requests to it. The selector is the tenant's; the controller refuses to write the three labels that make a pod part of its own Deployment and its NetworkPolicy (`app.kubernetes.io/component`, `app.kubernetes.io/name`, `control-plane`) |
| Events | create, patch | warnings and decisions, mostly on ScaledObjects |
| Leases | leader election | own namespace |
| ConfigMap `wva-utilization-share-history` | the optimizer's need history | own namespace, utilization-share only |
| ConfigMap `wva-learned-state` | the saturation analyzer's learned state | own namespace, `LEARNED_STATE_SNAPSHOT` only |
| Warm-pool supervisor (HTTP, not the API) | start, sleep, wake and stop engine instances | warm pool only |

A cluster-scoped grant to patch pods is therefore a grant to change which pods
an InferencePool serves from, wherever warm pools exist.

## Network exposure

| Port | Protocol | Exposed by | Authentication | Notes |
| --- | --- | --- | --- | --- |
| `9090` | gRPC, **plaintext** by default; TLS with `--external-scaler-cert-path` | Service `wva-external-scaler` (ClusterIP); NetworkPolicy `wva-external-scaler` admits only KEDA's namespace | **none** by default; mutual TLS with `--external-scaler-client-ca`, limited to named clients with `--external-scaler-client-san` | the KEDA external scaler |
| `8443` | HTTPS | Service `wva-controller-manager-metrics-service` (ClusterIP) | TokenReview + SubjectAccessReview on `get /metrics` | self-signed certificate unless `--metrics-cert-path` is set (OpenShift sets it, from the service CA) |
| `8081` | HTTP | no Service; the kubelet's probes | none | `/healthz`, `/readyz` |
| `8001` (warm pool only) | HTTP, **plaintext** | the warm-pool pod's launcher (supervisor); also `8002` (proxy control) and `9001`-`9016` (engine instances) | **none** | the supervisor starts engine processes with **caller-supplied argv and env**, in a pod that mounts the shared model cache (`model-pvc`) **read-write**. The only protection is the pool's NetworkPolicy (`config/warmpool/warmpool-networkpolicy.yaml`), which admits these ports only from controller pods in `workload-variant-autoscaler-system` — so it holds only if your network plugin enforces NetworkPolicy, and anyone who can start a pod with the controller's labels in that namespace passes it |

**The gRPC port is the one to close.** Without the TLS flags it has no
authentication, and every call registers the ScaledObject it names, with the
trigger metadata the caller sends. Any pod that can reach `:9090` can therefore
register a workload or overwrite a registered one's trigger metadata —
`modelID`, `variantCost` — until KEDA's next call replaces it, and can read the
controller's decisions for any workload it names. So every shipped overlay
includes a NetworkPolicy (`config/components/external-scaler-networkpolicy`)
that admits `:9090` only from KEDA's namespace and leaves `:8443` and `:8081`
open. It is written in the admin phase (`make setup-prereqs`), and only when the
installer knows KEDA's namespace: `KEDA_NAMESPACE` set explicitly, the namespace
serving the external-metrics APIService when that Service is KEDA's, or the one
an existing `wva-external-scaler` policy admits, which is kept.
`KEDA_HELM_INSTALL=true` makes `KEDA_NAMESPACE`'s default count only while that
APIService is NotFound, because the install is then the one putting KEDA there.
When it would only be a guess, **no policy is applied**, the installer warns
that `:9090` is reachable from every Pod, and `make verify-deployment` prints
`No NetworkPolicy: scaler port open`; close it with
`KEDA_NAMESPACE=<namespace> NAMESPACE=<ns> make setup-prereqs` or the mutual-TLS
flags below. `make check-prereqs` lists the policy as optional.
`WVA_SCALER_NETWORKPOLICY=false` leaves it out on purpose; the controller-only
`make deploy-wva` never deletes an existing one. A wrong namespace
fails closed: KEDA is refused and nothing scales. The policy holds
only where the network plugin enforces NetworkPolicy; for the rest, serve the
scaler over TLS and require KEDA's client certificate
([configuration](configuration.md#advanced)). The rule it ships is
equivalent to:

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

`keda-system` is the shipped manifest's default; the Custom Metrics
Autoscaler on OpenShift usually runs in `openshift-keda`, which the OpenShift
overlays name. A policy applied by hand from an earlier version of this page,
`wva-external-scaler-from-keda`, adds to the shipped one rather than replacing
it; delete it ([upgrade](upgrade-and-uninstall.md#the-external-scaler-networkpolicy)). Applying an overlay by hand with KEDA anywhere else needs the
`namespaceSelector` edited. Narrow the `8443` rule to your Prometheus's
namespace if you want that port closed too; it is already authenticated. The
shipped policy admits `8081` from every source, because kubelet probes come
from the node, which no `from:` selector can name.

## Data handled

Metrics only. The controller reads Prometheus series, EPP and model-server
`/metrics`, and Kubernetes object metadata and status. It sends no inference
requests and reads no prompts or completions. With a warm pool, it calls the
engines' control endpoints (`/sleep`, `/wake_up`, `/is_sleeping`, `/health`),
never the request endpoints. The warm-pool **proxy** — a separate image running
inside pool pods — does forward live requests; it is part of the data plane,
not the controller.

The controller sends its own ServiceAccount token to Prometheus on every query.
The shipped Kubernetes config sets `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "true"`,
so that token goes to whatever answers at the configured address. Set it to
`false` and give a CA with `PROMETHEUS_CA_CERT_PATH`.

## Supply chain

| Image | Default | Recommendation |
| --- | --- | --- |
| Controller | `ghcr.io/ev-shindin/llm-scaling-manager:main`, a **floating** tag rebuilt on every push to main, pulled with `imagePullPolicy: Always` | pin a release tag or a digest: `IMG=ghcr.io/ev-shindin/llm-scaling-manager:<release>` or `IMG=ghcr.io/ev-shindin/llm-scaling-manager@sha256:<digest>` (the digest form has not been verified end to end through the installer yet) ([upgrade and uninstall](upgrade-and-uninstall.md#pin-the-version)). A floating tag means any pod restart can run different code. A registry host with a port (`host:5000/...`) is not parsed correctly by the installer yet; use a Kustomize overlay for that until it is fixed |
| Warm-pool launcher | `ghcr.io/llm-d-incubation/llm-d-fast-model-actuation/launcher:v0.6.4@sha256:...` | pinned by digest in `config/warmpool/` |
| Warm-pool proxy | `ghcr.io/ev-shindin/llm-scaler/warmpool-proxy:v11@sha256:...` | pinned by digest; a fork-built image |

`WVA_IMAGE_PULL_SECRET` names an existing pull Secret for a private registry; the
installer never handles registry credentials itself.

## Multi-tenancy

**Where policy comes from.** The limiters and the optimizer selection — the GPU
budget — come from the policy namespace when one resolves, and otherwise from the
controller's own `wva-scaling-policy-config`. Thresholds, tiers, per-model
entries and utilization-share weights come from the controller's own ConfigMap
**unless the managed namespace has its own** `wva-scaling-policy-config`: the
controller watches the scaling-policy ConfigMap in the namespace it manages and
reads it as a namespace-local map that **replaces** the controller's map for
that namespace's models (`internal/controller/predicates.go`,
`configmap_reconciler.go`, `internal/config/config.go`). Limiters and the
optimizer selection are never taken from a namespace-local map. The policy
source for the budget, run once at startup
(`internal/controller/policy_namespace.go`):

| Controller shape | Policy source |
| --- | --- |
| cluster-scoped | its own ConfigMap |
| namespace-scoped, out of reach (`WVA_NS` ≠ `WVA_WATCH_NS`) | its own ConfigMap, which lives in the admin's namespace |
| namespace-scoped in the namespace it manages, can read Namespaces (on Kubernetes, the grant `make setup-prereqs` creates) | the `wva.llmd.ai/policy-namespace` label on its namespace, else `wva-policy` if it exists, else its own ConfigMap |
| namespace-scoped in the namespace it manages, **cannot** read Namespaces — on **Kubernetes**, an install whose admin has not run `make setup-prereqs` since the grant was added | **its own ConfigMap** — the label and `wva-policy` are not seen; logged at Info with the fix, and `wva_policy_namespace_unverified` = 1 |

The startup log says which applied, with one gap. The label and well-known
branches log `Cluster policy comes from ...` at Info. The two "own ConfigMap"
branches log the same prefix at **V(1)** only, which the default verbosity does
not show; a self-managed controller that was denied the Namespace read
instead logs `Could not read the managed Namespace` and `Cannot tell whether a
cluster admin published policy` at Info, and sets
`wva_policy_namespace_unverified` to 1:

```bash
kubectl logs -n <controller namespace> deploy/wva-controller-manager \
  | grep -E 'Cluster policy comes from|Could not read the managed Namespace|Cannot tell whether'
```

`Cluster policy comes from the well-known namespace` (or `... the namespace this
one is labelled with`) is the one that means a policy namespace governs this
controller. No such line means its own ConfigMap does.

**What a tenant can influence**, beyond their own workloads:

| Lever | Effect | Status |
| --- | --- | --- |
| Owning the controller's Deployment (tenant-owned shape) | image, args, env and therefore every bound; and the pod runs as an SA with cluster-wide node read, Kueue read and the EPP token | by design — use the [out-of-reach shape](../guides/admin-cluster-setup/README.md#keeping-the-controller-out-of-the-tenants-reach) where the bound must hold |
| The controller's own `wva-scaling-policy-config`, in the tenant-owned shape | thresholds, tiers, per-model entries; **and the limiters**, wherever no policy namespace resolves (including a namespace-scoped Kubernetes install without the Namespace read, which `wva_policy_namespace_unverified` flags) | by design where no policy is published; on Kubernetes a cluster admin re-runs `make setup-prereqs` so a published one is seen |
| A `wva-scaling-policy-config` in their own namespace, **in any shape** — out-of-reach included | read as a namespace-local map that replaces the controller's thresholds, tiers, per-model entries and weights for their models. It cannot set limiters or the optimizer | by design — the out-of-reach shape protects the budget, not the per-model tuning |
| `weightClass` / `weight` on their models' entries | under a shared utilization-share budget, a model set to `critical` (weight 4 in the default classes) takes **4x** the headroom of a `standard` one, from the other models' share | by design — keep weights in admin-owned policy, and treat a tenant-written namespace-local entry as their claim, not yours |
| `minReplicaCount` on their ScaledObjects | under a shared utilization-share budget, a replica floor is held first, so a higher floor takes from the other models' share | by design |
| `maxReplicaCount`, or a second KEDA trigger | bypasses the limiter: the HPA takes the maximum across triggers | by design — a `ResourceQuota` is the bound that holds |
| A LocalQueue pointing at any ClusterQueue | the controller grants their namespace that ClusterQueue's quota only when its `namespaceSelector` admits the namespace, with Kueue's semantics (none admits nothing, `{}` admits all); a Namespace whose labels cannot be read gets nothing from a selector-bearing queue | closed by the selector. An install whose Kueue reader lacks `get namespaces` (set up before it was added) withholds every selector-bearing queue until a cluster admin re-runs `make setup-prereqs` |
| An InferencePool selector | decides which labels the controller writes onto warm-pool pods in that namespace | by design, minus the guarded identity labels |
| A Deployment labelled `app.kubernetes.io/name=workload-variant-autoscaler` | `make enable-physical-limiter` treats it as a controller only if it runs as the controller ServiceAccount (`WVA_CONTROLLER_SA`) and has a `manager` container from a controller image repository (`WVA_CONTROLLER_IMAGE_REPOS`; a namespace named in `WVA_LIMITER_TARGETS` relaxes the image check). Anything else is `SKIPPED`, granted nothing, and listed as NOT bound under the banner. A verified one gets its ServiceAccount node read (and Kueue read), a `kubectl rollout restart` of that named Deployment when it must re-resolve its policy namespace (it could not read its Namespace, or started before the policy namespace existed), and `limiters` in the ConfigMap its policy resolves to, the `wva.llmd.ai/policy-namespace` label honoured | signals, not proof: any tenant can satisfy them. Where tenants are not trusted, run their controllers out of reach and name exactly those namespaces in `WVA_LIMITER_TARGETS` |
| Reaching `:9090` | register or overwrite trigger metadata; read decisions | closed by the shipped NetworkPolicy, above, where it was applied and the network plugin enforces it. Scaler mutual TLS closes it on any network only with `--external-scaler-client-san` pinning KEDA's identity, and only where an admin owns the controller's Deployment: in the tenant-owned shape the tenant owns its args, and so the TLS flags |

**For a tenant you do not trust, bound the namespace at admission**: a
`ResourceQuota` on `requests.nvidia.com/gpu`. The limiter bounds what the
scaling manager asks for; the quota bounds what the cluster admits
([how](../guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)).

## Fail-open and fail-closed

| Condition | Behaviour | Direction |
| --- | --- | --- |
| No limiter declared (the shipped policy) | scaling is bounded only by each workload's `maxReplicaCount`; scale-from-zero wakes without a capacity check | **open** |
| A limiter entry that fails validation | the whole `default` policy is rejected, thresholds included, and no limiter is built; logged at ERROR | **open** |
| Policy namespace resolved, its ConfigMap exists but cannot be read | the controller refuses to start (`refusing to start unbounded`) | closed |
| Policy namespace resolved, no ConfigMap in it | empty policy: no limiters | **open** |
| Cannot tell whether `wva-policy` exists (an API error other than NotFound or Forbidden) | refuses to start | closed |
| Forbidden reading Namespaces (namespace-scoped, Kubernetes, no `make setup-prereqs` grant) | not treated as "no policy namespace": it uses its own ConfigMap, logs `Cannot tell whether a cluster admin published policy` with the fix at Info, and sets `wva_policy_namespace_unverified` = 1 (alert `WVAPolicyNamespaceUnverified`) | **open**, and reported |
| A ClusterQueue with a `namespaceSelector`, the managed Namespace's labels unreadable | that queue's grant is withheld from the namespace | closed |
| `gpu-inventory` declared, node read denied | NotReady via the `gpu-policy` readiness check; `wva_node_access_denied` = 1 | closed |
| Quota entry with no entry for an accelerator type | that type is denied | closed |
| Kueue unreadable, never read since start | the static quota entry alone; with `-1` that is no cap | **open** |
| Kueue unreadable after a good read | the last good snapshot stays in force | held |
| Two limiter kinds declared | both are built; a decision must fit both | closed |
| One limiter unavailable in a cycle (usage not observed yet, or an error) | skipped and named in the log; the others still bound. Only when every one is unavailable is a decision unchecked; scale-from-zero then uses its last constraints if under 5 minutes old | held |
| The installer cannot tell where KEDA runs | no external-scaler NetworkPolicy is applied; the installer warns and `make verify-deployment` reports `:9090` open | **open**, and reported |
| The external-scaler NetworkPolicy names a namespace KEDA does not run in | KEDA's calls are dropped; ScaledObjects go not-Ready and nothing scales | closed |

## Production hardening checklist

- [ ] Prometheus TLS verified: `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "false"` and `PROMETHEUS_CA_CERT_PATH` set (the controller sends its token there).
- [ ] A real certificate for `:8443`: `--metrics-cert-path` (OpenShift does this; on Kubernetes the shipped ServiceMonitor also skips verification).
- [ ] Controller image pinned to a tag or digest, not `:main`.
- [ ] The shipped external-scaler NetworkPolicy in place (`WVA_SCALER_NETWORKPOLICY` not `false`), naming the KEDA operator's namespace (`make verify-deployment` prints no NetworkPolicy warning), on a network plugin that enforces it — or scaler mutual TLS (`--external-scaler-client-ca` with `--external-scaler-client-san`) on a controller Deployment an admin owns.
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
