// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sqlshape

import "testing"

// The lexer finds in every script of the corpus the number of statements that PostgreSQL finds (the test of package ops checks the same corpus against a real
// server of PostgreSQL 18 and 19).
func TestLexerAgreesWithTheCorpus(t *testing.T) {
	for i, c := range LexCorpus() {
		n, err := CountStatements(c.SQL)
		if err != nil || n != c.N {
			t.Errorf("case %d %q: %d statements (%v), PostgreSQL sees %d", i, c.SQL, n, err, c.N)
		}
	}
}

// A line comment ends at a carriage return (a hidden DROP behind it would run); a dollar-quote tag may start with a byte above 0x7f.
func TestCarriageReturnAndHighByteTags(t *testing.T) {
	if n, _ := CountStatements("CREATE TABLE t (a int) -- x\r; DROP TABLE users;"); n != 2 {
		t.Errorf("a comment that ends at a carriage return hides a statement: %d", n)
	}
	if n, _ := CountStatements("SELECT $é$ ; DROP TABLE users; $é$;"); n != 1 {
		t.Errorf("a tag of high bytes: %d statements", n)
	}
}
