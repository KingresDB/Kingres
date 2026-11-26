// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package crosscheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/cli"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// The publication of a real two-group database, as kingresd served it over HTTP (the cluster test
// TestLedgerPublicationOfADatabaseOnTwoGroups of kingres/control-plane, with KINGRES_PUBLICATION_FIXTURES
// set): the checkpoints, tiles and entry bundles of two chain logs and of the aggregate log, and the
// trust.json that the control plane gave out. The verifier reads it from the documents, never from the
// code of kingresd, so what it accepts here is what a real publication is.

const pubDir = "../../testdata/publication"

func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Run(args, &out, &errb, "test")
	return code, out.String() + errb.String()
}

type layout struct {
	chains []string // "chain:<id>:e<N>"
	aggs   []string
}

func discover(t *testing.T, dir string) layout {
	t.Helper()
	var l layout
	re := regexp.MustCompile(`^c/([0-9a-f]{64})/e([0-9]+)/checkpoint$`)
	rea := regexp.MustCompile(`^a/([0-9a-f]{64})/checkpoint$`)
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if m := re.FindStringSubmatch(rel); m != nil {
			l.chains = append(l.chains, "chain:"+m[1]+":e"+m[2])
		}
		if m := rea.FindStringSubmatch(rel); m != nil {
			l.aggs = append(l.aggs, m[1])
		}
		return nil
	})
	return l
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func prefixOf(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tf struct {
		OriginPrefix string `json:"origin_prefix"`
	}
	if err := json.Unmarshal(b, &tf); err != nil {
		t.Fatal(err)
	}
	return tf.OriginPrefix
}

const now = "2030-01-01T00:00:00Z"

func TestKingresdPublicationVerifiesLogByLog(t *testing.T) {
	l := discover(t, pubDir)
	if len(l.chains) != 2 || len(l.aggs) != 1 {
		t.Fatalf("the fixture holds %d chain log(s) and %d aggregate log(s), expected 2 and 1", len(l.chains), len(l.aggs))
	}
	trustFile := filepath.Join(pubDir, "trust.json")
	for _, c := range l.chains {
		if code, out := runCLI(t, "checkpoint", "--trust", trustFile, "--source", pubDir, "--log", c, "--now", now); code != 0 || !strings.Contains(out, "VERIFIED") {
			t.Fatalf("%s: %d\n%s", c, code, out)
		}
	}
	code, out := runCLI(t, "aggregate", "--trust", trustFile, "--source", pubDir, "--aggregate-id", l.aggs[0], "--now", now)
	if code != 0 || !strings.Contains(out, "2 member(s)") {
		t.Fatalf("aggregate: %d\n%s", code, out)
	}
}

// bundleOf copies the fixture and writes the bundle.json that a bundle of it would have.
func bundleOf(t *testing.T) (dir string, l layout) {
	t.Helper()
	l = discover(t, pubDir)
	dir = testdir.New(t)
	copyTree(t, pubDir, dir)
	prefix := prefixOf(t, dir)
	var logs []map[string]any
	for _, c := range l.chains {
		parts := strings.Split(c, ":")
		var e int
		_, _ = fmt.Sscan(parts[2][1:], &e)
		size := checkpointSize(t, filepath.Join(dir, "c", parts[1], parts[2], "checkpoint"))
		logs = append(logs, map[string]any{"kind": "chain", "origin": prefix + "/c/" + parts[1] + "/" + parts[2], "chain_id": parts[1], "epoch": e, "checkpoint": "c/" + parts[1] + "/" + parts[2] + "/checkpoint", "size": size})
	}
	logs = append(logs, map[string]any{"kind": "aggregate", "origin": prefix + "/a/" + l.aggs[0], "checkpoint": "a/" + l.aggs[0] + "/checkpoint", "size": checkpointSize(t, filepath.Join(dir, "a", l.aggs[0], "checkpoint"))})
	doc, _ := json.Marshal(map[string]any{"format": 1, "created_at": "2026-11-19T10:00:00Z", "cluster_id": "00", "origin_prefix": prefix, "logs": logs, "anchors": []string{}, "proofs": []string{}})
	if err := os.WriteFile(filepath.Join(dir, "bundle.json"), doc, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, l
}

func TestKingresdPublicationVerifiesAsABundle(t *testing.T) {
	dir, _ := bundleOf(t)
	trustFile := filepath.Join(dir, "trust.json")
	code, out := runCLI(t, "bundle", "verify", "--trust", trustFile, "--now", now, dir)
	if code != 0 || !strings.Contains(out, "2 chain log(s), 1 aggregate log(s)") {
		t.Fatalf("bundle verify: %d\n%s", code, out)
	}
}

func checkpointSize(t *testing.T, p string) int {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if _, err := fmt.Sscan(strings.Split(string(b), "\n")[1], &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every byte of every file of the real publication is mutated, one at a time, and the bundle never verifies.
func TestKingresdPublicationRejectsEveryByteOfEveryFile(t *testing.T) {
	dir, _ := bundleOf(t)
	trustFile := filepath.Join(dir, "trust.json")
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) != "trust.json" && filepath.Base(p) != "bundle.json" {
			files = append(files, p)
		}
		return nil
	})
	// a deterministic sample by default (every byte of a file of at most 2,048 bytes, the first and last 64 of every file, every 16th
	// byte of the rest); KINGRES_VERIFY_FULL_SWEEP=1 flips every byte (see internal/cli/sweep_test.go)
	full := os.Getenv("KINGRES_VERIFY_FULL_SWEEP") == "1"
	total := 0
	for _, p := range files {
		orig, _ := os.ReadFile(p)
		for i := range orig {
			if !full && len(orig) > 2048 && i%16 != 0 && i >= 64 && i < len(orig)-64 {
				continue
			}
			mut := append([]byte{}, orig...)
			mut[i] ^= 0x01
			_ = os.WriteFile(p, mut, 0o644)
			code, _ := runCLI(t, "bundle", "verify", "--trust", trustFile, "--now", now, dir)
			total++
			if code == 0 {
				_ = os.WriteFile(p, orig, 0o644)
				rel, _ := filepath.Rel(dir, p)
				t.Fatalf("byte %d of %s flipped and the bundle still verifies", i, rel)
			}
		}
		_ = os.WriteFile(p, orig, 0o644)
	}
	t.Logf("%d mutations of %d files", total, len(files))
}
