# Install a small llm-d model

## Overview

This deploys one small model, `Qwen/Qwen3-0.6B`, with an EPP and an
InferencePool, on a single GPU. It gives the scaling manager something real to
scale. Use it to try the scaling manager, to reproduce a scaling question, or as
the target for [`make benchmark-smoke`](../benchmarking/).

It installs the model only. [Install the scaling manager in a
namespace](../install-in-namespace/) comes next; that install refuses a
namespace with no model servers, so the model comes first.

llm-d's own guides need a fleet: [Optimized Baseline][ob] defaults to
`Qwen/Qwen3-32B` on 16 GPUs. No GPU at all? Use [Test the scaling manager
against a full llm-d stack](../testing-with-llm-d/), which runs simulators on
kind.

## Prerequisites

- [ ] One GPU, on a node that will schedule onto it.
- [ ] `kubectl`, and rights to create workloads in your namespace.
- [ ] `git`, `helmfile`, and `uv` (or Python 3 with `venv`). The standup stops
  and prints install commands if one is missing.

<!-- guide:prerequisites start -->
```bash
# Where the model goes, and which model.
export NAMESPACE=<your-namespace>
export MODEL_ID=Qwen/Qwen3-0.6B

# List nodes with allocatable GPUs. You need at least one.
kubectl get nodes -o custom-columns=NODE:.metadata.name,GPU:.status.allocatable.'nvidia\.com/gpu' --no-headers
```
<!-- guide:prerequisites end -->

## Installation Instructions

<!-- guide:deploy.standup start -->
```bash
# Deploy the model server, its EPP and InferencePool. BENCHMARK_WVA_DEPLOY=false leaves the scaling manager out.
make benchmark-standup BENCHMARK_NAMESPACE=${NAMESPACE} MODEL_ID=${MODEL_ID} \
  BENCHMARK_WVA_DEPLOY=false
```
<!-- guide:deploy.standup end -->

It also holds the engine image on the accelerator nodes (the `prepull-*` pods;
`BENCHMARK_PREPULL=false` skips that). The EPP it deploys has the `flowControl`
feature gate on, which the next guide requires.

Bringing your own EPP, or one from an llm-d guide? Its gate is off unless you
turned it on. Check, then add it to the `EndpointPickerConfig` and restart the
EPP:

```bash
# Show the feature gates in the namespace's ConfigMaps
kubectl get cm -n ${NAMESPACE} -o yaml | grep -A3 featureGates
```

```yaml
featureGates:
- flowControl
```

### Then install the scaling manager into that namespace

Follow [Install the scaling manager in a namespace](../install-in-namespace/)
with the same `NAMESPACE`. In short:

```bash
export NAMESPACE=<the namespace you just used>
make setup-prereqs        # cluster admin, once per namespace
make deploy-wva           # the controller
make scaledobjects-plan WVA_DEFAULT_SO_PLAN=wva-plan.yaml
# edit wva-plan.yaml: apply: yes|no|adopt, the modelID, the replica bounds
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```

Nothing scales until a ScaledObject exists.

## Verification

<!-- guide:verify.serving start -->
```bash
kubectl get pods -n ${NAMESPACE}
```
<!-- guide:verify.serving end -->

The decode pod, the EPP and one `prepull-*` pod per accelerator node should be
`Running`. A decode pod in `CrashLoopBackOff` is almost always the [memory
budget](#memory-budget): look for `torch.OutOfMemoryError` in its log first.

<!-- guide:verify.model start -->
```bash
# Ask the endpoint what it loaded. This is the name every request must use.
kubectl run probe -n ${NAMESPACE} --rm -i --restart=Never --quiet \
  --image=registry.k8s.io/e2e-test-images/agnhost:2.47 --command -- \
  /bin/sh -c "curl -s http://$(kubectl get svc -n ${NAMESPACE} -o name | grep epp | head -1 | cut -d/ -f2).${NAMESPACE}.svc.cluster.local:80/v1/models"
```
<!-- guide:verify.model end -->

The returned `id` is the *served* name, which is not always the path the weights
came from.

## Benchmarking

Once the scaling manager is installed too, this shows whether it scales the
model:

<!-- guide:verify.smoke start -->
```bash
# Drive load through it and snapshot the dashboard.
make benchmark-smoke NAMESPACE=${NAMESPACE}
```
<!-- guide:verify.smoke end -->

Five minutes of 1024-in/1024-out at 15 req/s, then a dashboard snapshot. It
checks KEDA, the controller, the model servers, the EPP and the ScaledObject
first, and names every gap at once. For numbers to compare, use [Benchmark the
scaling manager](../benchmarking/).

## Cleanup

<!-- guide:cleanup.teardown start -->
```bash
make benchmark-teardown BENCHMARK_NAMESPACE=${NAMESPACE}
```
<!-- guide:cleanup.teardown end -->

This removes the scaling manager first, then the model. To keep the model and
remove only the scaling manager, use `make undeploy-wva`
([how](../install-in-namespace/#cleanup)). Check nothing cluster-scoped was left:

```bash
# Any cluster RBAC still named for this namespace
kubectl get clusterrole,clusterrolebinding -o name | grep -i "${NAMESPACE}"
```

## Configuration

| Parameter | Default | Notes |
| --- | --- | --- |
| `BENCHMARK_NAMESPACE` | none (required) | where everything lands |
| `MODEL_ID` | `Qwen/Qwen3-0.6B` | the served model |
| `GPU_MEM_UTIL` | `0.90` | fraction of **total** GPU memory, one value for every model |
| `WARM_REPLICAS` | `0` | FMA warm pool size, if the scenario uses one |

## Details

### Memory budget

The standup sets vLLM's `gpuMemoryUtilization` to 0.90 for every model, so a
small model gets so much KV cache that one replica rarely looks full. To make it
scale on a small load, lower `kvCacheThreshold` in the scaling policy rather
than shrinking the cache. The out-of-memory failure and the override
(`GPU_MEM_UTIL`) are in [The engine's memory
budget](../../reference/workload-preparation.md#the-engines-memory-budget-gpumemoryutilization).

With flow control on, a backlog shows in the EPP's queue (the **EPP Flow-Control
Queue** dashboard panel), not in `vllm:num_requests_waiting`.

### Weights, once you scale it

With one replica the weights are fetched once. Once the scaling manager adds
replicas, each new pod fetches them again unless there is a shared cache.
llm-d's modelservice chart mounts one at `/model-cache` by default. Confirm it:

```bash
# Report the model cache, drain hook and engine caches, and write a patch for what is missing
make workload-patch NAMESPACE=${NAMESPACE}
```

Do not just list the pod's PVCs: a claim mounted at one path while the engine
downloads to another passes that check and still re-downloads. The command above
checks where the weights actually land. See [Weights and the model
cache](../../reference/workload-preparation.md#weights-and-the-model-cache). For
a large model, `BENCHMARK_MODEL_HOSTPATH=<dir>` on the standup (or `make weights`
for any deployment) puts a copy on every accelerator node's disk ([Weights on the
node's disk](../../reference/workload-preparation.md#weights-on-the-nodes-disk)).

## Next

1. [Install the scaling manager in a namespace](../install-in-namespace/).
2. `make benchmark-smoke NAMESPACE=<ns>`, once everything is deployed.
3. [Benchmark the scaling manager](../benchmarking/), for numbers to compare.
4. [After the install](../../reference/operations.md): what the metrics mean.

[ob]: https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline
