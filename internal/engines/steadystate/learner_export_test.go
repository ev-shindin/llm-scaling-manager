package steadystate

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/learnreplay"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
)

// The export's acceptance (docs/proposals/analyzer-learner.md, 2.4, "before
// commit"): the acceptance recording, exported, still replays byte for byte
// at this commit, and no name the script gave survives anywhere in its
// bytes. The names are listed from the script, not from the export's own
// tables, so a field the export forgot is found here.
//
// With LEARNREC_ACCEPTANCE_OUT set, the raw recording is also written there:
// the input `make learner-golden-add` takes to commit it as a golden.
func TestLearnerRecording_AnExportReplaysAndNamesNothing(t *testing.T) {
	recording, _ := recordRun(t, recStart, true)
	if out := os.Getenv("LEARNREC_ACCEPTANCE_OUT"); out != "" {
		require.NoError(t, os.WriteFile(out, recording, 0o644))
		t.Logf("wrote the raw acceptance recording to %s", out)
	}
	key := []byte("a key for this test only")
	exported, rep, err := learnreplay.Export(recording, learnreplay.ExportOptions{Key: key})
	require.NoError(t, err)
	t.Logf("export: %s", rep)
	assert.Equal(t, map[string]int{"ns": 1, "model": 3, "variant": 6, "pod": 26, "image": 1, "revision": 1, "token": 1},
		rep.Names, "names renamed, per category")
	assert.Positive(t, rep.Fingerprints, "fingerprints recomputed")

	again, _, err := learnreplay.Export(recording, learnreplay.ExportOptions{Key: key})
	require.NoError(t, err)
	require.Equal(t, string(exported), string(again), "the same recording and key export differently")
	assert.Equal(t, kindCounts(t, recording), kindCounts(t, exported), "the export has other events")

	replayed, err := learnreplay.Replay(context.Background(), exported, learnreplay.Options{Mode: learnreplay.Fidelity})
	require.NoError(t, err)
	require.Empty(t, mismatchText(replayed.Mismatches), "the export does not replay")
	require.Equal(t, string(exported), string(replayed.Recording), "the export's replay")
	assert.Equal(t, 28, replayed.Analyzes[saturation.KindAnalyze])

	names := scriptNames()
	require.Len(t, names, 40, "precondition: the script's names")
	for _, name := range names {
		require.True(t, bytes.Contains(recording, []byte(name)), "precondition: the recording names %q", name)
		assert.False(t, bytes.Contains(exported, []byte(name)), "%q survives the export", name)
	}
}

// scriptNames is every name the acceptance script gives: its namespace,
// models (and their organisation), variants, pods, image, revision and
// forget token.
func scriptNames() []string {
	s := newRecScript(recStart)
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	add(recNS)
	add("acme")
	add("vllm/vllm-openai:v0.11.0")
	add("forget-1")
	for _, v := range s.variants {
		add(v.model)
		add(v.name)
		for _, rm := range v.rows(0, recStart) {
			add(rm.PodName)
			add(rm.Revision)
		}
	}
	return out
}
