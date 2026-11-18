// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package cli

import (
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// L10-07: the memory of a proof does not depend on its size. A proof of 100,000
// headers (34 MB) and one of 400,000 (136 MB) are verified by a real process
// whose peak resident set stays within a few MiB (less than 16) of that of a small input; held
// in memory, the second would need some 150 MB for the headers and the file.
func TestPeakResidentSetOfAProofDoesNotGrowWithItsChain(t *testing.T) {
	if testing.Short() {
		t.Skip("writes files of some hundred MB")
	}
	_, _, base := childRun(t, "version")
	var peaks []int64
	for _, n := range []int{100_000, 400_000} {
		p, d, size := syntheticProof(t, testdir.New(t), n)
		code, out, peak := childRun(t, "proof", "--proof", p, "--digest", d)
		t.Logf("MEASURE proof of %d headers|%d bytes|%d KiB (a small input: %d KiB)", n, size, peak>>10, base>>10)
		if code != 0 || !strings.HasPrefix(out, "VERIFIED") {
			t.Fatalf("status %d for %d headers: %.300s", code, n, out)
		}
		if peak > base+(16<<20) {
			t.Errorf("%d headers (%d bytes): peak resident set %d MiB, a small input %d MiB", n, size, peak>>20, base>>20)
		}
		peaks = append(peaks, peak)
	}
	if peaks[1] > peaks[0]+(8<<20) {
		t.Errorf("four times the headers, %d MiB more: the memory grows with the proof", (peaks[1]-peaks[0])>>20)
	}
}
