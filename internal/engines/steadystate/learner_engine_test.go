package steadystate

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/learnreplay"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/enginerec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The engine-level recording (issue #345, step 0): the acceptance script run
// through collectV2ModelRequest, so each model's composite is recorded beside
// its analyzers, and replayed. Against the acceptance run it adds what the
// composite paths need:
//
//   - discovery metadata, which the capacity-build step joins onto every
//     analyzer's variant capacities;
//   - a bridge on agg-a (a warm-pool Pod lent to it), so the warm pool's
//     supply is not zero;
//   - acme/pd under a policy that lists the slo analyzer alone, which sizes
//     nothing: the composite holds both its roles (no listed analyzer sizes
//     them) and prices them per role.
//
// With LEARNREC_ENGINE_OUT set, the raw recording is also written there: the
// input `make learner-golden-add` takes to commit it as the engine golden.

// newEngineScript is the acceptance script with the engine layer on.
func newEngineScript(start time.Time) *recScript {
	s := newRecScript(start)
	s.engineLayer = true
	sloOnly := s.policy
	sloOnly.Analyzers = []config.AnalyzerScoreConfig{{Name: "slo"}}
	s.policies = map[string]config.ScalingPolicy{"acme/pd": sloOnly}
	for i := range s.variants {
		if s.variants[i].name != "agg-a" {
			continue
		}
		rows := s.variants[i].rows
		s.variants[i].rows = func(cycle int, at time.Time) []domain.ReplicaMetrics {
			out := rows(cycle, at)
			// The quietest replica is a bridge lent from the warm pool.
			out[0].FromWarmPool = true
			return out
		}
	}
	return s
}

// recMetadata is the discovery metadata of a model's variants, from their
// states.
func recMetadata(states []domain.VariantReplicaState, model string) []domain.VariantMetadata {
	out := make([]domain.VariantMetadata, 0, len(states))
	for i, vs := range states {
		out = append(out, domain.VariantMetadata{
			VariantName: vs.VariantName, ModelID: model, Namespace: recNS, Role: vs.Role,
			Cost: 10 + float64(i), AcceleratorName: vs.AcceleratorName, Engine: "vllm",
			GPUsPerReplica: vs.GPUsPerReplica, CurrentReplicas: vs.CurrentReplicas,
			DesiredReplicas: vs.DesiredReplicas, ReadyReplicas: vs.CurrentReplicas,
		})
	}
	return out
}

// recordEngineRun runs the engine script as recordRun runs the acceptance
// one: a first controller that learns and writes its snapshot, then a
// restarted one, recorded, that restores it. It returns the recording (nil
// with recording off) and every composite the restarted controller built.
func recordEngineRun(t *testing.T, start time.Time, record bool) ([]byte, [][]allocation.NamedAnalyzerResult) {
	t.Helper()
	s := newEngineScript(start)
	first := s.engine(t)
	for i := range 4 {
		s.cycle(t, first, i, start.Add(time.Duration(i)*2*time.Minute))
	}
	second := s.engine(t)
	var buf bytes.Buffer
	if record {
		second.recordLearners(learnrec.New(&buf, 0))
	}
	composites := make([][]allocation.NamedAnalyzerResult, 0, 12)
	for i, at := range recSchedule(start.Add(30 * time.Minute)) {
		composites = append(composites, s.cycle(t, second, i, at))
	}
	if !record {
		return nil, composites
	}
	require.NoError(t, second.learnRec.Err())
	return buf.Bytes(), composites
}

// A replay at the recording's own commit reproduces the engine run byte for
// byte: every analyzer event and every composite, each rebuilt from the
// analyzers' replayed results and the inputs the engine supplied.
func TestLearnerRecording_TheEngineLayerReplaysByteForByte(t *testing.T) {
	recording, _ := recordEngineRun(t, recStart, true)
	if out := os.Getenv("LEARNREC_ENGINE_OUT"); out != "" {
		require.NoError(t, os.WriteFile(out, recording, 0o644))
		t.Logf("wrote the raw engine recording to %s", out)
	}
	counts := kindCounts(t, recording)
	// One composite per model per cycle: three models for the first four
	// cycles, two after acme/gone leaves.
	require.Equal(t, 3*4+2*8, counts[enginerec.KindComposite], "composites recorded")

	engine := NewLearnerReplay()
	rep, err := learnreplay.Replay(context.Background(), recording,
		learnreplay.Options{Mode: learnreplay.Fidelity, Engine: engine})
	require.NoError(t, err)
	require.Empty(t, mismatchText(rep.Mismatches))
	require.Equal(t, string(recording), string(rep.Recording), "the replay's own recording")
	assert.Equal(t, counts[enginerec.KindComposite], rep.Analyzes[enginerec.KindComposite])
	assert.Empty(t, rep.Ignored, "events the replay skipped")
	for _, p := range LearnerReplayPathNames() {
		assert.Positive(t, rep.Paths[p], "the engine path %s never ran", p)
	}
	t.Logf("engine paths: %v", engine.PathCounts())

	// Without an engine replay, the analyzer events still replay, and the
	// composites are skipped as another owner's.
	plain, err := learnreplay.Replay(context.Background(), recording, learnreplay.Options{Mode: learnreplay.Fidelity})
	require.NoError(t, err)
	require.Empty(t, mismatchText(plain.Mismatches))
	assert.Equal(t, counts[enginerec.KindComposite], plain.Ignored[enginerec.KindComposite])
	assert.Equal(t, counts[saturation.KindAnalyze], plain.Analyzes[saturation.KindAnalyze])
}

// The export of the engine run names nothing the script gave, and replays
// byte for byte with the engine layer.
func TestLearnerRecording_TheEngineExportReplaysAndNamesNothing(t *testing.T) {
	recording, _ := recordEngineRun(t, recStart, true)
	exported, rep, err := learnreplay.Export(recording, learnreplay.ExportOptions{Key: []byte("a key for this test only")})
	require.NoError(t, err)
	t.Logf("export: %s", rep)
	assert.Equal(t, kindCounts(t, recording)[enginerec.KindComposite], kindCounts(t, exported)[enginerec.KindComposite],
		"the export cut a composite")
	replayed, err := learnreplay.Replay(context.Background(), exported,
		learnreplay.Options{Mode: learnreplay.Fidelity, Engine: NewLearnerReplay()})
	require.NoError(t, err)
	require.Empty(t, mismatchText(replayed.Mismatches), "the export does not replay")
	require.Equal(t, string(exported), string(replayed.Recording), "the export's replay")
	for _, name := range scriptNames() {
		require.True(t, bytes.Contains(recording, []byte(name)), "precondition: the recording names %q", name)
		assert.False(t, bytes.Contains(exported, []byte(name)), "%q survives the export", name)
	}
}

// Recording the engine layer changes no composite: the run with the recorder
// on builds every composite the run with it off builds.
func TestLearnerRecording_RecordingTheEngineChangesNoComposite(t *testing.T) {
	_, on := recordEngineRun(t, recStart, true)
	_, off := recordEngineRun(t, recStart, false)
	require.Len(t, on, len(off))
	for i := range on {
		assert.Equal(t, namedResultsText(t, off[i]), namedResultsText(t, on[i]), "cycle %d", i)
	}
}

// One collectV2ModelRequest records exactly one composite, whatever it calls
// inside, and records it after the model's analyzers.
func TestLearnerRecording_OneModelOneComposite(t *testing.T) {
	s := newEngineScript(recStart)
	e := s.engine(t)
	var buf bytes.Buffer
	e.recordLearners(learnrec.New(&buf, 0))
	s.cycle(t, e, 0, recStart)
	events, err := readEvents(buf.Bytes())
	require.NoError(t, err)
	var kinds []string
	for _, ev := range events {
		if ev.Kind == enginerec.KindComposite || ev.Kind == saturation.KindAnalyze {
			kinds = append(kinds, ev.Kind)
		}
	}
	// Three models, each its saturation Analyze and then its composite.
	want := []string{saturation.KindAnalyze, enginerec.KindComposite, saturation.KindAnalyze, enginerec.KindComposite,
		saturation.KindAnalyze, enginerec.KindComposite}
	assert.Equal(t, want, kinds)
}
