// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package transfer

import (
	"strings"
	"testing"
)

func fields(errs []string) string { return strings.Join(errs, ",") }

func checkFields(c CSV) []string {
	var out []string
	for _, e := range c.Fill().Check() {
		out = append(out, e.Field)
	}
	return out
}

// The format is checked as a whole, and every refusal names the member of the request that is wrong.
func TestCSVCheck(t *testing.T) {
	long := strings.Repeat("n", 33)
	cases := []struct {
		name string
		c    CSV
		want string
	}{
		{"defaults", CSV{}, ""},
		{"semicolon and LATIN1", CSV{Delimiter: ";", Encoding: "LATIN1"}, ""},
		{"tab and pipe and colon", CSV{Delimiter: "\t"}, ""},
		{"null of 32 bytes", CSV{NullString: strings.Repeat("n", 32)}, ""},
		{"null of 33 bytes", CSV{NullString: long}, "/nullString"},
		{"null with the quote", CSV{NullString: `a"b`}, "/nullString"},
		{"null with the delimiter", CSV{NullString: "a,b"}, "/nullString"},
		{"null with a line feed", CSV{NullString: "a\nb"}, "/nullString"},
		{"null with a carriage return", CSV{NullString: "a\rb"}, "/nullString"},
		{"null with NUL", CSV{NullString: "a\x00b"}, "/nullString"},
		{"null that is not UTF-8", CSV{NullString: "\xff\xfe"}, "/nullString"},
		{"null that is the backslash N", CSV{NullString: `\N`}, ""},
		{"delimiter unknown", CSV{Delimiter: "#"}, "/delimiter"},
		{"delimiter two bytes", CSV{Delimiter: ",,"}, "/delimiter"},
		{"delimiter a quote", CSV{Delimiter: `"`}, "/delimiter,/quote"},
		{"encoding unknown", CSV{Encoding: "UTF-16"}, "/encoding"},
		{"encoding lower case", CSV{Encoding: "utf8"}, "/encoding"},
		{"quote the delimiter", CSV{Delimiter: ";", Quote: ";"}, "/quote"},
		{"quote two bytes", CSV{Quote: `""`}, "/quote"},
		{"quote not ASCII", CSV{Quote: "\xc2\xab"}, "/quote"},
		{"quote a control character", CSV{Quote: "\x01"}, "/quote"},
		{"quote a backslash", CSV{Quote: `\`}, "/quote"},
		{"quote a dot", CSV{Quote: "."}, "/quote"},
		{"quote an apostrophe", CSV{Quote: "'"}, ""},
		{"SJIS with a quote below @", CSV{Encoding: "SJIS", Quote: `"`}, ""},
		{"SJIS with a quote above @", CSV{Encoding: "SJIS", Quote: "'", NullString: ""}, ""},
		{"SJIS with the quote A", CSV{Encoding: "SJIS", Quote: "A"}, "/quote"},
		{"BIG5 with the quote a", CSV{Encoding: "BIG5", Quote: "a"}, "/quote"},
		{"GBK with the quote ~", CSV{Encoding: "GBK", Quote: "~"}, "/quote"},
		{"UTF8 with the quote A", CSV{Quote: "A"}, ""},
		{"two refusals", CSV{Delimiter: "#", Encoding: "x"}, "/delimiter,/encoding"},
	}
	for _, tc := range cases {
		if got := fields(checkFields(tc.c)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	// The quote of a client-only encoding: exactly the bytes below 0x40 pass (the quote is never in the trail bytes there).
	for b := byte(0x21); b < 0x7f; b++ {
		if b == ',' || b == '\\' || b == '.' {
			continue
		}
		got := len(CSV{Encoding: "SJIS", Quote: string([]byte{b})}.Fill().Check()) == 0
		if got != (b < 0x40) {
			t.Errorf("SJIS quote %q allowed = %v", b, got)
		}
	}
}

func TestCopyFromSQL(t *testing.T) {
	c := CSV{Delimiter: ";", NullString: `\N`, Quote: "'", Encoding: "LATIN1"}
	got, err := CopyFromSQL("public", "orders", []string{"id", "Customer Name"}, c, HeaderMatch, []string{"id"}, []string{"Customer Name"})
	want := `COPY "public"."orders" ("id", "Customer Name") FROM STDIN WITH (FORMAT csv, HEADER MATCH, DELIMITER ';', NULL E'\\N', QUOTE '''', ESCAPE '''', ENCODING 'LATIN1', FORCE_NOT_NULL ("id"), FORCE_NULL ("Customer Name"))`
	if err != nil || got != want {
		t.Errorf("%v\n got %s\nwant %s", err, got, want)
	}
	for mode, kw := range map[HeaderMode]string{HeaderNone: "HEADER false", HeaderSkip: "HEADER true", HeaderMatch: "HEADER MATCH"} {
		s, err := CopyFromSQL("s", "t", []string{"a"}, DefaultCSV(), mode, nil, nil)
		if err != nil || !strings.Contains(s, kw) {
			t.Errorf("%s: %v %s", mode, err, s)
		}
	}
	// Hostile names stay inside their quotes, hostile values are refused or quoted.
	s, err := CopyFromSQL(`pub"lic`, `t"; DROP TABLE x; --`, []string{`a"b`}, DefaultCSV(), HeaderSkip, nil, nil)
	if err != nil || !strings.Contains(s, `"pub""lic"."t""; DROP TABLE x; --"`) || !strings.Contains(s, `("a""b")`) {
		t.Errorf("hostile names: %v %s", err, s)
	}
	for name, f := range map[string]func() (string, error){
		"no columns": func() (string, error) { return CopyFromSQL("s", "t", nil, DefaultCSV(), HeaderSkip, nil, nil) },
		"a column twice": func() (string, error) {
			return CopyFromSQL("s", "t", []string{"a", "a"}, DefaultCSV(), HeaderSkip, nil, nil)
		},
		"an empty name": func() (string, error) { return CopyFromSQL("s", "t", []string{""}, DefaultCSV(), HeaderSkip, nil, nil) },
		"a long name": func() (string, error) {
			return CopyFromSQL("s", strings.Repeat("x", 64), []string{"a"}, DefaultCSV(), HeaderSkip, nil, nil)
		},
		"a control character": func() (string, error) {
			return CopyFromSQL("s", "t\n", []string{"a"}, DefaultCSV(), HeaderSkip, nil, nil)
		},
		"force a column that is not in the file": func() (string, error) {
			return CopyFromSQL("s", "t", []string{"a"}, DefaultCSV(), HeaderSkip, []string{"b"}, nil)
		},
		"an unknown header mode": func() (string, error) { return CopyFromSQL("s", "t", []string{"a"}, DefaultCSV(), "x", nil, nil) },
		"a NUL in the null text": func() (string, error) {
			return CopyFromSQL("s", "t", []string{"a"}, CSV{Delimiter: ",", Quote: `"`, Encoding: "UTF8", NullString: "a\x00"}, HeaderSkip, nil, nil)
		},
	} {
		if s, err := f(); err == nil {
			t.Errorf("%s was built: %s", name, s)
		}
	}
}

func TestCopyToSQL(t *testing.T) {
	cols := []ExportColumn{{Name: "id"}, {Name: "note", Stringy: true}}
	got, err := CopyToSQL("public", "orders", cols, DefaultCSV(), true, false, false)
	want := `COPY (SELECT "id", "note" FROM "public"."orders") TO STDOUT WITH (FORMAT csv, HEADER true, DELIMITER ',', NULL '', QUOTE '"', ESCAPE '"', ENCODING 'UTF8')`
	if err != nil || got != want {
		t.Errorf("%v\n got %s\nwant %s", err, got, want)
	}
	// The guard is on the string columns only, and only when asked for.
	safe, err := CopyToSQL("public", "orders", cols, DefaultCSV(), false, true, true)
	if err != nil || !strings.Contains(safe, `"id", CASE WHEN "note"::text ~ `) || !strings.Contains(safe, `THEN '''' || "note"::text ELSE "note"::text END AS "note"`) ||
		!strings.Contains(safe, "HEADER false") || !strings.Contains(safe, `FROM ONLY "public"."orders"`) || strings.Contains(safe, `"id"::text`) {
		t.Errorf("%v %s", err, safe)
	}
	wantPat := "E'^[~~t~~r~~u00a0 ]*[=+@|%-]|^[~~t~~r]|^[~~uff1d~~uff0b~~uff0d~~uff20]'"
	wantPat = strings.ReplaceAll(wantPat, "~~", string(rune(92))+string(rune(92)))
	if !strings.Contains(safe, wantPat) {
		t.Errorf("the pattern: %s", safe)
	}
	for _, r := range safe {
		if r > 0x7e || (r < 0x20 && r != '\t') {
			t.Errorf("the statement has the character %U", r)
		}
	}
	if _, err := CopyToSQL("s", "t", nil, DefaultCSV(), true, false, false); err == nil {
		t.Error("an export without columns")
	}
	if _, err := CopyToSQL("s", "t", []ExportColumn{{Name: "a"}, {Name: "a"}}, DefaultCSV(), true, false, false); err == nil {
		t.Error("a column twice")
	}
	if _, err := CopyToSQL("s", "t\x00", []ExportColumn{{Name: "a"}}, DefaultCSV(), true, false, false); err == nil {
		t.Error("a NUL in a name")
	}
}

func TestSessionFormats(t *testing.T) {
	got, err := SessionFormats("WIN1256")
	if err != nil || len(got) != 7 || got[6] != "SET LOCAL client_encoding = 'WIN1256'" || got[0] != "SET LOCAL DateStyle = 'ISO, YMD'" {
		t.Errorf("%v %v", err, got)
	}
	for _, bad := range []string{"", "UTF8'; DROP", "utf8", "x"} {
		if _, err := SessionFormats(bad); err == nil {
			t.Errorf("%q was accepted as an encoding", bad)
		}
	}
}
