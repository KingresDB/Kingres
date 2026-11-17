// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// Vectors of https://homes.esat.kuleuven.be/~bosselae/ripemd160.html
func TestRIPEMD160Vectors(t *testing.T) {
	vecs := []struct{ in, want string }{
		{"", "9c1185a5c5e9fc54612808977ee8f548b2258d31"},
		{"a", "0bdc9d2d256b3ee9daae347be6f4dc835a467ffe"},
		{"abc", "8eb208f7e05d987a9b044a8e98c6b087f15a0bfc"},
		{"message digest", "5d0689ef49d2fae572b881b123a85ffa21595f36"},
		{"abcdefghijklmnopqrstuvwxyz", "f71c27109c692c1b56bbdceb5b9d2865b3708dbc"},
		{"abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq", "12a053384a9c0c88e405a06c27dcf49ada62eb2b"},
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789", "b0e20b6e3116640286ed3a87a5713079b21f5189"},
		{"12345678901234567890123456789012345678901234567890123456789012345678901234567890", "9b752e45573d4b39f4dbd3323cab82bf63326bfb"},
	}
	for _, v := range vecs {
		h := newRIPEMD160()
		h.Write([]byte(v.in))
		if got := hex.EncodeToString(h.Sum(nil)); got != v.want {
			t.Errorf("ripemd160(%q) = %s, want %s", v.in, got, v.want)
		}
	}
	h := newRIPEMD160()
	chunk := bytes.Repeat([]byte("a"), 1000)
	for i := 0; i < 1000; i++ {
		h.Write(chunk)
	}
	if got, want := hex.EncodeToString(h.Sum(nil)), "52783243c1697bdbe16d37f97f68f08325dc1528"; got != want {
		t.Errorf("million a = %s, want %s", got, want)
	}
}

func TestRIPEMD160Incremental(t *testing.T) {
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	whole := newRIPEMD160()
	whole.Write(data)
	want := whole.Sum(nil)
	for _, step := range []int{1, 3, 63, 64, 65, 200} {
		h := newRIPEMD160()
		for i := 0; i < len(data); i += step {
			e := i + step
			if e > len(data) {
				e = len(data)
			}
			h.Write(data[i:e])
		}
		if !bytes.Equal(h.Sum(nil), want) {
			t.Errorf("step %d differs", step)
		}
		// Sum must not change the state
		if !bytes.Equal(h.Sum(nil), want) {
			t.Errorf("step %d: second Sum differs", step)
		}
	}
}
