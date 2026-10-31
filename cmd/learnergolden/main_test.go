package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/learnreplay"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
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
	in := rawRecording(t)
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

	// The one golden reproduces again, and reaches none of k2's and mu's
	// measured paths: an add and an update say so, the check fails on it.
	assert.Contains(t, out, "no golden runs the learner path "+saturation.PathMuSampleWritten)
	out, err = runOut("check", "-dir", dir)
	require.ErrorContains(t, err, "6 learner paths no golden runs", out)
}
