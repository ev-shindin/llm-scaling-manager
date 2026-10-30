package learnreplay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The final check of PR #388 at db054bd31: a hyphenated recorded name was
// accepted as a fixture name, a name split by a character no token rule
// splits at ('@') or glued to letters inside a pod suffix survived, and the
// source refusal repeated the word it refused. The backstop (holdsName) now
// refuses any recorded name of four characters or more left anywhere.
func TestExportLeak5_Messages(t *testing.T) {
	for _, c := range []struct{ name, ns, model, msg, secret string }{
		{"a name split at @", "ns", "scale@demand", "model scale@demand pending", "scale@demand"},
		{"a name glued to letters in a suffix", "kpmg", "payroll", "pod payroll-kpmgx pending", "kpmg"},
		{"a name inside a hash", "kpmg", "payroll", "pod payroll-7kpmg2bc pending", "kpmg"},
		{"a name glued to digits inside a suffix", "kpmg", "payroll", "pod payroll-2kpmg7 pending", "kpmg"},
		{"overlapping names", "payroll-team", "team-x7k2p", "pod payroll-team-x7k2p pending", "x7k2p"},
		{"overlapping names, digits", "payroll-team", "team-20245", "pod payroll-team-20245 pending", "20245"},
		{"a dotted model", "ns", "acme.payroll", "pod acme.payroll-x7k2p pending", "payroll"},
		{"a dotted model in capitals, glued", "ns", "acme.payroll", "pod ACME.PAYROLL7-x7k2p pending", "PAYROLL"},
		{"a name glued in a composite", "payroll", "mdl", "key payroll7|mdl|v|H100|1 pending", "payroll"},
		{"a model path", "ns", "/models/payroll-7b", "load /models/payroll-7b/config pending", "payroll"},
	} {
		t.Run(c.name, func(t *testing.T) {
			zzLeak(t, zzRec(t, zzOpts{ns: c.ns, model: c.model, before: func(r *learnrec.Recorder, _ *fixture) {
				plan(r, struct{ Error string }{c.msg})
			}}), c.secret)
		})
	}
}

// A hyphenated recorded name is refused as a fixture's name, whole or as
// parts of one.
func TestExportLeak5_FixtureNames(t *testing.T) {
	rec := zzRec(t, zzOpts{ns: "scale-test"})
	for _, name := range []string{"e1-scale-test", "scale-test", "scale-test-e1"} {
		_, _, err := Export(rec, ExportOptions{Text: []string{name}})
		require.Error(t, err, name)
		assert.NotContains(t, err.Error(), "scale", name)
	}
}

// A source refusal says where, never what.
func TestExportLeak5_SourceRefusalsDoNotRepeat(t *testing.T) {
	rec := zzRec(t, zzOpts{ns: "payroll"})
	for _, src := range []string{"E1 run payroll7", "E1 run 2payroll", "E1 run payrollrun", "E1 run zorblax"} {
		_, _, err := Export(rec, ExportOptions{Source: src})
		require.Error(t, err, src)
		assert.NotContains(t, err.Error(), "payroll", src)
		assert.NotContains(t, err.Error(), "zorblax", src)
	}
}

// The backstop lets through a code word that equals a recorded name, as a
// whole value: a namespace named decode, beside the role decode.
func TestExportLeak5_ACodeWordEqualToANameIsKept(t *testing.T) {
	rec := zzRec(t, zzOpts{ns: "decode", before: func(r *learnrec.Recorder, _ *fixture) {
		plan(r, struct{ Role string }{"decode"})
	}})
	out, _, err := Export(rec, ExportOptions{Key: []byte("k")})
	require.NoError(t, err)
	assert.Contains(t, string(out), `"Role":"decode"`)
}
