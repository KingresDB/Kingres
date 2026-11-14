// SPDX-License-Identifier: PostgreSQL

package sqlx

import (
	"errors"
	"strings"
	"testing"
)

func TestNewIdent(t *testing.T) {
	good := map[string]string{
		"orders":         `"orders"`,
		`we"ird`:         `"we""ird"`,
		`a;DROP TABLE x`: `"a;DROP TABLE x"`,
		"naïve":          `"naïve"`,
		"UPPER":          `"UPPER"`,
		`'; --`:          `"'; --"`,
	}
	for in, want := range good {
		got, err := NewIdent(in)
		if err != nil || got.String() != want || got.Name() != in {
			t.Errorf("NewIdent(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	bad := []string{"", strings.Repeat("a", 64), "a\x00b", "a\nb", "a\tb", "\xff", "a\u0085b", "a b", "\x7f"}
	for _, in := range bad {
		if _, err := NewIdent(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("NewIdent(%q) accepted", in)
		}
	}
	if _, err := NewIdent(strings.Repeat("a", 63)); err != nil {
		t.Errorf("63 bytes rejected: %v", err)
	}
	if (Ident{}).Valid() {
		t.Error("zero Ident is valid")
	}
}

func TestQuoteLiteral(t *testing.T) {
	cases := map[string]string{
		"plain":    `'plain'`,
		"it's":     `'it''s'`,
		`back\sl`:  ` E'back\\sl'`,
		`\'; DROP`: ` E'\\''; DROP'`,
	}
	for in, want := range cases {
		if got, err := QuoteLiteral(in); err != nil || got != want {
			t.Errorf("QuoteLiteral(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"a\x00", "\xff"} {
		if _, err := QuoteLiteral(in); err == nil {
			t.Errorf("QuoteLiteral(%q) accepted", in)
		}
	}
}

var ops = NewKeywordSet("SELECT", "GRANT", "ALL PRIVILEGES")

func mustKw(s string) Keyword {
	k, err := ops.Parse(s)
	if err != nil {
		panic(err)
	}
	return k
}

func TestBuilder(t *testing.T) {
	var b Builder
	tbl, _ := QualifiedIdent("public", `o"rders`)
	st, err := b.Kw(mustKw("GRANT")).Kw(mustKw("ALL PRIVILEGES")).Text("ON TABLE").ID(tbl).Text("TO").ID(MustIdent("bob")).Build()
	if err != nil {
		t.Fatal(err)
	}
	if want := `GRANT ALL PRIVILEGES ON TABLE "public"."o""rders" TO "bob"`; st.SQL != want {
		t.Errorf("got %s", st.SQL)
	}
	var c Builder
	st, err = c.Text("SELECT").Arg(1).Text(",").Arg("x").Lit("it's").Build()
	if err != nil || st.SQL != `SELECT $1 , $2 'it''s'` || len(st.Args) != 2 {
		t.Errorf("got %+v %v", st, err)
	}
}

func TestBuilderRejects(t *testing.T) {
	for _, f := range []Fixed{"a; b", "a'b", `a"b`, "a$1", "a--b", "a/*b", "a\nb", "a\\b", "a:b"} {
		var b Builder
		if _, err := b.Text(f).Build(); err == nil {
			t.Errorf("Text(%q) accepted", f)
		}
	}
	for _, k := range []Keyword{"select", "A;B", "A  B", " A", "", "A'B", "A1"} {
		var b Builder
		if _, err := b.Kw(k).Build(); err == nil {
			t.Errorf("Kw(%q) accepted", k)
		}
	}
	var b Builder
	if _, err := b.ID(Ident{}).Build(); err == nil {
		t.Error("zero ident accepted")
	}
	if _, err := ops.Parse("select"); err == nil {
		t.Error("lower case keyword parsed")
	}
	if _, err := (&Builder{}).Build(); err == nil {
		t.Error("empty statement built")
	}
}

// lexIdent reads one quoted identifier at the start of s.
func lexIdent(s string) (val, rest string, ok bool) {
	if s == "" || s[0] != '"' {
		return "", "", false
	}
	var sb strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] == '"' {
			if i+1 < len(s) && s[i+1] == '"' {
				sb.WriteByte('"')
				i++
				continue
			}
			return sb.String(), s[i+1:], true
		}
		sb.WriteByte(s[i])
	}
	return "", "", false
}

func FuzzIdentQuoting(f *testing.F) {
	for _, s := range []string{"a", `"`, `""`, `a"; DROP TABLE t; --`, "é", `\`, "'", "a b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		id, err := NewIdent(in)
		if err != nil {
			return
		}
		val, rest, ok := lexIdent(id.String())
		if !ok || rest != "" || val != in {
			t.Fatalf("%q quoted to %s, which lexes to %q rest %q ok=%v", in, id, val, rest, ok)
		}
		var b Builder
		st, err := b.Text("DROP TABLE").ID(id).Text(",").ID(id).Build()
		if err != nil {
			t.Fatal(err)
		}
		s := strings.TrimPrefix(st.SQL, "DROP TABLE ")
		v1, r1, ok1 := lexIdent(s)
		r1 = strings.TrimPrefix(r1, " , ")
		v2, r2, ok2 := lexIdent(r1)
		if !ok1 || !ok2 || v1 != in || v2 != in || r2 != "" {
			t.Fatalf("statement for %q does not lex as two identifiers: %s", in, st.SQL)
		}
	})
}

func FuzzLiteralQuoting(f *testing.F) {
	for _, s := range []string{"a", "'", `\`, `\'`, `'; DROP`, "é"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		q, err := QuoteLiteral(in)
		if err != nil {
			return
		}
		body := strings.TrimPrefix(q, " E")
		esc := strings.HasPrefix(q, " E")
		if len(body) < 2 || body[0] != '\'' || body[len(body)-1] != '\'' {
			t.Fatalf("not quoted: %q", q)
		}
		var out strings.Builder
		b := body[1 : len(body)-1]
		for i := 0; i < len(b); i++ {
			switch {
			case b[i] == '\'':
				if i+1 >= len(b) || b[i+1] != '\'' {
					t.Fatalf("unescaped quote in %q", q)
				}
				out.WriteByte('\'')
				i++
			case esc && b[i] == '\\':
				if i+1 >= len(b) || b[i+1] != '\\' {
					t.Fatalf("lone backslash in %q", q)
				}
				out.WriteByte('\\')
				i++
			default:
				out.WriteByte(b[i])
			}
		}
		if out.String() != in {
			t.Fatalf("%q decodes to %q", q, out.String())
		}
		if !esc && strings.Contains(in, `\`) {
			t.Fatal("backslash in plain literal")
		}
	})
}

// stripQuoted removes quoted identifiers and quoted literals (with the
// backslash escapes of E” strings).
func stripQuoted(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '"' && c != '\'' {
			out.WriteByte(c)
			continue
		}
		for i++; i < len(s); i++ {
			if c == '\'' && s[i] == '\\' {
				i++
				continue
			}
			if s[i] == c {
				if i+1 < len(s) && s[i+1] == c {
					i++
					continue
				}
				break
			}
		}
	}
	return out.String()
}

func FuzzBuilderNeverSecondStatement(f *testing.F) {
	for _, s := range []string{"a", "a; b", "'", "--", "/*", "$$", "a\nb", `\`} {
		f.Add(s, s, s)
	}
	f.Fuzz(func(t *testing.T, fixed, ident, lit string) {
		var b Builder
		b.Text(Fixed(fixed))
		if id, err := NewIdent(ident); err == nil {
			b.ID(id)
		}
		b.Lit(lit).Arg(lit)
		st, err := b.Build()
		if err != nil {
			return
		}
		rest := stripQuoted(st.SQL)
		for _, bad := range []string{";", "--", "/*", "$$", "'", `"`, "\\"} {
			if strings.Contains(rest, bad) {
				t.Fatalf("%q survives outside quotes in %q", bad, st.SQL)
			}
		}
	})
}
