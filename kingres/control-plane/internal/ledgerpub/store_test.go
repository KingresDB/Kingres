// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerpub_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

var bg = context.Background()

// fixture is what a store test needs besides the Store: the term the store holds, a way to end it (another leader took over) and a way to
// change the key set (a key task committed).
type fixture struct {
	store    ledgerpub.Store
	term     scheduler.Term
	newTerm  func() scheduler.Term
	bumpKeys func() int64
	// rev is the revision of the key set now.
	rev func() int64
	// putEvent and putKeyring write what the store serves as events/<n> and keyring.json.
	putEvent   func(seq int, note []byte)
	putKeyring func(data []byte)
}

func memFixture(t *testing.T) *fixture {
	m := ledgerpub.NewMemStore(3)
	var rev int64
	f := &fixture{store: m, term: 3}
	f.newTerm = func() scheduler.Term { f.term++; m.SetTerm(f.term); return f.term }
	f.bumpKeys = func() int64 { rev++; m.SetKeysRevision(rev); return rev }
	f.rev = func() int64 { return rev }
	f.putEvent = func(seq int, note []byte) {
		for len(m.Events) <= seq {
			m.Events = append(m.Events, nil)
		}
		m.Events[seq] = note
	}
	f.putKeyring = func(d []byte) { m.Keyring = d }
	return f
}

func newLog(t *testing.T, f *fixture, chain byte) ledgerpub.LogRow {
	t.Helper()
	var id [32]byte
	id[0] = chain
	origin, err := ledgerlog.ChainOrigin("ledger.test/kingres", id, 1)
	if err != nil {
		t.Fatal(err)
	}
	g := int32(chain)
	l, err := f.store.CreateLog(bg, f.term, ledgerpub.LogRow{Kind: ledgerpub.KindChain, Database: "shop", ShardGroupID: &g, ChainID: id[:], Epoch: 1, Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func hash32(b []byte) (h [32]byte, err error) {
	if len(b) != 32 {
		return h, errors.New("not 32 bytes")
	}
	copy(h[:], b)
	return h, nil
}

// grow appends n entries to the log and stores the frontier, as a round does, and returns the size and the root.
func grow(t *testing.T, f *fixture, l ledgerpub.LogRow, from, n int) (size int64, root []byte) {
	t.Helper()
	root0, err := hash32(l.Root)
	if err != nil {
		t.Fatal(err)
	}
	lg, err := ledgerlog.Open(bg, l.Origin, f.store.Tiles(l.ID, f.term), l.Frontier, uint64(l.Size), root0, ledgerlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var es [][]byte
	for i := from; i < from+n; i++ {
		e := make([]byte, 87)
		e[0], e[1] = 0x13, byte(i)
		es = append(es, e)
	}
	a, err := lg.Append(bg, es)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveFrontier(bg, f.term, l.ID, int64(a.Size), a.Root[:], a.Frontier, []byte("headhash")); err != nil {
		t.Fatal(err)
	}
	return int64(a.Size), a.Root[:]
}

// testStore runs the rules of the store against a Store. The same rules hold for the memory store and the database store.
func testStore(t *testing.T, mk func(t *testing.T) *fixture) {
	t.Run("a log is created once and one chain has one open log", func(t *testing.T) {
		f := mk(t)
		l := newLog(t, f, 1)
		again := newLog(t, f, 1)
		if again.ID != l.ID || l.State != ledgerpub.StateActive || l.Size != 0 {
			t.Fatalf("%+v %+v", l, again)
		}
		var id [32]byte
		id[0] = 1
		other, _ := ledgerlog.ChainOrigin("ledger.test/kingres", id, 2)
		g := int32(1)
		if _, err := f.store.CreateLog(bg, f.term, ledgerpub.LogRow{Kind: ledgerpub.KindChain, Database: "shop", ShardGroupID: &g, ChainID: id[:], Epoch: 2, Origin: other}); !errors.Is(err, ledgerpub.ErrConflict) {
			t.Fatalf("a second open log of the chain: %v", err)
		}
		if _, err := f.store.LogByOrigin(bg, "nope"); !errors.Is(err, ledgerpub.ErrNotFound) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("every write is under the term of the leader", func(t *testing.T) {
		f := mk(t)
		l := newLog(t, f, 1)
		old := f.term
		f.newTerm()
		g := int32(2)
		var id [32]byte
		id[0] = 2
		if _, err := f.store.CreateLog(bg, old, ledgerpub.LogRow{Kind: ledgerpub.KindChain, ChainID: id[:], ShardGroupID: &g, Epoch: 1, Origin: "ledger.test/kingres/c/x"}); !errors.Is(err, scheduler.ErrDeposed) {
			t.Errorf("CreateLog: %v", err)
		}
		tiles := f.store.Tiles(l.ID, old)
		if err := tiles.PutTile(bg, ledgerlog.TileKey{Level: 0, Index: 0, Width: 1}, make([]byte, 32)); !errors.Is(err, scheduler.ErrDeposed) {
			t.Errorf("PutTile: %v", err)
		}
		if err := f.store.SaveFrontier(bg, old, l.ID, 1, make([]byte, 32), []byte("x"), nil); !errors.Is(err, scheduler.ErrDeposed) {
			t.Errorf("SaveFrontier: %v", err)
		}
		if err := f.store.PutPending(bg, old, l.ID, []byte("x")); !errors.Is(err, scheduler.ErrDeposed) {
			t.Errorf("PutPending: %v", err)
		}
		if err := f.store.SetState(bg, old, l.ID, ledgerpub.StateDiverged, "x"); !errors.Is(err, scheduler.ErrDeposed) {
			t.Errorf("SetState: %v", err)
		}
		if err := f.store.SetRetired(bg, old, l.ID, 1); !errors.Is(err, scheduler.ErrDeposed) {
			t.Errorf("SetRetired: %v", err)
		}
		if err := f.store.SetUnread(bg, old, l.ID, nil); !errors.Is(err, scheduler.ErrDeposed) {
			t.Errorf("SetUnread: %v", err)
		}
		if _, err := f.store.PutDigest(bg, old, ledgerpub.DigestView{Database: "shop", GeneratedAt: time.Now(), Chains: []byte("[]")}); !errors.Is(err, scheduler.ErrDeposed) {
			t.Errorf("PutDigest: %v", err)
		}
		if err := f.store.PutCheckpoint(bg, old, ledgerpub.Checkpoint{LogID: l.ID, Size: 0, Root: l.Root, Note: []byte("n"), SignedAt: time.Now()}, f.rev(), false, ""); !errors.Is(err, scheduler.ErrDeposed) {
			t.Errorf("PutCheckpoint: %v", err)
		}
		if err := f.store.ReplaceNote(bg, old, l.ID, 1, []byte("x"), f.rev()); !errors.Is(err, scheduler.ErrDeposed) {
			t.Errorf("ReplaceNote: %v", err)
		}
		got, _ := f.store.LogByOrigin(bg, l.Origin)
		if got.Size != 0 || got.State != ledgerpub.StateActive || len(got.PendingEntry) > 0 {
			t.Errorf("the deposed writer changed the log: %+v", got)
		}
		if logs, _ := f.store.Logs(bg); len(logs) != 1 {
			t.Errorf("%d logs", len(logs))
		}
	})

	t.Run("a published tile is never replaced", func(t *testing.T) {
		f := mk(t)
		l := newLog(t, f, 1)
		k := ledgerlog.TileKey{Level: 0, Index: 0, Width: 2}
		a, b := bytes.Repeat([]byte{1}, 64), bytes.Repeat([]byte{2}, 64)
		ts := f.store.Tiles(l.ID, f.term)
		if err := ts.PutTile(bg, k, a); err != nil {
			t.Fatal(err)
		}
		if err := ts.PutTile(bg, k, a); err != nil {
			t.Fatalf("an equal put: %v", err)
		}
		if err := ts.PutTile(bg, k, b); !errors.Is(err, ledgerlog.ErrConflict) {
			t.Fatalf("another content: %v", err)
		}
		if d, err := ts.GetTile(bg, k); err != nil || !bytes.Equal(d, a) {
			t.Fatalf("the tile changed: %v", err)
		}
		if err := ts.PutTile(bg, ledgerlog.TileKey{Level: 0, Index: 0, Width: 2}, []byte("short")); err == nil {
			t.Error("a tile of the wrong length was stored")
		}
		if w, _ := ts.Widths(bg, 0, 0); len(w) != 1 || w[0] != 2 {
			t.Errorf("widths %v", w)
		}
		if _, err := ts.GetTile(bg, ledgerlog.TileKey{Level: 3, Index: 9, Width: 1}); !errors.Is(err, ledgerlog.ErrNotFound) {
			t.Errorf("%v", err)
		}
	})

	t.Run("the frontier only moves forward and the checkpoint is for the stored tree", func(t *testing.T) {
		f := mk(t)
		l := newLog(t, f, 1)
		size5, root5 := grow(t, f, l, 0, 5)
		cur, _ := f.store.LogByOrigin(bg, l.Origin)
		size7, root7 := grow(t, f, cur, 5, 2)
		// a checkpoint for a smaller tree than the stored one is refused (the rollback: never signed)
		n7 := mkNote(t, "ledger.test/kingres/c/x\n7\nAAAA\n", 1)
		cp5 := ledgerpub.Checkpoint{LogID: l.ID, Size: size5, Root: root5, Note: []byte("note5"), SignedAt: time.Now()}
		if err := f.store.PutCheckpoint(bg, f.term, cp5, f.rev(), true, "ledger.test/kingres"); !errors.Is(err, ledgerpub.ErrConflict) {
			t.Fatalf("a smaller tree: %v", err)
		}
		// a checkpoint with another root for the stored size is refused (the fork)
		bad := ledgerpub.Checkpoint{LogID: l.ID, Size: size7, Root: bytes.Repeat([]byte{9}, 32), Note: []byte("fork"), SignedAt: time.Now()}
		if err := f.store.PutCheckpoint(bg, f.term, bad, f.rev(), false, ""); !errors.Is(err, ledgerpub.ErrConflict) {
			t.Fatalf("another root: %v", err)
		}
		ok := ledgerpub.Checkpoint{LogID: l.ID, Size: size7, Root: root7, Note: n7, SignedAt: time.Now()}
		if err := f.store.PutCheckpoint(bg, f.term, ok, f.rev(), true, "ledger.test/kingres"); err != nil {
			t.Fatal(err)
		}
		// the same tree again is a no-op and keeps the note
		again := ok
		again.Note = []byte("other note")
		if err := f.store.PutCheckpoint(bg, f.term, again, f.rev(), false, ""); err != nil {
			t.Fatalf("the same tree again: %v", err)
		}
		if got, err := f.store.Checkpoint(bg, l.ID, size7); err != nil || !bytes.Equal(got.Note, n7) {
			t.Fatalf("%v %v", got, err)
		}
		// the frontier does not go back, and another root for the stored size is refused
		if err := f.store.SaveFrontier(bg, f.term, l.ID, size5, root5, []byte("f"), nil); !errors.Is(err, ledgerpub.ErrConflict) {
			t.Errorf("a smaller frontier: %v", err)
		}
		if err := f.store.SaveFrontier(bg, f.term, l.ID, size7, bytes.Repeat([]byte{9}, 32), []byte("f"), nil); !errors.Is(err, ledgerpub.ErrConflict) {
			t.Errorf("another root for the stored size: %v", err)
		}
		// a note is replaced only by ReplaceNote (the re-signing)
		// P8-06: only a note that adds signatures to the same text; any other note, an unreadable one, and a re-signing that loaded the
		// signers before a key event are refused, and the stored note stays
		other := mkNote(t, "ledger.test/kingres/c/x\n7\nBBBB\n", 1, 2)
		if err := f.store.ReplaceNote(bg, f.term, l.ID, size7, other, f.rev()); !errors.Is(err, ledgerpub.ErrConflict) {
			t.Errorf("a note with another text: %v", err)
		}
		if err := f.store.ReplaceNote(bg, f.term, l.ID, size7, []byte("completely different text\n\n- sig\n"), f.rev()); !errors.Is(err, ledgerpub.ErrConflict) {
			t.Errorf("a note that does not parse: %v", err)
		}
		stale := f.rev()
		f.bumpKeys()
		two := mkNote(t, "ledger.test/kingres/c/x\n7\nAAAA\n", 1, 2)
		if err := f.store.ReplaceNote(bg, f.term, l.ID, size7, two, stale); !errors.Is(err, ledgerpub.ErrKeysChanged) {
			t.Errorf("a re-signing with signers of an older key set: %v", err)
		}
		if got, _ := f.store.LatestCheckpoint(bg, l.ID); !bytes.Equal(got.Note, n7) {
			t.Fatalf("a refused replacement changed the note")
		}
		if err := f.store.ReplaceNote(bg, f.term, l.ID, size7, two, f.rev()); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.store.LatestCheckpoint(bg, l.ID); !bytes.Equal(got.Note, two) || got.Size != size7 {
			t.Errorf("latest %+v", got)
		}
		if err := f.store.ReplaceNote(bg, f.term, l.ID, 99, []byte("x"), f.rev()); !errors.Is(err, ledgerpub.ErrNotFound) {
			t.Errorf("a size that has no checkpoint: %v", err)
		}
		if cps, _ := f.store.Checkpoints(bg, l.ID, 0, 0); len(cps) != 1 {
			t.Errorf("%d checkpoints", len(cps))
		}
	})

	t.Run("a checkpoint is refused when the key set changed after the signer was loaded", func(t *testing.T) {
		f := mk(t)
		l := newLog(t, f, 1)
		size, root := grow(t, f, l, 0, 3)
		loaded := f.rev()
		f.bumpKeys()
		cp := ledgerpub.Checkpoint{LogID: l.ID, Size: size, Root: root, Note: []byte("n"), SignedAt: time.Now()}
		if err := f.store.PutCheckpoint(bg, f.term, cp, loaded, false, ""); !errors.Is(err, ledgerpub.ErrKeysChanged) {
			t.Fatalf("%v", err)
		}
		if _, err := f.store.LatestCheckpoint(bg, l.ID); !errors.Is(err, ledgerpub.ErrNotFound) {
			t.Fatalf("a checkpoint was stored: %v", err)
		}
		if err := f.store.PutCheckpoint(bg, f.term, cp, f.rev(), false, ""); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a stopped log takes no frontier and no checkpoint, a closed log stays closed", func(t *testing.T) {
		f := mk(t)
		l := newLog(t, f, 1)
		size, root := grow(t, f, l, 0, 3)
		if err := f.store.SetState(bg, f.term, l.ID, ledgerpub.StateDiverged, "rollback"); err != nil {
			t.Fatal(err)
		}
		cp := ledgerpub.Checkpoint{LogID: l.ID, Size: size, Root: root, Note: []byte("n"), SignedAt: time.Now()}
		if err := f.store.PutCheckpoint(bg, f.term, cp, f.rev(), false, ""); !errors.Is(err, ledgerpub.ErrConflict) {
			t.Errorf("a checkpoint of a diverged log: %v", err)
		}
		if err := f.store.SaveFrontier(bg, f.term, l.ID, size+1, root, []byte("f"), nil); !errors.Is(err, ledgerpub.ErrConflict) {
			t.Errorf("a frontier of a diverged log: %v", err)
		}
		if err := f.store.SetState(bg, f.term, l.ID, ledgerpub.StateClosed, "retired"); err != nil {
			t.Fatal(err)
		}
		if err := f.store.SetState(bg, f.term, l.ID, ledgerpub.StateActive, ""); !errors.Is(err, ledgerpub.ErrConflict) {
			t.Errorf("reopening a closed log: %v", err)
		}
		got, _ := f.store.LogByOrigin(bg, l.Origin)
		if got.State != ledgerpub.StateClosed || got.ClosedAt == nil || got.Detail != "retired" {
			t.Errorf("%+v", got)
		}
		// a closed log frees the chain for another epoch
		var id [32]byte
		id[0] = 1
		o2, _ := ledgerlog.ChainOrigin("ledger.test/kingres", id, 2)
		g := int32(1)
		if _, err := f.store.CreateLog(bg, f.term, ledgerpub.LogRow{Kind: ledgerpub.KindChain, Database: "shop", ShardGroupID: &g, ChainID: id[:], Epoch: 2, Origin: o2}); err != nil {
			t.Errorf("a second epoch after the first was closed: %v", err)
		}
	})

	t.Run("pending leaf, retirement and unread marks", func(t *testing.T) {
		f := mk(t)
		l := newLog(t, f, 1)
		if err := f.store.PutPending(bg, f.term, l.ID, []byte("leaf")); err != nil {
			t.Fatal(err)
		}
		got, _ := f.store.LogByOrigin(bg, l.Origin)
		if string(got.PendingEntry) != "leaf" {
			t.Fatalf("%q", got.PendingEntry)
		}
		size, root := grow(t, f, l, 0, 2)
		got, _ = f.store.LogByOrigin(bg, l.Origin)
		if len(got.PendingEntry) != 0 || got.Size != size || !bytes.Equal(got.Root, root) || string(got.HeadBlockHash) != "headhash" {
			t.Fatalf("the frontier did not clear pending: %+v", got)
		}
		now := time.Now().UTC().Truncate(time.Second)
		if err := f.store.SetUnread(bg, f.term, l.ID, &now); err != nil {
			t.Fatal(err)
		}
		if err := f.store.SetRetired(bg, f.term, l.ID, 4); err != nil {
			t.Fatal(err)
		}
		got, _ = f.store.LogByOrigin(bg, l.Origin)
		if got.UnreadSince == nil || !got.UnreadSince.Equal(now) || got.RetiredInSeq == nil || *got.RetiredInSeq != 4 {
			t.Fatalf("%+v", got)
		}
		if err := f.store.SetUnread(bg, f.term, l.ID, nil); err != nil {
			t.Fatal(err)
		}
		if got, _ = f.store.LogByOrigin(bg, l.Origin); got.UnreadSince != nil {
			t.Error("not cleared")
		}
	})

	t.Run("digest views", func(t *testing.T) {
		f := mk(t)
		at := time.Now().UTC().Truncate(time.Second)
		size := int64(4)
		id1, err := f.store.PutDigest(bg, f.term, ledgerpub.DigestView{Database: "shop", GeneratedAt: at, GeneratedBy: "alice", HashVersion: 1, Chains: []byte(`[{"shardGroupId":0}]`), Complete: true})
		if err != nil {
			t.Fatal(err)
		}
		id2, err := f.store.PutDigest(bg, f.term, ledgerpub.DigestView{Database: "shop", GeneratedAt: at.Add(time.Minute), GeneratedBy: "system", HashVersion: 1, Chains: []byte(`[]`),
			Uncovered: []int32{2}, AggregateSize: &size, AggregateHash: bytes.Repeat([]byte{7}, 32)})
		if err != nil || id2 <= id1 {
			t.Fatalf("%d %d %v", id1, id2, err)
		}
		if _, err := f.store.PutDigest(bg, f.term, ledgerpub.DigestView{Database: "other", GeneratedAt: at, GeneratedBy: "x", HashVersion: 1, Chains: []byte(`[]`)}); err != nil {
			t.Fatal(err)
		}
		v, err := f.store.Digest(bg, "shop", id2)
		if err != nil || v.Complete || len(v.Uncovered) != 1 || v.Uncovered[0] != 2 || v.AggregateSize == nil || *v.AggregateSize != 4 || len(v.AggregateHash) != 32 {
			t.Fatalf("%+v %v", v, err)
		}
		if _, err := f.store.Digest(bg, "other", id2); !errors.Is(err, ledgerpub.ErrNotFound) {
			t.Errorf("a digest of another database: %v", err)
		}
		list, _ := f.store.Digests(bg, "shop", 0, 10)
		if len(list) != 2 || list[0].ID != id2 || list[1].ID != id1 || string(list[1].Chains) == "" {
			t.Errorf("list %+v", list)
		}
		if list, _ = f.store.Digests(bg, "shop", id2, 10); len(list) != 1 || list[0].ID != id1 {
			t.Errorf("below %d: %+v", id2, list)
		}
		if got, _ := f.store.LastDigestAt(bg, "shop"); !got.Equal(at.Add(time.Minute)) {
			t.Errorf("last %v", got)
		}
		if got, _ := f.store.LastDigestAt(bg, "none"); !got.IsZero() {
			t.Errorf("last of none %v", got)
		}
	})

	t.Run("objects", func(t *testing.T) {
		f := mk(t)
		l := newLog(t, f, 1)
		size, root := grow(t, f, l, 0, 3)
		cp := ledgerpub.Checkpoint{LogID: l.ID, Size: size, Root: root, Note: []byte("the signed note"), SignedAt: time.Now()}
		if err := f.store.PutCheckpoint(bg, f.term, cp, f.rev(), false, ""); err != nil {
			t.Fatal(err)
		}
		o, err := f.store.Object(bg, l.Origin, "checkpoint")
		if err != nil || string(o.Data) != "the signed note" || !o.Mutable {
			t.Fatalf("checkpoint %+v %v", o, err)
		}
		tile, err := f.store.Object(bg, l.Origin, "tile/0/000.p/3")
		if err != nil || len(tile.Data) != 96 || tile.Mutable || tile.ContentType != "application/octet-stream" {
			t.Fatalf("tile %+v %v", tile, err)
		}
		if _, err := f.store.Object(bg, l.Origin, "tile/entries/000.p/3"); err != nil {
			t.Fatalf("bundle: %v", err)
		}
		for _, rest := range []string{"tile/0/000.p/9", "tile/9/000", "tile/00/000", "nothing", "tile/0/0", "../checkpoint"} {
			if _, err := f.store.Object(bg, l.Origin, rest); !errors.Is(err, ledgerpub.ErrNotFound) {
				t.Errorf("%q: %v", rest, err)
			}
		}
		if _, err := f.store.Object(bg, "ledger.test/kingres/c/none/e1", "checkpoint"); !errors.Is(err, ledgerpub.ErrNotFound) {
			t.Errorf("unknown log: %v", err)
		}
		if _, err := f.store.Object(bg, "", "keyring.json"); !errors.Is(err, ledgerpub.ErrNotFound) {
			t.Errorf("no keyring yet: %v", err)
		}
		f.putKeyring([]byte(`{"format":1}`))
		if o, err := f.store.Object(bg, "", "keyring.json"); err != nil || string(o.Data) != `{"format":1}` || !o.Mutable || o.ContentType != "application/json" {
			t.Errorf("keyring %+v %v", o, err)
		}
		f.putEvent(0, []byte("event zero"))
		if o, err := f.store.Object(bg, "", "events/000000"); err != nil || string(o.Data) != "event zero" || o.Mutable {
			t.Errorf("event %+v %v", o, err)
		}
		if _, err := f.store.Object(bg, "", "events/000007"); !errors.Is(err, ledgerpub.ErrNotFound) {
			t.Errorf("event 7: %v", err)
		}
	})
}

func TestTheMemoryStoreKeepsTheRulesOfTheStore(t *testing.T) { testStore(t, memFixture) }

// mkNote is a signed note of the shape ParseNote reads: the text and one signature line per id.
func mkNote(t *testing.T, text string, ids ...uint32) []byte {
	t.Helper()
	n := &ledgerlog.Note{Text: []byte(text)}
	for _, id := range ids {
		n.Signatures = append(n.Signatures, ledgerlog.NoteSignature{Name: "ledger.test/kingres/c/x", ID: id, Sig: bytes.Repeat([]byte{7}, 64)})
	}
	b, err := n.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
