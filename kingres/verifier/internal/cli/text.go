// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxUntrustedText bounds one value in the text output, in bytes of the value.
// A publisher can put a string of any length into a key or a file name, and a
// screen full of it hides the verdict.
const maxUntrustedText = 2048

// cutMarker starts the note that a value was cut. It begins with a backslash
// and a full stop, which untrusted never writes for any input (a backslash of
// the input is written as two backslashes, and every other escape is \x, \u or
// \U), so no input can print it, and a reader can tell a cut value from one that
// ends with the same words.
const cutMarker = `\...`

// mustEscape says whether a character is written as an escape instead of as
// itself: the controls (the C0 range, DEL and the C1 range), the characters
// that change how text is drawn or split (bidirectional marks, overrides and
// isolates, line and paragraph separators, the byte order mark, zero width and
// other format characters), and everything that is not printable. Space and the
// printable ASCII characters other than the backslash stand for themselves. The
// JSON output escapes exactly these too (printJSON).
func mustEscape(r rune) bool {
	switch {
	case r == ' ':
		return false
	case r < 0x7f:
		return r < 0x20
	case r == 0x7f:
		return true
	}
	return !unicode.IsPrint(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

// untrusted renders a string that may come from an input file, a file name or
// an error text that quotes them, so that it can be printed on a line of the
// text output. It is the only place that decides what is safe: every value the
// text output shows passes it (see cli_out.go), so no input can start a line, end
// one, or move the terminal. The rendering is one to one: two different inputs
// never print the same, and an input that was not cut can be read back exactly.
//
//   - a backslash is written as \\ (so that no input can write an escape or the
//     cut marker itself);
//   - a control character, or DEL, is written as \xNN (a newline is \x0a, an
//     escape \x1b), and a byte that is not part of valid UTF-8 as \xNN too, with
//     NN of 80 or more, which a valid character never gives;
//   - any other character that mustEscape names is written as \uNNNN, or
//     \UNNNNNNNN above U+FFFF, with lower case hex digits;
//   - a value longer than maxUntrustedText bytes is cut at a character boundary,
//     and the cutMarker follows with how much was shown and the original length:
//     \...(cut: N of M bytes shown).
//
// Everything else is written as it is. Reading the output back: take each
// backslash followed by a backslash as one backslash, \xNN as the byte NN,
// \uNNNN and \UNNNNNNNN as the character, and stop at the cut marker.
func untrusted(s string) string {
	total := len(s)
	cut := total
	if total > maxUntrustedText {
		cut = maxUntrustedText
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\\':
			b.WriteString(`\\`)
		case !mustEscape(r):
			b.WriteRune(r)
		case r < 0x100 && (r < 0x20 || r == 0x7f):
			fmt.Fprintf(&b, `\x%02x`, r)
		case r > 0xffff:
			fmt.Fprintf(&b, `\U%08x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
		i += n
	}
	if cut < total {
		fmt.Fprintf(&b, "%s(cut: %d of %d bytes shown)", cutMarker, cut, total)
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
