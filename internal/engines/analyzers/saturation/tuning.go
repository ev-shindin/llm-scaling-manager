package saturation

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// tuning is the policy's analyzerTuning with every unset figure at its
// default, and the idle test it shares with the load-imbalance diagnostic
// (loadImbalance.idleKvUsage). One value per cycle, read from the model's
// resolved policy, so a per-model override reaches every judgement it names
// and nothing is held in a package variable.
type tuning struct {
	config.AnalyzerTuning
	// idleKvUsage is loadImbalance.idleKvUsage, never above kvCacheThreshold
	// (config.ScalingPolicy.IdleKvUsage): the busiest-rank KV utilization
	// below which a Ready replica with nothing waiting reads idle on one row
	// (replicaIdle).
	idleKvUsage float64
}

// tuningOf is cfg's tuning, defaults filled in. A nil cfg is all defaults.
func tuningOf(cfg *config.ScalingPolicy) tuning {
	var p config.ScalingPolicy
	if cfg != nil {
		p = *cfg
	}
	return tuning{
		AnalyzerTuning: p.AnalyzerTuning.WithDefaults(),
		idleKvUsage:    p.IdleKvUsage(),
	}
}

// steadiness is the ITL steady-state gate the tuning sets, mapped by the
// one constructor the slo analyzer uses too (config.SteadyGateTuning.Steadiness).
func (t tuning) steadiness() itl.Steadiness {
	return t.SteadyGate.Steadiness()
}
