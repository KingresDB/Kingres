// SPDX-License-Identifier: PostgreSQL

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureDir = "../../testdata/fixtures"

func fx(rel string) string { return filepath.Join(fixtureDir, rel) }

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(args, &out, &errb, "test")
	return code, out.String(), errb.String()
}

type manifestRow struct {
	Label   string `json:"label"`
	TableID int    `json:"table_id"`
	RowHash string `json:"row_hash"`
}

func manifestRows(t *testing.T) []manifestRow {
	t.Helper()
	b, err := os.ReadFile(fx("manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Rows []manifestRow `json:"rows"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m.Rows
}

func TestProofCommand(t *testing.T) {
	rows := manifestRows(t)
	r := rows[4] // acc5
	good := []string{"proof", "--proof", fx("proofs/" + r.Label + ".json"), "--digest", fx("digests/d5.json")}
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		notOut   string
	}{
		{"verified", good, 0, "VERIFIED", "FAILED"},
		{"verified with binding", append(append([]string{}, good...), "--table-id", "1", "--row-hash", r.RowHash), 0, "VERIFIED", ""},
		{"wrong table", append(append([]string{}, good...), "--table-id", "2"), 1, "FAILED at step binding", "VERIFIED"},
		{"wrong row hash", append(append([]string{}, good...), "--row-hash", strings.Repeat("0", 64)), 1, "FAILED at step binding", "VERIFIED"},
		{"earlier digest", []string{"proof", "--proof", fx("proofs/acc5.json"), "--digest", fx("digests/d0.json")}, 1, "FAILED at step chain-walk", "VERIFIED"},
		{"other chain", []string{"proof", "--proof", fx("proofs/acc5.json"), "--digest", fx("digests/other_chain.json")}, 1, "FAILED at step chain-id", "VERIFIED"},
		{"proof is a digest", []string{"proof", "--proof", fx("digests/d5.json"), "--digest", fx("digests/d5.json")}, 1, "FAILED at step parse", "VERIFIED"},
		{"missing proof flag", []string{"proof", "--digest", fx("digests/d5.json")}, 2, "", "VERIFIED"},
		{"missing file", []string{"proof", "--proof", fx("nope.json"), "--digest", fx("digests/d5.json")}, 2, "", "VERIFIED"},
		{"digest is garbage", []string{"proof", "--proof", fx("proofs/acc5.json"), "--digest", fx("blocks.jsonl")}, 2, "", "VERIFIED"},
		{"bad row hash flag", append(append([]string{}, good...), "--row-hash", "xyz"), 2, "", "VERIFIED"},
		{"bad table flag", append(append([]string{}, good...), "--table-id", "-5"), 2, "", "VERIFIED"},
		{"unknown flag", append(append([]string{}, good...), "--bogus"), 2, "", "VERIFIED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, errs := run(t, tt.args...)
			if code != tt.wantCode {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tt.wantCode, out, errs)
			}
			if tt.wantOut != "" && !strings.Contains(out, tt.wantOut) {
				t.Errorf("stdout %q does not contain %q", out, tt.wantOut)
			}
			if tt.notOut != "" && (strings.Contains(out, tt.notOut) || strings.Contains(errs, tt.notOut)) {
				t.Errorf("output contains %q", tt.notOut)
			}
		})
	}
}

func TestProofCommandJSON(t *testing.T) {
	code, out, _ := run(t, "proof", "--json", "--proof", fx("proofs/acc5.json"), "--digest", fx("digests/d5.json"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res struct {
		Verified bool `json:"verified"`
		Steps    []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !res.Verified || len(res.Steps) < 5 {
		t.Errorf("unexpected result %+v", res)
	}
	code, out, _ = run(t, "proof", "--json", "--proof", fx("proofs/acc5.json"), "--digest", fx("digests/d0.json"))
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	var bad struct {
		Verified bool   `json:"verified"`
		Failed   string `json:"failed_step"`
	}
	if err := json.Unmarshal([]byte(out), &bad); err != nil {
		t.Fatal(err)
	}
	if bad.Verified || bad.Failed != "chain-walk" {
		t.Errorf("unexpected failure result %+v", bad)
	}
}

func TestProofCommandMutatedFile(t *testing.T) {
	b, err := os.ReadFile(fx("proofs/acc5.json"))
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(b, []byte(`"row_hash": "`)) + len(`"row_hash": "`)
	b[i] = 'f' // the first hex character of the row hash was '5'
	p := filepath.Join(t.TempDir(), "mutated.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "proof", "--proof", p, "--digest", fx("digests/d5.json"))
	if code != 1 || strings.Contains(out, "VERIFIED") {
		t.Errorf("exit %d, output %q", code, out)
	}
}

func TestRowHashCommand(t *testing.T) {
	for _, r := range manifestRows(t) {
		code, out, errs := run(t, "row-hash", "--table-id", itoa(r.TableID), "--columns", fx("columns/"+r.Label+".json"))
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", r.Label, code, errs)
		}
		if strings.TrimSpace(out) != r.RowHash {
			t.Errorf("%s: got %q, want %s", r.Label, out, r.RowHash)
		}
	}
	code, out, _ := run(t, "row-hash", "--json", "--encoding", "--table-id", "1", "--columns", fx("columns/acc3.json"))
	if code != 0 || !strings.Contains(out, `"encoding"`) {
		t.Errorf("exit %d, output %q", code, out)
	}
	for _, args := range [][]string{
		{"row-hash"},
		{"row-hash", "--columns", fx("columns/acc3.json")},
		{"row-hash", "--table-id", "1"},
		{"row-hash", "--table-id", "1", "--columns", fx("digests/d0.json")},
		{"row-hash", "--table-id", "99999999999", "--columns", fx("columns/acc3.json")},
	} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestDigestChainCommand(t *testing.T) {
	code, out, _ := run(t, "digest-chain", "--digests", fx("digests"))
	// The directory also holds the digest of another database.
	if code != 1 || !strings.Contains(out, "INCONSISTENT") {
		t.Errorf("directory with a foreign digest: exit %d, output %q", code, out)
	}
	args := []string{"digest-chain"}
	for _, n := range []string{"d0", "d1", "d2", "d3", "d4", "d5"} {
		args = append(args, "--digests", fx("digests/"+n+".json"))
	}
	code, out, _ = run(t, args...)
	if code != 0 || !strings.Contains(out, "CONSISTENT") || !strings.Contains(out, "does not prove") {
		t.Errorf("exit %d, output %q", code, out)
	}
	code, _, _ = run(t, "digest-chain", fx("digests/d0.json"), fx("digests/d0.json"))
	if code != 0 {
		t.Errorf("positional files: exit %d", code)
	}
	if code, _, _ := run(t, "digest-chain"); code != 2 {
		t.Errorf("no files: exit %d, want 2", code)
	}
	if code, _, _ := run(t, "digest-chain", "--digests", fx("nope")); code != 2 {
		t.Errorf("missing path: exit %d, want 2", code)
	}
	if code, _, _ := run(t, "digest-chain", "--digests", fx("digests/d0.json")); code != 1 {
		t.Errorf("a single digest: exit %d, want 1", code)
	}
}

func TestBlocksCommand(t *testing.T) {
	code, out, errs := run(t, "blocks", "--blocks", fx("blocks.jsonl"), "--digest", fx("digests/d5.json"))
	if code != 0 || !strings.Contains(out, "VERIFIED") {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	if code, out, _ := run(t, "blocks", "--blocks", fx("blocks.jsonl"), "--digest", fx("digests/other_chain.json")); code != 1 || strings.Contains(out, "VERIFIED") {
		t.Errorf("other chain: exit %d, %s", code, out)
	}
	if code, _, _ := run(t, "blocks", "--blocks", fx("blocks.jsonl")); code != 2 {
		t.Errorf("no chain id: exit %d, want 2", code)
	}
	cid := strings.Repeat("0", 64)
	if code, _, _ := run(t, "blocks", "--blocks", fx("blocks.jsonl"), "--chain-id", cid); code != 1 {
		t.Errorf("wrong chain id: exit %d, want 1", code)
	}
	if code, _, _ := run(t, "blocks", "--blocks", fx("blocks.jsonl"), "--digest", fx("digests/d5.json"), "--chain-id", cid); code != 2 {
		t.Errorf("conflicting chain id: exit %d, want 2", code)
	}
	// A block file with one changed byte.
	b, err := os.ReadFile(fx("blocks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	b[bytes.Index(b, []byte(`"tx_count" : 2`))+len(`"tx_count" : `)] = '3'
	p := filepath.Join(t.TempDir(), "blocks.jsonl")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, "blocks", "--blocks", p, "--digest", fx("digests/d5.json"), "--json"); code != 1 || strings.Contains(out, `"verified": true`) {
		t.Errorf("mutated blocks: exit %d, %s", code, out)
	}
}

func TestVersionAndHelp(t *testing.T) {
	if code, out, _ := run(t, "version"); code != 0 || !strings.Contains(out, "kingres-verify test") {
		t.Errorf("version: %d %q", code, out)
	}
	for _, c := range []string{"proof", "row-hash", "digest-chain", "blocks"} {
		code, out, _ := run(t, c, "--help")
		if code != 0 || !strings.Contains(out, "Usage: kingres-verify "+c) || !strings.Contains(out, "Exit status") {
			t.Errorf("%s --help: %d %q", c, code, out)
		}
	}
	if code, out, _ := run(t, "--help"); code != 0 || !strings.Contains(out, "Commands:") {
		t.Errorf("--help: %d %q", code, out)
	}
	if code, _, _ := run(t); code != 2 {
		t.Errorf("no arguments: exit %d, want 2", code)
	}
	if code, _, errs := run(t, "frobnicate"); code != 2 || !strings.Contains(errs, "unknown command") {
		t.Errorf("unknown command: %d %q", code, errs)
	}
}
