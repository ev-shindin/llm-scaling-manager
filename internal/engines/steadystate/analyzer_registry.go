package steadystate

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// analyzerEntry binds a registered analyzer to its name. The registry keeps
// the built-in entries in registration order so runAnalyzers iterates the
// analyzers deterministically.
type analyzerEntry struct {
	name     string
	analyzer domain.Analyzer
}

// analyzerRegistry is the engine's one analyzer registry: the built-in
// analyzers, registered before the optimize loop starts, and the external
// (config-driven) ones, reconciled from the catalog every cycle. Every read
// takes the read lock, so the external half may change while the loop runs.
//
// Saturation is registered here like any other analyzer. What the engine
// reaches of it beyond Analyze is resolved once, when it is registered, into
// typed capabilities (saturationCapabilities); nothing else asserts on the
// analyzer's type.
//
// A nil registry reads as empty and removes nothing: an Engine a test builds
// by hand may have none. The engine creates one before it registers or
// starts (Engine.ensureAnalyzers).
type analyzerRegistry struct {
	mu sync.RWMutex
	// builtin is the built-in analyzers in registration order. Appended to
	// only before start; read-only after.
	builtin []analyzerEntry
	// external is the config-driven analyzers by name, changed at any time
	// by upsert and remove.
	external map[string]domain.Analyzer
	// started is set by start. A built-in registered after it is an error,
	// so "register before Start" is enforced rather than documented.
	started bool
	// saturation is the capabilities of the analyzer registered under
	// domain.SaturationAnalyzerName; the zero value when none is.
	saturation saturationCapabilities
}

// newAnalyzerRegistry is a registry holding builtin, in order. A duplicate
// name panics: the built-ins are the engine's own, so a duplicate is a
// programming error.
func newAnalyzerRegistry(builtin ...analyzerEntry) *analyzerRegistry {
	r := &analyzerRegistry{external: make(map[string]domain.Analyzer)}
	for _, en := range builtin {
		if err := r.register(en.name, en.analyzer); err != nil {
			panic(err)
		}
	}
	return r
}

// register adds a built-in analyzer, in registration order. It fails after
// start and on a name already registered.
func (r *analyzerRegistry) register(name string, a domain.Analyzer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return errors.New("RegisterAnalyzer: called after StartOptimizeLoop")
	}
	for i := range r.builtin {
		if r.builtin[i].name == name {
			return fmt.Errorf("RegisterAnalyzer: duplicate analyzer name %q", name)
		}
	}
	r.builtin = append(r.builtin, analyzerEntry{name: name, analyzer: a})
	if name == domain.SaturationAnalyzerName {
		r.saturation = saturationCapabilitiesOf(a)
	}
	return nil
}

// start freezes the built-in analyzers: register fails from now on.
func (r *analyzerRegistry) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = true
}

// upsert registers or replaces an external analyzer by name. A name that is
// a built-in's is still stored but never runs: the built-in wins in
// runEntries.
func (r *analyzerRegistry) upsert(name string, a domain.Analyzer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.external == nil {
		r.external = make(map[string]domain.Analyzer)
	}
	r.external[name] = a
}

// remove retires an external analyzer. Idempotent, and a no-op on a nil
// registry.
func (r *analyzerRegistry) remove(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.external, name)
}

// builtins is the built-in analyzers in registration order.
func (r *analyzerRegistry) builtins() []analyzerEntry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]analyzerEntry(nil), r.builtin...)
}

// isBuiltin is whether name is a built-in analyzer's.
func (r *analyzerRegistry) isBuiltin(name string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.builtinLocked(name)
}

func (r *analyzerRegistry) builtinLocked(name string) bool {
	for i := range r.builtin {
		if r.builtin[i].name == name {
			return true
		}
	}
	return false
}

// externalNames is the names of the external analyzers registered.
func (r *analyzerRegistry) externalNames() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.external))
	for name := range r.external {
		names = append(names, name)
	}
	return names
}

// externalAnalyzer is the external analyzer registered under name, if any.
func (r *analyzerRegistry) externalAnalyzer(name string) (domain.Analyzer, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.external[name]
	return a, ok
}

// runEntries is the analyzers to run this cycle: the built-ins in
// registration order, then the external analyzers sorted by name. An
// external analyzer whose name is a built-in's is skipped: the built-in wins
// name resolution.
func (r *analyzerRegistry) runEntries() []analyzerEntry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.external))
	for name := range r.external {
		if r.builtinLocked(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]analyzerEntry, 0, len(r.builtin)+len(names))
	entries = append(entries, r.builtin...)
	for _, name := range names {
		entries = append(entries, analyzerEntry{name: name, analyzer: r.external[name]})
	}
	return entries
}

// saturationCaps is the saturation analyzer's capabilities; the zero value
// when no saturation analyzer is registered.
func (r *analyzerRegistry) saturationCaps() saturationCapabilities {
	if r == nil {
		return saturationCapabilities{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.saturation
}

// saturationCapabilities is the saturation analyzer and what the engine
// reaches of it beyond Analyze, each as a typed interface, resolved once at
// registration (saturationCapabilitiesOf). A nil field is a capability the
// registered analyzer lacks. Production registers
// *saturation.SaturationAnalyzer, which has every one (pinned at compile time
// below); a test may register an analyzer without some, and each caller says
// what it does then.
type saturationCapabilities struct {
	// analyzer is the analyzer itself, run first every cycle (runAnalyzers).
	analyzer domain.Analyzer
	// stabiliser names the accelerator a variant's learned state is keyed
	// under (the input builder publishes and files under it).
	stabiliser acceleratorStabiliser
	// evictor sweeps the analyzer's stale learned state
	// (evictStaleLearnedState).
	evictor staleHistoryEvictor
	// forgetter forgets one variant on request (forgetOnRequest).
	forgetter variantForgetter
	// noter stamps the variants still present (notePresentVariants).
	noter presenceNoter
	// learned exports and restores the learned-state snapshot
	// (learned_state_store.go).
	learned learnedStater
	// recorded takes the learner recorder and states the horizons the
	// recording starts with (learner_recording.go).
	recorded saturationRecorded
}

// saturationCapabilitiesOf resolves a's capabilities. The one place the
// engine asserts on the saturation analyzer's type.
func saturationCapabilitiesOf(a domain.Analyzer) saturationCapabilities {
	c := saturationCapabilities{analyzer: a}
	c.stabiliser, _ = a.(acceleratorStabiliser)
	c.evictor, _ = a.(staleHistoryEvictor)
	c.forgetter, _ = a.(variantForgetter)
	c.noter, _ = a.(presenceNoter)
	c.learned, _ = a.(learnedStater)
	c.recorded, _ = a.(saturationRecorded)
	return c
}

// acceleratorStabiliser is the part of the saturation analyzer the
// engine-config metric needs: the accelerator a variant's learned state is
// actually keyed under.
//
// Without it the metric published the raw reading while the key used the
// stabilised one, which on a heterogeneous fleet made the label disagree with
// the key it exists to explain.
type acceleratorStabiliser interface {
	StableAcceleratorFor(namespace, variantName, accelerator string) string
}

// staleHistoryEvictor is the part of the saturation analyzer the per-cycle
// sweep of learned state needs.
type staleHistoryEvictor interface {
	EvictStaleHistory() saturation.Evicted
}

// variantForgetter is the part of the saturation analyzer forgetOnRequest
// needs.
type variantForgetter interface {
	ForgottenToken(namespace, variant string) string
	ForgetVariant(namespace, modelID, variant, token string) int
}

// presenceNoter is the part of the saturation analyzer notePresentVariants
// needs. The slo analyzer has it too (sloStateKeeper).
type presenceNoter interface {
	NoteVariantsPresent(namespace, modelID string, variantNames []string)
}

// saturationRecorded is the part of the saturation analyzer the learner
// recording needs: it takes the recorder, and its horizons are the
// recording's first line.
type saturationRecorded interface {
	WithRecorder(rec *learnrec.Recorder) *saturation.SaturationAnalyzer
	Horizons() capacity.Horizons
	BorrowTickGap() time.Duration
}

// Every capability is pinned at compile time on the analyzer production
// registers. A capability resolved at registration that nothing pins is a
// fallback waiting to become permanent: measured, renaming
// StableAcceleratorFor on the concrete type and updating that package's own
// tests left `go build`, `go vet`, saturation 339/339 and the whole 157-spec
// steadystate suite green, while the engine silently went back to publishing
// the raw accelerator for good; and the eviction entry point was once written
// with no caller anywhere on the reconcile path, so the analyzer kept one
// entry per variant it had ever seen for the lifetime of the process. These
// lines turn either into a build error that names the missing method.
var (
	_ acceleratorStabiliser = (*saturation.SaturationAnalyzer)(nil)
	_ staleHistoryEvictor   = (*saturation.SaturationAnalyzer)(nil)
	_ variantForgetter      = (*saturation.SaturationAnalyzer)(nil)
	_ presenceNoter         = (*saturation.SaturationAnalyzer)(nil)
	_ learnedStater         = (*saturation.SaturationAnalyzer)(nil)
	_ saturationRecorded    = (*saturation.SaturationAnalyzer)(nil)
)
