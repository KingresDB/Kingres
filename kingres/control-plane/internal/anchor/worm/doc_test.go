// SPDX-License-Identifier: AGPL-3.0-only

package worm

import (
	_ "embed"
	goparser "go/parser"
	"go/token"
	"strings"
	"testing"
)

// A5-07: the package comment is one comment. A blank line that cuts it in two
// leaves only the last part as the documentation of the package (the part before
// it is lost to go doc), so the test reads the source of doc.go, which is
// embedded in the test binary, and requires one comment group before the package
// clause (besides the licence line), its first words, and every section heading.
//
//go:embed doc.go
var docSource string

func TestPackageCommentIsOneCommentWithItsSections(t *testing.T) {
	fset := token.NewFileSet()
	f, err := goparser.ParseFile(fset, "doc.go", docSource, goparser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	if f.Doc == nil || !strings.HasPrefix(f.Doc.Text(), "Package worm ") {
		t.Fatal("the package comment does not start with 'Package worm '")
	}
	before := 0
	for _, g := range f.Comments {
		if g.End() < f.Package {
			before++
		}
	}
	if before != 2 {
		t.Fatalf("%d comment groups before the package clause: want the licence line and one package comment", before)
	}
	text := f.Doc.Text()
	for _, h := range []string{
		"What Submit does",
		"The time of the store (correction 89)",
		"The generation of the key: a way out per record (correction 95)",
		"The Test button (correction 105)",
		"The credential",
		"The retention floor",
		"Evidence",
		"Configuration",
		"Limits",
		"What the caller must do (the notes for the wiring)",
	} {
		if !strings.Contains(text, "\n# "+h+"\n") {
			t.Errorf("the package comment lacks the section %q", h)
		}
	}
}
