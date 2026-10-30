package learnrec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"time"
)

// The recorder's own encoding, and its decoder.
//
// encoding/json cannot carry what the analyzers read: it refuses NaN and
// ±Inf, which the collector and the analyzers both produce on ordinary
// cycles, and it would drop the whole line. So the recorder writes JSON with
// a fixed float format of its own:
//
//   - a finite float is a JSON number, strconv's shortest form that reads
//     back to the same bits ('g', -1);
//   - NaN, +Inf and -Inf are the strings "NaN", "+Inf" and "-Inf";
//   - a time is a string, RFC 3339 with nanoseconds, in UTC;
//   - a struct is an object of its exported fields in declaration order,
//     named as Go names them (json tags are ignored: the schema is the Go
//     type, so a tag cannot hide a field from a recording);
//   - a map is an object with its keys sorted, so no output depends on map
//     order;
//   - nil and empty are kept apart: a nil map, slice or pointer is null.
//
// A struct with an unexported field is refused rather than written without
// it: a recording that silently loses an input replays a controller that
// never ran. time.Time is the one exception, written as above.

var (
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(Raw(nil))
)

// Raw is a value already encoded by Marshal, written as it is, and read back
// as the exact bytes it was written with. An event encodes what a call read
// before the call runs, so nothing the call does to it can reach the
// recording; Recorder.Fragment makes one, and Decode reads it back.
type Raw []byte

// Decode decodes r into the value v points to.
func (r Raw) Decode(v any) error {
	if r == nil {
		return Unmarshal([]byte("null"), v)
	}
	return Unmarshal(r, v)
}

// recordedValuer is a type that keeps its state behind unexported fields and
// says how a recording should carry it: RecordedValue is what is written.
// Reading it back takes a pointer method
//
//	SetRecordedValue(v X) error
//
// where X is the type RecordedValue's value decodes into. The methods are
// found by name, so the type's package need not import this one.
type recordedValuer interface {
	RecordedValue() any
}

var (
	recordedValuerType = reflect.TypeFor[recordedValuer]()
	errorType          = reflect.TypeFor[error]()
)

// recordedSetter is t's SetRecordedValue, with the type it takes, when *t
// has one of the right shape.
func recordedSetter(t reflect.Type) (reflect.Method, reflect.Type, bool) {
	m, ok := reflect.PointerTo(t).MethodByName("SetRecordedValue")
	if !ok || m.Type.NumIn() != 2 || m.Type.NumOut() != 1 || m.Type.Out(0) != errorType {
		return reflect.Method{}, nil, false
	}
	return m, m.Type.In(1), true
}

// Marshal encodes v in the recorder's canonical form.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encode(&buf, reflect.ValueOf(v)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encode(buf *bytes.Buffer, v reflect.Value) error {
	if !v.IsValid() {
		buf.WriteString("null")
		return nil
	}
	if v.Kind() != reflect.Interface && v.Kind() != reflect.Pointer && v.Type().Implements(recordedValuerType) {
		return encode(buf, reflect.ValueOf(v.Interface().(recordedValuer).RecordedValue()))
	}
	switch v.Type() {
	case timeType:
		t := v.Interface().(time.Time)
		writeString(buf, t.UTC().Format(time.RFC3339Nano))
		return nil
	case rawType:
		if v.IsNil() {
			buf.WriteString("null")
		} else {
			buf.Write(v.Bytes())
		}
		return nil
	}
	switch v.Kind() {
	case reflect.Bool:
		buf.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buf.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		buf.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		writeFloat(buf, v.Float(), v.Type().Bits())
	case reflect.String:
		writeString(buf, v.String())
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return encode(buf, v.Elem())
	case reflect.Slice:
		if v.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return encodeList(buf, v)
	case reflect.Array:
		return encodeList(buf, v)
	case reflect.Map:
		if v.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return encodeMap(buf, v)
	case reflect.Struct:
		return encodeStruct(buf, v)
	default:
		return fmt.Errorf("learnrec: cannot encode %s", v.Type())
	}
	return nil
}

func writeFloat(buf *bytes.Buffer, f float64, bits int) {
	switch {
	case math.IsNaN(f):
		buf.WriteString(`"NaN"`)
	case math.IsInf(f, 1):
		buf.WriteString(`"+Inf"`)
	case math.IsInf(f, -1):
		buf.WriteString(`"-Inf"`)
	default:
		buf.WriteString(strconv.FormatFloat(f, 'g', -1, bits))
	}
}

func writeString(buf *bytes.Buffer, s string) {
	b, _ := json.Marshal(s) // a string always encodes
	buf.Write(b)
}

func encodeList(buf *bytes.Buffer, v reflect.Value) error {
	buf.WriteByte('[')
	for i := 0; i < v.Len(); i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := encode(buf, v.Index(i)); err != nil {
			return err
		}
	}
	buf.WriteByte(']')
	return nil
}

func encodeMap(buf *bytes.Buffer, v reflect.Value) error {
	type entry struct {
		key string
		val reflect.Value
	}
	entries := make([]entry, 0, v.Len())
	iter := v.MapRange()
	for iter.Next() {
		k, err := mapKey(iter.Key())
		if err != nil {
			return err
		}
		entries = append(entries, entry{k, iter.Value()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
	buf.WriteByte('{')
	for i, e := range entries {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeString(buf, e.key)
		buf.WriteByte(':')
		if err := encode(buf, e.val); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func mapKey(k reflect.Value) (string, error) {
	switch k.Kind() {
	case reflect.String:
		return k.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(k.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(k.Uint(), 10), nil
	}
	return "", fmt.Errorf("learnrec: cannot encode a map key of %s", k.Type())
}

func encodeStruct(buf *bytes.Buffer, v reflect.Value) error {
	t := v.Type()
	buf.WriteByte('{')
	first := true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			return fmt.Errorf("learnrec: %s has an unexported field %s; a recording would lose it", t, f.Name)
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		writeString(buf, f.Name)
		buf.WriteByte(':')
		if err := encode(buf, v.Field(i)); err != nil {
			return fmt.Errorf("%s.%s: %w", t, f.Name, err)
		}
	}
	buf.WriteByte('}')
	return nil
}

// WriteString writes s as a JSON string, escaped as Marshal escapes one.
func WriteString(buf *bytes.Buffer, s string) {
	writeString(buf, s)
}

// Line is one recording line, as Record writes it, for an event whose data
// is already encoded: what a tool that rewrites a recording writes back.
func Line(seq uint64, now time.Time, kind string, data []byte) ([]byte, error) {
	b, err := Marshal(Event{Seq: seq, Now: now, Kind: kind, Data: Raw(data)})
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
