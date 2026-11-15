// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
)

// The README's table of implemented operations is part of the documentation
// that operators and the web panel's engineers rely on. This test derives the
// "real" set from the handler registry (the same one the router uses) and
// fails when the README disagrees, in either column and in the count.
func TestReadmeTableMatchesTheRegistry(t *testing.T) {
	path := "../../README.md"
	if p := os.Getenv("KINGRES_README"); p != "" {
		path = p
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	real := map[string]bool{}
	for id := range newEnv(t).srv.handlers() {
		real[id] = true
	}
	ids := regexp.MustCompile("`([a-z][A-Za-z0-9]+)`")
	gotReal, gotNot := map[string]bool{}, map[string]bool{}
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "| Tag | Real | Not yet |"):
			in = true
			continue
		case in && !strings.HasPrefix(line, "|"):
			in = false
		}
		if !in || strings.HasPrefix(line, "|---") {
			continue
		}
		cols := strings.Split(strings.Trim(line, "|"), "|")
		if len(cols) != 3 {
			t.Fatalf("README table row has %d columns: %q", len(cols), line)
		}
		for _, m := range ids.FindAllStringSubmatch(cols[1], -1) {
			gotReal[m[1]] = true
		}
		for _, m := range ids.FindAllStringSubmatch(cols[2], -1) {
			gotNot[m[1]] = true
		}
	}
	all := map[string]bool{}
	for _, op := range contract.Operations() {
		all[op.ID] = true
	}
	// The table lists operation ids only; the generic non-operation entries
	// (instance start/stop, local settings) are not ids and are ignored.
	for id := range gotReal {
		if !all[id] {
			t.Errorf("README lists %s, which is not an operation of the contract", id)
		}
	}
	diff := func(name string, want, got map[string]bool) {
		var miss, extra []string
		for id := range want {
			if !got[id] {
				miss = append(miss, id)
			}
		}
		for id := range got {
			if !want[id] && all[id] {
				extra = append(extra, id)
			}
		}
		sort.Strings(miss)
		sort.Strings(extra)
		if len(miss) > 0 || len(extra) > 0 {
			t.Errorf("README column %q: missing %v, wrongly listed %v", name, miss, extra)
		}
	}
	diff("Real", real, gotReal)
	notYet := map[string]bool{}
	for id := range all {
		if !real[id] {
			notYet[id] = true
		}
	}
	diff("Not yet", notYet, gotNot)
	want := fmt.Sprintf("**%d of %d operations are real; %d answer \"not yet\".**", len(real), len(all), len(notYet))
	flat := strings.Join(strings.Fields(string(raw)), " ")
	if !strings.Contains(flat, want) {
		t.Errorf("README does not say %q", want)
	}
}
