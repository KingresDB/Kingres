// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/dumpnorm"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlshape"
)

// The differential test of the lexers: every script of the corpus (carriage returns, tags of high bytes, nested comments, E” and U&” strings, dollar quotes in
// strings and comments) holds only harmless SELECT statements. A real server (PostgreSQL 18 or 19, whichever the private installation is) reads each script
// with the simple protocol and answers with one result for each statement; the number of results is the number of statements that PostgreSQL sees. The lexer of
// the shape grammar and the splitter of the normaliser must find the same number, and the number that the corpus states must be that number too. No hostile
// statement is ever sent: a lexer that disagrees with the server is found by a count, not by an effect.
func TestLexersAgreeWithTheServer(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	for i, tc := range sqlshape.LexCorpus() {
		results, err := c.PgConn().Exec(context.Background(), tc.SQL).ReadAll()
		if err != nil {
			t.Errorf("case %d %q: the server refuses the script: %v", i, tc.SQL, err)
			continue
		}
		server := 0
		for _, r := range results {
			if r.Err != nil {
				t.Errorf("case %d %q: %v", i, tc.SQL, r.Err)
			}
			server++
		}
		shape, serr := sqlshape.CountStatements(tc.SQL)
		norm, nerr := dumpnorm.CountStatements(tc.SQL)
		if serr != nil || nerr != nil || shape != server || norm != server || tc.N != server {
			t.Errorf("case %d %q: the server sees %d statements, sqlshape %d (%v), dumpnorm %d (%v), the corpus says %d", i, tc.SQL, server, shape, serr, norm, nerr, tc.N)
		}
	}
}
