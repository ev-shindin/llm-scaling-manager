# Bridge a scale-up with a warm pool

A warm pool keeps a few Pods with models already loaded and asleep. When a
model scales up, one is lent to it and serves while the new replica loads. For
models whose spikes arrive faster than a replica can start.

**Maturity:** Stable.

## When not to take it

- Load is steady. There is nothing to bridge.
- Accelerators are the scarce thing. A pool of N lowers your largest possible
  fleet by N.
- Your models are too large for ordinary replicas to arrive at all. Then the
  pool is the capacity, not a bridge: take [the retained pool path](../retained-pool/).

## Start here

You need [the saturation path](../scale-on-saturation/#start-here) in place,
free accelerators of the same shape as the models, and the shared model cache
the model servers already mount. Every step is in
[Bridge a scale-up with a warm pool](../../guides/warm-pool/); the namespace
owner runs all of them.

1. [Check the model can be warmed here](../../guides/warm-pool/README.md#should-this-model-be-warmed-here-at-all), then [decide which pools you need](../../guides/warm-pool/README.md#which-pools-do-you-need).
2. Meet the [pool's prerequisites](../../guides/warm-pool/install.md#prerequisites), including [the two images](../../guides/warm-pool/install.md#the-images-a-pool-pod-runs) (mirror them first on an air-gapped registry).
3. [Create the pool and point the models at it](../../guides/warm-pool/install.md#adding-and-removing-a-pool-later), or [turn it on at install time](../../guides/warm-pool/install.md#turning-it-on-during-installation).
4. [Check it works](../../guides/warm-pool/operating.md#checking-it-works): `wva_warmpool_borrow_total{outcome="hit"}` rises on a scale-up.

To undo, [remove the pool](../../guides/warm-pool/install.md#adding-and-removing-a-pool-later)
and the `warmPool:` setting from the models.

## What it costs

- N accelerators, all the time, lending or idle. The pool is **insurance, not
  capacity**, so the scaling manager never creates one for you.
- Size it by how often you spike and how long a replica takes to start, never
  by peak load.
- Prepare the workload first: it shortens every start for disk, not
  accelerators. [Replica start time](../../reference/start-time.md) puts both
  side by side.

## Evidence

[What a warm pool buys](measured.md): two models bursting out of phase, with
quiet stretches between. Autoscaling alone vs a one-Pod pool vs a floor of two
held replicas, on H200 nodes, **one benchmark run per arm**, four scale-up
events each. The margins are one run's, not a distribution.

| | Result |
| --- | --- |
| Rise time at p95 | 5.2–8.8 s without the pool, 0.11–0.83 s with it |
| Against the floor | within 50 ms on three of the four rises |
| Cost | 17 % more than autoscaling alone, 12 % less than the floor |

The pool's advantage grows with the number of models sharing it, not with
more quiet. When a model is always bursting, a floor is the cheaper insurance.
[measured.md](measured.md) has the tables, plots and make targets to repeat it.

## Details

### What it is

A scale-up is slow because a new replica must load a model before it serves.
The borrowed Pod joins the model's InferencePool (the pods its router sends to)
and serves until the ordinary
replica is ready, then is handed back at once. Two traffic shapes make it pay:

- **Several models with bursty traffic** that do not all burst at once. One
  pool Pod holding all of them bridges every model's rise, and each model's
  insurance gets cheaper as more share it.
- **Scale-to-zero that must wake fast.** The bridge serves a parked model's
  first request while the ordinary replica loads.

### What it needs

Each item is detailed in the [guide's prerequisites](../../guides/warm-pool/install.md#prerequisites).

- Pool accelerators that match the model in **three** ways: GPUs per Pod, how
  those GPUs are split across Pods, and the accelerator type. Nothing refuses a
  mixed pool and it does not fail loudly: the reserve is counted pool-wide, so
  part of it cannot serve the model you hold it for. A model that fails any of
  the three is never warmed by that pool; fix the configuration.
- A shared model cache (a ReadWriteMany claim), so a warm copy loads from
  local storage rather than a download.
- The right to create Roles in the pool's namespace. `deploy/warmpool.sh create`
  grants the scaling manager `patch` on Pods there (Role `wva-pod-writer`); its
  ClusterRole only reads Pods. Without the grant it refuses to start the pool
  ([prerequisites](../../guides/warm-pool/install.md#prerequisites)).
- Two images. The proxy is ours (`ghcr.io/ev-shindin/llm-scaler/warmpool-proxy`,
  or your `--proxy-image`). A pool of Pods runs upstream Fast Model
  Actuation's launcher (`ghcr.io/llm-d-incubation/llm-d-fast-model-actuation/launcher`,
  not built here). A pool of **groups** runs our fork
  (`ghcr.io/ev-shindin/fma-launcher`), and `create` refuses without `--launcher-image`.
- A pool whose replica count **exceeds** its reserve, or it can never warm
  anything.

The pool is an ordinary Deployment. Its knobs are trigger metadata on the
ScaledObject that declares it, and `deploy/warmpool.sh create` writes them.
With more than one pool in the namespace, each model names its pool with
`warmPool: <pool>` in its trigger; a re-run of `make scaledobjects-plan` and
`make scaledobjects-apply` keeps it.

### Several models in one pool

One pool holding many models is the normal case.
[Several pools and several models](../../guides/warm-pool/multi-pool.md) and
[sizing](../../guides/warm-pool/sizing.md) cover it; the short version:

- **Which models can share.** A pool serves one (accelerator, GPUs per replica)
  shape. Two GPU types on the cluster want two pools. `deploy/warmpool.sh plan` groups the
  models by that pair and names models that select a pool nobody declared
  (worse than selecting none, because it reads as configured).
- **How many fit in one Pod.** The memory limit is the warm-set budget. On an
  H100 a resident model costs about 2.6 GiB + 1.4x its weights, so a 128Gi Pod
  holds about five 8B models. Models under about 3B hit the hard ceiling of 16
  instances before memory runs out. Overcommitting
  OOM-kills the launcher and **every model in that Pod**. Changing the limit
  rolls the pool, so size it at deploy time.
- **Which stay warm.** By default the pool ranks parked models first, then the
  busiest, then anything that missed twice. `warmPoolCopies` on a model
  overrides it: `"0"` never warm, `"1"` always one, `"N"` keep N so N
  scale-ups bridge at once; absent means automatic, at most one copy.

### Verifying it

The guide's [Checking it works](../../guides/warm-pool/operating.md#checking-it-works)
has the commands. A bridge is **not** one of the variant's own serving
replicas, and the engine prices it apart. So a bridged variant showing one
more Pod than `wva_current_replicas` is correct, not a miscount. The
controller log (the admin's, when the controller is out of your reach) has a
`warm pool state` line and a `warm-pool-bridge` line per loan.

### How it is tested

Nine suite files in `test/e2e/`. Eight are labelled `full` and run by
`make test-e2e-full`: `warm_pool_test.go`, `warm_pool_borrow_test.go`,
`warm_pool_return_drain_test.go`, `warm_pool_group_test.go`,
`warm_pool_policy_test.go`, `warm_pool_quota_test.go`,
`warm_pool_attribution_test.go` and `warm_pool_retained_test.go` (simulator
only). They cover the borrow, the hand-back, the drain on return, multi-GPU
groups, vetting an engine before pointing at it, quota refusal, how a bridge
is attributed, and a retained pool keeping its lent Pod. The ninth,
`warm_pool_drain_gpu_test.go` (a returned Pod finishing what it was writing),
is labelled `gpu` only, so neither `make test-e2e-smoke` nor
`make test-e2e-full` runs it.

### Tuning it

The knob this path turns is `--max-hold` at create: how long the pool lends
before reclaiming. Too short and the bridge is pulled before the replicas
arrive; too long and the pool is busy when the next model needs it.
`wva_warmpool_bridge_seconds` should track the ordinary start time. Sitting at
`--max-hold` means the scale-up never completed and the pool is hiding it. The
design, including what was measured and rejected, is in
[proposals/fast-model-loading.md](../../proposals/fast-model-loading.md).
