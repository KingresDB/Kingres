// SPDX-License-Identifier: AGPL-3.0-only

package transfer

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
)

// Delimiters and encodings of the contract (CsvDelimiter, CsvEncoding).
var (
	delimiters = []string{",", ";", "\t", "|", ":"}
	encodings  = []string{"UTF8", "LATIN1", "LATIN2", "LATIN9", "WIN1250", "WIN1251", "WIN1252", "WIN1256", "ISO_8859_6", "SJIS", "EUC_JP", "GBK", "BIG5", "KOI8R"}
	// clientOnly are the encodings a database cannot use: a byte-oriented scanner cannot tell a trail byte from a quote in them.
	clientOnly = map[string]bool{"SJIS": true, "BIG5": true, "GBK": true}
)

// MaxNullBytes is the longest text of a null.
const MaxNullBytes = 32

// MaxColumns is the most columns of a table.
const MaxColumns = 1600

// CSV is the format of a file as an export writes it and an import reads it. The control plane builds a COPY statement from it only after
// Check, and quotes every value of it as a literal in one function.
type CSV struct {
	Delimiter  string
	NullString string
	Quote      string
	Encoding   string
}

// DefaultCSV is the format of a request that names nothing.
func DefaultCSV() CSV { return CSV{Delimiter: ",", NullString: "", Quote: `"`, Encoding: "UTF8"} }

// Fill replaces the members a request left out by their defaults.
func (c CSV) Fill() CSV {
	d := DefaultCSV()
	if c.Delimiter == "" {
		c.Delimiter = d.Delimiter
	}
	if c.Quote == "" {
		c.Quote = d.Quote
	}
	if c.Encoding == "" {
		c.Encoding = d.Encoding
	}
	return c
}

func isOneOf(v string, set []string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// Check validates the format and returns the problems found, as the fields of the request they belong to (prefix is the pointer of the
// request member, for example "/delimiter" is prefix+"delimiter").
func (c CSV) Check() []problem.FieldError {
	var errs []problem.FieldError
	bad := func(field, msg, code string) {
		errs = append(errs, problem.FieldError{Field: "/" + field, Message: msg, Code: code})
	}
	if !isOneOf(c.Delimiter, delimiters) {
		bad("delimiter", "must be one of , ; tab | :", "unknown_value")
	}
	if !isOneOf(c.Encoding, encodings) {
		bad("encoding", "is not one of the encodings of the contract", "unknown_value")
	}
	if len(c.Quote) != 1 || c.Quote[0] < 0x20 || c.Quote[0] > 0x7e {
		bad("quote", "must be one printable ASCII character", "pattern")
	} else {
		if c.Quote == c.Delimiter {
			bad("quote", "must differ from the delimiter", "conflict")
		}
		// With the encodings of which a byte of a character can be a quote, only a byte below 0x40 can be told from a trail byte.
		if clientOnly[c.Encoding] && c.Quote[0] >= 0x40 {
			bad("quote", "with the encodings SJIS, BIG5 and GBK the quote must be an ASCII character below @", "conflict")
		}
		// The quote of PostgreSQL's CSV reader is never the backslash of its end marker or a letter of a line break.
		if c.Quote == `\` || c.Quote == "." {
			bad("quote", "must not be a backslash or a dot", "conflict")
		}
	}
	switch {
	case len(c.NullString) > MaxNullBytes:
		bad("nullString", "must be at most 32 bytes", "max_length")
	case !utf8.ValidString(c.NullString) || strings.ContainsRune(c.NullString, 0):
		bad("nullString", "must be valid text without a NUL", "pattern")
	case strings.ContainsAny(c.NullString, "\r\n"):
		bad("nullString", "must not contain a line break", "pattern")
	case c.Quote != "" && strings.Contains(c.NullString, c.Quote):
		bad("nullString", "must not contain the quote", "conflict")
	case c.Delimiter != "" && strings.Contains(c.NullString, c.Delimiter):
		bad("nullString", "must not contain the delimiter", "conflict")
	}
	return errs
}

// QuoteByte is the quote as a byte (after Check).
func (c CSV) QuoteByte() byte { return c.Quote[0] }

// literal quotes a value of an option as a PostgreSQL string literal; the value passed Check.
func literal(s string) (string, error) {
	q, err := sqlx.QuoteLiteral(s)
	return strings.TrimSpace(q), err
}

// options writes the part of the WITH list that the format decides: the delimiter, the null text, the quote, the escape (the quote itself,
// so that a quote is doubled) and the encoding.
func (c CSV) options() (string, error) {
	var out []string
	for _, kv := range [][2]string{{"DELIMITER", c.Delimiter}, {"NULL", c.NullString}, {"QUOTE", c.Quote}, {"ESCAPE", c.Quote}, {"ENCODING", c.Encoding}} {
		l, err := literal(kv[1])
		if err != nil {
			return "", err
		}
		out = append(out, kv[0]+" "+l)
	}
	return strings.Join(out, ", "), nil
}

// HeaderMode of an import: none, skip or match.
type HeaderMode string

// The header modes of ImportTableRequest.
const (
	HeaderNone  HeaderMode = "none"
	HeaderSkip  HeaderMode = "skip"
	HeaderMatch HeaderMode = "match"
)

func identList(names []string) (string, error) {
	var parts []string
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			return "", fmt.Errorf("column %q is listed twice", n)
		}
		seen[n] = true
		id, err := sqlx.NewIdent(n)
		if err != nil {
			return "", err
		}
		parts = append(parts, id.String())
	}
	return strings.Join(parts, ", "), nil
}

// CopyFromSQL is the statement of an import: COPY into the table with the columns of the file, in the format, with the header mode and the
// forced columns. The names passed sqlx.NewIdent; every option value is a literal.
func CopyFromSQL(schema, table string, columns []string, c CSV, header HeaderMode, forceNotNull, forceNull []string) (string, error) {
	rel, err := sqlx.QualifiedIdent(schema, table)
	if err != nil {
		return "", err
	}
	cols, err := identList(columns)
	if err != nil || cols == "" {
		return "", fmt.Errorf("the columns are not valid")
	}
	opts, err := c.options()
	if err != nil {
		return "", err
	}
	h := map[HeaderMode]string{HeaderNone: "HEADER false", HeaderSkip: "HEADER true", HeaderMatch: "HEADER MATCH"}[header]
	if h == "" {
		return "", fmt.Errorf("the header mode is not valid")
	}
	in := map[string]bool{}
	for _, n := range columns {
		in[n] = true
	}
	with := []string{"FORMAT csv", h, opts}
	for _, f := range []struct {
		kw    string
		names []string
	}{{"FORCE_NOT_NULL", forceNotNull}, {"FORCE_NULL", forceNull}} {
		if len(f.names) == 0 {
			continue
		}
		for _, n := range f.names {
			if !in[n] {
				return "", fmt.Errorf("%s names a column that is not in the file", f.kw)
			}
		}
		l, err := identList(f.names)
		if err != nil {
			return "", err
		}
		with = append(with, f.kw+" ("+l+")")
	}
	return "COPY " + rel.String() + " (" + cols + ") FROM STDIN WITH (" + strings.Join(with, ", ") + ")", nil
}

// ExportColumn is a column of an export: its name and whether the spreadsheet guard applies to it (a column of the string category or an enum).
type ExportColumn struct {
	Name    string
	Stringy bool
}

// spreadsheetPattern is the regular expression of the spreadsheet guard: a cell that begins with =, +, -, @, |, % (also after a tab, a
// carriage return, a no-break space or spaces), a tab or a carriage return, or one of the full-width forms of = + - and @. The characters are
// written as escapes of the regular expression, so that the statement is ASCII.
var spreadsheetPattern = "^[" + bs + "t" + bs + "r" + bs + "u00a0 ]*[=+@|%-]|^[" + bs + "t" + bs + "r]|^[" + bs + "uff1d" + bs + "uff0b" + bs + "uff0d" + bs + "uff20]"

// bs is the backslash: the pattern has the escapes of the regular expression as text, so that the statement stays ASCII.
const bs = "\\"

// SpreadsheetPattern returns the pattern (for tests of it against a table of cells).
func SpreadsheetPattern() string { return spreadsheetPattern }

// CopyToSQL is the statement of an export: COPY of a SELECT of the columns, never of anything the client wrote. only is true for an inheritance
// parent (FROM ONLY); a partitioned or a plain table is read with all its rows. safe puts the spreadsheet guard on the columns that need it.
func CopyToSQL(schema, table string, columns []ExportColumn, c CSV, header, only, safe bool) (string, error) {
	rel, err := sqlx.QualifiedIdent(schema, table)
	if err != nil {
		return "", err
	}
	pat, err := literal(spreadsheetPattern)
	if err != nil {
		return "", err
	}
	var sel []string
	seen := map[string]bool{}
	for _, col := range columns {
		if seen[col.Name] {
			return "", fmt.Errorf("column %q is listed twice", col.Name)
		}
		seen[col.Name] = true
		id, err := sqlx.NewIdent(col.Name)
		if err != nil {
			return "", err
		}
		alias := id.String()
		if safe && header && RiskyCell(col.Name) {
			// The header cells are cells too (DX14-04): the name is given an apostrophe in front, within the 63 bytes of an identifier.
			al, aerr := sqlx.NewIdent(clipName("'" + col.Name))
			if aerr != nil {
				return "", aerr
			}
			alias = al.String()
		}
		if safe && col.Stringy {
			// A risky cell gets an apostrophe in front; the others are the text of the value. The guard is a convenience for spreadsheets and
			// loses what it changes (the padding of a char(n) as well): the dialog says so.
			sel = append(sel, "CASE WHEN "+id.String()+"::text ~ "+pat+" THEN '''' || "+id.String()+"::text ELSE "+id.String()+"::text END AS "+alias)
			continue
		}
		sel = append(sel, id.String()+" AS "+alias)
	}
	if len(sel) == 0 {
		return "", fmt.Errorf("an export needs at least one column")
	}
	from := rel.String()
	if only {
		from = "ONLY " + from
	}
	opts, err := c.options()
	if err != nil {
		return "", err
	}
	h := "HEADER false"
	if header {
		h = "HEADER true"
	}
	return "COPY (SELECT " + strings.Join(sel, ", ") + " FROM " + from + ") TO STDOUT WITH (FORMAT csv, " + h + ", " + opts + ")", nil
}

// SessionFormats are the settings of every connection of a transfer (design "Session formats"), set with SET LOCAL after the connection check, so that
// a round trip does not depend on the settings of a role or of a database. client_encoding is the encoding of the file.
func SessionFormats(encoding string) ([]string, error) {
	if !isOneOf(encoding, encodings) {
		return nil, fmt.Errorf("the encoding is not valid")
	}
	return []string{
		"SET LOCAL DateStyle = 'ISO, YMD'",
		"SET LOCAL IntervalStyle = 'postgres'",
		"SET LOCAL extra_float_digits = 3",
		"SET LOCAL bytea_output = 'hex'",
		"SET LOCAL TimeZone = 'UTC'",
		"SET LOCAL lc_monetary = 'C'",
		"SET LOCAL client_encoding = '" + encoding + "'",
	}, nil
}

// RiskyCell is the Go form of the spreadsheet guard for a single text (a header cell): it begins, after tabs, carriage returns, no-break spaces or spaces,
// with one of = + @ | % -, or with a tab or carriage return, or with a full-width = + - or @.
func RiskyCell(v string) bool {
	t := strings.TrimLeft(v, "	  ")
	if t != "" && strings.ContainsRune("=+@|%-", rune(t[0])) {
		return true
	}
	if v == "" {
		return false
	}
	switch r := []rune(v)[0]; r {
	case '	', '', 0xff1d, 0xff0b, 0xff0d, 0xff20:
		return true
	}
	return false
}

// clipName cuts a name to 63 bytes on a character boundary.
func clipName(n string) string {
	if len(n) <= 63 {
		return n
	}
	b := []byte(n[:63])
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
