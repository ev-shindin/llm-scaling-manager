package learnreplay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The third verification of PR #388: messages were checked run of letters
// by run of letters, so a token such as "tenants-production" passed because
// "tenants" and "production" are each words of the code. Each case failed
// at 6a79e02df. A message is now checked whole token by whole token.
func TestExportLeak3_MessageTokens(t *testing.T) {
	for name, c := range map[string]struct {
		ns, msg string
		secret  string
	}{
		"a pod of a recorded name":      {"llm-inference", "pod llm-inference-7f9c4 pending", "llm-inference"},
		"a name with digits joined":     {"production", "pod production17 failed", "production17"},
		"a token of code words":         {"ns", "lender tenants-production pending", "tenants-production"},
		"an unrecorded pod-like name":   {"ns", "pod gpu-production-17 pending", "gpu-production-17"},
		"a host with a port":            {"ns", "pod gpu-node.local:8080 pending", "gpu-node.local"},
		"a three-label host":            {"ns", "pod inference.cloud.local pending", "inference.cloud.local"},
		"Cyrillic":                      {"ns", "pod Ромашка pending", "Ромашка"},
		"CJK":                           {"ns", "pod 財務部 pending", "財務部"},
		"a homoglyph":                   {"ns", "pod асmе pending", "асmе"},
		"a composite with a stray part": {"ai-production", "key ai-production|shadow-llm|core|H100|1 stale", "shadow-llm"},
		"a composite of strays":         {"ns", "key ai-production|shadow-llm stale", "ai-production|shadow-llm"},
	} {
		t.Run(name, func(t *testing.T) {
			zzLeak(t, zzRec(t, zzOpts{ns: c.ns, before: func(r *learnrec.Recorder, _ *fixture) {
				plan(r, struct{ Error string }{c.msg})
			}}), c.secret)
		})
	}
	t.Run("issues", func(t *testing.T) {
		zzLeak(t, zzRec(t, zzOpts{ns: "production", before: func(r *learnrec.Recorder, _ *fixture) {
			plan(r, struct{ Issues []string }{[]string{"pod production-0 pending"}})
		}}), "production-0")
	})
}

// A pod of a recorded name is renamed as the name's pseudonym with the
// pod's suffix, and kept.
func TestExportLeak3_APodOfARecordedNameIsRenamed(t *testing.T) {
	rec := zzRec(t, zzOpts{ns: "llm-inference", before: func(r *learnrec.Recorder, _ *fixture) {
		plan(r, struct{ Error string }{"pod llm-inference-7f9c4 pending"})
	}})
	out, _, err := Export(rec, ExportOptions{Key: []byte("k")})
	require.NoError(t, err)
	assert.Contains(t, string(out), `"Error":"pod ns-01-7f9c4 pending"`)
}

// A source is short and of ordinary words; the example passes, and any
// other word is refused by name.
func TestExportLeak3_SourceWords(t *testing.T) {
	assert.Empty(t, sourceRefusals(SourceExample))
	assert.Equal(t, []string{"word 4"}, sourceRefusals("E1 run on zorblax"))
	assert.Equal(t, []string{"a non-ASCII character"}, sourceRefusals("E1 Ромашка"))
	for w := range sourceWords {
		assert.Equal(t, asciiLower(w), w, "source_words.txt holds a capital: %q", w)
		assert.False(t, pseudonymWithSuffix.MatchString(w) && !randomSegment.MatchString(w), "%q is shaped like a pseudonym", w)
	}
	rec := zzRec(t, zzOpts{ns: "payroll"})
	_, _, err := Export(rec, ExportOptions{Source: "E1 benchmark on zorblax"})
	require.ErrorContains(t, err, "not ordinary words (word 4)")
	_, _, err = Export(rec, ExportOptions{Source: SourceExample})
	require.NoError(t, err)
}
