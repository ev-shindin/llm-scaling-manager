package kueue

import (
	"cmp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// Labels and scheduling gates Kueue puts on the pods it manages. The managed
// check that reads them is pod-side and belongs to the engine, not the reader.
const (
	// ManagedLabel marks a pod Kueue manages through the pod integration.
	ManagedLabel = "kueue.x-k8s.io/managed"
	// QueueNameLabel names the LocalQueue a workload asks for. It is a request,
	// not proof that Kueue manages the object.
	QueueNameLabel = "kueue.x-k8s.io/queue-name"
	// AdmissionGate holds a managed pod until Kueue admits its workload.
	AdmissionGate = "kueue.x-k8s.io/admission"
	// TopologyGate holds a managed pod until Topology-Aware Scheduling assigns it.
	TopologyGate = "kueue.x-k8s.io/topology"
)

// Reclaim policies of a ClusterQueue's spec.preemption.reclaimWithinCohort.
// Absent means ReclaimNever.
const (
	ReclaimNever         = "Never"
	ReclaimLowerPriority = "LowerPriority"
	ReclaimAny           = "Any"
)

// Reason says why borrowing is not counted for a queue, or qualifies its
// guaranteed figure. The empty Reason means borrowing is counted.
type Reason string

// Reasons a queue's borrowable figure is 0, and the one that qualifies its
// guaranteed figure.
const (
	// ReasonStale: the snapshot is not fresh; the last read failed.
	ReasonStale Reason = "stale"
	// ReasonNoStatus: a queue in the cohort has no reservation status, so its
	// usage is unknown. Kueue CRDs installed without a running Kueue look so.
	ReasonNoStatus Reason = "no-status"
	// ReasonHierarchicalCohort: the cohort has a parent, its own
	// resourceGroups, or is named as a parent by another Cohort.
	ReasonHierarchicalCohort Reason = "hierarchical-cohort"
	// ReasonHierarchyUnknown: the Cohort objects could not be listed, so a flat
	// cohort cannot be told apart from a nested one.
	ReasonHierarchyUnknown Reason = "hierarchy-unknown"
	// ReasonEffectiveQuotas: a queue in the cohort carries
	// status.effectiveQuotas, which Kueue then schedules from instead of spec.
	ReasonEffectiveQuotas Reason = "effective-quotas"
	// ReasonFairSharing: Kueue Fair Sharing is on (status.fairSharing).
	ReasonFairSharing Reason = "fair-sharing"
	// ReasonNoCohort: the queue is in no cohort, so there is nothing to borrow.
	ReasonNoCohort Reason = "no-cohort"
	// ReasonInactive: the queue admits nothing (a stopPolicy holds it, or it
	// names a flavor the cluster lacks).
	ReasonInactive Reason = "inactive"
	// ReasonMalformed: a borrowing field of a queue in the cohort did not parse.
	ReasonMalformed Reason = "malformed"
	// ReasonNominalLentOut qualifies a guaranteed figure below nominal: the
	// queue cannot reclaim, and a peer holds the rest of its nominal quota.
	ReasonNominalLentOut Reason = "nominal-lent-out"
)

// Reasons decided per namespace on the engine side, from the namespace's own
// pods and ResourceQuotas rather than from the Kueue read (B1).
const (
	// ReasonNotKueueManaged: a GPU pod in the namespace is not managed by
	// Kueue, so Kueue's usage misses GPUs serving holds and the cohort looks
	// idler than it is. Also the first answer for a namespace with no GPU pods.
	ReasonNotKueueManaged Reason = "not-kueue-managed"
	// ReasonNoGPUBound: nothing checks that borrowed GPUs exist: no physical
	// limiter, and no namespace ResourceQuota on a GPU resource at most
	// nominal plus borrowingLimit.
	ReasonNoGPUBound Reason = "no-gpu-bound"
	// ReasonResourceQuotaUnreadable: the namespace's ResourceQuotas could not
	// be read. Never read as "no ResourceQuota".
	ReasonResourceQuotaUnreadable Reason = "resourcequota-unreadable"
)

// ResourceKey names one GPU resource of one flavor.
type ResourceKey struct {
	Flavor   string
	Resource string
}

// QueueResource is one ClusterQueue's quota and reservation for one flavor and
// GPU resource. Figures are whole GPUs.
type QueueResource struct {
	// Nominal is spec nominalQuota.
	Nominal int
	// LendingLimit is spec lendingLimit; nil when unset (everything is lendable).
	LendingLimit *int
	// BorrowingLimit is spec borrowingLimit; nil when unset (no limit).
	BorrowingLimit *int
	// Reserved is status.flavorsReservation total: what Kueue has charged.
	Reserved int
	// Borrowed is status.flavorsReservation borrowed: Kueue's own figure, kept
	// as a cross-check on the computed one.
	Borrowed int
}

// QueueState is everything about one ClusterQueue that borrowing depends on.
type QueueState struct {
	Name string
	// Cohort is spec.cohortName (spec.cohort in v1beta1); empty for none.
	Cohort string
	// Reclaim is spec.preemption.reclaimWithinCohort; ReclaimNever when absent.
	Reclaim string
	// Pending is status.pendingWorkloads.
	Pending int
	// HasStatus is false when Kueue never wrote status.flavorsReservation.
	HasStatus bool
	// FairSharing is true when status.fairSharing is present.
	FairSharing bool
	// EffectiveQuotas is true when status.effectiveQuotas is present.
	EffectiveQuotas bool
	// Inactive is true for a queue Kueue admits nothing through.
	Inactive bool
	// Malformed is true when a borrowing field failed to parse.
	Malformed bool
	// Resources are the GPU resources the queue declares, per flavor.
	Resources map[ResourceKey]QueueResource
}

// CanReclaim reports whether the queue may preempt borrowers to get its
// nominal quota back.
func (q QueueState) CanReclaim() bool {
	return q.Reclaim == ReclaimLowerPriority || q.Reclaim == ReclaimAny
}

// CohortState is one Cohort object. A cohort named by a ClusterQueue with no
// Cohort object behind it has none, and is flat.
type CohortState struct {
	Name string
	// Parent is spec.parentName.
	Parent string
	// HasResourceGroups is true when the Cohort declares quota of its own.
	HasResourceGroups bool
}

// FlavorPlacement is where a flavor's pods may run: Kueue pins them to its
// node labels and tolerations.
type FlavorPlacement struct {
	// AcceleratorType is the product named by NodeLabels; empty when none is.
	AcceleratorType string
	NodeLabels      map[string]string
	Tolerations     []corev1.Toleration
}

// Topology is one read of everything borrowing depends on.
type Topology struct {
	Queues  map[string]QueueState
	Cohorts map[string]CohortState
	// CohortsUnreadable is set when the Cohort objects could not be listed.
	// Never read as "no Cohort objects": that would call every cohort flat.
	CohortsUnreadable bool
	Flavors           map[string]FlavorPlacement
}

// QueueFigures is what one ClusterQueue could reach for one flavor and GPU
// resource. Guaranteed plus Borrowable is the reach.
type QueueFigures struct {
	ResourceKey
	Nominal int
	// Guaranteed is the part of the reach that does not depend on the cohort.
	Guaranteed int
	// Borrowable is how far past Guaranteed the queue could reach; 0 whenever
	// Reason is set.
	Borrowable int
	// BorrowCap is the borrowing limit, config.QuotaUnlimited when unset.
	BorrowCap int
	// WithinNominal is what the queue could reach without borrowing (B7):
	// Nominal for a queue that can reclaim, min(N, max(U, G) + cohortFree) for
	// one that cannot. Nominal whenever Reason is set.
	WithinNominal int
	// LentOut is, for a counted queue that can reclaim, the part of its nominal
	// a peer may hold now: N - max(U, G). Growth into it is guaranteed but
	// debits the cohort's free pool (B3). 0 otherwise.
	LentOut int
	// CohortFree is the cohort's free pool for this key; 0 when withheld.
	CohortFree int
	// KueueBorrowed is Kueue's own borrowed figure, a cross-check.
	KueueBorrowed int
	// Reason is why Borrowable is 0; empty when borrowing is counted.
	Reason Reason
	// GuaranteedReason is ReasonNominalLentOut when Guaranteed is below
	// Nominal, empty otherwise.
	GuaranteedReason Reason
}

// SharedPool is one cohort's free capacity of one flavor and GPU resource,
// spent once across every queue in the cohort.
type SharedPool struct {
	Cohort string
	ResourceKey
	FlavorPlacement
	// Free is the cohort's free pool; 0 when Reason is set.
	Free   int
	Reason Reason
	// WaitingLenders are the member queues that, in this read, have pending
	// workloads and reserve less than their nominal quota of this key. This is
	// the raw per-read predicate; debouncing it is the caller's.
	WaitingLenders []string
	// Lenders is every active member queue that declares this key, with what
	// the release of borrowed capacity (B6) needs about it.
	Lenders []LenderState
}

// LenderState is one cohort member as a lender of one flavor and GPU resource.
type LenderState struct {
	Queue string
	// Waiting is LenderWaiting in this read, not debounced.
	Waiting bool
	// CanReclaim is whether Kueue preempts borrowers for it.
	CanReclaim bool
	// LentOut is the nominal it does not hold now: max(0, N - U), what it may
	// be waiting for.
	LentOut int
}

// Borrowing is the result of ComputeBorrowing.
type Borrowing struct {
	// Queues holds per ClusterQueue the figures of each GPU resource it
	// declares, sorted by flavor and resource.
	Queues map[string][]QueueFigures
	// Pools holds one SharedPool per cohort, flavor and GPU resource, sorted.
	Pools []SharedPool
}

// LenderWaiting reports whether q, in this read, has pending workloads while
// reserving less than its nominal quota of key.
func LenderWaiting(q QueueState, key ResourceKey) bool {
	r, ok := q.Resources[key]
	return ok && q.Pending > 0 && r.Reserved < r.Nominal
}

// lent returns L = min(lendingLimit, nominal), nominal when unset, and the kept
// part G = nominal - L.
func (r QueueResource) lent() (lent, kept int) {
	lent = r.Nominal
	if r.LendingLimit != nil {
		lent = min(max(*r.LendingLimit, 0), r.Nominal)
	}
	return lent, r.Nominal - lent
}

// ComputeBorrowing derives, from one read, what each ClusterQueue could reach
// in its cohort. Per cohort, flavor and GPU resource, over every member m that
// declares it, inactive ones included (see cohortFree):
//
//	L_m = min(lendingLimit, N_m)   (unset: N_m)
//	G_m = N_m - L_m                kept by m, never lent
//	cohortFree = max(0, Σ L_m − Σ max(0, U_m − G_m))
//
// and for a queue Q, with base = max(U_Q, G_Q):
//
//	reach = min(N_Q + B_Q, base + cohortFree)        reclaimWithinCohort Never
//	reach = max(N_Q, min(N_Q + B_Q, base + cohortFree))  otherwise
//
// Guaranteed is N_Q for a queue that can reclaim, min(N_Q, base) for one that
// cannot; Borrowable is reach minus guaranteed. Where borrowing cannot be
// counted the figures fall back to Guaranteed = N_Q, Borrowable = 0, with the
// reason. stale marks a snapshot that is not fresh.
func ComputeBorrowing(t Topology, stale bool) Borrowing {
	members := make(map[string][]QueueState)
	for _, q := range t.Queues {
		if q.Cohort != "" {
			members[q.Cohort] = append(members[q.Cohort], q)
		}
	}
	cohortReason := make(map[string]Reason, len(members))
	for cohort, qs := range members {
		cohortReason[cohort] = withheldReason(t, cohort, qs, stale)
	}

	out := Borrowing{Queues: make(map[string][]QueueFigures, len(t.Queues))}
	for name, q := range t.Queues {
		reason := ReasonNoCohort
		switch {
		case stale:
			reason = ReasonStale
		case q.Inactive:
			reason = ReasonInactive
		case q.Cohort != "":
			reason = cohortReason[q.Cohort]
		}
		figs := make([]QueueFigures, 0, len(q.Resources))
		for key, r := range q.Resources {
			f := QueueFigures{
				ResourceKey:   key,
				Nominal:       r.Nominal,
				Guaranteed:    r.Nominal,
				WithinNominal: r.Nominal,
				BorrowCap:     config.QuotaUnlimited,
				KueueBorrowed: r.Borrowed,
				Reason:        reason,
			}
			if r.BorrowingLimit != nil {
				f.BorrowCap = max(*r.BorrowingLimit, 0)
			}
			if reason == "" {
				f.CohortFree = cohortFree(members[q.Cohort], key)
				f.Guaranteed, f.Borrowable = reach(q, r, f.CohortFree)
				f.WithinNominal, f.LentOut = withinNominal(q, r, f.CohortFree)
				if f.Guaranteed < r.Nominal {
					f.GuaranteedReason = ReasonNominalLentOut
				}
			}
			figs = append(figs, f)
		}
		slices.SortFunc(figs, func(a, b QueueFigures) int { return compareKeys(a.ResourceKey, b.ResourceKey) })
		out.Queues[name] = figs
	}

	for cohort, qs := range members {
		keys := map[ResourceKey]bool{}
		for _, q := range qs {
			if q.Inactive {
				continue
			}
			for key := range q.Resources {
				keys[key] = true
			}
		}
		for key := range keys {
			p := SharedPool{Cohort: cohort, ResourceKey: key, FlavorPlacement: t.Flavors[key.Flavor], Reason: cohortReason[cohort]}
			if p.Reason == "" {
				p.Free = cohortFree(qs, key)
			}
			for _, q := range qs {
				r, declares := q.Resources[key]
				if q.Inactive || !declares {
					continue
				}
				waiting := LenderWaiting(q, key)
				if waiting {
					p.WaitingLenders = append(p.WaitingLenders, q.Name)
				}
				p.Lenders = append(p.Lenders, LenderState{Queue: q.Name, Waiting: waiting,
					CanReclaim: q.CanReclaim(), LentOut: max(0, r.Nominal-r.Reserved)})
			}
			slices.Sort(p.WaitingLenders)
			slices.SortFunc(p.Lenders, func(a, b LenderState) int { return strings.Compare(a.Queue, b.Queue) })
			out.Pools = append(out.Pools, p)
		}
	}
	slices.SortFunc(out.Pools, func(a, b SharedPool) int {
		return cmp.Or(strings.Compare(a.Cohort, b.Cohort), compareKeys(a.ResourceKey, b.ResourceKey))
	})
	return out
}

// withheldReason is why borrowing cannot be counted anywhere in a cohort, or
// empty. The checks run in order, so the reason shown is the first that holds.
func withheldReason(t Topology, cohort string, qs []QueueState, stale bool) Reason {
	if stale {
		return ReasonStale
	}
	for _, q := range qs {
		// Inactive members too: their usage counts in the free pool.
		if !q.HasStatus {
			return ReasonNoStatus
		}
	}
	if t.CohortsUnreadable {
		return ReasonHierarchyUnknown
	}
	if c, ok := t.Cohorts[cohort]; ok && (c.Parent != "" || c.HasResourceGroups) {
		return ReasonHierarchicalCohort
	}
	for _, c := range t.Cohorts {
		if c.Parent == cohort {
			// A root that looks flat while a child lends into it.
			return ReasonHierarchicalCohort
		}
	}
	for _, q := range qs {
		if q.EffectiveQuotas {
			return ReasonEffectiveQuotas
		}
	}
	for _, q := range qs {
		if q.FairSharing {
			return ReasonFairSharing
		}
	}
	for _, q := range qs {
		if q.Malformed {
			return ReasonMalformed
		}
	}
	return ""
}

// cohortFree is max(0, Σ L_m − Σ max(0, U_m − G_m)) over every member that
// declares key, inactive ones included. Kueue's cache accumulates every
// ClusterQueue into its cohort, and the scheduling snapshot clones the cohort's
// figures before it drops the inactive queues, so an inactive queue's lending
// and its usage both still count: a held queue keeps its admitted workloads
// running, and what they borrowed is not free. An inactive queue is left out
// only as a borrower (ReasonInactive) and as a lender that waits.
func cohortFree(qs []QueueState, key ResourceKey) int {
	var lent, used int64
	for _, q := range qs {
		r, ok := q.Resources[key]
		if !ok {
			continue
		}
		l, g := r.lent()
		lent += int64(l)
		used += int64(max(0, r.Reserved-g))
	}
	return clampQuota(max(0, lent-used))
}

// reach returns a counted queue's guaranteed and borrowable figures for one
// key, given the cohort's free pool.
func reach(q QueueState, r QueueResource, free int) (guaranteed, borrowable int) {
	_, kept := r.lent()
	base := max(r.Reserved, kept)
	top := addCapped(base, free)
	if r.BorrowingLimit != nil {
		top = min(top, addCapped(r.Nominal, max(*r.BorrowingLimit, 0)))
	}
	if q.CanReclaim() {
		return r.Nominal, max(top, r.Nominal) - r.Nominal
	}
	guaranteed = min(r.Nominal, base)
	return guaranteed, max(0, top-guaranteed)
}

// withinNominal returns what a counted queue could reach without borrowing, and
// the part of its nominal a peer may hold now (B3, B7).
//
// A queue that can reclaim reaches its nominal whatever its peers hold, since
// Kueue preempts them; the gap between what it keeps or uses and its nominal is
// lent out, and growing into it takes those GPUs back from the free pool. A
// queue that cannot reclaim reaches only what Kueue would admit now without
// borrowing: what it keeps or uses, plus the free pool, never past nominal.
// Using its guaranteed figure here instead would stop an idle queue on Kueue's
// default (Never) from ever waking.
func withinNominal(q QueueState, r QueueResource, free int) (within, lentOut int) {
	_, kept := r.lent()
	base := max(r.Reserved, kept)
	if q.CanReclaim() {
		return r.Nominal, max(0, r.Nominal-base)
	}
	return min(r.Nominal, addCapped(base, free)), 0
}

// compareKeys orders resource keys by flavor, then resource.
func compareKeys(a, b ResourceKey) int {
	return cmp.Or(strings.Compare(a.Flavor, b.Flavor), strings.Compare(a.Resource, b.Resource))
}
