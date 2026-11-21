// SPDX-License-Identifier: AGPL-3.0-only

package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
)

// testKey returns a key made in the test from a fixed seed: no real key is
// ever created by or committed with this package.
func testKey(seedByte byte) ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = seedByte
	}
	return ed25519.NewKeyFromSeed(seed)
}

func pubOf(priv ed25519.PrivateKey) ed25519.PublicKey { return priv.Public().(ed25519.PublicKey) }

func keyOf(t *testing.T, priv ed25519.PrivateKey) signednote.Key {
	t.Helper()
	k, err := ParseKey(VKey(pubOf(priv)))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sums(t *testing.T) []byte {
	t.Helper()
	var es []Entry
	for _, n := range []string{"kingres-verify-linux-amd64", "kingres-verify-windows-amd64.exe"} {
		es = append(es, Entry{Hash: sha256.Sum256([]byte(n)), Name: n})
	}
	b, err := FormatSums(es)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSignAndVerify(t *testing.T) {
	priv := testKey(1)
	s := sums(t)
	note, err := Sign(priv, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{VKey(pubOf(priv)), base64.StdEncoding.EncodeToString(pubOf(priv))} {
		k, err := ParseKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyNote(s, note, k); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	es, err := ParseSums(s)
	if err != nil || len(es) != 2 {
		t.Fatalf("%v %d", err, len(es))
	}
	if e, ok := Find(es, "kingres-verify-linux-amd64"); !ok || e.Hash != sha256.Sum256([]byte(e.Name)) {
		t.Error("Find")
	}
}

func TestVerifyRefuses(t *testing.T) {
	priv, other := testKey(1), testKey(2)
	key := keyOf(t, priv)
	s := sums(t)
	note, _ := Sign(priv, s)

	if err := VerifyNote(s, note, keyOf(t, other)); err == nil {
		t.Error("a note was accepted under another key")
	}
	otherNote, _ := Sign(other, s)
	if err := VerifyNote(s, otherNote, key); err == nil {
		t.Error("a note signed by another key was accepted")
	}
	changed := []byte(strings.Replace(string(s), "kingres-verify-linux", "kingres-verify-linuy", 1))
	if err := VerifyNote(changed, note, key); err != ErrNoteText {
		t.Errorf("sums that differ from the note text: %v", err)
	}
	flipped := append([]byte(nil), note...)
	flipped[3] ^= 1 // inside the text: the signature no longer covers it
	if err := VerifyNote(s, flipped, key); err == nil {
		t.Error("a note with changed text was accepted")
	}
	if err := VerifyNote(s, s, key); err == nil {
		t.Error("the sums file was accepted as a note")
	}
}

func TestParseSumsStrict(t *testing.T) {
	h := strings.Repeat("ab", 32)
	good := h + "  kingres-verify-linux-amd64\n"
	if _, err := ParseSums([]byte(good)); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"empty":         "",
		"no newline":    strings.TrimSuffix(good, "\n"),
		"one space":     h + " kingres-verify\n",
		"binary mark":   h + " *kingres-verify\n",
		"upper hex":     strings.ToUpper(h) + "  x\n",
		"short hash":    h[:62] + "  x\n",
		"path":          h + "  dir/x\n",
		"dot dot":       h + "  ..\n",
		"dash name":     h + "  -rf\n",
		"hidden":        h + "  .x\n",
		"space in name": h + "  a b\n",
		"blank line":    good + "\n",
		"twice":         good + good,
		"no name":       h + "  \n",
		"crlf":          h + "  x\r\n",
	}
	for n, in := range bad {
		if _, err := ParseSums([]byte(in)); err == nil {
			t.Errorf("%s: accepted", n)
		}
	}
	if _, err := Sign(testKey(1), []byte(h+"  ../x\n")); err == nil {
		t.Error("Sign accepted sums that ParseSums refuses")
	}
}

func TestParseKeyRefuses(t *testing.T) {
	good := VKey(pubOf(testKey(1)))
	for n, in := range map[string]string{
		"other name": strings.Replace(good, KeyName, "someone-else", 1),
		"wrong id":   strings.Replace(good, "+", "+00", 1),
		"short":      "AAAA",
		"not base64": "!!!!",
		"empty":      "",
	} {
		if _, err := ParseKey(in); err == nil {
			t.Errorf("%s: accepted", n)
		}
	}
}

func FuzzParseSums(f *testing.F) {
	f.Add([]byte(strings.Repeat("ab", 32) + "  x\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		es, err := ParseSums(in)
		if err != nil {
			return
		}
		out, err := FormatSums(es)
		if err != nil || string(out) != string(in) {
			t.Fatalf("not canonical: %q", in)
		}
	})
}

// A verifier key string holds the base64 of the key after two plus signs, and
// that base64 may hold plus signs itself: every one of these must parse.
func TestEveryKeyStringParses(t *testing.T) {
	withPlus := 0
	for i := 0; i < 200; i++ {
		pub := pubOf(testKey(byte(i)))
		s := VKey(pub)
		if strings.Contains(s[strings.LastIndex(s, "+")+1:], "+") || strings.Count(s, "+") > 2 {
			withPlus++
		}
		k, err := ParseKey(s)
		if err != nil || string(k.Public) != string(pub) {
			t.Fatalf("seed %d: %s: %v", i, s, err)
		}
	}
	if withPlus == 0 {
		t.Error("the test never met a key with a plus sign in its base64")
	}
}

// The raw base64 form of a key may hold plus signs and is still the raw form.
func TestRawKeyWithPlusParses(t *testing.T) {
	n := 0
	for i := 0; i < 200; i++ {
		pub := pubOf(testKey(byte(i)))
		s := base64.StdEncoding.EncodeToString(pub)
		if !strings.Contains(s, "+") {
			continue
		}
		n++
		if k, err := ParseKey(s); err != nil || string(k.Public) != string(pub) {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if n == 0 {
		t.Error("no key with a plus sign met")
	}
}
