# Bridge a scale-up with a warm pool

A scale-up is slow because a new replica must load a model before it serves. A
warm pool holds a few Pods with models already loaded and asleep, and lends one
to a variant that is scaling up. The borrowed Pod joins the model's
InferencePool, serves while the ordinary replica loads, and is handed back the
moment that replica is ready.

The pool is **insurance, not capacity**. Size it by how often you spike and how
long a replica takes to start, never by peak load. How long a start takes, and
what preparing the workload takes off it before you reach for a pool, is in
[Replica start time](../../reference/start-time.md).

**It is not free.** Every pool Pod holds its accelerators all the time, lending
or idle, and they count against your namespace's quota like any workload. A pool
of N Pods lowers your maximum fleet by N. That is a cost decision, so the scaling
manager never creates a pool for you.

Two things to know before you size one:

- **A pool Pod must match the model's shape in three ways.** The fit check
  declines per variant and says which one failed:

  | must match | declined with | set by |
  | --- | --- | --- |
  | GPUs per Pod — the Pod needs at least as many as the engine | `needs N GPUs, this Pod holds M` | `create --gpus` |
  | how those GPUs are **divided** — 16 over two Pods and over four are the same count and different engines | `spans N Pod(s), this warm unit is M` | `create --group-size` |
  | the accelerator itself — a warm copy is only reusable on the one it was loaded on | accelerator mismatch | `create --accelerator` (a nodeSelector) |

  A Pod cannot change node or device count, so a model that fails any of these
  is *never* warmed by that pool; it is not waiting. Nothing refuses a mixed
  pool, and it fails quietly: the reserve is counted pool-wide, so part of it
  cannot serve the model you hold it for.
- A pool whose replica count does not **exceed** its reserve never warms
  anything. See [Sizing](sizing.md#sizing).

## Where the rest of this guide is

| | |
| --- | --- |
| [install.md](install.md) | Installing and creating a pool |
| [sizing.md](sizing.md) | Sizing and tuning a pool |
| [multi-pool.md](multi-pool.md) | Several pools, and what each holds |
| [groups.md](groups.md) | Pools of groups: engines that span machines |
| [operating.md](operating.md) | Operating a pool: scraping, checks, troubleshooting |

## Should this model be warmed here at all?

First: can this cluster warm this model, and would it gain anything? Four
thresholds decide it, and all move with the hardware, so the command reads the
nodes:

```bash
# Read this cluster's nodes and answer the four questions for this model
deploy/warmpool.sh sizing --params 744B --dtype fp8
```

For a cluster you cannot reach (choosing hardware rather than using it),
describe a node instead:

```bash
# Answer the same questions for a described node
deploy/warmpool.sh sizing --params 744B --dtype fp8   --gpus-per-node 8 --gpu-mem-gib 141 --ram-gib 2016
```

| Question | Why it decides |
|---|---|
| Does the model fit one node? | If not, its engine spans Pods, and it needs a pool of **groups** rather than of Pods — see [pools of groups](groups.md#when-the-engine-spans-machines-pools-of-groups). |
| Can host RAM hold a level-1 sleeper at all? | Below this, warming is impossible here. |
| Can it hold **more than one**? | The whole question. A pool holding one model is an idle replica: same accelerators, no requests answered. |
| Is cold start dominated by reading weights, or by fixed startup? | Decides whether faster storage helps, or whether nothing does. |

Two results tend to surprise: **host RAM, not GPU memory, rules warming out**,
and a model split across MORE nodes is often more poolable, because the
per-node sleeper burden falls while total RAM rises.

## Which pools do you need?

A pool serves exactly one **(accelerator, GPUs-per-replica)** shape. Neither can
change at run time: a warm copy is reusable only on the GPU it was loaded on, and
a model needing more devices than a Pod holds cannot start in one. A pool absorbs
every other difference between models (size, traffic, policy).

```bash
# Group the namespace's models by shape and print a create line per group
deploy/warmpool.sh plan -n <namespace>
```

It groups the namespace's model ScaledObjects by that pair, says which could
share one pool, and prints a `create` line for each group. It also names models
that select a pool nobody declared, which is worse than selecting none because
it looks configured.
