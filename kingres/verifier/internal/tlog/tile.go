// SPDX-License-Identifier: AGPL-3.0-only

// Package tlog reads Merkle tree hashes out of tiles as C2SP "tlog-tiles"
// defines them (https://c2sp.org/tlog-tiles) and builds and checks the
// proofs of RFC 6962 / RFC 9162 from them. It never trusts a tile: every
// root and proof it computes is compared by the caller with a signed
// checkpoint.
package tlog

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
)

// Tile geometry (tlog-tiles, "Merkle Tree"): a tile is a subtree of height 8,
// so a full tile has 256 hashes of 32 bytes.
const (
	TileHeight = 8
	TileWidth  = 1 << TileHeight
	HashSize   = 32
	FullTile   = TileWidth * HashSize

	// MaxLevel is the largest tile level in a path.
	MaxLevel = 63
	// MaxTreeSize is the largest tree size accepted. A size below 2^62 keeps
	// every index computation in uint64 without overflow.
	MaxTreeSize = uint64(1) << 62
)

// PartialWidth returns the number of hashes of the partial tile at level for
// a tree of size, floor(size / 256^level) mod 256. Zero means no partial
// tile exists ("Empty tiles MUST NOT be served").
func PartialWidth(size uint64, level int) int {
	if level < 0 || level > MaxLevel || TileHeight*level >= 64 {
		return 0
	}
	return int((size >> (TileHeight * uint(level))) % TileWidth)
}

// TilePath returns the path of a tile below the log prefix:
// tile/<L>/<N>[.p/<W>]. The index is written in three digit groups, every
// group but the last prefixed with "x". A width of TileWidth (or more) is a
// full tile and gets no suffix.
func TilePath(level int, n uint64, width int) string {
	return "tile/" + strconv.Itoa(level) + "/" + indexPath(n, width)
}

// EntriesPath returns the path of an entry bundle: tile/entries/<N>[.p/<W>].
func EntriesPath(n uint64, width int) string {
	return "tile/entries/" + indexPath(n, width)
}

func indexPath(n uint64, width int) string {
	var groups []string
	for {
		groups = append([]string{fmt.Sprintf("%03d", n%1000)}, groups...)
		n /= 1000
		if n == 0 {
			break
		}
	}
	for i := 0; i < len(groups)-1; i++ {
		groups[i] = "x" + groups[i]
	}
	p := strings.Join(groups, "/")
	if width > 0 && width < TileWidth {
		p += ".p/" + strconv.Itoa(width)
	}
	return p
}

// ErrBadPath is returned for a path that is not a canonical tile path.
var ErrBadPath = errors.New("not a tile path")

// ParsedPath is the content of a tile path.
type ParsedPath struct {
	Level   int
	Index   uint64
	Width   int // TileWidth for a full tile
	Entries bool
}

// ParseTilePath parses a path as TilePath and EntriesPath write it, and only
// that form: no leading zeros in the level or the width, a level of at most
// 63, a width of 1 to 255, three digit groups with the "x" prefix on all but
// the last, no zero first group unless it is the only one, no ".." and no
// other element. Formatting the result gives the input back.
func ParseTilePath(p string) (ParsedPath, error) {
	var out ParsedPath
	fail := func(why string) (ParsedPath, error) {
		return ParsedPath{}, fmt.Errorf("%w: %q: %s", ErrBadPath, clip(p), why)
	}
	parts := strings.Split(p, "/")
	if len(parts) < 3 || parts[0] != "tile" {
		return fail("expected tile/<L>/<N>")
	}
	if parts[1] == "entries" {
		out.Entries = true
	} else {
		l, ok := canonicalDecimal(parts[1], MaxLevel)
		if !ok {
			return fail("level must be a decimal integer from 0 to 63 without leading zeros")
		}
		out.Level = int(l)
	}
	rest := parts[2:]
	out.Width = TileWidth
	// A partial tile ends in "<group>.p/<W>".
	if len(rest) >= 2 && strings.HasSuffix(rest[len(rest)-2], ".p") {
		w, ok := canonicalDecimal(rest[len(rest)-1], TileWidth-1)
		if !ok || w == 0 {
			return fail("width must be a decimal integer from 1 to 255 without leading zeros")
		}
		out.Width = int(w)
		rest = append(append([]string{}, rest[:len(rest)-2]...), strings.TrimSuffix(rest[len(rest)-2], ".p"))
	}
	if len(rest) == 0 || len(rest) > 7 {
		return fail("the index needs one to seven groups")
	}
	var n uint64
	for i, g := range rest {
		last := i == len(rest)-1
		if !last {
			if !strings.HasPrefix(g, "x") {
				return fail("every group but the last must start with x")
			}
			g = g[1:]
		}
		if len(g) != 3 || g[0] < '0' || g[0] > '9' || g[1] < '0' || g[1] > '9' || g[2] < '0' || g[2] > '9' {
			return fail("a group must be three digits")
		}
		v := uint64(g[0]-'0')*100 + uint64(g[1]-'0')*10 + uint64(g[2]-'0')
		if i == 0 && v == 0 && len(rest) > 1 {
			return fail("the first group must not be zero unless it is the only one")
		}
		if n > (^uint64(0)-v)/1000 {
			return fail("the index does not fit in 64 bits")
		}
		n = n*1000 + v
	}
	out.Index = n
	return out, nil
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// canonicalDecimal parses digits without a sign or leading zeros, at most max.
func canonicalDecimal(s string, max uint64) (uint64, bool) {
	if s == "" || (len(s) > 1 && s[0] == '0') || len(s) > 3 {
		return 0, false
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		v = v*10 + uint64(s[i]-'0')
	}
	return v, v <= max
}

// ParseTile splits the content of a tile of the given width into hashes. The
// length must be exactly width * 32 bytes: a tile of any other size is
// rejected, never truncated or padded.
func ParseTile(data []byte, width int) ([]merkle.Hash, error) {
	if width < 1 || width > TileWidth {
		return nil, fmt.Errorf("tile width %d is outside 1 to %d", width, TileWidth)
	}
	if len(data) != width*HashSize {
		return nil, fmt.Errorf("tile has %d bytes, expected %d for a width of %d", len(data), width*HashSize, width)
	}
	hs := make([]merkle.Hash, width)
	for i := range hs {
		copy(hs[i][:], data[i*HashSize:])
	}
	return hs, nil
}

// ParseEntryBundle splits an entry bundle (tlog-tiles, "Log entries") into
// entries: a sequence of big endian uint16 length prefixed entries. It
// rejects a truncated length or entry, trailing bytes and more than 256
// entries, and, when want is positive, a count different from want.
func ParseEntryBundle(data []byte, want int) ([][]byte, error) {
	var out [][]byte
	for len(data) > 0 {
		if len(data) < 2 {
			return nil, errors.New("entry bundle ends inside a length prefix")
		}
		n := int(data[0])<<8 | int(data[1])
		data = data[2:]
		if n > len(data) {
			return nil, fmt.Errorf("entry bundle entry of %d bytes is cut off after %d bytes", n, len(data))
		}
		if len(out) == TileWidth {
			return nil, fmt.Errorf("entry bundle has more than %d entries", TileWidth)
		}
		out = append(out, data[:n:n])
		data = data[n:]
	}
	if want > 0 && len(out) != want {
		return nil, fmt.Errorf("entry bundle has %d entries, expected %d", len(out), want)
	}
	return out, nil
}
