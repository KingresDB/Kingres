// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sqlshape

import (
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed testdata/lexcorpus.json
var lexCorpusJSON []byte

// LexCase is a script of harmless statements whose lexical shape is hard (comments that end at a carriage return, dollar quotes with tags of high bytes, nested
// comments, E” and U&” strings) and the number of statements that PostgreSQL sees in it. The corpus is checked against a real server by a test of package ops
// and against this package and package dumpnorm by their own tests; it is also the seed of the fuzz targets.
type LexCase struct {
	SQL string `json:"sql"`
	N   int    `json:"n"`
}

// LexCorpus returns the corpus.
func LexCorpus() []LexCase {
	var out []LexCase
	if err := json.Unmarshal(lexCorpusJSON, &out); err != nil {
		panic("sqlshape: the corpus of the lexer is not valid: " + err.Error())
	}
	return out
}

// CountStatements is the number of statements (and refusals) that the lexer finds in a script.
func CountStatements(sql string) (int, error) {
	p, err := ParseFile(strings.NewReader(sql), "c", Hints{})
	if err != nil {
		return 0, err
	}
	return p.Count(), nil
}
