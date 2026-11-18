// SPDX-License-Identifier: AGPL-3.0-only

package checkpoint

import (
	_ "embed"
	"strconv"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
)

// The corpus is part of the test binary: it runs where the source directory is
// absent. It is a byte-identical copy of the corpus of the anchoring libraries
// of the control plane (control-plane/internal/anchor/testdata/payload_corpus.txt;
// a test there fails when the two differ). Its "verifier" column is the verdict
// of this module's parsers on a note: signednote.Parse, then Parse of the text.
//
//go:embed testdata/payload_corpus.txt
var payloadCorpus string

// TestPayloadCorpusVerifierColumn holds the verifier column of the shared corpus
// to the real parsers, so that the claim "the payload rules of the anchoring
// libraries are the verifier's" holds by a test that runs here and not by a
// column written by hand.
func TestPayloadCorpusVerifierColumn(t *testing.T) {
	n, refused := 0, 0
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
		if f[2] != "accept" && f[2] != "refuse" {
			t.Fatalf("line %d (%s): verdict %q", i+1, f[0], f[2])
		}
		parsed, perr := signednote.Parse([]byte(note))
		if perr == nil {
			_, perr = Parse(parsed.Text)
		}
		if got := perr == nil; got != (f[2] == "accept") {
			t.Errorf("%s: the verifier's parsers accepted=%v (%v), the corpus says %s", f[0], got, perr, f[2])
		}
		if perr != nil {
			refused++
		}
		n++
	}
	if n < 30 || refused < 10 || refused > n-10 {
		t.Fatalf("the corpus is too small or one-sided: %d cases, %d refused", n, refused)
	}
}
