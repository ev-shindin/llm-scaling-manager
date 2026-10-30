package learnreplay

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		"ns|v|mdl|H100|1|decode|medium|q5#legacy:0123456789abcdef",
	})
	for _, part := range []string{"1", "decode", "medium", "imedium", "q5", "H100", "legacy:0123456789abcdef"} {
		assert.False(t, x.byCategory["name"][part], "%s was harvested as a name", part)
	}
	for _, name := range []string{"ns", "v", "mdl"} {
		assert.True(t, x.byCategory["name"][name], "%s, a name part, was not harvested", name)
	}

	for part, ok := range map[string]bool{
		"decode": true, "prefill": true, "both": true, "medium": true, "xxlong": true, "noout": true,
		"ishort": true, "q5": true, "q0.5": true,
		"payroll": false, "ipayroll": false, "qx": false, "q": false, "": false,
	} {
		assert.Equal(t, ok, shapePart(part), "shapePart(%q)", part)
	}
	for tag, ok := range map[string]bool{
		"0123456789abcdef": true, "incomplete:0123456789abcdef": true, "legacy:0123456789abcdef": true,
		"legacy:incomplete:0123456789abcdef": true, "payroll": false, "legacy:payroll": false, "": false,
	} {
		assert.Equal(t, ok, stashTag(tag), "stashTag(%q)", tag)
	}
}

// exportState records one Analyze after restoring st, under namespace ns,
// and exports it.
func exportState(t *testing.T, ns string, st saturation.LearnedState) ([]byte, error) {
	t.Helper()
	rec := zzRec(t, zzOpts{ns: ns, before: func(_ *learnrec.Recorder, f *fixture) {
		f.sat.RestoreLearnedState(st)
	}})
	out, _, err := Export(rec, ExportOptions{Key: []byte("k")})
	return out, err
}

// Every legal window key exports with only its names renamed: the k2 key, the
// throughput key, their stashed forms under each tag a stash carries, and the
// stashed variant key; and under a namespace named like a bucket or a role,
// whose name must not rename the bucket or the role of every key.
func TestExport_LegalWindowKeysExportWithOnlyTheirNamesRenamed(t *testing.T) {
	const fp = "0123456789abcdef"
	for _, ns := range []string{"payrollns", "medium", "decode"} {
		k2 := ns + "|v|mdl|H100|1|decode|medium|q5"
		tp := ns + "|v|mdl|H100|1|decode|imedium|medium|q5"
		st := saturation.LearnedState{
			K2:         map[string]capacity.RollingAverageSnapshot{k2: {}},
			Throughput: map[string]saturation.ThroughputSnapshot{tp: {}},
			K2Stash: map[string]capacity.RollingAverageSnapshot{
				k2 + "#" + fp: {}, k2 + "#legacy:" + fp: {}, k2 + "#incomplete:" + fp: {},
			},
			ThroughputStash:  map[string]saturation.ThroughputSnapshot{tp + "#legacy:incomplete:" + fp: {}},
			ITLBaselineStash: map[string]float64{ns + "|mdl|v|H100|1#" + fp: 0.01},
		}
		out, err := exportState(t, ns, st)
		require.NoError(t, err, "namespace %q", ns)
		got := string(out)
		// The stash tags keep their form; the fingerprint after one is
		// re-keyed, as every fingerprint is.
		for _, kept := range []string{
			`\|H100\|1\|decode\|medium\|q5"`, `\|H100\|1\|decode\|imedium\|medium\|q5"`,
			`\|decode\|medium\|q5#[0-9a-f]{16}"`, `\|decode\|medium\|q5#legacy:[0-9a-f]{16}"`,
			`\|decode\|medium\|q5#incomplete:[0-9a-f]{16}"`, `\|q5#legacy:incomplete:[0-9a-f]{16}"`,
			`\|H100\|1#[0-9a-f]{16}"`,
		} {
			assert.Regexp(t, kept, got, "namespace %q: the key's figures and code words are not kept", ns)
		}
		for _, name := range []string{"|v|mdl|", ns + "|v|", "mdl|v|"} {
			assert.NotContains(t, got, name, "namespace %q: a name survived", ns)
		}
	}
}

// A name in a window key's shape slot, in its stash tag, or in its count
// slot is refused, and the refusal names the site the key is at, never the
// name.
func TestExportLeak_NamesInAWindowKeysOtherSlots(t *testing.T) {
	for name, st := range map[string]saturation.LearnedState{
		"payrollrole": {K2: map[string]capacity.RollingAverageSnapshot{"ns|v|mdl|H100|1|payrollrole|medium|q5": {}}},
		"payrollsize": {K2: map[string]capacity.RollingAverageSnapshot{"ns|v|mdl|H100|1|decode|payrollsize|q5": {}}},
		"payrollq":    {K2: map[string]capacity.RollingAverageSnapshot{"ns|v|mdl|H100|1|decode|medium|payrollq": {}}},
		"payrollin":   {Throughput: map[string]saturation.ThroughputSnapshot{"ns|v|mdl|H100|1|decode|payrollin|medium|q5": {}}},
		"payrolltag":  {K2Stash: map[string]capacity.RollingAverageSnapshot{"ns|v|mdl|H100|1|decode|medium|q5#payrolltag": {}}},
		"payrollgpus": {K2: map[string]capacity.RollingAverageSnapshot{"ns|v|mdl|H100|payrollgpus|decode|medium|q5": {}}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := zzRec(t, zzOpts{before: func(_ *learnrec.Recorder, f *fixture) { f.sat.RestoreLearnedState(st) }})
			where := "a key of K2 in saturation.RestoreLearnedState"
			switch {
			case st.Throughput != nil:
				where = "a key of Throughput in saturation.RestoreLearnedState"
			case st.K2Stash != nil:
				where = "a key of K2Stash in saturation.RestoreLearnedState"
			}
			zzLeakAt(t, rec, where, name)
		})
	}
}

// zzLeakAt is zzLeak that accepts only a refusal naming where: a refusal for
// another reason would pass a leak test without testing the slot.
func zzLeakAt(t *testing.T, rec []byte, where string, secret string) {
	t.Helper()
	require.Contains(t, string(rec), secret, "precondition")
	out, _, err := Export(rec, ExportOptions{Key: []byte("k")})
	if err == nil {
		assert.NotContains(t, string(out), secret, "LEAK")
		t.Fatalf("the export committed a key with %q in a slot that holds no name", secret)
	}
	assert.NotContains(t, err.Error(), secret, "the refusal repeats the name")
	assert.True(t, strings.Contains(err.Error(), where), "a refusal that does not name %s: %v", where, err)
	t.Logf("REFUSED: %v", err)
}

// A recorded name that is all digits, or q and digits, is refused in a count
// or a queue slot, where a figure would pass: the verification probes P05 and
// P07 of #390. A small figure that is also a name (maxFigureDigits) still
// exports, since the controller writes those there.
func TestExportLeak_ANumericNameInAFigureSlot(t *testing.T) {
	for _, tc := range []struct{ name, ns, key string }{
		{"P05 digits in the count slot", "4155550123", "x|v|mdl|H100|4155550123|decode|medium|q5"},
		{"P07 q and digits in the queue slot", "q4155550123", "q4155550123|v|mdl|H100|1|decode|medium|q4155550123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := zzRec(t, zzOpts{ns: tc.ns, before: func(_ *learnrec.Recorder, f *fixture) {
				f.sat.RestoreLearnedState(saturation.LearnedState{K2: map[string]capacity.RollingAverageSnapshot{tc.key: {}}})
			}})
			zzLeakAt(t, rec, "a key of K2 in saturation.RestoreLearnedState", tc.ns)
		})
	}
	for _, ns := range []string{"8", "q5"} {
		_, err := exportState(t, ns, saturation.LearnedState{K2: map[string]capacity.RollingAverageSnapshot{
			ns + "|v|mdl|H100|8|decode|medium|q5": {}}})
		assert.NoError(t, err, "namespace %q: a small figure that is also a name", ns)
	}
}
