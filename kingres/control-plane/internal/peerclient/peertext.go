// SPDX-License-Identifier: PostgreSQL

package peerclient

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxPeerText is the longest message kept from the error answer of a peer.
const MaxPeerText = 500

// peerText makes the text a peer answered with safe to quote into a problem,
// a task error or a log line: control characters, line separators and invalid
// UTF-8 become U+FFFD, and the text is cut at max bytes (plus "...").
func peerText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || r == utf8.RuneError {
			return utf8.RuneError
		}
		return r
	}, s)
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "..."
}
