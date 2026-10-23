package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// The same Deployment applied to two namespaces has one variant name in both,
// so a line that names only the variant cannot be attributed to either. Issue
// #271: two identical models learned different lines, and the itl-fit records
// that would have shown which was which were indistinguishable.
func TestITLLinesNameTheirNamespace(t *testing.T) {
	const variant = "qwen32b-decode-wva"
	states := []domain.VariantReplicaState{
		{VariantName: variant, Role: domain.RoleDecode, AcceleratorName: "H100",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	serving := func(namespace, pod string) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, variant, 8000, 16000, 0, 1000, 500)
		rm.Namespace = namespace
		rm.KvUsageInstant = 0.5
		rm.AvgITL = 0.05
		// Far from anything the fitted line predicts, so itl-gps-mismatch
		// fires and its namespace can be checked too.
		rm.GenerationTokenRate = 1
		return rm
	}

	a := NewSaturationAnalyzer(capacity.NewStore())
	clock := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }

	logsByNs := map[string]*observer.ObservedLogs{}
	for cycle := 0; cycle < itl.DefaultMinSamples; cycle++ {
		for _, ns := range []string{"evgensh-sg1", "evgensh-sg2"} {
			ctx, logs := observedCtx(t)
			input := makeAnalyzerInput([]domain.ReplicaMetrics{serving(ns, ns+"-pod")}, states)
			input.Namespace = ns
			_, err := a.Analyze(ctx, input)
			require.NoError(t, err)
			logsByNs[ns] = logs
		}
		clock = clock.Add(15 * time.Second)
	}

	for ns, logs := range logsByNs {
		for _, msg := range []string{"itl-window", "itl-fit", "derived-mu", "itl-gps-mismatch"} {
			entries := logs.FilterMessage(msg).All()
			require.NotEmpty(t, entries, "%s: expected a %q line on the last cycle", ns, msg)
			for _, e := range entries {
				assert.Equal(t, ns, e.ContextMap()["namespace"],
					"%q must name the namespace it was fitted in; the variant name is shared", msg)
			}
		}
	}
}
