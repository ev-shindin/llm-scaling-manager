package allocation

import (
	"cmp"
	"maps"
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// MaxShareBudgetGPUs bounds the budget a group is evaluated over. No cluster
// holds a million GPUs of one type, so a larger budget is a quota written as
// "effectively unlimited", and it is treated as unlimited: sharing it would
// mean placing replicas one at a time across a bound nothing enforces. It also
// keeps free + held clear of integer overflow.
const MaxShareBudgetGPUs = 1 << 20

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
	// Committed is what each role holds now.
	Committed map[string]int
	// Thresholds is each role's scale-up threshold k_r.
	Thresholds map[string]float64
	Origins    map[string]ShareRoleOrigin
	// Frozen lists the models of this group that were not planned this cycle,
	// as "namespace/model: why".
	Frozen []string
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
	// Weight resolves a request's model weight.
	Weight func(req ModelScalingRequest) float64
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

		roles, why := shareRolesForRequest(req, records, acc, opts.Weight)
		if why != "" {
			g.Frozen = append(g.Frozen, modelKey(req)+": "+why)
			continue
		}
		stateMap := buildStateMap(req.VariantStates)
		for _, r := range roles {
			o := ShareRoleOrigin{Namespace: req.Namespace, ModelID: req.ModelID, Role: r.role}
			g.Roles = append(g.Roles, r.ShareRole)
			g.Committed[r.Key] = roleCurrentGPUs(records, stateMap, acc, r.role)
			g.Thresholds[r.Key] = req.CompositeSignal.ScaleUpThreshold
			g.Origins[r.Key] = o
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
		held := 0
		for _, c := range g.Committed {
			held += c
		}
		if held > MaxShareBudgetGPUs-free {
			continue
		}
		g.Budget = free + held
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
	weight func(ModelScalingRequest) float64) ([]shareRoleInput, string) {
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
		w = weight(req)
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
