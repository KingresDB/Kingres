// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerround

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
)

// The new epoch of a diverged chain (design 9.4, step 20).

// ErrNotDiverged: the chain of the group is not diverged (or there is no log for it).
var ErrNotDiverged = errors.New("ledgerround: the chain of this shard group is not diverged")

// ReasonEpochClosed is the reason in the detail of a log that an epoch start closed.
const ReasonEpochClosed = "epoch-closed"

// EpochKinds are what the operator says happened.
var EpochKinds = map[string]bool{"restore": true, "failover-loss": true, "other": true}

// epochLogs returns the diverged log of the group (state diverged) and, when an earlier call of the same epoch start already closed it, the
// closed log that waits for its successor.
func epochLogs(logs []ledgerpub.LogRow, db string, group int32) (diverged, closedWaiting *ledgerpub.LogRow) {
	var latest *ledgerpub.LogRow
	for i := range logs {
		l := &logs[i]
		if l.Kind != ledgerpub.KindChain || l.Database != db || l.ShardGroupID == nil || *l.ShardGroupID != group {
			continue
		}
		if latest == nil || l.Epoch > latest.Epoch {
			latest = l
		}
	}
	if latest == nil {
		return nil, nil
	}
	switch {
	case latest.State == ledgerpub.StateDiverged:
		return latest, nil
	case latest.State == ledgerpub.StateClosed && strings.HasPrefix(latest.Detail, "retired: "+ReasonEpochClosed):
		return nil, latest
	}
	return nil, nil
}

// IsDiverged tells whether the chain of a group of a database is diverged and waits for an acknowledgement (or has an epoch start that
// was begun and not finished).
func (c *Collector) IsDiverged(ctx context.Context, db string, group int32) (bool, error) {
	logs, err := c.cfg.Store.Logs(ctx)
	if err != nil {
		return false, err
	}
	d, w := epochLogs(logs, db, group)
	return d != nil || w != nil, nil
}

// StartEpoch closes the epoch of a diverged chain and opens the next one: the event epoch-closed is signed and published, the old log is
// closed (its tiles and last checkpoint stay served for ever, and its member is flagged retired and diverged in the next aggregate leaf), the
// log of epoch e+1 is created empty (the next round builds it from block 0 of the chain as it is now), the alert is cleared. Every step is
// idempotent and resumable: a call after a crash finds what was done. kind is restore, failover-loss or other; actor is the role that
// acknowledged.
//
// The event states final-size and final-root of the old log (what was published), and the common height as "unknown": finding the highest
// height whose block hash is the same in both histories needs the old blocks, which the database no longer has; the published entries are
// the only copy.
func (c *Collector) StartEpoch(ctx context.Context, db string, group int32, kind, reason, actor string) error {
	if !EpochKinds[kind] {
		return errors.New("ledgerround: the kind of an epoch start is restore, failover-loss or other")
	}
	l := c.lock(db)
	l.Lock()
	defer l.Unlock()
	logs, err := c.cfg.Store.Logs(ctx)
	if err != nil {
		return err
	}
	diverged, waiting := epochLogs(logs, db, group)
	if diverged == nil && waiting == nil {
		return ErrNotDiverged
	}
	pc, err := c.prepare(ctx)
	if err != nil {
		return err
	}
	old := diverged
	if old == nil {
		old = waiting
	}
	if diverged != nil {
		fields := []ledgersign.EventField{
			{Name: "chain", Value: hexOf(old.ChainID)},
			{Name: "epoch", Value: strconv.Itoa(old.Epoch)},
			{Name: "final-size", Value: strconv.FormatInt(old.Size, 10)},
			{Name: "final-root", Value: hexOf(old.Root)},
			{Name: "common-height", Value: "unknown"},
			{Name: "kind", Value: kind},
			{Name: "reason", Value: reason},
			{Name: "acknowledged-by", Value: actor},
		}
		if _, err := c.cfg.Keys.AppendEvent(ctx, ledgersign.EventEpochClosed, hexOf(old.ChainID)+":"+strconv.Itoa(old.Epoch), fields); err != nil {
			return fmt.Errorf("publishing the end of epoch %d of shard group %d: %w", old.Epoch, group, err)
		}
		detail := "retired: " + ReasonEpochClosed + wasDivergedMark + old.Detail + ")"
		if err := c.cfg.Store.SetState(ctx, pc.Term, old.ID, ledgerpub.StateClosed, detail); err != nil {
			return err
		}
	}
	// the successor: created once, with the origin of the next epoch
	have := false
	for _, x := range logs {
		if x.Kind == ledgerpub.KindChain && x.Database == db && x.ShardGroupID != nil && *x.ShardGroupID == group && x.Epoch == old.Epoch+1 {
			have = true
		}
	}
	if !have {
		var id [32]byte
		copy(id[:], old.ChainID)
		origin, err := ledgerlog.ChainOrigin(pc.Prefix, id, uint32(old.Epoch)+1) //nolint:gosec // an epoch is small
		if err != nil {
			return err
		}
		gi := group
		if _, err := c.cfg.Store.CreateLog(ctx, pc.Term, ledgerpub.LogRow{Kind: ledgerpub.KindChain, Database: db, ShardGroupID: &gi, ChainID: old.ChainID, Epoch: old.Epoch + 1, Origin: origin}); err != nil {
			return err
		}
	}
	c.alert(ctx, AlertDiverged, "critical", db, group, "", false)
	// the leaf that flags the old member retired and diverged; the new member joins at the next round, when its log has its first block. The
	// key set may have changed with the event: the leaf is made again, a few times.
	var aerr error
	for try := 0; try < 5; try++ {
		npc, perr := c.prepare(ctx)
		if perr != nil {
			return perr
		}
		res := Result{Database: db}
		if aerr = c.aggregate(ctx, npc, npc.Term, db, &res); !errors.Is(aerr, ErrKeysChanged) {
			break
		}
	}
	return aerr
}
