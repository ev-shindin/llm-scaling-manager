# Two models, anti-phase bursts, under one quota

[← Benchmarking guide](README.md)

> **Run once.** It ran on H200 nodes on 2026-10-08 with `PHASE_SECONDS=1200`,
> `CYCLES=1` and a 4-GPU quota. What it measured, and how far that goes:
> [measured](../../well-lit-paths/utilization-share/measured.md). One run is not
> yet a repeatable result: the `today` and `shadow` arms disagreed by up to 9 s
> p95 on identical decisions.

The [utilization-share optimizer](../../well-lit-paths/utilization-share/) is
built for several models under one quota whose peaks do not coincide. This
scenario measures that case. It uses the two models and anti-phase load of [the
warm-pool run](two-model-warm-pool.md), with no pool, under **one quota smaller
than their combined peak**. Model B rises while A falls, then the other way
round. Today's optimizer keeps the falling model's GPUs through its scale-down
window, and the quota refuses the rising one. The utilization-share optimizer
moves GPUs from the falling model to the rising one, scale-down first.

The run is made once per **arm**, with the same traffic each time:

| arm | the policy's `optimizer:` block | the controller's mode |
| --- | --- | --- |
| `today` | none: today's optimizer | `off` |
| `shadow` | `type: utilizationShare`, `shadow: true` | `shadow`: it computes and reports, and moves nothing |
| `share` | `type: utilizationShare`, `shadow: false` | `active`: it moves GPUs |

The arms differ **only** in that block. The quota is set once, at standup;
`run` rewrites the block and nothing else. So the comparison is about the
optimizer, not the budget.

`shadow` is the control: it evaluates every cycle and moves nothing, so its TTFT
should match `today`'s. If `shadow` differs from `today` about as much as
`share` does, run-to-run spread is larger than the effect and the run shows
nothing. It also records how long a move was planned.

## The quota

The quota is the experiment, so it has two bounds. With the defaults
(`MAX_REPLICAS=3`, `MIN_REPLICAS=1`, `GPUS_PER_REPLICA=1`) it must be **4 or 5**
GPUs:

- **Below the combined peak:** quota < `2 × MAX_REPLICAS × GPUS_PER_REPLICA`
  (6). At or above that, both models reach their ceiling together, nothing is
  refused, and the arms test nothing.
- **At least one model's peak plus the other's floor:** quota ≥
  `(MAX_REPLICAS + MIN_REPLICAS) × GPUS_PER_REPLICA` (4). Below that, even a
  perfect transfer cannot give the rising model its peak while the falling one
  keeps its floor (`quota-short`), and the run measures how a shortage is cut by
  weight, not whether GPUs arrive in time.

The tightest value, 4, is the sharpest test: at B's peak, A must be at its
floor, so every replica B adds beyond the free quota comes from A.

The driver does **not** check these bounds, and `preflight` still asks for the
full `2 × MAX_REPLICAS × GPUS_PER_REPLICA` placeable accelerators because it does
not read the quota. Choose the number yourself.

## What it needs

- [ ] Everything [the warm-pool run needs](two-model-warm-pool.md#what-it-needs),
  except the pool.
- [ ] **The quota at standup**, in the install's own policy:
  `WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=<n>'`. Leave `WVA_QUOTA_SCOPE`
  at `namespace` (the default): the benchmark installs the scaling manager
  namespace-scoped beside the models, so both fall in one namespace quota group.
  `<accelerator>` must be the name the controller resolves for the variants; a
  quota on any other name is a quota of zero, and both models freeze at their
  floor.
- [ ] **A controller in the benchmark namespace.** The share arms refuse
  `WVA_NS` other than `BENCHMARK_NAMESPACE`: a controller serving other
  namespaces would switch the optimizer on, or sweep transfer marks, for all of
  them. The standup's namespace-scoped controller passes.
  `SHARE_ALLOW_SHARED_POLICY=1` is the deliberate way past, for a policy that is
  really yours alone.
- [ ] **The benchmark namespace's own policy is the one in force.** `run`
  refuses a namespace with a `wva.llmd.ai/policy-namespace` label, a cluster with
  a `wva-policy` namespace, and a run that cannot read the namespaces to tell
  ([why](#details)). `SKIP_POLICY_NAMESPACE_CHECK=1` is the deliberate way past.
- [ ] **Prometheus reachable in the cluster** at the controller's
  `PROMETHEUS_BASE_URL`. `run` reads the mode from it before the load, and what
  the optimizer did after the arm.
- [ ] **This machine stays awake for the whole run**: the GPU sampler runs here
  ([keeping it awake](#keeping-the-sampling-machine-awake)).

## Running it

```bash
export BENCHMARK_NAMESPACE=<your namespace>
```

**1. Preflight, stand up with the quota, and verify.** The warm-pool run's steps
1 to 3 ([Running it](two-model-warm-pool.md#running-it)), with the quota added:

```bash
WVA_QUOTAS='<accelerator>=4' make benchmark-two-model-preflight   # places for the quota, not the ceilings
WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=4' make benchmark-two-model-standup
make benchmark-two-model-verify
```

Do **not** create a pool: every share arm refuses to run while one exists.

The standup applies these to its own copy of the scenario, so nothing under
`hack/` is edited by hand. Set them on the `preflight` line too, and re-run
`preflight` after changing one:

| setting | default | what it does |
| --- | --- | --- |
| `GATEWAY_REUSE` | `auto` | `auto` reuses the Istio the cluster already runs (an `istio.io/gateway-controller` GatewayClass exists) instead of installing `istio-base` over it, which fails part-way through. `true` and `false` force it. |
| `GATEWAY_PROVIDER_NAMESPACE` | `istio-system` | where that control plane runs |
| `DECODE_CPU`, `DECODE_MEMORY` | the scenario's (8, `64Gi`) | a decode Pod's request and limit. Lower them when `preflight` reports fewer placeable Pods than the run's peak because the nodes with free accelerators lack spare CPU. |
| `BENCHMARK_RENDER_DRAIN` | `true` | the drain hook and grace go into the engines before they first start, so the drain patch after the standup finds nothing to change and no model starts twice |
| `BENCHMARK_ENGINE_CACHE_HOSTPATH` | empty | a directory on the nodes for the engines' compile caches. Opt-in, so earlier runs stay comparable. |

**2. First arm: today's optimizer.**

```bash
make benchmark-two-model-reset
make benchmark-two-model-run ARM=today
```

Before the load, each arm refuses a policy with no quota limiter, rewrites the
`optimizer:` block for its arm, and waits up to `SHARE_MODE_TIMEOUT` (300 s) for
Prometheus to show the controller in the arm's mode (`off` for `today`). A policy
nobody read would measure the previous arm under a new name.

**3. Second arm: shadow.**

```bash
make benchmark-two-model-reset
make benchmark-two-model-run ARM=shadow
```

**4. Third arm: share.**

```bash
make benchmark-two-model-reset
make benchmark-two-model-run ARM=share
```

When the optimizer starts acting, the group holds still for one fill timeout,
about three minutes by default. The load starts only after `PRELOAD_GRACE`
(420 s by default), so the freeze ends before the first request. Do not shorten
`PRELOAD_GRACE` below it.

Each arm takes about 45 minutes at the defaults and keeps the policy it ran
under (`policy.yaml`) beside its results. After the arm it writes what the
controller recorded over its window (`share.json`): transfers by outcome,
model-seconds held back by blocked reason, the seconds a move was planned, and
the lowest value of the mode gauge. An arm whose loader never wrote results is
refused, but keeps the evidence first: the Pod's `/results` in
`harness-partial/`, the Pod as `loader-pod.json`, and loader b's process list as
`loader-b-ps.txt`.

**5. Report.**

```bash
make benchmark-two-model-report-share
```

It needs `today` and at least one other arm, and writes `report.md`,
`report.json` and SVG plots into the results directory (`two-model-results/` by
default):

- **The TTFT and accelerator-second tables** of the warm-pool report, with
  `today` as the baseline in place of `nopool`. Accelerator-seconds matter here:
  the optimizer spends spare quota as headroom by design, so an arm can hold more
  GPUs at low load than `today`.
- **What the optimizer did**, per arm: mode, transfers by outcome, the seconds a
  move was planned, and model-seconds held back by reason.

**6. Tear down.** The same as the warm-pool run:

```bash
make benchmark-two-model-teardown
make benchmark-two-model-status
```

## What the report refuses

Three runs that would mislead are refused, not tabulated. Every refusal of the
warm-pool report also applies (different schedule or seed, a different ceiling,
unissued arrivals, the driver queueing, one engine doing all the work); see
[What makes the arms comparable](two-model-warm-pool.md#what-makes-the-arms-comparable).

| refused | why |
| --- | --- |
| Arms whose policies differ apart from the `optimizer:` block, or an arm that kept no `policy.yaml` | The comparison is two optimizers under **one** quota. A different quota is a different experiment. |
| An arm whose controller did not hold the arm's mode for the whole window, or has no `share.json` | A mode that lapsed mid-arm means part of the arm measured something else. |
| A `today` or `shadow` arm that recorded a transfer | Neither may move a GPU. One that did was not the arm it is labelled. |

## Two different burst shapes

By default both models burst with the same request shape. To give them different
ones (say, one prompt-heavy and one output-heavy), stand up with the
`two-model-shapes` scenario and give each loader its own shape:

```bash
BENCH_SPEC=guides/two-model-shapes make benchmark-two-model-preflight
BENCH_SPEC=guides/two-model-shapes \
  WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=4' make benchmark-two-model-standup
# then every arm with the same shapes, for example:
INPUT_TOKENS=20000 OUTPUT_TOKENS=500 INPUT_TOKENS_B=1000 OUTPUT_TOKENS_B=6000 \
  make benchmark-two-model-run ARM=today
```

The scenario (`hack/benchmark/scenarios/guides/two-model-shapes.yaml`) is
`two-model-warm-pool.yaml` with `maxModelLen` 32768 on both stacks instead of
8192, so a 20 000-token prompt fits; `make lint-deploy-scripts` checks it stays
a copy otherwise. `BENCH_SPEC` is read by preflight and standup; the shapes are
read by each `run`, and every arm of one comparison must use the same ones.

## Three or four models

> **Not run on GPUs yet.** The driver, the schedule and every report are
> checked by `make lint-deploy-scripts`; no cluster run has measured them.

Two models in anti-phase are the smallest case of the claim. With more models,
each rise has more falling models to take GPUs from. `MODEL_COUNT=3` or `4` runs
the same arms with three or four models under one quota:

- **Bursts rotate.** In each cycle every model bursts once, one at a time, in
  the order B, C, D, A. Each burst starts one phase plus one band
  (`PHASE_SECONDS + OVERLAP_SECONDS`) after the one before. That is the period
  divided by the number of models. For two models it is the anti-phase run
  above, unchanged.
- **The scenario is `n-model-share`.** It has four stacks, each one GPU per
  replica: the two models above, `Qwen/Qwen3-30B-A3B-FP8` (model C) and
  `Qwen/Qwen3-32B` (model D). The standup keeps only the first `MODEL_COUNT`
  of them. Model D needs a GPU larger than 80 GB; on smaller GPUs run three.
- **Per-model settings** follow the A/B names: `STACK_C`, `MODEL_C`,
  `HIGH_RPS_C`, `MAX_REPLICAS_C`, `GPUS_PER_REPLICA_C`, and the same with `_D`.

The quota bounds above become, for N models:

- **below the sum of the peaks:** quota < `N × MAX_REPLICAS × GPUS_PER_REPLICA`;
- **at least one peak plus every other model's floor:** quota ≥
  `(MAX_REPLICAS + (N - 1) × MIN_REPLICAS) × GPUS_PER_REPLICA`.

With four models at the defaults that is 6 to 11 GPUs. Six is the sharpest
test: a rising model gets its peak only from GPUs the others give back.

```bash
export MODEL_COUNT=4
WVA_QUOTAS='<accelerator>=6' make benchmark-two-model-preflight
WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=6' make benchmark-two-model-standup
make benchmark-two-model-verify
SHARE_ARMS='today share' REPEATS=1 TEARDOWN_ON_EXIT=1 make benchmark-two-model-share-arms
```

One arm runs `LEAD_IN + N × CYCLES × PHASE_SECONDS + (N × CYCLES - 1) ×
OVERLAP_SECONDS` seconds of load: 2310 s for four models and one cycle at the
defaults. The load Pod runs one loader container per model, so it requests
`N × LOAD_CPU` CPU and `N × LOAD_MEMORY` on one node.

The reports cover every model: TTFT per model and per rise, held-back
model-seconds per model, transfers by model (which model gave, which received),
and GPU-seconds per model beside the fleet total. Each arm records which
Deployments belong to which model (`model_deploys` in `budget.json`), so the
split comes from the sampled Pods. An arm whose results lack a model the
schedule drove is refused, not reported as fewer models.

## Mixed replica sizes: a 2-GPU model beside a 1-GPU model

> **Not run on GPUs yet.** The flow has run end to end on kind with simulated
> engines (below). No real-GPU result exists.

This is the real-GPU stand-in for one large model beside small ones. Model A is
`Qwen/Qwen3-30B-A3B-FP8` at tensor parallel 2, so each replica takes **2 GPUs**.
Model B is `Qwen/Qwen3-8B` at 1 GPU. The scenario is
`hack/benchmark/scenarios/guides/two-model-mixed-sizes.yaml`.

What is new against the other runs:

- **Sizes and ceilings per model.** The driver reads each stack's GPUs per
  replica from the scenario. `MAX_REPLICAS_A` and `MAX_REPLICAS_B` set each
  model's ceiling. `preflight` and `standup` refuse a scenario whose GPU request
  is not tensor × dataLocal, a stack name the scenario does not have, and a
  `GPUS_PER_REPLICA_A`/`_B` that disagrees with the scenario. `verify` refuses
  a deployed replica that holds a different number of GPUs.
- **Node data.** The optimizer funds a 2-GPU replica from two 1-GPU replicas
  only when it can see they share a node. So the policy needs a `gpu-inventory`
  limiter beside the quota (`WVA_LIMITER=gpu-inventory,quota`), and the
  controller needs node read and a pod list in every namespace. Both are cluster
  admin grants. `make benchmark-two-model-node-data` checks them and prints the
  exact commands. Every share arm refuses to start without them; `NODE_DATA=off`
  is the deliberate way past.
- **One node.** `BENCH_NODE=<node>` pins every model replica to that node.
  `preflight` checks that node alone has the GPUs the run peaks at.
- **Several nodes**, when no single node has the peak free
  ([below](#on-several-nodes)).
- **Repeats in one command.** `make benchmark-two-model-share-arms` runs
  `REPEATS` (2) rounds of today, shadow and share, each after a reset, a report
  per repeat, then `report-repeats`. Even repeats run the arms in reverse order.
  `TEARDOWN_ON_EXIT=1` tears down on every exit path, then prints what still
  holds a GPU.
- **The repeats report** (`repeats.md`): TTFT p50 and p95 per burst by model and
  arm, the spread across repeats and between today and shadow, refused
  scale-ups, transfers by outcome counted from each arm's controller log,
  GPU-seconds, and a short "what this shows".

```bash
export BENCHMARK_NAMESPACE=<your namespace>
export BENCH_SPEC=guides/two-model-mixed-sizes
export STACK_A=qwen3-30b-a3b MODEL_A=Qwen/Qwen3-30B-A3B-FP8 MAX_REPLICAS_A=3
export STACK_B=qwen3-8b MODEL_B=Qwen/Qwen3-8B MAX_REPLICAS_B=4
export WVA_LIMITER=gpu-inventory,quota WVA_QUOTAS='<accelerator>=7'
export BENCH_NODE=<a node with at least 7 free GPUs>
make benchmark-two-model-preflight
make benchmark-two-model-standup
make benchmark-two-model-node-data     # an admin runs what it prints, once
make benchmark-two-model-verify
TEARDOWN_ON_EXIT=1 make benchmark-two-model-share-arms
```

Results land in `rep1/`, `rep2/` under the results directory, with
`repeats.md` beside them.

### On several nodes

On a shared cluster, no node may have 7 free GPUs while several have 3 to 5.
Name them in place of `BENCH_NODE`:

```bash
export BENCH_NODES=<node>,<node>,<node>
make benchmark-two-model-preflight
```

- **Where replicas land.** On any listed node, by node affinity. The list
  replaces the GPU product selector, so name nodes of one product.
- **What `preflight` checks.** The listed nodes' free GPUs must add up to the
  peak (the quota, 7). Each node must hold at least one replica of the larger
  model: 2 free GPUs, plus a decode Pod's CPU and memory, on that one node.
  Together they need the CPU and memory for every Pod the peak can hold.
- **The controller's GPU count.** The standup sets the controller's
  gpu-inventory to count only the listed nodes (`WVA_NODE_SELECTOR`), node by
  node. The quota still caps the total at 7. Each share arm refuses a
  controller that counts other nodes.
- **Where they landed.** Each arm writes `placement.json`. The report gives
  each node's GPUs and each model's replicas. Read it with the result. A 2-GPU
  replica needs 2 free GPUs on one node, so one free GPU on each of two nodes
  funds nothing, and `share` can fund less than on one node. The report refuses
  to compare arms run on different node lists.
- **Taints.** Nothing is tolerated by default. `preflight` refuses a listed
  node with a `NoSchedule` or `NoExecute` taint. If the run may use that node,
  name the taint: `BENCH_TOLERATE=<key>=<value>:<effect>` (a comma list; the
  value and effect are optional). It applies only with `BENCH_NODES`.

### A dry run on kind

`BENCH_SIM=1` replaces the harness standup with simulator stacks
(`llm-d-inference-sim`) that hold emulated GPUs. Everything after it is the real
path: the controller install, ScaledObject registration, node data, reset, the
arms, the loader, sampling, collection and every report. It refuses any cluster
that is not kind. It says nothing about latency: the simulator's TTFT is a
setting.

```bash
# A one-node kind cluster with 8 emulated H200s
bash deploy/kind-emulator/setup.sh -c bench-dryrun -n 1 -g 8 -t nvidia -d NVIDIA-H200-141GB
export BENCH_SIM=1 SIM_GPU_PRODUCT=NVIDIA-H200-141GB
export WVA_LIMITER=gpu-inventory,quota WVA_QUOTAS='H200=7' KEDA_HELM_INSTALL=true
export PHASE_SECONDS=90 RISE_WINDOW=30 OVERLAP_SECONDS=20 LEAD_IN=30 CYCLES=1 PRELOAD_GRACE=200
export LOAD_CPU=250m LOAD_MEMORY=1Gi LOAD_MEMORY_LIMIT=4Gi
make benchmark-two-model-standup
bash hack/benchmark/two_model_pool.sh node-data grant
make benchmark-two-model-verify
TEARDOWN_ON_EXIT=1 make benchmark-two-model-share-arms
```

Set the model and stack variables as in the block above. Give the controller an
image the kind node can load (`IMG=` and `WVA_IMAGE_PULL_POLICY=IfNotPresent`).

## The load shape, and the case it does not reach

The schedule is the warm-pool run's ([The phases](two-model-warm-pool.md#the-phases)):
8-minute phases, a 90 s band between bursts, two cycles. A full swing, from A's
burst to its next, is `2 × (PHASE_SECONDS + OVERLAP_SECONDS)`: 19 minutes at the
defaults.

That is short for this optimizer. By default a transfer takes effect about
twelve minutes after it is decided. In the proposal's simulator, moving GPUs
beats standing still only when the load swings back slower than about eight of
those latencies, roughly 100 minutes
([§6.7](../../proposals/utilization-share-optimizer.md#67-why-it-does-not-oscillate-and-what-it-cannot-follow)).
Those are design figures, not measurements. Two consequences, both the rules
working as documented:

- A model that gave GPUs during the other's burst may still be in its **reversal
  hold** when its own burst begins: about twice a release time from the start of
  the transfer, about 12 minutes with the scenario's 300 s scale-down window. The
  report's held-back table shows it as `reversal-hold`. Measured: in the
  different-burst-shapes runs it kept the second model to burst on 3 of its 8
  replicas for its whole burst
  ([measured](../../well-lit-paths/utilization-share/measured.md#rerun-with-the-cancel-fix-the-reversal-hold-did-the-same)).
  The hold is now lifted for a hard imbalance (a receiver at or near its
  scale-up threshold and a donor well below its own after giving,
  `stabilization.skipWaitsWhen`), which no run has measured yet. A model whose
  long-output burst just ended is usually not that lightly loaded, so the hold
  can still bite.
- A role that reverses direction three times (`stabilization.swingReversals`) within the swing window is planned on its
  mean need, and shows `swinging`.

To measure the slow-swing case the optimizer is built for, raise `PHASE_SECONDS`
until a full swing is well past 100 minutes. An arm then runs for hours;
`CYCLES=1` halves that, at the cost of one swap per arm instead of two. Every arm
of one comparison must use the same shape, or the report refuses them.

## Load of your own: vary the prompts

The arms' loader sends varied prompts, and the report refuses a run where one
engine did nearly all the work. A load of your own should vary its prompts too.
With one identical prompt, the EPP's prefix-cache routing sends every request to
the replica that already holds it, so added replicas serve nothing and TTFT
measures the queue at one engine.

A cluster walk of this guide saw exactly that: streamed TTFT held at about 91 s
and 83 s while each model went from 1 to 3 replicas. The first chunk was not
held on the gateway path: the rest of every response took about 10 s, in all
8,144 requests. Fleet throughput stayed at about 3.2 requests a second whatever
the concurrency and replica count, which is one replica's 32 sequences. That
cause is inferred from throughput, not yet confirmed per pod; the engines'
prompt-token counters, which every arm records, would show it.

## When the standup stalls on one node

An engine Pod stuck in `Init` or `ContainerCreating` while its siblings start is
usually a node fault. Look at its events:

```bash
# Sandbox creation failures in the benchmark namespace
kubectl get events -n $BENCHMARK_NAMESPACE --field-selector reason=FailedCreatePodSandBox
```

`no space left on device` means the node cannot create the Pod's sandbox. That
is for the cluster admin: cordon or taint the node. Without node rights, keep
this run's engines off it. This changes only the run's decode and prefill
Deployments, and the next standup renders them without it:

```bash
# Keep this run's decode and prefill Deployments off one node
BAD=<node name>
for d in $(kubectl get deploy -n $BENCHMARK_NAMESPACE -o name | grep -E 'decode|prefill'); do
  kubectl patch -n $BENCHMARK_NAMESPACE $d --type=merge -p "{\"spec\":{\"template\":{\"spec\":{\"affinity\":{\"nodeAffinity\":{\"requiredDuringSchedulingIgnoredDuringExecution\":{\"nodeSelectorTerms\":[{\"matchExpressions\":[{\"key\":\"kubernetes.io/hostname\",\"operator\":\"NotIn\",\"values\":[\"$BAD\"]}]}]}}}}}}}"
done
```

Each patch rolls that Deployment once. Re-run `preflight` afterwards: the node's
accelerators still count as free, so the placeable count is one node too high
until the node is fixed.

## What it does not measure

Everything [the warm-pool run does not measure](two-model-warm-pool.md#what-it-does-not-measure),
and also:

- **Weights.** Both models run at the default weight; how a weight class divides
  the headroom is not exercised.
- **Node-aware placement, whole-node pods and P/D.** One accelerator per replica
  and aggregated models: no donor sets, no LeaderWorkerSets, no 8-GPU replicas.
  The [mixed-size scenario](#mixed-replica-sizes-a-2-gpu-model-beside-a-1-gpu-model)
  adds donor sets and 2-GPU replicas, and has not run on GPUs yet.
  A P/D variant of the different-burst-shapes scenario,
  `hack/benchmark/scenarios/guides/two-model-shapes-pd.yaml` (both models
  disaggregated, four roles under one quota; how to drive it is in its header),
  has run once
  ([results](../../well-lit-paths/utilization-share/measured.md#pd-two-disaggregated-models-under-one-quota)).
  Its prefill and decode pods take one accelerator each, so it says nothing about
  whole-node pods either.
- **A cluster quota or a canary.** One namespace quota group.

## One P/D model (single-stack mode)

The same three arms can drive **one** P/D-disaggregated model, whose prefill and
decode share the quota. `PD_SINGLE_STACK=1` switches the driver to that mode:

- Stand the model up with
  `make benchmark-standup BENCHMARK_SPEC=guides/pd-disaggregation MODEL_ID=<model>`,
  not with `standup`. That scenario has one stack and reaches its router through
  a Service, so set `PD_ENDPOINT=http://<router Service clusterIP>:80`.
- `MODEL_A` and `MODEL_B` both name the model. The loaders take turns: loader a
  uses `INPUT_TOKENS`/`OUTPUT_TOKENS`, loader b `INPUT_TOKENS_B`/`OUTPUT_TOKENS_B`.
  Give one a prompt-heavy shape and the other a decode-heavy one.
- `MIN_PREFILL`/`MAX_PREFILL` and `MIN_DECODE`/`MAX_DECODE` bound each role,
  defaulting to the shared bounds. Reset, the ceiling and `budget.json` apply
  them per role, and the report refuses arms whose bounds differ.

**This mode has not produced a result.** On `Qwen/Qwen3-0.6B`, prefill stayed at
one replica through 20 minutes of 15 000-token prompts at 12 rps, so the arms had
nothing to compare ([why](../../well-lit-paths/utilization-share/measured.md#an-earlier-attempt-on-one-model)).
The two-model P/D scenario above did produce one. One tooling problem is open
(the standup rendering the scenario's 32 CPU / 128 GiB engine requests over the
2 CPU / 8 GiB override is fixed: the copy the standup deploys is now rendered
too):

- A decode-heavy loader with 4000-token outputs wrote no results within
  `RESULT_GRACE` (1200 s) after its load ended. The arm now keeps what the loader
  left (`harness-partial/`, `loader-pod.json`, `loader-b-ps.txt`) for the next
  occurrence; the cause is not known.

## Details

### Why the policy namespace is checked

A namespace-scoped controller that manages its own namespace reads, in order, the
namespace named by a `wva.llmd.ai/policy-namespace` label on it, then
`wva-policy` when that exists
([where policy lives](../../reference/gpu-limiter.md#what-the-controller-does-with-that)),
and only then its own ConfigMap. The arms rewrite only the benchmark namespace's
own policy. Against a shared policy the controller does not read, an arm would
measure a quota nobody wrote down.

### Keeping the sampling machine awake

The load runs in the cluster, but the GPU sampler runs on the machine that runs
these commands, and the arm's GPU-seconds come from it.

- Each arm holds a sleep lock where the machine offers one: `systemd-inhibit` on
  Linux, `caffeinate` on macOS. Neither reaches a Windows host from WSL. There,
  keep it on mains power and, for the run, set sleep to never and the lid to do
  nothing: `powercfg /change standby-timeout-ac 0`, then
  `powercfg /setacvalueindex SCHEME_CURRENT SUB_BUTTONS LIDACTION 0` and
  `powercfg /setactive SCHEME_CURRENT`. Put both back afterwards.
- **If it slept anyway:** `run` stops the arm with "the GPU sampler did not run
  for Ns", and `gpus.jsonl` has a line with `stalled_since`. The arm's cost
  cannot be priced, so it stops at once and gives its GPUs back; rerun it. The
  report refuses such an arm too, and says its latencies stand.
- Each sampler API call is bounded by `GPU_SAMPLE_TIMEOUT_SECONDS` (default
  `20`, whole seconds). A call that hangs past it counts as a failed poll, not a
  stall.

Running the sampler in the cluster, beside the load, would make the cost
independent of this machine; it is not built.

### Engine caches on the nodes

The scenarios' engines keep their compile caches inside the container, so every
start compiles from nothing: on an 8B model on H200, torch.compile took 14.4 s
cold against 2.9 s from a cache, and starts took 75-81 s against 51-56 s.
`BENCHMARK_ENGINE_CACHE_HOSTPATH=<node directory>` gives every engine role a
cache on that node directory, prepared on every accelerator node before the
engines start
([Engine caches on the node's disk](../../reference/workload-preparation.md#engine-caches-on-the-nodes-disk)).
Each node then compiles once. It needs leave to create PersistentVolumes. It
changes start times, so compare a run with it only against runs with it.
