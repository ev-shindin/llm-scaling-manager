package steadystate

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/enginerec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// buildComposite reduces namedResults (every analyzer that ran for the model)
// into the single allocation.NamedAnalyzerResult the optimizer consumes.
//
// Saturation's entry is the composite's identity and unit: its variants are the
// composite's variants, and demand stays in saturation's units (D_sat). The
// composite starts as a copy of saturation's result -- every field saturation
// set, so supply, holds, start times and queue loads reach the optimizer as
// they did when it consumed saturation alone -- and changes only what it
// decides: each role's demand, the model's, each variant's share of it, its
// per-replica capacity (PRC) where another analyzer decided it, its
// utilization, and its Reason, which names the decision path.
//
// Per variant, every eligible analyzer that defines a replica need N
// (allocation.TotalReplicas) contributes, and the composite takes the largest:
//   - one contributor: its N (DecisionSingle);
//   - several: the largest (DecisionAgree), so scale-up follows the analyzer
//     asking for most, and scale-down needs every one of them to agree;
//   - none: DecisionNoSignal, and saturation's demand is kept.
//
// Four rules hold a role whatever the needs say (compositeHolds,
// keepSaturationHolds), and each held role is logged with the rules that
// hold it:
//   - saturation is not listed and no variant of the role has a need: nothing
//     the policy lists can size it, so it neither grows nor shrinks;
//   - on P/D, another role is held that way: this one does not grow either;
//   - a listed analyzer other than saturation is inside its staleness grace
//     with no reading: it cannot ask for more this cycle, so the roles it
//     covers do not shrink until its grace runs out;
//   - saturation holds the role and another analyzer's demand replaced its
//     held figure: the role does not shrink.
//
// Each role is then priced in one analyzer's frame, whichever needs more
// replicas for the fleet the role runs (priceRole; one frame per listed
// analyzer, roleFrames). In another analyzer's frame the role's demand, in
// saturation's units, is the largest N x PRC_sat over the variants it sizes,
// defined when saturation sees the role as idle too, and each variant it sizes
// is priced at D / N so it needs exactly the N it asked. A scale-down is then
// capped so it is safe in every frame (boundAllSpare: the spare, the release
// bound; the share optimizer and rescale read the other frames as demand).
//
// eligibleAnalyzers is the analyzers the policy lists: namedResults less the
// observe-only slo analyzer, less saturation when the policy does not list
// it, plus an entry with no Result for each listed analyzer that returned
// nothing this cycle but is still inside its grace (the caller decides that
// once). Saturation remains the identity source either way.
//
// Every slice and map is copied, so the optimizer's bookkeeping on the
// composite cannot reach back into an analyzer's own result.
func buildComposite(
	ctx context.Context,
	namedResults, eligibleAnalyzers []allocation.NamedAnalyzerResult,
	scaleUp, scaleDown float64,
) allocation.NamedAnalyzerResult {
	composite, _ := composeSignal(ctx, namedResults, eligibleAnalyzers, scaleUp, scaleDown)
	return composite
}

// composeSignal is buildComposite, and, for each role the composite holds,
// the reasons it holds it, in the order they were found (nil when nothing is
// held): what the "composite holds a role" line logs, and what a learner
// recording keeps beside the composite.
//
// It reads nothing but its arguments: a learner recording keeps exactly
// these, and a replay rebuilds the composite from them
// (engine_recording.go).
func composeSignal(
	ctx context.Context,
	namedResults, eligibleAnalyzers []allocation.NamedAnalyzerResult,
	scaleUp, scaleDown float64,
) (allocation.NamedAnalyzerResult, map[string][]string) {
	logger := ctrl.LoggerFrom(ctx)
	sat := findSaturation(namedResults)
	// No saturation entry: nothing to express demand in. Unreachable in
	// production, where runAnalyzers fails before building any entry
	// without one; every test builds its own slice.
	if sat == nil || sat.Result == nil {
		return emptyComposite(), nil
	}

	warnModelLevelOnly(ctx, *sat, eligibleAnalyzers)
	if len(eligibleAnalyzers) == 0 {
		logger.Info("no listed analyzer can size this model this cycle, so it holds where it is",
			"modelID", sat.Result.ModelID, "namespace", sat.Result.Namespace)
	}

	satDemand := make(map[string]float64)
	for _, role := range rolesPresent(sat.Result.VariantCapacities) {
		if d, ok := aggregation.DemandForRole(sat.Result, role); ok {
			satDemand[role] = d
		}
	}

	byRole := make(map[string][]variantNeed)
	contributors := make(map[string]bool)
	compositeVCs := make([]domain.VariantCapacity, 0, len(sat.Result.VariantCapacities))
	decisions := make([]allocation.Decision, 0, len(sat.Result.VariantCapacities))
	for _, source := range sat.Result.VariantCapacities {
		vc := source
		vc.PendingAges = slices.Clone(source.PendingAges)
		// Saturation's own service-rate signal, published from its entry; the
		// composite does not price on it.
		vc.ServiceRate = nil

		d := allocation.ResolveSO(eligibleAnalyzers, sat, vc.VariantName)
		vc.Reason = string(d.Path)
		for _, name := range d.Contributors {
			contributors[name] = true
		}
		role := domain.RoleOfVC(vc)
		if _, ok := satDemand[role]; ok {
			byRole[role] = append(byRole[role], newVariantNeed(len(compositeVCs), vc, d))
		}

		logger.V(logging.VERBOSE).Info("composite-contributors",
			"modelID", sat.Result.ModelID, "namespace", sat.Result.Namespace,
			"variant", vc.VariantName, "decisionPath", d.Path,
			"contributors", d.Contributors, "totalReplicas", d.N)
		compositeVCs = append(compositeVCs, vc)
		decisions = append(decisions, d)
	}

	result := *sat.Result
	result.AnalyzerName = allocation.CompositeSignalName
	result.VariantCapacities = compositeVCs
	result.RoleDemand = maps.Clone(sat.Result.RoleDemand)
	result.HeldRoleDemand = maps.Clone(sat.Result.HeldRoleDemand)
	result.HotRankDemand = maps.Clone(sat.Result.HotRankDemand)
	result.RoleStartSeconds = maps.Clone(sat.Result.RoleStartSeconds)
	result.VariantBusiestQueueLoad = maps.Clone(sat.Result.VariantBusiestQueueLoad)
	roles := rolesPresent(sat.Result.VariantCapacities)
	sized := make(map[string]bool, len(byRole))
	pricings := make([]rolePricing, 0, len(byRole))
	for _, role := range roles {
		vs, ok := byRole[role]
		if !ok {
			continue
		}
		p, s, dropped := priceRole(&result, role, satDemand[role], vs)
		sized[role] = s
		pricings = append(pricings, p)
		logPricing(ctx, sat.Result, p)
		for _, i := range dropped {
			logger.Info("a listed analyzer's need is dropped: saturation has no per-replica capacity for the variant",
				"modelID", sat.Result.ModelID, "namespace", sat.Result.Namespace,
				"variant", result.VariantCapacities[i].VariantName, "totalReplicas", decisions[i].Others)
		}
	}
	holds := compositeHolds(*sat, eligibleAnalyzers, roles, sized)
	keepSaturationHolds(&holds, *sat, &result, eligibleAnalyzers)
	composite := allocation.NamedAnalyzerResult{
		Name:              allocation.CompositeSignalName,
		Result:            &result,
		Score:             maxScore(*sat, eligibleAnalyzers, contributors),
		ScaleUpThreshold:  scaleUp,
		ScaleDownBoundary: scaleDown,
		Live:              len(contributors) > 0 || sat.Live,
		Holds:             holds.roleHolds(),
	}
	buildCapacities(ctx, &composite, nil, scaleUp, scaleDown)
	boundAllSpare(&composite, pricings, scaleDown)
	applyHolds(&composite, roles)
	for _, role := range slices.Sorted(maps.Keys(composite.Holds)) {
		h := composite.Holds[role]
		logger.Info("composite holds a role",
			"modelID", sat.Result.ModelID, "namespace", sat.Result.Namespace, "role", role,
			"noScaleUp", h.NoScaleUp, "noScaleDown", h.NoScaleDown, "reason", holds.reasons[role])
	}
	composite.Remaining = composite.RequiredCapacity
	composite.Spare = composite.SpareCapacity
	if len(composite.Holds) == 0 {
		return composite, nil
	}
	return composite, holds.reasons
}

// warnModelLevelOnly logs each listed analyzer that cannot size a P/D model:
// one that reports model-level demand only (no RoleDemand) defines no need for
// any prefill or decode variant, so on P/D it is listed and silently has no
// effect.
func warnModelLevelOnly(ctx context.Context, sat allocation.NamedAnalyzerResult, eligible []allocation.NamedAnalyzerResult) {
	if len(sat.Result.RoleDemand) == 0 {
		return
	}
	for _, e := range eligible {
		if e.Name != sat.Name && allocation.Eligible(e) && len(e.Result.RoleDemand) == 0 {
			ctrl.LoggerFrom(ctx).Info("listed analyzer reports no per-role demand, so it cannot size this P/D model",
				"modelID", sat.Result.ModelID, "namespace", sat.Result.Namespace, "analyzer", e.Name)
		}
	}
}

// variantNeed is one variant's need as each analyzer reads it: saturation's,
// and each other listed analyzer's.
type variantNeed struct {
	index    int
	sat      float64
	satOK    bool
	others   map[string]float64
	replicas float64
	// prc is saturation's per-replica capacity for the variant.
	prc float64
}

// newVariantNeed reads one variant's decision. A need whose demand at
// saturation's PRC overflows is no reading.
func newVariantNeed(i int, vc domain.VariantCapacity, d allocation.Decision) variantNeed {
	v := variantNeed{
		index: i, sat: d.SaturationN, satOK: d.Saturation,
		replicas: float64(vc.ReplicaCount), prc: vc.PerReplicaCapacity,
	}
	for _, n := range d.Needs {
		if !(v.prc > 0) || !math.IsInf(n.N*v.prc, 0) {
			if v.others == nil {
				v.others = make(map[string]float64, len(d.Needs))
			}
			v.others[n.Analyzer] = n.N
		}
	}
	return v
}

// pricingFrame is one analyzer's view of a role: the role's demand in
// saturation's units, and each variant's need. Saturation's frame is its own
// demand and needs; another analyzer's demand is the largest N x PRC_sat over
// the variants it sizes, and a variant it does not size is read at
// saturation's ratio to those it does (D / PRC_sat).
type pricingFrame struct {
	analyzer string // "" for saturation
	demand   float64
	need     func(variantNeed) (float64, bool)
}

// prc is the per-replica capacity frame f gives variant v: D / N where it
// sizes v, saturation's PRC otherwise.
func (f pricingFrame) prc(v variantNeed) float64 {
	if f.analyzer == "" {
		return v.prc
	}
	if n, ok := v.others[f.analyzer]; ok && n > 0 && f.demand > 0 {
		return f.demand / n
	}
	return v.prc
}

// roleFrames returns the frames that size a role with variants vs and
// saturation demand dSat: saturation's when it contributed, then one per other
// analyzer that sizes a variant saturation can price, in name order.
func roleFrames(dSat float64, vs []variantNeed) []pricingFrame {
	var frames []pricingFrame
	if slices.ContainsFunc(vs, func(v variantNeed) bool { return v.satOK }) {
		frames = append(frames, pricingFrame{demand: dSat,
			need: func(v variantNeed) (float64, bool) { return v.sat, v.satOK }})
	}
	demand := make(map[string]float64)
	for _, v := range vs {
		for a, n := range v.others {
			if cur, ok := demand[a]; v.prc > 0 && (!ok || n*v.prc > cur) {
				demand[a] = n * v.prc
			}
		}
	}
	for _, a := range slices.Sorted(maps.Keys(demand)) {
		target := demand[a]
		frames = append(frames, pricingFrame{analyzer: a, demand: target,
			need: func(v variantNeed) (float64, bool) {
				if n, ok := v.others[a]; ok {
					return n, true
				}
				if v.prc > 0 {
					return target / v.prc, true
				}
				return 0, false
			}})
	}
	return frames
}

// coverage is how much of frame f's need the role's running replicas cover:
// the sum over its variants of w / N, with w the variant's replicas (1 each
// when none of them has replicas, so a role scaling from zero compares the
// needs themselves). A variant f has no need for is left out; a zero need is
// covered however few replicas there are. The lower the coverage, the larger
// the need.
func coverage(vs []variantNeed, f pricingFrame) float64 {
	weighted := slices.ContainsFunc(vs, func(v variantNeed) bool { return v.replicas > 0 })
	var c float64
	for _, v := range vs {
		n, ok := f.need(v)
		w := 1.0
		if weighted {
			w = v.replicas
		}
		if !ok || w == 0 {
			continue
		}
		c += w / n
	}
	return c
}

// rolePricing is how priceRole priced one role: every frame that sizes it and
// the one chosen, for the scale-down bound (boundAllSpare).
type rolePricing struct {
	role   string
	vs     []variantNeed
	frames []pricingFrame
	chosen int
}

// priceRole prices one role of result, whose saturation demand is dSat and
// whose variants' needs are vs, and reports whether a listed analyzer sized
// it. dropped is the index of each variant another analyzer sized but the
// chosen frame cannot carry: saturation has no PRC for it and the chosen
// frame does not size it.
//
// The role is priced in one analyzer's frame (roleFrames): whichever needs
// more replicas for the fleet the role runs, the lowest coverage, saturation's
// on a tie so it stays exact. Mixing frames per variant asked for more than
// any analyzer, and taking the largest need among the others per variant let
// one analyzer's zero need on a variant veto another's.
//
//   - Saturation's frame: saturation's demand and PRCs stand. Its
//     HeldRoleDemand (its demand before its own hold) is raised to the most
//     demanding other analyzer's need in its units, D_sat x coverage_sat /
//     coverage_other, and dropped when that reaches D_sat: the hold no longer
//     sets the demand, and kept, the share optimizer would give nothing from
//     the role.
//   - Another analyzer's frame: the role's demand is that frame's, defined
//     when saturation sees the role as idle, so another analyzer can grow it.
//     Each variant it sizes is priced at D / N, so it needs exactly the N it
//     asked, and variants on different accelerators keep its view of their
//     relative worth; a variant it does not size keeps saturation's PRC. The
//     bridge PRC of a variant it prices (WarmPoolPerReplicaCapacity) moves by
//     the same ratio, so warm-pool supply stays in the unit of its pressure.
//     The model's TotalDemand moves with the role, the role's variants share
//     it in saturation's proportions, and HeldRoleDemand is dropped.
//
// Scale-up follows the chosen frame alone: when the analyzers rate a role's
// accelerators differently, a variant the chosen frame grows may leave another
// frame short for a cycle, until the next cycle reads the new replicas.
// Scale-down is bounded by every frame (boundAllSpare).
func priceRole(result *domain.AnalyzerResult, role string, dSat float64, vs []variantNeed) (
	pricing rolePricing, sized bool, dropped []int) {
	pricing = rolePricing{role: role, vs: vs, frames: roleFrames(dSat, vs), chosen: -1}
	best := math.Inf(1)
	for i, f := range pricing.frames {
		if c := coverage(vs, f); pricing.chosen < 0 || c < best {
			pricing.chosen, best = i, c
		}
	}
	chosen := pricingFrame{}
	if pricing.chosen >= 0 {
		chosen = pricing.frames[pricing.chosen]
	}
	for _, v := range vs {
		if len(v.others) > 0 && !(v.prc > 0) {
			if _, ok := v.others[chosen.analyzer]; chosen.analyzer == "" || !ok {
				dropped = append(dropped, v.index)
			}
		}
	}
	if pricing.chosen < 0 {
		return pricing, false, dropped
	}

	if chosen.analyzer == "" {
		if before, held := result.HeldRoleDemand[role]; held && len(pricing.frames) > 1 {
			cs, co := coverage(vs, chosen), math.Inf(1)
			for _, f := range pricing.frames[1:] {
				co = min(co, coverage(vs, f))
			}
			equiv := dSat * cs / co
			switch {
			case equiv >= dSat:
				delete(result.HeldRoleDemand, role)
			case equiv > before:
				result.HeldRoleDemand[role] = equiv
			}
		}
		return pricing, true, dropped
	}

	target := chosen.demand
	delete(result.HeldRoleDemand, role)
	delete(result.HotRankDemand, role)
	if result.RoleDemand == nil {
		result.TotalDemand = target
	} else {
		result.RoleDemand[role] = target
		result.TotalDemand += target - dSat
	}
	redistributeVariantDemand(result.VariantCapacities, role, target)
	for _, v := range vs {
		n, ok := v.others[chosen.analyzer]
		if !ok || !(n > 0) || !(target > 0) {
			continue
		}
		vc := &result.VariantCapacities[v.index]
		// A bridge is priced in the same frame as the replicas it stands in
		// for, so the warm pool's supply (WarmPoolCapacity) and pressure
		// read one unit. Without saturation's PRC there is no ratio to
		// convert by, and the bridge keeps saturation's reading. The ratio
		// is the variant's own: for a bridge of another shape (a pool Pod
		// configured differently) that the other analyzer would rate
		// differently, it is an approximation, not a measurement.
		if v.prc > 0 {
			vc.WarmPoolPerReplicaCapacity *= target / n / v.prc
		}
		vc.PerReplicaCapacity = target / n
		vc.Utilization = 0
		if vc.ReplicaCount > 0 {
			vc.Utilization = vc.TotalDemand / (float64(vc.ReplicaCount) * vc.PerReplicaCapacity)
		}
	}
	return pricing, true, dropped
}

// otherFrames reads role p in every frame that sizes it other than the one
// it is priced in: shedding X of the chosen frame's capacity sheds at most
// X x r of frame f's, r the largest PRC_f / PRC_chosen over the variants
// running, so f bounds the release at its own figure over r. ok is false
// when no other frame sizes the role.
func otherFrames(p rolePricing, result *domain.AnalyzerResult) (allocation.OtherFrames, bool) {
	var out allocation.OtherFrames
	if p.chosen < 0 || len(p.frames) < 2 {
		return out, false
	}
	for _, v := range p.vs {
		if v.replicas > 0 {
			out.Supply += v.replicas * result.VariantCapacities[v.index].PerReplicaCapacity
		}
	}
	for i, f := range p.frames {
		if i == p.chosen {
			continue
		}
		var r allocation.FrameReading
		for _, v := range p.vs {
			if v.replicas == 0 {
				continue
			}
			pf := f.prc(v)
			if !(pf > 0) {
				continue
			}
			r.Supply += v.replicas * pf
			if pc := result.VariantCapacities[v.index].PerReplicaCapacity; pc > 0 {
				r.Ratio = max(r.Ratio, pf/pc)
			}
		}
		if r.Ratio > 0 {
			r.Demand = f.demand
			out.Frames = append(out.Frames, r)
		}
	}
	return out, len(out.Frames) > 0
}

// boundAllSpare bounds each priced role's release by every frame that sizes
// it, not only the one it is priced in: the optimizer releases in the chosen
// frame's units, and where analyzers rate a role's accelerators differently,
// the replicas it sheds can be worth far more to another. The role's own
// spare on P/D, the model's on an aggregated model (no role capacities), is
// capped at the other frames' Releasable at the scale-down boundary;
// ReleaseBound, at the release target, caps the releasable capacity the
// optimizers also shed on; and OtherFrames is published for the share
// optimizer and rescale, which size from demand at their own utilization
// (NamedAnalyzerResult.OtherFrameDemand). On P/D the model-level spare is
// capped at the roles' sum when a bound lowered one.
func boundAllSpare(nr *allocation.NamedAnalyzerResult, pricings []rolePricing, scaleDown float64) {
	target := nr.ReleaseTargetUtilization()
	publish := func(role string, o allocation.OtherFrames) {
		if nr.ReleaseBound == nil {
			nr.ReleaseBound = make(map[string]float64)
			nr.OtherFrames = make(map[string]allocation.OtherFrames)
		}
		nr.ReleaseBound[role] = o.Releasable(target)
		nr.OtherFrames[role] = o
	}
	if len(nr.RoleCapacities) == 0 {
		for _, p := range pricings {
			if p.role != domain.RoleBoth {
				continue
			}
			if o, ok := otherFrames(p, nr.Result); ok {
				nr.SpareCapacity = min(nr.SpareCapacity, o.Releasable(scaleDown))
				publish(p.role, o)
			}
		}
		return
	}
	lowered := false
	for _, p := range pricings {
		rc, ok := nr.RoleCapacities[p.role]
		if !ok {
			continue
		}
		o, ok := otherFrames(p, nr.Result)
		if !ok {
			continue
		}
		publish(p.role, o)
		if sb := o.Releasable(scaleDown); sb < rc.SpareCapacity {
			rc.SpareCapacity, lowered = sb, true
			nr.RoleCapacities[p.role] = rc
		}
	}
	if lowered {
		// In role order: a float sum taken in a map's random order differs in
		// its last bit from one cycle, or one replay, to the next.
		var sum float64
		for _, role := range slices.Sorted(maps.Keys(nr.RoleCapacities)) {
			sum += nr.RoleCapacities[role].SpareCapacity
		}
		nr.SpareCapacity = min(nr.SpareCapacity, sum)
	}
}

// logPricing logs, at VERBOSE, the frame each role is priced in, every
// frame's coverage and demand: a role sized on another analyzer's frame can
// differ from every variant's composite-contributors line.
func logPricing(ctx context.Context, sat *domain.AnalyzerResult, p rolePricing) {
	logger := ctrl.LoggerFrom(ctx).V(logging.VERBOSE)
	if !logger.Enabled() || p.chosen < 0 {
		return
	}
	name := func(f pricingFrame) string {
		if f.analyzer == "" {
			return domain.SaturationAnalyzerName
		}
		return f.analyzer
	}
	frames := make([]string, 0, len(p.frames))
	for _, f := range p.frames {
		frames = append(frames, fmt.Sprintf("%s:coverage=%g,demand=%g", name(f), coverage(p.vs, f), f.demand))
	}
	logger.Info("composite-pricing",
		"modelID", sat.ModelID, "namespace", sat.Namespace, "role", p.role,
		"frame", name(p.frames[p.chosen]), "frames", frames)
}

// redistributeVariantDemand sets the TotalDemand of role's variants in vcs to
// sum to demand, in the proportions they already hold (evenly when they hold
// none), and recomputes each one's utilization from it.
func redistributeVariantDemand(vcs []domain.VariantCapacity, role string, demand float64) {
	var sum float64
	var count int
	for _, vc := range vcs {
		if domain.RoleOfVC(vc) == role {
			sum += vc.TotalDemand
			count++
		}
	}
	for i := range vcs {
		vc := &vcs[i]
		if domain.RoleOfVC(*vc) != role {
			continue
		}
		if sum > 0 {
			vc.TotalDemand = demand * vc.TotalDemand / sum
		} else {
			vc.TotalDemand = demand / float64(count)
		}
		vc.Utilization = 0
		if vc.ReplicaCount > 0 && vc.PerReplicaCapacity > 0 {
			vc.Utilization = vc.TotalDemand / (float64(vc.ReplicaCount) * vc.PerReplicaCapacity)
		}
	}
}

// Why the composite holds a role, logged as the reason of each
// "composite holds a role" line. A role can be held for more than one.
// Defined in enginerec, which a learner recording's export reads them from.
const (
	holdUnsized          = enginerec.HoldUnsized
	holdOtherRoleUnsized = enginerec.HoldOtherRoleUnsized
	holdAnalyzerInGrace  = enginerec.HoldAnalyzerInGrace
	holdSaturationHolds  = enginerec.HoldSaturationHolds
)

// roleHoldSet is what the composite forbids each role this cycle, and why.
type roleHoldSet struct {
	holds   map[string]allocation.RoleHold
	reasons map[string][]string
}

// add forbids role scale-up when up, scale-down when down, for reason.
func (s *roleHoldSet) add(role string, up, down bool, reason string) {
	if s.holds == nil {
		s.holds = make(map[string]allocation.RoleHold)
		s.reasons = make(map[string][]string)
	}
	h := s.holds[role]
	h.NoScaleUp = h.NoScaleUp || up
	h.NoScaleDown = h.NoScaleDown || down
	s.holds[role] = h
	if !slices.Contains(s.reasons[role], reason) {
		s.reasons[role] = append(s.reasons[role], reason)
	}
}

// roleHolds is the set as allocation.NamedAnalyzerResult.Holds: nil when
// nothing is held.
func (s roleHoldSet) roleHolds() map[string]allocation.RoleHold {
	if len(s.holds) == 0 {
		return nil
	}
	return s.holds
}

// compositeHolds returns what each role is forbidden this cycle, and why.
//
// Saturation unlisted: a role none of whose variants a listed analyzer could
// size is held both ways. Saturation still supplies the units and the
// variants, but sizing the role on it would be sizing on an analyzer the
// policy left out.
//
// A listed analyzer other than saturation that is live (inside its staleness
// grace) but has no reading this cycle blocks scale-down of the roles it
// reports variants for, or of every role when it reports none. Without this,
// one missed scrape from the analyzer asking for most replicas let the
// others' smaller need shrink the fleet for a cycle. Saturation's own grace
// is left as it was before the composite existed.
func compositeHolds(sat allocation.NamedAnalyzerResult, eligible []allocation.NamedAnalyzerResult,
	roles []string, sized map[string]bool) roleHoldSet {
	var holds roleHoldSet
	satListed := slices.ContainsFunc(eligible, func(e allocation.NamedAnalyzerResult) bool { return e.Name == sat.Name })
	if !satListed {
		var unsized []string
		for _, role := range roles {
			if !sized[role] {
				unsized = append(unsized, role)
				holds.add(role, true, true, holdUnsized)
			}
		}
		// On P/D a role that cannot grow stops the other growing too: decode
		// grown against a frozen prefill is capacity prefill cannot feed. The
		// share optimizer freezes such a model whole for the same reason.
		if len(unsized) > 0 && len(roles) > 1 {
			for _, role := range roles {
				if !slices.Contains(unsized, role) {
					holds.add(role, true, false, holdOtherRoleUnsized)
				}
			}
		}
	}
	satRole := make(map[string]string, len(sat.Result.VariantCapacities))
	for _, vc := range sat.Result.VariantCapacities {
		satRole[vc.VariantName] = domain.RoleOfVC(vc)
	}
	for _, e := range eligible {
		if e.Name == sat.Name || !e.Live || allocation.Eligible(e) {
			continue
		}
		covered := make(map[string]bool)
		if e.Result != nil {
			for _, vc := range e.Result.VariantCapacities {
				if role, ok := satRole[vc.VariantName]; ok {
					covered[role] = true
				}
			}
		}
		for _, role := range roles {
			if len(covered) == 0 || covered[role] {
				holds.add(role, false, true, holdAnalyzerInGrace+e.Name)
			}
		}
	}
	return holds
}

// keepSaturationHolds holds against scale-down every role saturation, when
// listed, holds (HeldRoleDemand) but whose held figure the pricing dropped:
// another analyzer's demand replaced it, or the role is priced in another
// frame. Read as demand at the scale-up threshold the held figure,
// scaleDown x supply, released 1 - scaleDown/scaleUp of the role, so an
// analyzer asking for more than saturation made a role saturation holds
// shed: 10 prefill GPUs went to 5 under contention. Scale-down needs every
// analyzer to agree, and saturation's hold is its refusal; growth is
// unaffected. Saturation alone never drops its hold, so it decides as before.
// A saturation that is listed but not eligible this cycle (stale, or no
// reading) blocks nothing, as allocation.Eligible promises. composite is the
// composite's result after pricing.
func keepSaturationHolds(holds *roleHoldSet, sat allocation.NamedAnalyzerResult, composite *domain.AnalyzerResult,
	eligible []allocation.NamedAnalyzerResult) {
	if !slices.ContainsFunc(eligible, func(e allocation.NamedAnalyzerResult) bool {
		return e.Name == sat.Name && allocation.Eligible(e)
	}) {
		return
	}
	for _, role := range slices.Sorted(maps.Keys(sat.Result.HeldRoleDemand)) {
		if _, kept := composite.HeldRoleDemand[role]; kept {
			continue
		}
		holds.add(role, false, true, holdSaturationHolds)
	}
}

// applyHolds zeroes the signal each hold forbids: a role's RequiredCapacity
// for NoScaleUp, its SpareCapacity for NoScaleDown, and the model-level one
// when every role in roles is held that way. An aggregated model has one
// role, domain.RoleBoth, and only the model-level signals.
func applyHolds(nr *allocation.NamedAnalyzerResult, roles []string) {
	if len(nr.Holds) == 0 {
		return
	}
	allUp, allDown := true, true
	for _, role := range roles {
		h := nr.Holds[role]
		allUp = allUp && h.NoScaleUp
		allDown = allDown && h.NoScaleDown
		rc, ok := nr.RoleCapacities[role]
		if !ok {
			continue
		}
		if h.NoScaleUp {
			rc.RequiredCapacity = 0
		}
		if h.NoScaleDown {
			rc.SpareCapacity = 0
		}
		nr.RoleCapacities[role] = rc
	}
	if allUp {
		nr.RequiredCapacity = 0
	}
	if allDown {
		nr.SpareCapacity = 0
	}
}

// findSaturation returns the saturation entry from namedResults, or nil.
func findSaturation(namedResults []allocation.NamedAnalyzerResult) *allocation.NamedAnalyzerResult {
	for i := range namedResults {
		if namedResults[i].Name == domain.SaturationAnalyzerName {
			return &namedResults[i]
		}
	}
	return nil
}

// emptyComposite is the composite of a model with no saturation entry: a
// non-nil Result with no variants, so allocation.CompositeHasSignal reports
// false through its ordinary path.
func emptyComposite() allocation.NamedAnalyzerResult {
	return allocation.NamedAnalyzerResult{
		Name:   allocation.CompositeSignalName,
		Result: &domain.AnalyzerResult{AnalyzerName: allocation.CompositeSignalName},
	}
}

// rolesPresent returns the distinct roles across vcs, in first-seen order.
func rolesPresent(vcs []domain.VariantCapacity) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, vc := range vcs {
		role := domain.RoleOfVC(vc)
		if _, ok := seen[role]; !ok {
			seen[role] = struct{}{}
			out = append(out, role)
		}
	}
	return out
}

// maxScore is the composite's Score: the largest score among the analyzers
// that contributed a need this cycle, or saturation's when none did. The
// cross-model fair share reads it (greedy_score_optimizer.go), so an analyzer
// that sized nothing cannot raise it.
func maxScore(sat allocation.NamedAnalyzerResult, eligible []allocation.NamedAnalyzerResult, contributors map[string]bool) float64 {
	best, found := 0.0, false
	for _, e := range eligible {
		if contributors[e.Name] && (!found || e.Score > best) {
			best, found = e.Score, true
		}
	}
	if !found {
		return sat.Score
	}
	return best
}
