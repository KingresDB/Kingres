// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerlog

import (
	"errors"
	"fmt"
)

// Limits of entries and of what one call appends.
const (
	// MaxEntrySize is the largest entry: an entry bundle prefixes each entry
	// with a big endian u16 length.
	MaxEntrySize = 1<<16 - 1
	// MaxBundleSize is the largest entry bundle: 256 entries of the largest
	// size, each with its two byte length.
	MaxBundleSize = TileWidth * (2 + MaxEntrySize)
	// MaxBatch is the most entries one Append takes (design 4.5: at most
	// 10,000 leaves a round).
	MaxBatch = 10000
	// MaxBatchBytes is the most entry bytes one Append takes.
	MaxBatchBytes = 64 << 20
)

// ErrTileMalformed marks a tile or entry bundle that is not valid.
var ErrTileMalformed = errors.New("ledgerlog: the tile is malformed")

// EncodeTile writes hashes as a tile: the hashes one after the other. There
// must be 1 to TileWidth of them.
func EncodeTile(hs []Hash) ([]byte, error) {
	if len(hs) < 1 || len(hs) > TileWidth {
		return nil, fmt.Errorf("%w: %d hashes, expected 1 to %d", ErrTileMalformed, len(hs), TileWidth)
	}
	out := make([]byte, 0, len(hs)*HashSize)
	for _, h := range hs {
		out = append(out, h[:]...)
	}
	return out, nil
}

// ParseTile splits a tile of the given width into hashes. The length must be
// exactly width * 32 bytes: another size is refused, never truncated or padded.
func ParseTile(data []byte, width int) ([]Hash, error) {
	if width < 1 || width > TileWidth {
		return nil, fmt.Errorf("%w: width %d is outside 1 to %d", ErrTileMalformed, width, TileWidth)
	}
	if len(data) != width*HashSize {
		return nil, fmt.Errorf("%w: %d bytes, expected %d for a width of %d", ErrTileMalformed, len(data), width*HashSize, width)
	}
	hs := make([]Hash, width)
	for i := range hs {
		copy(hs[i][:], data[i*HashSize:])
	}
	return hs, nil
}

// EncodeBundle writes entries as an entry bundle (tlog-tiles): each entry
// prefixed by its length as a big endian u16. There must be 1 to TileWidth
// entries, each of at most MaxEntrySize bytes.
func EncodeBundle(entries [][]byte) ([]byte, error) {
	if len(entries) < 1 || len(entries) > TileWidth {
		return nil, fmt.Errorf("%w: %d entries, expected 1 to %d", ErrTileMalformed, len(entries), TileWidth)
	}
	n := 0
	for _, e := range entries {
		if len(e) > MaxEntrySize {
			return nil, fmt.Errorf("%w: an entry has %d bytes, expected at most %d", ErrTileMalformed, len(e), MaxEntrySize)
		}
		n += 2 + len(e)
	}
	out := make([]byte, 0, n)
	for _, e := range entries {
		out = append(out, byte(len(e)>>8), byte(len(e)))
		out = append(out, e...)
	}
	return out, nil
}

// ParseBundle splits an entry bundle into entries. It refuses an empty bundle,
// a cut length or entry, trailing bytes, more than TileWidth entries, and, when
// want is positive, a count other than want. EncodeBundle of the result gives
// the input back.
func ParseBundle(data []byte, want int) ([][]byte, error) {
	if len(data) > MaxBundleSize {
		return nil, fmt.Errorf("%w: %d bytes, expected at most %d", ErrTileMalformed, len(data), MaxBundleSize)
	}
	var out [][]byte
	for len(data) > 0 {
		if len(data) < 2 {
			return nil, fmt.Errorf("%w: the bundle ends inside a length", ErrTileMalformed)
		}
		n := int(data[0])<<8 | int(data[1])
		data = data[2:]
		if n > len(data) {
			return nil, fmt.Errorf("%w: an entry of %d bytes is cut off after %d", ErrTileMalformed, n, len(data))
		}
		if len(out) == TileWidth {
			return nil, fmt.Errorf("%w: more than %d entries", ErrTileMalformed, TileWidth)
		}
		out = append(out, data[:n:n])
		data = data[n:]
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: the bundle is empty", ErrTileMalformed)
	}
	if want > 0 && len(out) != want {
		return nil, fmt.Errorf("%w: %d entries, expected %d", ErrTileMalformed, len(out), want)
	}
	return out, nil
}

// checkTileData checks that data is a valid tile or bundle for the key.
func checkTileData(k TileKey, data []byte) error {
	if !k.Valid() {
		return fmt.Errorf("%w: the key %+v is not valid", ErrTileMalformed, k)
	}
	if k.Level == EntryLevel {
		_, err := ParseBundle(data, k.Width)
		return err
	}
	_, err := ParseTile(data, k.Width)
	return err
}
