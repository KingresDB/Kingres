// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestBlockEntryAgainstTheHeadersOfARealServer reads the block header export that
// the verifier keeps as a fixture (made by a real kingres_ledger server) and
// checks that the leaf codec reproduces every block hash, that the blocks link,
// and that a log built from them has proofs that verify.
func TestBlockEntryAgainstTheHeadersOfARealServer(t *testing.T) {
	path := filepath.Join("..", "..", "..", "verifier", "testdata", "fixtures", "blocks.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("the verifier fixtures are not here: %v", err)
	}
	defer f.Close()
	var blocks []BlockEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var row struct {
			Height       uint64 `json:"height"`
			HashVersion  uint16 `json:"hash_version"`
			PrevHash     string `json:"prev_hash"`
			TxMerkleRoot string `json:"tx_merkle_root"`
			TxCount      uint32 `json:"tx_count"`
			ClosedAtUS   int64  `json:"closed_at_us"`
			BlockHash    string `json:"block_hash"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		b := BlockEntry{HashVersion: row.HashVersion, Height: row.Height, TxCount: row.TxCount, ClosedAtUS: row.ClosedAtUS}
		for dst, src := range map[*[32]byte]string{&b.PrevHash: row.PrevHash, &b.TxMerkleRoot: row.TxMerkleRoot} {
			raw, err := hex.DecodeString(src)
			if err != nil || len(raw) != 32 {
				t.Fatal("fixture hash")
			}
			copy(dst[:], raw)
		}
		got, err := b.BlockHash()
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(got[:]) != row.BlockHash {
			t.Fatalf("block %d: the hash of the leaf data is %x, the server says %s", row.Height, got, row.BlockHash)
		}
		if n := len(blocks); n > 0 {
			if err := CheckLink(blocks[n-1], b); err != nil {
				t.Fatal(err)
			}
		}
		blocks = append(blocks, b)
	}
	if len(blocks) < 5 {
		t.Fatalf("%d blocks in the fixture", len(blocks))
	}
	store := NewMemStore()
	l, _ := Create(bg, testOrigin(1), store)
	var es [][]byte
	for _, b := range blocks {
		d, _ := b.Marshal()
		es = append(es, d)
	}
	if _, err := l.Append(bg, es); err != nil {
		t.Fatal(err)
	}
	r, _ := NewReader(bg, store, uint64(len(blocks)))
	for i := range blocks {
		p, err := r.InclusionProof(uint64(i))
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyInclusion(LeafHash(es[i]), uint64(i), uint64(len(blocks)), p, l.Root()); err != nil {
			t.Fatal(err)
		}
	}
}
