// SPDX-License-Identifier: AGPL-3.0-only

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
