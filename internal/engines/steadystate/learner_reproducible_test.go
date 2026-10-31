package steadystate

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
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
func TestLearnerRecording_EachGeneratorIsReproducible(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) []byte
	}{
		{"acceptance", func(t *testing.T) []byte { b, _ := recordRun(t, recStart, true); return b }},
		{"mu-paths", func(t *testing.T) []byte { b, _ := recordMuRun(t, recStart); return b }},
		{"engine", func(t *testing.T) []byte { b, _ := recordEngineRun(t, recStart, true); return b }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := tc.run(t)
			require.NotEmpty(t, first, "precondition: the run recorded")
			// Several runs, not two: two map orders can agree by chance.
			for i := range 4 {
				require.Empty(t, firstDiff(first, tc.run(t)), "run %d recorded other bytes", i+2)
			}
		})
	}
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
