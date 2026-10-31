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
	if analyzer == analyzerThroughput {
		from, to = `"Throughput":false`, `"Throughput":true`
	}
	out := bytes.Replace(rec, []byte(from), []byte(to), 1)
	require.NotEqual(t, rec, out, "precondition: the start now builds %s", analyzer)
	require.NotContains(t, string(out), slo.KindAnalyze, "precondition: slo never analyzed")
	require.NotContains(t, string(out), throughput.KindAnalyze, "precondition: throughput never analyzed")
	return out
}

// listing is a policy whose analyzers list saturation and the entries
// given.
func listing(more ...config.AnalyzerScoreConfig) *config.ScalingPolicy {
	return &config.ScalingPolicy{KvCacheThreshold: 0.8,
		Analyzers: append([]config.AnalyzerScoreConfig{{Name: "saturation"}}, more...)}
}

// named is a policy entry listed by name alone.
func named(name string) config.AnalyzerScoreConfig { return config.AnalyzerScoreConfig{Name: name} }

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
	for _, analyzer := range []string{analyzerSLO, analyzerThroughput} {
		t.Run(analyzer, func(t *testing.T) {
			ctx := context.Background()
			rec := builtStart(t, probeRecording(t, "ns", "mdl", []string{"v"}, listing(), nil), analyzer)

			r, err := goldenReplay(ctx, rec, Fidelity)
			require.NoError(t, err)
			require.NotEmpty(t, r.outputs)
			assert.Equal(t, []string{analyzer}, r.facts.notRun)

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
// A policy entry lists the analyzer by its effective type: its type when
// set, else its name (config.AnalyzerScoreConfig.EffectiveType).
func TestNeverAnalyzed_AListedAnalyzerWithNoAnalyzeIsRefused(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name    string
		built   string
		entry   config.AnalyzerScoreConfig
		refused bool
		notRun  []string
	}{
		{name: "slo by name", built: analyzerSLO, entry: named(analyzerSLO), refused: true},
		{name: "throughput by name", built: analyzerThroughput, entry: named(analyzerThroughput), refused: true},
		{name: "slo by type, under another name", built: analyzerSLO,
			entry: config.AnalyzerScoreConfig{Name: "my-latency", Type: analyzerSLO}, refused: true},
		{name: "the name slo with another type does not list slo", built: analyzerSLO,
			entry: config.AnalyzerScoreConfig{Name: analyzerSLO, Type: "saturation"}, notRun: []string{analyzerSLO}},
		{name: "an entry with enabled false does not list it", built: analyzerSLO,
			entry: config.AnalyzerScoreConfig{Name: analyzerSLO, Enabled: &off}, notRun: []string{analyzerSLO}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rec := builtStart(t, probeRecording(t, "ns", "mdl", []string{"v"}, listing(tc.entry), nil), tc.built)
			use, err := readAnalyzerUse(rec, 0)
			require.NoError(t, err)
			assert.Equal(t, tc.notRun, use.neverAnalyzed())

			_, err = goldenOutputs(ctx, rec, Fidelity)
			if !tc.refused {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "the goldens would compare nothing for "+tc.built+": the policy lists it and it has 0 Analyze events")
			_, err = AddGolden(ctx, t.TempDir(), "x", rec, AddOptions{Source: "s"})
			require.ErrorContains(t, err, "the policy lists it and it has 0 Analyze events")
		})
	}
}

// An analyzer a policy lists and the start did not build is no analyzer of
// the run: nothing is asked of it, as before the never-analyzed rule.
func TestNeverAnalyzed_AListedAnalyzerTheStartDidNotBuildIsAccepted(t *testing.T) {
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, listing(named(analyzerSLO)), nil)
	require.Contains(t, string(rec), `"SLO":null`, "precondition: the start did not build slo")
	use, err := readAnalyzerUse(rec, 0)
	require.NoError(t, err)
	assert.Empty(t, use.neverAnalyzed())
	_, err = goldenOutputs(context.Background(), rec, Fidelity)
	require.NoError(t, err)
}

// A recording that does not open with a start line is refused by name.
func TestReadAnalyzerUse_NeedsAStartLine(t *testing.T) {
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, nil, nil)
	_, rest, ok := bytes.Cut(rec, []byte("\n"))
	require.True(t, ok)
	_, err := readAnalyzerUse(nil, 0)
	require.ErrorContains(t, err, "does not open with a start line")
	_, err = readAnalyzerUse(rest, 0)
	require.Error(t, err, "a recording without its start line does not read")
}

// firstLine is the index of the first line of acc whose kind is kind.
func firstLine(t *testing.T, lines []string, kind string) int {
	t.Helper()
	for i, l := range lines {
		if strings.Contains(l, `"Kind":"`+kind+`"`) {
			return i
		}
	}
	t.Fatalf("no %s line", kind)
	return -1
}

// A head someone chose may not drop every Analyze of an analyzer the whole
// recording analyzed: cut there, it would read as never analyzed, and no
// golden would compare it. Each optional analyzer is held to it.
func TestAddGolden_AHeadThatDropsAnAnalyzerIsRefused(t *testing.T) {
	acc := fixtureRecording(t)
	lines := strings.SplitAfter(string(acc), "\n")
	firstSLO, firstThroughput := firstLine(t, lines, slo.KindAnalyze), firstLine(t, lines, throughput.KindAnalyze)
	require.Less(t, firstSLO, firstThroughput, "precondition: the acceptance run analyzes slo before throughput")
	use, err := readAnalyzerUse(acc, 0)
	require.NoError(t, err)

	for _, tc := range []struct {
		analyzer string
		head     int
	}{
		{analyzerSLO, firstSLO},
		{analyzerThroughput, firstThroughput},
	} {
		t.Run(tc.analyzer, func(t *testing.T) {
			err := headKeepsAnalyzers(acc, tc.head)
			require.ErrorContains(t, err, "drops every "+tc.analyzer+" Analyze; the full recording has "+strconv.Itoa(use.analyzeCount(tc.analyzer)))

			dir := t.TempDir()
			_, err = AddGolden(context.Background(), dir, "x", acc, AddOptions{Source: "s", HeadLines: tc.head})
			require.ErrorContains(t, err, "drops every "+tc.analyzer+" Analyze")
			_, statErr := os.Stat(filepath.Join(dir, "x"))
			assert.True(t, os.IsNotExist(statErr), "a refusal writes nothing")
		})
	}
	// A head that keeps one Analyze of each analyzer the recording analyzed
	// is not refused.
	require.NoError(t, headKeepsAnalyzers(acc, firstThroughput+1))
}

// An output a rule may allow to be empty (emptyRuled) must hold rows unless
// the fixture's recording allows it. Refuse side: on the acceptance fixture,
// whose recording publishes lines, analyzes and publishes slo, and exports
// learned state, each output emptied is a problem. Accept side: a probe
// recording that does none of those leaves each empty, and the check is
// clean.
func TestGoldens_AnEmptyOutputNeedsARule(t *testing.T) {
	ctx := context.Background()
	acc, err := readAnalyzerUse(fixtureRecording(t), 0)
	require.NoError(t, err)
	require.Positive(t, acc.lines, "precondition: the acceptance run published a line")
	require.Positive(t, acc.exports, "precondition: the acceptance run exported learned state")
	require.Positive(t, acc.analyzeCount(analyzerSLO), "precondition: the acceptance run analyzed slo")
	empty, err := writeRows(nil)
	require.NoError(t, err)

	for _, output := range emptyRuled {
		t.Run("refused: "+output, func(t *testing.T) {
			dir := copyGoldens(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "acceptance", output), empty, 0o644))
			c, err := CheckGoldens(ctx, dir)
			require.NoError(t, err)
			assert.False(t, c.mayBeEmpty["acceptance"][output])
			assert.Contains(t, strings.Join(c.Problems, "\n"), "acceptance: acceptance/"+output+" holds no row, and nothing in the recording allows it to be empty")
		})
	}

	t.Run("accepted: a recording that fills none of them", func(t *testing.T) {
		dir := t.TempDir()
		_, err := AddGolden(ctx, dir, "x", probeRecording(t, "ns", "mdl", []string{"v"}, nil, nil), AddOptions{Source: "s"})
		require.NoError(t, err)
		c, err := CheckGoldens(ctx, dir)
		require.NoError(t, err)
		assert.Empty(t, c.Problems)
		for _, output := range emptyRuled {
			assert.Zero(t, c.Rows["x/"+output], "precondition: %s is empty", output)
			assert.True(t, c.mayBeEmpty["x"][output], "%s is allowed empty", output)
		}
	})
}

// A fixture whose replay disagrees with its manifest's never-analyzed set is
// a problem, in every direction: the manifest leaves out an analyzer the
// replay never analyzed, names one the replay analyzed, or names another.
// So a fixture that moves into or out of the set is a diff of goldens.yaml,
// and an update says so.
func TestGoldens_TheNotRunSetIsPinned(t *testing.T) {
	ctx := context.Background()
	rec := builtStart(t, probeRecording(t, "ns", "mdl", []string{"v"}, listing(), nil), analyzerSLO)
	for _, tc := range []struct {
		name     string
		manifest []string
		want     string
	}{
		{"left out", nil, "x: the analyzers built and never analyzed are [slo], and goldens.yaml says []"},
		{"another analyzer", []string{analyzerThroughput}, "x: the analyzers built and never analyzed are [slo], and goldens.yaml says [throughput]"},
		{"one more", []string{analyzerSLO, analyzerThroughput}, "x: the analyzers built and never analyzed are [slo], and goldens.yaml says [slo throughput]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := AddGolden(ctx, dir, "x", rec, AddOptions{Source: "s"})
			require.NoError(t, err)
			m, err := ReadManifest(dir)
			require.NoError(t, err)
			m.Goldens[0].NotRun = tc.manifest
			require.NoError(t, writeManifest(dir, m))
			c, err := CheckGoldens(ctx, dir)
			require.NoError(t, err)
			assert.Contains(t, strings.Join(c.Problems, "\n"), tc.want)

			moved, err := RewriteGoldens(ctx, dir)
			require.NoError(t, err)
			assert.Equal(t, []string{"x: built and never analyzed was " + fmtList(tc.manifest) + ", now [slo]"}, moved, "the update says what it changed")
			c, err = CheckGoldens(ctx, dir)
			require.NoError(t, err)
			assert.Empty(t, c.Problems, "the update writes the set back")
		})
	}

	t.Run("names an analyzer the replay analyzed", func(t *testing.T) {
		dir := copyGoldens(t)
		m, err := ReadManifest(dir)
		require.NoError(t, err)
		found := false
		for i := range m.Goldens {
			if m.Goldens[i].Name == "acceptance" {
				m.Goldens[i].NotRun = []string{analyzerSLO}
				found = true
			}
		}
		require.True(t, found)
		require.NoError(t, writeManifest(dir, m))
		c, err := CheckGoldens(ctx, dir)
		require.NoError(t, err)
		assert.Contains(t, strings.Join(c.Problems, "\n"), "acceptance: the analyzers built and never analyzed are [], and goldens.yaml says [slo]")
	})
}

// fmtList is a list as %v prints it.
func fmtList(s []string) string { return "[" + strings.Join(s, " ") + "]" }
