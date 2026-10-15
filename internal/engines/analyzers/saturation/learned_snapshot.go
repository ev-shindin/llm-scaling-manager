package saturation

import (
	"maps"
	"math"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// What the analyzer has learned, as carried across a controller restart. See
// docs/proposals/learned-state-snapshot.md.
//
// Two rules decide everything below.
//
// A RESTORE NEVER MAKES A FIGURE YOUNGER. Every entry keeps the clocks it had,
// and the horizons are applied to them on the way in, exactly as the sweep
// would have applied them had the controller not stopped.
//
// A PER-VARIANT ENTRY IS INSTALLED ONLY UNDER THE ENGINE CONFIGURATION IT WAS
// LEARNED UNDER. The configuration a variant runs is known only once its scale
// target has been read, which happens per cycle in the engine, so restored
// per-variant entries wait as pending and are installed, or rejected, the first
// time Analyze sees their variant (installRestored). An entry whose variant
// cannot be named from its key, or whose variant had no configuration when it
// was written, cannot be checked and is not restored.
//
// "The configuration" means one this controller read COMPLETELY. A flag it
// could not resolve -- `--max-num-seqs $VLLM_MAX_NUM_SEQS` from an envFrom --
// hashes as its default, so two processes that both failed to read it agree
// whatever its value: equality there proves nothing, and a k2 learned at 256
// sequences installed on a 64-sequence engine prices it too high. Such a
// variant's state is neither recorded nor installed (engineFingerprint makes
// the same call for sharing).
//
// Pending entries are exported until they are installed or rejected. The first
// write after a restart happens a minute in, and a variant that has not been
// analysed by then -- a model parked at zero, one whose policy has not loaded
// -- would otherwise be missing from it, and lost at the next restart.
//
// No clock is restored in the future. A document is not trusted: a clock
// ahead of now would make a figure younger than it is, a window that never
// goes stale or a borrow held at full strength for as long as the clock is
// ahead. A clock within maxClockSkew of now is taken as now, for two nodes
// whose clocks disagree slightly; one further ahead drops its entry.

// LearnedState is the analyzer's learned state in a form that survives a
// process: plain values and their clocks, keyed by the analyzer's own keys.
type LearnedState struct {
	K2            map[string]capacity.RollingAverageSnapshot `json:"k2,omitempty"`
	Throughput    map[string]ThroughputSnapshot              `json:"throughput,omitempty"`
	ITLBaseline   map[string]float64                         `json:"itlBaseline,omitempty"`
	StartSeconds  map[string]float64                         `json:"startSeconds,omitempty"`
	VariantSeenAt map[string]time.Time                       `json:"variantSeenAt,omitempty"`
	// Accelerators is keyed namespace/variant: a per-variant entry like the
	// rest, checked against the variant's configuration before it is used.
	Accelerators    map[string]AcceleratorSnapshot `json:"accelerators,omitempty"`
	ITLLines        map[string]LineSnapshot        `json:"itlLines,omitempty"`
	K2BorrowedSince map[string]time.Time           `json:"k2BorrowedSince,omitempty"`
	// K2BorrowState is what each borrow clock keeps beside its start, keyed
	// like K2BorrowedSince. A document written before it existed has none;
	// its clocks are restored under the old rule and take today's figures as
	// what they were armed on (borrowedK2).
	K2BorrowState map[string]BorrowStateSnapshot `json:"k2BorrowState,omitempty"`
	// Forgotten is the last forget token acted on per namespace/variant
	// (ForgetVariant): the tombstone that keeps a restart from acting on an
	// annotation that is still in place, and forgetting again.
	Forgotten map[string]ForgetMarkSnapshot `json:"forgotten,omitempty"`
	// Config is the engine-configuration fingerprint each variant ran when
	// this was exported, keyed namespace/variant.
	Config map[string]string `json:"config,omitempty"`
}

// ThroughputSnapshot is a saturated-throughput window with the two side
// figures that travel with it.
type ThroughputSnapshot struct {
	capacity.RollingAverageSnapshot
	SampledAt time.Time `json:"sampledAt"`
	LastRead  float64   `json:"lastRead"`
}

// AcceleratorSnapshot is the last accelerator that resolved for a variant.
type AcceleratorSnapshot struct {
	Name     string    `json:"name"`
	LastUsed time.Time `json:"lastUsed"`
}

// BorrowStateSnapshot is a borrow clock's state (borrowState).
type BorrowStateSnapshot struct {
	Tick    time.Time `json:"tick"`
	Seen    time.Time `json:"seen"`
	DonorK2 int64     `json:"donorK2"`
	Args    string    `json:"args"`
}

// LineSnapshot is a fitted ITL line shared under an engine configuration.
type LineSnapshot struct {
	A           float64   `json:"a"`
	B           float64   `json:"b"`
	FromVariant string    `json:"fromVariant"`
	LearnedAt   time.Time `json:"learnedAt"`
}

// RestoreReport counts what a restore did with each kind of entry, for the
// log line an operator reads after a restart.
type RestoreReport struct {
	// Installed now: the shared ITL lines, whose key is the configuration.
	ITLLines int
	// Pending: per-variant entries waiting for their variant's first cycle.
	PendingVariants int
	// Not restored: past their horizon, unusable values -- a clock in the
	// future included -- or a variant that cannot be named or had no complete
	// configuration recorded.
	Aged, Unusable, Unattributable int
	// BorrowClocks is how many k2 borrow clocks were installed now. A clock
	// is not a figure and is not checked against a configuration: it can
	// only end a borrow sooner than re-arming it would, never price a
	// variant above its own figure (borrowedK2).
	BorrowClocks int
	// Forgotten is how many entries were left out because this process
	// forgot their variant on request before the restore ran.
	Forgotten int
}

// maxClockSkew is how far ahead of now a restored clock may be and still be
// taken, as now: two nodes' clocks disagreeing slightly is not a reason to
// lose a figure.
const maxClockSkew = time.Minute

// notFuture returns t, or now when t is less than maxClockSkew ahead of it;
// ok is false for a clock further ahead.
func notFuture(t, now time.Time) (time.Time, bool) {
	switch {
	case t.After(now.Add(maxClockSkew)):
		return t, false
	case t.After(now):
		return now, true
	}
	return t, true
}

// windowClocks applies notFuture to both clocks of a restored window.
func windowClocks(w capacity.RollingAverageSnapshot, now time.Time) (capacity.RollingAverageSnapshot, bool) {
	used, ok1 := notFuture(w.LastUsed, now)
	written, ok2 := notFuture(w.LastWritten, now)
	w.LastUsed, w.LastWritten = used, written
	return w, ok1 && ok2
}

// RestoredVariant is what installRestored did with one variant's entries.
type RestoredVariant struct {
	Namespace, Variant string
	// Installed is false when the configuration differed.
	Installed bool
	// Entries is how many entries were installed, or rejected.
	Entries int
}

// pendingVariant is one variant's restored entries awaiting the check.
type pendingVariant struct {
	expect string // the fingerprint they were learned under
	state  LearnedState
}

// alive says whether any of the entries could still be installed: one within
// its horizon on the clock it ages on live. A variant noted present keeps its
// liveness fresh (notePendingPresent), so a parked one stays; a deleted one's
// entries all age out, and it goes -- however often the controller restarts.
// A clock in the future counts as dead, as it does at install: kept alive by
// it, an entry that can never be installed would be written back forever.
func (p *pendingVariant) alive(now, wall time.Time, h capacity.Horizons) bool {
	s := p.state
	for _, w := range s.K2 {
		if within(w.LastUsed, wall, h.BucketRetention) {
			return true
		}
	}
	for _, w := range s.Throughput {
		if within(w.LastUsed, wall, h.BucketRetention) {
			return true
		}
	}
	for _, seen := range s.VariantSeenAt {
		if within(seen, now, h.VariantTimeout) {
			return true
		}
	}
	for _, m := range s.Accelerators {
		if within(m.LastUsed, wall, h.BucketRetention) {
			return true
		}
	}
	borrowLife := BorrowedK2Hold + BorrowedK2Relax + h.BucketRetention
	for k, since := range s.K2BorrowedSince {
		if st, ok := s.K2BorrowState[k]; ok {
			if within(st.Seen, now, h.VariantTimeout) {
				return true
			}
			continue
		}
		if within(since, now, borrowLife) {
			return true
		}
	}
	return false
}

// pruneWindows drops the k2 and throughput windows past the bucket retention
// from a variant kept pending by its liveness: install would drop them, and
// written back they only take room from the size bound.
func (p *pendingVariant) pruneWindows(wall time.Time, h capacity.Horizons) {
	maps.DeleteFunc(p.state.K2, func(_ string, w capacity.RollingAverageSnapshot) bool {
		return !within(w.LastUsed, wall, h.BucketRetention)
	})
	maps.DeleteFunc(p.state.Throughput, func(_ string, w ThroughputSnapshot) bool {
		return !within(w.LastUsed, wall, h.BucketRetention)
	})
}

// within says whether clock t, as of ref, is inside horizon and not further
// ahead of ref than maxClockSkew.
func within(t, ref time.Time, horizon time.Duration) bool {
	return !t.After(ref.Add(maxClockSkew)) && ref.Sub(t) <= horizon
}

func (p *pendingVariant) size() int {
	s := p.state
	return len(s.K2) + len(s.Throughput) + len(s.VariantSeenAt) + len(s.K2BorrowedSince) + len(s.Accelerators)
}

// variantOfKey names the variant (namespace/variant) and model a learned-state
// key belongs to, for every per-variant key shape the analyzer files:
//
//	namespace|variant|model|...                  k2, throughput and a borrowed k2's clock
//	namespace|model|variant|accelerator|gpus     ITL baseline, start estimate, liveness (itlWindowKey)
//	namespace|variant                            a borrowed k2's clock, as #132 first keyed it
//
// The shape is told apart by field count, which no field can fake: no
// Kubernetes name and no model ID contains "|".
func variantOfKey(key string) (variant, model string, ok bool) {
	f := strings.Split(key, "|")
	switch {
	case len(f) == 2:
		return f[0] + "/" + f[1], "", true
	case len(f) == 5:
		return f[0] + "/" + f[2], f[1], true
	case len(f) >= 6:
		return f[0] + "/" + f[1], f[2], true
	}
	return "", "", false
}

func finitePositive(v float64) bool { return v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }

// ExportLearnedState copies what the analyzer has learned, with the engine
// configuration each of its variants runs now. Safe to call concurrently with
// analysis.
func (a *SaturationAnalyzer) ExportLearnedState() LearnedState {
	a.mu.Lock()
	s := LearnedState{
		K2:              make(map[string]capacity.RollingAverageSnapshot, len(a.computeCapacityHistory)),
		Throughput:      make(map[string]ThroughputSnapshot, len(a.saturatedThroughput)),
		ITLBaseline:     make(map[string]float64, len(a.itlBaseline)),
		StartSeconds:    make(map[string]float64, len(a.startSeconds)),
		VariantSeenAt:   make(map[string]time.Time, len(a.variantSeenAt)),
		Accelerators:    make(map[string]AcceleratorSnapshot, len(a.lastAccelerator)),
		ITLLines:        make(map[string]LineSnapshot, len(a.itlLines)),
		K2BorrowedSince: make(map[string]time.Time, len(a.k2BorrowedSince)),
		K2BorrowState:   make(map[string]BorrowStateSnapshot, len(a.k2BorrowState)),
		Forgotten:       make(map[string]ForgetMarkSnapshot, len(a.forgotten)),
		Config:          map[string]string{},
	}
	for k, ra := range a.computeCapacityHistory {
		s.K2[k] = ra.Snapshot()
	}
	for k, ra := range a.saturatedThroughput {
		s.Throughput[k] = ThroughputSnapshot{
			RollingAverageSnapshot: ra.Snapshot(),
			SampledAt:              a.throughputSampledAt[k],
			LastRead:               a.throughputLastRead[k],
		}
	}
	for k, v := range a.itlBaseline {
		s.ITLBaseline[k] = v
	}
	for k, v := range a.startSeconds {
		s.StartSeconds[k] = v
	}
	for k, v := range a.variantSeenAt {
		s.VariantSeenAt[k] = v
	}
	for k, m := range a.lastAccelerator {
		s.Accelerators[k] = AcceleratorSnapshot{Name: m.name, LastUsed: m.lastUsed}
	}
	for k, l := range a.itlLines {
		s.ITLLines[k] = LineSnapshot{A: l.model.A, B: l.model.B, FromVariant: l.fromVariant, LearnedAt: l.learnedAt}
	}
	for k, v := range a.k2BorrowedSince {
		s.K2BorrowedSince[k] = v
	}
	for k, st := range a.k2BorrowState {
		s.K2BorrowState[k] = BorrowStateSnapshot{Tick: st.tick, Seen: st.seen, DonorK2: st.donorK2, Args: st.args}
	}
	for k, m := range a.forgotten {
		s.Forgotten[k] = ForgetMarkSnapshot{Token: m.token, Seen: m.seen}
	}
	a.mu.Unlock()

	// The configuration each variant runs, read outside a.mu: the store has
	// its own lock. A variant with no parsed configuration gets no entry, and
	// its learned state will not be restored -- it could not be checked.
	models := map[string]string{}
	note := func(key string) {
		if v, m, ok := variantOfKey(key); ok && m != "" {
			models[v] = m
		}
	}
	for k := range s.K2 {
		note(k)
	}
	for k := range s.Throughput {
		note(k)
	}
	for k := range s.VariantSeenAt {
		note(k)
	}
	// A borrowing variant can have a clock and nothing else: no window of its
	// own, which is why it borrows. Unnamed here, its clock had no
	// configuration to be checked against and was not restored.
	for k := range s.K2BorrowedSince {
		note(k)
	}
	for v, m := range models {
		ns, name, _ := strings.Cut(v, "/")
		if rec := a.capacityStore.Get(ns, m, name); rec != nil && rec.EngineParams.Complete() {
			s.Config[v] = rec.EngineParams.Fingerprint()
		}
	}

	// Entries still pending go out as they came in, under the configuration
	// they were learned under -- unless this process has learned the variant
	// under another one since, when its own record is the truth. Live entries
	// win a key both have.
	a.mu.Lock()
	defer a.mu.Unlock()
	for v, p := range a.pendingRestore {
		cfg, ok := s.Config[v]
		if ok && cfg != p.expect {
			continue
		}
		if !ok && models[v] != "" {
			// Learned live with no configuration known -- the scale target not
			// read yet, or not completely. Under the pending entries' fingerprint
			// those figures would read as checked; on their own they would not
			// be restored anyway.
			dropVariant(&s, v)
		}
		s.Config[v] = p.expect
		mergeAbsent(s.K2, p.state.K2)
		mergeAbsent(s.Throughput, p.state.Throughput)
		mergeAbsent(s.VariantSeenAt, p.state.VariantSeenAt)
		mergeAbsent(s.ITLBaseline, p.state.ITLBaseline)
		mergeAbsent(s.StartSeconds, p.state.StartSeconds)
		mergeAbsent(s.K2BorrowedSince, p.state.K2BorrowedSince)
		mergeAbsent(s.K2BorrowState, p.state.K2BorrowState)
		mergeAbsent(s.Accelerators, p.state.Accelerators)
	}
	return s
}

// dropVariant removes one variant's per-variant entries from s.
func dropVariant(s *LearnedState, v string) {
	of := func(k string) bool { got, _, ok := variantOfKey(k); return ok && got == v }
	maps.DeleteFunc(s.K2, func(k string, _ capacity.RollingAverageSnapshot) bool { return of(k) })
	maps.DeleteFunc(s.Throughput, func(k string, _ ThroughputSnapshot) bool { return of(k) })
	maps.DeleteFunc(s.VariantSeenAt, func(k string, _ time.Time) bool { return of(k) })
	maps.DeleteFunc(s.ITLBaseline, func(k string, _ float64) bool { return of(k) })
	maps.DeleteFunc(s.StartSeconds, func(k string, _ float64) bool { return of(k) })
	// Borrow clocks stay: they are not figures, and are restored without a
	// configuration to check them against (RestoreLearnedState).
	delete(s.Accelerators, v)
}

// mergeAbsent copies into dst the keys of src it does not have.
func mergeAbsent[V any](dst, src map[string]V) {
	for k, v := range src {
		if _, have := dst[k]; !have {
			dst[k] = v
		}
	}
}

// RestoreLearnedState takes a stored state in. The shared ITL lines, whose key
// IS the configuration, are installed now; per-variant entries -- the
// accelerator memo among them -- are held until installRestored checks their
// variant's configuration. Nothing already learned in this process is
// overwritten.
func (a *SaturationAnalyzer) RestoreLearnedState(s LearnedState) RestoreReport {
	var rep RestoreReport
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	bucketRetention := a.horizons.BucketRetention

	// A variant this process forgot on request before the restore ran -- the
	// first read failed and the cycles went on -- has a forget mark the
	// stored document does not: what the document holds for it is what the
	// forget dropped, and installing it would undo the forget while its token
	// stays consumed. A mark the document shares was acted on before it was
	// written, so the document is already past it.
	forgottenHere := func(v string) bool {
		live, ok := a.forgotten[v]
		return ok && s.Forgotten[v].Token != live.token
	}
	forgottenKey := func(key string) bool {
		v, _, ok := variantOfKey(key)
		return ok && forgottenHere(v)
	}

	for k, l := range s.ITLLines {
		_, live := a.itlLines[k]
		learnedAt, inTime := notFuture(l.LearnedAt, now)
		switch {
		case live:
		case forgottenKey(l.FromVariant):
			rep.Forgotten++
		case l.LearnedAt.IsZero() || !inTime || !itl.ValidModel(l.A, l.B):
			rep.Unusable++
		case now.Sub(learnedAt) > bucketRetention:
			rep.Aged++
		default:
			a.itlLines[k] = learnedLine{model: itl.Model{A: l.A, B: l.B}, fromVariant: l.FromVariant, learnedAt: learnedAt}
			rep.ITLLines++
		}
	}

	// Forget marks are installed now: a mark is not a figure, has nothing to
	// check against a configuration, and must be in place before the
	// engine's first cycle reads the annotation it answers.
	for k, m := range s.Forgotten {
		seen, inTime := notFuture(m.Seen, now)
		if _, live := a.forgotten[k]; live || m.Token == "" || seen.IsZero() || !inTime ||
			now.Sub(seen) > a.horizons.VariantTimeout {
			continue
		}
		a.forgotten[k] = forgetMark{token: m.Token, seen: seen}
	}

	if a.pendingRestore == nil {
		a.pendingRestore = map[string]*pendingVariant{}
	}
	pendingFor := func(v string) *pendingVariant {
		if forgottenHere(v) {
			rep.Forgotten++
			return nil
		}
		expect := s.Config[v]
		if expect == "" {
			rep.Unattributable++
			return nil
		}
		p := a.pendingRestore[v]
		if p == nil {
			p = &pendingVariant{expect: expect}
			a.pendingRestore[v] = p
		}
		return p
	}
	pending := func(key string) *pendingVariant {
		v, _, ok := variantOfKey(key)
		if !ok {
			rep.Unattributable++
			return nil
		}
		return pendingFor(v)
	}
	for k, m := range s.Accelerators {
		if p := pendingFor(k); p != nil {
			if p.state.Accelerators == nil {
				p.state.Accelerators = map[string]AcceleratorSnapshot{}
			}
			p.state.Accelerators[k] = m
		}
	}
	for k, w := range s.K2 {
		if p := pending(k); p != nil {
			if p.state.K2 == nil {
				p.state.K2 = map[string]capacity.RollingAverageSnapshot{}
			}
			p.state.K2[k] = w
		}
	}
	for k, w := range s.Throughput {
		if p := pending(k); p != nil {
			if p.state.Throughput == nil {
				p.state.Throughput = map[string]ThroughputSnapshot{}
			}
			p.state.Throughput[k] = w
		}
	}
	for k, seen := range s.VariantSeenAt {
		p := pending(k)
		if p == nil {
			continue
		}
		if p.state.VariantSeenAt == nil {
			p.state.VariantSeenAt = map[string]time.Time{}
			p.state.ITLBaseline = map[string]float64{}
			p.state.StartSeconds = map[string]float64{}
		}
		p.state.VariantSeenAt[k] = seen
		if b, ok := s.ITLBaseline[k]; ok {
			p.state.ITLBaseline[k] = b
		}
		if t, ok := s.StartSeconds[k]; ok {
			p.state.StartSeconds[k] = t
		}
	}
	for k, since := range s.K2BorrowedSince {
		// A clock with its state is installed now, with no configuration
		// check: it is not a figure, and a borrower whose configuration was
		// not read completely still borrows, so held back it re-armed at full
		// strength on every restart. A changed configuration is caught when
		// the borrow is next read (borrowState.rearms). One from an older
		// document, with no state, keeps the old rule and waits for its
		// variant's configuration.
		if st, ok := s.K2BorrowState[k]; ok {
			switch {
			case forgottenKey(k):
				rep.Forgotten++
			case a.installBorrowClockLocked(k, since, st, now):
				rep.BorrowClocks++
			default:
				rep.Aged++
			}
			continue
		}
		if p := pending(k); p != nil {
			if p.state.K2BorrowedSince == nil {
				p.state.K2BorrowedSince = map[string]time.Time{}
			}
			p.state.K2BorrowedSince[k] = since
		}
	}
	for v, p := range a.pendingRestore {
		if !p.alive(now, time.Now(), a.horizons) {
			rep.Aged += p.size()
			delete(a.pendingRestore, v)
		}
	}
	rep.PendingVariants = len(a.pendingRestore)
	return rep
}

// installRestored checks one variant's pending entries against the
// configuration it runs now and installs them, or drops them, once. The
// configuration is read from the capacity store, which the engine has loaded
// from the scale target before Analyze runs. A variant whose configuration is
// not known yet keeps its entries for the next cycle; ok is false then.
func (a *SaturationAnalyzer) installRestored(namespace, modelID, variant string) (RestoredVariant, bool) {
	key := namespace + "/" + variant
	a.mu.Lock()
	p := a.pendingRestore[key]
	a.mu.Unlock()
	if p == nil {
		return RestoredVariant{}, false
	}
	rec := a.capacityStore.Get(namespace, modelID, variant)
	if rec == nil || rec.EngineParams == nil {
		return RestoredVariant{}, false
	}
	// A configuration not read completely does not match, even an equal
	// fingerprint: see the package comment.
	match := rec.EngineParams.Complete() && rec.EngineParams.Fingerprint() == p.expect

	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pendingRestore, key)
	out := RestoredVariant{Namespace: namespace, Variant: variant, Entries: p.size()}
	if !match {
		return out, true
	}
	out.Installed = true
	out.Entries = a.installLocked(p.state)
	return out, true
}

// installLocked applies the horizons to a checked variant's entries, keeps
// what is still within them and returns how many it kept. Callers hold a.mu.
// The windows and the memo age on the wall clock, as they do live; the rest on
// the analyzer's.
func (a *SaturationAnalyzer) installLocked(s LearnedState) int {
	now := a.now()
	wall := time.Now()
	h := a.horizons
	kept := 0
	for k, w := range s.K2 {
		w, inTime := windowClocks(w, wall)
		if _, live := a.computeCapacityHistory[k]; live || !inTime || wall.Sub(w.LastUsed) > h.BucketRetention {
			continue
		}
		if ra, ok := capacity.RestoreRollingAverage(w, capacity.RollingAverageWindowSize); ok {
			a.computeCapacityHistory[k] = ra
			kept++
		}
	}
	for k, w := range s.Throughput {
		snap, inTime := windowClocks(w.RollingAverageSnapshot, wall)
		sampledAt, sampledInTime := notFuture(w.SampledAt, wall)
		if _, live := a.saturatedThroughput[k]; live || !inTime || !sampledInTime || wall.Sub(snap.LastUsed) > h.BucketRetention {
			continue
		}
		if ra, ok := capacity.RestoreRollingAverage(snap, capacity.RollingAverageWindowSize); ok {
			a.saturatedThroughput[k] = ra
			kept++
			if !sampledAt.IsZero() {
				a.throughputSampledAt[k] = sampledAt
			}
			if finitePositive(w.LastRead) {
				a.throughputLastRead[k] = w.LastRead
			}
		}
	}
	for k, m := range s.Accelerators {
		lastUsed, inTime := notFuture(m.LastUsed, wall)
		if _, live := a.lastAccelerator[k]; live || m.Name == "" || m.LastUsed.IsZero() || !inTime ||
			wall.Sub(lastUsed) > h.BucketRetention {
			continue
		}
		a.lastAccelerator[k] = acceleratorMemo{name: m.Name, lastUsed: lastUsed}
		kept++
	}
	for k, seen := range s.VariantSeenAt {
		seen, inTime := notFuture(seen, now)
		if _, live := a.variantSeenAt[k]; live || seen.IsZero() || !inTime || now.Sub(seen) > h.VariantTimeout {
			continue
		}
		a.variantSeenAt[k] = seen
		kept++
		if b, ok := s.ITLBaseline[k]; ok && finitePositive(b) {
			if _, have := a.itlBaseline[k]; !have {
				a.itlBaseline[k] = b
			}
		}
		if t, ok := s.StartSeconds[k]; ok && finitePositive(t) {
			if _, have := a.startSeconds[k]; !have {
				a.startSeconds[k] = t
			}
		}
	}
	borrowLife := BorrowedK2Hold + BorrowedK2Relax + h.BucketRetention
	for k, since := range s.K2BorrowedSince {
		since, inTime := notFuture(since, now)
		if _, live := a.k2BorrowedSince[k]; live || since.IsZero() || !inTime {
			continue
		}
		// A clock with its state is kept for as long as its variant is seen,
		// spent or not: dropping a spent clock is what re-arms a borrow with
		// no new evidence (k2_borrow.go). One from an older document has no
		// state and keeps the old rule, its start.
		st, hasState := s.K2BorrowState[k]
		if !hasState {
			if now.Sub(since) > borrowLife {
				continue
			}
			a.k2BorrowedSince[k] = since
			kept++
			continue
		}
		if a.installBorrowClockLocked(k, since, st, now) {
			kept++
		}
	}
	return kept
}

// installBorrowClockLocked installs one borrow clock with its state, unless
// the process already runs one under the key or the stored clock is unusable
// or its variant has not been seen within VariantTimeout. It reports whether
// it installed it. The start is taken as stored: the tick it is paired with
// carries the time the controller was down as a gap, which the next serving
// cycle reads as paused time. Callers hold a.mu.
func (a *SaturationAnalyzer) installBorrowClockLocked(k string, since time.Time, st BorrowStateSnapshot, now time.Time) bool {
	since, inTime := notFuture(since, now)
	seen, seenInTime := notFuture(st.Seen, now)
	tick, tickInTime := notFuture(st.Tick, now)
	if _, live := a.k2BorrowedSince[k]; live || since.IsZero() || !inTime ||
		seen.IsZero() || !seenInTime || !tickInTime || now.Sub(seen) > a.horizons.VariantTimeout {
		return false
	}
	a.k2BorrowedSince[k] = since
	a.k2BorrowState[k] = &borrowState{tick: tick, seen: seen, donorK2: st.DonorK2, args: st.Args}
	return true
}

// notePendingPresent keeps a parked variant's pending liveness and memo as
// fresh as NoteVariantsPresent keeps the live ones: a model that stays at zero
// past VariantTimeout after a restart must not lose its ITL baseline and start
// estimate at install for having been parked. Callers hold a.mu.
func (a *SaturationAnalyzer) notePendingPresent(namespace string, prefixes, variantNames []string, now time.Time) {
	for _, v := range variantNames {
		p := a.pendingRestore[namespace+"/"+v]
		if p == nil {
			continue
		}
		for key := range p.state.VariantSeenAt {
			for _, pre := range prefixes {
				if strings.HasPrefix(key, pre) {
					p.state.VariantSeenAt[key] = now
					break
				}
			}
		}
		if m, ok := p.state.Accelerators[namespace+"/"+v]; ok {
			m.LastUsed = time.Now()
			p.state.Accelerators[namespace+"/"+v] = m
		}
		for key, st := range p.state.K2BorrowState {
			if strings.HasPrefix(key, namespace+"|"+v+"|") {
				st.Seen = now
				p.state.K2BorrowState[key] = st
			}
		}
	}
}

// ForgetLearnedState drops everything learned and everything pending, for an
// operator who deleted the stored snapshot: written back from memory, the
// deletion would undo nothing.
func (a *SaturationAnalyzer) ForgetLearnedState() {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.computeCapacityHistory)
	clear(a.saturatedThroughput)
	clear(a.throughputSampledAt)
	clear(a.throughputLastRead)
	clear(a.itlBaseline)
	clear(a.startSeconds)
	clear(a.variantSeenAt)
	clear(a.lastAccelerator)
	clear(a.itlLines)
	clear(a.k2BorrowedSince)
	clear(a.k2BorrowState)
	clear(a.forgotten)
	clear(a.pendingRestore)
}
