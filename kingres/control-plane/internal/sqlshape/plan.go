// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package sqlshape is the shape grammar of the SQL files of a database archive (design data-export-import.md, "What may run: the shape
// grammar"). A statement is accepted only if a parser of its exact kind consumes all of it, and each parser accepts only the clauses that
// pg_dump writes for that kind. This version is the validate-only mode: it classifies and checks, it runs nothing and it creates nothing; the
// import steps of phase 14c are built on the Plan it returns.
//
// The guarantee that an archive cannot do more than the importing role could do by hand rests on PostgreSQL's privileges. The grammar narrows
// what is attempted, bounds the resources (statement count and size) and makes the plan checkable before a database exists.
//
// A refusal never carries text of the statement: it has the kind (from a fixed list of words), the position (file and statement number), a
// fingerprint (the length and a hash prefix, as the workbench records statements) and a fixed reason.
package sqlshape

import (
	"fmt"
	"io"
	"sort"
)

// Hints are the facts of the catalog document that the grammar needs to decide a statement.
type Hints struct {
	// InheritanceChildren holds the tables ("schema.table", as written, unquoted) that the catalog says are inheritance children; only
	// they may have INHERITS in their CREATE TABLE.
	InheritanceChildren map[string]bool
}

// Column is a column of a CREATE TABLE.
type Column struct {
	Name      string
	Type      string // the tokens of the type joined by one space; empty for a column of a partition that only adds options
	Generated bool   // GENERATED ALWAYS AS (expression)
	Identity  bool   // GENERATED ... AS IDENTITY
}

// Statement is an accepted statement. Nothing in it is the text of the statement.
type Statement struct {
	Kind         string // for example "CREATE TABLE", "ALTER TABLE ATTACH PARTITION", "\\restrict"
	Position     string // file#N, N counted from 1 over every statement and meta line of the file
	Length       int
	Hash         string
	Name         string   // the object that is created or altered, qualified as written: schema.name
	Refs         []string // other relations the statement names: the parent, inherited tables, referenced tables, tables of a view body
	Functions    []string // functions the expressions of the statement call, sorted, without duplicates
	Columns      []Column // CREATE TABLE
	PartitionOf  string   // CREATE TABLE ... PARTITION OF parent, ALTER TABLE ... ATTACH PARTITION: the parent
	Partition    string   // for ATTACH PARTITION: the child
	PartitionBy  string   // RANGE, LIST or HASH
	PartitionKey []string // the simple column names of PARTITION BY, when every element is a column
	AccessMethod string   // heap, columnar or empty
	Inherits     []string
	OwnedBy      string // ALTER SEQUENCE ... OWNED BY
	WithNoData   bool   // CREATE MATERIALIZED VIEW
	Comment      string // COMMENT ON: the object kind, for example "TABLE"
}

// Fingerprint is the length and a hash prefix of the statement, as the workbench records statements.
func (s Statement) Fingerprint() string { return fingerprint(s.Length, s.Hash) }

// Refusal is a statement the grammar refused.
type Refusal struct {
	Kind        string
	Position    string
	Fingerprint string
	Reason      string
}

func fingerprint(n int, hash string) string { return fmt.Sprintf("%d:%s", n, hash) }

// Plan is the result of reading one SQL file.
type Plan struct {
	File       string
	Statements []Statement
	ByKind     map[string]int
	Refused    []Refusal
	Functions  []string // every function that an expression of the file calls, sorted, without duplicates
	Relations  []string // every relation that the statements name besides the object they create, sorted
}

// Valid says that every statement was accepted.
func (p *Plan) Valid() bool { return len(p.Refused) == 0 }

// ParseFile reads one SQL file (a stream: at most MaxStatementBytes of it is held at a time) and returns its plan. Refused statements are in
// Plan.Refused; the error is for what is not a property of one statement: a read error, a file that ends inside a statement, a string or a
// comment, and more than MaxStatements statements.
func ParseFile(r io.Reader, file string, hints Hints) (*Plan, error) {
	l := newLexer(r)
	plan := &Plan{File: file, ByKind: map[string]int{}}
	funcs, rels := map[string]bool{}, map[string]bool{}
	for n := 1; ; n++ {
		raw, err := l.next()
		if err != nil {
			return plan, err
		}
		if raw == nil {
			break
		}
		if n > MaxStatements {
			return plan, ErrTooManyStatement
		}
		pos := fmt.Sprintf("%s#%d", file, n)
		st, ref := parseRaw(raw, pos, hints)
		if ref != nil {
			plan.Refused = append(plan.Refused, *ref)
			continue
		}
		plan.Statements = append(plan.Statements, *st)
		plan.ByKind[st.Kind]++
		for _, f := range st.Functions {
			funcs[f] = true
		}
		for _, f := range st.Refs {
			rels[f] = true
		}
	}
	plan.Functions = sortedKeys(funcs)
	plan.Relations = sortedKeys(rels)
	return plan, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Count is the number of statements the lexer found: the accepted and the refused.
func (p *Plan) Count() int { return len(p.Statements) + len(p.Refused) }
