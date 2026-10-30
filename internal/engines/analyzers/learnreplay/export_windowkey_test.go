package learnreplay

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// A k2 window key (namespace|variant|model|accelerator|GPUs|role|bucket|qN)
// and a throughput window key (the same with the input bucket) keep their
// shape parts, and their GPU count is never harvested as a name. Before
// their layouts were known every part of one was a name: the count "1" of
// a k2 window became name-01, and every variant key holding a GPU count of
// 1 then failed its count part, so a recording that measured k2 could not
// be exported (the mu-paths golden found it).
func TestExport_WindowKeysKeepTheirShapeParts(t *testing.T) {
	x := &exporter{byCategory: map[string]map[string]bool{}}
	x.harvestComposites([]string{
		"ns|v|mdl|H100|1|decode|medium|q5",
		"ns|v|mdl|H100|1|decode|imedium|medium|q5",
	})
	for _, part := range []string{"1", "decode", "medium", "imedium", "q5", "H100"} {
		assert.False(t, x.byCategory["name"][part], "%s was harvested as a name", part)
	}
	for _, name := range []string{"ns", "v", "mdl"} {
		assert.True(t, x.byCategory["name"][name], "%s, a name part, was not harvested", name)
	}

	for part, ok := range map[string]bool{
		"decode": true, "prefill": true, "both": true, "medium": true, "xxlong": true, "noout": true,
		"ishort": true, "q5": true, "q0.5": true, "q5#0123456789abcdef": true,
		"payroll": false, "ipayroll": false, "qx": false, "q": false, "q5#payroll": false, "": false,
	} {
		assert.Equal(t, ok, shapePart(part), "shapePart(%q)", part)
	}
}

// A name in a window key's shape slot is not kept as a shape part: the
// export renames or refuses it.
func TestExportLeak_NameInAWindowKeysShapeSlot(t *testing.T) {
	key := "ns|v|mdl|H100|1|payrollrole|medium|q5"
	zzLeak(t, zzRec(t, zzOpts{before: func(_ *learnrec.Recorder, f *fixture) {
		f.sat.RestoreLearnedState(saturation.LearnedState{K2: map[string]capacity.RollingAverageSnapshot{key: {}}})
	}}), "payrollrole")
}
