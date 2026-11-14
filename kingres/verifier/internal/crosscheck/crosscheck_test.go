// SPDX-License-Identifier: PostgreSQL

package crosscheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/chain"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/proof"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/rowfile"
)

const fixtureDir = "../../testdata/fixtures"

type manifest struct {
	Digests []struct {
		File   string `json:"file"`
		Height uint64 `json:"height"`
	} `json:"digests"`
	Rows []struct {
		Label    string `json:"label"`
		Relation string `json:"relation"`
		TableID  uint32 `json:"table_id"`
		RowHash  string `json:"row_hash"`
		Proof    string `json:"proof"`
		Columns  string `json:"columns"`
	} `json:"rows"`
	Blocks           string `json:"blocks"`
	OtherChainDigest string `json:"other_chain_digest"`
}

func read(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func load(t testing.TB) manifest {
	t.Helper()
	var m manifest
	if err := json.Unmarshal(read(t, "manifest.json"), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Rows) < 10 || len(m.Digests) < 5 {
		t.Fatalf("manifest is too small: %d rows, %d digests", len(m.Rows), len(m.Digests))
	}
	return m
}

func digest(t testing.TB, rel string) chain.Digest {
	t.Helper()
	d, err := chain.ParseDigest(read(t, rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return d
}

// Every proof the server made verifies against the digest taken after it,
// and against every later digest, fails against every earlier digest (when
// the digest is older than the proof's block) and against a digest of another
// database.
func TestServerProofsAgainstDigests(t *testing.T) {
	m := load(t)
	other := digest(t, m.OtherChainDigest)
	latest := digest(t, m.Digests[len(m.Digests)-1].File)
	for _, row := range m.Rows {
		p, err := proof.Parse(read(t, row.Proof))
		if err != nil {
			t.Fatalf("%s: %v", row.Label, err)
		}
		want, err := format.DecodeHash(row.RowHash)
		if err != nil {
			t.Fatal(err)
		}
		exp := proof.Expect{TableID: &row.TableID, RowHash: &want}
		okSomewhere := false
		for _, md := range m.Digests {
			d := digest(t, md.File)
			_, err := proof.Verify(p, d, exp)
			if md.Height >= p.Block.Height {
				if err != nil {
					t.Errorf("%s (block %d) against digest at height %d: %v", row.Label, p.Block.Height, md.Height, err)
				}
				okSomewhere = true
			} else if !proof.IsStep(err, proof.StepChainWalk) {
				t.Errorf("%s (block %d) against the earlier digest at height %d: err = %v, want a chain-walk failure", row.Label, p.Block.Height, md.Height, err)
			}
		}
		if !okSomewhere {
			t.Errorf("%s: no digest at or after block %d", row.Label, p.Block.Height)
		}
		if _, err := proof.Verify(p, other, exp); !proof.IsStep(err, proof.StepChainID) {
			t.Errorf("%s against the digest of another database: err = %v, want a chain-id failure", row.Label, err)
		}
		if p.ChainID != latest.ChainID {
			t.Errorf("%s: proof chain id differs from the digests'", row.Label)
		}
		if p.TableID != row.TableID || p.RowHash != want {
			t.Errorf("%s: table_id/row_hash of the proof differ from the server's answers", row.Label)
		}
	}
}

// The proofs cover the cases the task asks for: rows in different blocks, and
// a row whose block is older than the digest.
func TestFixtureCoverage(t *testing.T) {
	m := load(t)
	latest := digest(t, m.Digests[len(m.Digests)-1].File)
	blocks := map[uint64]bool{}
	older := 0
	for _, row := range m.Rows {
		p, err := proof.Parse(read(t, row.Proof))
		if err != nil {
			t.Fatal(err)
		}
		blocks[p.Block.Height] = true
		if p.Block.Height < latest.Height {
			older++
		}
	}
	if len(blocks) < 5 {
		t.Errorf("proofs cover %d blocks, want at least 5", len(blocks))
	}
	if older == 0 {
		t.Error("no proof for a row in a block older than the latest digest")
	}
}

// Every row hash recomputed from the binary send forms equals the server's.
func TestRowHashesMatchServer(t *testing.T) {
	m := load(t)
	n := 0
	for _, row := range m.Rows {
		if row.Columns == "" {
			continue
		}
		cols, err := rowfile.Parse(read(t, row.Columns))
		if err != nil {
			t.Fatalf("%s: %v", row.Label, err)
		}
		got, err := format.RowHash(row.TableID, cols)
		if err != nil {
			t.Fatalf("%s: %v", row.Label, err)
		}
		if format.EncodeHex(got[:]) != row.RowHash {
			t.Errorf("%s: recomputed row hash %x, server says %s", row.Label, got, row.RowHash)
		}
		n++
	}
	if n < 10 {
		t.Errorf("only %d rows had column descriptions", n)
	}
}

// The recomputed block chain equals the server's digests, and the headers
// inside the proofs equal the exported headers.
func TestBlockChainMatchesDigests(t *testing.T) {
	m := load(t)
	headers, err := chain.ParseBlocks(read(t, m.Blocks))
	if err != nil {
		t.Fatal(err)
	}
	latest := digest(t, m.Digests[len(m.Digests)-1].File)
	if err := chain.VerifyBlocks(headers, latest.ChainID); err != nil {
		t.Fatal(err)
	}
	for _, md := range m.Digests {
		d := digest(t, md.File)
		if err := chain.CheckDigestOnBlocks(headers, d); err != nil {
			t.Errorf("digest at height %d: %v", md.Height, err)
		}
		if d.Height != md.Height {
			t.Errorf("%s: height %d, manifest says %d", md.File, d.Height, md.Height)
		}
	}
	for _, row := range m.Rows {
		p, err := proof.Parse(read(t, row.Proof))
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range append([]format.Header{p.Block}, p.Chain...) {
			if int(h.Height) >= len(headers) || headers[h.Height] != h {
				t.Errorf("%s: header of height %d differs from the block export", row.Label, h.Height)
			}
		}
	}
	// A wrong chain id breaks block 0.
	wrong := latest.ChainID
	wrong[0] ^= 1
	if err := chain.VerifyBlocks(headers, wrong); err == nil {
		t.Error("blocks verified with a wrong chain id")
	}
	// The digest of the other database is not on this chain.
	other := digest(t, m.OtherChainDigest)
	if err := chain.CheckDigestOnBlocks(headers, other); err == nil {
		t.Error("the digest of another database reaches this chain")
	}
}

// Digests taken at different times are mutually consistent.
func TestDigestSetConsistent(t *testing.T) {
	m := load(t)
	var ds []chain.NamedDigest
	for _, md := range m.Digests {
		ds = append(ds, chain.NamedDigest{Name: md.File, Digest: digest(t, md.File)})
	}
	if problems := chain.CheckDigestSet(ds); len(problems) != 0 {
		t.Errorf("problems: %v", problems)
	}
	ds = append(ds, chain.NamedDigest{Name: "other", Digest: digest(t, m.OtherChainDigest)})
	if problems := chain.CheckDigestSet(ds); len(problems) == 0 {
		t.Error("a digest of another chain was not noticed")
	}
}
