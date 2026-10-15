package allocation

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// QuotaInventory enforces operator-declared GPU quotas independent of physical
// cluster inventory. It implements the Inventory interface for use in the
// limiter pipeline. Each instance is tied to exactly one QuotaLimiterConfig
// scope — a deployment that needs both cluster and namespace caps declares two
// entries, and NewLimiterFromConfig groups them in a CompositeLimiter.
//
// Quota values come from the entry's static maps and, when the entry names one,
// an external QuotaSource (Kueue): Refresh reads the source and installs the
// static entry bounded by it (config.QuotaLimiterConfig.BoundBy) as the effective
// config. Without a source Refresh is a no-op. Usage is tracked the same way as
// TypeInventory: callers invoke SetUsed (or SetUsedByNamespace) before each
// cycle, then read the pools.
//
// QuotaInventory is decoupled from physical inventory. When a policy also
// declares a physical limiter, NewLimiterFromConfig builds both and the
// optimizer intersects their constraints, so allocations are bounded by
// min(physical, quota); a quota declared alone enforces no physical bound.
type QuotaInventory struct {
	name string
	// static is the entry as configured, immutable after construction.
	static config.QuotaLimiterConfig
	// source, when non-nil, supplies the external caps static is bounded by.
	source QuotaSource

	mu sync.RWMutex
	// cfg is the EFFECTIVE entry: static bounded by the last external snapshot,
	// or static itself when there is no source. Replaced by Refresh under mu and
	// read under mu everywhere else, because a swap between two reads within one
	// ComputeConstraints would let usage be reconciled onto one key set and pools
	// be built from another.
	cfg config.QuotaLimiterConfig
	// lastExternal fingerprints the external snapshot cfg was built from, so a
	// change is logged once rather than every cycle.
	lastExternal string
	// lastBorrow fingerprints the last borrowing view logged, for the same reason.
	lastBorrow string
	// usedByType is the cluster-scoped usage map (Scope == QuotaScopeCluster).
	usedByType map[string]int
	// usedByNS is the per-namespace usage map (Scope == QuotaScopeNamespace).
	// Outer key: namespace; inner key: accelerator type.
	usedByNS map[string]map[string]int

	// borrowOpts, set by EnableBorrowing, lets the entry count cohort borrowing
	// when it also says countBorrowable. Zero (no Eligibility) counts none.
	borrowOpts BorrowOptions
	// borrow is the borrowing view the last Refresh installed with cfg, nil
	// when no namespace's borrowing is counted. cfg then holds every counted
	// namespace at its guaranteed figure.
	borrow *borrowView
	// withheld is, per namespace the last read granted borrowing rows for and
	// whose borrowing is not counted, why (a kueue.Reason): the source's, the
	// engine-side one, or stale. Kept beside borrow, which is nil when no
	// namespace is counted.
	withheld map[string]string
	// now is the clock the staleness of a snapshot is judged by; nil is
	// time.Now.
	now func() time.Time
}

// BorrowEligibility evaluates the engine-side B1 conditions per namespace
// (kueue.Eligibility in production).
type BorrowEligibility interface {
	Evaluate(ctx context.Context, namespaces []string) map[string]kueue.NamespaceEligibility
}

// BorrowOptions turns on counting cohort borrowing for a Kueue-backed,
// namespace-scope entry with countBorrowable on.
type BorrowOptions struct {
	// Physical is set when a physical limiter is declared beside the entry: a
	// GPU bound is then in force for every namespace (B1).
	Physical bool
	// Eligibility evaluates whether Kueue manages a namespace's GPU pods and
	// reads its ResourceQuotas. Nil counts no borrowing.
	Eligibility BorrowEligibility
	// Memory, when set, keeps the last counted borrowing view across a limiter
	// rebuild, so a stale read after one still freezes. Shared per entry with
	// its Kueue reader (limiter_factory.go); nil keeps none.
	Memory *BorrowMemory
}

// borrowView is the borrowing an entry counts after one Refresh.
type borrowView struct {
	// effective is the entry with every counted namespace at its guaranteed
	// figure; kept so a stale read can freeze it.
	effective config.QuotaLimiterConfig
	byNS      map[string]map[string]NamespaceBorrow
	pools     []SharedPoolChunk
	cohorts   map[string][]string
	// withheld is, per namespace read but not counted, why.
	withheld map[string]string
	// frozen marks a view kept over a stale read: no new borrowing.
	frozen bool
}

// EnableBorrowing lets the inventory count cohort borrowing, when its entry
// reads Kueue at namespace scope and does not set countBorrowable: false.
// Without it the entry caps at nominal whatever countBorrowable says.
func (q *QuotaInventory) EnableBorrowing(opts BorrowOptions) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.borrowOpts = opts
}

// countsBorrowing reports whether this entry may count borrowing at all.
func (q *QuotaInventory) countsBorrowing() bool {
	return q.borrowOpts.Eligibility != nil && q.static.KueueCountsBorrowable() && q.static.Scope == config.QuotaScopeNamespace
}

// clock is now, or time.Now.
func (q *QuotaInventory) clock() time.Time {
	if q.now != nil {
		return q.now()
	}
	return time.Now()
}

// QuotaSource supplies caps owned by an external authority that a quota entry
// is bounded by. Quotas returns the current snapshot; on failure it returns the
// last good snapshot with the error, and a zero ObservedAt when it never had one.
type QuotaSource interface {
	Quotas(ctx context.Context) (config.ExternalQuotas, error)
}

// NewQuotaInventory constructs an inventory from a validated config entry.
// The caller is responsible for running config.QuotaLimiterEntries.Validate()
// before this call — the constructor accepts the entry as-is and trusts it.
func NewQuotaInventory(cfg config.QuotaLimiterConfig) *QuotaInventory {
	return &QuotaInventory{
		name:       cfg.Name,
		static:     cfg,
		cfg:        cfg,
		usedByType: make(map[string]int),
		usedByNS:   make(map[string]map[string]int),
	}
}

// NewQuotaInventoryWithSource constructs an inventory whose entry is bounded by
// an external source on every Refresh. A nil source is the same as
// NewQuotaInventory.
func NewQuotaInventoryWithSource(cfg config.QuotaLimiterConfig, source QuotaSource) *QuotaInventory {
	q := NewQuotaInventory(cfg)
	q.source = source
	return q
}

// Compile-time interface assertions.
var (
	_ Inventory               = (*QuotaInventory)(nil)
	_ NamespaceAwareInventory = (*QuotaInventory)(nil)
	_ UsageBasisReporter      = (*QuotaInventory)(nil)
	_ ClosedPoolsInventory    = (*QuotaInventory)(nil)
	_ BorrowingInventory      = (*QuotaInventory)(nil)
)

// BorrowingInventory is a NamespaceAwareInventory that may count cohort
// borrowing. DefaultLimiter carries its figures onto ResourceConstraints.
type BorrowingInventory interface {
	NamespaceAwareInventory

	// Borrowing returns, for the active namespaces it counts borrowing for,
	// their figures per accelerator type, the shared pools of their cohorts,
	// and every namespace's cohorts. All nil when it counts none.
	Borrowing(activeNamespaces []string) (map[string]map[string]NamespaceBorrow, []SharedPoolChunk, map[string][]string)
	// BorrowWithheld returns, for the active namespaces whose borrowing it
	// reads and does not count, why; nil when it counts none.
	BorrowWithheld(activeNamespaces []string) map[string]string
}

// PoolsClosed reports true for a cluster-scope quota, which denies every type
// the operator did not list. A namespace-scope quota's per-type pools are the
// sum of its namespace caps and deny nothing: its allowlists are per namespace
// (NamespaceResourcePools), and a namespace it excludes stays open. Scope is
// read from the static entry, which never changes after construction.
func (q *QuotaInventory) PoolsClosed() bool {
	return q.static.Scope == config.QuotaScopeCluster
}

// UsageBasis returns ManagedUsage: a quota is an allowance granted to WVA, so
// only the GPUs WVA's own variants hold may be drawn against it.
//
// Fed the physical figure instead, the cap binds on consumption it does not
// govern — an unrelated training job sharing the namespace spends the operator's
// WVA allowance without WVA having placed a single replica, and scale-up is
// refused with the allowance nominally untouched. The physical constraint still
// applies separately; that is the limiter that exists to say "the hardware is
// full".
func (q *QuotaInventory) UsageBasis() UsageBasis {
	return ManagedUsage
}

// Name returns the limiter identifier (e.g., "cluster-quota"). Used in logs
// and DecisionStep traces (the trace format is `limited by quota[scope=...]`,
// per issue #1002).
func (q *QuotaInventory) Name() string {
	return q.name
}

// Refresh re-reads the external source, if any, and installs the static entry
// bounded by its snapshot as the effective config. Without a source it is a
// no-op: the static caps have nothing to refresh from.
//
// It never returns an error, deliberately. Both engines treat a failed
// ComputeConstraints as "no constraint" and fall back to their unlimited path,
// so surfacing a Kueue outage as an error would LIFT the cap the operator
// declared — the opposite of what a quota is for. Instead:
//   - with a previous snapshot, it stays in force (the source keeps it) and the
//     error is logged with the snapshot's age;
//   - with none, the static entry alone applies, which is at least as tight as
//     no limiter and at most as loose as the operator typed, and the error is
//     logged every cycle until a read succeeds.
func (q *QuotaInventory) Refresh(ctx context.Context) error {
	if q.source == nil {
		return nil
	}
	logger := ctrl.LoggerFrom(ctx).WithValues("limiter", q.name)
	ext, err := q.source.Quotas(ctx)
	if err != nil {
		if ext.ObservedAt.IsZero() {
			logger.Error(err, "external quota source unreadable and never read; applying the static quota entry alone")
		} else {
			logger.Error(err, "external quota source unreadable; keeping its last snapshot",
				"snapshotAge", time.Since(ext.ObservedAt).Round(time.Second).String())
		}
	}
	effective := q.static
	if !ext.ObservedAt.IsZero() {
		effective = q.static.BoundBy(ext)
	}
	// Borrowing is computed outside the lock: the eligibility check may read
	// the API server. A namespace whose ResourceQuota read may pass by the
	// next cycle makes the whole read stale: it freezes like a failed read.
	var view *borrowView
	stale := false
	if q.countsBorrowing() && !ext.ObservedAt.IsZero() {
		stale = q.snapshotStale(ext, err)
		if !stale {
			view, stale = q.computeBorrowing(ctx, ext)
			if stale {
				view = nil
			}
		}
	}
	// Maps marshal with sorted keys, so equal snapshots fingerprint equal.
	fp, _ := json.Marshal(struct {
		Read bool // never-read and read-but-empty must not fingerprint alike
		N    map[string]config.ExternalCaps
		C    config.ExternalCaps
	}{!ext.ObservedAt.IsZero(), ext.Namespace, ext.Cluster})

	q.mu.Lock()
	defer q.mu.Unlock()
	if string(fp) != q.lastExternal {
		q.lastExternal = string(fp)
		if ext.ObservedAt.IsZero() {
			logger.Info("quota entry effective without external caps")
		} else {
			msg := "quota entry bounded by external caps"
			if len(ext.Namespace) == 0 && ext.Cluster.IsZero() {
				msg = "external quota source grants no GPUs; the static quota entry applies as written"
			}
			logger.Info(msg,
				"externalNamespaceQuotas", ext.Namespace, "externalClusterQuotas", ext.Cluster,
				"effectiveNamespaceQuotas", effective.NamespaceQuotas, "effectiveClusterQuotas", effective.ClusterQuotas)
			if unapplied := q.static.UnappliedUntyped(ext); len(unapplied) > 0 {
				// A real grant the limiter cannot budget under: the source could
				// not name the accelerator (a Kueue flavor with no product
				// label) and the static entry names none for these subjects
				// either. Saying so is the difference between "Kueue grants 16
				// and nothing scales" reading as a bug and as a config gap.
				logger.Info("external quota source grants GPUs of no named accelerator type; "+
					"the quota entry names no type to apply them to, so they do not bound anything. "+
					"Name the accelerator types in the entry (with -1 to let the source supply the figure)",
					"subjects", unapplied)
			}
		}
	}
	// The view to freeze from: this inventory's own, or, for an inventory
	// built by a limiter rebuild while the source was stale, the last one the
	// shared memory holds for the same entry.
	last := q.borrow
	if stale && last == nil {
		last = q.borrowOpts.Memory.load(q.memoryKey())
	}
	switch {
	case view != nil && len(view.byNS) > 0:
		effective = view.effective
	case stale && last != nil:
		// Stale reads freeze, never shrink (B6): every figure stays where it
		// was, and nothing new is borrowed. An API blip must not cause a
		// reclaim the cluster did not ask for.
		frozen := *last
		frozen.frozen = true
		frozen.pools = slices.Clone(last.pools)
		for i := range frozen.pools {
			frozen.pools[i].Free = 0
			frozen.pools[i].Stale = true
		}
		view, effective = &frozen, last.effective
	}
	q.withheld = q.withheldReasons(ext, view, stale)
	q.logBorrowing(logger, ext, view, stale)
	if view != nil && len(view.byNS) == 0 {
		// Nothing counted: the entry caps at nominal, exactly as with
		// countBorrowable: false. The view only carried the reasons to log.
		view = nil
	}
	if !stale && q.countsBorrowing() && !ext.ObservedAt.IsZero() {
		q.borrowOpts.Memory.store(q.memoryKey(), view)
	}
	q.cfg = effective
	q.borrow = view
	return nil
}

// memoryKey identifies what a remembered borrowing view was computed for: the
// static entry and whether a physical limiter is the GPU bound. A rebuild that
// changed either does not freeze at the old figures.
func (q *QuotaInventory) memoryKey() string {
	key, _ := json.Marshal(struct {
		Entry    config.QuotaLimiterConfig
		Physical bool
	}{q.static, q.borrowOpts.Physical})
	return string(key)
}

// BorrowMemory keeps the last borrowing view an entry counted from a fresh
// read, beside the entry's shared Kueue reader, so it outlives the
// QuotaInventory that computed it. A limiter rebuilt while the source is stale
// starts with no view of its own; without the memory it would fall to the
// nominal cap, the shrink a stale read must never cause. The zero value and a
// nil *BorrowMemory remember nothing.
type BorrowMemory struct {
	mu   sync.Mutex
	key  string
	view *borrowView
}

// store remembers view (nil: nothing counted) for key.
func (m *BorrowMemory) store(key string, view *borrowView) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.key, m.view = key, view
}

// load returns the view remembered for key, or nil.
func (m *BorrowMemory) load(key string) *borrowView {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.key != key {
		return nil
	}
	return m.view
}

// snapshotStale reports whether a Kueue snapshot is stale (B1): the last read
// failed, its borrowing half failed (a transient Cohort list error), or the
// snapshot is older than twice the refresh interval.
func (q *QuotaInventory) snapshotStale(ext config.ExternalQuotas, readErr error) bool {
	return readErr != nil || ext.BorrowStale || q.clock().Sub(ext.ObservedAt) > 2*q.static.KueueRefreshInterval()
}

// computeBorrowing derives the borrowing this entry counts from a fresh
// snapshot. A view with no counted namespace (empty byNS) carries only the
// reasons, and the entry then caps at nominal exactly as it does with
// countBorrowable: false.
//
// A namespace's borrowing is counted only when every B1 condition holds: the
// source's (fresh, status written, flat cohort, no effectiveQuotas, no Fair
// Sharing -- a row whose borrowing is withheld carries the reason and nominal
// figures), and the engine-side ones evaluated here (Kueue manages every GPU pod
// in the namespace, its ResourceQuotas are readable, and a GPU bound is in
// force). A counted namespace's cap becomes its guaranteed figure, bounded by
// the static entry as the nominal figure is; every other namespace keeps the
// nominal figure.
//
// stale reports that a namespace's engine-side answer was kueue.ReasonStale (a
// read that may pass): the caller then treats the whole read as stale.
func (q *QuotaInventory) computeBorrowing(ctx context.Context, ext config.ExternalQuotas) (view *borrowView, stale bool) {
	candidates := make([]string, 0, len(ext.Borrow))
	for ns, rows := range ext.Borrow {
		if ns == config.QuotaLimiterReservedNamespaceKey || q.static.IsExcluded(ns) || !slices.ContainsFunc(rows, config.ExternalBorrow.Counted) {
			continue
		}
		candidates = append(candidates, ns)
	}
	slices.Sort(candidates)
	elig := q.borrowOpts.Eligibility.Evaluate(ctx, candidates)

	view = &borrowView{withheld: map[string]string{}, cohorts: map[string][]string{}}
	counted := map[string]bool{}
	for _, ns := range candidates {
		e := elig[ns]
		stale = stale || e.Reason == kueue.ReasonStale
		reason := string(e.Reason)
		if reason == "" && !q.borrowOpts.Physical && !resourceQuotaBounds(ext.Borrow[ns], e.GPUQuota) {
			reason = string(kueue.ReasonNoGPUBound)
		}
		if reason != "" {
			view.withheld[ns] = reason
			continue
		}
		counted[ns] = true
	}
	if len(counted) == 0 || stale {
		return view, stale
	}

	at := func(figures map[string]config.ExternalCaps) config.ExternalQuotas {
		out := ext
		out.Namespace = maps.Clone(ext.Namespace)
		for ns := range counted {
			if caps, ok := figures[ns]; ok {
				out.Namespace[ns] = caps
			}
		}
		return out
	}
	view.effective = q.static.BoundBy(at(ext.NamespaceGuaranteed))
	within := q.static.BoundBy(at(ext.NamespaceWithinNominal))

	view.byNS = map[string]map[string]NamespaceBorrow{}
	for ns := range counted {
		perType := map[string]NamespaceBorrow{}
		for accType, guaranteed := range view.effective.NamespaceQuotas[ns] {
			b, ok := q.namespaceBorrow(ns, accType, guaranteed, ext.Borrow[ns], elig[ns].GPUQuota)
			if !ok {
				continue
			}
			if key, found := accelerator.FindKey(within.NamespaceQuotas[ns], accType); found {
				b.WithinNominal = within.NamespaceQuotas[ns][key]
			}
			perType[accType] = b
		}
		view.byNS[ns] = perType
	}
	inCohort := map[string]bool{}
	for ns, rows := range ext.Borrow {
		for _, r := range rows {
			if r.Cohort == "" || !r.Counted() || slices.Contains(view.cohorts[ns], r.Cohort) {
				continue
			}
			view.cohorts[ns] = append(view.cohorts[ns], r.Cohort)
			if counted[ns] {
				inCohort[r.Cohort] = true
			}
		}
	}
	for _, p := range ext.SharedPools {
		if p.Reason != "" || !inCohort[p.Cohort] {
			continue
		}
		lenders := make([]kueue.LenderState, 0, len(p.Lenders))
		for _, l := range p.Lenders {
			lenders = append(lenders, kueue.LenderState{Queue: l.Queue, Waiting: l.Waiting, CanReclaim: l.CanReclaim, LentOut: l.LentOut})
		}
		view.pools = append(view.pools, SharedPoolChunk{
			Cohort: p.Cohort, Flavor: p.Flavor, Resource: p.Resource, Free: p.Free,
			Lenders: lenders, ReadAt: ext.ObservedAt,
			FlavorPlacement: kueue.FlavorPlacement{
				AcceleratorType: p.Accelerator, NodeLabels: maps.Clone(p.NodeLabels), Tolerations: slices.Clone(p.Tolerations),
			},
		})
	}
	return view, false
}

// namespaceBorrow derives one namespace's borrowing figures for one type from
// the rows of the queues it is granted: the typed rows naming that accelerator,
// or, when none does, the untyped ones (a flavor without a product label grants
// every type the entry names, as in BoundBy).
//
// The reach is each counted row's nominal plus its borrowing limit (unlimited
// when one is unset) and each withheld row's nominal, bounded by the static
// entry and the namespace ResourceQuota; the cap is the reach past the
// guaranteed figure. A type with an unlimited guaranteed figure needs no
// borrowing. Reports false when no row names the type.
func (q *QuotaInventory) namespaceBorrow(ns, accType string, guaranteed int, rows []config.ExternalBorrow, gpuQuota map[string]int) (NamespaceBorrow, bool) {
	matched := slices.DeleteFunc(slices.Clone(rows), func(r config.ExternalBorrow) bool {
		return r.Accelerator == "" || !accelerator.SameName(r.Accelerator, accType)
	})
	if len(matched) == 0 {
		matched = slices.DeleteFunc(slices.Clone(rows), func(r config.ExternalBorrow) bool { return r.Accelerator != "" })
	}
	if len(matched) == 0 {
		return NamespaceBorrow{}, false
	}
	b := NamespaceBorrow{WithinNominal: guaranteed}
	if guaranteed < 0 {
		return b, true
	}
	reach, unlimited := 0, false
	for _, r := range matched {
		switch {
		case !r.Counted():
			reach += r.Nominal
		case r.BorrowCap < 0:
			unlimited = true
		default:
			reach += r.Nominal + r.BorrowCap
		}
		if r.Counted() {
			b.LentOut += r.LentOut
		}
		if r.Reason == config.BorrowReasonNominalLentOut && r.Nominal > r.Guaranteed {
			b.NominalLentOut += r.Nominal - r.Guaranteed
		}
	}
	bound := func(c int) {
		if c < 0 {
			return
		}
		if unlimited || c < reach {
			reach, unlimited = c, false
		}
	}
	if static, excluded := q.static.QuotaForNamespace(ns); !excluded {
		if key, ok := accelerator.FindKey(static, accType); ok {
			bound(static[key])
		}
	}
	for _, r := range matched {
		if hard, ok := gpuQuota[r.Resource]; ok {
			bound(hard)
		}
	}
	b.Cap = config.QuotaUnlimited
	if !unlimited {
		b.Cap = max(0, reach-guaranteed)
	}
	return b, true
}

// resourceQuotaBounds reports whether a namespace ResourceQuota is a GPU bound
// (B1): for every GPU resource the namespace's counted queues borrow, a hard
// limit at most nominal plus borrowingLimit. A ResourceQuota of 1000 proves
// nothing, and with borrowingLimit unset nothing does.
func resourceQuotaBounds(rows []config.ExternalBorrow, gpuQuota map[string]int) bool {
	reach := map[string]int{}
	for _, r := range rows {
		if !r.Counted() {
			continue
		}
		if r.BorrowCap < 0 {
			return false
		}
		reach[r.Resource] += r.Nominal + r.BorrowCap
	}
	if len(reach) == 0 {
		return false
	}
	for res, n := range reach {
		hard, ok := gpuQuota[res]
		if !ok || hard > n {
			return false
		}
	}
	return true
}

// Borrowing returns, for the active namespaces this entry counts borrowing
// for, their borrowing figures, the shared pools their cohorts hold, and the
// cohorts of every namespace read. All nil when no borrowing is counted.
func (q *QuotaInventory) Borrowing(activeNamespaces []string) (map[string]map[string]NamespaceBorrow, []SharedPoolChunk, map[string][]string) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.borrow == nil {
		return nil, nil, nil
	}
	byNS := map[string]map[string]NamespaceBorrow{}
	for _, ns := range activeNamespaces {
		if perType, ok := q.borrow.byNS[ns]; ok {
			byNS[ns] = maps.Clone(perType)
		}
	}
	cohorts := make(map[string][]string, len(q.borrow.cohorts))
	for ns, cs := range q.borrow.cohorts {
		cohorts[ns] = slices.Clone(cs)
	}
	return byNS, slices.Clone(q.borrow.pools), cohorts
}

// logBorrowing logs, on change only, the borrowing this entry counts for each
// namespace it governs: guaranteed, borrowable, Kueue's own borrowed figure,
// and why borrowable is 0 where it is -- the source's reason, or the
// engine-side one (not-kueue-managed, no-gpu-bound, resourcequota-unreadable).
// A cluster-scope entry logs nothing, since borrowing moves capacity between
// queues and leaves the cluster figure as it is. stale marks a snapshot served
// after a failed read or past twice the refresh interval: figures freeze and
// nothing new is borrowed. Callers must hold q.mu.
func (q *QuotaInventory) logBorrowing(logger logr.Logger, ext config.ExternalQuotas, view *borrowView, stale bool) {
	if !q.countsBorrowing() || ext.ObservedAt.IsZero() {
		return
	}
	rowsByNS := make(map[string][]config.ExternalBorrow, len(ext.Borrow))
	for ns, rows := range ext.Borrow {
		if !q.governsBorrowRows(ns, rows) {
			continue
		}
		reason := ""
		switch {
		case stale && (view == nil || view.byNS[ns] == nil):
			reason = string(kueue.ReasonStale)
		case view != nil && view.withheld[ns] != "":
			reason = view.withheld[ns]
		}
		if reason != "" {
			rows = slices.Clone(rows)
			for i := range rows {
				if rows[i].Counted() {
					rows[i] = rows[i].Withheld(reason)
				}
			}
		}
		rowsByNS[ns] = rows
	}
	// The applied figures per namespace and type: the guaranteed cap and how
	// far past it the namespace may borrow, after the static entry and the
	// ResourceQuota bound it -- what the optimizer spends, beside the
	// per-queue rows the reader computed.
	applied := map[string]map[string]appliedBorrow{}
	if view != nil {
		for ns, perType := range view.byNS {
			applied[ns] = map[string]appliedBorrow{}
			for t, b := range perType {
				applied[ns][t] = appliedBorrow{Guaranteed: view.effective.NamespaceQuotas[ns][t], BorrowCap: b.Cap,
					WithinNominal: b.WithinNominal, NominalLentOut: b.NominalLentOut}
			}
		}
	}
	fp, _ := json.Marshal(struct {
		Rows    map[string][]config.ExternalBorrow
		Applied map[string]map[string]appliedBorrow
		Frozen  bool
	}{rowsByNS, applied, view != nil && view.frozen})
	if string(fp) == q.lastBorrow {
		return
	}
	q.lastBorrow = string(fp)
	msg := BorrowAppliedLogMessage
	if view != nil && view.frozen {
		msg = BorrowFrozenLogMessage
	}
	logger.Info(msg, "borrowByNamespace", rowsByNS, "appliedByNamespace", applied, "withheldByNamespace", q.withheld)
}

// The borrowing log line, on change only. The e2e suite and operators search
// for these.
const (
	// BorrowAppliedLogMessage: borrowing is applied for the namespaces whose
	// every condition holds, and withheld, with the reason, for the others.
	BorrowAppliedLogMessage = "Kueue borrowing applied where every condition holds and withheld elsewhere; " +
		"guaranteed is the namespace cap, borrowable is drawn from the cohort's free pool"
	// BorrowFrozenLogMessage: the snapshot is stale; the last figures stay.
	BorrowFrozenLogMessage = "Kueue snapshot stale: borrowing frozen at the last figures, nothing new is borrowed"
)

// appliedBorrow is one namespace's applied borrowing figures for one type, as
// logged.
type appliedBorrow struct {
	Guaranteed     int `json:"guaranteed"`
	BorrowCap      int `json:"borrowCap"`
	WithinNominal  int `json:"withinNominal"`
	NominalLentOut int `json:"nominalLentOut,omitempty"`
}

// governsBorrowRows reports whether ns's borrowing rows are this entry's to
// count: not the reserved key, not excluded, and some row read.
func (q *QuotaInventory) governsBorrowRows(ns string, rows []config.ExternalBorrow) bool {
	return ns != config.QuotaLimiterReservedNamespaceKey && !q.static.IsExcluded(ns) && len(rows) > 0
}

// withheldReasons is, per namespace the read granted borrowing rows for and
// whose borrowing is not counted, why: stale, the engine-side reason, or --
// when the source withheld every row -- the source's. Nil when the entry
// counts no borrowing.
func (q *QuotaInventory) withheldReasons(ext config.ExternalQuotas, view *borrowView, stale bool) map[string]string {
	if !q.countsBorrowing() || ext.ObservedAt.IsZero() {
		return nil
	}
	out := map[string]string{}
	for ns, rows := range ext.Borrow {
		if !q.governsBorrowRows(ns, rows) {
			continue
		}
		switch {
		case stale && (view == nil || view.byNS[ns] == nil):
			out[ns] = string(kueue.ReasonStale)
		case view != nil && view.withheld[ns] != "":
			out[ns] = view.withheld[ns]
		case view != nil && view.byNS[ns] != nil:
			// Counted.
		default:
			if i := slices.IndexFunc(rows, func(r config.ExternalBorrow) bool { return !r.Counted() }); i >= 0 {
				out[ns] = rows[i].Reason
			}
		}
	}
	return out
}

// BorrowWithheld returns, for the active namespaces this entry reads borrowing
// for and does not count it, why. Nil when the entry counts no borrowing.
func (q *QuotaInventory) BorrowWithheld(activeNamespaces []string) map[string]string {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if len(q.withheld) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, ns := range activeNamespaces {
		if r, ok := q.withheld[ns]; ok {
			out[ns] = r
		}
	}
	return out
}

// EffectiveConfig returns the entry currently enforced: the static entry bounded
// by the last external snapshot, or the static entry itself.
func (q *QuotaInventory) EffectiveConfig() config.QuotaLimiterConfig {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.cfg
}

// SetUsed updates the cluster-scoped usage map for cluster-scoped inventories.
// For namespace-scoped inventories this method is a no-op — namespace-scoped
// inventories need per-namespace usage and should use SetUsedByNamespace
// instead (the Inventory interface predates the per-namespace case; rather
// than widen the interface we accept both shapes and only the relevant one
// is consulted).
// Incoming keys are reconciled onto the CONFIGURED quota keys before being
// stored. Quota keys are whatever the operator typed (conventionally short names
// like "A100"); usage arrives keyed by the name a workload declares, which for
// the common nodeSelector deployment is the raw product label
// ("NVIDIA-A100-PCIE-80GB"). Unreconciled, every lookup in TotalUsed /
// GetResourcePools / NamespaceResourcePools misses, Used stays 0, and a quota
// reports its full allowance free however much is running — so the cap never
// binds, which is the one thing a quota exists to do.
func (q *QuotaInventory) SetUsed(usedByType map[string]int) {
	if q.static.Scope != config.QuotaScopeCluster {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.usedByType = reconcileUsageKeys(q.cfg.ClusterQuotas, usedByType)
}

// reconcileUsageKeys maps declared usage keys onto the key set `known` uses,
// summing any that collide. Usage matching no known key is dropped: it cannot be
// charged to a budget, and counting it would make the totals contradict the
// per-type view.
func reconcileUsageKeys(known map[string]int, declared map[string]int) map[string]int {
	out := make(map[string]int, len(declared))
	for name, count := range declared {
		if key, ok := resolveAcceleratorKey(known, name); ok {
			out[key] += count
		}
	}
	return out
}

// SetUsedByNamespace updates the per-namespace usage map used by
// namespace-scoped inventories. The outer key is the namespace; the inner
// key is the accelerator type. Excluded namespaces are still tracked but
// NamespaceResourcePools omits them, so no cap is enforced for them.
//
// For cluster-scoped inventories this method is a no-op.
// Each namespace's usage is reconciled onto that namespace's configured quota
// keys, for the reason given on SetUsed.
func (q *QuotaInventory) SetUsedByNamespace(usedByNS map[string]map[string]int) {
	if q.static.Scope != config.QuotaScopeNamespace {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	reconciled := make(map[string]map[string]int, len(usedByNS))
	for ns, perType := range usedByNS {
		// Note the second return is `excluded`, NOT `found`: an excluded
		// namespace yields (nil, true), and every other case yields a map with
		// false. An excluded or unquotaed namespace has nothing to reconcile
		// against, so its usage is kept verbatim — the key set of this map is
		// also what callers use as the active-namespace set, so entries must not
		// be dropped.
		quotas, excluded := q.cfg.QuotaForNamespace(ns)
		if excluded || len(quotas) == 0 {
			reconciled[ns] = maps.Clone(perType)
			continue
		}
		reconciled[ns] = reconcileUsageKeys(quotas, perType)
	}
	q.usedByNS = reconciled
}

// TotalLimit returns the cluster-wide sum of quotas across accelerator types
// for the cluster scope, or the sum of all per-namespace quotas (excluding
// excluded namespaces and the reserved default key) for the namespace scope.
// QuotaUnlimited (-1) is excluded from sums — there is no finite total.
func (q *QuotaInventory) TotalLimit() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.totalLimitLocked()
}

// totalLimitLocked computes TotalLimit. Callers must hold q.mu (read or write).
func (q *QuotaInventory) totalLimitLocked() int {
	total := 0
	switch q.cfg.Scope {
	case config.QuotaScopeCluster:
		for _, v := range q.cfg.ClusterQuotas {
			if v == config.QuotaUnlimited {
				continue
			}
			total += v
		}
	case config.QuotaScopeNamespace:
		for ns, perType := range q.cfg.NamespaceQuotas {
			if ns == config.QuotaLimiterReservedNamespaceKey {
				continue
			}
			if q.cfg.IsExcluded(ns) {
				continue
			}
			for _, v := range perType {
				if v == config.QuotaUnlimited {
					continue
				}
				total += v
			}
		}
	}
	return total
}

// TotalUsed returns the sum of recorded usage, counting only the key set that
// TotalLimit does so the two stay symmetric and TotalAvailable does not
// under-report. At cluster scope that is the accelerator types with a finite
// ClusterQuota; at namespace scope it is the non-excluded, non-"default"
// namespaces and their finite-quota types. Usage of unquotaed, unlimited (-1),
// excluded, or default-fall-through entries is therefore excluded — matching
// TotalLimit, which does not count their caps.
func (q *QuotaInventory) TotalUsed() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.totalUsedLocked()
}

// totalUsedLocked computes TotalUsed. Callers must hold q.mu (read or write).
// Both branches iterate the SAME key set as totalLimitLocked so that TotalUsed
// and TotalLimit cover identical entries.
func (q *QuotaInventory) totalUsedLocked() int {
	total := 0
	switch q.cfg.Scope {
	case config.QuotaScopeCluster:
		// Count usage only for types with a finite cluster quota, mirroring
		// totalLimitLocked. Usage of unquotaed types (real replicas with no
		// configured cap) must not be subtracted from the quotaed budget.
		for accType, limit := range q.cfg.ClusterQuotas {
			if limit == config.QuotaUnlimited {
				continue
			}
			total += q.usedByType[accType]
		}
	case config.QuotaScopeNamespace:
		// Mirror totalLimitLocked: skip the reserved "default" key, excluded
		// namespaces, and unlimited types, so excluded/unlimited/default-
		// fall-through usage is not charged against the finite namespace budget.
		for ns, perType := range q.cfg.NamespaceQuotas {
			if ns == config.QuotaLimiterReservedNamespaceKey {
				continue
			}
			if q.cfg.IsExcluded(ns) {
				continue
			}
			for accType, limit := range perType {
				if limit == config.QuotaUnlimited {
					continue
				}
				total += q.usedByNS[ns][accType]
			}
		}
	}
	return total
}

// TotalAvailable returns max(0, TotalLimit - TotalUsed). Notes the same
// caveat as TotalLimit: unlimited (-1) entries are excluded from both terms,
// so TotalAvailable underreports availability when unlimited quotas are
// configured. Limit and used are read under a single lock so the pair is
// internally consistent.
func (q *QuotaInventory) TotalAvailable() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	avail := q.totalLimitLocked() - q.totalUsedLocked()
	if avail < 0 {
		return 0
	}
	return avail
}

// GetResourcePools returns per-accelerator-type pools. For the cluster scope it
// is one pool per configured type. For the namespace scope it is the per-type
// SUM across explicitly-listed (non-excluded, non-"default") namespaces — i.e.
// the aggregate cluster budget that the per-namespace caps partition. The
// per-(namespace, type) breakdown is exposed separately via
// NamespaceResourcePools; the optimizer combines them as
// min(per-type total, that namespace's cap).
//
// Quota value semantics on the emitted Limit:
//   - Unlimited (-1) entries are emitted as a sentinel ResourcePool{Limit ==
//     QuotaUnlimited} (matching NamespaceResourcePools), so the V2 optimizer can
//     tell "unlimited" (allow up to demand) apart from a type with no configured
//     quota (absent → deny). mergeConstraints branches on Limit < 0 and treats
//     the sentinel as unbounded; TotalLimit/TotalUsed/TotalAvailable exclude it,
//     so the reported totals are unchanged.
//   - A quota of 0 is a real cap (deny) and is emitted with Limit == 0.
func (q *QuotaInventory) GetResourcePools() map[string]ResourcePool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	pools := make(map[string]ResourcePool)
	switch q.cfg.Scope {
	case config.QuotaScopeCluster:
		for accType, limit := range q.cfg.ClusterQuotas {
			// Emit every configured type, including unlimited (-1). An omitted
			// type is read as 0 available by the V2 optimizer and silently
			// denied, which would invert the -1 = unlimited semantic (see the
			// function doc above). The sentinel is carried through
			// mergeConstraints as an unbounded budget.
			pools[accType] = ResourcePool{
				Limit: limit,
				Used:  q.usedByType[accType],
			}
		}
	case config.QuotaScopeNamespace:
		for ns, perType := range q.cfg.NamespaceQuotas {
			if ns == config.QuotaLimiterReservedNamespaceKey {
				continue
			}
			if q.cfg.IsExcluded(ns) {
				continue
			}
			for accType, limit := range perType {
				if limit == config.QuotaUnlimited {
					continue
				}
				pool := pools[accType]
				pool.Limit += limit
				pool.Used += q.usedByNS[ns][accType]
				pools[accType] = pool
			}
		}
	}
	return pools
}

// NamespaceResourcePools implements NamespaceAwareInventory. For the cluster
// scope it returns nil (no namespace dimension). For the namespace scope it
// exposes each active namespace as a CLOSED allowlist, applying the namespace
// lookup rules from issue #1002 (exact match → cap; fall-through to `default`
// → cap; excluded → open; otherwise deny):
//
//   - An excluded namespace is OMITTED from the result entirely. Its absence
//     signals "open" to the optimizer (no namespace cap; only the cluster/
//     per-type constraint applies).
//   - Every other active namespace is always present (even with an empty inner
//     map), which marks it as a closed allowlist. An empty map is a real
//     deny-all (a namespace with neither an explicit quota nor a "default"
//     fall-through): the optimizer allocates it nothing.
//   - Within a present namespace, each listed accelerator type is emitted: a
//     finite cap as ResourcePool{Limit: n} (including 0, a real deny cap), and
//     an unlimited (-1) cap as a sentinel ResourcePool{Limit: QuotaUnlimited}
//     so the optimizer can tell "unlimited" (allow up to the cluster budget)
//     apart from "type not listed" (deny). A type the namespace does not list
//     is simply absent, which the optimizer denies — it does NOT fall through
//     to the cluster aggregate (that fall-through was the cross-namespace leak).
//
// Used counts come from the most recent SetUsedByNamespace call.
func (q *QuotaInventory) NamespaceResourcePools(activeNamespaces []string) map[string]map[string]ResourcePool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.cfg.Scope != config.QuotaScopeNamespace {
		return nil
	}
	out := make(map[string]map[string]ResourcePool)
	for _, ns := range activeNamespaces {
		quotas, excluded := q.cfg.QuotaForNamespace(ns)
		if excluded {
			// Omit: an absent namespace is "open" (pass-through), so the
			// model is bound only by the cluster/per-type constraint.
			continue
		}
		// Always materialize the namespace so it is a closed allowlist; an
		// empty map is a deny-all namespace.
		perType, ok := out[ns]
		if !ok {
			perType = make(map[string]ResourcePool)
			out[ns] = perType
		}
		for accType, limit := range quotas {
			// Emit unlimited as a sentinel (Limit == QuotaUnlimited) rather
			// than dropping it, so the optimizer distinguishes it from a
			// type the namespace does not list (which it must deny).
			perType[accType] = ResourcePool{
				Limit: limit,
				Used:  q.usedByNS[ns][accType],
			}
		}
	}
	return out
}

// copyNestedIntMap returns a deep copy of a two-level nested int map.
// maps.Clone is shallow, so the inner maps are cloned explicitly.
