package learnrec

import (
	"fmt"
	"sort"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Input is an Analyze call's input as a recording holds it: the rows raw, as
// Analyze received them, with every presence bit; the variants' states; the
// merged policy, analyzerTuning and loadImbalance included, every cycle; the
// scheduler queue and the arrival rate.
//
// It mirrors domain.AnalyzerInput field for field. Config is written as its
// dynamic value, and ConfigType names that value's type, so a replay knows
// what to read it back into: the engine passes a *config.ScalingPolicy. This
// package does not import config, whose tests import the analyzers that
// import this package.
type Input struct {
	ModelID        string
	Namespace      string
	ReplicaMetrics []domain.ReplicaMetrics
	VariantStates  []domain.VariantReplicaState
	Config         any
	// ConfigType is Config's dynamic type, empty for a nil Config.
	ConfigType     string
	SchedulerQueue *domain.SchedulerQueueMetrics
	ArrivalRate    float64
}

// InputOf is the recorded form of in.
func InputOf(in domain.AnalyzerInput) Input {
	out := Input{
		ModelID:        in.ModelID,
		Namespace:      in.Namespace,
		ReplicaMetrics: in.ReplicaMetrics,
		VariantStates:  in.VariantStates,
		SchedulerQueue: in.SchedulerQueue,
		ArrivalRate:    in.ArrivalRate,
	}
	if in.Config != nil {
		out.Config, out.ConfigType = in.Config, fmt.Sprintf("%T", in.Config)
	}
	return out
}

// VariantNames is every variant an input names, in its states or its rows,
// sorted.
func VariantNames(in domain.AnalyzerInput) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, vs := range in.VariantStates {
		add(vs.VariantName)
	}
	for _, rm := range in.ReplicaMetrics {
		add(rm.VariantName)
	}
	sort.Strings(out)
	return out
}

// ResultOf is what a recording holds of an Analyze result: the result
// without AnalyzedAt, which is the clock and not an output. Nil stays nil.
func ResultOf(r *domain.AnalyzerResult) *domain.AnalyzerResult {
	if r == nil {
		return nil
	}
	c := *r
	c.AnalyzedAt = time.Time{}
	return &c
}

// ErrorOf is an error as a recording holds it: its text, empty for nil.
func ErrorOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// KindStart is the first line of every recording, with Start.
const KindStart = "start"

// Start is how the run was configured when the recording began: what the
// analyzers' configuration builders set, and which analyzers ran. A replay
// builds its analyzers from it and refuses a recording that has none.
type Start struct {
	// Saturation is the saturation analyzer's configuration.
	Saturation SaturationStart
	// SLO is the slo analyzer's configuration; nil when the run had none.
	SLO *SLOStart
	// Throughput is whether the run registered the throughput analyzer.
	Throughput bool
	// StoreRecords is how many records the capacity store held. A recording
	// starts only at a controller start, when the store is new and empty; a
	// replay refuses any other count, since it has no way to file them.
	StoreRecords int
}

// SaturationStart is what WithHorizons and WithOptimizationInterval set: the
// horizons in force, and the borrow clock's longest counted gap
// (BorrowTickGap), which is what the optimization interval sets.
type SaturationStart struct {
	EpisodeGap, VariantTimeout, BucketRetention time.Duration
	BorrowTickGap                               time.Duration
}

// SLOStart is what WithStateTimeout and WithSlopeHoldFor set, and whether the
// analyzer had a router to judge with (Judging): a replay serves the recorded
// latency through a router exactly when the run had one.
type SLOStart struct {
	StateTimeout, SlopeHoldFor time.Duration
	Judging                    bool
}
