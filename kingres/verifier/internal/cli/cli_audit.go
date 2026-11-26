// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/aggregate"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/chain"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/pubsrc"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/tlog"
)

// MaxAuditEntries bounds the entries one audit reads (a monitor that is far behind catches up in
// several runs). The memory of an audit does not depend on it: one entry bundle at a time.
var maxAuditEntries uint64 = 5_000_000

// auditResult says what an audit of the entries of a log proved.
type auditResult struct {
	from, to uint64 // entries from to to-1 were read (from is the one before the first new entry, when there is one)
	last     format.Header
	lastTime int64
	checked  uint64
}

// bindLog binds the tiles of the range to the signed root: every full tile of the entries lo to
// size-1 hashes to the entry of the next level, and the root recomputed from the tiles is the root
// of the checkpoint. A tile that was rewritten, or a signed root the tiles do not lead to, is a
// failure; so is a tile the publication does not have a hash for.
func bindLog(ol *openedLog, lo uint64) outcome {
	if ol.cp.Size == 0 {
		return outcome{}
	}
	if err := ol.reader.BindRange(lo, ol.cp.Size); err != nil {
		return tileOutcome(err, ol.rel())
	}
	root, err := ol.reader.Root()
	if err != nil {
		return tileOutcome(err, ol.rel())
	}
	if root != ol.cp.Root {
		return failedOut("tiles", fmt.Errorf("the tiles of %s lead to root %x, the signed checkpoint says %x for size %d: the tiles do not belong to the checkpoint", ol.rel(), root, ol.cp.Root, ol.cp.Size))
	}
	return outcome{}
}

// auditChainLog reads the entries of a chain log from `from` (0 for all) to the size of its
// checkpoint and checks them (design 4.1 and 7.5): the entry hash is the leaf hash in the tile, the
// entry is a block header whose height is its index, block_hash is SHA-256 of the entry (which is
// how ParseBlockLeaf makes it), prev_hash is the hash of the block before (the genesis hash for
// block 0), and block times never go back. With from > 0 the entry before it is read too, to link.
func auditChainLog(ol *openedLog, chainID [32]byte, from uint64) (*auditResult, outcome) {
	size := ol.cp.Size
	res := &auditResult{to: size}
	if from >= size {
		return res, outcome{}
	}
	start := from
	if start > 0 {
		start--
	}
	res.from = start
	limit := start + maxAuditEntries
	end := size
	if end > limit {
		end = limit
		res.to = end
	}
	if out := bindLog(ol, start); !out.ok() {
		return nil, out
	}
	var prev format.Header
	havePrev := false
	for idx := start; idx < end; {
		n := idx / 256
		es, err := entriesOf(ol, n)
		if err != nil {
			return nil, tileOutcome(err, "tile/entries")
		}
		for i := int(idx % 256); i < len(es) && idx < end; i, idx = i+1, idx+1 {
			e := es[i]
			th, err := ol.reader.LeafHash(idx)
			if err != nil {
				return nil, tileOutcome(err, ol.rel())
			}
			if merkle.LeafHash(e) != th {
				return nil, failedOut("entries", fmt.Errorf("the entry %d of %s does not hash to the leaf hash that the tile holds for it: the entry bundle or the tile was rewritten", idx, ol.rel()))
			}
			h, err := format.ParseBlockLeaf(e)
			if err != nil {
				return nil, failedOut("entries", fmt.Errorf("the entry %d of %s: %w", idx, ol.rel(), err))
			}
			if h.Height != idx {
				return nil, failedOut("entries", fmt.Errorf("the entry %d of %s is the block of height %d: the height of a block is its index", idx, ol.rel(), h.Height))
			}
			switch {
			case idx == 0:
				if want := format.GenesisHash(chainID); h.PrevHash != want {
					return nil, failedOut("entries", fmt.Errorf("block 0 of %s has prev_hash %x, expected %x (SHA-256 of 0x14 and the chain id)", ol.rel(), h.PrevHash, want))
				}
			case havePrev:
				if err := chain.CheckLink(prev, h); err != nil {
					return nil, failedOut("entries", fmt.Errorf("%s: %w", ol.rel(), err))
				}
			}
			prev, havePrev = h, true
			res.checked++
		}
	}
	res.last = prev
	return res, outcome{}
}

// entriesOf reads entry bundle n of the log at the size of its checkpoint (entries are at most as long as the longest leaf of either kind).
func entriesOf(ol *openedLog, n uint64) ([][]byte, error) {
	max := format.BlockLeafSize
	if ol.in.spec.kind == "aggregate" {
		max = aggregate.MaxLeafSize
	}
	return entriesAt(ol, n, max)
}

// auditAggregateLog checks every leaf of an aggregate log from `from` on: it parses, its sequence
// number is its index, the leader's time does not go back, and its hash is the leaf hash in the
// tile. The members are not checked against the chain logs here (verifyAggregate does that for
// the last leaf).
func auditAggregateLog(ol *openedLog, from uint64) (*auditResult, outcome) {
	return auditAggregateLogEach(ol, from, nil)
}

// auditAggregateLogEach is auditAggregateLog with a function called for every leaf that was read.
func auditAggregateLogEach(ol *openedLog, from uint64, each func(idx uint64, l *aggregate.Leaf) outcome) (*auditResult, outcome) {
	size := ol.cp.Size
	res := &auditResult{to: size}
	if from >= size {
		return res, outcome{}
	}
	start := from
	if start > 0 {
		start--
	}
	res.from = start
	end := size
	if end > start+maxAuditEntries {
		end = start + maxAuditEntries
		res.to = end
	}
	if out := bindLog(ol, start); !out.ok() {
		return nil, out
	}
	var lastTime int64
	for idx := start; idx < end; {
		es, err := entriesOf(ol, idx/256)
		if err != nil {
			return nil, tileOutcome(err, "tile/entries")
		}
		for i := int(idx % 256); i < len(es) && idx < end; i, idx = i+1, idx+1 {
			e := es[i]
			th, err := ol.reader.LeafHash(idx)
			if err != nil {
				return nil, tileOutcome(err, ol.rel())
			}
			if merkle.LeafHash(e) != th {
				return nil, failedOut("entries", fmt.Errorf("the entry %d of %s does not hash to the leaf hash that the tile holds for it: the entry bundle or the tile was rewritten", idx, ol.rel()))
			}
			l, err := aggregate.Parse(e)
			if err != nil {
				return nil, failedOut("entries", fmt.Errorf("the entry %d of %s: %w", idx, ol.rel(), err))
			}
			if l.Seq != idx {
				return nil, failedOut("entries", fmt.Errorf("the entry %d of %s says it is round %d", idx, ol.rel(), l.Seq))
			}
			if idx > start && l.GeneratedAtUS < lastTime {
				return nil, failedOut("entries", fmt.Errorf("the round %d of %s was generated at %s, before the round before it: the leader's times went back", idx, ol.rel(), chain.FormatUS(l.GeneratedAtUS)))
			}
			lastTime = l.GeneratedAtUS
			if each != nil {
				if o := each(idx, l); !o.ok() {
					return nil, o
				}
			}
			res.checked++
		}
	}
	res.lastTime = lastTime
	return res, outcome{}
}

// maxTileFiles bounds the files the walk of the tile directories reads.
const maxTileFiles = 2_000_000

// auditLayout checks every file of the tile directories of a log, not only those the checkpoint needs:
// each must be a tile or an entry bundle of the tree of the signed checkpoint, or a partial one of an
// older size, which must be the first hashes (entries) of the tile (bundle) of the current tree. A tile that
// was rewritten and that no proof reads, or one the tree does not have, is a failure. It reads the layout
// through the source's listing: an HTTP source has none, and the second result says so.
func auditLayout(ol *openedLog) (files int, listed bool, out outcome) {
	src, err := ol.in.openSource()
	if err != nil {
		return 0, false, inputOut(err)
	}
	var paths []string
	var walk func(rel string, depth int) error
	walk = func(rel string, depth int) error {
		if depth > 8 {
			return fmt.Errorf("%s is nested deeper than a layout of tiles is", rel)
		}
		fs, err := src.Files(rel)
		if err != nil {
			return err
		}
		for _, f := range fs {
			if paths = append(paths, rel+"/"+f); len(paths) > maxTileFiles {
				return fmt.Errorf("more than %d files below %s/tile", maxTileFiles, ol.rel())
			}
		}
		dirs, _, err := src.SubDirs(rel)
		if err != nil {
			return err
		}
		for _, d := range dirs {
			if err := walk(rel+"/"+d, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(ol.rel()+"/tile", 0); err != nil {
		if errors.Is(err, pubsrc.ErrNoListing) {
			return 0, false, outcome{}
		}
		if absent(err) {
			return 0, true, outcome{}
		}
		return 0, true, pubOutcome(classifyPub(ol.rel()+"/tile", err))
	}
	max := format.BlockLeafSize
	if ol.in.spec.kind == "aggregate" {
		max = aggregate.MaxLeafSize
	}
	for _, p := range paths {
		rel := strings.TrimPrefix(p, ol.rel()+"/")
		pp, err := tlog.ParseTilePath(rel)
		if err != nil {
			return 0, true, failedOut("tiles", fmt.Errorf("%s: the layout of a log holds no such file: %w", p, err))
		}
		if pp.Entries {
			if pp.Index*tlog.TileWidth >= ol.cp.Size {
				return 0, true, failedOut("tiles", fmt.Errorf("%s: the tree of size %d has no such entry bundle", p, ol.cp.Size))
			}
			cur, err := tlog.EntriesAt(ol.files, ol.cp.Size, pp.Index, max)
			if err != nil {
				return 0, true, tileOutcome(err, p)
			}
			data, err := ol.files.ReadFile(rel, int64(pp.Width)*int64(2+max))
			if err != nil {
				return 0, true, pubOutcome(classifyPub(p, err))
			}
			es, perr := tlog.ParseEntryBundle(data, pp.Width)
			if perr != nil || pp.Width > len(cur) {
				return 0, true, failedOut("tiles", fmt.Errorf("%s is not a prefix of the entry bundle of the signed tree: %v", p, perr))
			}
			for i, e := range es {
				if string(e) != string(cur[i]) {
					return 0, true, failedOut("tiles", fmt.Errorf("%s differs from the entry bundle of the signed tree at entry %d", p, i))
				}
			}
			files++
			continue
		}
		cur, err := ol.reader.Tile(pp.Level, pp.Index)
		if err != nil {
			if errors.Is(err, tlog.ErrMissing) || errors.Is(err, tlog.ErrBadTile) {
				return 0, true, tileOutcome(err, p)
			}
			return 0, true, failedOut("tiles", fmt.Errorf("%s: the tree of size %d has no such tile (%v)", p, ol.cp.Size, err))
		}
		data, err := ol.files.ReadFile(rel, int64(pp.Width)*tlog.HashSize)
		if err != nil {
			return 0, true, pubOutcome(classifyPub(p, err))
		}
		hs, perr := tlog.ParseTile(data, pp.Width)
		if perr != nil || pp.Width > len(cur) {
			return 0, true, failedOut("tiles", fmt.Errorf("%s is not a prefix of the tile of the signed tree: %v", p, perr))
		}
		for i := range hs {
			if hs[i] != cur[i] {
				return 0, true, failedOut("tiles", fmt.Errorf("%s differs from the tile of the signed tree at hash %d", p, i))
			}
		}
		files++
	}
	return files, true, outcome{}
}
