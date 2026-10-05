# A per-token economy for the saturation analyzer

Status: proposal. Nothing here is built. The reference half (§2, §3) describes
code that exists today and is accurate as of `7a504ead`.

Two documents already argue the halves of this: [shape-shift
treatment](shape-shift-treatment.md) derives decode's `mu` from the ITL line so
a shape change reprices on the cycle it is seen, and [prefill capacity from a
fitted TTFT model](prefill-ttft-model.md) gets prefill a throughput ceiling it
has never had. Both then convert their result **back into requests per second**
at the boundary, because that is the unit the floor speaks. This document is
about deleting that conversion.

## 1. Why

A request is not a unit of work. One decode request with a 6000-token output is
~24x the work of one with 250, so "requests per second per replica" is a
property of a replica **and a shape**, not of a replica. Every mechanism that
exists to cope with that — the per-output-length bucket keys, the shape tracker,
the stale-shape hold, the borrowed-reading rule — is downstream of the units.

Measured cost, run QF (2026-10-03, kermit/`evgensh-shapeswap`, H200,
Qwen3-0.6B, the `1k/6000 -> 20k/250` trace):

| time | `heldWhy` | `replicasImplied` | `desired` | scheduler queue |
|---|---|---|---|---|
| 06:37:11Z | `shape-change` | 6.53 | 1 | ~1000 |
| 06:38:56Z | `shape-change` | 10.39 | 1 | ~1800 |
| 06:39:11Z | `shape-change` | 13.68 | 1 | 2503 |
| 06:39:26Z | *(released)* | 13.80 | **10** | 2503 |

For six minutes the analyzer computed that it needed up to 13.7 replicas and
acted on one, because every `mu` on record had been learned under a different
shape. Client TTFT for that run was a median of 77.5 s. The same stack, which
by accident started with its shape already registered (run QE), measured 44.0 s.

Two further facts from the same run, both of which the unit choice causes:

- The "shape change" at 06:33:41Z was `inputTokensWas: 20000 ->
  inputTokensNow: 1000`, where the 20000 was recorded at 22:17Z the previous
  night. The fleet had been idle for eight hours. **An idle fleet meeting new
  traffic is not a shape change**, but it is indistinguishable from one when
  capacity is keyed by shape.
- Prefill held 439 queued requests with 3 of its 4 pods idle, at 0.2 KV
  utilization, while its share of the scheduler queue was dropped from demand
  entirely — because `tf.ByRole` has no prefill entry, because prefill has no
  `mu`, because prefill completes ~1 token per request and a request rate
  cannot describe it.

## 2. What exists today: components

Pipeline for one `(model, namespace)` per reconcile cycle (~15 s):

```
collector  ──> domain.AnalyzerInput ──> SaturationAnalyzer ──> domain.AnalyzerResult
                                              │                        │
                   ┌──────────────────────────┤                        v
                   │  capacity (k1, k2)       │                 steadystate engine
                   │  shape.Tracker (I, O)    │                   RC / SC per role
                   │  itl.Window + Fit        │                        │
                   │  deriveMu                │                        v
                   │  floor.Estimate          │                 actuator ──> KEDA
                   └──────────────────────────┘                  (external scaler)
```

**Collector** (`internal/collector/registration/saturation.go`) registers one
PromQL query per signal and fills `domain.ReplicaMetrics` per pod. The fields
this document depends on: `TotalKvCapacityTokens` (C), `KvUsageInstant` (k),
`AvgITL`, `GenerationTokenRate`, `QueueLength`, `LocalQueueDemand`,
`AvgOutputTokensRecent`. The arrival rate and the router queue come from the
EPP: `llm_d_epp_flow_control_queue_size` and the model arrival rate.

**Capacity** (`internal/signals/capacity`) is what one replica can hold, learned
two ways and kept per variant so a variant with no live replica can still be
sized. `k1` is memory-bound, `k2` compute-bound, and `k2` comes from a priority
chain — the analyzer takes the first that yields:

| priority | label | source |
|---|---|---|
| 1 | `P1-obs` | the queue is saturated: `tokensInUse` observed now |
| 2 | `P2-hist` | rolling average over prior observations |
| 3 | `P3-k2` | estimated from deployment args |
| 4 | `P4-k1` | fall back to `k1`, the memory bound |

**Shape** (`internal/signals/shape`) is the `(I, O, hitRate)` the fleet serves,
with a tracker that reports when it has moved outside a tolerance band measured
from an **anchor** (not from the previous reading, which would make the
tolerance a per-cycle rate limit and never fire on a slow drift).

**ITL model** (`internal/signals/itl`) fits inter-token latency against KV
utilization over a rolling window, two tiers: OLS when the window has both
sample count and k-spread, otherwise slope-only with `B` pinned to the learned
hardware floor. The window is **not** cleared on a shape change — `ITL(k)` is a
property of the hardware, which is the whole reason a shape-independent `mu` is
derivable.

**Demand floor** (`internal/signals/floor`, applied by
`saturation.applyThroughputFloor`) raises demand to what the offered load
implies, per role, and holds it at the fleet's own size when the reading it
would order on is not trustworthy.

**Engine** (`internal/engines/steadystate`) turns demand into a replica count.
**Actuator** publishes it; KEDA scales on it.

## 3. What exists today: every formula

Units are stated because the proposal is a change of units.

**Shape** (`shape.New`):

```
ILeff = I * (1 - hitRate)                       tokens/request
KVreq = ILeff + O/2                             tokens/request
```

`O/2` because sequence ages are uniform over `[0, O]` in steady state, so the
average resident sequence carries half its generation.

Change detection (`Shape.Within`, 20% default tolerance), on `I` and `O` only —
`hitRate` is computed but deliberately not compared:

```
changed = |I - I_anchor| / I_anchor > tol  OR  |O - O_anchor| / O_anchor > tol
```

**Capacity:**

```
k1 = C * KvCacheThreshold                       tokens        (memoryBound)
P  = PerReplicaCapacity                         tokens        (scale-target units)
```

**ITL and the derived rate** (`itl`, `deriveMu`):

```
ITL(k)            = A*k + B                                   seconds/token
Sequences(k,C,Q)  = k * C / Q                                 requests resident
TokenRate(k)      = Sequences(k,C,Q) / ITL(k)                 tokens/second

kPrice   = clamp(KvCacheThreshold * ScaleUpThreshold,
                 DefaultMinObservableK, DefaultMaxObservableK=0.80)
                                                              dimensionless
           (defaults 0.80 * 0.85 = 0.68; DefaultKSat = 0.85)

seqs     = min(Sequences(kPrice, C, KVreq), MaxNumSeqs)       requests
itlSec   = ITL(kPrice)                                        seconds/token
tokenSec = seqs / itlSec                                      TOKENS/second
rate     = tokenSec / avgOutput                               REQUESTS/second  <-- (!)
```

The last line is the subject of this document. `avgOutput` is deliberately the
**short-window** output length, not the shape's `[5m]` figure: a count-weighted
mean over five minutes is dominated by the previous shape's stragglers for five
minutes after they stop arriving (measured decaying 3750 -> 250 across one
`6000 -> 250` switch), and the divisor is where that error reaches `mu`
undamped.

**Diagnostic only**, never a gate (`noteLineMismatch`):

```
GPSErrorPct = |TokenRate(k) - GenerationTokenRate| / GenerationTokenRate * 100
```

**The floor** (`floor.Estimate`), per role:

```
mu       = median over the role's own replicas of SaturatedThroughput   req/s
P        = median over the role's variants of PerReplicaCapacity        tokens
Backlog  = own engine queues + (scheduler queue, charged to ONE role)   requests
Replicas = (lambda + Backlog / DrainSeconds) / mu                       replicas
floor    = Replicas * P                                                 tokens
```

with `DrainSeconds = BacklogDrainSeconds = 60`, which must not be shorter than a
replica start or the order drains nothing.

Backlog is projected forward to the moment ordered capacity becomes Ready
(`backlogAtLanding`), because pricing the queue as it stands sizes for a backlog
the fleet will have outgrown by the time the replica arrives.

**The hold**, when the reading may not be ordered on:

```
hold  = ScaleUpThreshold * anticipatedSupply                            tokens
floor = min(floor, hold)        and  Held = true
HeldWhy ∈ { "single-sample", "borrowed", "shape-change" }
```

`mayOrder` is false for three reasons: fewer than `MinThroughputSamplesToOrder`
(=2) readings; the only reading is borrowed from a neighbouring output-length
bucket; or `staleShape`. A derived `mu` bypasses all three.

**The queue release**, which lets a thin window order anyway:

```
fires when   !mayOrder  AND  !staleShape  AND  !borrowedOnly
             AND  schedulerQueued > max(lambda, mu)
QJR    = max(1, floor(Q / (mu * DrainSeconds)))                         replicas
step   = ScaleUpThreshold * (readySupply + QJR * smallestP)             tokens
floor  = min(floor, step)
```

`staleShape` is excluded deliberately: "a queue does not make a reading for the
wrong shape right". That exclusion is guarded by a test, and it is what cost run
QF its six minutes.

**The engine:**

```
RC          = max(0, TotalDemand / ScaleUpThreshold - TotalAnticipatedSupply)
SC          = max(0, TotalSupply - TotalDemand / ScaleDownThreshold)
Utilization = TotalDemand / TotalSupply
```

## 4. The proposal

Make tokens the native unit of both demand and capacity, per role. Delete the
two conversions back to requests.

**Decode.** Capacity is already computed in tokens and then thrown away:

```
capacity_decode = tokenSec = min(Sequences(kPrice,C,KVreq), MaxNumSeqs) / ITL(kPrice)
                                                              output tokens/s/replica
demand_decode   = lambda * O                                  output tokens/s
replicas_decode = demand_decode / (ScaleUpThreshold * capacity_decode)
```

**Prefill.** Capacity from the fitted TTFT line of
[prefill-ttft-model.md](prefill-ttft-model.md), used directly rather than
converted:

```
TTFT(T)          = A_p*T + B_p                                seconds
capacity_prefill = 1 / A_p                                    input tokens/s/replica
demand_prefill   = lambda * ILeff                             input tokens/s
replicas_prefill = demand_prefill / (ScaleUpThreshold * capacity_prefill)
```

**Backlog, in each role's own currency.** This is what unblocks the dropped
share. A queue of `Q` requests is `Q * ILeff` input tokens of prefill work and
`Q * O` output tokens of decode work — the same queue, priced twice, in two
units, with no double count, because the two roles consume different things:

```
demand_prefill = (lambda + Q / DrainSeconds) * ILeff
demand_decode  = (lambda + Q / DrainSeconds) * O
```

Today the queue is charged to prefill as a **request count** and then discarded
because prefill has no `mu` to divide it by. In tokens it needs no `mu`.

**What this removes.** A shape change moves `I` or `O`, which appear only in
**demand**. Capacity — `tokenSec` for decode, `1/A_p` for prefill — contains
neither. So:

- `staleShape` has nothing to protect: no capacity figure is invalidated by a
  shape change, so there is no stale reading to withhold an order on.
- The shape tracker stays, but as an *input to demand*, not a trigger for a
  hold. Its anchor/tolerance logic is still needed to decide when the ITL and
  TTFT windows should be refitted; it stops gating scale-up.
- The idle-fleet false positive (§1) stops mattering: a stale `(I, O)` produces
  a stale *demand* for one cycle, corrected on the next, instead of a
  six-minute hold.
- The per-output-length bucket keys on the throughput window
  (`outputBuckets`, `nearestSaturatedThroughput`, the `borrowed` rule) lose
  their reason to exist for the floor. `outputBuckets` is still used for `k2`
  and stays.

**Direction** (`O` down, `I` up, etc.) also falls out rather than needing a
rule. The direction table in
[shape-shift-treatment.md](shape-shift-treatment.md#the-two-directions-and-what-each-breaks)
enumerates four shifts and what each breaks late. Under a token economy each is
just a demand term moving: `O` down lowers `demand_decode` immediately, `I` up
raises `demand_prefill` immediately. No hold, no discount, no asymmetry to
encode.

### Composing the two roles, and three rules to take from the composite work

Per-role token demand is necessary but not sufficient: it says how much each
role needs, not what the model can serve. The composite-analyzer work on
`deanscaler/composite-analyzer` already settles that, and this proposal should
adopt its rules rather than invent parallel ones.

**1. The roles are in series, so the model's capacity is a minimum.**
`modelCoverageFromRoles` (spec §5.4):

```
coverage(M) = min(cov(prefill), cov(decode)) + cov(both)
```

A request must be prefilled *and* decoded, so a fleet serves
`min(capacity_prefill / ILeff, capacity_decode / O)` requests per second.
Adding decode replicas while prefill is the bottleneck buys nothing — which is
exactly what runs QE and QF measured: decode's imbalance and thrash were fixed
and the fleet still delivered 44–77 s TTFT, because prefill was the binding
term. A per-token economy that sizes each role against its own demand in
isolation would not have caught that; the min does.

**2. Absence is undefined, not zero.** `internal/engines/aggregation/undefined.go`
(spec §2.5, A14) states the rule this proposal most needs:

> a skipped/undefined contribution must never silently enter a min or max as a
> magic number. Feeding it in as 0 would make a min wrongly veto a scale-down;
> feeding it in as +Inf would make a max wrongly demand infinite replicas.

That is precisely the defect behind prefill's dropped backlog. Prefill has no
`mu`, so it gets no `tf.ByRole` entry, so its share of the scheduler queue is
*subtracted from demand* — an undefined capacity silently became zero demand,
and 439 queued requests justified nothing. Under this proposal prefill acquires
a defined capacity (`1/A_p`), which removes the immediate cause; the discipline
still has to be honoured for the case where the TTFT fit is not yet ready, and
the `(value float64, ok bool)` pair plus `minOfDefined`/`maxOfDefined` are the
existing shape for it.

**3. Normalize to dimensionless coverage at the optimizer boundary.**
`normalizeToCompositeUnits` on `deanscaler/single-analyzer-normalize` converts a
role's demand and supply so demand reads 1.0 at capacity. Tokens are the right
unit for *measuring* a role; a ratio is the right unit for *combining* roles and
analyzers, because input tokens and output tokens cannot be added. So the two
changes compose in a fixed order: price per role in tokens, then normalize to
coverage, then take the min across roles. Getting that order wrong is how input
and output token rates end up summed into a number that means nothing.

## 4a. Most of the decode half already exists, in the analyzer that is off

The throughput analyzer (`internal/engines/analyzers/throughput`) already prices
decode per token, and not merely as a measurement — it is the whole economy:

```
demand = Σ RequestRate_r * AvgOutputTokens_r              = lambda * O, output tokens/s
supply = computeVariantSupply(metrics, shape, ITLAt(kSat)) = Sequences/ITL, output tokens/s
```

with `computeLocalDemand` falling back to `itl.TokenRate` directly when the EPP
arrival rate and the engine request rate are both missing. So §4's decode half
is mostly a matter of **not re-deriving** what is already written, rather than
new arithmetic.

It is off by default, and it was off for every run in §5. `cmd/main.go` registers
it only when a saturation-config entry enables `throughput`, and the configs on
both benchmark clusters list `[{name: saturation, score: 1.0}]`. The comment at
that call site records the consequence of the earlier coupling: gating the
arrival-rate query on it "made that floor structurally inoperable whenever
throughput was disabled -- which is the default."

**The duplication, and why it is worse than duplication:**

| | throughput analyzer | `saturation` |
|---|---|---|
| decode demand | `lambda * O`, output tokens/s | `tokenSec` then divided by `O` -> requests/s |
| decode supply | `Sequences / ITLAt(kSat)` | the same `tokenSec`, discarded |
| shape tracker | owns one | owns another (`shapeMemo`) |
| ITL window + two-tier fit | owns one | owns another (`noteITL`, `itlWindows`) |
| ITL window on a shape change | **cleared** | **deliberately not cleared** |
| GPS check | clears the window after N mismatches | diagnostic only, never gates |

The last two rows are the problem. Two copies of the same state answer the same
question with opposite policies, each documented as correct in its own file:
`noteITL` says the window must survive a shape change because `ITL(k)` is a
property of the hardware — which is the whole premise of a derived mu — while
the throughput analyzer clears it. Both cannot be right, and today neither is
wrong in practice only because one of them never runs.

**One analyzer or two?** Two questions with different answers.

*How many signal producers* can stay plural. The multi-analyzer pipeline exists,
carries per-analyzer scores and thresholds, and Dean's
`replace AnalyzerResults slice with single CompositeSignal` keeps producers
plural while giving the optimizer one normalized signal to read. Nothing here
argues for collapsing that.

*Who owns shape and ITL* must be exactly one. That is the defect, and the fix is
smaller than merging analyzers: `internal/signals/shape` and
`internal/signals/itl` are already shared packages, and only the **state** — the
`Tracker` and the `Window` — was copied into each analyzer. Give that state a
single owner at the signals level and have both analyzers read it. Then the
shape-change policy is decided once, in one place, which is the precondition for
item 4 of the build order meaning anything.

## 5. Evidence

Measured in runs QE and QF. **Both runs are the `I`-up cell the direction table
lists as "predicted: prefill under-ordered. Not run."** That row can now be
marked measured:

| claim | measurement | status |
|---|---|---|
| `I`-up under-orders prefill | QE phase 2: prefill 439 queued, 3 of 4 pods idle, KV 0.2; its queue share dropped from demand every cycle | **measured** |
| the stale-shape hold suppresses a needed scale-up | QF: `heldWhy="shape-change"` for 6 min at `desired=1` while `replicasImplied` reached 13.68 | **measured** |
| an idle fleet is misread as a shape change | QF 06:33:41Z: `20000 -> 1000`, the 20000 recorded 8 h earlier | **measured** |
| `tokenSec` is a usable per-replica token rate | QF logged `tokenSec` 9,056–9,502; measured output tok/s per replica 8,198 at the same occupancy | **measured** |
| per-replica **request** rate is shape-dependent | 3.0 / 1.3 / 3.9 req/s/replica across three windows of one run | **measured** |
| per-replica **token** rate is occupancy-dependent, not shape-dependent | 8,198 / 3,205 / 3,825 output tok/s/replica at 165 / 103 / 12 seqs per replica | **measured, and it is why capacity must be priced at a chosen `k`, not observed raw** |
| prefill per-replica token ceiling | 79,433 input tok/s/replica under load; 45,000 and 1,500 in under-loaded windows | **measured, and it reproduces the sampling problem recorded below** |

The second-to-last row is the one to be careful about. Raw observed token rate
is **not** invariant — it moves with batch size. The invariant is
`TokenRate(kPrice)`, evaluated at a chosen utilization from a fitted line, which
is what `deriveMu` already computes. A per-token economy must price at `kPrice`,
not read the meter.

## 6. Open decisions

1. **Which `k` to price capacity at.** Today `kPrice = KvCacheThreshold *
   ScaleUpThreshold = 0.68` and the engine then divides demand by
   `ScaleUpThreshold` again. Under a token economy that composition sets fleet
   size directly for both roles, and double-applying the threshold would
   over-provision by `1/ScaleUpThreshold`. The arithmetic has to be stated once,
   in one place, before anything is built. Prior art: pricing at `k_sat = 0.85`
   is what sank the first mu-from-ITL attempt.
2. **Prefill's sampling moment.** Recorded in shape-shift-treatment and
   independently reproduced here: at 16 s scrapes a small model's prefill reads
   zero prompt tokens/s on some intervals and 50–156k on others (this run:
   1,500 / 45,000 / 79,433 depending on load). A TTFT fit needs a defined
   sampling moment. Unresolved.
3. **Prefill saturation is never recorded while decode is saturated** (the
   downstream rule in `analyzer.go`), which on a P/D fleet under a shift is
   nearly always. The TTFT fit is designed to avoid needing saturation at all
   (`1/A_p` is obtainable from ordinary traffic), which is the reason to prefer
   it over a `k2`-style observation — but the rule itself still has to be
   revisited for prefill's own capacity record.
4. **`MaxNumSeqs` interacts with the cap.** `seqs` is capped at the engine's
   `max_num_seqs`, so `capacity_decode` is piecewise: KV-bound below the cap,
   admission-bound above. Measured on this stack: KV holds 1,162,368 tokens, so
   at a 7000-token footprint 166 sequences fit and `max_num_seqs` of 256
   admitted 1.55x more than KV could hold, which preempted at up to 25.9/s.
   Pricing must take the min, as `deriveMu` already does.
5. **What remains of the hold.** If `staleShape` stops gating, the
   `single-sample` and `borrowed` holds still apply to the *ITL fit* and the
   *TTFT fit*. Those are fits over hardware behaviour, so the holds get rarer,
   not absent.
6. **Who owns the normalization boundary.** The composite-analyzer work already
   normalizes demand and supply so demand reads 1.0 at capacity, and already
   defines the `min` across roles and the undefined convention. This proposal
   changes what is measured upstream of that boundary. The two should not both
   grow their own version of it — so either this lands on top of that work, or
   the boundary moves here, and that is a coordination decision rather than a
   technical one.

## 7. What to build, in order

This supersedes neither existing proposal; it is the unit change that makes both
land. Item 1 of shape-shift-treatment (the ITL-derived `mu`) is already built in
`mu_from_itl.go`.

0. **One owner for shape and ITL.** Move the `shape.Tracker` and `itl.Window`
   state out of both analyzers to a single owner at the signals level, and
   decide the shape-change clearing policy once (§4a: the two copies disagree
   today, and `noteITL`'s reasoning is the one consistent with a derived mu).
   This is a refactor with no behaviour change, it is the smallest item, and
   everything below is unsound without it — two owners means two answers.
1. **Decode in tokens.** Stop dividing `tokenSec` by `avgOutput`; carry
   `capacity_decode` in output tokens/s and `demand_decode = (lambda + Q/drain)
   * O`. Reuse the throughput analyzer's existing arithmetic (§4a) rather than
   writing it again — `computeDemand` and `computeVariantSupply` are already
   this, and `computeLocalDemand` is a fallback saturation lacks. Settles open
   decision 1 first. *Touches:* `deriveMu`, `floor.Term` (`Mu`/`PerReplica`
   become token quantities), `applyThroughputFloor`, the engine's demand
   comparison.
2. **Prefill in tokens.** Collect TTFT per replica, fit `TTFT(T) = A_p*T + B_p`
   beside `internal/signals/itl`, publish `1/A_p`, and price
   `demand_prefill = (lambda + Q/drain) * ILeff`. Closes the dropped share.
   Blocked on open decision 2.
3. **Compose the roles.** Normalize each role's token demand to coverage, then
   take `min(prefill, decode)` per `modelCoverageFromRoles`, with undefined
   contributions skipped rather than read as zero. Without this, items 1 and 2
   size each role correctly and the model is still sized wrong whenever one
   role binds. *Depends on:* the composite-analyzer work landing, or on
   agreeing which branch owns the normalization boundary.
4. **Retire the shape gate.** Keep `shape.Tracker` as a demand input and a
   refit trigger; remove `staleShape` from the hold and from the queue-release
   exclusion. Delete the idle-fleet false positive by resetting the tracker when
   the fleet has served nothing for longer than the window.
5. **Benchmark matrix.** The `I`-up cell is now measured; `O`-down and `I`-down
   remain unrun. Same scorecard.

## 8. Validation

Every item ships with a negative control, run against the parent commit:

- **Item 1:** run QF's own log is the control. The floor must order on the
  cycle that logged `heldWhy="shape-change"` with `replicasImplied=6.53`, and
  the fleet must reach ~10 decode replicas inside two cycles rather than 24.
- **Item 2:** QE's phase-2 window is the control: prefill at 439 queued with 3
  of 4 pods idle must become a nonzero `demand_prefill` and an order.
- **Item 3:** the existing test "holds a role whose shape has changed, however
  many readings it has" asserts the behaviour being removed. It must be
  rewritten, not deleted, to assert what replaces it — and the rewrite is the
  record of the decision.
- **End to end:** TTFT median against QE's 44,045 ms and QF's 77,506 ms on the
  same trace, started from a *cold* shape tracker both times. Run QE's 44 s was
  obtained with a warm tracker by accident and is not a fair baseline unless the
  comparison run is warm too.

## 9. Claims in this document, and their status

- Measured on runs QE/QF: every row of §5 marked measured, the §1 table, the
  `1,162,368`-token KV capacity, `max_num_seqs` 256 vs 166 fitting, preemption
  at 25.9/s.
- Read from the code at `7a504ead`: every formula in §3, the K2 priority chain,
  the hold labels, the queue-release condition.
- Read from the code at `7a504ead` for §4a: the throughput analyzer's
  `computeDemand`, `computeVariantSupply` and `computeLocalDemand`; its
  `variantState` holding a second `shape.Tracker` and `itl.Window`; the opposite
  shape-change clearing policies; and `cmd/main.go`'s conditional registration.
- Measured on both benchmark clusters: the analyzers list is
  `[{name: saturation, score: 1.0}]`, so **the throughput analyzer did not run
  in any of runs QB-QF**. Every number in §5 was produced with it off.
- Read from `deanscaler/composite-analyzer` and
  `deanscaler/single-analyzer-normalize` (not merged): `modelCoverageFromRoles`
  and its `min(prefill, decode) + both` rule, the undefined-vs-zero convention
  in `aggregation/undefined.go`, and `normalizeToCompositeUnits` normalizing
  demand to 1.0 at capacity. Quoted, not paraphrased, where the wording carries
  the rule. Those branches are moving; check them before building on this
  section.
- **Unresolved:** why `fleet-shape-change` re-fired at 06:33:56Z on
  `1000 -> 1000.0000000000001`, which `Shape.Within` at a 20% tolerance should
  have reported as unchanged. Either the logged `was` is not the anchor, or
  something moves the anchor between cycles. This matters for item 3 and is not
  yet explained.
- **Predicted, not measured:** that removing the `/O` division fixes the
  end-to-end TTFT. The mechanism is measured; the outcome is not.
