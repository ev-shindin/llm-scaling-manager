# llm-scaling-manager

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![CI](https://github.com/ev-shindin/llm-scaling-manager/actions/workflows/ci-pr-checks.yaml/badge.svg?branch=main)](https://github.com/ev-shindin/llm-scaling-manager/actions/workflows/ci-pr-checks.yaml)
[![Go](https://img.shields.io/github/go-mod/go-version/ev-shindin/llm-scaling-manager)](go.mod)

llm-scaling-manager lets several LLM models on llm-d share one GPU quota. Each
model gets the GPUs its load needs. The spare GPUs are kept running as
headroom, more of them for models you weight higher. When one model's load
falls while another's rises, GPUs move from the first to the second. It sizes
every model by the work its replicas can actually serve, and lets KEDA do the
scaling.

**Two P/D models bursting in turn under one 10-GPU quota: p95 time to first
token 0.09 s and 3.5–6.0 s with sharing, against 51–73 s and 96–100 s
without it, and no dropped requests against 116–123. It cost 39–43 % more
GPU-seconds.** (Two runs, one rise per model each, two 8B models on H200:
[measured](docs/well-lit-paths/utilization-share/measured.md#pd-two-disaggregated-models-under-one-quota).)
Shadow mode, which plans and moves nothing and so should match no sharing,
differed from it by up to 32 s, and in one run the quota did not bind. Without P/D, the latest run served the first
burst at once (median 0.07 s, against 25.6 s in shadow mode, which plans the
same and moves nothing); the second burst had the same p95, 287 s. That
run's GPU cost was not measured.

Sharing is **experimental**: nine benchmark runs so far, two models at 1 GPU
per Pod. Whole-node and multi-GPU pods, more than two models, a cluster-wide
quota and the physical GPU limiter are not yet measured on real hardware.

llm-d itself autoscales with KEDA, mainly on the router's (EPP) metrics, one
ScaledObject per Deployment, and leaves a shared GPU budget to Kueue; this
project plugs into the same KEDA, as an external scaler.
([Sharing a GPU budget, compared with Kueue](docs/comparison/gpu-sharing.md);
[KEDA alone, compared](docs/comparison/keda-alone.md);
[beyond per-model scaling](#beyond-per-model-scaling).)

**Choose it** for several models whose GPUs are paid for whether used or not
(on-prem, a reservation, a team allotment) and whose peaks come at different
times. **Do not** take sharing if you want idle GPUs given back (it holds
headroom on purpose), if there is only one model per quota, or if load swings
back within tens of minutes (a model that just gave GPUs waits about 12
minutes before it takes them back). For one model, one GPU type, or a
latency-SLO target, **use llm-d's KEDA recipes**
([where KEDA alone is enough](docs/comparison/keda-alone.md#where-keda-alone-is-enough)).

**[Share one GPU quota between models](docs/well-lit-paths/utilization-share/)**
(the scenario, its costs and evidence) ·
**[Turn it on, safely](docs/guides/utilization-share/)** ·
**[Why llm-scaling-manager](docs/why.md)** ·
**[Other scenarios](docs/well-lit-paths/)**

## The problem it solves

Without sharing, a GPU quota is first come, first served. A model that scaled
up earlier keeps its GPUs, and one that needs them later is refused, even when
the quota has room for both. The worst case is a swap: one model's load falls
while the other's rises, and the rising model waits for the falling model's
whole scale-down window before it can start a replica.

## How sharing works

1. **Each model first gets its need:** the GPUs that keep it at its scale-up
   threshold.
2. **The rest is headroom,** shared by weight (`standard` 1, `important` 2,
   `critical` 4), so a model that bursts first is already serving on spare
   replicas.
3. **A model too far from its share gets GPUs through a transfer.** One donor
   pod is chosen and removed through an ordinary scale-down, so it drains its
   in-flight requests. The receiver is raised only once those GPUs are free.

You turn it on in **shadow** first, where it reports what it would move and
moves nothing, then **active**. A floor (`minReplicaCount`) is never taken.
Details, and the cases where it does not fit:
[the well-lit path](docs/well-lit-paths/utilization-share/).

## Measured on GPU clusters

Single runs unless noted; each link has the method and caveats.

- **Sharing one quota**: the results are at the top of this page; all nine
  runs are in [sharing, measured](docs/well-lit-paths/utilization-share/measured.md).
- **Scale-ups at near-floor latency on 3 of 4 rises.** With a one-Pod warm
  pool, p95 time to first token per rise falls from **5.2–8.8 s to
  0.11–0.83 s**: within 50 ms of a two-replica floor on three rises, 831 ms
  against the floor's 127 ms on the fourth. It uses **12 % fewer GPU-seconds**
  than that floor and 17 % more than no pool
  ([warm pool, measured](docs/well-lit-paths/warm-pool-bridge/measured.md)).
- **It prices the request shape, not just the queue** (experimental P/D path).
  Qwen3-0.6B at a constant 6 req/s: about four minutes after the request shape
  changed, once the new shape's throughput was measured, it ordered a third
  decode replica and no queue formed. On a second exposure, with both shapes
  already seen, p95 TTFT was **155 ms with 0 errors in 13 288 requests**; a
  cold controller reached a 3.9 s p99
  ([P/D path, measured](docs/well-lit-paths/pd-disaggregation/measured.md);
  [known gaps](docs/why.md#known-gaps)).
- **A warm Pod switches models in about 437 ms**, against about 41 s for a
  cold start (the engine switch itself). In the warm-pool benchmark, the lend
  waits for the scale-up decision: it came 33–143 s after each rise
  ([why llm-scaling-manager](docs/why.md)).

## Models and variants

A **model** is what a client asks for by name. A **variant** is one way of
serving it: a GPU type, a serving configuration (tensor parallelism, batch
size, quantization), or a role such as prefill or decode. Each variant is one
KEDA ScaledObject plus the workload it scales. Variants whose triggers name the
same `modelID` are sized together. Creating the ScaledObject is the whole
registration.

## Key features

- **Sharing one GPU quota between models** (opt-in, **experimental**): need
  first, headroom by weight, GPUs moved by transfer when demand shifts.
- **Scaling from real load**: replica counts follow KV-cache use, queues and
  token throughput, per model and across its variants.
- **A GPU budget** (opt-in): the GPUs that are free, a per-namespace quota, or
  Kueue's quotas (**experimental**). Sharing needs one of them.
- **Cost-aware variant choice**: it grows the variant with the lowest cost per
  unit of capacity (`variantCost`). Unit-tested; an end-to-end proof and a
  two-GPU-type benchmark are on the roadmap.
- **Scale to zero and back**: KEDA's own scale to zero. The controller polls
  the router queue every 100 ms and pushes activation without waiting for
  KEDA's poll.
- **Warm-pool bridging** (opt-in): keep models loaded and asleep, and lend one
  while a variant's own replica starts.
- **Prefill/decode roles** (**experimental**).

## Installing

```bash
export NAMESPACE=<the namespace running llm-d>

make check-prereqs                    # read-only: tools, namespace, Prometheus
make setup-prereqs                    # ONCE per namespace, by a cluster admin
WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=<n>' \
  make deploy-wva                     # the controller, with the quota the models share
make workload-patch                   # writes a patch (drain hook, weights, engine cache); apply it through your model-server chart
make scaledobjects-plan               # list your model servers; nothing is applied
make scaledobjects-apply              # register them — this is what makes it scale

make utilization-share-check          # is there a budget to share?
make utilization-share-shadow         # report what it would move; moves nothing
make utilization-share-status         # the mode, decisions and what it would move
```

Leave shadow on through each model's peak and read what it would move. Then:

```bash
make utilization-share-activate       # let it act
make utilization-share-status         # transfers should end "done"
```

Leave out `WVA_LIMITER` and the sharing steps to scale each model on its own,
with no budget. `<accelerator>` is the GPU product name your nodes carry (for
example `H200` or `NVIDIA-H200`); `make utilization-share-check` fails if it
does not match. `make utilization-share-off` turns sharing off again and keeps
the quota.

Before you start:

- [ ] KEDA is on the cluster (on Kubernetes, `KEDA_HELM_INSTALL=true` installs
  it; on OpenShift, use the platform's KEDA).
- [ ] The router (EPP) runs with `featureGates: [flowControl]` in its
  EndpointPickerConfig; `make check-prereqs` refuses the install when it sees
  the gate off.
- [ ] Prometheus is on the cluster (it is installed if there is none).
- [ ] For sharing: two or more models, and model servers that drain on
  termination (`preStop` and `terminationGracePeriodSeconds`).

A cluster admin runs `setup-prereqs` once per namespace; the namespace owner
runs the rest, and every later upgrade. If you are both, `make deploy-wva` does
the two in one command. An upgrade that adds a prerequisite needs the admin's
step again first ([upgrading](docs/reference/upgrade-and-uninstall.md#upgrade)).
Re-running `make deploy-wva` turns sharing off; repeat the check, shadow and
activate steps after an upgrade.

The `scaledobjects` steps are not optional: a **KEDA ScaledObject** is how a
workload registers. Until one exists, the controller runs and scales nothing.

**On a shared cluster**, have a cluster admin run the controller out of the
tenant's reach: `WVA_NS=wva-<ns> WVA_WATCH_NS=<ns>` on `setup-prereqs` and
`deploy-wva` ([cluster-admin setup](docs/guides/admin-cluster-setup/README.md#keeping-the-controller-out-of-the-tenants-reach)).
The tenant then registers with
`NAMESPACE=<ns> make scaledobjects-plan WVA_NS=wva-<ns> WVA_WATCH_NS=<ns>`, and
the same for `scaledobjects-apply`, so every trigger names the controller in
`wva-<ns>`. The quota and sharing are then the admin's too: the admin runs
the sharing steps with `CONTROLLER_NAMESPACE=wva-<ns>` and `RESTART=1`
([when the controller is out of your reach](docs/well-lit-paths/utilization-share/README.md#when-the-controller-is-out-of-your-reach)).
Either way, the bound that holds is a GPU
ResourceQuota on the namespace
([bound the namespace's GPUs at admission](docs/guides/admin-cluster-setup/README.md#bound-the-namespaces-gpus-at-admission)).

| Then | |
| --- | --- |
| Share a quota, step by step | [utilization-share guide](docs/guides/utilization-share/) |
| Models in several namespaces, one budget | [cluster-wide](docs/guides/utilization-share-cluster-wide/) or [namespace-scoped](docs/guides/utilization-share-namespaced/) |
| Pick another scenario; each links its guide | [well-lit paths](docs/well-lit-paths/) |
| Full install guide | [deploy/](deploy/) |
| Run it day to day | [operations.md](docs/reference/operations.md) |
| Watch what it decides | [monitoring.md](docs/reference/monitoring.md) |
| Make a workload scalable | [workload-preparation.md](docs/reference/workload-preparation.md) |
| Bridge a slow scale-up | [warm-pool/](docs/guides/warm-pool/) — opt-in; a pool holds accelerators |

## How it works

![How it fits together](docs/concepts/figures/how-it-fits.svg)

1. It learns which workloads it manages **from KEDA's calls**: it does not list
   or watch workloads. Being called is being managed, and the trigger
   `metadata` is the per-workload configuration.
2. It reads request rates and server performance from Prometheus.
3. Its capacity model (KV-cache use, queue depth, token throughput) decides the
   replica count each model needs, across all its variants and within any GPU
   budget you declared. With sharing on, it then splits that budget between
   the models and plans any transfer.
4. It returns that count to KEDA over the external-scaler gRPC contract. KEDA
   owns the HPA; llm-scaling-manager never writes the scale subresource.

More: [how the scaling manager fits together](docs/concepts/architecture.md),
[concepts](docs/concepts/), and the [docs](docs/) index.

## Example

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: llama-8b-scaler
  namespace: llm-inference
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: llama-8b
  pollingInterval: 5
  minReplicaCount: 1        # 0 to allow scale to zero
  maxReplicaCount: 10
  triggers:
  - type: external-push     # push: wakes a parked workload immediately
    name: wva-external-scaler
    metadata:
      scalerAddress: wva-external-scaler.<controller-namespace>.svc:9090  # your NAMESPACE on a namespace install
      modelID: meta/llama-3-8b   # required — the only field you must supply
      scalingPolicy: interactive # optional — a tier you add to the scaling policy; none ship enabled
      variantCost: "10.0"        # optional — defaults to "10.0"
    authenticationRef:      # TLS, the default. Plaintext install: ...svc.cluster.local:9090 and no authenticationRef
      name: wva-external-scaler-<controller-namespace>
      kind: ClusterTriggerAuthentication
```

`modelID` is the one required field. The accelerator, the role, GPUs per
replica and the InferencePool are **derived** from the workload, so they cannot
drift from reality.

You do not have to write this by hand: `make scaledobjects-plan` reads
`modelID` from each container's `--served-model-name`. A `modelID` that does not
match what the container serves groups the workload with the wrong model and
mis-scales both. Every trigger key, its default and who reads it:
[the trigger reference](docs/reference/scaledobject-trigger.md). More examples in
[config/samples/keda/](config/samples/keda/).

A model's share weight is set on its scaling-policy entry (`weightClass:
important`), not on the trigger
([for model owners](docs/reference/scaling-policy.md#for-model-owners-what-the-optimizer-may-take-and-how-to-protect-a-model)).

## Beyond per-model scaling

- the **utilization-share optimizer** (experimental), which shares one GPU
  quota between models and moves GPUs by transfer, and **quotas read from
  Kueue**, alongside the GPU and quota limiters;
- a **KEDA external scaler**: it answers KEDA directly over gRPC instead of
  publishing a metric, and pushes activation from zero instead of waiting for a
  poll;
- a **warm pool** that bridges a slow scale-up by lending a Pod with the model
  already loaded and asleep;
- **ScaledObject discovery and install tooling**: `make scaledobjects-plan`
  writes an editable plan before anything is applied;
- a **capacity model** that prices demand from the throughput one replica
  sustains and holds through a request-shape change, with **learners** that
  measure each fleet instead of relying on fixed defaults.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Questions, bugs and ideas:
[GitHub issues](https://github.com/ev-shindin/llm-scaling-manager/issues).

## License

Apache 2.0 - see [LICENSE](LICENSE) for details.

## Origin

Started in 2026 from the code of llm-d's workload-variant-autoscaler; since
redesigned. Nearly 90% of the production Go code is new since August 2026,
and nothing has been taken from that project since. Not affiliated with or
endorsed by llm-d.

## Related Projects

- [llm-d-autoscaling](https://github.com/llm-d/llm-d-autoscaling) - llm-d's KEDA autoscaling blueprints
- [llm-d infrastructure](https://github.com/llm-d/llm-d-infra)
- [llm-d main repository](https://github.com/llm-d/llm-d)
