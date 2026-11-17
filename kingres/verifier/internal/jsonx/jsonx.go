// SPDX-License-Identifier: AGPL-3.0-only

// Package jsonx is a strict JSON reader for ledger files. It rejects what
// encoding/json accepts silently: duplicate object keys, invalid UTF-8,
// lone surrogate escapes, trailing data, and numbers that are not plain
// integers when an integer is asked for. Every error names the JSON path and
// says what was expected.
//
// It is also bounded (type Limits): depth, number of values, members of an
// object, length of a key and of a string. The files it reads come from the
// database server or from a publisher and may be hostile, so what one file can
// make the reader allocate must not depend on how it is shaped.
package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits bound what one document may make the reader allocate, whoever signed
// or published it. Memory is a constant plus a small multiple of the largest
// single value (one string, one streamed element), never a multiple of the
// input: a document of 16 MiB that is eight million zeros is refused after
// MaxValues of them, not turned into a tree of eight million nodes.
type Limits struct {
	// MaxDepth is the deepest nesting accepted.
	MaxDepth int
	// MaxValues is the number of values (every scalar, array and object) in one
	// document; for a streamed array it counts per element.
	MaxValues int
	// MaxMembers is the number of members of one object.
	MaxMembers int
	// MaxKey is the length of an object key in bytes.
	MaxKey int
	// MaxString is the length of one string as written in the document, in
	// bytes (an escape counts as written).
	MaxString int
}

// MaxDepth is the deepest nesting accepted by default.
const MaxDepth = 32

// MaxSpaceRun is the longest run of white space between two tokens.
const MaxSpaceRun = 1 << 16

// MaxNumber is the longest number, in characters. Every number of the ledger
// formats has at most 20 digits.
const MaxNumber = 64

// DefaultLimits are the limits of Parse: far above what a digest, a block
// header, a trust file or one proof element needs, far below what hurts.
var DefaultLimits = Limits{MaxDepth: MaxDepth, MaxValues: 1 << 14, MaxMembers: 1 << 10, MaxKey: 256, MaxString: 1 << 16}

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

// locator says where a value sits, as a link to its parent. The path text is
// built only when an error needs it: building it for every value (a string per
// node, each as long as its parents) made the memory of a document a hundred
// times its size.
type locator struct {
	parent *locator
	key    string
	index  int
	isIdx  bool
}

func (l *locator) String() string {
	var parts []string
	for ; l != nil; l = l.parent {
		if l.isIdx {
			parts = append(parts, "["+strconv.Itoa(l.index)+"]")
		} else {
			parts = append(parts, "."+l.key)
		}
	}
	var b strings.Builder
	b.WriteString("$")
	for i := len(parts) - 1; i >= 0; i-- {
		b.WriteString(parts[i])
	}
	return b.String()
}

// Value is a parsed JSON value that remembers where it came from.
type Value struct {
	kind Kind
	at   *locator
	text string // string contents, or the literal of a number
	b    bool
	arr  []Value
	obj  map[string]Value
}

// Kind returns the JSON type.
func (v Value) Kind() Kind { return v.kind }

// Path returns the JSON path of the value, for example "$.block.height".
func (v Value) Path() string { return v.at.String() }

// Parse parses one JSON document with the checks described in the package
// comment and the DefaultLimits.
func Parse(data []byte) (Value, error) { return ParseLimits(data, DefaultLimits) }

// ParseLimits is Parse with the limits of the caller.
func ParseLimits(data []byte, l Limits) (Value, error) {
	return parse(data, l, "", nil)
}

// ParseStreaming parses a document that is an object with a member name that
// is an array too long to hold: the elements of that array are given to each
// one at a time, each parsed with the limits as a document of its own, and are
// not kept. The returned value has the member as an empty array. An error of
// each stops the parse and is returned. The array is the one of the top
// object only; a document without it (or with another type there) is parsed
// as usual and each is never called.
func ParseStreaming(data []byte, l Limits, name string, each func(Value) error) (Value, error) {
	return parse(data, l, name, each)
}

type parser struct {
	dec    *json.Decoder
	lim    Limits
	count  int
	stream string
	each   func(Value) error
}

func parse(data []byte, l Limits, stream string, each func(Value) error) (Value, error) {
	return ParseReader(bytes.NewReader(data), l, stream, each)
}

// ParseReader is ParseStreaming over a reader: the document is read in pieces
// and checked as it goes (UTF-8, escapes, the length of a string, a run of white
// space), so that the memory of a parse is a constant plus a small multiple of
// the largest single value whatever the size of the document. The document is
// accepted only when the whole of it was read and was valid; each is called for
// the elements as they come, so a caller that acts on them must not treat them
// as verified until ParseReader returns without an error. With each nil it is
// Parse over a reader.
func ParseReader(r io.Reader, l Limits, stream string, each func(Value) error) (Value, error) {
	cr := &checkedReader{r: r, max: l.MaxString}
	dec := json.NewDecoder(cr)
	dec.UseNumber()
	p := &parser{dec: dec, lim: l, stream: stream, each: each}
	v, err := p.value(nil, 0)
	if cr.err != nil {
		return Value{}, cr.err // what the checks found comes first, as before
	}
	if err != nil {
		return Value{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if cr.err != nil {
			return Value{}, cr.err
		}
		return Value{}, errors.New("unexpected data after the JSON document, expected end of input")
	}
	if cr.err != nil {
		return Value{}, cr.err
	}
	return v, nil
}

func (p *parser) value(at *locator, depth int) (Value, error) {
	if depth > p.lim.MaxDepth {
		return Value{}, fmt.Errorf("%s: nesting deeper than %d levels", at, p.lim.MaxDepth)
	}
	if p.count++; p.count > p.lim.MaxValues {
		return Value{}, fmt.Errorf("%s: more than %d values in the document, expected a smaller document", at, p.lim.MaxValues)
	}
	tok, err := p.dec.Token()
	if err != nil {
		return Value{}, fmt.Errorf("%s: invalid JSON: %w", at, err)
	}
	return p.finish(tok, at, depth)
}

func (p *parser) object(at *locator, depth int) (Value, error) {
	obj := map[string]Value{}
	for p.dec.More() {
		kt, err := p.dec.Token()
		if err != nil {
			return Value{}, fmt.Errorf("%s: invalid JSON: %w", at, err)
		}
		key, ok := kt.(string)
		if !ok {
			return Value{}, fmt.Errorf("%s: expected an object key", at)
		}
		if len(key) > p.lim.MaxKey {
			return Value{}, fmt.Errorf("%s: a key of %d bytes, expected at most %d", at, len(key), p.lim.MaxKey)
		}
		if _, dup := obj[key]; dup {
			return Value{}, fmt.Errorf("%s: duplicate key %q, expected each key once", at, key)
		}
		if len(obj) >= p.lim.MaxMembers {
			return Value{}, fmt.Errorf("%s: more than %d members, expected a smaller object", at, p.lim.MaxMembers)
		}
		child := &locator{parent: at, key: key}
		if depth == 0 && p.each != nil && key == p.stream {
			v, err := p.streamMember(child)
			if err != nil {
				return Value{}, err
			}
			obj[key] = v
			continue
		}
		v, err := p.value(child, depth+1)
		if err != nil {
			return Value{}, err
		}
		obj[key] = v
	}
	if _, err := p.dec.Token(); err != nil {
		return Value{}, fmt.Errorf("%s: invalid JSON: %w", at, err)
	}
	return Value{kind: KindObject, at: at, obj: obj}, nil
}

// streamMember reads the member that is delivered element by element. When the
// member is not an array it is read as an ordinary value.
func (p *parser) streamMember(at *locator) (Value, error) {
	if p.count++; p.count > p.lim.MaxValues {
		return Value{}, fmt.Errorf("%s: more than %d values in the document, expected a smaller document", at, p.lim.MaxValues)
	}
	tok, err := p.dec.Token()
	if err != nil {
		return Value{}, fmt.Errorf("%s: invalid JSON: %w", at, err)
	}
	if d, isDelim := tok.(json.Delim); !isDelim || d != '[' {
		return p.finish(tok, at, 1)
	}
	n := 0
	for p.dec.More() {
		// Each element is a document of its own as far as the count goes.
		inner := &parser{dec: p.dec, lim: p.lim}
		el, err := inner.value(&locator{parent: at, index: n, isIdx: true}, 2)
		if err != nil {
			return Value{}, err
		}
		if err := p.each(el); err != nil {
			return Value{}, err
		}
		n++
	}
	if _, err := p.dec.Token(); err != nil {
		return Value{}, fmt.Errorf("%s: invalid JSON: %w", at, err)
	}
	return Value{kind: KindArray, at: at}, nil
}

// finish completes a value whose first token was already read.
func (p *parser) finish(tok json.Token, at *locator, depth int) (Value, error) {
	switch t := tok.(type) {
	case json.Delim:
		if depth >= p.lim.MaxDepth {
			return Value{}, fmt.Errorf("%s: nesting deeper than %d levels", at, p.lim.MaxDepth)
		}
		if t == '{' {
			return p.object(at, depth)
		}
		return p.array(at, depth)
	case string:
		return Value{kind: KindString, at: at, text: t}, nil
	case json.Number:
		if len(t) > MaxNumber {
			return Value{}, fmt.Errorf("%s: a number of %d characters, expected at most %d", at, len(t), MaxNumber)
		}
		return Value{kind: KindNumber, at: at, text: string(t)}, nil
	case bool:
		return Value{kind: KindBool, at: at, b: t}, nil
	case nil:
		return Value{kind: KindNull, at: at}, nil
	}
	return Value{}, fmt.Errorf("%s: unexpected token %v", at, tok)
}

func (p *parser) array(at *locator, depth int) (Value, error) {
	var arr []Value
	for p.dec.More() {
		child, err := p.value(&locator{parent: at, index: len(arr), isIdx: true}, depth+1)
		if err != nil {
			return Value{}, err
		}
		arr = append(arr, child)
	}
	if _, err := p.dec.Token(); err != nil {
		return Value{}, fmt.Errorf("%s: invalid JSON: %w", at, err)
	}
	return Value{kind: KindArray, at: at, arr: arr}, nil
}

// Object returns the value as an object.
func (v Value) Object() (Object, error) {
	if v.kind != KindObject {
		return Object{}, fmt.Errorf("%s: is %s, expected object", v.Path(), v.kind)
	}
	return Object{v: v}, nil
}

// Array returns the elements of an array value.
func (v Value) Array() ([]Value, error) {
	if v.kind != KindArray {
		return nil, fmt.Errorf("%s: is %s, expected array", v.Path(), v.kind)
	}
	return v.arr, nil
}

// String returns the contents of a string value.
func (v Value) String() (string, error) {
	if v.kind != KindString {
		return "", fmt.Errorf("%s: is %s, expected string", v.Path(), v.kind)
	}
	return v.text, nil
}

// Int64 returns an integer value in [min, max]. A number with a fraction, an
// exponent, a leading zero, a plus sign or a minus zero is rejected, and so is
// a number outside the range or outside int64.
func (v Value) Int64(minV, maxV int64) (int64, error) {
	if v.kind != KindNumber {
		return 0, fmt.Errorf("%s: is %s, expected integer", v.Path(), v.kind)
	}
	if !plainInteger(v.text) {
		return 0, fmt.Errorf("%s: %q is not a plain integer, expected digits only (no fraction, exponent or leading zero)", v.Path(), v.text)
	}
	n, err := strconv.ParseInt(v.text, 10, 64)
	if err != nil || n < minV || n > maxV {
		return 0, fmt.Errorf("%s: %s is out of range, expected an integer from %d to %d", v.Path(), v.text, minV, maxV)
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
func (o Object) Path() string { return o.v.Path() }

// Field returns a member that must be present.
func (o Object) Field(name string) (Value, error) {
	f, ok := o.v.obj[name]
	if !ok {
		return Value{}, fmt.Errorf("%s: missing field %q", o.v.Path(), name)
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
	return fmt.Errorf("%s: unknown field %q, expected only %v", o.v.Path(), bad[0], allowed)
}

// Bool returns a boolean value.
func (v Value) Bool() (bool, error) {
	if v.kind != KindBool {
		return false, fmt.Errorf("%s: is %s, expected boolean", v.Path(), v.kind)
	}
	return v.b, nil
}

// checkedReader passes bytes on to the decoder after checking them, and keeps
// the first thing it found. It rejects invalid UTF-8, unpaired UTF-16 surrogate
// escapes in strings (which encoding/json would turn into U+FFFD without a
// word), strings longer than max as written (before the decoder holds them) and
// a run of white space longer than MaxSpaceRun. Syntax errors are left to the
// decoder. The checks are a state machine over the bytes, so that a document is
// checked in the pieces it is read in and never held whole.
type checkedReader struct {
	r   io.Reader
	max int
	err error

	carry []byte // the start of a UTF-8 sequence that a piece ended in
	state byte
	strN  int // bytes of the current string so far, after its opening quote
	hexN  int
	hexV  rune
	space int
	done  bool // the escape check stopped: the decoder reports the syntax error
}

const (
	stNormal   = iota
	stString   // inside a string
	stEscape   // after a backslash
	stHex      // reading the four digits of \u
	stHighBack // after a high surrogate: a backslash must follow
	stHighU    // after that backslash: a u must follow
	stHighHex  // reading the four digits of the second \u
)

func (c *checkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.r.Read(p)
	if n > 0 {
		if cerr := c.check(p[:n]); cerr != nil {
			c.err = cerr
			return 0, cerr
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			if len(c.carry) > 0 {
				c.err = errors.New("input is not valid UTF-8, expected a UTF-8 JSON document")
				return n, c.err
			}
			if c.state == stHighBack || c.state == stHighU || c.state == stHighHex {
				c.err = errors.New("string has an unpaired high surrogate escape, expected a valid Unicode escape")
				return n, c.err
			}
		}
		return n, err
	}
	return n, nil
}

var errNotUTF8 = errors.New("input is not valid UTF-8, expected a UTF-8 JSON document")

// check feeds the bytes of one piece to the checks in the order they come, so
// that what is reported does not depend on how the document was cut into
// pieces. A UTF-8 sequence that the piece ends in is held back for the next.
func (c *checkedReader) check(b []byte) error {
	if len(c.carry) > 0 {
		for len(c.carry) < utf8.UTFMax && !utf8.FullRune(c.carry) && len(b) > 0 {
			c.carry = append(c.carry, b[0])
			b = b[1:]
		}
		if !utf8.FullRune(c.carry) {
			return nil
		}
		if err := c.rune(c.carry); err != nil {
			return err
		}
		c.carry = c.carry[:0]
	}
	for len(b) > 0 {
		if b[0] < utf8.RuneSelf {
			if err := c.step(b[0]); err != nil {
				return err
			}
			b = b[1:]
			continue
		}
		if !utf8.FullRune(b) {
			c.carry = append(c.carry[:0], b...)
			return nil
		}
		n := 1
		if _, size := utf8.DecodeRune(b); size > 1 {
			n = size
		}
		if err := c.rune(b[:n]); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// rune checks one complete sequence and feeds its bytes to the escape check.
func (c *checkedReader) rune(seq []byte) error {
	if r, n := utf8.DecodeRune(seq); r == utf8.RuneError && n <= 1 {
		return errNotUTF8
	}
	for _, ch := range seq {
		if err := c.step(ch); err != nil {
			return err
		}
	}
	return nil
}

func hexVal(ch byte) (rune, bool) {
	switch {
	case ch >= '0' && ch <= '9':
		return rune(ch - '0'), true
	case ch >= 'a' && ch <= 'f':
		return rune(ch-'a') + 10, true
	case ch >= 'A' && ch <= 'F':
		return rune(ch-'A') + 10, true
	}
	return 0, false
}

var (
	errHigh = errors.New("string has an unpaired high surrogate escape, expected a valid Unicode escape")
	errLow  = errors.New("string has an unpaired low surrogate escape, expected a valid Unicode escape")
)

func (c *checkedReader) step(ch byte) error {
	if c.done {
		return nil
	}
	switch c.state {
	case stNormal:
		// The decoder keeps a run of white space in its buffer until the next
		// token: a file that is all white space would be held whole, twice.
		if ch == ' ' || ch == 0x0a || ch == 0x09 || ch == 0x0d {
			if c.space++; c.space > MaxSpaceRun {
				return fmt.Errorf("a run of more than %d white space characters, expected a smaller document", MaxSpaceRun)
			}
			return nil
		}
		c.space = 0
		if ch == '"' {
			c.state, c.strN = stString, 0
		}
		return nil
	case stHighBack:
		if ch != '\\' {
			return errHigh
		}
		c.state = stHighU
		return nil
	case stHighU:
		if ch != 'u' {
			return errHigh
		}
		c.state, c.hexN, c.hexV = stHighHex, 0, 0
		return nil
	case stHighHex:
		d, ok := hexVal(ch)
		if !ok {
			return errHigh
		}
		c.hexV = c.hexV<<4 | d
		if c.hexN++; c.hexN == 4 {
			if c.hexV < 0xDC00 || c.hexV >= 0xE000 {
				return errHigh
			}
			c.state = stString
		}
		return nil
	}
	// inside a string: every byte counts against the length
	if c.strN++; c.strN > c.max+1 {
		return fmt.Errorf("a string is longer than %d bytes, expected a smaller document", c.max)
	}
	switch c.state {
	case stString:
		switch ch {
		case '"':
			c.state = stNormal
		case '\\':
			c.state = stEscape
		}
	case stEscape:
		if ch == 'u' {
			c.state, c.hexN, c.hexV = stHex, 0, 0
		} else {
			c.state = stString
		}
	case stHex:
		d, ok := hexVal(ch)
		if !ok {
			c.done = true // not a valid escape: the decoder says so
			return nil
		}
		c.hexV = c.hexV<<4 | d
		if c.hexN++; c.hexN == 4 {
			switch {
			case c.hexV >= 0xD800 && c.hexV < 0xDC00:
				c.state = stHighBack
			case c.hexV >= 0xDC00 && c.hexV < 0xE000:
				return errLow
			default:
				c.state = stString
			}
		}
	}
	return nil
}
