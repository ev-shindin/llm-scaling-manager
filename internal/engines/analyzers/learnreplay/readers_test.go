package learnreplay

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The coverage test trusts its buckets: a reader or a builder records
// nothing because it changes nothing a later call reads. This holds them to
// it. Two fixtures run the same cycles; one also calls the method; the
// method writes no event, and every event after it is the same on both.

var readersAt = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

func readersInput() domain.AnalyzerInput {
	rows := make([]domain.ReplicaMetrics, 0, 4)
	for i, k := range []float64{0.3, 0.45, 0.6, 0.75} {
		rows = append(rows, domain.ReplicaMetrics{PodName: string(rune('a' + i)), VariantName: "v", Namespace: "ns",
			ModelID: "m", Ready: true, ReadySince: readersAt.Add(-time.Hour), KvCacheUsage: k, KvUsageInstant: k,
			TotalKvCapacityTokens: 10_000, NumGpuBlocks: 625, BlockSize: 16, TokensInUse: int64(k * 10_000),
			AvgITL: 0.006 + 0.02*k, AvgITLKnown: true, AvgInputTokens: 500, AvgOutputTokens: 200,
			RequestRate: 1, GenerationTokenRate: 100, RunningRequests: 8})
	}
	return domain.AnalyzerInput{ModelID: "m", Namespace: "ns", ReplicaMetrics: rows,
		Config: &config.ScalingPolicy{KvCacheThreshold: 0.8,
			SLO: &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: 50 * time.Millisecond}}}},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 4, GPUsPerReplica: 1,
			AcceleratorName: "H100", Role: domain.RoleDecode}}}
}

// cycle is one round of every recorded call that reads learned state.
func (f *fixture) cycle(t *testing.T, n int) {
	t.Helper()
	at := readersAt.Add(time.Duration(n) * 15 * time.Second)
	f.clk.SetTime(at)
	ctx := context.Background()
	f.store.FileScaleTarget("ns", "m", "v", "H100", 1,
		capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64}, false, at)
	f.sat.NoteVariantsPresent("ns", "m", []string{"v"})
	f.slo.NoteVariantsPresent("ns", "m", []string{"v"})
	f.sat.EvictStaleHistory()
	f.slo.EvictStale(at)
	f.store.EvictStale(at, time.Hour)
	in := readersInput()
	for _, a := range []interface {
		Analyze(context.Context, domain.AnalyzerInput) (*domain.AnalyzerResult, error)
	}{f.sat, f.slo, f.tp} {
		_, err := a.Analyze(ctx, in)
		require.NoError(t, err)
	}
	f.sat.ExportLearnedState()
}

func TestReadersAndBuildersChangeNothing(t *testing.T) {
	params := &capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64}
	calls := map[string]func(*fixture){
		"saturation.Name":                     func(f *fixture) { f.sat.Name() },
		"saturation.Horizons":                 func(f *fixture) { f.sat.Horizons() },
		"saturation.BorrowTickGap":            func(f *fixture) { f.sat.BorrowTickGap() },
		"saturation.WithHorizons":             func(f *fixture) { f.sat.WithHorizons(f.sat.Horizons()) },
		"saturation.WithOptimizationInterval": func(f *fixture) { f.sat.WithOptimizationInterval(0) },
		"saturation.WithClock":                func(f *fixture) { f.sat.WithClock(f.clk) },
		"slo.Name":                            func(f *fixture) { f.slo.Name() },
		"slo.StateTimeout":                    func(f *fixture) { f.slo.StateTimeout() },
		"slo.SlopeHoldFor":                    func(f *fixture) { f.slo.SlopeHoldFor() },
		"slo.Judging":                         func(f *fixture) { f.slo.Judging() },
		"slo.WithClock":                       func(f *fixture) { f.slo.WithClock(f.clk) },
		"slo.WithStateTimeout":                func(f *fixture) { f.slo.WithStateTimeout(f.slo.StateTimeout()) },
		"slo.WithSlopeHoldFor":                func(f *fixture) { f.slo.WithSlopeHoldFor(f.slo.SlopeHoldFor()) },
		"throughput.Name":                     func(f *fixture) { f.tp.Name() },
		"throughput.VariantState":             func(f *fixture) { f.tp.VariantState("m", "ns", "v") },
		"throughput.WithClock":                func(f *fixture) { f.tp.WithClock(f.clk) },
		"store.Get":                           func(f *fixture) { f.store.Get("ns", "m", "v") },
		"store.IsStale":                       func(f *fixture) { f.store.IsStale("ns", "m", "v", readersAt) },
		"store.FindCompatible":                func(f *fixture) { f.store.FindCompatible("m", "H100", 1, params) },
		"store.FindClampBound":                func(f *fixture) { f.store.FindClampBound("ns", "m", "H100", 1, params) },
		"store.Len":                           func(f *fixture) { f.store.Len() },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			with, without := newFixture(readersAt), newFixture(readersAt)
			for n := range 3 {
				with.cycle(t, n)
				without.cycle(t, n)
			}
			require.Equal(t, with.buf.String(), without.buf.String(), "precondition: the same cycles")
			before := with.buf.Len()
			call(with)
			assert.Equal(t, before, with.buf.Len(), "%s wrote an event", name)
			for n := 3; n < 6; n++ {
				with.cycle(t, n)
				without.cycle(t, n)
			}
			assert.Equal(t, without.buf.String(), with.buf.String(), "%s changed what a later call does", name)
		})
	}
}

// Update and Delete are not recorded because only the saturation analyzer
// calls them, and replaying it replays them. This holds the tree to that: no
// file outside the saturation and capacity packages calls Update or Delete
// on a value it names as a store. The receiver is matched by name, not by
// type, which is what the codebase's naming makes reliable.
func TestStoreUpdateAndDeleteAreCalledOnlyByTheSaturationAnalyzer(t *testing.T) {
	root := moduleRoot(t)
	var callers []string
	files := 0
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if strings.HasPrefix(rel, "internal/engines/analyzers/saturation/") || strings.HasPrefix(rel, "internal/signals/capacity/") {
			return nil
		}
		src, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		imports := false
		for _, im := range src.Imports {
			if strings.HasSuffix(strings.Trim(im.Path.Value, `"`), "/internal/signals/capacity") {
				imports = true
			}
		}
		if !imports {
			return nil
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Update" && sel.Sel.Name != "Delete") {
				return true
			}
			name := ""
			switch x := sel.X.(type) {
			case *ast.Ident:
				name = x.Name
			case *ast.SelectorExpr:
				name = x.Sel.Name
			}
			if strings.HasSuffix(strings.ToLower(name), "store") {
				callers = append(callers, rel+": "+name+"."+sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	require.Positive(t, files, "no file outside the two packages imports capacity; the walk read nothing")
	assert.Empty(t, callers, "a store is written outside the saturation analyzer; record it or move it")
	t.Logf("checked %d files that import capacity", files)
}
