package saturation

import (
	"maps"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
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
// A PER-VARIANT ENTRY IS INSTALLED ONLY UNDER THE ENGINE ARGUMENTS IT WAS
// LEARNED UNDER. The configuration a variant runs is known only once its scale
// target has been read, which happens per cycle in the engine, so restored
// per-variant entries wait as pending and are installed, or rejected, the first
// time Analyze sees their variant (installRestored). An entry whose variant
// cannot be named from its key, or whose variant had no configuration when it
// was written, cannot be checked and is not restored.
//
// The k2 and throughput WINDOWS are checked per window, not here: each is
// installed with the engine arguments it was written under (K2Args,
// ThroughputArgs), and its first read applies the same rule a live rollout
// does (window_args.go) -- a k2 window from other arguments becomes the
// variant's own lower-only figure, a throughput window from other arguments
// is dropped. What is checked per variant is what is not per window, in
// three outcomes by how much of the configuration still holds:
//
//   - the whole configuration, image included: everything is installed;
//   - the arguments but not the image (a rollout of a new release while the
//     controller was down): everything but the per-variant ITL baseline is
//     installed -- latency is the engine's code;
//   - neither: only the k2 and throughput windows are installed, under the
//     arguments they were written under, so their first read makes each k2
//     the variant's own lower-only, expiring figure (P2-prior, k2_borrow.go)
//     and puts each throughput window aside, where the old service rate
//     holds the demand floor hold-only for a bounded serving time
//     (throughput_floor.go), as after a live rollout. The start-time
//     estimate and the ITL baseline describe the engine the variant no
//     longer runs, and are not.
//
// A document written before the arguments were recorded (no Args entry) gets
// the old rule: the whole configuration or nothing.
//
// "The configuration" means one this controller read COMPLETELY. A flag it
// could not resolve -- `--max-num-seqs $VLLM_MAX_NUM_SEQS` from an envFrom --
// hashes as its default, so two processes that both failed to read it agree
// whatever its value: equality there proves nothing, and a k2 learned at 256
// sequences installed on a 64-sequence engine prices it too high. Such a
// variant is exported under its incomplete configuration, its windows tagged
// behind incompletePrefix, and only its k2 and throughput windows and
// accelerator memo come back -- as its own if it restarts under the same
// unread configuration, as lower-only previous figures otherwise. Its start
// estimate and ITL baseline are not installed: a match proves nothing about
// them (engineFingerprint makes the same call for sharing).
//
// The per-variant tag is the configuration the variant runs at export. A
// window tagged with the arguments it names carries no tag of its own -- it
// is read as written under them -- and a window still tagged with OTHER
// arguments does not go out among the variant's windows at all: it goes out
// put aside (K2Stash, ThroughputStash), under the arguments it was written
// under, as the windows a rollout put aside do. Among the windows, a
// controller from before the tags (a rollback) would install it as a figure
// of the arguments the variant runs now, with full authority.
//
// Lower bounds (k2_borrow.go) are installed at once, with no configuration
// check: a bound can only make a variant more conservative, for a bounded
// time, and a borrower whose configuration was not read completely still
// borrows, so a bound held back for the check was lost on every restart.
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
	K2          map[string]capacity.RollingAverageSnapshot `json:"k2,omitempty"`
	Throughput  map[string]ThroughputSnapshot              `json:"throughput,omitempty"`
	ITLBaseline map[string]float64                         `json:"itlBaseline,omitempty"`
	// ITLBaselineStash is itlBaselineStash: a variant's ITL baseline put
	// aside under the arguments it was learned under, for a rollback to
	// them. Keyed like the ITL baseline plus "#" and the arguments.
	ITLBaselineStash map[string]float64 `json:"itlBaselineStash,omitempty"`
	// ITLSlopes is each ITL window key's last fitted slope, which the light
	// load hold keeps a line on (itl.Tracker.Observe). Keyed like the ITL
	// baseline and restored with it. Without it a restart under light load
	// had a baseline and no slope, so no line, and the variant was sized by
	// KV occupancy alone (issue #316).
	ITLSlopes     map[string]SlopeSnapshot `json:"itlSlopes,omitempty"`
	StartSeconds  map[string]float64       `json:"startSeconds,omitempty"`
	VariantSeenAt map[string]time.Time     `json:"variantSeenAt,omitempty"`
	// Accelerators is keyed namespace/variant: a per-variant entry like the
	// rest, checked against the variant's configuration before it is used.
	Accelerators map[string]AcceleratorSnapshot `json:"accelerators,omitempty"`
	ITLLines     map[string]LineSnapshot        `json:"itlLines,omitempty"`
	// K2BorrowedSince is the start of each running sibling borrow, the form
	// a controller before LowerBounds reads. Written for a rollback; read only
	// for a key LowerBounds does not have.
	K2BorrowedSince map[string]time.Time `json:"k2BorrowedSince,omitempty"`
	// LowerBounds is each k2 key's lower bound (lowerBound), whole.
	LowerBounds map[string]LowerBoundSnapshot `json:"lowerBounds,omitempty"`
	// K2Args and ThroughputArgs are the engine arguments each k2 and
	// throughput window was written under (window_args.go), keyed like the
	// windows. A window with none is taken as written under the arguments its
	// variant runs when it is installed.
	K2Args         map[string]string `json:"k2Args,omitempty"`
	ThroughputArgs map[string]string `json:"throughputArgs,omitempty"`
	// K2Stash and ThroughputStash are windows put aside under engine
	// arguments their variant no longer runs, keyed window key + "#" + those
	// arguments (window_args.go). A controller from before them ignores the
	// fields, so a rollback never reads them as the variant's own.
	K2Stash         map[string]capacity.RollingAverageSnapshot `json:"k2Stash,omitempty"`
	ThroughputStash map[string]ThroughputSnapshot              `json:"throughputStash,omitempty"`
	// Forgotten is the last forget token acted on per namespace/variant
	// (ForgetVariant): the tombstone that keeps a restart from acting on an
	// annotation that is still in place, and forgetting again.
	Forgotten map[string]ForgetMarkSnapshot `json:"forgotten,omitempty"`
	// Config is the engine-configuration fingerprint each variant ran when
	// this was exported, keyed namespace/variant.
	Config map[string]string `json:"config,omitempty"`
	// Args is the same configuration without the image
	// (EngineParams.ArgsFingerprint), keyed alike.
	Args map[string]string `json:"args,omitempty"`
	// PriorK2 is each previous figure in the form a build with restore
	// priors but before LowerBounds (PR #151) reads, keyed by the variant's
	// own k2 key, dated from its last use. Written for a rollback to such a
	// build: it reads neither LowerBounds nor the stash, so without it every
	// variant whose arguments had changed fell to derived or k1 there. Read
	// from such a build's document, each is installed as a lower bound
	// holding that figure, where LowerBounds has none for the key.
	PriorK2 map[string]PriorK2Snapshot `json:"priorK2,omitempty"`
}

// PriorK2Snapshot is one previous figure as a PR #151 build exported it.
type PriorK2Snapshot struct {
	K2          int64     `json:"k2"`
	InstalledAt time.Time `json:"installedAt"`
}

// ThroughputSnapshot is a saturated-throughput window with the two side
// figures that travel with it.
type ThroughputSnapshot struct {
	capacity.RollingAverageSnapshot
	SampledAt time.Time `json:"sampledAt"`
	LastRead  float64   `json:"lastRead"`
	// HeldSeconds is, for a window put aside, how long the floor has held a
	// fleet on it in serving time (stashedThroughput.heldFor).
	HeldSeconds float64 `json:"heldSeconds,omitempty"`
}

// SlopeSnapshot is an itl.Slope as exported: the slope, when it was fitted,
// which bounds how long it may be held, and the engine arguments it was fitted
// under.
type SlopeSnapshot struct {
	A        float64   `json:"a"`
	FittedAt time.Time `json:"fittedAt"`
	Args     string    `json:"args,omitempty"`
}

// AcceleratorSnapshot is the last accelerator that resolved for a variant.
type AcceleratorSnapshot struct {
	Name     string    `json:"name"`
	LastUsed time.Time `json:"lastUsed"`
}

// LowerBoundSnapshot is a lowerBound as exported.
type LowerBoundSnapshot struct {
	Armed   bool      `json:"armed,omitempty"`
	Since   time.Time `json:"since,omitzero"`
	Tick    time.Time `json:"tick,omitzero"`
	Seen    time.Time `json:"seen"`
	Donor   string    `json:"donor,omitempty"`
	Own     bool      `json:"own,omitempty"`
	DonorK2 int64     `json:"donorK2,omitempty"`
	Args    string    `json:"args,omitempty"`
	// ArgsVersion is the FingerprintVersion Args was hashed under; 0 in a
	// document from before it was recorded.
	ArgsVersion int `json:"argsVersion,omitempty"`
	// Offered is the lowest figure on offer while the bound ran.
	Offered int64 `json:"offered,omitempty"`
	// Prior is the variant's own figure from engine arguments it no longer
	// runs; PriorSince when it became
	// one, PriorAgeSeconds how long ago its window had last been written
	// then, and PriorUsed when it was last offered to a decision, which it
	// ages on. A document from a build that folded rollout readings into
	// the figure also carries rolloutHeld, rolloutRearm, rolloutAt and
	// rolloutStart; they are not read.
	Prior           int64     `json:"prior,omitempty"`
	PriorSince      time.Time `json:"priorSince,omitzero"`
	PriorAgeSeconds float64   `json:"priorAgeSeconds,omitempty"`
	PriorUsed       time.Time `json:"priorUsed,omitzero"`
	// FigureWritten is lowerBound.figureWritten: kept, so a rollback and a
	// re-roll across a restart do not re-arm the same figure either.
	FigureWritten time.Time `json:"figureWritten,omitzero"`
	// RearmPending is lowerBound.rearmPending: kept, so a restart between a
	// change of arguments and the first serving cycle after it does not cost
	// the new arguments part of their hold.
	RearmPending bool `json:"rearmPending,omitempty"`
}

func (lb *lowerBound) snapshot() LowerBoundSnapshot {
	return LowerBoundSnapshot{
		Armed: lb.armed, Since: lb.since, Tick: lb.tick, Seen: lb.seen,
		Donor: lb.donor, Own: lb.own, DonorK2: lb.donorK2, Args: lb.args,
		ArgsVersion: lb.argsVersion, Offered: lb.offered,
		Prior: lb.prior, PriorSince: lb.priorSince, PriorAgeSeconds: lb.priorAge.Seconds(),
		PriorUsed: lb.priorUsed, RearmPending: lb.rearmPending, FigureWritten: lb.figureWritten,
	}
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
	// future included -- or a variant that cannot be named or had no
	// configuration recorded at all (one read incompletely is recorded under
	// its incomplete tag, and its windows are restored).
	Aged, Unusable, Unattributable int
	// BorrowClocks is how many lower bounds (k2_borrow.go) were installed
	// now. A bound is not checked against a configuration: it can only make
	// a variant more conservative, for a bounded time, never price it above
	// its own figure (borrowedK2).
	BorrowClocks int
	// Forgotten is how many entries were left out because this process
	// forgot their variant on request before the restore ran.
	Forgotten int
	// Stashed is how many windows put aside under engine arguments their
	// variant no longer ran were installed now: a stashed window names the
	// arguments it was written under, and only a variant running them takes
	// it back.
	Stashed int
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

// RestoreOutcome is which of installRestored's rules a variant's entries
// met.
type RestoreOutcome string

const (
	// RestoredWhole: the whole configuration matched; everything installed.
	RestoredWhole RestoreOutcome = "whole"
	// RestoredArgs: the arguments matched on another image; everything but
	// the ITL baseline installed.
	RestoredArgs RestoreOutcome = "args"
	// RestoredPrior: the arguments differed; only the k2 and throughput
	// windows installed (installPreviousK2Locked),
	// under the arguments they were written under, each to become the
	// variant's own lower-only figure on its first read (possibly none, when
	// no window was recent enough or none can be read on the hardware the
	// variant runs now).
	RestoredPrior RestoreOutcome = "prior"
	// RestoredIncomplete: the configuration the variant runs now was not read
	// completely, so nothing could be checked against it. The k2 windows of a
	// document that recorded the arguments are still installed under them,
	// as for RestoredPrior: the live path would have made each a lower-only
	// previous figure on its first read under the incomplete configuration's
	// tag, and the restore does the same.
	RestoredIncomplete RestoreOutcome = "incomplete"
	// RestoredDiffers: a document from before the arguments were recorded,
	// whose whole configuration differs -- the first restart after upgrading
	// from a release without them, or past a FingerprintVersion bump. Its k2
	// windows are installed as for RestoredPrior, under a tag no
	// configuration has (legacyArgsPrefix), so each becomes a lower-only
	// previous figure on its first read: dropping them priced every variant
	// in the cluster at derived or k1 at once.
	RestoredDiffers RestoreOutcome = "differs"
)

// legacyArgsPrefix tags the windows of a document that recorded no arguments
// and whose whole configuration differs: no configuration's tag starts with
// it, so the first read moves each window aside and makes its k2 the
// variant's previous figure.
const legacyArgsPrefix = "legacy:"

// RestoredVariant is what installRestored did with one variant's entries.
type RestoredVariant struct {
	Namespace, Variant string
	// Outcome is the rule the entries met; the fields below are its detail.
	Outcome RestoreOutcome
	// Installed is false when the arguments differed.
	Installed bool
	// ImageChanged is true when they were installed under the same arguments
	// on another image, so the ITL baseline was left out.
	ImageChanged bool
	// Priors is how many k2 windows were installed, to become lower-only
	// figures, when the arguments differed. Windows taken back as the
	// variant's own under the same unread configuration (RestoredIncomplete
	// with Installed) are counted in Entries, not here.
	Priors int
	// Throughput is how many throughput windows were installed beside them,
	// each to hold the demand floor hold-only once read under the arguments
	// the variant runs now. Zero for RestoredDiffers, whose windows hold
	// none.
	Throughput int
	// NoArgs is a RestoredIncomplete whose document recorded no arguments for
	// the variant: no window could be checked, and none was installed.
	NoArgs bool
	// Stored is how many k2 windows the document held for the variant, when
	// it was read: with none, nothing could have been kept, and "none recent
	// enough" would send an operator looking for an age or a hardware move
	// that did not happen.
	Stored int
	// BoundPriors is how many of the variant's lower bounds hold a previous
	// figure on the hardware it runs now when its entries are installed
	// (boundPriorsLocked): a figure restored with its bound
	// -- a window set aside before the export -- rather than from a window.
	// It prices the variant (P2-prior) as a restored window would.
	BoundPriors int
	// Entries is how many entries were installed, or rejected.
	Entries int
}

// priorKeyRuns says whether a k2 key can be read by the variant on the
// hardware its record names now. A key filed under another accelerator or GPU
// count is never looked up again, so installing its window to become a
// previous figure keeps nothing; an unknown on either side is not a
// difference.
func priorKeyRuns(key string, rec *capacity.Record) bool {
	f := strings.Split(key, "|")
	if len(f) < 5 {
		return false
	}
	if constants.IsAcceleratorResolved(f[3]) && constants.IsAcceleratorResolved(rec.AcceleratorName) && f[3] != rec.AcceleratorName {
		return false
	}
	if g, err := strconv.Atoi(f[4]); err == nil && g > 0 && rec.GpuCount > 0 && g != rec.GpuCount {
		return false
	}
	return true
}

// pendingVariant is one variant's restored entries awaiting the check.
type pendingVariant struct {
	expect     string // the fingerprint they were learned under
	expectArgs string // the same without the image; empty in an older document
	state      LearnedState
	// stored is how many k2 windows the document held for the variant,
	// counted when it was read: the sweep that runs before the variant's
	// first cycle prunes windows past retention from state, and a window
	// that aged out is not one the variant never had.
	stored int
}

// alive says whether any of the entries could still be installed: one within
// its horizon on the clock it ages on live. A variant noted present keeps its
// liveness fresh (notePendingPresent), so a parked one stays; a deleted one's
// entries all age out, and it goes -- however often the controller restarts.
// A clock in the future counts as dead, as it does at install: kept alive by
// it, an entry that can never be installed would be written back forever.
func (p *pendingVariant) alive(now time.Time, h capacity.Horizons) bool {
	s := p.state
	for _, w := range s.K2 {
		if within(w.LastUsed, now, h.BucketRetention) {
			return true
		}
	}
	for _, w := range s.Throughput {
		if within(w.LastUsed, now, h.BucketRetention) {
			return true
		}
	}
	for _, seen := range s.VariantSeenAt {
		if within(seen, now, h.VariantTimeout) {
			return true
		}
	}
	for _, m := range s.Accelerators {
		if within(m.LastUsed, now, h.BucketRetention) {
			return true
		}
	}
	return false
}

// pruneWindows drops the k2 and throughput windows past the bucket retention
// from a variant kept pending by its liveness: install would drop them, and
// written back they only take room from the size bound.
func (p *pendingVariant) pruneWindows(now time.Time, h capacity.Horizons) {
	maps.DeleteFunc(p.state.K2, func(_ string, w capacity.RollingAverageSnapshot) bool {
		return !within(w.LastUsed, now, h.BucketRetention)
	})
	maps.DeleteFunc(p.state.Throughput, func(_ string, w ThroughputSnapshot) bool {
		return !within(w.LastUsed, now, h.BucketRetention)
	})
	maps.DeleteFunc(p.state.K2Args, func(k, _ string) bool { _, ok := p.state.K2[k]; return !ok })
	maps.DeleteFunc(p.state.ThroughputArgs, func(k, _ string) bool { _, ok := p.state.Throughput[k]; return !ok })
}

// within says whether clock t, as of ref, is inside horizon and not further
// ahead of ref than maxClockSkew.
func within(t, ref time.Time, horizon time.Duration) bool {
	return !t.After(ref.Add(maxClockSkew)) && ref.Sub(t) <= horizon
}

func (p *pendingVariant) size() int {
	s := p.state
	return len(s.K2) + len(s.Throughput) + len(s.VariantSeenAt) + len(s.Accelerators)
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

// exportLearnedState copies what the analyzer has learned, with the engine
// configuration each of its variants runs now. Safe to call concurrently with
// analysis.
func (a *SaturationAnalyzer) exportLearnedState() LearnedState {
	a.mu.Lock()
	s := LearnedState{
		K2:               make(map[string]capacity.RollingAverageSnapshot, len(a.computeCapacityHistory)),
		Throughput:       make(map[string]ThroughputSnapshot, len(a.saturatedThroughput)),
		ITLBaseline:      a.itlTracker.Baselines(),
		ITLBaselineStash: maps.Clone(a.itlBaselineStash),
		StartSeconds:     make(map[string]float64, len(a.startSeconds)),
		VariantSeenAt:    make(map[string]time.Time, len(a.variantSeenAt)),
		Accelerators:     make(map[string]AcceleratorSnapshot, len(a.lastAccelerator)),
		ITLLines:         make(map[string]LineSnapshot, len(a.itlLines)),
		K2BorrowedSince:  map[string]time.Time{},
		LowerBounds:      make(map[string]LowerBoundSnapshot, len(a.lowerBounds)),
		K2Args:           maps.Clone(a.k2WindowArgs),
		ThroughputArgs:   maps.Clone(a.throughputWindowArgs),
		K2Stash:          make(map[string]capacity.RollingAverageSnapshot, len(a.k2Stash)),
		ThroughputStash:  make(map[string]ThroughputSnapshot, len(a.throughputStash)),
		Forgotten:        make(map[string]ForgetMarkSnapshot, len(a.forgotten)),
		Config:           map[string]string{},
		Args:             map[string]string{},
		PriorK2:          map[string]PriorK2Snapshot{},
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
	for k, ra := range a.k2Stash {
		s.K2Stash[k] = ra.Snapshot()
	}
	for k, st := range a.throughputStash {
		s.ThroughputStash[k] = ThroughputSnapshot{
			RollingAverageSnapshot: st.window.Snapshot(),
			SampledAt:              st.sampledAt,
			LastRead:               st.lastRead,
			HeldSeconds:            st.heldFor.Seconds(),
		}
	}
	for k, sl := range a.itlTracker.Slopes() {
		if s.ITLSlopes == nil {
			s.ITLSlopes = map[string]SlopeSnapshot{}
		}
		s.ITLSlopes[k] = SlopeSnapshot{A: sl.A, FittedAt: sl.FittedAt, Args: sl.Args}
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
	for k, lb := range a.lowerBounds {
		s.LowerBounds[k] = lb.snapshot()
		if lb.armed && !lb.own {
			s.K2BorrowedSince[k] = lb.since
		}
	}
	for k, m := range a.forgotten {
		s.Forgotten[k] = ForgetMarkSnapshot{Token: m.token, Seen: m.seen}
	}
	a.mu.Unlock()

	// The configuration each variant runs, read outside a.mu: the store has
	// its own lock. A variant with no parsed configuration gets no entry, and
	// its learned state will not be restored -- it could not be checked. One
	// whose configuration was not read completely (flags from envFrom, say)
	// is exported under it, with its windows' tag behind incompletePrefix:
	// it cannot match a configuration, but its windows are restored under
	// that tag, and the first read under the same incomplete configuration
	// takes them back as its own (window_args.go). Left out, such a variant
	// lost its own k2 on every restart.
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
	for v, m := range models {
		ns, name, _ := strings.Cut(v, "/")
		if rec := a.capacityStore.Get(ns, m, name); rec != nil && rec.EngineParams != nil {
			s.Config[v] = rec.EngineParams.Fingerprint()
			s.Args[v] = windowArgs(rec.EngineParams)
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
		if p.expectArgs != "" {
			s.Args[v] = p.expectArgs
		} else {
			delete(s.Args, v)
		}
		mergeAbsent(s.K2, p.state.K2)
		mergeAbsent(s.Throughput, p.state.Throughput)
		mergeAbsent(s.VariantSeenAt, p.state.VariantSeenAt)
		mergeAbsent(s.ITLBaseline, p.state.ITLBaseline)
		if len(p.state.ITLBaselineStash) > 0 {
			if s.ITLBaselineStash == nil {
				s.ITLBaselineStash = map[string]float64{}
			}
			mergeAbsent(s.ITLBaselineStash, p.state.ITLBaselineStash)
		}
		if len(p.state.ITLSlopes) > 0 {
			if s.ITLSlopes == nil {
				s.ITLSlopes = map[string]SlopeSnapshot{}
			}
			mergeAbsent(s.ITLSlopes, p.state.ITLSlopes)
		}
		mergeAbsent(s.StartSeconds, p.state.StartSeconds)
		mergeAbsent(s.K2Args, p.state.K2Args)
		mergeAbsent(s.ThroughputArgs, p.state.ThroughputArgs)
		mergeAbsent(s.Accelerators, p.state.Accelerators)
	}
	// A k2 window set aside here is one no read has moved yet, so the
	// previous figure the read would have made of it is made here, by the
	// same rules (notePreviousFigureLocked): restored into the stash alone, a
	// window learned before a rollout the controller saw would hold the
	// variant's figure for a rollback and nothing for the arguments it runs
	// now.
	//
	// From the document alone, so an export a minute later writes the same
	// bytes, and on the window's own clocks, so it ages as the window would
	// have: it became a figure when its window was last written (PriorSince,
	// for its age) and was last used when the window was last read
	// (PriorUsed, which it ages on). Stamped with now(), the document changed
	// on every export and the store rewrote its ConfigMap every minute while
	// any such window existed; stamped from the last WRITE alone, a restore
	// dropped the figure of every window a coping fleet had kept alive by
	// reading it -- the common case, and a k2 raise caused by the restart. A
	// running bound is exported unarmed with its start cleared, where the
	// live path restarts its clock: the arguments changed, and the first
	// serving cycle after a restore arms it afresh. Not when the window is
	// the one the bound already ran its course on (a rollback and re-roll),
	// which the live path does not restart either (notePreviousFigureLocked).
	for k, tag := range setAsideByArgs(s.K2, s.K2Args, s.K2Stash, s.Args) {
		w := s.K2Stash[stashKey(k, tag)]
		ra, ok := capacity.RestoreRollingAverage(w, capacity.RollingAverageWindowSize)
		if !ok || ra.Len() == 0 || w.LastWritten.IsZero() {
			continue
		}
		v := int64(ra.Average())
		if v <= 0 {
			continue
		}
		lb := s.LowerBounds[k]
		lb.Prior, lb.PriorSince, lb.PriorAgeSeconds, lb.PriorUsed = v, w.LastWritten, 0, w.LastUsed
		if lb.PriorUsed.Before(w.LastWritten) {
			lb.PriorUsed = w.LastWritten
		}
		same := lb.Own && sameFigure(lb.FigureWritten, w.LastWritten)
		if lb.Armed && !same {
			lb.Armed, lb.Since, lb.Tick = false, time.Time{}, time.Time{}
		}
		lb.FigureWritten = w.LastWritten
		// The legacy clock goes with it: a build that reads only
		// K2BorrowedSince and PriorK2 (main, #151) would otherwise run the new
		// previous figure on the old borrow's clock and relax it early.
		delete(s.K2BorrowedSince, k)
		if lb.Seen.Before(lb.PriorUsed) {
			lb.Seen = lb.PriorUsed
		}
		s.LowerBounds[k] = lb
	}
	setAsideByArgs(s.Throughput, s.ThroughputArgs, s.ThroughputStash, s.Args)
	// A window under a legacy tag was restored from a document that recorded
	// no arguments, for its k2 to become a previous figure on its first read,
	// which the loop above has just made of it. It is not kept for a rollback
	// that cannot come, as the live path does not keep it
	// (k2WindowUnderLocked): put aside here, a legacy throughput window came
	// back after the next restart as a hold-only mu of a configuration
	// nothing runs.
	isLegacy := func(k string) bool { return strings.Contains(k, "#"+legacyArgsPrefix) }
	maps.DeleteFunc(s.K2Stash, func(k string, _ capacity.RollingAverageSnapshot) bool { return isLegacy(k) })
	maps.DeleteFunc(s.ThroughputStash, func(k string, _ ThroughputSnapshot) bool { return isLegacy(k) })
	for k, lb := range s.LowerBounds {
		if lb.Prior <= 0 {
			continue
		}
		at := lb.PriorUsed
		if at.IsZero() {
			at = lb.PriorSince
		}
		s.PriorK2[k] = PriorK2Snapshot{K2: lb.Prior, InstalledAt: at}
	}
	return s
}

// setAsideByArgs sorts each exported window by the arguments it was written
// under against the ones its variant is exported under (args): the same
// arguments leave it among the windows with no tag of its own -- it is read
// as written under them, and a tag per window repeated the whole key -- and
// other arguments move it to the stash under them. A window whose variant
// has no arguments exported, or which has no tag, is left as it is. It
// returns the windows it moved, with the arguments each was written under.
func setAsideByArgs[V any](windows map[string]V, tags map[string]string, stash map[string]V, args map[string]string) map[string]string {
	moved := map[string]string{}
	for k, w := range windows {
		v, _, ok := variantOfKey(k)
		want, known := args[v]
		tag, tagged := tags[k]
		switch {
		case !ok || !known || !tagged || tag == "":
		case tag == want:
			delete(tags, k)
		default:
			if _, have := stash[stashKey(k, tag)]; !have {
				stash[stashKey(k, tag)] = w
			}
			delete(windows, k)
			delete(tags, k)
			moved[k] = tag
		}
	}
	return moved
}

// dropVariant removes one variant's per-variant entries from s.
func dropVariant(s *LearnedState, v string) {
	of := func(k string) bool { got, _, ok := variantOfKey(k); return ok && got == v }
	maps.DeleteFunc(s.K2, func(k string, _ capacity.RollingAverageSnapshot) bool { return of(k) })
	maps.DeleteFunc(s.Throughput, func(k string, _ ThroughputSnapshot) bool { return of(k) })
	maps.DeleteFunc(s.VariantSeenAt, func(k string, _ time.Time) bool { return of(k) })
	maps.DeleteFunc(s.ITLBaseline, func(k string, _ float64) bool { return of(k) })
	maps.DeleteFunc(s.ITLBaselineStash, func(k string, _ float64) bool { return of(k) })
	maps.DeleteFunc(s.ITLSlopes, func(k string, _ SlopeSnapshot) bool { return of(k) })
	maps.DeleteFunc(s.StartSeconds, func(k string, _ float64) bool { return of(k) })
	maps.DeleteFunc(s.K2Args, func(k, _ string) bool { return of(k) })
	maps.DeleteFunc(s.ThroughputArgs, func(k, _ string) bool { return of(k) })
	// Lower bounds stay: they are restored without a configuration to check
	// them against (RestoreLearnedState).
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

// restoreLearnedState takes a stored state in. The shared ITL lines, whose key
// IS the configuration, are installed now; per-variant entries -- the
// accelerator memo among them -- are held until installRestored checks their
// variant's configuration. Nothing already learned in this process is
// overwritten.
func (a *SaturationAnalyzer) restoreLearnedState(s LearnedState) RestoreReport {
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
			p = &pendingVariant{expect: expect, expectArgs: s.Args[v]}
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
				p.state.K2Args = map[string]string{}
			}
			p.state.K2[k] = w
			p.stored++
			if args, ok := s.K2Args[k]; ok {
				p.state.K2Args[k] = args
			}
		}
	}
	for k, w := range s.Throughput {
		if p := pending(k); p != nil {
			if p.state.Throughput == nil {
				p.state.Throughput = map[string]ThroughputSnapshot{}
				p.state.ThroughputArgs = map[string]string{}
			}
			p.state.Throughput[k] = w
			if args, ok := s.ThroughputArgs[k]; ok {
				p.state.ThroughputArgs[k] = args
			}
		}
	}
	// A put-aside baseline waits with its variant's other entries: installed
	// now, the sweep before the variant's first cycle took it as stale.
	for k, b := range s.ITLBaselineStash {
		if p := pending(k); p != nil {
			if p.state.ITLBaselineStash == nil {
				p.state.ITLBaselineStash = map[string]float64{}
			}
			p.state.ITLBaselineStash[k] = b
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
		if sl, ok := s.ITLSlopes[k]; ok {
			if p.state.ITLSlopes == nil {
				p.state.ITLSlopes = map[string]SlopeSnapshot{}
			}
			p.state.ITLSlopes[k] = sl
		}
		if t, ok := s.StartSeconds[k]; ok {
			p.state.StartSeconds[k] = t
		}
	}
	// Lower bounds are installed now, with no configuration check: a bound
	// can only make a variant more conservative, for a bounded time, and a
	// borrower whose configuration was not read completely still borrows, so
	// one held back for the check was lost on every restart. A changed
	// configuration is caught when the bound is next read
	// (lowerBound.rearms). A clock from a document written before
	// LowerBounds, a start alone, is installed as an armed bound with no
	// record of what it ran on, under the rule it was written under: kept a
	// hold, a relaxation and a retention from its start.
	borrowLife := BorrowedK2Hold + BorrowedK2Relax + bucketRetention
	count := func(r boundInstall) {
		switch r {
		case boundInstalled:
			rep.BorrowClocks++
		case boundUnusable:
			rep.Unusable++
		case boundAged:
			rep.Aged++
		}
	}
	// A forgotten variant's RUNNING bounds are installed spent, as the forget
	// would have left them had the restore come first (spendLowerBoundsLocked):
	// skipped, the bound was simply absent, and the variant's next serving
	// cycle armed the sibling's figure afresh at full strength -- the very
	// borrow the forget had ended. What it held of its own goes.
	for k, lb := range s.LowerBounds {
		if forgottenKey(k) {
			rep.Forgotten++
			if lb.Armed {
				a.installLowerBoundLocked(k, spentLowerBound(lb, now), now)
			}
			continue
		}
		count(a.installLowerBoundLocked(k, lb, now))
	}
	// A previous figure from a PR #151 document, where LowerBounds has none
	// for the key: a bound holding only that figure, aged from when it was
	// installed, a bucket retention at most, and last used then -- not at
	// the restore, which handed a figure days old a fresh retention. None
	// where the document holds a window for the key: a #151 build priced a
	// key with a window of its own from the window, never from the prior,
	// so installed here it priced the next rollout from a figure the
	// variant had since replaced.
	for k, p := range s.PriorK2 {
		if _, ok := s.LowerBounds[k]; ok {
			continue
		}
		if w, ok := s.K2[k]; ok && len(w.Values) > 0 {
			continue
		}
		since, inTime := notFuture(p.InstalledAt, now)
		switch {
		case forgottenKey(k):
			rep.Forgotten++
		case p.K2 <= 0 || since.IsZero() || !inTime:
			rep.Unusable++
		case now.Sub(since) > bucketRetention:
			rep.Aged++
		default:
			// The clock the #151 document kept under the same key ran the
			// prior's hold and relaxation; started afresh, it re-armed the
			// figure at full strength on every restart.
			lb := LowerBoundSnapshot{Prior: p.K2, PriorSince: since, PriorUsed: since, Seen: now}
			if started, ok := s.K2BorrowedSince[k]; ok {
				if started, inTime := notFuture(started, now); inTime && !started.IsZero() {
					lb.Armed, lb.Since = true, started
				}
			}
			count(a.installLowerBoundLocked(k, lb, now))
		}
	}
	for k, since := range s.K2BorrowedSince {
		if _, ok := s.LowerBounds[k]; ok {
			continue
		}
		since, inTime := notFuture(since, now)
		switch {
		case forgottenKey(k):
			rep.Forgotten++
			if inTime && !since.IsZero() {
				a.installLowerBoundLocked(k, spentLowerBound(LowerBoundSnapshot{Armed: true, Since: since}, now), now)
			}
		case since.IsZero() || !inTime:
			rep.Unusable++
		case now.Sub(since) > borrowLife:
			rep.Aged++
		default:
			count(a.installLowerBoundLocked(k, LowerBoundSnapshot{Armed: true, Since: since, Seen: now}, now))
		}
	}
	// Windows put aside under arguments their variant no longer ran are
	// installed now, with no configuration check: each names the arguments
	// it was written under, and only a variant running them takes it back
	// (window_args.go).
	for k, w := range s.K2Stash {
		w, inTime := windowClocks(w, now)
		_, live := a.k2Stash[k]
		switch {
		case live, strings.Contains(k, "#"+legacyArgsPrefix):
		case forgottenKey(k):
			rep.Forgotten++
		case !inTime:
			rep.Unusable++
		case now.Sub(w.LastUsed) > bucketRetention:
			rep.Aged++
		default:
			if ra, ok := capacity.RestoreRollingAverage(w, capacity.RollingAverageWindowSize); ok && ra.Len() > 0 {
				a.k2Stash[k] = ra
				rep.Stashed++
			} else {
				rep.Unusable++
			}
		}
	}
	for k, w := range s.ThroughputStash {
		snap, inTime := windowClocks(w.RollingAverageSnapshot, now)
		sampledAt, sampledInTime := notFuture(w.SampledAt, now)
		_, live := a.throughputStash[k]
		switch {
		case live, strings.Contains(k, "#"+legacyArgsPrefix):
		case forgottenKey(k):
			rep.Forgotten++
		case !inTime || !sampledInTime:
			rep.Unusable++
		case now.Sub(snap.LastUsed) > bucketRetention:
			rep.Aged++
		default:
			if ra, ok := capacity.RestoreRollingAverage(snap, capacity.RollingAverageWindowSize); ok && ra.Len() > 0 {
				st := stashedThroughput{window: ra, sampledAt: sampledAt}
				if finitePositive(w.LastRead) {
					st.lastRead = w.LastRead
				}
				if finitePositive(w.HeldSeconds) {
					st.heldFor = time.Duration(w.HeldSeconds * float64(time.Second))
				}
				a.throughputStash[k] = st
				rep.Stashed++
			} else {
				rep.Unusable++
			}
		}
	}
	for v, p := range a.pendingRestore {
		if !p.alive(now, a.horizons) {
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
	// fingerprint: see the package comment. Nor does it take a prior: a
	// figure is only known to come from OTHER arguments when both sides were
	// read.
	complete := rec.EngineParams.Complete()
	whole := complete && rec.EngineParams.Fingerprint() == p.expect
	args := complete && !whole && p.expectArgs != "" && rec.EngineParams.ArgsFingerprint() == p.expectArgs
	prior := complete && !whole && !args && p.expectArgs != ""
	// A window the document gives no arguments for was written under the
	// variant's whole recorded configuration; under a document from before
	// the arguments were recorded, under the configuration that just matched.
	windowsUnder := p.expectArgs
	if windowsUnder == "" && whole {
		windowsUnder = rec.EngineParams.ArgsFingerprint()
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pendingRestore, key)
	out := RestoredVariant{Namespace: namespace, Variant: variant, Entries: p.size(), Stored: p.stored,
		BoundPriors: a.boundPriorsLocked(namespace, modelID, variant, rec)}
	switch {
	case whole:
		out.Outcome, out.Installed = RestoredWhole, true
		out.Entries = a.installLocked(p.state, windowsUnder, true)
	case args:
		out.Outcome, out.Installed, out.ImageChanged = RestoredArgs, true, true
		out.Entries = a.installLocked(p.state, windowsUnder, false)
	case prior:
		out.Outcome = RestoredPrior
		out.Priors, out.Throughput = a.installPreviousK2Locked(p.state, windowsUnder, rec)
	case !complete:
		out.Outcome = RestoredIncomplete
		out.NoArgs = p.expectArgs == ""
		if !out.NoArgs {
			n, tp := a.installPreviousK2Locked(p.state, p.expectArgs, rec)
			// Under the same unread configuration the windows are the
			// variant's own (window_args.go), not previous figures.
			if p.expectArgs == windowArgs(rec.EngineParams) {
				out.Installed, out.Entries = n+tp > 0, n+tp
			} else {
				out.Priors, out.Throughput = n, tp
			}
		}
	default:
		// Its throughput windows are installed under the legacy tag too, and
		// dropped on their first read without being put aside
		// (throughputWindowUnderLocked): they hold no floor, and are not
		// reported as if they did.
		out.Outcome = RestoredDiffers
		out.Priors, _ = a.installPreviousK2Locked(p.state, legacyArgsPrefix+p.expect, rec)
	}
	a.installITLStashLocked(p, out.Installed)
	return out, true
}

// installPreviousK2Locked installs a variant's restored k2 and throughput
// windows when the arguments it runs differ from the ones it was exported
// under: each under the arguments it was written under, so its first read
// decides (window_args.go) -- a k2 window from other arguments becomes the
// variant's own lower-only, expiring figure, a throughput window from other
// arguments is put aside, and one written under the arguments the variant
// runs now (it had been rolled back to them) is its own. A window the variant
// already has one of, or filed under hardware it no longer runs on (rec), is
// left out. Returns how many k2 and throughput windows it installed. Callers
// hold a.mu.
func (a *SaturationAnalyzer) installPreviousK2Locked(s LearnedState, windowsUnder string, rec *capacity.Record) (k2, throughput int) {
	now := a.now()
	kept := 0
	for k, w := range s.K2 {
		w, inTime := windowClocks(w, now)
		if _, live := a.computeCapacityHistory[k]; live || !inTime || now.Sub(w.LastUsed) > a.horizons.BucketRetention ||
			!priorKeyRuns(k, rec) {
			continue
		}
		if ra, ok := capacity.RestoreRollingAverage(w, capacity.RollingAverageWindowSize); ok && ra.Len() > 0 {
			a.computeCapacityHistory[k] = ra
			a.tagRestoredLocked(a.k2WindowArgs, k, s.K2Args, windowsUnder)
			kept++
		}
	}
	// The accelerator memo, as installLocked installs it: on a fleet whose
	// pods intermittently report an unresolved accelerator, the windows
	// installed here are filed under the stable one, and without the memo a
	// cycle that read none looked them up under a key nothing holds.
	a.installAcceleratorsLocked(s, now, rec)
	for k, w := range s.Throughput {
		snap, inTime := windowClocks(w.RollingAverageSnapshot, now)
		sampledAt, sampledInTime := notFuture(w.SampledAt, now)
		if _, live := a.saturatedThroughput[k]; live || !inTime || !sampledInTime ||
			now.Sub(snap.LastUsed) > a.horizons.BucketRetention || !priorKeyRuns(k, rec) {
			continue
		}
		if ra, ok := capacity.RestoreRollingAverage(snap, capacity.RollingAverageWindowSize); ok && ra.Len() > 0 {
			a.saturatedThroughput[k] = ra
			a.tagRestoredLocked(a.throughputWindowArgs, k, s.ThroughputArgs, windowsUnder)
			if !sampledAt.IsZero() {
				a.throughputSampledAt[k] = sampledAt
			}
			if finitePositive(w.LastRead) {
				a.throughputLastRead[k] = w.LastRead
			}
			throughput++
		}
	}
	return kept, throughput
}

// installAcceleratorsLocked installs a variant's restored accelerator memo
// entries still within the bucket retention and returns how many. Callers
// hold a.mu.
//
// rec, when not nil, is the record of a variant whose arguments changed: a
// memo naming another resolved accelerator than the one it runs now is left
// out, as its windows are (priorKeyRuns).
func (a *SaturationAnalyzer) installAcceleratorsLocked(s LearnedState, now time.Time, rec *capacity.Record) int {
	kept := 0
	for k, m := range s.Accelerators {
		lastUsed, inTime := notFuture(m.LastUsed, now)
		if _, live := a.lastAccelerator[k]; live || m.Name == "" || m.LastUsed.IsZero() || !inTime ||
			now.Sub(lastUsed) > a.horizons.BucketRetention {
			continue
		}
		if rec != nil && constants.IsAcceleratorResolved(rec.AcceleratorName) && m.Name != rec.AcceleratorName {
			continue
		}
		a.lastAccelerator[k] = acceleratorMemo{name: m.Name, lastUsed: lastUsed}
		kept++
	}
	return kept
}

// tagRestoredLocked records the arguments a restored window was written
// under: its own from the document, or windowsUnder for a window the document
// gives none for. Callers hold a.mu.
func (a *SaturationAnalyzer) tagRestoredLocked(tags map[string]string, k string, stored map[string]string, windowsUnder string) {
	switch args, ok := stored[k]; {
	case ok && args != "":
		tags[k] = args
	case windowsUnder != "":
		tags[k] = windowsUnder
	}
}

// installLocked applies the horizons to a checked variant's entries, keeps
// what is still within them and returns how many it kept. Callers hold a.mu.
// Everything ages on the analyzer's clock, as it does live. Each window is
// installed with the arguments it was written under (windowsUnder for one the
// document gives none for), so one learned before a rollout is moved out of
// the way on its first read. latency is false when the image changed: the ITL
// baseline, a latency figure, is then left out.
func (a *SaturationAnalyzer) installLocked(s LearnedState, windowsUnder string, latency bool) int {
	now := a.now()
	h := a.horizons
	kept := 0
	for k, w := range s.K2 {
		w, inTime := windowClocks(w, now)
		if _, live := a.computeCapacityHistory[k]; live || !inTime || now.Sub(w.LastUsed) > h.BucketRetention {
			continue
		}
		if ra, ok := capacity.RestoreRollingAverage(w, capacity.RollingAverageWindowSize); ok {
			a.computeCapacityHistory[k] = ra
			a.tagRestoredLocked(a.k2WindowArgs, k, s.K2Args, windowsUnder)
			kept++
		}
	}
	for k, w := range s.Throughput {
		snap, inTime := windowClocks(w.RollingAverageSnapshot, now)
		sampledAt, sampledInTime := notFuture(w.SampledAt, now)
		if _, live := a.saturatedThroughput[k]; live || !inTime || !sampledInTime || now.Sub(snap.LastUsed) > h.BucketRetention {
			continue
		}
		if ra, ok := capacity.RestoreRollingAverage(snap, capacity.RollingAverageWindowSize); ok {
			a.saturatedThroughput[k] = ra
			a.tagRestoredLocked(a.throughputWindowArgs, k, s.ThroughputArgs, windowsUnder)
			kept++
			if !sampledAt.IsZero() {
				a.throughputSampledAt[k] = sampledAt
			}
			if finitePositive(w.LastRead) {
				a.throughputLastRead[k] = w.LastRead
			}
		}
	}
	kept += a.installAcceleratorsLocked(s, now, nil)
	for k, seen := range s.VariantSeenAt {
		seen, inTime := notFuture(seen, now)
		if _, live := a.variantSeenAt[k]; live || seen.IsZero() || !inTime || now.Sub(seen) > h.VariantTimeout {
			continue
		}
		a.variantSeenAt[k] = seen
		kept++
		if b, ok := s.ITLBaseline[k]; ok && latency && finitePositive(b) {
			a.itlTracker.RestoreBaseline(k, b)
		}
		// The slope is a latency figure like the baseline, and installed on
		// the same terms. Its hold is then bounded as a live one is, from
		// when it was fitted and by the arguments it was fitted under
		// (itl.Admission); one fitted in the future or past the retention is
		// not installed at all.
		if sl, ok := s.ITLSlopes[k]; ok && latency && finitePositive(sl.A) {
			if fittedAt, inTime := notFuture(sl.FittedAt, now); inTime && !fittedAt.IsZero() &&
				now.Sub(fittedAt) <= h.BucketRetention {
				a.itlTracker.RestoreSlope(k, itl.Slope{A: sl.A, FittedAt: fittedAt, Args: sl.Args})
			}
		}
		if t, ok := s.StartSeconds[k]; ok && finitePositive(t) {
			if _, have := a.startSeconds[k]; !have {
				a.startSeconds[k] = t
			}
		}
	}
	return kept
}

// installITLStashLocked installs a variant's put-aside ITL baselines, which
// name the arguments they were learned under and need no configuration check.
// When the variant's own entries were not installed (its arguments differ, or
// its configuration could not be matched), its stored baseline is put aside
// too, under the arguments the document recorded, so a rollback to them gets
// it back: only a full fit rewrites a baseline, which a balanced fleet rarely
// spans. Callers hold a.mu.
func (a *SaturationAnalyzer) installITLStashLocked(p *pendingVariant, installed bool) {
	for k, b := range p.state.ITLBaselineStash {
		if _, live := a.itlBaselineStash[k]; !live && finitePositive(b) {
			a.itlBaselineStash[k] = b
		}
	}
	if installed || p.expectArgs == "" {
		return
	}
	for k, b := range p.state.ITLBaseline {
		sk := stashKey(k, p.expectArgs)
		if _, live := a.itlBaselineStash[sk]; !live && finitePositive(b) {
			a.itlBaselineStash[sk] = b
		}
	}
}

// boundPriorsLocked counts the variant's lower bounds holding a previous
// figure on the hardware it runs now (priorKeyRuns, rec). A bound filed under
// another accelerator or GPU count is never looked up; counting it made the
// restart line say a figure priced a variant that had moved off it. Callers
// hold a.mu.
func (a *SaturationAnalyzer) boundPriorsLocked(namespace, modelID, variant string, rec *capacity.Record) int {
	prefix := namespace + "|" + variant + "|" + modelID + "|"
	n := 0
	for k, lb := range a.lowerBounds {
		if lb.prior > 0 && strings.HasPrefix(k, prefix) && priorKeyRuns(k, rec) {
			n++
		}
	}
	return n
}

// spentLowerBound is a stored running bound as a forget leaves it: armed, its
// hold and relaxation run, keeping what it last ran on, its own previous
// figure dropped (spendLowerBoundsLocked).
func spentLowerBound(s LowerBoundSnapshot, now time.Time) LowerBoundSnapshot {
	if spentAt := now.Add(-(BorrowedK2Hold + BorrowedK2Relax)); s.Since.IsZero() || s.Since.After(spentAt) {
		s.Since = spentAt
	}
	s.Tick, s.Seen, s.RearmPending = now, now, false
	s.Prior, s.PriorSince, s.PriorAgeSeconds, s.PriorUsed = 0, time.Time{}, 0, time.Time{}
	return s
}

// boundInstall is what installLowerBoundLocked did with one lower bound.
type boundInstall int

const (
	boundInstalled boundInstall = iota
	// boundLive: the process already keeps one under the key; not counted.
	boundLive
	// boundUnusable: a clock in it is in the future or missing, or it holds
	// nothing -- no running clock and no previous figure.
	boundUnusable
	// boundAged: its variant has not been seen within VariantTimeout.
	boundAged
)

// installLowerBoundLocked installs one lower bound, unless the process already
// keeps one under the key, a clock in it is unusable, or its variant has not
// been seen within VariantTimeout, and says which. The start is taken as
// stored: the tick it is paired with carries the time the controller was down
// as a gap, which the next serving cycle reads as paused time. A bound from an
// older document has no tick; its first serving cycle then counts from that
// cycle. Callers hold a.mu.
func (a *SaturationAnalyzer) installLowerBoundLocked(k string, s LowerBoundSnapshot, now time.Time) boundInstall {
	since, sinceInTime := notFuture(s.Since, now)
	seen, seenInTime := notFuture(s.Seen, now)
	tick, tickInTime := notFuture(s.Tick, now)
	priorSince, priorInTime := notFuture(s.PriorSince, now)
	priorUsed, usedInTime := notFuture(s.PriorUsed, now)
	if priorUsed.IsZero() {
		priorUsed = priorSince
	}
	// A bound holding only a previous figure ages as the figure does: on its
	// last use, a bucket retention. One with a running clock goes with its
	// variant.
	aged := now.Sub(seen) > a.horizons.VariantTimeout
	if !s.Armed && s.Prior > 0 {
		aged = now.Sub(priorUsed) > a.horizons.BucketRetention
	}
	switch {
	case a.lowerBounds[k] != nil:
		return boundLive
	case !sinceInTime || !seenInTime || !tickInTime || !priorInTime || !usedInTime || seen.IsZero():
		return boundUnusable
	case aged:
		return boundAged
	}
	lb := &lowerBound{
		armed: s.Armed && !since.IsZero(), since: since, tick: tick, seen: seen,
		donor: s.Donor, own: s.Own, donorK2: s.DonorK2, args: s.Args, argsVersion: s.ArgsVersion,
		offered: max(s.Offered, 0), rearmPending: s.RearmPending && s.Armed, figureWritten: s.FigureWritten,
	}
	if s.Prior > 0 && !priorSince.IsZero() {
		lb.prior, lb.priorSince, lb.priorUsed = s.Prior, priorSince, priorUsed
		// An age the document cannot give (NaN, negative, infinite) is
		// taken as none: it only reaches a metric.
		if age := s.PriorAgeSeconds; age > 0 && !math.IsInf(age, 1) {
			lb.priorAge = time.Duration(age * float64(time.Second))
		}
	}
	if lb.armed && lb.tick.IsZero() {
		lb.tick = now
	}
	if !lb.armed && lb.prior <= 0 {
		return boundUnusable
	}
	a.lowerBounds[k] = lb
	return boundInstalled
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
			m.LastUsed = now
			p.state.Accelerators[namespace+"/"+v] = m
		}
	}
}

// takeRestoreOutcomes returns how many variants' restored state installRestored
// decided since it was last called, by outcome, and resets the counts.
func (a *SaturationAnalyzer) takeRestoreOutcomes() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.restoreOutcomes))
	for o, n := range a.restoreOutcomes {
		out[string(o)] = n
	}
	clear(a.restoreOutcomes)
	return out
}

// forgetLearnedState drops everything learned and everything pending, for an
// operator who deleted the stored snapshot: written back from memory, the
// deletion would undo nothing. The working memories a flush is not about
// (learnedMap.flushed) stay: the decode-saturation memory dropped mid-episode
// let a queued prefill replica record decode's back-pressure as its own k2.
func (a *SaturationAnalyzer) forgetLearnedState() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range a.learnedMapsLocked() {
		if m.flushed {
			m.clear()
		}
	}
	// The baselines may have been restored; the ITL windows are this
	// process's own readings, a working memory like the ones above.
	a.itlTracker.ForgetBaselines()
	// The held slopes too: they are exported, so one kept here would be
	// written back and undo the deletion.
	a.itlTracker.ForgetSlopes()
}
