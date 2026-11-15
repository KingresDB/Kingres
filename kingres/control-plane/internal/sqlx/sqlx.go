// SPDX-License-Identifier: PostgreSQL

// Package sqlx is the only way kingresd builds SQL that contains names or
// text taken from a request. It offers two things:
//
//   - Ident: a validated, quoted PostgreSQL identifier. The only constructor
//     is NewIdent, so a value of this type is always safe to splice into SQL.
//   - Builder: a statement builder that accepts nothing but identifiers
//     (Ident), keywords from closed enumerations (KeywordSet), fixed
//     punctuation text, bind parameters (Arg) and, for utility statements
//     that PostgreSQL does not let take parameters, quoted literals (Lit).
//     Build fails when anything else could end up in the statement, so no
//     input can produce a second statement or an unquoted identifier.
package sqlx

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxIdentBytes is PostgreSQL's NAMEDATALEN-1.
const MaxIdentBytes = 63

// ErrInvalid is wrapped by every validation error of the package.
var ErrInvalid = errors.New("invalid SQL input")

// Ident is a validated and quoted identifier. The zero value is invalid.
type Ident struct {
	name   string
	quoted string
}

// NewIdent validates name and quotes it. A name must be valid UTF-8, 1 to 63
// bytes, and free of control characters (NUL, C0, DEL, C1) and of the Unicode
// line and paragraph separators. Every other character, including quotes,
// is allowed and escaped.
func NewIdent(name string) (Ident, error) {
	if name == "" || len(name) > MaxIdentBytes {
		return Ident{}, fmt.Errorf("%w: identifier length must be 1 to %d bytes", ErrInvalid, MaxIdentBytes)
	}
	if !utf8.ValidString(name) {
		return Ident{}, fmt.Errorf("%w: identifier is not valid UTF-8", ErrInvalid)
	}
	for _, r := range name {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			return Ident{}, fmt.Errorf("%w: identifier contains a control character", ErrInvalid)
		}
	}
	return Ident{name: name, quoted: `"` + strings.ReplaceAll(name, `"`, `""`) + `"`}, nil
}

// MustIdent is NewIdent for names written in the program; it panics on an
// invalid name.
func MustIdent(name string) Ident {
	i, err := NewIdent(name)
	if err != nil {
		panic(err)
	}
	return i
}

// Name returns the unquoted name.
func (i Ident) Name() string { return i.name }

// String returns the quoted form.
func (i Ident) String() string { return i.quoted }

// Valid reports whether the identifier was made by NewIdent.
func (i Ident) Valid() bool { return i.quoted != "" }

// QualifiedIdent returns schema.name quoted.
func QualifiedIdent(schema, name string) (Ident, error) {
	s, err := NewIdent(schema)
	if err != nil {
		return Ident{}, err
	}
	n, err := NewIdent(name)
	if err != nil {
		return Ident{}, err
	}
	return Ident{name: schema + "." + name, quoted: s.quoted + "." + n.quoted}, nil
}

// QuoteLiteral quotes s as a PostgreSQL string literal the way libpq's
// PQescapeLiteral does: quotes are doubled and, if s contains a backslash,
// the E” form with doubled backslashes is used, so the result is correct
// whatever standard_conforming_strings is. NUL and invalid UTF-8 are errors.
func QuoteLiteral(s string) (string, error) {
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return "", fmt.Errorf("%w: literal is not valid text", ErrInvalid)
	}
	q := strings.ReplaceAll(s, "'", "''")
	if strings.Contains(q, `\`) {
		return ` E'` + strings.ReplaceAll(q, `\`, `\\`) + `'`, nil
	}
	return `'` + q + `'`, nil
}

// Keyword is an SQL keyword phrase chosen from a KeywordSet. Build rejects
// any keyword that is not upper-case words separated by single spaces, so a
// converted arbitrary string cannot smuggle anything in.
type Keyword string

// KeywordSet is a closed enumeration of keywords.
type KeywordSet struct{ set map[string]Keyword }

// NewKeywordSet builds a set; it panics on a malformed keyword.
func NewKeywordSet(words ...string) KeywordSet {
	ks := KeywordSet{set: map[string]Keyword{}}
	for _, w := range words {
		if !validKeyword(w) {
			panic("sqlx: malformed keyword " + strconv.Quote(w))
		}
		ks.set[w] = Keyword(w)
	}
	return ks
}

// Parse returns the keyword whose text is exactly s.
func (ks KeywordSet) Parse(s string) (Keyword, error) {
	if k, ok := ks.set[s]; ok {
		return k, nil
	}
	return "", fmt.Errorf("%w: %q is not one of the allowed values", ErrInvalid, truncate(s))
}

// Words lists the members (unordered).
func (ks KeywordSet) Words() []string {
	out := make([]string, 0, len(ks.set))
	for w := range ks.set {
		out = append(out, w)
	}
	return out
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func validKeyword(w string) bool {
	if w == "" || len(w) > 40 || w[0] == ' ' || w[len(w)-1] == ' ' || strings.Contains(w, "  ") {
		return false
	}
	for i := 0; i < len(w); i++ {
		c := w[i]
		if (c < 'A' || c > 'Z') && c != ' ' && c != '_' {
			return false
		}
	}
	return true
}

// Fixed is SQL text written in the program: punctuation and the shape of the
// statement. Build allows only letters, digits, spaces and the characters
// `_ , ( ) . = * < > + -`, and refuses `--`, so it cannot carry a quote, a
// parameter marker, a semicolon, a comment or a dollar-quoted string.
type Fixed string

func validFixed(f Fixed) bool {
	s := string(f)
	if strings.Contains(s, "--") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte(" _,().=*<>+-", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// Stmt is a finished statement.
type Stmt struct {
	// SQL has $n placeholders for Args.
	SQL string
	// Args are the bind parameters.
	Args []any
}

// Builder assembles one statement. Methods return the builder; the first
// error is kept and reported by Build.
type Builder struct {
	sb   strings.Builder
	args []any
	err  error
}

func (b *Builder) sep() {
	if b.sb.Len() > 0 {
		b.sb.WriteByte(' ')
	}
}

func (b *Builder) fail(err error) {
	if b.err == nil {
		b.err = err
	}
}

// Kw appends a keyword.
func (b *Builder) Kw(k Keyword) *Builder {
	if !validKeyword(string(k)) {
		b.fail(fmt.Errorf("%w: malformed keyword", ErrInvalid))
		return b
	}
	b.sep()
	b.sb.WriteString(string(k))
	return b
}

// Text appends fixed program text (see Fixed).
func (b *Builder) Text(f Fixed) *Builder {
	if !validFixed(f) {
		b.fail(fmt.Errorf("%w: fixed text %q has forbidden characters", ErrInvalid, truncate(string(f))))
		return b
	}
	b.sep()
	b.sb.WriteString(string(f))
	return b
}

// Id appends a quoted identifier.
func (b *Builder) ID(i Ident) *Builder {
	if !i.Valid() {
		b.fail(fmt.Errorf("%w: zero identifier", ErrInvalid))
		return b
	}
	b.sep()
	b.sb.WriteString(i.quoted)
	return b
}

// Arg appends a bind parameter placeholder and records v.
func (b *Builder) Arg(v any) *Builder {
	b.args = append(b.args, v)
	b.sep()
	b.sb.WriteString("$" + strconv.Itoa(len(b.args)))
	return b
}

// Lit appends a quoted string literal. Use it only where PostgreSQL does not
// accept a parameter (utility statements such as CREATE ROLE ... PASSWORD
// or CREATE DATABASE ... ENCODING); everywhere else use Arg.
func (b *Builder) Lit(s string) *Builder {
	q, err := QuoteLiteral(s)
	if err != nil {
		b.fail(err)
		return b
	}
	b.sep()
	b.sb.WriteString(q)
	return b
}

// Build returns the statement or the first error.
func (b *Builder) Build() (Stmt, error) {
	if b.err != nil {
		return Stmt{}, b.err
	}
	if b.sb.Len() == 0 {
		return Stmt{}, fmt.Errorf("%w: empty statement", ErrInvalid)
	}
	return Stmt{SQL: b.sb.String(), Args: b.args}, nil
}
