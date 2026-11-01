package learnreplay

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/enginerec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/steadystate"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
)

// engineFixture is the in-process golden built to reach every engine path.
const engineFixture = "engine"

// engineFixtures is every committed golden whose recording carries engine
// events: the in-process engine run, and a cluster recording made after the
// engine layer landed. The others were recorded before it.
var engineFixtures = []string{engineFixture, "pd-restart"}

// engineMayBeEmpty is the engine outputs a cluster recording may leave
// without a row: a run in which no role was held, and no other analyzer's
// frame bounded a release. The engine fixture holds rows in every one.
var engineMayBeEmpty = []string{OutputCompositeHolds, OutputCompositeRelease}

// Each golden that carries engine events lists every engine output, and no
// other lists one. The engine golden (issue #345, step 0) compares rows in
// each: a file that compared nothing would pass any change.
func TestEngineGoldens_EveryEngineOutputComparesRows(t *testing.T) {
	m, err := ReadManifest(goldenDir)
	require.NoError(t, err)
	c, err := CheckGoldens(context.Background(), goldenDir)
	require.NoError(t, err)
	require.Empty(t, c.Problems)
	seen := map[string]bool{}
	for _, g := range m.Goldens {
		if !slices.Contains(engineFixtures, g.Name) {
			for _, o := range g.Outputs {
				assert.NotContains(t, EngineOutputNames(), filepath.Base(o),
					"%s carries no engine events and lists an engine output", g.Name)
			}
			continue
		}
		seen[g.Name] = true
		for _, o := range EngineOutputNames() {
			f := g.Name + "/" + o
			assert.Contains(t, g.Outputs, f)
			if g.Name != engineFixture && slices.Contains(engineMayBeEmpty, o) {
				continue
			}
			assert.Positive(t, c.Rows[f], "%s compared no row", f)
		}
	}
	for _, name := range engineFixtures {
		require.True(t, seen[name], "precondition: the manifest lists %s", name)
	}
	for _, p := range steadystate.LearnerReplayPathNames() {
		assert.Positive(t, c.Paths[p], "no golden runs the engine path %s", p)
	}
}

// Negative control: an engine golden that no longer matches its replay is a
// problem, named by its file, for each engine output.
func TestEngineGoldens_AnEditedEngineGoldenFails(t *testing.T) {
	for _, o := range EngineOutputNames() {
		t.Run(o, func(t *testing.T) {
			dir := copyGoldens(t)
			p := filepath.Join(dir, engineFixture, o)
			b, err := os.ReadFile(p)
			require.NoError(t, err)
			lines := strings.SplitAfter(string(b), "\n")
			require.Greater(t, len(lines), 2, "precondition: %s has rows", o)
			// Drop the first row.
			require.NoError(t, os.WriteFile(p, []byte(strings.Join(append(lines[:1], lines[2:]...), "")), 0o644))
			got := strings.Join(checkProblems(t, dir), "\n")
			assert.Contains(t, got, "golden no longer reproduces: "+engineFixture+"/"+o)
		})
	}
}

// Negative control: without the engine fixture, the check names each engine
// path no other golden runs. pd-restart still builds composites, prices P/D
// roles and pushes past the scale-up threshold; it holds no role, binds no
// release, reads no external analyzer, has none in grace, clamps no replica
// and lends no bridge. The engine fixture is also the only one with a
// warm-pool bridge, so the settle tracker's lend verdict goes with it.
func TestEngineGoldens_WithoutTheEngineFixtureNoEnginePathRuns(t *testing.T) {
	dir := copyGoldens(t)
	require.NoError(t, os.RemoveAll(filepath.Join(dir, engineFixture)))
	m, err := ReadManifest(dir)
	require.NoError(t, err)
	kept := m.Goldens[:0]
	for _, g := range m.Goldens {
		if g.Name != engineFixture {
			kept = append(kept, g)
		}
	}
	m.Goldens = kept
	require.NoError(t, writeManifest(dir, m))
	c, err := CheckGoldens(context.Background(), dir)
	require.NoError(t, err)
	require.Empty(t, c.Problems, "the analyzer goldens alone still reproduce")
	// Pinned: an engine path a cluster golden starts or stops reaching moves
	// this list, a reviewed change.
	want := []string{steadystate.PathCompositeHold, steadystate.PathCompositeReleaseBound,
		steadystate.PathCompositeExternalResult, steadystate.PathCompositeGrace,
		steadystate.PathCompositeReplicaClamped, steadystate.PathWarmPoolSupply,
		settlePathPrefix + string(settle.Lent)}
	require.Len(t, c.Uncovered, len(want), strings.Join(c.Uncovered, "\n"))
	for _, p := range want {
		assert.Contains(t, strings.Join(c.Uncovered, "\n"), "learner path "+p+":")
	}
}

// A recording that carries engine events is not a golden without the
// engine's replay: the check says so rather than comparing the analyzer
// outputs alone and passing.
func TestEngineGoldens_NoEngineRegisteredIsAProblem(t *testing.T) {
	RegisterEngine(nil)
	t.Cleanup(func() {
		RegisterEngine(func() EngineReplayer { return steadystate.NewLearnerReplay() })
	})
	got := strings.Join(checkProblems(t, goldenDir), "\n")
	assert.Contains(t, got, engineFixture+": the recording carries engine events ("+enginerec.KindComposite+
		") and no engine replay is registered")
}
