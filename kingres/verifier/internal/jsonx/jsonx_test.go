// SPDX-License-Identifier: PostgreSQL

package jsonx

import (
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
