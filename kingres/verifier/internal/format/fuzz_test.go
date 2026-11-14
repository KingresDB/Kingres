// SPDX-License-Identifier: PostgreSQL

package format

import (
	"bytes"
	"testing"
)

// FuzzDecodeHex checks that DecodeHex never panics and that what it accepts
// round trips exactly.
func FuzzDecodeHex(f *testing.F) {
	for _, s := range []string{"", "00", "ff", "0", "zz", "AB", "0123456789abcdef"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, err := DecodeHex(s)
		if err != nil {
			return
		}
		if EncodeHex(b) != s {
			t.Errorf("DecodeHex(%q) = %x does not round trip", s, b)
		}
		if h, err := DecodeHash(s); err == nil && !bytes.Equal(h[:], b) {
			t.Errorf("DecodeHash and DecodeHex differ for %q", s)
		}
	})
}

// FuzzEncodeRow checks that the row encoder never panics and gives the
// same bytes twice.
func FuzzEncodeRow(f *testing.F) {
	f.Add(uint32(1), "id", []byte{0, 0, 0, 1})
	f.Fuzz(func(t *testing.T, table uint32, name string, value []byte) {
		cols := []Column{{name, value}, {name + "x", nil}}
		a, err := EncodeRow(table, cols)
		if err != nil {
			return
		}
		b, _ := EncodeRow(table, cols)
		if !bytes.Equal(a, b) {
			t.Error("encoding is not deterministic")
		}
	})
}
