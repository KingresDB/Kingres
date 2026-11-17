// SPDX-License-Identifier: AGPL-3.0-only

package signednote

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// The example of the signed-note specification, "Verifier keys / Example".
const (
	specVKey = "example.com/foo+530d903a+AekyeRrm56hApGFkyQR4ZCbV54Id2LKaANYcrnKv3U2k"
	specNote = "This is an example message.\n\n" +
		"\u2014 example.com/foo Uw2QOkn8srV1yJGh2VYRlL1Tnagv1YEq6TfXppzi2ONncAlTgK7Ztg1ERYNZXsYjOBH3mFXmRKuwHjG1Yu72IneyaQM=\n"
)

// signLine builds one signature line with the standard library only, so the
// tests do not share code with the production parser.
func signLine(name string, priv ed25519.PrivateKey, text string) string {
	pub := priv.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(append(append([]byte(name), 0x0A, 0x01), pub...))
	raw := append(append([]byte{}, sum[:4]...), ed25519.Sign(priv, []byte(text))...)
	return "\u2014 " + name + " " + base64.StdEncoding.EncodeToString(raw) + "\n"
}

func newPair(t testing.TB, seed byte) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	priv := ed25519.NewKeyFromSeed(s)
	return priv, priv.Public().(ed25519.PublicKey)
}

func TestSpecVector(t *testing.T) {
	k, err := ParseVKey(specVKey)
	if err != nil {
		t.Fatal(err)
	}
	if k.ID() != 0x530d903a {
		t.Fatalf("key id %08x", k.ID())
	}
	n, err := Parse([]byte(specNote))
	if err != nil {
		t.Fatal(err)
	}
	if string(n.Text) != "This is an example message.\n" {
		t.Fatalf("text %q", n.Text)
	}
	if got, err := Verify(n, []Key{k}); err != nil || len(got) != 1 {
		t.Fatalf("verify: %v %v", got, err)
	}
	// One changed character of the text must fail, not be ignored.
	bad, _ := Parse([]byte(strings.Replace(specNote, "example message", "example massage", 1)))
	if _, err := Verify(bad, []Key{k}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("changed text: %v", err)
	}
}

func TestKeyIDFormula(t *testing.T) {
	_, pub := newPair(t, 7)
	sum := sha256.Sum256(append([]byte("example.com/x\n\x01"), pub...))
	if got, want := KeyID("example.com/x", pub), binary.BigEndian.Uint32(sum[:4]); got != want {
		t.Fatalf("%08x != %08x", got, want)
	}
}

func TestVKeyRejections(t *testing.T) {
	good := specVKey
	for name, s := range map[string]string{
		"two parts":      "example.com/foo+530d903a",
		"four parts":     good + "+x",
		"short id":       "example.com/foo+530d90+AekyeRrm56hApGFkyQR4ZCbV54Id2LKaANYcrnKv3U2k",
		"upper hex id":   "example.com/foo+530D903A+AekyeRrm56hApGFkyQR4ZCbV54Id2LKaANYcrnKv3U2k",
		"wrong id":       "example.com/foo+530d903b+AekyeRrm56hApGFkyQR4ZCbV54Id2LKaANYcrnKv3U2k",
		"wrong name":     "example.com/bar+530d903a+AekyeRrm56hApGFkyQR4ZCbV54Id2LKaANYcrnKv3U2k",
		"wrong type":     "example.com/foo+530d903a+AukyeRrm56hApGFkyQR4ZCbV54Id2LKaANYcrnKv3U2k",
		"bad base64":     "example.com/foo+530d903a+AekyeRrm56hApGFkyQR4ZCbV54Id2LKaANYcrnKv3U2",
		"short key":      "example.com/foo+530d903a+AekyeRrm",
		"empty name":     "+530d903a+AekyeRrm56hApGFkyQR4ZCbV54Id2LKaANYcrnKv3U2k",
		"no name at all": "",
	} {
		if _, err := ParseVKey(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSignAndVerifyOwnNote(t *testing.T) {
	priv1, pub1 := newPair(t, 1)
	priv2, pub2 := newPair(t, 2)
	text := "hello\nworld\n"
	data := text + "\n" + signLine("a.example/log", priv1, text) + signLine("b.example/other", priv2, text)
	n, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	k1, _ := NewKey("a.example/log", pub1)
	k2, _ := NewKey("b.example/other", pub2)
	got, err := Verify(n, []Key{k1})
	if err != nil || len(got) != 1 || got[0].Name != "a.example/log" {
		t.Fatalf("one trusted: %v %v", got, err)
	}
	if got, err = Verify(n, []Key{k1, k2}); err != nil || len(got) != 2 {
		t.Fatalf("two trusted: %v %v", got, err)
	}
	// Only unknown signers: rejected as untrusted, not as a bad signature.
	other, _ := NewKey("c.example/none", pub1)
	if _, err = Verify(n, []Key{other}); !errors.Is(err, ErrNoTrustedSignature) {
		t.Fatalf("unknown signers: %v", err)
	}
	// Same name, other key: the key id differs, so the line is unknown and ignored.
	_, pub3 := newPair(t, 3)
	sameName, _ := NewKey("a.example/log", pub3)
	if _, err = Verify(n, []Key{sameName}); !errors.Is(err, ErrNoTrustedSignature) {
		t.Fatalf("same name other key: %v", err)
	}
	// A known key with a forged signature rejects the note even though a
	// second trusted signature is good.
	forged := text + "\n" + signLine("a.example/log", priv1, "other text\n") + signLine("b.example/other", priv2, text)
	fn, err := Parse([]byte(forged))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(fn, []Key{k1, k2}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("forged line: %v", err)
	}
	// An unknown line with garbage signature bytes is ignored.
	junk := make([]byte, 4+10)
	junk[0] = 1
	withJunk := data + "\u2014 d.example/zz " + base64.StdEncoding.EncodeToString(junk) + "\n"
	jn, err := Parse([]byte(withJunk))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(jn, []Key{k1}); err != nil {
		t.Fatalf("junk unknown line: %v", err)
	}
}

func TestManySignaturesAccepted(t *testing.T) {
	text := "x\n"
	data := text + "\n"
	var keys []Key
	for i := 0; i < 16; i++ {
		priv, pub := newPair(t, byte(10+i))
		name := "k" + string(rune('a'+i)) + ".example/log"
		data += signLine(name, priv, text)
		k, _ := NewKey(name, pub)
		keys = append(keys, k)
	}
	n, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Verify(n, keys); err != nil || len(got) != 16 {
		t.Fatalf("%d %v", len(got), err)
	}
	// More than the limit is refused.
	line := signLine("z.example/log", mustKey(t, 99), text)
	var b strings.Builder
	b.WriteString(text + "\n")
	for i := 0; i <= MaxSignatures; i++ {
		b.WriteString(strings.Replace(line, "z.example", "z"+strings.Repeat("a", i)+".example", 1))
	}
	if _, err := Parse([]byte(b.String())); err == nil {
		t.Fatal("accepted too many signatures")
	}
}

func mustKey(t testing.TB, seed byte) ed25519.PrivateKey {
	priv, _ := newPair(t, seed)
	return priv
}

func TestParseRejections(t *testing.T) {
	priv, _ := newPair(t, 1)
	text := "hello\n"
	line := signLine("a.example/log", priv, text)
	good := text + "\n" + line
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatal(err)
	}
	b64 := line[strings.LastIndex(line, " ")+1 : len(line)-1]
	cases := map[string]string{
		"empty":                   "",
		"no final newline":        strings.TrimSuffix(good, "\n"),
		"no blank line":           text + line,
		"no signatures":           text + "\n",
		"truncated line":          good[:len(good)-12] + "\n",
		"truncated in the middle": good[:len(good)/2],
		"extra blank line after":  good + "\n",
		"text only":               text + "\n\n",
		"invalid utf8":            "he\xffllo\n\n" + line,
		"control char in text":    "he\x01llo\n\n" + line,
		"carriage return":         "hello\r\n\n" + line,
		"tab in text":             "he\tllo\n\n" + line,
		"nul in signature":        text + "\n" + strings.Replace(line, "a.example", "a\x00example", 1),
		"hyphen not em dash":      text + "\n" + "- " + line[len("\u2014 "):],
		"no space after dash":     text + "\n" + "\u2014a.example/log " + b64 + "\n",
		"empty name":              text + "\n" + "\u2014  " + b64 + "\n",
		"name with plus":          text + "\n" + "\u2014 a+b " + b64 + "\n",
		"name with nbsp":          text + "\n" + "\u2014 a\u00a0b " + b64 + "\n",
		"no base64":               text + "\n" + "\u2014 a.example/log\n",
		"extra field":             text + "\n" + "\u2014 a.example/log " + b64 + " extra\n",
		"two spaces":              text + "\n" + "\u2014 a.example/log  " + b64 + "\n",
		"missing padding":         text + "\n" + "\u2014 a.example/log " + strings.TrimRight(b64, "=") + "\n",
		"url base64":              text + "\n" + "\u2014 a.example/log " + strings.NewReplacer("+", "-", "/", "_").Replace(b64) + "\n",
		"trailing bits":           text + "\n" + "\u2014 a.example/log " + nonCanonical(b64) + "\n",
		"short signature":         text + "\n" + "\u2014 a.example/log " + base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) + "\n",
		"duplicate name and id":   good + line,
		"garbage line":            good + "garbage\n",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A very large input is refused before it is scanned.
	if _, err := Parse(make([]byte, MaxNoteSize+1)); err == nil {
		t.Error("oversized note accepted")
	}
}

// nonCanonical changes the unused trailing bits of the last base64 character
// group, which a lenient decoder ignores.
func nonCanonical(s string) string {
	// 68 raw bytes encode to 92 characters ending in "=" (two padding
	// characters would be "=="): flip the last data character.
	body := strings.TrimRight(s, "=")
	pad := s[len(body):]
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	last := strings.IndexByte(alphabet, body[len(body)-1])
	repl := alphabet[(last+1)%64]
	return body[:len(body)-1] + string(repl) + pad
}

func TestExtraTextLinesAndBlankLinesInText(t *testing.T) {
	priv, pub := newPair(t, 5)
	text := "a\n\nb\n"
	n, err := Parse([]byte(text + "\n" + signLine("a.example/log", priv, text)))
	if err != nil {
		t.Fatal(err)
	}
	if string(n.Text) != text {
		t.Fatalf("text %q", n.Text)
	}
	k, _ := NewKey("a.example/log", pub)
	if _, err := Verify(n, []Key{k}); err != nil {
		t.Fatal(err)
	}
}

func TestAmbiguousTrustedKeys(t *testing.T) {
	priv, pub := newPair(t, 5)
	text := "t\n"
	n, _ := Parse([]byte(text + "\n" + signLine("a.example/log", priv, text)))
	k, _ := NewKey("a.example/log", pub)
	if _, err := Verify(n, []Key{k, k}); err != nil {
		t.Fatalf("the same key twice is fine: %v", err)
	}
}

func TestDecodeBase64Canonical(t *testing.T) {
	for _, in := range []string{"AAAA\n", "AA\nAA", "AAAA ", "AB==", "AAA", "A", "AAAAA"} {
		if _, err := DecodeBase64(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	if b, err := DecodeBase64("AAAA"); err != nil || len(b) != 3 {
		t.Fatal(err)
	}
}

func TestNewKeyRejections(t *testing.T) {
	_, pub := newPair(t, 1)
	for _, name := range []string{"", "a b", "a+b", "a\tb", "a\u2003b", "a\xff"} {
		if _, err := NewKey(name, pub); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	if _, err := NewKey("a", pub[:31]); err == nil {
		t.Error("short key accepted")
	}
}

// FuzzParse checks that Parse and Verify never panic and that a parsed note
// has no control character in its text.
func FuzzParse(f *testing.F) {
	f.Add([]byte(specNote))
	f.Add([]byte("a\n\n\u2014 x AAAAAA==\n"))
	f.Add([]byte(""))
	f.Add([]byte("\n\n\n"))
	k, _ := ParseVKey(specVKey)
	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := Parse(data)
		if err != nil {
			return
		}
		for _, b := range n.Text {
			if b < 0x20 && b != '\n' {
				t.Fatalf("control character in text %q", n.Text)
			}
		}
		_, _ = Verify(n, []Key{k})
	})
}

// L9-04: the size of a note, the length of its lines and the lines of its text
// are bounded at the one place every note passes through, so that what a
// signer can make the verifier store is bounded.
func TestParseBoundsTheShapeOfANote(t *testing.T) {
	priv, _ := newPair(t, 1)
	sign := func(text string) string {
		return text + "\n" + signLine("a.example/log", priv, text)
	}
	lines := func(n int) string {
		var b strings.Builder
		b.WriteString("origin\n")
		for i := 1; i < n; i++ {
			b.WriteString("l\n")
		}
		return b.String()
	}
	cases := []struct {
		name string
		note string
		ok   bool
	}{
		{"text lines at the limit", sign(lines(MaxTextLines)), true},
		{"one text line over", sign(lines(MaxTextLines + 1)), false},
		{"line at the limit", sign("o\n" + strings.Repeat("x", MaxLineLength) + "\n"), true},
		{"line one over", sign("o\n" + strings.Repeat("x", MaxLineLength+1) + "\n"), false},
		{"a line of 700000 bytes", sign("o\n" + strings.Repeat("<", 700000) + "\n"), false},
	}
	for _, c := range cases {
		_, err := Parse([]byte(c.note))
		if (err == nil) != c.ok {
			t.Errorf("%s: err %v, accepted should be %v", c.name, err, c.ok)
		}
	}
}

// FuzzParseBounds checks the bounds of a note for any input: whatever Parse
// accepts is within the size, the line length, the lines of text and the
// signatures that bound what a signer can make the verifier store.
func FuzzParseBounds(f *testing.F) {
	f.Add([]byte(specNote))
	f.Add([]byte("o\n" + strings.Repeat("x", MaxLineLength+1) + "\n\n\u2014 a AAAAAA==\n"))
	f.Add([]byte(strings.Repeat("l\n", MaxTextLines+2) + "\n\u2014 a AAAAAA==\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := Parse(data)
		if err != nil {
			return
		}
		if len(data) > MaxNoteSize {
			t.Fatalf("a note of %d bytes was accepted", len(data))
		}
		if len(n.Signatures) > MaxSignatures {
			t.Fatalf("%d signatures were accepted", len(n.Signatures))
		}
		if c := bytes.Count(n.Text, []byte("\n")); c > MaxTextLines {
			t.Fatalf("%d lines of text were accepted", c)
		}
		for _, l := range bytes.Split(data, []byte("\n")) {
			if len(l) > MaxLineLength {
				t.Fatalf("a line of %d bytes was accepted", len(l))
			}
		}
	})
}
