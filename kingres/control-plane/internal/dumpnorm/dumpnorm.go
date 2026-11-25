// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package dumpnorm turns the schema-only output of pg_dump of a KingresDB database into the logical schema of a database archive (design
// data-export-import.md, "Normalising the dump"; result of spike S3). A dump of a node is the dump of its partitions: a hash-distributed table is written
// as a table partitioned by hash with one partition per bucket, a user-partitioned one as a tree of bucket partitions that hold the user's leaves, a
// ledger table with its history table and capture triggers, a columnar table behind a SET of the default access method. The normaliser removes and
// rewrites a closed set of these constructs and moves views to a file of their own; it never invents a statement from text, only from the structured
// facts that the caller read from the catalog. Every other statement is kept as pg_dump wrote it, and the shape grammar (package sqlshape) refuses what
// the archive may not carry.
package dumpnorm

import (
	"errors"
	"fmt"
	"strings"
)

// Facts are what the catalog says about the database. The keys are "schema.name" with the names as they are stored (not quoted).
type Facts struct {
	// Remove are the relations that exist only because of the distribution or the ledger: the bucket partitions and their leaves, the history tables of
	// ledger tables. Every statement about one of them, and every attach of one, is removed.
	Remove map[string]bool
	// RemoveIndex are the indexes and constraints of the relations in Remove; ALTER INDEX ... ATTACH PARTITION of one of them is removed.
	RemoveIndex map[string]bool
	// Hash are the hash-distributed tables. The partitioning that pg_dump writes for them is the bucket partitioning.
	Hash map[string]HashTable
	// Ledger are the tables that carry the capture and guard triggers of kingres_ledger (history tables included).
	Ledger map[string]bool
}

// HashTable is a hash-distributed table. When User is not nil the table is also partitioned by the user and the logical table is that partitioning.
type HashTable struct {
	Column string
	User   *UserPartitioning
}

// UserPartitioning is a partitioning that the user chose: a method (RANGE, LIST or HASH), the key as text and the leaves.
type UserPartitioning struct {
	Method     string
	Key        string
	Partitions []Leaf
}

// Leaf is a user partition. Bound is the text that follows FOR VALUES, or DEFAULT.
type Leaf struct {
	Name  string
	Bound string
}

// Output is the three SQL files of the archive.
type Output struct {
	Pre, Views, Post string
}

// Refusal is returned when the dump holds a construct that the archive does not carry and the normaliser can name before the validator does.
type Refusal struct {
	Kind   string
	Reason string
}

func (r *Refusal) Error() string { return "dumpnorm: " + r.Kind + ": " + r.Reason }

// IsRefusal reports whether err is a Refusal.
func IsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// allowedSet is the closed list of SET statements and the one SELECT of the preamble of pg_dump; they are replaced by the fixed settings of the importer.
var preambleSet = map[string]bool{
	"statement_timeout": true, "lock_timeout": true, "idle_in_transaction_session_timeout": true, "transaction_timeout": true,
	"client_encoding": true, "standard_conforming_strings": true, "check_function_bodies": true, "xmloption": true,
	"client_min_messages": true, "row_security": true, "default_with_oids": true,
}

// Normalise normalises the two sections of the dump. The sections are processed in order and views that are found in pre-data and the statements of
// post-data that depend on a view go to the views file.
func Normalise(pre, post string, f Facts) (Output, error) {
	n := &normaliser{f: f, am: "heap", views: map[string]bool{}}
	for _, sec := range []struct {
		text string
		post bool
	}{{pre, false}, {post, true}} {
		stmts, err := split(sec.text)
		if err != nil {
			return Output{}, err
		}
		// A section starts with the default access method of the server; the SET lines of the preamble do not carry over.
		n.am = "heap"
		for _, s := range stmts {
			if err := n.statement(s, sec.post); err != nil {
				return Output{}, err
			}
		}
	}
	return Output{Pre: n.pre.String(), Views: n.views2.String(), Post: n.post.String()}, nil
}

type normaliser struct {
	f      Facts
	am     string
	pre    strings.Builder
	views2 strings.Builder
	post   strings.Builder
	views  map[string]bool
}

func (n *normaliser) emit(b *strings.Builder, text string) {
	b.WriteString(text)
	b.WriteString(";\n\n")
}

// statement decides what becomes of one statement of the dump.
func (n *normaliser) statement(s stmt, post bool) error {
	w := s.words
	out := &n.pre
	if post {
		out = &n.post
	}
	if s.meta != "" {
		// \restrict and \unrestrict are the only backslash commands pg_dump writes; anything else is kept so that the shape grammar refuses it.
		if isRestrict(s.meta) {
			return nil
		}
		n.emit(out, s.text)
		return nil
	}
	if len(w) == 0 {
		return nil
	}
	switch w[0].lower() {
	case "set":
		return n.set(s)
	case "select":
		if qname(w[1:]) == "pg_catalog.set_config" {
			return nil
		}
	case "create":
		return n.create(s, out)
	case "alter":
		return n.alter(s, out)
	case "comment":
		return n.comment(s, out)
	}
	n.emit(out, s.text)
	return nil
}

func isRestrict(meta string) bool {
	f := strings.Fields(meta)
	return len(f) == 2 && (f[0] == `\restrict` || f[0] == `\unrestrict`)
}

func (n *normaliser) set(s stmt) error {
	w := s.words
	if len(w) < 2 {
		return &Refusal{Kind: "SET", Reason: "a SET that is not understood"}
	}
	name := strings.ToLower(w[1].text)
	if name == "default_table_access_method" {
		// SET default_table_access_method = columnar; (or TO)
		val := ""
		for _, t := range w[2:] {
			if t.kind == kWord || t.kind == kString {
				val = strings.ToLower(strings.Trim(t.text, "'"))
			}
		}
		switch val {
		case "heap", "columnar":
			n.am = val
			return nil
		}
		return &Refusal{Kind: "access method", Reason: "a table access method that the archive does not carry"}
	}
	if preambleSet[name] {
		return nil
	}
	return &Refusal{Kind: "SET", Reason: "a SET that the dump does not write by default"}
}

func (n *normaliser) create(s stmt, out *strings.Builder) error {
	w := s.words
	if len(w) < 2 {
		n.emit(out, s.text)
		return nil
	}
	switch w[1].lower() {
	case "extension":
		return nil
	case "table":
		return n.createTable(s, out)
	case "unlogged", "temp", "temporary", "foreign":
		n.emit(out, s.text)
		return nil
	case "view":
		return n.createView(s, 2)
	case "materialized":
		if len(w) > 2 && w[2].lower() == "view" {
			return n.createView(s, 3)
		}
	case "index", "unique":
		return n.createIndex(s, out)
	case "trigger":
		// CREATE TRIGGER <name> <timing> <event> ON <rel> ...
		name := w[2].ident()
		rel := n.after(w, "on")
		if strings.HasPrefix(name, "kingres_ledger_") && n.f.Ledger[rel] {
			return nil
		}
	}
	n.emit(out, s.text)
	return nil
}

// after returns the unquoted qualified name that follows the first word kw at depth zero.
func (n *normaliser) after(w []word, kw string) string {
	for i, t := range w {
		if t.kind == kWord && t.lower() == kw && i+1 < len(w) {
			j := i + 1
			if w[j].kind == kWord && w[j].lower() == "only" && j+1 < len(w) {
				j++
			}
			return qname(w[j:])
		}
	}
	return ""
}

func (n *normaliser) createView(s stmt, nameAt int) error {
	w := s.words
	j := nameAt
	// CREATE [OR REPLACE] [RECURSIVE] VIEW is not what pg_dump writes; the shape grammar refuses it. The name follows VIEW.
	name := qname(w[j:])
	n.views[name] = true
	n.emit(&n.views2, s.text)
	return nil
}

func (n *normaliser) createIndex(s stmt, out *strings.Builder) error {
	w := s.words
	// CREATE [UNIQUE] INDEX [CONCURRENTLY] name ON [ONLY] rel ...
	onAt := -1
	for i, t := range w {
		if t.kind == kWord && t.lower() == "on" {
			onAt = i
			break
		}
	}
	if onAt < 0 || onAt+1 >= len(w) {
		n.emit(out, s.text)
		return nil
	}
	only := w[onAt+1].kind == kWord && w[onAt+1].lower() == "only"
	relAt := onAt + 1
	if only {
		relAt++
	}
	rel := qname(w[relAt:])
	if n.f.Remove[rel] {
		return nil
	}
	text := s.text
	if only {
		if _, ok := n.f.Hash[rel]; ok {
			// ON ONLY a distributed table would leave an invalid index over bucket partitions that do not exist yet when the statement runs.
			text = cut(s, w[onAt+1])
		}
	}
	if n.views[rel] {
		n.emit(&n.views2, text)
		return nil
	}
	n.emit(out, text)
	return nil
}

func (n *normaliser) createTable(s stmt, out *strings.Builder) error {
	w := s.words
	if len(w) < 3 {
		n.emit(out, s.text)
		return nil
	}
	// CREATE TABLE [IF NOT EXISTS] name ...
	j := 2
	if w[j].lower() == "if" {
		j += 3
	}
	if j >= len(w) {
		n.emit(out, s.text)
		return nil
	}
	name := qname(w[j:])
	if n.f.Remove[name] {
		return nil
	}
	text := s.text
	partitioned := false
	var pbAt = -1
	depth := 0
	for i, t := range w {
		if t.kind == kPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				depth--
			}
		}
		if depth == 0 && t.kind == kWord && t.lower() == "partition" && i+2 < len(w) && w[i+1].lower() == "by" {
			partitioned = true
			pbAt = i
		}
	}
	if h, ok := n.f.Hash[name]; ok {
		if !partitioned {
			return &Refusal{Kind: "distributed table", Reason: "a hash-distributed table whose dump is not partitioned"}
		}
		// PARTITION BY HASH (<dist column>) is what the bucket partitioning looks like. It is removed only when the column is the catalog's.
		if w[pbAt+2].lower() != "hash" || !oneColumn(w[pbAt+3:], h.Column) {
			return &Refusal{Kind: "distributed table", Reason: "the partitioning of a distributed table is not the bucket partitioning of the catalog"}
		}
		end := clauseEnd(w, pbAt)
		if h.User != nil {
			clause := " PARTITION BY " + strings.ToUpper(h.User.Method) + " (" + h.User.Key + ")"
			text = replace(s, w[pbAt], end, clause)
		} else {
			text = replace(s, w[pbAt], end, "")
		}
		partitioned = h.User != nil
		n.emit(out, text)
		if h.User != nil {
			for _, l := range h.User.Partitions {
				qs, ts := splitName(name)
				n.emit(out, "CREATE TABLE "+quoteIdent(qs)+"."+quoteIdent(l.Name)+" PARTITION OF "+quoteIdent(qs)+"."+quoteIdent(ts)+" "+boundSQL(l.Bound))
			}
		}
		return nil
	}
	if partitioned && strings.EqualFold(w[pbAt+2].text, "hash") && n.looksBucketed(name) {
		return &Refusal{Kind: "distributed table", Reason: "a table partitioned by hash that the catalog does not know"}
	}
	n.emit(out, withAM(text, n.am, partitioned, w))
	return nil
}

func (n *normaliser) looksBucketed(name string) bool { return n.f.Remove[name] }

func boundSQL(b string) string {
	if strings.EqualFold(strings.TrimSpace(b), "default") {
		return "DEFAULT"
	}
	return "FOR VALUES " + b
}

// withAM adds USING <am> to the CREATE TABLE of a table that was made while the default access method was not heap. A partitioned table has no storage, and
// pg_dump does not name the method for it.
func withAM(text, am string, partitioned bool, w []word) string {
	if am == "heap" || partitioned {
		return text
	}
	// Before a trailing WITH (...) when there is one.
	depth := 0
	for _, t := range w {
		if t.kind == kPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				depth--
			}
		}
		if depth == 0 && t.kind == kWord && t.lower() == "with" {
			return text[:t.start] + "USING " + am + " " + text[t.start:]
		}
	}
	return text + " USING " + am
}

func (n *normaliser) alter(s stmt, out *strings.Builder) error {
	w := s.words
	if len(w) < 3 {
		n.emit(out, s.text)
		return nil
	}
	switch w[1].lower() {
	case "index":
		// ALTER INDEX a ATTACH PARTITION b
		a := qname(w[2:])
		if i := indexOfWord(w, "attach"); i > 0 && i+2 < len(w) {
			b := qname(w[i+2:])
			if n.f.RemoveIndex[a] || n.f.RemoveIndex[b] {
				return nil
			}
		}
	case "table":
		j := 2
		if w[j].lower() == "if" {
			j += 2
		}
		only := false
		if j < len(w) && w[j].lower() == "only" {
			only = true
			j++
		}
		if j >= len(w) {
			break
		}
		name := qname(w[j:])
		if n.f.Remove[name] {
			return nil
		}
		if i := indexOfWord(w, "attach"); i > 0 && i+2 < len(w) && w[i+1].lower() == "partition" {
			if n.f.Remove[qname(w[i+2:])] {
				return nil
			}
		}
		// ALTER TABLE <ledger table> ENABLE ALWAYS TRIGGER kingres_ledger_*
		if i := indexOfWord(w, "trigger"); i > 0 && i+1 < len(w) && indexOfWord(w, "enable") > 0 && strings.HasPrefix(w[i+1].ident(), "kingres_ledger_") && n.f.Ledger[name] {
			return nil
		}
		text := s.text
		if only {
			if _, ok := n.f.Hash[name]; ok {
				// The parent index of a distributed table would stay invalid over bucket partitions that do not exist yet when the statement runs.
				text = cut(s, w[2+boolInt(w[2].lower() == "if")*2])
			}
		}
		if n.views[name] {
			n.emit(&n.views2, text)
			return nil
		}
		n.emit(out, text)
		return nil
	}
	n.emit(out, s.text)
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (n *normaliser) comment(s stmt, out *strings.Builder) error {
	w := s.words
	// COMMENT ON <kind> <name> IS '...'
	if len(w) < 4 {
		n.emit(out, s.text)
		return nil
	}
	kind := w[2].lower()
	rest := w[3:]
	switch kind {
	case "extension":
		return nil
	case "materialized":
		if len(rest) > 0 && rest[0].lower() == "view" {
			if n.views[qname(rest[1:])] {
				n.emit(&n.views2, s.text)
				return nil
			}
		}
	case "view":
		n.emit(&n.views2, s.text)
		return nil
	case "table", "index", "sequence":
		if n.f.Remove[qname(rest)] || n.f.RemoveIndex[qname(rest)] {
			return nil
		}
	case "column":
		full := qnameParts(rest)
		if len(full) >= 3 {
			rel := full[0] + "." + full[1]
			if n.f.Remove[rel] {
				return nil
			}
			if n.views[rel] {
				n.emit(&n.views2, s.text)
				return nil
			}
		}
	case "constraint":
		// COMMENT ON CONSTRAINT name ON rel IS ...
		if i := indexOfWord(w, "on"); i > 3 && i+1 < len(w) {
			if n.f.Remove[qname(w[i+1:])] {
				return nil
			}
		}
	}
	n.emit(out, s.text)
	return nil
}

func indexOfWord(w []word, kw string) int {
	for i, t := range w {
		if t.kind == kWord && t.lower() == kw {
			return i
		}
	}
	return -1
}

// oneColumn reports whether the tokens after PARTITION BY HASH are exactly "(" column ")".
func oneColumn(w []word, col string) bool {
	if len(w) < 3 || w[0].text != "(" || w[2].text != ")" {
		return false
	}
	return w[1].ident() == col
}

// clauseEnd is the offset at which the clause that starts at the word index at ends: after the closing parenthesis of the key list.
func clauseEnd(w []word, at int) int {
	depth := 0
	for i := at; i < len(w); i++ {
		if w[i].kind == kPunct {
			switch w[i].text {
			case "(":
				depth++
			case ")":
				depth--
				if depth == 0 {
					return w[i].end
				}
			}
		}
	}
	return w[len(w)-1].end
}

// cut removes the word w from the statement text and returns the rest (used to drop ONLY).
func cut(s stmt, w word) string {
	return s.text[:w.start] + strings.TrimLeft(s.text[w.end:], " \t\n")
}

func replace(s stmt, from word, to int, with string) string {
	return strings.TrimRight(s.text[:from.start], " \t\n") + with + s.text[to:]
}

func quoteIdent(s string) string {
	ok := s != ""
	for i, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9')) {
			ok = false
		}
	}
	if ok && !reserved[s] {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// reserved is a short list of words that pg_dump would quote; a name that is one of them is quoted, a name that is not is written as it is.
var reserved = map[string]bool{"user": true, "table": true, "select": true, "order": true, "group": true, "from": true, "where": true, "end": true, "all": true, "primary": true, "references": true, "check": true, "default": true}

func splitName(q string) (schema, name string) {
	i := strings.Index(q, ".")
	if i < 0 {
		return "", q
	}
	return q[:i], q[i+1:]
}

// ErrSplit is returned for a dump that ends inside a statement.
var ErrSplit = errors.New("dumpnorm: the dump ends inside a statement")

func (t word) String() string { return fmt.Sprintf("%d:%q", t.kind, t.text) }
