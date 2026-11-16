// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExitStatusFollowsTheRule walks the error paths of checkpoint and
// consistency and asserts the status the rule of correction 16 (and 18) gives:
//
//	2  a thing the auditor named or holds cannot be used (command line, trust file,
//	   --source, --state, --old, and a publication file the account cannot read)
//	3  a thing the publication should hold is absent
//	1  a thing the publication holds is in a form it must not have, or the thing
//	   under test (--note, --new, the checkpoint of the source) does not verify
func TestExitStatusFollowsTheRule(t *testing.T) {
	type sc struct {
		name string
		want int
		args func(t *testing.T) []string
	}
	pub := func(t *testing.T) *publication { return newPublication(t, 300, 700) }
	cp := func(p *publication, more ...string) []string {
		return append([]string{"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir}, more...)
	}
	co := func(p *publication, more ...string) []string {
		return append([]string{"consistency", "--trust", p.trust, "--log", p.log(), "--source", p.dir}, more...)
	}
	oldNote := func(p *publication) string { return filepath.Join(p.logDir, "old.note") }
	scenarios := []sc{
		// ---- 0 ----
		{"checkpoint ok", 0, func(t *testing.T) []string { return cp(pub(t)) }},
		{"consistency ok", 0, func(t *testing.T) []string { p := pub(t); return co(p, "--old", oldNote(p)) }},
		// ---- 2: the auditor's own things ----
		{"no --trust", 2, func(t *testing.T) []string {
			p := pub(t)
			return []string{"checkpoint", "--log", p.log(), "--source", p.dir}
		}},
		{"no --log", 2, func(t *testing.T) []string {
			p := pub(t)
			return []string{"checkpoint", "--trust", p.trust, "--source", p.dir}
		}},
		{"bad --log", 2, func(t *testing.T) []string { p := pub(t); return cp(p, "--log", "x") }},
		{"trust missing", 2, func(t *testing.T) []string { p := pub(t); return cp(p, "--trust", filepath.Join(t.TempDir(), "no")) }},
		{"trust garbage", 2, func(t *testing.T) []string {
			p := pub(t)
			return cp(p, "--trust", writeTemp(t, t.TempDir(), "t", "garbage"))
		}},
		{"trust is a directory", 2, func(t *testing.T) []string { p := pub(t); return cp(p, "--trust", t.TempDir()) }},
		{"source missing", 2, func(t *testing.T) []string { p := pub(t); return cp(p, "--source", filepath.Join(t.TempDir(), "no")) }},
		{"source is a file", 2, func(t *testing.T) []string { p := pub(t); return cp(p, "--source", p.trust) }},
		{"--note missing", 2, func(t *testing.T) []string { p := pub(t); return cp(p, "--note", filepath.Join(t.TempDir(), "no")) }},
		{"--old missing", 2, func(t *testing.T) []string { p := pub(t); return co(p, "--old", filepath.Join(t.TempDir(), "no")) }},
		{"--old not a note", 2, func(t *testing.T) []string { p := pub(t); return co(p, "--old", p.trust) }},
		{"--old signed by an unknown key", 2, func(t *testing.T) []string {
			p := pub(t)
			return co(p, "--old", writeNote(t, newSigner(7).note(p.origin, 300, refMTH(p.all[:300]))))
		}},
		{"--old of another log", 2, func(t *testing.T) []string {
			p := pub(t)
			return co(p, "--old", writeNote(t, p.signer.note(testPrefix+"/c/"+testChain+"/e9", 300, refMTH(p.all[:300]))))
		}},
		{"--old bad size:root", 2, func(t *testing.T) []string { p := pub(t); return co(p, "--old", "300:!!") }},
		{"--old signed beyond a key limit", 2, func(t *testing.T) []string {
			p := pub(t)
			return []string{"consistency", "--trust", trustWith(t, p, `,"max_tree_size":299`), "--log", p.log(), "--source", p.dir, "--old", oldNote(p)}
		}},
		{"--state garbage", 2, func(t *testing.T) []string {
			return cp(pub(t), "--state", writeTemp(t, t.TempDir(), "s", "garbage"))
		}},
		{"--state is a directory", 2, func(t *testing.T) []string { return cp(pub(t), "--state", t.TempDir()) }},
		{"--state in a missing directory", 2, func(t *testing.T) []string {
			return cp(pub(t), "--state", filepath.Join(t.TempDir(), "no", "dir", "s"))
		}},
		{"--now bad", 2, func(t *testing.T) []string { return cp(pub(t), "--now", "yesterday") }},
		{"--now empty", 2, func(t *testing.T) []string { return cp(pub(t), "--now", "") }},
		{"--now with a comma", 2, func(t *testing.T) []string { return cp(pub(t), "--now", "2026-01-01T00:00:00,5Z") }},
		{"no key usable at --now", 2, func(t *testing.T) []string {
			p := pub(t)
			return cp(p, "--trust", trustWith(t, p, `,"revoked":true`))
		}},
		{"unknown flag", 2, func(t *testing.T) []string { return []string{"checkpoint", "--nope"} }},
		{"two epochs without :e", 2, func(t *testing.T) []string {
			p := pub(t)
			if err := os.MkdirAll(filepath.Join(p.dir, "c", testChain, "e2"), 0o755); err != nil {
				t.Fatal(err)
			}
			return cp(p, "--log", "chain:"+testChain)
		}},
		// ---- 3: the publication lacks it ----
		{"no checkpoint in the source", 3, func(t *testing.T) []string {
			p := pub(t)
			_ = os.Remove(filepath.Join(p.logDir, "checkpoint"))
			return cp(p)
		}},
		{"no tile for the proof", 3, func(t *testing.T) []string {
			p := pub(t)
			_ = os.RemoveAll(filepath.Join(p.logDir, "tile"))
			return co(p, "--old", oldNote(p))
		}},
		{"chain directory without an epoch", 3, func(t *testing.T) []string {
			p := pub(t)
			_ = os.RemoveAll(p.logDir)
			return cp(p, "--log", "chain:"+testChain)
		}},
		{"no chain directory", 3, func(t *testing.T) []string {
			p := pub(t)
			_ = os.RemoveAll(filepath.Join(p.dir, "c"))
			return cp(p, "--log", "chain:"+testChain)
		}},
		{"epoch given, directory absent", 3, func(t *testing.T) []string {
			p := pub(t)
			_ = os.RemoveAll(p.logDir)
			return cp(p)
		}},
		{"state advance without tiles", 3, func(t *testing.T) []string {
			p := pub(t)
			st := filepath.Join(t.TempDir(), "s")
			if code, out, _ := run(t, cp(p, "--note", oldNote(p), "--state", st)...); code != 0 {
				t.Fatal(out)
			}
			_ = os.RemoveAll(filepath.Join(p.logDir, "tile"))
			return cp(p, "--state", st)
		}},
		// ---- 1: present in a form it must not have, or fails ----
		{"checkpoint forged", 1, func(t *testing.T) []string {
			p := pub(t)
			p.write(t, "checkpoint", newSigner(7).note(p.origin, 700, refMTH(p.all)))
			return cp(p)
		}},
		{"checkpoint of another log", 1, func(t *testing.T) []string {
			p := pub(t)
			p.write(t, "checkpoint", p.signer.note(testPrefix+"/c/"+testChain+"/e9", 700, refMTH(p.all)))
			return cp(p)
		}},
		{"checkpoint is a directory", 1, func(t *testing.T) []string {
			p := pub(t)
			_ = os.Remove(filepath.Join(p.logDir, "checkpoint"))
			_ = os.Mkdir(filepath.Join(p.logDir, "checkpoint"), 0o755)
			return cp(p)
		}},
		{"checkpoint garbage", 1, func(t *testing.T) []string {
			p := pub(t)
			p.write(t, "checkpoint", "garbage\n")
			return cp(p)
		}},
		{"--note garbage", 1, func(t *testing.T) []string { return cp(pub(t), "--note", writeTemp(t, t.TempDir(), "n", "garbage\n")) }},
		{"--new garbage", 1, func(t *testing.T) []string {
			p := pub(t)
			return co(p, "--old", oldNote(p), "--new", writeTemp(t, t.TempDir(), "n", "garbage\n"))
		}},
		{"tile of the wrong size", 1, func(t *testing.T) []string {
			p := pub(t)
			_ = filepath.Walk(filepath.Join(p.logDir, "tile"), func(f string, fi os.FileInfo, err error) error {
				if err == nil && !fi.IsDir() {
					_ = os.WriteFile(f, []byte("short"), 0o644)
				}
				return nil
			})
			return co(p, "--old", oldNote(p))
		}},
		{"wrong old root", 1, func(t *testing.T) []string {
			p := pub(t)
			return co(p, "--old", "300:"+b64root(sha([]byte("x"))))
		}},
		{"--min-size above the size", 1, func(t *testing.T) []string { return cp(pub(t), "--min-size", "701") }},
		{"size beyond a key limit", 1, func(t *testing.T) []string {
			p := pub(t)
			return []string{"checkpoint", "--trust", trustWith(t, p, `,"max_tree_size":699`), "--log", p.log(), "--source", p.dir}
		}},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			args := s.args(t)
			code, out, errs := run(t, args...)
			if code != s.want {
				t.Errorf("status %d, want %d\n%s%s", code, s.want, out, errs)
			}
			if code != 0 && (strings.Contains(out, "VERIFIED:") || strings.Contains(out, "CONSISTENT:")) {
				t.Errorf("a success verdict with status %d", code)
			}
		})
	}
}
