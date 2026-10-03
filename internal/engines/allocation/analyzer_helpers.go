package allocation

import (
	"context"
	"maps"
	"math"
	"slices"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// rolesOf returns the distinct roles among the given variants, sorted for
// determinism. A variant with no role is the synthetic RoleBoth.
func rolesOf(vcs []variantRecord) []string {
	set := make(map[string]struct{}, len(vcs))
	for _, vc := range vcs {
		r := vc.Role
		if r == "" {
			r = domain.RoleBoth
		}
		set[r] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}

// Sentinel VariantCapacity.Reason values that indicate a variant carries no
// usable capacity signal (see domain.VariantCapacity.Reason doc). Analyzers
// that skip a variant entirely on failure (e.g. throughput's ITL-model
// resolution) never emit these — the variant is simply absent from
// VariantCapacities, which ResultIsInformative also treats as uninformative.
//
// These are the single source of truth for the no-data/error sentinels:
// producer packages (e.g. saturation_v2) reference them rather than
// re-declaring the literals, so ResultIsInformative and the producers cannot
// drift apart.
const (
	// ReasonNoData marks a variant for which the analyzer had no usable input
	// (no live replicas and no store record).
	ReasonNoData = domain.ReasonNoData
	// ReasonError marks a variant whose capacity could not be resolved due to
	// an internal analyzer error.
	ReasonError = domain.ReasonError
)

// ResultIsInformative reports whether nr carries a usable capacity signal:
// a non-nil Result with at least one VariantCapacity whose Reason is not a
// no-data/error sentinel. Used by the engine to decide whether to refresh
// the analyzer's last-good-analysis timestamp for the liveness gate.
func ResultIsInformative(nr NamedAnalyzerResult) bool {
	if nr.Result == nil {
		return false
	}
	for _, vc := range nr.Result.VariantCapacities {
		if vc.Reason != ReasonNoData && vc.Reason != ReasonError {
			return true
		}
	}
	return false
}

// applyAllocation subtracts the capacity provided by n replicas of variant v
// from the entry's Remaining counter. Clamps to 0. Result.RequiredCapacity is
// never mutated.
//
// Contract: Remaining/Spare are engine-calibrated on entry (via the universal
// threshold post-step). Helpers do not read or mutate PendingReplicas.
func applyAllocation(e *NamedAnalyzerResult, v string, n int) {
	if e.Result == nil {
		return
	}
	prc := prcForVariant(e.Result, v)
	if prc <= 0 {
		return
	}
	e.Remaining -= float64(n) * prc
	if e.Remaining < 0 {
		e.Remaining = 0
	}
}

// prcForVariant returns the PerReplicaCapacity for variant v in result r.
// Returns 0 if the variant is not present.
func prcForVariant(r *domain.AnalyzerResult, v string) float64 {
	for _, vc := range r.VariantCapacities {
		if vc.VariantName == v {
			return vc.PerReplicaCapacity
		}
	}
	return 0
}

// =============================================================================
// Paired helpers — disaggregated (P/D) models
// =============================================================================

// releaseTargetUtilization is the utilisation a release aims to leave the fleet
// at: one QUARTER of the way up the hysteresis band, not the middle of it.
//
// It is derived, not configured -- scaleUpThreshold and scaleDownBoundary are
// already validated against each other (config enforces up > down), and a third
// knob whose only sane value is between them would be a knob nobody can set
// correctly.
//
// The quarter rather than the half, because the distance left between a released
// fleet and the scale-up threshold IS the anti-flap margin, and the midpoint
// spends too much of it. Worst case -- a release that lands exactly on the
// target -- leaves demand this much room before the replica is re-ordered:
//
//	boundary (old behaviour)  0.7000   +21.4 %
//	quarter  (this)           0.7375   +15.3 %
//	midpoint                  0.7750    +9.7 %
//
// Two couplings make a narrow margin worse than it looks. sticky.go's
// HoldPublishedScaleDown releases the hold precisely when demand at that count
// would reach the scale-up threshold, so the mechanism that protects a descent
// steps aside exactly at the line this target creates; and KEDA's HPA takes the
// MAX over a 300 s window on the way down, so one re-ordered replica in twenty
// cycles pins the fleet for the rest of the window and the release is worth
// nothing.
//
// The quarter costs nothing measurable: on run T's figures it releases the same
// single replica the midpoint did (budget 1,075,155 against a per-replica
// 930,508) and stops at the same eight.
//
// It falls back to scaleDownBoundary -- the previous behaviour exactly -- when
// either threshold is missing or inverted, so a malformed configuration loses
// the improvement rather than gaining a release it cannot justify.
func (e NamedAnalyzerResult) releaseTargetUtilization() float64 {
	if e.ScaleUpThreshold > 0 && e.ScaleDownBoundary > 0 &&
		e.ScaleUpThreshold > e.ScaleDownBoundary {
		return e.ScaleDownBoundary + (e.ScaleUpThreshold-e.ScaleDownBoundary)/4
	}
	return e.ScaleDownBoundary
}

// releasableFor is supply minus the supply the role would need to sit at the
// release target. Zero when the target is unusable or demand is unknown, which
// leaves the caller on RoleSpare and therefore on the old behaviour.
func releasableFor(supply, demand, target float64) float64 {
	if !(target > 0) || !(supply > 0) {
		return 0
	}
	// Written as !(v > 0) rather than v <= 0 on purpose: every comparison
	// against NaN is false, so v <= 0 ADMITS a NaN, and a NaN budget divided by
	// prc yields a NaN that int(math.Floor(NaN)) turns into a huge negative --
	// or, worse on another platform, a huge positive release.
	v := supply - demand/target
	if !(v > 0) {
		return 0
	}
	return v
}

// initRoleState initialises picker-local role state for one model's allocation pass.
// It unifies disaggregated and non-disaggregated models into one (model, role) view:
//
//   - Disaggregated (RoleCapacities != nil): roles = sorted keys of RoleCapacities;
//     per-role RC → pickerState[role]; per-role SC → e.RoleSpare[role].
//   - Non-disaggregated (RoleCapacities == nil): one synthetic role "both" using
//     the engine-calibrated model-level RC/SC (via the Remaining/Spare working
//     copies). No re-aggregation — the engine already summed all variants into
//     those scalars.
//
// Returns the list of active roles and the picker-local RolePairedState.
// Remaining/Spare scalars on NamedAnalyzerResult are read-only after this call;
// all dynamic bookkeeping moves to pickerState (scale-up) and RoleSpare (scale-down).
//
// # Role-visibility contract
//
// Roles are derived exclusively from the composite result's own RoleCapacities map keys.
// A role that exists in discovery but has no analyzer-attributed capacity entry is
// permanently invisible to scale-up until the analyzer starts emitting demand for it.
//
// This is NOT a complete gap: for disaggregated models, saturation's
// estimateSchedulerQueueDemand (internal/engines/analyzers/saturation_v2/analyzer.go)
// provides a purpose-built demand estimate for zero-replica roles from EPP queue-depth
// signals, producing a real nonzero RequiredCapacity that does trigger scale-up for that
// role before any replica of it exists — covering the ordinary cold-start case.
//
// The remaining, narrower gap has two cases:
//   (a) No EPP queue signal (SchedulerQueue == nil or empty): the estimator returns
//       all-zeros, and scale-up for the missing role cannot be triggered.
//   (b) The role is absent from VariantStates/VariantCapacities entirely (discovery-side
//       omission): neither the capacity store nor the queue estimator can help, since both
//       are iterated from the same variant list that is already missing the entry.
//
// Mixed-P/D+"both" models (disaggregated roles AND a "both" role simultaneously) are not
// supported today: initRoleState assigns a model to exactly one of the two branches above.
// This is a shallow implementation choice — no deep type constraint prevents lifting it —
// and is explicitly deferred as a future requirement.

func initRoleState(e *NamedAnalyzerResult) (roles []string, pickerState RolePairedState) {
	pickerState = make(RolePairedState)
	roleSet := make(map[string]struct{})

	if e.Result == nil {
		return nil, pickerState
	}
	if e.RoleCapacities != nil {
		// Disaggregated: per-role RC/SC from engine-calibrated RoleCapacities.
		if e.RoleSpare == nil {
			e.RoleSpare = make(map[string]float64, len(e.RoleCapacities))
		}
		if e.RoleReleasable == nil {
			e.RoleReleasable = make(map[string]float64, len(e.RoleCapacities))
		}
		target := e.releaseTargetUtilization()
		for role, rc := range e.RoleCapacities {
			pickerState[role] = rc.RequiredCapacity
			e.RoleSpare[role] = rc.SpareCapacity
			// Recorded only when the supply figure is real. An absent key means
			// "no release budget was computed", which safeRemovalReplicasForRole
			// reads as "use RoleSpare" -- the previous behaviour. A present zero
			// means "computed, and it is nothing", which it must honour.
			if rc.TotalSupply > 0 {
				e.RoleReleasable[role] = releasableFor(rc.TotalSupply, rc.TotalDemand, target)
			}
			roleSet[role] = struct{}{}
		}
	} else {
		// Non-disaggregated: synthesize a single "both" role from model-level scalars.
		pickerState[domain.RoleBoth] = e.Remaining
		if e.RoleSpare == nil {
			e.RoleSpare = make(map[string]float64, 1)
		}
		e.RoleSpare[domain.RoleBoth] = e.Spare
		if e.RoleReleasable == nil {
			e.RoleReleasable = make(map[string]float64, 1)
		}
		if e.Result != nil && e.TotalSupply > 0 {
			e.RoleReleasable[domain.RoleBoth] = releasableFor(
				e.TotalSupply, e.Result.TotalDemand, e.releaseTargetUtilization())
		}
		roleSet[domain.RoleBoth] = struct{}{}
	}

	roles = make([]string, 0, len(roleSet))
	for role := range roleSet {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles, pickerState
}

// =============================================================================
// Paired helpers — role-generic scale-up and scale-down
// =============================================================================
//
// Design § Architecture/D: (model, role) is the unit of allocation math.
// Per-role sizing is independent, scoped to each role's picker-local demand.
// The joint-commit step bounds by the min-util role (the coupling constraint).
//
// RolePairedState holds picker-local per-role demand tracked during one
// model's allocation pass. Maps role → remaining demand (in that role's own
// capacity units). Initialized from RoleCapacities[role].RC; decremented per
// joint commit. Lives only inside the allocation loop — not stored on
// NamedAnalyzerResult (per design A10).
type RolePairedState map[string]float64

// roleBottleneckReplicas returns ceil(state[role] / PRC[v]) for the single
// entry. Returns 0 if the entry has no result or PRC ≤ 0.
func roleBottleneckReplicas(e NamedAnalyzerResult, state RolePairedState, role, v string) int {
	if e.Result == nil {
		return 0
	}
	prc := prcForVariant(e.Result, v)
	if prc <= 0 {
		return 0
	}
	return int(math.Ceil(state[role] / prc))
}

// roleAggRemaining returns the remaining demand for role from the picker state.
func roleAggRemaining(state RolePairedState, role string) float64 {
	return state[role]
}

// anyRoleNeedsScaleUp is the per-role scale-up gate for the unified dispatcher.
// Returns true when any role has remaining demand > 0.
func anyRoleNeedsScaleUp(state RolePairedState, roles []string) bool {
	for _, role := range roles {
		if state[role] > 0 {
			return true
		}
	}
	return false
}

// variantsForRole returns the capacities whose role matches role exactly,
// canonicalizing an empty Role to domain.RoleBoth.
func variantsForRole(vcs []variantRecord, role string) []variantRecord {
	out := make([]variantRecord, 0, len(vcs))
	for _, vc := range vcs {
		r := vc.Role
		if r == "" {
			r = domain.RoleBoth
		}
		if r == role {
			out = append(out, vc)
		}
	}
	return out
}

// safeRemovalReplicasForRole returns the number of replicas of variant v that
// can safely be removed: floor(budget / PRC[v]), where budget is the LARGER of
// RoleSpare[role] and RoleReleasable[role] -- the boundary-measured budget and
// the release-target one. Returns 0 if the entry is not live, has no
// Result/RoleSpare, PRC <= 0, or the budget is not positive.
func safeRemovalReplicasForRole(e NamedAnalyzerResult, v, role string) int {
	if !e.Live {
		return 0 // non-live: no current basis to constrain removal
	}
	if e.Result == nil || e.RoleSpare == nil {
		return 0
	}
	prc := prcForVariant(e.Result, v)
	if prc <= 0 {
		return 0
	}
	// Measured against the release target, not the boundary. RoleSpare has
	// already decided that this role MAY release (needsScaleDownForRole); this
	// decides how many, and asking it against the bottom of the band is what
	// pinned run T at nine replicas it did not need.
	//
	// Falls back to RoleSpare when RoleReleasable was never populated, so an
	// optimizer path that does not call initRoleState behaves as before rather
	// than releasing nothing.
	//
	// The LARGER of the two, never the smaller. This change exists to widen a
	// release the boundary-measured budget was refusing, so it must only ever
	// ADD permission -- taking the release target unconditionally would shrink
	// the budget wherever RoleSpare was derived from something other than this
	// supply figure, and silently withdraw a scale-down that works today.
	//
	// Because it is a max, an absent entry and a present zero behave alike here.
	// The comma-ok is kept so that this reads the same way as the write side,
	// which does have to tell them apart -- see applyDeallocationForRole.
	budget := e.RoleSpare[role]
	if rel, ok := e.RoleReleasable[role]; ok && rel > budget {
		budget = rel
	}
	n := int(math.Floor(budget / prc))
	if n < 0 {
		return 0
	}
	return n
}

// applyDeallocationForRole charges n replicas of variant v against this role's
// budgets -- RoleSpare and, when one was computed, RoleReleasable -- by
// n x PRC[v]. Clamps both to 0. Never mutates Result.
func applyDeallocationForRole(e *NamedAnalyzerResult, v, role string, n int) {
	if e.Result == nil || e.RoleSpare == nil {
		return
	}
	prc := prcForVariant(e.Result, v)
	if prc <= 0 {
		return
	}
	e.RoleSpare[role] -= float64(n) * prc
	if e.RoleSpare[role] < 0 {
		e.RoleSpare[role] = 0
	}
	// Both budgets move together. RoleReleasable is what bounds the NEXT
	// variant of this role, so leaving it untouched would let a two-variant
	// role release the same capacity twice.
	//
	// Charged only where a budget EXISTS. `m[k] -= x` on an absent key creates
	// it at -x, clamped here to 0 -- which converts "no budget was computed for
	// this role" into "computed, and it is nothing", the one distinction the
	// read side documents. It is harmless while the read takes a max, and
	// becomes a role that releases nothing after its first variant the moment
	// that max is replaced.
	if cur, ok := e.RoleReleasable[role]; ok {
		cur -= float64(n) * prc
		if cur < 0 {
			cur = 0
		}
		e.RoleReleasable[role] = cur
	}
}

// needsScaleDownForRole reports whether the single live entry agrees this role
// has spare capacity. Returns false if the entry is not live, has no
// Result/RoleSpare, or RoleSpare[role] ≤ 0. Safety floor: a non-live entry
// means no current basis to scale down.
func needsScaleDownForRole(e NamedAnalyzerResult, role string) bool {
	if !e.Live {
		return false // non-live: no current basis to scale down
	}
	if e.Result == nil || e.RoleSpare == nil || e.RoleSpare[role] <= 0 {
		return false
	}
	return true
}

// roleAtCeiling reports whether every usable variant of role has reached its
// own MaxReplicas, so the role cannot grow again in this pass no matter what
// the cluster does.
//
// It exists because a RolePickFn reports both of its failures the same way --
// ("", 0) -- and allocateForModelPaired must tell them apart: a role held back
// by the GPU budget keeps its partner paired with it, while a role at its
// administrative ceiling must not block its partner at all. Checked
// independently of the picker rather than by extending RolePickFn, so both
// pickers get the distinction without either having to report it.
//
// A variant with no MaxReplicas, or one set to zero, is unbounded and means the
// role is not at a ceiling. A role with no usable variant is not "at a ceiling"
// either -- there is nothing to be at the ceiling of -- so it stays on the
// scarcity path, which ends the pass rather than spinning on it.
func roleAtCeiling(
	role string,
	variants []variantRecord,
	stateMap map[string]domain.VariantReplicaState,
	targets map[string]int,
) bool {
	usable := false
	for _, vc := range variantsForRole(variants, role) {
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		usable = true
		state := stateMap[vc.VariantName]
		if state.MaxReplicas == nil || *state.MaxReplicas <= 0 {
			return false
		}
		if targets[vc.VariantName] < *state.MaxReplicas {
			return false
		}
	}
	return usable
}

// RolePickFn is the role-generic optimizer variant selector for the unified
// allocateForModelPaired loop. Called once per role per iteration; returns the
// chosen variant and its resource cap. Returning ("", 0) signals no variant
// is available for this role.
type RolePickFn func(
	role string,
	variants []variantRecord,
	stateMap map[string]domain.VariantReplicaState,
	available map[string]int,
	targets map[string]int,
) (variant string, capN int)

// allocateForModelPaired is the Phase-3 role-generic scale-up loop.
// Handles any set of roles (including the arity-1 "both" single-role case).
// Per iteration: pick one variant per role, size independently, compute
// Δ_util = min_role util_role, trim to matched joint commit.
// Arity-1 (roles = ["both"]) reduces to plain per-variant allocation.
//
// The commit is joint so a P/D fleet keeps its ratio, and Δ_util is the minimum
// across roles so no role is over-ordered relative to its partner. But a role
// with no headroom left -- every variant of it at its own maxReplicas -- is
// finished, not blocking: it is dropped from the iteration, its unservable
// remainder is zeroed so the loop can terminate, and the remaining roles are
// committed without it. Treating it as blocking is what left prefill pinned at
// one replica through a 1119-deep queue while decode sat at its ceiling
// (docs/proposals/prefill-starved-by-exhausted-role.md).
func allocateForModelPaired(
	ctx context.Context,
	e *NamedAnalyzerResult,
	variants []variantRecord,
	stateMap map[string]domain.VariantReplicaState,
	available map[string]int,
	targets map[string]int,
	pick RolePickFn,
	pickerState RolePairedState,
	roles []string,
) {
	logger := ctrl.LoggerFrom(ctx)
	for anyRoleNeedsScaleUp(pickerState, roles) {
		variantByRole := make(map[string]string, len(roles))
		capByRole := make(map[string]int, len(roles))
		prcByRole := make(map[string]float64, len(roles))
		// picked is the roles this iteration can actually commit to, in the
		// caller's order.
		//
		// A pick can fail for two reasons, and they are not the same thing.
		//
		// SCARCITY -- the GPU budget could not fit a replica. The role would
		// grow if the cluster allowed it, so the pair is kept intact and the
		// pass ends: spending the last GPUs on one side of a P/D fleet whose
		// other side cannot follow buys no throughput. That is the behaviour
		// "should handle GPU exhaustion for one role without affecting the
		// other" pins down, and it is deliberate.
		//
		// CEILING -- every variant of the role is at its own maxReplicas. The
		// role is FINISHED, not blocking, and will never grow however long the
		// pass waits. Vetoing its partner is then permanent starvation, not
		// ratio-preservation. Measured: decode at 9/9 of max 9 left prefill
		// ordered 1 -> 1 in every cycle of a run while prefill held a queue of
		// 1119 and its demand floor asked for five replicas, with nine
		// replicas of prefill headroom unused.
		// See docs/proposals/prefill-starved-by-exhausted-role.md.
		picked := make([]string, 0, len(roles))
		allPicked := true
		for _, role := range roles {
			v, capN := pick(role, variants, stateMap, available, targets)
			if v == "" {
				if roleAtCeiling(role, variants, stateMap, targets) {
					// Drop its unservable remainder too, or
					// anyRoleNeedsScaleUp stays true on it forever and this
					// loop spins.
					pickerState[role] = 0
					continue
				}
				allPicked = false
				break
			}
			variantByRole[role] = v
			capByRole[role] = capN
			prcByRole[role] = prcFromVCs(variants, v)
			picked = append(picked, role)
		}
		if !allPicked || len(picked) == 0 {
			break
		}

		// Every loop below iterates `picked`, never `roles`. An unpicked role
		// has no entry in prcByRole, so its utilByRole would compute as 0 and
		// deltaUtil <= 0 would break the loop -- the same starvation by
		// another route.
		nByRole := make(map[string]int, len(picked))
		utilByRole := make(map[string]float64, len(picked))
		for _, role := range picked {
			prc := prcByRole[role]
			n := min(roleBottleneckReplicas(*e, pickerState, role, variantByRole[role]), capByRole[role])
			nByRole[role] = n
			demand := roleAggRemaining(pickerState, role)
			if demand <= 0 {
				utilByRole[role] = 1.0
			} else {
				utilByRole[role] = float64(n) * prc / demand
			}
		}

		deltaUtil := math.MaxFloat64
		for _, role := range picked {
			if utilByRole[role] < deltaUtil {
				deltaUtil = utilByRole[role]
			}
		}
		if deltaUtil <= 0 {
			break
		}

		kByRole := make(map[string]int, len(picked))
		anyPositive := false
		for _, role := range picked {
			demand := roleAggRemaining(pickerState, role)
			prc := prcByRole[role]
			n := nByRole[role]
			k := 0
			if prc > 0 && demand > 0 {
				k = max(int(math.Floor(deltaUtil*demand/prc)), min(1, n))
			}
			kByRole[role] = k
			if k > 0 {
				anyPositive = true
			}
		}
		if !anyPositive {
			break
		}

		for _, role := range picked {
			v := variantByRole[role]
			k := kByRole[role]
			prc := prcByRole[role]
			targets[v] += k
			pickerState[role] = math.Max(0, pickerState[role]-float64(k)*prc)
			if available != nil {
				available[accFromVCs(variants, v)] -= k * gpusPerReplicaFromState(stateMap, v)
			}
		}
		// Update model-level Remaining via the P-anchor role so fairShareValue
		// reflects committed capacity. For "both" (non-disaggregated) use the
		// single role; for P/D prefer "prefill".
		for _, anchor := range []string{"prefill", domain.RoleBoth} {
			if v, ok := variantByRole[anchor]; ok {
				applyAllocation(e, v, kByRole[anchor])
				break
			}
		}
		logger.V(logging.DEBUG).Info("scale-up: joint role commit", "deltaUtil", deltaUtil)
	}
}
