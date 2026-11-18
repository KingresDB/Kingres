// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// EntryLevel is the Level of a TileKey that names an entry bundle
// (tile/entries/<N>), as the design's ledger_tile table writes it (level -1).
const EntryLevel = -1

// MaxLevel is the highest level a tile path may name.
const MaxLevel = 63

// TileKey names one tile or entry bundle of a log: the level (EntryLevel for
// entry bundles), the index of the tile at that level, and its width, 1 to
// TileWidth (TileWidth is a full tile).
type TileKey struct {
	Level int
	Index uint64
	Width int
}

// Valid reports whether the key is within the format.
func (k TileKey) Valid() bool {
	return k.Level >= EntryLevel && k.Level <= MaxLevel && k.Width >= 1 && k.Width <= TileWidth
}

// Full reports whether the key names a full tile.
func (k TileKey) Full() bool { return k.Width == TileWidth }

// String is the path of the key, tlog-tiles style.
func (k TileKey) String() string { return k.Path() }

// Path returns the path below the log prefix: tile/<L>/<N>[.p/<W>] or
// tile/entries/<N>[.p/<W>]. The index is written in three digit groups, every
// group but the last prefixed with "x"; a partial tile has the suffix .p/<W>.
func (k TileKey) Path() string {
	head := "tile/entries/"
	if k.Level >= 0 {
		head = "tile/" + strconv.Itoa(k.Level) + "/"
	}
	var groups []string
	n := k.Index
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
	p := head + strings.Join(groups, "/")
	if k.Width > 0 && k.Width < TileWidth {
		p += ".p/" + strconv.Itoa(k.Width)
	}
	return p
}

// ErrBadPath is returned for a path that is not a canonical tile path.
var ErrBadPath = errors.New("ledgerlog: not a canonical tile path")

// ParseTilePath parses a path as TileKey.Path writes it, and only that form: no
// leading zeros in the level or the width, a level of at most 63, a width of 1
// to 255, three digit groups with "x" on all but the last, no zero first group
// unless it is the only one, no other element. Path of the result gives the
// input back.
func ParseTilePath(p string) (TileKey, error) {
	fail := func(why string) (TileKey, error) {
		return TileKey{}, fmt.Errorf("%w: %q: %s", ErrBadPath, clipStr(p), why)
	}
	parts := strings.Split(p, "/")
	if len(parts) < 3 || parts[0] != "tile" {
		return fail("expected tile/<L>/<N>")
	}
	var k TileKey
	if parts[1] == "entries" {
		k.Level = EntryLevel
	} else {
		l, ok := canonicalDecimal(parts[1], MaxLevel)
		if !ok {
			return fail("the level must be a decimal integer from 0 to 63 without leading zeros")
		}
		k.Level = int(l)
	}
	rest := parts[2:]
	k.Width = TileWidth
	if len(rest) >= 2 && strings.HasSuffix(rest[len(rest)-2], ".p") {
		w, ok := canonicalDecimal(rest[len(rest)-1], TileWidth-1)
		if !ok || w == 0 {
			return fail("the width must be a decimal integer from 1 to 255 without leading zeros")
		}
		k.Width = int(w)
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
	k.Index = n
	return k, nil
}

func clipStr(s string) string {
	if len(s) > 80 {
		return strings.ToValidUTF8(s[:80], "?") + "..."
	}
	return strings.ToValidUTF8(s, "?")
}

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

// MaxRequiredSize is the largest tree size Required accepts: 2^28 leaves, for
// which it lists about two million keys (some 50 MB). A larger size is refused
// with ErrTooLarge instead of allocating in proportion to a number that a caller
// may have read from a file.
const MaxRequiredSize = uint64(1) << 28

// Required returns the keys of every tile and entry bundle that a reader of the
// tree of the given size may ask for: the full tiles and the partial tile of
// each level, and the entry bundles. Level 0 and EntryLevel come first. A size
// above MaxRequiredSize is refused with ErrTooLarge.
func Required(size uint64) ([]TileKey, error) {
	if size > MaxRequiredSize {
		return nil, fmt.Errorf("%w: size %d, Required lists at most %d", ErrTooLarge, size, MaxRequiredSize)
	}
	var out []TileKey
	add := func(level int, count uint64) {
		for n := uint64(0); n < count/TileWidth; n++ {
			out = append(out, TileKey{level, n, TileWidth})
		}
		if w := int(count % TileWidth); w > 0 {
			out = append(out, TileKey{level, count / TileWidth, w})
		}
	}
	add(EntryLevel, size)
	for l := 0; l <= maxLevel+1 && TileHeight*l < 64; l++ {
		c := size >> (TileHeight * uint(l))
		if c == 0 {
			break
		}
		add(l, c)
	}
	return out, nil
}
