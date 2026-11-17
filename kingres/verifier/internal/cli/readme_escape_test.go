// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// L10-11: a decoder written from the README alone must read the output back. The
// README used to write the escape of a backslash with one backslash, which no
// decoder can tell from the start of another escape. This test reads the rule
// from the text of the README and decodes with a function that follows only
// that text.
func decodeFromReadme(out string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(out); i++ {
		if out[i] != '\\' {
			b.WriteByte(out[i])
			continue
		}
		if i+1 >= len(out) {
			return "", false
		}
		switch out[i+1] {
		case '\\': // two backslashes are one backslash
			b.WriteByte('\\')
			i++
		case 'x':
			n, err := strconv.ParseUint(out[i+2:min(i+4, len(out))], 16, 8)
			if err != nil || i+4 > len(out) {
				return "", false
			}
			b.WriteByte(byte(n))
			i += 3
		case 'u', 'U':
			w := 4
			if out[i+1] == 'U' {
				w = 8
			}
			if i+2+w > len(out) {
				return "", false
			}
			n, err := strconv.ParseUint(out[i+2:i+2+w], 16, 32)
			if err != nil {
				return "", false
			}
			b.WriteRune(rune(n))
			i += 1 + w
		case '.': // the cut marker ends the text
			return b.String(), true
		default:
			return "", false
		}
	}
	return b.String(), true
}

func TestReadmeBackslashRuleDecodesTheOutput(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(raw)
	if !strings.Contains(readme, "two backslashes (`\\\\`) are one backslash") || !strings.Contains(readme, "written as **two** backslashes (`\\\\`)") {
		t.Fatal("the README does not say that a backslash is written as two backslashes")
	}
	if strings.Contains(readme, "a backslash is written `\\`,") || strings.Contains(readme, "(`\\` for a backslash") {
		t.Fatal("the README still writes the escape of a backslash with one backslash")
	}
	for _, h := range append([]string{`a\b`, `\\`, `\x41`, "\\" + "u0041", "\\" + "U0001F600", `\n`, "\\\n", `\.`, `\...(cut: 1 of 2 bytes shown)`, `end\`, "\xff\\x"}, hostileStrings...) {
		if len(h) > maxUntrustedText {
			continue // cut, and the cut is marked
		}
		got, ok := decodeFromReadme(untrusted(h))
		if !ok || got != h {
			t.Errorf("%q is printed as %q and read back as %q (%v)", h, untrusted(h), got, ok)
		}
	}
}
