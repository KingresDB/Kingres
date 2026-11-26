// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// An exit site is a place in the source where a run of a command can end with a
// status other than 0: a call of one of the finish functions (the statuses of
// package cli are decided there and nowhere else), of the flag parser, of exit,
// or of the closure fail of applyState. The sites are found by reading the
// source of the package, not from a list kept by hand, so that a new error
// return without a row in exitSites fails TestEveryExitSiteHasARowAndAScenario.

type site struct {
	key        string
	file       string
	start, end int
	allowed    []int // the statuses this kind of call can end a run with
}

// siteKinds maps the name of a call to the statuses it can give.
var siteKinds = map[string][]int{
	"inputError":       {ExitUsage},
	"inputErrorInfo":   {ExitUsage}, // counted as inputError (see siteAlias)
	"finishInputWith":  {ExitUsage},
	"fail":             {ExitUsage},
	"finishFailed":     {ExitFailed},
	"finishProblems":   {ExitFailed},
	"finishIncomplete": {ExitIncomplete},
	"finishFailedWith": {ExitFailed, ExitEvidenceLost},
	"conflict":         {ExitFailed, ExitEvidenceLost},
	"finishPub":        {ExitFailed, ExitUsage, ExitIncomplete},
	"parseFlags":       {ExitOK, ExitUsage},
	"exitUsage":        {ExitUsage},
	"exitFailed":       {ExitFailed},
}

// siteAlias names the kinds that are another kind with more arguments: the key of a
// site of inputErrorInfo is that of an inputError, so that the rows do not change.
var siteAlias = map[string]string{"inputErrorInfo": "inputError"}

// siteFreeFuncs are the functions that implement the statuses: the calls inside
// them are not sites of a command.
var siteFreeFuncs = map[string]bool{
	"inputError": true, "finishInputWith": true, "finishFailed": true, "finishFailedWith": true,
	"finishProblems": true, "finishIncomplete": true, "finishOK": true, "exit": true, "traceExit": true,
}

func deriveSites(t testing.TB) []site {
	t.Helper()
	fset := token.NewFileSet()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []site
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || siteFreeFuncs[fd.Name.Name] {
				continue
			}
			count := map[string]int{}
			ast.Inspect(fd.Body, func(nd ast.Node) bool {
				call, ok := nd.(*ast.CallExpr)
				if !ok {
					return true
				}
				kind := ""
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					if _, ok := siteKinds[fn.Sel.Name]; ok {
						kind = fn.Sel.Name
						if a, ok := siteAlias[kind]; ok {
							kind = a
						}
					}
				case *ast.Ident:
					switch fn.Name {
					case "fail":
						kind = "fail"
					case "exit":
						if len(call.Args) == 1 {
							if id, ok := call.Args[0].(*ast.Ident); ok {
								switch id.Name {
								case "ExitUsage":
									kind = "exitUsage"
								case "ExitFailed":
									kind = "exitFailed"
								}
							}
						}
					}
				}
				if kind == "" {
					return true
				}
				count[kind]++
				out = append(out, site{
					key:  fmt.Sprintf("%s:%s#%d", fd.Name.Name, kind, count[kind]),
					file: n, start: fset.Position(call.Pos()).Line, end: fset.Position(call.End()).Line,
					allowed: siteKinds[kind],
				})
				return true
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// siteRecorder collects which sites were reached with which status.
type siteRecorder struct {
	mu    sync.Mutex
	sites []site
	hits  map[string]map[int]bool
}

func newSiteRecorder(sites []site) *siteRecorder {
	return &siteRecorder{sites: sites, hits: map[string]map[int]bool{}}
}

func (r *siteRecorder) record(status int, pcs []uintptr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	frames := runtime.CallersFrames(pcs)
	for {
		fr, more := frames.Next()
		base := filepath.Base(fr.File)
		for _, s := range r.sites {
			if s.file == base && fr.Line >= s.start && fr.Line <= s.end {
				if r.hits[s.key] == nil {
					r.hits[s.key] = map[int]bool{}
				}
				r.hits[s.key][status] = true
			}
		}
		if !more {
			break
		}
	}
}
