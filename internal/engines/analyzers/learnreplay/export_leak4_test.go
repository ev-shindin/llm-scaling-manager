package learnreplay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The fourth verification of PR #388: at 3961e5c91 a recorded name was
// renamed only at the start of a token, a token starting with ':' or '/'
// was not renamed, any vowel-free or hex segment passed after a pseudonym,
// and a fixture's name was checked only for whole names. Each case below
// leaked there.
func TestExportLeak4_MessageSegments(t *testing.T) {
	for _, c := range []struct{ name, ns, model, msg, secret string }{
		{"L1 a recorded model after a pod hash", "llm-inference", "dbrx", "pod llm-inference-7f9c4-dbrx pending", "dbrx"},
		{"L1 a recorded hex model after an ordinal", "payroll", "cafe", "pod payroll-0-cafe pending", "cafe"},
		{"L1 a recorded namespace after a model's pod hash", "kpmg", "llama-x", "pod llama-x-7f9c4-kpmg pending", "kpmg"},
		{"L3 an acronym suffix", "payroll", "mdl", "pod payroll-hsbc pending", "hsbc"},
		{"L3 a hex-shaped suffix", "payroll", "mdl", "pod payroll-ada pending", "ada"},
		{"L3 a chain of suffixes", "payroll", "mdl", "pod payroll-jpmc-kpmg pending", "jpmc"},
		{"L3 a suffix after an upper-case name", "payroll", "mdl", "pod PAYROLL-hsbc pending", "hsbc"},
		{"L4 a name after a colon", "demand", "mdl", "pod :demand pending", "demand"},
		{"L4 a name after a slash", "bridge", "mdl", "pod /bridge pending", "bridge"},
	} {
		t.Run(c.name, func(t *testing.T) {
			zzLeak(t, zzRec(t, zzOpts{ns: c.ns, model: c.model, before: func(r *learnrec.Recorder, _ *fixture) {
				plan(r, struct{ Error string }{c.msg})
			}}), c.secret)
		})
	}
}

// A real pod name of a recorded variant is still renamed and kept: an
// ordinal, a generated five-character suffix, a ReplicaSet hash and suffix.
func TestExportLeak4_RealPodSuffixesAreKept(t *testing.T) {
	rec := zzRec(t, zzOpts{ns: "payroll", before: func(r *learnrec.Recorder, _ *fixture) {
		plan(r, struct{ Error string }{"pod payroll-0 payroll-x7k2p payroll-7c9dfb6d84-x7k2p pending"})
	}})
	out, _, err := Export(rec, ExportOptions{Key: []byte("k")})
	require.NoError(t, err)
	assert.Contains(t, string(out), `"Error":"pod ns-01-0 ns-01-x7k2p ns-01-7c9dfb6d84-x7k2p pending"`)
}

// L2: a fixture's name is held part by part.
func TestExportLeak4_FixtureNames(t *testing.T) {
	rec := zzRec(t, zzOpts{ns: "payroll"})
	for _, name := range []string{"payroll2-e1", "e1-payrollrun", "2payroll", "payroll-e1", "acmecorp-e1"} {
		_, _, err := Export(rec, ExportOptions{Text: []string{name}})
		require.Error(t, err, name)
		assert.NotContains(t, err.Error(), "payroll", name)
	}
	for _, name := range []string{"e1", "e1-tuned", "acceptance"} {
		_, _, err := Export(rec, ExportOptions{Text: []string{name}})
		require.NoError(t, err, name)
	}
}
