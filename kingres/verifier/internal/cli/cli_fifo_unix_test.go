// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package cli

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func fifo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("cannot make a named pipe here: %v", err)
	}
}

// L7-01: every file the verifier opens may be a named pipe; none of them may
// hang it, and each ends with the status of its kind: 1 for what the
// publication holds, 2 for what the auditor named.
func TestNamedPipesNeverHang(t *testing.T) {
	p := newPublication(t, 300, 700)
	old := filepath.Join(p.logDir, "old.note")
	pipe := filepath.Join(t.TempDir(), "pipe")
	fifo(t, pipe)
	dir := []string{"--trust", p.trust, "--log", p.log(), "--source", p.dir}

	// In the publication: a tile, the checkpoint, the epoch directory.
	tile := filepath.Join(p.logDir, "tile", "0", "001")
	_ = os.Remove(tile)
	fifo(t, tile)
	if code, out, _ := runWithin(t, append([]string{"consistency", "--old", old}, dir...)...); code != 1 || strings.Contains(out, "VERIFIED") || !strings.Contains(out, "named pipe") {
		t.Errorf("pipe as tile: %d %s", code, out)
	}
	cp := filepath.Join(p.logDir, "checkpoint")
	saved, _ := os.ReadFile(cp)
	fifo(t, cp)
	if code, out, _ := runWithin(t, append([]string{"checkpoint"}, dir...)...); code != 1 || !strings.Contains(out, "named pipe") {
		t.Errorf("pipe as checkpoint: %d %s", code, out)
	}
	if code, out, _ := runWithin(t, append([]string{"consistency", "--old", old}, dir...)...); code != 1 || strings.Contains(out, "VERIFIED") {
		t.Errorf("pipe as the new checkpoint of consistency: %d %s", code, out)
	}
	_ = os.Remove(cp)
	_ = os.WriteFile(cp, saved, 0o644)
	other := filepath.Join(p.dir, "c", testChain, "e2")
	fifo(t, other)
	if code, out, _ := runWithin(t, "checkpoint", "--trust", p.trust, "--log", "chain:"+testChain, "--source", p.dir); code != 1 || strings.Contains(out, "VERIFIED") {
		t.Errorf("pipe named like an epoch directory: %d %s", code, out)
	}
	_ = os.Remove(other)

	// Named by the auditor: status 2.
	for name, args := range map[string][]string{
		"--note":              {"checkpoint", "--trust", p.trust, "--log", p.log(), "--note", pipe},
		"--trust":             {"checkpoint", "--trust", pipe, "--log", p.log(), "--source", p.dir},
		"--old":               {"consistency", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--old", pipe},
		"--new":               {"consistency", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--old", "0:" + base64.StdEncoding.EncodeToString(emptyRoot()), "--new", pipe},
		"--state":             {"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--state", pipe},
		"--source":            {"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", pipe},
		"trust show":          {"trust", "show", "--trust", pipe},
		"trust init key file": {"trust", "init", "--origin-prefix", testPrefix, "--public-key-file", pipe, "--fingerprint", p.signer.fp(), "--out", filepath.Join(t.TempDir(), "t.json")},
		"proof":               {"proof", "--proof", pipe, "--digest", fx("digests/d8.json")},
		"proof digest":        {"proof", "--proof", fx("proofs/itm1_v1_d.json"), "--digest", pipe},
		"row-hash":            {"row-hash", "--table-id", "1", "--columns", pipe},
		"digest-chain":        {"digest-chain", "--digests", pipe},
		"blocks":              {"blocks", "--blocks", pipe, "--chain-id", strings.Repeat("ab", 32)},
	} {
		if code, _, errs := runWithin(t, args...); code != 2 || !strings.Contains(errs, "regular file") && !strings.Contains(errs, "directory") {
			t.Errorf("%s: %d %s", name, code, errs)
		}
	}
	// Writing: trust init never opens an existing name, so a pipe or a link at the
	// path is refused at once and nothing is written through it.
	if code, _, errs := runWithin(t, "trust", "init", "--origin-prefix", testPrefix, "--public-key", base64.StdEncoding.EncodeToString(p.signer.pub), "--fingerprint", p.signer.fp(), "--out", pipe); code != 2 || !strings.Contains(errs, "not writing") {
		t.Errorf("trust init --out a pipe: %d %s", code, errs)
	}
	// A pipe inside a directory of digests, named like a digest file.
	digests := t.TempDir()
	fifo(t, filepath.Join(digests, "x.json"))
	if code, _, errs := runWithin(t, "digest-chain", "--digests", digests); code != 2 || !strings.Contains(errs, "regular file") {
		t.Errorf("pipe in a digest directory: %d %s", code, errs)
	}
}

// A device that never ends is refused as well.
func TestDeviceAsNamedFile(t *testing.T) {
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("no /dev/zero")
	}
	p := newPublication(t, 300, 700)
	if code, _, errs := runWithin(t, "checkpoint", "--trust", p.trust, "--log", p.log(), "--note", "/dev/zero"); code != 2 || !strings.Contains(errs, "regular file") {
		t.Errorf("/dev/zero: %d %s", code, errs)
	}
}
