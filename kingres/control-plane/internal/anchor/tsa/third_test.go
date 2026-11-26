// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package tsa

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Tests of the third review (A3-09): the comparison of names, and the pin.

// cnName is a name of one attribute, the common name, with the value written
// in the string type tag as the bytes given.
func cnName(tag byte, content []byte) []byte {
	atv := tl(0x30, append(tl(0x06, []byte{0x55, 0x04, 0x03}), tl(tag, content)...))
	return tl(0x30, tl(0x31, atv))
}

func utf16be(units ...uint16) []byte {
	var b []byte
	for _, u := range units {
		b = append(b, byte(u>>8), byte(u))
	}
	return b
}

func utf32be(vals ...uint32) []byte {
	var b []byte
	for _, v := range vals {
		b = append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
	return b
}

const (
	tagUTF8, tagPrintable, tagNumeric, tagIA5, tagVisible, tagTeletex, tagUniversal, tagBMP = 0x0c, 0x13, 0x12, 0x16, 0x1a, 0x14, 0x1c, 0x1e
)

type nameCase struct {
	name string
	a, b []byte
	want bool
}

// A3-09: every string type and every pair of the review.
func TestSameNameStringTypes(t *testing.T) {
	u8 := func(s string) []byte { return cnName(tagUTF8, []byte(s)) }
	cases := []nameCase{
		// invalid in the string type: never equal to another value, whatever a
		// lenient reader would make of the bytes
		{"printable, invalid bytes differ", cnName(tagPrintable, []byte("a\xff")), cnName(tagPrintable, []byte("a\xfe")), false},
		{"printable, invalid byte against utf8 U+FFFD", cnName(tagPrintable, []byte("a\xff")), u8("a\uFFFD"), false},
		{"printable, a character outside the set", cnName(tagPrintable, []byte("a@b")), u8("a@b"), false},
		{"ia5, bytes above 0x7f differ", cnName(tagIA5, []byte("a\xc3")), cnName(tagIA5, []byte("a\xc4")), false},
		{"ia5, invalid against utf8", cnName(tagIA5, []byte("\xc3\xa9")), u8("\u00e9"), false},
		{"numeric, letters", cnName(tagNumeric, []byte("1a")), u8("1a"), false},
		{"numeric, digits and space", cnName(tagNumeric, []byte("12 3")), u8("12 3"), true},
		{"visible, control character", cnName(tagVisible, []byte("a\x01")), cnName(tagVisible, []byte("a\x02")), false},
		{"utf8, invalid", cnName(tagUTF8, []byte("a\xff")), cnName(tagUTF8, []byte("a\xfe")), false},
		{"bmp, lone high surrogates differ", cnName(tagBMP, utf16be(0xd800)), cnName(tagBMP, utf16be(0xd801)), false},
		{"bmp, lone surrogate against U+FFFD", cnName(tagBMP, utf16be(0xd800)), u8("\uFFFD"), false},
		{"bmp, lone low surrogate", cnName(tagBMP, utf16be('a', 0xdc00)), cnName(tagBMP, utf16be('a', 0xdc01)), false},
		{"bmp, odd length", cnName(tagBMP, append(utf16be('a'), 0x00)), cnName(tagBMP, append(utf16be('a'), 0x01)), false},
		{"bmp, a valid surrogate pair", cnName(tagBMP, utf16be(0xd83d, 0xde00)), u8("\U0001F600"), true},
		{"bmp, plain", cnName(tagBMP, utf16be('T', 'S', 'A')), u8("TSA"), true},
		{"universal, beyond Unicode", cnName(tagUniversal, utf32be(0x110000)), cnName(tagUniversal, utf32be(0x110001)), false},
		{"universal, a surrogate", cnName(tagUniversal, utf32be(0xd800)), cnName(tagUniversal, utf32be(0xd801)), false},
		{"universal, length not a multiple of four", cnName(tagUniversal, []byte{0, 0, 0}), cnName(tagUniversal, []byte{0, 0, 1}), false},
		{"universal, plain", cnName(tagUniversal, utf32be('T', 'S', 'A')), u8("TSA"), true},
		// TeletexString: bytes, unless ASCII
		{"teletex 0xE9 against utf8 e-acute", cnName(tagTeletex, []byte{0xe9}), u8("\u00e9"), false},
		{"teletex 0xE9 against teletex 0xE8", cnName(tagTeletex, []byte{0xe9}), cnName(tagTeletex, []byte{0xe8}), false},
		{"teletex bytes of a utf8 e-acute against utf8 e-acute", cnName(tagTeletex, []byte("\xc3\xa9")), u8("\u00e9"), false},
		{"teletex 0xE9 against itself", cnName(tagTeletex, []byte{0xe9}), cnName(tagTeletex, []byte{0xe9}), true},
		{"teletex 0xE9 against ia5 0xE9", cnName(tagTeletex, []byte{0xe9}), cnName(tagIA5, []byte{0xe9}), false},
		{"teletex ascii against utf8, other case", cnName(tagTeletex, []byte("abc")), u8("ABC"), true},
		// string types that are valid compare by value
		{"printable against utf8", cnName(tagPrintable, []byte("Time Stamp")), u8("Time Stamp"), true},
		{"ia5 against bmp", cnName(tagIA5, []byte("Time")), cnName(tagBMP, utf16be('T', 'i', 'm', 'e')), true},
		// case: ASCII letters only
		{"ascii case", u8("My TSA"), u8("mY tsa"), true},
		{"kelvin sign is not k", u8("\u212a"), u8("k"), false},
		{"long s is not s", u8("\u017f"), u8("s"), false},
		{"sharp s is not ss", u8("stra\u00dfe"), u8("STRASSE"), false},
		{"dotless i is not i", u8("\u0131"), u8("i"), false},
		{"cyrillic a is not a", u8("\u0430"), u8("a"), false},
		{"non-ascii case is not folded", u8("\u00c9"), u8("\u00e9"), false},
		// white space: ASCII space only
		{"inner runs of spaces", u8("My   TSA"), u8("My TSA"), true},
		{"leading and trailing spaces", u8("  My TSA  "), u8("My TSA"), true},
		{"no-break space is not a space", u8("My\u00a0TSA"), u8("My TSA"), false},
		{"ideographic and em space are not spaces", u8("My\u3000\u2003TSA"), u8("My TSA"), false},
		{"tab is not a space", u8("My\tTSA"), u8("My TSA"), false},
		{"empty against blanks", u8(""), u8("   "), false},
		{"blanks against one blank", u8("   "), u8(" "), true},
		{"zero-width space", u8("My\u200bTSA"), u8("MyTSA"), false},
		{"NUL", u8("My\x00TSA"), u8("MyTSA"), false},
	}
	for _, c := range cases {
		if got := sameName(c.a, c.b); got != c.want {
			t.Errorf("%s: sameName = %v, want %v", c.name, got, c.want)
		}
		if got := sameName(c.b, c.a); got != c.want {
			t.Errorf("%s (reversed): sameName = %v, want %v", c.name, got, c.want)
		}
	}
}

// A3-09: names that differ in structure stay different (the pairs of the review
// that were never equal).
func TestSameNameStructure(t *testing.T) {
	rdn := func(oid byte, v string) []byte {
		return tl(0x31, tl(0x30, append(tl(0x06, []byte{0x55, 0x04, oid}), tl(tagUTF8, []byte(v))...)))
	}
	cn := func(v string) []byte { return rdn(0x03, v) }
	o := func(v string) []byte { return rdn(0x0a, v) }
	seq := func(rdns ...[]byte) []byte {
		var b []byte
		for _, r := range rdns {
			b = append(b, r...)
		}
		return tl(0x30, b)
	}
	if !sameName(seq(cn("A"), o("B")), seq(cn("a"), o("b"))) {
		t.Error("equal names differ")
	}
	if sameName(seq(cn("A"), o("B")), seq(o("B"), cn("A"))) {
		t.Error("another RDN order is the same name")
	}
	if sameName(seq(cn("A")), seq(o("A"))) {
		t.Error("another attribute type is the same name")
	}
	atv := func(oid byte, v string) []byte {
		return tl(0x30, append(tl(0x06, []byte{0x55, 0x04, oid}), tl(tagUTF8, []byte(v))...))
	}
	multi := tl(0x31, append(atv(0x03, "A"), atv(0x0a, "B")...))
	if sameName(tl(0x30, multi), seq(cn("A"), o("B"))) {
		t.Error("a multi-valued RDN is the same as two RDNs")
	}
}

// A3-09: the pin of the signer. The key hash is the SHA-256 of the
// SubjectPublicKeyInfo; a token of the pinned key is accepted, a token under the
// same roots whose signer has another key is refused with its own code.
func TestSignerPin(t *testing.T) {
	a := newAuthority(t, authOpts{})
	tok := a.token(testReq(), issueOpts{})
	h := SignerKeyHash(a.tsa)
	if h != sha256.Sum256(a.tsa.RawSubjectPublicKeyInfo) {
		t.Fatal("not the hash of the SubjectPublicKeyInfo")
	}
	if _, err := VerifyToken(tok, Expect{Digest: testDigest, SignerKeySHA256: h[:]}, a.rootPool, a.interPool); err != nil {
		t.Fatalf("the pinned signer: %v", err)
	}
	other := SignerKeyHash(a.root)
	_, err := VerifyToken(tok, Expect{Digest: testDigest, SignerKeySHA256: other[:]}, a.rootPool, a.interPool)
	if err == nil || anchor.CodeOf(err) != CodeSignerPin {
		t.Fatalf("another key: %v", err)
	}
	// no pin: any signer under the roots, as before
	if _, err := VerifyToken(tok, Expect{Digest: testDigest}, a.rootPool, a.interPool); err != nil {
		t.Fatal(err)
	}
	// a pin of the wrong length cannot match
	if _, err := VerifyToken(tok, Expect{Digest: testDigest, SignerKeySHA256: h[:5]}, a.rootPool, a.interPool); err == nil {
		t.Fatal("a short pin accepted")
	}
}

// A3-09: the configuration member.
func TestSignerPinConfig(t *testing.T) {
	base := func(pin string) json.RawMessage {
		return json.RawMessage(`{"url":"https://tsa.example/tsr","caBundlePath":"/x","signerKeySha256":"` + pin + `"}`)
	}
	good := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	if c, err := ParseConfig(base(good)); err != nil || c.SignerKeySHA256 != good {
		t.Fatalf("%+v %v", c, err)
	}
	for name, pin := range map[string]string{
		"short": good[:62], "long": good + "00", "upper case": "00112233445566778899AABBCCDDEEFF00112233445566778899aabbccddeeff", "not hex": "zz" + good[2:],
	} {
		if _, err := ParseConfig(base(pin)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if c, err := ParseConfig(json.RawMessage(`{"url":"https://tsa.example/tsr","caBundlePath":"/x"}`)); err != nil || c.SignerKeySHA256 != "" {
		t.Fatalf("%+v %v", c, err)
	}
}

// A3-09: a target with a pin accepts the pinned signer and refuses a token of any
// other signing key under the same roots, through the whole exchange.
func TestTargetPinsTheSigner(t *testing.T) {
	var pin string
	good := newEnv(t, nil)
	h := SignerKeyHash(good.a.tsa)
	pin = hex.EncodeToString(h[:])
	e := newEnv(t, func(o *Options) { o.Config.SignerKeySHA256 = pin })
	// the environment has its own authority, so the pin of the first one is the wrong key here
	_, err := e.target.Submit(context.Background(), testPayload(t))
	if err == nil || anchor.CodeOf(err) != CodeSignerPin {
		t.Fatalf("a token of another key: %v", err)
	}
	h2 := SignerKeyHash(e.a.tsa)
	e.target.cfg.SignerKeySHA256 = hex.EncodeToString(h2[:])
	if _, err := e.target.Submit(context.Background(), testPayload(t)); err != nil {
		t.Fatalf("the pinned key: %v", err)
	}
}

// A3-09: a visible string with a control character is not equal to a UTF-8
// string with the same bytes, and the DNS names are folded for ASCII only.
func TestSameNameVisibleAndDNS(t *testing.T) {
	if sameName(cnName(tagVisible, []byte("a\x01")), cnName(tagUTF8, []byte("a\x01"))) {
		t.Error("an invalid visible string equals a utf8 string")
	}
	cert := &x509.Certificate{DNSNames: []string{"k.example"}}
	if !tsaNameMatches(tl(0x82, []byte("K.EXAMPLE")), cert) {
		t.Error("ascii case not folded in a dns name")
	}
	if tsaNameMatches(tl(0x82, []byte("\u212a.example")), cert) {
		t.Error("the kelvin sign matches k in a dns name")
	}
}
