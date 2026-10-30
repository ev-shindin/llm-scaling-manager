package learnreplay

import (
	"bytes"
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The export refuses a string under a field it has not classified, at run
// time. This holds the tables to every string field a recorded event can
// reach, statically, so a field added to an input type fails here, in CI,
// and not on the first export of a cluster recording.
func TestExport_EveryRecordedStringFieldIsClassified(t *testing.T) {
	seen := map[reflect.Type]bool{}
	fields := map[string]string{}
	var walk func(typ reflect.Type, field, path string)
	walk = func(typ reflect.Type, field, path string) {
		if typ.Kind() == reflect.String {
			if _, ok := fields[field]; !ok {
				fields[field] = path
			}
			return
		}
		if typ == reflect.TypeFor[time.Time]() || typ == reflect.TypeFor[learnrec.Raw]() {
			return
		}
		if typ.Kind() != reflect.Pointer && typ.Kind() != reflect.Interface {
			if _, ok := typ.MethodByName("RecordedValue"); ok {
				set, ok := reflect.PointerTo(typ).MethodByName("SetRecordedValue")
				require.True(t, ok, "%s: RecordedValue without SetRecordedValue", path)
				walk(set.Type.In(1), field, path+"<recorded>")
				return
			}
		}
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(typ.Elem(), field, path+"[]")
		case reflect.Map:
			walk(typ.Elem(), field, path+"{}")
		case reflect.Struct:
			// A struct type is walked once; its fields carry their own names.
			if seen[typ] {
				return
			}
			seen[typ] = true
			for i := range typ.NumField() {
				f := typ.Field(i)
				walk(f.Type, f.Name, path+"."+f.Name)
			}
		}
	}
	for _, typ := range recordedTypes {
		walk(typ, "", typ.String())
	}
	require.Greater(t, len(fields), 50, "the walk found only %d string fields; it is not walking what the events hold", len(fields))
	var unclassified []string
	for f, path := range fields {
		_, renamed := renamedFields[f]
		if !renamed && !keptFields[f] && !textFields[f] {
			unclassified = append(unclassified, path)
		}
	}
	sort.Strings(unclassified)
	assert.Empty(t, unclassified, "string fields the export does not classify")
	t.Logf("%d string fields, each classified", len(fields))
}

// A string under a field no table names stops the export, whatever event
// carries it: an event of an owner the replay does not drive included.
func TestExport_RefusesAnUnclassifiedField(t *testing.T) {
	rec := mixedRecordingWith(t, func(r *learnrec.Recorder) {
		r.Record("composite.Plan", replayAt, struct{ Cluster string }{"east"})
	})
	_, _, err := Export(rec, ExportOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the string field Cluster of composite.Plan is not classified")
	assert.NotContains(t, err.Error(), "east", "the error repeats the value it refused")
}

// A name the replacement passes over -- here inside a host name, where the
// dots join it to its neighbours -- is still found, and nothing is written.
func TestExport_RefusesWhenANameSurvives(t *testing.T) {
	rec := mixedRecordingWith(t, func(r *learnrec.Recorder) {
		r.Record("composite.Plan", replayAt, struct{ Error string }{"dial svc.ns.cluster.local: refused"})
	})
	out, _, err := Export(rec, ExportOptions{})
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Contains(t, err.Error(), "the names behind ns-01 survive in the export")
}

// Names keep their order within a category, a renamed map is re-sorted as
// the recorder writes one, composite keys and messages are renamed inside,
// and a fingerprint with no recorded parameters behind it is replaced by a
// hash under the export's key.
func TestExport_RenamesInOrderAndKeysWhatItCannotRecompute(t *testing.T) {
	orphan := "0123456789abcdef"
	rec := mixedRecordingWith(t, func(r *learnrec.Recorder) {
		r.Record("composite.Plan", replayAt, struct {
			Variants []string
			ByName   map[string]int
			Error    string
			Args     map[string]string
		}{
			Variants: []string{"zeta", "alpha"},
			ByName:   map[string]int{"ns|mdl|zeta": 1, "ns|mdl|alpha": 2},
			Error:    "variant zeta of mdl in ns has no line",
			Args:     map[string]string{"ns/alpha": orphan},
		})
	})
	out, rep, err := Export(rec, ExportOptions{Key: []byte("k1")})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"ns": 1, "model": 1, "variant": 4, "image": 1}, rep.Names)
	last := lastLine(out)
	// alpha < old < v < zeta: their ranks keep that order.
	assert.Contains(t, last, `"Variants":["variant-04","variant-01"]`)
	assert.Contains(t, last, `"ByName":{"ns-01|model-01|variant-01":2,"ns-01|model-01|variant-04":1}`)
	assert.Contains(t, last, `"Error":"variant variant-04 of model-01 in ns-01 has no line"`)
	assert.NotContains(t, string(out), orphan)
	assert.Equal(t, 1, rep.KeyedFingerprints)

	same, _, err := Export(rec, ExportOptions{Key: []byte("k1")})
	require.NoError(t, err)
	assert.Equal(t, string(out), string(same), "one key, one export")
	other, _, err := Export(rec, ExportOptions{Key: []byte("k2")})
	require.NoError(t, err)
	assert.NotEqual(t, lastLine(out), lastLine(other), "the keyed hash ignored its key")
	assert.Equal(t, firstLines(out), firstLines(other), "the key moved what it does not key")

	// The export still replays at this commit.
	r, err := Replay(context.Background(), out, Options{Mode: Fidelity})
	require.NoError(t, err)
	require.Empty(t, r.Failed())
	assert.Empty(t, r.Mismatches)
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

func firstLines(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.Join(lines[:len(lines)-1], "\n")
}

// mixedRecordingWith is a start, two filed variants, one saturation Analyze,
// then whatever add records.
func mixedRecordingWith(t *testing.T, add func(*learnrec.Recorder)) []byte {
	t.Helper()
	f := newFixture(replayAt)
	f.buf.Reset()
	rec := learnrec.New(f.buf, 0)
	f.attach(rec)
	rec.Record(learnrec.KindStart, replayAt, defaultStart())
	params := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, ModelPath: "mdl", Image: "img:1"}
	f.store.FileScaleTarget("ns", "mdl", "old", "H100", 1, params, false, replayAt)
	f.store.FileScaleTarget("ns", "mdl", "v", "H100", 1, params, false, replayAt)
	_, err := f.sat.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "mdl", Namespace: "ns",
		Config:        &config.ScalingPolicy{KvCacheThreshold: 0.8},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", GPUsPerReplica: 1, AcceleratorName: "H100"}}})
	require.NoError(t, err)
	add(rec)
	require.NoError(t, rec.Err())
	return bytes.Clone(f.buf.Bytes())
}
