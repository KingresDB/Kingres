// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sqlshape

import (
	"errors"
	"strings"
	"testing"
)

// each runs Each over a script and returns the texts it handed out and the error.
func each(script string) ([]string, []Statement, error) {
	var texts []string
	var sts []Statement
	err := Each(strings.NewReader(script), "pre-data.sql", Hints{}, func(st Statement, text string) error {
		texts = append(texts, text)
		sts = append(sts, st)
		return nil
	})
	return texts, sts, err
}

func TestEachHandsOutTheExactTextOfEveryAcceptedStatement(t *testing.T) {
	script := "\\restrict abc123\n-- a comment before\nCREATE SCHEMA app;\n\nCREATE TABLE app.t (\n    id bigint NOT NULL, -- the key\n    note text DEFAULT 'it''s; fine' /* a ; comment */\n);\n" +
		"COMMENT ON TABLE app.t IS 'a ; b';\n\\unrestrict abc123\n"
	texts, sts, err := each(script)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CREATE SCHEMA app",
		"CREATE TABLE app.t (\n    id bigint NOT NULL, -- the key\n    note text DEFAULT 'it''s; fine' /* a ; comment */\n)",
		"COMMENT ON TABLE app.t IS 'a ; b'",
	}
	if len(texts) != len(want) {
		t.Fatalf("%d statements %q, want %d", len(texts), texts, len(want))
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Errorf("statement %d: %q, want %q", i, texts[i], want[i])
		}
	}
	if sts[0].Kind != "CREATE SCHEMA" || sts[1].Kind != "CREATE TABLE" || sts[1].Name != "app.t" || sts[2].Kind != "COMMENT" {
		t.Errorf("kinds: %+v", sts)
	}
}

// Whatever the file holds, a refused statement ends the walk: nothing after it is handed out, and the refusal carries no text.
func TestEachStopsAtTheFirstRefusal(t *testing.T) {
	script := "CREATE SCHEMA app;\nSET ROLE postgres;\nCREATE SCHEMA other;\n"
	texts, _, err := each(script)
	var re *RefusedError
	if !errors.As(err, &re) {
		t.Fatalf("error %v", err)
	}
	if len(texts) != 1 || texts[0] != "CREATE SCHEMA app" {
		t.Errorf("texts %q", texts)
	}
	if re.Refusal.Position != "pre-data.sql#2" || re.Refusal.Kind != "SET" || strings.Contains(re.Error(), "postgres") {
		t.Errorf("refusal %+v: %v", re.Refusal, re)
	}
}

// Statements that are hidden: in a comment, in a dollar quote, in a string, after a carriage return that ends a comment. The text that is handed out holds them as
// data of the statement that has them (and the statement is run as one), or they are statements of their own and are refused.
func TestEachHiddenStatements(t *testing.T) {
	cases := []struct {
		name, script string
		refused      bool
		count        int
	}{
		{"block comment", "CREATE SCHEMA a /* ; DROP SCHEMA public CASCADE */;", false, 1},
		{"line comment to a line feed", "CREATE SCHEMA a -- ; DROP SCHEMA public\n;", false, 1},
		{"dollar quote in a default", "CREATE TABLE a.t (x text DEFAULT $x$; DROP SCHEMA public CASCADE; $x$);", false, 1},
		{"dollar quote with a high byte tag", "CREATE TABLE a.t (x text DEFAULT $é$; DROP SCHEMA public; $é$);", false, 1},
		{"a comment that is a dollar quote is not a form pg_dump writes", "COMMENT ON SCHEMA a IS $x$; DROP SCHEMA public CASCADE; $x$;", true, 0},
		{"string", "COMMENT ON SCHEMA a IS 'x''; DROP SCHEMA public; --';", false, 1},
		{"E string", "COMMENT ON SCHEMA a IS E'x\\'; DROP SCHEMA public; --';", false, 1},
		{"nested comment", "CREATE SCHEMA a /* x /* ; */ ; DROP SCHEMA public */;", false, 1},
		{"a carriage return ends a line comment", "CREATE SCHEMA a -- c\r; DROP SCHEMA public;", true, 1},
		{"a carriage return and a line feed", "CREATE SCHEMA a -- c\r\n; DROP SCHEMA public;", true, 1},
		{"a semicolon after a comment on the same line", "CREATE SCHEMA a; -- ; DROP SCHEMA public\nCREATE SCHEMA b;", false, 2},
		{"a backslash command inside a statement", "CREATE SCHEMA a\n\\! id\n;", true, 0},
		{"a meta line other than restrict", "\\connect postgres\nCREATE SCHEMA a;", true, 0},
		{"two statements on one line", "CREATE SCHEMA a;CREATE SCHEMA b;", false, 2},
	}
	for _, tc := range cases {
		texts, _, err := each(tc.script)
		var re *RefusedError
		refused := errors.As(err, &re)
		if refused != tc.refused || (!refused && err != nil) {
			t.Errorf("%s: error %v, want refused=%v", tc.name, err, tc.refused)
			continue
		}
		if len(texts) != tc.count {
			t.Errorf("%s: %d texts %q, want %d", tc.name, len(texts), texts, tc.count)
		}
		for _, x := range texts {
			if strings.Count(x, ";") > 0 && strings.Contains(x, "\n;") {
				t.Errorf("%s: a text holds its terminator: %q", tc.name, x)
			}
		}
	}
}

func TestEachCountsAreTheLexersOwn(t *testing.T) {
	// The statements that Each hands out are the statements that CountStatements counts, for every script of the corpus that the grammar accepts completely.
	for _, tc := range LexCorpus() {
		n, err := CountStatements(tc.SQL)
		if err != nil || n != tc.N {
			t.Errorf("%q: CountStatements %d (%v), corpus %d", tc.SQL, n, err, tc.N)
		}
	}
}

func TestEachMaximumStatement(t *testing.T) {
	pad := strings.Repeat("x", MaxStatementBytes-len("CREATE SCHEMA a /*  */"))
	ok := "CREATE SCHEMA a /* " + pad + " */;"
	texts, _, err := each(ok)
	if err != nil || len(texts) != 1 || len(texts[0]) != MaxStatementBytes {
		t.Fatalf("a statement of exactly the maximum: %v (%d texts)", err, len(texts))
	}
	_, _, err = each("CREATE SCHEMA a /* " + pad + "x */;")
	var re *RefusedError
	if !errors.As(err, &re) || re.Refusal.Kind != "OTHER" {
		t.Errorf("a statement of the maximum plus one byte: %v", err)
	}
}

func TestEachFileEndsInsideAStatement(t *testing.T) {
	for _, s := range []string{"CREATE SCHEMA a", "CREATE SCHEMA a /* x", "COMMENT ON SCHEMA a IS 'x", "COMMENT ON SCHEMA a IS $q$ x"} {
		if _, _, err := each(s); !errors.Is(err, ErrUnterminated) {
			t.Errorf("%q: %v", s, err)
		}
	}
}

// The text is lexed again alone and must read the same. A text that does not is a refusal (the guard that makes the verdict and the text one thing).
func TestSameTokensGuard(t *testing.T) {
	l := newLexer(strings.NewReader("CREATE SCHEMA a;"))
	raw, err := l.next()
	if err != nil || raw == nil {
		t.Fatal(err)
	}
	if err := sameTokens("CREATE SCHEMA a", raw); err != nil {
		t.Errorf("the same text: %v", err)
	}
	for _, other := range []string{"CREATE SCHEMA b", "CREATE SCHEMA a --", "CREATE SCHEMA", "CREATE SCHEMA a; DROP SCHEMA a", "CREATE SCHEMA a\x00"} {
		if err := sameTokens(other, raw); err == nil {
			t.Errorf("%q passes as the text of the statement", other)
		}
	}
}

func FuzzEach(f *testing.F) {
	for _, tc := range LexCorpus() {
		f.Add(tc.SQL)
	}
	f.Add("CREATE SCHEMA a;\nCREATE TABLE a.t (id int);\n")
	f.Fuzz(func(t *testing.T, s string) {
		// Every text that Each hands out is one statement of the lexer and is accepted by the grammar when it stands alone.
		var texts []string
		err := Each(strings.NewReader(s), "f.sql", Hints{}, func(st Statement, text string) error {
			texts = append(texts, text)
			return nil
		})
		_ = err
		for _, x := range texts {
			n, cerr := CountStatements(x + ";")
			if cerr != nil || n != 1 {
				t.Fatalf("a text of Each is %d statements (%v): %q", n, cerr, x)
			}
			if _, _, werr := each(x + ";"); werr != nil {
				t.Fatalf("a text of Each is not accepted alone (%v): %q", werr, x)
			}
		}
	})
}
