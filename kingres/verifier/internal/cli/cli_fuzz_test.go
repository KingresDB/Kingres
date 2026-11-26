// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

func FuzzUntrusted(f *testing.F) {
	for _, h := range hostileStrings {
		f.Add(h)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := untrusted(s)
		if !utf8.ValidString(out) || strings.ContainsAny(out, "\n\r\x1b\x00\t\x07") {
			t.Fatalf("%q came out as %q", clipForLog(s), clipForLog(out))
		}
		if len(out) > maxUntrustedText*6+100 {
			t.Fatalf("not bounded: %d", len(out))
		}
	})
}

// FuzzHostileText feeds random bytes to every place where a string of an input
// reaches the output (a key of a JSON file, a path, a flag value, an origin) and
// checks the invariant of the text output: no line looks like a verdict unless
// the verdict is the program's, and no raw control character comes out.
func FuzzHostileText(f *testing.F) {
	for _, h := range hostileStrings[:8] {
		f.Add([]byte(h))
	}
	dir := testdir.New(f)
	pub := filepath.Join(dir, "p.json")
	dig := filepath.Join(dir, "d.json")
	trustP := filepath.Join(dir, "t.json")
	f.Fuzz(func(t *testing.T, b []byte) {
		s := strings.ReplaceAll(string(b), "\x00", "")
		if len(s) > 4000 {
			s = s[:4000]
		}
		_ = os.WriteFile(pub, []byte(`{"format":1,"`+s+`":[1,}`), 0o644)
		_ = os.WriteFile(dig, []byte(`{"`+s+`":1}`), 0o644)
		_ = os.WriteFile(trustP, []byte(`{"format":1,"origin_prefix":"`+s+`","keys":[],"`+s+`":1}`), 0o644)
		for _, args := range [][]string{
			{"proof", "--proof", pub, "--digest", dig},
			{"digest-chain", dig, dig},
			{"trust", "show", "--trust", trustP},
			{"checkpoint", "--trust", trustP, "--log", s, "--source", filepath.Join(dir, s)},
			{"consistency", "--trust", trustP, "--log", "chain:" + strings.Repeat("a", 64), "--old", s},
			{"checkpoint", "--" + s},
			{s},
		} {
			code, out, errs := run(t, args...)
			checkSafeOutput(t, args[0], code, out, errs)
		}
	})
}
