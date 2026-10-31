package learnreplay

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
)

// builtStart patches a probe recording's start to build one more analyzer:
// slo or throughput. The probe records no Analyze of either.
func builtStart(t *testing.T, rec []byte, analyzer string) []byte {
	t.Helper()
	from, to := `"SLO":null`, `"SLO":{"StateTimeout":3600000000000,"SlopeHoldFor":3600000000000,"Judging":false}`
	if analyzer == "throughput" {
		from, to = `"Throughput":false`, `"Throughput":true`
	}
	out := bytes.Replace(rec, []byte(from), []byte(to), 1)
	require.NotEqual(t, rec, out, "precondition: the start now builds %s", analyzer)
	require.NotContains(t, string(out), slo.KindAnalyze, "precondition: slo never analyzed")
	require.NotContains(t, string(out), throughput.KindAnalyze, "precondition: throughput never analyzed")
	return out
}

// listing is a policy whose analyzers list saturation and, when set, one
// more.
func listing(more string) *config.ScalingPolicy {
	p := &config.ScalingPolicy{KvCacheThreshold: 0.8, Analyzers: []config.AnalyzerScoreConfig{{Name: "saturation"}}}
	if more != "" {
		p.Analyzers = append(p.Analyzers, config.AnalyzerScoreConfig{Name: more})
	}
	return p
}

// An analyzer the start built, that no recorded policy lists, and that has no
// Analyze event was built and never analyzed. The controller builds the slo
// analyzer whatever the policy lists, so a run whose models list only
// saturation has no slo results to compare. The add accepts it and pins the
// set in the manifest; the check holds the replay to it and reports it; no
// golden compares the analyzer, and its outputs may be empty. On the parent
// commit, which asked results of every analyzer the start built, the add
// refused the slo case ("would compare nothing for slo results, slo
// published"), as it refused the E1 benchmark recordings.
func TestNeverAnalyzed_ABuiltUnlistedAnalyzerIsNotRun(t *testing.T) {
	for _, analyzer := range []string{"slo", "throughput"} {
		t.Run(analyzer, func(t *testing.T) {
			ctx := context.Background()
			rec := builtStart(t, probeRecording(t, "ns", "mdl", []string{"v"}, listing(""), nil), analyzer)

			out, _, facts, err := goldenReplay(ctx, rec, Fidelity)
			require.NoError(t, err)
			require.NotEmpty(t, out)
			assert.Equal(t, []string{analyzer}, facts.notRun)

			dir := t.TempDir()
			_, err = AddGolden(ctx, dir, "x", rec, AddOptions{Source: "s"})
			require.NoError(t, err)
			m, err := ReadManifest(dir)
			require.NoError(t, err)
			require.Len(t, m.Goldens, 1)
			assert.Equal(t, []string{analyzer}, m.Goldens[0].NotRun, "the add pins the set")

			c, err := CheckGoldens(ctx, dir)
			require.NoError(t, err)
			assert.Empty(t, c.Problems)
			assert.Equal(t, 1, c.Fixtures)
			assert.Equal(t, map[string][]string{"x": {analyzer}}, c.NotRun)
			assert.Positive(t, c.Rows["x/"+OutputResults], "the saturation results are still compared")
		})
	}
}

// An analyzer a recorded policy lists, that the start built, and that has
// no Analyze event is the regression the guard is for: the controller was
// asked to run it and never did. It is refused, not read as never analyzed.
func TestNeverAnalyzed_AListedAnalyzerWithNoAnalyzeIsRefused(t *testing.T) {
	for _, analyzer := range []string{"slo", "throughput"} {
		t.Run(analyzer, func(t *testing.T) {
			ctx := context.Background()
			rec := builtStart(t, probeRecording(t, "ns", "mdl", []string{"v"}, listing(analyzer), nil), analyzer)
			use, err := readAnalyzerUse(rec)
			require.NoError(t, err)
			require.Empty(t, use.neverAnalyzed(), "a listed analyzer is never read as never analyzed")

			_, err = goldenOutputs(ctx, rec, Fidelity)
			require.ErrorContains(t, err, "the goldens would compare nothing for "+analyzer+": the policy lists it and it has 0 Analyze events")
			_, err = AddGolden(ctx, t.TempDir(), "x", rec, AddOptions{Source: "s"})
			require.ErrorContains(t, err, "the policy lists it and it has 0 Analyze events")
		})
	}
	t.Run("an analyzer listed as disabled is not listed", func(t *testing.T) {
		off := false
		pol := listing("")
		pol.Analyzers = append(pol.Analyzers, config.AnalyzerScoreConfig{Name: "slo", Enabled: &off})
		rec := builtStart(t, probeRecording(t, "ns", "mdl", []string{"v"}, pol, nil), "slo")
		use, err := readAnalyzerUse(rec)
		require.NoError(t, err)
		assert.Equal(t, []string{"slo"}, use.neverAnalyzed())
	})
}

// A head someone chose may not drop every Analyze of an analyzer the whole
// recording analyzed: cut there, it would read as never analyzed, and no
// golden would compare it.
func TestAddGolden_AHeadThatDropsAnAnalyzerIsRefused(t *testing.T) {
	acc := fixtureRecording(t)
	lines := strings.SplitAfter(string(acc), "\n")
	first := -1
	for i, l := range lines {
		if strings.Contains(l, `"Kind":"`+slo.KindAnalyze+`"`) {
			first = i
			break
		}
	}
	require.Positive(t, first, "precondition: the acceptance run has an slo Analyze, not on its first line")
	use, err := readAnalyzerUse(acc)
	require.NoError(t, err)

	err = headKeepsAnalyzers(acc, first)
	require.ErrorContains(t, err, "drops every slo Analyze; the full recording has")
	assert.Contains(t, err.Error(), "has "+strconv.Itoa(use.analyzed("slo")))

	dir := t.TempDir()
	_, err = AddGolden(context.Background(), dir, "x", acc, AddOptions{Source: "s", HeadLines: first})
	require.ErrorContains(t, err, "drops every slo Analyze")
	_, statErr := os.Stat(filepath.Join(dir, "x"))
	assert.True(t, os.IsNotExist(statErr), "a refusal writes nothing")

	// A head that keeps one Analyze of each analyzer the recording analyzed
	// (the acceptance run has slo and throughput) is not refused.
	last := first
	for i, l := range lines {
		if strings.Contains(l, `"Kind":"`+throughput.KindAnalyze+`"`) {
			last = max(last, i)
			break
		}
	}
	require.NoError(t, headKeepsAnalyzers(acc, last+1), "a head that keeps one Analyze of each keeps them")
}

// Every analyzer output of a committed fixture compares rows, but for the
// outputs its recording allows to be empty, each by a named rule
// (emptyRules): slo-published.csv where slo was never analyzed, lines.csv
// where no line was published or borrowed, learned-state.csv where the run
// took no snapshot. An output emptied any other way is a check problem.
func TestGoldens_AnEmptyOutputNeedsARule(t *testing.T) {
	dir := copyGoldens(t)
	f := filepath.Join(dir, "acceptance", OutputLearnedState)
	empty, err := writeRows(nil)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f, empty, 0o644))
	c, err := CheckGoldens(context.Background(), dir)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(c.Problems, "\n"), "acceptance: acceptance/learned-state.csv holds no row, and nothing in the recording allows it to be empty")
	assert.Empty(t, c.MayBeEmpty["acceptance"][OutputLearnedState], "the acceptance run took snapshots")
}

// A fixture whose replay moves into or out of the never-analyzed set no
// longer matches its manifest: a problem, so the change is a diff of
// goldens.yaml.
func TestGoldens_TheNotRunSetIsPinned(t *testing.T) {
	ctx := context.Background()
	rec := builtStart(t, probeRecording(t, "ns", "mdl", []string{"v"}, listing(""), nil), "slo")
	dir := t.TempDir()
	_, err := AddGolden(ctx, dir, "x", rec, AddOptions{Source: "s"})
	require.NoError(t, err)
	m, err := ReadManifest(dir)
	require.NoError(t, err)
	m.Goldens[0].NotRun = nil
	require.NoError(t, writeManifest(dir, m))
	c, err := CheckGoldens(ctx, dir)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(c.Problems, "\n"), "x: the analyzers built and never analyzed are [slo], and goldens.yaml says []")

	require.NoError(t, UpdateGoldens(ctx, dir), "update writes the set back")
	c, err = CheckGoldens(ctx, dir)
	require.NoError(t, err)
	assert.Empty(t, c.Problems)
}
