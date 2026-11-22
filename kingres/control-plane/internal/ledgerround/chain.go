// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

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
		// The answer names the chain of another group's open log: two groups swapped (a mapping error), or an answer was forged. That is a
		// divergence of both, never a retirement (P8-04): nothing is closed, nothing is signed for either log, and the alert is critical.
		if other := openLogByChain(logs, chainID); other != nil && other.ID != existing.ID {
			gr.Log, gr.Diverged = existing, true
			why := "the group answered the chain of shard group " + groupText(*other) + " (two groups swapped, or a forged answer)"
			if err := c.diverge(ctx, term, db, g, *existing, why); err != nil {
				return err
			}
			if other.State == ledgerpub.StateActive && other.ShardGroupID != nil {
				return c.diverge(ctx, term, other.Database, *other.ShardGroupID, *other, "the group "+strconv.Itoa(int(g))+" answered the chain of this group")
			}
			return nil
		}
		// The group holds another chain than the one published for it (the database was dropped and made again outside the control plane, or
		// its ledger was replaced): the old log cannot be extended, and it must not simply stop. It is closed with an event and flagged
		// retired in the aggregate, as a removed group is, but not on the first answer (P7A-06): the end is irreversible and public, and one
		// wrong answer (a forged one, a mapping fault, a group that briefly answers from another database) is not the proof of it. The same
		// new chain id must answer in every round for MissingFor, as for a database that vanished, and an alert is raised from the first answer.
		if ok, left := c.replacementConfirmed(db, g, chainID); !ok {
			c.alert(ctx, AlertDiverged, "critical", db, g, fmt.Sprintf("Shard group %d answered another chain than the one published for it. If it keeps answering the same chain for %s the published log ends for good (a signed chain-retired event) and the log of the new chain starts; until then nothing is published for the group.", g, left.Round(time.Minute)), true)
			gr.Log = existing
			gr.Err = &ReadError{Group: g, Code: "chain_changed", Message: "the group answers another chain than the one published for it; waiting to see whether it keeps doing so"}
			return nil
		}
		c.replacementDone(db, g)
		if err := c.retireLogs(ctx, *pc, term, db, []ledgerpub.LogRow{*existing}, "chain-replaced"); err != nil {
			return err
		}
		// a replacement is not a quiet event (P8-04): the old chain ended with a signed event and a new log starts; an operator is told
		c.alert(ctx, AlertDiverged, "critical", db, g, fmt.Sprintf("The chain of shard group %d was replaced by another chain: the published log ended with a signed event and a log of the new chain starts. Check that the database was meant to be replaced.", g), true)
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
	if existing != nil {
		// the group answers the chain of its log: a replacement that was being watched ended with that answer
		c.replacementEnded(ctx, db, g)
	}
	if existing == nil {
		if closed := closedLog(logs, chainID); closed != nil {
			// The chain's log was closed (retired for a drop, a removal, a vanished database or a replacement, or acknowledged) and the same
			// chain is back: its log is NOT opened again and nothing is published for the new blocks, until the next epoch of step 20 is
			// started by an operator. That is said loudly (P8-03): a critical alert names the group, the height now and why the log was
			// closed. An auditor sees the closed log with its chain-retired event and nothing after it.
			// The task that drops a database ends the publication first and drops the database a few steps later, so the chain is still
			// there for a short time after the retirement for "dropped": that window is no return of a closed chain (found by the cluster
			// test of the drop in the round of part A). Nothing is published for it and no alert is raised until DropGrace has passed.
			if closed.Detail == "retired: "+ReasonDropped && closed.ClosedAt != nil && c.cfg.Clock().Sub(*closed.ClosedAt) < DropGrace {
				gr.Log, gr.Diverged = closed, true
				return nil
			}
			c.alert(ctx, AlertDiverged, "critical", db, g, fmt.Sprintf("The chain of shard group %d (head %d) is back with the chain id of a log whose publication ended (%s). Its new blocks are not published until a new epoch is started.", g, gr.Resp.Height, closed.Detail), true)
			gr.Log, gr.Diverged = closed, true
			return nil
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
		if c.cfg.OnNewChain != nil {
			if err := c.cfg.OnNewChain(ctx, db); err != nil {
				c.log.Warn("copying the client keys to a new chain failed", "database", db, "group", g, "error", err)
			}
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

// closedLog returns a closed chain log with the chain id (the newest), nil when there is none.
func closedLog(logs []ledgerpub.LogRow, chainID []byte) *ledgerpub.LogRow {
	var found *ledgerpub.LogRow
	for i := range logs {
		l := &logs[i]
		if l.Kind == ledgerpub.KindChain && bytes.Equal(l.ChainID, chainID) && l.State == ledgerpub.StateClosed {
			found = l
		}
	}
	return found
}

// openLogByChain returns the chain log with this chain id that is not closed, nil when there is none.
func openLogByChain(logs []ledgerpub.LogRow, chainID []byte) *ledgerpub.LogRow {
	for i := range logs {
		l := &logs[i]
		if l.Kind == ledgerpub.KindChain && bytes.Equal(l.ChainID, chainID) && l.State != ledgerpub.StateClosed {
			return l
		}
	}
	return nil
}
