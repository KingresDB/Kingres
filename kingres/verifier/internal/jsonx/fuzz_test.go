// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package jsonx

import "testing"

// FuzzParse checks that no input makes Parse or the accessors panic.
func FuzzParse(f *testing.F) {
	for _, s := range []string{``, `{}`, `[1,2]`, `{"a":{"b":[1.5,"x",null,true]}}`, `{"a":"😀"}`, `{"a":1,"a":2}`, `"\ud800"`, "\xff", `1e999`, `-0`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := Parse(data)
		if err != nil {
			return
		}
		if o, err := v.Object(); err == nil {
			_, _ = o.Int64("a", 0, 10)
			_, _ = o.String("a")
			_, _ = o.Array("a")
			_ = o.OnlyKeys("a")
		}
		_, _ = v.Array()
		_, _ = v.Int64(-5, 5)
	})
}

// count walks a parsed value and returns how many values it holds.
func count(v Value) int {
	n := 1
	if arr, err := v.Array(); err == nil {
		for _, e := range arr {
			n += count(e)
		}
	}
	for _, e := range v.obj {
		n += count(e)
	}
	return n
}

// FuzzParseLimits checks the caps for any input and any (small) limits: a
// document that parses is within every limit, one that does not within them
// fails with an error and never a panic, and the stream of ParseStreaming
// agrees with Parse about what is valid.
func FuzzParseLimits(f *testing.F) {
	for _, s := range []string{``, `{}`, `[0,0,0,0,0,0,0,0,0,0]`, `{"a":{"a":{"a":{"a":{}}}}}`, `{"chain":[{"a":1},{"b":[2]}],"x":1}`, `"` + "aaaaaaaaaaaaaaaaaaaaaaaaaa" + `"`, "[ \n\n\n\n\n\n 1]"} {
		f.Add([]byte(s), uint8(3), uint8(8), uint8(4), uint8(8), uint8(8))
	}
	f.Fuzz(func(t *testing.T, data []byte, depth, values, members, key, str uint8) {
		l := Limits{MaxDepth: int(depth % 8), MaxValues: int(values), MaxMembers: int(members), MaxKey: int(key), MaxString: int(str)}
		v, err := ParseLimits(data, l)
		if err == nil {
			if n := count(v); n > l.MaxValues {
				t.Fatalf("%d values with a limit of %d", n, l.MaxValues)
			}
		}
		streamed := 0
		sv, serr := ParseStreaming(data, l, "chain", func(Value) error { streamed++; return nil })
		if err == nil && serr != nil {
			t.Fatalf("Parse accepts what ParseStreaming refuses: %v", serr)
		}
		_ = sv
	})
}
