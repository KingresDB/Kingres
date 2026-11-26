// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerround

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// Reasons written in the chain-retired event (printable ASCII, at most 200 bytes). A verifier tells them apart: "dropped" and
// "group-removed" are acts of the control plane after a final round; "database-missing" and "chain-replaced" are what the collector found
// without any act of the control plane (a database dropped from SQL by an owner who is not a superuser, a database made again), and
// the final head is the last one that was published, not one read at the end.
const (
	ReasonDropped         = "dropped"
	ReasonGroupRemoved    = "group-removed"
	ReasonDatabaseMissing = "database-missing"
	ReasonChainReplaced   = "chain-replaced"
)

// retireLogs ends the publication of chain logs: for each one that has published a block, the event chain-retired (the final height and
// block hash are the published head) is signed and stored, then the log is closed. The event comes first and is idempotent (one per
// chain and epoch), so a crash between the two is repaired by the next call. The aggregate leaf that flags the members retired is the
// next aggregate update (Collector.aggregate), which the callers run after this.
func (c *Collector) retireLogs(ctx context.Context, pc ledgersign.PubContext, term scheduler.Term, db string, ls []ledgerpub.LogRow, reason string) error {
	for _, l := range ls {
		if l.Kind != ledgerpub.KindChain || l.State == ledgerpub.StateClosed {
			continue
		}
		if l.Size > 0 {
			fields := []ledgersign.EventField{
				{Name: "chain", Value: hexOf(l.ChainID)},
				{Name: "epoch", Value: strconv.Itoa(l.Epoch)},
				{Name: "final-height", Value: strconv.FormatInt(l.Size-1, 10)},
				{Name: "final-block-hash", Value: hexOf(l.HeadBlockHash)},
				{Name: "reason", Value: reason},
			}
			if _, err := c.cfg.Keys.AppendEvent(ctx, ledgersign.EventChainRetired, hexOf(l.ChainID)+":"+strconv.Itoa(l.Epoch), fields); err != nil {
				return fmt.Errorf("publishing the end of the chain of shard group %s: %w", groupText(l), err)
			}
		}
		// A diverged log keeps its flag (P8-08): the retiring leaf says retired and diverged, and the critical alert is not cleared by a drop
		// or a removal (an administrator who rolled a database back must not end the signal by dropping it).
		detail := "retired: " + reason
		if l.State == ledgerpub.StateDiverged {
			detail += wasDivergedMark + l.Detail + ")"
		}
		if err := c.cfg.Store.SetState(ctx, term, l.ID, ledgerpub.StateClosed, detail); err != nil {
			return err
		}
		if l.ShardGroupID != nil {
			if l.State != ledgerpub.StateDiverged {
				c.alert(ctx, AlertDiverged, "critical", db, *l.ShardGroupID, "", false)
			}
			c.alert(ctx, AlertUnread, "warning", db, *l.ShardGroupID, "", false)
		}
		c.log.Info("ledger chain retired", "database", db, "group", groupText(l), "origin", l.Origin, "size", l.Size, "reason", reason)
	}
	return nil
}

func groupText(l ledgerpub.LogRow) string {
	if l.ShardGroupID == nil {
		return "?"
	}
	return strconv.Itoa(int(*l.ShardGroupID))
}

func openChains(logs []ledgerpub.LogRow, db string, groups map[int32]bool) []ledgerpub.LogRow {
	var out []ledgerpub.LogRow
	for _, l := range logs {
		if l.Kind != ledgerpub.KindChain || l.Database != db || l.State == ledgerpub.StateClosed {
			continue
		}
		if groups != nil && (l.ShardGroupID == nil || !groups[*l.ShardGroupID]) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// ErrCannotRetire: the keys cannot sign the final checkpoint or the event now. The operation that called is retried (it is a task step).
var ErrCannotRetire = errors.New("ledgerround: the end of the publication cannot be signed now")

// prepare returns the publication context of a retirement: it must be able to sign.
func (c *Collector) prepare(ctx context.Context) (ledgersign.PubContext, error) {
	pc, err := c.awaitTerm(ctx)
	if err != nil {
		return pc, err
	}
	if pc.SignErr != nil {
		return pc, fmt.Errorf("%w: %v", ErrCannotRetire, pc.SignErr)
	}
	if err := c.checkPublished(ctx); err != nil {
		if errors.Is(err, ErrPublishedState) {
			return pc, fmt.Errorf("%w: %v", ErrCannotRetire, err)
		}
		return pc, err
	}
	return pc, nil
}

// RetireDatabase ends the publication of every chain of a database (the hook of dropDatabase, and what the collector does for a database
// that vanished without it): a final round reads the groups once more (finalRound; a group that cannot be read leaves its member stale),
// each open chain log gets its chain-retired event and is closed, and one aggregate leaf flags every member retired; the aggregate log
// then ends. A database with no log is not an error and does nothing. Every step is idempotent, so a task step that fails halfway is
// repeated.
func (c *Collector) RetireDatabase(ctx context.Context, db, reason string, finalRound bool) error {
	l := c.lock(db)
	l.Lock()
	defer l.Unlock()
	logs, err := c.cfg.Store.Logs(ctx)
	if err != nil {
		return err
	}
	if len(openChains(logs, db, nil)) == 0 && openAggregate(logs, db) == nil {
		return nil
	}
	if _, err := c.prepare(ctx); err != nil {
		return err
	}
	if finalRound {
		// The digest closes the open block, so the published log has the last blocks. A group that cannot be read is stale in it (not an
		// error); a failure to write is one, and the retirement is repeated.
		if err := c.finalRound(ctx, db, nil); err != nil {
			return err
		}
	}
	if err := c.endChains(ctx, db, nil, reason); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.missingSince, db)
	c.mu.Unlock()
	c.alert(ctx, AlertUnread, "critical", db, -1, "", false)
	return nil
}

// RetireServer ends the publication of the chains of the groups whose primary runs on a server that is being removed (the hook of the
// removal of a server): for each database with such a chain, a final read of those groups, the events, the closing of the logs and one
// aggregate leaf in which the members are retired. A server that runs no group with a published chain does nothing.
func (c *Collector) RetireServer(ctx context.Context, server string) error {
	logs, err := c.cfg.Store.Logs(ctx)
	if err != nil {
		return err
	}
	dbs := map[string]bool{}
	for _, l := range logs {
		if l.Kind == ledgerpub.KindChain && l.State != ledgerpub.StateClosed && l.Database != "" {
			dbs[l.Database] = true
		}
	}
	for db := range dbs {
		if err := c.retireServerIn(ctx, db, server); err != nil {
			return err
		}
	}
	return nil
}

func (c *Collector) retireServerIn(ctx context.Context, db, server string) error {
	l := c.lock(db)
	l.Lock()
	defer l.Unlock()
	groups, err := c.cfg.Source.Groups(ctx, db)
	if err != nil {
		return fmt.Errorf("listing the shard groups of %s: %w", db, err)
	}
	mine := map[int32]bool{}
	for _, g := range groups {
		if g.Server == server {
			mine[g.ID] = true
		}
	}
	if len(mine) == 0 {
		return nil
	}
	logs, err := c.cfg.Store.Logs(ctx)
	if err != nil {
		return err
	}
	open := openChains(logs, db, mine)
	if len(open) == 0 {
		return nil
	}
	if _, err := c.prepare(ctx); err != nil {
		return err
	}
	if err := c.finalRound(ctx, db, mine); err != nil {
		return err
	}
	return c.endChains(ctx, db, mine, ReasonGroupRemoved)
}

// endChains closes the open chain logs (of the groups named, or all) with their events and then appends the aggregate leaf that flags them
// retired. The events change the key set (a new revision), so the keys are read again before the leaf is signed, and a key task that commits
// meanwhile makes the leaf be made again, a few times.
func (c *Collector) endChains(ctx context.Context, db string, only map[int32]bool, reason string) error {
	var err error
	for try := 0; try < 5; try++ {
		logs, lerr := c.cfg.Store.Logs(ctx)
		if lerr != nil {
			return lerr
		}
		pc, perr := c.prepare(ctx)
		if perr != nil {
			return perr
		}
		if err = c.retireLogs(ctx, pc, pc.Term, db, openChains(logs, db, only), reason); err != nil {
			return err
		}
		if pc, perr = c.prepare(ctx); perr != nil {
			return perr
		}
		res := Result{Database: db}
		if err = c.aggregate(ctx, pc, pc.Term, db, &res); !errors.Is(err, ErrKeysChanged) {
			return err
		}
	}
	return err
}

// finalRound is the round that precedes a retirement: it publishes the last blocks (every group, or only the groups named). A key task that
// commits meanwhile makes a checkpoint be refused (ErrKeysChanged); the round is made again, a few times.
func (c *Collector) finalRound(ctx context.Context, db string, only map[int32]bool) error {
	op := OpDigest
	if only != nil {
		op = OpHead
	}
	for try := 0; ; try++ {
		res, err := c.round(ctx, db, op, only)
		if err != nil {
			return err
		}
		if !res.KeysChanged {
			return nil
		}
		if try == 3 {
			return fmt.Errorf("%w: the key set kept changing", ErrKeysChanged)
		}
	}
}

// wasDivergedMark separates the reason of a retirement from the reason of the divergence in the detail of a closed log.
const wasDivergedMark = " (was diverged: "
