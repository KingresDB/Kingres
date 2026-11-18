// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

func goodCheckpoint() Checkpoint {
	return Checkpoint{Origin: testOrigin(7), Size: 17, Root: LeafHash([]byte("root"))}
}

func TestCheckpointTextRoundTrip(t *testing.T) {
	c := goodCheckpoint()
	text, err := c.Text()
	if err != nil {
		t.Fatal(err)
	}
	want := c.Origin + "\n17\n" + base64.StdEncoding.EncodeToString(c.Root[:]) + "\n"
	if string(text) != want {
		t.Fatalf("%q", text)
	}
	back, err := ParseCheckpoint(text)
	if err != nil || back.Origin != c.Origin || back.Size != 17 || back.Root != c.Root || back.Extensions != nil {
		t.Fatalf("%+v %v", back, err)
	}
	again, _ := back.Text()
	if !bytes.Equal(again, text) {
		t.Fatal("serialise(parse(b)) != b")
	}
	c.Extensions = []string{"one", "two words"}
	text, err = c.Text()
	if err != nil {
		t.Fatal(err)
	}
	back, err = ParseCheckpoint(text)
	if err != nil || len(back.Extensions) != 2 || back.Extensions[1] != "two words" {
		t.Fatalf("%+v %v", back, err)
	}
}

func TestCheckpointRefusals(t *testing.T) {
	c := goodCheckpoint()
	root := base64.StdEncoding.EncodeToString(c.Root[:])
	empty := base64.StdEncoding.EncodeToString(func() []byte { h := EmptyRoot(); return h[:] }())
	o := c.Origin
	cases := map[string]string{
		"two lines":         o + "\n17\n",
		"no final newline":  o + "\n17\n" + root,
		"empty line":        o + "\n\n" + root + "\n",
		"blank in the end":  o + "\n17\n" + root + "\n\n",
		"leading zero":      o + "\n017\n" + root + "\n",
		"plus sign":         o + "\n+17\n" + root + "\n",
		"minus":             o + "\n-1\n" + root + "\n",
		"hex":               o + "\n0x11\n" + root + "\n",
		"space in size":     o + "\n17 \n" + root + "\n",
		"size 2^62":         o + "\n4611686018427387904\n" + root + "\n",
		"size too long":     o + "\n99999999999999999999\n" + root + "\n",
		"root short":        o + "\n17\n" + root[:40] + "\n",
		"root unpadded":     o + "\n17\n" + strings.TrimRight(root, "=") + "\n",
		"root url alphabet": o + "\n17\n" + strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))) + "\n",
		"size 0 other root": o + "\n0\n" + root + "\n",
		"origin with space": "a b\n17\n" + root + "\n",
		"origin with plus":  "a+b\n17\n" + root + "\n",
		"empty origin":      "\n17\n" + root + "\n",
		"crlf":              o + "\r\n17\r\n" + root + "\r\n",
		"control char":      o + "\n17\n" + root + "\n\x01\n",
		"invalid utf8":      o + "\n17\n" + root + "\n\xff\n",
		"empty extension":   o + "\n17\n" + root + "\n\nx\n",
		"origin too long":   strings.Repeat("o", MaxOriginSize+1) + "\n17\n" + root + "\n",
		"17 extensions":     o + "\n17\n" + root + "\n" + strings.Repeat("e\n", MaxExtensionLines+1),
		"33 lines":          o + "\n17\n" + root + "\n" + strings.Repeat("e\n", 30),
		"line over 1024":    o + "\n17\n" + root + "\n" + strings.Repeat("e", MaxLineLength+1) + "\n",
		"nothing":           "",
		"newline only":      "\n",
	}
	for name, in := range cases {
		if _, err := ParseCheckpoint([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The boundaries that pass.
	ok := map[string]string{
		"origin 255":    strings.Repeat("o", MaxOriginSize) + "\n17\n" + root + "\n",
		"16 extensions": o + "\n17\n" + root + "\n" + strings.Repeat("e\n", MaxExtensionLines),
		"line 1024":     o + "\n17\n" + root + "\n" + strings.Repeat("e", MaxLineLength) + "\n",
		"size 0":        o + "\n0\n" + empty + "\n",
		"largest size":  o + "\n4611686018427387903\n" + root + "\n",
	}
	for name, in := range ok {
		c, err := ParseCheckpoint([]byte(in))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if back, err := c.Text(); err != nil || string(back) != in {
			t.Errorf("%s: text does not round trip: %v", name, err)
		}
	}
	// Text refuses what Parse refuses.
	bad := goodCheckpoint()
	bad.Origin = "a b"
	if _, err := bad.Text(); err == nil {
		t.Error("Text of an invalid origin")
	}
	bad = goodCheckpoint()
	bad.Size = MaxTreeSize
	if _, err := bad.Text(); err == nil {
		t.Error("Text of size 2^62")
	}
	bad = Checkpoint{Origin: o, Size: 0, Root: c.Root}
	if _, err := bad.Text(); err == nil {
		t.Error("Text of size 0 with another root")
	}
	bad = goodCheckpoint()
	bad.Extensions = make([]string, MaxExtensionLines+1)
	for i := range bad.Extensions {
		bad.Extensions[i] = "e"
	}
	if _, err := bad.Text(); err == nil {
		t.Error("Text with 17 extension lines")
	}
}

func TestSignedCheckpointWithTwoKeys(t *testing.T) {
	k1, k2 := testKey(t, 1), testKey(t, 100)
	c := goodCheckpoint()
	note, err := SignCheckpoint(bg, c, k1, k2)
	if err != nil {
		t.Fatal(err)
	}
	n, err := ParseNote(note)
	if err != nil || len(n.Signatures) != 2 {
		t.Fatalf("%v", err)
	}
	for i, k := range []*ledgerkeys.FileSigner{k1, k2} {
		s := n.Signatures[i]
		if s.Name != c.Origin || s.ID != ledgerkeys.KeyID(c.Origin, k.Public()) {
			t.Errorf("signature %d: name %q id %08x", i, s.Name, s.ID)
		}
	}
	back, err := n.Marshal()
	if err != nil || !bytes.Equal(back, note) {
		t.Fatal("serialise(parse(b)) != b")
	}
	got, signers, err := VerifyCheckpoint(note, c.Origin, k1.Public(), k2.Public())
	if err != nil || got.Size != 17 || len(signers) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	// Either key alone is enough, an unknown key is ignored.
	if _, signers, err := VerifyCheckpoint(note, c.Origin, k2.Public()); err != nil || len(signers) != 1 {
		t.Fatal(err)
	}
	if _, _, err := VerifyCheckpoint(note, c.Origin, testKey(t, 50).Public()); !errors.Is(err, ErrNoTrustedSignature) {
		t.Fatalf("an unknown key: %v", err)
	}
	if _, _, err := VerifyCheckpoint(note, c.Origin+"x", k1.Public()); err == nil {
		t.Fatal("another origin accepted")
	}
	// A bad signature from a known key rejects the whole note.
	bad := bytes.Clone(note)
	i := bytes.Index(bad, []byte("\n\n")) + 2 + len("\u2014 ") + len(c.Origin) + 1 + 10
	bad[i] ^= 1
	if _, _, err := VerifyCheckpoint(bad, c.Origin, k1.Public(), k2.Public()); err == nil {
		t.Fatal("a damaged signature was accepted")
	}
	// Re-signing adds the second key to a note with one signature; the same
	// key twice is refused.
	one, _ := SignCheckpoint(bg, c, k1)
	two, err := ResignCheckpoint(bg, one, usable(k1.Public()), k2)
	if err != nil || !bytes.Equal(two, note) {
		t.Fatalf("resign: %v", err)
	}
	if _, err := ResignCheckpoint(bg, one, usable(k1.Public()), k1); err == nil {
		t.Fatal("the same key twice")
	}
	if _, err := SignCheckpoint(bg, c, k1, k1); err == nil {
		t.Fatal("the same signer twice")
	}
	if _, err := SignCheckpoint(bg, c); err == nil {
		t.Fatal("no signer")
	}
}

func TestSignatureCountBoundary(t *testing.T) {
	c := goodCheckpoint()
	var signers []ledgerkeys.Signer
	for i := 0; i < MaxSignatures+1; i++ {
		signers = append(signers, testKey(t, byte(i+1)))
	}
	note, err := SignCheckpoint(bg, c, signers[:MaxSignatures]...)
	if err != nil {
		t.Fatalf("MaxSignatures: %v", err)
	}
	if n, err := ParseNote(note); err != nil || len(n.Signatures) != MaxSignatures {
		t.Fatalf("parse of MaxSignatures: %v", err)
	}
	if _, err := SignCheckpoint(bg, c, signers...); err == nil {
		t.Fatal("MaxSignatures+1 signed")
	}
	// A parser sees 33 lines too: build one by hand.
	n, _ := ParseNote(note)
	n.Signatures = append(n.Signatures, NoteSignature{Name: c.Origin, ID: 1, Sig: make([]byte, 64)})
	if _, err := n.Marshal(); err == nil {
		t.Fatal("Marshal of 33 signatures")
	}
	raw := append(bytes.Clone(note), []byte(sigLine(NoteSignature{Name: c.Origin, ID: 1, Sig: make([]byte, 64)}))...)
	if _, err := ParseNote(raw); err == nil {
		t.Fatal("Parse of 33 signatures")
	}
}

func TestNoteSizeBoundaries(t *testing.T) {
	k := testKey(t, 1)
	c := goodCheckpoint()
	// 16 extension lines of 1,024 bytes: the longest checkpoint text.
	c.Extensions = nil
	for i := 0; i < MaxExtensionLines; i++ {
		c.Extensions = append(c.Extensions, strings.Repeat(string(rune('a'+i)), MaxLineLength))
	}
	note, err := SignCheckpoint(bg, c, k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseNote(note); err != nil {
		t.Fatal(err)
	}
	// MaxNoteSize: pad with signatures until the note passes 64 KiB is not
	// possible with 32 lines of 1,024 bytes (about 64 KiB), so test Parse of
	// a note of exactly MaxNoteSize and one more byte directly.
	atLimit := bytes.Repeat([]byte("a"), MaxNoteSize)
	if _, err := ParseNote(atLimit); err == nil || strings.Contains(err.Error(), "at most") {
		t.Errorf("MaxNoteSize bytes: %v", err)
	}
	if _, err := ParseNote(append(atLimit, 'a')); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("MaxNoteSize+1 bytes: %v", err)
	}
	// 32 text lines pass, 33 do not; 1,024 bytes a line pass, 1,025 do not.
	lines := func(n, width int) []byte {
		var b bytes.Buffer
		for i := 0; i < n; i++ {
			b.WriteString(strings.Repeat("x", width) + "\n")
		}
		return b.Bytes()
	}
	if _, err := SignNote(bg, lines(MaxTextLines, 10), "n", k); err != nil {
		t.Errorf("32 lines: %v", err)
	}
	if _, err := SignNote(bg, lines(MaxTextLines+1, 10), "n", k); err == nil {
		t.Error("33 lines")
	}
	if _, err := SignNote(bg, lines(1, MaxLineLength), "n", k); err != nil {
		t.Errorf("a line of 1,024 bytes: %v", err)
	}
	if _, err := SignNote(bg, lines(1, MaxLineLength+1), "n", k); err == nil {
		t.Error("a line of 1,025 bytes")
	}
	// The key name can make the signature line too long.
	longName := strings.Repeat("n", MaxLineLength)
	if _, err := SignNote(bg, lines(1, 3), longName, k); err == nil {
		t.Error("a signature line over 1,024 bytes")
	}
	// The signature line of an origin at the limit fits.
	if _, err := SignCheckpoint(bg, Checkpoint{Origin: strings.Repeat("o", MaxOriginSize), Size: 1, Root: Hash{1}}, k); err != nil {
		t.Errorf("origin of 255 bytes: %v", err)
	}
}

func TestParseNoteRefusals(t *testing.T) {
	k := testKey(t, 1)
	note, _ := SignCheckpoint(bg, goodCheckpoint(), k)
	s := string(note)
	sep := strings.Index(s, "\n\n")
	text, sigs := s[:sep+1], s[sep+2:]
	cases := map[string]string{
		"empty":               "",
		"no blank line":       text + sigs[:len(sigs)],
		"no signatures":       text + "\n",
		"no final newline":    strings.TrimSuffix(s, "\n"),
		"hyphen for dash":     text + "\n- " + sigs[len("\u2014 "):],
		"no space after dash": text + "\n\u2014" + sigs[len("\u2014 "):],
		"two spaces":          text + "\n" + strings.Replace(sigs, " ", "  ", 1),
		"extra field":         text + "\n" + strings.TrimSuffix(sigs, "\n") + " extra\n",
		"short signature":     text + "\n\u2014 " + goodCheckpoint().Origin + " " + base64.StdEncoding.EncodeToString(make([]byte, 20)) + "\n",
		"3 byte sig":          text + "\n\u2014 " + goodCheckpoint().Origin + " " + base64.StdEncoding.EncodeToString(make([]byte, 3)) + "\n",
		"unpadded":            text + "\n" + strings.TrimSuffix(strings.TrimSuffix(sigs, "\n"), "=") + "\n",
		"duplicate line":      s + sigs,
		"control char":        strings.Replace(s, "kingres", "king\x07res", 1),
		"invalid utf8":        s + "\xff",
		"cr":                  strings.Replace(s, "\n", "\r\n", 1),
	}
	for name, in := range cases {
		if _, err := ParseNote([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A key name with a space or plus on a signature line.
	for _, name := range []string{"a+b", "\u00a0x"} {
		line := "\u2014 " + name + " " + base64.StdEncoding.EncodeToString(make([]byte, 68)) + "\n"
		if _, err := ParseNote([]byte(text + "\n" + line)); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	// A text with a blank line cannot be signed.
	if _, err := SignNote(bg, []byte("a\n\nb\n"), "n", k); err == nil {
		t.Error("a text with a blank line")
	}
	if _, err := SignNote(bg, []byte("no newline"), "n", k); err == nil {
		t.Error("a text without a final newline")
	}
	if _, err := SignNote(bg, []byte("a\x00b\n"), "n", k); err == nil {
		t.Error("a text with a control character")
	}
	if _, err := SignNote(bg, []byte("a\n"), "bad name", k); err == nil {
		t.Error("an invalid key name")
	}
}

func TestChainAndAggregateOrigins(t *testing.T) {
	var id [32]byte
	id[0], id[31] = 0xab, 0xcd
	o, err := ChainOrigin(testPrefix, id, 12)
	if err != nil || o != testPrefix+"/c/ab000000000000000000000000000000000000000000000000000000000000cd/e12" {
		t.Fatalf("%q %v", o, err)
	}
	a, err := AggregateOrigin(testPrefix, id)
	if err != nil || a != testPrefix+"/a/ab000000000000000000000000000000000000000000000000000000000000cd" {
		t.Fatalf("%q %v", a, err)
	}
	if _, err := ChainOrigin(testPrefix, id, 0); err == nil {
		t.Error("epoch 0")
	}
	// The longest origin fits in 255 bytes when the prefix is at its limit.
	long := strings.Repeat("p", ledgerkeys.MaxPrefixSize)
	if o, err := ChainOrigin(long, id, 4294967295); err != nil || len(o) > MaxOriginSize || !ValidOrigin(o) {
		t.Errorf("longest chain origin: %d bytes %v", len(o), err)
	}
	if _, err := ChainOrigin(long+"p", id, 1); err == nil {
		t.Error("a prefix over the limit")
	}
	if _, err := AggregateOrigin("https://x", id); err == nil {
		t.Error("a prefix with a scheme")
	}
}
