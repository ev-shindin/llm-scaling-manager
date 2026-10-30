package learnrec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"time"
)

// Unmarshal decodes data, in the recorder's canonical form, into the value v
// points to. It is Marshal's inverse: what Marshal wrote reads back to a
// value Marshal writes as the same bytes. A Raw field keeps the exact bytes
// it was written with. A field the data names and the type does not have is
// an error, so a recording read with the wrong schema fails rather than
// replays half an input.
func Unmarshal(data []byte, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("learnrec: Unmarshal needs a non-nil pointer, got %T", v)
	}
	if !json.Valid(data) {
		return errors.New("learnrec: not valid JSON")
	}
	return decode(bytes.TrimSpace(data), rv.Elem())
}

var nullJSON = []byte("null")

// decode reads one JSON value, raw, into v. Objects and lists are split into
// their raw members, so a Raw anywhere below keeps its bytes as written.
func decode(raw []byte, v reflect.Value) error {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, nullJSON) {
		v.SetZero()
		return nil
	}
	if v.Type() == rawType {
		v.SetBytes(bytes.Clone(raw))
		return nil
	}
	if v.Kind() != reflect.Interface && v.Kind() != reflect.Pointer && v.Type().Implements(recordedValuerType) {
		return decodeRecorded(raw, v)
	}
	if v.Type() == timeType {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("learnrec: a time is a string: %w", err)
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
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("learnrec: want a bool for %s: %w", v.Type(), err)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return fmt.Errorf("learnrec: want an integer for %s: %w", v.Type(), err)
		}
		v.SetInt(i)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u, err := strconv.ParseUint(string(raw), 10, 64)
		if err != nil {
			return fmt.Errorf("learnrec: want an unsigned integer for %s: %w", v.Type(), err)
		}
		v.SetUint(u)
	case reflect.Float32, reflect.Float64:
		f, err := decodeFloat(raw, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(f)
	case reflect.String:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("learnrec: want a string for %s: %w", v.Type(), err)
		}
		v.SetString(s)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		if err := decode(raw, p.Elem()); err != nil {
			return err
		}
		v.Set(p)
	case reflect.Interface:
		// Only an empty interface can be filled from data alone: its value is
		// the data's own (a number reads as a float64).
		if v.NumMethod() != 0 {
			return fmt.Errorf("learnrec: cannot decode into the interface %s", v.Type())
		}
		var tree any
		if err := json.Unmarshal(raw, &tree); err != nil {
			return fmt.Errorf("learnrec: %w", err)
		}
		v.Set(reflect.ValueOf(tree))
	case reflect.Slice:
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			return fmt.Errorf("learnrec: want a list for %s: %w", v.Type(), err)
		}
		s := reflect.MakeSlice(v.Type(), len(list), len(list))
		for i, item := range list {
			if err := decode(item, s.Index(i)); err != nil {
				return err
			}
		}
		v.Set(s)
	case reflect.Array:
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil || len(list) != v.Len() {
			return fmt.Errorf("learnrec: want a list of %d for %s", v.Len(), v.Type())
		}
		for i, item := range list {
			if err := decode(item, v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("learnrec: want an object for %s: %w", v.Type(), err)
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
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("learnrec: want an object for %s: %w", v.Type(), err)
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

// decodeRecorded reads a type written through RecordedValue back through its
// SetRecordedValue.
func decodeRecorded(raw []byte, v reflect.Value) error {
	m, in, ok := recordedSetter(v.Type())
	if !ok {
		return fmt.Errorf("learnrec: %s is written by RecordedValue and has no SetRecordedValue to read it back", v.Type())
	}
	arg := reflect.New(in).Elem()
	if err := decode(raw, arg); err != nil {
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

func decodeFloat(raw []byte, bits int) (float64, error) {
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			switch s {
			case "NaN":
				return math.NaN(), nil
			case "+Inf":
				return math.Inf(1), nil
			case "-Inf":
				return math.Inf(-1), nil
			}
		}
		return 0, fmt.Errorf("learnrec: want a float, got %s", raw)
	}
	f, err := strconv.ParseFloat(string(raw), bits)
	if err != nil {
		return 0, fmt.Errorf("learnrec: %w", err)
	}
	return f, nil
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
