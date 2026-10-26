# Sharing a GPU budget between models

## The use case

Several models share a fixed GPU budget: a quota, or a fixed fleet. Load moves
between them during the day. When one model's load drops and another's rises,
the GPUs the first one holds should go to the second, without dropping requests
in flight and, while the budget covers every model, without taking the first
model below what its own load needs.

llm-scaling-manager does this with the experimental
[utilization-share optimizer](../well-lit-paths/utilization-share/):

- **It picks the donor.** Each model is first given the GPUs its demand
  needs, and the rest is shared by weight. GPUs are taken from the model best
  off against its demand and weight, and only down to its share of the budget;
  never below its floor or its last replica. When the budget cannot cover
  every model's need, GPUs go to the model worse off by weight, and a donor
  can go below its need, down to its floor.
- **It plans each move.** The donor is lowered first. The receiver is raised
  only once those GPUs are actually free. A move is called off while it is
  still waiting if the load swings back, and a model that just gave GPUs
  cannot take them back for a while (the reversal hold).
- **It drains through scale-down.** The chosen donor pod is marked to be
  removed first and goes through an ordinary scale-down, so in-flight requests
  are kept as well as the model server drains them.
- **It knows the nodes.** On a cluster-scoped install it opens a hole that fits
  every pod of the receiver's replica, node by node, so a multi-pod replica
  (for example a LeaderWorkerSet group) is funded whole. A P/D model is planned
  as one model, and is held whole when either role cannot be sized. The node
  fit and multi-pod replicas are tested on kind only, not yet on real
  hardware.
- **It shares what is left.** What a warm pool wants and does not hold is
  taken out of the shared budget. The budget can be a declared quota, the
  quota Kueue holds (see [With Kueue](#with-kueue) for when that works), or,
  on a cluster-scoped install, the physical GPUs.

It costs headroom held ahead of need (36% to 43% more GPU-seconds in the P/D
runs). A move takes minutes: the donor's scale-down window and drain, then
the receiver's own start (image, weights, engine ready), the same wait Kueue
sharing has when a quieter model's HPA gives GPUs up. Kueue preemption skips
the window; nothing here does. When the imbalance is large (the busier model at 0.9 or more of
its scale-up threshold), the move skips every extra wait of its own; a smaller
imbalance first waits two confirmation cycles. In the latest runs the model
rising second was not held back by the first model's headroom. The evidence
is one or two runs per configuration, on 1-GPU pods of 8B models. See
[what it costs](../well-lit-paths/utilization-share/README.md#what-it-costs)
and the [evidence](../well-lit-paths/utilization-share/README.md#evidence).

## What llm-d offers today

As of 2026-10-26, nothing in llm-d moves GPUs from a quieter model to a
busier one according to their load. Each model's HPA scales on
its own and does not see the others' load. Sharing a budget is left to Kueue,
which acts on the pods each HPA requests:

- **With the guide's floors (its default),** a model borrows GPUs the others
  are not using. Borrowed GPUs come back when the borrower's HPA scales down,
  or when Kueue preempts a borrowed pod for a model returning to its floor. A
  lightly loaded model keeps what it holds until its own HPA scales it down,
  and a busy model at or above its floor cannot take GPUs that a quieter
  model's HPA still wants, even when the busy model needs them more. The guide's rule is *"you may preempt to reach your
  own floor, never to go beyond it. Above your floor you can only take what is
  idle."*
- **With Kueue fair sharing,** which the guide's Tuning section offers instead
  of floors, a model whose HPA asks for more pods can preempt pods of another
  model. The donor is the model furthest over its weighted share, whatever its
  own load.

In detail:

- **Each Deployment scales on its own.** llm-d scales with KEDA on the
  router's (EPP) metrics, one ScaledObject per Deployment. Its strategies are
  queue (stable), saturation (experimental), token-aware (experimental) and
  SLO-aware (experimental / scope-limited)
  ([autoscaling workflow](https://github.com/llm-d/llm-d-autoscaling/blob/e036371f7d04317920ef03a596fbe0c64745942a/docs/developer-guide/autoscaling-workflow.md)).
- **Sharing a GPU budget is left to Kueue.** The
  [Kueue rebalancing guide](https://github.com/llm-d/llm-d/blob/d5e53624ca354290b48323a3786b4a9bba836e0b/guides/workload-autoscaling/kueue-rebalancing/README.md),
  which llm-d marks experimental, gives each model a ClusterQueue whose
  `nominalQuota` is its guaranteed floor, in a shared cohort. A pod waits at a
  scheduling gate until quota is free. *"The HPA is never touched"*: each
  model's HPA still scales on its own. Its
  [ClusterQueues](https://github.com/llm-d/llm-d/blob/d5e53624ca354290b48323a3786b4a9bba836e0b/guides/workload-autoscaling/kueue-rebalancing/optimized-baseline/base/clusterqueues.yaml)
  set only `reclaimWithinCohort: Any` and
  `withinClusterQueue: LowerOrNewerEqualPriority`, so `borrowWithinCohort`
  stays at `Never`.
- **Fair sharing.** The guide's
  [Tuning](https://github.com/llm-d/llm-d/blob/d5e53624ca354290b48323a3786b4a9bba836e0b/guides/workload-autoscaling/kueue-rebalancing/README.md#tuning)
  section describes it: `nominalQuota: 0` and a `fairSharing.weight` on each
  ClusterQueue, *"Fully elastic, with no guaranteed floor."* Kueue then
  *"prefers to preempt Workloads from ClusterQueues that have the highest
  share value first"*
  ([Kueue preemption](https://kueue.sigs.k8s.io/docs/concepts/preemption/)).

## Kueue rebalancing and utilization share

| | Kueue rebalancing (llm-d) | Utilization share (llm-scaling-manager) |
| --- | --- | --- |
| Idle GPUs used | Yes: borrowed when a model's HPA asks for more pods. | Yes: spread ahead of need as running headroom, by weight; it is not given back while every model's peak fits. |
| GPUs moved from a quieter model to a busier one when the budget is full | No with the guide's floors. With fair sharing, from the model furthest over its weighted share, not by load. | Yes, by each model's demand against its share. |
| Who decides a model shrinks | Its own HPA, on its own load. Kueue preempts a borrowed pod when a lender reclaims its floor (or, with fair sharing, by share). | The optimizer picks the donor and how many GPUs it gives. |
| How GPUs move | The HPA deletes pods, or Kueue preempts one; a waiting pod is then admitted. | A planned move: the donor gives first, and the receiver grows once those GPUs are free. The donor gives through an ordinary scale-down, choosing which pod goes first; it drains as any deleted pod does. |
| Placement | GPUs counted against each ClusterQueue's quota, as the guide configures it; node placement is left to the scheduler. | Node-aware on a cluster-scoped install: the receiver's replica must fit in the holes the donors open, node by node, and multi-pod replicas are funded whole. Tested on kind only. A namespace-scoped install counts GPUs only. |
| Signal | The pods each HPA requests. | Each model's demand against its share, with confirmation cycles, a tolerance band and a reversal hold. |
| Time until the busier model serves more | The busier model waits for GPUs to come free: with the guide's floors, until the quieter model's own HPA scales down (its scale-down window), then for its new pod to start (image pull, weights load, vLLM engine ready), which is minutes for a large model. Preemption skips the scale-down window: with floors only for a model returning to its floor; with fair sharing for any model under its weighted share. A preempted pod drains for its `terminationGracePeriodSeconds`, as a scale-down does. | The donor's scale-down window and the new pod's start, as with the guide's floors; there is no preemption shortcut. When the imbalance is large (on by default: the busier model at 0.9 or more of its scale-up threshold, the donor at 0.6 or less after giving), nothing else is added: the move skips the confirmation cycles and the reversal hold, and from the next cycle the busier model takes every replica it is short at once. A smaller imbalance first waits two confirmation cycles, and a model that just gave GPUs waits out the reversal hold (about twice a release time, 12 minutes with a 300 s scale-down window). Headroom held ahead of need serves the model that rises first at once; in one run, median TTFT 0.07 s for the first burst and 87 s for the second, against 41 s and 144 s without sharing. |
| Cost and maturity | Experimental. Holds no headroom: idle GPUs stay unused until a model's HPA asks for them. | Experimental. Headroom held ahead of need (36% to 43% more GPU-seconds in the P/D runs). A group freezes for about three minutes when it starts acting, after every restart or upgrade. One or two runs per configuration, 1-GPU pods of 8B models; whole-node, multi-GPU pods and a cluster-wide quota not measured. |

## With Kueue

The optimizer can take its budget from Kueue. To share GPUs between the models
on a set of ClusterQueues, read Kueue at cluster scope: a quota entry with
`scope: cluster` and `kueue.enabled`. The budget is then the sum of every
ClusterQueue's `nominalQuota`, one budget for every namespace, and Kueue still
admits each pod. This also works for the guide's own layout, both models in
one namespace on two ClusterQueues: 10 GPUs shared by two models.

The guide's flavor has no product label, so the entry names the accelerator
type and `-1` lets Kueue give the number
([merge rules](../reference/quota-limiter.md#merge-rules)). Name your nodes'
GPU product in `quotas`, and make sure the models' type resolves to it: the
guide's model servers set no GPU nodeSelector, so the type comes from the
nodes' `nvidia.com/gpu.product` label (or set a GPU nodeSelector). A model whose type stays unknown is
left out of sharing. The entry goes in the scaling policy's `default` entry,
beside `optimizer:`
([scaling policy](../reference/scaling-policy.md#optimizer-cluster-default-only-live)):

```yaml
limiters:
  - name: cluster-quota
    type: quota
    scope: cluster
    kueue: { enabled: true }
    quotas:
      H100: -1        # Kueue's ClusterQueues give the number
```

The [cluster-wide utilization-share guide](../guides/utilization-share-cluster-wide/)
sets this up (its option C), including the product label check. Its preflight warns about a namespace with
LocalQueues on several ClusterQueues; that rule is for namespace-scope
entries, and a cluster-scope budget is the sum either way.

What does not work, or is not shown yet:

- **A namespace-scope entry** gives each namespace its own budget, so with one
  model per namespace there is nothing to share.
- **The guide's fair-sharing layout** sets every `nominalQuota` to 0 and keeps
  the budget on a Cohort object, which the scaling manager does not read, so
  it sees a budget of zero.
- **Queues that also run other GPU work** (training, batch) need a
  `gpu-inventory` limiter beside the entry
  ([Kueue-bounded quotas](../well-lit-paths/kueue-bounded-quotas/README.md#when-not-to-take-it)).
- **Borrowing.** The budget is the nominal quota. Moves between two queues
  rely on Kueue lending a donor's idle floor through the cohort, so
  `lendingLimit: 0` (offered in the guide's Tuning section) stops them. The
  optimizer itself starts no borrowing beyond the budget.
- **Measurement.** A cluster-wide quota has not been measured yet.
