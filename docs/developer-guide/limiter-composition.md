# How the limiter set is built

Developer notes for the code behind
[Selection & lifecycle](../reference/quota-limiter.md#selection--lifecycle). An
operator needs the reference page, not this one.

## The declared set

`config.Config.EffectiveLimiterTypes()` returns a `config.LimiterTypes`: the kinds
the `limiters:` list on the saturation `default` entry declares,
`LimiterTypeInventory` first, then `LimiterTypeQuota`. Empty means no limiter.
It reads the live ConfigMap under the config lock, so a caller asking more than
one question takes one snapshot and asks it (`Declared`, `HasPhysical`,
`HasQuota`). `String` is the one rendering for logs and the controller status:
the kinds joined by `+`, or `none` for the empty set. `LimiterTypeNone` is only
that spelling, never a member of a set.

`Config.HasPhysicalLimiter()` is the shorthand for callers with that one
question. `Config.EffectiveLimiterSummary()` adds every entry's name
(`inventory+quota/gpu,tenant-quota`); it is what `wva-controller-status` publishes
as `limiter`.

`gpu-inventory` and `inventory` are two spellings of the physical kind
(`isPhysicalLimiterType`). A second physical entry is accepted and ignored;
`ScalingPolicy.IgnoredPhysicalLimiters` / `Config.IgnoredPhysicalLimiters` name
it, and both log sites use `config.IgnoredPhysicalLimitersMessage`.

## The factory

`allocation.NewLimiterFromConfig` (`internal/engines/allocation/limiter_factory.go`)
builds:

- the physical kind: a `DefaultLimiter` named `allocation.PhysicalLimiterName`
  (`gpu-limiter`) around `TypeInventoryWithUsage`;
- each quota entry: a `DefaultLimiter` named after the entry, around one
  `QuotaInventory`;
- more than one: a `CompositeLimiter`, physical first. It has no logic: the
  engines reach the constituents through `Constituents()`.

`allocation.LimiterSignature` is the types plus every quota entry, marshalled
with sorted keys. Both engines compare it every cycle and rebuild on a change.
The physical entry itself is not in it: it takes no fields, and its name only
labels the summary.

## The quota inventory

`QuotaInventory` implements `Inventory` (`limiter_interfaces.go`) and the
`NamespaceAwareInventory` extension; it is tied to one scope. Each cycle
`DefaultLimiter.ComputeConstraints` calls `SetUsed(usedByType)` and, through
feature detection, `SetUsedByNamespace(usedByNS)`; each instance ignores the one
irrelevant to its scope. It then reads `GetResourcePools` (and, at namespace
scope, `NamespaceResourcePools`) into the `ResourceConstraints` the optimizer
allocates within. `UsageBasis()` is `ManagedUsage`: the usage it is fed is the
saturation engine's population sum, never the pod walk.

`GetResourcePools` is one pool per configured type at cluster scope. At
namespace scope it is the per-type sum over the listed namespaces, a summary
only (`ResourceConstraints.NamespaceScoped`): the bounds are the per-namespace
caps in `NamespaceResourcePools`. An unlimited `-1` is a sentinel pool
(`Limit == -1`) at either scope, so `Limit = 0` is always a real deny;
`TotalLimit`, `TotalAvailable` and `Remaining` leave the sentinels out.

`Refresh` is a no-op for a static entry. With `kueue.enabled` it re-reads Kueue
through `internal/kueue.Reader` (at most once per `refreshInterval`) and installs
`static.BoundBy(snapshot)` as the effective entry. It never returns an error: a
failed `ComputeConstraints` skips the provider, so a Kueue outage would lift the
cap; the last snapshot, or the static entry alone, stays in force instead. Type
keys across the static and Kueue maps match by `accelerator.SameName`, and
usage reconciliation finds its pool by `accelerator.FindKey`.

`QuotaLimiterEntries.Validate()` holds the rules the reference lists, with
`MaxQuotaValue` the per-type ceiling; `ScalingPolicy.validateLimiters`
(`config/saturation_scaling.go`) runs it at parse time.

Quotas alone read no nodes: the limiter paths never call
`K8sWithGpuOperator.Discover`, `DiscoverUsage` or `DiscoverNodes`;
`collector.CollectInventoryK8S` (the `WVA_LIMITED_MODE` log) is gated by
`scalingpolicy.ShouldCollectClusterInventory`; and the `gpuusage.Refresher` timer
by `Refresher.Periodic` (wired in `cmd/main.go`), which asks
`allocation.NodeUsageWanted`: a physical limiter, or the utilization-share
optimizer on a cluster-scoped install. The scale-from-zero engine's
`EnsureFresh` is ungated but asks only for a provider on the physical basis.

## Usage bases

A provider declares the usage it is charged with through
`allocation.UsageBasis` (`UsageBasisOf`; physical when it declares none, the
safe direction):

| basis | counts | produced by | consumed by |
|---|---|---|---|
| `PhysicalUsage` | every GPU held on the GPU nodes, attributed by node | `internal/gpuusage` (the pod walk, `gpunodes.usageWalk`) | `TypeInventory` |
| `ManagedUsage` | the managed population, `CurrentReplicas * GPUsPerReplica` by resolved accelerator, plus the warm pools' Pods (`addWarmPoolGPUs`) | the saturation engine (`gpuUsageByType`, published as `decision.LatestManagedGPUUsage`, an explicit zero when the fleet is parked) | `QuotaInventory` |

`GPUUsageViews` carries both; `For(cp)` hands each provider its pair and
`Has(basis)` says whether a basis was observed. A nil view is "not observed",
never "nothing in use": a provider fed a nil map would report its whole
capacity free. `ComputeAvailableConstraints` therefore skips a provider whose
basis is unobserved (below). Each engine builds its views in its own
`gpuUsageViews`; the scale-from-zero one reads only the bases its providers
ask for.

`TypeInventory.SetUsed` reconciles usage keys onto the discovered pool keys
with `accelerator.FindKey`. A key no discovered type owns is dropped and
reported in `wva_unattributed_gpus`, except on a cluster with one accelerator
type (`soleAcceleratorType`), where it can only be that type. The demand side
looks its key up with `accelerator.FindBudgetKey` (Budgets and the family tree,
below).

`TypeInventory`'s limit is node Allocatable per type and still counts a node
that refuses new pods. Node fit leaves such a node out
(`gpunodes.refusesNewPods` sets `decision.NodeGPU.Unschedulable`), so the gap
remains only on a cycle with no node view.

The physical view counts pods by node, so a namespace-scoped controller sees
other namespaces' pods only through an uncached, cluster-wide Pod list
(`gpunodes` `listPods`). Denied, the walk counts its own namespace and sets
`wva_pod_access_denied` while a physical limiter is in force.

## Budgets and the family tree

The budget model is in `budget_merge.go`. `mergeConstraints` intersects the
cluster bounds and `mergeNamespaceConstraints` the namespace allowlists, each
per provider through `allowlistBound` (a listed key; a family listed only
through products bounds their sum; a product falls back to an EXPLICITLY listed
family key). `allowlistBound` and `clusterBound` wrap
`accelerator.AllowlistBound` and `accelerator.ClusterAllowlistBound`
(`internal/accelerator/allowlist.go`), the one reading of an allowlist: the
configuration's exclude warnings (`QuotaLimiterEntries.unboundedExcludeWarnings`
in `internal/config`) read the quota entries through the same two functions,
so the warning and the planner agree on which key bounds which name. The
merged map no longer says which family keys were explicit, so
`accelerator.FindBudgetKey` denies an unlisted product beside a listed sibling,
and a family name never resolves to one of several product keys.

Both merges therefore take the accelerator names the cycle's workloads declare
(`declaredAccelerators`) and materialize each as a key of its own when every
allowlist admits it (`materialized`): an unlisted product under an explicit
family key, and a bare family name at its summed bound. `FitsGPUBudget`
materializes its demand's names, and fits the demand per budget key and per
key that covers several of its names (`accelerator.Covers`), as
`fitPairToBudget` fits a P/D pair.

A family key across several allowlists is read by
`accelerator.ClusterAllowlistsBound`. When no allowlist lists the family
itself, or only some do, its GPUs can only be listed products, so the family
is also bounded by the sum, per product, of the tightest bound on that
product. When every allowlist lists the family itself, the tightest family
figure alone applies. `mergeConstraints` uses it for the cluster bounds, and
`mergeNamespaceConstraints` for a family one holder lists only through
products (`accelerator.ClosesFamily`), there over the holders and the cluster
bounds together. The exclude warnings read the same function.

The bound on a key a namespace allowance lists must not depend on the names
the cycle declares. `mergeWithAllowanceKeys` materializes those keys
(`namespaceKeys`) on top of the declared names. The reads that bound a
namespace's allowance use it: the optimizer's pass, `gpuBudgets` for a
namespace the quotas hold, and `namespaceHeadroom`. The reads of the cluster
alone keep `mergeConstraints`: an open namespace in `gpuBudgets`, the
utilization share, and the warm pool's cluster figure
(`passHeadroom.clusterKeys`). An extra family or product key there would
read as a listed sibling to `accelerator.FindBudgetKey` and deny products the
cluster allows. The namespace budgets of the pass and of `namespaceHeadroom`
also materialize the cluster's products (`clusterProducts`), so the warm pool
reads each product a namespace may use at that product's figure.

## Node fit

`NodeView` (`node_view.go`) is one cycle's free GPUs per node, consumed in
order. `NodeViewFor` builds it from `decision.NodeGPUStore.Read` (no label
copy; it keeps the snapshot's label and taint slices, which `Publish` copied
and nothing writes) when the physical limiter's constraints are among the
cycle's (`PhysicalBounds`), so a skipped physical provider skips node fit with
it; with no snapshot younger than `decision.NodeGPUsMaxAge` it returns a reason
for the caller to log. Nodes are `nodeSlot`s in name order, addressed by index.
A `Clone` shares the slots and the eligibility cache (`eligible`, keyed by
accelerator and tolerations, and `largest` per accelerator) and copies only the
sparse debits; `adopt` commits a clone's debits.

- Placement (`place`, `bestFit`): a replica's pods, largest first, each into
  the smallest hole that holds it (first in name order on a tie). A node is
  eligible (`eligibleNode`) when schedulable, of the accelerator
  (`accelerator.SameName`), and every `decision.NodeGPU.Taints` entry
  (`gpunodes.placementTaints`: NoSchedule/NoExecute taints `refusesNewPods`
  does not already judge) is tolerated by `ReplicaShape.Tolerations`
  (`scaletarget.PodTolerations`, the leader's template). With
  `ReplicaShape.DomainKey` (`scaletarget.ExclusiveTopology`) and more than one
  pod, all pods land in one value of that label (`domainsOf`, tried in order),
  and a value holds one replica (`domainTaken`): one the view placed or held
  a replica in (`NodeView.domains`, per owner, so `without` and
  `ReleaseUncreated` free it with the owner's debits), or one a LeaderWorkerSet
  group's pod is on (`decision.NodeGPU.LWSGroup`, set by `gpunodes.usageWalk`
  from the `leaderworkerset.sigs.k8s.io/group-key` label; LWS's exclusive
  anti-affinity matches any pod carrying it).
  `ReplicaShape.Placements` (the templates' nodeSelector and required node
  affinity) narrows the nodes further (`eligibleFor`) on every path: the
  greedy pass (`nodeFit.replica`), the borrowing placer, a wake (`shapeOf`)
  and the ledger's holes (`steadystate.admitInFlight`). Each pod is judged
  on its own (`podFits`, `podTemplates`): with two entries
  (`variantmeta.podPlacements`: leader, then workers, an unconstrained one as
  a zero `PodPlacement`) pod 0 is judged by the first and the rest by the
  second. A replica placed as one pod (layout unknown) and the rest of a
  partial group (`holdPartial`) cannot be mapped, and are judged by neither
  (`eligibleFit`).
  `VariantReplicaState.Tolerations`/`TopologyKey`/`Placements` and
  `Candidate.Tolerations`/`DomainKey`/`Placements` (`variantmeta.PodPlacements`)
  carry them from the scale target, which discovery already reads.
- Owners: every debit is charged to `NodeView.owner` (`setOwner`): the model
  (`modelKey`) for the greedy and rescale passes, `PromisedOwner` for
  `ReservePromised`, `InFlightOwner` for the ledger. `DebitsExcept` and
  `without` take one owner's placements back.
- Steady state: `selectV2Optimizer` sets `GreedyByScoreOptimizer.Nodes`
  (`ShadowOptimize` clears it: the quota-alone pass has no physical limiter);
  `Optimize` clears it when the pass ends, and drops the previous pass's
  `pass` when it starts. `decideV2` holds the share's promises on it first
  (`ReservePromised`, from `decision.LatestSharePromisedHoles`): each hole on
  the node it is on, and what has no known node, or more than its node shows
  free, off the freest nodes, whole, as `shareFreeNodesAfter` does.
  `fairShareRolePickOn` caps a variant at the replicas `fits` on nodes (0 is
  scarcity), and `fitPairToBudget` runs `fitPairToNodes` after the pool fit:
  the roles are placed a replica at a time, the one furthest behind its share
  first, until each has its k or one does not place; a role with k > 0 that
  placed none fails the iteration. A cut the nodes made is
  `gpuLimitTracker.markNodeFit`, and `applyGPULimitAttribution` sets
  `LimitedBy` to `NodeFitLimiterName` (`node-fit`) for it; the steady-state
  engine's `constrainedMessage` adds `(limited by <LimitedBy>)` to the
  `ResourceConstrained` Event.
- Rescale: `rescaleModelDecisions` fills each role by pool (`fillRole`), then
  `fitRescaleToNodes` places the fill like `fitPairToNodes` does, charges the
  model, and takes back what did not place (refunding the cycle's free GPUs).
  A model filling more than one role grows only if each placed a replica.
- A replica's pods are `VariantReplicaState.PodGPUs` when they sum to the
  replica, else one pod; `replicaPods` returns nil (not judged) for an unknown
  layout larger than every node of the accelerator, logged once per variant in
  the engine's `LayoutLog` (`WithLayoutLog`).
- Scale-from-zero: `decideWake` (`wake_tick.go`) holds `wakeMu` across the check
  and the debit, withholds `tickWakes` (this tick's admitted demand, by
  namespace) from fresh constraints with `WithholdPromised`, and fits sets to
  `tickNodes`, built on first use per tick with the promises held on it;
  `recordWake` debits both and admits the wake to the in-flight ledger.
  `claimShareTransfer` and snapshot-checked wakes have no node view and are
  admitted to nothing (commented at `recordWake`).
- The warm pool: `GreedyByScoreOptimizer.Optimize` publishes headroom once the
  pass is over from `passHeadroom` (the debited budgets and `NodeView.Holes`).
  `RepublishHeadroom(planned, promised)` republishes it `without` the models
  the utilization share planned this cycle (`plannedByOverrides`: their
  targets are the share's) and with the share's promises made since withheld
  from the pools and held on the nodes. The idle-fleet path publishes holes
  through `PublishNamespaceHeadroomOnNodes`. The reconciler caps growth by
  `decision.HeadroomStore.Units`, which is (0, known) for a Pod larger than
  every node.
- The utilization share after the pass: `decideV2` hands the evaluation
  `GreedyByScoreOptimizer.LastPass` (`utilizationShareState.left`).
  `BuildShareGroups` replays the pass's spends (`spendOp`, recorded per model
  in order) for every model it does not plan onto its own merge of the
  constraints (`PassLeft.Replay`), so each group's `Budget` and `ClusterFree`
  are net of them; `shareNodeInputs` subtracts `PassLeft.NodeDebits` (other
  models' placements and the in-flight holds) from the node snapshot
  (`shareFreeNodesAfter`) before the share holds its own promises. A node
  counts by the view's rule (`allocation.NodeAdmits`: `accelerator.SameName`,
  schedulable, taints tolerated by every receiver role's
  `scaletarget.PodTolerations`).
- The share's groups follow the view's domain rule. `ShareNode.Groups` is the
  snapshot's `LWSGroups`: the groups on the node, `namespace/<group hash>`,
  from `usageWalk`. `ShareNode.Claimed` is the label keys for which the
  node's domain already got a group this cycle: from the pass and the
  in-flight holds (`PassLeft.TakenDomains`, net of the models the share
  plans, via `ClaimShareDomains`), or from the share itself. `ShareFitPods`
  (the idle fill and the planner's idle credit) builds a `NodeView` from the
  share's picture (`shareNodeView`: `Groups` as `LWSGroup`, each claim taken
  in `NodeView.domains` under its own key) and places through
  `NodeView.place`. That covers one group per domain, best fit, and
  each pod judged by its own template (`podTemplates`,
  `SharePlanInput.Placements`). A group it places claims its domain. The
  donor-set search (`shareNodeSet`) opens holes the view does not model. It
  judges templates with the same functions (`sharePodNodes`) and skips a
  domain claimed for its key. It also skips a domain where a group stays
  (`freesGroups`): a domain stays allowed only when every group on its nodes
  is a donor group the set takes (`ShareUnit.Group`, named as in `Groups`).
  The planner/fill agreement table checks both walk domains, groups, claims
  and templates the same way.
- The utilization share's cluster budget is the cluster bounds alone
  (`clusterBudgets`, over `mergeConstraints`): a namespace-scoped quota is no
  budget over every namespace. A model of a namespace such a quota excludes is
  planned in no group (`ShareGroupOptions.Unshared`) unless a finite cluster
  bound covers its type. The cluster group's budget and `ClusterFree` are what
  that bound leaves after the namespace groups' claims, in whole replicas; then
  `splitClusterFree` divides each finite cluster key between every group that
  draws on it. Both round to whole replicas of the smallest variant each group
  grows (`growStepGPUs`, over `ShareGroup.Grow`), since a group's fills place
  only those. A group that grows nothing (every role at its ceiling) claims
  nothing in `splitClusterFree`. The namespace's claim also counts the models
  no group plans (frozen, multi-accelerator, or in a namespace disabled for
  sharing), which today's optimizer may still grow: `claimSteps` gives the
  smallest replica of their variants on the type that are below `maxReplicas`
  and whose role has no `RoleHold.NoScaleUp`. The claim's step is the smaller
  of that and `growStepGPUs`. A namespace with models on the type but neither
  step claims nothing.
- `gpunodes.usageWalk` counts a Pending pod on its `Status.NominatedNodeName`
  (`occupiedNode`), in every view.

### The in-flight ledger

`allocation.InFlightLedger` (`inflight.go`; `DefaultInFlight` for both
engines) holds the holes of replicas admitted but not yet holding a node.
`NodeViewFor` applies it to every view it builds (`holdOn`, charged to
`InFlightOwner`). An entry is per variant (`namespace/name`): `want` replicas
of a `ReplicaShape` to hold a node, `base` placed when admitted, and later
placed counts with when each was first seen. A count is applied only to a view
whose snapshot (`NodeView.takenAt`, from `NodeGPUStore.Read`) was taken after
it was seen; before, the snapshot does not show those pods on their nodes
either. `settle` reads each entry's `placed` func (`variantmeta.PlacedGroups`:
scheduled or nominated pods, not deleting; LWS groups whole, and apart from
them each group only some of whose pods are placed, `domain.PartialReplica`)
outside the lock, and ends an entry at `want`. `holdOn` holds a partial group
first, by `holdPartial`: its unplaced pods only, in the topology domain of
its placed pods' node, plus the placed pods' GPUs on that node when the count
is newer than the view's snapshot.

- Admission: the steady-state engine admits the pass's targets provisionally
  right after `Optimize` (`admitPassInFlight` in `decideV2`,
  `InFlightLedger.Provisional`, which returns the entries it replaced as an
  `InFlightMark`), so a wake during the rest of the cycle does not take the
  nodes the pass chose. After the enforcer and the borrowing releases,
  `admitInFlight` commits the final targets (`Commit`: the marked entries set
  back, then the final admissions, under one lock), skipping the models the
  share plans (their fills are promises); placed-now is `FilledReplicas` when
  the pods were listed. Only a cycle fitted to nodes admits (`cycleFitted`).
  Only growth starts an entry: a target at or below the current replicas
  updates an entry the variant has (`InFlightAdmission.ExistingOnly`), so a
  replacement pod at Kueue's gate after a preemption holds no hole. That
  applies when the variant has an unplaced replica (`InFlightAdmission.Unplaced`)
  or the ledger has already sighted it in this process (`sighted`, set by
  `Commit` and `Admit`, not by `Provisional`). On the first sighting after a
  restart a variant with no unplaced replica and no restored entry starts
  one, so a Pending pod's node stays held even when the persisted ledger
  did not carry it. The scale-from-zero engine admits each woken candidate at
  want 1 (`wake_tick.go`).
- Persistence (`inflight_persist.go` in `allocation` and in `steadystate`):
  every steady-state cycle hands `MarshalInFlight` to the status publisher
  (`controllerstatus.KeyInFlightLedger`, carried over to a new term). It
  holds each live entry's shape, `want`, placed count, expiry and TTL, and
  its partial groups' placed pods. It is bounded by `maxInFlightPersisted`
  and `maxInFlightBytes`, keeping the holds that end last. The engine reads
  it back once, before its first cycle (`restoreInFlight`, from
  `StartOptimizeLoop` and each cycle until a read succeeds), and persists
  nothing before that read. `RestoreInFlight` keeps each entry's clock,
  skips a variant the ledger already holds, and drops an entry with figures
  this controller does not write, then keeps the `maxInFlightPersisted` holds
  that end last. A TTL is cut to `max(InFlightTTL, 3 x PlacementTimeout)`.
  A restored entry has no `placed` func: it counts no pods until the
  variant's next `Commit`. The first fitted cycle ends every restored entry
  it does not admit (`DropRestoredExcept`, from `admitInFlight`; a committed
  admission, a wake's included, takes the entry off `restored`).
  `MarshalInFlight` encodes each entry under the lock. On shutdown the
  publisher writes once more, bounded by 2s (`flushOnShutdown`), so the last
  cycle's ledger reaches the next leader. A restored partial
  group's placed pods are seen at the zero time, so they are never charged
  again on their node. An Unschedulable replica and a half-placed group thus
  keep their holes across a restart: the first cycle's `ExistingOnly`
  admission updates the restored entry. A ledger that is not JSON, or of
  another `InFlightFormat`, is ignored.
- TTL: `InFlightTTL` (5 minutes), or `InFlightAdmission.TTL` when longer
  (a replica on a Kueue flavor: the governor's `PlacementTimeout`, from both
  engines; from the steady-state engine three times it while
  `BorrowCycle.ScaleUpPending`), from the
  admission that raised `want`; an admission of the same replicas with another
  TTL moves the end by the difference, so it shrinks once the entry leaves
  its flavor; an
  equal or lower `want` keeps the clock, so a replica Pending for good is not
  held anew every cycle. An expired entry holds nothing and is dropped a TTL
  later.
- Re-placing its own growth: `holdOn` charges each held replica to its own
  owner (`inFlightReplicaOwner`: the variant and the replica's number). The
  steady-state pass sizes a variant from its current replicas, so before
  `selectV2Optimizer` hands the view to the optimizer it calls
  `ReleaseUncreated` with each variant's `CurrentReplicas`: the holes of the
  replicas numbered above them, whose pods do not exist yet, are the pass's to
  re-place. Held against it, the target alternated 3/2/3 on one free node. A
  replica whose pod exists keeps its hole, and the wake's view releases
  nothing.

`BenchmarkNodeFitCycle` measures one cycle on 2000 nodes with 80 labels each and
50 models, on a half-free cluster and on a mostly-full one fragmented into
4-GPU holes, with and without node fit;
`TestFragmentedClusterDecisionsDifferFromPoolsOnly` checks the fragmented arms
decide differently.

## Skipping an unavailable provider

`allocation.ComputeAvailableConstraints` computes constraints from each
provider on the usage basis it asks for and returns the ones it skipped as
`SkippedProvider`, with `Cause` one of `constants.ProviderSkip*`
(`unobserved`, `failed`, `no_constraints`). The survivors are intersected, so a
skip can only make the answer more permissive.

`wva_constraint_provider_skipped{provider, reason}` has two producers, each
owning its reasons (`metrics.SetConstraintProvidersSkipped`):

- the steady-state engine (`providerSkipReport`,
  `internal/engines/steadystate/provider_skip_report.go`) owns `unobserved`,
  `failed` and `no_constraints`. It replaces its series every time it computes
  constraints, on the optimizing and idle-fleet paths, and logs only when the
  skipped set changes;
- the `gpu-policy` readiness gate (`gpuPolicyReady`, `cmd/main.go`) owns
  `node_read_denied`. It fails the pod only for a physical limiter declared
  alone. Beside quota entries it probes node read, publishes a denial here, and
  logs on change (`composedNodeReadState`).

## Scale-from-zero

`scalefromzero.Engine.placementFor` returns the `placement` one namespace's wakes
are judged against. When every provider is unavailable it falls back to the
namespace's last successful constraints younger than `lastConstraintsMaxAge`
(5 minutes). Each wake admitted against that snapshot is debited from it
(`recordWake` -> `debitSnapshot`), and `SelectionInput.Admitted` adds the debit
to every later fit check, so two wakes cannot pass on the same free GPUs. The
debit is per namespace and per accelerator name as each wake declared it:
best-effort. With limiters declared, every provider unavailable and no young
snapshot, a wake is counted in `wva_scale_from_zero_wake_unchecked_total`.

Against fresh constraints, the wakes of one tick are carried by hand
(`tickWakes`, see [Node fit](#node-fit)): their pods do not hold a node yet,
so the next wake's usage does not count them. Across ticks the in-flight
ledger carries a wake's node hole ([The in-flight ledger](#the-in-flight-ledger)).
Its pool demand is not carried: a quota counts it once the steady-state engine
next publishes the managed usage, the physical pool once its pods land. Under
a quota alone, a second wake in that window can pass on the same quota.

## Per-namespace usage feeding

`DefaultLimiter.ComputeConstraints` feature-detects `NamespaceAwareInventory`
and calls `SetUsedByNamespace(usedByNS)` beside the always-safe `SetUsed`. For a
quota the caller supplies the managed view: `CurrentReplicas * GPUsPerReplica`
summed by `(Namespace, AcceleratorName)` over the cycle's population
(`computeCurrentGPUUsageByNamespace`).

Every namespace being decided about must be **present** in that map, with an
empty inner map when it holds nothing. `NamespaceResourcePools` builds caps only
for the namespaces it is given, so an absent one silently loses its quota. Both
callers guarantee it: the saturation engine for every namespace it optimizes,
the scale-from-zero engine for the namespace it places into.

## The transitional shadow pass

For a policy declaring both kinds, `reportQuotaAloneAllocation`
(`internal/engines/steadystate/quota_alone_report.go`) runs the optimizer a
second time over the quota constraints alone, once every 10 eligible cycles, and
logs `Upgrade note (transitional)` when the set of differing variants changes.
It is marked `TODO(#164)`: remove it, `quotaAloneReportEvery`, the two
`quotaAlone*` engine fields and `GreedyByScoreOptimizer.ShadowOptimize` one
release after the first release that composes limiters.

## Testing the wake's capacity check end to end

`test/e2e/scale_from_zero_capacity_test.go` (label `full`) exercises the
**deny** branch: it declares the physical limiter, fills an accelerator with an
occupier, and expects a parked variant on it not to wake. A denial needs three
things together:

1. the variant's accelerator resolves from a GPU `nodeSelector`: a parked
   variant has no pods to observe, and an unresolved candidate adds no
   demand, so `FitsGPUBudget` passes having checked nothing;
2. a usage view for the provider's basis: for the physical limiter the
   engine's `EnsureFresh` observes one at decision time; a quota needs a
   completed saturation cycle;
3. the occupier's usage lands on the pool key the limits use (Usage bases,
   above).

The trigger fights condition 2. A wake fires on EPP flow-control queueing,
which happens only when the pool has **no ready endpoints**, so the occupier
must not sit in the pool under test. It serves its own model in its own pool
(`fixtures.WithPoolGuide`).

## Assumption: one model, one InferencePool

The scale-from-zero engine reads a model's pending requests from one EPP.
`buildCandidates` takes the pool of the first variant whose pool resolves and
uses it for the whole model group, with no log when another variant resolves
to a different pool. One scrape per model per tick keeps the wake cheap, and
it matches the project's one-model-one-pool contract.

Per-role pools (decode behind one EPP, prefill behind another) are not
supported: a variant would be judged by the other pool's queue. Supporting
them means resolving the pool per candidate, reading each distinct pool, and
still selecting across the whole model so a P/D pair is fitted to one joint
budget.
