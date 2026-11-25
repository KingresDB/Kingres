// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sqlshape

import (
	"errors"
	"strings"
	"testing"
)

func lexAll(t *testing.T, sql string) ([]*rawStmt, error) {
	t.Helper()
	l := newLexer(strings.NewReader(sql))
	var out []*rawStmt
	for {
		r, err := l.next()
		if err != nil {
			return out, err
		}
		if r == nil {
			return out, nil
		}
		out = append(out, r)
	}
}

func texts(r *rawStmt) string {
	var parts []string
	for _, t := range r.toks {
		parts = append(parts, t.s)
	}
	return strings.Join(parts, "|")
}

func TestLexerTokens(t *testing.T) {
	for _, c := range []struct{ name, sql, want string }{
		{"keywords lowered", `Create TABLE Foo;`, "create|table|foo"},
		{"semicolon in string", `COMMENT ON TABLE a.b IS 'x;y';`, "comment|on|table|a|.|b|is|x;y"},
		{"doubled quote", `SELECT 'it''s';`, "select|it's"},
		{"semicolon in quoted ident", `CREATE SCHEMA "a;b";`, "create|schema|a;b"},
		{"doubled quote ident", `CREATE SCHEMA "a""b";`, `create|schema|a"b`},
		{"dollar plain", `DO $$ a; b $$;`, "do| a; b "},
		{"dollar tag", `DO $fn$ x $$ y $fn$;`, "do| x $$ y "},
		{"dollar nested tags", `DO $a$ $b$ ; $b$ ; $a$;`, "do| $b$ ; $b$ ; "},
		{"dollar tag ends only at same tag", `DO $a$ $ab$ ; $a$;`, "do| $ab$ ; "},
		{"dollar is not a tag in identifier", `SELECT a$$b;`, "select|a$$b"},
		{"param", `SELECT $1, $22;`, "select|$1|,|$22"},
		{"E string backslash quote", `SELECT E'a\'b;c';`, `select|a\'b;c`},
		{"E string backslash backslash", `SELECT E'a\\';`, `select|a\\`},
		{"plain string backslash is literal", `SELECT 'a\';`, `select|a\`},
		{"U& string", `SELECT U&'d\0061t\+000061';`, `select|d\0061t\+000061`},
		{"U& ident", `CREATE SCHEMA U&"d\0061t";`, `create|schema|d\0061t`},
		{"line comment", "CREATE -- a;b\nSCHEMA x;", "create|schema|x"},
		{"block comment", `CREATE /* a; b */ SCHEMA x;`, "create|schema|x"},
		{"nested block comment", `CREATE /* a /* ; */ ; */ SCHEMA x;`, "create|schema|x"},
		{"numbers", `SELECT 1.5e+10, .5, 1_000;`, "select|1.5e+10|,|.5|,|1_000"},
		{"cast", `SELECT 1::int;`, "select|1|:|:|int"},
		{"leading comments are not statement", "-- c\n/* d */ CREATE SCHEMA a;", "create|schema|a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rs, err := lexAll(t, c.sql)
			if err != nil || len(rs) != 1 {
				t.Fatalf("%v %d", err, len(rs))
			}
			if got := texts(rs[0]); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestLexerSplitsStatements(t *testing.T) {
	rs, err := lexAll(t, "A 'x;' ; B \"y;\" ; $$ ; $$ ; C /* ; */ ;\n-- end ;\n")
	if err != nil || len(rs) != 4 {
		t.Fatalf("%v %d", err, len(rs))
	}
}

func TestLexerUnterminated(t *testing.T) {
	for _, sql := range []string{"CREATE SCHEMA a", "CREATE SCHEMA a -- x", "SELECT 'abc", `SELECT "abc`, "SELECT $a$ x", "SELECT /* x", "SELECT /* /* */ ;", "SELECT E'a\\'"} {
		if _, err := lexAll(t, sql); !errors.Is(err, ErrUnterminated) {
			t.Errorf("%q: %v", sql, err)
		}
	}
	// a trailing comment or space after the last statement is fine
	if _, err := lexAll(t, "CREATE SCHEMA a;  -- the end"); err != nil {
		t.Error(err)
	}
}

// 1 MiB is accepted and 1 MiB + 1 is refused with the reason "too long"; the length of the statement counts from its first token to the terminator.
func TestStatementSize(t *testing.T) {
	prefix := "COMMENT ON TABLE public.t IS '"
	mk := func(n int) string { return prefix + strings.Repeat("a", n-len(prefix)-1) + "'" }
	p, err := ParseFile(strings.NewReader(mk(MaxStatementBytes)+";"), "f", Hints{})
	if err != nil || !p.Valid() || p.Statements[0].Length != MaxStatementBytes {
		t.Fatalf("1 MiB: %v %+v", err, p.Refused)
	}
	p, err = ParseFile(strings.NewReader(mk(MaxStatementBytes+1)+";CREATE SCHEMA a;"), "f", Hints{})
	if err != nil || len(p.Refused) != 1 || p.Refused[0].Reason != "the statement is longer than 1 MiB" || len(p.Statements) != 1 {
		t.Fatalf("1 MiB + 1: %v %+v %+v", err, p.Refused, p.Statements)
	}
	if !strings.HasPrefix(p.Refused[0].Fingerprint, "1048577:") {
		t.Errorf("fingerprint %q", p.Refused[0].Fingerprint)
	}
	// a comment inside the statement counts too
	big := "CREATE SCHEMA a /*" + strings.Repeat("x", MaxStatementBytes) + "*/;"
	p, err = ParseFile(strings.NewReader(big), "f", Hints{})
	if err != nil || len(p.Refused) != 1 {
		t.Fatalf("%v %+v", err, p.Refused)
	}
	// a long file of many statements is streamed
	var sb strings.Builder
	for i := 0; i < 50000; i++ {
		sb.WriteString("CREATE SCHEMA abcdefghij;\n")
	}
	if p, err = ParseFile(strings.NewReader(sb.String()), "f", Hints{}); err != nil || len(p.Statements) != 50000 {
		t.Fatalf("%v", err)
	}
}

func TestTooManyTokens(t *testing.T) {
	sql := "CREATE TABLE public.t (a int DEFAULT (" + strings.Repeat("1+", 200000) + "1));"
	p, err := ParseFile(strings.NewReader(sql), "f", Hints{})
	if err != nil || len(p.Refused) != 1 || p.Refused[0].Reason != "the statement has too many tokens" {
		t.Fatalf("%v %+v", err, p.Refused)
	}
}

func TestMetaLines(t *testing.T) {
	p, err := ParseFile(strings.NewReader("\\restrict abc\r\n\\unrestrict abc"), "f", Hints{})
	if err != nil || !p.Valid() || len(p.Statements) != 2 {
		t.Fatalf("%v %+v", err, p.Refused)
	}
	for _, line := range []string{"\\restrict", "\\restrict  abc", "\\restrict abc def", "\\restrict ab-c", "\\unrestrict", " \\restrict abc x", "\\q", "\\!ls", "\\i /etc/passwd", "\\restrict " + strings.Repeat("a", 3000)} {
		p, err := ParseFile(strings.NewReader(line+"\n"), "f", Hints{})
		if err != nil || p.Valid() || p.Refused[0].Kind != "BACKSLASH" {
			t.Errorf("%q: %v %+v", line, err, p)
		}
	}
	// a backslash line does not hide the next statement
	p, _ = ParseFile(strings.NewReader("\\q\nCREATE SCHEMA a;"), "f", Hints{})
	if len(p.Refused) != 1 || len(p.Statements) != 1 {
		t.Errorf("%+v", p)
	}
}

func TestReadError(t *testing.T) {
	_, err := ParseFile(errReader{}, "f", Hints{})
	if err == nil {
		t.Error("a read error is an error")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }
