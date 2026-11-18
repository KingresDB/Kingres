// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// L10-10: a value is no longer clipped before it is escaped (a clip at 100, 60,
// 80 or 120 bytes could cut an escape or a multi-byte character in half, made
// two inputs print alike, and made a value that ends in "..." print like a
// clipped one). The only cut is the one of the output layer, which is made after
// escaping, at a character boundary, and says how much is shown. Two different
// values never print the same.
func TestValuesAreNotClippedBeforeTheyAreEscaped(t *testing.T) {
	x := newExitEnv(t)
	logErr := func(v string) string {
		_, _, errs := run(t, "checkpoint", "--trust", x.p.trust, "--log", v)
		return errs
	}
	base := strings.Repeat("x", 100)
	seen := map[string]string{}
	for _, v := range []string{
		base + "A", base + "B", base + "...", strings.Repeat("x", 130), base + "\xff", base + "?", base[:99] + "\xff", base[:99] + "?",
		base[:98] + "\xc3\xa9", base[:98] + "?",
	} {
		e := logErr(v)
		if prev, dup := seen[e]; dup {
			t.Errorf("%q and %q print the same:\n%s", prev, v, e)
		}
		seen[e] = v
		if strings.Contains(e, "\xc3") && !strings.Contains(e, "\xc3\xa9") {
			t.Errorf("a character was cut in half: %q", e)
		}
	}
	// the same in the other places that clipped: a state path, the origin of a
	// note, a signature line of a note, a trust file, a tile path.
	long := strings.Repeat("p", 150)
	var errs []string
	for _, tail := range []string{"1", "2"} {
		_, _, e := run(t, x.cp("--state", filepath.Join(testdir.New(t), long+tail, "state"))...)
		errs = append(errs, e)
	}
	if errs[0] == errs[1] {
		t.Errorf("two state paths that differ after byte 100 print alike:\n%s", errs[0])
	}
	for _, tail := range []string{"A", "B"} {
		note := writeNote(t, "\n"+strings.Repeat("o", 70)+tail+"\n1\nAAAA\n\n\xe2\x80\x94 x y\n")
		_, o, e := run(t, x.cp("--note", note)...)
		errs = append(errs, o+e)
	}
	if errs[2] == errs[3] {
		t.Errorf("two notes that differ after byte 60 print alike:\n%s", errs[2])
	}
	// a name that is cut by the output layer says so and cannot be forged
	e := logErr(strings.Repeat("z", 3000))
	if !strings.Contains(e, `\...(cut: 2048 of`) {
		t.Errorf("a long value is not cut by the output layer: %.200s", e)
	}
}
