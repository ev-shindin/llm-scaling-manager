package steadystate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
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
//     them) and prices them per role;
//   - an external analyzer, listed for the other two models, that fails
//     every third call of a model: its results are recorded, since the
//     replay cannot run it, and on the cycles it fails it is inside its
//     staleness grace, so the composite holds the roles it covers against
//     scale-down.
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
	s.policy.Analyzers = append(s.policy.Analyzers, config.AnalyzerScoreConfig{Name: "ext"})
	s.external = &scriptedExternal{calls: map[string]int{}}
	for i := range s.variants {
		if s.variants[i].name != recScaledVariant {
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

// scriptedExternal is an external analyzer the replay cannot run: it sizes
// every variant at 10 a replica with 9 a replica of demand, and fails every
// third call for a model.
type scriptedExternal struct{ calls map[string]int }

func (x *scriptedExternal) Name() string { return "ext" }

func (x *scriptedExternal) Analyze(_ context.Context, in domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	x.calls[in.ModelID]++
	if x.calls[in.ModelID]%3 == 0 {
		return nil, errors.New("the scripted external analyzer fails this cycle")
	}
	res := &domain.AnalyzerResult{AnalyzerName: x.Name(), ModelID: in.ModelID, Namespace: in.Namespace}
	for _, vs := range in.VariantStates {
		demand := 9 * float64(vs.CurrentReplicas)
		res.VariantCapacities = append(res.VariantCapacities, domain.VariantCapacity{VariantName: vs.VariantName,
			Role: vs.Role, ReplicaCount: vs.CurrentReplicas, PerReplicaCapacity: 10, TotalDemand: demand})
		res.TotalDemand += demand
	}
	return res, nil
}

// recScaledDownAt is the cycle from which agg-a's scale target is scaled
// down by two replicas while its rows still report all ten: the
// capacity-build step clamps the replica count each analyzer measured to the
// target's current replicas (clampReplicaCountToScaleTarget). Its Ready
// replicas stay at ten, so a clamp that read them would not clamp.
const recScaledDownAt = 6

// recScaledVariant is the variant the engine script lends a bridge to and
// scales below its rows.
const recScaledVariant = "agg-a"

// recMetadata is the discovery metadata of a model's variants in a cycle,
// from their states. Every field is set, most to filler values: the
// recording keeps only the three the capacity-build step reads (enginerec.
// VariantMeta), so a replay that matches proves it reads no other.
func recMetadata(states []domain.VariantReplicaState, model string, cycle int) []domain.VariantMetadata {
	out := make([]domain.VariantMetadata, 0, len(states))
	for i, vs := range states {
		var m domain.VariantMetadata
		fillNonZero(reflect.ValueOf(&m).Elem())
		m.VariantName, m.ModelID, m.Namespace, m.Role = vs.VariantName, model, recNS, vs.Role
		m.Cost, m.AcceleratorName, m.Engine, m.GPUsPerReplica = 10+float64(i), vs.AcceleratorName, "vllm", vs.GPUsPerReplica
		m.CurrentReplicas, m.DesiredReplicas, m.ReadyReplicas = vs.CurrentReplicas, vs.DesiredReplicas, vs.CurrentReplicas
		if vs.VariantName == recScaledVariant && cycle >= recScaledDownAt {
			m.CurrentReplicas, m.DesiredReplicas = vs.CurrentReplicas-2, vs.CurrentReplicas-2
		}
		out = append(out, m)
	}
	return out
}

// fillNonZero sets every settable field under v to a value that is not its
// zero: a filler, for fields a test must show are not read.
func fillNonZero(v reflect.Value) {
	if v.Type() == reflect.TypeFor[time.Time]() {
		v.Set(reflect.ValueOf(recStart))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("filler")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(3)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(3)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillNonZero(p.Elem())
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(s.Index(0))
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		e := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(k)
		fillNonZero(e)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillNonZero(v.Field(i))
			}
		}
	}
}

// The capacity-build step reads only the three metadata fields the
// recording keeps: the script sets every field of the metadata, the replay
// has only those three, and every composite still matches, the clamped
// cycles included.
func TestLearnerRecording_TheCapacityBuildReadsOnlyTheRecordedMetadata(t *testing.T) {
	states := []domain.VariantReplicaState{{VariantName: recScaledVariant, Role: domain.RoleBoth, CurrentReplicas: 10, DesiredReplicas: 10,
		AcceleratorName: "NVIDIA-H100-80GB-HBM3", GPUsPerReplica: 1}}
	m := recMetadata(states, "acme/agg", recScaledDownAt)[0]
	mv := reflect.ValueOf(m)
	for i := range mv.NumField() {
		assert.False(t, mv.Field(i).IsZero(), "precondition: the script sets %s", mv.Type().Field(i).Name)
	}
	assert.Less(t, m.CurrentReplicas, m.ReadyReplicas, "precondition: agg-a is scaled below its Ready replicas")

	recording, _ := recordEngineRun(t, recStart, true)
	engine := NewLearnerReplay()
	rep, err := learnreplay.Replay(context.Background(), recording,
		learnreplay.Options{Mode: learnreplay.Fidelity, Engine: engine})
	require.NoError(t, err)
	require.Empty(t, mismatchText(rep.Mismatches))
	assert.Positive(t, engine.PathCounts()[PathCompositeReplicaClamped], "no composite clamped a replica count")
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
	// The external analyzer's name is renamed wherever it stands: an entry,
	// a result, the policy, a grace entry and a hold reason. The built-in
	// analyzers keep theirs.
	assert.Equal(t, 1, rep.Names["analyzer"], "analyzer names renamed")
	for _, site := range []string{`"Name":"ext"`, `"AnalyzerName":"ext"`, `analyzer-in-grace:ext`} {
		require.True(t, bytes.Contains(recording, []byte(site)), "precondition: the recording holds %s", site)
		assert.False(t, bytes.Contains(exported, []byte(site)), "%s survives the export", site)
	}
	for _, site := range []string{`"Name":"analyzer-01"`, `analyzer-in-grace:analyzer-01`, `"Name":"throughput"`} {
		assert.True(t, bytes.Contains(exported, []byte(site)), "the export holds no %s", site)
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
