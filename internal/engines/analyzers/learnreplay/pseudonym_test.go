package learnreplay

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"slices"
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

// recordedFields walks recordedTypes and returns every string value's site
// ("<parent>.<field>", the parent "" at the top) and every string-keyed
// map's key rule name ("<field>" plus "{}" per map level inside it).
func recordedFields(t *testing.T) (values, keys map[string]string) {
	t.Helper()
	values, keys = map[string]string{}, map[string]string{}
	type visit struct {
		typ           reflect.Type
		field, parent string
		depth         int
	}
	seen := map[visit]bool{}
	var walk func(typ reflect.Type, field, parent string, depth int, path string)
	walk = func(typ reflect.Type, field, parent string, depth int, path string) {
		v := visit{typ, field, parent, depth}
		if seen[v] {
			return
		}
		seen[v] = true
		if typ.Kind() == reflect.String {
			if _, ok := values[parent+"."+field]; !ok {
				values[parent+"."+field] = path
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
				walk(set.Type.In(1), field, parent, depth, path+"<recorded>")
				return
			}
		}
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(typ.Elem(), field, parent, depth, path+"[]")
		case reflect.Map:
			if typ.Key().Kind() == reflect.String {
				name := field + strings.Repeat("{}", depth)
				if _, ok := keys[name]; !ok {
					keys[name] = path
				}
			}
			walk(typ.Elem(), field, parent, depth+1, path+"{}")
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				walk(f.Type, f.Name, field, 0, path+"."+f.Name)
			}
		}
	}
	for _, typ := range recordedTypes {
		field := ""
		if typ.Kind() == reflect.Map {
			field = "Records" // the capacity records an Analyze event carries
		}
		walk(typ, field, "", 0, typ.String())
	}
	return values, keys
}

// The export refuses a string it cannot place, at run time. This holds the
// tables to every string field and every string-keyed map a recorded event
// can reach, statically, so a field added to an input type fails here, in
// CI, and not on the first export of a cluster recording; and it fails on a
// table entry no field matches any more.
func TestExport_EveryRecordedStringIsClassified(t *testing.T) {
	values, keys := recordedFields(t)
	var unclassified []string
	usedValue := map[string]bool{}
	for site, path := range values {
		parent, field, _ := strings.Cut(site, ".")
		if _, ok := valueRules[parent+"."+field]; ok && parent != "" {
			usedValue[parent+"."+field] = true
			continue
		}
		if _, ok := valueRules[field]; ok {
			usedValue[field] = true
			continue
		}
		unclassified = append(unclassified, path)
	}
	usedKey := map[string]bool{}
	for name, path := range keys {
		if _, ok := keyRules[name]; ok {
			usedKey[name] = true
			continue
		}
		unclassified = append(unclassified, path+" (its keys)")
	}
	slices.Sort(unclassified)
	assert.Empty(t, unclassified, "strings the export does not classify")

	var stale []string
	for name := range valueRules {
		if !usedValue[name] {
			stale = append(stale, "valueRules "+name)
		}
	}
	for name := range keyRules {
		if !usedKey[name] && !strings.Contains(name, ".") {
			stale = append(stale, "keyRules "+name)
		}
	}
	slices.Sort(stale)
	assert.Empty(t, stale, "table entries no recorded field matches")
	// Pinned, so a new field is a reviewed change.
	t.Logf("%d string value sites, %d string-keyed maps", len(values), len(keys))
	assert.Len(t, values, 140, "string value sites")
	assert.Len(t, keys, 45, "string-keyed maps")
}

// canaryRecording is a small recording with one more event, of an owner the
// replay does not drive, holding data.
func canaryRecording(t *testing.T, data any) []byte {
	t.Helper()
	return mixedRecordingWith(t, func(r *learnrec.Recorder) { r.Record("composite.Canary", replayAt, data) })
}

// nest builds {A: {B: value}} for the site "A.B", {B: value} for "B", and
// {A: {"ns/v": {B: value}}} when A is always a map.
func nest(site string, value reflect.Value) any {
	parts := strings.Split(site, ".")
	v := value
	for i := len(parts) - 1; i >= 0; i-- {
		typ := reflect.StructOf([]reflect.StructField{{Name: parts[i], Type: v.Type()}})
		s := reflect.New(typ).Elem()
		s.Field(0).Set(v)
		v = s
		// A parent that is always a map holds the struct under a key.
		if i > 0 {
			if _, mapped := keyRules[parts[i-1]]; mapped && !structSites[parts[i-1]] {
				m := reflect.MakeMap(reflect.MapOf(reflect.TypeFor[string](), v.Type()))
				m.SetMapIndex(reflect.ValueOf("ns/v"), v)
				v = m
			}
		}
	}
	return v.Interface()
}

const canary = "zz-canary-7"

// outcomeSurvived is a canary the export committed.
const outcomeSurvived = "survived"

// canaryOutcome exports a recording whose canary stands at one site, and
// says what became of it: renamed, refused, or survived.
func canaryOutcome(t *testing.T, data any) string {
	rec := canaryRecording(t, data)
	require.Contains(t, string(rec), canary, "precondition: recorded")
	out, _, err := Export(rec, ExportOptions{})
	switch {
	case err != nil:
		return "refused"
	case bytes.Contains(out, []byte(canary)):
		return outcomeSurvived
	default:
		return "renamed"
	}
}

// Every classified field and map holds a canary in turn: each is renamed or
// refused: none keeps it.
func TestExport_ACanaryInEachFieldIsRenamedOrRefused(t *testing.T) {
	survived := map[string]bool{}
	outcomes := map[string]int{}
	for site, r := range valueRules {
		o := canaryOutcome(t, nest(site, reflect.ValueOf(canary)))
		outcomes[o]++
		if o == outcomeSurvived {
			survived[site] = true
		}
		if r.class == classRename || r.class == classLabelKey || r.class == classResource || r.class == classAccelerator {
			assert.Equal(t, "renamed", o, "%s", site)
		}
	}
	for name, r := range keyRules {
		if strings.Contains(name, ".") {
			continue // a whole event's data, not a field
		}
		field := strings.TrimRight(name, "{}")
		depth := (len(name) - len(field)) / 2
		v := reflect.ValueOf(map[string]int{canary: 1})
		for range depth {
			outer := reflect.MakeMap(reflect.MapOf(reflect.TypeFor[string](), v.Type()))
			outer.SetMapIndex(reflect.ValueOf("default"), v)
			v = outer
		}
		o := canaryOutcome(t, nest(field, v))
		outcomes[o]++
		if o == outcomeSurvived {
			survived["key of "+name] = true
		}
		if r.class == classVocab {
			assert.Equal(t, "refused", o, "a key of %s", name)
		} else {
			assert.Equal(t, "renamed", o, "a key of %s", name)
		}
	}
	assert.Equal(t, map[string]bool{},
		survived, "what keeps a canary")
	t.Logf("outcomes: %v", outcomes)
}

// A string under a field no table names stops the export, whatever event
// carries it, and the refusal names the field and never the value.
func TestExport_RefusesAnUnclassifiedField(t *testing.T) {
	rec := canaryRecording(t, struct{ Cluster string }{"east-prod"})
	_, _, err := Export(rec, ExportOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "(the event data) in composite.Canary is a string field the export does not classify")
	assert.NotContains(t, err.Error(), "east-prod")

	rec = canaryRecording(t, nest("Data.Region", reflect.ValueOf("east-prod")))
	_, _, err = Export(rec, ExportOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "in composite.Canary")
	assert.NotContains(t, err.Error(), "east-prod")
}

// A host name, an IP address and a URL in a message are replaced; a message
// word that is still not allowed stops the export.
func TestExport_MessagesAreScrubbedOrRefused(t *testing.T) {
	assert.Equal(t, "Get URL dial IP refused for HOST, IP",
		scrub("Get https://thanos.acme.example.com/api: dial 10.20.30.40:443 refused for svc.mdl.cluster.local, [fd00::1]:9090"))

	// A message of the code's own words, names and numbers is renamed and
	// kept; PAYROLL is the name payroll, whatever its case.
	rec := probeRecording(t, "payroll", "mdl", []string{"v"}, nil, func(r *learnrec.Recorder, _ *fixture) {
		r.Record("composite.Plan", replayAt, struct{ Error string }{"variant v of PAYROLL has no line after 3 cycles"})
	})
	out, _, err := Export(rec, ExportOptions{})
	require.NoError(t, err)
	assert.Contains(t, string(out), `"Error":"variant variant-01 of ns-01 has no line after 3 cycles"`)

	// Any other word stops the export, and the refusal does not repeat it.
	for _, msg := range []string{
		"quota of team-payroll2 exhausted",
		"lookup prometheus in somewhere failed: team=finance",
		"quota of payrolls exhausted",
	} {
		rec = canaryRecording(t, struct{ Error string }{msg})
		_, _, err = Export(rec, ExportOptions{})
		require.Error(t, err, msg)
		assert.Contains(t, err.Error(), "Error in composite.Canary holds a value that is not allowed")
		assert.NotContains(t, err.Error(), "finance")
	}
}

// Names keep their order within a category, a renamed map is re-sorted as
// the recorder writes one, composite keys and messages are renamed inside,
// and a fingerprint with no recorded parameters behind it is replaced by a
// hash under the export's key.
func TestExport_RenamesInOrderAndKeysWhatItCannotRecompute(t *testing.T) {
	orphan := "0123456789abcdef"
	rec := canaryRecording(t, struct {
		Variants []string
		Records  map[string]int
		Error    string
		Args     map[string]string
	}{
		Variants: []string{"zeta", "alpha"},
		Records:  map[string]int{"zeta": 1, "alpha": 2},
		Error:    "variant zeta of mdl in ns has no line",
		Args:     map[string]string{"ns/alpha": orphan},
	})
	out, rep, err := Export(rec, ExportOptions{Key: []byte("k1")})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"ns": 1, "model": 1, "variant": 4, "image": 1}, rep.Names)
	last := canaryLine(out)
	// alpha < old < v < zeta: their ranks keep that order.
	assert.Contains(t, last, `"Variants":["variant-04","variant-01"]`)
	assert.Contains(t, last, `"Records":{"variant-01":2,"variant-04":1}`)
	assert.Contains(t, last, `"Error":"variant variant-04 of model-01 in ns-01 has no line"`)
	assert.NotContains(t, string(out), orphan)
	assert.Equal(t, 1, rep.KeyedFingerprints)
	assert.Regexp(t, `"Args":\{"ns-01/variant-01":"[0-9a-f]{16}"\}`, last)

	same, _, err := Export(rec, ExportOptions{Key: []byte("k1")})
	require.NoError(t, err)
	assert.Equal(t, string(out), string(same), "one key, one export")
	other, _, err := Export(rec, ExportOptions{Key: []byte("k2")})
	require.NoError(t, err)
	assert.NotEqual(t, canaryLine(out), canaryLine(other), "the keyed hash ignored its key")
	assert.Equal(t, otherLines(out), otherLines(other), "the key moved what it does not key")
	derived, _, err := Export(rec, ExportOptions{})
	require.NoError(t, err)
	again, _, err := Export(rec, ExportOptions{})
	require.NoError(t, err)
	assert.Equal(t, string(derived), string(again), "a derived key is not deterministic")

	r, err := Replay(context.Background(), out, Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.Empty(t, r.Mismatches)

}

// The edges of naming.
func TestExport_Paths(t *testing.T) {
	t.Run("no start line", func(t *testing.T) {
		rec := mixedRecordingWith(t, func(*learnrec.Recorder) {})
		lines := strings.SplitAfter(string(rec), "\n")
		_, _, err := Export([]byte(strings.Join(lines[1:], "")), ExportOptions{})
		require.Error(t, err)
	})
	t.Run("no Analyze", func(t *testing.T) {
		var buf bytes.Buffer
		r := learnrec.New(&buf, 0)
		r.Record(learnrec.KindStart, replayAt, defaultStart())
		_, _, err := Export(buf.Bytes(), ExportOptions{})
		require.ErrorContains(t, err, "no Analyze event")
	})
	t.Run("the events after the last Analyze are cut, and a torn tail dropped", func(t *testing.T) {
		rec := mixedRecordingWith(t, func(*learnrec.Recorder) {})
		after, err := learnrec.Line(uint64(bytes.Count(rec, []byte("\n"))+1), replayAt, "composite.Canary", []byte(`{}`))
		require.NoError(t, err)
		torn := append(append(bytes.Clone(rec), after...), []byte(`{"Seq":99,"No`)...)
		out, rep, err := Export(torn, ExportOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, rep.Trimmed)
		assert.True(t, rep.TornTail)
		assert.NotContains(t, string(out), "composite.Canary")
	})
	t.Run("digits are a name except as a revision", func(t *testing.T) {
		rec := probeRecording(t, "4711", "mdl", []string{"v"}, nil, nil)
		out, rep, err := Export(rec, ExportOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, rep.Names["ns"])
		assert.NotContains(t, string(out), `"4711"`)
		assert.NotContains(t, string(out), `4711|`)
	})
	t.Run("a namespace named like an accelerator is still a namespace", func(t *testing.T) {
		rec := probeRecording(t, "h100", "mdl", []string{"v"}, nil, nil)
		out, _, err := Export(rec, ExportOptions{})
		require.NoError(t, err)
		assert.NotContains(t, string(out), `"h100"`)
	})
	t.Run("a name under two categories takes the first", func(t *testing.T) {
		rec := probeRecording(t, "same", "same", []string{"v"}, nil, nil)
		out, rep, err := Export(rec, ExportOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, rep.Names["ns"])
		assert.Equal(t, 0, rep.Names["model"])
		assert.Contains(t, string(out), `"ModelID":"ns-01"`)
	})
	t.Run("a name shaped like another name's pseudonym is refused", func(t *testing.T) {
		rec := probeRecording(t, "ns", "mdl", []string{"variant-02", "a", "b"}, nil, nil)
		_, _, err := Export(rec, ExportOptions{})
		require.ErrorContains(t, err, "a name shaped like a pseudonym")
	})
	t.Run("an export exports again unchanged", func(t *testing.T) {
		rec := probeRecording(t, "ns", "mdl", []string{"v"}, nil, nil)
		once, _, err := Export(rec, ExportOptions{Key: []byte("k")})
		require.NoError(t, err)
		twice, _, err := Export(once, ExportOptions{Key: []byte("k")})
		require.NoError(t, err)
		assert.Equal(t, string(once), string(twice))
	})
	t.Run("a hundred names pad to three digits", func(t *testing.T) {
		vs := make([]string, 0, 100)
		for i := range 100 {
			vs = append(vs, fmt.Sprintf("v%03d", i))
		}
		rec := probeRecording(t, "ns", "mdl", vs, nil, nil)
		out, rep, err := Export(rec, ExportOptions{})
		require.NoError(t, err)
		assert.Equal(t, 100, rep.Names["variant"])
		assert.Contains(t, string(out), `"variant-001"`)
		assert.Contains(t, string(out), `"variant-100"`)
	})
	t.Run("a name is replaced only as a whole word", func(t *testing.T) {
		rec := canaryRecording(t, struct{ Error string }{"ns pending"})
		out, _, err := Export(rec, ExportOptions{})
		require.NoError(t, err)
		assert.Contains(t, canaryLine(out), `"Error":"ns-01 pending"`)
	})
	t.Run("text to commit beside the export may not hold a name", func(t *testing.T) {
		rec := probeRecording(t, "payroll", "mdl", []string{"v"}, nil, nil)
		_, _, err := Export(rec, ExportOptions{Text: []string{"E1 on payroll"}})
		require.ErrorContains(t, err, "holds a recorded name")
	})
}

// canaryLine is the composite.Canary event's line of an export.
func canaryLine(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, `"Kind":"composite.Canary"`) {
			return l
		}
	}
	return ""
}

// otherLines is every line of an export but the composite.Canary event's.
func otherLines(b []byte) string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.Contains(l, `"Kind":"composite.Canary"`) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// mixedRecordingWith is a start, two filed variants and one saturation
// Analyze, with add's events recorded just before the Analyze, so the export
// keeps them.
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
	add(rec)
	_, err := f.sat.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "mdl", Namespace: "ns",
		Config:        &config.ScalingPolicy{KvCacheThreshold: 0.8},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", GPUsPerReplica: 1, AcceleratorName: "H100"}}})
	require.NoError(t, err)
	require.NoError(t, rec.Err())
	return bytes.Clone(f.buf.Bytes())
}
