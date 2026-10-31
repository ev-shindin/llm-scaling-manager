package learnreplay

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// Integers must be equal; other numbers equal within FloatTolerance.
func TestSameNumber(t *testing.T) {
	for _, tc := range []struct {
		want, got string
		same      bool
	}{
		{"8192", "8192", true},
		{"1000000000", "1000000001", false}, // a count off by one, under any relative tolerance
		{"0", "1", false},
		{"0.1", "0.1000000000000001", true}, // the last bits of a sum
		{"0.1", "0.10000000001", true},      // 1e-10 relative: inside
		{"0.1", "0.1000000002", false},      // 2e-9 relative: outside
		{"0.1", "0.1000001", false},         // a 1e-6 move
		{"0", "1e-300", false},              // zero against tiny
		{"0.5", "-0.5", false},              // a sign flip
		{"1.5e-05", "1.5000000000000001e-05", true},
		{"x", "y", false},
	} {
		assert.Equal(t, tc.same, sameNumber(tc.want, tc.got), "%s against %s", tc.want, tc.got)
	}
}

// Rows with the same key pair up in data order: two swapped rows within the
// tolerance are equal, and one changed row is one change.
func TestCompareOutput_PairsDuplicateKeys(t *testing.T) {
	mk := func(datas ...string) []byte {
		rows := make([]row, 0, len(datas))
		for _, d := range datas {
			rows = append(rows, row{"k", "t", "g", "i", d})
		}
		b, err := writeRows(rows)
		require.NoError(t, err)
		return b
	}
	d, err := compareOutput("x.csv", mk(`{"A":1}`, `{"A":2}`), mk(`{"A":2}`, `{"A":1}`))
	require.NoError(t, err)
	assert.Nil(t, d, "rows in another order")
	d, err = compareOutput("x.csv", mk(`{"A":1}`, `{"A":2}`), mk(`{"A":1}`, `{"A":3}`))
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.Equal(t, 1, d.Changed)
	assert.Equal(t, 0, d.Added+d.Removed)
}

func TestHeadLines(t *testing.T) {
	b := []byte("a\nb\nc\n")
	for _, tc := range []struct {
		n    int
		want string
	}{{0, ""}, {1, "a\n"}, {3, "a\nb\nc\n"}, {9, "a\nb\nc\n"}} {
		assert.Equal(t, tc.want, string(headLines(b, tc.n)), "n=%d", tc.n)
	}
	assert.Equal(t, "a\nb", string(headLines([]byte("a\nb"), 5)), "no newline at the end")
}

// fixtureRecording reads the committed acceptance recording.
func fixtureRecording(t *testing.T) []byte {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join(goldenDir, "acceptance", RecordingName))
	require.NoError(t, err)
	b, err := gunzip(gz)
	require.NoError(t, err)
	return b
}

// writeFixtureRecording replaces the copy's acceptance recording.
func writeFixtureRecording(t *testing.T, dir string, b []byte) {
	t.Helper()
	gz, err := Gzip(b)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "acceptance", RecordingName), gz, 0o644))
}

// The check's other problem paths: each is reported, none passes.
func TestCheckGoldens_ProblemPaths(t *testing.T) {
	manifest := func(t *testing.T, dir string, edit func(string) string) {
		p := filepath.Join(dir, ManifestName)
		b, err := os.ReadFile(p)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p, []byte(edit(string(b))), 0o644))
	}
	for _, tc := range []struct {
		name  string
		edit  func(t *testing.T, dir string)
		want  string
		error bool
	}{
		{name: "no fixture", edit: func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestName), []byte("goldens: []\n"), 0o644))
			require.NoError(t, os.RemoveAll(filepath.Join(dir, "acceptance")))
		}, want: "goldens.yaml lists no fixture"},
		{name: "listed twice", edit: func(t *testing.T, dir string) {
			manifest(t, dir, func(s string) string {
				return strings.Replace(s, "  - acceptance/lines.csv\n", "  - acceptance/lines.csv\n  - acceptance/lines.csv\n", 1)
			})
		}, want: "goldens.yaml lists acceptance/lines.csv twice"},
		{name: "not an output", edit: func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "acceptance", "other.csv"), []byte("x\n"), 0o644))
			manifest(t, dir, func(s string) string {
				return strings.Replace(s, "  - acceptance/lines.csv\n", "  - acceptance/lines.csv\n  - acceptance/other.csv\n", 1)
			})
		}, want: "acceptance/other.csv is not an output a replay produces"},
		{name: "a bad header", edit: func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "acceptance", OutputLines), []byte("a,b\n"), 0o644))
		}, want: "acceptance/lines.csv: the header is not kind,now,group,item,data"},
		{name: "a data cell that is not JSON", edit: func(t *testing.T, dir string) {
			editGolden(t, dir, OutputLines, func(rows []row) []row { rows[0].data = "{"; return rows })
		}, want: "the golden row is not JSON"},
		{name: "a corrupt recording", edit: func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "acceptance", RecordingName), []byte("not gzip"), 0o644))
		}, want: "acceptance: acceptance/recording.jsonl.gz: "},
		{name: "a replay that fails", edit: func(t *testing.T, dir string) {
			// The engine filed the variant as rolling out; the store check
			// before the next Analyze finds the record that says it is not.
			rec := fixtureRecording(t)
			i := bytes.Index(rec, []byte(`"RollingOut":false}`))
			require.Positive(t, i)
			writeFixtureRecording(t, dir, append(append(bytes.Clone(rec[:i]), []byte(`"RollingOut":true}`)...), rec[i+len(`"RollingOut":false}`):]...))
		}, want: "acceptance: the replay failed: seq"},
		{name: "a recording whose start leaves out an analyzer it ran", edit: func(t *testing.T, dir string) {
			rec := fixtureRecording(t)
			writeFixtureRecording(t, dir, bytes.Replace(rec, []byte(`"Throughput":true`), []byte(`"Throughput":false`), 1))
		}, want: "learnreplay: seq"},
		{name: "an unknown manifest field", edit: func(t *testing.T, dir string) {
			manifest(t, dir, func(s string) string {
				return strings.Replace(s, "- name: acceptance", "- name: acceptance\n  owner: x", 1)
			})
		}, error: true},
		{name: "a renamed key", edit: func(t *testing.T, dir string) {
			editGolden(t, dir, OutputResults, func(rows []row) []row { rows[0].group = "ns-01/model-09"; return rows })
		}, want: "1 added, 1 removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyGoldens(t)
			tc.edit(t, dir)
			c, err := CheckGoldens(context.Background(), dir)
			if tc.error {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, c.Problems)
			assert.Contains(t, strings.Join(c.Problems, "\n"), tc.want)
			t.Log(strings.Join(c.Problems, "\n"))
		})
	}
}

// A golden that would compare nothing is refused, by the check and by the
// add alike: an analyzer that ran must have results. The slo analyzer here
// ran: it has Analyze events, and its results are taken away.
func TestComparedNothing(t *testing.T) {
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, nil, nil)
	out, err := goldenOutputs(context.Background(), rec, Fidelity)
	require.NoError(t, err, "a saturation-only run compares what it ran")
	require.NotEmpty(t, out)

	acc := fixtureRecording(t)
	read, err := learnrec.Read(acc)
	require.NoError(t, err)
	var start learnrec.Start
	require.NoError(t, read.Events[0].DecodeData(&start))
	require.NotNil(t, start.SLO, "precondition: the acceptance run built the slo analyzer")
	require.Empty(t, builtNotRun(start, read.Events), "precondition: and ran it")
	outputs, err := goldenOutputs(context.Background(), acc, Normal)
	require.NoError(t, err)
	require.Empty(t, comparedNothing(start, outputs, builtNotRun(start, read.Events)))

	empty, err := writeRows(nil)
	require.NoError(t, err)
	outputs[OutputSLOPublished] = empty
	rows, err := readRows(outputs[OutputResults])
	require.NoError(t, err)
	rows = slices.DeleteFunc(rows, func(r row) bool { return r.kind == slo.KindAnalyze })
	outputs[OutputResults], err = writeRows(rows)
	require.NoError(t, err)
	assert.Equal(t, []string{"slo results", "slo published"}, comparedNothing(start, outputs, builtNotRun(start, read.Events)),
		"an analyzer with Analyze events whose results compare nothing is refused")
}

// An analyzer the start built and that ran no Analyze is not run: the
// controller builds the slo analyzer whatever the policy lists, so a run
// whose models list only saturation has no slo results to compare. The add
// and the check accept it and say so; no golden compares it. On the parent
// commit, which asked results of every analyzer the start built, the add
// refused this recording ("would compare nothing for slo results, slo
// published"), as it refused the E1 benchmark recordings.
func TestComparedNothing_ABuiltAnalyzerThatNeverRanIsNotRun(t *testing.T) {
	ctx := context.Background()
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, nil, nil)
	withSLO := bytes.Replace(rec, []byte(`"SLO":null`), []byte(`"SLO":{"StateTimeout":3600000000000,"SlopeHoldFor":3600000000000,"Judging":false}`), 1)
	require.NotEqual(t, rec, withSLO, "precondition: the start now builds the slo analyzer")
	require.NotContains(t, string(withSLO), slo.KindAnalyze, "precondition: and it never ran")

	out, _, notes, err := goldenReplay(ctx, withSLO, Fidelity)
	require.NoError(t, err)
	require.NotEmpty(t, out)
	assert.Equal(t, []string{"slo: built, not run (0 Analyze events)"}, notes)

	dir := t.TempDir()
	_, err = AddGolden(ctx, dir, "x", withSLO, AddOptions{Source: "s"})
	require.NoError(t, err)
	c, err := CheckGoldens(ctx, dir)
	require.NoError(t, err)
	assert.Empty(t, c.Problems)
	assert.Equal(t, 1, c.Fixtures)
	assert.Equal(t, []string{"x: slo: built, not run (0 Analyze events)"}, c.NotRun)
	assert.Positive(t, c.Rows["x/"+OutputResults], "the saturation results are still compared")
}

// listDir is every file under dir with its bytes.
func listDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

func TestAddGolden(t *testing.T) {
	ctx := context.Background()
	raw := probeRecording(t, "ns", "mdl", []string{"v"}, nil, nil)

	t.Run("into a fresh directory, and the check passes", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "goldens")
		_, err := AddGolden(ctx, dir, "small", raw, AddOptions{Source: "a small run"})
		require.NoError(t, err)
		c, err := CheckGoldens(ctx, dir)
		require.NoError(t, err)
		assert.Empty(t, c.Problems)
		assert.Equal(t, 1, c.Fixtures)
	})
	t.Run("refusals write nothing", func(t *testing.T) {
		dir := copyGoldens(t)
		before := listDir(t, dir)
		for _, tc := range []struct {
			name string
			raw  []byte
			opts AddOptions
			want string
		}{
			{"Bad_Name", raw, AddOptions{Source: "s"}, "a fixture name is lowercase"},
			{"acceptance", raw, AddOptions{Source: "s"}, "the fixture acceptance exists"},
			{"x", raw, AddOptions{Source: "a run in ns"}, "holds a name"},
			{"x", []byte("{}\n"), AddOptions{Source: "s"}, "learnrec: line 1"},
		} {
			_, err := AddGolden(ctx, dir, tc.name, tc.raw, tc.opts)
			require.ErrorContains(t, err, tc.want, tc.name)
		}
		assert.Equal(t, before, listDir(t, dir), "a refusal changed the directory")
	})
	t.Run("a recording made at another commit is refused", func(t *testing.T) {
		at := bytes.Replace(raw, []byte(`"Commit":""`), []byte(`"Commit":"1111111"`), 1)
		require.NotEqual(t, raw, at, "precondition")
		_, err := AddGolden(ctx, t.TempDir(), "x", at, AddOptions{Source: "s", Commit: "2222222"})
		require.ErrorContains(t, err, "the recording was made at 1111111 and you are at 2222222")
	})
	t.Run("a directory the manifest does not list is not replaced", func(t *testing.T) {
		dir := copyGoldens(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "stray"), 0o755))
		_, err := AddGolden(ctx, dir, "stray", raw, AddOptions{Source: "s", Replace: true})
		require.ErrorContains(t, err, "does not list it")
	})
	t.Run("replace swaps the fixture and lists it once", func(t *testing.T) {
		dir := copyGoldens(t)
		_, err := AddGolden(ctx, dir, "acceptance", raw, AddOptions{Source: "replaced", Replace: true})
		require.NoError(t, err)
		m, err := ReadManifest(dir)
		require.NoError(t, err)
		var listed []ManifestEntry
		for _, g := range m.Goldens {
			if g.Name == "acceptance" {
				listed = append(listed, g)
			}
		}
		require.Len(t, listed, 1)
		assert.Equal(t, "replaced", listed[0].Source)

		c, err := CheckGoldens(ctx, dir)
		require.NoError(t, err)
		assert.Empty(t, c.Problems)
		_, err = os.Stat(filepath.Join(dir, "acceptance", "old"))
		assert.True(t, os.IsNotExist(err), "the old fixture was left inside the new one")
	})
	t.Run("a head cut anywhere is added", func(t *testing.T) {
		full := fixtureRecording(t)
		n := bytes.Count(full, []byte("\n")) / 3
		_, err := AddGolden(ctx, t.TempDir(), "head", full, AddOptions{Source: "s", HeadLines: n})
		require.NoError(t, err)
	})
}

// UpdateGoldens repairs what the check reports, and leaves a fixture whose
// replay fails as it was.
func TestUpdateGoldens_Repairs(t *testing.T) {
	ctx := context.Background()
	dir := copyGoldens(t)
	editGolden(t, dir, OutputVariantCapacities, func(rows []row) []row { rows[0].data = `{"moved":1}`; return rows })
	require.NoError(t, os.Remove(filepath.Join(dir, "acceptance", OutputLines)))
	p := filepath.Join(dir, ManifestName)
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, []byte(strings.Replace(string(b), "  - acceptance/slo-published.csv\n", "", 1)), 0o644))
	require.NotEmpty(t, checkProblems(t, dir), "precondition: broken")
	require.NoError(t, UpdateGoldens(ctx, dir))
	assert.Empty(t, checkProblems(t, dir))

	broken := copyGoldens(t)
	require.NoError(t, os.WriteFile(filepath.Join(broken, "acceptance", RecordingName), []byte("not gzip"), 0o644))
	before := listDir(t, broken)
	require.Error(t, UpdateGoldens(ctx, broken))
	assert.Equal(t, before, listDir(t, broken))

	big := copyGoldens(t)
	require.NoError(t, os.WriteFile(filepath.Join(big, "acceptance", RecordingName), make([]byte, MaxFixtureBytes+1), 0o644))
	require.ErrorContains(t, UpdateGoldens(ctx, big), "over the")
}

// A recording too large for a fixture is refused before any replay, with a
// head to cut it to that ends on an Analyze.
func TestAddGolden_SuggestsAHead(t *testing.T) {
	full := fixtureRecording(t)
	exported, _, err := Export(full, ExportOptions{})
	require.NoError(t, err)
	gz, err := Gzip(exported)
	require.NoError(t, err)
	// A limit of a third of this recording's compressed size.
	n := suggestHead(exported, len(gz), len(gz)/3)
	read, err := learnrec.Read(exported)
	require.NoError(t, err)
	require.Less(t, n, len(read.Events))
	require.Positive(t, n)
	assert.True(t, strings.HasSuffix(read.Events[n-1].Kind, ".Analyze"), "the head ends on %s", read.Events[n-1].Kind)
}
