package learnreplay

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/enginerec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The engine-level layer (issue #345, step 0): a recording also carries the
// composite's inputs and what it built (internal/engines/enginerec). The
// engine replays those itself, with its own functions, and this package
// cannot import it -- the engine's tests import this package -- so the
// engine's replay is handed in: Options.Engine, or the one RegisterEngine
// set. Without one, engine events are skipped and counted as ignored, as
// they were before the engine recorded any.

// EngineReplayer replays the engine events of one recording
// (steadystate.LearnerReplay).
type EngineReplayer interface {
	// Owns reports whether the replayer applies events of kind.
	Owns(kind string) bool
	// Apply replays one event at its clock and records what it built with
	// rec. replayed is what the replay has reproduced before the event.
	Apply(ctx context.Context, ev learnrec.RawEvent, replayed enginerec.Replayed, rec *learnrec.Recorder) error
	// PathCounts is how often each engine path ran in this replay.
	PathCounts() map[string]int64
}

var (
	engineMu      sync.Mutex
	engineFactory func() EngineReplayer
)

// RegisterEngine sets how every later replay whose Options name no engine
// gets one: a fresh replayer per replay. cmd/learnergolden and the tests
// register the engine's (steadystate.NewLearnerReplay). nil unregisters.
func RegisterEngine(f func() EngineReplayer) {
	engineMu.Lock()
	defer engineMu.Unlock()
	engineFactory = f
}

// registeredEngine is a fresh replayer from RegisterEngine's factory, nil
// when none is registered.
func registeredEngine() EngineReplayer {
	engineMu.Lock()
	f := engineFactory
	engineMu.Unlock()
	if f == nil {
		return nil
	}
	return f()
}

// enginePathNames is the engine's per-path counters, sorted; nil with no
// engine registered.
func enginePathNames() []string {
	e := registeredEngine()
	if e == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(e.PathCounts()))
}

// resultKey keys the replay's last result of an analyzer for a model.
func resultKey(analyzer, namespace, modelID string) string {
	return analyzer + "\x00" + namespace + "\x00" + modelID
}

// notePolicy keeps the policy an Analyze received, for the engine's events
// after it: the replay derives from it what the engine derives from the
// cycle's policy.
func (r *replayer) notePolicy(in domain.AnalyzerInput) {
	if r.policies == nil {
		r.policies = map[string]*config.ScalingPolicy{}
	}
	p, _ := in.Config.(*config.ScalingPolicy)
	r.policies[resultKey("", in.Namespace, in.ModelID)] = p
}

// Policy implements enginerec.Replayed: the policy the model's last
// saturation Analyze received.
func (r *replayer) Policy(namespace, modelID string) (*config.ScalingPolicy, bool) {
	p := r.policies[resultKey("", namespace, modelID)]
	return p, p != nil
}

// noteResult keeps an Analyze's result for the engine's events after it; a
// failed Analyze leaves none.
func (r *replayer) noteResult(analyzer, namespace, modelID string, res *domain.AnalyzerResult, err error) {
	if r.results == nil {
		r.results = map[string]*domain.AnalyzerResult{}
	}
	if err != nil {
		res = nil
	}
	r.results[resultKey(analyzer, namespace, modelID)] = res
}

// Result implements enginerec.Replayed: the replay's last result of an
// analyzer for a model.
func (r *replayer) Result(analyzer, namespace, modelID string) (*domain.AnalyzerResult, bool) {
	res := r.results[resultKey(analyzer, namespace, modelID)]
	return res, res != nil
}
