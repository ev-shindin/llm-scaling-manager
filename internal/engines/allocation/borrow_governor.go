package allocation

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Giving borrowed capacity back
// (docs/proposals/kueue-borrowing-and-limiter-composition.md, B5 and B6).
//
// The steady-state engine owns one BorrowGovernor. Each cycle it Observes the
// Kueue reads the constraints carry and the variants' placement, before the
// optimizer runs, and asks it what borrowing is blocked; after the optimizer it
// asks it for Releases, which lower targets and stay in force until the release
// has landed. Everything here is a pure function of what it is told plus its
// own state, so the rules can be tested without a cluster.

// ReleaseCause says why borrowed capacity was released.
type ReleaseCause string

// Release causes.
const (
	// ReleaseLenderWaiting: a lender that cannot reclaim waited for capacity
	// it lent (B6).
	ReleaseLenderWaiting ReleaseCause = "lender-waiting"
	// ReleasePlacementTimeout: a borrowed unit stayed unplaced past the
	// placement timeout (B5). It takes a P/D partner that did place with it.
	ReleasePlacementTimeout ReleaseCause = "placement-timeout"
)

// ReasonReleaseNotActuated marks a ledger entry still in flight after twice
// the termination grace plus the KEDA scale-down window: the target was never
// actuated, or a pod is stuck. It keeps blocking.
const ReasonReleaseNotActuated = "release-not-actuated"

// BorrowSettings are the governor's timings.
type BorrowSettings struct {
	PlacementTimeout time.Duration
	Cooldown         time.Duration
	LenderDebounce   time.Duration
	// ReleaseActuation is how long a release may take to land before its
	// ledger entry is marked ReasonReleaseNotActuated: twice the termination
	// grace plus the KEDA scale-down window.
	ReleaseActuation time.Duration
}

// DefaultBorrowSettings are the proposal's defaults.
func DefaultBorrowSettings() BorrowSettings {
	return BorrowSettings{
		PlacementTimeout: config.DefaultPlacementTimeout,
		Cooldown:         config.DefaultBorrowCooldown,
		LenderDebounce:   config.DefaultLenderDebounce,
		ReleaseActuation: 2*k8sDefaultTerminationGrace + hpaDefaultScaleDownWindow,
	}
}

// LenderKey is one lender queue for one flavor and GPU resource of a cohort.
type LenderKey struct {
	Cohort   string `json:"cohort"`
	Lender   string `json:"lender"`
	Flavor   string `json:"flavor"`
	Resource string `json:"resource"`
}

func (k LenderKey) flavorKey() string { return k.Cohort + "/" + k.Flavor + "/" + k.Resource }

// ReleasedReplicas is one variant's part of a released unit: its target after
// the release.
type ReleasedReplicas struct {
	Namespace string `json:"namespace"`
	Variant   string `json:"variant"`
	To        int    `json:"to"`
}

// ReleaseLedgerEntry is a release in flight for one waiting lender. No further
// unit is released for that lender while it exists. It clears once every
// pod of the unit is gone AND a fresh read of the lender postdates that, and
// resets when the lender stops waiting.
type ReleaseLedgerEntry struct {
	LenderKey
	Units      []ReleasedReplicas `json:"units,omitempty"`
	ReleasedAt time.Time          `json:"releasedAt"`
	PodsGoneAt *time.Time         `json:"podsGoneAt,omitempty"`
	// Unknown is an entry assumed after a restart for a waiting lender with
	// nothing persisted: in flight until the next fresh read.
	Unknown bool `json:"unknown,omitempty"`
	// Reason is ReasonReleaseNotActuated once the entry outlived
	// ReleaseActuation.
	Reason string `json:"reason,omitempty"`
}

// BorrowLedger is the persisted state: the entries and, per cohort flavor, the
// GPUs released while its lenders keep waiting.
type BorrowLedger struct {
	Entries  []ReleaseLedgerEntry `json:"entries"`
	Released map[string]int       `json:"released,omitempty"`
}

// BorrowRelease is one release, for logs now and for events and metrics later.
type BorrowRelease struct {
	Namespace string
	ModelID   string
	Variant   string
	From, To  int
	Cause     ReleaseCause
	// Lender is the waiting lender (ReleaseLenderWaiting), else empty.
	Lender LenderKey
}

// BorrowCycle is what the governor is told about one cycle.
type BorrowCycle struct {
	Now         time.Time
	Constraints []*ResourceConstraints
	Requests    []ModelScalingRequest
	// Gated is, per namespace and accelerator, GPUs of pods at Kueue's
	// admission gate: they hold no reservation, so they are not borrowed.
	Gated map[string]map[string]int
	// ScaleUpPending is, keyed "namespace/variant", whether an unplaced pod
	// has a cluster-autoscaler TriggeredScaleUp not followed by a
	// NotTriggerScaleUp: the placement timeout then waits, up to three times.
	ScaleUpPending map[string]bool
}

type lenderWatch struct {
	since    time.Time
	reads    int
	lastRead time.Time
}

// stickyRelease keeps a lowered target in force until the pods are gone.
type stickyRelease struct {
	to int
}

// BorrowGovernor holds the release and cooldown state across cycles.
type BorrowGovernor struct {
	mu       sync.Mutex
	settings BorrowSettings

	watches        map[LenderKey]*lenderWatch
	ledger         map[LenderKey]*ReleaseLedgerEntry
	released       map[string]int
	cohortCooldown map[string]time.Time
	modelCooldown  map[string]time.Time
	lastGrown      map[string]time.Time
	timeoutHeld    map[string]stickyRelease
	held           map[string]bool
	started        bool
}

// NewBorrowGovernor returns an empty governor.
func NewBorrowGovernor(s BorrowSettings) *BorrowGovernor {
	return &BorrowGovernor{
		settings: s, watches: map[LenderKey]*lenderWatch{}, ledger: map[LenderKey]*ReleaseLedgerEntry{},
		released: map[string]int{}, cohortCooldown: map[string]time.Time{}, modelCooldown: map[string]time.Time{},
		lastGrown: map[string]time.Time{}, timeoutHeld: map[string]stickyRelease{}, held: map[string]bool{},
	}
}

// SetSettings replaces the timings, for a live config change.
func (g *BorrowGovernor) SetSettings(s BorrowSettings) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settings = s
}

// Restore loads a persisted ledger. Call before the first Observe.
func (g *BorrowGovernor) Restore(l BorrowLedger) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range l.Entries {
		e := l.Entries[i]
		g.ledger[e.LenderKey] = &e
	}
	maps.Copy(g.released, l.Released)
}

// Ledger returns the state to persist, in a stable order.
func (g *BorrowGovernor) Ledger() BorrowLedger {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ledgerLocked()
}

func (g *BorrowGovernor) ledgerLocked() BorrowLedger {
	out := BorrowLedger{Entries: make([]ReleaseLedgerEntry, 0, len(g.ledger))}
	for _, e := range g.ledger {
		out.Entries = append(out.Entries, *e)
	}
	slices.SortFunc(out.Entries, func(a, b ReleaseLedgerEntry) int { return compareLenderKeys(a.LenderKey, b.LenderKey) })
	if len(g.released) > 0 {
		out.Released = maps.Clone(g.released)
	}
	return out
}

// MarshalLedger is Ledger as JSON, for the controller-status ConfigMap.
func (g *BorrowGovernor) MarshalLedger() string {
	b, _ := json.Marshal(g.Ledger())
	return string(b)
}

// UnmarshalBorrowLedger parses a persisted ledger.
func UnmarshalBorrowLedger(s string) (BorrowLedger, error) {
	var l BorrowLedger
	if s == "" {
		return l, nil
	}
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		return BorrowLedger{}, fmt.Errorf("borrow release ledger: %w", err)
	}
	return l, nil
}

// MarkPreempted starts the model's cooldown: Kueue preempted one of its
// borrowed pods. modelKey is "namespace/model".
func (g *BorrowGovernor) MarkPreempted(modelKey string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.modelCooldown[modelKey] = now.Add(g.settings.Cooldown)
}

// BlockedCohorts is the cohorts where nothing new may be borrowed now: a
// lender waited within the cooldown (one that can reclaim is preempting; one
// that cannot is being handed capacity back).
func (g *BorrowGovernor) BlockedCohorts(now time.Time) map[string]bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]bool{}
	for c, until := range g.cohortCooldown {
		if now.Before(until) {
			out[c] = true
		}
	}
	return out
}

// BorrowBlocked reports whether a variant may not grow on borrowed capacity
// now: its model is cooling down after a preemption, or the variant has
// unplaced replicas (the placement hold; for a LeaderWorkerSet until the whole
// group placed). Guaranteed growth is never blocked here.
func (g *BorrowGovernor) BorrowBlocked(namespace, modelID, variant string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if until, ok := g.modelCooldown[namespace+"/"+modelID]; ok && now.Before(until) {
		return true
	}
	return g.held[namespace+"/"+variant]
}

// Observe takes one cycle's Kueue reads and variant states: it debounces the
// lenders, advances the ledger and the cooldowns, and records the placement
// hold. A stale read changes nothing about its lenders or their entries.
func (g *BorrowGovernor) Observe(c BorrowCycle) {
	g.mu.Lock()
	defer g.mu.Unlock()
	states := indexStates(c.Requests)
	g.held = map[string]bool{}
	for k, s := range states {
		if s.state.UnplacedReplicas > 0 {
			g.held[k] = true
		}
	}
	for k, r := range g.timeoutHeld {
		if s, ok := states[k]; !ok || heldOf(s.state) <= r.to {
			delete(g.timeoutHeld, k)
		}
	}
	chunks, _ := mergeSharedPools(c.Constraints)
	waitingOn := map[string]bool{}
	seen := map[LenderKey]bool{}
	for _, ch := range chunks {
		for _, l := range ch.Lenders {
			key := LenderKey{Cohort: ch.Cohort, Lender: l.Queue, Flavor: ch.Flavor, Resource: ch.Resource}
			seen[key] = true
			if ch.Stale {
				if g.waiting(key, c.Now) {
					waitingOn[key.flavorKey()] = true
				}
				continue // frozen: nothing about this lender changes
			}
			g.observeLender(key, l.Waiting, ch.ReadAt, c.Now)
			if e := g.ledger[key]; e != nil {
				g.advanceEntry(e, ch.ReadAt, states, c.Now)
			}
			if g.waiting(key, c.Now) {
				waitingOn[key.flavorKey()] = true
				g.cohortCooldown[ch.Cohort] = c.Now.Add(g.settings.Cooldown)
			}
		}
	}
	for fk := range g.released {
		if !waitingOn[fk] {
			delete(g.released, fk)
		}
	}
	g.started = true
}

// observeLender advances one lender's debounce on a new read, and resets it,
// with its ledger entry, when a fresh read says it no longer waits. The first
// read after a start assumes a release in flight for a waiting lender that has
// no entry: one may have been made and not persisted.
func (g *BorrowGovernor) observeLender(key LenderKey, waiting bool, readAt, now time.Time) {
	w := g.watches[key]
	newRead := w == nil || readAt.After(w.lastRead)
	if !newRead {
		return
	}
	if !waiting {
		delete(g.watches, key)
		delete(g.ledger, key)
		return
	}
	if w == nil {
		w = &lenderWatch{since: now}
		g.watches[key] = w
	}
	w.reads++
	w.lastRead = readAt
	if !g.started && g.ledger[key] == nil {
		g.ledger[key] = &ReleaseLedgerEntry{LenderKey: key, ReleasedAt: readAt, Unknown: true}
	}
}

// waiting reports whether a lender counts as waiting: two fresh reads in a
// row and at least the debounce since the first.
func (g *BorrowGovernor) waiting(key LenderKey, now time.Time) bool {
	w := g.watches[key]
	return w != nil && w.reads >= 2 && now.Sub(w.since) >= g.settings.LenderDebounce
}

// advanceEntry marks a release's pods gone, clears it once a fresh read
// postdates that, and marks one that outlived ReleaseActuation.
func (g *BorrowGovernor) advanceEntry(e *ReleaseLedgerEntry, readAt time.Time, states map[string]variantRef, now time.Time) {
	if e.Unknown {
		if readAt.After(e.ReleasedAt) {
			delete(g.ledger, e.LenderKey)
		}
		return
	}
	if e.PodsGoneAt == nil {
		gone := true
		for _, u := range e.Units {
			if s, ok := states[u.Namespace+"/"+u.Variant]; ok && heldOf(s.state) > u.To {
				gone = false
			}
		}
		if gone {
			at := now
			e.PodsGoneAt = &at
		}
	}
	if e.PodsGoneAt != nil && readAt.After(*e.PodsGoneAt) {
		delete(g.ledger, e.LenderKey)
		return
	}
	if now.Sub(e.ReleasedAt) > g.settings.ReleaseActuation {
		e.Reason = ReasonReleaseNotActuated
	}
}

// Releases decides this cycle's releases after the optimizer, lowers the
// decisions' targets accordingly, and keeps every release still in flight in
// force -- over the analyzer's demand floor and hold, which would otherwise
// raise the target again next cycle. It also records which variants grew, for
// the release order.
func (g *BorrowGovernor) Releases(c BorrowCycle, decisions []domain.VariantDecision) []BorrowRelease {
	g.mu.Lock()
	defer g.mu.Unlock()
	states := indexStates(c.Requests)
	byVariant := map[string]int{}
	for i := range decisions {
		byVariant[decisions[i].Namespace+"/"+decisions[i].VariantName] = i
	}
	nsBorrow := namespaceBorrowed(c.Constraints, c.Gated)
	out := g.timeoutReleases(c, states, namespaceHeldPast(c.Constraints))
	out = append(out, g.lenderReleases(c, states, nsBorrow)...)

	// Releases in flight stay in force.
	for _, e := range g.ledger {
		for _, u := range e.Units {
			lowerTo(decisions, byVariant, u.Namespace+"/"+u.Variant, u.To, "borrow-released")
		}
	}
	for k, r := range g.timeoutHeld {
		lowerTo(decisions, byVariant, k, r.to, "borrow-unplaced")
	}
	for i := range decisions {
		d := &decisions[i]
		if d.TargetReplicas > d.CurrentReplicas {
			g.lastGrown[d.Namespace+"/"+d.VariantName] = c.Now
		}
	}
	return out
}

// timeoutReleases releases borrowed units unplaced past the placement timeout
// (B5): the unplaced replicas a borrowing namespace holds past its guaranteed
// figure, a P/D unit at the pair's ratio with the partner that placed. Growth
// within guaranteed -- a reclaiming queue's growth into nominal it lent out --
// is exempt, since Kueue reclaims for it. "Past guaranteed" counts the
// replicas the namespace holds, gated ones included (heldPast): a replica
// Kueue gates while its queue already reserves its whole guaranteed figure --
// the replacement of a preempted borrowed pod -- has nothing to reclaim and
// would wait forever. The model's cooldown starts.
func (g *BorrowGovernor) timeoutReleases(c BorrowCycle, states map[string]variantRef, heldPast map[string]map[string]int) []BorrowRelease {
	var out []BorrowRelease
	for _, req := range c.Requests {
		for _, st := range req.VariantStates {
			key := req.Namespace + "/" + st.VariantName
			if st.UnplacedReplicas <= 0 || st.UnplacedSince.IsZero() {
				continue
			}
			if _, sticky := g.timeoutHeld[key]; sticky {
				continue
			}
			timeout := g.settings.PlacementTimeout
			if c.ScaleUpPending[key] {
				timeout *= 3
			}
			if c.Now.Sub(st.UnplacedSince) < timeout {
				continue
			}
			borrowed := borrowedFor(heldPast, req.Namespace, accOf(req, st.VariantName))
			gpr := max(st.GPUsPerReplica, 1)
			if borrowed < gpr {
				continue // within guaranteed: Kueue reclaims for it
			}
			unit := releaseUnit(req, st.VariantName)
			if unit == nil {
				continue
			}
			if !req.Disaggregated {
				unit[st.VariantName] = min(st.UnplacedReplicas, borrowed/gpr, st.CurrentReplicas-minOf(st))
			}
			if !unitFits(unit, req.Namespace, states) {
				continue
			}
			for v, n := range unit {
				k := req.Namespace + "/" + v
				s := states[k].state
				to := s.CurrentReplicas - n
				g.timeoutHeld[k] = stickyRelease{to: to}
				out = append(out, BorrowRelease{Namespace: req.Namespace, ModelID: req.ModelID, Variant: v,
					From: s.CurrentReplicas, To: to, Cause: ReleasePlacementTimeout})
			}
			g.modelCooldown[req.Namespace+"/"+req.ModelID] = c.Now.Add(g.settings.Cooldown)
		}
	}
	return out
}

// lenderReleases releases, for each waiting lender that cannot reclaim and has
// no release in flight, one borrowed unit of its cohort and flavor (B6): the
// smallest whole unit, even when larger than the need -- for whole-node models
// the alternative is releasing nothing. The total stays within the lent-out
// nominal of every waiting lender on the flavor, rounded up to a unit. A
// release never takes a namespace below its guaranteed figure nor a variant
// below minReplicas. Order: lowest priority first, then the most recently
// grown, then (after a restart, when growth is not known) the most expensive.
func (g *BorrowGovernor) lenderReleases(c BorrowCycle, states map[string]variantRef, nsBorrow map[string]map[string]int) []BorrowRelease {
	chunks, cohorts := mergeSharedPools(c.Constraints)
	bound := map[string]int{}
	type waiter struct {
		key   LenderKey
		chunk *SharedPoolChunk
	}
	var waiters []waiter
	for _, ch := range chunks {
		if ch.Stale {
			continue // a stale reader freezes the release too
		}
		for _, l := range ch.Lenders {
			key := LenderKey{Cohort: ch.Cohort, Lender: l.Queue, Flavor: ch.Flavor, Resource: ch.Resource}
			if !g.waiting(key, c.Now) || l.CanReclaim {
				continue
			}
			bound[key.flavorKey()] += l.LentOut
			waiters = append(waiters, waiter{key, ch})
		}
	}
	slices.SortFunc(waiters, func(a, b waiter) int { return compareLenderKeys(a.key, b.key) })
	out := make([]BorrowRelease, 0, len(waiters))
	for _, w := range waiters {
		if g.ledger[w.key] != nil {
			continue // a release in flight blocks the next one
		}
		fk := w.key.flavorKey()
		if g.released[fk] >= bound[fk] {
			continue
		}
		cand := g.releaseCandidates(c, states, nsBorrow, cohorts, w.chunk)
		if len(cand) == 0 {
			continue
		}
		pick := cand[0]
		entry := &ReleaseLedgerEntry{LenderKey: w.key, ReleasedAt: c.Now}
		for v, n := range pick.unit {
			k := pick.req.Namespace + "/" + v
			s := states[k].state
			to := s.CurrentReplicas - n
			entry.Units = append(entry.Units, ReleasedReplicas{Namespace: pick.req.Namespace, Variant: v, To: to})
			out = append(out, BorrowRelease{Namespace: pick.req.Namespace, ModelID: pick.req.ModelID, Variant: v,
				From: s.CurrentReplicas, To: to, Cause: ReleaseLenderWaiting, Lender: w.key})
		}
		slices.SortFunc(entry.Units, func(a, b ReleasedReplicas) int { return strings.Compare(a.Variant, b.Variant) })
		g.ledger[w.key] = entry
		g.released[fk] += pick.gpus
		nsBorrow[pick.req.Namespace][pick.typeKey] -= pick.gpus
	}
	return out
}

type releaseCandidate struct {
	req     ModelScalingRequest
	unit    map[string]int
	gpus    int
	typeKey string
	grown   time.Time
	cost    float64
}

// releaseCandidates is every model that borrows on the chunk's flavor in a
// namespace of its cohort, with the unit it would release, in release order.
func (g *BorrowGovernor) releaseCandidates(c BorrowCycle, states map[string]variantRef, nsBorrow map[string]map[string]int,
	cohorts map[string][]string, ch *SharedPoolChunk) []releaseCandidate {
	var out []releaseCandidate
	for _, req := range c.Requests {
		if !slices.Contains(cohorts[req.Namespace], ch.Cohort) {
			continue
		}
		for _, st := range req.VariantStates {
			acc := accOf(req, st.VariantName)
			if !ch.serves(acc) {
				continue
			}
			typeKey, ok := accelerator.FindKey(nsBorrow[req.Namespace], acc)
			if !ok {
				continue
			}
			unit := releaseUnit(req, st.VariantName)
			if unit == nil || !unitFits(unit, req.Namespace, states) {
				continue
			}
			gpus := 0
			var grown time.Time
			cost := 0.0
			for v, n := range unit {
				s := states[req.Namespace+"/"+v]
				gpus += n * max(s.state.GPUsPerReplica, 1)
				if t, ok := g.lastGrown[req.Namespace+"/"+v]; ok && t.After(grown) {
					grown = t
				}
				cost += float64(n) * s.cost
			}
			if gpus > nsBorrow[req.Namespace][typeKey] {
				continue // never below the guaranteed figure
			}
			if slices.ContainsFunc(out, func(o releaseCandidate) bool { return modelKey(o.req) == modelKey(req) && maps.Equal(o.unit, unit) }) {
				continue
			}
			out = append(out, releaseCandidate{req: req, unit: unit, gpus: gpus, typeKey: typeKey, grown: grown, cost: cost})
		}
	}
	slices.SortStableFunc(out, func(a, b releaseCandidate) int {
		if c := cmp.Compare(a.req.Priority, b.req.Priority); c != 0 {
			return c
		}
		switch {
		case !a.grown.IsZero() && !b.grown.IsZero() && !a.grown.Equal(b.grown):
			return b.grown.Compare(a.grown)
		case !a.grown.IsZero() && b.grown.IsZero():
			return -1
		case a.grown.IsZero() && !b.grown.IsZero():
			return 1
		}
		if c := cmp.Compare(b.cost, a.cost); c != 0 {
			return c
		}
		return strings.Compare(modelKey(a.req)+fmt.Sprint(a.unit), modelKey(b.req)+fmt.Sprint(b.unit))
	})
	return out
}

// releaseUnit is the unit releasing variant takes: one replica of it, or for
// a disaggregated model one replica-set of every role at the pair's ratio (the
// current replica counts reduced by their greatest common divisor). Nil when a
// role has nothing to release.
func releaseUnit(req ModelScalingRequest, variant string) map[string]int {
	if !req.Disaggregated {
		return map[string]int{variant: 1}
	}
	byRole := map[string]domain.VariantReplicaState{}
	for _, st := range req.VariantStates {
		if prev, ok := byRole[st.Role]; !ok || st.CurrentReplicas > prev.CurrentReplicas {
			byRole[st.Role] = st
		}
	}
	d := 0
	for _, st := range byRole {
		if st.CurrentReplicas <= 0 {
			return nil
		}
		d = gcd(d, st.CurrentReplicas)
	}
	unit := map[string]int{}
	for _, st := range byRole {
		unit[st.VariantName] = st.CurrentReplicas / d
	}
	return unit
}

// unitFits reports whether every variant of a unit releases at least one
// replica and stays at or above its minReplicas.
func unitFits(unit map[string]int, ns string, states map[string]variantRef) bool {
	for v, n := range unit {
		s, ok := states[ns+"/"+v]
		if !ok || n <= 0 || s.state.CurrentReplicas-n < minOf(s.state) {
			return false
		}
	}
	return true
}

func minOf(s domain.VariantReplicaState) int {
	if s.MinReplicas != nil {
		return *s.MinReplicas
	}
	return 0
}

// heldOf is the replicas a variant's pods still hold: HeldReplicas when its
// pods were listed (terminating pods hold their GPUs), else CurrentReplicas.
func heldOf(s domain.VariantReplicaState) int {
	if s.HeldKnown {
		return max(s.HeldReplicas, s.CurrentReplicas)
	}
	return s.CurrentReplicas
}

// lowerTo caps a decision's target at to, as a scale-down the release owns.
func lowerTo(decisions []domain.VariantDecision, byVariant map[string]int, key string, to int, reason string) {
	i, ok := byVariant[key]
	if !ok {
		return
	}
	d := &decisions[i]
	if d.TargetReplicas <= to {
		return
	}
	d.TargetReplicas = to
	d.GPUsAllocated = to * max(d.GPUsPerReplica, 1)
	action := domain.ActionScaleDown
	if to >= d.CurrentReplicas {
		action = domain.ActionNoChange
	}
	d.SetDecisionReason(action, domain.DecisionReasonV2, string(domain.DecisionReasonV2)+" ("+reason+")")
}

type variantRef struct {
	state domain.VariantReplicaState
	cost  float64
}

func indexStates(requests []ModelScalingRequest) map[string]variantRef {
	out := map[string]variantRef{}
	for _, req := range requests {
		costs := map[string]float64{}
		for _, v := range req.Variants {
			costs[v.VariantName] = v.Cost
		}
		for _, st := range req.VariantStates {
			out[req.Namespace+"/"+st.VariantName] = variantRef{state: st, cost: costs[st.VariantName]}
		}
	}
	return out
}

// accOf is a variant's accelerator as discovery resolved it.
func accOf(req ModelScalingRequest, variant string) string {
	for _, v := range req.Variants {
		if v.VariantName == variant {
			return v.AcceleratorName
		}
	}
	for _, st := range req.VariantStates {
		if st.VariantName == variant {
			return st.AcceleratorName
		}
	}
	return ""
}

// namespaceHeldPast is, per borrowing namespace and type, the GPUs its
// variants hold past its guaranteed figure, gated and unplaced replicas
// included: the placement timeout asks whether an unplaced replica depends on
// borrowing, and one past the guaranteed figure does whether or not Kueue has
// reserved for it. Within the figure Kueue admits, or reclaims lent-out
// nominal, for it.
func namespaceHeldPast(constraints []*ResourceConstraints) map[string]map[string]int {
	return namespaceBorrowed(constraints, nil)
}

// namespaceBorrowed is, per borrowing namespace and type, the GPUs it holds
// past its guaranteed figure, not counting gated pods: they hold no
// reservation, so there is nothing of theirs to hand back to a lender.
func namespaceBorrowed(constraints []*ResourceConstraints, gated map[string]map[string]int) map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil {
			continue
		}
		for ns := range c.NamespaceBorrow {
			gatedNS := 0
			for _, n := range gated[ns] {
				gatedNS += n
			}
			for t, p := range c.NamespacePools[ns] {
				if p.Limit < 0 {
					continue
				}
				if out[ns] == nil {
					out[ns] = map[string]int{}
				}
				out[ns][t] = max(0, p.Used-gatedNS-p.Limit)
			}
		}
	}
	return out
}

func borrowedFor(nsBorrow map[string]map[string]int, ns, acc string) int {
	key, ok := accelerator.FindKey(nsBorrow[ns], acc)
	if !ok {
		return 0
	}
	return nsBorrow[ns][key]
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func compareLenderKeys(a, b LenderKey) int {
	return cmp.Or(strings.Compare(a.Cohort, b.Cohort), strings.Compare(a.Flavor, b.Flavor),
		strings.Compare(a.Resource, b.Resource), strings.Compare(a.Lender, b.Lender))
}
