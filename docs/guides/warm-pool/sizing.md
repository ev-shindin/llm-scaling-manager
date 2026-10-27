# Sizing and tuning a pool

[← Warm pool guide](README.md)

## Sizing

Two numbers sit in the pool's ScaledObject, and they must agree.
`warmPoolSleepMinSize` is the **reserve**: Pods kept free for the next spike.
The pool warms models only into what is left over, so:

> **maxReplicaCount must be greater than warmPoolSleepMinSize.**

At equality the pool warms nothing, ever, while holding every accelerator it
has. Nothing looks like an error, because the reserve is doing what it was told.
The scaling manager reports it, so you do not find out from the bill.

The **ceiling** is what matters, not the current size. A pool momentarily at its
reserve grows on the next pass; a pool whose ceiling *is* its reserve never can.
This is easiest to get wrong on a pinned pool (`min == max`).

Start with `maxReplicaCount: 6`, `warmPoolSleepMinSize: "1"`, and raise the
ceiling if you see borrows blocked.

The start time a pool bridges, and what a pool costs against preparing the
workload or holding a floor, is in
[Replica start time](../../reference/start-time.md).

### How many models fit in one Pod

A third number lives in the pod template, not the trigger:

```yaml
resources:
  limits:   { memory: 128Gi }
  requests: { memory: 128Gi }
```

**This is the warm-set budget.** Measured on an H100, a resident model costs
about **2.6 GiB + 1.4× its weights**:

| model | weights | charged while asleep | fit in 128Gi |
| --- | --- | --- | --- |
| 0.6B | 1.1 GiB | 4.1 GiB | ~30 (capped at 16) |
| 8B | 14.9 GiB | 23.4 GiB | ~5 |

The hard ceiling is 16 instances per Pod, but above about 3B **memory runs out
first**.

- **Getting it wrong is expensive.** One model too many does not fail its own
  admission: it OOM-kills the launcher and takes **every model already resident
  in that Pod** with it. That is why the scaling manager never admits against a
  budget larger than the limit.
- **Changing it rolls the pool.** It is a pod-template field, so every Pod
  restarts and every resident model loads again. Size it at deploy time; the
  trigger keys are the ones that are free to retune.

Watch `memory.current`, not `anon`: a sleeper's anonymous memory barely moves,
so `anon` reports it as costing nothing.

## Tuning

Everything else is trigger metadata on the pool's ScaledObject. Edits apply on
the next reconcile and do **not** restart Pods, so retuning a live pool is free.

| key | does |
| --- | --- |
| `warmPoolSleepMinSize` | Pods held free for the next spike (`--reserve` at create) |
| `warmPoolRetained` | whether the pool keeps a lent Pod rather than reclaiming it (`--type bridge`/`--type retained` at create) |
| `warmPoolMaxHold` | how long a borrowed Pod may serve before it is returned regardless (`--max-hold` at create; meaningless when retained, and refused together with it) |
| `warmPoolPreloadTop` | warm this many of the busiest variants without waiting for a miss |
| `warmPoolGPUMemoryUtilization` | how much of the card a warm copy claims |

- A value that cannot be read refuses the **whole** pool, and the scaling
  manager names the pool and the key. These values decide how many accelerators
  it holds, so applying only some would show you numbers that are in force
  nowhere.
- `max-hold` covers an ordinary replica that never arrives, so a failing
  scale-up cannot turn your reserve into permanent capacity for one variant.
- `gpu-memory-utilization` trades KV cache for warm-set size. A workload's own
  value usually claims nearly the whole card, leaving no room for sleepers beside
  an awake engine, so the pool's default is lower.

You do **not** set here how many accelerators a Pod holds or how much memory it
may use. Both are read from the pool Pod's own spec, so they cannot disagree with
it. The memory limit is still a load-bearing decision: see [How many models fit
in one Pod](#how-many-models-fit-in-one-pod).

## Letting the pool resize itself

The pool scales like every other workload here: **the scaling manager computes a
size, KEDA writes it.** `config/warmpool` ships a ScaledObject beside the
Deployment, and it is not optional: the trigger **declares** the pool. To pin
the size, see [Keeping the pool a fixed size](#keeping-the-pool-a-fixed-size).

```yaml
spec:
  minReplicaCount: 2      # must EXCEED the pool's reserve
  maxReplicaCount: 6
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:   { stabilizationWindowSeconds: 60 }
        scaleDown: { stabilizationWindowSeconds: 900 }
  triggers:
    - type: external-push
      metadata:
        scalerAddress: wva-external-scaler.<wva-namespace>.svc:9090
        warmPoolName: default   # must match the Deployment's llm-d.ai/warm-pool
      authenticationRef:      # TLS, the default. Plaintext install: ...svc.cluster.local:9090 and no authenticationRef
        name: wva-external-scaler-<wva-namespace>
        kind: ClusterTriggerAuthentication
```

The scaling manager publishes `lent + reserve + 1`: the Pods now bridging, the
free reserve, and one spare that makes admission possible at all.

- **`minReplicaCount` must exceed the reserve**, or the pool spends every quiet
  period unable to warm anything.
- **Grow fast, shrink slowly** is the `behavior` block above. Too small costs
  latency on every spike; too large costs money; growing back costs a full model
  load.

**Under a quota the pool asks only for what the namespace can pay for.** Its
GPUs count against the same allowance as model replicas, and the published size
is capped at what it holds plus what the allowance leaves. The log then says
`warm pool is not growing: the namespace has no GPU allowance left for it`, with
the figures. Without a limiter nothing bounds the pool and it grows straight to
`lent + reserve + 1`. See [Details](#details) for the edge cases.

### Keeping the pool a fixed size

Set them equal:

```yaml
minReplicaCount: 3
maxReplicaCount: 3
```

KEDA creates the HPA with min and max both 3, and the pool stays there whatever
the scaling manager publishes.

- This is the **only** way to pin a pool. Deleting the ScaledObject gives you no
  pool at all: the Deployment keeps its accelerators and the scaling manager
  reports it as undeclared.
- With `min == max`, `maxReplicaCount` must still exceed `warmPoolSleepMinSize`.
- A pinned pool never relieves its own blocked borrows: `outcome="blocked"` keeps
  appearing until you raise the numbers. A fixed GPU budget is easier to reason
  about, and that is a fair trade.
- Never scale the pool to zero. The first spike after a quiet period then pays a
  full cold start, and the pool is ready just after the spike is over.

## Details

- **Why memory is the budget.** A level-1 sleeper moves its weights into shared
  memory, and the container's cgroup is charged for all of it. What a Pod can
  hold is decided by its memory limit and nothing else.
- **Why the tuning is on the trigger, not the Deployment.** A warm pool is a
  scaling-manager concept that happens to have Pods; nothing outside the scaling
  manager reads or creates one. Declaring it through a trigger keeps one rule
  (the scaling manager manages what it is called about) and puts the reserve
  beside the ceiling it must fit inside.
- **Why KEDA resizes it.** The scaling manager's ClusterRole stays read-only. A
  cluster-wide licence to change replica counts is the permission an admin is
  most right to refuse, and the pool does not ask for one.
- **Quota edge cases.** A pool that cannot yet name its accelerator (no readable
  Pod, or Pods on nodes without a product label) holds its size under a bounded
  namespace until it can; without a limiter it grows regardless. A pool already
  above its allowance (a quota lowered later) is not shrunk; it just does not
  grow. After the pool charges a new Pod it holds its size for one optimize
  cycle, until the allowance has counted that Pod (`warm pool is holding its size
  until the namespace allowance has accounted for the Pods it holds`). Without
  that hold, a pool that had just taken its first Pod sized itself against an
  allowance that still credited it, and a one-GPU quota ended up with three Pods.
