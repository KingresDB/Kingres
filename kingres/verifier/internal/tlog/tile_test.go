// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package tlog

import (
	"bytes"
	"errors"
	"testing"
)

func TestTilePathSpecExamples(t *testing.T) {
	// tlog-tiles: "index 1234067 will be encoded as x001/x234/067".
	if got := TilePath(0, 1234067, TileWidth); got != "tile/0/x001/x234/067" {
		t.Fatalf("got %s", got)
	}
	cases := []struct {
		level int
		n     uint64
		width int
		want  string
	}{
		{0, 0, 256, "tile/0/000"},
		{0, 7, 256, "tile/0/007"},
		{1, 999, 256, "tile/1/999"},
		{2, 1000, 256, "tile/2/x001/000"},
		{3, 1234067, 17, "tile/3/x001/x234/067.p/17"},
		{63, 5, 1, "tile/63/005.p/1"},
		{0, 18446744073709551615, 255, "tile/0/x018/x446/x744/x073/x709/x551/615.p/255"},
	}
	for _, c := range cases {
		if got := TilePath(c.level, c.n, c.width); got != c.want {
			t.Errorf("TilePath(%d,%d,%d) = %s, want %s", c.level, c.n, c.width, got, c.want)
		}
		p, err := ParseTilePath(c.want)
		if err != nil || p.Level != c.level || p.Index != c.n || p.Width != min(c.width, TileWidth) || p.Entries {
			t.Errorf("ParseTilePath(%s) = %+v, %v", c.want, p, err)
		}
	}
	if got := EntriesPath(1234067, 255); got != "tile/entries/x001/x234/067.p/255" {
		t.Fatalf("got %s", got)
	}
	if p, err := ParseTilePath("tile/entries/x001/x234/067.p/255"); err != nil || !p.Entries || p.Index != 1234067 || p.Width != 255 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestPartialWidths(t *testing.T) {
	// tlog-tiles: a tree of size 70,000 has 273 full level 0 tiles, a partial
	// level 0 tile of width 112, a level 1 partial of width 17 and a level 2
	// partial of width 1; a tree of 256 has no partial level 0 tile and a
	// partial level 1 tile of width 1.
	for level, want := range []int{112, 17, 1, 0} {
		if got := PartialWidth(70000, level); got != want {
			t.Errorf("size 70000 level %d: %d, want %d", level, got, want)
		}
	}
	if PartialWidth(256, 0) != 0 || PartialWidth(256, 1) != 1 {
		t.Error("size 256")
	}
	if PartialWidth(1<<62, 8) != 0 || PartialWidth(5, 99) != 0 || PartialWidth(5, -1) != 0 {
		t.Error("out of range levels")
	}
	// Property for sizes 255 to 66000: the widths are the base 256 digits.
	for s := uint64(255); s <= 66000; s++ {
		d0, d1, d2 := s%256, (s/256)%256, s/65536
		if uint64(PartialWidth(s, 0)) != d0 || uint64(PartialWidth(s, 1)) != d1 || uint64(PartialWidth(s, 2)) != d2 {
			t.Fatalf("size %d", s)
		}
	}
}

func TestParseTilePathRejections(t *testing.T) {
	bad := []string{
		"", "tile", "tile/0", "tile/0/", "/tile/0/000", "tile/0/000/", "tile/0//000",
		"tile/00/000", "tile/01/000", "tile/64/000", "tile/-1/000", "tile/a/000", "tile/1000/000", "tile/0x1/000",
		"tile/0/00", "tile/0/0000", "tile/0/00a", "tile/0/x000", "tile/0/001/002", "tile/0/x001/x234",
		"tile/0/x000/001", "tile/0/x000/x000/001", "tile/0/X001/234", "tile/0/x001/x234/067/", "tile/0/x001/x234/067/068",
		"tile/0/000.p", "tile/0/000.p/", "tile/0/000.p/0", "tile/0/000.p/256", "tile/0/000.p/01", "tile/0/000.p/-1", "tile/0/000.p/1/2",
		"tile/0/000.q/5", "tile/0/000.P/5",
		"tile/../0/000", "tile/0/../000", "tile/0/000/..", "../tile/0/000", "tile/0/x001/../067", "tile/0/./000",
		"tile/entries", "tile/entries/", "tile/entries/00", "tile/entries/..", "tile/entries/x001/../000",
		"TILE/0/000", "tile/0/000\x00", "tile/0/000 ", "tile\\0\\000", "tile/0/\u0660\u0660\u0660",
		// Eight groups cannot be a uint64 index; the value of seven groups can overflow.
		"tile/0/x001/x000/x000/x000/x000/x000/x000/000",
		"tile/0/x999/x999/x999/x999/x999/x999/999",
		"tile/0/x018/x446/x744/x073/x709/x551/616",
	}
	for _, p := range bad {
		if _, err := ParseTilePath(p); err == nil {
			t.Errorf("%q accepted", p)
		} else if !errors.Is(err, ErrBadPath) {
			t.Errorf("%q: error does not wrap ErrBadPath: %v", p, err)
		}
	}
}

func TestParseTile(t *testing.T) {
	for _, w := range []int{0, -1, 257} {
		if _, err := ParseTile(make([]byte, 64), w); err == nil {
			t.Errorf("width %d accepted", w)
		}
	}
	for _, n := range []int{0, 31, 33, 63, 65, 31 * 256, 33 * 256, 8192 + 32} {
		if _, err := ParseTile(make([]byte, n), 2); err == nil && n != 64 {
			t.Errorf("%d bytes accepted for width 2", n)
		}
	}
	if _, err := ParseTile(make([]byte, 8191), TileWidth); err == nil {
		t.Error("8191 bytes accepted for a full tile")
	}
	hs, err := ParseTile(bytes.Repeat([]byte{7}, 64), 2)
	if err != nil || len(hs) != 2 || hs[1][31] != 7 {
		t.Fatalf("%v %v", hs, err)
	}
}

func TestParseEntryBundle(t *testing.T) {
	good := []byte{0, 3, 'a', 'b', 'c', 0, 0, 0, 1, 'x'}
	es, err := ParseEntryBundle(good, 3)
	if err != nil || len(es) != 3 || string(es[0]) != "abc" || len(es[1]) != 0 || string(es[2]) != "x" {
		t.Fatalf("%q %v", es, err)
	}
	for name, in := range map[string][]byte{
		"cut length":  {0},
		"cut entry":   {0, 3, 'a', 'b'},
		"huge length": {0xff, 0xff, 1},
		"trailing":    append(append([]byte{}, good...), 0),
	} {
		if _, err := ParseEntryBundle(in, 0); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := ParseEntryBundle(good, 2); err == nil {
		t.Error("wrong count accepted")
	}
	many := bytes.Repeat([]byte{0, 0}, 257)
	if _, err := ParseEntryBundle(many, 0); err == nil {
		t.Error("257 entries accepted")
	}
	if es, err := ParseEntryBundle(many[:512], 256); err != nil || len(es) != 256 {
		t.Errorf("256 entries: %v", err)
	}
}

// FuzzParseTilePath checks that a path that parses is canonical: formatting
// it again gives the same string.
func FuzzParseTilePath(f *testing.F) {
	for _, s := range []string{"tile/0/x001/x234/067", "tile/3/005.p/17", "tile/entries/x001/002.p/255", "tile/../0", "tile/0/000"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, err := ParseTilePath(s)
		if err != nil {
			return
		}
		var again string
		if p.Entries {
			again = EntriesPath(p.Index, p.Width)
		} else {
			again = TilePath(p.Level, p.Index, p.Width)
		}
		if again != s {
			t.Fatalf("%q parsed as %+v and formats as %q", s, p, again)
		}
	})
}

// FuzzParseTile checks that the tile and bundle parsers never panic and that
// the sizes they accept are exact.
func FuzzParseTile(f *testing.F) {
	f.Add(bytes.Repeat([]byte{1}, 64), 2)
	f.Add([]byte{0, 3, 'a', 'b', 'c'}, 1)
	f.Fuzz(func(t *testing.T, data []byte, width int) {
		if hs, err := ParseTile(data, width); err == nil && (len(hs) != width || len(data) != width*HashSize) {
			t.Fatalf("accepted %d bytes as width %d", len(data), width)
		}
		if es, err := ParseEntryBundle(data, 0); err == nil {
			total := 0
			for _, e := range es {
				total += 2 + len(e)
			}
			if total != len(data) {
				t.Fatalf("bundle lengths add up to %d of %d bytes", total, len(data))
			}
		}
	})
}
