// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package dumpnorm

import (
	"strings"
)

type wordKind uint8

const (
	kWord   wordKind = iota // an unquoted identifier, keyword or number
	kQIdent                 // a quoted identifier (text unescaped)
	kString                 // a string literal, with its quotes
	kPunct                  // one punctuation character
)

type word struct {
	kind       wordKind
	text       string
	start, end int // offsets in the statement text
}

func (w word) lower() string {
	if w.kind == kWord {
		return strings.ToLower(w.text)
	}
	return ""
}

// ident is the name that a word stands for: an unquoted word folds to lower case, a quoted one is as written.
func (w word) ident() string {
	switch w.kind {
	case kWord:
		return strings.ToLower(w.text)
	case kQIdent:
		return w.text
	}
	return ""
}

// stmt is one statement of the dump: its text without the terminator, its tokens, and for a backslash command the line.
type stmt struct {
	text  string
	words []word
	meta  string
}

// qnameParts reads name(.name)* from the start of w.
func qnameParts(w []word) []string {
	var parts []string
	i := 0
	for i < len(w) {
		if w[i].kind != kWord && w[i].kind != kQIdent {
			break
		}
		parts = append(parts, w[i].ident())
		if i+1 < len(w) && w[i+1].kind == kPunct && w[i+1].text == "." {
			i += 2
			continue
		}
		break
	}
	return parts
}

// qname is the unquoted "schema.name" that starts w (a name with more parts keeps the first two).
func qname(w []word) string {
	p := qnameParts(w)
	switch len(p) {
	case 0:
		return ""
	case 1:
		return p[0]
	}
	return p[0] + "." + p[1]
}

// split cuts the dump into statements. Comments and blank lines are dropped; a line that starts a statement with a backslash is a command.
func split(src string) ([]stmt, error) {
	var out []stmt
	i, n := 0, len(src)
	startOfLine := true
	for i < n {
		// Skip blanks and comments before a statement.
		c := src[i]
		switch {
		case c == '\n':
			startOfLine = true
			i++
			continue
		case c == ' ' || c == '\t' || c == '\r':
			i++
			continue
		case c == '-' && i+1 < n && src[i+1] == '-':
			for i < n && src[i] != 10 && src[i] != 13 {
				i++
			}
			continue
		case c == '/' && i+1 < n && src[i+1] == '*':
			j, ok := skipBlock(src, i)
			if !ok {
				return nil, ErrSplit
			}
			i = j
			continue
		case c == '\\' && startOfLine:
			j := i
			for j < n && src[j] != '\n' {
				j++
			}
			out = append(out, stmt{meta: strings.TrimSpace(src[i:j]), text: strings.TrimSpace(src[i:j])})
			i = j
			continue
		}
		startOfLine = false
		// A statement: scan to the terminator outside quotes.
		begin := i
		var words []word
		done := false
		for i < n && !done {
			c := src[i]
			switch {
			case c == ';':
				done = true
			case c == '\'' || ((c == 'E' || c == 'e') && i+1 < n && src[i+1] == '\''):
				s := i
				esc := c != '\''
				if esc {
					i++
				}
				j, ok := skipString(src, i, esc)
				if !ok {
					return nil, ErrSplit
				}
				words = append(words, word{kind: kString, text: src[s:j], start: s - begin, end: j - begin})
				i = j
			case c == '"':
				j, txt, ok := skipQuoted(src, i)
				if !ok {
					return nil, ErrSplit
				}
				words = append(words, word{kind: kQIdent, text: txt, start: i - begin, end: j - begin})
				i = j
			case c == '$':
				if j, ok := skipDollar(src, i); ok {
					words = append(words, word{kind: kString, text: src[i:j], start: i - begin, end: j - begin})
					i = j
				} else {
					words = append(words, word{kind: kPunct, text: "$", start: i - begin, end: i + 1 - begin})
					i++
				}
			case c == '-' && i+1 < n && src[i+1] == '-':
				for i < n && src[i] != 10 && src[i] != 13 {
					i++
				}
			case c == '/' && i+1 < n && src[i+1] == '*':
				j, ok := skipBlock(src, i)
				if !ok {
					return nil, ErrSplit
				}
				i = j
			case c == ' ' || c == '\t' || c == '\n' || c == '\r':
				i++
			case isWordByte(c):
				s := i
				for i < n && isWordByte(src[i]) {
					i++
				}
				words = append(words, word{kind: kWord, text: src[s:i], start: s - begin, end: i - begin})
			default:
				words = append(words, word{kind: kPunct, text: string(c), start: i - begin, end: i + 1 - begin})
				i++
			}
		}
		if !done {
			return nil, ErrSplit
		}
		text := src[begin:i]
		i++ // the terminator
		out = append(out, stmt{text: text, words: words})
	}
	return out, nil
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c >= 0x80
}

func skipBlock(s string, i int) (int, bool) {
	depth := 0
	for i < len(s) {
		switch {
		case s[i] == '/' && i+1 < len(s) && s[i+1] == '*':
			depth++
			i += 2
		case s[i] == '*' && i+1 < len(s) && s[i+1] == '/':
			depth--
			i += 2
			if depth == 0 {
				return i, true
			}
		default:
			i++
		}
	}
	return i, false
}

func skipString(s string, i int, esc bool) (int, bool) {
	i++ // the opening quote
	for i < len(s) {
		switch {
		case esc && s[i] == '\\':
			i += 2
		case s[i] == '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				i += 2
				continue
			}
			return i + 1, true
		default:
			i++
		}
	}
	return i, false
}

func skipQuoted(s string, i int) (int, string, bool) {
	var b strings.Builder
	i++
	for i < len(s) {
		if s[i] == '"' {
			if i+1 < len(s) && s[i+1] == '"' {
				b.WriteByte('"')
				i += 2
				continue
			}
			return i + 1, b.String(), true
		}
		b.WriteByte(s[i])
		i++
	}
	return i, "", false
}

// skipDollar reads a dollar-quoted string that starts at i; it reports false when the text at i is not a dollar quote (a parameter $1, for example).
func skipDollar(s string, i int) (int, bool) {
	j := i + 1
	for j < len(s) && (s[j] == '_' || (s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z') || (s[j] >= '0' && s[j] <= '9') || s[j] >= 0x80) {
		j++
	}
	if j >= len(s) || s[j] != '$' || (j > i+1 && s[i+1] >= '0' && s[i+1] <= '9') {
		return 0, false
	}
	tag := s[i : j+1]
	k := strings.Index(s[j+1:], tag)
	if k < 0 {
		return len(s), false
	}
	return j + 1 + k + len(tag), true
}

// CountStatements is the number of statements (backslash commands excluded) that the splitter finds in a script; the corpus of package sqlshape is checked
// against it, and against PostgreSQL.
func CountStatements(src string) (int, error) {
	st, err := split(src)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range st {
		if s.meta == "" {
			n++
		}
	}
	return n, nil
}
