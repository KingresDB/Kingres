// SPDX-License-Identifier: PostgreSQL

package chain

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
)

const fixtureDir = "../../testdata/fixtures"

func fixture(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const goodDigest = `{"database": "db", "chain_id": "19dda7df2a32e701554852ce819d624ce85bcf244a0ecb3f82521f2ad316e0c9",
 "hash_version": 1, "height": 12, "block_hash": "097174923036d2dc9058455b9944e8ffde81d07333fd27230754297653f46bf6",
 "block_closed_at": "2026-11-14T10:20:30.123456Z", "generated_at": "2026-11-14T10:21:00.000000Z"}`

func TestParseDigest(t *testing.T) {
	d, err := ParseDigest([]byte(goodDigest))
	if err != nil {
		t.Fatal(err)
	}
	if d.Height != 12 || d.Database != "db" || !d.HasClosedAt || !d.HasGeneratedAt {
		t.Errorf("unexpected digest %+v", d)
	}
	if got := FormatUS(d.ClosedAtUS); got != "2026-11-14T10:20:30.123456Z" {
		t.Errorf("FormatUS(block_closed_at) = %s", got)
	}
}

func TestParseDigestRejects(t *testing.T) {
	rep := func(from, to string) string { return strings.Replace(goodDigest, from, to, 1) }
	tests := []struct {
		name string
		in   string
	}{
		{"not an object", `[]`},
		{"missing chain_id", rep(`"chain_id"`, `"chain_idx"`)},
		{"missing hash_version", rep(`"hash_version"`, `"hash_versionx"`)},
		{"missing height", rep(`"height"`, `"heightx"`)},
		{"missing block_hash", rep(`"block_hash"`, `"block_hashx"`)},
		{"chain_id number", rep(`"chain_id": "19dd`, `"chain_id": 1, "x": "19dd`)},
		{"short chain_id", rep(`"19dda7df`, `"19dda7d`)},
		{"upper case block_hash", rep(`"097174`, `"097174`[:1]+`A97174`)},
		{"height string", rep(`"height": 12`, `"height": "12"`)},
		{"height fraction", rep(`"height": 12`, `"height": 12.5`)},
		{"height negative", rep(`"height": 12`, `"height": -1`)},
		{"height too large", rep(`"height": 12`, `"height": 9223372036854775808`)},
		{"height huge", rep(`"height": 12`, `"height": 1e400`)},
		{"hash_version 2", rep(`"hash_version": 1`, `"hash_version": 2`)},
		{"hash_version fraction", rep(`"hash_version": 1`, `"hash_version": 1.0`)},
		{"bad closed_at", rep(`2026-11-14T10:20:30.123456Z`, `yesterday`)},
		{"closed_at number", rep(`"2026-11-14T10:20:30.123456Z"`, `5`)},
		{"database number", rep(`"database": "db"`, `"database": 5`)},
		{"duplicate key", rep(`"height": 12`, `"height": 12, "height": 12`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseDigest([]byte(tt.in)); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func headers(t *testing.T) ([]format.Header, [32]byte) {
	t.Helper()
	hs, err := ParseBlocks(fixture(t, "blocks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseDigest(fixture(t, "digests/d5.json"))
	if err != nil {
		t.Fatal(err)
	}
	return hs, d.ChainID
}

func TestVerifyBlocksFixture(t *testing.T) {
	hs, cid := headers(t)
	if err := VerifyBlocks(hs, cid); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyBlocksRejectsEveryHeaderFieldChange(t *testing.T) {
	_, cid := headers(t)
	fields := []struct {
		name string
		edit func(h *format.Header)
	}{
		{"height", func(h *format.Header) { h.Height++ }},
		{"prev_hash", func(h *format.Header) { h.PrevHash[3] ^= 1 }},
		{"tx_merkle_root", func(h *format.Header) { h.TxMerkleRoot[3] ^= 1 }},
		{"tx_count", func(h *format.Header) { h.TxCount++ }},
		{"closed_at_us", func(h *format.Header) { h.ClosedAtUS++ }},
		{"block_hash", func(h *format.Header) { h.BlockHash[3] ^= 1 }},
		{"hash_version", func(h *format.Header) { h.HashVersion++ }},
	}
	for i := 0; i < 7; i++ {
		for _, f := range fields {
			hs, _ := headers(t)
			f.edit(&hs[i])
			if err := VerifyBlocks(hs, cid); err == nil {
				t.Errorf("block %d %s changed: verified", i, f.name)
			}
		}
	}
}

func TestVerifyBlocksStructure(t *testing.T) {
	_, cid := headers(t)
	t.Run("empty", func(t *testing.T) {
		if err := VerifyBlocks(nil, cid); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("missing block 0", func(t *testing.T) {
		hs, _ := headers(t)
		if err := VerifyBlocks(hs[1:], cid); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("gap", func(t *testing.T) {
		hs, _ := headers(t)
		hs = append(hs[:2:2], hs[3:]...)
		if err := VerifyBlocks(hs, cid); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("swapped", func(t *testing.T) {
		hs, _ := headers(t)
		hs[2], hs[3] = hs[3], hs[2]
		if err := VerifyBlocks(hs, cid); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("time going back with a valid hash", func(t *testing.T) {
		hs, _ := headers(t)
		// Rebuild blocks 2.. so that every hash is right but block 2 is
		// earlier than block 1.
		hs[2].ClosedAtUS = hs[1].ClosedAtUS - 1
		hs[2].BlockHash = hs[2].ComputeHash()
		for i := 3; i < len(hs); i++ {
			hs[i].PrevHash = hs[i-1].BlockHash
			hs[i].BlockHash = hs[i].ComputeHash()
		}
		err := VerifyBlocks(hs, cid)
		if err == nil || !strings.Contains(err.Error(), "earlier") {
			t.Errorf("err = %v, want a block time error", err)
		}
	})
}

func TestParseBlocksRejects(t *testing.T) {
	good := string(fixture(t, "blocks.jsonl"))
	first := strings.SplitN(good, "\n", 2)[0]
	for _, tt := range []struct{ name, in string }{
		{"empty", ""},
		{"blank line", first + "\n\n" + first},
		{"not json", "x"},
		{"array", "[]"},
		{"missing field", strings.Replace(first, `"tx_count"`, `"tx_countx"`, 1)},
		{"height fraction", strings.Replace(first, `"height" : 0`, `"height" : 0.5`, 1)},
		{"closed_at_us huge", strings.Replace(first, `"closed_at_us" : 847984104663947`, `"closed_at_us" : 9223372036854775808`, 1)},
		{"tx_count zero", strings.Replace(first, `"tx_count" : 2`, `"tx_count" : 0`, 1)},
		{"two objects on a line", first + first},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseBlocks([]byte(tt.in)); err == nil {
				t.Error("want an error")
			}
		})
	}
	if _, err := ParseBlocks(bytes.ReplaceAll([]byte(good), []byte("\n"), []byte("\r\n"))); err != nil {
		t.Errorf("CRLF line endings: %v", err)
	}
}

func TestCheckDigestOnBlocks(t *testing.T) {
	hs, _ := headers(t)
	d, err := ParseDigest(fixture(t, "digests/d2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDigestOnBlocks(hs, d); err != nil {
		t.Fatal(err)
	}
	bad := d
	bad.BlockHash[0] ^= 1
	if CheckDigestOnBlocks(hs, bad) == nil {
		t.Error("wrong hash accepted")
	}
	bad = d
	bad.ClosedAtUS++
	if CheckDigestOnBlocks(hs, bad) == nil {
		t.Error("wrong block time accepted")
	}
	bad = d
	bad.Height = 100
	if CheckDigestOnBlocks(hs, bad) == nil {
		t.Error("digest beyond the blocks accepted")
	}
	if CheckDigestOnBlocks(nil, d) == nil {
		t.Error("no blocks accepted")
	}
}

func TestCheckDigestSet(t *testing.T) {
	mk := func(name string, h uint64, hashByte byte, closed, gen int64) NamedDigest {
		d := Digest{Height: h, ClosedAtUS: closed, HasClosedAt: true, GeneratedAtUS: gen, HasGeneratedAt: true}
		d.BlockHash[0] = hashByte
		return NamedDigest{Name: name, Digest: d}
	}
	tests := []struct {
		name string
		ds   []NamedDigest
		ok   bool
	}{
		{"consistent", []NamedDigest{mk("a", 1, 1, 10, 100), mk("b", 3, 3, 30, 200)}, true},
		{"same height same hash", []NamedDigest{mk("a", 1, 1, 10, 100), mk("b", 1, 1, 10, 200)}, true},
		{"same height other hash", []NamedDigest{mk("a", 1, 1, 10, 100), mk("b", 1, 2, 10, 200)}, false},
		{"height went back", []NamedDigest{mk("a", 3, 3, 30, 100), mk("b", 1, 1, 10, 200)}, false},
		{"time went back", []NamedDigest{mk("a", 1, 1, 30, 100), mk("b", 3, 3, 10, 200)}, false},
		{"one digest", []NamedDigest{mk("a", 1, 1, 10, 100)}, false},
		{"other chain", func() []NamedDigest {
			x := mk("b", 3, 3, 30, 200)
			x.ChainID[0] = 1
			return []NamedDigest{mk("a", 1, 1, 10, 100), x}
		}(), false},
		{"no block time", func() []NamedDigest {
			x := mk("b", 3, 3, 30, 200)
			x.HasClosedAt = false
			return []NamedDigest{mk("a", 1, 1, 10, 100), x}
		}(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(CheckDigestSet(tt.ds)) == 0; got != tt.ok {
				t.Errorf("consistent = %v, want %v: %v", got, tt.ok, CheckDigestSet(tt.ds))
			}
		})
	}
}

func TestFormatUS(t *testing.T) {
	for _, tt := range []struct {
		us   int64
		want string
	}{
		{0, "2000-01-01T00:00:00.000000Z"},
		{-1, "1999-12-31T23:59:59.999999Z"},
		{1_000_001, "2000-01-01T00:00:01.000001Z"},
		{1 << 62, "4611686018427387904 us since 2000-01-01"},
	} {
		if got := FormatUS(tt.us); got != tt.want {
			t.Errorf("FormatUS(%d) = %q, want %q", tt.us, got, tt.want)
		}
	}
}
