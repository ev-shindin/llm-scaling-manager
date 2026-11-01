package allocation

import (
	"math/bits"
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// podSearchMaxUnits bounds the exact pod search: a model with more than one
// replica size grows or sheds at most this many units a cycle, where a unit is the
// greatest common divisor of the pod sizes searched (4 GPUs for 4- and 8-GPU
// pods). Clamping a bound to a constant keeps the result monotone in the
// bound; the rest is taken on a later cycle.
//
// Worst case per model and cycle: every set is at most podSearchMaxUnits+1
// bits (65 words). Each variant's reachable set is built by binary-split
// shifts, at most 13 per variant; composing a role's pods keeps one suffix set
// per variant; and the joint search over roles shifts one set per reachable
// value of each role, at most 4097 x 65 word operations, about 0.27M, per role.
const podSearchMaxUnits = 4096

// podBits is a set of small non-negative integers: GPU sums, in units.
type podBits []uint64

// newPodBits returns an empty set over 0..n.
func newPodBits(n int) podBits { return make(podBits, n/64+1) }

func (b podBits) has(i int) bool {
	return i >= 0 && i/64 < len(b) && b[i/64]&(1<<(uint(i)%64)) != 0
}

func (b podBits) set(i int) { b[i/64] |= 1 << (uint(i) % 64) }

func (b podBits) unset(i int) { b[i/64] &^= 1 << (uint(i) % 64) }

// orShifted sets b |= src << k, dropping what falls past b's last word. b and
// src may be the same set: the words are written from the top down, so each
// read is of a word not yet written.
func (b podBits) orShifted(src podBits, k int) {
	w, s := k/64, uint(k%64)
	for i := len(b) - 1; i >= w; i-- {
		j := i - w
		v := src[j] << s
		if s != 0 && j > 0 {
			v |= src[j-1] >> (64 - s)
		}
		b[i] |= v
	}
}

// maxAtMost is the largest member of b no greater than n, or -1.
func (b podBits) maxAtMost(n int) int {
	if n < 0 {
		return -1
	}
	n = min(n, len(b)*64-1)
	for i := n / 64; i >= 0; i-- {
		w := b[i]
		if i == n/64 && n%64 != 63 {
			w &= (1 << (uint(n%64) + 1)) - 1
		}
		if w != 0 {
			return i*64 + 63 - bits.LeadingZeros64(w)
		}
	}
	return -1
}

// podItem is a variant the search may add or remove pods of: size units a
// pod, at most count pods.
type podItem struct {
	name  string
	size  int
	count int
}

// podSums is the set of sums over 0..bound that items' pods make.
func podSums(items []podItem, bound int) podBits {
	b := newPodBits(bound)
	b.set(0)
	for _, it := range items {
		left := min(it.count, bound/max(it.size, 1))
		for take := 1; left > 0; take *= 2 {
			n := min(take, left)
			b.orShifted(b, n*it.size)
			left -= n
		}
	}
	return b
}

// composePods splits amount, a member of podSums(items, amount), into pods
// per item: as many of each as the items after it can still complete, in
// items' order, so the first items are preferred as a greedy pass prefers
// them.
func composePods(items []podItem, amount int) []int {
	suffix := make([]podBits, len(items)+1)
	suffix[len(items)] = newPodBits(amount)
	suffix[len(items)].set(0)
	for i := len(items) - 1; i >= 0; i-- {
		suffix[i] = slices.Clone(suffix[i+1])
		left := min(items[i].count, amount/max(items[i].size, 1))
		for take := 1; left > 0; take *= 2 {
			n := min(take, left)
			suffix[i].orShifted(suffix[i], n*items[i].size)
			left -= n
		}
	}
	out := make([]int, len(items))
	rem := amount
	for i, it := range items {
		for n := min(it.count, rem/max(it.size, 1)); n >= 0; n-- {
			if suffix[i+1].has(rem - n*it.size) {
				out[i], rem = n, rem-n*it.size
				break
			}
		}
	}
	return out
}

// jointPods picks one member of each sets[i], at most caps[i], whose sum is
// the largest at most bound, and returns the picks. Among the picks that
// reach that sum, each role, from the last, takes the member nearest its
// ideal, the smaller on a tie. A larger bound or cap only adds choices, so the
// sum never falls as either rises.
func jointPods(sets []podBits, caps []int, bound int, ideal []int) []int {
	layers := make([]podBits, len(sets)+1)
	layers[0] = newPodBits(bound)
	layers[0].set(0)
	for i, s := range sets {
		layers[i+1] = newPodBits(bound)
		for a := s.maxAtMost(min(caps[i], bound)); a >= 0; a = s.maxAtMost(a - 1) {
			layers[i+1].orShifted(layers[i], a)
		}
	}
	out := make([]int, len(sets))
	sum := layers[len(sets)].maxAtMost(bound)
	for i := len(sets) - 1; i >= 0 && sum > 0; i-- {
		pick, best := 0, -1
		for a := sets[i].maxAtMost(min(caps[i], sum)); a >= 0; a = sets[i].maxAtMost(a - 1) {
			if !layers[i].has(sum - a) {
				continue
			}
			d := a - ideal[i]
			if d < 0 {
				d = -d
			}
			if best < 0 || d <= best {
				pick, best = a, d
			}
		}
		out[i] = pick
		sum -= pick
	}
	return out
}

// podUnit is the greatest common divisor of the GPUs per replica of
// variants, the unit the pod search counts in; 1 when there are none.
func podUnit(variants []variantRecord, stateMap map[string]domain.VariantReplicaState) int {
	g := 0
	for _, vc := range variants {
		a, b := gpusPerReplicaFromState(stateMap, vc.VariantName), g
		for b != 0 {
			a, b = b, a%b
		}
		g = a
	}
	return max(g, 1)
}

// toUnits converts gpus to whole units, at most podSearchMaxUnits.
func toUnits(gpus, unit int) int {
	return min(max(gpus, 0)/unit, podSearchMaxUnits)
}

// uniformPods reports whether every variant that can be sized (a capacity
// reading) takes one replica size, across all roles: the case
// reclaimWholeReplicas and roundFillsToReplicas round by summing GPUs. A model
// with more than one size, in one role or across roles, is rounded by an
// exact pod search.
func uniformPods(variants []variantRecord, stateMap map[string]domain.VariantReplicaState) bool {
	size := 0
	for _, vc := range variants {
		g := gpusPerReplicaFromState(stateMap, vc.VariantName)
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		if size != 0 && g != size {
			return false
		}
		size = g
	}
	return true
}

// fillItems are role's variants fillRole can grow, in its cost order, with
// the pods each has room for under its maxReplicas.
func fillItems(variants []variantRecord, role string, stateMap map[string]domain.VariantReplicaState,
	targets map[string]int, unit, bound int) []podItem {
	var out []podItem
	for _, vc := range sortByCostEfficiencyAsc(variantsForRole(variants, role)) {
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		size := gpusPerReplicaFromState(stateMap, vc.VariantName) / unit
		room := bound / max(size, 1)
		if st := stateMap[vc.VariantName]; st.MaxReplicas != nil && *st.MaxReplicas > 0 {
			room = min(room, max(*st.MaxReplicas-targets[vc.VariantName], 0))
		}
		out = append(out, podItem{name: vc.VariantName, size: size, count: room})
	}
	return out
}

// fillRoleExact adds role's replicas worth the most GPUs at most wantGPUs and
// returns the GPUs consumed. A role of one replica size is filled by fillRole;
// with mixed sizes the pods come from an exact search, so +8 on 6- and 4-GPU
// pods is two 4s, where a greedy pass took a 6 and stopped.
func fillRoleExact(variants []variantRecord, role string, stateMap map[string]domain.VariantReplicaState,
	targets map[string]int, wantGPUs int) int {
	if uniformReplicaGPUs(variants, role, stateMap) {
		return fillRole(variants, role, stateMap, targets, wantGPUs)
	}
	unit := podUnit(variantsForRole(variants, role), stateMap)
	bound := toUnits(wantGPUs, unit)
	items := fillItems(variants, role, stateMap, targets, unit, bound)
	amount := podSums(items, bound).maxAtMost(bound)
	spent := 0
	for i, n := range composePods(items, amount) {
		targets[items[i].name] += n
		spent += n * items[i].size * unit
	}
	return spent
}

// roundFillsExact is roundFillsToReplicas for a model with more than one
// replica size. The roles not cut (tgt at or above cur) share the model's
// growth, the sum of their shares above cur, as whole pods by an exact
// search: the most GPUs within the growth, each role taking no more than its
// demand dem still asks for or its own share rounded up to one replica, the
// same bound the greedy pass applies. With more growth or a larger share a
// role only gains choices, so a larger target never places fewer GPUs; the
// greedy pass placed 10 GPUs at a target of 10 and 8 at 11.
func roundFillsExact(records []variantRecord, stateMap map[string]domain.VariantReplicaState,
	roles []string, tgt, cur, dem, targets map[string]int) {
	unit := podUnit(records, stateMap)
	var growing []string
	growth := 0
	for _, role := range roles {
		if tgt[role] >= cur[role] {
			growing = append(growing, role)
			growth += tgt[role] - cur[role]
		}
	}
	bound := toUnits(growth, unit)
	sets := make([]podBits, len(growing))
	caps := make([]int, len(growing))
	ideal := make([]int, len(growing))
	for i, role := range growing {
		want := tgt[role] - cur[role]
		limit := max(dem[role]-cur[role], ceilToReplica(want, roleReplicaStepGPUs(records, role, stateMap)))
		caps[i] = min(toUnits(limit, unit), bound)
		ideal[i] = toUnits(want, unit)
		sets[i] = podSums(fillItems(records, role, stateMap, targets, unit, caps[i]), caps[i])
	}
	for i, a := range jointPods(sets, caps, bound, ideal) {
		tgt[growing[i]] = cur[growing[i]] + a*unit
	}
}

// reclaimExact is reclaimWholeReplicas for a model with more than one
// replica size. The cut roles' cuts are summed, each counted no further than
// what the role can give, and shed by an exact search: the most GPUs within
// the sum, each role no further than its cut rounded up to its smallest
// removable pod, and each role's pods taken most expensive first. A smaller
// target only adds choices, so a larger one never sheds more, and a summed cut
// of at least one pod sheds one. Each variant keeps its minReplicas, a role
// under a scale-down hold is not cut, and a role keeps its last replica when
// it has a share or the cheapest-at-one rule protects it.
func reclaimExact(e NamedAnalyzerResult, records []variantRecord, stateMap map[string]domain.VariantReplicaState,
	roles []string, tgt, cur, targets map[string]int) {
	unit := podUnit(records, stateMap)
	var sets []podBits
	var items [][]podItem
	var caps, ideal []int
	total := 0
	for _, role := range roles {
		if tgt[role] >= cur[role] || e.Holds[role].NoScaleDown {
			continue
		}
		cut := cur[role] - tgt[role]
		ordered := sortVariantsForScaleDown(e, variantsForRole(records, role))
		var its []podItem
		held, removable, step := 0, 0, 0
		for _, vc := range ordered {
			g := gpusPerReplicaFromState(stateMap, vc.VariantName)
			n := targets[vc.VariantName]
			held += n * g
			if vc.PerReplicaCapacity <= 0 {
				continue
			}
			if st := stateMap[vc.VariantName]; st.MinReplicas != nil {
				n -= *st.MinReplicas
			}
			if n <= 0 {
				continue
			}
			its = append(its, podItem{name: vc.VariantName, size: g / unit, count: n})
			removable += n * g
			if step == 0 || g < step {
				step = g
			}
		}
		top := toUnits(removable, unit)
		set := podSums(its, top)
		// Shedding all the role holds is the one way to reach its top sum.
		// It is barred when the role has a share or its cheapest variant
		// still holds a replica (the cheapest-at-one rule).
		if removable > 0 && removable == held && top == removable/unit &&
			(tgt[role] > 0 || targets[ordered[len(ordered)-1].VariantName] >= 1) {
			set.unset(top)
		}
		can := set.maxAtMost(top)
		if can <= 0 {
			continue
		}
		sets = append(sets, set)
		items = append(items, its)
		caps = append(caps, toUnits(ceilToReplica(cut, step), unit))
		ideal = append(ideal, toUnits(cut, unit))
		// Summed in GPUs: two cuts each short of a unit still make one.
		total += min(cut, can*unit)
	}
	if toUnits(total, unit) == 0 {
		return
	}
	for i, shed := range jointPods(sets, caps, toUnits(total, unit), ideal) {
		for j, n := range composePods(items[i], shed) {
			targets[items[i][j].name] -= n
		}
	}
}
