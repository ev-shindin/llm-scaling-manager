# Watching what the scaling manager decides

The dashboard, the metrics that answer specific questions, and how to read the
logs. If the scaling manager is installed but you cannot tell what it is doing, this is the page.

> Part of the [the scaling manager deployment guide](../../deploy/). For whether the install
> worked at all, see [After the install](operations.md).

## Watching what the scaling manager decides

The scaling manager writes no custom resource. Its decisions are visible in three places, and you
want them in this order: the **dashboard** for whether things are healthy, the
**metrics** for a specific question, the **logs** only for why a single decision
came out the way it did.

### The operational dashboard

The install publishes a Grafana dashboard, *llm-scaling-manager Operational Dashboard*, covering
the whole pipeline: GPU discovery, metric collection health and freshness,
saturation, capacity, scaling decisions and limiter impact.

```bash
kubectl port-forward -n <monitoring-namespace> svc/kube-prometheus-stack-grafana 3000:80
# then http://localhost:3000  (default admin password: prom-operator)
```

It ships as a labelled ConfigMap, so **an existing Grafana picks it up** — you do
not need to have let this install deploy Prometheus — *provided you publish into a
namespace that Grafana's sidecar actually watches*:

```bash
# publish into whichever namespace your Grafana's sidecar watches
DASHBOARD_NS=my-monitoring ./deploy/install.sh   # DEPLOY_PROMETHEUS=false is fine
```

A sidecar watches only the namespaces it is configured for — `ALL` for a Grafana
this installer deployed, often just its own for one you already had — so a wrong
`DASHBOARD_NS` can produce a ConfigMap nobody reads. The install checks and warns;
see [If you cannot write to the monitoring namespace](#if-you-cannot-write-to-the-monitoring-namespace).

Skip it entirely with `DEPLOY_OPERATIONAL_DASHBOARD=false`.

Read the panels top-down. The upper row answers "is the scaling manager seeing the cluster at all";
until those are healthy, the scaling panels below them are meaningless.

#### Who owns the dashboard

The dashboard is published as a ConfigMap into a **monitoring** namespace, which
is normally not the namespace the scaling manager runs in — so publishing the shared one is a
**cluster-admin** action even though it happens during the tenant install step.

| | can do |
| --- | --- |
| Cluster admin | publish and update the shared dashboard in the monitoring namespace; decide the datasource, and therefore who sees what |
| Namespace admin | use the shared dashboard through their `?var-namespace=` link; or import the JSON into Grafana by hand; or publish into a namespace **their Grafana already watches** — see the caveat below |

A namespace admin running `make deploy-wva` without rights to the monitoring
namespace gets a message saying so — the install continues, and only the
dashboard step is skipped. Nothing about the scaling manager's scaling depends on it.

#### If you cannot write to the monitoring namespace

`DASHBOARD_NS=<your-namespace>` publishes a private copy into your own namespace,
which needs no cluster rights at all. **Whether it renders depends on the Grafana**,
so the installer checks and tells you:

- **A Grafana this installer deployed watches every namespace.** The bundled chart
  (grafana 12.10.4 via kube-prometheus-stack) sets the dashboard sidecar's
  `searchNamespace` to `ALL`, verified on a live install. So `DASHBOARD_NS` simply
  works, and the install says so.
- **A Grafana you already had may watch only its own namespace.** That is the
  sidecar's other common configuration, and there is no error when it applies: the
  ConfigMap is created, is correctly labelled, and is silently never read. The
  install detects this and warns rather than reporting success.

Ways out, cheapest first:

1. **Import the JSON by hand.** `deploy/grafana/operational-dashboard.json` is an
   ordinary Grafana dashboard. Anyone with Grafana editor rights can import it
   through the UI — *Dashboards → New → Import* — with **no Kubernetes permission
   at all**. This is the fastest self-service route and the right answer for most
   namespace admins.
2. **Use the shared dashboard, scoped to you.** If an admin has already published
   it, you need nothing: open the link the install prints,
   `…/d/wva-operational/wva-operational-dashboard?var-namespace=<your-namespace>`.
3. **Ask for the sidecar to watch all namespaces** — only needed for a
   pre-existing Grafana that does not already. One change,
   `grafana.sidecar.dashboards.searchNamespace=ALL` or an explicit list, after which
   `DASHBOARD_NS=<your-namespace>` works for every tenant, self-service.
4. **Run your own Grafana in your namespace**, then publish to it with
   `DASHBOARD_NS=<your-namespace>`. Fully self-service and needs no admin, at the
   cost of operating another Grafana.

On OpenShift, note that user-workload monitoring ships **no Grafana** — so options
1 and 4 are the practical ones there unless your cluster runs its own.

#### One dashboard, many installs

The ConfigMap has a **fixed name in whatever namespace you publish it to**, so
every install on a cluster writes the same object. That is deliberate — the
dashboard is generic and driven by variables, and one copy per tenant would fill
the picker with identical dashboards — but it has consequences worth knowing.

**The first row is the admin's view.** *Installs present* counts llm-scaling-manager Deployments
from kube-state-metrics; *Controllers reporting metrics* counts the ones whose
metrics actually arrive. A gap between them is an install nobody is scraping,
and it is the difference that matters: a controller that is not scraped looks
exactly like a controller that is not scaling. *Scrape targets DOWN* names how
many, and the **Controller** variable says which.

**Your namespace's view is a link, not a default.** A Grafana variable's default
lives inside the dashboard JSON, and this object is shared by every install on
the cluster — so there is no per-tenant default to set: pinning it would mean
everyone sees whichever tenant installed last. Each namespace gets its own entry
point into the one dashboard instead, which the install prints:

```text
<your-grafana>/d/wva-operational/wva-operational-dashboard?var-namespace=<your-namespace>
```

Drop the query string for the cluster-wide view. If you would rather have your
own dashboard object — pinned, private, nobody else writing it — publish a copy
into your own namespace:

```bash
DASHBOARD_NS=<your-namespace> make deploy-wva   # pinned to that namespace
```

This renders whenever the Grafana's sidecar watches that namespace, which is the
case for one this installer deployed (`searchNamespace: ALL`). The install verifies
it and warns if not, in which case import the JSON by hand instead.

Note the trade-off: the shared object is the default on purpose. One generic,
variable-driven dashboard serves every install, whereas a copy per tenant fills the
picker with near-identical entries.

**Namespace label:** `wva_*` metrics carry both `namespace` and
`exported_namespace`. `exported_namespace` is the *workload's* namespace and
`namespace` is the *controller's* — the same for a namespace-scoped install,
different for a cluster-scoped one, where grouping by `namespace` would collapse
every workload onto the controller's namespace. The dashboard defaults to
`exported_namespace` for that reason; the benchmark dashboard defaults to
`namespace`, because vLLM metrics carry only that one.

**Versions.** The ConfigMap records the scaling manager version that published it, and an
older install will not overwrite a newer dashboard — it says so and leaves it.
Panels for metrics a given version does not emit stay empty for that install:
on a cluster running several versions, an empty panel may mean "older
controller", not "nothing happening". Force a republish with:

```bash
kubectl delete configmap wva-operation-dashboard -n <dashboard-namespace>
```

**Who can see what is the datasource's decision, not the scaling manager's.** On OpenShift,
`thanos-querier:9091` is cluster-monitoring-view — anything querying through it
reads every namespace, whatever scope the scaling manager was installed with. For a tenant
Grafana that must only see its own namespace, point the datasource at
`thanos-querier:9092`, which enforces per-namespace RBAC.

That is also what makes the shared dashboard safe rather than merely tidy: with
a per-namespace datasource the **Namespace** variable only lists namespaces the
viewer may read, so "All" already means "all of mine". Pinning a default never
provided isolation — it only hid names from the dropdown while every query still
ran with the datasource's own permissions.

### How the dashboard is organised

The panels sit in three rows, and the row says what a panel is scoped to:

| row | scope | what it answers |
| --- | --- | --- |
| **Fleet health — all namespaces** | every namespace the scaling manager manages | Is the scaling manager itself working? Installs present, controllers reporting, scrape targets down, collection and optimization timings. |
| **Selected namespace: `$namespace`** | the **Namespace** variable | What did the scaling manager do here? Replicas, capacity, saturation, scaling decisions, limiters, wake-from-zero. |
| **Serving** (collapsed) | the **Namespace** variable | What the workload experienced: TTFT, inter-token latency, router and per-replica queues, KV utilization. Mostly from the EPP, so it reads the same for vLLM and SGLang. |

The split exists because the two audiences differ. A panel in the fleet row does
not change when you pick a namespace, and one in either lower row does — before
the rows, that was invisible, and a "Models blocked from scaling" stat counting
the whole cluster sat beside a chart of the same thing counting one namespace.
`make dashboards-check` fails if a panel's query disagrees with the row it sits
under.

### When the dashboard shows nothing

- **No Grafana at all** — `services "kube-prometheus-stack-grafana" not found`
  means the install ran with `DEPLOY_OPERATIONAL_DASHBOARD=false`, or against a
  cluster whose Grafana it did not deploy. Point the port-forward at your own.
- **Panels render but say No Data** — hover the panel's `i` icon first; several
  say which metric is missing and why. Then test the Prometheus datasource under
  *Connections / Data sources*.
- **The dashboard is not in the picker** — it lives in the `wva-operation-dashboard`
  ConfigMap. If that object exists in a namespace your Grafana's sidecar does not
  watch, nothing reads it: see [If you cannot write to the monitoring
  namespace](#if-you-cannot-write-to-the-monitoring-namespace).
- **Every variant shows the controller's namespace** — toggle the
  `$namespace_label` variable at the top of the dashboard between
  `exported_namespace` (the default, correct when Prometheus scrapes with
  `honorLabels: false`) and `namespace`.
- **You want an editable copy** — the shipped dashboard is read-only. Import
  `deploy/grafana/operational-dashboard.json` under a name of your own, or
  publish a private copy with `DASHBOARD_NS=<your-namespace>`.

### The metrics that answer specific questions

All are exposed by the controller and scraped by the ServiceMonitor the install
creates. Full list: [Prometheus metrics](prometheus.md).

**Is the scaling manager working at all?**

| metric | healthy | what it means when it is not |
| --- | --- | --- |
| `wva_models_processed` | > 0 | no workload has registered — no ScaledObject names this scaler |
| `wva_metrics_pods_discovered` | > 0 per model | the scaling manager cannot find the pods behind a model |
| `wva_metrics_freshness_status{status="fresh"}` | equals the pod count | pods sitting in `status="stale"` or `"missing"` are being decided on with old data, or none at all |
| `wva_errors_total` | flat | rising means the optimization cycle is failing |

`wva_metrics_freshness_status` is a per-`(variant_name, status)` gauge holding *how
many pods* are in that state — not a 0/1 flag. Compare the series:

```promql
wva_metrics_freshness_status{status!="fresh"} > 0
```

**Why is nothing scaling up?** — the silent-stall causes, each of which looks
like "the scaling manager is fine" everywhere else:

| metric | meaning |
| --- | --- |
| `wva_node_access_denied` | `1` = a GPU limiter is configured but nodes are unreadable. Every variant gets no budget and **will not scale up**. |
| `wva_decisions_limited_total` | rising = a limiter is capping decisions. Pair with `wva_available_gpus` and `wva_spare_capacity`. A fully spent quota can leave it flat: compare what the quota allows with what the model holds. |
| `wva_variant_at_max_replicas` | `1` = the variant sits on its own `maxReplicaCount`. With utilization still above the scale-up threshold, raise `maxReplicaCount`; more GPUs will not help. |
| `wva_unmeasured_queue` | non-zero = requests queue for a model with no attributed replica: the pods serving it are not scraped or not attributed, so nothing scales. |
| `wva_unattributed_gpus` | GPUs in use that could not be charged to a pool — usually a workload whose accelerator did not resolve |

**Is the decision itself sane?** — `wva_desired_replicas` vs `wva_current_replicas`,
with `wva_saturation_utilization` and `wva_analyzer_demand` / `wva_analyzer_target`
showing what drove it. A desired that never becomes current is an actuation
problem (KEDA, the HPA, or the workload), not a decision problem. A desired
that looks too low is worth checking against `wva_analyzer_observed_replicas`:
how many of the variant's replicas reported metrics in the analyzer's last
cycle, not capped at the scale target. Compare it with the target's **ready**
replicas, not with `wva_current_replicas`, which also counts pods still
starting and legitimately absent during every scale-up. Below ready means a
Pod's metrics were missing from the cycle (a scrape gap, and the low desired is
correct for what was seen); above it means something the scale target does not
own is serving the model.

**Tenant: query Prometheus.** The simplest way to read these for your own
namespace is the cluster's Prometheus (or Thanos on OpenShift), filtered by
`exported_namespace="<your-namespace>"`. On OpenShift, the tenant port of
`thanos-querier` (9092) enforces per-namespace access.

**Tenant whose controller runs elsewhere.** In the out-of-reach shape the admin
installs your controller in `wva-<your-namespace>`, which you cannot read. Its
series carry `namespace="wva-<your-namespace>"` (the scrape target's namespace)
with yours only in `exported_namespace`, and the OpenShift tenancy port filters
on `namespace`, so through port 9092 you see none of them. What you can read
yourself, in your own namespace:

```bash
kubectl get events -n <your-namespace> --field-selector involvedObject.kind=ScaledObject
kubectl get scaledobject,hpa -n <your-namespace>
kubectl describe hpa -n <your-namespace> keda-hpa-<so-name>   # desired vs current, conditions
```

For anything more, ask the admin for read access to `wva-<your-namespace>`
(pods and `pods/log`, for the controller log), or for a Prometheus or Grafana
datasource that can query the controller's series by `exported_namespace`.

**Straight off the controller.** The endpoint is HTTPS and authenticated: a
plain `curl` gets `401 Unauthorized`. It needs a bearer token for an identity
allowed `get` on the non-resource URL `/metrics`, which only a ClusterRole and
ClusterRoleBinding can grant (cluster admin). The install's `epp-metrics`
token is allowed it once the admin's setup step has created that binding:

```bash
TOKEN=$(kubectl get secret wva-epp-metrics-token -n "$NS" -o jsonpath='{.data.token}' | base64 -d)
kubectl port-forward -n "$NS" svc/wva-controller-manager-metrics-service 8443:8443 &
curl -sSfk -H "Authorization: Bearer $TOKEN" https://localhost:8443/metrics | grep -E '^wva_'
kill %1
```

Without the token, or before the admin's binding exists, it answers `401` and the grep prints nothing.

**What would the utilization-share optimizer do, and is it doing it?** — only
when an [`optimizer:` block](scaling-policy.md#optimizer-cluster-default-only-live)
is set. Full list, with labels:
[utilization-share metrics](prometheus.md#utilization-share-optimizer-metrics).

| metric | meaning |
| --- | --- |
| `wva_utilization_share_mode{mode=...} == 1` | which state it is in: `off`, `invalid` (the block did not validate), `shadow` or `active`. Published by the leader every cycle, from its first, including cycles with no active model |
| `wva_utilization_share_actionable == 1` | roles a move would fix now. Published in shadow mode too: this is what shadow mode is for |
| `wva_utilization_share_replicas_to_move` | replicas the target would move, per group |
| `wva_utilization_share_spare_gpus` | below `0` = the quota cannot cover every role's need; rebalancing cannot fix that |
| `wva_utilization_share_in_flight{state=...}` | transfers per group still `releasing` or `filling`; while it acts |
| `wva_utilization_share_headroom_capped == 1` | the models' recent peaks do not fit the quota together, so each keeps at most `maxHeadroomReplicas` (times its weight) of headroom and the rest is returned; `0` while the spare is spread by weight. After a restart that finds no usable stored need history (the ConfigMap `wva-utilization-share-history`) it is on until a `flappingWindow` of history exists unless the models' `maxReplicas` fit the quota together. While it acts, and not during the restart quiet period, when the planner does not plan |
| `wva_utilization_share_starts_total` | what started, by `kind` (`transfer`, `fill`, `refill`, `return`) and `skipped_waits`. A rising `return` with no rising `fill` or `transfer` is models giving headroom back to the quota |
| `wva_utilization_share_transfers_total` | by `outcome`: `done` is healthy; rising `aborted`, `fill-timeout` or `wrong-pod` means transfers are not landing. Every outcome is published at `0` while a group acts, so `increase()` counts the first one too |
| `wva_model_scaling_blocked{reason=...}` | which model the optimizer is holding back, and why ([reasons](prometheus.md#wva_model_scaling_blocked-reasons-set-by-the-utilization-share-optimizer)); set only while it acts. No `role` label: on a P/D model, read `wva_utilization_share_headroom` / `_actionable` by `role` to see which role it is |

Alerts worth having once it is configured (adjust the `for:` to a few of your
release times; `wva_utilization_share_effective_seconds{param="release-timeout"}`
is one):

```promql
# The optimizer block is broken: today's optimizer is running instead. This
# includes a block whose limiters: list was removed (no budget to share).
wva_utilization_share_mode{mode="invalid"} == 1

# You configured it, but the controller reports it off: no optimizer: block
# (it was edited out by mistake) or an empty type.
wva_utilization_share_mode{mode="off"} == 1

# Transfers are not landing: donors that do not release, receivers that do not
# fill, or donors losing a pod other than the marked one (wrong-pod is detected
# only for transfers planned with node information). The threshold (3) is an
# example; check the donors' scale-down windows and pods, and the blocked reasons.
# Each outcome's series exists at 0 from the group's first acting cycle, so the
# first transfer of an outcome is counted by increase(). After a restart the new
# controller creates them at 0 and counts its first cycle's outcomes on the
# next. A release that timed out while no controller ran is not counted: its
# mark is removed, and the log line "removed a transfer mark older than the
# release timeout" names the pod.
sum by (accelerator_type, scope) (
  increase(wva_utilization_share_transfers_total{outcome=~"aborted|fill-timeout|wrong-pod"}[1h])
) > 3

# Releases are running close to their bound: the p90 release time over the last
# hour exceeds 80% of the release timeout in force.
histogram_quantile(0.9, sum by (le, accelerator_type, scope) (
  rate(wva_utilization_share_release_seconds_bucket[1h])))
> on (accelerator_type, scope)
0.8 * max by (accelerator_type, scope) (
  wva_utilization_share_effective_seconds{param="release-timeout"})

# A model held back by a condition that holds until someone changes something
# (alert with for: of a few release timeouts). marks-unreadable freezes every
# model of the group until the controller can read its transfer marks again.
wva_model_scaling_blocked{reason=~"no-compatible-donor|floors-exceed-quota|marks-unreadable"} == 1

# A role stuck short while the optimizer acts: holding less than three quarters
# of its need (headroom below -0.25). Alert with for: of about one reversal
# hold, wva_utilization_share_effective_seconds{param="reversal-hold"} (about
# 12 minutes with a 300 s scale-down window): a transfer should have reached it
# by then. Read the model's wva_model_scaling_blocked reason: reversal-hold
# for the whole hold means no other role was lightly loaded enough to give at once, often a
# model whose burst just ended. On a short quota (spare_gpus below 0) it fires
# by design: rebalancing cannot cover every need.
wva_utilization_share_headroom < -0.25
  and on() (wva_utilization_share_mode{mode="active"} == 1)

# Reasons that expire by themselves and come back: release-taken and
# release-shape-mismatch last one release timeout after a fill timed out, and
# donor-not-steerable lasts only its back-off (quiet-period lasts one fill
# timeout after a restart and is expected; do not alert on it). A long for: may
# never fire on them; alert on how long in a window they were present instead.
# The series exists only while its reason does, so count it at a fixed 1m
# step (a subquery), which does not depend on the scrape interval: more than
# 60 minutes in 6h.
sum by (exported_namespace, model_name, reason) (
  count_over_time(wva_model_scaling_blocked{reason=~"release-taken|release-shape-mismatch|donor-not-steerable"}[6h:1m])
) > 60
```

Do not alert on `wva_utilization_share_in_flight{state="releasing"} > 0` with a
long `for:`: one release cannot outlive its release timeout (it is aborted
then), and a busy group keeps the series above zero with transfers that each
land on time.

#### Events on the models' scale targets

While it acts, the optimizer records each transfer as Kubernetes Events on the
Deployment or LeaderWorkerSet it moves, so a model's owner can read why it shrank
or grew without the controller's log:

```bash
kubectl describe deployment <name> -n <namespace>     # or: leaderworkerset <name>
kubectl get events -n <namespace> --field-selector reason=UtilizationShareGiving
```

| reason | type | on | when |
| --- | --- | --- | --- |
| `UtilizationShareGiving` | Normal | donor | a transfer started: it gives one replica. Names the pod (or pods) that go, whom it gives to, and why: the receiver is below its need, or to even out headroom by weight. A move that skipped the waits says "without the usual waits: it is at its scale-up threshold and this model is nearly idle (stabilization.skipWaitsWhen)". A donor in a set names the receiver its set funds and how many other donor replicas fund it; a refill names the quota group's scale-from-zero reserve (`reserveGPUs`); a return names "the quota group's unassigned GPUs", "because it holds more headroom than maxHeadroomReplicas gives it while no model is short, and stays calm without it; the replica drains after its scale-down window, and is kept if its demand comes back before then; a model whose demand rises takes the GPUs at once" -- the reason a model shrank while no other model grew. |
| `UtilizationShareReceiving` | Normal | receiver | a transfer started for it; it is raised once the donor's replica is released |
| `UtilizationShareReceived` | Normal | receiver | the transfer landed: it holds the GPUs. An idle fill that lands says it received them "from the quota group's unassigned GPUs", and why: "because its demand rose past what it held", or "as headroom (maxHeadroomReplicas); given back when this model no longer needs it" -- the reason a model grew with no load to grow it |
| `UtilizationShareCancelled` | Normal | donor and receiver | the transfer was called off because demand reversed; the donor's replica count is restored. A return called off says "this model's demand came back, so it keeps the replica it was giving back" |
| `UtilizationShareRedirected` | Normal | receiver | the GPUs it was to receive went to a model waking from zero; it is planned again |
| `UtilizationShareReleaseAborted` | Warning | donor | its replica was not released within the release timeout; its count is restored, and the Event gives the time before which it is not asked to give again (the wait doubles with each abort in a row, up to 16 release timeouts). When the transfer was called off because its donor set could not complete (another member broke, or this donor released while a contributor did not), the Event says so instead, and sets no back-off: it was not this donor's failure |
| `UtilizationShareWrongPod` | Warning | donor | a pod other than the marked one was removed (a rollout, an eviction, another scale-down); the donor stays one replica lower and the receiver is not raised |
| `UtilizationShareFillTimedOut` | Warning | receiver | its new replica did not take the released GPUs within the fill timeout; it keeps its target |
| `UtilizationShareDonorNotSteerable` | Warning | donor | it was asked to give, but the pod that would go could not be chosen; the Event carries the cause (a pod not Ready or not yet scheduled, a rollout, a sibling's deletion cost leaving no room, the patch failing) and the time before which it is not asked again. Not recorded for a donor that has simply given every pod it has to transfers still in flight |

Events expire: the API server keeps them for an hour by default (its
`--event-ttl`), so they explain what happened recently, not a model's history.
For history, use the counters above and the controller log.

An Event names the other model only when it is in the same namespace; otherwise
it says "a model in another namespace of its quota group", so one tenant's Events
never name another tenant's models. A P/D model is named with its role, for
example `model llama (decode)`; an aggregated model by its name alone.

### Events

The controller records Kubernetes Events for a model on its **ScaledObject**
(the scaling manager has no object of its own), utilization-share transfers on
the **scale target** (Deployment or LeaderWorkerSet), and one config warning on
the **ConfigMap**. Read them per namespace:

```bash
kubectl get events -n <model-namespace> --field-selector involvedObject.kind=ScaledObject
kubectl describe scaledobject <name> -n <model-namespace>     # Events at the bottom
kubectl get events -n "$NS" --field-selector involvedObject.kind=ConfigMap
```

Every reason the controller emits:

| reason | type | object | meaning | action |
| --- | --- | --- | --- | --- |
| `ScaledUp` | Normal | ScaledObject | the controller decided to add replicas; the message is the decision reason | none |
| `ScaledDown` | Normal | ScaledObject | the controller decided to remove replicas | none |
| `ScaledToZero` | Normal | ScaledObject | the controller decided to park the model at zero | none; if unexpected, check the model's `scaleToZero` policy |
| `ResourceConstrained` | Warning | ScaledObject | a limiter (GPU inventory or quota) capped the decision below what demand asked for | add GPUs or raise the quota, or accept it. A fully spent quota can record nothing: see [GPUs held](troubleshooting.md#gpus-held-or-quota-spent) |
| `MetricsUnavailable` | Warning | ScaledObject | the model's metrics went from available to unavailable; recorded on that transition only | [Metrics health](metrics.md#the-metricsunavailable-event) |
| `OptimizationFailed` | Warning | ScaledObject | the controller could not publish the replica metric KEDA reads (`Failed to emit metrics for external autoscalers`) | read the controller log for the error |
| `UnattributedReadyPods` | Warning | ScaledObject | the scale target has Ready pods, but none of their metrics were attributed to it | check the pods' ownerReferences reach the scale target; on FMA, scrape the launcher pods ([FMA launcher pods](operations.md#fma-launcher-pods)) |
| `AcceleratorNotResolved` | Warning | ScaledObject | the workload constrains no accelerator, so its GPUs are charged to no pool | set a GPU product `nodeSelector` or `nodeAffinity` on the workload |
| `ThroughputAnalyzerRestartRequired` | Warning | ConfigMap (global scaling policy, controller namespace) | the throughput analyzer was enabled or disabled in config, but analyzer registration is fixed at startup | restart the controller: `kubectl rollout restart -n "$NS" deploy/wva-controller-manager` |
| `UtilizationShareGiving`, `UtilizationShareReceiving`, `UtilizationShareReceived`, `UtilizationShareCancelled`, `UtilizationShareRedirected` | Normal | Deployment / LeaderWorkerSet | a utilization-share transfer started, landed, was called off or redirected | none; [details](#events-on-the-models-scale-targets) |
| `UtilizationShareReleaseAborted`, `UtilizationShareWrongPod`, `UtilizationShareFillTimedOut`, `UtilizationShareDonorNotSteerable` | Warning | Deployment / LeaderWorkerSet | a transfer did not land | [details](#events-on-the-models-scale-targets) |
| `BorrowedCapacityReleased` | Normal (cause `lender-waiting`), Warning (cause `placement-timeout`) | Deployment / LeaderWorkerSet | the scaling manager lowered the model to give borrowed Kueue GPUs back: to the lender ClusterQueue the message names, or because borrowed replicas did not place in time. One per release | none, normally; [replicas dropped](troubleshooting.md#replicas-dropped-under-kueue-borrowing) |

At most one Event per model variant per cycle, except `ResourceConstrained`,
which can join a `ScaledUp`. The API server keeps Events for about an hour
(`--event-ttl`), so they explain recent behavior, not history.

### The logs

Useful when a metric tells you *which* model is wrong and you want to know *why*.

| grep for | tells you | level |
| --- | --- | --- |
| `scaling-decision` | what the scaling manager decided for a model, and the replica counts | Info |
| `Effective scaling policy` | which policy tier a model resolved to | Info |
| `GPU limiter (re)built from config` | a `limiters:` edit took effect, live | Info |
| `Utilization share:` | every line the utilization-share optimizer writes; narrow with the rows below. Group lines carry `accelerator` and `scope` as the metrics label it (`cluster` for the cluster group), so a line joins its group's series | Info / Error |
| `Utilization share: would rebalance (shadow)` | shadow mode found moves it would make; carries `actionable` and `frozen`. Logged when the set of actionable roles changes, not every cycle | Info |
| `Utilization share: rebalancing` | the optimizer is acting on a group with actionable roles; same fields, same once-per-change rule | Info |
| `Utilization share: transfer` | a transfer started, ended (with its `outcome`), was cancelled, or was redirected to a wake. `transfer started` carries `urgent`, `skippedWaits` and `returned` (`true` for a spare given back to the quota, which has no `receiver`) | Info |
| `Utilization share: the models' recent peaks`, `Utilization share: the models' peaks are not known yet` | the headroom cap turned on or off; carries `peakClaimGPUs` and `budget`. On: `cause=peaks-exceed-budget` (`recent peaks do not fit the quota together`) or `cause=peaks-unknown` (`peaks are not known yet`, with `unknownPeaks`: no model's peak is known, as after a restart with no history or for a new group), each with `maxHeadroomReplicas`. Off: `fit the quota together again`. Logged when it flips, and on a group's first acting cycle if capped | Info |
| `Utilization share: spare returned to the quota` | a model's replica above its need plus its headroom (`maxHeadroomReplicas`) was released back to the quota group's unassigned GPUs; carries `donor` and `gpus` | Info |
| `Utilization share: could not mark a donor pod` | a transfer was not started; the donor backs off (`donor-not-steerable`, and a `UtilizationShareDonorNotSteerable` Event on it) | Error |
| `Utilization share: donor has nothing left to give` | a transfer was not started because every pod the donor could give is already given to a transfer in flight; it is held until one of them lands, or for one release timeout, with no back-off, blocked reason or Event | **`-v=4`** |
| `Utilization share: donor's workload is changing` | a transfer was not started because the donor is mid-rollout, has a pod not yet created, scheduled or Ready, or an LWS group being replaced; it is held one release timeout, with no back-off, blocked reason or Event | **`-v=4`** |
| `Utilization share: removed a transfer mark older than the release timeout` | a restarted controller found a mark whose transfer it can no longer judge; the mark is removed and not counted | Info |
| `Utilization share: need history restored` | a rebuilt group read its stored need history back from the ConfigMap `wva-utilization-share-history`; carries `group`, `roles` (roles restored), `rolesKnownNow` (roles whose peak is known at once), `peaksKnownAt` (when the first restored role's peak is known), `writtenAt` and `writtenBy`. `stored need history not used` says why an entry was ignored (`reason`; `older than its window` when every sample has aged out) | Info |
| `Utilization share: could not read the stored need history`, `Utilization share: could not store the need history` | the need history store failed for `group` (typically Forbidden: the Role `wva-utilization-share-history-role` is missing, as on an install upgraded before an admin re-ran `make setup-prereqs`). After a failed read nothing is restored or written for that group until a read succeeds; logged once per group and change, `the need history store works again` on that group's recovery | Error |
| `Utilization share: need history truncated; the roles left out count at their ceiling after a restart` | a group's entry was over its bound (128 roles, or its equal share of 900 KiB): the roles with the smallest stored peaks were left out; carries `group`, `droppedRoles`, `dropped` (the first ten). Logged once per change | Info |
| `Utilization share: the need history ConfigMap was deleted; every model's need history is forgotten` | the ConfigMap was deleted after the controller had read or written it: the flush. Every model's peak is unknown again for a `flappingWindow` | Info |
| `Utilization share: invalid optimizer block` | the block did not validate; today's optimizer runs | Error |
| `Utilization share: evaluation` | the per-group table, every cycle | **`-v=4`** |
| `Collected replica metrics` | metrics are arriving | **`-v=4`** |

The controller runs at `-v=2` by default, so `Collected replica metrics` prints
nothing and grepping for it proves nothing either way. Use
`wva_metrics_pods_discovered` and `wva_metrics_freshness_status` for that question
instead — they are always on. If you do want the line, raise verbosity on the
container and put it back afterwards:

```bash
kubectl patch deployment -n "$NS" wva-controller-manager --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--v=4"}]'
```

Put it back when done. The patch above made a new rollout, so undoing it
returns the template from before it (and drops any other change made to the
Deployment after the patch):

```bash
kubectl rollout undo deployment/wva-controller-manager -n "$NS"
kubectl get deployment wva-controller-manager -n "$NS" \
  -o jsonpath='{.spec.template.spec.containers[0].args}'; echo   # no --v=4 left
```

```bash
kubectl logs -n "$NS" -l app.kubernetes.io/name=workload-variant-autoscaler -f
```

The scaling manager writes no custom resource, so its decisions are visible only in these logs, in
the metrics it publishes ([Prometheus metrics](prometheus.md)),
and in the HPA state KEDA derives from them.
