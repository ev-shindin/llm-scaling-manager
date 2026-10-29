# Benchmark the scaling manager

## Overview

This stands up an llm-d stack with the scaling manager on a GPU cluster, drives
load through it with [llm-d-benchmark](https://github.com/llm-d/llm-d-benchmark),
and reports latency, replica counts and cost per run. Use it to compare scaling
behaviour across configurations: thresholds, limiters, one variant against two.

No GPUs? See [Test the scaling manager against a full llm-d
stack](../testing-with-llm-d/). Already installed and just want to see it scale?
`make benchmark-smoke NAMESPACE=<ns>` sends five minutes of load and snapshots
the dashboard, with no standup.

## Prerequisites

- [ ] A GPU cluster.
- [ ] A **ReadWriteMany** StorageClass for the model cache
  ([why](#the-model-cache-must-be-readwritemany)).
- [ ] An `IMG` built from this branch, or a release image from a checkout of
  the same release. Upstream WVA's images reject a flag these manifests pass
  ([why](#img-decides-what-is-measured)).
- [ ] Running Fast Model Actuation in the namespace? Read
  [FMA namespaces](#fma-namespaces) first.

<!-- guide:prerequisites start -->
```bash
# The namespace to benchmark in, and the controller image to measure.
export BENCHMARK_NAMESPACE=<namespace>
export IMG=<your build>

# Install the llm-d-benchmark CLI.
make benchmark-install
```
<!-- guide:prerequisites end -->

## Installation Instructions

### 1. Stand up the stack

<!-- guide:deploy.standup start -->
```bash
make benchmark-standup BENCHMARK_NAMESPACE=${BENCHMARK_NAMESPACE} IMG=${IMG}
```
<!-- guide:deploy.standup end -->

It deploys the model servers through llm-d-benchmark, installs the scaling
manager from this repo, and registers the workloads. Check the `Model cache:`
line it prints:

```text
Model cache:
  <claim>  <size>  [ReadWriteMany]  Bound  class=<your-rwx-class>
```

Keep the namespace between runs you want to compare: a re-run there reuses the
cached weights, and a fresh namespace downloads them again.

The standup also:

- **adds a drain hook** to the model servers (a `sleep 45` preStop under a 120 s
  grace period) and waits for the restart. This shifts scale-down timing by about
  the drain window, so only compare runs taken on the same side of this change.
  `BENCHMARK_DRAIN_ROLLOUT_TIMEOUT` (default 900 s) bounds the wait.
- **holds the engine image on every accelerator node** before deploying.
  Check with `make prepull-status NAMESPACE=$BENCHMARK_NAMESPACE`.
- **refuses to overwrite** an EPP or controller already in the namespace
  ([standing up beside something](#standing-up-beside-something-that-already-exists)).

## Verification

### Run a workload

<!-- guide:verify.run start -->
```bash
make benchmark-run BENCHMARK_NAMESPACE=${BENCHMARK_NAMESPACE}
```
<!-- guide:verify.run end -->

<!-- guide:verify.report start -->
```bash
make benchmark-report
```
<!-- guide:verify.report end -->

`make benchmark-report` renders a markdown table from the newest results. A run
worth keeping has, per scenario:

- a non-zero request count and **0** errors;
- a replica timeline that moves. A variant flat at one replica through a deep
  queue means the scaling manager never saw the load.

If queue depth and replicas disagree, see [After the
install](../../reference/operations.md) before re-running.

**Restart the controller between runs** with `make benchmark-restart-controller`.
Otherwise learned capacity and the request-shape memo carry into the next run,
and its first minutes look like a shape change. The target scales the
controller to zero, deletes the `wva-learned-state` snapshot, and scales it
back, so a controller with `LEARNED_STATE_SNAPSHOT` on starts cold too; a plain
restart, or a new image digest, would restore it. No controller runs for the
few seconds in between, so run it between runs, not during one.

### Check the router spreads load

A fleet can scale correctly and still serve almost everything from one replica.
Check before you trust a latency number:

```bash
# Prefix-cache hit ratio. 0.000 means the prefix scorer is not earning its weight.
curl -sG "$PROM/api/v1/query" --data-urlencode \
  'query=sum(rate(llm_d_epp_prefix_indexer_hit_ratio_sum[5m]))/sum(rate(llm_d_epp_prefix_indexer_hit_ratio_count[5m]))'

# Running requests per replica. Many idle Ready replicas means the router is concentrating.
curl -sG "$PROM/api/v1/query" --data-urlencode 'query=vllm:num_requests_running'
```

If the hit ratio is zero and few replicas serve, weight the spreading scorer
above the prefix scorer before reading latency ([why](#router-weights)). Prefer
the harness's `analysis/summary.txt`: its TTFT is measured at the client and
includes the router wait that `vllm:time_to_first_token_seconds` misses.

### Snapshot the run

Prometheus ages the data out, so capture it while it is there:

```bash
# Run every benchmark-dashboard query over the window into panels.json
hack/benchmark/snapshot.py \
  --namespace ${BENCHMARK_NAMESPACE} \
  --prometheus-url https://thanos-querier-openshift-monitoring.apps.<cluster>/ \
  --token "$(oc whoami -t)" --insecure \
  --since 30m --out <run-dir>/snapshot

# Render one PNG per panel, offline, through the real dashboard (needs docker)
hack/benchmark/snapshot-images/render.sh <run-dir>/snapshot
```

See [Snapshot details](#snapshot-details) for the operational dashboard and the
namespace variable.

## Cleanup

<!-- guide:cleanup.teardown start -->
```bash
make benchmark-teardown BENCHMARK_NAMESPACE=${BENCHMARK_NAMESPACE}
```
<!-- guide:cleanup.teardown end -->

It removes the scaling manager first, then the llm-d releases, so the
cluster-scoped RBAC goes too.

## Configuration

Only `BENCHMARK_NAMESPACE` is required.

| Parameter | Default | Example |
| --- | --- | --- |
| `BENCHMARK_NAMESPACE` | none (required) | `my-bench` |
| `IMG` | a build of this branch | `ghcr.io/you/wva:dev` |
| `BENCHMARK_SPEC` | `guides/workload-autoscaling` | `guides/epp-keda-saturation` |
| `BENCHMARK_HARNESS` | `guidellm` | `inference-perf` |
| `BENCHMARK_WORKLOAD` | `prefill_heavy` | `flat_8k1000_10rps_12m` |
| `MODEL_ID` | `Qwen/Qwen3-0.6B` | `Qwen/Qwen3-32B` |
| `BENCHMARK_REPO_REF` | `v0.7.8` | `main` |
| `BENCHMARK_IMAGE_TAG` | the value of `BENCHMARK_REPO_REF` | `nightly` |
| `BENCHMARK_HARNESS_RUN_AS_USER` | `0` | `1000` |
| `BENCHMARK_KEDA_SCALE_UP_PERIOD` | `5` | `15` |
| `BENCHMARK_KEDA_SCALE_UP_STABILIZATION` | `0` | `60` |
| `BENCHMARK_KEDA_SCALE_DOWN_PERIOD` | `120` | `60` |
| `BENCHMARK_KEDA_SCALE_DOWN_STABILIZATION` | `300` | `120` |
| `BENCHMARK_ALLOW_EPP_REUSE` | `false` | `true` |
| `BENCHMARK_WVA_DEPLOY` | `true` | `false` |
| `BENCHMARK_PREPULL` | `true` | `false` |
| `BENCHMARK_PREPULL_IMAGES` | the engine image the harness pins | `ghcr.io/you/engine:tag` |
| `PREPULL_NODE_SELECTOR` | *(empty: every node with a known GPU product label)* | `example.com/accelerator=h200` |
| `PREPULL_TOLERATIONS` | *(empty: `nvidia.com/gpu` only)* | `dedicated,example.com/pool` |
| `BENCHMARK_MODEL_HOSTPATH` | *(empty: the shared volume)* | `/mnt/local/wva-weights` |
| `BENCHMARK_ENGINE_CACHE_HOSTPATH` | `BENCHMARK_MODEL_HOSTPATH` | `/mnt/local/wva-weights` |
| `BENCHMARK_ENGINE_CACHE_SEED` | `auto` | `none` |

Notes on the defaults:

- **`BENCHMARK_IMAGE_TAG` follows `BENCHMARK_REPO_REF`.** The harness pod runs
  the checkout's scripts, so the image must match the ref. Mixing them fails at
  the first treatment, for example `Error: No such command 'run'`.
- **`BENCHMARK_HARNESS_RUN_AS_USER=0`**, because the harness writes to
  `/usr/local/bin` at startup. On OpenShift any other UID dies with
  `cp: cannot create regular file ...: Permission denied`, and `anyuid` alone may
  lose to a higher-priority SCC.
- **The scale-up stabilization window (`0`) is the knob that matters.** HPA
  takes its most conservative recommendation across the window; the periods are
  only rate limits. Scale-down waits 300 s, because removing a replica too early
  costs a cold start, and keeping one too long only costs money.
- **The default model is small on purpose.** A 0.6B model exercises the same
  scaling path as a 32B one, on one GPU per replica. Its latency says nothing
  about a 32B model. Pass `MODEL_ID=Qwen/Qwen3-32B`, or `BENCHMARK_MODEL_ID=`
  (empty) to use the scenario's own model.

## Next

- [Two models, anti-phase bursts, warm pool on and off](two-model-warm-pool.md)
- [Two models, anti-phase bursts, under one quota](two-model-utilization-share.md):
  today's optimizer, the utilization-share optimizer in shadow, and acting
  ([what it measured](../../well-lit-paths/utilization-share/measured.md))
- [FMA post-mortem](../../proposals/fma-post-mortem.md)
- [After the install](../../reference/operations.md): what the metrics mean
- [Configuration](../../reference/configuration.md): every installer variable

## Details

### The model cache must be ReadWriteMany

The two scaling-manager scenarios in this repo ask for RWX. The
`guides/epp-keda-saturation` scenario (`BENCHMARK_DIRECT_KEDA=true`) comes from
llm-d-benchmark and does not, so check the `Model cache:` line there. An RWO
cache stands up fine with one replica, then the first scale-up sits Pending on a
volume it cannot attach. Many clusters' default StorageClass is RWO (kind's
`standard` is). The standup warns when the bound claim is not shareable.

### `IMG` decides what is measured

Nothing in the results says which binary produced them. Upstream WVA's images
(`ghcr.io/llm-d/...`) reject `--external-scaler-bind-address`, which these
manifests pass, so a run against one measures a CrashLoopBackOff. Set `IMG` to
your own build whenever you change controller code.

To benchmark a release of this project, check out its tag so the manifests
match the image, and pin the image by tag or digest. The digest of each release
is in its notes on the
[Releases page](https://github.com/ev-shindin/llm-scaling-manager/releases):

```bash
git checkout v0.10.2
export IMG=ghcr.io/ev-shindin/llm-scaling-manager:v0.10.2
# or the exact build
export IMG=ghcr.io/ev-shindin/llm-scaling-manager@sha256:<v0.10.2 digest, from its release notes>
```

`:main` is rebuilt on every push to `main`, so two runs against it a day apart
may measure different code.

### Why the drain hook

A replica removed mid-stream cuts its open responses. Each scale-down then adds
client-side stream failures to the TTFT percentiles and error counts the run
measures, so comparing two policies would partly measure which one removed more
pods. The patch is safe here because the standup deployed these workloads; it is
pinned to `BENCHMARK_NAMESPACE`. `make benchmark-add-variant` patches the
variant it adds too. The ScaledObject plan is written before the patch, so it
reports `vllm declares no preStop hook` for workloads that get one seconds later.

### Router weights

The scorer that sends a request to the least busy endpoint (`active-request-scorer`,
or `queue-scorer` on a non-disaggregated profile) is weighted **below**
`prefix-cache-scorer` in the EPP configs shipped here:
`deploy/lib/epp-optimized-baseline.values.yaml` gives 3 against 2. With no shared
prefixes, the prefix scorer adds no signal but still outranks the others.
Measured on this benchmark: nine decode replicas Ready, **one** serving, 766
requests queued in the router, and engine latency reading 0.04 s. Weighting the
spreading scorer higher gave nine of nine serving and cut the phase-2 fleet from
a median of nine replicas to three. See "Judge the ramp on the client's TTFT" in
[analyzer-evidence](../../developer-guide/analyzer-evidence.md).

### FMA namespaces

This stack renders a PodMonitor named `vllm-<model>`, the same name the FMA guide
uses. A scenario without `fma.enabled` **overwrites the FMA one, and the
launchers silently stop being scraped**. Standup warns about it. Also:

- `BENCHMARK_KEDA_MAX_REPLICAS` is a starting value, not the ceiling. An FMA
  variant is capped at the launcher pods present. Raising it only adds Pending
  requester pods, which suppress the scale-up they were meant to give.
- The launcher pool does not grow with load. It is a fixed count per node.

The placement check at the top of standup is described in [the FMA
post-mortem](../../proposals/fma-post-mortem.md).

### Standing up beside something that already exists

A standup applies a whole stack unconditionally. On a shared cluster that can
break someone else's workload with no error. Three refusals guard it:

- **An EPP already serving in the namespace.** Standup stops and offers: a
  clean namespace, `make benchmark-run` alone against what is there, or
  `BENCHMARK_ALLOW_EPP_REUSE=true` to re-render deliberately.
- **A controller already running.** Standup prints the running image against
  the new one. `BENCHMARK_WVA_DEPLOY=false` reuses the installed controller.
- **The `prometheus-adapter-resource-reader` ClusterRole.** The standup creates
  it so llm-d-benchmark does not install prometheus-adapter, which would fight
  KEDA for the `external.metrics.k8s.io` APIService. If the object already
  exists, the standup leaves it alone and says so; rewriting its Helm ownership
  would break the real release's next `helm upgrade`.

### Replica start time in the harness

Every ramp is sized by how long a new replica takes to become Ready
([why](../../reference/workload-preparation.md#the-rest-of-the-start-path)).
llm-d-benchmark adds steps of its own, and a benchmark that keeps them measures
the harness, not the autoscaler. The scenarios under
`hack/benchmark/scenarios/guides/` fix three (package installs, the startup
probe, engine caches); copy those blocks into a scenario of your own.
`patch_harness.sh` fixes a fourth (the `libcuda` walk) for every scenario. Two
more belong to the cluster: the image and, for large models, the weights.

**The image on every node.** A replica on a node without the engine image pulls
10-20 GB first. The standup holds the image the harness pins on every
accelerator node before deploying (one sleeping DaemonSet per image, no
accelerator requested). `BENCHMARK_PREPULL=false` skips it,
`BENCHMARK_PREPULL_IMAGES` overrides it, and `PREPULL_TOLERATIONS` adds taints.
`PREPULL_NODE_SELECTOR` picks the nodes: one or more `key=value` pairs,
comma-separated, all of which a node must carry. It replaces the default (nodes
with a known GPU product label), so include the product label to stay on GPU
nodes. The scenarios pull the engine
`IfNotPresent`. See [Holding the image on the
nodes](../../reference/workload-preparation.md#holding-the-image-on-the-nodes).

**The weights on every node's disk.** `BENCHMARK_MODEL_HOSTPATH=<dir>` turns on
the harness's node-local mode: `model-pvc` binds to a hostPath volume, a
DaemonSet downloads the model to every accelerator node (the placement
`make prepull` uses; `PREPULL_NODE_SELECTOR` replaces it, `PREPULL_TOLERATIONS`
included), and the engines deploy
once every node holds it. For a 0.6B model this changes nothing measurable; it
is for models where reading over the shared volume dominates start time.

```bash
# One downloader per accelerator node, Ready when its download finished
kubectl get pods -n $BENCHMARK_NAMESPACE -l component=model-download -o wide
```

What to know about node-local weights:

- The harness waits for every node the DaemonSet counts, including cordoned
  nodes and nodes under `DiskPressure`. The standup keeps the downloader off
  nodes an engine cannot use (cordoned, or with an untolerated `NoSchedule` /
  `NoExecute` taint) with a `NotIn` hostname affinity, and names them. It never
  changes the nodes.
- The standup refuses a namespace whose `model-pvc` already uses another
  storage class. Use a fresh namespace or delete the claim.
- The downloader is the harness's, not `make weights`
  ([Weights on the node's disk](../../reference/workload-preparation.md#weights-on-the-nodes-disk)),
  because the harness owns `model-pvc`'s name and the engine's `pvc://` URI. It
  is much more privileged: it mounts the hostPath with `seLinuxOptions.type:
  spc_t` (Pod Security `privileged`; on OpenShift only the `privileged` SCC,
  which is why the standup is cluster-admin-only there), automounts its
  ServiceAccount token, `pip install`s an unpinned `huggingface_hub` at every
  start, and writes the HF token to `/tmp`. On RHCOS use a directory under
  `/var`. Not yet run on OpenShift.
- A copy with the completion marker `<dir>/models/<id>/.download-complete`
  (written by both `make weights` and this downloader) is reused. A copy without
  it goes back through `snapshot_download`.
- `patch_harness.sh` fixes the harness here: fix 11 adds a readiness probe on
  the completion marker, skips the SELinux relabel only where SELinux is absent,
  and labels at level `s0`. Fix 12 names the volume per namespace with a
  `claimRef`, and scopes the teardown's `kubectl delete pv -l usage=model-cache`.
  Unpatched, a second benchmark namespace stays Pending forever, and any
  teardown deletes every such volume in the cluster.

**Package installs at engine start.** The `preprocess` init container writes
`/shared-config/llmdbench_env.sh`, which every engine sources before
`vllm serve`. It runs `apt-get install` for `iproute2` (needed only for the
`ip route` / `ip rule` lines on a multi-NIC RDMA node) and `infiniband-diags`
(never used when serving). Mirror speed then varies start time day to day. The
scenarios wrap the init container's command to strip `infiniband-diags` always,
and `iproute2` unless routing lines are present.

**The startup probe.** The harness default is `30 / 30 / 60`
(`initialDelaySeconds / periodSeconds / failureThreshold`). Fix 10 makes it
`1 / 5 / 360`: the same 30-minute budget, polled faster (1, not 0, because the
API server drops a zero). Scenarios with their own `probes:` block use the same
numbers.

**The `libcuda` walk.** The harness preamble ran two `find / -name libcuda.so.1`
walks across every mount, 0.6-1.0 s each on fast local disks, and a network walk
on a large shared cache. Fix 14 asks the loader cache (`ldconfig -p`, 2 ms) and
falls back to `/usr` and `/opt` only. A scenario that writes its own preamble
keeps what it wrote.

**Engine caches.** The model PVC is mounted read-only, so the scenarios put the
compile caches on the harness's `workload-pvc` at `/engine-cache` and point
`VLLM_CACHE_ROOT`, `FLASHINFER_WORKSPACE_DIR` and `TRITON_CACHE_DIR` there. A
guard unsets any that is not writable, so the engine compiles instead of failing.
That fallback was the start-time variance: nine starts on three nodes split into
51-56 s and 75-81 s, and the slow nodes had `workload-pvc` mounted read-only
(compile 14.4 s against 2.9 s cached). If you edit that command: Kubernetes
rewrites `$$` and expands `$(NAME)` in `command`/`args`, and the generated
script has no trailing newline.

`BENCHMARK_ENGINE_CACHE_HOSTPATH=<dir>` puts the caches on the node's disk
instead: `deploy/enginecache.sh apply` creates a hostPath volume at
`<dir>/engine-cache`, a claim named `engine-cache` and a preparing DaemonSet
([Engine caches on the node's disk](../../reference/workload-preparation.md#engine-caches-on-the-nodes-disk)),
and `hack/benchmark/engine_cache_claim.sh` points the scenario at that claim. The
standup fails if the claim is not Bound. The first start per node compiles; the
rest hit. To avoid a cold node at scale-up (measured: a replica Ready 96 s after
it was wanted), `BENCHMARK_ENGINE_CACHE_SEED=auto` copies the old
`workload-pvc` cache to each node when that claim exists; `none` seeds nothing;
`<claim>[:<subPath>]` seeds from that claim. `make engine-cache-status
NAMESPACE=$BENCHMARK_NAMESPACE` lists the nodes.

To check the torch.compile cache is on every node before a pass, do not
start a second replica. Ask the standup's first replica of each role which
key it compiled under, then check that key on every node, with no GPU:

```bash
make engine-cache-keys NAMESPACE=$BENCHMARK_NAMESPACE ENGINE_CACHE_PODS='<decode pod> <prefill pod>'
make engine-cache-verify NAMESPACE=$BENCHMARK_NAMESPACE ENGINE_CACHE_KEYS='<key>[,<key>]'
```

Prefill and decode compile under different keys, so name one pod of each role.
Copy the keys as `engine-cache-keys` prints them, rank list included.
Run `make engine-cache-publish` first if the nodes have not been seeded with
what the first starts compiled. For a multi-node engine, name the leader and
every worker pod; the check then passes only after a publish. A published key
is owned by root: if the engines do not run as root, add
`ENGINE_CACHE_UID=$(kubectl exec -n $BENCHMARK_NAMESPACE <engine pod> -- id -u)`,
or the check judges it for root. Without it, the last line names the user the
check assumed (`VERIFIED for root`, `VERIFIED for uid 1000`). The check does
not cover the FlashInfer and Triton caches. What each result means:
[Check the cache without a second start](../../reference/workload-preparation.md#check-the-cache-without-a-second-start).

After a standup, check what was rendered:

```bash
D=$(kubectl get pods -n $BENCHMARK_NAMESPACE -l llm-d.ai/role=decode -o jsonpath='{.items[0].metadata.name}')
# Probe timing and cache paths on the engine container
kubectl get pod -n $BENCHMARK_NAMESPACE $D -o jsonpath='{range .spec.containers[?(@.name=="vllm")]}startup {.startupProbe.periodSeconds}s x{.startupProbe.failureThreshold}{"\n"}{range .env[*]}{.name}={.value}{"\n"}{end}{end}' | grep -E 'startup|CACHE|FLASHINFER|TRITON'
# apt-get runs left: 0 on a single-NIC node, 1 where routing lines exist
kubectl exec -n $BENCHMARK_NAMESPACE $D -c vllm -- grep -c apt-get /shared-config/llmdbench_env.sh
# Cache hits; "not writable" means the guard fell back to the engine default
kubectl logs -n $BENCHMARK_NAMESPACE $D -c vllm | grep -E 'torch.compile took|Using cache directory|not writable'
# 0 expected: the preamble no longer walks /
kubectl get pod -n $BENCHMARK_NAMESPACE $D -o jsonpath='{.spec.containers[?(@.name=="vllm")].args}' | grep -c 'find / -name libcuda'
# With BENCHMARK_ENGINE_CACHE_HOSTPATH: the claim name, and no subPath
kubectl get pod -n $BENCHMARK_NAMESPACE $D -o jsonpath='{range .spec.volumes[?(@.name=="engine-cache")]}{.persistentVolumeClaim.claimName}{"\n"}{end}{range .spec.containers[?(@.name=="vllm")].volumeMounts[?(@.name=="engine-cache")]}{.mountPath} subPath={.subPath}{"\n"}{end}'
# Scheduled time against container start: seconds apart means the image was on the node
kubectl get pod -n $BENCHMARK_NAMESPACE $D -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].lastTransitionTime}{" scheduled, container up "}{.status.containerStatuses[?(@.name=="vllm")].state.running.startedAt}{"\n"}'
```

### Snapshot details

`snapshot.py` is stdlib-only, so it runs in the benchmark venv. `render.sh`
starts Grafana and its image renderer in docker with this repo's dashboard, and
writes one PNG per panel plus the whole dashboard to
`<run-dir>/snapshot/<dashboard-slug>/`. The data is a file, so a run can be
re-rendered months later with no cluster.

To capture the operational dashboard instead (useful for checking a layout
change, since a cluster Grafana without the renderer plugin cannot):

```bash
# Capture and render the operational dashboard
hack/benchmark/snapshot.py --dashboard deploy/grafana/operational-dashboard.json ...
DASHBOARD=$PWD/deploy/grafana/operational-dashboard.json \
  hack/benchmark/snapshot-images/render.sh <run-dir>/snapshot
```

The dashboard's **Namespace** variable is filled from `wva_current_replicas`.
The **Namespace label** variable applies to the `wva_*` panels only: those
series carry the workload namespace as `exported_namespace`, while engine and
EPP series carry plain `namespace`. `--namespace` selects it for a capture.
`make dashboards-check` enforces that split and the panel layout.
