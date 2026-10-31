package learnreplay

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// fillNonZero sets every exported field reachable from v to a non-zero value
// distinct per path: strings name their path, numbers count up, slices and
// maps hold one element, pointers point at a filled value. Interfaces are
// left for the caller.
func fillNonZero(t *testing.T, v reflect.Value, path string, n *int) {
	t.Helper()
	*n++
	switch v.Kind() {
	case reflect.String:
		v.SetString(fmt.Sprintf("s%d", *n))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(*n))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(*n))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(*n) + 0.5)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillNonZero(t, p.Elem(), path+"*", n)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(t, s.Index(0), path+"[]", n)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fillNonZero(t, k, path+"{key}", n)
		e := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(t, e, path+"{}", n)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(time.Date(2026, 3, 2, 9, 0, *n, 0, time.UTC)))
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillNonZero(t, v.Field(i), path+"."+v.Type().Field(i).Name, n)
			}
		}
	case reflect.Interface:
		// The caller's.
	default:
		t.Fatalf("%s: a %s the filler does not know", path, v.Kind())
	}
}

// Every field of the analyzer input survives the recording and a replay's
// reading of it: InputOf, Marshal, decodeInput. Each field is non-zero, so a
// field InputOf or decodeInput drops, or reads as another type, fails.
//
// VariantMetadata is the one field not recorded: no analyzer reads it yet.
// The exemption is revisited at #345 step 2, which takes the composite's
// variants from it; the step that first has an analyzer read it must record
// it and end the exemption, or that analyzer cannot be replayed.
func TestInput_EveryAnalyzerInputFieldSurvivesTheRecording(t *testing.T) {
	var in domain.AnalyzerInput
	n := 0
	fillNonZero(t, reflect.ValueOf(&in).Elem(), "AnalyzerInput", &n)
	in.Config = &config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.7, KvCacheThreshold: 0.8}
	require.NotEmpty(t, in.VariantMetadata, "precondition: the filler reached VariantMetadata")

	raw, err := learnrec.Marshal(learnrec.InputOf(in))
	require.NoError(t, err)
	got, err := decodeInput(learnrec.Raw(raw))
	require.NoError(t, err)

	want := in
	want.VariantMetadata = nil
	assert.Equal(t, want, got)
	// The comparison covered every field: each one of the input is set.
	typ := reflect.TypeFor[domain.AnalyzerInput]()
	for i := range typ.NumField() {
		assert.False(t, reflect.ValueOf(in).Field(i).IsZero(), "%s was left zero", typ.Field(i).Name)
	}
}
