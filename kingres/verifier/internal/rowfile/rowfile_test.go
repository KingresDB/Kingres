// SPDX-License-Identifier: PostgreSQL

package rowfile

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cols, err := Parse([]byte(`[{"name":"id","send_hex":"00000001"},{"name":"größe","send_hex":""}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 2 || cols[0].Name != "id" || len(cols[0].Value) != 4 || cols[1].Name != "größe" || len(cols[1].Value) != 0 {
		t.Errorf("unexpected columns %+v", cols)
	}
	if cols, err := Parse([]byte(`[]`)); err != nil || len(cols) != 0 {
		t.Errorf("empty list: %v %v", cols, err)
	}
}

func TestParseRejects(t *testing.T) {
	for _, tt := range []struct{ name, in string }{
		{"object", `{}`},
		{"element not an object", `[1]`},
		{"missing name", `[{"send_hex":""}]`},
		{"missing send_hex", `[{"name":"a"}]`},
		{"name number", `[{"name":1,"send_hex":""}]`},
		{"upper case hex", `[{"name":"a","send_hex":"AB"}]`},
		{"odd hex", `[{"name":"a","send_hex":"abc"}]`},
		{"prefixed hex", `[{"name":"a","send_hex":"0x00"}]`},
		{"misspelled member", `[{"name":"a","send_hex":"00","value":"00"}]`},
		{"duplicate member", `[{"name":"a","name":"b","send_hex":"00"}]`},
		{"lone surrogate in name", `[{"name":"\ud800","send_hex":"00"}]`},
		{"invalid UTF-8", "[{\"name\":\"\xff\",\"send_hex\":\"00\"}]"},
		{"null", `null`},
		{"too long", strings.Repeat("[", 100)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.in)); err == nil {
				t.Error("want an error")
			}
		})
	}
}

// FuzzParse checks that Parse never panics.
func FuzzParse(f *testing.F) {
	f.Add([]byte(`[{"name":"id","send_hex":"00000001"}]`))
	f.Add([]byte(`[{"name":"x","send_hex":""},{"name":"y","send_hex":"ff"}]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Parse(data)
	})
}
