package learnreplay

import (
	"bytes"
	"context"
	"os"
	"path/filepath"

	"testing"

	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// A digits-only namespace inside a message is renamed.
func TestExportLeak2_DigitsNamespaceInText(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{ns: "4711", before: func(r *learnrec.Recorder, _ *fixture) {
		plan(r, struct{ Error string }{"namespace 4711 over quota"})
	}}), "namespace 4711")
}

// A word shaped like a number ("Infinity", a hex float) is not a number.
func TestExportLeak2_NameShapedLikeNumberInText(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{before: func(r *learnrec.Recorder, _ *fixture) {
		plan(r, struct{ Error string }{"team Infinity and NaN and 0x1p3 and 1e5"})
	}}), "Infinity")
}

// What a killed add left (".add-<name>-*") is cleaned up, and the add then
// replaces the fixture cleanly.
func TestExportLeak2_StaleAddDirectory(t *testing.T) {
	ctx := context.Background()
	dir := copyGoldens(t)
	raw := probeRecording(t, "ns", "mdl", []string{"v"}, nil, nil)
	stale := filepath.Join(dir, ".add-acceptance-ready")
	require.NoError(t, os.MkdirAll(filepath.Join(stale, "junk"), 0o755))
	_, err := AddGolden(ctx, dir, "acceptance", raw, AddOptions{Source: "replaced", Replace: true})
	require.NoError(t, err)
	_, err = os.Stat(stale)
	require.True(t, os.IsNotExist(err), "the stale directory is still there")
	c, err := CheckGoldens(ctx, dir)
	require.NoError(t, err)
	require.Empty(t, c.Problems)
}

// The commit is checked before the export: a recording from another commit
// says so, even when it holds a field this commit does not classify.
func TestExportLeak2_CommitCheckComesFirst(t *testing.T) {
	raw := zzRec(t, zzOpts{before: func(r *learnrec.Recorder, _ *fixture) {
		plan(r, struct{ NewFieldFromAnotherCommit string }{"x"})
	}})
	at := bytes.Replace(raw, []byte(`"Commit":""`), []byte(`"Commit":"1111111"`), 1)
	require.NotEqual(t, raw, at)
	_, err := AddGolden(context.Background(), t.TempDir(), "x", at, AddOptions{Source: "s", Commit: "2222222"})
	require.ErrorContains(t, err, "the recording was made at 1111111 and you are at 2222222")
}
