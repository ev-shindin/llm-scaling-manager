# One claims ledger for replicas ordered but not yet visible

- **Status:** proposed, revised after review (see [Review](#review)).
- **Problem:** "this capacity is already spoken for" is kept in six places,
  each with its own keys, clocks and settle rules. Every limiter defect fixed
  in #183, #184, #186 and #187 was one of those places missing a fact another
  one had.
- **Decision:** one ledger of replica claims, written by every producer, and
  one view function both engines read capacity through. Claims stay separate
  from what the controller observes (gated pods, admitted usage). Kueue
  borrowing goes behind a seam interface, then into its own package.
- **Shape:** six stages, each one PR, each behind a flag, none allowed to
  change a scaling decision (multi-cycle golden traces unchanged).

## Summary

When the scaling manager orders a replica, the cluster does not show it at
once. Its pods are created, wait at Kueue's admission gate, are scheduled, and
only then appear on a node and in a Kueue read. Until then the controller must
remember that the node, the queue's GPUs and the cohort's free pool are taken,
or the next decision spends them again.

Today that memory is split across six stores. This proposal replaces them with
one **claims ledger**: per variant, a count of replicas ordered and not yet
visible, with the pool chunks they draw from and, for a gang whose pods are
partly placed, where the placed pods are. Each claim has one explicit
lifecycle, including the ways a claim ends without the replica ever showing
up. Both engines read capacity through one function that applies every
deduction in a fixed order and reports the governor's gates with it.

For an operator nothing changes in what is decided. What changes: the claims
are visible (a gauge, an expiry counter, a list in the controller status),
and the same wait is reported the same way wherever it comes from.

## Motivation

### The six stores today

Read on `llm-scaler/main`; paths are under `internal/engines/` unless shown.

| Store | Where | Key | Clock | Settles when |
|---|---|---|---|---|
| In-flight node ledger | `allocation.InFlightLedger` (`DefaultInFlight`, `allocation/inflight.go`) | `namespace/variant`: a count (`want`, `base`, `seen`), plus `partialSeen` for a replica part of whose pods hold a node | `max(InFlightTTL, TTL)`, where the steady pass sets `TTL` to the placement timeout on a Kueue flavor (three times it while a scale-up is pending); `sightedFor` 10m | placed count reaches `want`, the target drops to the placed count, or the TTL |
| Borrow record | `allocation.BorrowInFlight` (`DefaultBorrowInFlight`, `allocation/borrow_inflight.go`) | `namespace/variant`, cohort, flavor, resource, reclaim bit | `InFlightTTL` from the order; a resolved draw lags until a Kueue read after it resolved, at most `InFlightTTL` | pods exist (`Current` reaches the target), the target drops below the ordered one (`Trim`, or `Desired < Target`), or the TTL |
| Gated and admitted pods | `BorrowInFlight.SetCycle` (`gated`, `admitted`, `blocked`, `cycleAt`), fed by `gatedGPUs` (`steadystate/borrowing.go`) | namespace and accelerator | the steady cycle that set them | the next steady cycle |
| Wake's copies | `scalefromzero`: `tickWakes`, `debitTick`, `debitSnapshot`, `lastGood`; the wake's `RecordWake` | as above | the tick | end of tick, or the steady cycle |
| Share promises | `decision.DefaultSharePromised` (`decision/share_promised.go`), applied by `ReservePromised` and `WithholdPromised` | scope (namespace, or "" for the cluster group) and accelerator: a count, with the nodes its holes are on where known (`PromisedHoles`) | `SharePromisedMaxAge` (5m) | the share publishes a new snapshot |
| Governor's watches and persisted ledger | `BorrowGovernor` (`observeNoHole`, `NoHoleOf`, `timeoutReleases`), `BorrowLedger` in the controller-status ConfigMap; the steady engine's `reclaimedUntil` | `namespace/variant`, `namespace/model@cohort` | placement timeout, backoff 4x capped | release, or timeout |

`DefaultBorrowCooldowns` exists only so the wake can read the governor that
the steady engine owns.

Three rules in the node ledger are easy to miss, and all must survive:

- **Pinning.** A held replica whose pods hold no node is a count: each view
  places it best-fit again (`holdOn`). Only a replica part of whose pods hold
  a node is anchored, to the domain of the node those pods were seen on
  (`holdPartial`). The domain comes from where pods landed, never from where
  the decision fitted them: the scheduler (or Kueue's topology-aware
  scheduling) picks the domain, and a claim pinned to D1 while the leader
  lands in D2 would hold 16 GPUs nobody uses.
- **Snapshot order.** A rise in the placed count is subtracted only from a
  view whose snapshot was taken after the rise was seen; an older snapshot
  does not show the pod on its node either (`holdOn`).
- **Unplaced.** A variant with a pod Unschedulable, or gated past a cycle,
  starts no hold for a target that does not grow it (`ExistingOnly` with
  `Unplaced`, `steadystate/inflight.go`): a hole would only keep a node from a
  peer that can use it.

### What the gaps cost (8-GPU units)

Each was fixed with a targeted change; the point is how many places each fix
had to touch.

1. **A wake ignored the borrowing cooldown** (AR1): it preempted a peer's 8
   GPUs, timed out and did it again. #187 made it read `DefaultBorrowCooldowns`.
2. **The no-hole safety net did not survive a restart** (AR2). #187 added
   `NoHole` to `BorrowLedger`.
3. **A node was promised twice in one cycle** (WN5): a wake took the node the
   steady pass had chosen but not yet recorded. #187 added `Provisional` and
   `Commit`.
4. **A half-placed gang lost its hole** (WN2): nothing was held for a 16-GPU
   group's worker once its leader landed. #187 added `partialSeen`.
5. **A gated replica cancelled another workload's Kueue charge** (KU1): #186
   taught the quota inventory to read the gated pods (`SetGatedSource`), a
   seventh consumer of the same fact.

Every fix was right. Together they show the shape: a new fact about a pending
replica is written into the store that owns it, then copied by hand into each
consumer. The engines also apply the transforms in different orders:

- steady (`prepareBorrowing`): `DeductUnreservedAt`, `DeductReclaimed`,
  `BlockCohorts`, `WithholdByVariants`, the governor; then each variant's own
  draws are credited back inside the optimizer (`credit`, `reclaimCredit`),
  and the share pass applies `ReservePromised` and `WithholdPromised`
  (`steadystate/utilization_share_actuate.go`);
- wake (`placementForModel`, `scalefromzero/candidates.go`):
  `WithholdPromised`; `DeductReclaimed` only to compute what is reclaimable
  (`ReclaimableNominal`); then `WithinNominalNow` (which applies
  `DeductUnreservedAt` and `BlockCohorts` itself and needs the figures from
  before them) and `CreditReclaimable`.

### Layering

`internal/engines/allocation` holds about 5,400 lines of Kueue policy in its
`borrow_*.go` files, and nine non-test files there import `internal/kueue`
(among them `quota_inventory.go`, `limiter_factory.go`, `node_view.go` and
`gpu_limit_attribution.go`, which are not borrowing files). `ResourceConstraints`
carries seven fields only a Kueue quota sets: `NamespaceBorrow`,
`SharedPools`, `NamespaceCohorts`, `BorrowWithheld`, `NamespaceNominal`,
`BorrowNamespacesSet`, `KueueNeverRead`.

## Goals

- One record of every replica the controller ordered and the cluster does not
  show yet, whoever ordered it: steady pass, wake, share fill.
- One lifecycle for that record, with every exit named.
- Claims visible to on-call: a gauge, an expiry counter, the controller status.
- One function both engines call to turn a snapshot, the observations and the
  ledger into the capacity a decision may spend.
- Kueue borrowing behind an interface, then in its own package.

## Non-goals

- Changing any scaling decision. A behaviour change is its own PR, after this.
  That includes merging the two clocks (see [Decisions](#decisions)).
- The warm pool's growth. It writes no claims; the `Producer` field leaves room.
- Changing what Kueue figures mean (that is [per-ClusterQueue
  modelling](kueue-per-clusterqueue.md)).
- Several controllers. One ledger per process; cross-controller claims stay
  the multi-tenant Lease question.
- The governor's policy (backoff, cooldown, release order).

## Design

### Claims and observations are kept apart

A **claim** is what this controller ordered. An **observation** is what it
read: pods at Kueue's gate (`gatedGPUs`), usage Kueue admitted since the last
read (`Unreserved.Admitted`), placed pods. The two overlap but neither covers
the other. Gated pods include pods no claim covers: a rollout's surge, a pod
recreated after an eviction, a replica whose claim expired while it is still
gated. Folding the gated figure into "created claims not yet admitted" would
drop those, and KU1 would come back.

So the ledger holds claims only. `ViewFor` takes both, and each deduction
names its source.

### A claim

Claims are counts, not replica identities. Kueue, the scheduler and the
ReplicaSet controller decide which pod becomes which replica; the controller
only knows how many it asked for.

```go
type Claim struct {
    Variant   string          // namespace/variant
    Queue     string          // ClusterQueue, once per-queue binding lands
    Producer  Producer        // Steady, Wake, ShareFill; open for later ones
    Shape     ReplicaShape
    Want      int             // replicas the producer ordered the variant to
    Base      int             // replicas placed when the claim was made
    Seen      []PlacedAt      // later placed counts, with when they were seen
    Pool      []PoolDraw      // per cohort/flavor/resource chunk: replicas, GPUs, reclaim
    Partial   []PartialHole   // per gang replica part-placed: the node its pods are on
    Flags     ClaimFlags      // NoHole, Borrowed, Wake
    Stage     Stage
    OrderedAt, StageAt time.Time
    NodeTTL, PoolTTL   time.Duration
}
```

- **Node holes** are `Want - placed` replicas of `Shape`, placed best-fit by
  each view, exactly as `holdOn` does now. `Partial` is today's `partialSeen`:
  only for a gang (a LeaderWorkerSet group, or any replica of several pods)
  some of whose pods hold a node, anchored to that node's domain.
- **Pool draws** are today's borrow record, one per chunk, as counts.
- A claim never names a node or domain that no pod has landed in.

### Lifecycle

```
ordered -> created -> scheduled -> visible        (removed)
   |          |           |
   +----------+-----------+--> cancelled | gone | expired   (removed)
              |
              +--> unplaced  (holds pool GPUs, no node hole)
```

- **ordered**: the decision was made. Holds node holes and pool draws.
- **created**: the pods exist (`Current` rose). Still holds both: a gated or
  Pending pod holds no node in the snapshot.
- **unplaced**: a pod is Unschedulable, or gated past a cycle. Starts no new
  hold for a target that does not grow the variant (today's `Unplaced` rule);
  holes and draws it already holds keep holding until their clocks run out.
  Releasing them early changes decisions and belongs to the one-clock
  follow-up.
- **scheduled**: every pod holds a node. Node holes go, on the snapshot-order
  rule: only for views whose snapshot was taken after the rise was seen. Pool
  draws stay, because the Kueue read may predate the admission.
- **visible**: a Kueue read taken after `StageAt` shows it. Removed.

Exits that end a claim without it becoming visible:

- **cancelled**: the target went below `Want` (a release, `Trim`, the share
  lowering it, or `Desired < Want` read back). The claim shrinks to the new
  target; at or below the placed count it ends.
- **gone**: the variant was deleted, or its pods were deleted or preempted
  before they became visible (the variant's replica count fell while the
  claim was open). The missing replicas' claims end; a replacement is a new
  claim.
- **expired**: a clock ran out (below). The governor is told, as
  `observeNoHole` and `timeoutReleases` are told now.

**scheduled to visible** has its own timeout. Today a lagging draw waits at
most `InFlightTTL` for a Kueue read; with Kueue unreadable, an unbounded wait
would hold the pool forever. The claim keeps that bound: a scheduled claim
with no fresh Kueue read within `PoolTTL` of `StageAt` expires, counted with
reason `read-timeout`.

### Clocks

Each claim keeps today's two clocks: `NodeTTL = max(InFlightTTL, TTL)` for
node holes (with `TTL` the placement timeout on a Kueue flavor, tripled while
a scale-up is pending) and `PoolTTL = InFlightTTL` for pool draws. Where the
placement timeout is shorter than `InFlightTTL`, the node hole is still held
`InFlightTTL`, as now. A raise of `Want` restarts the node clock; an equal or
lower one keeps it. `sightedFor` stays with the first-sighting rule below.

### Writing a claim: compare-and-claim

```go
func (l *Ledger) Claim(c Claim, readAt Version) (Version, error)
```

Every view carries the ledger version it was built from. `Claim` succeeds only
if no claim for the same variant changed since that version; otherwise it
returns `ErrStale` and the producer rebuilds its view. This replaces
`Provisional`/`Commit`:

- The steady pass claims right after the optimizer (as `Provisional` does
  now), and updates its own claims at the end of the cycle.
- A wake that read its view before the steady pass claimed gets `ErrStale`
  and re-decides on the fresh view: it cannot take the node the pass chose
  (WN5).
- The steady pass's end-of-cycle commit is exempt from the `ErrStale`
  check. For every variant the cycle covered it replaces whatever claim stands,
  a wake's made since included, so a wake and a steady pass on one variant do
  not stack. That is today's `Commit` rule, now explicit rather than a side
  effect.

### Share promises

The share keeps its own ledger (`utilization_share_ledger.go`): it tracks
transfers between roles, which are not replicas. It publishes its promises as
count-only claims with `Producer: ShareFill`, keyed by scope and accelerator
as `SharePromisedStore` is today. They hold no node: a promise is GPUs a
donor freed for a receiver, and the receiver's pods may land anywhere they
fit. The view subtracts a promise once, in its own step, and never also as a
node hole or pool draw. `SharePromisedMaxAge` becomes the claim's `PoolTTL`.

### Persisted, and what a crash loses

A claim is written in memory at the decision and persisted to the
controller-status ConfigMap at the end of the cycle, leader-only, beside the
governor's clocks. A crash between the two loses the cycle's new claims. The
first-sighting rule (`InFlightAdmission.ExistingOnly`, `sightedFor`) is kept
as the fallback for exactly that window: after a restart, a variant with
Pending pods and no restored claim gets a hold as today.

On restart a claim is restored with its stage and clocks, never younger than
it was, and dropped at its first cycle if its variant is gone or its pods
show it visible.

Size: count claims are one per variant and producer, so the ledger is small.
It is capped (default 200 claims); past the cap the oldest are dropped and
counted. If the cap is reached in practice, the ledger moves to its own
ConfigMap.

### One view

```go
func (l *Ledger) ViewFor(consumer Consumer, snap Snapshot, obs Observations,
    now time.Time) View

type View struct {
    Before, After []*ResourceConstraints // before and after the deductions
    Nodes   *NodeView
    Credit  map[string]Credit             // per variant: its own claims, to add back
    Gates   Gates                         // blocked cohorts, cooldowns, backoff, lender waiting
    Version Version
}
```

- **Per-variant credit.** The steady pass computes constraints once and sizes
  many variants against them. So `ViewFor` deducts every claim, and returns
  each variant's own claims as `Credit`, which the optimizer adds back for the
  variant it sizes (today's `credit` and `reclaimCredit`).
- **Before and After.** `WithinNominalNow` needs the figures from before the
  in-flight deduction; the view returns both.

The deductions, in one order:

1. observations: gated pods and admitted usage (`DeductUnreservedAt`);
2. claims' node holes onto the node view, `Partial` on their domains;
3. claims' pool draws off the shared pools;
4. reclaim draws off what is reclaimable (`DeductReclaimed`); for the wake,
   into `ReclaimableNominal` only, as now;
5. share promises (`WithholdPromised`; `ReservePromised` on the share's nodes);
6. what only templates show (`WithholdByVariants`);
7. cohorts closed: unreadable gated pods and the governor (`BlockCohorts`);
8. the consumer's scope: the steady pass the full figures; the wake
   `WithinNominalNow` and `CreditReclaimable`; the share guaranteed only.

Steps 4 and 8 differ per consumer today, and must still differ: the view
makes the difference one `switch` on `consumer` instead of two call chains.
Stage 1's traces say whether any step can move without a decision changing.

### Observability

- `wva_claims{stage, producer}`: claims held now, by lifecycle stage.
- `wva_claims_ended_total{producer, outcome}`: `visible`, `cancelled`,
  `gone`, `expired`, `read-timeout`, `dropped` (over the cap).
- The controller status lists claims per variant: stage, want, placed, pool
  chunks, age.
- `wva_claims_view_mismatch_total{consumer, field}` while shadowed (stage 2).

### Kueue borrowing behind a seam, then in its own package

The borrowing code cannot simply move: it is methods on `NodeView`
(`borrow_node_fit.go`), on `QuotaInventory` (`borrow_nominal.go`) and on
`GreedyByScoreOptimizer` (`borrow_placement.go`, `borrow_rescale.go`), and
four non-borrowing files import `internal/kueue`. So:

1. **Seam.** `allocation` defines the interface it needs from Kueue
   borrowing (pool figures, gates, the per-variant fit of a borrowed replica)
   and the optimizer and node view call it. The Kueue implementation stays in
   `allocation` for now.
2. **Move.** The implementation, the governor (fed from the ledger; engines
   read only `Gates`) and the pool half of the ledger move to a Kueue package.
   The seven Kueue-only fields leave `ResourceConstraints`. Last, a diff of
   moves.

## Alternatives considered

- **Keep the targeted fixes.** Every known defect is fixed, and the stores
  work. The cost is the next change: a new producer has to be wired into five
  stores and two transform orders. This is the fallback if stage 1 shows the
  ledger cannot reproduce today's decisions.
- **Pin claims to the node fitted at order time.** The scheduler picks the
  domain; a claim pinned elsewhere holds GPUs nobody uses.
- **One lock, not one ledger.** Fixes ordering races (WN5) but not the
  duplicated keys and clocks.
- **Rebuild from pods every cycle.** Pods tell what was created, not which
  pool it drew from, and a namespace-scoped install cannot list every pod.

## Staged rollout

Each stage is one PR, passes the golden traces unchanged, and stages 3 and 4
are behind a manager-config flag (`claimsLedger.wake`, `claimsLedger.steady`)
that defaults off in the release that adds it and on in the next.

1. **Golden traces.** Multi-cycle scenarios as tests: each a sequence of
   cycles, wakes and share passes over a changing snapshot, asserting targets,
   reasons and releases after every step. Covers WN2, WN5, AR1, AR2 and KU1,
   which are cross-cycle or wake-versus-steady. No production change.
2. **The ledger, shadowed.** Producers also write claims; `ViewFor` runs
   beside today's figures; any difference increments
   `wva_claims_view_mismatch_total`, and the trace tests assert it stays zero.
   The claim gauges ship here.
3. **The wake reads the view** (flag `claimsLedger.wake`). The wake's copies,
   `debitTick` and `DefaultBorrowCooldowns` go once the flag's default is on.
4. **The steady pass reads the view** (flag `claimsLedger.steady`).
   `BorrowInFlight`, `InFlightLedger` and the share promise store go.
   `BorrowLedger.NoHole` is still written for one release, from the ledger, so
   a rollback to a release before this one keeps the AR2 safety net.
5. **Seam interface** for Kueue borrowing, no move.
6. **Package move.** Last, alone, no logic change.

Where this lands relative to per-ClusterQueue modelling is in [Landing
order](#landing-order).

## Test strategy

- The golden traces from stage 1 run on every stage.
- One table test per lifecycle edge: each state and each exit (cancelled,
  gone before visible, preempted before visible, unplaced, expired,
  read-timeout), and a restart in each state.
- Compare-and-claim: a wake between the steady pass's claim and its commit;
  a wake whose view predates the claim gets `ErrStale`.
- Observations without claims: a rollout surge and an evicted pod still
  gated are deducted with no claim covering them.
- Every regression test added in #183, #184, #186 and #187 stays and passes
  unchanged; their names are the list of cases the ledger must cover.
- The kind e2e suites for borrowing and scale-from-zero run on stages 3 and 4,
  with each flag on and off.

## Risks

- **A silent decision change.** Stage 2's mismatch counter and the traces are
  there to find it before any reader switches; the flags take it back.
- **Restoring a stale claim.** A claim whose pods were deleted while the
  controller was down must be dropped at the first cycle, never restored
  younger.
- **Rollback after stage 4.** Covered by the one-release `NoHole` dual-write.

## What it removes

`InFlightLedger`, `BorrowInFlight` (with `lagging`, `SetCycle`, `Unreserved`),
`SharePromisedStore`, `DefaultBorrowCooldowns`, `Provisional`/`Commit`, the
wake's `tickWakes`/`debitTick`, `BorrowLedger.NoHole` (one release later), the
per-engine transform chains, and seven fields of `ResourceConstraints`. Kept:
`InFlightTTL`, the placement timeout, the first-sighting rule.

## Decisions

The open questions of the first draft, as the review answered them.

1. **One clock or two?** Two, per claim part, as today. Merging them changes
   when an unflavored hold ends and is its own PR. *Dissent:* one reviewer
   preferred one clock (`placementTimeout`) now, keeping the `Unplaced` early
   release; that remains the follow-up.
2. **Warm pool claims?** No. `Producer` is left open for it.
3. **Share fill a claim, or its own ledger?** Its own ledger, publishing
   count-only claims, subtracted once.
4. **Governor's home?** It moves to the Kueue package with borrowing, fed
   from the ledger; engines read only `Gates`.
5. **Persisted size?** Count claims are small: cap at 200, drop the oldest,
   count it; move to its own object if the cap is reached in practice.

## Landing order

Agreed across both proposals:

1. Stage 1 of both: these golden traces, and [per-ClusterQueue](kueue-per-clusterqueue.md)
   read-and-report. Both read-only.
2. Per-ClusterQueue stages 2 and 3, with per-queue usage keying done there.
   This fixes live over-ordering on shared queues; the traces are re-baselined
   once.
3. Ledger stages 2 to 4, with claims keyed per queue from the start.
4. Ledger stages 5 and 6 (seam, then move), and per-ClusterQueue stage 4.

## Review

Reviewed by a whole-node P/D and SRE reviewer, an architect and a Kueue
maintainer. This revision: keeps today's pinning rule (the draft pinned at
order time, which was wrong); makes claims counts with per-pod holes for
gangs only; separates claims from observations; adds per-variant credit,
the full lifecycle with a scheduled-to-visible timeout, compare-and-claim,
count-only share promises, the first-sighting fallback, metrics, per-stage
flags, the `NoHole` dual-write, a seam stage before the move, and multi-cycle
traces in place of log diffs.

## Related

- [Kueue borrowing, and limiters that compose](kueue-borrowing-and-limiter-composition.md)
- [Model Kueue per ClusterQueue](kueue-per-clusterqueue.md)
- [Sizing against in-flight capacity](sizing-against-in-flight-capacity.md)
- [Limiter composition (developer guide)](../developer-guide/limiter-composition.md)
