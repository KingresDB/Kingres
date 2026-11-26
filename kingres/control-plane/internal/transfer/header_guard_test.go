// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package transfer

import (
	"strings"
	"testing"
)

// DX14-04: the header cells are under the spreadsheet guard: a risky column name is aliased with an apostrophe in front, a plain one is not.
func TestRiskyCellAndHeaderAlias(t *testing.T) {
	risky := []string{"=1+1", "+x", "-x", "@x", "|x", "%x", "\tx", "\rx", " =x", "\u00a0=x", "\uff1dx", "\uff0bx", "\uff0dx", "\uff20x"}
	for _, v := range risky {
		if !RiskyCell(v) {
			t.Errorf("%q is not seen as risky", v)
		}
	}
	for _, v := range []string{"", "id", "a=b", "name", "x-y", "1abc"} {
		if RiskyCell(v) {
			t.Errorf("%q is seen as risky", v)
		}
	}
	sql, err := CopyToSQL("public", "t", []ExportColumn{{Name: "=1+1"}, {Name: "ok"}}, DefaultCSV(), true, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, `AS "'=1+1"`) || !strings.Contains(sql, `"ok" FROM`) {
		t.Errorf("statement: %s", sql)
	}
	// Without the guard, or without a header, the names are left alone.
	for _, c := range []struct{ header, safe bool }{{true, false}, {false, true}} {
		s, err := CopyToSQL("public", "t", []ExportColumn{{Name: "=1+1"}}, DefaultCSV(), c.header, false, c.safe)
		if err != nil || strings.Contains(s, `'=1+1`) {
			t.Errorf("header %v safe %v: %v %s", c.header, c.safe, err, s)
		}
	}
	// A name of 63 bytes stays within 63 bytes with the apostrophe, on a character boundary.
	long := "=" + strings.Repeat("\u00e9", 31)
	s, err := CopyToSQL("public", "t", []ExportColumn{{Name: long}}, DefaultCSV(), true, false, true)
	if err != nil || !strings.Contains(s, `AS "'=`) {
		t.Errorf("long name: %v %s", err, s)
	}
}
