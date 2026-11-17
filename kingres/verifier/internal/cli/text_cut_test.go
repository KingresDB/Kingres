// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"os"
	"strings"
	"testing"
)

// L12-07: the README said that the cut of a long value is made after escaping; it
// is made before, on the raw value, and the escaped text is longer than the cut.
// The README now says what the code does, and this test holds both to it.
func TestTheCutIsMadeBeforeTheEscape(t *testing.T) {
	raw := strings.Repeat("\x01", 3000)
	got := untrusted(raw)
	shown := strings.Repeat(`\x01`, maxUntrustedText)
	want := shown + `\...(cut: 2048 of 3000 bytes shown)`
	if got != want {
		t.Fatalf("a value of 3,000 control characters prints as %d bytes, not the 2,048 cut and its escapes (%d)", len(got), len(want))
	}
	if n := len(shown); n != 4*maxUntrustedText {
		t.Fatalf("the escaped part is %d bytes, at most four times the cut", n)
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	if !strings.Contains(text, "**before** escaping") || strings.Contains(text, "it is made after\n  escaping") {
		t.Error("the README does not say that the cut is made before escaping")
	}
}
