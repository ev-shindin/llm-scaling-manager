package allocation

import (
	"maps"
	"math"
	"slices"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// gpuLimitTracker records the variants of ONE model whose scale-up the optimizer
// had to cut short because a GPU budget ran out. It is the bridge between the
// allocation loops — which are the only place that knows a target was clamped —
// and the decisions built afterwards, which carry the operator-facing signal.
//
// Only genuine scarcity is recorded. A variant held back by its own MaxReplicas
// ceiling is honouring user intent, not competing for GPUs, and marking it would
// raise a spurious ResourceConstrained warning; the loops therefore distinguish
// the two bounds before marking.
//
// Keyed by variant name, which is unique within a model. Trackers are per-model
// so names never collide across models or namespaces. The value is what cut
// the variant short: a pool (the provider bindingProvider names), or node fit.
type gpuLimitTracker map[string]limitCause

// limitCause is what cut a variant's scale-up short.
type limitCause int

const (
	// limitedByPool: a GPU budget ran out.
	limitedByPool limitCause = iota
	// limitedByNodeFit: the pools had the GPUs, but no node held a whole
	// replica (NodeView).
	limitedByNodeFit
	// limitedByPairWhole: a rule, not scarcity: a P/D pair borrows only
	// whole, and its next pair would straddle guaranteed and borrowed
	// capacity.
	limitedByPairWhole
	// limitedByLenderCannotReclaim: a rule, not scarcity: the GPUs the
	// replica needed were there, lent only by LowerPriority lenders that do
	// not outrank the model, which Kueue would never reclaim them from.
	limitedByLenderCannotReclaim
)

// LenderCannotReclaimName is what LimitedBy carries when a borrowed replica
// was refused because the only GPUs that would have held it are lent by
// LowerPriority lenders that do not outrank its model (K1): Kueue could never
// reclaim them for their lender. It is kueue.ReasonLenderCannotReclaim, the
// namespace-level reason for the same rule.
const LenderCannotReclaimName = string(kueue.ReasonLenderCannotReclaim)

// markLenderCannotReclaim records that only lending Kueue could never reclaim
// from variantName's model held the GPUs it needed.
func (t gpuLimitTracker) markLenderCannotReclaim(variantName string) {
	if t != nil {
		t[variantName] = limitedByLenderCannotReclaim
	}
}

// PairBorrowsWholeName is what LimitedBy carries when the rule that a P/D
// pair borrows only whole (the Kueue borrowing proposal, B9) -- not a pool,
// not node fit -- held a pair back: its next pair would have put one role on
// guaranteed and the other on borrowed capacity, and the guaranteed remainder
// could not be filled by one role alone.
const PairBorrowsWholeName = "pd-pair-borrows-whole"

// markPairWhole records that the pair-borrows-whole rule held variantName back.
func (t gpuLimitTracker) markPairWhole(variantName string) {
	if t != nil {
		t[variantName] = limitedByPairWhole
	}
}

// NodeFitLimiterName is what LimitedBy, the ResourceConstrained event and the
// limiter_name label of wva_decisions_limited_total carry when node fit, not a
// pool, cut a scale-up short: the pools held the GPUs, but no schedulable node
// held a whole replica. It names no limiter entry: node fit belongs to the
// physical limiter (PhysicalLimiterName), and is told apart from its pool
// running out because the remedy differs -- defragment or add a node of the
// right size, not add GPUs.
const NodeFitLimiterName = "node-fit"

// mark records that variantName wanted more replicas than the GPU budget allowed.
// Repeated marks are idempotent — a variant clamped in several fair-share
// iterations is still one constrained decision — and the last cut names the
// cause: the budgets only shrink within a pass, so the last bound met is the one
// that held.
func (t gpuLimitTracker) mark(variantName string) {
	if t != nil {
		t[variantName] = limitedByPool
	}
}

// markNodeFit records that node fit, not a pool, held variantName back.
func (t gpuLimitTracker) markNodeFit(variantName string) {
	if t != nil {
		t[variantName] = limitedByNodeFit
	}
}

// applyGPULimitAttribution stamps the resource-constraint signal onto a model's
// decisions: WasLimited for every variant the tracker recorded, and LimitedBy
// naming the provider whose pool bound it, or NodeFitLimiterName when node fit
// did. It also emits the decisions_limited_total metric, once per constrained
// decision.
//
// Decisions of every action carry the signal, not just scale-ups: a variant that
// wanted to grow and was denied every GPU ends up at ActionNoChange, and that is
// precisely the case an operator needs to see.
func applyGPULimitAttribution(
	decisions []domain.VariantDecision,
	limited gpuLimitTracker,
	constraints []*ResourceConstraints,
) {
	// Published on EVERY pass, including the one where nothing was limited.
	// An empty snapshot is a real answer -- it says this pass denied nobody --
	// and publishing only the non-empty ones would leave the last contended
	// reading standing forever, holding warm pools down long after the burst.
	contended := map[string]map[string]bool{}
	// Headroom is published once the pass is over (publishPassHeadroom), from
	// what the pass left: published here, per model, it read the constraints
	// as they were before any model's growth was debited, and a warm pool
	// sized on it took GPUs the pass had just ordered for a model.
	defer func() {
		decision.PublishGPUContention(contended, time.Now())
	}()

	if len(limited) == 0 {
		return
	}
	emitter := metrics.NewMetricsEmitter()
	for i := range decisions {
		d := &decisions[i]
		cause, ok := limited[d.VariantName]
		if !ok {
			continue
		}
		d.WasLimited = true
		switch cause {
		case limitedByNodeFit:
			d.LimitedBy = NodeFitLimiterName
		case limitedByPairWhole, limitedByLenderCannotReclaim:
			// A rule held it back, not scarcity: no warm pool contends.
			d.LimitedBy = PairBorrowsWholeName
			if cause == limitedByLenderCannotReclaim {
				d.LimitedBy = LenderCannotReclaimName
			}
			emitter.RecordDecisionsLimitedTotalMetric(d.VariantName, d.Namespace, d.LimitedBy)
			continue
		default:
			if names := bindingProviders(constraints, d.Namespace, d.AcceleratorName); len(names) > 0 {
				d.LimitedBy, d.LimitedByAlso = names[0], names[1:]
				if len(d.LimitedByAlso) == 0 {
					d.LimitedByAlso = nil
				}
			}
		}
		emitter.RecordDecisionsLimitedTotalMetric(d.VariantName, d.Namespace, d.LimitedBy)

		// A warm pool of this accelerator type, in this namespace, should stop
		// competing for GPUs a model replica is being denied. See
		// decision.GPUContention.
		if d.AcceleratorName != "" {
			if contended[d.Namespace] == nil {
				contended[d.Namespace] = map[string]bool{}
			}
			contended[d.Namespace][d.AcceleratorName] = true
		}
	}
}

// namespaceHeadroom is how many GPUs of each accelerator each namespace may
// still take, across every provider, tightest bound winning -- the closed
// cluster-scope bound (clusterHeadroom) included, as GPUBudgets bounds a
// namespace's allowance by the cluster pools: a namespace may not be granted
// more than the cluster has free.
//
// Only namespaces some provider actually CAPS appear. A namespace nobody caps
// is absent rather than present-with-a-large-number, because "unconstrained" and
// "a lot left" are different answers and a caller has to be able to tell them
// apart -- a warm pool must grow freely in the first case. It is still bounded
// by the cluster figure, which is published beside this (Headroom.Cluster).
//
// Mirrors effectiveAvailable and GPUBudgets: a negative Limit is the unlimited
// sentinel and imposes no namespace bound, an accelerator a namespace's
// allowlist does not name is a hard deny, which is zero rather than absent, and
// a type the cluster bounds do not list is zero too however the namespace lists
// it (clusterBudgetFor) -- a physical pool of {H100: 8} leaves a namespace
// allowance of {A100: 4}, or of unlimited A100s, nothing to place.
//
// The cluster bound on each key is read from mergeWithAllowanceKeys, so a key
// the cluster bounds list only through products, such as a family under a
// quota of products, reads their sum. A namespace's map also holds every
// product the cluster bounds list that its allowance admits (clusterProducts),
// each at the tighter of the two figures: a namespace allowed {H100: -1} under
// {NVIDIA-H100-80GB-HBM3: 8, NVIDIA-H100-PCIE-80GB: 8} reads 8 of each
// product, not the family's 16, and a product the cluster does not list reads
// 0, as the optimizer plans it.
func namespaceHeadroom(constraints []*ResourceConstraints) map[string]map[string]int {
	out := namespaceAllowance(constraints)
	cluster := mergeWithAllowanceKeys(constraints)
	for _, perType := range out {
		for accType, free := range perType {
			cv, bounded := clusterBudgetFor(cluster, accType)
			if !bounded || cv == math.MaxInt {
				continue
			}
			if free < 0 {
				// Unlimited in the namespace: the cluster figure is the bound.
				perType[accType] = cv
			} else {
				perType[accType] = min(free, cv)
			}
		}
	}
	return out
}

// clusterHeadroom is the free GPUs per accelerator under every cluster bound
// (the physical inventory, a cluster-scope quota), merged as the optimizer
// merges them, or nil when none is in force. A namespace-scope quota bounds a
// namespace through that namespace's own allowance instead.
func clusterHeadroom(constraints []*ResourceConstraints) map[string]int {
	return mergeConstraints(constraints)
}

// publishHeadroom publishes the warm pool's headroom from one cycle's
// constraints: each capped namespace's allowance and the closed cluster-scope
// bound.
func publishHeadroom(constraints []*ResourceConstraints, now time.Time) {
	// The warm pool spends capacity within nominal only, never borrowed
	// capacity (B7): a pool Pod on borrowed GPUs is the first thing a reclaim
	// takes.
	constraints = WithinNominal(constraints)
	decision.PublishHeadroomBounds(namespaceHeadroom(constraints), clusterHeadroom(constraints),
		poolsVersionOf(constraints), now)
}

// namespaceAllowance is the free GPUs per (namespace, accelerator) under the
// namespace pools: mergeNamespaceConstraints, the optimizer's own merge, so the
// warm pool cannot read a type the optimizer refuses, with the cluster's
// products materialized as the optimizer's pass does. An unlimited type
// is decision.HeadroomUnlimited -- skipping it would leave the type absent from
// a present namespace, which a reader takes as DENIED.
func namespaceAllowance(constraints []*ResourceConstraints) map[string]map[string]int {
	out := mergeNamespaceConstraints(constraints, clusterProducts(constraints)...)
	for _, perType := range out {
		for accType, free := range perType {
			if free < 0 {
				perType[accType] = decision.HeadroomUnlimited
			}
		}
	}
	if out == nil {
		out = map[string]map[string]int{}
	}
	return out
}

// bindingProvider names the constraint provider whose pool most tightly bounds
// (namespace, accType) — the one to credit when a GPU budget clamps a scale-up.
//
// Cluster and namespace pools are weighed together, mirroring how
// effectiveAvailable combines them: the smallest availability wins. A provider
// that carries a closed allowlist for this namespace is judged on that allowlist
// alone (its per-type Pools are the aggregate *derived* from it, so counting
// both would weigh one provider twice), and a type the namespace does not list
// is a hard deny — availability 0, the tightest bound there is.
//
// The cluster pools follow mergeConstraints: a cluster bound that does not list
// the type denies it, availability 0, and is credited for it -- otherwise a
// cluster quota of {H100: 8} beside a physical pool of {A100: 4} would let A100
// be blamed on the physical limiter, which is the one that allowed it. A
// namespace-scope provider that does not hold the namespace bounds nothing
// here. Types are matched over the family/product tree (allowlistBound), as
// everywhere a workload's accelerator meets a pool key.
//
// Unlimited sentinels (Limit < 0) impose no bound and are skipped. Returns ""
// when no provider finitely bounds the type, which leaves LimitedBy empty rather
// than crediting a limiter that did not constrain anything. On a tie it is the
// first provider in the declared order; bindingProviders names them all.
func bindingProvider(constraints []*ResourceConstraints, namespace, accType string) string {
	if names := bindingProviders(constraints, namespace, accType); len(names) > 0 {
		return names[0]
	}
	return ""
}

// bindingProviders is bindingProvider naming every provider tied at the
// tightest bound, in the declared order: two limiters that leave the same
// availability both held the decision back, and crediting only the first
// sends the operator to raise one and find the other still binding. Nil when
// no provider finitely bounds the type.
func bindingProviders(constraints []*ResourceConstraints, namespace, accType string) []string {
	names := make([]string, 0, 2)
	tightest := math.MaxInt
	consider := func(avail int, provider string) {
		switch {
		case avail < tightest:
			tightest, names = avail, append(names[:0], provider)
		case avail == tightest && !slices.Contains(names, provider):
			names = append(names, provider)
		}
	}
	joint, jointBinders := jointClusterBound(constraints, accType)
	for _, c := range constraints {
		if c == nil {
			continue
		}
		if perType, ok := c.NamespacePools[namespace]; ok {
			avail, unlimited, listed := allowlistBound(perType, accType)
			switch {
			case !listed:
				consider(0, c.ProviderName)
			case !unlimited:
				consider(avail, c.ProviderName)
			}
			continue
		}
		if !isClusterBound(c) {
			continue
		}
		if jointBinders[c] {
			consider(joint, c.ProviderName)
			continue
		}
		if avail, bounds := clusterBound(c.Pools, accType); bounds && avail != math.MaxInt {
			consider(avail, c.ProviderName)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// jointClusterBound is the bound the cluster bounds put on the family accType
// together (accelerator.ClusterAllowlistsBound), and the bounds that hold it,
// when it is tighter than any of them alone: on each product of accType a
// cluster bound lists, those at the tightest figure for it. Cluster quotas of
// {NVIDIA-H100-80GB-HBM3: 32, NVIDIA-H100-PCIE-80GB: 8} and
// {NVIDIA-H100-80GB-HBM3: 8, NVIDIA-H100-PCIE-80GB: 32} each allow 40 H100s,
// and a physical pool of 40 does too, but only 16 fit under both quotas: they
// bind, the physical pool does not. Nil binders when the joint bound is no
// tighter than the tightest bound alone.
func jointClusterBound(constraints []*ResourceConstraints, accType string) (int, map[*ResourceConstraints]bool) {
	cluster := clusterBounds(constraints)
	allowlists := make([]map[string]ResourcePool, 0, len(cluster))
	alone := math.MaxInt
	for _, c := range cluster {
		allowlists = append(allowlists, c.Pools)
		if avail, bounds := clusterBound(c.Pools, accType); bounds {
			alone = min(alone, avail)
		}
	}
	joint, bounds := clusterBoundAll(allowlists, accType)
	if !bounds || joint >= alone {
		return 0, nil
	}
	// The joint bound is then the finite sum over these products, so each
	// has a finite tightest figure. A product listed twice marks the same
	// bounds again.
	binders := map[*ResourceConstraints]bool{}
	for _, product := range familyProducts(allowlists, accType) {
		tightest := math.MaxInt
		for _, c := range cluster {
			if avail, bounds := clusterBound(c.Pools, product); bounds {
				tightest = min(tightest, avail)
			}
		}
		for _, c := range cluster {
			if avail, bounds := clusterBound(c.Pools, product); bounds && avail == tightest {
				binders[c] = true
			}
		}
	}
	return joint, binders
}

// familyProducts is every product of the family called name that one of the
// allowlists lists. Unordered, and it may repeat names.
func familyProducts(allowlists []map[string]ResourcePool, name string) []string {
	var out []string
	for _, pools := range allowlists {
		for key := range pools {
			if accelerator.IsProductOf(key, name) {
				out = append(out, key)
			}
		}
	}
	return out
}

// passHeadroom is what an optimizer pass left of the budgets: the cluster
// budget and the namespace budgets after every replica the pass ordered was
// debited, and the node view after its placements. The warm pool's headroom is
// published from it, not from the constraints the pass started with: the GPUs
// ordered are not held yet, and a pool growing into them leaves the model's
// replica Pending.
type passHeadroom struct {
	available   map[string]int
	byNamespace map[string]map[string]int
	nodes       *NodeView
	version     uint64
	// start and startByNS are the budgets as the pass found them, and spends
	// what it took from them, in order: without replays them less a model's.
	start     map[string]int
	startByNS map[string]map[string]int
	spends    []spendOp
	// clusterKeys are the keys of available the warm pool's cluster figure
	// keeps: available also holds every key a namespace allowance lists
	// (mergeWithAllowanceKeys), which bound those namespaces and nothing else.
	clusterKeys []string
}

// cloneNamespaceBudgets copies a namespace -> accelerator -> GPUs map, nil for
// nil.
func cloneNamespaceBudgets(byNS map[string]map[string]int) map[string]map[string]int {
	if byNS == nil {
		return nil
	}
	out := make(map[string]map[string]int, len(byNS))
	for ns, perType := range byNS {
		out[ns] = maps.Clone(perType)
	}
	return out
}

// replay applies the pass's spends to available and byNS, in the order the
// pass made them, skipping every model in exclude (namespace/modelID).
func replay(spends []spendOp, available map[string]int, byNS map[string]map[string]int, exclude map[string]bool) {
	for _, s := range spends {
		if exclude[s.model] {
			continue
		}
		if s.ns == "" {
			debitBudget(available, s.key, s.gpus)
		} else {
			debitBudget(byNS[s.ns], s.key, s.gpus)
		}
	}
}

// without returns a copy of h as if the pass had ordered nothing for the
// models in exclude (namespace/modelID): their spends are not replayed and
// their node placements are taken back. h itself when exclude is empty.
func (h passHeadroom) without(exclude map[string]bool) passHeadroom {
	if len(exclude) == 0 {
		return h
	}
	out := h
	out.available = maps.Clone(h.start)
	out.byNamespace = cloneNamespaceBudgets(h.startByNS)
	replay(h.spends, out.available, out.byNamespace, exclude)
	out.nodes = h.nodes.without(func(owner string) bool { return exclude[owner] })
	return out
}

// withheld returns a copy of h with promised GPUs (scope "" for the cluster,
// else a namespace -> accelerator -> holes) debited too, as WithholdPromised
// charges them: a namespace promise to the namespace and the cluster budget, a
// cluster promise to the cluster budget alone; and held on the nodes their
// holes are on (ReservePromised).
func (h passHeadroom) withheld(promised map[string]map[string]decision.PromisedHoles) passHeadroom {
	out := h
	out.available = maps.Clone(h.available)
	out.byNamespace = cloneNamespaceBudgets(h.byNamespace)
	for _, scope := range slices.Sorted(maps.Keys(promised)) {
		for _, acc := range slices.Sorted(maps.Keys(promised[scope])) {
			g := promised[scope][acc].Total()
			if g <= 0 {
				continue
			}
			if scope != "" {
				if nsBudget, ok := out.byNamespace[scope]; ok {
					debitBudget(nsBudget, budgetKey(nsBudget, acc), g)
				}
			}
			debitBudget(out.available, budgetKey(out.available, acc), g)
		}
	}
	if out.nodes != nil && len(promised) > 0 {
		out.nodes = out.nodes.Clone()
		ReservePromised(out.nodes, promised)
	}
	return out
}

// PassLeft is what an optimizer pass left, for the utilization share's step
// that follows it (GreedyByScoreOptimizer.LastPass): the share plans the
// models it owns on the cycle's budgets and nodes less what the pass ordered
// for every OTHER model, not on the raw constraints -- planned on those, its
// idle fill counted free the GPUs and node holes the pass had just ordered for
// another model.
type PassLeft struct {
	h *passHeadroom
}

// Replay debits from available and byNS (the share's own merge of the
// cycle's constraints) what the pass ordered for every model not in exclude
// (namespace/modelID).
func (p *PassLeft) Replay(available map[string]int, byNS map[string]map[string]int, exclude map[string]bool) {
	if p == nil {
		return
	}
	replay(p.h.spends, available, byNS, exclude)
}

// NodeDebits is the GPUs the pass's node view debited per node for every
// owner other than the models in exclude (namespace/modelID) and the promised
// GPUs (PromisedOwner): the in-flight ledger's holes and the other models'
// placements. Nil when the pass had no node view.
func (p *PassLeft) NodeDebits(exclude map[string]bool) map[string]int {
	if p == nil {
		return nil
	}
	return p.h.nodes.DebitsExcept(func(owner string) bool { return owner == PromisedOwner || exclude[owner] })
}

// TakenDomains is, per topology label key, the domains the pass's node view
// gave an exclusive-topology group -- the in-flight ledger's holds and the
// other models' placements -- for every owner other than the models in
// exclude (namespace/modelID): the utilization share places no group there
// (ClaimShareDomains). Nil when the pass had no node view.
func (p *PassLeft) TakenDomains(exclude map[string]bool) map[string][]string {
	if p == nil {
		return nil
	}
	return p.h.nodes.TakenDomains(func(owner string) bool { return exclude[owner] })
}

// publish records h as the warm pool's headroom: each capped namespace's
// allowance bounded by the cluster budget (as namespaceHeadroom bounds it),
// the cluster budget, and the nodes' holes.
func (h passHeadroom) publish(now time.Time) {
	free := make(map[string]map[string]int, len(h.byNamespace))
	for ns, perType := range h.byNamespace {
		inner := make(map[string]int, len(perType))
		for acc, v := range perType {
			cv, bounded := clusterBudgetFor(h.available, acc)
			bounded = bounded && cv != math.MaxInt
			switch {
			case v < 0 && bounded:
				inner[acc] = cv
			case v < 0:
				inner[acc] = decision.HeadroomUnlimited
			case bounded:
				inner[acc] = min(v, cv)
			default:
				inner[acc] = v
			}
		}
		free[ns] = inner
	}
	decision.PublishHeadroomBoundsOnNodes(free, h.clusterFigure(), h.nodes.Holes(), h.version, now)
}

// clusterFigure is available with only clusterKeys: the cluster figure a
// namespace no quota holds reads, as mergeConstraints keys it. Nil when
// available is (no cluster bound in force).
func (h passHeadroom) clusterFigure() map[string]int {
	if h.available == nil {
		return nil
	}
	out := make(map[string]int, len(h.clusterKeys))
	for _, key := range h.clusterKeys {
		out[key] = h.available[key] // a key mergeConstraints gives is one of available's
	}
	return out
}

// PublishNamespaceHeadroom records what each namespace may still take, from a
// caller that built constraints outside an optimization pass.
//
// Headroom is normally published as a side effect of attribution, which runs
// only when there is something to optimize. A namespace whose ONLY WVA
// consumption is a warm pool has no variants and therefore no optimization pass
// -- so it never got an answer, and "no answer" used to mean unbounded to the
// pool, which is the one caller that reads it. Measured: a namespace with a
// one-GPU quota and no models grew its pool to three Pods.
//
// Exported rather than exposing namespaceHeadroom itself, so the mapping from
// constraints to headroom stays in one place and every publisher agrees about
// what an absent namespace means.
func PublishNamespaceHeadroom(constraints []*ResourceConstraints, now time.Time) {
	publishHeadroom(constraints, now)
}

// PublishNamespaceHeadroomOnNodes is PublishNamespaceHeadroom with the nodes'
// holes (decision.Headroom.Nodes) from nodes; a nil view publishes none.
func PublishNamespaceHeadroomOnNodes(constraints []*ResourceConstraints, nodes *NodeView, now time.Time) {
	decision.PublishHeadroomBoundsOnNodes(namespaceHeadroom(constraints), clusterHeadroom(constraints),
		nodes.Holes(), poolsVersionOf(constraints), now)
}

// PublishUnboundedHeadroom records that NO limiter bounds any namespace, as of
// the given warm-pool figure version.
//
// This is a real answer and it has to be published: a warm pool holds until it
// has a headroom snapshot at least as new as its own last charge, and on a
// cluster with no limiter nothing else would ever publish one -- the pool would
// wait forever for an allowance nobody is keeping. An empty snapshot says
// "nothing bounds you", which is what the pool needs to hear to grow.
func PublishUnboundedHeadroom(poolsVersion uint64, now time.Time) {
	decision.PublishHeadroom(nil, poolsVersion, now)
}

// poolsVersionOf is the warm-pool figure version every constraint was computed
// against: the smallest across them, so a snapshot never claims to have charged
// a figure some constraint in it has not seen. Zero when no constraint is
// stamped, which no pool that has ever published can match -- an unstamped
// snapshot never lets a pool grow, and a publisher that forgets to stamp shows
// up as a pool that stops growing rather than one that overshoots.
func poolsVersionOf(constraints []*ResourceConstraints) uint64 {
	var v uint64
	first := true
	for _, c := range constraints {
		if c == nil {
			continue
		}
		if first || c.PoolsVersion < v {
			v = c.PoolsVersion
			first = false
		}
	}
	return v
}
