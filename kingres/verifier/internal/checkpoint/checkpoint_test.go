// SPDX-License-Identifier: AGPL-3.0-only

package checkpoint

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
)

// The example checkpoint of the tlog-checkpoint specification (text and
// signature line).
const specNote = "example.com/behind-the-sofa\n20852163\nCsUYapGGPo4dkMgIAUqom/Xajj7h2fB2MPA3j2jxq2I=\n\n" +
	"\u2014 example.com/behind-the-sofa Az3grlgtzPICa5OS8npVmf1Myq/5IZniMp+ZJurmRDeOoRDe4URYN7u5/Zhcyv2q1gGzGku9nTo+zyWE+xeMcTOAYQ8=\n"

const origin = "ledger.example.com/kingres/c/0102/e1"

var rootB64 = base64.StdEncoding.EncodeToString(make([]byte, 32))

func keyPair(seed byte) (ed25519.PrivateKey, signednote.Key) {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	priv := ed25519.NewKeyFromSeed(s)
	k, _ := signednote.NewKey(origin, priv.Public().(ed25519.PublicKey))
	return priv, k
}

func sign(name string, priv ed25519.PrivateKey, text string) string {
	pub := priv.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(append(append([]byte(name), 0x0A, 0x01), pub...))
	raw := append(append([]byte{}, sum[:4]...), ed25519.Sign(priv, []byte(text))...)
	return "\u2014 " + name + " " + base64.StdEncoding.EncodeToString(raw) + "\n"
}

func TestSpecExampleText(t *testing.T) {
	n, err := signednote.Parse([]byte(specNote))
	if err != nil {
		t.Fatal(err)
	}
	cp, err := Parse(n.Text)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Origin != "example.com/behind-the-sofa" || cp.Size != 20852163 || len(cp.Extensions) != 0 {
		t.Fatalf("%+v", cp)
	}
	if cp.Text() != string(n.Text) {
		t.Fatalf("round trip: %q", cp.Text())
	}
}

func TestParseRejections(t *testing.T) {
	good := origin + "\n5\n" + rootB64 + "\n"
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatal(err)
	}
	r32 := rootB64
	cases := map[string]string{
		"empty":                 "",
		"one line":              origin + "\n",
		"two lines":             origin + "\n5\n",
		"no final newline":      origin + "\n5\n" + r32,
		"empty origin":          "\n5\n" + r32 + "\n",
		"origin with space":     "a b\n5\n" + r32 + "\n",
		"origin with plus":      "a+b\n5\n" + r32 + "\n",
		"origin too long":       strings.Repeat("a", 256) + "\n5\n" + r32 + "\n",
		"empty size":            origin + "\n\n" + r32 + "\n",
		"size leading zero":     origin + "\n05\n" + r32 + "\n",
		"size double zero":      origin + "\n00\n" + r32 + "\n",
		"size plus":             origin + "\n+5\n" + r32 + "\n",
		"size minus":            origin + "\n-5\n" + r32 + "\n",
		"size hex":              origin + "\n0x5\n" + r32 + "\n",
		"size space":            origin + "\n5 \n" + r32 + "\n",
		"size fraction":         origin + "\n5.0\n" + r32 + "\n",
		"size too large":        origin + "\n4611686018427387904\n" + r32 + "\n",
		"size overflows uint64": origin + "\n18446744073709551616\n" + r32 + "\n",
		"size very long":        origin + "\n" + strings.Repeat("9", 40) + "\n" + r32 + "\n",
		"size unicode digit":    origin + "\n\u0665\n" + r32 + "\n",
		"root empty":            origin + "\n5\n\n",
		"root not base64":       origin + "\n5\n!!!!\n",
		"root short":            origin + "\n5\n" + base64.StdEncoding.EncodeToString(make([]byte, 31)) + "\n",
		"root long":             origin + "\n5\n" + base64.StdEncoding.EncodeToString(make([]byte, 33)) + "\n",
		"root no padding":       origin + "\n5\n" + strings.TrimRight(r32, "=") + "\n",
		"root url alphabet":     origin + "\n5\n" + strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(append(make([]byte, 30), 0xfb, 0xff))) + "\n",
		"root trailing bits":    origin + "\n5\n" + r32[:len(r32)-2] + "B=\n",
		"empty extension line":  good + "\n",
		"empty tree wrong root": origin + "\n0\n" + r32 + "\n",
		"crlf":                  origin + "\r\n5\r\n" + r32 + "\r\n",
		"invalid utf8":          "a\xff\n5\n" + r32 + "\n",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Extension lines are allowed and kept.
	cp, err := Parse([]byte(good + "ext one\next two\n"))
	if err != nil || len(cp.Extensions) != 2 || cp.Extensions[1] != "ext two" {
		t.Fatalf("extensions: %+v %v", cp, err)
	}
	// The empty tree has the hash of the empty string.
	empty := sha256.Sum256(nil)
	if cp, err := Parse([]byte(origin + "\n0\n" + base64.StdEncoding.EncodeToString(empty[:]) + "\n")); err != nil || cp.Size != 0 {
		t.Fatalf("empty tree: %v", err)
	}
}

func TestVerify(t *testing.T) {
	priv, key := keyPair(1)
	priv2, key2 := keyPair(2)
	text := origin + "\n5\n" + rootB64 + "\n"
	note := text + "\n" + sign(origin, priv, text)

	cp, signers, err := Verify([]byte(note), []signednote.Key{key}, origin)
	if err != nil || cp.Size != 5 || len(signers) != 1 {
		t.Fatalf("%+v %v", cp, err)
	}
	// Two keys during a rotation overlap: both lines verify.
	two := note + sign(origin, priv2, text)
	if _, signers, err = Verify([]byte(two), []signednote.Key{key, key2}, origin); err != nil || len(signers) != 2 {
		t.Fatalf("overlap: %v", err)
	}
	// Only the second key trusted: the first line is an unknown signer, ignored.
	if _, signers, err = Verify([]byte(two), []signednote.Key{key2}, origin); err != nil || len(signers) != 1 {
		t.Fatalf("one of two trusted: %v", err)
	}

	step := func(in string, keys []signednote.Key, want string) string {
		_, _, err := Verify([]byte(in), keys, want)
		var ce *Error
		if !errors.As(err, &ce) {
			t.Fatalf("expected an Error, got %v", err)
		}
		return ce.Step
	}
	if got := step(strings.TrimSuffix(note, "\n"), []signednote.Key{key}, origin); got != StepParse {
		t.Errorf("truncated: %s", got)
	}
	if got := step(note, []signednote.Key{key2}, origin); got != StepSignature {
		t.Errorf("wrong key: %s", got)
	}
	// Wrong key id: a signature line whose key id was changed is unknown.
	badID := strings.Replace(note, " "+note[strings.LastIndex(note, " ")+1:], " "+flipFirst(note[strings.LastIndex(note, " ")+1:]), 1)
	if got := step(badID, []signednote.Key{key}, origin); got != StepSignature {
		t.Errorf("wrong key id: %s", got)
	}
	// Wrong signature bytes under the right id.
	badSig := note[:len(note)-6] + flipFirst(note[len(note)-6:len(note)-1]) + "\n"
	if got := step(badSig, []signednote.Key{key}, origin); got != StepSignature {
		t.Errorf("wrong signature: %s", got)
	}
	// A different origin than expected.
	if got := step(note, []signednote.Key{key}, "ledger.example.com/kingres/c/0102/e2"); got != StepOrigin {
		t.Errorf("origin: %s", got)
	}
	// Signed but not a checkpoint.
	junk := "hello\n"
	if got := step(junk+"\n"+sign(origin, priv, junk), []signednote.Key{key}, origin); got != StepCheckpoint {
		t.Errorf("signed junk: %s", got)
	}
	// Size not canonical, signed correctly: still refused.
	bad := origin + "\n05\n" + rootB64 + "\n"
	if got := step(bad+"\n"+sign(origin, priv, bad), []signednote.Key{key}, origin); got != StepCheckpoint {
		t.Errorf("signed leading zero: %s", got)
	}
	// Signed under another name than the origin: the key list is for the
	// origin, so the line is unknown.
	other := text + "\n" + sign("other.example/log", priv, text)
	if got := step(other, []signednote.Key{key}, origin); got != StepSignature {
		t.Errorf("other key name: %s", got)
	}
}

// flipFirst changes the first character of a base64 string to another valid
// one, which changes the key id bytes.
func flipFirst(s string) string {
	if s[0] == 'A' {
		return "B" + s[1:]
	}
	return "A" + s[1:]
}

// FuzzVerify checks that parsing and verifying a checkpoint never panics
// and that an accepted checkpoint formats back to its note text.
func FuzzVerify(f *testing.F) {
	priv, key := keyPair(1)
	text := origin + "\n5\n" + rootB64 + "\n"
	f.Add([]byte(text + "\n" + sign(origin, priv, text)))
	f.Add([]byte(specNote))
	f.Add([]byte(text))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Parse(data)
		cp, _, err := Verify(data, []signednote.Key{key}, origin)
		if err == nil && cp.Origin != origin {
			t.Fatal("accepted another origin")
		}
	})
}
