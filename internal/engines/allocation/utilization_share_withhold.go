package allocation

import (
	"maps"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
)

// WithholdPromised returns copies of constraints with the utilization-share
// optimizer's promised GPUs counted as used, for the consumers that must not
// take them: the scale-from-zero budget check and the warm pool's headroom
// (docs/proposals/utilization-share-optimizer.md, section 6.3). promised is
// scope ("" for the cluster group, else a namespace) -> accelerator -> GPUs.
//
// A promise in a namespace group is charged to that namespace's pool and to
// the cluster pool of its accelerator, since the receiver's pods will hold
// both. A cluster-group promise is charged to the cluster pool only. Pools
// with no finite limit are left alone. The inputs are not modified: the
// optimizer itself already counts promised GPUs as its receivers' and must
// read the constraints unchanged.
func WithholdPromised(constraints []*ResourceConstraints, promised map[string]map[string]int) []*ResourceConstraints {
	if len(promised) == 0 {
		return constraints
	}
	// The promise is keyed by the merged budget's spelling of the accelerator,
	// which need not be this provider's; match by identity. Reports whether a
	// finite pool was charged.
	charge := func(pools map[string]ResourcePool, acc string, g int) bool {
		key, ok := accelerator.FindKey(pools, acc)
		if !ok {
			return false
		}
		p := pools[key]
		if p.Limit < 0 {
			return false
		}
		p.Used += g
		pools[key] = p
		return true
	}
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil {
			out = append(out, nil)
			continue
		}
		cc := *c
		cc.Pools = maps.Clone(c.Pools)
		if c.NamespacePools != nil {
			cc.NamespacePools = make(map[string]map[string]ResourcePool, len(c.NamespacePools))
			for ns, perType := range c.NamespacePools {
				cc.NamespacePools[ns] = maps.Clone(perType)
			}
		}
		withheld := 0
		for scope, perType := range promised {
			for acc, g := range perType {
				if g <= 0 {
					continue
				}
				if scope != "" {
					if pools, ok := cc.NamespacePools[scope]; ok {
						charge(pools, acc, g)
					}
				}
				if charge(cc.Pools, acc, g) {
					withheld += g
				}
			}
		}
		cc.TotalUsed += withheld
		cc.TotalAvail = max(0, cc.TotalAvail-withheld)
		out = append(out, &cc)
	}
	return out
}
