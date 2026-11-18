// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A2-12: the packages hold no package-level state that a call changes. Every
// package-level variable of the five packages is listed here, with why it is
// harmless: a sentinel error, a table that nothing writes, or an interface
// check. A new one must be added on purpose (and be one of those three kinds);
// a test must never need to replace one.
var allowedGlobals = map[string]string{
	"anchor.ErrTooLarge":        "sentinel error",
	"anchor.ErrRedirectRefused": "sentinel error",
	"anchor.ErrNotAllowed":      "sentinel error",
	"anchor.States":             "table, read only",
	"anchor.Events":             "table, read only",
	"anchor.transitions":        "table, read only",
	"ots.checkpoints":           "table of facts of the chain, read only",
	"ots.refusedOps":            "table, read only",
	"ots.errNoCalendars":        "sentinel error",
	"tsa.errDER":                "sentinel error",
	"tsa.hashByOID":             "table, read only",
	"worm.errRepairable":        "sentinel error",
	"ots.rmdLeftK":              "table, read only",
	"ots.rmdLeftR":              "table, read only",
	"ots.rmdLeftS":              "table, read only",
	"ots.rmdRightK":             "table, read only",
	"ots.rmdRightR":             "table, read only",
	"ots.rmdRightS":             "table, read only",
	"sigv4.unsigned":            "table, read only",
	"worm.codeRE":               "compiled regular expression, read only",
	"ots._":                     "interface check",
	"tsa._":                     "interface check",
	"worm._":                    "interface check",
}

func TestNoPackageLevelState_SkipsWithoutSources(t *testing.T) {
	var found []string
	for _, pkg := range []string{".", "worm", "tsa", "ots", "sigv4"} {
		files, err := filepath.Glob(filepath.Join(pkg, "*.go"))
		if err != nil || len(files) == 0 {
			t.Skipf("the sources are not beside the test binary (%s)", pkg)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			if _, err := os.Stat(f); err != nil {
				t.Skip(err)
			}
			fs := token.NewFileSet()
			af, err := parser.ParseFile(fs, f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range af.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, s := range gd.Specs {
					vs := s.(*ast.ValueSpec)
					for _, n := range vs.Names {
						name := af.Name.Name + "." + n.Name
						if isSentinel(vs) {
							continue
						}
						if _, ok := allowedGlobals[name]; !ok {
							found = append(found, name)
						}
					}
				}
			}
		}
	}
	sort.Strings(found)
	if len(found) > 0 {
		t.Fatalf("package-level variables that are not on the list of harmless ones (sentinel errors, read-only tables): %v", found)
	}
}

// isSentinel reports whether the variable is made by errors.New or
// fmt.Errorf: a sentinel error, which nothing changes.
func isSentinel(vs *ast.ValueSpec) bool {
	if len(vs.Values) != 1 {
		return false
	}
	call, ok := vs.Values[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && (id.Name == "errors" && sel.Sel.Name == "New" || id.Name == "fmt" && sel.Sel.Name == "Errorf")
}
