// SPDX-License-Identifier: PostgreSQL

// Package jsonx is a strict JSON reader for ledger files. It rejects what
// encoding/json accepts silently: duplicate object keys, invalid UTF-8,
// lone surrogate escapes, trailing data, and numbers that are not plain
// integers when an integer is asked for. Every error names the JSON path and
// says what was expected.
package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"unicode/utf8"
)

// MaxDepth is the deepest nesting accepted.
const MaxDepth = 32

// Kind is the JSON type of a Value.
type Kind int

// The JSON types.
const (
	KindNull Kind = iota
	KindBool
	KindNumber
	KindString
	KindArray
	KindObject
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "boolean"
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindArray:
		return "array"
	case KindObject:
		return "object"
	}
	return "unknown"
}

// Value is a parsed JSON value that remembers where it came from.
type Value struct {
	kind Kind
	path string
	text string // string contents, or the literal of a number
	b    bool
	arr  []Value
	obj  map[string]Value
}

// Kind returns the JSON type.
func (v Value) Kind() Kind { return v.kind }

// Path returns the JSON path of the value, for example "$.block.height".
func (v Value) Path() string { return v.path }

// Parse parses one JSON document with the checks described in the package
// comment.
func Parse(data []byte) (Value, error) {
	if !utf8.Valid(data) {
		return Value{}, errors.New("input is not valid UTF-8, expected a UTF-8 JSON document")
	}
	if err := checkEscapes(data); err != nil {
		return Value{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseValue(dec, "$", 0)
	if err != nil {
		return Value{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Value{}, errors.New("unexpected data after the JSON document, expected end of input")
	}
	return v, nil
}

func parseValue(dec *json.Decoder, path string, depth int) (Value, error) {
	if depth > MaxDepth {
		return Value{}, fmt.Errorf("%s: nesting deeper than %d levels", path, MaxDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return Value{}, fmt.Errorf("%s: invalid JSON: %w", path, err)
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			return parseObject(dec, path, depth)
		}
		return parseArray(dec, path, depth)
	case string:
		return Value{kind: KindString, path: path, text: t}, nil
	case json.Number:
		return Value{kind: KindNumber, path: path, text: string(t)}, nil
	case bool:
		return Value{kind: KindBool, path: path, b: t}, nil
	case nil:
		return Value{kind: KindNull, path: path}, nil
	}
	return Value{}, fmt.Errorf("%s: unexpected token %v", path, tok)
}

func parseObject(dec *json.Decoder, path string, depth int) (Value, error) {
	obj := map[string]Value{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return Value{}, fmt.Errorf("%s: invalid JSON: %w", path, err)
		}
		key, ok := kt.(string)
		if !ok {
			return Value{}, fmt.Errorf("%s: expected an object key", path)
		}
		if _, dup := obj[key]; dup {
			return Value{}, fmt.Errorf("%s: duplicate key %q, expected each key once", path, key)
		}
		child, err := parseValue(dec, path+"."+key, depth+1)
		if err != nil {
			return Value{}, err
		}
		obj[key] = child
	}
	if _, err := dec.Token(); err != nil {
		return Value{}, fmt.Errorf("%s: invalid JSON: %w", path, err)
	}
	return Value{kind: KindObject, path: path, obj: obj}, nil
}

func parseArray(dec *json.Decoder, path string, depth int) (Value, error) {
	var arr []Value
	for dec.More() {
		child, err := parseValue(dec, path+"["+strconv.Itoa(len(arr))+"]", depth+1)
		if err != nil {
			return Value{}, err
		}
		arr = append(arr, child)
	}
	if _, err := dec.Token(); err != nil {
		return Value{}, fmt.Errorf("%s: invalid JSON: %w", path, err)
	}
	return Value{kind: KindArray, path: path, arr: arr}, nil
}

// checkEscapes rejects unpaired UTF-16 surrogate escapes in strings, which
// encoding/json would turn into U+FFFD without a word. Syntax errors are left
// to the decoder.
func checkEscapes(data []byte) error {
	inStr := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inStr {
			if c == '"' {
				inStr = true
			}
			continue
		}
		switch c {
		case '"':
			inStr = false
		case '\\':
			if i+1 >= len(data) {
				return nil
			}
			if data[i+1] != 'u' {
				i++
				continue
			}
			r, ok := hex4(data, i+2)
			if !ok {
				return nil
			}
			i += 5
			switch {
			case r >= 0xD800 && r < 0xDC00:
				if i+6 < len(data) && data[i+1] == '\\' && data[i+2] == 'u' {
					if r2, ok2 := hex4(data, i+3); ok2 && r2 >= 0xDC00 && r2 < 0xE000 {
						i += 6
						continue
					}
				}
				return errors.New("string has an unpaired high surrogate escape, expected a valid Unicode escape")
			case r >= 0xDC00 && r < 0xE000:
				return errors.New("string has an unpaired low surrogate escape, expected a valid Unicode escape")
			}
		}
	}
	return nil
}

func hex4(data []byte, at int) (rune, bool) {
	if at+4 > len(data) {
		return 0, false
	}
	var r rune
	for _, c := range data[at : at+4] {
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		r = r<<4 | rune(d)
	}
	return r, true
}

// Object returns the value as an object.
func (v Value) Object() (Object, error) {
	if v.kind != KindObject {
		return Object{}, fmt.Errorf("%s: is %s, expected object", v.path, v.kind)
	}
	return Object{v: v}, nil
}

// Array returns the elements of an array value.
func (v Value) Array() ([]Value, error) {
	if v.kind != KindArray {
		return nil, fmt.Errorf("%s: is %s, expected array", v.path, v.kind)
	}
	return v.arr, nil
}

// String returns the contents of a string value.
func (v Value) String() (string, error) {
	if v.kind != KindString {
		return "", fmt.Errorf("%s: is %s, expected string", v.path, v.kind)
	}
	return v.text, nil
}

// Int64 returns an integer value in [min, max]. A number with a fraction, an
// exponent, a leading zero, a plus sign or a minus zero is rejected, and so is
// a number outside the range or outside int64.
func (v Value) Int64(minV, maxV int64) (int64, error) {
	if v.kind != KindNumber {
		return 0, fmt.Errorf("%s: is %s, expected integer", v.path, v.kind)
	}
	if !plainInteger(v.text) {
		return 0, fmt.Errorf("%s: %q is not a plain integer, expected digits only (no fraction, exponent or leading zero)", v.path, v.text)
	}
	n, err := strconv.ParseInt(v.text, 10, 64)
	if err != nil || n < minV || n > maxV {
		return 0, fmt.Errorf("%s: %s is out of range, expected an integer from %d to %d", v.path, v.text, minV, maxV)
	}
	return n, nil
}

func plainInteger(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' {
		i = 1
	}
	digits := s[i:]
	if digits == "" || (len(digits) > 1 && digits[0] == '0') || (i == 1 && digits == "0") {
		return false
	}
	for j := 0; j < len(digits); j++ {
		if digits[j] < '0' || digits[j] > '9' {
			return false
		}
	}
	return true
}

// Object is a JSON object.
type Object struct{ v Value }

// Path returns the JSON path of the object.
func (o Object) Path() string { return o.v.path }

// Field returns a member that must be present.
func (o Object) Field(name string) (Value, error) {
	f, ok := o.v.obj[name]
	if !ok {
		return Value{}, fmt.Errorf("%s: missing field %q", o.v.path, name)
	}
	return f, nil
}

// Has reports whether the member is present.
func (o Object) Has(name string) bool {
	_, ok := o.v.obj[name]
	return ok
}

// String returns a string member that must be present.
func (o Object) String(name string) (string, error) {
	f, err := o.Field(name)
	if err != nil {
		return "", err
	}
	return f.String()
}

// Int64 returns an integer member in [min, max] that must be present.
func (o Object) Int64(name string, minV, maxV int64) (int64, error) {
	f, err := o.Field(name)
	if err != nil {
		return 0, err
	}
	return f.Int64(minV, maxV)
}

// Array returns an array member that must be present.
func (o Object) Array(name string) ([]Value, error) {
	f, err := o.Field(name)
	if err != nil {
		return nil, err
	}
	return f.Array()
}

// Object returns an object member that must be present.
func (o Object) Object(name string) (Object, error) {
	f, err := o.Field(name)
	if err != nil {
		return Object{}, err
	}
	return f.Object()
}

// OnlyKeys returns an error naming the first member that is not in allowed.
func (o Object) OnlyKeys(allowed ...string) error {
	known := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		known[a] = true
	}
	var bad []string
	for k := range o.v.obj {
		if !known[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("%s: unknown field %q, expected only %v", o.v.path, bad[0], allowed)
}
