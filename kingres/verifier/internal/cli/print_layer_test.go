// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// L9-10 (3): the output layer is the only place that prints a value that can
// come from an input. The first version of this test looked at lines for
// substrings; this one reads the source. Every call that writes to the standard
// output or error outside cli_out.go must show only what the program itself
// wrote: string literals, a call of untrusted, the name of the flag set, the
// hash format version. A variable is allowed only when it is named below with
// the function it is in and the reason it holds nothing from an input.
type use struct{ fn, ident string }

func TestNoPrintOutsideTheOutputLayer(t *testing.T) {
	allowed := map[use]string{
		{"dispatch", "version"}:          "the version of the program, set at build time",
		{"dispatch", "usageText"}:        "a constant of the program",
		{"newFlagsWithExit", "name"}:     "the name of a command, a literal at every call",
		{"newFlagsWithExit", "synopsis"}: "a literal at every call",
		{"newFlagsWithExit", "help"}:     "a literal at every call",
		{"newFlagsWithExit", "exitText"}: "a literal at every call",
		{"runRowHash", "h"}:              "a hash, printed as hex",
		{"runRowHash", "enc"}:            "the row encoding, printed as hex",
		{"runTrustInit", "note"}:         "a literal of the program",
		{"runTrustInit", "data"}:         "the trust file the program built, written to the output the auditor chose",
		{"runTrustInit", "fingerprint"}:  "hex",
		{"runBlocks", "head"}:            "a header, printed as hex",
	}
	fset := token.NewFileSet()
	ents, _ := os.ReadDir(".")
	checked := 0
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || n == "cli_out.go" || n == "text.go" {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(nd ast.Node) bool {
				call, ok := nd.(*ast.CallExpr)
				if !ok {
					return true
				}
				args, prints := printArgs(call)
				if !prints {
					return true
				}
				checked++
				for _, a := range args {
					if why := unsafeArg(a, fd.Name.Name, allowed); why != "" {
						t.Errorf("%s:%d in %s prints %s without untrusted", n, fset.Position(call.Pos()).Line, fd.Name.Name, why)
					}
				}
				return true
			})
		}
	}
	if checked < 10 {
		t.Fatalf("only %d print calls were found: the scan does not see the source", checked)
	}
}

func isOut(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "stdout" || x.Name == "stderr"
	case *ast.SelectorExpr:
		return x.Sel.Name == "stdout" || x.Sel.Name == "stderr"
	}
	return false
}

// printArgs returns the values a call writes to the standard output or error,
// and whether it is such a call.
func printArgs(call *ast.CallExpr) ([]ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	if id, ok := sel.X.(*ast.Ident); ok && id.Name == "fmt" && strings.HasPrefix(sel.Sel.Name, "Fprint") && len(call.Args) > 0 && isOut(call.Args[0]) {
		return call.Args[1:], true
	}
	if sel.Sel.Name == "Write" && isOut(sel.X) {
		return call.Args, true
	}
	if sel.Sel.Name == "SetOutput" || sel.Sel.Name == "Fprint" {
		return nil, false
	}
	return nil, false
}

// unsafeArg returns what is wrong with an argument, or "".
func unsafeArg(a ast.Expr, fn string, allowed map[use]string) string {
	switch x := a.(type) {
	case *ast.BasicLit:
		return ""
	case *ast.CallExpr:
		switch f := x.Fun.(type) {
		case *ast.Ident:
			if f.Name == "untrusted" {
				return ""
			}
		case *ast.SelectorExpr:
			if f.Sel.Name == "Name" { // the name of the flag set
				return ""
			}
		}
		return "a call"
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && id.Name == "format" && x.Sel.Name == "HashVersion" {
			return ""
		}
		if x.Sel.Name == "Fingerprint" { // hex of a key
			return ""
		}
		return "the field " + x.Sel.Name
	case *ast.Ident:
		if _, ok := allowed[use{fn, x.Name}]; ok {
			return ""
		}
		return "the variable " + x.Name
	case *ast.BinaryExpr:
		if l := unsafeArg(x.X, fn, allowed); l != "" {
			return l
		}
		return unsafeArg(x.Y, fn, allowed)
	case *ast.IndexExpr:
		return "an index expression"
	}
	return "an expression"
}
