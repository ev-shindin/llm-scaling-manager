package allocation

import (
	"maps"
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// BorrowDraw is borrowed capacity the greedy optimizer ordered for one variant
// in one pass: GPUs drawn from one shared-pool chunk, past the namespace's
// guaranteed figure. The engine keeps it as in flight until the pods exist
// (BorrowInFlight), since Kueue reserves nothing for them before.
type BorrowDraw struct {
	Namespace string
	ModelID   string
	Variant   string
	// Cohort, Flavor and Resource name the chunk drawn from.
	Cohort   string
	Flavor   string
	Resource string
	// GPUs is the borrowed part of the replicas ordered, Replicas how many
	// replicas drew on it.
	GPUs     int
	Replicas int
	// Target is the variant's target after the pass.
	Target int
}

// nsBorrowLeft is what one namespace may still borrow of one accelerator type
// in this pass.
type nsBorrowLeft struct {
	// cap is the GPUs it may still draw past its guaranteed figure; negative
	// is no cap but the pool's.
	cap int
	// lentOut is guaranteed growth still to debit from the pool.
	lentOut int
}

// borrowState is one optimizer pass's borrowing: what each namespace may still
// draw, the shared pools, and the node view. Draws mutate it, so a pool is
// spent once across every namespace in its cohort and a node hole once across
// every model. Nil when no constraint counts borrowing.
type borrowState struct {
	borrow  map[string]map[string]*nsBorrowLeft
	reach   map[string]map[string]int
	chunks  []*SharedPoolChunk
	cohorts map[string][]string
	nodes   *NodeView
	draws   []BorrowDraw
	// blocked reports a variant that may not borrow this pass; nil blocks none.
	blocked func(namespace, modelID, variant string) bool
}

// newBorrowState builds the pass's borrowing state, or nil when no constraint
// counts borrowing. nodes is the cycle's node view, nil without a physical
// limiter or a fresh node snapshot.
func newBorrowState(constraints []*ResourceConstraints, nodes *NodeView) *borrowState {
	if !HasBorrowing(constraints) {
		return nil
	}
	bs := &borrowState{borrow: map[string]map[string]*nsBorrowLeft{}, nodes: nodes}
	bs.chunks, bs.cohorts = mergeSharedPools(constraints)
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns, perType := range c.NamespaceBorrow {
			if bs.borrow[ns] == nil {
				bs.borrow[ns] = map[string]*nsBorrowLeft{}
			}
			for accType, b := range perType {
				prev, seen := bs.borrow[ns][accType]
				if !seen {
					bs.borrow[ns][accType] = &nsBorrowLeft{cap: b.Cap, lentOut: b.LentOut}
					continue
				}
				// Two Kueue-backed entries hold the namespace: the tighter cap.
				prev.cap = tighterBudget(prev.cap, b.Cap)
				prev.lentOut = max(prev.lentOut, b.LentOut)
			}
		}
	}
	reach := mergeNamespaceConstraints(reachConstraints(constraints))
	bs.reach = map[string]map[string]int{}
	for ns := range bs.borrow {
		if r, ok := reach[ns]; ok {
			bs.reach[ns] = r
		}
	}
	return bs
}

// reachFor is the namespace's budget at its reach (guaranteed plus borrow
// cap), the upper bound a borrowing namespace's model is sized against; nil
// for a namespace that borrows nothing.
func (bs *borrowState) reachFor(ns string) map[string]int {
	if bs == nil {
		return nil
	}
	return bs.reach[ns]
}

// cohortChunks is the chunks a namespace may draw a replica of acc from, in a
// stable order.
func (bs *borrowState) cohortChunks(ns, acc string) []*SharedPoolChunk {
	var out []*SharedPoolChunk
	for _, c := range bs.chunks {
		if slices.Contains(bs.cohorts[ns], c.Cohort) && c.serves(acc) {
			out = append(out, c)
		}
	}
	return out
}

// debitPool takes n GPUs of acc out of the namespace's cohort chunks, first
// chunk first, each clamped at 0. Used for growth that needs no gate -- a
// reclaiming queue growing into nominal it lent out -- so that no other queue
// borrows the same GPUs in the same cycle.
func (bs *borrowState) debitPool(ns, acc string, n int) {
	for _, c := range bs.cohortChunks(ns, acc) {
		if n <= 0 {
			return
		}
		take := min(c.Free, n)
		c.Free -= take
		n -= take
	}
}

// borrowSave is a borrowState taken before a tentative placement.
type borrowSave struct {
	borrow     map[string]map[string]nsBorrowLeft
	free       []int
	nodes      *NodeView
	draws      int
	guaranteed map[string]int
}

func (bs *borrowState) save(guaranteed map[string]int) borrowSave {
	s := borrowSave{borrow: map[string]map[string]nsBorrowLeft{}, nodes: bs.nodes.Clone(), draws: len(bs.draws), guaranteed: maps.Clone(guaranteed)}
	for ns, perType := range bs.borrow {
		s.borrow[ns] = map[string]nsBorrowLeft{}
		for t, b := range perType {
			s.borrow[ns][t] = *b
		}
	}
	for _, c := range bs.chunks {
		s.free = append(s.free, c.Free)
	}
	return s
}

func (bs *borrowState) restore(s borrowSave, guaranteed map[string]int) {
	for ns, perType := range s.borrow {
		for t, b := range perType {
			*bs.borrow[ns][t] = b
		}
	}
	for i, c := range bs.chunks {
		c.Free = s.free[i]
	}
	if bs.nodes != nil {
		*bs.nodes = *s.nodes
	}
	bs.draws = bs.draws[:s.draws]
	clear(guaranteed)
	maps.Copy(guaranteed, s.guaranteed)
}

// modelPlacer places one model's replicas, one at a time, against its
// namespace's guaranteed budget first and the shared pools after (B4, B5).
type modelPlacer struct {
	bs        *borrowState
	namespace string
	modelID   string
	// guaranteed is this model's working copy of its namespace's guaranteed
	// budget; nil for an open namespace, where everything is guaranteed.
	guaranteed map[string]int
	// borrows is whether the namespace may borrow at all.
	borrows bool
	targets map[string]int
}

// newModelPlacer returns the placer for one model, or nil when the pass counts
// no borrowing, which leaves allocation exactly as it was.
func (bs *borrowState) newModelPlacer(namespace, modelID string, nsBudget map[string]int, targets map[string]int) *modelPlacer {
	if bs == nil {
		return nil
	}
	_, borrows := bs.borrow[namespace]
	return &modelPlacer{bs: bs, namespace: namespace, modelID: modelID, guaranteed: maps.Clone(nsBudget), borrows: borrows, targets: targets}
}

// placeUnit places one iteration's commit -- kByRole replicas of each picked
// role's variant -- as one unit: all of it, or a smaller unit at the same
// shape, never one role without its partner. It trims kByRole as
// fitPairToBudget does (the role holding the most GPUs gives one replica at a
// time, none below one) until the unit places, and reports false when not
// even one replica of each role places, in which case nothing is committed.
func (p *modelPlacer) placeUnit(picked []string, kByRole map[string]int, variantByRole map[string]string,
	variants []variantRecord, stateMap map[string]domain.VariantReplicaState) bool {
	gpus := func(role string) int { return gpusPerReplicaFromState(stateMap, variantByRole[role]) }
	for {
		saved := p.bs.save(p.guaranteed)
		if p.tryUnit(picked, kByRole, variantByRole, variants, stateMap) {
			return true
		}
		p.bs.restore(saved, p.guaranteed)
		largest := ""
		for _, role := range picked {
			if kByRole[role] > 1 && (largest == "" || kByRole[role]*gpus(role) > kByRole[largest]*gpus(largest)) {
				largest = role
			}
		}
		if largest == "" {
			return false
		}
		kByRole[largest]--
	}
}

// tryUnit places every replica of the unit, one at a time, and reports
// whether all placed.
func (p *modelPlacer) tryUnit(picked []string, kByRole map[string]int, variantByRole map[string]string,
	variants []variantRecord, stateMap map[string]domain.VariantReplicaState) bool {
	for _, role := range picked {
		v := variantByRole[role]
		acc := accFromVCs(variants, v)
		g := gpusPerReplicaFromState(stateMap, v)
		pods := replicaPods(stateMap[v].PodGPUs, g)
		for i := 0; i < kByRole[role]; i++ {
			if !p.placeReplica(v, acc, g, pods, p.targets[v]+kByRole[role]) {
				return false
			}
		}
	}
	return true
}

// placeReplica places one replica of g GPUs on acc. Within the guaranteed
// budget it always places: it debits the node it fits, or nothing when it
// fits none (it stays Pending and is reported as unplaced, B5), and growth into
// lent-out nominal debits the pool, clamped. Past it the borrowed part must fit
// the namespace's cap, one chunk of its cohorts, and -- with a node view -- a
// node of that chunk's flavor, whole replica at once.
func (p *modelPlacer) placeReplica(variant, acc string, g int, pods []int, target int) bool {
	bs := p.bs
	left := -1 // unbounded: an open namespace, or an unlimited allowance
	key := ""
	if p.guaranteed != nil {
		key = budgetKey(p.guaranteed, acc)
		left = p.guaranteed[key]
	}
	var borrow *nsBorrowLeft
	if p.borrows {
		if k, ok := accelerator.FindKey(bs.borrow[p.namespace], acc); ok {
			borrow = bs.borrow[p.namespace][k]
		}
	}
	if left < 0 || left >= g {
		if left >= 0 {
			p.guaranteed[key] = left - g
		}
		if borrow != nil && borrow.lentOut > 0 {
			n := min(g, borrow.lentOut)
			borrow.lentOut -= n
			bs.debitPool(p.namespace, acc, n)
		}
		bs.nodes.place(acc, pods, nil, true)
		return true
	}
	if borrow == nil || (bs.blocked != nil && bs.blocked(p.namespace, p.modelID, variant)) {
		return false
	}
	need := g - left
	if borrow.cap >= 0 && borrow.cap < need {
		return false
	}
	for _, c := range bs.cohortChunks(p.namespace, acc) {
		if c.Free < need || !bs.nodes.place(acc, pods, &c.FlavorPlacement, false) {
			continue
		}
		bs.nodes.place(acc, pods, &c.FlavorPlacement, true)
		c.Free -= need
		if borrow.cap >= 0 {
			borrow.cap -= need
		}
		p.guaranteed[key] = 0
		bs.draws = append(bs.draws, BorrowDraw{
			Namespace: p.namespace, ModelID: p.modelID, Variant: variant,
			Cohort: c.Cohort, Flavor: c.Flavor, Resource: c.Resource,
			GPUs: need, Replicas: 1, Target: target,
		})
		return true
	}
	return false
}

// drawsByVariant folds the pass's draws into one per variant and chunk.
func (bs *borrowState) drawsByVariant() []BorrowDraw {
	if bs == nil || len(bs.draws) == 0 {
		return nil
	}
	type key struct{ ns, variant, cohort, flavor, resource string }
	idx := map[key]int{}
	var out []BorrowDraw
	for _, d := range bs.draws {
		k := key{d.Namespace, d.Variant, d.Cohort, d.Flavor, d.Resource}
		i, ok := idx[k]
		if !ok {
			idx[k] = len(out)
			out = append(out, d)
			continue
		}
		out[i].GPUs += d.GPUs
		out[i].Replicas += d.Replicas
		out[i].Target = max(out[i].Target, d.Target)
	}
	return out
}
