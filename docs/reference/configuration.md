# Deployment configuration reference

Every option `deploy/install.sh` reads. Each entry is read by something, and every `VAR=${VAR:-default}` in `install.sh` and `deploy/lib/*.sh` is listed.

> Part of the [the scaling manager deployment guide](../../deploy/).

## Required

| Variable | Description | Required For |
|----------|-------------|--------------|
| `HF_TOKEN` | HuggingFace token | llm-d deployment |

## Core configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `ENVIRONMENT` | Deployment environment (`kubernetes` or `openshift`) | `kubernetes` |
| `WVA_SCOPE` | `cluster` or `namespace`; see [Scope](../guides/install-cluster-wide/). The default is `namespace` everywhere, including `deploy/install.sh` run directly; it is no longer inferred from the platform. Cluster scope works and warns that it is WIP | `namespace` (`SCOPE=cluster` to change it) |
| `WVA_LIMITER` | `none`, `gpu-inventory`, `quota`, or a list such as `gpu-inventory,quota` — declares the limiters in the scaling-policy ConfigMap. Every limiter in the list bounds scaling. `gpu-inventory` needs node read, which `make setup-prereqs` grants. Unset on a re-run keeps the limiters already there; `none` removes them | unset (a fresh install has none) |
| `WVA_QUOTAS` | Required when `WVA_LIMITER` names `quota`: the per-accelerator budget, as `TYPE=N` pairs, separated by spaces or commas (`'H200=8 A100=4'`). `-1` is no cap on that type. No default — an empty quota entry is zero, not unlimited | — |
| `WVA_QUOTA_SCOPE` | `namespace` (each managed namespace gets the budget) or `cluster` (one budget across all of them) | `namespace` |
| `WVA_QUOTA_KUEUE` | With a quota limiter: `true` also reads Kueue, and per namespace and accelerator type the **smaller** of the Kueue nominal quota and `WVA_QUOTAS` applies. Kueue's usual flavors name no accelerator type, so name the types in `WVA_QUOTAS` with `-1` and Kueue sets the figure (`WVA_QUOTAS='H100=-1' WVA_QUOTA_KUEUE=true`). Needs the Kueue CRDs and, for a namespace-scoped install, the `kueue-reader` prerequisite. See [the Kueue-bounded quotas path](../well-lit-paths/kueue-bounded-quotas/) | `false` |
| `WVA_WATCH_NS` | Namespace a namespace-scoped controller **manages**, when it differs from the one it runs in. The workloads' owner then does not administer the controller, which is where a GPU bound actually holds. See [the GPU limiter](gpu-limiter.md#the-arrangement-where-the-bound-does-hold) | the controller's own namespace |
| `INSTALL_PHASE` | `prereqs` (cluster admin: namespace, RBAC, ServiceMonitor, Prometheus/KEDA) \| `wva` (the controller) \| `all`. **Usually leave it unset**: the install picks the half left to do, `wva` when the caller cannot create cluster-scoped objects or the admin half is already in place. `make setup-prereqs` is the `prereqs` phase; see [deploy/](../../deploy/) | auto |
| `WVA_PROJECT` | Repository root the script installs from | `$PWD` |

## Image

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_IMAGE_REPO` | controller image repository | `ghcr.io/ev-shindin/llm-scaling-manager` |
| `WVA_IMAGE_TAG` | controller image tag | `newTag` in `config/base/manager/kustomization.yaml`: `main`, or the release tag on a release commit |
| `WVA_IMAGE_DIGEST` | pin one exact build; set by `IMG=<repo>@sha256:<digest>` and used instead of the tag | unset |
| `WVA_IMAGE_PULL_POLICY` | Image pull policy | by reference: `IfNotPresent` for a digest or a `vX.Y.Z` tag, `Always` for anything else |

`main`, `latest`, `dev` and `nightly` are floating tags: the same install deploys a
different binary after the next push, and the installer warns. To reproduce a
deployment, pass `IMG=<repo>@sha256:<digest>` or a release tag.

## Namespaces

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_NS` | the scaling manager controller namespace | `workload-variant-autoscaler-system` |
| `MONITORING_NAMESPACE` | Prometheus namespace | `workload-variant-autoscaler-monitoring` |
| `NAMESPACE` | Where llm-d runs; **llm-d's own variable**, exported by its guides. `deploy/install-epp.sh` installs EPP there, `DEPLOY_LLMD_NS=true` creates it, and it **defaults `WVA_NS` for the `*-namespace-on-*` targets** (an explicit `WVA_NS` wins; cluster-scoped targets are unaffected). It is never passed to the controller; see [Which namespace is which](#which-namespace-is-which). **Required in namespace scope**, the default: the install refuses to guess. The fallback applies only to cluster scope and `kind-emulator` | `llm-d-optimized-baseline` (cluster scope only) |
| `LLMD_NS` | **Deprecated** alias for `NAMESPACE`, this repo's own former name for it. Still honoured, with a warning | — |

## Deployment flags

| Variable | Description | Default |
|----------|-------------|---------|
| `DEPLOY_PROMETHEUS` | Deploy the Prometheus stack **when the cluster has none of its own**. A Prometheus outside `MONITORING_NAMESPACE` is used as it is, and no monitoring namespace is created. Set `PROMETHEUS_FORCE_INSTALL=true` to deploy alongside one (two operators then contend over the same CRs) | `true` |
| `DEPLOY_OPERATIONAL_DASHBOARD` | Publish the llm-scaling-manager Grafana dashboard. On Kubernetes also enables Grafana in kube-prometheus-stack. Publishing into a **shared** monitoring namespace is a cluster-admin action; a namespace admin is told so and the install continues ([details](monitoring.md#if-you-cannot-write-to-the-monitoring-namespace)) | `true` |
| `DEPLOY_WVA` | Deploy the scaling manager controller | `true` |
| `DEPLOY_LWS` | Deploy LeaderWorkerSet (needed only for full e2e suite; skip for smoke, benchmarks, or pre-installed clusters) | `false` |
| `DEPLOY_ALERTING_RULES` | Install the PrometheusRule alerts | `false` |
| `WVA_FMA_LAUNCHER_METRICS` | Apply a PodMonitor that scrapes FMA launcher pods, which nothing else does — they declare no container ports, so a port-name endpoint generates no target for them. Applied to the **workload** namespace (`WVA_WATCH_NS`, else `WVA_NS`), and skipped if another PodMonitor already scrapes launchers there. See [FMA launcher pods](operations.md#fma-launcher-pods) | `false` |
| `DEPLOY_LLMD_NS` | Create an empty llm-d namespace up front, for a demo that wants it before anything is deployed. `deploy/install-epp.sh` creates its own when it deploys EPP, and the scaling manager does not watch namespaces | `false` |
| `ENABLE_SCALE_TO_ZERO` | Allow a model to be parked at zero replicas, and enable the EPP `flowControl` gate that makes waking it possible | `true` |
| `SKIP_CHECKS` | Turn the preflight's refusals (no model servers in the namespace, model servers or EPP not scraped, the EPP `flowControl` gate off, missing permissions) into warnings: findings are still reported. It does not skip the guard against a second install on the cluster, or the post-install verification | `false` |
| `SCALER_BACKEND` | `keda` or `none` (use a pre-installed backend) | `keda` |
| `KEDA_NAMESPACE` | Namespace KEDA is installed in. Set explicitly, it is also the namespace the external-scaler NetworkPolicy admits. Unset, that namespace is detected, never guessed; with no answer **no NetworkPolicy is applied**, and the installer warns that `:9090` is reachable from every Pod and prints the fix for the install's shape. How it is detected, and which existing policies are left as `state UNKNOWN`: [the external-scaler NetworkPolicy](upgrade-and-uninstall.md#the-external-scaler-networkpolicy). The policy is an admin-phase object (`make setup-prereqs`); `make check-prereqs` and `INSTALL_PHASE=wva` treat it as optional. `make verify-deployment` warns when the policy and KEDA's namespace differ, and prints `No NetworkPolicy: scaler port open` when there is none | `keda-system` for the KEDA install; detected for the NetworkPolicy |
| `WVA_SCALER_NETWORKPOLICY` | Install the NetworkPolicy that admits the external scaler port (`:9090`) **only from KEDA's namespace**; metrics (`:8443`) and probes (`:8081`) stay open. `false` leaves `:9090` open to every Pod that can reach the controller, which can then rewrite any workload's trigger metadata and read every scaling decision. `false` removes an existing policy only in the admin phase (`make setup-prereqs`, or `INSTALL_PHASE=all`); the controller-only `make deploy-wva` leaves it in place and reports it `state UNKNOWN`, printing the admin's `make setup-prereqs WVA_SCALER_NETWORKPOLICY=false` command | `true` |
| `KEDA_HELM_INSTALL` | Install KEDA with Helm rather than assuming it is present | `false` |
| `KEDA_CHART_VERSION` | KEDA Helm chart version | `2.19.0` |
| `UNDEPLOY` | Remove instead of install (`install.sh` doubles as the uninstaller) | `false` |
| `DELETE_NAMESPACES` | With `UNDEPLOY=true`, also delete the scaling manager and monitoring namespaces | `false` |
| `DELETE_LLMD_NS` | With `DELETE_NAMESPACES=true`, also delete `LLMD_NS`. Separate because that namespace holds the model servers: deleting it takes the workloads with it | `false` |
| `CHECK_ONLY` | Run the prerequisite and permission checks, then exit without deploying. Set by `--check` / `make check-prereqs` | `false` |
| `WVA_REPLICAS` | Controller replicas. Extra replicas are **warm standbys, not extra throughput**: only the elected leader runs the optimization loops. Two turn a node drain from "no decisions until rescheduled" into a lease timeout | `1` |
| `UNDEPLOY_SCALEDOBJECTS` | On uninstall, remove the ScaledObjects **this installer created** (KEDA then restores each workload's pre-autoscaling replica count). Adopted objects are never removed, but are listed, because their trigger now calls a scaler that is gone | `true` |
| `UNDEPLOY_SHARED` | With `UNDEPLOY=true`, also remove Prometheus, the scaler backend and EPP. **Off by default**: they are shared, and removing them breaks everything else on the cluster that uses them | `false` |

> `make deploy-e2e-infra` passes `ENABLE_SCALE_TO_ZERO=$(SCALE_TO_ZERO_ENABLED)`,
> whose Makefile default is **`false`** — the opposite of `install.sh`'s. So an
> e2e deploy has scale-to-zero OFF unless you pass
> `SCALE_TO_ZERO_ENABLED=true`, while a plain `make deploy-wva-on-k8s` has it ON.
> Three e2e suites skip silently when it is off.

ScaledObjects, HPA stabilization (`spec.advanced.horizontalPodAutoscalerConfig.behavior`) and vLLM ModelService tuning are not controlled by `install.sh`; manage them via `kubectl apply` directly (see the [llm-d guides](https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline) for reference manifests).

## Default ScaledObjects

**Read this if the scaling manager is installed and nothing is scaling.** A
ScaledObject is how a workload *registers*: the controller has no watch and no
listing, so it only learns about workloads KEDA calls it about. With no
ScaledObject, the controller is idle and reports itself healthy.

These options create one per llm-d model server: any Deployment or
LeaderWorkerSet labelled `llm-d.ai/inferenceServing=true`. Its model is read from
`--served-model-name`, or `--model` when that is the only one.

It works as **plan, then apply**, so you see what will be created first:

```bash
# 1. See what would be created. Nothing is applied.
make scaledobjects-plan WVA_DEFAULT_SO_PLAN=wva-plan.yaml

# 2. Edit it: apply: yes|no|adopt, the modelID, the replica bounds, the cost.
$EDITOR wva-plan.yaml

# 3. Apply exactly that file.
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```

The plan is YAML, and its comments explain every field. It is also printed as a
table. It looks like this:

```yaml
# llm-scaling-manager ScaledObject plan. Nothing here has been applied yet.
#
# apply          Required. One of:
#                  yes    create a ScaledObject for this workload
#                  no     leave the workload alone
#                  adopt  it already has a ScaledObject — repoint that one at the scaling manager
#                         instead of adding a second.
# ...
plan:

  - apply: "yes"
    namespace: llm-d-sim
    kind: Deployment
    name: sotest-a
    modelID: "e2ewva/dummy-model"
    minReplicas: 1
    maxReplicas: 10
    variantCost: "10.0"
    # scalingPolicy: "standard"
    # inferencePool: optimized-baseline

  # note: no --served-model-name or --model on the container, so the model could
  # not be read. Fill in modelID and set apply: yes to include it.
  - apply: "no"  # yes | no
    namespace: llm-d-sim
    kind: Deployment
    name: sotest-b
    modelID: ""
    minReplicas: 1
    maxReplicas: 10
    variantCost: "10.0"
    # scalingPolicy: "standard"
    # inferencePool: (none)
```

- The `apply:` line lists the values **that entry** accepts; `adopt` appears only
  where there is something to adopt.
- Anything informational is a comment, so editing it changes nothing:
  `# scaledObject:` names the object `adopt` would repoint, and
  `# inferencePool:` the EPP queue the workload sits behind.

| field | |
| --- | --- |
| `apply` | `yes` creates, `no` skips, `adopt` repoints a ScaledObject the workload already has. Two ScaledObjects on one target are two HPAs writing one replica count, so `yes` on a workload that already has one is refused, not quietly adopted |
| `modelID` | **Required.** What the container serves, and the grouping key: entries sharing a `modelID` are sized against each other. An entry without one is never applied |
| `minReplicas` / `maxReplicas` | What KEDA holds the workload between; the scaling manager decides within them. Default 1 and 10. For `adopt` they are read from the adopted object, so applying it unedited changes only who decides the count |
| `variantCost` | The relative price of one replica of this variant. Only the ratio between variants of one model matters, so with one variant it changes nothing |
| `scalingPolicy` | Optional, commented out by default. Names a policy tier (`interactive`, `standard`, `batch`) from the scaling-policy ConfigMap. Left out, the workload follows the cluster default, which an admin can change for every workload at once. A name no tier matches falls back to the `default` entry and is logged once per change: `Scaling policy not found; this variant is scaling under the default entry`, with the name and the known tiers |
| `inferencePool` | A comment, never applied: the EPP queue the workload sits behind, found by matching pod labels against each pool's selector, as the scaling manager does. `(none)` means no pool selects it |

Entries that cannot be applied are kept as `no`, with the reason, so the file
shows everything that was found.

To review interactively, `make scaledobjects-edit` opens the plan in `$EDITOR`
and asks before applying. It needs a terminal; plan-then-apply does the same
without one.

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_DEFAULT_SO` | `false` (do nothing), `plan` (list and stop), `edit` (list, `$EDITOR`, confirm), `true` (apply everything found) | `false` |
| `WVA_DEFAULT_SO_NS` | Namespace to scan. `wva` for the scaling manager's own, `all` for every namespace holding model servers. The default follows what this install can reach — `all` when cluster-scoped, its own namespace when namespace-scoped — so you rarely need to set it | scope-derived |
| `WVA_DEFAULT_SO_PLAN` | An existing file is applied as-is, edits included. Otherwise, where the generated plan is written | a temp file |
| `WVA_DEFAULT_SO_MIN` | `minReplicaCount` on generated objects. Not `0` even with scale-to-zero on: parking a model costs its next request a cold start, which is a decision about that workload's users | `1` |
| `WVA_DEFAULT_SO_MAX` | `maxReplicaCount` on generated objects | `10` |
| `WVA_DEFAULT_SO_TEMPLATE` | Your own ScaledObject template, substituted per workload. Placeholders: `{{NAMESPACE}}` `{{NAME}}` `{{KIND}}` `{{APIVERSION}}` `{{MODEL_ID}}` `{{SCALER_ADDRESS}}` `{{MIN}}` `{{MAX}}` `{{VARIANT_COST}}` `{{SCALING_POLICY}}`. Start from `config/samples/keda/external-scaler/scaledobject-template.yaml` | the shipped shape |
| `WVA_SO_SCALE_UP_PERIOD` | `periodSeconds` on the generated object's scale-up policy. A rate-limit window, not a delay — and nothing acts faster than the HPA control loop's sync period (15s by default) | `5` |
| `WVA_SO_SCALE_UP_STABILIZATION` | `stabilizationWindowSeconds` for scale-up. This is the knob that delays a scale-up: HPA takes its most conservative recommendation across the window. `0` means act on the current one | `0` |
| `WVA_SO_SCALE_DOWN_PERIOD` | `periodSeconds` on the scale-down policy | `120` |
| `WVA_SO_SCALE_DOWN_STABILIZATION` | `stabilizationWindowSeconds` for scale-down — how long demand must stay low before a replica is removed | `300` |
| `WVA_SO_FALLBACK` | `true` writes a KEDA `fallback` (`replicas` = the object's `minReplicaCount`, `behavior: currentReplicasIfHigher`) into every generated ScaledObject with a `minReplicaCount` of 1 or more, and into such an adopted one that has none. It covers a controller that is down or unreachable. A floor of `0` never gets one: KEDA would park a serving model on any trigger error. Nor does a cluster whose KEDA lacks `fallback.behavior` (before 2.17), checked at plan and apply time with `kubectl explain` (the OpenAPI any authenticated user may read) and then the CRD; a check that cannot answer writes none either, shown as `(no fallback: KEDA version unknown)`. A tenant install normally gets its answer from the OpenAPI. `false` leaves it out everywhere. See [the fallback](scaledobject-trigger.md#before-the-first-decision-and-the-fallback) | `true` |
| `WVA_SO_FALLBACK_FAILURES` | `failureThreshold` of that fallback. KEDA applies the fallback once more than this many reads of the metric by the HPA (about one every 15 s) have failed in a row. A whole number from `1` to `2147483647`, no leading zero | `3` |

`WVA_DEFAULT_SO_MIN` and `_MAX` are only the values the plan is *written* with.
What gets applied is what the file says when you apply it, per entry.

Scope follows the same rules as the ScaledObject plan above: `NAMESPACE=<ns>`
pins the scan to one namespace, and without it a cluster-scoped install walks
every namespace holding model servers.

The scaling behaviour is written into every generated ScaledObject, not
inherited. Kubernetes' defaults match the windows above, but they belong to the
API server, and a cluster that retunes them would silently change every
generated workload. Scale-up acts at once because the wait is already paid in
cold start. Scale-down is patient because removing a replica too early costs a
cold start, and keeping one too long only costs money.

**Adopting an existing ScaledObject leaves its behaviour alone.** `apply: adopt`
repoints the triggers and bounds; polling interval, cooldown, fallback and
`behavior` stay as whoever tuned them left them. The one addition: an object
with no `fallback` at all gets the default one, unless `WVA_SO_FALLBACK=false`,
its floor is `0`, or KEDA is older than 2.17. One that has a fallback keeps it
unchanged, with a warning if it is `replicas: 0` on a floor of `0` or if its
`replicas` is above the new floor. The adopted trigger is written with
`metricType: AverageValue`, and keeps every key set on it by hand
(`defaultOutputTokens`, `warmPool`, ...). An object the installer cannot read
is not patched at all, with a warning.

Set them on a deploy to do this during install:

```bash
make deploy-wva-on-k8s WVA_DEFAULT_SO=plan   # install, then show what it would create
make deploy-wva-on-k8s WVA_DEFAULT_SO=true WVA_DEFAULT_SO_NS=all
```

It never does two things:

- **Touch a workload that already has a ScaledObject.** That one may be
  hand-tuned or GitOps-managed, and two ScaledObjects on one target make two
  HPAs fight over one replica count.
- **Guess a model.** A workload whose model it cannot determine is skipped: a
  wrong `modelID` groups it with a model it does not serve and mis-scales both.

Generated objects use an `external-push` trigger. KEDA holds a stream open and
the scaling manager pushes activation as soon as it decides, so a parked
workload wakes in about the detection interval instead of waiting for a poll.

### Workload readiness (`make workload-patch`)

Two pod-spec settings make autoscaling a model server safe: a preStop hook, so
a removed replica finishes what it is writing, and weights on a mounted volume,
so a new replica does not re-download them. Both belong to the chart that owns
the pod spec, so `make workload-patch` writes a patch and, on request, applies
the half that is safe to apply. Full description in
[After the install](workload-preparation.md#writing-the-patch-make-workload-patch).

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_WORKLOAD_PATCH_FILE` | Where the patch is written | `wva-workload-patch.yaml` |
| `WVA_WORKLOAD_PATCH_APPLY` | `true` also applies the **drain half** to the live workloads, which replaces their pods | `false` |
| `WVA_WORKLOAD_PATCH_APPLY_WEIGHTS` | `true` also applies the **weights volume**, but only where it cannot break the workload: no volume of that name, nothing already mounted at that path, and the claim exists. Otherwise it is emitted with the reason, and the drain half still applies | `false` |
| `WVA_MODEL_PVC_SIZE` | Size for `make model-cache`. No default — it depends on how many models share the claim | *(required)* |
| `WVA_MODEL_PVC_CLASS` | StorageClass for `make model-cache`. No default, and it must support ReadWriteMany. Run `make model-cache` with neither to list the classes this cluster already serves RWX from | *(required)* |
| `WVA_MODEL_PVC_TIMEOUT` | How long to wait for the claim to bind. A `WaitForFirstConsumer` class is detected and not waited on | `120` |
| `WVA_DRAIN_GRACE_SECONDS` | `terminationGracePeriodSeconds` in the emitted patch. An existing longer value is kept, never lowered | `120` |
| `WVA_DRAIN_SLEEP_SECONDS` | The preStop sleep — the drain window itself | `45` |
| `WVA_MODEL_PVC_NAME` | The claim the emitted weights volume names. Nothing creates it | `model-pvc` |
| `WVA_MODEL_VOLUME_NAME` | The volume name in the emitted patch | `model-storage` |
| `WVA_MODEL_CACHE_PATH` | Where that volume is mounted, and the parent of the emitted `HF_HOME` | `/model-cache` |

### Holding the engine image (`make prepull`)

One DaemonSet per image keeps the engine image present on every accelerator
node, so a scale-up never starts with a pull. Full description in
[Holding the image on the nodes](workload-preparation.md#holding-the-image-on-the-nodes).
`NAMESPACE` must be given on the command line for these targets.

| Variable | Description | Default |
|----------|-------------|---------|
| `IMAGES` | Comma-separated image references, exactly as the model server's pod spec names them | *(required for `prepull`)* |
| `PREPULL_NODE_SELECTOR` | One or more `key=value` pairs, comma-separated (`k1=v1,k2=v2`), set to what the model servers select on; a node must carry every pair. An explicit selector replaces the default (nodes carrying a known GPU product label, `deploy/lib/accelerator_nodes.sh`); include the product label among the pairs to stay on GPU nodes. A value may be empty, and then selects the nodes whose label is empty (`gpu.nvidia.com/model=` is CoreWeave's CPU nodes, not its GPU nodes). Empty: the default | *(empty)* |
| `PREPULL_TOLERATIONS` | Comma-separated taint keys the holder tolerates beyond `nvidia.com/gpu` | *(empty)* |
| `BENCHMARK_PREPULL` | `false` makes `benchmark-standup` skip the pre-pull | `true` |
| `BENCHMARK_PREPULL_IMAGES` | What `benchmark-standup` holds instead of the image the harness pins | *(the harness's pin)* |

### Weights on the node's disk (`make weights`)

One static hostPath volume, one claim, one DaemonSet downloading the model
onto every accelerator node, so a replica reads its weights from the node's
own disk. Full description in
[Weights on the node's disk](workload-preparation.md#weights-on-the-nodes-disk).
`NAMESPACE` must be given on the command line for these targets.

| Variable | Description | Default |
|----------|-------------|---------|
| `WEIGHTS_MODEL` | Hugging Face model id | *(required for `weights`)* |
| `WEIGHTS_PATH` | Absolute directory on the node, one per namespace (under `/var` on RHCOS, e.g. `/var/mnt/weights/<ns>`; system paths refused); the model lands under `<path>/models/<id>` | *(required for `weights`)* |
| `WEIGHTS_IMAGE` | Image to download with -- any image carrying `huggingface_hub`, i.e. the engine image | *(required for `weights`)* |
| `WEIGHTS_HF_TOKEN_SECRET` | `<secret>[/<key>]` holding a Hugging Face token, for gated models (key defaults to `HF_TOKEN`) | *(none)* |
| `WEIGHTS_CAPACITY` | The claim's declared capacity -- no disk limit (hostPath has none), but a `requests.storage` quota charges it; set it near the model's size in a quota'd project. Unset on a re-run, an existing claim keeps its size; set to another size, the re-run is refused (a node-local claim cannot be resized -- scale the model servers using the claim to 0, then `make weights-delete WEIGHTS_MODEL=<id> NAMESPACE=<ns>` first; without `WEIGHTS_MODEL` it deletes every model's claim in the namespace) | `1Ti` |
| `WEIGHTS_NODE_SELECTOR` | As `PREPULL_NODE_SELECTOR` | `$(PREPULL_NODE_SELECTOR)` |
| `WEIGHTS_TOLERATIONS` | As `PREPULL_TOLERATIONS` | `$(PREPULL_TOLERATIONS)` |
| `BENCHMARK_MODEL_HOSTPATH` | A directory on the node: `benchmark-standup` then binds the harness's model claim to it and has the harness download the model on every accelerator node (the placement `make prepull` uses, `PREPULL_TOLERATIONS` included) before the engines start. Empty: the shared volume | *(empty)* |

## Pointing at an existing Prometheus

Required whenever this install did not deploy Prometheus itself. `PROMETHEUS_URL`
is written into the controller's config; without it the controller keeps the
shipped default and **exits at startup** if nothing answers there
(`CRITICAL: Failed to connect to Prometheus`).

| Variable | Description | Default |
|----------|-------------|---------|
| `PROMETHEUS_URL` | Full base URL, e.g. `https://prom.monitoring.svc.cluster.local:9090` | the kube-prometheus-stack this install deploys |
| `PROMETHEUS_SA` | `<namespace>/<serviceaccount>` of the Prometheus that scrapes the controller's `/metrics`; it is bound to the metrics-reader ClusterRole. Unset, the installer picks only from sources you set, never an auto-detected Prometheus or `default`, and otherwise creates no binding and says so (`--check` lists it as optional). A `make setup-prereqs` re-run without it keeps an existing binding; `make deploy-wva` never touches it. Full rules: [who `metrics-reader` is bound to](security-model.md#namespace-scoped-kubernetes) | none |
| `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` | Connect without verifying the server certificate. The installer warns when an HTTPS install ends up unverified; see [TLS configuration](prometheus.md#tls-configuration) for the production setting | `true` in the shipped Kubernetes config, `false` on OpenShift |
| `DEPLOY_PROMETHEUS` | Deploy a Prometheus stack when the cluster has none of its own — see [Deployment flags](#deployment-flags) | `true` |

## Which namespace is which

Three namespaces appear in these options and they do different jobs:

| variable | what lives there | who reads it |
| --- | --- | --- |
| `WVA_NS` | the controller, its ConfigMaps and its external-scaler Service | the installer, and the controller (as `POD_NAMESPACE`) |
| `NAMESPACE` | your model servers | **the installer only** — never passed to the controller |
| `MONITORING_NAMESPACE` | Prometheus and Grafana, if this install deploys them | the installer |

**Which namespaces get ScaledObjects follows the scope, not `NAMESPACE`.** A
cluster-scoped install scans every namespace holding model servers; a
namespace-scoped install scans the one it can read. `WVA_DEFAULT_SO_NS` narrows
it.

`NAMESPACE` never reaches the controller, on purpose. The scaling manager has no
watch and no listing: it learns about a workload when KEDA calls its external
scaler, from any namespace, so it has no use for a namespace name.

### What the scope actually changes

`WVA_SCOPE` controls the controller's **cache**, via `--watch-namespace`:

| scope | `--watch-namespace` | consequence |
| --- | --- | --- |
| `cluster` | unset | reads Deployments, pods, InferencePools and nodes in **any** namespace |
| `namespace` | its own (`POD_NAMESPACE`) | reads only **its own namespace** |

The scopes differ in who can install them as well as in what is read. **Every
overlay carries cluster-scoped RBAC**, so every install starts with a cluster
admin (counted from `kubectl kustomize config/overlays/<scope>/<platform>`):

| overlay | ClusterRoles | ClusterRoleBindings | what they are for |
| --- | --- | --- | --- |
| `namespace-scoped/kubernetes` | 6 | 6 | metrics authn (`metrics-auth`), the Prometheus scrape (`metrics-reader`), EPP metrics (`epp-metrics-reader`), nodes (`node-reader`), Kueue (`kueue-reader`), and the namespace label check for cluster policy (`policy-namespace-reader`) |
| `namespace-scoped/openshift` | 6 | 7 | the same, plus a binding to the platform's `cluster-monitoring-view` so the controller can query Thanos |
| `cluster-scoped/kubernetes` | 4 | 4 | `manager-role` (every read and the pod patch, cluster-wide), `metrics-auth`, `metrics-reader`, `epp-metrics-reader` |
| `cluster-scoped/openshift` | 4 | 5 | the same, plus `cluster-monitoring-view` |

Each overlay also carries namespaced Roles in the controller's namespace:
4 Roles and 4 RoleBindings for namespace-scoped (`manager`, `leader-election`,
`controller-status`, `utilization-share-history`), 3 and 3 for cluster-scoped (the
same without `manager`, which is a ClusterRole there).

A namespace admin can still own a namespace-scoped controller: a cluster admin
creates the cluster-scoped half once with `make setup-prereqs`
(`INSTALL_PHASE=prereqs`), and the controller phase needs none of it. Every verb,
resource and binding subject is in the [security model](security-model.md#permissions).

`./deploy/install.sh --check` answers for your install specifically: it renders
the overlay this install would apply and asks whether you may create each kind
in it.

KEDA does the actuation, so the scaling manager never writes Deployments,
ScaledObjects or HPAs. It is not read-only, though. It patches pods (deletion
cost, utilization-share marks, and warm-pool InferencePool labels, which move
live traffic), and writes Events, Leases and two ConfigMaps of its own
(`wva-controller-status`, `wva-utilization-share-history`); see
[what it writes](security-model.md#what-it-writes). The cluster-scoped read a
namespace-scoped install depends on is **nodes**, to resolve each variant's
accelerator; that is why the `gpu-inventory` limiter needs the node-reader
ClusterRole the prereqs phase creates
([GPU limiter](gpu-limiter.md#permission-nodes)).

> **Easy to get wrong:** a namespace-scoped scaling manager manages model servers
> **only in the namespace it watches**. One installed in `wva-system` while the
> models run in `llm-d-prod` is called by KEDA but cannot read the workload. Put
> the controller in the models' namespace, or point it there with `WVA_WATCH_NS`.
> A cluster-scoped install has no such constraint.

## Living beside what the cluster already has

**KEDA is never overwritten.** On `kubernetes` and `openshift` the install does
not Helm-install KEDA. It checks that `scaledobjects.keda.sh` exists, and fails
with instructions if not. `KEDA_HELM_INSTALL=true` opts in to installing one,
and even then it skips when a working KEDA (CRD, running operator, metrics
APIService) is present. `SCALER_BACKEND=none` skips the check.

**Uninstalling the scaling manager leaves KEDA, Prometheus and EPP.** Removing
them is a separate, explicit decision: `UNDEPLOY_SHARED=true`.

**Several namespace-scoped installs can share a cluster**, at most one per
namespace (`deploy/lib/single_install.sh`). The installer refuses only an
overlap with a cluster-scoped install, in either order, because a cluster-scoped
controller manages every namespace. Workloads never overlap (each registers with
the scaler address its trigger names), but GPU budgets do: each controller reads
the same nodes and allocates without seeing the others' claims. Bound them with
quota limiters or separate GPU nodes.

The refusal needs the installer to **see** the other installs, which takes
listing Deployments cluster-wide. A namespace admin cannot, so a tenant-run
`make deploy-wva` logs `Cannot check whether another WVA is already installed`
and **carries on**. Run `make check-prereqs` as a cluster admin for that
namespace first, or confirm no cluster-scoped controller exists (any row whose
args lack `--watch-namespace`):

```bash
kubectl get deploy -A -l app.kubernetes.io/name=workload-variant-autoscaler,control-plane=controller-manager \
  -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,ARGS:.spec.template.spec.containers[0].args
```

## Adding a model later

Deploy the model server, then re-run:

```bash
make scaledobjects-apply NAMESPACE=<your namespace>
```

It creates a ScaledObject for the **new** workload and reports and skips every
workload that already has one, so it is safe to re-run:

```text
[SUCCESS]   llm-d-sim/model-new (Deployment) -> ScaledObject model-new-wva (modelID: org/new-model)
[SUCCESS] Default ScaledObjects: 1 created, 1 not applied
```

Run `make scaledobjects-plan` first to see the list. The controller needs no
restart: it learns about the workload from the first KEDA call.

## High availability

The controller elects a leader (`--leader-elect=true`, with tunable lease, renew
and retry durations), so more than one replica is safe, but the extra replicas
are **standbys**. Only the leader runs the collection and optimization loops.

A standby answers KEDA on one point: which metric to put on the HPA
(`GetMetricSpec`). KEDA asks that when it reconciles a ScaledObject, and an HPA
created without an answer scales on CPU until the ScaledObject is edited.
Anything that needs a decision it refuses with `Unavailable`, which KEDA reports
as a trigger error and does not act on. It also drops the connection every few
seconds, so KEDA re-dials and reaches the leader. A single replica behaves the
same during a rollout, between the new pod being Ready and the old pod releasing
the lease. Until its first cycle has decided a model, the new leader answers
`GetMetrics` with the target's current replica count, so the HPA holds through
the hand-off ([before the first
decision](scaledobject-trigger.md#before-the-first-decision-and-the-fallback)).

```bash
make deploy-wva-on-k8s WVA_REPLICAS=2
```

That buys failover: a node drain or a crash costs a lease timeout, not a pod
reschedule. It does not buy throughput. The cycle is one process reasoning about
everything it manages, on purpose: a GPU budget cannot be split across
controllers that cannot see each other's decisions. For the same reason,
namespace-scoped installs side by side each have their own budget.

## Advanced

| Variable | Description | Default |
|----------|-------------|---------|
| `SKIP_TLS_VERIFY` | **Inert.** The installer sets it but nothing reads it. To skip or require Prometheus certificate checks, set `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` (see [TLS configuration](prometheus.md#tls-configuration)) | — |
| `WVA_STICKY_SCALE_DOWN` | Hold a published scale-down against demand noise until utilization at the published count would reach the scale-up threshold, so the fleet actually descends. Off, a model idling near the scale-down boundary flips its target N ↔ N−1 cycle to cycle and KEDA's 300 s window keeps the N-th replica for as long as the noise lasts. Turn off only to compare | `true` |
| `WVA_LOG_LEVEL` | **Inert.** The installer sets it but nothing reads it, and the controller's `LOG_LEVEL` env var is not read either. Verbosity is the controller's `--v=N` argument (default `2`; `4` is debug) — edit the Deployment's args to change it | `info` |
| `LEARNED_STATE_EPISODE_GAP` | How long a gap in saturated observations before the next one starts a **new window** instead of blending into the old one. A variant that goes quiet over a weekend should not average Monday's capacity with Friday's | `24h` |
| `LEARNED_STATE_VARIANT_TIMEOUT` | How long **per-variant** learned state — the fitted ITL window and its baseline, the replica start estimate — is kept after the variant stops being discovered, i.e. once its scale target or ScaledObject is gone. A variant parked at zero replicas is still discovered and keeps it for as long as it is parked | `24h` |
| `LEARNED_STATE_SNAPSHOT` | `true` carries what the saturation analyzer learned across a controller restart or a leader change, in the ConfigMap `wva-learned-state` in the controller's namespace. **Off by default** — see [Learned state across a restart](#learned-state-across-a-restart) | `false` |
| `LEARNED_STATE_BUCKET_RETENTION` | How long a **bucket-keyed** window — the k2 history and the mu windows the demand floor prices from, one per variant and traffic-shape bucket — is kept from when it was last read. Longer than the timeout on purpose: a retained measurement is the lowest capacity figure available for its bucket, and dropping it falls through to an estimate that orders *fewer* replicas | `168h` (7 days) |
| `PROMETHEUS_SECRET_NAME` | Secret holding the Prometheus serving cert | `prometheus-web-tls` |
| `PROMETHEUS_SECRET_NS` | Namespace of that secret | `$MONITORING_NAMESPACE` |
| `PROM_CA_CERT_PATH` | Local file where the installer saves the Prometheus CA to check it. The controller never sees this file; to verify Prometheus, mount the CA as in [TLS configuration](prometheus.md#tls-configuration) | `/tmp/prometheus-ca.crt` |
| `GATEWAY_API_VERSION` | Gateway API version installed for llm-d | `v1.2.0` |
| `LWS_NAMESPACE` | Namespace for LeaderWorkerSet installation | `lws-system` |
| `LWS_CHART_VERSION` | LeaderWorkerSet Helm chart version | `0.8.0` |

The three `LEARNED_STATE_*` horizons are how long the saturation analyzer keeps
what it has learned. Each is checked on its own: a value that is unset, negative,
below one minute, or has no unit (`24` parses as 24 **nanoseconds**) goes back
to its default, and the controller logs which it replaced. The horizons in force
are logged once at startup as `learned-state horizons`. Shorten them to exercise
this inside a test run instead of over a week.

None of the `LEARNED_STATE_*` variables is set by the installer: they are read from
the controller's own environment, so set them on its Deployment
(`kubectl set env deployment/<controller> -n <controller-namespace> LEARNED_STATE_SNAPSHOT=true`),
which restarts it.

The external scaler serves **plaintext** gRPC by default and relies on the
NetworkPolicy above. Three controller flags add TLS, all off by default and not
set by the shipped manifests:

- `--external-scaler-cert-path`: a directory holding `tls.crt` and `tls.key`,
  reloaded on rotation. Serves TLS; KEDA then needs `caCert` in the trigger's
  TriggerAuthentication.
- `--external-scaler-client-ca`: a PEM bundle, read once. Every client must also
  present a certificate it signed (KEDA's `tlsClientCert` and `tlsClientKey`).
- `--external-scaler-client-san` (comma-separated): accepts only clients whose
  certificate carries one of those names as a DNS SAN, URI SAN or Common Name.

Before turning TLS on:

- **Every ScaledObject that names this scaler needs a TriggerAuthentication
  first.** Those `make scaledobjects-apply` generates or adopts carry no
  `authenticationRef`, so with `--external-scaler-cert-path` set KEDA keeps
  dialling plaintext and every one goes not-Ready. In KEDA v2.18 the external
  scaler dials TLS when `caCert` is set, when **both** `tlsClientCert` and
  `tlsClientKey` are set (one alone leaves the connection plaintext), or when
  the trigger's metadata sets `enableTLS: "true"`:
  ```yaml
  apiVersion: keda.sh/v1alpha1
  kind: TriggerAuthentication
  metadata:
    name: wva-external-scaler-tls
    namespace: <model namespace>          # the ScaledObject's namespace
  spec:
    secretTargetRef:
    - {parameter: caCert,        name: wva-scaler-client-tls, key: ca.crt}
    - {parameter: tlsClientCert, name: wva-scaler-client-tls, key: tls.crt}   # mutual TLS only
    - {parameter: tlsClientKey,  name: wva-scaler-client-tls, key: tls.key}   # mutual TLS only
  ```
  and on each trigger, `authenticationRef: {name: wva-external-scaler-tls}`. An
  adopt merge-patches `triggers` as a whole, so re-running
  `make scaledobjects-apply` on an adopted object drops the reference; check
  it afterwards.
- **The serving certificate must name the Service KEDA dials**:
  `wva-external-scaler.<controller namespace>.svc`, and
  `wva-external-scaler.<controller namespace>.svc.cluster.local`, the host in
  the `scalerAddress` the installer writes.
- **Mount the certificate** as a Secret volume (`tls.crt`, `tls.key`) on the
  `manager` container, at the directory passed to `--external-scaler-cert-path`,
  for example `/etc/wva/scaler-tls`. The root filesystem is read-only, so the
  files must arrive by a mount. A client CA for `--external-scaler-client-ca`
  is mounted the same way.

What the NetworkPolicy does not cover:

- It is enforced only by a CNI that implements NetworkPolicy. Without one, `:9090`
  is open to every Pod; use the scaler TLS and client CA above.
- It admits every Pod in KEDA's namespace, and most CNIs also let node and
  `hostNetwork` traffic through.
- The client CA must be dedicated to KEDA: any certificate it signed is accepted.
  Pass `--external-scaler-client-san` to pin KEDA's identity.

### Learned state across a restart

Without `LEARNED_STATE_SNAPSHOT`, a restarted controller has forgotten every
measured capacity, and until a model saturates again it prices replicas from
the deployment-derived figure, which is higher. That orders **fewer** replicas,
so TTFT suffers after every upgrade or leader change until the fleet has
saturated once more. With it on, the acting controller writes what it learned
to the ConfigMap `wva-learned-state` at most once a minute, and the next
controller reads it on its first cycle.

What is restored, and what is not:

- a variant's measured capacity, saturated throughput, ITL baseline and
  replica start estimate are restored **only if it runs the same engine
  configuration** as when they were learned, read completely: a deployment
  whose flags come from a variable the controller cannot resolve restores
  nothing, because a changed value would look identical;
- the ages travel with the figures, so a restore never makes a figure younger,
  and one past its horizon above is not restored;
- a figure the new controller has already measured is never replaced.

The log line `Learned state: snapshot restored` says how many variants are
pending their first cycle; `learned state restored` or `learned state not
restored` follows for each on its first cycle. It needs the Role `wva-learned-state-role`, which
`make setup-prereqs` installs; an install upgraded from before the store existed
reports it as an optional prerequisite. See
[troubleshooting](troubleshooting.md#capacity-priced-high-after-a-restart) for
what the log lines mean when it does not work, and how to flush the store.

## Optional: scaling band after `make deploy-e2e-infra`

If `SCALE_UP_THRESHOLD` and/or `SCALE_DOWN_BOUNDARY` are set in the environment, the Makefile patches the `wva-scaling-policy-config` ConfigMap after install. Note the patch replaces the whole `default` entry, so it writes `analyzerName: saturation` alongside the band.

## What a change needs: a restart, or not

The controller's configuration has two halves, and which one a setting is in
decides whether a change needs a restart.

**Static**: read once at startup, so a change needs a restart. The metrics and
probe addresses, leader election, the Prometheus URL and its TLS material, the
feature flags, **the optimization interval** (`GLOBAL_OPT_INTERVAL`), **the
learned-state horizons** (`LEARNED_STATE_*`, read when the analyzer is built)
and **the Prometheus cache settings**. The interval and cache settings look
dynamic but are not: only test code changes them after load.

**Dynamic**: re-read when the ConfigMap changes, so a change applies in place.
The scaling thresholds, the analyzer selection and tiers, scale-to-zero, and the
limiter. The exception: a namespace-scoped controller reading a policy ConfigMap
outside the namespace it manages (cluster policy in `wva-policy` or a labelled
policy namespace, or, for an out-of-reach install, its own ConfigMap). Its cache
holds only the managed namespace (`cmd/main.go`), so it reads that ConfigMap at
startup, and an edit needs a restart.

The limiter is not a flag or an environment variable. It is the `limiters:` list
on the scaling-policy ConfigMap's `default` entry:

- no `limiters:` list — no limiter: scaling is bounded only by each workload's
  `maxReplicaCount`. This is what ships.
- `{type: gpu-inventory}` — cap scaling at the GPU inventory actually discovered
  on the cluster.
- `{type: quota, ...}` — enforce operator-declared per-accelerator caps, at
  cluster or namespace scope, declared inline.
- both — every entry is built, and scaling must fit all of them.

The engine rebuilds the limiter when the ConfigMap changes, with no restart,
except in the startup-read case above, where `make enable-physical-limiter` and
`make disable-physical-limiter` restart the controller for you. The schema,
scopes and validation rules are in the [quota limiter reference](quota-limiter.md).
