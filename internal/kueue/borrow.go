package kueue

import (
	"cmp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
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
	// ReasonTopologyAware: a queue in the cohort declares a GPU resource of a
	// flavor with Topology-Aware Scheduling on (spec.topologyName). Kueue then
	// places the pods itself, by topology, which the node fit here does not
	// model.
	ReasonTopologyAware Reason = "topology-aware"
	// ReasonAdmissionChecks: a queue in the cohort admits only after admission
	// checks (spec.admissionChecks or spec.admissionChecksStrategy), MultiKueue
	// included. Quota reserved is then not admission, and a check may send the
	// workload to another cluster.
	ReasonAdmissionChecks Reason = "admission-checks"
	// ReasonKueueVersionUntested: the Kueue controller runs a version outside
	// TestedKueueVersions, so the fields and semantics borrowing relies on
	// (kueue_constants.go) are not known to hold. The nominal quotas still
	// apply.
	ReasonKueueVersionUntested Reason = "kueue-version-untested"
	// ReasonKueueControllerDown: the Kueue controller's Deployment reports no
	// ready replica, or is gone, while Kueue's API still answers
	// (ControllerVersion.Down). Nothing admits or preempts, and queue status
	// stops moving, so no new borrowing is counted; what is borrowed is frozen,
	// not shrunk. Decided on the engine side from
	// config.ExternalQuotas.KueueControllerDown, not carried on the rows.
	ReasonKueueControllerDown Reason = "kueue-controller-down"
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
	// ReasonNotSelected: the quota entry's kueue.borrowNamespaces does not
	// list the namespace, so its borrowing is not counted (a pilot).
	ReasonNotSelected Reason = "not-selected"
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
	// Pending is status.pendingWorkloads: every pending Workload, whatever it
	// asks for. Kept for the log; the lender signal reads PendingWorkloads.
	Pending int
	// PendingWorkloads are the queue's pending Workloads that request a GPU
	// resource, read from the Workloads themselves (readPending).
	PendingWorkloads []PendingWorkload
	// ReservedPriority is the highest spec.priority of a Workload Kueue has
	// reserved quota for in the queue; nil when none was read.
	ReservedPriority *int32
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
	// AdmissionChecks is true when the queue names admission checks, in
	// spec.admissionChecks or spec.admissionChecksStrategy.
	AdmissionChecks bool
	// Resources are the GPU resources the queue declares, per flavor.
	Resources map[ResourceKey]QueueResource
}

// CanReclaim reports whether the queue may preempt borrowers to get its
// nominal quota back, whatever they run: only under ReclaimAny.
//
// ReclaimLowerPriority is read as ReclaimNever for the guaranteed and
// within-nominal figures: Kueue then preempts only borrowed workloads of lower
// priority than the one it admits, and with serving above batch (the
// documented setup) a batch lender never reclaims from serving. As a lender
// that waits, a LowerPriority queue whose pending Workload outranks every
// borrower's reserved Workload is one Kueue reclaims for
// (LenderState.CanReclaim; ComputeBorrowing).
func (q QueueState) CanReclaim() bool {
	return q.Reclaim == ReclaimAny
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
	// TopologyName is spec.topologyName: set, Kueue places the flavor's pods
	// by Topology-Aware Scheduling.
	TopologyName string
}

// Topology is one read of everything borrowing depends on.
type Topology struct {
	Queues  map[string]QueueState
	Cohorts map[string]CohortState
	// CohortsUnreadable is set when the Cohort objects could not be listed.
	// Never read as "no Cohort objects": that would call every cohort flat.
	CohortsUnreadable bool
	Flavors           map[string]FlavorPlacement
	// VersionUntested is set when the Kueue controller runs a version outside
	// TestedKueueVersions. An unknown version does not set it.
	VersionUntested bool
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
	// WaitingLenders are the member queues that, in this read, wait for this
	// key (LenderWaiting). This is the raw per-read predicate; debouncing it
	// is the caller's.
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
	// CanReclaim is whether Kueue preempts borrowers for it: reclaimWithinCohort
	// Any, or LowerPriority while its pending Workload outranks the highest
	// priority Kueue reserved quota for in a queue of the cohort that borrows
	// this key. A LowerPriority lender is read as one that cannot reclaim
	// otherwise, also when no borrower's priority could be read.
	CanReclaim bool
	// LentOut is the nominal it does not hold now: max(0, N - U), the most it
	// may be waiting for.
	LentOut int
	// PendingGPUs is what it waits for now: the GPU request of its largest
	// pending Workload that asks for this resource and fits within LentOut
	// (lenderPending); 0 when it does not wait. It bounds what is released to
	// it, which the caller rounds up to whole replicas.
	PendingGPUs int
}

// Borrowing is the result of ComputeBorrowing.
type Borrowing struct {
	// Queues holds per ClusterQueue the figures of each GPU resource it
	// declares, sorted by flavor and resource.
	Queues map[string][]QueueFigures
	// Pools holds one SharedPool per cohort, flavor and GPU resource, sorted.
	Pools []SharedPool
}

// LenderWaiting reports whether q, in this read, waits for capacity of key it
// lent: a pending Workload of it requests the key's GPU resource and fits
// within its unused nominal quota of key (lenderPending). A pending count
// alone is not enough: anyone who may create a Workload in a lender's
// namespace -- a CPU job, or one too large to admit -- would otherwise make
// the borrowers give capacity back for nothing.
func LenderWaiting(q QueueState, key ResourceKey) bool {
	gpus, _ := lenderPending(q, key)
	return gpus > 0
}

// borrowerPriority is the highest priority Kueue reserved quota for in a
// member of qs other than lender that borrows key now (usage past nominal, or
// Kueue's own borrowed figure). known is false when no such queue's priority
// was read.
func borrowerPriority(qs []QueueState, lender string, key ResourceKey) (priority int32, known bool) {
	for _, q := range qs {
		r, ok := q.Resources[key]
		if q.Name == lender || !ok || (r.Reserved <= r.Nominal && r.Borrowed <= 0) || q.ReservedPriority == nil {
			continue
		}
		if !known || *q.ReservedPriority > priority {
			priority = *q.ReservedPriority
		}
		known = true
	}
	return priority, known
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
//	reach = min(N_Q + B_Q, base + cohortFree)        reclaimWithinCohort Never or LowerPriority
//	reach = max(N_Q, min(N_Q + B_Q, base + cohortFree))  reclaimWithinCohort Any
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
				pending, priority := lenderPending(q, key)
				waiting := pending > 0
				if waiting {
					p.WaitingLenders = append(p.WaitingLenders, q.Name)
				}
				canReclaim := q.CanReclaim()
				if q.Reclaim == ReclaimLowerPriority && waiting {
					// Kueue preempts borrowed work of lower priority than the
					// Workload it admits: when the lender's outranks every
					// borrower's, Kueue takes the capacity back itself.
					borrower, known := borrowerPriority(qs, q.Name, key)
					canReclaim = known && priority > borrower
				}
				p.Lenders = append(p.Lenders, LenderState{Queue: q.Name, Waiting: waiting,
					CanReclaim: canReclaim, LentOut: max(0, r.Nominal-r.Reserved), PendingGPUs: pending})
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
	if t.VersionUntested {
		return ReasonKueueVersionUntested
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
		for key := range q.Resources {
			if t.Flavors[key.Flavor].TopologyName != "" {
				return ReasonTopologyAware
			}
		}
	}
	for _, q := range qs {
		if q.AdmissionChecks {
			return ReasonAdmissionChecks
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

// BorrowDrift is, per ClusterQueue in a cohort whose reservation status Kueue
// has written, the GPUs by which the borrowed figure computed here (usage
// past nominal, per flavor and GPU resource) differs from Kueue's own
// status.flavorsReservation[].borrowed, summed over the queue's GPU resources
// as absolute differences. It is the conformance check of this package's
// reading of Kueue: 0 while the two agree (wva_kueue_borrow_drift_gpus, and an
// e2e assertion against a live Kueue).
func BorrowDrift(t Topology) map[string]int {
	out := map[string]int{}
	for name, q := range t.Queues {
		if q.Cohort == "" || !q.HasStatus {
			continue
		}
		drift := 0
		for _, r := range q.Resources {
			computed := max(0, r.Reserved-r.Nominal)
			drift = addCapped(drift, max(computed-r.Borrowed, r.Borrowed-computed))
		}
		out[name] = drift
	}
	return out
}

// compareKeys orders resource keys by flavor, then resource.
func compareKeys(a, b ResourceKey) int {
	return cmp.Or(strings.Compare(a.Flavor, b.Flavor), strings.Compare(a.Resource, b.Resource))
}
