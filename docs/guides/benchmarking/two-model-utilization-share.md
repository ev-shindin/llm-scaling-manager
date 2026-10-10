# Two models, anti-phase bursts, under one quota

[← Benchmarking guide](README.md)

> **Run once.** It ran on H200 nodes on 2026-10-08 with `PHASE_SECONDS=1200`,
> `CYCLES=1` and a 4-GPU quota. What it measured, and how far that goes:
> [measured](../../well-lit-paths/utilization-share/measured.md). One run is not
> a result you can repeat yet: the `today` and `shadow` arms disagreed by up to
> 9 s p95 on identical decisions.

The [utilization-share optimizer](../../well-lit-paths/utilization-share/) is
built for several models under one quota whose peaks do not coincide. This
scenario measures exactly that case. It uses the two models and the anti-phase
load of [the warm-pool run](two-model-warm-pool.md), with no pool, and puts both
models under **one quota smaller than their combined peak**. Model B rises
while model A falls, and then the other way round. Under today's optimizer the
falling model keeps its GPUs through its scale-down window, and the quota
refuses the rising one. Under the utilization-share optimizer, GPUs are moved
from the falling model to the rising one, scale-down first.

The run is made once per **arm**, with the same traffic each time:

| arm | the policy's `optimizer:` block | the controller's mode |
| --- | --- | --- |
| `today` | none: today's optimizer | `off` |
| `shadow` | `type: utilizationShare`, `shadow: true` | `shadow`: it computes and reports, and moves nothing |
| `share` | `type: utilizationShare`, `shadow: false` | `active`: it moves GPUs |

The arms differ **only** in that block. The quota is set once, at standup, and
never per arm. `run` rewrites the block and nothing else in the policy. That is
what makes the comparison about the optimizer and not about the budget.

`shadow` is the control. It runs the optimizer's evaluation every cycle and
moves nothing, so its TTFT should match `today`'s. If `shadow` differs from
`today` by about as much as `share` does, the run-to-run spread is larger than
the effect, and the run shows nothing. It also records how long a move was
planned, which shows what the optimizer would have acted on.

## The quota

The quota is the experiment, so it has two bounds. With the defaults below
(`MAX_REPLICAS=3`, `MIN_REPLICAS=1`, `GPUS_PER_REPLICA=1`), it must be **4 or 5**
GPUs.

**Below the combined peak:** quota < `2 × MAX_REPLICAS × GPUS_PER_REPLICA` (6).
At or above that, both models can reach their ceiling at the same time. The
quota never refuses anything, there is nothing to share, and the three arms do
not test what this path is for.

**At least one model's peak plus the other's floor:** quota ≥
`(MAX_REPLICAS + MIN_REPLICAS) × GPUS_PER_REPLICA` (4). Below that, even a
perfect transfer cannot give the rising model its peak while the falling one
keeps its floor. The quota is short whatever moves (`quota-short`), and the run
measures how a shortage is cut by weight, not whether GPUs arrive in time.

The tightest value, 4, is the sharpest test. At B's peak, A must be down to its
floor, so every replica B adds beyond the free quota has to come from A.

The driver does **not** check these bounds. `preflight` still asks for the full
`2 × MAX_REPLICAS × GPUS_PER_REPLICA` placeable accelerators, because it does
not read the quota. Choose the number yourself.

## What it needs

Everything [the warm-pool run needs](two-model-warm-pool.md#what-it-needs),
except the pool. In addition:

- **The quota at standup**, in the install's own policy:
  `WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=<n>'`. Leave `WVA_QUOTA_SCOPE`
  at its default, `namespace`. The benchmark installs the scaling manager
  namespace-scoped beside the models, so the entry is keyed on that namespace
  and both models fall in one namespace quota group. `<accelerator>` must be
  the name the controller resolves for the variants: a quota on a name nobody
  resolves to is a quota of zero, and both models freeze at their floor.
- **A controller in the benchmark namespace.** The share arms refuse a
  controller that runs anywhere else (`WVA_NS` other than
  `BENCHMARK_NAMESPACE`): a cluster-scoped controller, or one that serves other
  namespaces, applies its policy to every tenant it manages, and an arm would
  switch the optimizer on, or sweep every live transfer mark, for all of them.
  The standup installs a namespace-scoped controller beside the models, which
  passes. `SHARE_ALLOW_SHARED_POLICY=1` is the deliberate way past, for a policy
  that really is yours alone.
- **The policy in force is the benchmark namespace's own.** A namespace-scoped
  controller that manages its own namespace reads the namespace named by a
  `wva.llmd.ai/policy-namespace` label on it first, then `wva-policy` when that
  exists
  ([where policy lives](../../reference/gpu-limiter.md#what-the-controller-does-with-that)),
  and only then its own ConfigMap. The arms rewrite only the benchmark
  namespace's own policy, so `run` refuses a namespace with the label and a
  cluster with a `wva-policy` namespace: that policy is shared, and an arm run
  against a copy the controller does not read measures a quota nobody wrote
  down. It also refuses when it cannot read the namespaces to tell;
  `SKIP_POLICY_NAMESPACE_CHECK=1` is the deliberate way past.
- **Prometheus reachable from inside the cluster**, at the controller's own
  `PROMETHEUS_BASE_URL`. `run` reads the mode from it before the load starts,
  and reads what the optimizer did after the arm ends.

## Running it

```bash
export BENCHMARK_NAMESPACE=<your namespace>
```

**1. Preflight, stand up with the quota, and verify.** These are the warm-pool
run's steps 1 to 3
([Running it](two-model-warm-pool.md#running-it)), with the quota added to the
standup:

```bash
make benchmark-two-model-preflight
WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=4' make benchmark-two-model-standup
make benchmark-two-model-verify
```

Do **not** create a pool. Every share arm refuses to run while one is present.

**2. First arm: today's optimizer.**

```bash
make benchmark-two-model-reset
make benchmark-two-model-run ARM=today
```

Before it starts the load, each share arm does three things:

1. It refuses a policy that declares no quota limiter.
2. It rewrites the policy's `optimizer:` block for its arm.
3. It waits up to `SHARE_MODE_TIMEOUT` (300 s) for Prometheus to show the
   controller in the arm's mode.

A policy nobody read would measure the previous arm under a new name. For
`today`, the mode is `off`.

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
about three minutes with default timings. The arm writes its policy before the
load Job exists, and the load starts only after `PRELOAD_GRACE` (420 s by
default), so the freeze should end before the first request. Do not shorten
`PRELOAD_GRACE` below it.

Each arm takes about 45 minutes at the defaults, as in the warm-pool run. Each
keeps the policy it ran under (`policy.yaml`) beside its results. An arm whose
loader never wrote its results is still refused, but it keeps the evidence
before the loader Pod is deleted: what the Pod's `/results` held, in
`harness-partial/`, the Pod itself as `loader-pod.json`, and loader b's process
list as `loader-b-ps.txt`. After the arm
ends, it also writes what the controller recorded over the arm's window
(`share.json`):

- transfers, by outcome;
- model-seconds held back, by blocked reason;
- the seconds during which a move was planned;
- the lowest value the mode gauge took.

**5. Report.**

```bash
make benchmark-two-model-report-share
```

It needs the `today` arm and at least one of the other two. It writes
`report.md`, `report.json` and the SVG plots into the results directory
(`two-model-results/` by default):

- **The TTFT and accelerator-second tables** of the warm-pool report, with
  `today` as the baseline in place of `nopool`. Accelerator-seconds matter
  here: the utilization-share optimizer spends spare quota as headroom by
  design, so an arm can hold more GPUs at low load than `today` does.
- **What the optimizer did**, per arm: mode, transfers by outcome, the seconds a
  move was planned, and model-seconds held back by reason.

**6. Tear down.** The same as the warm-pool run:

```bash
make benchmark-two-model-teardown
make benchmark-two-model-status
```

## What the report refuses

It refuses, rather than tabulates, three runs that would mislead. It also
applies every refusal of the warm-pool report (different schedule or seed, a
different ceiling, unissued arrivals, the driver queueing, one engine doing all
the work); see
[What makes the arms comparable](two-model-warm-pool.md#what-makes-the-arms-comparable).

| refused | why |
| --- | --- |
| Arms whose policies differ apart from the `optimizer:` block, or an arm that kept no `policy.yaml` | The comparison is two optimizers under **one** quota. A different quota is a different experiment. |
| An arm whose controller did not hold the arm's mode for the whole window, or has no `share.json` | A mode that lapsed mid-arm means part of the arm measured something else. |
| A `today` or `shadow` arm that recorded a transfer | Neither may move a GPU. One that did was not the arm it is labelled. |

## Two different burst shapes

By default both models burst with the same request shape. To give them
different ones, for example one model's bursts prompt-heavy and the other's
output-heavy, stand up with the `two-model-shapes` scenario and give each loader
its own shape:

```bash
BENCH_SPEC=guides/two-model-shapes make benchmark-two-model-preflight
BENCH_SPEC=guides/two-model-shapes \
  WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=4' make benchmark-two-model-standup
# then every arm with the same shapes, for example:
INPUT_TOKENS=20000 OUTPUT_TOKENS=500 INPUT_TOKENS_B=1000 OUTPUT_TOKENS_B=6000 \
  make benchmark-two-model-run ARM=today
```

The scenario (`hack/benchmark/scenarios/guides/two-model-shapes.yaml`) is
`two-model-warm-pool.yaml` with one change: `maxModelLen` 32768 on both stacks,
in place of 8192, so a 20 000-token prompt fits. `make lint-deploy-scripts`
checks that it stays a copy in every other line. Set `BENCH_SPEC` for the
preflight and the standup, which read the scenario; the shapes are read by each
`run`, and every arm of one comparison must use the same ones.

## The load shape, and the case it does not reach

The schedule is the warm-pool run's
([The phases](two-model-warm-pool.md#the-phases)): 8-minute phases, a 90 s band
between bursts, and two cycles. A full swing, from model A's burst to its next
one, is `2 × (PHASE_SECONDS + OVERLAP_SECONDS)`, which is 19 minutes at the
defaults.

That is short for this optimizer. With default settings a transfer takes effect
about twelve minutes after it is decided. In the proposal's simulator, moving
GPUs beats standing still only when the load swings back slower than about
eight of those latencies, roughly 100 minutes
([§6.7](../../proposals/utilization-share-optimizer.md#67-why-it-does-not-oscillate-and-what-it-cannot-follow)).
Those are design figures, not measurements. Two consequences of the default
shape follow from the documented rules:

- A model that gave GPUs during the other's burst may still be inside its
  **reversal hold** when its own burst begins. The hold is about twice a
  release time from the start of the transfer it gave in, about 12 minutes
  with the scenario's 300 s scale-down window. If that happens, the report's
  held-back table shows it as `reversal-hold`. This has been measured: in the
  different-burst-shapes runs it kept the second model to burst on 3 of its 8
  replicas for its whole burst
  ([measured](../../well-lit-paths/utilization-share/measured.md#rerun-with-the-cancel-fix-the-reversal-hold-did-the-same)).
  The hold is now lifted for a hard imbalance -- a receiver at or near its
  scale-up threshold and a donor well below its own after giving
  (`immediateRebalance`) -- which no run
  has measured yet. A model whose long-output burst just ended is usually not
  that lightly loaded, so the hold can still bite.
- A role that reverses direction twice within the swing window is planned on
  its mean need, and shows `swinging`.

Both are the rules working as documented. To measure the slow-swing case the
optimizer is built for, raise `PHASE_SECONDS` until a full swing is well past
100 minutes. An arm then runs for hours; `CYCLES=1` halves that, at the cost of
one swap per arm instead of two. Every arm of one comparison must use the same
shape, or the report refuses them.

## What it does not measure

Everything [the warm-pool run does not measure](two-model-warm-pool.md#what-it-does-not-measure),
and, in addition:

- **Weights.** Both models run at the default weight. How a weight class
  divides the headroom is not exercised.
- **Node-aware placement, whole-node pods and P/D.** One accelerator per
  replica and aggregated models: no donor sets, no LeaderWorkerSets, no
  8-GPU replicas. A P/D variant of the different-burst-shapes scenario,
  `hack/benchmark/scenarios/guides/two-model-shapes-pd.yaml` (both models
  disaggregated, four roles under one quota; how to drive it is in its
  header), exists and has not been run. Its prefill and decode pods take one
  accelerator each, so it says nothing about whole-node pods either.
- **A cluster quota or a canary.** One namespace quota group.

## One P/D model (single-stack mode)

The same three arms can drive **one** P/D-disaggregated model, whose prefill
and decode share the quota. `PD_SINGLE_STACK=1` switches the driver to that
mode:

- Stand the model up with
  `make benchmark-standup BENCHMARK_SPEC=guides/pd-disaggregation MODEL_ID=<model>`,
  not with `standup`. That scenario has one stack and reaches its router
  through a Service, so set `PD_ENDPOINT=http://<router Service clusterIP>:80`.
- `MODEL_A` and `MODEL_B` both name the model. The two loaders take turns:
  loader a uses `INPUT_TOKENS`/`OUTPUT_TOKENS`, loader b uses
  `INPUT_TOKENS_B`/`OUTPUT_TOKENS_B`. Give one a prompt-heavy shape and the other
  a decode-heavy one.
- `MIN_PREFILL`/`MAX_PREFILL` and `MIN_DECODE`/`MAX_DECODE` bound each role. They
  default to the shared bounds. Reset, the ceiling and `budget.json` all apply
  them per role, and the report refuses arms whose bounds differ.

**It has not produced a result.** On `Qwen/Qwen3-0.6B`, prefill stayed at one
replica through 20 minutes of 15 000-token prompts at 12 rps, so there was
nothing for the arms to compare ([why](../../well-lit-paths/utilization-share/measured.md#pd-not-measured)).
Two tooling problems are also open:

- The standup rendered the scenario's 32 CPU / 128 GiB engine requests despite
  logging the 2 CPU / 8 GiB override for Qwen3-0.6B. Check the Deployments, and
  set the requests by hand if needed.
- A decode-heavy loader with 4000-token outputs wrote no results within
  `RESULT_GRACE` (1200 s) after its load ended. The arm now keeps what the
  loader left (`harness-partial/`, `loader-pod.json`, `loader-b-ps.txt`) for
  the next occurrence; the cause is not known.
