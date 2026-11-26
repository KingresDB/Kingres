// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/release"
)

func do(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var o, e bytes.Buffer
	c := run(args, &o, &e)
	return c, o.String(), e.String()
}

// The key of this test is made here, in a directory the test removes.
func TestKeygenSumsSignRoundTrip(t *testing.T) {
	dir := t.TempDir()
	kf := filepath.Join(dir, "release.key")
	code, out, errs := do(t, "keygen", "--out", kf)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	var vkey string
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "key string:  "); ok {
			vkey = v
		}
	}
	if vkey == "" || strings.Contains(out, "seed") {
		t.Fatalf("output: %q", out)
	}
	if c, _, _ := do(t, "keygen", "--out", kf); c == 0 {
		t.Error("keygen overwrote a key file")
	}
	a := filepath.Join(dir, "kingres-verify-linux-amd64")
	if err := os.WriteFile(a, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	sums, note := filepath.Join(dir, "SHA256SUMS"), filepath.Join(dir, "SHA256SUMS.note")
	if c, _, e := do(t, "sums", "--out", sums, a); c != 0 {
		t.Fatal(e)
	}
	if c, _, e := do(t, "sign", "--key-file", kf, "--sums", sums, "--out", note); c != 0 {
		t.Fatal(e)
	}
	if c, _, _ := do(t, "sign", "--key-file", kf, "--sums", sums, "--out", note); c == 0 {
		t.Error("sign overwrote a note")
	}
	key, err := release.ParseKey(vkey)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := os.ReadFile(sums)
	n, _ := os.ReadFile(note)
	if err := release.VerifyNote(s, n, key); err != nil {
		t.Fatal(err)
	}
	if c, o, _ := do(t, "pub", "--key-file", kf); c != 0 || !strings.Contains(o, vkey) {
		t.Errorf("pub: %d %s", c, o)
	}
}

func TestKeyFileMustBePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not permissions here")
	}
	dir := t.TempDir()
	kf := filepath.Join(dir, "k")
	if c, _, _ := do(t, "keygen", "--out", kf); c != 0 {
		t.Fatal("keygen")
	}
	if err := os.Chmod(kf, 0o644); err != nil {
		t.Fatal(err)
	}
	if c, _, e := do(t, "pub", "--key-file", kf); c == 0 || !strings.Contains(e, "0600") {
		t.Errorf("%d %s", c, e)
	}
}

func TestBadInput(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad")
	_ = os.WriteFile(bad, []byte("not a key\n"), 0o600)
	for _, a := range [][]string{
		{}, {"nonsense"}, {"keygen"}, {"pub"}, {"pub", "--key-file", bad}, {"sums", "--out", filepath.Join(dir, "s")},
		{"sign", "--key-file", bad, "--sums", bad, "--out", filepath.Join(dir, "n")},
		{"sums", "--out", filepath.Join(dir, "s2"), filepath.Join(dir, "missing")},
	} {
		if c, _, _ := do(t, a...); c != 2 {
			t.Errorf("%v: status %d", a, c)
		}
	}
}
