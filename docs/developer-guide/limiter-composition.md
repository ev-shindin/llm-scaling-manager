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
