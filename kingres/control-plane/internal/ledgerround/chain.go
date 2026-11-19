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

// openLog returns the open (not closed) chain log of a group of a database, nil when there is none.
func openLog(logs []ledgerpub.LogRow, db string, group int32) *ledgerpub.LogRow {
	for i := range logs {
		l := &logs[i]
		if l.Kind == ledgerpub.KindChain && l.Database == db && l.ShardGroupID != nil && *l.ShardGroupID == group && l.State != ledgerpub.StateClosed {
			return l
		}
	}
	return nil
}

func hasClosed(logs []ledgerpub.LogRow, chainID []byte) bool {
	for _, l := range logs {
		if l.Kind == ledgerpub.KindChain && bytes.Equal(l.ChainID, chainID) && l.State == ledgerpub.StateClosed {
			return true
		}
	}
	return false
}

// chain updates the chain log of one group from what the round read (design 4.5 and 4.7): the checks of divergence, then the order of a
// round of ledgerlog (append the tiles, store the frontier with its size and root, sign, store the checkpoint). Every call is idempotent,
// so a crash at any write is repaired by the next round.
func (c *Collector) chain(ctx context.Context, pc *ledgersign.PubContext, term scheduler.Term, db string, gr *GroupResult, logs []ledgerpub.LogRow, res *Result) error {
	g := gr.Group.ID
	existing := openLog(logs, db, g)
	if gr.Err != nil {
		gr.Log = existing
		gr.Diverged = existing != nil && existing.State == ledgerpub.StateDiverged
		return nil
	}
	if gr.Resp.Empty {
		gr.Log = existing
		if existing != nil && existing.State == ledgerpub.StateActive && existing.Size > 0 {
			gr.Diverged = true
			return c.diverge(ctx, term, db, g, *existing, fmt.Sprintf("the ledger of the group has no block, and %d blocks were published (a restore of an older state, or a lost tail)", existing.Size))
		}
		gr.Diverged = existing != nil && existing.State == ledgerpub.StateDiverged
		return nil
	}
	chainID, err := hex.DecodeString(gr.Resp.ChainID)
	if err != nil || len(chainID) != 32 || gr.Resp.Height < 0 {
		return fmt.Errorf("shard group %d answered a head that is not valid", g)
	}
	if existing != nil && !bytes.Equal(existing.ChainID, chainID) {
		// The group holds another chain than the one published for it (the database was dropped and made again outside the control plane, or
		// its ledger was replaced): the old log cannot be extended, and it must not simply stop. It is closed with an event and flagged
		// retired in the aggregate, as a removed group is.
		if err := c.retireLogs(ctx, *pc, term, db, []ledgerpub.LogRow{*existing}, "chain-replaced"); err != nil {
			return err
		}
		// the event is a change of the key set: the signers are read again, with the revision they have now
		fresh, err := c.cfg.Keys.Publication(ctx)
		if err != nil {
			return err
		}
		if fresh.SignErr != nil {
			return fresh.SignErr
		}
		*pc = fresh
		l2, err := c.cfg.Store.Logs(ctx)
		if err != nil {
			return err
		}
		copy(logs, l2)
		existing = nil
	}
	if existing == nil {
		if hasClosed(logs, chainID) {
			return nil // the chain's log was closed (retired or acknowledged) and awaits the next epoch (step 20)
		}
		var id [32]byte
		copy(id[:], chainID)
		origin, err := ledgerlog.ChainOrigin(pc.Prefix, id, 1)
		if err != nil {
			return err
		}
		gi := g
		row, err := c.cfg.Store.CreateLog(ctx, term, ledgerpub.LogRow{Kind: ledgerpub.KindChain, Database: db, ShardGroupID: &gi, ChainID: chainID, Epoch: 1, Origin: origin})
		if err != nil {
			return err
		}
		existing = &row
	}
	gr.Log = existing
	if existing.State == ledgerpub.StateDiverged {
		gr.Diverged = true
		return nil
	}
	l, err := c.extend(ctx, *pc, term, db, gr.Group, *existing, gr.Resp)
	if l != nil {
		gr.Log = l
	}
	var gre *groupReadError
	switch {
	case errors.Is(err, errDiverged):
		gr.Diverged = true
		return nil
	case errors.As(err, &gre):
		// the blocks could not be read after the head was: the group is unread in this round, its member stale
		var re *ReadError
		if !errors.As(gre.err, &re) {
			re = &ReadError{Group: g, Transport: true, Message: gre.err.Error()}
		}
		gr.Err = re
		return nil
	}
	return err
}
