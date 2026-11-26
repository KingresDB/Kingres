// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package anchor

import (
	"bytes"
	"os"
	"testing"
)

// verifierCorpus is where the byte-identical copy of the payload corpus lives in
// the verifier module (A4-09, correction 97).
const verifierCorpus = "../../../verifier/internal/checkpoint/testdata/payload_corpus.txt"

// TestCorpusCopiesAreIdentical fails when the corpus of this package and its copy
// in the verifier module differ: the verifier column is held to the real parsers
// by a test in that module, so the two files must be one. When the sibling module
// is not beside this one (a copy of this module alone) the test skips, and says so.
func TestCorpusCopiesAreIdentical(t *testing.T) {
	other, err := os.ReadFile(verifierCorpus)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("the verifier module is not beside this one (%s): the copies of the corpus were not compared", verifierCorpus)
		}
		t.Fatal(err)
	}
	if !bytes.Equal(other, []byte(payloadCorpus)) {
		t.Fatalf("testdata/payload_corpus.txt differs from %s: change both together", verifierCorpus)
	}
}
