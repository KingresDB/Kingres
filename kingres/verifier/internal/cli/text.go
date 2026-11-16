// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxUntrustedText bounds one value in the text output. A publisher can put a
// string of any length into a key or a file name, and a screen full of it hides
// the verdict.
const maxUntrustedText = 2048

// untrusted renders a string that may come from an input file, a file name or
// an error text that quotes them, so that it can be printed on a line of the
// text output. It is the only place that decides what is safe: every value the
// text output shows passes it (see cli_out.go), so no input can start a line, end
// one, or move the terminal.
//
//   - control characters (newline, carriage return, tab, escape, the C1 range
//     and DEL) are written as \n, \r, \t, \xNN or \uNNNN;
//   - bytes that are not UTF-8 are written as \xNN;
//   - characters that change how text is drawn or split (the bidirectional
//     marks and overrides and isolates, the line and paragraph separators, the
//     byte order mark, zero width and other format characters, and anything
//     that is not printable) are written as \uNNNN;
//   - a value longer than maxUntrustedText bytes is cut and says how much was cut.
//
// A backslash is left alone: file names on Windows are full of them, and the
// escapes above cannot start a line whatever the text around them.
func untrusted(s string) string {
	extra := 0
	if len(s) > maxUntrustedText {
		cut := maxUntrustedText
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		extra = len(s) - cut
		s = s[:cut]
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r == ' ' || (r < 0x7f):
			b.WriteRune(r)
		case unicode.IsPrint(r) && !unicode.Is(unicode.Cf, r) && !unicode.Is(unicode.Zl, r) && !unicode.Is(unicode.Zp, r):
			b.WriteRune(r)
		case r > 0xffff:
			fmt.Fprintf(&b, `\U%08x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
		i += n
	}
	if extra > 0 {
		fmt.Fprintf(&b, "... (%d more bytes not shown)", extra)
	}
	return b.String()
}

// untrustedAll renders a list of strings.
func untrustedAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = untrusted(s)
	}
	return out
}
