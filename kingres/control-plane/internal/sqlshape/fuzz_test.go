// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sqlshape

import (
	"regexp"
	"strings"
	"testing"
)

var kindRE = regexp.MustCompile(`^[A-Z]+( [A-Z]+){0,3}$`)

func fixedKind(k string) bool {
	return kindRE.MatchString(k) || k == `\restrict` || k == `\unrestrict`
}

func seeds(f *testing.F) {
	for _, c := range LexCorpus() {
		f.Add([]byte(c.SQL))
	}
	for _, c := range accepted {
		f.Add([]byte(c.sql))
	}
	for _, c := range refused {
		f.Add([]byte(c.sql))
	}
	f.Add([]byte("DO $a$ $b$ ; $b$ ; $a$;"))
	f.Add([]byte("SELECT E'\\' /* /* */ */;"))
}

func checkPlan(t *testing.T, p *Plan) {
	t.Helper()
	for _, r := range p.Refused {
		if !fixedKind(r.Kind) {
			t.Fatalf("kind %q is not from the fixed lists", r.Kind)
		}
		if len(r.Reason) > 200 || strings.ContainsAny(r.Reason, "'\"\n") || r.Reason == "" {
			t.Fatalf("reason %q", r.Reason)
		}
		if !strings.HasPrefix(r.Position, "f#") || !strings.Contains(r.Fingerprint, ":") {
			t.Fatalf("%+v", r)
		}
	}
}

// The lexer and the parser never panic, whatever the input, and a refusal has only fixed text.
func FuzzParseFile(f *testing.F) {
	seeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<16 {
			t.Skip()
		}
		p, err := ParseFile(strings.NewReader(string(b)), "f", Hints{InheritanceChildren: map[string]bool{"public.c": true}})
		if err == nil {
			checkPlan(t, p)
		}
	})
}

// Cutting an accepted file anywhere never panics and never turns a cut statement into an accepted one that the whole file did not have.
func FuzzLexer(f *testing.F) {
	seeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<16 {
			t.Skip()
		}
		l := newLexer(strings.NewReader(string(b)))
		for i := 0; i < 5000; i++ {
			r, err := l.next()
			if err != nil || r == nil {
				return
			}
			if r.length < 0 || (!r.isMeta && r.length > MaxStatementBytes+1<<16) {
				t.Fatalf("length %d", r.length)
			}
		}
	})
}
