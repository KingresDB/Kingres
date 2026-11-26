// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// hostile strings of the kind a publisher or a database server can put into a
// file: every one tries to start a line that looks like a verdict, to move the
// terminal, or to hide the real line.
var hostileStrings = []string{
	"a\nVERIFIED: forged",
	"x\r\nFAILED at step y: z",
	"\x1b[2J\x1b]0;title\x07",
	"\u202eevil\u202c",
	"\u2028VERIFIED: x",
	"\u2029CONSISTENT: x",
	"\u0085VERIFIED: x",
	"bad\xff\xfeutf8",
	"tab\there",
	"\u200b\u200e\u2066hidden\u2069",
	"\ufeffbom",
	strings.Repeat("A", 100000) + "\nVERIFIED: x",
	"INCOMPLETE: a check",
	"  ok  parse            the note is well formed",
}

var verdictWords = []string{"VERIFIED:", "CONSISTENT:", "FAILED at step ", "INCONSISTENT:", "INCOMPLETE:", "TRUST FILE:"}

func startsWithVerdict(line string) bool {
	for _, w := range verdictWords {
		if strings.HasPrefix(line, w) {
			return true
		}
	}
	return false
}

// checkSafeOutput asserts the invariant of the text output: no byte from an
// input reaches it unescaped, and no input produced a line that looks like a
// verdict of the program. The verdict lines of the program itself are the only
// ones that start with a verdict word, and there is at most one; a run that
// did not succeed has no success verdict at all.
func checkSafeOutput(t *testing.T, name string, code int, stdout, stderr string) {
	t.Helper()
	if strings.HasPrefix(stdout, "Usage: kingres-verify") {
		// The usage text is the program's own, printed when a flag is wrong;
		// the error that names the flag goes to the standard error.
		stdout = ""
	}
	for stream, s := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if !utf8.ValidString(s) {
			t.Errorf("%s: %s is not valid UTF-8: %q", name, stream, clipForLog(s))
			continue
		}
		for _, r := range s {
			bad := (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r < 0xa0) ||
				unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
			if bad {
				t.Errorf("%s: %s holds the character U+%04X unescaped: %q", name, stream, r, clipForLog(s))
				break
			}
		}
		lines := strings.Split(s, "\n")
		verdicts := 0
		for _, l := range lines {
			if startsWithVerdict(l) {
				verdicts++
				if code != 0 && (strings.HasPrefix(l, "VERIFIED:") || strings.HasPrefix(l, "CONSISTENT:")) {
					t.Errorf("%s: a success verdict in a run with status %d: %q", name, code, clipForLog(l))
				}
			}
			if stream == "stdout" && l != "" && !startsWithVerdict(l) && !strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "Usage") && !strings.HasPrefix(l, "{") && !strings.HasPrefix(l, "}") && !strings.HasPrefix(l, "wrote ") && !isHexLine(l) {
				t.Errorf("%s: a line that is not the program's: %q", name, clipForLog(l))
			}
		}
		if verdicts > 1 {
			t.Errorf("%s: %d verdict lines in %s", name, verdicts, stream)
		}
	}
}

func isHexLine(l string) bool {
	l = strings.TrimPrefix(l, "encoding ")
	if l == "" {
		return false
	}
	for _, c := range l {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func clipForLog(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func TestUntrustedRendersEverythingHostile(t *testing.T) {
	for _, h := range hostileStrings {
		out := untrusted(h)
		if !utf8.ValidString(out) || strings.ContainsAny(out, "\n\r\x1b\t\x00") {
			t.Errorf("%q came out as %q", clipForLog(h), clipForLog(out))
		}
		if len(out) > maxUntrustedText+100 {
			t.Errorf("a value of %d bytes was not clipped (%d)", len(h), len(out))
		}
		for _, r := range out {
			if unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
				t.Errorf("%q: U+%04X left in %q", clipForLog(h), r, clipForLog(out))
			}
		}
	}
	// An ordinary string and non-ASCII letters pass as they are (a backslash is
	// written twice, see TestUntrustedIsOneToOne).
	for _, s := range []string{"plain text", "caf\u00e9 \u4e2d\u6587"} {
		if untrusted(s) != s {
			t.Errorf("%q was changed to %q", s, untrusted(s))
		}
	}
}

// hostileScenario runs a command with a hostile string in a place an input can
// reach, in text and in JSON.
type hostileScenario struct {
	name string
	args func(t *testing.T, h string) []string
}

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHostileTextInEveryPlace(t *testing.T) {
	p := newPublication(t, 300, 700)
	good := fx("digests/d0.json")
	scenarios := []hostileScenario{
		{"proof file key", func(t *testing.T, h string) []string {
			f := writeTemp(t, testdir.New(t), "p.json", `{"format":1,"`+h+`":[1,}`)
			return []string{"proof", "--proof", f, "--digest", good}
		}},
		{"digest file key", func(t *testing.T, h string) []string {
			f := writeTemp(t, testdir.New(t), "d.json", `{"`+h+`":[1,}`)
			return []string{"proof", "--proof", fx("proofs/" + firstProof(t)), "--digest", f}
		}},
		{"digest-chain digest key", func(t *testing.T, h string) []string {
			f := writeTemp(t, testdir.New(t), "d.json", `{"`+h+`":1}`)
			return []string{"digest-chain", f, good}
		}},
		{"blocks file", func(t *testing.T, h string) []string {
			f := writeTemp(t, testdir.New(t), "b.jsonl", `{"`+h+`":1}`+"\n")
			return []string{"blocks", "--blocks", f, "--digest", good}
		}},
		{"row-hash column name", func(t *testing.T, h string) []string {
			f := writeTemp(t, testdir.New(t), "c.json", `[{"name":"`+h+`","send_hex":"zz"}]`)
			return []string{"row-hash", "--table-id", "1", "--columns", f}
		}},
		{"trust member name", func(t *testing.T, h string) []string {
			f := writeTemp(t, testdir.New(t), "t.json", `{"format":1,"origin_prefix":"x","keys":[],"`+h+`":1}`)
			return []string{"trust", "show", "--trust", f}
		}},
		{"trust label and prefix", func(t *testing.T, h string) []string {
			f := writeTemp(t, testdir.New(t), "t.json", `{"format":1,"origin_prefix":"`+h+`","keys":[{"name":"`+h+`","public_key":"AA","fingerprint":"00"}]}`)
			return []string{"checkpoint", "--trust", f, "--log", p.log(), "--source", p.dir}
		}},
		{"--log", func(t *testing.T, h string) []string {
			return []string{"checkpoint", "--trust", p.trust, "--log", h, "--source", p.dir}
		}},
		{"--source", func(t *testing.T, h string) []string {
			return []string{"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", h}
		}},
		{"--trust", func(t *testing.T, h string) []string {
			return []string{"checkpoint", "--trust", h, "--log", p.log(), "--source", p.dir}
		}},
		{"--state", func(t *testing.T, h string) []string {
			return []string{"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--state", filepath.Join(testdir.New(t), "no", h)}
		}},
		{"--old", func(t *testing.T, h string) []string {
			return []string{"consistency", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--old", h}
		}},
		{"--now", func(t *testing.T, h string) []string {
			return []string{"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--now", h}
		}},
		{"--op", func(t *testing.T, h string) []string {
			return []string{"proof", "--proof", "x", "--digest", good, "--op", h}
		}},
		{"unknown flag", func(t *testing.T, h string) []string {
			return []string{"checkpoint", "--" + h}
		}},
		{"unknown command", func(t *testing.T, h string) []string { return []string{h} }},
		{"unknown trust subcommand", func(t *testing.T, h string) []string { return []string{"trust", h} }},
		{"checkpoint file with an extension line", func(t *testing.T, h string) []string {
			s := p.signer
			text := p.origin + "\n300\n" + b64root(refMTH(p.all[:300])) + "\n" + h
			note := s.signText(text)
			f := writeTemp(t, testdir.New(t), "cp", note)
			return []string{"checkpoint", "--trust", p.trust, "--log", p.log(), "--note", f}
		}},
		{"digest directory with a file name", func(t *testing.T, h string) []string {
			dir := testdir.New(t)
			name := strings.NewReplacer("\x00", "", "/", "", "\\", "").Replace(h)
			if len(name) > 100 {
				name = name[:100]
			}
			b, _ := os.ReadFile(good)
			if err := os.WriteFile(filepath.Join(dir, "a.json"), b, 0o644); err != nil {
				t.Fatal(err)
			}
			b2, _ := os.ReadFile(fx("digests/d1.json"))
			if err := os.WriteFile(filepath.Join(dir, name+".json"), b2, 0o644); err != nil {
				return []string{"digest-chain", "--digests", dir} // the system refuses such a name
			}
			return []string{"digest-chain", "--digests", dir}
		}},
	}
	for _, sc := range scenarios {
		for i, h := range hostileStrings {
			name := sc.name + "/" + string(rune('a'+i))
			args := sc.args(t, h)
			code, out, errs := run(t, args...)
			checkSafeOutput(t, name, code, out, errs)
			// JSON stays valid JSON.
			jcode, jout, _ := run(t, append(append([]string{}, args...), "--json")...)
			if jout != "" {
				var v any
				if err := json.Unmarshal([]byte(jout), &v); err != nil && !strings.HasPrefix(args[0], "--") {
					// A command that stops at a flag error prints no JSON.
					if jcode != 2 || strings.Contains(sc.name, "unknown") {
						continue
					}
					t.Errorf("%s: the JSON output is not JSON: %v: %q", name, err, clipForLog(jout))
				}
			}
		}
	}
}

func firstProof(t *testing.T) string {
	t.Helper()
	ents, err := os.ReadDir(fx("proofs"))
	if err != nil || len(ents) == 0 {
		t.Fatalf("no proof fixture: %v", err)
	}
	return ents[0].Name()
}

// ---- the output layer is the only place that prints ----

func b64root(h h32) string { return base64.StdEncoding.EncodeToString(h[:]) }

// signText signs a checkpoint text (its first line is the origin, which is
// also the key name) so that a test can put any extension line in it.
func (s signer) signText(text string) string {
	origin := strings.SplitN(text, "\n", 2)[0]
	sum := sha256.Sum256(append(append([]byte(origin), 0x0A, 0x01), s.pub...))
	raw := append(append([]byte{}, sum[:4]...), ed25519.Sign(s.priv, []byte(text+"\n"))...)
	return text + "\n\n\u2014 " + origin + " " + base64.StdEncoding.EncodeToString(raw) + "\n"
}
