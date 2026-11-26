// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

func newDirStore(t testing.TB) *DirStore {
	t.Helper()
	s, err := NewDirStore(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	// Only the crash tests are about what a flush does, and they use hooks, not
	// the machine: a real flush costs seconds on a loaded Windows disk.
	s.NoSync = true
	return s
}

func TestDirStoreLayoutAndEquality(t *testing.T) {
	d := newDirStore(t)
	m := NewMemStore()
	ld, _ := Create(bg, testOrigin(1), d)
	lm, _ := Create(bg, testOrigin(1), m)
	sizes := []int{700}
	if longTests() {
		sizes = append(sizes, 66000)
	} else {
		t.Log("the 66,000-leaf layout (levels 1 and 2) runs with KINGRES_LONG_TESTS=1; 700 leaves run here")
	}
	final := sizes[len(sizes)-1]
	for _, n := range sizes {
		from := int(ld.Size())
		mustAppend(t, ld, from, n)
		mustAppend(t, lm, from, n)
	}
	if ld.Root() != lm.Root() {
		t.Fatal("roots differ")
	}
	for _, k := range m.Keys() {
		want, _ := m.GetTile(bg, k)
		got, err := d.GetTile(bg, k)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %v", k.Path(), err)
		}
		// The file is where the verifier looks for it.
		fi, err := os.Stat(filepath.Join(d.Root(), filepath.FromSlash(k.Path())))
		if err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("%s: %v", k.Path(), err)
		}
	}
	if _, err := os.Stat(filepath.Join(d.Root(), "tile", "0", "000")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.Root(), "tile", "entries", fmt.Sprintf("%03d.p", final/256), fmt.Sprint(final%256))); err != nil {
		t.Fatal(err)
	}
	ws, err := d.Widths(bg, 0, uint64(final/256))
	if err != nil || len(ws) == 0 || ws[len(ws)-1] != final%256 {
		t.Fatalf("widths %v %v", ws, err)
	}
	if ws, _ := d.Widths(bg, 0, 0); len(ws) == 0 || ws[len(ws)-1] != TileWidth {
		t.Fatalf("widths of a full tile %v", ws)
	}
	if err := Audit(bg, d, uint64(final), ld.Root()); err != nil {
		t.Fatal(err)
	}
	// No temporary file is left.
	if n, err := d.RemoveTemporaryFiles(); err != nil || n != 0 {
		t.Fatalf("%d temporary files left: %v", n, err)
	}
	// Reopen from the frontier.
	l2, err := Open(bg, testOrigin(1), d, ld.Frontier().Marshal(), uint64(final), ld.Root(), Options{})
	if err != nil || l2.Root() != ld.Root() {
		t.Fatalf("reopen: %v", err)
	}
}

func TestDirStoreDeepPathsAndLimits(t *testing.T) {
	d := newDirStore(t)
	h, _ := EncodeTile([]Hash{{1}, {2}})
	for _, k := range []TileKey{{0, 1234067, 2}, {3, 999999999, 2}, {0, 1, 2}} {
		if err := d.PutTile(bg, k, h); err != nil {
			t.Fatalf("%s: %v", k.Path(), err)
		}
		got, err := d.GetTile(bg, k)
		if err != nil || !bytes.Equal(got, h) {
			t.Fatalf("%s: %v", k.Path(), err)
		}
	}
	if _, err := os.Stat(filepath.Join(d.Root(), "tile", "0", "x001", "x234", "067.p", "2")); err != nil {
		t.Fatal(err)
	}
	// Invalid keys and content.
	for _, k := range []TileKey{{0, 0, 0}, {0, 0, 257}, {-2, 0, 1}, {64, 0, 1}} {
		if err := d.PutTile(bg, k, h); err == nil {
			t.Errorf("%+v stored", k)
		}
		if _, err := d.GetTile(bg, k); err == nil {
			t.Errorf("%+v read", k)
		}
	}
	if err := d.PutTile(bg, TileKey{0, 5, 3}, h); err == nil {
		t.Error("a tile of the wrong size stored")
	}
	if _, err := d.GetTile(bg, TileKey{0, 77, 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
	// A conflicting put changes nothing.
	other, _ := EncodeTile([]Hash{{9}, {9}})
	if err := d.PutTile(bg, TileKey{0, 1, 2}, other); !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
	if got, _ := d.GetTile(bg, TileKey{0, 1, 2}); !bytes.Equal(got, h) {
		t.Fatal("the tile changed")
	}
	if err := d.PutTile(bg, TileKey{0, 1, 2}, h); err != nil {
		t.Fatalf("an identical put: %v", err)
	}
}

func TestDirStoreRefusesWhatIsNotATile(t *testing.T) {
	d := newDirStore(t)
	k := TileKey{0, 0, 2}
	good, _ := EncodeTile([]Hash{{1}, {2}})
	if err := d.PutTile(bg, k, good); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d.Root(), filepath.FromSlash(k.Path()))
	// A file of the wrong size, and of content that is not a tile.
	if err := os.WriteFile(p, good[:40], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetTile(bg, k); err == nil {
		t.Error("a short tile file was read")
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 100000), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetTile(bg, k); err == nil {
		t.Error("an oversize tile file was read")
	}
	// A directory in place of a file.
	_ = os.Remove(p)
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetTile(bg, k); err == nil {
		t.Error("a directory was read as a tile")
	}
	_ = os.Remove(p)
	// An empty entry bundle and a checkpoint over the limit.
	if err := d.PutTile(bg, TileKey{EntryLevel, 0, 1}, nil); err == nil {
		t.Error("an empty bundle stored")
	}
	if err := d.PutCheckpoint(bg, nil); err == nil {
		t.Error("an empty checkpoint stored")
	}
	if err := d.PutCheckpoint(bg, make([]byte, MaxNoteSize+1)); err == nil {
		t.Error("an oversize checkpoint stored")
	}
	if _, err := NewDirStore(""); err == nil {
		t.Error("an empty directory name")
	}
}

func TestCheckpointFile(t *testing.T) {
	d := newDirStore(t)
	if _, err := d.GetCheckpoint(bg); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	k := testKey(t, 1)
	n1, _ := SignCheckpoint(bg, goodCheckpoint(), k)
	if err := d.PutCheckpoint(bg, n1); err != nil {
		t.Fatal(err)
	}
	c2 := goodCheckpoint()
	c2.Size = 18
	n2, _ := SignCheckpoint(bg, c2, k)
	if err := d.PutCheckpoint(bg, n2); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetCheckpoint(bg)
	if err != nil || !bytes.Equal(got, n2) {
		t.Fatalf("%v", err)
	}
	// The file is named checkpoint at the root of the log directory.
	if b, err := os.ReadFile(filepath.Join(d.Root(), "checkpoint")); err != nil || !bytes.Equal(b, n2) {
		t.Fatal("the checkpoint file")
	}
	// A note of exactly MaxNoteSize bytes can be stored and read.
	big := bytes.Repeat([]byte("x"), MaxNoteSize)
	if err := d.PutCheckpoint(bg, big); err != nil {
		t.Fatal(err)
	}
	if got, err := d.GetCheckpoint(bg); err != nil || len(got) != MaxNoteSize {
		t.Fatalf("%v", err)
	}
}

// ---- crashes ----

// pubState is what a publisher keeps between runs for one log: the directory
// of the log and the file with the frontier. The checkpoint is in the
// directory.
type pubState struct {
	store    *DirStore
	frontier string
	signer   ledgerkeys.Signer
	origin   string
}

var errCrash = errors.New("crash")

// round is one round of the publisher: append, save the frontier, then sign and
// store the checkpoint. stopAfter ends it after a phase as a crash would: 1
// after the tiles, 2 after the frontier file, 3 after the checkpoint.
func (p *pubState) round(ctx context.Context, l *Log, from, to, stopAfter int) error {
	a, err := l.Append(ctx, entriesRange(from, to))
	if err != nil {
		return err
	}
	if stopAfter == 1 {
		return errCrash
	}
	if err := SaveFrontierFile(p.frontier, l.Frontier()); err != nil {
		return err
	}
	if stopAfter == 2 {
		return errCrash
	}
	note, err := SignCheckpoint(ctx, Checkpoint{Origin: p.origin, Size: a.Size, Root: a.Root}, p.signer)
	if err != nil {
		return err
	}
	if err := p.store.PutCheckpoint(ctx, note); err != nil {
		return err
	}
	if stopAfter == 3 {
		return errCrash
	}
	return nil
}

// restart opens the log the way a publisher does after a start: from the
// checkpoint that was stored (size and root), the frontier file, and the tiles.
// It returns the log and the number of leaves that the stored checkpoint and
// frontier say are done.
func (p *pubState) restart(t *testing.T, allowAhead bool) (*Log, error) {
	t.Helper()
	data, ferr := os.ReadFile(p.frontier)
	var fr Frontier
	if ferr == nil {
		var err error
		if fr, err = ParseFrontier(data); err != nil {
			t.Fatalf("the frontier file is damaged after a crash: %v", err)
		}
	}
	// The frontier is the state; its own root is what is stored with it. (A
	// publisher keeps size and root in the same row; here the frontier file is
	// atomic, so the pair is always consistent.)
	return Open(bg, p.origin, p.store, fr.Marshal(), fr.Size(), fr.Root(), Options{AllowTilesAhead: allowAhead})
}

func TestCrashAtEveryStepOfAWrite(t *testing.T) {
	rounds := [][2]int{{0, 40}, {40, 41}, {41, 90}}
	total := 90
	if longTests() {
		// Spans a full tile: the tile is written, the next partial one starts.
		rounds = [][2]int{{0, 260}, {260, 261}, {261, 520}}
		total = 520
	} else {
		t.Log("the crash points of a log that spans a full tile (520 leaves) run with KINGRES_LONG_TESTS=1; 90 leaves run here")
	}
	ref := NewMemStore()
	lr, _ := Create(bg, testOrigin(3), ref)
	mustAppend(t, lr, 0, total)
	signer := testKey(t, 5)
	// How many hook calls does an undisturbed run make?
	count := func() int {
		n := 0
		d := newDirStore(t)
		d.Hook = func(string) error { n++; return nil }
		p := &pubState{store: d, frontier: filepath.Join(t.TempDir(), "frontier"), signer: signer, origin: testOrigin(3)}
		l, _ := Create(bg, p.origin, d)
		for _, r := range rounds {
			if err := p.round(bg, l, r[0], r[1], 0); err != nil {
				t.Fatal(err)
			}
		}
		return n
	}()
	if count < 20 {
		t.Fatalf("only %d hook calls", count)
	}
	for crashAt := 1; crashAt <= count; crashAt++ {
		for _, stop := range []int{0, 1, 2, 3} {
			if stop != 0 && crashAt > 1 {
				continue // the phases are tried once, with the first hook call
			}
			d := newDirStore(t)
			calls := 0
			d.Hook = func(step string) error {
				calls++
				if stop == 0 && calls == crashAt {
					return errCrash
				}
				return nil
			}
			p := &pubState{store: d, frontier: filepath.Join(t.TempDir(), "frontier"), signer: signer, origin: testOrigin(3)}
			l, _ := Create(bg, p.origin, d)
			done := 0 // rounds that finished
			var crashed bool
			for i, r := range rounds {
				s := 0
				if stop != 0 && i == 1 {
					s = stop
				}
				if err := p.round(bg, l, r[0], r[1], s); err != nil {
					if !errors.Is(err, errCrash) {
						t.Fatalf("crash %d/%d: %v", crashAt, stop, err)
					}
					crashed = true
					break
				}
				done = i + 1
			}
			if stop == 0 && !crashed {
				t.Fatalf("crash %d did not happen", crashAt)
			}
			d.Hook = nil
			// After the crash: the state on disk loads, or the load says the tiles
			// are ahead (never that the frontier is ahead or the tiles are wrong).
			l2, err := p.restart(t, false)
			if err != nil {
				if !errors.Is(err, ErrTilesAhead) {
					t.Fatalf("crash %d/%d: the load fails with %v, expected success or ErrTilesAhead", crashAt, stop, err)
				}
				if l2, err = p.restart(t, true); err != nil {
					t.Fatalf("crash %d/%d: %v", crashAt, stop, err)
				}
			}
			// Every tile on disk is whole and the log up to the frontier audits.
			if err := Audit(bg, d, l2.Size(), l2.Root()); err != nil {
				t.Fatalf("crash %d/%d: audit of the saved state: %v", crashAt, stop, err)
			}
			// A checkpoint on disk is never ahead of the frontier, and verifies.
			if note, err := d.GetCheckpoint(bg); err == nil {
				c, _, err := VerifyCheckpoint(note, p.origin, signer.Public())
				if err != nil {
					t.Fatalf("crash %d/%d: the checkpoint on disk: %v", crashAt, stop, err)
				}
				if c.Size > l2.Size() {
					t.Fatalf("crash %d/%d: the checkpoint (%d) is ahead of the frontier (%d)", crashAt, stop, c.Size, l2.Size())
				}
			}
			// Roll the rounds again from the start of the unfinished one: identical
			// tiles are not an error, and the log ends where the reference does.
			if _, err := d.RemoveTemporaryFiles(); err != nil {
				t.Fatal(err)
			}
			for i := done; i < len(rounds); i++ {
				r := rounds[i]
				if l2.Size() >= uint64(r[1]) {
					// The frontier already holds this round: only the rest of the
					// round (the checkpoint) is to be done.
					note, err := SignCheckpoint(bg, Checkpoint{Origin: p.origin, Size: l2.Size(), Root: l2.Root()}, signer)
					if err != nil {
						t.Fatal(err)
					}
					if err := d.PutCheckpoint(bg, note); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if l2.Size() != uint64(r[0]) {
					t.Fatalf("crash %d/%d: the frontier has %d leaves, a round starts at %d", crashAt, stop, l2.Size(), r[0])
				}
				if err := p.round(bg, l2, r[0], r[1], 0); err != nil {
					t.Fatalf("crash %d/%d: redo of round %d: %v", crashAt, stop, i, err)
				}
			}
			if l2.Root() != lr.Root() || l2.Size() != uint64(total) {
				t.Fatalf("crash %d/%d: ends at %d leaves with another root", crashAt, stop, l2.Size())
			}
			if err := Audit(bg, d, uint64(total), lr.Root()); err != nil {
				t.Fatalf("crash %d/%d: final audit: %v", crashAt, stop, err)
			}
		}
	}
}

func TestCrashInTheFirstRoundOpensFromTheEmptyFrontier(t *testing.T) {
	d := newDirStore(t)
	p := &pubState{store: d, frontier: filepath.Join(t.TempDir(), "frontier"), signer: testKey(t, 1), origin: testOrigin(4)}
	l, _ := Create(bg, p.origin, d)
	if err := p.round(bg, l, 0, 100, 1); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	if _, err := Create(bg, p.origin, d); !errors.Is(err, ErrTilesAhead) {
		t.Fatalf("create over tiles that a crash left: %v", err)
	}
	empty := Frontier{}
	if _, err := Open(bg, p.origin, d, empty.Marshal(), 0, EmptyRoot(), Options{}); !errors.Is(err, ErrTilesAhead) {
		t.Fatalf("open: %v", err)
	}
	l2, err := Open(bg, p.origin, d, empty.Marshal(), 0, EmptyRoot(), Options{AllowTilesAhead: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.round(bg, l2, 0, 100, 0); err != nil {
		t.Fatal(err)
	}
	// Other leaves than the ones that were stored are a conflict, never a repair.
	d2 := newDirStore(t)
	l3, _ := Create(bg, p.origin, d2)
	if _, err := l3.Append(bg, entriesRange(0, 100)); err != nil {
		t.Fatal(err)
	}
	l4, err := Open(bg, p.origin, d2, empty.Marshal(), 0, EmptyRoot(), Options{AllowTilesAhead: true})
	if err != nil {
		t.Fatal(err)
	}
	other := entriesRange(1000, 1100)
	if _, err := l4.Append(bg, other); !errors.Is(err, ErrConflict) {
		t.Fatalf("a divergent append: %v", err)
	}
	if l4.Size() != 0 {
		t.Fatal("the log moved")
	}
}

func TestFrontierAheadOfTheTilesIsReported(t *testing.T) {
	d := newDirStore(t)
	l, _ := Create(bg, testOrigin(5), d)
	a := mustAppend(t, l, 0, 700)
	frontierPath := filepath.Join(t.TempDir(), "f")
	if err := SaveFrontierFile(frontierPath, l.Frontier()); err != nil {
		t.Fatal(err)
	}
	// Lose the tiles of the right edge (an older copy of the directory).
	if err := os.Remove(filepath.Join(d.Root(), filepath.FromSlash(TileKey{0, 2, 188}.Path()))); err != nil {
		t.Fatal(err)
	}
	fr, err := LoadFrontierFile(frontierPath, a.Size, a.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(bg, testOrigin(5), d, fr.Marshal(), a.Size, a.Root, Options{}); !errors.Is(err, ErrFrontierAhead) {
		t.Fatalf("%v", err)
	}
	// The frontier file is checked against the stored root.
	if _, err := LoadFrontierFile(frontierPath, a.Size, flipBit(a.Root, 0)); !errors.Is(err, ErrFrontierRoot) {
		t.Fatalf("%v", err)
	}
	if _, err := LoadFrontierFile(filepath.Join(t.TempDir(), "none"), 0, EmptyRoot()); err == nil {
		t.Fatal("a missing file")
	}
	big := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(big, make([]byte, MaxFrontierSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrontierFile(big, 0, EmptyRoot()); err == nil {
		t.Fatal("an oversize file")
	}
}

func TestRemoveTemporaryFiles(t *testing.T) {
	d := newDirStore(t)
	d.Hook = func(step string) error {
		if step == "temp-written" {
			return errCrash
		}
		return nil
	}
	h, _ := EncodeTile([]Hash{{1}})
	if err := d.PutTile(bg, TileKey{0, 0, 1}, h); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	if _, err := d.GetTile(bg, TileKey{0, 0, 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a crash before the publish left a tile: %v", err)
	}
	var leftover []string
	_ = filepath.WalkDir(d.Root(), func(p string, e os.DirEntry, _ error) error {
		if e != nil && strings.HasPrefix(e.Name(), ".tmp-") {
			leftover = append(leftover, p)
		}
		return nil
	})
	if len(leftover) != 1 {
		t.Fatalf("temporary files: %v", leftover)
	}
	if n, err := d.RemoveTemporaryFiles(); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	d.Hook = nil
	if err := d.PutTile(bg, TileKey{0, 0, 1}, h); err != nil {
		t.Fatal(err)
	}
}

func TestDirStoreConcurrentIdenticalPuts(t *testing.T) {
	d := newDirStore(t)
	h, _ := EncodeTile([]Hash{{1}, {2}, {3}})
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { errs <- d.PutTile(bg, TileKey{0, 9, 3}, h) }()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if runtime.GOOS == "linux" {
		fi, _ := os.Stat(filepath.Join(d.Root(), "tile", "0", "009.p", "3"))
		if fi == nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("mode of a tile file: %v", fi)
		}
	}
	_ = fmt.Sprint
}

func TestDirStoreRefusesLinks(t *testing.T) {
	d := newDirStore(t)
	k := TileKey{0, 0, 2}
	good, _ := EncodeTile([]Hash{{1}, {2}})
	if err := d.PutTile(bg, k, good); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d.Root(), filepath.FromSlash(k.Path()))
	_ = os.Remove(p)
	// A link: to a file, and a link on the way to a directory.
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, good, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Logf("no symbolic links here: %v", err)
		return
	}
	if _, err := d.GetTile(bg, k); err == nil {
		t.Error("a link was read as a tile")
	}
	_ = os.Remove(p)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(d.Root(), "tile", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(d.Root(), "tile", "5")); err != nil {
		t.Fatal(err)
	}
	if err := d.PutTile(bg, TileKey{5, 0, 2}, good); err == nil {
		t.Error("a tile was written through a link")
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Error("something was written through the link")
	}
	if _, err := d.GetTile(bg, TileKey{5, 0, 2}); err == nil {
		t.Error("a tile was read through a link")
	}
}
