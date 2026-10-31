package learnreplay

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/steadystate"
)

// The engine's replay drives the engine events of the committed goldens, as
// it does in cmd/learnergolden: this package cannot import the engine, whose
// tests import this package, so its tests register it.
func init() {
	RegisterEngine(func() EngineReplayer { return steadystate.NewLearnerReplay() })
}
