// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	_ "embed"
	"strconv"
	"strings"
	"testing"
)

// the corpus is part of the test binary: it runs where the source directory is absent
//
//go:embed testdata/payload_corpus.txt
var payloadCorpus string

// A3-05: ParsePayload is held to a corpus of notes with the verdict of each rule
// set (testdata/payload_corpus.txt). The test needs no other module: the
// verifier column is data, checked against the verifier's parsers when the
// corpus was written.
func TestPayloadCorpus(t *testing.T) {
	n, stricter := 0, 0
	for i, line := range strings.Split(payloadCorpus, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			t.Fatalf("line %d: %d fields, want 4", i+1, len(f))
		}
		note, err := strconv.Unquote(f[3])
		if err != nil {
			t.Fatalf("line %d (%s): %v", i+1, f[0], err)
		}
		for _, v := range f[1:3] {
			if v != "accept" && v != "refuse" {
				t.Fatalf("line %d (%s): verdict %q", i+1, f[0], v)
			}
		}
		_, perr := ParsePayload("ab", []byte(note))
		if got := perr == nil; got != (f[1] == "accept") {
			t.Errorf("%s: ParsePayload accepted=%v (%v), the corpus says %s", f[0], got, perr, f[1])
		}
		// the same through NewPayload
		_, nerr := NewPayload("ab", []byte(note), t0)
		if (nerr == nil) != (perr == nil) {
			t.Errorf("%s: NewPayload and ParsePayload disagree", f[0])
		}
		// where the two rule sets differ, this package is the stricter one, on
		// purpose, and the case says so in its name
		if f[1] != f[2] {
			stricter++
			if f[1] != "refuse" || !strings.Contains(f[0], "stricter-on-purpose") {
				t.Errorf("%s: the verdicts differ and this package is not the stricter, or the name does not say so", f[0])
			}
		} else if strings.Contains(f[0], "stricter-on-purpose") {
			t.Errorf("%s: named stricter-on-purpose but the verdicts agree", f[0])
		}
		n++
	}
	if n < 30 || stricter < 3 {
		t.Fatalf("the corpus is too small: %d cases, %d of them stricter on purpose", n, stricter)
	}
}

// A3-05: the other stricter place, the size of the note: 16 KiB here, 64 KiB at
// the verifier. A note between the two is refused by this package on purpose.
func TestNoteSizeIsStricterThanTheVerifierOnPurpose(t *testing.T) {
	if _, err := ParsePayload("ab", sizedNote(MaxSignedNote)); err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePayload("ab", sizedNote(MaxSignedNote+1)); err == nil {
		t.Fatal("a note over the limit of this package accepted")
	}
}
