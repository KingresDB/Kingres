// SPDX-License-Identifier: AGPL-3.0-only

package signednote

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// These tests came from a mutation run of the package (every comparison, every
// constant and every statement changed one at a time): each of them kills
// mutants that the earlier tests let live. The limits are exact, and so are
// the messages that tell a reader which rule a note broke.

// sigLineOfLength is a well formed signature line of exactly n bytes (without its
// newline), with a name made of letters.
func sigLineOfLength(t testing.TB, seed int, n int) string {
	t.Helper()
	const fixed = 4 /* em dash and space */ + 1 /* space */ + 92 /* base64 of 68 bytes */
	nameLen := n - fixed
	if nameLen < 1 {
		t.Fatalf("a line of %d bytes has no room for a name", n)
	}
	name := fmt.Sprintf("%0*d", nameLen, seed)
	raw := make([]byte, 68)
	return emDashPrefix + name + " " + base64.StdEncoding.EncodeToString(raw)
}

func TestLimitsAreExact(t *testing.T) {
	priv, _ := newPair(t, 1)
	text := "origin/x\n1\nAAAA\n"
	one := signLine("origin/x", priv, text)

	// the number of signature lines: 32 are read, 33 are not
	mk := func(n int) string {
		s := text + "\n"
		for i := 0; i < n; i++ {
			s += signLine(fmt.Sprintf("n%d", i), priv, text)
		}
		return s
	}
	if _, err := Parse([]byte(mk(MaxSignatures))); err != nil {
		t.Errorf("%d signatures: %v", MaxSignatures, err)
	}
	if _, err := Parse([]byte(mk(MaxSignatures + 1))); err == nil || !strings.Contains(err.Error(), "more than 32 signatures") {
		t.Errorf("%d signatures: %v", MaxSignatures+1, err)
	}

	// the lines of the text: 32 are read, 33 are not
	lines := func(n int) string { return strings.Repeat("line\n", n) + "\n" + one }
	if _, err := Parse([]byte(lines(MaxTextLines))); err != nil {
		t.Errorf("%d text lines: %v", MaxTextLines, err)
	}
	if _, err := Parse([]byte(lines(MaxTextLines + 1))); err == nil || !strings.Contains(err.Error(), "text has 33 lines") {
		t.Errorf("%d text lines: %v", MaxTextLines+1, err)
	}

	// the length of a line: 1,024 bytes are read, 1,025 are not, in the text and in a signature line
	long := func(n int) string { return strings.Repeat("x", n) + "\n\n" + one }
	if _, err := Parse([]byte(long(MaxLineLength))); err != nil {
		t.Errorf("a text line of %d bytes: %v", MaxLineLength, err)
	}
	if _, err := Parse([]byte(long(MaxLineLength + 1))); err == nil || !strings.Contains(err.Error(), "line of 1025 bytes") {
		t.Errorf("a text line of %d bytes: %v", MaxLineLength+1, err)
	}
	sigAt := func(n int) string { return text + "\n" + sigLineOfLength(t, 1, n) + "\n" }
	if _, err := Parse([]byte(sigAt(MaxLineLength))); err != nil {
		t.Errorf("a signature line of %d bytes: %v", MaxLineLength, err)
	}
	if _, err := Parse([]byte(sigAt(MaxLineLength + 1))); err == nil || !strings.Contains(err.Error(), "line of 1025 bytes") {
		t.Errorf("a signature line of %d bytes: %v", MaxLineLength+1, err)
	}

	// the size of the note: exactly 64 KiB is read, one byte more is not. 32 signature lines
	// and 32 text lines of the greatest length fill it with room to spare, so the size is made
	// exact with the length of the last text line.
	build := func(total int) string {
		var sig strings.Builder
		for i := 0; i < MaxSignatures; i++ {
			sig.WriteString(sigLineOfLength(t, i, MaxLineLength) + "\n")
		}
		rest := total - sig.Len() - 1 // the blank line
		var b strings.Builder
		for i := 0; i < MaxTextLines; i++ {
			n := MaxLineLength + 1 // with its newline
			if i == MaxTextLines-1 {
				n = rest - b.Len()
			}
			b.WriteString(strings.Repeat("y", n-1) + "\n")
		}
		return b.String() + "\n" + sig.String()
	}
	exact := build(MaxNoteSize)
	if len(exact) != MaxNoteSize {
		t.Fatalf("the test note has %d bytes, want %d", len(exact), MaxNoteSize)
	}
	if _, err := Parse([]byte(exact)); err != nil {
		t.Errorf("a note of %d bytes: %v", MaxNoteSize, err)
	}
	if _, err := Parse([]byte(build(MaxNoteSize + 1))); err == nil || !strings.Contains(err.Error(), "note has 65537 bytes") {
		t.Errorf("a note of %d bytes: %v", MaxNoteSize+1, err)
	}
}

func TestParseDetailsAndMessages(t *testing.T) {
	priv, pub := newPair(t, 1)
	// a note with no text at all, which only the checkpoint layer refuses
	empty := "\n\n" + signLine("n", priv, "\n")
	n, err := Parse([]byte(empty))
	if err != nil || string(n.Text) != "\n" {
		t.Fatalf("a note with an empty text: %v %q", err, empty)
	}
	// no signature line: the blank line is the last thing
	if _, err := Parse([]byte("t\n\n")); err == nil || !strings.Contains(err.Error(), "no signature line") {
		t.Errorf("no signature line: %v", err)
	}
	// a line with no space after the name
	if _, err := Parse([]byte("t\n\n" + emDashPrefix + "name\n")); err == nil || !strings.Contains(err.Error(), "does not have the form") {
		t.Errorf("no space: %v", err)
	}
	// a signature of the four bytes of the key id alone is a line; three bytes are not
	idOnly := func(n int) string {
		return "t\n\n" + emDashPrefix + "name " + base64.StdEncoding.EncodeToString(make([]byte, n)) + "\n"
	}
	if p, err := Parse([]byte(idOnly(4))); err != nil || len(p.Signatures[0].Sig) != 0 {
		t.Errorf("a signature of 4 bytes: %v", err)
	}
	if _, err := Parse([]byte(idOnly(3))); err == nil || !strings.Contains(err.Error(), "at least 4") {
		t.Errorf("a signature of 3 bytes: %v", err)
	}
	// such a line never verifies, and VerifyHeld counts it and refuses the note
	key := Key{Name: "name", Public: pub}
	p4, _ := Parse([]byte(idOnly(4)))
	raw4 := p4.Signatures[0]
	raw4.ID = key.ID()
	p4.Signatures[0] = raw4
	if _, wrong, err := VerifyHeld(p4, []Key{key}); !errors.Is(err, ErrBadSignature) || wrong != 1 {
		t.Errorf("a line with no signature bytes: %d %v", wrong, err)
	}
	// nothing named a key: no line was wrong
	if _, wrong, err := VerifyHeld(p4, []Key{{Name: "other", Public: pub}}); !errors.Is(err, ErrNoTrustedSignature) || wrong != 0 {
		t.Errorf("an unknown key: %d %v", wrong, err)
	}
}

// The canonical form orders lines by key name, then by key id, whatever the
// order of the note as served.
func TestCanonicalOrderIsByNameThenKeyID(t *testing.T) {
	text := "origin/x\n1\nAAAA\n"
	var keys []Key
	var privs []ed25519.PrivateKey
	for i, name := range []string{"b", "a", "a", "a", "c"} {
		priv, pub := newPair(t, byte(10+i))
		keys = append(keys, Key{Name: name, Public: pub})
		privs = append(privs, priv)
	}
	// the note as served: reverse order
	note := text + "\n"
	for i := len(keys) - 1; i >= 0; i-- {
		note += signLine(keys[i].Name, privs[i], text)
	}
	n, err := Parse([]byte(note))
	if err != nil {
		t.Fatal(err)
	}
	ok, wrong, err := VerifyHeld(n, keys)
	if err != nil || wrong != 0 || len(ok) != 5 {
		t.Fatalf("%v %d %v", ok, wrong, err)
	}
	out, err := Parse(Canonical(n, ok))
	if err != nil {
		t.Fatal(err)
	}
	type nid struct {
		name string
		id   uint32
	}
	var got []nid
	for _, s := range out.Signatures {
		got = append(got, nid{s.Name, s.ID})
	}
	want := append([]nid(nil), got...)
	sort.Slice(want, func(i, j int) bool {
		if want[i].name != want[j].name {
			return want[i].name < want[j].name
		}
		return want[i].id < want[j].id
	})
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("not ordered by name and key id:\n%v\n%v", got, want)
	}
	if got[0].name != "a" || got[len(got)-1].name != "c" {
		t.Errorf("names out of order: %v", got)
	}
}
