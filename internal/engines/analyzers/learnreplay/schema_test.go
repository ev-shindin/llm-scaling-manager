package learnreplay

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The recorder refuses a struct with an unexported field at run time, and
// stops the recording on it. That is safe, and it is late: a field added to
// an input type stops every recording on its first cycle, on a cluster. This
// walks every type a recorded event can reach (recordedTypes), statically,
// and fails on an unexported field whose type does not say how to record it
// (RecordedValue and SetRecordedValue).
func TestSchema_NoRecordedTypeHasAnUnreadableField(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var bad []string
	walked := 0
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		if seen[typ] {
			return
		}
		seen[typ] = true
		walked++
		if typ == reflect.TypeFor[time.Time]() || typ == reflect.TypeFor[learnrec.Raw]() {
			return
		}
		if typ.Kind() != reflect.Pointer && typ.Kind() != reflect.Interface {
			if _, ok := typ.MethodByName("RecordedValue"); ok {
				set, ok := reflect.PointerTo(typ).MethodByName("SetRecordedValue")
				if !ok || set.Type.NumIn() != 2 {
					bad = append(bad, path+" ("+typ.String()+"): RecordedValue without SetRecordedValue")
					return
				}
				walk(set.Type.In(1), path+"<recorded>")
				return
			}
		}
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(typ.Elem(), path+"[]")
		case reflect.Map:
			walk(typ.Key(), path+"{key}")
			walk(typ.Elem(), path+"{}")
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				if !f.IsExported() {
					bad = append(bad, path+"."+f.Name+" ("+typ.String()+"): unexported, and the type has no RecordedValue")
					continue
				}
				walk(f.Type, path+"."+f.Name)
			}
		case reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Complex64, reflect.Complex128:
			bad = append(bad, path+" ("+typ.String()+"): a kind the recorder cannot write")
		}
	}
	for _, typ := range recordedTypes {
		walk(typ, typ.String())
	}
	sort.Strings(bad)
	if walked < 100 {
		t.Fatalf("the walk reached only %d types; it is not walking what the events hold", walked)
	}
	for _, b := range bad {
		t.Error(b)
	}
	t.Logf("walked %d types", walked)
}
