// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// errDiverged is returned by extend after it stopped the log.
var errDiverged = errors.New("ledgerround: the log diverged")

// openStored opens the log with its stored frontier. A store that holds tiles beyond the frontier (a crash between the tiles and the
// frontier) is opened with AllowTilesAhead, and the same leaves are appended again, which is the repair ledgerlog prescribes: identical
// tiles are not an error, different ones are ErrConflict, which is a divergence. AllowTilesAhead is used for that case only, never as
// the way to open a log that failed for another reason.
func (c *Collector) openStored(ctx context.Context, term scheduler.Term, l ledgerpub.LogRow) (*ledgerlog.Log, error) {
	root, err := hash32(l.Root)
	if err != nil {
		return nil, err
	}
	tiles := c.cfg.Store.Tiles(l.ID, term)
	lg, err := ledgerlog.Open(ctx, l.Origin, tiles, l.Frontier, uint64(l.Size), root, ledgerlog.Options{}) //nolint:gosec // not negative
	if errors.Is(err, ledgerlog.ErrTilesAhead) {
		lg, err = ledgerlog.Open(ctx, l.Origin, tiles, l.Frontier, uint64(l.Size), root, ledgerlog.Options{AllowTilesAhead: true}) //nolint:gosec // not negative
	}
	return lg, err
}

// extend does one round of one chain log: the checks of design 4.7, the append, and the checkpoint of the stored size.
func (c *Collector) extend(ctx context.Context, pc ledgersign.PubContext, term scheduler.Term, db string, grp Group, l ledgerpub.LogRow, head ReadResponse) (*ledgerpub.LogRow, error) {
	g := grp.ID
	published := l.Size
	headLen := head.Height + 1
	stop := func(why string) (*ledgerpub.LogRow, error) {
		if err := c.diverge(ctx, term, db, g, l, why); err != nil {
			return nil, err
		}
		l.State, l.Detail = ledgerpub.StateDiverged, why
		return &l, errDiverged
	}
	headHash, err := hex.DecodeString(head.BlockHash)
	if err != nil || len(headHash) != 32 {
		return nil, fmt.Errorf("shard group %d answered a head hash that is not valid", g)
	}
	if headLen < published {
		return stop(fmt.Sprintf("the head of the chain is block %d, below the %d blocks that were published (a restore of an older state, or a lost tail)", head.Height, published))
	}
	lg := &l
	var opened *ledgerlog.Log
	if headLen == published {
		// Nothing new: the head block is the last published leaf (check 1 of 4.7, on the head of every round).
		if !bytes.Equal(headHash, l.HeadBlockHash) {
			return stop(fmt.Sprintf("block %d of the chain is not the block that was published at that height", head.Height))
		}
	} else {
		from, max := published-1, c.cfg.MaxBatch+1
		if published == 0 {
			from, max = 0, c.cfg.MaxBatch
		}
		resp, err := c.cfg.Source.Read(ctx, grp, ReadRequest{Op: OpEntries, Database: db, From: from, Max: max})
		if err != nil {
			return lg, &groupReadError{err: err}
		}
		if len(resp.Entries) == 0 {
			return lg, &groupReadError{err: errors.New("the blocks that the head names were not returned")}
		}
		leaves := make([][]byte, 0, len(resp.Entries))
		var prev *ledgerlog.BlockEntry
		var lastHash [32]byte
		for i, e := range resp.Entries {
			if e.Height != from+int64(i) {
				return stop(fmt.Sprintf("block %d was returned where block %d was asked for", e.Height, from+int64(i)))
			}
			be, err := ledgerlog.ParseBlockEntry(e.Entry)
			if err != nil || int64(be.Height) != e.Height { //nolint:gosec // below 2^62
				return stop(fmt.Sprintf("the entry of block %d is not a block header", e.Height))
			}
			if prev != nil {
				if err := ledgerlog.CheckLink(*prev, be); err != nil {
					return stop(err.Error())
				}
			}
			h, err := be.BlockHash()
			if err != nil {
				return stop(err.Error())
			}
			if i == 0 && published > 0 {
				// the first block read is the last published leaf: it must still be what was published (check 1 of 4.7)
				if !bytes.Equal(h[:], l.HeadBlockHash) {
					return stop(fmt.Sprintf("block %d of the chain is not the block that was published at that height", e.Height))
				}
			} else {
				leaves = append(leaves, e.Entry)
			}
			b := be
			prev, lastHash = &b, h
		}
		if len(leaves) > 0 {
			opened, err = c.openStored(ctx, term, l)
			if err != nil {
				return lg, fmt.Errorf("opening the log %s: %w", l.Origin, err)
			}
			app, err := opened.Append(ctx, leaves)
			switch {
			case errors.Is(err, ledgerlog.ErrConflict):
				return stop("a tile that was published differs from the blocks the chain has now (" + err.Error() + ")")
			case err != nil:
				return lg, err
			}
			if err := c.cfg.Store.SaveFrontier(ctx, term, l.ID, int64(app.Size), app.Root[:], app.Frontier, lastHash[:]); err != nil { //nolint:gosec // below 2^62
				return lg, err
			}
			l.Size, l.Root, l.Frontier, l.HeadBlockHash = int64(app.Size), app.Root[:], app.Frontier, lastHash[:] //nolint:gosec // below 2^62
		}
	}
	if err := c.ensureCheckpoint(ctx, pc, term, db, g, &l, opened); err != nil {
		if errors.Is(err, errDiverged) {
			l.State = ledgerpub.StateDiverged
			return &l, errDiverged
		}
		return &l, err
	}
	return &l, nil
}

// groupReadError marks a failed second read of a round (the blocks): the group counts as unread for the round.
type groupReadError struct{ err error }

func (e *groupReadError) Error() string { return "reading the blocks: " + e.err.Error() }
func (e *groupReadError) Unwrap() error { return e.err }

// ensureCheckpoint stores the signed checkpoint of the stored tree of the log when there is none: the step that completes a round, also the
// one a crash between the frontier and the checkpoint left undone. It signs only what the log is at this moment (Log.Checkpoint of the
// stored frontier): never a smaller tree than one that was signed, never a second root for a size.
func (c *Collector) ensureCheckpoint(ctx context.Context, pc ledgersign.PubContext, term scheduler.Term, db string, g int32, l *ledgerpub.LogRow, lg *ledgerlog.Log) error {
	latest, err := c.cfg.Store.LatestCheckpoint(ctx, l.ID)
	switch {
	case err == nil:
		if latest.Size > l.Size {
			return c.divergeFatal(ctx, term, db, g, *l, fmt.Sprintf("a checkpoint of size %d is stored for a log whose frontier has size %d", latest.Size, l.Size))
		}
		if latest.Size == l.Size {
			if !bytes.Equal(latest.Root, l.Root) {
				return c.divergeFatal(ctx, term, db, g, *l, fmt.Sprintf("the stored checkpoint of size %d has another root than the frontier", l.Size))
			}
			return nil
		}
	case !errors.Is(err, ledgerpub.ErrNotFound):
		return err
	}
	if l.Size == 0 {
		return nil
	}
	if lg == nil {
		if lg, err = c.openStored(ctx, term, *l); err != nil {
			return fmt.Errorf("opening the log %s: %w", l.Origin, err)
		}
	}
	cp := lg.Checkpoint()
	if cp.Size != uint64(l.Size) || !bytes.Equal(cp.Root[:], l.Root) { //nolint:gosec // not negative
		return fmt.Errorf("the log %s is at size %d and the stored frontier at %d: nothing is signed", l.Origin, cp.Size, l.Size)
	}
	if p := c.cfg.Published; p != nil {
		// what was published is not signed again with other content (P8-01), whatever the database holds
		if err := p.Allow(l.Origin, l.Size, l.Root); err != nil {
			c.publishedAlert(ctx, "A checkpoint of "+l.Origin+" would contradict what was published: "+err.Error(), true)
			return fmt.Errorf("%w: %v", ErrPublishedState, err)
		}
	}
	note, err := ledgerlog.SignCheckpoint(ctx, cp, pc.Signers...)
	if err != nil {
		return fmt.Errorf("signing the checkpoint of %s: %w", l.Origin, err)
	}
	err = c.cfg.Store.PutCheckpoint(ctx, term, ledgerpub.Checkpoint{LogID: l.ID, Size: l.Size, Root: l.Root, Note: note, SignedAt: c.cfg.Clock().UTC()},
		pc.Revision, !pc.Locked, pc.Prefix)
	if errors.Is(err, ledgerpub.ErrConflict) {
		return c.divergeFatal(ctx, term, db, g, *l, "the store refused the checkpoint: "+err.Error())
	}
	if err == nil && c.cfg.Published != nil {
		if rerr := c.cfg.Published.RecordCheckpoint(l.Origin, l.Size, l.Root); rerr != nil {
			// The checkpoint is stored and the record in memory is current, but the file is behind: that is not a crash, it is a disk that cannot be
			// written, and the record is the guard of every later signature. The round says so and stops (the next one writes it again before it
			// signs anything, ledgerstate.ErrNotWritten) and a critical alert names the cause (P7A-11).
			c.log.Error("a checkpoint was stored and could not be noted in the record of what was published", "origin", l.Origin, "error", rerr.Error())
			c.publishedAlert(ctx, "A checkpoint was stored and the record of what was published could not be written to the key directory ("+rerr.Error()+"). Nothing is signed until it can: free the disk or fix the directory.", true)
			return fmt.Errorf("%w: %v", ErrPublishedState, rerr)
		}
	}
	return err
}

// divergeFatal stops a log whose stored state contradicts itself and returns errDiverged.
func (c *Collector) divergeFatal(ctx context.Context, term scheduler.Term, db string, g int32, l ledgerpub.LogRow, why string) error {
	if err := c.diverge(ctx, term, db, g, l, why); err != nil {
		return err
	}
	return errDiverged
}
