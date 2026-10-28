package saturation

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The zero-replica estimate's ceiling from a live sibling is found under the
// looser equality. The borrow's equality compares the checkpoint, prefix
// caching and the parallel layout too, and refusing a sibling on those left a
// parked variant on its raw derived figure -- a ceiling removed, not
// tightened.
func TestACeilingSurvivesADifferenceTheBorrowRefuses(t *testing.T) {
	const (
		ns, model, accel = "test-ns", "test-model", "H100"
		zero, donor      = "variant-zero", "variant-donor"
		ceiling          = 5_000
	)
	for name, mutate := range map[string]func(*capacity.EngineParams){
		"the same checkpoint spelled as a node-local path": func(p *capacity.EngineParams) { p.ModelPath = "/models/org-m" },
		"prefix caching off on the parked variant":         func(p *capacity.EngineParams) { p.PrefixCachingDisabled = true },
		"the control, identical":                           func(*capacity.EngineParams) {},
	} {
		t.Run(name, func(t *testing.T) {
			donorParams := reuseSwitchParams()
			donorParams.ModelPath = "org/m"
			donorParams.EffectiveMaxBatchedTokens = 1_000_000
			zeroParams := *donorParams
			mutate(&zeroParams)

			store := capacity.NewStore()
			store.Update(ns, model, zero, capacity.Record{
				AcceleratorName: accel, GpuCount: 1,
				EffectiveCapacity: 900_000, EngineParams: &zeroParams, LearnedFrom: "deployment",
			}, time.Now())
			store.Update(ns, model, donor, capacity.Record{
				AcceleratorName: accel, GpuCount: 1,
				EffectiveCapacity: ceiling, EngineParams: donorParams, LearnedFrom: capacity.LearnedFromLive,
			}, time.Now())
			a := NewSaturationAnalyzer(store)
			rec := a.capacityStore.Get(ns, model, zero)
			require.NotNil(t, rec)

			got := a.estimateStoredCapacity(rec, model, ns, zero, accel, 1, 0.9, 1000, 600, logr.Discard())
			require.Equal(t, float64(ceiling), got,
				"the parked variant's estimate is not bounded by its live sibling's measured capacity")
		})
	}
}
