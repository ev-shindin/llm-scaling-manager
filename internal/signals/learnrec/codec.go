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

// Raw is a value already encoded by Marshal, written as it is. An event
// encodes what a call read before the call runs, so nothing the call does to
// it can reach the recording; Fragment makes one, and Decode reads it back.
type Raw []byte

// Fragment encodes v for a later event, or returns the error as a Raw that
// will fail that event's encoding.
func Fragment(v any) (Raw, error) {
	b, err := Marshal(v)
	return Raw(b), err
}

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

// Unmarshal decodes data, in the recorder's canonical form, into the value v
// points to. It is Marshal's inverse: what Marshal wrote reads back to a
// value Marshal writes as the same bytes. A field the data names and the type
// does not have is an error, so a recording read with the wrong schema fails
// rather than replays half an input.
func Unmarshal(data []byte, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("learnrec: Unmarshal needs a non-nil pointer, got %T", v)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return fmt.Errorf("learnrec: %w", err)
	}
	return decode(tree, rv.Elem())
}

func decode(tree any, v reflect.Value) error {
	if tree == nil {
		v.SetZero()
		return nil
	}
	if v.Kind() != reflect.Interface && v.Kind() != reflect.Pointer && v.Type().Implements(recordedValuerType) {
		m, in, ok := recordedSetter(v.Type())
		if !ok {
			return fmt.Errorf("learnrec: %s is written by RecordedValue and has no SetRecordedValue to read it back", v.Type())
		}
		arg := reflect.New(in).Elem()
		if err := decode(tree, arg); err != nil {
			return err
		}
		if !v.CanAddr() {
			return fmt.Errorf("learnrec: cannot read %s back into a value that is not addressable", v.Type())
		}
		if out := m.Func.Call([]reflect.Value{v.Addr(), arg}); !out[0].IsNil() {
			return fmt.Errorf("learnrec: %s: %w", v.Type(), out[0].Interface().(error))
		}
		return nil
	}
	if v.Type() == rawType {
		// Kept as JSON for its own Decode. The tree holds the numbers as
		// written (json.Number), so nothing is lost on the way.
		b, err := json.Marshal(tree)
		if err != nil {
			return fmt.Errorf("learnrec: %w", err)
		}
		v.SetBytes(b)
		return nil
	}
	if v.Type() == timeType {
		s, ok := tree.(string)
		if !ok {
			return fmt.Errorf("learnrec: a time is a string, got %T", tree)
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return fmt.Errorf("learnrec: %w", err)
		}
		v.Set(reflect.ValueOf(t.UTC()))
		return nil
	}
	switch v.Kind() {
	case reflect.Bool:
		b, ok := tree.(bool)
		if !ok {
			return fmt.Errorf("learnrec: want a bool for %s, got %T", v.Type(), tree)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, ok := tree.(json.Number)
		if !ok {
			return fmt.Errorf("learnrec: want a number for %s, got %T", v.Type(), tree)
		}
		i, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil {
			return fmt.Errorf("learnrec: %w", err)
		}
		v.SetInt(i)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, ok := tree.(json.Number)
		if !ok {
			return fmt.Errorf("learnrec: want a number for %s, got %T", v.Type(), tree)
		}
		u, err := strconv.ParseUint(string(n), 10, 64)
		if err != nil {
			return fmt.Errorf("learnrec: %w", err)
		}
		v.SetUint(u)
	case reflect.Float32, reflect.Float64:
		f, err := decodeFloat(tree, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(f)
	case reflect.String:
		s, ok := tree.(string)
		if !ok {
			return fmt.Errorf("learnrec: want a string for %s, got %T", v.Type(), tree)
		}
		v.SetString(s)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		if err := decode(tree, p.Elem()); err != nil {
			return err
		}
		v.Set(p)
	case reflect.Interface:
		// Only an empty interface can be filled from data alone: its value is
		// the data's own (a number reads as a float64).
		if v.NumMethod() != 0 {
			return fmt.Errorf("learnrec: cannot decode into the interface %s", v.Type())
		}
		v.Set(reflect.ValueOf(plain(tree)))
	case reflect.Slice:
		list, ok := tree.([]any)
		if !ok {
			return fmt.Errorf("learnrec: want a list for %s, got %T", v.Type(), tree)
		}
		s := reflect.MakeSlice(v.Type(), len(list), len(list))
		for i, item := range list {
			if err := decode(item, s.Index(i)); err != nil {
				return err
			}
		}
		v.Set(s)
	case reflect.Array:
		list, ok := tree.([]any)
		if !ok || len(list) != v.Len() {
			return fmt.Errorf("learnrec: want a list of %d for %s", v.Len(), v.Type())
		}
		for i, item := range list {
			if err := decode(item, v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		obj, ok := tree.(map[string]any)
		if !ok {
			return fmt.Errorf("learnrec: want an object for %s, got %T", v.Type(), tree)
		}
		m := reflect.MakeMapWithSize(v.Type(), len(obj))
		for k, item := range obj {
			key := reflect.New(v.Type().Key()).Elem()
			if err := decodeKey(k, key); err != nil {
				return err
			}
			val := reflect.New(v.Type().Elem()).Elem()
			if err := decode(item, val); err != nil {
				return err
			}
			m.SetMapIndex(key, val)
		}
		v.Set(m)
	case reflect.Struct:
		obj, ok := tree.(map[string]any)
		if !ok {
			return fmt.Errorf("learnrec: want an object for %s, got %T", v.Type(), tree)
		}
		t := v.Type()
		for name, item := range obj {
			f, ok := t.FieldByName(name)
			if !ok || len(f.Index) != 1 || !f.IsExported() {
				return fmt.Errorf("learnrec: %s has no field %s", t, name)
			}
			if err := decode(item, v.Field(f.Index[0])); err != nil {
				return fmt.Errorf("%s.%s: %w", t, name, err)
			}
		}
	default:
		return fmt.Errorf("learnrec: cannot decode %s", v.Type())
	}
	return nil
}

func decodeFloat(tree any, bits int) (float64, error) {
	switch x := tree.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(x), bits)
		if err != nil {
			return 0, fmt.Errorf("learnrec: %w", err)
		}
		return f, nil
	case string:
		switch x {
		case "NaN":
			return math.NaN(), nil
		case "+Inf":
			return math.Inf(1), nil
		case "-Inf":
			return math.Inf(-1), nil
		}
	}
	return 0, fmt.Errorf("learnrec: want a float, got %v", tree)
}

func decodeKey(k string, v reflect.Value) error {
	switch v.Kind() {
	case reflect.String:
		v.SetString(k)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			return fmt.Errorf("learnrec: %w", err)
		}
		v.SetInt(i)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			return fmt.Errorf("learnrec: %w", err)
		}
		v.SetUint(u)
	default:
		return fmt.Errorf("learnrec: cannot decode a map key into %s", v.Type())
	}
	return nil
}

// plain turns a decoded tree into the values encoding/json would have given
// an empty interface: numbers as float64, the non-finite strings left as
// strings.
func plain(tree any) any {
	switch x := tree.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return string(x)
		}
		return f
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = plain(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = plain(item)
		}
		return out
	}
	return tree
}
