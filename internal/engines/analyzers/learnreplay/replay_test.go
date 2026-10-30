package learnreplay

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

var replayAt = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// smallRecording is a start, a filed record and one saturation Analyze.
func smallRecording(t *testing.T, start learnrec.Start) []byte {
	t.Helper()
	f := newFixture(replayAt)
	rec := learnrec.New(f.buf, 0)
	f.store.WithRecorder(rec)
	f.sat.WithRecorder(rec)
	f.buf.Reset()
	rec.Record(learnrec.KindStart, replayAt, start)
	f.store.FileScaleTarget("ns", "m", "v", "H100", 1,
		capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, Image: "img:1"}, false, replayAt)
	_, err := f.sat.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
		Config: &config.ScalingPolicy{KvCacheThreshold: 0.8},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 0, GPUsPerReplica: 1,
			AcceleratorName: "H100"}}})
	require.NoError(t, err)
	return bytes.Clone(f.buf.Bytes())
}

func defaultStart() learnrec.Start {
	h := capacity.DefaultHorizons()
	return learnrec.Start{Saturation: learnrec.SaturationStart{EpisodeGap: h.EpisodeGap,
		VariantTimeout: h.VariantTimeout, BucketRetention: h.BucketRetention, BorrowTickGap: saturation.BorrowedK2TickGap}}
}

func TestReplay_ReproducesASmallRecording(t *testing.T) {
	rec := smallRecording(t, defaultStart())
	rep, err := Replay(context.Background(), rec, Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.Empty(t, rep.Mismatches)
	assert.Equal(t, string(rec), string(rep.Recording))
	assert.Equal(t, 1, rep.Analyzes[saturation.KindAnalyze])
	assert.Equal(t, 1, rep.StoreChecks)
}

func TestReplay_RefusesARecordingWithoutAStart(t *testing.T) {
	rec := smallRecording(t, defaultStart())
	_, rest, _ := bytes.Cut(rec, []byte("\n"))
	_, err := Replay(context.Background(), rest, Options{Mode: Fidelity})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no start line")
}

func TestReplay_RefusesAStartWithStoreRecords(t *testing.T) {
	start := defaultStart()
	start.StoreRecords = 3
	_, err := Replay(context.Background(), smallRecording(t, start), Options{Mode: Fidelity})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty store")
}

func TestReplay_RefusesAnEventForAnAnalyzerTheRunDidNotHave(t *testing.T) {
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	rec.Record(learnrec.KindStart, replayAt, defaultStart())
	rec.Record("slo.BeginCycle", replayAt, struct{}{})
	_, err := Replay(context.Background(), buf.Bytes(), Options{Mode: Fidelity})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no slo analyzer")
}

func TestReplay_RefusesUnknownSwitches(t *testing.T) {
	_, err := Replay(context.Background(), smallRecording(t, defaultStart()),
		Options{Mode: Normal, Switches: map[string]map[string]bool{PackageSaturation: {"nope": true}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no replay switch")
}

// A record the run read that the replay's store does not hold: Fidelity
// compares whole records, Normal only what the engine supplied.
func TestReplay_TheStoreCheckByMode(t *testing.T) {
	rec := smallRecording(t, defaultStart())
	// The run's record named another image, an engine-supplied field: in the
	// Analyze event's copy of the record only, so the filing keeps img:1.
	i := bytes.Index(rec, []byte(saturation.KindAnalyze))
	require.Positive(t, i)
	tampered := append(bytes.Clone(rec[:i]), bytes.Replace(rec[i:], []byte(`"Image":"img:1"`), []byte(`"Image":"img:2"`), 1)...)

	for _, mode := range []Mode{Fidelity, Normal} {
		rep, err := Replay(context.Background(), tampered, Options{Mode: mode})
		require.NoError(t, err)
		var store []string
		for _, m := range rep.Mismatches {
			if m.What == "store" {
				store = append(store, m.Detail)
			}
		}
		require.Len(t, store, 1, "mode %d", mode)
		if mode == Normal {
			assert.Contains(t, store[0], `image "img:1", want "img:2"`)
		}
	}

	// A field the analyzer writes, not the engine: only Fidelity holds the
	// replay to it.
	analyzerSide := append(bytes.Clone(rec[:i]), bytes.Replace(rec[i:], []byte(`"EffectiveCapacity":0`), []byte(`"EffectiveCapacity":7`), 1)...)
	require.NotEqual(t, rec, analyzerSide, "precondition: the record names EffectiveCapacity")
	fid, err := Replay(context.Background(), analyzerSide, Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.True(t, hasStoreMismatch(fid.Mismatches), "Fidelity must hold the whole record")
	norm, err := Replay(context.Background(), analyzerSide, Options{Mode: Normal})
	require.NoError(t, err)
	assert.False(t, hasStoreMismatch(norm.Mismatches), "Normal holds only the engine-supplied fields")
}

func hasStoreMismatch(ms []Mismatch) bool {
	for _, m := range ms {
		if m.What == "store" {
			return true
		}
	}
	return false
}

// A line the replay records differently is named with the event and the
// first difference.
func TestReplay_FidelityNamesTheFirstDifference(t *testing.T) {
	rec := smallRecording(t, defaultStart())
	i := bytes.Index(rec, []byte(saturation.KindAnalyze))
	tampered := append(bytes.Clone(rec[:i]), bytes.Replace(rec[i:], []byte(`"Error":""`), []byte(`"Error":"boom"`), 1)...)
	rep, err := Replay(context.Background(), tampered, Options{Mode: Fidelity})
	require.NoError(t, err)
	require.NotEmpty(t, rep.Mismatches)
	m := rep.Mismatches[len(rep.Mismatches)-1]
	assert.Equal(t, "line", m.What)
	assert.Equal(t, saturation.KindAnalyze, m.Kind)
	assert.True(t, strings.Contains(m.Detail, "boom"), m.Detail)
}

// A config of another type than the policy the engine passes cannot be read
// back: the replay says so rather than analyzing with none.
func TestReplay_RefusesAConfigItCannotRebuild(t *testing.T) {
	type other struct{ domain.AnalyzerConfig }
	raw, err := learnrec.Fragment(learnrec.InputOf(domain.AnalyzerInput{Config: other{}}))
	require.NoError(t, err)
	_, err = decodeInput(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot rebuild")
}
