// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package edkey

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func h(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

var z31 = strings.Repeat("00", 31)

// The vectors are those of contrib/kingres_ledger/t/038_public_key_check.pl: the eight points of
// order dividing 8, their other encodings, values that are not on the curve. They were computed
// there with Python integers, from the definitions, independently of any code.
var bad = []struct {
	label string
	key   string
	want  error
}{
	{"the identity point (order 1)", "01" + z31, ErrSmallOrder},
	{"the point (0, -1) (order 2)", "ec" + strings.Repeat("ff", 30) + "7f", ErrSmallOrder},
	{"the all-zero key: y = 0, x positive (order 4)", strings.Repeat("00", 32), ErrSmallOrder},
	{"y = 0, x negative (order 4)", strings.Repeat("00", 31) + "80", ErrSmallOrder},
	{"an order 8 point", "26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05", ErrSmallOrder},
	{"an order 8 point (the sign bit set)", "26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85", ErrSmallOrder},
	{"an order 8 point (the other y)", "c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a", ErrSmallOrder},
	{"an order 8 point (the other y, the sign bit set)", "c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa", ErrSmallOrder},
	{"the identity with the sign bit set (x = 0 has no sign)", "01" + strings.Repeat("00", 30) + "80", ErrNotCanonical},
	{"the point (0, -1) with the sign bit set", "ec" + strings.Repeat("ff", 31), ErrNotCanonical},
	{"y = p + 1, an alias of the identity", "ee" + strings.Repeat("ff", 30) + "7f", ErrNotCanonical},
	{"y = p + 1 with the sign bit set", "ee" + strings.Repeat("ff", 31), ErrNotCanonical},
	{"y = p, an alias of y = 0", "ed" + strings.Repeat("ff", 30) + "7f", ErrNotCanonical},
	{"y = p with the sign bit set", "ed" + strings.Repeat("ff", 31), ErrNotCanonical},
	{"y = 2^255 - 1", strings.Repeat("ff", 32), ErrNotCanonical},
	{"y = p + 3, an alias of the good point y = 3", "f0" + strings.Repeat("ff", 30) + "7f", ErrNotCanonical},
	{"y = 2 (no x)", "02" + z31, ErrNotOnCurve},
	{"y = 7 (no x)", "07" + z31, ErrNotOnCurve},
	{"y = 2 with the sign bit set", "02" + strings.Repeat("00", 30) + "80", ErrNotOnCurve},
}

func TestRefusedKeysWithTheReasonOfTheExtension(t *testing.T) {
	for _, v := range bad {
		err := Check(h(v.key))
		if !errors.Is(err, v.want) {
			t.Errorf("%s: got %v, want %v", v.label, err, v.want)
		}
	}
}

func TestGoodKeysAreAccepted(t *testing.T) {
	// y = 3 is a point of the curve of large order: the check asks for a point of large order, not for a known private key.
	if err := Check(h("03" + z31)); err != nil {
		t.Errorf("y = 3: %v", err)
	}
	for i := 0; i < 200; i++ {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := Check(pub); err != nil {
			t.Fatalf("a generated key %x: %v", pub, err)
		}
	}
}

func TestWrongLength(t *testing.T) {
	for _, n := range []int{0, 1, 31, 33, 64} {
		if err := Check(bytes.Repeat([]byte{3}, n)); err == nil {
			t.Errorf("a key of %d bytes was accepted", n)
		}
	}
}

// A signature under a key of small order that verifies is what the check exists for: the standard
// library accepts it (when it does), this package refuses the key.
func TestTheCheckRefusesTheIdentityWhateverTheLibrarySays(t *testing.T) {
	pub := ed25519.PublicKey(h("01" + z31))
	sig := h("01" + strings.Repeat("00", 63)) // R = identity, S = 0
	_ = ed25519.Verify(pub, []byte("any message"), sig)
	if Check(pub) == nil {
		t.Fatal("the identity key passed the check")
	}
}
