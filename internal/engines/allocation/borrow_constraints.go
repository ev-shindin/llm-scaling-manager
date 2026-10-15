package allocation

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// NamespaceBorrow is one namespace's borrowing figures for one accelerator type
// (the Kueue borrowing proposal, PR #161, B3 and B4). The
// namespace's pool Limit is its guaranteed figure; these say what lies past it.
type NamespaceBorrow struct {
	// Cap is how many GPUs past the guaranteed Limit the namespace may reach,
	// drawn from its cohorts' shared pools: the queues' borrowing limits plus,
	// for a queue that cannot reclaim, the nominal it lent out, bounded by the
	// static entry and the namespace ResourceQuota. config.QuotaUnlimited when
	// nothing but the shared pool bounds it.
	Cap int
	// WithinNominal is the namespace's Limit within nominal (B7): what
	// scale-from-zero, the warm pool and rescale may spend. config.QuotaUnlimited
	// when the static entry leaves the type unlimited and Kueue does not bound
	// it either.
	WithinNominal int
	// LentOut is the part of the guaranteed figure a peer may hold now: nominal
	// of a reclaiming queue that is lent out. Growth into it is not gated, since
	// Kueue preempts the borrower, but debits the shared pool in the same cycle,
	// clamped at 0, so no other queue borrows the same GPUs.
	LentOut int
	// NominalLentOut is the part of the namespace's nominal quota it cannot
	// reach: a queue that cannot reclaim lent it to a peer, so its guaranteed
	// figure is below nominal (kueue.ReasonNominalLentOut). Reported, not
	// spent: it is why a capped model of the namespace sits below nominal.
	NominalLentOut int
	// Frozen marks a namespace whose ResourceQuota read failed for a reason
	// that may pass: its figures are the last it read, and nothing new is
	// borrowed for it (its draw allowance is 0) until a read passes. What it
	// holds stays, and releases to a waiting lender still apply to it.
	Frozen bool
	// Withheld is, on a Frozen namespace whose borrowing is withheld (say
	// not-kueue-managed) while it still holds borrowed GPUs, why: it borrows
	// nothing new, and what it holds can still be released to a waiting lender.
	// Empty otherwise.
	Withheld string
}

// SharedPoolChunk is one cohort's free GPUs of one flavor and GPU resource. A
// replica uses one flavor, so a borrowed replica is placed whole into one chunk,
// and on that flavor's nodes.
type SharedPoolChunk struct {
	Cohort   string
	Flavor   string
	Resource string
	kueue.FlavorPlacement
	// Free is what the chunk still holds.
	Free int
	// Lenders are the cohort's members declaring this flavor and resource, as
	// the read saw them; the release of borrowed capacity (B6) debounces them.
	Lenders []kueue.LenderState
	// ReadAt is when the Kueue snapshot behind the chunk was read.
	ReadAt time.Time
	// Stale marks a chunk frozen over a stale read: Free is 0, and nothing
	// about its lenders may change what has been decided.
	Stale bool
	// ReleaseToNonReclaiming is set when the entry that read the chunk opts in
	// to handing borrowed replicas back to a waiting lender Kueue does not
	// reclaim for (kueue.releaseToNonReclaimingLenders). Off, such a lender
	// only closes the cohort to new borrowing and is reported as waiting.
	ReleaseToNonReclaiming bool
}

// key identifies a chunk across providers that read the same cohort.
func (c SharedPoolChunk) key() string {
	return c.Cohort + "/" + c.Flavor + "/" + c.Resource
}

// serves reports whether a replica on accelerator acc may be placed into the
// chunk: the flavor names that accelerator by identity, or names none (an
// untyped flavor, which Kueue's quickstart uses, admits any).
func (c SharedPoolChunk) serves(acc string) bool {
	return c.AcceleratorType == "" || accelerator.SameName(c.AcceleratorType, acc)
}

// HasBorrowing reports whether any constraint counts cohort borrowing. Every
// borrowing path is skipped when it does not, so a cycle without it is exactly
// the cycle before borrowing was counted.
func HasBorrowing(constraints []*ResourceConstraints) bool {
	return slices.ContainsFunc(constraints, func(c *ResourceConstraints) bool {
		return c != nil && c.NamespaceBorrow != nil
	})
}

// WithinNominal returns copies of constraints for the consumers that must never
// spend borrowed capacity: scale-from-zero, the warm pool and rescale (B7). A
// model woken onto borrowed GPUs can be preempted before it serves.
//
// Each borrowing namespace's Limit becomes its within-nominal figure and the
// borrowing figures are dropped; the reported aggregate is rebuilt from the new
// limits, as DefaultLimiter builds it. Within nominal is not the guaranteed
// figure: under Kueue's default (reclaimWithinCohort: Never) an idle queue's
// guaranteed figure is 0, and that would stop every idle namespace on a default
// setup from waking, which it can today. Constraints without borrowing are
// returned as they are, so without it this is the identity.
func WithinNominal(constraints []*ResourceConstraints) []*ResourceConstraints {
	return withoutBorrowing(constraints, true)
}

// GuaranteedOnly returns copies of constraints without the borrowing figures,
// each namespace at its guaranteed Limit: the view of the utilization-share
// optimizer, which budgets the guaranteed figure plus the borrowed GPUs a group
// holds now (its free figure clamps at 0 and adds back what the group holds), so
// held borrowed GPUs are never debt, and starts no new borrowing (B4). Its
// ClusterFree reads only the cluster bounds -- a namespace-scope provider, the
// only one that borrows, is never one -- and so never includes a shared pool.
func GuaranteedOnly(constraints []*ResourceConstraints) []*ResourceConstraints {
	return withoutBorrowing(constraints, false)
}

// withoutBorrowing drops the borrowing figures from every constraint carrying
// them, moving each borrowing namespace's Limit to its within-nominal figure
// when within is set.
func withoutBorrowing(constraints []*ResourceConstraints, within bool) []*ResourceConstraints {
	if !HasBorrowing(constraints) {
		return constraints
	}
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil {
			out = append(out, c)
			continue
		}
		cc := *c
		cc.NamespacePools = make(map[string]map[string]ResourcePool, len(c.NamespacePools))
		for ns, perType := range c.NamespacePools {
			pools := maps.Clone(perType)
			if within {
				for accType, b := range c.NamespaceBorrow[ns] {
					if p, ok := pools[accType]; ok {
						p.Limit = b.WithinNominal
						pools[accType] = p
					}
				}
			}
			cc.NamespacePools[ns] = pools
		}
		cc.NamespaceBorrow, cc.SharedPools, cc.NamespaceCohorts = nil, nil, nil
		cc.Pools = aggregateNamespacePools(cc.NamespacePools)
		cc.TotalLimit, cc.TotalUsed, cc.TotalAvail = poolTotals(cc.Pools)
		out = append(out, &cc)
	}
	return out
}

// reachPools is nsPools with each borrowing type's Limit raised by its cap: the
// most the namespace may reach. An unlimited cap is the unlimited sentinel.
// Used for the reported aggregate of a borrowing provider and for the greedy
// optimizer's upper bound per model.
func reachPools(nsPools map[string]map[string]ResourcePool, borrow map[string]map[string]NamespaceBorrow) map[string]map[string]ResourcePool {
	out := make(map[string]map[string]ResourcePool, len(nsPools))
	for ns, perType := range nsPools {
		pools := maps.Clone(perType)
		for accType, b := range borrow[ns] {
			p, ok := pools[accType]
			if !ok || p.Limit < 0 {
				continue
			}
			if b.Cap < 0 {
				p.Limit = config.QuotaUnlimited
			} else {
				p.Limit += b.Cap
			}
			pools[accType] = p
		}
		out[ns] = pools
	}
	return out
}

// reachConstraints returns copies of the borrowing constraints with each
// namespace at its reach, for the greedy optimizer's per-model upper bound.
func reachConstraints(constraints []*ResourceConstraints) []*ResourceConstraints {
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil {
			out = append(out, c)
			continue
		}
		cc := *c
		cc.NamespacePools = reachPools(c.NamespacePools, c.NamespaceBorrow)
		out = append(out, &cc)
	}
	return out
}

// mergeSharedPools collects every provider's chunks, one per cohort, flavor and
// resource at the smallest figure any provider gives, in a stable order, and the
// namespaces' cohorts. Two Kueue-backed entries read the same cohorts; their
// chunks are one pool, not two. The merge takes the cautious side of every
// field: stale when either read is, the older read time (a lender's read is
// new only once both entries read past it), the union of the lenders -- a
// lender seen by both waits when either saw it wait, counts as reclaiming
// when either says so, and has lent out the smaller figure -- and a release
// to a non-reclaiming lender only when every entry opts in.
func mergeSharedPools(constraints []*ResourceConstraints) ([]*SharedPoolChunk, map[string][]string) {
	byKey := map[string]*SharedPoolChunk{}
	cohorts := map[string][]string{}
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil {
			continue
		}
		for _, p := range c.SharedPools {
			if prev, ok := byKey[p.key()]; ok {
				mergeChunk(prev, p)
				continue
			}
			chunk := p
			chunk.Lenders = slices.Clone(p.Lenders)
			byKey[p.key()] = &chunk
		}
		for ns, cs := range c.NamespaceCohorts {
			for _, co := range cs {
				if !slices.Contains(cohorts[ns], co) {
					cohorts[ns] = append(cohorts[ns], co)
				}
			}
		}
	}
	chunks := slices.Collect(maps.Values(byKey))
	slices.SortFunc(chunks, func(a, b *SharedPoolChunk) int {
		return cmp.Or(strings.Compare(a.Cohort, b.Cohort), strings.Compare(a.Flavor, b.Flavor), strings.Compare(a.Resource, b.Resource))
	})
	return chunks, cohorts
}

// mergeChunk folds p, another provider's read of the same chunk, into prev
// (mergeSharedPools).
func mergeChunk(prev *SharedPoolChunk, p SharedPoolChunk) {
	prev.Free = min(prev.Free, p.Free)
	prev.Stale = prev.Stale || p.Stale
	prev.ReleaseToNonReclaiming = prev.ReleaseToNonReclaiming && p.ReleaseToNonReclaiming
	if !p.ReadAt.IsZero() && (prev.ReadAt.IsZero() || p.ReadAt.Before(prev.ReadAt)) {
		prev.ReadAt = p.ReadAt
	}
	for _, l := range p.Lenders {
		i := slices.IndexFunc(prev.Lenders, func(o kueue.LenderState) bool { return o.Queue == l.Queue })
		if i < 0 {
			prev.Lenders = append(prev.Lenders, l)
			continue
		}
		o := &prev.Lenders[i]
		o.Waiting = o.Waiting || l.Waiting
		o.CanReclaim = o.CanReclaim || l.CanReclaim
		o.LentOut = min(o.LentOut, l.LentOut)
		o.PendingGPUs = min(o.PendingGPUs, l.PendingGPUs)
	}
	slices.SortFunc(prev.Lenders, func(a, b kueue.LenderState) int { return strings.Compare(a.Queue, b.Queue) })
}

// releaseBound is the GPUs a waiting lender may be handed back: the request of
// the pending Workload it waits for, at most what it lent out. The release
// loop (lenderReleases) rounds it up to whole units.
func releaseBound(l kueue.LenderState) int {
	return max(0, min(l.PendingGPUs, l.LentOut))
}
