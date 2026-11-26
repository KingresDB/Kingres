// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerround

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

func openAggregate(logs []ledgerpub.LogRow, db string) *ledgerpub.LogRow {
	for i := range logs {
		l := &logs[i]
		if l.Kind == ledgerpub.KindAggregate && l.Database == db && l.State != ledgerpub.StateClosed {
			return l
		}
	}
	return nil
}

// leafBytes is the canonical bytes of a leaf with these members at no seq and no time, used to compare the members of two leaves.
func membersKey(m []ledgerlog.AggregateMember) ([]byte, error) {
	return ledgerlog.AggregateEntry{Members: m}.Marshal()
}

// aggregateMembers builds the members of the next leaf from the chain logs of the database (design 5.2): every chain log that has a
// block, with the head the log holds (the last good head for a stale or diverged chain) and its flags. A closed log is flagged retired in
// the one leaf that follows its closing; the leaf after that omits it. prev is the last leaf (nil when none).
func (c *Collector) aggregateMembers(logs []ledgerpub.LogRow, db string, stale map[int32]bool, prev *ledgerlog.AggregateEntry) (members []ledgerlog.AggregateMember, retired []int64, alreadyRetired map[int64]int64, err error) {
	alreadyRetired = map[int64]int64{}
	for _, l := range logs {
		if l.Kind != ledgerpub.KindChain || l.Database != db || l.Size <= 0 {
			continue
		}
		var m ledgerlog.AggregateMember
		copy(m.ChainID[:], l.ChainID)
		m.Epoch = uint32(l.Epoch) //nolint:gosec // an epoch is at least 1 and small
		m.Height = uint64(l.Size - 1)
		copy(m.BlockHash[:], l.HeadBlockHash)
		copy(m.LogRoot[:], l.Root)
		switch l.State {
		case ledgerpub.StateActive:
			if l.ShardGroupID != nil && stale[*l.ShardGroupID] {
				m.Flags = ledgerlog.FlagStale
			}
		case ledgerpub.StateDiverged:
			m.Flags = ledgerlog.FlagDiverged
		case ledgerpub.StateClosed:
			if l.RetiredInSeq != nil {
				continue
			}
			if prev != nil {
				for _, pm := range prev.Members {
					if pm.ChainID == m.ChainID && pm.Epoch == m.Epoch && pm.Flags&ledgerlog.FlagRetired != 0 {
						alreadyRetired[l.ID] = int64(prev.Seq) //nolint:gosec // below 2^62
					}
				}
				if _, ok := alreadyRetired[l.ID]; ok {
					continue
				}
			}
			m.Flags = ledgerlog.FlagRetired
			if strings.Contains(l.Detail, wasDivergedMark) {
				// a diverged log that is retired by a drop or a removal stays flagged diverged in its retiring leaf (P8-08)
				m.Flags |= ledgerlog.FlagDiverged
			}
			retired = append(retired, l.ID)
		}
		members = append(members, m)
	}
	return members, retired, alreadyRetired, nil
}

// aggregate updates the aggregate log of the database after the chain logs of the round (design 5.4 step 3): it appends a leaf when the
// members differ from the previous leaf, then signs the checkpoint. The leaf holds the time of the round, so it is stored before it is
// appended (PutPending): a crash between the tiles and the frontier appends the same bytes again, which is the repair ledgerlog prescribes.
func (c *Collector) aggregate(ctx context.Context, pc ledgersign.PubContext, term scheduler.Term, db string, res *Result) error {
	logs, err := c.cfg.Store.Logs(ctx)
	if err != nil {
		return err
	}
	agg := openAggregate(logs, db)
	if agg != nil && agg.State == ledgerpub.StateDiverged {
		return nil
	}
	if agg != nil {
		// a leaf that was stored and not appended, or appended and not signed (a crash in the middle of an earlier round)
		if err := c.finishAggregate(ctx, pc, term, db, agg); err != nil {
			return err
		}
	}
	var prev *ledgerlog.AggregateEntry
	if agg != nil && agg.Size > 0 {
		p, err := c.lastLeaf(ctx, term, agg)
		if err != nil {
			return err
		}
		prev = &p
	}
	stale := map[int32]bool{}
	for _, g := range res.Groups {
		if g.Err != nil {
			stale[g.Group.ID] = true
		}
	}
	members, retired, already, err := c.aggregateMembers(logs, db, stale, prev)
	if err != nil {
		return err
	}
	for id, seq := range already {
		if err := c.cfg.Store.SetRetired(ctx, term, id, seq); err != nil {
			return err
		}
	}
	if len(members) > ledgerlog.MaxMembers {
		return fmt.Errorf("%w: %d chains", ErrTooManyChains, len(members))
	}
	if len(members) > 0 {
		changed := true
		if prev != nil {
			a, errA := membersKey(prev.Members)
			b, errB := membersKey(members)
			changed = errA != nil || errB != nil || !bytes.Equal(a, b)
		}
		if changed {
			if agg == nil {
				var id [32]byte
				copy(id[:], pc.ClusterID)
				origin, err := ledgerlog.AggregateOrigin(pc.Prefix, ledgerlog.AggregateID(id, db))
				if err != nil {
					return err
				}
				row, err := c.cfg.Store.CreateLog(ctx, term, ledgerpub.LogRow{Kind: ledgerpub.KindAggregate, Database: db, Epoch: 1, Origin: origin})
				if err != nil {
					return err
				}
				agg = &row
			}
			e := ledgerlog.AggregateEntry{Seq: uint64(agg.Size), GeneratedAtUS: microsSince2000(c.cfg.Clock()), Members: members} //nolint:gosec // not negative
			raw, err := e.Marshal()
			if err != nil {
				return err
			}
			if err := c.cfg.Store.PutPending(ctx, term, agg.ID, raw); err != nil {
				return err
			}
			agg.PendingEntry = raw
			if err := c.appendAggregate(ctx, pc, term, db, agg, raw); err != nil {
				return err
			}
			for _, id := range retired {
				if err := c.cfg.Store.SetRetired(ctx, term, id, int64(e.Seq)); err != nil { //nolint:gosec // below 2^62
					return err
				}
			}
		} else {
			for _, id := range retired {
				// the previous leaf already carries this member flagged retired (a crash before SetRetired)
				if err := c.cfg.Store.SetRetired(ctx, term, id, int64(prev.Seq)); err != nil { //nolint:gosec // below 2^62
					return err
				}
			}
		}
	}
	if agg != nil {
		res.AggregateSize = agg.Size
		res.AggregateHash = append([]byte(nil), agg.Root...)
		if agg.Size == 0 {
			res.AggregateSize, res.AggregateHash = 0, nil
		}
	}
	return nil
}

// finishAggregate repairs what a crash left: a pending leaf is appended (the same bytes), and a stored tree without a checkpoint is signed.
func (c *Collector) finishAggregate(ctx context.Context, pc ledgersign.PubContext, term scheduler.Term, db string, agg *ledgerpub.LogRow) error {
	if len(agg.PendingEntry) > 0 {
		pe, err := ledgerlog.ParseAggregateEntry(agg.PendingEntry)
		if err == nil && int64(pe.Seq) == agg.Size { //nolint:gosec // below 2^62
			if err := c.appendAggregate(ctx, pc, term, db, agg, agg.PendingEntry); err != nil {
				return err
			}
			return nil
		}
	}
	return c.ensureCheckpoint(ctx, pc, term, db, -1, agg, nil)
}

// appendAggregate appends one leaf to the aggregate log, stores the frontier and signs the checkpoint: the order of a round.
func (c *Collector) appendAggregate(ctx context.Context, pc ledgersign.PubContext, term scheduler.Term, db string, agg *ledgerpub.LogRow, raw []byte) error {
	lg, err := c.openStored(ctx, term, *agg)
	if err != nil {
		return fmt.Errorf("opening the aggregate log of %s: %w", db, err)
	}
	app, err := lg.Append(ctx, [][]byte{raw})
	if errors.Is(err, ledgerlog.ErrConflict) {
		if derr := c.divergeFatal(ctx, term, db, -1, *agg, "a tile of the aggregate log differs from the leaf of this round: "+err.Error()); derr != nil {
			return derr
		}
		return errDiverged
	}
	if err != nil {
		return err
	}
	if err := c.cfg.Store.SaveFrontier(ctx, term, agg.ID, int64(app.Size), app.Root[:], app.Frontier, nil); err != nil { //nolint:gosec // below 2^62
		return err
	}
	agg.Size, agg.Root, agg.Frontier, agg.PendingEntry = int64(app.Size), app.Root[:], app.Frontier, nil //nolint:gosec // below 2^62
	return c.ensureCheckpoint(ctx, pc, term, db, -1, agg, lg)
}

// lastLeaf reads the newest leaf of the aggregate log from its tiles.
func (c *Collector) lastLeaf(ctx context.Context, term scheduler.Term, agg *ledgerpub.LogRow) (ledgerlog.AggregateEntry, error) {
	r, err := ledgerlog.NewReader(ctx, c.cfg.Store.Tiles(agg.ID, term), uint64(agg.Size)) //nolint:gosec // not negative
	if err != nil {
		return ledgerlog.AggregateEntry{}, err
	}
	raw, err := r.Entry(uint64(agg.Size) - 1) //nolint:gosec // not negative
	if err != nil {
		return ledgerlog.AggregateEntry{}, fmt.Errorf("reading the last leaf of the aggregate log of %s: %w", agg.Database, err)
	}
	return ledgerlog.ParseAggregateEntry(raw)
}
