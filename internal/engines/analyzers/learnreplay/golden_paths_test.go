package learnreplay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
)

// The per-path counters (docs/proposals/analyzer-learner.md, 2.5): the
// committed goldens run every learner path between them, and the check fails
// when one runs on no golden.
func TestGoldens_EveryLearnerPathRuns(t *testing.T) {
	c, err := CheckGoldens(context.Background(), goldenDir)
	require.NoError(t, err)
	require.Empty(t, c.Problems)
	require.Empty(t, c.Uncovered)
	for _, p := range requiredPaths() {
		assert.Positive(t, c.Paths[p], "no golden runs %s", p)
	}
	t.Logf("learner paths over %d fixtures: %v", c.Fixtures, c.Paths)
}

// Negative control: without the mu-paths golden, the others leave some of
// k2's and mu's measured paths unrun, and the check names each.
func TestGoldens_APathNoGoldenRunsIsAProblem(t *testing.T) {
	dir := copyGoldens(t)
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "mu-paths")))
	m, err := ReadManifest(dir)
	require.NoError(t, err)
	kept := m.Goldens[:0]
	for _, g := range m.Goldens {
		if g.Name != "mu-paths" {
			kept = append(kept, g)
		}
	}
	require.Len(t, kept, len(m.Goldens)-1, "precondition: the manifest lists mu-paths")
	m.Goldens = kept
	require.NoError(t, writeManifest(dir, m))

	c, err := CheckGoldens(context.Background(), dir)
	require.NoError(t, err)
	require.Empty(t, c.Problems, "the other goldens still reproduce")
	got := strings.Join(c.Uncovered, "\n")
	// The paths named are exactly those the other goldens ran on no cycle,
	// by their own counts.
	var zero []string
	for _, p := range saturation.PathNames() {
		if c.Paths[p] == 0 {
			zero = append(zero, p)
		}
	}
	require.NotEmpty(t, zero, "precondition: the other goldens leave a path uncovered")
	require.Len(t, c.Uncovered, len(zero), got)
	for _, p := range zero {
		assert.Contains(t, got, "no golden runs the learner path "+p)
	}
	t.Log(got)
}

// Each counter's zero-check: with every path counted but one, the check
// names exactly that one.
func TestGoldens_EachPathAtZeroIsNamedAlone(t *testing.T) {
	for _, zero := range requiredPaths() {
		paths := map[string]int64{}
		for _, p := range requiredPaths() {
			paths[p] = 1
		}
		paths[zero] = 0
		got := pathProblems(paths)
		if assert.Len(t, got, 1, "%s at zero", zero) {
			assert.Contains(t, got[0], "learner path "+zero+":", "%s at zero", zero)
		}
	}
	assert.Len(t, pathProblems(map[string]int64{}), len(requiredPaths()))
}
