// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sqlshape

import (
	"regexp"
	"sort"
	"strings"
)

// refuse is the panic payload of a parser that meets something its kind does not allow. Reasons are fixed texts; they never hold a token.
type refuse struct{ reason string }

var metaRE = regexp.MustCompile(`^\\(un)?restrict [A-Za-z0-9]+$`)

// Words that may start a statement or name a kind. A kind that is not in these lists is reported as OTHER, so that no refusal repeats text of
// the file.
var firstWords = setOf("create", "alter", "drop", "comment", "set", "reset", "select", "insert", "update", "delete", "copy", "do", "grant", "revoke",
	"reassign", "load", "call", "begin", "commit", "rollback", "start", "savepoint", "release", "abort", "end", "lock", "truncate", "vacuum", "analyze",
	"cluster", "refresh", "security", "import", "listen", "notify", "prepare", "execute", "explain", "with", "values", "table", "show", "discard",
	"checkpoint", "declare", "fetch", "move", "close", "unlisten", "reindex", "merge", "alter")

var secondWords = setOf("schema", "table", "sequence", "type", "domain", "index", "unique", "view", "materialized", "function", "procedure", "trigger", "policy",
	"rule", "event", "foreign", "collation", "extension", "publication", "subscription", "server", "tablespace", "role", "user", "group", "database",
	"operator", "cast", "aggregate", "language", "text", "conversion", "transform", "access", "statistics", "temp", "temporary", "unlogged", "global",
	"local", "system", "default", "owned", "routine", "large", "constraint", "column", "on", "or", "recursive", "data")

var thirdWords = setOf("trigger", "table", "data", "wrapper", "search", "method", "mapping", "procedure", "view", "index", "privileges")

var txWords = setOf("begin", "commit", "rollback", "start", "savepoint", "release", "abort", "end")

func setOf(w ...string) map[string]bool {
	m := make(map[string]bool, len(w))
	for _, x := range w {
		m[x] = true
	}
	return m
}

// kindOf names the kind of a statement from its first words, only from the lists above.
func kindOf(ts []token) string {
	if len(ts) == 0 {
		return "EMPTY"
	}
	w := func(i int) string {
		if i < len(ts) && ts[i].k == tIdent {
			return ts[i].s
		}
		return ""
	}
	if !firstWords[w(0)] {
		return "OTHER"
	}
	k := strings.ToUpper(w(0))
	switch w(0) {
	case "create", "alter", "drop", "comment":
		i := 1
		if w(0) == "create" && w(1) == "or" && w(2) == "replace" {
			i = 3
		}
		if w(0) == "comment" && w(1) == "on" {
			i = 2
		}
		if secondWords[w(i)] {
			k += " " + strings.ToUpper(w(i))
			if thirdWords[w(i+1)] && (w(i) == "event" || w(i) == "foreign" || w(i) == "text" || w(i) == "access" || w(i) == "user" || w(i) == "materialized" || w(i) == "unique" || w(i) == "default" || w(i) == "large") {
				k += " " + strings.ToUpper(w(i+1))
			}
		} else {
			k += " OTHER"
		}
	}
	return k
}

// parseRaw turns what the lexer read into a statement or a refusal.
func parseRaw(raw *rawStmt, pos string, h Hints) (*Statement, *Refusal) {
	ref := func(kind, reason string) (*Statement, *Refusal) {
		return nil, &Refusal{Kind: kind, Position: pos, Fingerprint: fingerprint(raw.length, raw.hash), Reason: reason}
	}
	if raw.isMeta {
		if raw.bad != "" {
			return ref("BACKSLASH", raw.bad)
		}
		if !metaRE.MatchString(raw.meta) {
			return ref("BACKSLASH", "a backslash command other than \\restrict and \\unrestrict")
		}
		kind := `\restrict`
		if strings.HasPrefix(raw.meta, `\unrestrict`) {
			kind = `\unrestrict`
		}
		return &Statement{Kind: kind, Position: pos, Length: raw.length, Hash: raw.hash}, nil
	}
	switch {
	case raw.tooBig:
		return ref("OTHER", "the statement is longer than 1 MiB")
	case raw.nTok:
		return ref("OTHER", "the statement has too many tokens")
	case len(raw.toks) == 0:
		return ref("EMPTY", "an empty statement")
	case raw.bad != "":
		return ref(kindOf(raw.toks), raw.bad)
	}
	p := &parser{toks: raw.toks, h: h, funcs: map[string]bool{}, rels: map[string]bool{}}
	st := &Statement{Position: pos, Length: raw.length, Hash: raw.hash}
	p.st = st
	if r := p.run(); r != nil {
		return ref(kindOf(raw.toks), r.reason)
	}
	st.Functions = sortedKeys(p.funcs)
	st.Refs = sortedKeys(p.rels)
	sort.Strings(st.Inherits)
	return st, nil
}

type parser struct {
	toks  []token
	i     int
	h     Hints
	st    *Statement
	funcs map[string]bool
	rels  map[string]bool
}

func (p *parser) fail(reason string) { panic(refuse{reason}) }

func (p *parser) run() (r *refuse) {
	defer func() {
		if x := recover(); x != nil {
			if rr, ok := x.(refuse); ok {
				r = &rr
				return
			}
			panic(x)
		}
	}()
	p.statement()
	return nil
}

func (p *parser) more() bool { return p.i < len(p.toks) }

func (p *parser) cur() token {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return token{k: 255}
}

func (p *parser) at(n int) token {
	if p.i+n < len(p.toks) {
		return p.toks[p.i+n]
	}
	return token{k: 255}
}

func (p *parser) isKw(k string) bool { t := p.cur(); return t.k == tIdent && t.s == k }
func (p *parser) isKwAt(n int, k string) bool {
	t := p.at(n)
	return t.k == tIdent && t.s == k
}
func (p *parser) isPunct(c string) bool { t := p.cur(); return t.k == tPunct && t.s == c }
func (p *parser) acceptKw(k string) bool {
	if p.isKw(k) {
		p.i++
		return true
	}
	return false
}
func (p *parser) acceptPunct(c string) bool {
	if p.isPunct(c) {
		p.i++
		return true
	}
	return false
}
func (p *parser) expectKw(k string) {
	if !p.acceptKw(k) {
		p.fail("a clause that pg_dump does not write here (expected " + k + ")")
	}
}
func (p *parser) expectPunct(c string) {
	if !p.acceptPunct(c) {
		p.fail("a clause that pg_dump does not write here (expected " + c + ")")
	}
}
func (p *parser) end() {
	if p.more() {
		p.fail("the statement has more than the allowed clauses")
	}
}

func (p *parser) isName() bool { t := p.cur(); return t.k == tIdent || t.k == tQIdent }

// ident reads one identifier (quoted or not).
func (p *parser) ident() string {
	if !p.isName() {
		p.fail("a name is missing")
	}
	s := p.cur().s
	p.i++
	return s
}

// qname reads a name of one to three parts.
func (p *parser) qname() []string {
	parts := []string{p.ident()}
	for p.isPunct(".") && (p.at(1).k == tIdent || p.at(1).k == tQIdent) {
		p.i++
		parts = append(parts, p.ident())
		if len(parts) > 4 {
			p.fail("a name has too many parts")
		}
	}
	return parts
}

func protectedSchema(s string) bool {
	l := strings.ToLower(s)
	return l == "kingres" || l == "information_schema" || strings.HasPrefix(l, "pg_")
}

// target reads the name of an object that the statement creates or changes and refuses the schemas of the system and of the extension.
func (p *parser) target() (name string, parts []string) {
	parts = p.qname()
	p.checkSchema(parts)
	return strings.Join(parts, "."), parts
}

func (p *parser) checkSchema(parts []string) {
	if len(parts) >= 2 && protectedSchema(parts[0]) {
		p.fail("the schema is a schema of the system or of KingresDB")
	}
}

// ref reads the name of another relation and records it.
func (p *parser) ref() string {
	parts := p.qname()
	p.checkSchema(parts)
	n := strings.Join(parts, ".")
	p.rels[n] = true
	return n
}

// balanced reads "( ... )" with the brackets matched and returns what is inside.
func (p *parser) balanced() []token {
	if !p.isPunct("(") {
		p.fail("a parenthesis is missing")
	}
	var stack []string
	start := p.i + 1
	for ; p.i < len(p.toks); p.i++ {
		t := p.toks[p.i]
		if t.k != tPunct {
			continue
		}
		switch t.s {
		case "(":
			stack = append(stack, ")")
		case "[":
			stack = append(stack, "]")
		case ")", "]":
			if len(stack) == 0 || stack[len(stack)-1] != t.s {
				p.fail("the brackets do not match")
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				in := p.toks[start:p.i]
				p.i++
				return in
			}
		}
	}
	p.fail("the brackets do not match")
	return nil
}

// balancedExpr reads "( expression )" and collects the functions it calls.
func (p *parser) balancedExpr() []token {
	in := p.balanced()
	p.collect(in)
	return in
}

// expr reads an expression up to a comma, a closing bracket or one of the stop words at depth 0, and collects the functions it calls.
func (p *parser) expr(stops map[string]bool) []token {
	start, depth := p.i, 0
	for p.i < len(p.toks) {
		t := p.toks[p.i]
		if t.k == tPunct {
			switch t.s {
			case "(", "[":
				depth++
			case ")", "]":
				if depth == 0 {
					goto done
				}
				depth--
			case ",":
				if depth == 0 {
					goto done
				}
			}
		} else if depth == 0 && t.k == tIdent {
			if stops[t.s] {
				goto done
			}
			if t.s == "not" && p.i > start {
				n := p.at(1)
				prev := p.toks[p.i-1]
				if n.k == tIdent && (n.s == "null" || n.s == "deferrable" || n.s == "valid" || n.s == "enforced") && !(prev.k == tIdent && prev.s == "is") {
					goto done
				}
			}
		}
		p.i++
	}
done:
	if depth != 0 {
		p.fail("the brackets do not match")
	}
	ts := p.toks[start:p.i]
	if len(ts) == 0 {
		p.fail("an expression is missing")
	}
	p.collect(ts)
	return ts
}

// restExpr reads every token that is left (a view body, a WHERE clause), with the brackets matched.
func (p *parser) restExpr() []token {
	start, depth := p.i, 0
	for ; p.i < len(p.toks); p.i++ {
		t := p.toks[p.i]
		if t.k != tPunct {
			continue
		}
		switch t.s {
		case "(", "[":
			depth++
		case ")", "]":
			depth--
			if depth < 0 {
				p.fail("the brackets do not match")
			}
		}
	}
	if depth != 0 {
		p.fail("the brackets do not match")
	}
	ts := p.toks[start:]
	if len(ts) == 0 {
		p.fail("an expression is missing")
	}
	p.collect(ts)
	return ts
}

// Words that are followed by a parenthesis and are not function calls: operators, clauses and the constructs that the SQL syntax defines.
var notFunctions = setOf("and", "or", "not", "in", "any", "all", "some", "exists", "array", "case", "when", "then", "else", "end", "between", "is", "like",
	"ilike", "similar", "distinct", "from", "where", "select", "on", "using", "as", "by", "over", "filter", "within", "row", "values", "union", "intersect",
	"except", "join", "left", "right", "inner", "outer", "cross", "natural", "limit", "offset", "order", "group", "having", "with", "recursive", "lateral",
	"only", "varying", "precision", "zone", "coalesce", "nullif", "greatest", "least", "cast", "current_timestamp", "current_time", "localtimestamp",
	"localtime", "interval", "timestamp", "time", "numeric", "decimal", "varchar", "char", "character", "bit", "float", "operator", "collate", "returning",
	"escape", "at", "to", "into", "set", "do", "default", "primary", "unique", "check", "references", "foreign", "key", "constraint", "partition", "range", "rows", "groups", "asc", "desc", "nulls", "first", "last", "true", "false", "null", "xmlelement", "xmlforest", "xmlconcat", "xmlparse",
	"xmlpi", "xmlroot", "xmlserialize", "xmlagg")

// collect adds the functions that the tokens call: a name that is followed by an opening parenthesis, unless it is a word of the syntax or the
// name of a type in a cast. Names of the syntax that PostgreSQL still lists in pg_proc (for example trim) are reported; 14c checks every name.
func (p *parser) collect(ts []token) {
	for i, t := range ts {
		if i+1 >= len(ts) || ts[i+1].k != tPunct || ts[i+1].s != "(" {
			continue
		}
		if t.k != tIdent && t.k != tQIdent {
			continue
		}
		if t.k == tIdent && notFunctions[t.s] {
			continue
		}
		name, j := t.s, i
		for j-2 >= 0 && ts[j-1].k == tPunct && ts[j-1].s == "." && (ts[j-2].k == tIdent || ts[j-2].k == tQIdent) {
			name = ts[j-2].s + "." + name
			j -= 2
		}
		if j >= 1 && ts[j-1].k == tPunct && ts[j-1].s == ":" {
			continue // the type of a cast
		}
		if j >= 1 && ts[j-1].k == tIdent && (ts[j-1].s == "as" || ts[j-1].s == "returns") {
			continue
		}
		p.funcs[name] = true
	}
}

// collectRelations adds the relations named after FROM and JOIN in a view body.
func (p *parser) collectRelations(ts []token) {
	for i := 0; i+1 < len(ts); i++ {
		if ts[i].k != tIdent || (ts[i].s != "from" && ts[i].s != "join") {
			continue
		}
		j := i + 1
		if ts[j].k == tIdent && ts[j].s == "only" && j+1 < len(ts) {
			j++
		}
		if ts[j].k != tIdent && ts[j].k != tQIdent {
			continue
		}
		if ts[j].k == tIdent && (ts[j].s == "lateral" || ts[j].s == "select" || ts[j].s == "unnest" || ts[j].s == "generate_series") {
			continue
		}
		name := ts[j].s
		for j+2 < len(ts) && ts[j+1].k == tPunct && ts[j+1].s == "." && (ts[j+2].k == tIdent || ts[j+2].k == tQIdent) {
			name += "." + ts[j+2].s
			j += 2
		}
		if j+1 < len(ts) && ts[j+1].k == tPunct && ts[j+1].s == "(" {
			continue
		}
		p.rels[name] = true
	}
}

func joinTokens(ts []token) string {
	var sb strings.Builder
	for i, t := range ts {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(t.s)
	}
	return sb.String()
}

// ---------------------------------------------------------------- statements

func (p *parser) statement() {
	if !p.isName() || p.cur().k != tIdent {
		p.fail("this statement kind is not allowed")
	}
	w := p.cur().s
	p.i++
	switch w {
	case "create":
		p.create()
	case "alter":
		p.alter()
	case "comment":
		p.comment()
	default:
		if txWords[w] {
			p.fail("transaction control is not allowed")
		}
		p.fail("this statement kind is not allowed")
	}
	p.end()
}

func (p *parser) create() {
	if p.isKw("or") {
		p.fail("CREATE OR REPLACE is not allowed")
	}
	if !p.isName() || p.cur().k != tIdent {
		p.fail("this statement kind is not allowed")
	}
	w := p.cur().s
	p.i++
	switch w {
	case "schema":
		p.createSchema()
	case "table":
		p.createTable()
	case "sequence":
		p.createSequence()
	case "type":
		p.createType()
	case "domain":
		p.createDomain()
	case "index":
		p.createIndex()
	case "unique":
		p.expectKw("index")
		p.createIndex()
	case "view":
		p.createView(false)
	case "materialized":
		p.expectKw("view")
		p.createView(true)
	case "unlogged", "temp", "temporary", "global", "local":
		p.fail("temporary and unlogged objects are not allowed")
	default:
		p.fail("this statement kind is not allowed")
	}
}

func (p *parser) ifNotExists() {
	if p.isKw("if") {
		p.i++
		p.expectKw("not")
		p.expectKw("exists")
	}
}

func (p *parser) createSchema() {
	p.st.Kind = "CREATE SCHEMA"
	p.ifNotExists()
	n := p.ident()
	if protectedSchema(n) {
		p.fail("the schema is a schema of the system or of KingresDB")
	}
	p.st.Name = n
	if p.more() {
		p.fail("a schema element or AUTHORIZATION is not allowed")
	}
}

var colStops = setOf("constraint", "check", "references", "primary", "unique", "generated", "deferrable", "initially", "default")

func (p *parser) createTable() {
	p.st.Kind = "CREATE TABLE"
	p.ifNotExists()
	name, _ := p.target()
	p.st.Name = name
	partOf := false
	if p.isKw("partition") {
		p.i++
		p.expectKw("of")
		parent := p.ref()
		p.st.PartitionOf = parent
		partOf = true
	} else if p.isKw("of") {
		p.fail("a typed table (OF type) is not allowed")
	}
	if p.isKw("as") {
		p.fail("CREATE TABLE AS is not allowed")
	}
	if p.isPunct("(") {
		p.tableElements(partOf)
	} else if !partOf {
		p.fail("the column list is missing")
	}
	if partOf {
		if p.acceptKw("default") {
		} else {
			p.expectKw("for")
			p.expectKw("values")
			p.partitionBound()
		}
	}
	if p.isKw("inherits") {
		p.i++
		in := p.balanced()
		if !p.h.InheritanceChildren[name] {
			p.fail("INHERITS is allowed only for a table that the catalog says is an inheritance child")
		}
		// the names: parts separated by commas
		var cur []string
		flush := func() {
			if len(cur) > 0 {
				n := strings.Join(cur, ".")
				p.st.Inherits = append(p.st.Inherits, n)
				p.rels[n] = true
				cur = nil
			}
		}
		for _, t := range in {
			switch {
			case t.k == tPunct && t.s == ",":
				flush()
			case t.k == tPunct && t.s == ".":
			case t.k == tIdent || t.k == tQIdent:
				cur = append(cur, t.s)
			default:
				p.fail("a name is missing")
			}
		}
		flush()
	}
	if p.isKw("partition") {
		p.i++
		p.expectKw("by")
		m := p.ident()
		switch m {
		case "range", "list", "hash":
		default:
			p.fail("the partition method is not allowed")
		}
		p.st.PartitionBy = strings.ToUpper(m)
		in := p.balancedExpr()
		p.st.PartitionKey = simpleColumns(in)
	}
	if p.isKw("using") {
		p.i++
		am := p.ident()
		if am != "heap" && am != "columnar" {
			p.fail("the access method is not allowed")
		}
		p.st.AccessMethod = am
	}
	if p.isKw("with") {
		p.i++
		if p.isKw("oids") {
			p.fail("WITH OIDS is not allowed")
		}
		p.storageParams()
	}
	if p.isKw("tablespace") {
		p.fail("TABLESPACE is not allowed")
	}
	if p.isKw("on") {
		p.fail("ON COMMIT is not allowed")
	}
}

// simpleColumns returns the column names of a key list when every element is one name, else nil.
func simpleColumns(in []token) []string {
	var out []string
	expectName := true
	for _, t := range in {
		switch {
		case expectName && (t.k == tIdent || t.k == tQIdent):
			out = append(out, t.s)
			expectName = false
		case !expectName && t.k == tPunct && t.s == ",":
			expectName = true
		default:
			return nil
		}
	}
	if expectName {
		return nil
	}
	return out
}

// storageParams reads "( name [= value] {, name [= value]} )" of a WITH clause.
func (p *parser) storageParams() {
	in := p.balanced()
	var ts []token
	for _, t := range in {
		if t.k == tPunct && (t.s == "(" || t.s == ")" || t.s == "[" || t.s == "]") {
			p.fail("a storage parameter has a nested bracket")
		}
		ts = append(ts, t)
	}
	if len(ts) == 0 {
		p.fail("a storage parameter is missing")
	}
	// name [. name] [= value]: no expression is accepted, so no function is called
	for _, t := range ts {
		if t.k == tIdent || t.k == tQIdent || t.k == tStr || t.k == tNum || (t.k == tPunct && (t.s == "," || t.s == "=" || t.s == "." || t.s == "-" || t.s == "+")) {
			continue
		}
		p.fail("a storage parameter is not a name and a value")
	}
}

func (p *parser) partitionBound() {
	switch {
	case p.acceptKw("in"):
		p.balancedExpr()
	case p.acceptKw("from"):
		p.balancedExpr()
		p.expectKw("to")
		p.balancedExpr()
	case p.acceptKw("with"):
		p.balancedExpr()
	default:
		p.fail("the partition bound is not allowed")
	}
}

// tableElements reads the parenthesised list of columns and constraints of CREATE TABLE.
func (p *parser) tableElements(partOf bool) {
	p.expectPunct("(")
	if p.acceptPunct(")") {
		return
	}
	for {
		p.tableElement(partOf)
		if p.acceptPunct(",") {
			continue
		}
		p.expectPunct(")")
		return
	}
}

func (p *parser) tableElement(partOf bool) {
	switch {
	case p.isKw("like"):
		p.fail("LIKE is not allowed")
	case p.isKw("constraint"), p.isKw("primary"), p.isKw("unique"), p.isKw("check"), p.isKw("foreign"), p.isKw("exclude"):
		p.tableConstraint()
		return
	}
	col := Column{Name: p.ident()}
	if !partOf {
		col.Type = p.typeName()
	} else if p.isKw("with") {
		p.i++
		p.expectKw("options")
	}
	if p.acceptKw("collate") {
		p.qname()
	}
	p.columnConstraints(false, &col)
	p.st.Columns = append(p.st.Columns, col)
}

func (p *parser) tableConstraint() {
	if p.acceptKw("constraint") {
		p.ident()
	}
	p.constraintBody(true)
}

// typeName reads a type: a name of up to three parts or the SQL spelling of a type of several words, an optional modifier and array brackets.
func (p *parser) typeName() string {
	start := p.i
	first := p.cur()
	if first.k == tIdent && first.s == "setof" {
		p.fail("SETOF is not allowed")
	}
	q := p.qname()
	if len(q) >= 2 {
		p.checkSchema(q[:2])
	}
	if first.k == tIdent && len(q) == 1 {
		switch first.s {
		case "double":
			p.expectKw("precision")
		case "national":
			if !p.acceptKw("character") {
				p.expectKw("char")
			}
			p.acceptKw("varying")
		case "character", "char", "nchar", "bit":
			p.acceptKw("varying")
		case "interval":
			for p.isKw("year") || p.isKw("month") || p.isKw("day") || p.isKw("hour") || p.isKw("minute") || p.isKw("second") || p.isKw("to") {
				p.i++
				if p.isPunct("(") {
					p.balanced()
				}
			}
		}
	}
	if p.isPunct("(") {
		p.balanced()
	}
	if first.k == tIdent && len(q) == 1 && (first.s == "timestamp" || first.s == "time") {
		if p.isKw("with") || p.isKw("without") {
			p.i++
			p.expectKw("time")
			p.expectKw("zone")
		}
	}
	for {
		if p.isPunct("[") {
			p.i++
			if p.at(0).k == tNum {
				p.i++
			}
			p.expectPunct("]")
			continue
		}
		if p.isKw("array") {
			p.i++
			if p.isPunct("[") {
				p.i++
				if p.at(0).k == tNum {
					p.i++
				}
				p.expectPunct("]")
			}
			continue
		}
		break
	}
	return joinTokens(p.toks[start:p.i])
}

// columnConstraints reads the constraints that follow a column or a domain. In a domain only NOT NULL, NULL, CHECK and DEFAULT are allowed.
func (p *parser) columnConstraints(domain bool, col *Column) {
	for p.more() {
		if p.isKw("constraint") {
			p.i++
			p.ident()
		}
		switch {
		case p.isKw("not") && p.isKwAt(1, "null"):
			p.i += 2
			if p.isKw("no") && p.isKwAt(1, "inherit") {
				p.i += 2
			}
		case p.isKw("null"):
			p.i++
		case p.isKw("default"):
			p.i++
			p.expr(colStops)
		case p.isKw("generated"):
			if domain {
				p.fail("GENERATED is not allowed in a domain")
			}
			p.generated(col, true)
		case p.isKw("primary") || p.isKw("unique") || p.isKw("references"):
			if domain {
				p.fail("this constraint is not allowed in a domain")
			}
			p.constraintBody(false)
		case p.isKw("check"):
			p.constraintBody(false)
		case p.isKw("deferrable") || p.isKw("initially") || (p.isKw("not") && (p.isKwAt(1, "deferrable") || p.isKwAt(1, "valid") || p.isKwAt(1, "enforced"))):
			p.tails()
		case p.isKw("collate"):
			p.i++
			p.qname()
		default:
			return
		}
	}
}

// generated reads GENERATED ... AS IDENTITY [( sequence options )] and GENERATED ALWAYS AS ( expression ) [STORED|VIRTUAL].
func (p *parser) generated(col *Column, allowExpr bool) {
	p.expectKw("generated")
	switch {
	case p.acceptKw("always"):
	case p.acceptKw("by"):
		p.expectKw("default")
	default:
		p.fail("a clause that pg_dump does not write here")
	}
	p.expectKw("as")
	switch {
	case p.acceptKw("identity"):
		if col != nil {
			col.Identity = true
		}
		if p.isPunct("(") {
			p.i++
			p.seqOptions(false, true)
			p.expectPunct(")")
		}
	case allowExpr && p.isPunct("("):
		if p.toks[p.i-1].k == tIdent && p.toks[p.i-2].k == tIdent && p.toks[p.i-2].s == "default" {
			p.fail("GENERATED BY DEFAULT AS (expression) is not allowed")
		}
		p.balancedExpr()
		if p.isKw("stored") || p.isKw("virtual") {
			p.i++
		}
		if col != nil {
			col.Generated = true
		}
	default:
		p.fail("a clause that pg_dump does not write here")
	}
}

// constraintBody reads PRIMARY KEY, UNIQUE, CHECK, FOREIGN KEY, REFERENCES or EXCLUDE with the tails pg_dump writes.
func (p *parser) constraintBody(table bool) {
	switch {
	case p.acceptKw("primary"):
		p.expectKw("key")
		p.keyList(table)
	case p.acceptKw("unique"):
		if p.isKw("nulls") {
			p.i++
			p.acceptKw("not")
			p.expectKw("distinct")
		}
		p.keyList(table)
	case p.acceptKw("check"):
		p.balancedExpr()
	case p.acceptKw("foreign"):
		p.expectKw("key")
		p.balanced()
		p.expectKw("references")
		p.references()
	case p.acceptKw("references"):
		p.references()
	case p.acceptKw("exclude"):
		if p.acceptKw("using") {
			p.ident()
		}
		p.balancedExpr()
		if p.isKw("include") {
			p.i++
			p.balanced()
		}
		if p.isKw("where") {
			p.i++
			p.balancedExpr()
		}
	default:
		p.fail("a clause that pg_dump does not write here")
	}
	p.tails()
}

func (p *parser) keyList(table bool) {
	switch {
	case p.isPunct("("):
		p.balancedExpr()
	case p.isKw("using") && p.isKwAt(1, "index"):
		p.i += 2
		p.ident()
	case table:
		p.fail("the column list is missing")
	}
}

func (p *parser) references() {
	p.ref()
	if p.isPunct("(") {
		p.balanced()
	}
}

// tails reads what may follow a constraint: INCLUDE, WITH, MATCH, ON DELETE, ON UPDATE, DEFERRABLE, INITIALLY, NOT VALID, NO INHERIT, ENFORCED.
func (p *parser) tails() {
	for p.more() {
		switch {
		case p.isKw("include"):
			p.i++
			p.balanced()
		case p.isKw("with") && p.at(1).k == tPunct && p.at(1).s == "(":
			p.i++
			p.storageParams()
		case p.isKw("without") && p.isKwAt(1, "overlaps"):
			p.i += 2
		case p.isKw("match"):
			p.i++
			p.ident()
		case p.isKw("on") && (p.isKwAt(1, "delete") || p.isKwAt(1, "update")):
			p.i += 2
			switch {
			case p.acceptKw("cascade"), p.acceptKw("restrict"):
			case p.acceptKw("no"):
				p.expectKw("action")
			case p.acceptKw("set"):
				if !p.acceptKw("null") {
					p.expectKw("default")
				}
				if p.isPunct("(") {
					p.balanced()
				}
			default:
				p.fail("the referential action is not allowed")
			}
		case p.isKw("deferrable"):
			p.i++
		case p.isKw("initially"):
			p.i++
			if !p.acceptKw("deferred") {
				p.expectKw("immediate")
			}
		case p.isKw("not") && (p.isKwAt(1, "deferrable") || p.isKwAt(1, "valid") || p.isKwAt(1, "enforced")):
			p.i += 2
		case p.isKw("enforced"):
			p.i++
		case p.isKw("no") && p.isKwAt(1, "inherit"):
			p.i += 2
		default:
			return
		}
	}
}

// seqOptions reads the options of a sequence or an identity column. owned adds OWNED BY; in a parenthesis the options end at ")".
func (p *parser) seqOptions(owned, inParens bool) {
	num := func() {
		if p.isPunct("-") || p.isPunct("+") {
			p.i++
		}
		if p.cur().k != tNum {
			p.fail("a number is missing")
		}
		p.i++
	}
	for p.more() {
		if inParens && p.isPunct(")") {
			return
		}
		switch {
		case p.acceptKw("as"):
			p.typeName()
		case p.acceptKw("increment"):
			p.acceptKw("by")
			num()
		case p.acceptKw("minvalue"), p.acceptKw("maxvalue"):
			num()
		case p.acceptKw("no"):
			if !(p.acceptKw("minvalue") || p.acceptKw("maxvalue") || p.acceptKw("cycle")) {
				p.fail("a sequence option is not allowed")
			}
		case p.acceptKw("start"):
			p.acceptKw("with")
			num()
		case p.acceptKw("cache"):
			num()
		case p.acceptKw("cycle"):
		case p.isKw("sequence") && p.isKwAt(1, "name"):
			p.i += 2
			p.target()
		case owned && p.isKw("owned"):
			p.i++
			p.expectKw("by")
			p.ownedBy()
		default:
			p.fail("a sequence option is not allowed")
		}
	}
}

func (p *parser) ownedBy() {
	if p.acceptKw("none") {
		p.st.OwnedBy = "none"
		return
	}
	_, parts := p.target()
	if len(parts) < 2 {
		p.fail("OWNED BY needs a table and a column")
	}
	p.st.OwnedBy = strings.Join(parts, ".")
}

func (p *parser) createSequence() {
	p.st.Kind = "CREATE SEQUENCE"
	p.ifNotExists()
	p.st.Name, _ = p.target()
	p.seqOptions(true, false)
}

func (p *parser) createType() {
	p.st.Kind = "CREATE TYPE"
	p.st.Name, _ = p.target()
	if !p.acceptKw("as") {
		p.fail("a base type, a shell type and a function of a type are not allowed")
	}
	switch {
	case p.acceptKw("enum"):
		p.expectPunct("(")
		if !p.acceptPunct(")") {
			for {
				if p.cur().k != tStr {
					p.fail("an enum label is missing")
				}
				p.i++
				if p.acceptPunct(",") {
					continue
				}
				p.expectPunct(")")
				break
			}
		}
	case p.isPunct("("):
		p.i++
		if !p.acceptPunct(")") {
			for {
				p.ident()
				p.typeName()
				if p.acceptKw("collate") {
					p.qname()
				}
				if p.acceptPunct(",") {
					continue
				}
				p.expectPunct(")")
				break
			}
		}
	case p.acceptKw("range"):
		p.expectPunct("(")
		for {
			opt := p.ident()
			p.expectPunct("=")
			switch opt {
			case "subtype":
				p.typeName()
			case "subtype_opclass", "collation", "multirange_type_name":
				p.qname()
			default:
				p.fail("this range type option is not allowed (it names a function or is unknown)")
			}
			if p.acceptPunct(",") {
				continue
			}
			p.expectPunct(")")
			break
		}
	default:
		p.fail("this kind of type is not allowed")
	}
}

func (p *parser) createDomain() {
	p.st.Kind = "CREATE DOMAIN"
	p.st.Name, _ = p.target()
	p.acceptKw("as")
	p.typeName()
	if p.acceptKw("collate") {
		p.qname()
	}
	p.columnConstraints(true, nil)
}

func (p *parser) createIndex() {
	p.st.Kind = "CREATE INDEX"
	if p.isKw("concurrently") {
		p.fail("CONCURRENTLY is not allowed")
	}
	p.ifNotExists()
	if !p.isKw("on") {
		p.st.Name, _ = p.target()
	}
	p.expectKw("on")
	p.acceptKw("only")
	p.ref()
	if p.acceptKw("using") {
		p.ident()
	}
	p.balancedExpr()
	for p.more() {
		switch {
		case p.isKw("include"):
			p.i++
			p.balanced()
		case p.isKw("nulls"):
			p.i++
			p.acceptKw("not")
			p.expectKw("distinct")
		case p.isKw("with"):
			p.i++
			p.storageParams()
		case p.isKw("tablespace"):
			p.fail("TABLESPACE is not allowed")
		case p.isKw("where"):
			p.i++
			p.restExpr()
		default:
			p.fail("the index has a clause that pg_dump does not write")
		}
	}
}

func (p *parser) createView(mat bool) {
	p.st.Kind = "CREATE VIEW"
	if mat {
		p.st.Kind = "CREATE MATERIALIZED VIEW"
		p.ifNotExists()
	}
	if p.isKw("recursive") {
		p.fail("a recursive view is not allowed")
	}
	p.st.Name, _ = p.target()
	if p.isPunct("(") {
		p.balanced()
	}
	if mat && p.isKw("using") {
		p.i++
		am := p.ident()
		if am != "heap" && am != "columnar" {
			p.fail("the access method is not allowed")
		}
		p.st.AccessMethod = am
	}
	if p.isKw("with") {
		p.i++
		p.storageParams()
	}
	if p.isKw("tablespace") {
		p.fail("TABLESPACE is not allowed")
	}
	p.expectKw("as")
	if !(p.isKw("select") || p.isKw("with") || p.isKw("values") || p.isKw("table") || p.isPunct("(")) {
		p.fail("the body of the view is not a query")
	}
	body := p.restExpr()
	if mat {
		n := len(body)
		if n < 3 || !(body[n-3].k == tIdent && body[n-3].s == "with" && body[n-2].k == tIdent && body[n-2].s == "no" && body[n-1].k == tIdent && body[n-1].s == "data") {
			p.fail("a materialized view must be created WITH NO DATA")
		}
		body = body[:n-3]
		p.st.WithNoData = true
	}
	p.collectRelations(body)
}

func (p *parser) alter() {
	switch {
	case p.acceptKw("table"):
		p.alterTable()
	case p.acceptKw("index"):
		p.st.Kind = "ALTER INDEX ATTACH PARTITION"
		if p.isKw("if") {
			p.fail("IF EXISTS is not allowed")
		}
		p.st.Name, _ = p.target()
		p.expectKw("attach")
		p.expectKw("partition")
		p.st.Partition, _ = p.target()
	case p.acceptKw("sequence"):
		p.st.Kind = "ALTER SEQUENCE OWNED BY"
		p.st.Name, _ = p.target()
		p.expectKw("owned")
		p.expectKw("by")
		p.ownedBy()
	default:
		p.fail("this statement kind is not allowed")
	}
}

func (p *parser) alterTable() {
	if p.isKw("if") {
		p.fail("IF EXISTS is not allowed")
	}
	p.acceptKw("only")
	p.st.Name, _ = p.target()
	switch {
	case p.acceptKw("add"):
		p.st.Kind = "ALTER TABLE ADD CONSTRAINT"
		if !p.acceptKw("constraint") {
			p.fail("only ADD CONSTRAINT is allowed")
		}
		p.ident()
		p.constraintBody(true)
	case p.acceptKw("alter"):
		p.st.Kind = "ALTER TABLE ALTER COLUMN"
		p.acceptKw("column")
		p.ident()
		switch {
		case p.acceptKw("set"):
			if p.acceptKw("default") {
				p.expr(map[string]bool{})
			} else {
				p.expectKw("not")
				p.expectKw("null")
			}
		case p.isKw("add"):
			p.i++
			p.generated(nil, false)
		default:
			p.fail("this ALTER COLUMN action is not allowed")
		}
	case p.acceptKw("attach"):
		p.st.Kind = "ALTER TABLE ATTACH PARTITION"
		p.expectKw("partition")
		p.st.PartitionOf = p.st.Name
		p.st.Partition, _ = p.target()
		p.rels[p.st.Partition] = true
		if !p.acceptKw("default") {
			p.expectKw("for")
			p.expectKw("values")
			p.partitionBound()
		}
	case p.acceptKw("replica"):
		p.st.Kind = "ALTER TABLE REPLICA IDENTITY"
		p.expectKw("identity")
		switch {
		case p.acceptKw("default"), p.acceptKw("full"), p.acceptKw("nothing"):
		case p.acceptKw("using"):
			p.expectKw("index")
			p.ident()
		default:
			p.fail("this replica identity is not allowed")
		}
	default:
		p.fail("this ALTER TABLE action is not allowed")
	}
	if p.isPunct(",") {
		p.fail("several actions in one ALTER TABLE are not allowed")
	}
}

func (p *parser) comment() {
	p.st.Kind = "COMMENT"
	p.expectKw("on")
	if !p.isName() || p.cur().k != tIdent {
		p.fail("this COMMENT target is not allowed")
	}
	w := p.cur().s
	p.i++
	switch w {
	case "table", "column", "index", "sequence", "type", "domain", "schema", "view":
		p.st.Comment = strings.ToUpper(w)
		if w == "schema" {
			n := p.ident()
			if protectedSchema(n) {
				p.fail("the schema is a schema of the system or of KingresDB")
			}
			p.st.Name = n
		} else {
			p.st.Name, _ = p.target()
		}
	case "materialized":
		p.expectKw("view")
		p.st.Comment = "MATERIALIZED VIEW"
		p.st.Name, _ = p.target()
	case "constraint":
		p.st.Comment = "CONSTRAINT"
		p.ident()
		p.expectKw("on")
		if p.acceptKw("domain") {
			p.st.Comment = "CONSTRAINT ON DOMAIN"
		}
		p.st.Name, _ = p.target()
	default:
		p.fail("COMMENT on this kind of object is not allowed")
	}
	p.expectKw("is")
	if p.cur().k == tStr {
		p.i++
	} else {
		p.expectKw("null")
	}
}
