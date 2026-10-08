package allocation

import (
	"cmp"
	"maps"
	"math"
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// MaxShareBudgetGPUs bounds the budget a group is evaluated over. No cluster
// holds a million GPUs of one type, so a larger budget is a quota written as
// "effectively unlimited", and it is treated as unlimited: sharing it would
// mean placing replicas one at a time across a bound nothing enforces. It also
// keeps free + held clear of integer overflow.
const MaxShareBudgetGPUs = 1 << 20

// ShareVariant is the variant of a role a transfer acts on.
type ShareVariant struct {
	Name string
	// GPUs is one replica of it.
	GPUs int
	// Current is its replica count now.
	Current int
	// Min and Max are its replica bounds; Max 0 is unbounded.
	Min, Max int
	// PodGPUs is each pod's GPUs in one replica; nil when unknown.
	PodGPUs []int
}

// ShareCovers reports whether a donor replica's pods can fund a receiver
// replica's (§6.5): every receiver pod needs a donor pod of its own at least
// its size, since nothing shows that several smaller donor pods share a node.
// The donor's surplus returns to the free budget. With either shape unknown it
// falls back to the replicas' totals.
func ShareCovers(donor, receiver ShareVariant) bool {
	return decision.PodsCover(donor.PodGPUs, donor.GPUs, receiver.PodGPUs, receiver.GPUs)
}

func shareVariantOf(vc variantRecord, stateMap map[string]domain.VariantReplicaState) ShareVariant {
	st := stateMap[vc.VariantName]
	v := ShareVariant{Name: vc.VariantName, GPUs: gpusPerReplicaFromState(stateMap, vc.VariantName), Current: st.CurrentReplicas}
	if st.MinReplicas != nil {
		v.Min = *st.MinReplicas
	}
	if st.MaxReplicas != nil {
		v.Max = *st.MaxReplicas
	}
	v.PodGPUs = st.PodGPUs
	return v
}

// ShareRoleOrigin names the model and role a ShareRole was built from.
type ShareRoleOrigin struct {
	Namespace string
	ModelID   string
	Role      string
}

// ShareGroup is one closed budget group of the utilization-share optimizer: an
// accelerator type paired with a budget scope (proposal §2, §6.1).
type ShareGroup struct {
	AcceleratorType string
	// Scope is the namespace of a namespace-quota group, "" for the cluster group.
	Scope string
	// Budget is what the group plans over: the budget's free GPUs plus what its
	// planned roles hold now. Models that are not planned -- frozen, or spanning
	// several accelerator types -- hold GPUs that are simply not added back, so
	// they come out of the budget exactly once.
	Budget int
	Roles  []ShareRole
	// Committed is the GPUs each role holds now: scheduled pods, terminating
	// ones included (HeldReplicas), falling back to the replica count when the
	// pods could not be listed. The ledger adjusts it for transfers in flight.
	Committed map[string]int
	// Give and Grow name the variant of each role that gives a replica (the
	// most expensive with replicas above its floor) and the one that grows (the
	// cheapest with room below its ceiling), as rescale reclaims and fills.
	// A role absent from Give has nothing it can give.
	Give, Grow map[string]ShareVariant
	// Variants lists every variant of each planned role. Once the optimizer is
	// active it owns all their targets, not only the ones a transfer touches.
	Variants map[string][]ShareVariant
	// PhysicalFree bounds what a fill may place this cycle: the cluster's free
	// GPUs of the type, which a namespace quota can exceed (as in applyRescale).
	// math.MaxInt when the cluster pool is unbounded or unknown.
	PhysicalFree int
	// PoolCarve is the warm pools' unheld target taken out of Budget.
	PoolCarve int
	// Thresholds is each role's scale-up threshold k_r.
	Thresholds map[string]float64
	Origins    map[string]ShareRoleOrigin
	// Frozen lists the models of this group that were not planned this cycle,
	// as "namespace/model: why".
	Frozen []string

	listed int // the planned roles' GPUs by replica count, for the budget
}

// ShareGroupOptions controls which groups BuildShareGroups returns.
type ShareGroupOptions struct {
	// ClusterIsQuota reports whether the cluster-scope budget is a quota. When
	// it is not, it is the physical inventory, and the cluster group is built
	// only with PhysicalGroups.
	ClusterIsQuota bool
	PhysicalGroups bool
	// NamespaceEnabled reports whether a namespace-quota group takes part. Nil
	// means every namespace does.
	NamespaceEnabled func(namespace string) bool
	// Weight resolves a request's model weight. clusterScope is true when the
	// model is planned in the cluster group, where models of different
	// namespaces share one budget.
	Weight func(req ModelScalingRequest, clusterScope bool) float64
	// Exclude reports why a request must not be planned in its group, or "".
	// An excluded model is frozen: today's optimizer keeps it, and its GPUs
	// stay outside the group's budget.
	Exclude func(req ModelScalingRequest, clusterScope bool) string
	// ReserveGPUs are held out of every group's budget, so a scale-from-zero
	// wake finds them free in the quota without waiting for a transfer
	// (proposal section 7.2). Spending them leaves the group over its budget,
	// which the planner pays back first (SharePlan.Refills).
	ReserveGPUs int
	// PoolUnheld is, per namespace and accelerator, what the warm pools want
	// and do not hold. It is carved out of the budget of the group the
	// namespace plans in -- its own quota group where it has one, else the
	// cluster group -- so the pools can grow into GPUs the optimizer would
	// otherwise fill (proposal section 7.2).
	PoolUnheld map[string]map[string]int
}

// BuildShareGroups turns one cycle's requests and constraints into the groups
// the utilization-share optimizer evaluates. A model is planned when its
// composite signal is live and informative and all its variants share one
// accelerator type; otherwise it is frozen for the cycle (proposal §6.1 step 1).
//
// Committed is today's replica count, not yet the GPUs held including
// terminating pods: that count (VariantReplicaState.HeldReplicas) is a
// prerequisite of actuation, not of the shadow evaluation this serves.
func BuildShareGroups(requests []ModelScalingRequest, constraints []*ResourceConstraints, opts ShareGroupOptions) []ShareGroup {
	if len(constraints) == 0 {
		return nil
	}
	available := mergeConstraints(constraints)
	availableByNS := mergeNamespaceConstraints(constraints)

	type key struct{ acc, scope string }
	groups := make(map[key]*ShareGroup)
	group := func(acc, scope string) *ShareGroup {
		k := key{acc, scope}
		g, ok := groups[k]
		if !ok {
			g = &ShareGroup{
				AcceleratorType: acc,
				Scope:           scope,
				Committed:       map[string]int{},
				Give:            map[string]ShareVariant{},
				Grow:            map[string]ShareVariant{},
				Variants:        map[string][]ShareVariant{},
				Thresholds:      map[string]float64{},
				Origins:         map[string]ShareRoleOrigin{},
			}
			groups[k] = g
		}
		return g
	}

	for _, req := range requests {
		records := recordsForRequest(req)
		if records == nil {
			continue
		}
		acc, single := singleAccType(records)
		if !single {
			continue // multi-accelerator: a fixed consumer; its GPUs are already out of the free budget
		}
		_, nsScoped := availableByNS[req.Namespace]
		scope := ""
		if nsScoped {
			if opts.NamespaceEnabled != nil && !opts.NamespaceEnabled(req.Namespace) {
				continue
			}
			scope = req.Namespace
		} else if !opts.ClusterIsQuota && !opts.PhysicalGroups {
			continue
		}
		g := group(acc, scope)

		why := ""
		if opts.Exclude != nil {
			why = opts.Exclude(req, scope == "")
		}
		var roles []shareRoleInput
		if why == "" {
			roles, why = shareRolesForRequest(req, records, acc, opts.Weight, scope == "")
		}
		if why != "" {
			g.Frozen = append(g.Frozen, modelKey(req)+": "+why)
			continue
		}
		stateMap := buildStateMap(req.VariantStates)
		for _, r := range roles {
			o := ShareRoleOrigin{Namespace: req.Namespace, ModelID: req.ModelID, Role: r.role}
			g.Roles = append(g.Roles, r.ShareRole)
			g.Committed[r.Key] = roleHeldGPUs(records, stateMap, acc, r.role)
			g.listed += roleCurrentGPUs(records, stateMap, acc, r.role)
			g.Thresholds[r.Key] = req.CompositeSignal.ScaleUpThreshold
			g.Origins[r.Key] = o
			vs := variantsForRole(variantsOnType(records, acc), r.role)
			for _, vc := range vs {
				g.Variants[r.Key] = append(g.Variants[r.Key], shareVariantOf(vc, stateMap))
			}
			if give, ok := shareGiveVariant(vs, stateMap); ok {
				g.Give[r.Key] = give
			}
			if grow, ok := shareGrowVariant(vs, stateMap); ok {
				g.Grow[r.Key] = grow
			}
		}
	}

	out := make([]ShareGroup, 0, len(groups))
	for _, k := range slices.SortedFunc(maps.Keys(groups), func(a, b key) int {
		return cmp.Or(cmp.Compare(a.acc, b.acc), cmp.Compare(a.scope, b.scope))
	}) {
		g := groups[k]
		free, ok := available[k.acc]
		if k.scope != "" {
			free, ok = availableByNS[k.scope][k.acc]
		}
		if !ok || free < 0 || free > MaxShareBudgetGPUs {
			continue // unlimited, unknown, or no real bound: nothing to "use all of" (proposal §7.3)
		}
		// The limiter's free figure is net of what WVA's variants hold by
		// replica count, so the budget adds back that count -- not the held
		// figure, which also counts terminating pods. Those then show up as
		// committed and not idle, which is the point of counting them.
		if g.listed > MaxShareBudgetGPUs-free {
			continue
		}
		g.PoolCarve = poolCarve(opts.PoolUnheld, k.acc, k.scope, func(ns string) bool {
			_, own := groups[key{k.acc, ns}]
			return own
		})
		g.Budget = max(0, free+g.listed-opts.ReserveGPUs-g.PoolCarve)
		g.PhysicalFree = math.MaxInt
		if pf, ok := available[k.acc]; ok && pf >= 0 && pf < math.MaxInt {
			g.PhysicalFree = pf
		}
		slices.SortFunc(g.Roles, func(a, b ShareRole) int { return cmp.Compare(a.Key, b.Key) })
		slices.Sort(g.Frozen)
		out = append(out, *g)
	}
	return out
}

type shareRoleInput struct {
	ShareRole
	role string
}

// shareRolesForRequest builds one model's roles on acc, or says why the model
// is frozen this cycle. A P/D model is frozen whole when either role cannot be
// sized: equalizing one role against a guess about the other is the failure
// the freeze exists to prevent.
func shareRolesForRequest(req ModelScalingRequest, records []variantRecord, acc string,
	weight func(ModelScalingRequest, bool) float64, clusterScope bool) ([]shareRoleInput, string) {
	sig := req.CompositeSignal
	switch {
	case sig.Result == nil:
		return nil, "no analyzer result"
	case !sig.Live:
		return nil, "analyzer result not live"
	case !(sig.ScaleUpThreshold > 0):
		return nil, "no scale-up threshold"
	}
	w := 1.0
	if weight != nil {
		w = weight(req, clusterScope)
	}
	stateMap := buildStateMap(req.VariantStates)
	var out []shareRoleInput
	for _, role := range modelRolesOnType(records, acc) {
		demand := sig.Result.TotalDemand
		if role != domain.RoleBoth {
			rc, ok := sig.RoleCapacities[role]
			if !ok {
				return nil, "role " + role + " has no demand reading"
			}
			demand = rc.TotalDemand
		}
		vs := variantsForRole(variantsOnType(records, acc), role)
		best, g, measured := bestVariantForRole(records, stateMap, acc, role)
		need := 0.0
		if demand > 0 {
			if !measured {
				return nil, "role " + role + " has demand but no measured capacity"
			}
			// need = demand / scale-up threshold, in GPUs of the role's most
			// cost-efficient variant: the GPU form of RequiredCapacity.
			perGPU := best.PerReplicaCapacity / float64(g)
			need = demand / sig.ScaleUpThreshold / perGPU
		}
		if !measured && len(vs) > 0 {
			g = gpusPerReplicaFromState(stateMap, vs[0].VariantName)
		}
		floor := roleFloorGPUs(records, stateMap, acc, role)
		ceiling := roleCeilingGPUs(vs, stateMap)
		out = append(out, shareRoleInput{
			ShareRole: ShareRole{
				Key:         modelKey(req) + "/" + role,
				Weight:      w,
				Need:        need,
				Floor:       floor,
				Ceiling:     ceiling,
				ReplicaGPUs: max(g, 1),
			},
			role: role,
		})
	}
	return out, ""
}

// roleCeilingGPUs is the GPUs a role's maxReplicas allow, summed over its
// variants. Zero means unbounded: one variant without a ceiling lets the role
// grow without limit.
func roleCeilingGPUs(variants []variantRecord, stateMap map[string]domain.VariantReplicaState) int {
	ceiling := 0
	for _, vc := range variants {
		st := stateMap[vc.VariantName]
		if st.MaxReplicas == nil || *st.MaxReplicas <= 0 {
			return 0
		}
		ceiling += *st.MaxReplicas * gpusPerReplicaFromState(stateMap, vc.VariantName)
	}
	return ceiling
}

// roleHeldGPUs sums the GPUs a role's variants on accType hold: HeldReplicas
// where the pods were listed, CurrentReplicas where they could not be.
func roleHeldGPUs(records []variantRecord, stateMap map[string]domain.VariantReplicaState, accType, role string) int {
	total := 0
	for _, vc := range variantsForRole(variantsOnType(records, accType), role) {
		st := stateMap[vc.VariantName]
		n := st.CurrentReplicas
		if st.HeldKnown {
			n = st.HeldReplicas
		}
		total += n * gpusPerReplicaFromState(stateMap, vc.VariantName)
	}
	return total
}

// shareGiveVariant picks the variant a role gives a replica from: the least
// cost-efficient one that is above its floor.
func shareGiveVariant(vs []variantRecord, stateMap map[string]domain.VariantReplicaState) (ShareVariant, bool) {
	sorted := sortByCostEfficiencyAsc(vs)
	for i := len(sorted) - 1; i >= 0; i-- {
		st := stateMap[sorted[i].VariantName]
		floor := 0
		if st.MinReplicas != nil {
			floor = *st.MinReplicas
		}
		if st.CurrentReplicas > floor {
			return shareVariantOf(sorted[i], stateMap), true
		}
	}
	return ShareVariant{}, false
}

// shareGrowVariant picks the variant a role grows: the most cost-efficient one
// with room below its ceiling.
func shareGrowVariant(vs []variantRecord, stateMap map[string]domain.VariantReplicaState) (ShareVariant, bool) {
	for _, vc := range sortByCostEfficiencyAsc(vs) {
		st := stateMap[vc.VariantName]
		if st.MaxReplicas != nil && *st.MaxReplicas > 0 && st.CurrentReplicas >= *st.MaxReplicas {
			continue
		}
		return shareVariantOf(vc, stateMap), true
	}
	return ShareVariant{}, false
}

// poolCarve is a group's share of the warm pools' unheld target: a namespace
// group's own, or for the cluster group every namespace's that has no group of
// its own on the accelerator.
func poolCarve(unheld map[string]map[string]int, acc, scope string, ownGroup func(ns string) bool) int {
	if scope != "" {
		return max(0, unheld[scope][acc])
	}
	sum := 0
	for ns, byType := range unheld {
		if !ownGroup(ns) {
			sum += max(0, byType[acc])
		}
	}
	return sum
}
