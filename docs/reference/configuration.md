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
| `WVA_QUOTA_KUEUE` | With a quota limiter: `true` also reads Kueue, and per namespace and accelerator type the **smaller** of the Kueue grant and `WVA_QUOTAS` applies. At namespace scope the grant counts cohort borrowing where it is safe ([the guide](../guides/kueue-borrowing/)). Kueue's usual flavors name no accelerator type, so name the types in `WVA_QUOTAS` with `-1` and Kueue sets the figure (`WVA_QUOTAS='H100=-1' WVA_QUOTA_KUEUE=true`). Needs the Kueue CRDs and, for a namespace-scoped install, the `kueue-reader` prerequisite. See [the Kueue-bounded quotas path](../well-lit-paths/kueue-bounded-quotas/) | `false` |
| `WVA_QUOTA_KUEUE_BORROW` | With `WVA_QUOTA_KUEUE=true`, `false` writes `countBorrowable: false` on the quota entry: the Kueue cap stays at nominal and no cohort borrowing is counted. Ignored, with a warning, without `WVA_QUOTA_KUEUE=true`. `make enable-physical-limiter` reads it too. A re-run keeps the rest of the entry's `kueue:` block. It also decides the cluster-wide Kueue Workload read of a namespace-scoped install: `make setup-prereqs` and `make enable-physical-limiter` grant it only while a policy counts borrowing, and revoke it on `false` | `true` |
| `KUEUE_NAMESPACE` | Where Kueue runs. The prereqs phase puts the Role that reads the Kueue controller's Deployment and leader Lease there; the uninstall finds it by label wherever it is. Unset, the `kueue.controllerNamespace` the policy keeps is used, so a re-run need not repeat it: `KUEUE_NAMESPACE=<kueue-ns> NAMESPACE=<ns> make setup-prereqs` | the entry's `kueue.controllerNamespace`, else `kueue-system` |
| `WVA_WATCH_NS` | Namespace a namespace-scoped controller **manages**, when it differs from the one it runs in. The workloads' owner then does not administer the controller, which is where a GPU bound actually holds. See [the GPU limiter](gpu-limiter.md#the-arrangement-where-the-bound-does-hold). A comma-separated list (`team-a,team-b`), or `auto` for every namespace with llm-d, manages several from `WVA_NS`: a Role in each, nothing cluster-wide on workloads. See [Share GPUs between namespaces with a namespace-scoped install](../guides/utilization-share-namespaced/) | the controller's own namespace |
| `INSTALL_PHASE` | `prereqs` (cluster admin: namespace, RBAC, ServiceMonitor, Prometheus/KEDA) \| `wva` (the controller) \| `all`. **Usually leave it unset**: the install picks the half left to do, `wva` when the caller cannot create cluster-scoped objects or the admin half is already in place. `make setup-prereqs` is the `prereqs` phase; see [deploy/](../../deploy/) | auto |
| `RENDER` | A directory. `make setup-prereqs RENDER=<dir>` runs the cluster-admin phase with every read and check as usual and **applies nothing**. It writes `admin.yaml`, every object the phase creates or updates, and `admin.sh`, the Helm installs and deletes that are not plain objects, each with its reason, then `kubectl apply -f admin.yaml`. A cluster admin reviews both and runs `bash admin.sh`, with no checkout. Needs read rights only; see [Rendering the admin phase](#rendering-the-admin-phase) | unset |
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
deployment, install from a release checkout (`git checkout v0.10.0-rc3`, which
deploys `:v0.10.0-rc3`), or pass `IMG=<repo>@sha256:<digest>` with the digest
from the release notes. See
[Pin the version](upgrade-and-uninstall.md#pin-the-version).

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
| `SKIP_CHECKS` | Turn the preflight's refusals (no model servers in the namespace, model servers or EPP not scraped, missing permissions) into warnings: findings are still reported. It does not skip the guard against a second install on the cluster, or the post-install verification | `false` |
| `SCALER_BACKEND` | `keda` or `none` (use a pre-installed backend) | `keda` |
| `KEDA_NAMESPACE` | Namespace KEDA is installed in. Set explicitly, it is also the namespace the external-scaler NetworkPolicy admits. Unset, that namespace is detected, never guessed; with no answer **no NetworkPolicy is applied**, and the installer warns that `:9090` is reachable from every Pod and prints the fix for the install's shape. How it is detected, and which existing policies are left as `state UNKNOWN`: [the external-scaler NetworkPolicy](upgrade-and-uninstall.md#the-external-scaler-networkpolicy). The policy is an admin-phase object (`make setup-prereqs`); `make check-prereqs` and `INSTALL_PHASE=wva` treat it as optional. `make verify-deployment` warns when the policy and KEDA's namespace differ, and prints `No NetworkPolicy: scaler port open` when there is none | `keda-system` for the KEDA install; detected for the NetworkPolicy |
| `WVA_SCALER_NETWORKPOLICY` | Install the NetworkPolicy that admits the external scaler port (`:9090`) **only from KEDA's namespace**; metrics (`:8443`) and probes (`:8081`) stay open. `false` leaves `:9090` open to every Pod that can reach the controller, which can then rewrite any workload's trigger metadata and read every scaling decision. `false` removes an existing policy only in the admin phase (`make setup-prereqs`, or `INSTALL_PHASE=all`); the controller-only `make deploy-wva` leaves it in place and reports it `state UNKNOWN`, printing the admin's `make setup-prereqs WVA_SCALER_NETWORKPOLICY=false` command | `true` |
| `WVA_SCALER_TLS` | Serve the external scaler (`:9090`) over **mutual TLS**. Unset: TLS when KEDA's side is in place and no ScaledObject would be left on plaintext; otherwise plaintext, and the installer says why. `true`: the same, but a refusal instead of plaintext. `false`: plaintext. A controller that already has TLS keeps it. [How it works](#external-scaler-mutual-tls) | unset: TLS when it can |
| `WVA_SCALER_TLS_PROVIDER` | Who issues those certificates: `cert-manager`, `openssl` (a private CA the installer keeps in a Secret), or `auto` — cert-manager when it is installed. On OpenShift the serving certificate always comes from the service CA. Unset, a re-install keeps the running controller's provider | `auto` |
| `WVA_SCALER_TLS_MIGRATE` | `true` moves the ScaledObjects that name this scaler with it when its TLS mode changes: to `wva-external-scaler.<ns>.svc:9090` with KEDA's `authenticationRef`, or back. Without it, such ScaledObjects keep the default on plaintext, and make an explicit `WVA_SCALER_TLS` a refusal | `false` |
| `KEDA_HELM_INSTALL` | Install KEDA with Helm rather than assuming it is present | `false` |
| `KEDA_RESTART` | Let the installer restart KEDA's operator (`kubectl rollout restart deployment/keda-operator`). After an uninstall and a reinstall in the same namespace, KEDA keeps its old connection to the scaler and every call is refused, so the ScaledObjects stay unwired. `scaledobjects-apply`, `deploy-wva` and `verify-deployment` detect that and name the ScaledObjects and the command. With `true` they restart KEDA, and `undeploy-wva` restarts it once its ScaledObjects are gone; with `false` nothing restarts KEDA, which is shared, and the install goes on with the warning | `false` |
| `DEPLOY_LLMD_MONITORS` | In the admin phase (`setup-prereqs`, or `INSTALL_PHASE=all`), add a model-server PodMonitor and an EPP ServiceMonitor in each llm-d namespace where nothing scrapes them yet. Each object is labelled `llm-d.ai/applied-by=wva-setup-prereqs`, and `undeploy-wva` removes exactly those. `false` adds nothing, and the preflight then refuses a namespace that is not scraped | `true` |
| `KEDA_CHART_VERSION` | KEDA Helm chart version. [Supported versions](keda.md#supported-versions) | `2.21.0` |
| `KEDA_CMA_NAMESPACE` | OpenShift: where the installer reads the Custom Metrics Autoscaler's version, for its [version line](keda.md#what-the-installer-prints) | `openshift-keda` |
| `KUBE_PROMETHEUS_STACK_CHART_VERSION` | kube-prometheus-stack Helm chart version, when the install deploys Prometheus. Unset, a newer release already installed is kept rather than downgraded, with a warning | `92.7.0` |
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
| `scalingPolicy` | Optional, commented out by default. Names a policy tier (`interactive`, `standard`, `batch`) from the scaling-policy ConfigMap. Left out, the workload follows the tier its model's other variants name; when none names one, it follows the cluster default, which an admin can change for every workload at once. A name no tier matches falls back to the `default` entry and is logged once per change: `Scaling policy not found; this variant is scaling under the default entry`, with the name and the known tiers |
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
| `PROMETHEUS_URL` | Full base URL, e.g. `https://prom.monitoring.svc.cluster.local:9090`. Set, it is used as given. Unset, the installer picks one; see [How the Prometheus address is chosen](#how-the-prometheus-address-is-chosen) | detected |
| `PROMETHEUS_SA` | `<namespace>/<serviceaccount>` of the Prometheus that scrapes the controller's `/metrics`; it is bound to the metrics-reader ClusterRole. Unset, the installer picks only from sources you set, never an auto-detected Prometheus or `default`, and otherwise creates no binding and says so (`--check` lists it as optional). A `make setup-prereqs` re-run without it keeps an existing binding; `make deploy-wva` never touches it. Full rules: [who `metrics-reader` is bound to](security-model.md#namespace-scoped-kubernetes) | none |
| `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` | Connect without verifying the server certificate. The installer warns when an HTTPS install ends up unverified, and when it verifies with no CA set; see [TLS configuration](prometheus.md#tls-configuration) for the production setting | `false`; `true` for the Prometheus this install deploys or, on an upgrade, a Prometheus the controller already ran unverified with no CA. See [TLS configuration](prometheus.md#tls-configuration) |
| `PROMETHEUS_CA_CERT_PATH` | File, inside the controller, of the CA that signed the Prometheus certificate. Set, you mount it yourself and the installer copies nothing. Unset, a re-run keeps the one the controller runs with; otherwise the installer copies the CA and mounts it at `/etc/wva-prometheus-ca/ca.crt` ([Prometheus with TLS](prometheus.md#prometheus-with-tls-eg-kube-prometheus-stack-on-a-managed-gpu-cloud)) | unset |
| `PROMETHEUS_SECRET_NAME`, `PROMETHEUS_SECRET_NS` | The Secret the installer copies the Prometheus CA from (key `ca.crt`, else `tls.crt`; never the key) | the Secret the Prometheus resource's `web.tlsConfig` names |
| `PROMETHEUS_CA_FILE` | A local CA file to copy instead of a Secret | unset |
| `PROMETHEUS_TLS_PROBE` | `false` skips the in-cluster TLS probe the install runs before it reports success. The certificate name check still runs | `true` |
| `PROMETHEUS_TLS_PROBE_IMAGE` | The curl image that probe runs, for a cluster that cannot pull the default | `quay.io/curl/curl:8.11.1` |
| `DEPLOY_PROMETHEUS` | Deploy a Prometheus stack when the cluster has none of its own — see [Deployment flags](#deployment-flags) | `true` |

### How the Prometheus address is chosen

1. `PROMETHEUS_URL`, when you set it. It is used as given, and still checked
   (step 4).
2. On OpenShift, the Thanos Querier at its fixed address.
3. Otherwise the first Prometheus resource (Prometheus Operator) on the cluster,
   reached through a Service its certificate names. The controller verifies the
   certificate, so the host must be one of its names:
   - the installer reads the certificate from the Secret or ConfigMap the
     Prometheus resource's `web.tlsConfig` names (the certificate only), and
     takes the first Service that routes to that Prometheus and whose
     `<service>.<namespace>.svc.cluster.local` or `<service>.<namespace>.svc`
     the certificate names;
   - the Prometheus's own Service comes before `prometheus-operated`. A
     kube-prometheus-stack serves its certificate for
     `<release>-kube-prometheus-stack-prometheus`, not for `prometheus-operated`;
   - when the certificate cannot be read (no TLS, or you may not read it), its
     own Service, then `prometheus-operated`.
4. Before it reports success, the install checks the choice. A certificate
   known not to name the host stops the install with the names it holds and the
   `PROMETHEUS_URL` to use. Then a one-shot Pod in the controller's namespace
   connects with curl, with the CA the controller gets and no option that skips
   verification. A refused certificate stops the install; a probe that cannot
   run (Pod creation Forbidden, image not pullable, a NetworkPolicy) is a
   warning that says so.

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
5 Roles and 5 RoleBindings for namespace-scoped (`manager`, `leader-election`,
`controller-status`, `utilization-share-history`, `learned-state`), 4 and 4 for
cluster-scoped (the same without `manager`, which is a ClusterRole there).

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

The analyzer's own thresholds, windows and counts are not environment
variables: they are scaling-policy keys, `analyzerTuning` and `loadImbalance`,
set per tier or per model and applied live. See
[Advanced tuning](scaling-policy.md#advanced-tuning-analyzertuning-and-loadimbalance).
The defaults are the calibrated values; change them only on a measurement.

### Rendering the admin phase

When the person installing is not a cluster admin, they render the admin
phase and hand it over:

```bash
make setup-prereqs <the same settings> RENDER=admin-setup   # you: read rights only
bash admin-setup/admin.sh                                    # the cluster admin
```

Then the namespace owner runs `make deploy-wva INSTALL_PHASE=wva` as usual.

The render runs every read and check a live run makes, with your rights, and
writes nothing to the cluster. What it would have written goes to the two
files:

- `admin.yaml`: every object, without server fields, namespaces and RBAC
  first. It holds the private keys the scaler certificates need, so it is
  mode 600; send it privately.
- `admin.sh`: the Helm installs (exact chart, version and values), CRDs
  applied from a URL, deletes of objects already on the cluster (an
  immutable `roleRef`, an obsolete token), each with a comment saying why,
  then `kubectl apply -f admin.yaml`. Waits and rollout checks are left to
  the admin's run.

Questions about what you may write are answered for the admin, so a render
needs read rights only. The built-in `view` role is not quite enough. A
render also reads CustomResourceDefinitions, ClusterRoles,
ClusterRoleBindings, Roles, RoleBindings and InferencePools (`get` and
`list`). A read the cluster refuses is listed at the end of the render, since
the admin's run would have seen that object and might decide differently.

A render describes the cluster when it ran. Render again after the cluster
changes. With cert-manager on the cluster, render with
`WVA_SCALER_TLS_PROVIDER=openssl`: a render cannot wait for cert-manager to
issue certificates, and stays on plaintext without it.

### External scaler mutual TLS

KEDA calls the scaling manager's external scaler on `:9090`. By default the
install serves it over **mutual TLS**: the scaler proves who it is to KEDA, and
KEDA proves who it is to the scaler. Only KEDA can call it.

This authenticates KEDA, not whoever wrote a ScaledObject. Anyone who may
create a ScaledObject can point it at the scaler with this install's
`authenticationRef`, and KEDA then calls the scaler for them. Who may create
ScaledObjects is still your RBAC
([security model](security-model.md#scaler-mutual-tls)).

**What you do:** nothing, on a new install. The installer turns TLS on when it
can, and says why when it cannot.

**When it stays on plaintext** (the installer prints which one, and the fix):

- KEDA is not installed. Fix: install it, or let the install do it with
  `KEDA_HELM_INSTALL=true`.
- KEDA is installed, but the installer cannot tell where it runs or where it
  keeps its Secrets. Fix: set `KEDA_NAMESPACE`.
- The cluster admin has not written KEDA's side yet (a tenant's
  `make deploy-wva` cannot). Fix: the admin runs `make setup-prereqs`, then
  you re-run `make deploy-wva`.
- You changed `WVA_SCALER_CLIENT_SAN`, and KEDA still holds a certificate for
  the old name. Fix: the admin runs `make setup-prereqs` with the new name and
  restarts KEDA's operator; then you re-run `make deploy-wva`.
- You may not read or create what it needs (below), or cert-manager does not
  issue the certificates.
- ScaledObjects already point at the scaler in plaintext. Fix: re-run with
  `WVA_SCALER_TLS_MIGRATE=true`, which moves them with it.

`WVA_SCALER_TLS=true` turns each of these into a refusal.

**Turn it off.**

```bash
make deploy-wva WVA_SCALER_TLS=false WVA_SCALER_TLS_MIGRATE=true
```

This moves the ScaledObjects back to the plaintext address. Without
`WVA_SCALER_TLS_MIGRATE=true` it refuses while any ScaledObject still uses TLS.
With `WVA_SCALER_TLS` unset, a controller that has TLS keeps it. Add
`INSTALL_PHASE=wva` only if you pass `INSTALL_PHASE` on your installs; otherwise
the installer picks the phase itself.

**What a ScaledObject needs** on a TLS install. The installer writes this into
every ScaledObject it creates or adopts, and the warm pool's:

```yaml
triggers:
  - type: external-push
    metadata:
      scalerAddress: wva-external-scaler.<controller namespace>.svc:9090
    authenticationRef:
      name: wva-external-scaler-<controller namespace>
      kind: ClusterTriggerAuthentication
```

Use that address and that `authenticationRef` exactly. A plaintext
ScaledObject uses `...svc.cluster.local:9090` and no `authenticationRef`.

**Do not mix them.** A ScaledObject on the TLS address without the
`authenticationRef`, or with a TriggerAuthentication that carries only a CA,
is unsupported. It may seem to work: KEDA keeps one connection per address,
so it can ride the connection an authenticated ScaledObject opened. But after
a KEDA operator restart, if it is the first to connect, that one connection
opens without KEDA's certificate. Then **every** ScaledObject on that address
stops scaling, until the bad one is fixed and KEDA is restarted again.

`make verify-deployment` lists such ScaledObjects by name. To fix them, add the
`authenticationRef` above, or run the command under "Moving existing
ScaledObjects".

**Moving existing ScaledObjects.**

```bash
make deploy-wva WVA_SCALER_TLS=true WVA_SCALER_TLS_MIGRATE=true
```

Once the new controller Pods alone serve the scaler, this rewrites each
ScaledObject's scaler trigger in place: the address, and the
`authenticationRef`. "Alone" means the rollout is done, the old Pods are gone
from the Service, and a new Pod holds the leader lease. KEDA's calls for each
may fail for a few seconds while the two catch up; the fallback holds its
replicas. If that does not happen within three minutes
(`WVA_SCALER_SETTLE_TIMEOUT`, in seconds), nothing is moved, and the installer
prints the command to run later.

Then it checks that KEDA wired each HPA with the scaler's metric. If not, it
changes the trigger metadata key `wvaRewire`, so KEDA asks for the metric
again, for about three minutes. See
[External scaler TLS on upgrade](upgrade-and-uninstall.md#external-scaler-tls-on-upgrade). The same flag with
`WVA_SCALER_TLS=false` moves them back. It also moves them back whenever the
scaler stays on plaintext, for example while KEDA's side is not ready.

ScaledObjects managed by GitOps: run the command, then commit the new trigger
(the TLS address and the `authenticationRef`) to git right away. Otherwise
GitOps puts the old one back.

**Rotate after a compromise.** If KEDA's client key leaked:

1. Delete Secret `wva-external-scaler-client-tls` in the controller namespace.
   If the CA key leaked too, also delete Secret `wva-external-scaler-ca`.
2. A cluster admin runs `make setup-prereqs`; then run `make deploy-wva`.
3. Restart KEDA's operator (the command is below, under Rotation).

<details>
<summary>Details: why two addresses, and who writes what</summary>

KEDA keeps one connection per address string, built for whichever ScaledObject
connected first, and never closes it. So the two modes need two different
addresses for the same Service.

The same pooling is why mixing is order-dependent. On a real cluster, a
ScaledObject on the TLS address with no `authenticationRef`, and one whose
TriggerAuthentication held only a CA, were both Ready and scaled: they shared
the connection an authenticated ScaledObject had opened. The scaler itself
refuses such clients in the TLS handshake (no certificate, a wrong name, an
unknown CA), and logs each refusal at most once per remote host per minute.
The failure after a KEDA restart is not covered by an automated test: it needs
a KEDA restart, and control over which ScaledObject KEDA connects first.

| Object | Written by | Where |
| --- | --- | --- |
| Serving certificate, Secret `wva-external-scaler-tls` | both phases | controller namespace. From cert-manager if installed, else a private CA kept in Secret `wva-external-scaler-ca`. On OpenShift, the service CA |
| KEDA's client certificate, Secret `wva-external-scaler-client-tls` | both phases | controller namespace. Identity `wva-keda-client`, the only client the scaler accepts |
| A copy of that Secret, named `wva-external-scaler-<controller namespace>` | admin (`make setup-prereqs`) | KEDA's namespace. A ClusterTriggerAuthentication can read only there |
| ClusterTriggerAuthentication `wva-external-scaler-<controller namespace>` | admin | cluster-scoped |
| ConfigMap `wva-scaler-keda-auth` | admin | controller namespace. Tells the controller phase that KEDA's side is in place |

The copy never goes to a model namespace: whoever can read it can call the
scaler as KEDA.

</details>

**Permissions.** Only when `WVA_SCALER_TLS=true` or TLS is already on does
`make check-prereqs` ask for them. Without them the default stays on
plaintext.

- Controller phase: `get` and `create` on Secrets in the controller namespace.
  With cert-manager, also on its `Issuers` and `Certificates` there.
- Admin phase: also `create` on ConfigMaps there,
  `clustertriggerauthentications.keda.sh`, and Secrets in KEDA's namespace.

**Rotation.**

- The serving certificate lasts a year. cert-manager renews it; without
  cert-manager, re-run `make deploy-wva`. The controller reloads it without a
  restart.
- KEDA's client certificate lasts as long as the CA (10 years). KEDA reads it
  when it first connects and keeps it until KEDA's operator restarts, so it is
  not renewed sooner.
- **Restart KEDA's operator when the installer tells you to.** KEDA keeps the
  CA and client certificate it started with, and every handshake fails until
  it restarts. `make setup-prereqs` or the admin's `make undeploy-wva` prints
  the command:
  - after the CA or KEDA's certificate changed (the CA nears its end, you
    switched certificate provider, or changed `WVA_SCALER_CLIENT_SAN`);
  - after a reinstall: KEDA's side is written again while KEDA runs and
    ScaledObjects already use the TLS address;
  - after KEDA's side was removed. Restart it once the scaler is installed
    again, not before: a restart in between reconnects without TLS.

  On a plain first install it is only a note.

  ```bash
  kubectl rollout restart deployment/keda-operator -n <KEDA namespace>
  ```

  On OpenShift's Custom Metrics Autoscaler, the KEDA namespace is usually
  `openshift-keda`.

- **OpenShift** rotates its service CA by itself. When it does, a cluster admin
  re-runs `make setup-prereqs`, then restarts KEDA's operator.
  `make verify-deployment` warns when KEDA trusts an old CA. Until it is fixed,
  `make deploy-wva` does not turn TLS on for a controller that is on plaintext.

**Check it.** `make verify-deployment` prints:

- the mode (plaintext, TLS, mutual TLS);
- the certificates' expiry;
- whether KEDA's side is in place, and whether KEDA trusts the current CA;
- every ScaledObject whose address or `authenticationRef` does not match the
  mode, in either mode.

The controller log also names each client the scaler refuses in the TLS
handshake (`refused a TLS handshake`), with its address and the reason.

**Undo.** `make undeploy-wva` removes what carries the `wva.llmd.ai/scaler-tls`
label. The admin's uninstall also removes KEDA's side, again only what carries
the label, and prints the KEDA restart command. An uninstall that cannot
remove KEDA's side keeps the `wva-scaler-keda-auth` ConfigMap, which records
where it is, and says so. The installer refuses
to overwrite a Secret, Issuer, Certificate, ClusterTriggerAuthentication or the
`wva-scaler-keda-auth` ConfigMap without that label, or one it cannot read.

<details>
<summary>Details: the controller flags, and setting TLS up by hand</summary>

The installer sets three controller flags. Set by hand, the installer refuses
to drop them until you choose: `WVA_SCALER_TLS=true` takes them over and lists
what it replaced, `WVA_SCALER_TLS=false` removes them.

- `--external-scaler-cert-path`: a directory holding `tls.crt` and `tls.key`,
  reloaded on rotation. Mount it as a Secret volume: the root filesystem is
  read-only.
- `--external-scaler-client-ca`: a PEM bundle, read once. Every client must
  present a certificate it signed. The CA must be KEDA's alone: any
  certificate it signed is accepted.
- `--external-scaler-client-san` (comma-separated): accepts only clients whose
  certificate carries one of those names as a DNS SAN, URI SAN or Common Name.
  The installer sets one name, `wva-keda-client`, or `WVA_SCALER_CLIENT_SAN`
  when given; that must be one lowercase DNS name, or the install refuses.
  The controller refuses to start when the flag is given but names nothing
  (`,` or a blank), or when `--external-scaler-client-ca` is given empty.

Setting `--external-scaler-client-ca` by hand: do not point it at a shared CA,
such as a cluster or mesh CA. With a plain name in the allowlist, anyone who
can get a certificate with that name from that CA passes as KEDA. The
installer avoids this with a CA of its own for each install.

In KEDA v2.18 the external scaler dials TLS when the authentication sets
`caCert`, when it sets **both** `tlsClientCert` and `tlsClientKey` (one alone
leaves the connection plaintext), or when the trigger sets `enableTLS: "true"`.
The serving certificate must name the host in `scalerAddress`.

</details>

What the NetworkPolicy does not cover:

- It is enforced only by a CNI that implements NetworkPolicy. Without one, `:9090`
  is open to every Pod; mutual TLS (above) still holds.
- It admits every Pod in KEDA's namespace, and most CNIs also let node and
  `hostNetwork` traffic through.

### Kueue borrowing timings

Set on the quota entry's `kueue:` block in the scaling policy, beside
`countBorrowable`. Each is a Go duration.

| Key | What it does | Default |
|---|---|---|
| `placementTimeout` | A borrowed replica that stays unschedulable, or held at Kueue's admission gate, this long is released; a placed pod still loading its weights does not count. So is a queue's growth back into nominal it lent out that was placed without a node hole, timed from when it was ordered, and the model's cooldown (`borrowCooldown`) follows. The wait stretches up to three times while the cluster autoscaler reports `TriggeredScaleUp` on its pod. Also how long the utilization-share optimizer waits before it retries a receiver whose replica could not be placed. Unset, that wait is the timeout derived from the KEDA and pod settings, as before. | `5m` |
| `borrowCooldown` | After a lender in a cohort waited for its capacity, nothing new is borrowed in the cohort for this long. After Kueue preempted one of a model's pods, or a borrowed unit of it was released at the placement timeout, the model borrows nothing new in that cohort for this long; a repeat during the backoff doubles it, up to four times and never past 40m (unless `borrowCooldown` itself is longer, then that), and one clean cooldown resets it. Published on `wva_kueue_borrow_cooldown_seconds`. | `10m` |
| `lenderDebounce` | A lender counts as waiting only after two fresh reads and this long. | `60s` |
| `controllerNamespace` | Not a duration: the namespace of the `kueue-controller-manager` Deployment, whose image tag gives the Kueue version borrowing is checked against, and of its leader-election Lease, which tells whether Kueue runs (not renewed, or neither readable: borrowing freezes as `kueue-controller-down`). | `kueue-system` |
| `assumeTestedVersion` | Not a duration: `true` counts a Kueue whose version cannot be read from its image tag as tested, lifting `kueue-version-unknown`. It does not lift `kueue-controller-down`. | `false` |
| `releaseToNonReclaimingLenders` | Not a duration, experimental: `true` counts what lenders Kueue does not reclaim for lend (`reclaimWithinCohort: Never`, or `LowerPriority` below the model), and gives it back when they wait ([how](quota-limiter.md#lenders-kueue-does-not-reclaim-for)). Refused with `countBorrowable: false`; a warning with `enabled: false`. Applies to every namespace the entry governs. Kept by a re-install with the rest of the `kueue:` block. | `false` |
| `borrowNamespaces` | A list of namespaces, not a duration: borrowing is counted only in these (a pilot); every other namespace is capped at nominal, withheld as `not-selected`. A re-run of the install or of `make enable-physical-limiter` keeps it, with the rest of the `kueue:` block ([pilot](../guides/kueue-borrowing/#pilot-on-a-few-namespaces)). | empty: all |

The cooldowns and the placement-timeout releases still in flight are recorded
under `borrowReleaseLedger` in the `wva-controller-status` ConfigMap, so a
restart neither releases twice nor forgets a cooldown. The
budget the controller used last cycle, per quota entry, namespace and
accelerator, is under `limiterBudgets` in the same ConfigMap
([what it holds](prometheus.md#kueue-borrowing-metrics)).

### Learned state across a restart

Without `LEARNED_STATE_SNAPSHOT`, a restarted controller has forgotten every
measured capacity, and until a model saturates again it prices replicas from
the deployment-derived figure, which is higher. That orders **fewer** replicas,
so TTFT suffers after every upgrade or leader change until the fleet has
saturated once more. With it on, the acting controller writes what it learned
to the ConfigMap `wva-learned-state` at most once a minute, and the next
controller reads it on its first cycle. The startup line `learned-state horizons`
shows `snapshot` as `true` when it is on. Its error counters read 0 both when
it is off and when it works; `wva_learned_state_snapshot_bytes` above 0 on the
leader confirms it is writing.
If that read fails it is retried every minute; until it succeeds, for at
most 10 minutes, a lower bound (`P2-borrow`, `P2-prior`) is read at full
strength and none is started or moved, so none started in that time
replaces the stored one. After 10 minutes bounds start as usual, and a read
that succeeds later keeps those over the stored ones.

What is restored, and what is not:

- a variant's measured capacity, saturated throughput and replica start
  estimate are restored **if it runs the same engine arguments** as when they
  were learned. A deployment whose flags come from a variable the controller
  cannot resolve gets back only its measured capacity and saturated
  throughput (see below): a changed value would look identical, so its start
  estimate and ITL baseline are not restored. A new engine image with the same arguments
  keeps them; only the ITL baseline, a latency figure, needs the same image
  too;
- a variant whose arguments changed while the controller was down restores
  its old measured capacity only as a **lower-only, expiring figure**, and
  its old saturated throughput only as a hold-only floor (below). The
  capacity (`P2-prior` in the [cycle log](cycle-log.md)) is used where it
  is below what the variant would otherwise be priced at, for 30 minutes of
  serving at full strength, then relaxes to the variant's own figure over 6
  hours of serving -- the same rule as a capacity borrowed from a sibling.
  The 30 minutes start when an output-length bucket first uses it, not at the
  restart; one no bucket uses goes after the bucket retention. Serving means a
  Ready replica of the variant's own with requests on it: one idle at
  `minReplicas` overnight does not use up the hold. It is written
  back with its clock, so another restart inside those hours resumes it.
  Its old saturated throughput comes back too, but only to hold the
  throughput floor: it may keep the fleet, never grow it, for at most 30
  minutes plus 6 hours of serving, until the new arguments measure their
  own. A saturated throughput measured under the arguments it runs now is
  restored as its own. Its start estimate and ITL baseline are not restored.
  The same happens without a restart: a rollout that changes the arguments
  while the controller runs turns the old capacity into the same lower-only
  figure, and the old saturated throughput into the same hold-only floor
  (below). A newer figure -- the variant saturated under the arguments it
  then changes away from -- replaces it and restarts the 30 minutes; a
  change that measured nothing new does not, so one figure is held for at
  most 6.5 hours of serving however often the arguments change. The figure
  is the one measured last, and it goes a bucket retention after it was last
  used. The ITL line is fitted afresh under the new arguments;
- a **rollback** to engine arguments a variant ran before gets back the
  capacity and saturated throughput it measured under them, as its own, for
  as long as they would have been retained (the bucket retention), across a
  restart too. While any rollout is in progress -- an image-only one too --
  nothing new is measured (`P1-obs-rollout` in the cycle log): a reading may
  come from an old pod. The fleet is priced from what the variant already
  holds, so a rollout that lowers capacity is priced on the figure from
  before it until the new pods saturate after it ends. A replica that is
  saturated below that figure is priced at its reading for that cycle only
  (`P1-obs-unadmitted`), and nothing is kept of it -- so a bucket with no
  figure at all is not priced at k1 while it shows full. A pod that is not
  Ready -- an old one draining, a new one failing its readiness probe -- is
  never priced at its reading. The ITL line takes no readings either. A lower-only figure
  from a change of arguments waits for the rollout, and its 30 minutes start
  when the rollout ends, however long it stalled. A LeaderWorkerSet held
  part way by a partition learns nothing until it is let through. An undo or a restart in mid-rollout is a rollout
  like any other. A LeaderWorkerSet scale-up is not a rollout; a template
  change its controller has not acted on yet is not seen as one until a
  group has moved;
- across a change of arguments the old saturated throughput still holds the
  throughput floor -- it may hold the fleet, never grow it -- until the new
  arguments measure their own, for at most 6.5 hours of serving;
- a variant whose engine configuration was not read completely (flags from an
  `envFrom`) gets its own figures back after a restart if it runs the same
  unread configuration, and lower-only figures otherwise;
- each measured capacity and throughput is checked against the arguments it
  was itself measured under, not only the variant's: one measured before a
  rollout the controller saw, and written after it, is restored as the old
  arguments' figure, so it too is only a lower-only figure;
- a parallel size computed when the pod starts -- llm-d's wide-EP
  LeaderWorkerSet passes `--data-parallel-size $((LWS_GROUP_SIZE *
  DP_SIZE_LOCAL))` -- does not stop a variant restoring its own state: the
  expression is compared as written. It only keeps the variant from sharing a
  latency line with another (`wva_engine_config`'s `sharing_unresolved`);
- the first restart after an upgrade that changes how the configuration is
  identified (the `fingerprint_version` label of `wva_engine_config`), or
  from a release that did not record the engine arguments, restores each
  variant's measured capacity only as that lower-only figure
  (`restores_total{outcome="differs"}`, or `"prior"`, and `P2-prior` in the
  cycle log). With `differs`, its saturated throughput, start time and
  latency baseline are not restored; with `prior`, as for any change of
  arguments, the old saturated throughput holds the floor hold-only (above);
- **rolling the controller back** to the previous release, which already
  records engine arguments under the same `fingerprint_version` (3), restores
  each variant whose configuration it can read whole, as an ordinary restart
  does. It does not see what only this release writes: the windows set aside
  under other arguments for a rollback, the new fields of a k2 lower bound,
  or the record of a forget annotation acted on. A variant's previous
  lower-only figure survives only because this release also writes it in the
  form the previous one reads. Upgrading again restores what the previous
  release wrote, whole. Only a rollback to a release that did not record
  engine arguments, or that has another `fingerprint_version`, starts every
  variant cold once, and upgrading from it restores lower-only figures. The
  document keeps one key across releases on purpose: under a new key, the
  older release would read the document it wrote before the upgrade and
  install figures days stale as its own;
- the ages travel with the figures, so a restore never makes a figure younger,
  and one past its horizon above is not restored;
- a figure the new controller has already measured is never replaced;
- a k2 borrow keeps its place across the restart: a borrow that has worn off
  stays off, and the time the controller was down is not counted beyond a
  few minutes' allowance for a missed cycle. This holds for a variant whose
  engine configuration was not read completely too: a borrow can only make a
  variant more conservative, for a bounded time, so it is restored without
  the configuration check;
- a variant forgotten on request (the `llm-d.ai/forget-learned-state`
  annotation) stays forgotten, and the annotation is not acted on again.

The log line `Learned state: snapshot restored` says how many variants are
pending their first cycle; a `learned state restored` line (or, with nothing
to keep, `learned state not restored`) follows for each on its first cycle. Writing the snapshot needs the Role `wva-learned-state-role` (the manager Role reads it, in every install mode), which
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
`make disable-limiter` restart the controller for you. The schema,
scopes and validation rules are in the [quota limiter reference](quota-limiter.md).
