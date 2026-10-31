package learnreplay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/enginerec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/steadystate"
)

// engineFixture is the committed golden that carries the engine layer.
const engineFixture = "engine"

// The engine golden (issue #345, step 0) compares every engine output, and
// each compares rows: a file that compared nothing would pass any change.
func TestEngineGoldens_EveryEngineOutputComparesRows(t *testing.T) {
	m, err := ReadManifest(goldenDir)
	require.NoError(t, err)
	c, err := CheckGoldens(context.Background(), goldenDir)
	require.NoError(t, err)
	require.Empty(t, c.Problems)
	var listed []string
	for _, g := range m.Goldens {
		if g.Name != engineFixture {
			for _, o := range g.Outputs {
				assert.NotContains(t, EngineOutputNames(), filepath.Base(o),
					"%s carries no engine events and lists an engine output", g.Name)
			}
			continue
		}
		listed = g.Outputs
	}
	require.NotEmpty(t, listed, "precondition: the manifest lists the engine fixture")
	for _, o := range EngineOutputNames() {
		f := engineFixture + "/" + o
		assert.Contains(t, listed, f)
		assert.Positive(t, c.Rows[f], "%s compared no row", f)
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

// Negative control: without the engine fixture, no golden runs an engine
// path, and the check names each.
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
	require.Len(t, c.Uncovered, len(steadystate.LearnerReplayPathNames()))
	for _, p := range steadystate.LearnerReplayPathNames() {
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
