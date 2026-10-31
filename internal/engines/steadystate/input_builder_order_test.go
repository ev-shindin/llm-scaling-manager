package steadystate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// engineRunEventCounts is the committed count of each event kind per cycle
// of the engine fixture's run (recordEngineRun), recorded on the commit
// before the input builder existed (#345 step 1's parent). Counts, not
// order: the builder ranges a map of variants, so the order of its events
// within a cycle is not stable between two runs (#400).
var engineRunEventCounts = filepath.Join("testdata", "engine-run-event-counts.txt")

// The input builder's side effects all land before the model's saturation
// Analyze reads them, and the run records exactly the events it recorded
// before the builder existed. A lost or moved scale-target load, forget or
// stabilised lookup changes the counts; one moved after Analyze fails the
// order.
func TestInputBuilder_TheEngineRunRecordsTheSameEventsInTheSamePhases(t *testing.T) {
	recording, _ := recordEngineRun(t, recStart, true)
	events, err := readEvents(recording)
	require.NoError(t, err)

	// Order: per (cycle, model), every forget and every scale-target load
	// comes before the model's saturation Analyze.
	type cycleModel struct {
		now   time.Time
		model string
	}
	analyzedAt := map[cycleModel]uint64{}
	modelOf := map[string]string{} // namespace/variant -> namespace/model
	for _, ev := range events {
		switch ev.Kind {
		case saturation.KindAnalyze:
			var d saturation.AnalyzeEvent
			require.NoError(t, ev.DecodeData(&d))
			var in learnrec.Input
			require.NoError(t, d.Input.Decode(&in))
			key := cycleModel{ev.Now, utils.GetNamespacedKey(in.Namespace, in.ModelID)}
			_, seen := analyzedAt[key]
			require.False(t, seen, "two saturation Analyzes for %v", key)
			analyzedAt[key] = ev.Seq
		case capacity.KindFileScaleTarget:
			var d capacity.FileScaleTargetEvent
			require.NoError(t, ev.DecodeData(&d))
			modelOf[utils.GetNamespacedKey(d.Namespace, d.Variant)] = utils.GetNamespacedKey(d.Namespace, d.ModelID)
		}
	}
	checked := map[string]int{}
	for _, ev := range events {
		var key cycleModel
		switch ev.Kind {
		case capacity.KindFileScaleTarget:
			var d capacity.FileScaleTargetEvent
			require.NoError(t, ev.DecodeData(&d))
			key = cycleModel{ev.Now, utils.GetNamespacedKey(d.Namespace, d.ModelID)}
		case saturation.KindForgetVariant:
			var d saturation.ForgetVariantEvent
			require.NoError(t, ev.DecodeData(&d))
			key = cycleModel{ev.Now, utils.GetNamespacedKey(d.Namespace, d.ModelID)}
		case saturation.KindForgottenToken:
			var d saturation.ForgottenTokenEvent
			require.NoError(t, ev.DecodeData(&d))
			model, ok := modelOf[utils.GetNamespacedKey(d.Namespace, d.Variant)]
			require.True(t, ok, "a forgotten-token read for a variant no load names: %s/%s", d.Namespace, d.Variant)
			key = cycleModel{ev.Now, model}
		default:
			continue
		}
		at, ok := analyzedAt[key]
		require.True(t, ok, "a %s for %v, a model saturation never analyzed that cycle", ev.Kind, key)
		assert.Less(t, ev.Seq, at, "%s (seq %d) after the saturation Analyze of %v", ev.Kind, ev.Seq, key)
		checked[ev.Kind]++
	}
	// The order check reached every kind it names: a fixture that stopped
	// forgetting would pass it vacuously.
	for _, kind := range []string{capacity.KindFileScaleTarget, saturation.KindForgetVariant, saturation.KindForgottenToken} {
		assert.Positive(t, checked[kind], "no %s event was checked", kind)
	}

	// Counts per cycle and kind, against the parent's.
	counts := map[string]int{}
	for _, ev := range events {
		counts[ev.Now.UTC().Format(time.RFC3339)+" "+ev.Kind]++
	}
	lines := make([]string, 0, len(counts))
	for k, n := range counts {
		lines = append(lines, fmt.Sprintf("%s %d", k, n))
	}
	sort.Strings(lines)
	got := strings.Join(lines, "\n") + "\n"
	if os.Getenv("ENGINE_RUN_EVENT_COUNTS_WRITE") != "" {
		if os.Getenv("CI") != "" {
			t.Fatal("ENGINE_RUN_EVENT_COUNTS_WRITE is refused under CI: the counts are a golden, " +
				"rewritten locally and committed with the change named in the PR's goldens: section")
		}
		require.NoError(t, os.WriteFile(engineRunEventCounts, []byte(got), 0o644))
		t.Logf("wrote %s (%d rows)", engineRunEventCounts, len(lines))
	}
	want, err := os.ReadFile(engineRunEventCounts)
	require.NoError(t, err)
	require.Greater(t, len(lines), 100, "the run recorded too few kinds per cycle to pin anything")
	assert.Equal(t, strings.ReplaceAll(string(want), "\r\n", "\n"), got,
		"the engine run's events per cycle and kind moved; set ENGINE_RUN_EVENT_COUNTS_WRITE=1 only for a change the PR names")
}
