package learnreplay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// Each golden that carries engine events lists every engine output, and no
// other lists one. Each compares rows in each, but for an output its replay
// allows to be empty (engineEmptyRules); the engine golden (issue #345,
// step 0) allows none: a file that compared nothing would pass any change.
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
			if c.mayBeEmpty[g.Name][o] && c.Rows[f] == 0 {
				assert.NotEqual(t, engineFixture, g.Name, "the engine fixture fills %s", o)
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

// The engine's path names engineEmptyRules reads are the engine's own.
func TestEngineGoldens_TheEmptyRulesReadTheEnginesPathNames(t *testing.T) {
	assert.Equal(t, steadystate.PathCompositeHold, enginePathHold)
	assert.Equal(t, steadystate.PathCompositeReleaseBound, enginePathReleaseBound)
	assert.Contains(t, enginePathNames(), enginePathHold)
	assert.Contains(t, enginePathNames(), enginePathReleaseBound)
}

// An engine output a rule may allow to be empty (engineEmptyRuled) must hold
// rows unless the fixture's replay allows it. Refuse side: the engine
// fixture holds roles and bounds releases, so each emptied is a problem.
// Accept side: pd-restart's replay does neither, and its two files are empty
// by the rule, not by its name.
func TestEngineGoldens_AnEmptyEngineOutputNeedsARule(t *testing.T) {
	ctx := context.Background()
	empty, err := writeRows(nil)
	require.NoError(t, err)
	for _, output := range engineEmptyRuled {
		t.Run("refused: "+output, func(t *testing.T) {
			dir := copyGoldens(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, engineFixture, output), empty, 0o644))
			c, err := CheckGoldens(ctx, dir)
			require.NoError(t, err)
			assert.False(t, c.mayBeEmpty[engineFixture][output])
			assert.Contains(t, strings.Join(c.Problems, "\n"),
				engineFixture+": "+engineFixture+"/"+output+" holds no row, and nothing in the recording allows it to be empty")
		})
	}
	t.Run("accepted: pd-restart held no role and bound no release", func(t *testing.T) {
		c, err := CheckGoldens(ctx, goldenDir)
		require.NoError(t, err)
		require.Empty(t, c.Problems)
		for _, output := range engineEmptyRuled {
			assert.Zero(t, c.Rows["pd-restart/"+output], "precondition: %s is empty", output)
			assert.True(t, c.mayBeEmpty["pd-restart"][output], "%s is allowed empty", output)
		}
	})
}

// pd-restart pins the real restart: its own replay reaches the settle
// tracker's restart verdict, which the summed path check cannot tell from
// the in-process generators' restarts. Negative control: the same recording
// cut before the decode engine was killed reaches none.
func TestGoldens_PDRestartReplayRestartsForReal(t *testing.T) {
	gz, err := os.ReadFile(filepath.Join(goldenDir, "pd-restart", RecordingName))
	require.NoError(t, err)
	rec, err := gunzip(gz)
	require.NoError(t, err)
	restarted := settlePathPrefix + string(settle.Restarted)

	rep, err := Replay(context.Background(), rec, Options{Mode: Normal})
	require.NoError(t, err)
	require.Empty(t, rep.Failed())
	assert.Positive(t, rep.Paths[restarted], "pd-restart's own replay reaches %s", restarted)

	// The engine was killed at 12:48:54Z and Ready again at 12:49:46Z; cut at
	// the first cycle that begins after 12:48:00Z.
	cut := time.Date(2026, 11, 1, 12, 48, 0, 0, time.UTC)
	lines := strings.SplitAfter(string(rec), "\n")
	n := 0
	for i, l := range lines {
		var ev struct {
			Now  time.Time
			Kind string
		}
		if l == "" {
			continue
		}
		require.NoError(t, json.Unmarshal([]byte(l), &ev))
		if ev.Kind == "slo.BeginCycle" && !ev.Now.Before(cut) {
			n = i
			break
		}
	}
	require.Positive(t, n, "precondition: the recording runs past the cut")
	head, err := Replay(context.Background(), []byte(strings.Join(lines[:n], "")), Options{Mode: Normal})
	require.NoError(t, err)
	require.Empty(t, head.Failed())
	assert.Zero(t, head.Paths[restarted], "before the kill, no restart")
	assert.Positive(t, head.Paths[settlePathPrefix+string(settle.Joined)], "precondition: the head replays, joins included")
}
