// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package jsonx

import (
	"io"
	"strconv"
	"strings"
	"testing"
)

// L12-05: the memory of a streamed document is bounded by the size of one
// element and of the part outside the stream, not by what the other limits allow
// together (16,384 values of 64 KiB, a gigabyte in one element).

// endless is a reader that produces a document prefix and then the same unit
// for ever, and counts what was asked of it.
type endless struct {
	prefix string
	unit   string
	pos    int
	n      int64
}

func (e *endless) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		var b byte
		if e.pos < len(e.prefix) {
			b = e.prefix[e.pos]
		} else {
			b = e.unit[(e.pos-len(e.prefix))%len(e.unit)]
		}
		p[n] = b
		n++
		e.pos++
	}
	e.n += int64(n)
	return n, nil
}

var _ io.Reader = (*endless)(nil)

// members is a document {"chain":[],"m0":"AAA...","m1":"AAA...", ... that does
// not end: every member has its own key and a string of 60,000 bytes.
type members struct {
	buf []byte
	i   int
	n   int64
}

func (m *members) Read(p []byte) (int, error) {
	if m.buf == nil {
		m.buf = []byte(`{"chain":[],`)
	}
	n := 0
	for n < len(p) {
		if len(m.buf) == 0 {
			m.buf = []byte(`"m` + strconv.Itoa(m.i) + `":"` + strings.Repeat("A", 60000) + `",`)
			m.i++
		}
		c := copy(p[n:], m.buf)
		m.buf = m.buf[c:]
		n += c
	}
	m.n += int64(n)
	return n, nil
}

func TestAnElementOfManyStringsIsRefusedBeforeItIsHeld(t *testing.T) {
	unit := `"` + strings.Repeat("A", 60000) + `",`
	e := &endless{prefix: `{"chain":[{"x":[`, unit: unit}
	_, err := ParseReader(e, DefaultLimits, "chain", func(Value) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "an element of more than 4096 bytes") {
		t.Fatalf("an element of a gigabyte: %v", err)
	}
	// the reader was not asked for more than a few strings of the element: what is
	// held is what was read, so this is the bound of the memory too
	if e.n > 1<<20 {
		t.Fatalf("%d bytes were read before the element was refused, want under 1 MiB", e.n)
	}
}

func TestAnElementIsExactlyAsLargeAsItMayBe(t *testing.T) {
	element := func(size int) string {
		// {"a":"<pad>"} is 8 bytes without the pad
		return `{"a":"` + strings.Repeat("A", size-8) + `"}`
	}
	for _, c := range []struct {
		size int
		ok   bool
	}{{MaxElementBytes - 1, true}, {MaxElementBytes, true}, {MaxElementBytes + 1, false}, {MaxElementBytes + 100, false}} {
		doc := `{"chain":[` + element(c.size) + `]}`
		_, err := ParseStreaming([]byte(doc), DefaultLimits, "chain", func(Value) error { return nil })
		if (err == nil) != c.ok {
			t.Errorf("an element of %d bytes: %v, want ok=%v", c.size, err, c.ok)
		}
	}
	// white space is part of the size
	doc := `{"chain":[{"a":1` + strings.Repeat(" ", MaxElementBytes) + `}]}`
	if _, err := ParseStreaming([]byte(doc), DefaultLimits, "chain", func(Value) error { return nil }); err == nil {
		t.Error("an element of white space was accepted")
	}
	// the elements are measured one by one: many small ones are no problem
	doc = `{"chain":[` + strings.TrimSuffix(strings.Repeat(`{"a":1},`, 20000), ",") + `]}`
	n := 0
	if _, err := ParseStreaming([]byte(doc), DefaultLimits, "chain", func(Value) error { n++; return nil }); err != nil || n != 20000 {
		t.Errorf("20,000 small elements: %d %v", n, err)
	}
}

func TestWhatIsOutsideTheStreamIsBounded(t *testing.T) {
	m := &members{}
	_, err := ParseReader(m, DefaultLimits, "chain", func(Value) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "bytes outside the member") {
		t.Fatalf("members around the stream of a gigabyte: %v", err)
	}
	if m.n > 4<<20 {
		t.Fatalf("%d bytes were read before the document was refused", m.n)
	}
	// a stream of any length does not count against it
	doc := `{"a":1,"chain":[` + strings.TrimSuffix(strings.Repeat(`{"a":1},`, 200000), ",") + `],"b":2}`
	if _, err := ParseStreaming([]byte(doc), DefaultLimits, "chain", func(Value) error { return nil }); err != nil {
		t.Errorf("a long stream with members around it: %v", err)
	}
	// a document that is not streamed is not subject to either limit
	big := `{"x":"` + strings.Repeat("A", 60000) + `"}`
	if _, err := Parse([]byte(strings.Repeat(" ", 10) + big)); err != nil {
		t.Errorf("an ordinary document: %v", err)
	}
}
