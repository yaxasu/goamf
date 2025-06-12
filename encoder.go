// Copyright 2011 baihaoping@gmail.com.
// BSD-style license; see LICENSE file.

package amf

import (
	"errors"
	"io"
	"math"
	"reflect"
	"strconv"
	"unicode"
)

type Encoder struct {
	writer       io.Writer
	stringCache  map[string]int
	objectCache  map[uintptr]int
	reservStruct bool
}

/* ───── lifecycle ───── */

func NewEncoder(w io.Writer, reservStruct bool) *Encoder {
	e := &Encoder{writer: w, reservStruct: reservStruct}
	e.Reset()
	return e
}

func (e *Encoder) Reset() {
	e.objectCache = make(map[uintptr]int)
	e.stringCache = make(map[string]int)
}

/* ───── helpers ───── */

func (e *Encoder) getFieldName(f reflect.StructField) string {
	r := []rune(f.Name)
	if unicode.IsLower(r[0]) {
		return ""
	}
	if tag := f.Tag.Get("amf.name"); tag != "" {
		return tag
	}
	if !e.reservStruct {
		r[0] = unicode.ToLower(r[0])
		return string(r)
	}
	return f.Name
}

func (e *Encoder) writeBytes(b []byte) error {
	if n, err := e.writer.Write(b); n != len(b) || err != nil {
		return errors.New("write failed")
	}
	return nil
}

func (e *Encoder) writeMarker(m byte) error { return e.writeBytes([]byte{m}) }

/* ───── primitive encoders ───── */

func (e *Encoder) encodeBool(v bool) error {
	if v {
		return e.writeMarker(TRUE_MARKER)
	}
	return e.writeMarker(FALSE_MARKER)
}

func (e *Encoder) encodeNull() error { return e.writeMarker(NULL_MARKER) }

func (e *Encoder) encodeUint(v uint64) error {
	if v >= 0x20000000 {
		if v <= 0xffffffff {
			return e.encodeFloat(float64(v))
		}
		return e.encodeString(strconv.FormatUint(v, 10))
	}
	if err := e.writeMarker(INTEGER_MARKER); err != nil {
		return err
	}
	return e.writeU29(uint32(v))
}

func (e *Encoder) encodeInt(v int64) error {
	if v < -0x0fffffff {
		if v > -0x7fffffff {
			return e.encodeFloat(float64(v))
		}
		return e.encodeString(strconv.FormatInt(v, 10))
	}
	if err := e.writeMarker(INTEGER_MARKER); err != nil {
		return err
	}
	return e.writeU29(uint32(v))
}

func (e *Encoder) encodeFloat(v float64) error {
	buf := make([]byte, 9)
	buf[0] = DOUBLE_MARKER
	u := math.Float64bits(v)
	for i := 8; i > 0; i-- {
		buf[i] = byte(u & 0xff)
		u >>= 8
	}
	return e.writeBytes(buf)
}

func (e *Encoder) encodeString(s string) error {
	if err := e.writeMarker(STRING_MARKER); err != nil {
		return err
	}
	return e.writeString(s)
}

/* ───── compound encoders ───── */

func (e *Encoder) encodeMap(v reflect.Value) error {
	if err := e.writeMarker(OBJECT_MARKER); err != nil {
		return err
	}

	if idx, ok := e.objectCache[v.Pointer()]; ok {
		return e.writeU29(uint32(idx << 2)) // ((idx<<1)|1)<<1
	}
	e.objectCache[v.Pointer()] = len(e.objectCache)

	// dynamic object flag
	if err := e.writeMarker(0x0b); err != nil {
		return err
	}
	if err := e.writeString(""); err != nil {
		return err
	}

	for _, k := range v.MapKeys() {
		if k.Kind() != reflect.String {
			return errors.New("map key must be string")
		}
		if err := e.writeString(k.String()); err != nil {
			return err
		}

		elem := v.MapIndex(k)

		// Map elements are never addressable; if it's a struct, always copy it into
		// an addressable wrapper so downstream code can take its address safely.
		if elem.Kind() == reflect.Struct {
			ptr := reflect.New(elem.Type())
			ptr.Elem().Set(elem)
			elem = ptr // treat as *Struct for further encoding
		}
		if err := e.encode(elem); err != nil {
			return err
		}
	}
	return e.writeString("") // end-of-object marker
}

func (e *Encoder) encodeStruct(v reflect.Value) error {
	if err := e.writeMarker(OBJECT_MARKER); err != nil {
		return err
	}

	if idx, ok := e.objectCache[v.Pointer()]; ok {
		return e.writeU29(uint32(idx << 2))
	}
	e.objectCache[v.Pointer()] = len(e.objectCache)

	if err := e.writeMarker(0x0b); err != nil {
		return err
	}
	if err := e.writeString(""); err != nil { // dynamic
		return err
	}

	sv := v.Elem()
	st := sv.Type()
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		name := e.getFieldName(f)
		if name == "" {
			continue
		}
		if err := e.writeString(name); err != nil {
			return err
		}
		fv := sv.Field(i)
		if fv.Kind() == reflect.Struct {
			fv = fv.Addr()
		}
		if err := e.encode(fv); err != nil {
			return err
		}
	}
	return e.writeString("")
}

func (e *Encoder) encodeSlice(v reflect.Value) error {
	if err := e.writeMarker(ARRAY_MARKER); err != nil {
		return err
	}

	if idx, ok := e.objectCache[v.Pointer()]; ok {
		return e.writeU29(uint32(idx << 2))
	}
	e.objectCache[v.Pointer()] = len(e.objectCache)

	if err := e.writeU29(uint32(v.Len())<<1 | 0x01); err != nil {
		return err
	}
	if err := e.writeString(""); err != nil { // no ECMA part
		return err
	}

	for i := 0; i < v.Len(); i++ {
		elem := v.Index(i)
		if elem.Kind() == reflect.Struct {
			elem = elem.Addr()
		}
		if err := e.encode(elem); err != nil {
			return err
		}
	}
	return nil
}

/* ───── dispatcher ───── */

func (e *Encoder) encode(v reflect.Value) error {
	switch v.Kind() {
	case reflect.Map:
		return e.encodeMap(v)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return e.encodeUint(v.Uint())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return e.encodeInt(v.Int())
	case reflect.Bool:
		return e.encodeBool(v.Bool())
	case reflect.String:
		return e.encodeString(v.String())
	case reflect.Array:
		return e.encodeSlice(v.Slice(0, v.Len()))
	case reflect.Slice:
		return e.encodeSlice(v)
	case reflect.Float32, reflect.Float64:
		return e.encodeFloat(v.Float())
	case reflect.Interface:
		return e.encode(reflect.ValueOf(v.Interface()))
	case reflect.Ptr:
		if v.IsNil() {
			return e.encodeNull()
		}
		if v.Elem().Kind() == reflect.Struct {
			return e.encodeStruct(v)
		}
		return e.encode(v.Elem())
	default:
		return errors.New("unsupported type: " + v.Type().String())
	}
}

func (e *Encoder) Encode(v AMFAny) error { return e.encode(reflect.ValueOf(v)) }

/* ───── low-level helpers ───── */

func (e *Encoder) writeString(s string) error {
	if idx, ok := e.stringCache[s]; ok {
		return e.writeU29(uint32(idx << 1))
	}
	if err := e.writeU29(uint32(len(s)<<1 | 0x01)); err != nil {
		return err
	}
	if s != "" {
		e.stringCache[s] = len(e.stringCache)
	}
	return e.writeBytes([]byte(s))
}

func (e *Encoder) writeU29(v uint32) error {
	switch {
	case v < 0x80:
		return e.writeBytes([]byte{byte(v)})
	case v < 0x4000:
		return e.writeBytes([]byte{byte((v >> 7) | 0x80), byte(v & 0x7f)})
	case v < 0x200000:
		return e.writeBytes([]byte{
			byte((v >> 14) | 0x80),
			byte((v >> 7) | 0x80),
			byte(v & 0x7f),
		})
	case v < 0x20000000:
		return e.writeBytes([]byte{
			byte((v >> 22) | 0x80),
			byte((v >> 15) | 0x80),
			byte((v >> 7) | 0x80),
			byte(v & 0xff),
		})
	default:
		return errors.New("u29 overflow")
	}
}
