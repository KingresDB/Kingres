// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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

// CleanText is peerText for the text of an answer that goes into a response of
// this server (a log message, an alert message): control characters become
// U+FFFD and the text is cut at max bytes. Every string that comes from another
// server and reaches a client passes through it.
func CleanText(s string, max int) string { return peerText(s, max) }
