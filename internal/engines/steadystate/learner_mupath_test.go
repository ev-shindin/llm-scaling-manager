package steadystate

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/learnreplay"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The mu-paths run: a second scripted engine run, recorded, for the learner
// paths the acceptance run does not reach (docs/proposals/analyzer-learner.md,
// 2.5, per-path counters). Its replicas saturate, so k2 records readings and
// mu's window takes samples folded by the mean across pods (#282), and each
// of mu's three yields runs on a variant of its own:
//
//   - yv-a, the served yield: a measured mu under the derived one gives way to it once a
//     sibling serves more than maxRatio (2x) the measurement;
//   - yv-b, the sibling yield: one slow replica's measurement gives way to what its two
//     full, unqueued siblings serve (siblingYield);
//   - yv-c, the stale yield: a measurement far above the derived figure gives way to it
//     once its bucket has read nothing in agreement for freshFor (5 min);
//   - yv-d, a restart: a saturated replica restarts under the same name (a new Ready
//     time), and k2 refuses its readings for a minute (noteReady, #288).
//
// With LEARNREC_MUPATHS_OUT set, the raw recording is also written there: the
// input `make learner-golden-add` takes to commit it as the mu-paths golden.

// muCycles is the run's length: twelve minutes of 15 s cycles.
const muCycles = 48

// The cycle each variant's second phase starts at.
const (
	muServedPhase = 24 // yv-a's siblings start to serve 2.5x its measurement
	muStalePhase  = 16 // yv-c's replica stops saturating
	muJoinPhase   = 30 // yv-d's replica restarts under its own name
)

// muRow is one Ready replica of the mu-paths model at fill k, Ready since
// ready, completing mu requests a second at the script's 300-token outputs,
// with queue requests waiting.
func muRow(variant, pod string, k float64, at, ready time.Time, mu float64, queue int) domain.ReplicaMetrics {
	// A rate that repeats to the digit across a sample boundary is the same
	// reading, not a new sample (recordSaturatedThroughputFrom): each cycle
	// reads a little apart, as a real gauge does.
	mu *= 1 + 0.01*float64(at.Unix()/15%3)
	rm := recRow("acme/yv", variant, pod, k, at)
	rm.ReadySince = ready
	rm.GenerationTokenRate, rm.RequestRate = mu*300, mu
	rm.QueueLength, rm.MaxRankQueueLength = queue, queue
	rm.CapacityQueueLength, rm.MaxRankCapacityQueueLength = queue, queue
	return rm
}

// newMuScript is the mu-paths fleet: one aggregated model of four variants,
// on the acceptance script's engine and clock.
func newMuScript(start time.Time) *recScript {
	ready := start.Add(-30 * time.Minute)
	s := &recScript{
		base:   clocktesting.NewFakePassiveClock(start),
		router: &scriptedRouter{},
		policy: config.ScalingPolicy{
			KvCacheThreshold: 0.8, QueueLengthThreshold: 5, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.7,
			Analyzers: []config.AnalyzerScoreConfig{
				{Name: "saturation"}, {Name: "slo"}, {Name: "throughput"},
			},
			SLO: &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: 40 * time.Millisecond}}},
		},
		forgetAt:  muCycles + 1,
		goneAfter: muCycles + 1,
	}
	// Two replicas inside the ITL band, held at two levels, give each
	// variant a line of its own, and so a derived mu.
	line := func(variant string, served float64, at time.Time) []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{
			muRow(variant, variant+"-0", 0.30, at, ready, served, 0),
			muRow(variant, variant+"-1", 0.60, at, ready, served, 0),
		}
	}
	s.variants = []recVariant{
		{model: "acme/yv", name: "yv-a", role: domain.RoleDecode, gpus: 1, rows: func(cycle int, at time.Time) []domain.ReplicaMetrics {
			const measured = 6.0
			served := 0.5 * measured
			if cycle >= muServedPhase {
				served = 2.5 * measured
			}
			// Two saturated replicas a fifth apart: each minute's sample is their
			// mean (#282).
			return append(line("yv-a", served, at),
				muRow("yv-a", "yv-a-2", 0.92, at, ready, measured, 6),
				muRow("yv-a", "yv-a-3", 0.92, at, ready, 1.2*measured, 6))
		}},
		{model: "acme/yv", name: "yv-b", role: domain.RoleDecode, gpus: 1, rows: func(_ int, at time.Time) []domain.ReplicaMetrics {
			return append(line("yv-b", 1, at),
				muRow("yv-b", "yv-b-2", 0.92, at, ready, 8, 6),
				muRow("yv-b", "yv-b-3", 0.92, at, ready, 12, 0),
				muRow("yv-b", "yv-b-4", 0.92, at, ready, 12, 0))
		}},
		{model: "acme/yv", name: "yv-c", role: domain.RoleDecode, gpus: 1, rows: func(cycle int, at time.Time) []domain.ReplicaMetrics {
			sat := muRow("yv-c", "yv-c-2", 0.92, at, ready, 25, 6)
			if cycle >= muStalePhase {
				sat = muRow("yv-c", "yv-c-2", 0.50, at, ready, 1, 0)
			}
			return append(line("yv-c", 1, at), sat)
		}},
		{model: "acme/yv", name: "yv-d", role: domain.RoleDecode, gpus: 1, rows: func(cycle int, at time.Time) []domain.ReplicaMetrics {
			since := ready
			if cycle >= muJoinPhase {
				since = start.Add(time.Duration(muJoinPhase)*15*time.Second - 10*time.Second)
			}
			// Two replicas close together inside the band: a line from the pinned
			// fit, never a full one.
			return []domain.ReplicaMetrics{
				muRow("yv-d", "yv-d-0", 0.40, at, ready, 1, 0),
				muRow("yv-d", "yv-d-1", 0.45, at, ready, 1, 0),
				muRow("yv-d", "yv-d-2", 0.92, at, since, 6, 6),
			}
		}},
	}
	objs := make([]client.Object, 0, len(s.variants))
	for _, v := range s.variants {
		objs = append(objs, recDeployment(v, ""))
	}
	s.k8s = fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
	return s
}

// recordMuRun runs the mu-paths script on one controller, recorded from its
// first cycle, and returns the recording and the saturation analyzer's path
// counts.
func recordMuRun(t *testing.T, start time.Time) ([]byte, map[string]int64) {
	t.Helper()
	s := newMuScript(start)
	e := s.engine(t)
	var buf bytes.Buffer
	e.recordLearners(learnrec.New(&buf, 0))
	for i := range muCycles {
		s.cycle(t, e, i, start.Add(time.Duration(i)*15*time.Second))
	}
	require.NoError(t, e.learnRec.Err())
	return buf.Bytes(), e.saturationAnalyzer().(*saturation.SaturationAnalyzer).PathCounts()
}

// The mu-paths run reaches every learner path the acceptance run does not,
// and its replay counts each as the run did.
func TestLearnerRecording_TheMuPathsRunReachesEveryLearnerPath(t *testing.T) {
	recording, counted := recordMuRun(t, recStart)
	if out := os.Getenv("LEARNREC_MUPATHS_OUT"); out != "" {
		require.NoError(t, os.WriteFile(out, recording, 0o644))
		t.Logf("wrote the raw mu-paths recording to %s", out)
	}
	t.Logf("path counts: %v", counted)
	for _, p := range saturation.PathNames() {
		assert.Positive(t, counted[p], "the mu-paths run never ran %s", p)
	}
	rep, err := learnreplay.Replay(context.Background(), recording, learnreplay.Options{Mode: learnreplay.Fidelity})
	require.NoError(t, err)
	require.Empty(t, mismatchText(rep.Mismatches), "the mu-paths replay diverged from the recording")
	// The saturation analyzer's paths only: the settle and engine counters
	// the replay adds are not the analyzer's, and are held elsewhere
	// (TestLearnerRecording_AReplayAtTheSameCommitReproducesEveryResult).
	replayed := map[string]int64{}
	for _, p := range saturation.PathNames() {
		replayed[p] = rep.Paths[p]
	}
	assert.Equal(t, counted, replayed, "the replay counts the paths the run did")
}
