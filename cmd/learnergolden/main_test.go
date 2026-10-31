package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/learnreplay"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/steadystate"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// goldens is the committed golden directory, from this package.
var goldens = filepath.Join("..", "..", filepath.FromSlash(learnreplay.GoldenDir))

// rawRecording writes the committed acceptance recording, unzipped, to a
// file: an export exports again as it is, so it stands in for a raw one.
func rawRecording(t *testing.T) string {
	t.Helper()
	gz, err := os.Open(filepath.Join(goldens, "acceptance", learnreplay.RecordingName))
	require.NoError(t, err)
	defer func() { _ = gz.Close() }()
	r, err := gzip.NewReader(gz)
	require.NoError(t, err)
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), "raw.jsonl")
	require.NoError(t, os.WriteFile(p, b, 0o644))
	return p
}

// headRecording writes a raw recording made at this commit to a file: a
// start and six cycles of one model, its variant filed in the capacity store
// and analysed by the saturation analyzer. A test that adds a recording uses
// it rather than the committed one, whose Fidelity replay at a later commit
// writes the input fields added since and so no longer matches it byte for
// byte (#400).
func headRecording(t *testing.T) string {
	t.Helper()
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clk := clocktesting.NewFakePassiveClock(at)
	store := capacity.NewStore()
	sat := saturation.NewSaturationAnalyzer(store).WithClock(clk)
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	store.WithRecorder(rec)
	sat.WithRecorder(rec)
	h := sat.Horizons()
	rec.Record(learnrec.KindStart, at, learnrec.Start{Saturation: learnrec.SaturationStart{EpisodeGap: h.EpisodeGap,
		VariantTimeout: h.VariantTimeout, BucketRetention: h.BucketRetention, BorrowTickGap: sat.BorrowTickGap()}})
	const ns, model, variant = "team-q", "org/model-q", "var-q"
	params := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, Image: "img:1"}
	for n := range 6 {
		now := at.Add(time.Duration(n) * 15 * time.Second)
		clk.SetTime(now)
		store.FileScaleTarget(ns, model, variant, "H100", 1, params, false, now)
		rows := make([]domain.ReplicaMetrics, 0, 4)
		for i, k := range []float64{0.3, 0.45, 0.6, 0.75} {
			rows = append(rows, domain.ReplicaMetrics{PodName: fmt.Sprintf("%s-%d", variant, i), VariantName: variant,
				Namespace: ns, ModelID: model, Ready: true, ReadySince: at.Add(-time.Hour), KvCacheUsage: k,
				KvUsageInstant: k, TotalKvCapacityTokens: 10_000, NumGpuBlocks: 625, BlockSize: 16,
				TokensInUse: int64(k * 10_000), AvgInputTokens: 500, AvgOutputTokens: 200, RunningRequests: 8})
		}
		_, err := sat.Analyze(context.Background(), domain.AnalyzerInput{ModelID: model, Namespace: ns,
			ReplicaMetrics: rows, Config: &config.ScalingPolicy{KvCacheThreshold: 0.8},
			VariantStates: []domain.VariantReplicaState{{VariantName: variant, CurrentReplicas: len(rows),
				GPUsPerReplica: 1, AcceleratorName: "H100"}}})
		require.NoError(t, err)
	}
	require.NoError(t, rec.Err())
	p := filepath.Join(t.TempDir(), "head.jsonl")
	require.NoError(t, os.WriteFile(p, buf.Bytes(), 0o644))
	return p
}

func runOut(args ...string) (string, error) {
	var out bytes.Buffer
	err := run(context.Background(), args, &out)
	return out.String(), err
}

func TestRun_ArgumentErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "usage: learnergolden"},
		{[]string{"nope"}, `unknown command "nope"`},
		{[]string{"export"}, "export needs -in and -out"},
		{[]string{"add", "-in", "x"}, "add needs -in, -name and -source"},
		{[]string{"check", "-bogus"}, "flag provided but not defined"},
	} {
		_, err := runOut(tc.args...)
		require.Error(t, err, "%v", tc.args)
		assert.Contains(t, err.Error(), tc.want, "%v", tc.args)
	}
}

func TestRun_HelpIsNotAnError(t *testing.T) {
	for _, cmd := range []string{"export", "add", "update", "check"} {
		out, err := runOut(cmd, "-h")
		require.NoError(t, err, cmd)
		assert.Contains(t, out, "Usage of learnergolden "+cmd, cmd)
	}
	out, _ := runOut("export", "-h")
	assert.NotContains(t, out, "-dir", "export takes no golden directory")
}

func TestRun_ExportPlainAndGzipped(t *testing.T) {
	in := rawRecording(t)
	dir := t.TempDir()
	out, err := runOut("export", "-in", in, "-out", filepath.Join(dir, "x.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, out, "kept as they are")
	plain, err := os.ReadFile(filepath.Join(dir, "x.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, byte('{'), plain[0])

	_, err = runOut("export", "-in", in, "-out", filepath.Join(dir, "x.jsonl.gz"))
	require.NoError(t, err)
	gz, err := os.ReadFile(filepath.Join(dir, "x.jsonl.gz"))
	require.NoError(t, err)
	assert.Equal(t, []byte{0x1f, 0x8b}, gz[:2])
}

// add writes a fixture and runs the check; the check then fails on an
// edited golden, and update repairs it.
func TestRun_AddCheckUpdate(t *testing.T) {
	in := headRecording(t)
	dir := filepath.Join(t.TempDir(), "goldens")
	out, err := runOut("add", "-dir", dir, "-in", in, "-name", "e1", "-source", "a test run")
	require.NoError(t, err, out)
	assert.Contains(t, out, "golden check: 1 fixtures replayed")

	p := filepath.Join(dir, "e1", learnreplay.OutputResults)
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	lines := strings.SplitAfter(string(b), "\n")
	require.NoError(t, os.WriteFile(p, []byte(strings.Join(append(lines[:1], lines[2:]...), "")), 0o644))
	out, err = runOut("check", "-dir", dir)
	require.ErrorContains(t, err, "1 golden problems")
	assert.Contains(t, out, "golden no longer reproduces: e1/results.csv")

	out, err = runOut("update", "-dir", dir)
	require.NoError(t, err, out)
	assert.Contains(t, out, "rewrote the goldens")

	// The one golden reproduces again, and reaches none of mu's measured
	// paths: an add and an update say so, the check fails on it.
	assert.Contains(t, out, "no golden runs the learner path "+saturation.PathMuSampleWritten)
	// Each of saturation's paths the recording's own replay never ran, and
	// every engine path: the recording carries no engine events.
	raw, err := os.ReadFile(in)
	require.NoError(t, err)
	rep, err := learnreplay.Replay(context.Background(), raw, learnreplay.Options{Mode: learnreplay.Normal})
	require.NoError(t, err)
	unrun := 0
	for _, p := range saturation.PathNames() {
		if rep.Paths[p] == 0 {
			unrun++
		}
	}
	// Pinned, so a change in what the replay counts is a reviewed change; the
	// replay's own count above is only the cross-check.
	require.Equal(t, 8, unrun, "the saturation paths the head recording leaves unrun")
	out, err = runOut("check", "-dir", dir)
	require.ErrorContains(t, err, fmt.Sprintf("%d learner paths no golden runs", unrun+len(steadystate.LearnerReplayPathNames())), out)
}
