// Copyright 2011 baihaoping@gmail.com. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package amf implements a basic AMF3 decoder.
package amf

import (
	"errors"
	"io"
	"math"
	"reflect"
	"strconv"
	"unicode"
)

type Decoder struct {
	reader      io.Reader
	stringCache []string
	objectCache []reflect.Value
}

func NewDecoder(r io.Reader) *Decoder {
	d := &Decoder{reader: r}
	d.Reset()
	return d
}

func (d *Decoder) Reset() {
	d.objectCache = make([]reflect.Value, 0, 10)
	d.stringCache = make([]string, 0, 10)
}

/* ─────────────────────── helpers ─────────────────────── */

func (d *Decoder) getField(key string, t reflect.Type) (reflect.StructField, bool) {
	r := []rune(key)
	upperKey := key
	if len(r) > 0 && unicode.IsLower(r[0]) {
		r[0] = unicode.ToUpper(r[0])
		upperKey = string(r)
	}

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Name == upperKey || f.Tag.Get("amf.name") == key {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

/* ─────────────────────── decode entry ─────────────────────── */

func (d *Decoder) Decode(v AMFAny) error {
	return d.decode(reflect.ValueOf(v))
}

func (d *Decoder) DecodeValue(v reflect.Value) error {
	return d.decode(v)
}

func (d *Decoder) decode(value reflect.Value) error {
	marker, err := d.readMarker()
	if err != nil {
		return err
	}

	/* ----- NULL handling ----- */
	if marker == NULL_MARKER {
		if value.IsNil() {
			return nil
		}
		switch value.Kind() {
		case reflect.Interface, reflect.Slice, reflect.Map, reflect.Ptr:
			value.Set(reflect.Zero(value.Type()))
			return nil
		default:
			return errors.New("invalid type: " + value.Type().String() + " for nil")
		}
	}

	/* ----- Unwrap interface / pointer ----- */
	if value.Kind() == reflect.Interface {
		if v := reflect.ValueOf(value.Interface()); v.Kind() == reflect.Ptr {
			value = v
		}
	}
	for value.Kind() == reflect.Ptr {
		if value.IsNil() {
			value.Set(reflect.New(value.Type().Elem()))
		}
		value = value.Elem()
	}

	/* ----- Dispatch by marker ----- */
	switch marker {
	case FALSE_MARKER:
		return d.setBool(value, false)
	case TRUE_MARKER:
		return d.setBool(value, true)
	case STRING_MARKER:
		return d.readString(value)
	case DOUBLE_MARKER:
		return d.readFloat(value)
	case INTEGER_MARKER:
		return d.readInteger(value)
	case ARRAY_MARKER:
		return d.readSlice(value)
	case OBJECT_MARKER:
		return d.readObject(value)
	default:
		return errors.New("unsupported marker: " + strconv.Itoa(int(marker)))
	}
}

/* ───────────────────── primitives ───────────────────── */

func (d *Decoder) setBool(value reflect.Value, v bool) error {
	switch value.Kind() {
	case reflect.Bool:
		value.SetBool(v)
	case reflect.Interface:
		value.Set(reflect.ValueOf(v))
	default:
		return errors.New("invalid type: " + value.Type().String() + " for bool")
	}
	return nil
}

func (d *Decoder) readFloat(value reflect.Value) error {
	bytes, err := d.readBytes(8)
	if err != nil {
		return err
	}
	var n uint64
	for _, b := range bytes {
		n = (n << 8) | uint64(b)
	}
	v := math.Float64frombits(n)

	switch value.Kind() {
	case reflect.Float32, reflect.Float64:
		value.SetFloat(v)
	case reflect.Int32, reflect.Int, reflect.Int64:
		value.SetInt(int64(v))
	case reflect.Uint32, reflect.Uint, reflect.Uint64:
		value.SetUint(uint64(v))
	case reflect.Interface:
		value.Set(reflect.ValueOf(v))
	default:
		return errors.New("invalid type: " + value.Type().String() + " for double")
	}
	return nil
}

func (d *Decoder) readInteger(value reflect.Value) error {
	uv, err := d.readU29()
	if err != nil {
		return err
	}
	vv := int32(uv)
	if uv > 0x0fffffff {
		vv = int32(uv - 0x20000000)
	}

	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(int64(vv))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(uint64(uv))
	case reflect.Interface:
		value.Set(reflect.ValueOf(uv))
	default:
		return errors.New("invalid type: " + value.Type().String() + " for integer")
	}
	return nil
}

/* ───────────────────── strings ───────────────────── */

func (d *Decoder) readString(value reflect.Value) error {
	index, err := d.readU29()
	if err != nil {
		return err
	}

	var s string
	if (index & 0x01) == 0 {
		s = d.stringCache[int(index>>1)]
	} else {
		index >>= 1
		bytes, err := d.readBytes(int(index))
		if err != nil {
			return err
		}
		s = string(bytes)
		if s != "" {
			d.stringCache = append(d.stringCache, s)
		}
	}

	switch value.Kind() {
	case reflect.Int, reflect.Int32, reflect.Int64:
		num, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		value.SetInt(num)
	case reflect.Uint, reflect.Uint32, reflect.Uint64:
		num, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return err
		}
		value.SetUint(num)
	case reflect.String:
		value.SetString(s)
	case reflect.Interface:
		value.Set(reflect.ValueOf(s))
	default:
		return errors.New("invalid type: " + value.Type().String() + " for string")
	}
	return nil
}

/* ───────────────────── compound (object / slice) ───────────────────── */

func (d *Decoder) readObject(value reflect.Value) error {
	index, err := d.readU29()
	if err != nil {
		return err
	}

	/* ----- object reference ----- */
	if (index & 0x01) == 0 {
		value.Set(d.objectCache[int(index>>1)])
		return nil
	}

	/* ----- dynamic anonymous object ----- */
	if index != 0x0b {
		return errors.New("invalid object type")
	}
	sep, err := d.readMarker()
	if err != nil {
		return err
	}
	if sep != 0x01 {
		return errors.New("typed object not supported")
	}

	/* Interface → map[string]AMFAny */
	if value.Kind() == reflect.Interface {
		var dummy map[string]AMFAny
		m := reflect.MakeMap(reflect.TypeOf(dummy))
		value.Set(m)
		value = m
	}

	/* ------ Map target ------ */
	if value.Kind() == reflect.Map {
		if value.IsNil() {
			m := reflect.MakeMap(value.Type())
			value.Set(m)
			value = m
		}
		d.objectCache = append(d.objectCache, value)

		for {
			var k string
			if err := d.readString(reflect.ValueOf(&k).Elem()); err != nil {
				return err
			}
			if k == "" {
				break
			}
			elem := reflect.New(value.Type().Elem())
			if err := d.decode(elem); err != nil {
				return err
			}
			value.SetMapIndex(reflect.ValueOf(k), elem.Elem())
		}
		return nil
	}

	/* ------ Struct target ------ */
	if value.Kind() != reflect.Struct {
		return errors.New("struct expected, found: " + value.Type().String())
	}
	d.objectCache = append(d.objectCache, value)

	for {
		var key string
		if err := d.readString(reflect.ValueOf(&key).Elem()); err != nil {
			return err
		}
		if key == "" {
			break
		}
		f, ok := d.getField(key, value.Type())
		if !ok {
			return errors.New("key " + key + " not found in struct " + value.Type().String())
		}
		if err := d.decode(value.FieldByName(f.Name)); err != nil {
			return err
		}
	}
	return nil
}

func (d *Decoder) readSlice(value reflect.Value) error {
	index, err := d.readU29()
	if err != nil {
		return err
	}

	/* ----- slice reference ----- */
	if (index & 0x01) == 0 {
		value.Set(d.objectCache[int(index>>1)])
		return nil
	}
	index >>= 1

	sep, err := d.readMarker()
	if err != nil {
		return err
	}
	if sep != 0x01 {
		return errors.New("ECMA array not allowed")
	}

	/* Ensure we have a concrete slice or []AMFAny */
	if value.IsNil() {
		var v reflect.Value
		switch value.Type().Kind() {
		case reflect.Slice:
			v = reflect.MakeSlice(value.Type(), int(index), int(index))
		case reflect.Interface:
			v = reflect.ValueOf(make([]AMFAny, int(index)))
		default:
			return errors.New("invalid type: " + value.Type().String() + " for array")
		}
		value.Set(v)
		value = v
	}
	d.objectCache = append(d.objectCache, value)

	for i := 0; i < int(index); i++ {
		if err := d.decode(value.Index(i)); err != nil {
			return err
		}
	}
	return nil
}

/* ───────────────────── low-level IO ───────────────────── */

func (d *Decoder) readU29() (uint32, error) {
	var ret uint32
	for i := 0; i < 4; i++ {
		b, err := d.readMarker()
		if err != nil {
			return 0, err
		}
		if i != 3 {
			ret = (ret << 7) | uint32(b&0x7f)
			if (b & 0x80) == 0 {
				break
			}
		} else {
			ret = (ret << 8) | uint32(b)
		}
	}
	return ret, nil
}

func (d *Decoder) readBytes(n int) ([]byte, error) {
	buf := make([]byte, n)
	for n > 0 {
		read, err := d.reader.Read(buf[len(buf)-n:])
		if err != nil {
			return nil, err
		}
		n -= read
	}
	return buf, nil
}

func (d *Decoder) readMarker() (byte, error) {
	b, err := d.readBytes(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}
