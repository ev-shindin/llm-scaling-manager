package learnreplay

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// fullStart is a start with the slo and throughput analyzers.
func fullStart() learnrec.Start {
	s := defaultStart()
	s.SLO = &learnrec.SLOStart{StateTimeout: time.Hour, SlopeHoldFor: time.Hour, Judging: true}
	s.Throughput = true
	return s
}

// mixedRecording is a start, a store sweep that removes a record, an event
// of an owner the replay does not drive, and an Analyze of each analyzer.
func mixedRecording(t *testing.T) []byte {
	t.Helper()
	f := newFixture(replayAt)
	f.buf.Reset()
	rec := learnrec.New(f.buf, 0)
	f.attach(rec)
	rec.Record(learnrec.KindStart, replayAt, fullStart())
	params := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64}
	f.store.FileScaleTarget("ns", "m", "old", "H100", 1, params, false, replayAt.Add(-2*time.Hour))
	f.store.FileScaleTarget("ns", "m", "v", "H100", 1, params, false, replayAt)
	f.store.EvictStale(replayAt, time.Hour)
	rec.Record("composite.Plan", replayAt, map[string]int{"replicas": 3})
	in := domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
		Config: &config.ScalingPolicy{KvCacheThreshold: 0.8,
			SLO: &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: 50 * time.Millisecond}}}},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", GPUsPerReplica: 1, AcceleratorName: "H100"}}}
	_, err := f.sat.Analyze(context.Background(), in)
	require.NoError(t, err)
	_, err = f.slo.Analyze(context.Background(), in)
	require.NoError(t, err)
	_, err = f.tp.Analyze(context.Background(), in)
	require.NoError(t, err)
	return bytes.Clone(f.buf.Bytes())
}

// An event of an owner the replay does not drive (the engine-level layer
// #345 adds to the same recording) is skipped, counted, and left out of the
// comparison, so the events after it still compare.
func TestReplay_IgnoresAndCountsOtherOwnersEvents(t *testing.T) {
	rec := mixedRecording(t)
	rep, err := Replay(context.Background(), rec, Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.Empty(t, rep.Failed())
	assert.Equal(t, map[string]int{"composite.Plan": 1}, rep.Ignored)
	assert.Equal(t, 1, rep.Analyzes["slo.Analyze"])
	assert.Equal(t, 1, rep.Analyzes["throughput.Analyze"])
}

// What the run's store sweep removed is a check that fails only a Fidelity
// replay; in Normal mode it is reported.
func TestReplay_AnEvictCountFailsOnlyFidelity(t *testing.T) {
	rec := mixedRecording(t)
	tampered := bytes.Replace(rec, []byte(`"Removed":1`), []byte(`"Removed":2`), 1)
	require.NotEqual(t, rec, tampered, "precondition: the sweep removed one record")
	for mode, fatal := range map[Mode]bool{Fidelity: true, Normal: false} {
		rep, err := Replay(context.Background(), tampered, Options{Mode: mode})
		require.NoError(t, err)
		var evict []Mismatch
		for _, m := range rep.Mismatches {
			if m.What == "evict" {
				evict = append(evict, m)
			}
		}
		require.Len(t, evict, 1, "mode %d", mode)
		assert.Equal(t, fatal, evict[0].Fatal, "mode %d", mode)
		assert.Contains(t, evict[0].Detail, "removed 1 records, the run removed 2")
	}
}

func TestReplay_SwitchesReachEveryPackage(t *testing.T) {
	rec := mixedRecording(t)
	for _, pkg := range []string{PackageSaturation, PackageSLO, PackageThroughput, PackageCapacity} {
		_, err := Replay(context.Background(), rec, Options{Mode: Normal,
			Switches: map[string]map[string]bool{pkg: {}}})
		assert.NoError(t, err, "%s: the empty set", pkg)
		_, err = Replay(context.Background(), rec, Options{Mode: Normal,
			Switches: map[string]map[string]bool{pkg: {"b": true, "a": false}}})
		if assert.Error(t, err, pkg) {
			assert.Contains(t, err.Error(), pkg+": no replay switch a, b", "names sorted")
		}
	}
	_, err := Replay(context.Background(), rec, Options{Switches: map[string]map[string]bool{"composite": {}}})
	assert.Error(t, err, "a package with no hook")

	// A run without the slo and throughput analyzers has no switches there.
	small := smallRecording(t, defaultStart())
	for _, pkg := range []string{PackageSLO, PackageThroughput} {
		_, err := Replay(context.Background(), small, Options{Switches: map[string]map[string]bool{pkg: {}}})
		if assert.Error(t, err, pkg) {
			assert.Contains(t, err.Error(), "did not have")
		}
	}
}

func TestReplay_StopsOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Replay(ctx, mixedRecording(t), Options{Mode: Fidelity})
	require.ErrorIs(t, err, context.Canceled)
}

func TestReplay_RefusesAnEmptyRecording(t *testing.T) {
	_, err := Replay(context.Background(), nil, Options{})
	require.Error(t, err)
}

// compareEvents names each way two recordings can differ.
func TestCompareEvents_Branches(t *testing.T) {
	ev := func(seq uint64, kind string, at time.Time, data string) learnrec.RawEvent {
		return learnrec.RawEvent{Seq: seq, Kind: kind, Now: at, Data: []byte(data)}
	}
	encode := func(evs ...learnrec.RawEvent) []byte {
		var buf bytes.Buffer
		r := learnrec.New(&buf, 0)
		for _, e := range evs {
			r.Record(e.Kind, e.Now, learnrec.Raw(e.Data))
		}
		return buf.Bytes()
	}
	a := ev(1, "store.EvictStale", replayAt, `{"Timeout":1,"Removed":0}`)
	b := ev(2, "slo.BeginCycle", replayAt, `{}`)

	assert.Empty(t, compareEvents([]learnrec.RawEvent{a, b}, encode(a, b)), "the same events")

	got := compareEvents([]learnrec.RawEvent{a, b}, encode(b, a))
	require.Len(t, got, 2)
	assert.Equal(t, "the replay recorded slo.BeginCycle here", got[0].Detail)

	later := ev(1, a.Kind, replayAt.Add(time.Second), string(a.Data))
	got = compareEvents([]learnrec.RawEvent{a}, encode(later))
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Detail, "read the clock at")

	other := ev(1, a.Kind, replayAt, `{"Timeout":1,"Removed":9}`)
	got = compareEvents([]learnrec.RawEvent{a}, encode(other))
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Detail, `"Removed":0`)
	assert.Contains(t, got[0].Detail, `"Removed":9`)

	got = compareEvents([]learnrec.RawEvent{a}, encode(a, b))
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Detail, "the run recorded 1 events, the replay 2; the replay's extra events begin slo.BeginCycle")

	got = compareEvents([]learnrec.RawEvent{a, b}, encode(a))
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Detail, "the run's extra events begin slo.BeginCycle")
	for _, m := range got {
		assert.True(t, m.Fatal)
	}
}
