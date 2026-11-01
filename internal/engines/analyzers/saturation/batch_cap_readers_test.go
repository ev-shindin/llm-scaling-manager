package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// Issue #420: the batch cap's readers besides P3 read the cap #399 added --
// the llm-d.ai/max-num-seqs annotation, and the learned cap -- where the
// engine's flags state none. Each reader is driven through Analyze, with the
// flags unread (MaxNumSeqs the parser's default of 256, not stated).

// declaredParams is an engine whose flags state no cap, and whose scale
// target declares perRank.
func declaredParams(perRank int64) *capacity.EngineParams {
	p := deploymentParams()
	p.DeclaredBatchCap = perRank
	return p
}

// cappedITLWindow runs one cycle of a decode replica holding running
// sequences, 768 waiting, at KV 0.87 -- above the ITL band -- under params,
// with edit applied to the analyzer and the policy learning set, and returns
// the itl-window line.
func cappedITLWindow(t *testing.T, params *capacity.EngineParams, running int, learning bool,
	edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		GpuCount: 1, EngineParams: params, LearnedFrom: "deployment",
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	if edit != nil {
		edit(a)
	}
	capped := makeReplicaMetrics("pod-1", "variant-d", 14000, 16000, 768, 1000, 500)
	capped.Ready = true
	capped.KvUsageInstant, capped.AvgITL = 0.87, 0.079
	capped.RunningRequests, capped.PreemptionRateKnown = running, true
	in := makeAnalyzerInput([]domain.ReplicaMetrics{capped}, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H100",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	})
	in.Config.(*config.ScalingPolicy).EnableBatchCapLearning = learning
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	return requireLogged(t, logs, "itl-window")
}

// A replica at the cap its scale target declares teaches the line from
// above the band, as one at the cap its flags state does (issue #287).
// Before #420 only the flags' cap reached the window: the declared cap was
// read by P3 alone, and the reading counted as above the band.
func TestBatchCapReaders_TheITLWindowTakesAReplicaAtItsDeclaredCap(t *testing.T) {
	fields := cappedITLWindow(t, declaredParams(128), 128, false, nil)
	assert.EqualValues(t, 128, fields["batchCap"])
	assert.EqualValues(t, 1, fields["capped"])
	assert.EqualValues(t, 0, fields["aboveBand"])

	// Below the declared cap the batch is still growing.
	fields = cappedITLWindow(t, declaredParams(128), 127, false, nil)
	assert.EqualValues(t, 0, fields["capped"])
	assert.EqualValues(t, 1, fields["aboveBand"])
}

// A learned cap is a cap something states, for the window as for P3, and
// only while the policy learns.
func TestBatchCapReaders_TheITLWindowTakesAReplicaAtItsLearnedCap(t *testing.T) {
	params := deploymentParams()
	learned := func(a *SaturationAnalyzer) {
		a.batchCaps[batchCapKey("test-ns", "test-model", "variant-d")] = &learnedBatchCap{perRank: 128, args: windowArgs(params)}
	}
	fields := cappedITLWindow(t, params, 128, true, learned)
	assert.EqualValues(t, 128, fields["batchCap"])
	assert.EqualValues(t, 1, fields["capped"])

	fields = cappedITLWindow(t, params, 128, false, learned)
	assert.EqualValues(t, 0, fields["batchCap"], "learning off: no learned cap is read")
	assert.EqualValues(t, 0, fields["capped"])
}

// The switch off reads the flags' cap alone, as before #420.
func TestBatchCapReaders_TheITLAdmissionSwitchPicksTheSource(t *testing.T) {
	off := func(a *SaturationAnalyzer) { a.batchCapITLAdmission = false }
	fields := cappedITLWindow(t, declaredParams(128), 128, false, off)
	assert.EqualValues(t, 0, fields["batchCap"])
	assert.EqualValues(t, 0, fields["capped"])
	assert.EqualValues(t, 1, fields["aboveBand"])

	stated := declaredParams(128)
	stated.MaxNumSeqs, stated.MaxNumSeqsSet = 128, true
	fields = cappedITLWindow(t, stated, 128, false, off)
	assert.EqualValues(t, 1, fields["capped"], "the flags' cap is read either way")
}

// pdDecodeAtCap runs four cycles of a P/D decode replica holding running
// sequences with every waiting request deferred (#287's shape), under
// params, with edit applied to the analyzer.
func pdDecodeAtCap(t *testing.T, params *capacity.EngineParams, running int,
	edit func(*SaturationAnalyzer)) (*SaturationAnalyzer, *domain.AnalyzerResult) {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", decodeVariant, capacity.Record{
		GpuCount: 1, EngineParams: params, LearnedFrom: "deployment",
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	if edit != nil {
		edit(a)
	}
	clock := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	rm := withReason(pdDecodeRow(halfCache), 0)
	rm.RunningRequests = running
	in := makeAnalyzerInput([]domain.ReplicaMetrics{rm, pdPrefillRow()}, pdStates)
	var res *domain.AnalyzerResult
	for range 4 {
		var err error
		res, err = a.Analyze(context.Background(), in)
		require.NoError(t, err)
		clock = clock.Add(30 * time.Second)
	}
	return a, res
}

// A P/D decode at the cap its scale target declares keeps its full queue,
// as one at the cap its flags state does: at the cap every waiting request
// reads deferred, and waits for a batch slot here (#287). Before #420 the
// view read the flags' cap alone, so an annotated decode at its cap dropped
// the queue: no k2, and no queue demand.
func TestBatchCapReaders_APDDecodeAtItsDeclaredCapKeepsTheFullQueue(t *testing.T) {
	a, res := pdDecodeAtCap(t, declaredParams(128), 128, nil)
	assert.True(t, recordedK2(a), "768 waiting behind a full batch: saturated")
	assert.InDelta(t, float64(halfCache)+queueCharge, decodeDemand(t, res), 1,
		"every request waits for a batch slot on this replica: the whole queue is charged")

	a, res = pdDecodeAtCap(t, declaredParams(128), 127, nil)
	assert.False(t, recordedK2(a), "below the declared cap a deferred request is in flight")
	assert.InDelta(t, float64(halfCache), decodeDemand(t, res), 1)
}

// The switch off reads the flags' cap alone, as before #420.
func TestBatchCapReaders_TheDecodeViewSwitchPicksTheSource(t *testing.T) {
	a, res := pdDecodeAtCap(t, declaredParams(128), 128, func(a *SaturationAnalyzer) { a.batchCapDecodeView = false })
	assert.False(t, recordedK2(a))
	assert.InDelta(t, float64(halfCache), decodeDemand(t, res), 1)
}

// derivedMuSeqs runs one cycle of a decode replica with a short shape whose
// cache holds far more sequences than declared, under params with an ITL
// line published, edit applied, and returns the derived-mu line.
func derivedMuSeqs(t *testing.T, params *capacity.EngineParams, edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H200", GpuCount: 1, EngineParams: params,
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	a.publishLine(a.itlPhysicsKey("test-ns", "test-model", "variant-d", "H200", 1, ""),
		"a fitted line", itl.Model{A: 0.08, B: 0.01}, time.Now())
	if edit != nil {
		edit(a)
	}
	rm := makeReplicaMetrics("pod-1", "variant-d", 5000, 100_000, 0, 100, 50)
	in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H200",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	})
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	fields := requireLogged(t, logs, "derived-mu")
	require.Equal(t, true, fields["ok"], "the line prices mu: %v", fields)
	return fields
}

// The derived mu caps the resident sequences at the cap the scale target
// declares, as P3 does. Before #420 it capped them at the parser's default
// of 256, so a sequence-bound engine read a mu above what it serves and the
// throughput floor ordered too few replicas.
func TestBatchCapReaders_TheDerivedMuIsCappedAtTheDeclaredCap(t *testing.T) {
	fields := derivedMuSeqs(t, declaredParams(32), nil)
	assert.InDelta(t, 32, fields["seqs"], 1e-9)

	// With nothing declared the parser's default binds: the cache holds
	// 0.68 x 100000 / 125 = 544 sequences, capped at 256.
	fields = derivedMuSeqs(t, deploymentParams(), nil)
	assert.InDelta(t, 256, fields["seqs"], 1e-9)
}

// The switch off caps at the flags' cap or the default, as before #420.
func TestBatchCapReaders_TheDerivedMuSwitchPicksTheSource(t *testing.T) {
	on := derivedMuSeqs(t, declaredParams(32), nil)
	assert.Equal(t, batchCapDeclared, on["batchCapSource"])
	off := derivedMuSeqs(t, declaredParams(32), func(a *SaturationAnalyzer) { a.batchCapDerivedMu = false })
	assert.Equal(t, batchCapDefault, off["batchCapSource"])
	assert.EqualValues(t, 256, off["batchCap"])
	assert.InDelta(t, 256, off["seqs"], 1e-9)
}

// statedBatchCap is the pricing cap where something states it, and 0 where
// only the default stands in.
func TestBatchCapReaders_StatedBatchCap(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	stated := deploymentParams()
	stated.MaxNumSeqs, stated.MaxNumSeqsSet = 64, true
	assert.EqualValues(t, 64, a.statedBatchCap("ns", "m", "v", stated, false))
	assert.EqualValues(t, 96, a.statedBatchCap("ns", "m", "v", declaredParams(96), false))
	assert.EqualValues(t, 0, a.statedBatchCap("ns", "m", "v", deploymentParams(), true), "the default is no stated cap")
	assert.EqualValues(t, 0, a.statedBatchCap("ns", "m", "v", nil, true))
	a.batchCaps[batchCapKey("ns", "m", "v")] = &learnedBatchCap{perRank: 120, args: windowArgs(deploymentParams())}
	assert.EqualValues(t, 120, a.statedBatchCap("ns", "m", "v", deploymentParams(), true))
	assert.EqualValues(t, 0, a.statedBatchCap("ns", "m", "v", deploymentParams(), false))
}
