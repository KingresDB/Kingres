// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sqlshape

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
)

// Limits of the lexer (design "Limits and timeouts", archive section).
const (
	// MaxStatementBytes is the largest statement that is parsed. A statement of exactly this size is accepted, one byte more is refused.
	MaxStatementBytes = 1 << 20
	// MaxStatements is the largest number of statements (and meta lines) of one file.
	MaxStatements = 200000
	// maxTokens bounds the memory of one statement: a statement of 1 MiB of one-byte tokens would otherwise hold millions of tokens.
	maxTokens = 262144
	// maxMetaLine is the longest line of a \restrict or \unrestrict command that is looked at.
	maxMetaLine = 1024
	// maxTag is the longest dollar-quote tag.
	maxTag = 512
)

// Errors of the lexer. A file that ends inside a statement, a string or a comment is not a file that was written whole.
var (
	ErrUnterminated     = errors.New("sqlshape: the file ends inside a statement, a string or a comment")
	ErrTooManyStatement = errors.New("sqlshape: too many statements")
)

type tokKind uint8

const (
	tIdent  tokKind = iota // an unquoted identifier or keyword, text in lower case
	tQIdent                // a quoted identifier, text unescaped
	tStr                   // a string literal (quotes removed)
	tNum                   // a number
	tPunct                 // one punctuation or operator character
	tParam                 // $1
	tDollar                // a dollar-quoted string (body)
)

type token struct {
	k tokKind
	s string
}

// rawStmt is what the lexer reads for one statement or one meta line.
type rawStmt struct {
	toks   []token
	length int    // bytes from the first token to the terminator, terminator excluded
	hash   string // 16 hex characters of the SHA-256 of those bytes (of the first MaxStatementBytes+1 bytes when the statement is longer)
	meta   string // for a line that starts with a backslash: the line without the line end
	isMeta bool
	tooBig bool   // the statement is longer than MaxStatementBytes; toks is empty
	bad    string // a fixed reason: the statement holds something that is refused at the lexical level
	nTok   bool   // too many tokens
}

type lexer struct {
	br      *bufio.Reader
	started bool
	raw     []byte
	n       int
	over    bool
	toks    []token
	bad     string
	err     error
}

func newLexer(r io.Reader) *lexer {
	return &lexer{br: bufio.NewReaderSize(r, 64<<10)}
}

// pk returns the byte at offset i ahead, or -1 at the end of the input.
func (l *lexer) pk(i int) int {
	if l.err != nil {
		return -1
	}
	b, err := l.br.Peek(i + 1)
	if len(b) <= i {
		if err != nil && err != io.EOF && !errors.Is(err, bufio.ErrBufferFull) {
			l.err = err
		}
		return -1
	}
	return int(b[i])
}

// adv consumes k bytes that pk saw. Inside a statement they are counted and hashed.
func (l *lexer) adv(k int) {
	if k <= 0 {
		return
	}
	b, _ := l.br.Peek(k)
	if l.started {
		l.n += len(b)
		if !l.over {
			if l.n > MaxStatementBytes {
				l.over = true
				l.toks = nil
				// keep the bytes that fit, so that the hash covers the first MaxStatementBytes+1 bytes
				room := MaxStatementBytes + 1 - len(l.raw)
				if room > 0 && room <= len(b) {
					l.raw = append(l.raw, b[:room]...)
				}
			} else {
				l.raw = append(l.raw, b...)
			}
		}
	}
	_, _ = l.br.Discard(k)
}

func (l *lexer) add(t token) {
	if l.over || l.started == false {
		return
	}
	if len(l.toks) >= maxTokens {
		l.over = true
		l.toks = nil
		l.bad = "the statement has too many tokens"
		return
	}
	l.toks = append(l.toks, t)
}

func isSpace(b int) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v'
}
func isIdentStart(b int) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}
func isIdentPart(b int) bool { return isIdentStart(b) || (b >= '0' && b <= '9') || b == '$' }
func isDigit(b int) bool     { return b >= '0' && b <= '9' }

// skipComment consumes a comment at the current position and says whether there was one. An unterminated block comment sets ErrUnterminated.
func (l *lexer) skipComment() bool {
	switch {
	case l.pk(0) == '-' && l.pk(1) == '-':
		for {
			b := l.pk(0)
			if b == -1 {
				return true
			}
			l.adv(1)
			if b == '\n' {
				return true
			}
		}
	case l.pk(0) == '/' && l.pk(1) == '*':
		l.adv(2)
		depth := 1
		for depth > 0 {
			switch {
			case l.pk(0) == -1:
				if l.err == nil {
					l.err = ErrUnterminated
				}
				return true
			case l.pk(0) == '/' && l.pk(1) == '*':
				l.adv(2)
				depth++
			case l.pk(0) == '*' && l.pk(1) == '/':
				l.adv(2)
				depth--
			default:
				l.adv(1)
			}
		}
		return true
	}
	return false
}

// next reads the next statement. It returns (nil, nil) at a clean end of the input.
func (l *lexer) next() (*rawStmt, error) {
	l.started, l.over, l.raw, l.n, l.toks, l.bad = false, false, l.raw[:0], 0, nil, ""
	// Before the first token: space and comments are not part of the statement.
	for l.err == nil {
		b := l.pk(0)
		if b == -1 {
			return nil, l.err
		}
		if isSpace(b) {
			l.adv(1)
			continue
		}
		if l.skipComment() {
			continue
		}
		break
	}
	if l.err != nil {
		return nil, l.err
	}
	if l.pk(0) == '\\' {
		return l.metaLine()
	}
	l.started = true
	for {
		if l.err != nil {
			return nil, l.err
		}
		b := l.pk(0)
		switch {
		case b == -1:
			return nil, ErrUnterminated
		case isSpace(b):
			l.adv(1)
		case b == '-' && l.pk(1) == '-', b == '/' && l.pk(1) == '*':
			l.skipComment()
		case b == ';':
			// the terminator is not part of the statement
			_, _ = l.br.Discard(1)
			return l.finish(), nil
		case b == '\\':
			if l.bad == "" {
				l.bad = "a backslash command inside a statement"
			}
			for l.pk(0) != -1 && l.pk(0) != '\n' {
				l.adv(1)
			}
		case b == '\'':
			l.add(token{tStr, l.quoted('\'', false)})
		case b == '"':
			l.add(token{tQIdent, l.quoted('"', false)})
		case b == '$':
			l.dollar()
		case isDigit(b) || (b == '.' && isDigit(l.pk(1))):
			l.add(token{tNum, l.number()})
		case isIdentStart(b):
			l.word()
		case b == '(' || b == ')' || b == '[' || b == ']' || b == ',' || b == '.':
			l.add(token{tPunct, string(rune(b))})
			l.adv(1)
		default:
			l.add(token{tPunct, string(rune(b))})
			l.adv(1)
		}
	}
}

func (l *lexer) finish() *rawStmt {
	sum := sha256.Sum256(l.raw)
	r := &rawStmt{toks: l.toks, length: l.n, hash: hex.EncodeToString(sum[:8]), bad: l.bad}
	if l.over {
		r.toks = nil
		if l.n > MaxStatementBytes {
			r.tooBig = true
		} else {
			r.nTok = true
		}
	}
	return r
}

// metaLine reads a line that starts with a backslash. Only \restrict and \unrestrict with one token are accepted.
func (l *lexer) metaLine() (*rawStmt, error) {
	var sb strings.Builder
	long := false
	for {
		b := l.pk(0)
		if b == -1 || b == '\n' {
			break
		}
		if sb.Len() < maxMetaLine {
			sb.WriteByte(byte(b))
		} else {
			long = true
		}
		_, _ = l.br.Discard(1)
	}
	if l.err != nil {
		return nil, l.err
	}
	if l.pk(0) == '\n' {
		_, _ = l.br.Discard(1)
	}
	line := strings.TrimRight(sb.String(), "\r")
	sum := sha256.Sum256([]byte(line))
	r := &rawStmt{isMeta: true, meta: line, length: len(line), hash: hex.EncodeToString(sum[:8])}
	if long {
		r.bad = "the line is too long"
	}
	return r, nil
}

// quoted reads a quoted string or identifier whose opening quote is at the current position. Doubled quotes are one quote; with esc, a backslash
// takes the next byte literally (an E” string).
func (l *lexer) quoted(q byte, esc bool) string {
	l.adv(1)
	var sb strings.Builder
	for {
		b := l.pk(0)
		switch {
		case b == -1:
			if l.err == nil {
				l.err = ErrUnterminated
			}
			return sb.String()
		case esc && b == '\\':
			c := l.pk(1)
			if c == -1 {
				l.adv(1)
				continue
			}
			sb.WriteByte(byte(b))
			sb.WriteByte(byte(c))
			l.adv(2)
		case b == int(q):
			if l.pk(1) == int(q) {
				sb.WriteByte(q)
				l.adv(2)
				continue
			}
			l.adv(1)
			return sb.String()
		default:
			if sb.Len() <= MaxStatementBytes {
				sb.WriteByte(byte(b))
			}
			l.adv(1)
		}
	}
}

// word reads an identifier or keyword, and the string forms that an identifier-like prefix introduces: E”, U&”, U&"", B”, X”, N”.
func (l *lexer) word() {
	i := 0
	for isIdentPart(l.pk(i)) {
		i++
	}
	b, _ := l.br.Peek(i)
	text := string(b)
	lower := strings.ToLower(text)
	next := l.pk(i)
	switch {
	case next == '\'' && (lower == "e"):
		l.adv(i)
		l.add(token{tStr, l.quoted('\'', true)})
		return
	case next == '\'' && (lower == "b" || lower == "x" || lower == "n"):
		l.adv(i)
		l.add(token{tStr, l.quoted('\'', false)})
		return
	case lower == "u" && next == '&' && (l.pk(i+1) == '\'' || l.pk(i+1) == '"'):
		q := l.pk(i + 1)
		l.adv(i + 1)
		if q == '\'' {
			l.add(token{tStr, l.quoted('\'', false)})
		} else {
			l.add(token{tQIdent, l.quoted('"', false)})
		}
		return
	}
	l.adv(i)
	l.add(token{tIdent, lower})
}

func (l *lexer) number() string {
	i := 0
	for {
		c := l.pk(i)
		if isDigit(c) || c == '.' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			i++
			if (c == 'e' || c == 'E') && (l.pk(i) == '+' || l.pk(i) == '-') {
				i++
			}
			continue
		}
		break
	}
	b, _ := l.br.Peek(i)
	s := string(b)
	l.adv(i)
	return s
}

// dollar handles a $ that starts a parameter, a dollar-quoted string, or nothing special.
func (l *lexer) dollar() {
	if isDigit(l.pk(1)) {
		i := 1
		for isDigit(l.pk(i)) {
			i++
		}
		b, _ := l.br.Peek(i)
		s := string(b)
		l.adv(i)
		l.add(token{tParam, s})
		return
	}
	// $tag$ or $$
	i := 1
	if isIdentStart(l.pk(1)) && l.pk(1) < 0x80 {
		for isIdentPart(l.pk(i)) && l.pk(i) != '$' && i <= maxTag {
			i++
		}
	}
	if l.pk(i) != '$' || i > maxTag {
		l.add(token{tPunct, "$"})
		l.adv(1)
		return
	}
	tb, _ := l.br.Peek(i + 1)
	tag := append([]byte(nil), tb...)
	l.adv(i + 1)
	var body strings.Builder
	for {
		c := l.pk(0)
		if c == -1 {
			if l.err == nil {
				l.err = ErrUnterminated
			}
			return
		}
		if c == '$' && l.matchTag(tag) {
			l.adv(len(tag))
			l.add(token{tDollar, body.String()})
			return
		}
		if body.Len() <= MaxStatementBytes {
			body.WriteByte(byte(c))
		}
		l.adv(1)
	}
}

func (l *lexer) matchTag(tag []byte) bool {
	for j, c := range tag {
		if l.pk(j) != int(c) {
			return false
		}
	}
	return true
}
