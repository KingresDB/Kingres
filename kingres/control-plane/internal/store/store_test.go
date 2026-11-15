// SPDX-License-Identifier: PostgreSQL

package store

import (
	"encoding/base64"
	"errors"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

func TestOffsetCursorRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 50, 123456} {
		got, err := DecodeOffset(EncodeOffset(n))
		if err != nil || got != n {
			t.Errorf("offset %d -> %d, %v", n, got, err)
		}
	}
	if n, err := DecodeOffset(""); err != nil || n != 0 {
		t.Errorf("empty cursor = %d, %v", n, err)
	}
	for _, bad := range []string{"!!!", "AAAA", EncodeKey(5), EncodeOffset(-1), enc("oabc")} {
		if _, err := DecodeOffset(bad); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("DecodeOffset(%q) = %v, want ErrInvalidCursor", bad, err)
		}
	}
}

func TestKeyCursorRoundTrip(t *testing.T) {
	for _, n := range []int64{-5, 0, 7, 1 << 40} {
		got, present, err := DecodeKey(EncodeKey(n))
		if err != nil || !present || got != n {
			t.Errorf("key %d -> %d %v %v", n, got, present, err)
		}
	}
	if _, present, err := DecodeKey(""); err != nil || present {
		t.Errorf("empty cursor: present=%v err=%v", present, err)
	}
	for _, bad := range []string{"!!!", EncodeOffset(3), enc("kabc"), enc("k")} {
		if _, _, err := DecodeKey(bad); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("DecodeKey(%q) = %v", bad, err)
		}
	}
}

func TestPaginate(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	tests := []struct {
		name    string
		page    Page
		want    []int
		hasNext bool
	}{
		{"first", Page{Limit: 2}, []int{1, 2}, true},
		{"second", Page{Limit: 2, Cursor: EncodeOffset(2)}, []int{3, 4}, true},
		{"last partial", Page{Limit: 2, Cursor: EncodeOffset(4)}, []int{5}, false},
		{"exact fit", Page{Limit: 5}, []int{1, 2, 3, 4, 5}, false},
		{"beyond the end", Page{Limit: 2, Cursor: EncodeOffset(99)}, []int{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, next, err := Paginate(items, tc.page)
			if err != nil || got == nil || len(got) != len(tc.want) || (next != nil) != tc.hasNext {
				t.Fatalf("got %v next=%v err=%v", got, next, err)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
	if _, _, err := Paginate(items, Page{Limit: 2, Cursor: "bad"}); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("bad cursor: %v", err)
	}
	if got, _, _ := Paginate([]int(nil), Page{Limit: 2}); got == nil || len(got) != 0 {
		t.Errorf("empty input must give an empty, non-nil page: %#v", got)
	}
}

func TestGrantMatches(t *testing.T) {
	sc, tb := "public", "orders"
	g := api.Grant{Grantee: "alice", Level: "table", Database: "shop", Schema: &sc, Table: &tb}
	tests := []struct {
		name string
		f    GrantFilter
		want bool
	}{
		{"empty", GrantFilter{}, true},
		{"grantee", GrantFilter{Grantee: "alice"}, true},
		{"other grantee", GrantFilter{Grantee: "bob"}, false},
		{"level", GrantFilter{Level: "schema"}, false},
		{"schema and table", GrantFilter{Schema: "public", Table: "orders"}, true},
		{"other table", GrantFilter{Schema: "public", Table: "x"}, false},
		{"database", GrantFilter{Database: "other"}, false},
	}
	for _, tc := range tests {
		if got := GrantMatches(g, tc.f); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	dbLevel := api.Grant{Grantee: "alice", Level: "database", Database: "shop"}
	if GrantMatches(dbLevel, GrantFilter{Schema: "public"}) {
		t.Error("a database-level grant has no schema")
	}
}

func enc(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestTextKeyCursors(t *testing.T) {
	c := EncodeTextKey("public", "orders")
	parts, err := DecodeTextKey(c, 2)
	if err != nil || len(parts) != 2 || parts[0] != "public" || parts[1] != "orders" {
		t.Fatalf("%v %v", parts, err)
	}
	if parts, err := DecodeTextKey("", 2); parts != nil || err != nil {
		t.Errorf("no cursor: %v %v", parts, err)
	}
	// A key of another shape, an offset cursor, junk.
	for _, bad := range []string{EncodeTextKey("a"), EncodeOffset(3), EncodeKey(4), "!!", "dA"} {
		if _, err := DecodeTextKey(bad, 2); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// Names with every character that matters survive.
	odd := EncodeTextKey("a\"b", "c\x00d\n")
	if got, err := DecodeTextKey(odd, 2); err != nil || got[0] != "a\"b" {
		t.Errorf("odd names: %v %v", got, err)
	}
}

func TestKeysetPage(t *testing.T) {
	items := []string{"a", "b", "c", "d"}
	page, next := KeysetPage(items, Page{Limit: 3}, func(s string) string { return "after-" + s })
	if len(page) != 3 || next == nil || *next != "after-c" {
		t.Errorf("%v %v", page, next)
	}
	if page, next := KeysetPage(items[:3], Page{Limit: 3}, func(s string) string { return s }); len(page) != 3 || next != nil {
		t.Errorf("last page: %v %v", page, next)
	}
}
