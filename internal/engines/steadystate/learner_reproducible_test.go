package steadystate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// Each recording generator, run again at one commit, writes the same bytes
// (docs/proposals/analyzer-learner.md, 2.4: a recording is a reproducible
// fixture). Events that share a timestamp come out in a fixed order, not in
// map order: the NoteVariantsPresent events by model (namespace/model key),
// each naming its variants sorted, and each model's StableAcceleratorFor
// and FileScaleTarget events by variant (namespace/variant key). The
// generators analyse models in sorted order themselves. So two recordings
// made at one commit have no diff, and a diff between two means an input
// changed (#400).
//
// The variant names inside one NoteVariantsPresent come from a slice in the
// generators, so this test cannot see their sort; the test after it does.
func TestLearnerRecording_EachGeneratorIsReproducible(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) []byte
		// manyModels: the run notes more than one model in a cycle, so the
		// model sort in notePresentVariants is reached. The mu-paths run has
		// one model.
		manyModels bool
	}{
		{"acceptance", func(t *testing.T) []byte { b, _ := recordRun(t, recStart, true); return b }, true},
		{"mu-paths", func(t *testing.T) []byte { b, _ := recordMuRun(t, recStart); return b }, false},
		{"engine", func(t *testing.T) []byte { b, _ := recordEngineRun(t, recStart, true); return b }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := tc.run(t)
			require.NotEmpty(t, first, "precondition: the run recorded")
			models, widest := presentShape(t, first)
			// A model of two variants or more: the variant sort in
			// buildAnalyzerInput is reached.
			require.Greater(t, widest, 1, "precondition: some model has more than one variant")
			if tc.manyModels {
				require.Greater(t, models, 1, "precondition: more than one model")
			}
			// Several runs, not two: two map orders can agree by chance.
			for i := range 4 {
				require.Empty(t, firstDiff(first, tc.run(t)), "run %d recorded other bytes", i+2)
			}
		})
	}
}

// notePresentVariants names each model's variants sorted, whatever order the
// group holds them in: a running controller builds the group from a map.
func TestNotePresentVariants_NamesTheVariantsSorted(t *testing.T) {
	e := sloWiringEngine(t)
	spy := &sloKeeperSpy{}
	e.sloAnalyzer = spy
	va := func(name string) llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
		return llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "chat"},
			Spec:       llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{ModelID: "m"},
		}
	}
	e.notePresentVariants(context.Background(), map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		"chat/m": {va("c"), va("a"), va("b")},
	})
	assert.Equal(t, []string{"a", "b", "c"}, spy.present["chat/m"])
}

// presentShape is how many distinct models a recording's saturation
// NoteVariantsPresent events name, and the most variants one of them names.
func presentShape(t *testing.T, recording []byte) (models, widest int) {
	t.Helper()
	events, err := readEvents(recording)
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, ev := range events {
		if ev.Kind != saturation.KindNoteVariantsPresent {
			continue
		}
		var d struct {
			Namespace, ModelID string
			Variants           []string
		}
		require.NoError(t, json.Unmarshal(ev.Data, &d))
		seen[d.Namespace+"/"+d.ModelID] = true
		widest = max(widest, len(d.Variants))
	}
	return len(seen), widest
}

// firstDiff is the first line at which two recordings differ, from each, or
// "" when they are equal.
func firstDiff(a, b []byte) string {
	al, bl := bytes.Split(a, []byte("\n")), bytes.Split(b, []byte("\n"))
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y []byte
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if !bytes.Equal(x, y) {
			return fmt.Sprintf("line %d:\n  first: %s\n  again: %s", i+1, x, y)
		}
	}
	return ""
}
