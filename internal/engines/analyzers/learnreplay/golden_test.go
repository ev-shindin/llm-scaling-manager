package learnreplay

import (
	"context"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goldenDir is the committed goldens, from this package's directory.
const goldenDir = "testdata/goldens"

// The golden check (docs/proposals/analyzer-learner.md, 2.5): every
// committed fixture replays, and every output equals its golden file. `make
// test` runs it; a golden that no longer reproduces, or a manifest that
// disagrees with the directory, fails the build.
func TestGoldens_EveryCommittedGoldenReproduces(t *testing.T) {
	m, err := ReadManifest(goldenDir)
	require.NoError(t, err)
	c, err := CheckGoldens(context.Background(), goldenDir)
	require.NoError(t, err)
	assert.Empty(t, c.Problems, "run `make learner-goldens` only for a change the PR names (docs/developer-guide/testing.md)")
	assert.Empty(t, c.Uncovered, "the committed goldens must run every learner path between them")
	// A check that compared nothing is not a pass.
	require.NotEmpty(t, m.Goldens, "no fixture")
	require.Equal(t, len(m.Goldens), c.Fixtures, "fixtures replayed")
	// Every output of every fixture compares rows, but for an output its
	// recording allows to be empty, each by a named rule (emptyRules): an
	// analyzer built and never analyzed, no ITL line published or borrowed,
	// no learned-state snapshot taken.
	for _, g := range m.Goldens {
		for _, o := range g.Outputs {
			if c.mayBeEmpty[g.Name][path.Base(o)] && c.Rows[o] == 0 {
				t.Logf("%s is empty, by its rule", o)
				continue
			}
			assert.Positive(t, c.Rows[o], "%s compared no row", o)
		}
	}
	// The cluster goldens ran under a policy that lists only saturation: slo
	// was built and never analyzed in each, and no other fixture has an
	// analyzer it never analyzed.
	assert.Equal(t, map[string][]string{"e1": {analyzerSLO}, "e1-tuned": {analyzerSLO}, "pd-restart": {analyzerSLO}}, c.NotRun)
	t.Logf("%d fixtures, rows compared per file: %v; built and never analyzed: %v", c.Fixtures, c.Rows, c.NotRun)
}

// copyGoldens copies the committed goldens to a directory a negative control
// can break.
func copyGoldens(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(goldenDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(goldenDir, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	require.NoError(t, err)
	return dst
}

func checkProblems(t *testing.T, dir string) []string {
	t.Helper()
	c, err := CheckGoldens(context.Background(), dir)
	require.NoError(t, err)
	return c.Problems
}

// editGolden rewrites one golden file of the first fixture through edit.
func editGolden(t *testing.T, dir, output string, edit func([]row) []row) {
	t.Helper()
	m, err := ReadManifest(dir)
	require.NoError(t, err)
	p := filepath.Join(dir, m.Goldens[0].Name, output)
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	rows, err := readRows(b)
	require.NoError(t, err)
	rows = edit(rows)
	out, err := writeRows(rows)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, out, 0o644))
}

// Negative controls: each way a golden can stop holding is a problem the
// check reports, and two ways a replay varies that are not a change of
// output are not.
func TestGoldens_TheCheckFails(t *testing.T) {
	require.Empty(t, checkProblems(t, copyGoldens(t)), "precondition: the copy passes")

	t.Run("when an output moves", func(t *testing.T) {
		dir := copyGoldens(t)
		editGolden(t, dir, OutputVariantCapacities, func(rows []row) []row {
			rows[3].data = strings.Replace(rows[3].data, `"PerReplicaCapacity":`, `"PerReplicaCapacity":1`, 1)
			return rows
		})
		p := checkProblems(t, dir)
		require.Len(t, p, 1, "%v", p)
		assert.Contains(t, p[0], "golden no longer reproduces: acceptance/variant-capacities.csv: 1 of 180 rows changed, 0 added, 0 removed; first:")
		assert.Contains(t, p[0], ".PerReplicaCapacity:")
		t.Log(p[0])
	})
	t.Run("when a row is gone", func(t *testing.T) {
		dir := copyGoldens(t)
		editGolden(t, dir, OutputLines, func(rows []row) []row { return rows[1:] })
		p := checkProblems(t, dir)
		require.Len(t, p, 1, "%v", p)
		assert.Contains(t, p[0], "acceptance/lines.csv: 0 of 19 rows changed, 1 added, 0 removed")
		t.Log(p[0])
	})
	t.Run("when a file is not listed", func(t *testing.T) {
		dir := copyGoldens(t)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "acceptance", "notes.csv"), []byte("x\n"), 0o644))
		p := checkProblems(t, dir)
		assert.Equal(t, []string{"acceptance/notes.csv is not listed in goldens.yaml"}, p)
	})
	t.Run("when a listed file is missing", func(t *testing.T) {
		dir := copyGoldens(t)
		require.NoError(t, os.Remove(filepath.Join(dir, "acceptance", OutputResults)))
		p := checkProblems(t, dir)
		assert.Equal(t, []string{"goldens.yaml lists acceptance/results.csv, which is missing"}, p)
	})
	t.Run("when the manifest leaves an output out", func(t *testing.T) {
		dir := copyGoldens(t)
		b, err := os.ReadFile(filepath.Join(dir, ManifestName))
		require.NoError(t, err)
		cut := strings.Replace(string(b), "  - acceptance/slo-published.csv\n", "", 1)
		require.NotEqual(t, string(b), cut, "precondition: the manifest lists the file")
		require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestName), []byte(cut), 0o644))
		p := checkProblems(t, dir)
		assert.Equal(t, []string{
			"acceptance/slo-published.csv is not listed in goldens.yaml",
			"acceptance: the output slo-published.csv has no golden file in goldens.yaml",
		}, p)
	})
	t.Run("when a recording is over the limit", func(t *testing.T) {
		dir := copyGoldens(t)
		big := make([]byte, MaxFixtureBytes+1)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "acceptance", RecordingName), big, 0o644))
		p := checkProblems(t, dir)
		require.Len(t, p, 1, "%v", p)
		assert.Contains(t, p[0], "over the 4194304-byte fixture limit")
	})

	t.Run("but not when the rows come in another order", func(t *testing.T) {
		dir := copyGoldens(t)
		m, err := ReadManifest(dir)
		require.NoError(t, err)
		p := filepath.Join(dir, m.Goldens[0].Name, OutputStoreRecords)
		b, err := os.ReadFile(p)
		require.NoError(t, err)
		lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		body := lines[1:]
		for i, j := 0, len(body)-1; i < j; i, j = i+1, j-1 {
			body[i], body[j] = body[j], body[i]
		}
		require.NoError(t, os.WriteFile(p, []byte(lines[0]+"\n"+strings.Join(body, "\n")+"\n"), 0o644))
		assert.Empty(t, checkProblems(t, dir))
	})
	t.Run("but not when a float moves in its last bits", func(t *testing.T) {
		dir := copyGoldens(t)
		moved := 0
		editGolden(t, dir, OutputVariantCapacities, func(rows []row) []row {
			for i := range rows {
				n, err := parseNode([]byte(rows[i].data))
				require.NoError(t, err)
				v := n.get("PerReplicaCapacity")
				if v == nil || v.kind != nodeNumber {
					continue
				}
				f, err := strconv.ParseFloat(v.text, 64)
				require.NoError(t, err)
				if f == 0 {
					continue
				}
				// A few ulps: what a sum taken in another order gives.
				v.text = strconv.FormatFloat(f*(1+4e-16), 'g', -1, 64)
				if v.text == strconv.FormatFloat(f, 'g', -1, 64) {
					v.text = strconv.FormatFloat(math.Nextafter(f, math.Inf(1)), 'g', -1, 64)
				}
				rows[i].data = string(n.bytes())
				moved++
			}
			return rows
		})
		require.Positive(t, moved, "precondition: rows to move")
		assert.Empty(t, checkProblems(t, dir))
	})
}

// Rewriting the goldens at the commit that wrote them changes no byte.
func TestGoldens_UpdateAtTheSameCommitChangesNothing(t *testing.T) {
	dir := copyGoldens(t)
	require.NoError(t, UpdateGoldens(context.Background(), dir))
	err := filepath.WalkDir(goldenDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(goldenDir, p)
		if err != nil {
			return err
		}
		want, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		assert.Equal(t, normalizeEOL(want), normalizeEOL(got), "%s", rel)
		return nil
	})
	require.NoError(t, err)
}

// normalizeEOL undoes a CRLF checkout of a text golden; a gzipped recording
// is compared as it is.
func normalizeEOL(b []byte) string {
	if len(b) > 1 && b[0] == 0x1f && b[1] == 0x8b {
		return string(b)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// The goldens' key ignores what varies between replays of one recording:
// sequence numbers, and the order of rows.
func TestOutputs_HaveNoSequenceNumbers(t *testing.T) {
	out, err := Outputs(mixedRecording(t))
	require.NoError(t, err)
	for name, b := range out {
		rows, err := readRows(b)
		require.NoError(t, err)
		for _, r := range rows {
			assert.NotContains(t, r.data, `"Seq"`, "%s", name)
		}
	}
	rows, err := readRows(out[OutputResults])
	require.NoError(t, err)
	assert.Len(t, rows, 3, "one result per analyzer")
}
