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
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The review's leak probes (PR #388), kept as regression tests. Each puts a
// name where the first export missed it, and holds the export to not
// committing it: the export either renames it, or refuses. Each failed on
// the commit the review read (67fb4e80f): the name survived.

// probeRecording is a start, the variants filed and states for one
// Analyze under pol, and whatever before records first.
func probeRecording(t *testing.T, ns, model string, variants []string, pol *config.ScalingPolicy, before func(*learnrec.Recorder, *fixture)) []byte {
	t.Helper()
	if pol == nil {
		pol = &config.ScalingPolicy{KvCacheThreshold: 0.8}
	}
	f := newFixture(replayAt)
	f.buf.Reset()
	rec := learnrec.New(f.buf, 0)
	f.attach(rec)
	rec.Record(learnrec.KindStart, replayAt, defaultStart())
	if before != nil {
		before(rec, f)
	}
	params := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, ModelPath: model, Image: "img:1"}
	states := make([]domain.VariantReplicaState, 0, len(variants))
	for _, v := range variants {
		f.store.FileScaleTarget(ns, model, v, "H100", 1, params, false, replayAt)
		states = append(states, domain.VariantReplicaState{VariantName: v, GPUsPerReplica: 1, AcceleratorName: "H100", CurrentReplicas: 1})
	}
	_, err := f.sat.Analyze(context.Background(), domain.AnalyzerInput{ModelID: model, Namespace: ns, Config: pol, VariantStates: states})
	require.NoError(t, err)
	require.NoError(t, rec.Err())
	return bytes.Clone(f.buf.Bytes())
}

// notCommitted holds an export of rec to not committing secret: refused, or
// renamed and still replaying byte for byte.
func notCommitted(t *testing.T, rec []byte, secret string) {
	t.Helper()
	require.Contains(t, string(rec), secret, "precondition: the recording holds it")
	out, _, err := Export(rec, ExportOptions{Key: []byte("k")})
	if err != nil {
		t.Logf("refused: %v", err)
		assert.NotContains(t, err.Error(), secret, "the refusal repeats the name")
		return
	}
	assert.NotContains(t, string(out), secret, "the name survives the export")
	r, err := Replay(context.Background(), out, Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.Empty(t, r.Failed())
	assert.Empty(t, r.Mismatches, "the export does not replay")
}

// A variant of another namespace that only a restored snapshot names, as a
// VariantSeenAt key.
func TestExportProbe_ARestoredSnapshotKey(t *testing.T) {
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, nil, func(_ *learnrec.Recorder, f *fixture) {
		f.sat.RestoreLearnedState(saturation.LearnedState{
			VariantSeenAt: map[string]time.Time{"payroll/llama-finance": replayAt.Add(-time.Hour), "ns/v": replayAt.Add(-time.Hour)},
		})
	})
	notCommitted(t, rec, "payroll")
}

// A namespace that only a quota limiter's NamespaceQuotas key names, and the
// limiter's own name.
func TestExportProbe_ANamespaceQuotaKey(t *testing.T) {
	pol := &config.ScalingPolicy{KvCacheThreshold: 0.8, Limiters: []config.QuotaLimiterConfig{{
		Name: "acme-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"secret-team": {"nvidia.com/gpu": 8}},
	}}}
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, pol, nil)
	notCommitted(t, rec, "secret-team")
	notCommitted(t, rec, "acme-quota")
}

// A Kueue priority class.
func TestExportProbe_APriorityClass(t *testing.T) {
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, nil, func(r *learnrec.Recorder, _ *fixture) {
		r.Record("composite.Plan", replayAt, struct{ PriorityClass string }{"payroll-critical"})
	})
	notCommitted(t, rec, "payroll-critical")
}

// A topology key outside the well-known prefixes.
func TestExportProbe_ATopologyKey(t *testing.T) {
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, nil, func(r *learnrec.Recorder, _ *fixture) {
		r.Record("composite.Plan", replayAt, struct{ TopologyKey string }{"racks.acme-corp.io/rack"})
	})
	notCommitted(t, rec, "acme-corp")
}

// An analyzer query in the policy.
func TestExportProbe_AQuery(t *testing.T) {
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, nil, func(r *learnrec.Recorder, _ *fixture) {
		r.Record("composite.Plan", replayAt, struct{ Query string }{`sum(rate(x{cluster="east-prod-7"}[1m]))`})
	})
	notCommitted(t, rec, "east-prod-7")
}

// A message with a host name and an IP address.
func TestExportProbe_AnErrorWithAHostAndAnIP(t *testing.T) {
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, nil, func(r *learnrec.Recorder, _ *fixture) {
		r.Record("composite.Plan", replayAt, struct{ Error string }{"Get https://thanos.acme-prod-east.example.com/api: dial 10.20.30.40:443 refused"})
	})
	notCommitted(t, rec, "acme-prod-east")
	notCommitted(t, rec, "10.20.30.40")
}

// A manifest source that names a namespace the recording holds.
func TestExportProbe_ASourceWithAName(t *testing.T) {
	rec := probeRecording(t, "payroll", "mdl", []string{"v"}, nil, nil)
	dir := t.TempDir()
	_, err := AddGolden(context.Background(), dir, "probe", rec, AddOptions{Source: "E1 run in payroll"})
	require.Error(t, err, "a source naming the recording's namespace was committed")
}
