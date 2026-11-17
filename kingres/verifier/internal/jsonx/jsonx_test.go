// SPDX-License-Identifier: AGPL-3.0-only

package jsonx

import (
	"errors"
	"strings"
	"testing"
)

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"duplicate key", `{"a":1,"a":2}`},
		{"nested duplicate key", `{"o":{"a":1,"a":1}}`},
		{"trailing data", `{"a":1} {"b":2}`},
		{"trailing garbage", `{"a":1}x`},
		{"invalid UTF-8", "{\"a\":\"\xff\"}"},
		{"lone high surrogate", `{"a":"\ud800"}`},
		{"lone low surrogate", `{"a":"\udc00"}`},
		{"high surrogate then letter", `{"a":"\ud800x"}`},
		{"truncated", `{"a":`},
		{"too deep", strings.Repeat("[", MaxDepth+2) + strings.Repeat("]", MaxDepth+2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.in)); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestParseAccepts(t *testing.T) {
	for _, in := range []string{`{}`, `[]`, `{"a":"😀"}`, ` {"a" : [1, 2, {"b": null}]} `, `"x"`, `12`} {
		if _, err := Parse([]byte(in)); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestInt64(t *testing.T) {
	tests := []struct {
		in      string
		min     int64
		max     int64
		want    int64
		wantErr bool
	}{
		{"0", 0, 10, 0, false},
		{"10", 0, 10, 10, false},
		{"11", 0, 10, 0, true},
		{"-1", 0, 10, 0, true},
		{"-1", -5, 10, -1, false},
		{"1.0", 0, 10, 0, true},
		{"1.5", 0, 10, 0, true},
		{"1e1", 0, 10, 0, true},
		{"1E1", 0, 10, 0, true},
		{"01", 0, 10, 0, true},
		{"-0", -5, 5, 0, true},
		{"9223372036854775807", 0, 9223372036854775807, 9223372036854775807, false},
		{"9223372036854775808", 0, 9223372036854775807, 0, true},
		{"1" + strings.Repeat("0", 400), 0, 9223372036854775807, 0, true},
		{"-9223372036854775808", -9223372036854775808, 0, -9223372036854775808, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			v, err := Parse([]byte(`{"n":` + tt.in + `}`))
			if err != nil {
				if tt.wantErr {
					return // not even valid JSON, for example a leading zero
				}
				t.Fatal(err)
			}
			o, _ := v.Object()
			got, err := o.Int64("n", tt.min, tt.max)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAccessorsNameThePath(t *testing.T) {
	v, err := Parse([]byte(`{"a":{"b":"s","c":1,"d":[1]}}`))
	if err != nil {
		t.Fatal(err)
	}
	o, _ := v.Object()
	a, err := o.Object("a")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"missing", func() error { _, e := a.Field("zz"); return e }(), `$.a: missing field "zz"`},
		{"string as int", func() error { _, e := a.Int64("b", 0, 1); return e }(), "$.a.b: is string, expected integer"},
		{"int as string", func() error { _, e := a.String("c"); return e }(), "$.a.c: is number, expected string"},
		{"int as array", func() error { _, e := a.Array("c"); return e }(), "$.a.c: is number, expected array"},
		{"array as object", func() error { _, e := a.Object("d"); return e }(), "$.a.d: is array, expected object"},
		{"unknown key", a.OnlyKeys("b"), `$.a: unknown field "c"`},
	} {
		if tt.err == nil || !strings.HasPrefix(tt.err.Error(), tt.want) {
			t.Errorf("%s: got %v, want prefix %q", tt.name, tt.err, tt.want)
		}
	}
	if !a.Has("b") || a.Has("zz") {
		t.Error("Has")
	}
}

func TestBool(t *testing.T) {
	v, err := Parse([]byte(`{"a":true,"b":false,"c":"true","d":1}`))
	if err != nil {
		t.Fatal(err)
	}
	o, _ := v.Object()
	for name, want := range map[string]bool{"a": true, "b": false} {
		f, _ := o.Field(name)
		if got, err := f.Bool(); err != nil || got != want {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	for _, name := range []string{"c", "d"} {
		f, _ := o.Field(name)
		if _, err := f.Bool(); err == nil {
			t.Errorf("%s: accepted as a boolean", name)
		}
	}
}

// L9-05: what one document can make the reader hold is bounded by caps, not by
// the size of the file.
func TestLimits(t *testing.T) {
	small := Limits{MaxDepth: 4, MaxValues: 10, MaxMembers: 3, MaxKey: 5, MaxString: 8}
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"values at the limit", `[0,0,0,0,0,0,0,0,0]`, true},
		{"one value over", `[0,0,0,0,0,0,0,0,0,0]`, false},
		{"members at the limit", `{"a":0,"b":0,"c":0}`, true},
		{"one member over", `{"a":0,"b":0,"c":0,"d":0}`, false},
		{"key at the limit", `{"abcde":0}`, true},
		{"key one over", `{"abcdef":0}`, false},
		{"string at the limit", `["12345678"]`, true},
		{"string one over", `["123456789"]`, false},
		{"escapes count as written", `["\u0041\u0041"]`, false},
		{"depth at the limit", `[[[[0]]]]`, true},
		{"depth one over", `[[[[[0]]]]]`, false},
		{"a long number", `[` + strings.Repeat("1", MaxNumber+1) + `]`, false},
		{"a run of white space", "[" + strings.Repeat(" ", MaxSpaceRun+1) + "0]", false},
	}
	for _, tt := range tests {
		_, err := ParseLimits([]byte(tt.in), small)
		if tt.name == "a long number" || tt.name == "a run of white space" {
			_, err = ParseLimits([]byte(tt.in), DefaultLimits)
		}
		if (err == nil) != tt.ok {
			t.Errorf("%s: err %v, accepted should be %v", tt.name, err, tt.ok)
		}
	}
	// A document of many values is refused by the default limits too, long
	// before it is a tree.
	if _, err := Parse([]byte("[" + strings.Repeat("0,", 1<<20) + "0]")); err == nil {
		t.Error("a million values were accepted")
	}
}

func TestPathsAreBuiltFromTheLocation(t *testing.T) {
	v, err := Parse([]byte(`{"a":[{"b":1},{"c":[true]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	o, _ := v.Object()
	arr, _ := o.Array("a")
	second, _ := arr[1].Object()
	c, _ := second.Array("c")
	if got := c[0].Path(); got != "$.a[1].c[0]" {
		t.Errorf("path %q", got)
	}
	if got := v.Path(); got != "$" {
		t.Errorf("root path %q", got)
	}
}

func TestParseStreamingDeliversElementsOneByOne(t *testing.T) {
	var seen []string
	v, err := ParseStreaming([]byte(`{"x":1,"chain":[{"h":1},{"h":2},{"h":3}],"y":"z"}`), DefaultLimits, "chain", func(e Value) error {
		o, err := e.Object()
		if err != nil {
			return err
		}
		n, err := o.Int64("h", 0, 9)
		seen = append(seen, strings.Repeat("#", int(n)))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "#,##,###" {
		t.Errorf("elements %v", seen)
	}
	o, _ := v.Object()
	if arr, err := o.Array("chain"); err != nil || len(arr) != 0 {
		t.Errorf("the streamed member keeps %d elements (%v)", len(arr), err)
	}
	if s, err := o.String("y"); err != nil || s != "z" {
		t.Errorf("a member after the stream: %q %v", s, err)
	}
	// An error of the callback stops the parse and is the error returned.
	stop := errors.New("stop")
	if _, err := ParseStreaming([]byte(`{"chain":[1,2,3]}`), DefaultLimits, "chain", func(Value) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("callback error: %v", err)
	}
	// A member that is not an array is an ordinary value, and a stream of a
	// million elements costs no memory but is limited per element.
	if v, err := ParseStreaming([]byte(`{"chain":7}`), DefaultLimits, "chain", func(Value) error { return stop }); err != nil || v.Kind() != KindObject {
		t.Errorf("a number member: %v", err)
	}
	big := "{\"chain\":[" + strings.Repeat("[0,0],", 100000) + "[0]]}"
	n := 0
	if _, err := ParseStreaming([]byte(big), DefaultLimits, "chain", func(Value) error { n++; return nil }); err != nil || n != 100001 {
		t.Errorf("a long stream: %d elements, %v", n, err)
	}
	// The same document is refused by Parse: 200 thousand values.
	if _, err := Parse([]byte(big)); err == nil {
		t.Error("Parse took a document the stream exists for")
	}
	// Duplicate keys and depth still apply to a document that streams.
	if _, err := ParseStreaming([]byte(`{"chain":[],"chain":[]}`), DefaultLimits, "chain", func(Value) error { return nil }); err == nil {
		t.Error("a duplicate streamed key was accepted")
	}
}
