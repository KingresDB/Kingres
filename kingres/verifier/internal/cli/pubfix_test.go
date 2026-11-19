// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/aggregate"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// A world is a whole publication made by this test, from the documents and nothing else: chain logs of
// real block headers (each block holds one transaction, entry 0 a row of table 7), an aggregate log, signed events,
// tiles, entry bundles, signed checkpoints, a trust file. It shares no code with the verifier except
// the hash functions of format and merkle for what a block contains (which the cross-check against kingresd
// and the fixtures of the README check from the other side).

const tableUser = 7

type chainFix struct {
	id      [32]byte
	idHex   string
	epoch   uint32
	entries [][]byte // leaf data of block i
	leaves  []h32
	hdrs    []format.Header
	rows    []h32 // row hash of entry 0 of block i
}

func rowHashOf(chain byte, i int) h32 { return sha([]byte{chain}, []byte(fmt.Sprintf("row %d", i))) }

// blockSpec describes the transactions of a block that is not the default one.
type extraEntry struct {
	table uint32
	op    byte
	row   h32
}

// makeChain makes n blocks. extra[i] lists entries added after entry 0 of block i.
func makeChain(idb byte, n int, extra map[int][]extraEntry) *chainFix {
	c := &chainFix{epoch: 1}
	for i := range c.id {
		c.id[i] = idb
	}
	c.idHex = hex.EncodeToString(c.id[:])
	prev := format.GenesisHash(c.id)
	for i := 0; i < n; i++ {
		row := rowHashOf(idb, i)
		c.rows = append(c.rows, row)
		var leaves []h32
		leaves = append(leaves, format.EntryLeafHash(0, tableUser, format.OpInsert, row))
		for j, x := range extra[i] {
			leaves = append(leaves, format.EntryLeafHash(uint32(j+1), x.table, x.op, x.row))
		}
		txRoot := refMTH(leaves)
		txLeaf, err := format.BlockTxLeafHash(uint64(100+i), uint32(len(leaves)), txRoot, "alice")
		if err != nil {
			panic(err)
		}
		h := format.Header{Height: uint64(i), HashVersion: 1, PrevHash: prev, TxMerkleRoot: txLeaf, TxCount: 1, ClosedAtUS: int64(1_000_000 * (i + 1))}
		h.BlockHash = h.ComputeHash()
		prev = h.BlockHash
		c.hdrs = append(c.hdrs, h)
		e := h.BlockHashInput()
		c.entries = append(c.entries, e)
		c.leaves = append(c.leaves, merkle.LeafHash(e))
	}
	return c
}

// proofJSON is the proof of entry k of block i (a block of makeChain), as the format 1 document.
func (c *chainFix) proofJSON(i int, extra []extraEntry, k int) []byte {
	var leaves []h32
	leaves = append(leaves, format.EntryLeafHash(0, tableUser, format.OpInsert, c.rows[i]))
	table, op, row := uint32(tableUser), format.OpInsert, c.rows[i]
	for j, x := range extra {
		leaves = append(leaves, format.EntryLeafHash(uint32(j+1), x.table, x.op, x.row))
		if j+1 == k {
			table, op, row = x.table, x.op, x.row
		}
	}
	path := refPath(k, leaves)
	h := c.hdrs[i]
	hx := func(b [32]byte) string { return hex.EncodeToString(b[:]) }
	ps := []string{}
	for _, p := range path {
		ps = append(ps, hx(p))
	}
	hdr := map[string]any{"height": h.Height, "hash_version": 1, "prev_hash": hx(h.PrevHash), "tx_merkle_root": hx(h.TxMerkleRoot), "tx_count": h.TxCount, "closed_at_us": h.ClosedAtUS, "block_hash": hx(h.BlockHash)}
	opS := "I"
	if op == format.OpDelete {
		opS = "D"
	}
	doc := map[string]any{"format": 1, "chain_id": c.idHex, "table_id": table, "ledger_txid": 100 + i, "seq": k, "op": opS, "row_hash": hx(row), "entry_index": k, "entry_count": len(leaves),
		"entry_path": ps, "tx_root": hx(refMTH(leaves)), "user_name": "alice", "tx_index": 0, "tx_path": []string{}, "block": hdr, "chain": []any{}}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return b
}

func refPath(m int, d []h32) []h32 {
	if len(d) == 1 {
		return nil
	}
	k := 1
	for k*2 < len(d) {
		k *= 2
	}
	if m < k {
		return append(refPath(m, d[:k]), refMTH(d[k:]))
	}
	return append(refPath(m-k, d[k:]), refMTH(d[:k]))
}

// signText signs a note text under a key name with every signer.
func signText(text, name string, ss ...signer) string {
	var b strings.Builder
	b.WriteString(text + "\n")
	for _, s := range ss {
		sum := sha256.Sum256(append(append([]byte(name), 0x0A, 0x01), s.pub...))
		raw := append(append([]byte{}, sum[:4]...), ed25519.Sign(s.priv, []byte(text))...)
		b.WriteString("— " + name + " " + base64.StdEncoding.EncodeToString(raw) + "\n")
	}
	return b.String()
}

func cpText(origin string, size int, root h32) string {
	return fmt.Sprintf("%s\n%d\n%s\n", origin, size, base64.StdEncoding.EncodeToString(root[:]))
}

func entryBundle(entries [][]byte) []byte {
	var b []byte
	for _, e := range entries {
		b = binary.BigEndian.AppendUint16(b, uint16(len(e)))
		b = append(b, e...)
	}
	return b
}

// writeEntries writes the entry bundles of the tree of the first size entries (full bundles and the partial one).
func writeEntries(t *testing.T, dir string, entries [][]byte, size int) {
	t.Helper()
	put := func(n, width int, es [][]byte) {
		name := "tile/entries/" + tileIndex(n)
		if width < 256 {
			name += fmt.Sprintf(".p/%d", width)
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, entryBundle(es), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for n := 0; n*256 < size; n++ {
		end := min((n+1)*256, size)
		put(n, end-n*256, entries[n*256:end])
	}
}

type world struct {
	t      *testing.T
	dir    string
	prefix string
	s1, s2 signer
	trust  string
	chains []*chainFix
	aggID  string
	leaves []*aggregate.Leaf
	aggEnc [][]byte
	events []string // notes by sequence
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, dir: testdir.New(t), prefix: testPrefix, s1: newSigner(1), s2: newSigner(2)}
	sum := sha256.Sum256([]byte("aggregate id of the test"))
	w.aggID = hex.EncodeToString(sum[:])
	w.trust = w.writeTrust(w.s1)
	return w
}

func (w *world) writeTrust(ss ...signer) string {
	w.t.Helper()
	p := filepath.Join(testdir.New(w.t), "trust.json")
	code, out, errs := run(w.t, "trust", "init", "--origin-prefix", w.prefix, "--public-key", base64.StdEncoding.EncodeToString(ss[0].pub), "--fingerprint", ss[0].fp(), "--out", p)
	if code != 0 {
		w.t.Fatalf("trust init: %d %s %s", code, out, errs)
	}
	return p
}

func (w *world) logDir(rel string) string { return filepath.Join(w.dir, filepath.FromSlash(rel)) }

func (w *world) put(rel string, data []byte) {
	w.t.Helper()
	p := filepath.Join(w.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// publishChain writes the tiles, bundles and signed checkpoint of the chain at a size.
func (w *world) publishChain(c *chainFix, size int, ss ...signer) {
	w.t.Helper()
	rel := "c/" + c.idHex + "/e" + fmt.Sprint(c.epoch)
	writeTiles(w.t, w.logDir(rel), c.leaves, size)
	writeEntries(w.t, w.logDir(rel), c.entries, size)
	if len(ss) == 0 {
		ss = []signer{w.s1}
	}
	origin := w.prefix + "/" + rel
	w.put(rel+"/checkpoint", []byte(signText(cpText(origin, size, refMTH(c.leaves[:size])), origin, ss...)))
}

func (w *world) addChain(c *chainFix, size int) {
	w.chains = append(w.chains, c)
	w.publishChain(c, size)
}

// member of a chain at a number of blocks.
func member(c *chainFix, blocks int, flags byte) aggregate.Member {
	return aggregate.Member{Flags: flags, ChainID: c.id, Epoch: c.epoch, Height: uint64(blocks - 1), BlockHash: c.hdrs[blocks-1].BlockHash, LogRoot: refMTH(c.leaves[:blocks])}
}

// appendRound adds a leaf to the aggregate log and publishes the log at its new size.
func (w *world) appendRound(atUS int64, ms ...aggregate.Member) {
	w.t.Helper()
	sortMembers(ms)
	l := &aggregate.Leaf{Seq: uint64(len(w.leaves)), GeneratedAtUS: atUS, Members: ms}
	w.leaves = append(w.leaves, l)
	w.aggEnc = append(w.aggEnc, l.Encode())
	w.publishAggregate(len(w.leaves))
}

func sortMembers(ms []aggregate.Member) {
	for i := range ms {
		for j := i + 1; j < len(ms); j++ {
			a, b := ms[i], ms[j]
			if string(append(a.ChainID[:], byte(a.Epoch>>24), byte(a.Epoch>>16), byte(a.Epoch>>8), byte(a.Epoch))) > string(append(b.ChainID[:], byte(b.Epoch>>24), byte(b.Epoch>>16), byte(b.Epoch>>8), byte(b.Epoch))) {
				ms[i], ms[j] = ms[j], ms[i]
			}
		}
	}
}

func (w *world) publishAggregate(size int, ss ...signer) {
	w.t.Helper()
	rel := "a/" + w.aggID
	var lh []h32
	for _, e := range w.aggEnc {
		lh = append(lh, merkle.LeafHash(e))
	}
	writeTiles(w.t, w.logDir(rel), lh, size)
	writeEntries(w.t, w.logDir(rel), w.aggEnc, size)
	if len(ss) == 0 {
		ss = []signer{w.s1}
	}
	origin := w.prefix + "/" + rel
	w.put(rel+"/checkpoint", []byte(signText(cpText(origin, size, refMTH(lh[:size])), origin, ss...)))
}

// eventText builds the text of an event.
func eventText(seq int, typ string, lines ...string) string {
	return fmt.Sprintf("kingres-ledger-event/1\nseq: %d\ntime: 2026-11-19T10:%02d:00Z\ntype: %s\n%s", seq, seq%60, typ, strings.Join(append(lines, ""), "\n"))
}

func (w *world) event(seq int, typ string, signers []signer, lines ...string) {
	w.t.Helper()
	note := signText(eventText(seq, typ, lines...), w.prefix, signers...)
	w.put(fmt.Sprintf("events/%06d", seq), []byte(note))
}

func chainRetiredLines(c *chainFix, final int, reason string) []string {
	return []string{"chain: " + c.idHex, fmt.Sprintf("epoch: %d", c.epoch), fmt.Sprintf("final-height: %d", final), "final-block-hash: " + hex.EncodeToString(c.hdrs[final].BlockHash[:]), "reason: " + reason}
}

func keyAddedLines(s signer) []string {
	return []string{"key: " + base64.StdEncoding.EncodeToString(s.pub), "fingerprint: " + s.fp(), "not-before: 2026-11-19T00:00:00Z"}
}

// std makes the usual world: two chains of 5 and 3 blocks, an aggregate of two rounds.
func stdWorld(t *testing.T) (*world, *chainFix, *chainFix) {
	w := newWorld(t)
	a, b := makeChain(0xa1, 5, nil), makeChain(0xb2, 3, nil)
	w.addChain(a, 5)
	w.addChain(b, 3)
	w.appendRound(10_000_000, member(a, 3, 0), member(b, 2, 0))
	w.appendRound(20_000_000, member(a, 5, 0), member(b, 3, 0))
	return w, a, b
}

func (w *world) aggArgs(more ...string) []string {
	return append([]string{"aggregate", "--trust", w.trust, "--source", w.dir, "--aggregate-id", w.aggID, "--now", "2026-11-19T12:00:00Z"}, more...)
}
