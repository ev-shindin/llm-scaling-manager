package allocation

import "maps"

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
	charge := func(pools map[string]ResourcePool, acc string, g int) {
		if p, ok := pools[acc]; ok && p.Limit >= 0 {
			p.Used += g
			pools[acc] = p
		}
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
				if p, ok := cc.Pools[acc]; ok && p.Limit >= 0 {
					withheld += g
				}
				charge(cc.Pools, acc, g)
			}
		}
		cc.TotalUsed += withheld
		cc.TotalAvail = max(0, cc.TotalAvail-withheld)
		out = append(out, &cc)
	}
	return out
}
