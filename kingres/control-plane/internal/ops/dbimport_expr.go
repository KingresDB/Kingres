// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"regexp"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlshape"
)

// The expressions of a table that the importing role made are read back from the catalog as text (pg_get_expr, pg_get_constraintdef and pg_get_indexdef), because the
// catalog does not record a dependency on a function that PostgreSQL pins (every builtin function): pg_depend alone would show a call of a user function and miss a
// call of pg_read_file. The text is the deparsed form with the search path of the session pg_catalog, so a builtin function is written bare and any other is
// written with its schema.

var (
	literalRE = regexp.MustCompile(`'(?:[^']|'')*'`)
	// A call: one to three parts, each a plain or a quoted identifier, joined by dots, then an opening parenthesis.
	callRE = regexp.MustCompile(`((?:"(?:[^"]|"")*"|[A-Za-z_][A-Za-z0-9_$]*)(?:\.(?:"(?:[^"]|"")*"|[A-Za-z_][A-Za-z0-9_$]*)){0,2})\(`)
)

// exprFunctionNames returns the names, as "name" or "schema.name", that an expression of the deparsed form calls. A string literal is not looked into, a word of
// the SQL syntax is not a function, and a name that is preceded by a double colon is the type of a cast and is looked up like a function of that name (the cast
// function of a type is a function of pg_proc).
func exprFunctionNames(text string) []string {
	text = literalRE.ReplaceAllString(text, "''")
	seen := map[string]bool{}
	var out []string
	for _, m := range callRE.FindAllStringSubmatchIndex(text, -1) {
		name := text[m[2]:m[3]]
		// A foreign key names the table that it references, then its columns in parentheses: that is not a call.
		if strings.HasSuffix(strings.ToLower(strings.TrimRight(text[:m[2]], " ")), "references") {
			continue
		}
		parts := splitIdent(name)
		if len(parts) == 1 && sqlshape.IsSyntaxWord(parts[0]) {
			continue
		}
		// A preceding dot would make this the tail of a longer name that the pattern cut (a name of four parts): keep it, the check refuses it.
		full := strings.Join(parts, ".")
		if !seen[full] {
			seen[full] = true
			out = append(out, full)
		}
	}
	return out
}

// splitIdent splits "a.b" and unquotes each part.
func splitIdent(s string) []string {
	var parts []string
	for len(s) > 0 {
		if s[0] == '"' {
			j := 1
			for j < len(s) {
				if s[j] == '"' {
					if j+1 < len(s) && s[j+1] == '"' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j >= len(s) {
				parts = append(parts, s[1:])
				break
			}
			parts = append(parts, strings.ReplaceAll(s[1:j], `""`, `"`))
			s = s[j+1:]
		} else {
			j := strings.IndexByte(s, '.')
			if j < 0 {
				parts = append(parts, s)
				break
			}
			parts = append(parts, s[:j])
			s = s[j:]
		}
		if len(s) > 0 && s[0] == '.' {
			s = s[1:]
		}
	}
	return parts
}
