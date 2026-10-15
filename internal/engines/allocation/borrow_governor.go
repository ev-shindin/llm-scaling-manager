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

// EntryReason says why a ledger entry is notable.
type EntryReason string

// ReasonReleaseNotActuated marks a ledger entry still in flight after twice
// the termination grace plus the KEDA scale-down window: the target was never
// actuated, or a pod is stuck. It keeps blocking.
const ReasonReleaseNotActuated EntryReason = "release-not-actuated"

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

// releasedReplicas is one variant's part of a released unit: its target after
// the release.
type releasedReplicas struct {
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
	Units      []releasedReplicas `json:"units,omitempty"`
	ReleasedAt time.Time          `json:"releasedAt"`
	PodsGoneAt *time.Time         `json:"podsGoneAt,omitempty"`
	// Unknown is an entry assumed after a restart for a waiting lender with
	// nothing persisted: in flight until the next fresh read.
	Unknown bool `json:"unknown,omitempty"`
	// Reason is ReasonReleaseNotActuated once the entry outlived
	// ReleaseActuation.
	Reason EntryReason `json:"reason,omitempty"`
}

// BorrowLedger is the persisted state: the entries and, per cohort flavor, the
// GPUs released while its lenders keep waiting; the cooldowns in force, per
// cohort and per "namespace/model", until when (rounded up to the minute, so
// a cooldown extended every cycle rewrites the ledger once a minute); and the
// placement-timeout releases still in force, per "namespace/variant", their
// target. All of it is bounded by the cohorts, models and variants the
// controller governs, and pruned as it expires.
type BorrowLedger struct {
	Entries        []ReleaseLedgerEntry `json:"entries"`
	Released       map[string]int       `json:"released,omitempty"`
	CohortCooldown map[string]time.Time `json:"cohortCooldown,omitempty"`
	ModelCooldown  map[string]time.Time `json:"modelCooldown,omitempty"`
	TimeoutHeld    map[string]int       `json:"timeoutHeld,omitempty"`
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
	// PreemptionUnknown is the namespaces whose Kueue preemptions could not
	// be read this cycle: a model there may have been preempted unseen, so
	// none of them grows on borrowed capacity.
	PreemptionUnknown map[string]bool
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
	preemptUnknown map[string]bool
	started        bool
	// idleSince is when cycles stopped counting borrowing (HoldReleases
	// without Observe); zero while they count it.
	idleSince time.Time
}

// NewBorrowGovernor returns an empty governor.
func NewBorrowGovernor(s BorrowSettings) *BorrowGovernor {
	return &BorrowGovernor{
		settings: s, watches: map[LenderKey]*lenderWatch{}, ledger: map[LenderKey]*ReleaseLedgerEntry{},
		released: map[string]int{}, cohortCooldown: map[string]time.Time{}, modelCooldown: map[string]time.Time{},
		lastGrown: map[string]time.Time{}, timeoutHeld: map[string]stickyRelease{}, held: map[string]bool{},
		preemptUnknown: map[string]bool{},
	}
}

// SetSettings replaces the timings, for a live config change.
func (g *BorrowGovernor) SetSettings(s BorrowSettings) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settings = s
}

// Restore loads a persisted ledger, normally before the first Observe. What it
// cannot use is dropped: an entry naming no complete lender, one that is
// neither assumed (Unknown) nor carries a unit, a unit that releases nothing,
// and a released figure that is not positive. A Restore that comes late --
// after cycles have run -- adds only what the governor does not already hold,
// never overwriting a newer entry. Entries stamped in the future, and figures
// past what the settings allow, are clamped by the next Observe.
func (g *BorrowGovernor) Restore(l BorrowLedger) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range l.Entries {
		e := l.Entries[i]
		k := e.LenderKey
		if k.Cohort == "" || k.Lender == "" || k.Flavor == "" || k.Resource == "" || e.ReleasedAt.IsZero() {
			continue
		}
		e.Units = slices.DeleteFunc(slices.Clone(e.Units), func(u releasedReplicas) bool {
			return u.Namespace == "" || u.Variant == "" || u.To < 0
		})
		if len(e.Units) == 0 && !e.Unknown {
			continue
		}
		if _, held := g.ledger[k]; held {
			continue
		}
		g.ledger[k] = &e
	}
	for fk, n := range l.Released {
		if _, held := g.released[fk]; n > 0 && !held && strings.Count(fk, "/") >= 2 {
			g.released[fk] = n
		}
	}
	restoreClocks(g.cohortCooldown, l.CohortCooldown)
	restoreClocks(g.modelCooldown, l.ModelCooldown)
	for k, to := range l.TimeoutHeld {
		if _, held := g.timeoutHeld[k]; !held && to >= 0 && strings.Contains(k, "/") {
			g.timeoutHeld[k] = stickyRelease{to: to}
		}
	}
}

// restoreClocks adds the persisted untils dst does not hold.
func restoreClocks(dst, src map[string]time.Time) {
	for k, until := range src {
		if _, held := dst[k]; !held && k != "" && !until.IsZero() {
			dst[k] = until
		}
	}
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
	out.CohortCooldown = persistClocks(g.cohortCooldown)
	out.ModelCooldown = persistClocks(g.modelCooldown)
	if len(g.timeoutHeld) > 0 {
		out.TimeoutHeld = map[string]int{}
		for k, r := range g.timeoutHeld {
			out.TimeoutHeld[k] = r.to
		}
	}
	return out
}

// persistClocks is the untils rounded up to the minute, nil when empty.
func persistClocks(m map[string]time.Time) map[string]time.Time {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(m))
	for k, until := range m {
		r := until.Truncate(time.Minute)
		if r.Before(until) {
			r = r.Add(time.Minute)
		}
		out[k] = r.UTC()
	}
	return out
}

// MarshalLedger is Ledger as JSON, for the controller-status ConfigMap.
func (g *BorrowGovernor) MarshalLedger() (string, error) {
	b, err := json.Marshal(g.Ledger())
	if err != nil {
		return "", fmt.Errorf("borrow release ledger: %w", err)
	}
	return string(b), nil
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

// MarkPreempted starts the model's cooldown from at, when Kueue preempted one
// of its borrowed pods: a preemption found again after a restart cools down
// only for what is left of its cooldown, and never shortens a later one.
// modelKey is "namespace/model".
func (g *BorrowGovernor) MarkPreempted(modelKey string, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if until := at.Add(g.settings.Cooldown); until.After(g.modelCooldown[modelKey]) {
		g.modelCooldown[modelKey] = until
	}
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
// now: its model is cooling down after a preemption, its namespace's
// preemptions could not be read this cycle, or the variant has unplaced
// replicas (the placement hold; for a LeaderWorkerSet until the whole group
// placed). Guaranteed growth is never blocked here.
func (g *BorrowGovernor) BorrowBlocked(namespace, modelID, variant string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.preemptUnknown[namespace] {
		return true
	}
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
	g.idleSince = time.Time{}
	g.preemptUnknown = maps.Clone(c.PreemptionUnknown)
	for _, e := range g.ledger {
		if e.ReleasedAt.After(c.Now) {
			e.ReleasedAt = c.Now // a restored stamp from the future
		}
	}
	states := indexStates(c.Requests)
	g.pruneClocks(c.Now, states)
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
	seen := map[LenderKey]bool{}
	// listed is every pool of the cycle, fresh holds the value false for a
	// stale one; rawWaiting is the fresh pools some lender of which read
	// waiting, undebounced.
	listed, rawWaiting := map[string]bool{}, map[string]bool{}
	anyStale := false
	for _, ch := range chunks {
		listed[ch.key()] = !ch.Stale
		anyStale = anyStale || ch.Stale
		for _, l := range ch.Lenders {
			key := LenderKey{Cohort: ch.Cohort, Lender: l.Queue, Flavor: ch.Flavor, Resource: ch.Resource}
			seen[key] = true
			if ch.Stale {
				continue // frozen: nothing about this lender changes
			}
			rawWaiting[ch.key()] = rawWaiting[ch.key()] || l.Waiting
			g.observeLender(key, l.Waiting, ch.ReadAt, c.Now)
			if e := g.ledger[key]; e != nil {
				g.advanceEntry(e, ch.ReadAt, states, c.Now)
			}
			if g.waiting(key, c.Now) {
				g.cohortCooldown[ch.Cohort] = c.Now.Add(g.settings.Cooldown)
			}
		}
	}
	// known is a pool read fresh this cycle, or one no longer listed in a cycle
	// that read nothing stale (its cohort or flavor went).
	known := func(fk string) bool {
		fresh, ok := listed[fk]
		return fresh || (!ok && !anyStale)
	}
	// A lender missing from a pool read fresh -- its ClusterQueue deleted,
	// stopped, or moved to another cohort -- waits for nothing: its watch and
	// its release in flight go, or the release would pin its variant forever.
	for k := range g.watches {
		if !seen[k] && known(k.flavorKey()) {
			delete(g.watches, k)
		}
	}
	for k := range g.ledger {
		if !seen[k] && known(k.flavorKey()) {
			delete(g.ledger, k)
		}
	}
	// What a flavor's lenders were handed back is forgotten only once a fresh
	// read finds none of them waiting. Not on the debounced figure: right
	// after a restart no lender has two reads yet, and the persisted figure
	// must survive that.
	for fk := range g.released {
		if known(fk) && !rawWaiting[fk] {
			delete(g.released, fk)
		}
	}
	g.started = true
}

// pruneClocks drops expired cooldowns and clamps one further away than the
// cooldown (a restored, or changed, setting), and forgets the growth of
// variants no longer observed.
func (g *BorrowGovernor) pruneClocks(now time.Time, states map[string]variantRef) {
	for _, m := range []map[string]time.Time{g.cohortCooldown, g.modelCooldown} {
		for k, until := range m {
			switch {
			case !now.Before(until):
				delete(m, k)
			case until.Sub(now) > g.settings.Cooldown:
				m[k] = now.Add(g.settings.Cooldown)
			}
		}
	}
	for k := range g.lastGrown {
		if _, ok := states[k]; !ok {
			delete(g.lastGrown, k)
		}
	}
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
	g.applyPins(decisions, byVariant)
	for i := range decisions {
		d := &decisions[i]
		if d.TargetReplicas > d.CurrentReplicas {
			g.lastGrown[d.Namespace+"/"+d.VariantName] = c.Now
		}
	}
	return out
}

// applyPins keeps every release in flight in force on decisions.
func (g *BorrowGovernor) applyPins(decisions []domain.VariantDecision, byVariant map[string]int) {
	for _, e := range g.ledger {
		for _, u := range e.Units {
			lowerTo(decisions, byVariant, u.Namespace+"/"+u.Variant, u.To, "borrow-released")
		}
	}
	for k, r := range g.timeoutHeld {
		lowerTo(decisions, byVariant, k, r.to, "borrow-unplaced")
	}
}

// HoldReleases keeps the releases in flight in force in a cycle that counts
// no borrowing, so neither the demand floor nor the hold raises a released
// variant again while, say, a Kueue read failed; nothing new is decided and
// nothing advances. Once cycles have counted no borrowing for the cooldown
// plus the actuation window, borrowing is taken to be off: the release state
// is dropped, and the next cycle that counts borrowing starts as after a
// restart, assuming a release in flight for every waiting lender until its
// next fresh read.
func (g *BorrowGovernor) HoldReleases(now time.Time, decisions []domain.VariantDecision) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.idleSince.IsZero() {
		g.idleSince = now
	}
	if now.Sub(g.idleSince) > g.settings.Cooldown+g.settings.ReleaseActuation {
		clear(g.ledger)
		clear(g.watches)
		clear(g.released)
		clear(g.timeoutHeld)
		g.started = false
		return
	}
	byVariant := map[string]int{}
	for i := range decisions {
		byVariant[decisions[i].Namespace+"/"+decisions[i].VariantName] = i
	}
	g.applyPins(decisions, byVariant)
}

// timeoutReleases releases borrowed units unplaced past the placement timeout
// (B5): the unplaced replicas a borrowing namespace holds past its guaranteed
// figure, for a P/D model with the partner replicas that keep the pair's ratio
// (releaseUnit). Growth within guaranteed -- a reclaiming queue's growth into
// nominal it lent out -- is exempt, since Kueue reclaims for it. "Past
// guaranteed" counts the replicas the namespace holds, gated ones included
// (heldPast): a replica Kueue gates while its queue already reserves its whole
// guaranteed figure -- the replacement of a preempted borrowed pod -- has
// nothing to reclaim and would wait forever. Each release is taken off
// heldPast, so two models of one namespace never both release the same
// borrowed GPUs; one release per model per cycle. The model's cooldown starts.
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
			typeKey, ok := accelerator.FindKey(heldPast[req.Namespace], accOf(req, st.VariantName))
			if !ok {
				continue
			}
			borrowed := heldPast[req.Namespace][typeKey]
			gpr := max(st.GPUsPerReplica, 1)
			if borrowed < gpr {
				continue // within guaranteed: Kueue reclaims for it
			}
			k := min(st.UnplacedReplicas, borrowed/gpr, st.CurrentReplicas-minOf(st))
			unit, gpus := releaseUnit(req, st.VariantName, k, borrowed, states)
			if unit == nil || !unitFits(unit, req.Namespace, states) {
				continue
			}
			for _, v := range slices.Sorted(maps.Keys(unit)) {
				k := req.Namespace + "/" + v
				s := states[k].state
				to := s.CurrentReplicas - unit[v]
				g.timeoutHeld[k] = stickyRelease{to: to}
				out = append(out, BorrowRelease{Namespace: req.Namespace, ModelID: req.ModelID, Variant: v,
					From: s.CurrentReplicas, To: to, Cause: ReleasePlacementTimeout})
			}
			heldPast[req.Namespace][typeKey] -= gpus
			g.modelCooldown[req.Namespace+"/"+req.ModelID] = c.Now.Add(g.settings.Cooldown)
			break // one release per model per cycle: the counts above are now stale
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
		for _, v := range slices.Sorted(maps.Keys(pick.unit)) {
			k := pick.req.Namespace + "/" + v
			s := states[k].state
			to := s.CurrentReplicas - pick.unit[v]
			entry.Units = append(entry.Units, releasedReplicas{Namespace: pick.req.Namespace, Variant: v, To: to})
			out = append(out, BorrowRelease{Namespace: pick.req.Namespace, ModelID: pick.req.ModelID, Variant: v,
				From: s.CurrentReplicas, To: to, Cause: ReleaseLenderWaiting, Lender: w.key})
		}
		slices.SortFunc(entry.Units, func(a, b releasedReplicas) int { return strings.Compare(a.Variant, b.Variant) })
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
			unit, gpus := releaseUnit(req, st.VariantName, 1, -1, states)
			if unit == nil || !unitFits(unit, req.Namespace, states) {
				continue
			}
			if gpus > nsBorrow[req.Namespace][typeKey] {
				continue // never below the guaranteed figure
			}
			var grown time.Time
			cost := 0.0
			for v, n := range unit {
				s := states[req.Namespace+"/"+v]
				if t, ok := g.lastGrown[req.Namespace+"/"+v]; ok && t.After(grown) {
					grown = t
				}
				cost += float64(n) * s.cost
			}
			cand := releaseCandidate{req: req, unit: unit, gpus: gpus, typeKey: typeKey, grown: grown, cost: cost}
			// One candidate per model: its smallest unit. Releasing one decode
			// of a 1P:2D model hands back what releasing the pair would, without
			// taking the model down.
			i := slices.IndexFunc(out, func(o releaseCandidate) bool { return modelKey(o.req) == modelKey(req) })
			switch {
			case i < 0:
				out = append(out, cand)
			case gpus < out[i].gpus:
				out[i] = cand
			}
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

// releaseUnit is the unit that releases k replicas of variant, and its GPUs.
// For a single-role model that is the k replicas. For a disaggregated model it
// adds, per partner role, the replicas that keep the pair's ratio -- the
// role's total times k over the variant's role total, rounded down -- so a
// role reaches 0 only when every role does: 1P:2D releasing one decode keeps
// its prefill (1P:1D), releasing its prefill takes both decodes with it. A
// role split over several variants counts all of them, and its part is taken
// from the variants with the most replicas above minReplicas first. k shrinks
// until the unit's GPUs fit capGPUs (negative: no cap), but never below one
// replica: a unit larger than what is borrowed is still released. Nil when no
// unit releases a replica of variant within minReplicas.
func releaseUnit(req ModelScalingRequest, variant string, k, capGPUs int, states map[string]variantRef) (map[string]int, int) {
	if k <= 0 {
		return nil, 0
	}
	gprOf := func(v string) int { return max(states[req.Namespace+"/"+v].state.GPUsPerReplica, 1) }
	if !req.Disaggregated {
		return map[string]int{variant: k}, k * gprOf(variant)
	}
	role := ""
	totals := map[string]int{}
	byRole := map[string][]domain.VariantReplicaState{}
	for _, st := range req.VariantStates {
		if st.VariantName == variant {
			role = st.Role
		}
		totals[st.Role] += st.CurrentReplicas
		byRole[st.Role] = append(byRole[st.Role], st)
	}
	if totals[role] <= 0 {
		return nil, 0
	}
	for ; k >= 1; k-- {
		unit := map[string]int{variant: k}
		gpus := k * gprOf(variant)
		fits := true
		for r, total := range totals {
			if r == role {
				continue
			}
			n := k * total / totals[role]
			sts := slices.Clone(byRole[r])
			slices.SortStableFunc(sts, func(a, b domain.VariantReplicaState) int {
				return cmp.Or(cmp.Compare(b.CurrentReplicas-minOf(b), a.CurrentReplicas-minOf(a)), strings.Compare(a.VariantName, b.VariantName))
			})
			for _, st := range sts {
				take := min(n, max(0, st.CurrentReplicas-minOf(st)))
				if take > 0 {
					unit[st.VariantName] += take
					gpus += take * gprOf(st.VariantName)
					n -= take
				}
			}
			fits = fits && n == 0
		}
		if fits && (capGPUs < 0 || gpus <= capGPUs || k == 1) {
			return unit, gpus
		}
	}
	return nil, 0
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

func compareLenderKeys(a, b LenderKey) int {
	return cmp.Or(strings.Compare(a.Cohort, b.Cohort), strings.Compare(a.Flavor, b.Flavor),
		strings.Compare(a.Resource, b.Resource), strings.Compare(a.Lender, b.Lender))
}
