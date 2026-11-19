// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// Statement is the text of what an aggregate proves and does not prove (design 5.6), shown with every aggregate.
const Statement = "Containment: for every member, the chain had a block of that height with that hash, and the chain log of that size has that root, at some instant of " +
	"the round that the signer reports as generated_at; together with an anchor, no later than the time of the anchor. Continuity: for two leaves of this log, each chain " +
	"of the earlier one is present in the later one (or flagged retired in the last leaf in which it appears), its height does not decrease, and its chain log is a " +
	"consistent extension of the earlier one, unless the aggregate says diverged. The set of chains is the set the leader found in the catalog: a chain that exists and is " +
	"missing cannot be shown missing by the leaf alone. It does not prove that the heads were read at the same instant, that a cross-group transaction is seen in all its groups " +
	"at the same time, an order of commits between groups, or that a group's chain is the whole database's data (only ledger tables are in a chain). A statement about a " +
	"consistent state of the whole database at one time needs the global snapshot of phase 8."

// known tells whether the database exists (DatabaseExists), or else whether it is one that has a ledger in the settings.
func (c *Collector) known(ctx context.Context, db string) (bool, error) {
	if c.cfg.DatabaseExists != nil {
		return c.cfg.DatabaseExists(ctx, db)
	}
	if c.cfg.Databases == nil {
		return false, nil
	}
	dbs, err := c.cfg.Databases(ctx)
	if err != nil {
		return false, err
	}
	for _, d := range dbs {
		if d.Name == db {
			return true, nil
		}
	}
	return false, nil
}

// aggregateLog returns the aggregate log of a database: the open one, or else the newest closed one.
func aggregateLog(logs []ledgerpub.LogRow, db string) *ledgerpub.LogRow {
	var best *ledgerpub.LogRow
	for i := range logs {
		l := &logs[i]
		if l.Kind != ledgerpub.KindAggregate || l.Database != db {
			continue
		}
		if best == nil || (best.State == ledgerpub.StateClosed && l.State != ledgerpub.StateClosed) || (l.State == best.State && l.ID > best.ID) {
			best = l
		}
	}
	return best
}

func serviceDown(c *Collector, what string, err error) error {
	c.log.Warn("reading the publication", "what", what, "error", err)
	return problem.New(problem.ServiceUnavailable, "The publication cannot be read now.").WithRetryAfter(5)
}

// leaf reads the leaf at the end of the aggregate log of the given checkpoint, after checking the tiles it reads against the checkpoint's
// root (a damaged store is an error, never a wrong answer).
func (c *Collector) leaf(ctx context.Context, logID int64, cp ledgerpub.Checkpoint) (ledgerlog.AggregateEntry, error) {
	r, err := ledgerlog.NewReader(ctx, c.cfg.Store.Tiles(logID, 0), uint64(cp.Size)) //nolint:gosec // a stored size
	if err != nil {
		return ledgerlog.AggregateEntry{}, err
	}
	root, err := r.Root()
	if err != nil {
		return ledgerlog.AggregateEntry{}, err
	}
	if !bytes.Equal(root[:], cp.Root) {
		return ledgerlog.AggregateEntry{}, fmt.Errorf("the tiles of size %d do not have the root of the checkpoint", cp.Size)
	}
	raw, err := r.Entry(uint64(cp.Size) - 1) //nolint:gosec // a stored size of at least 1
	if err != nil {
		return ledgerlog.AggregateEntry{}, err
	}
	return ledgerlog.ParseAggregateEntry(raw)
}

func flagNames(f byte) []string {
	out := []string{}
	if f&ledgerlog.FlagStale != 0 {
		out = append(out, "stale")
	}
	if f&ledgerlog.FlagDiverged != 0 {
		out = append(out, "diverged")
	}
	if f&ledgerlog.FlagRetired != 0 {
		out = append(out, "retired")
	}
	return out
}

func summary(cp ledgerpub.Checkpoint, e ledgerlog.AggregateEntry, groupOf func(chain [32]byte, epoch uint32) int32) (api.LedgerAggregateSummary, []api.LedgerAggregateMember) {
	var members []api.LedgerAggregateMember
	unc := map[int32]bool{}
	for _, m := range e.Members {
		g := groupOf(m.ChainID, m.Epoch)
		members = append(members, api.LedgerAggregateMember{ShardGroupID: g, ChainID: hex.EncodeToString(m.ChainID[:]), Epoch: int(m.Epoch), Height: int64(m.Height), //nolint:gosec // below 2^62
			BlockHash: hex.EncodeToString(m.BlockHash[:]), LogRoot: hex.EncodeToString(m.LogRoot[:]), Flags: flagNames(m.Flags)})
		if m.Flags&(ledgerlog.FlagStale|ledgerlog.FlagDiverged) != 0 {
			unc[g] = true
		}
	}
	list := []int32{}
	for g := range unc {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
	at := ledgerlogTime(e.GeneratedAtUS)
	return api.LedgerAggregateSummary{Size: cp.Size, Root: hex.EncodeToString(cp.Root), Checkpoint: string(cp.Note), GeneratedAt: at,
		Complete: len(list) == 0, UncoveredShardGroupIDs: list}, members
}

func groupLookup(logs []ledgerpub.LogRow) func([32]byte, uint32) int32 {
	return func(chain [32]byte, epoch uint32) int32 {
		for _, l := range logs {
			if l.Kind == ledgerpub.KindChain && bytes.Equal(l.ChainID, chain[:]) && uint32(l.Epoch) == epoch && l.ShardGroupID != nil { //nolint:gosec // small
				return *l.ShardGroupID
			}
		}
		return 0
	}
}

// Aggregate returns the aggregate checkpoint of a database at a size (the newest when size is nil): the signed note, the members, the
// statement. 404 for an unknown database or a size that was not published; 409 ledger_not_published when the database has no aggregate.
func (c *Collector) Aggregate(ctx context.Context, db string, size *int64) (api.LedgerAggregate, error) {
	logs, err := c.cfg.Store.Logs(ctx)
	if err != nil {
		return api.LedgerAggregate{}, serviceDown(c, "logs", err)
	}
	agg := aggregateLog(logs, db)
	if agg == nil {
		ok, err := c.known(ctx, db)
		if err != nil {
			return api.LedgerAggregate{}, serviceDown(c, "databases", err)
		}
		if !ok {
			return api.LedgerAggregate{}, problem.New(problem.NotFound, "No such database.")
		}
		return api.LedgerAggregate{}, problem.New(problem.LedgerNotPublished, "The ledger of this database has no published aggregate yet.")
	}
	var cp ledgerpub.Checkpoint
	if size == nil {
		cp, err = c.cfg.Store.LatestCheckpoint(ctx, agg.ID)
	} else {
		cp, err = c.cfg.Store.Checkpoint(ctx, agg.ID, *size)
	}
	switch {
	case errors.Is(err, ledgerpub.ErrNotFound) && size == nil:
		return api.LedgerAggregate{}, problem.New(problem.LedgerNotPublished, "The ledger of this database has no published aggregate yet.")
	case errors.Is(err, ledgerpub.ErrNotFound):
		return api.LedgerAggregate{}, problem.New(problem.NotFound, "That size of the aggregate was not published.")
	case err != nil:
		return api.LedgerAggregate{}, serviceDown(c, "checkpoint", err)
	}
	e, err := c.leaf(ctx, agg.ID, cp)
	if err != nil {
		return api.LedgerAggregate{}, serviceDown(c, "aggregate leaf", err)
	}
	s, members := summary(cp, e, groupLookup(logs))
	return api.LedgerAggregate{LedgerAggregateSummary: s, Members: members, Anchors: []api.DigestAnchor{}, Statement: Statement}, nil
}

// Aggregates lists the aggregate checkpoints of a database without members, newest first.
func (c *Collector) Aggregates(ctx context.Context, db string, p store.Page) (api.LedgerAggregatePage, error) {
	page := api.LedgerAggregatePage{Items: []api.LedgerAggregateSummary{}}
	before, _, err := store.DecodeKey(p.Cursor)
	if err != nil {
		return page, problem.Validation("The cursor is not valid.", problem.FieldError{Field: "cursor", Message: "not a cursor issued by this server", Code: "invalid_cursor"})
	}
	logs, err := c.cfg.Store.Logs(ctx)
	if err != nil {
		return page, serviceDown(c, "logs", err)
	}
	agg := aggregateLog(logs, db)
	if agg == nil {
		ok, err := c.known(ctx, db)
		if err != nil {
			return page, serviceDown(c, "databases", err)
		}
		if !ok {
			return page, problem.New(problem.NotFound, "No such database.")
		}
		return page, nil
	}
	cps, err := c.cfg.Store.Checkpoints(ctx, agg.ID, before, p.Limit+1)
	if err != nil {
		return page, serviceDown(c, "checkpoints", err)
	}
	if len(cps) > p.Limit {
		next := store.EncodeKey(cps[p.Limit-1].Size)
		page.NextCursor = &next
		cps = cps[:p.Limit]
	}
	groupOf := groupLookup(logs)
	for _, cp := range cps {
		e, err := c.leaf(ctx, agg.ID, cp)
		if err != nil {
			return page, serviceDown(c, "aggregate leaf", err)
		}
		s, _ := summary(cp, e, groupOf)
		page.Items = append(page.Items, s)
	}
	return page, nil
}

// Publication is getLedgerPublication: whether publication is on, the listener, and every log with its size, state and last checkpoint.
func (c *Collector) Publication(ctx context.Context) (api.LedgerPublication, error) {
	out := api.LedgerPublication{Logs: []api.LedgerPublicationLog{}, Checks: map[string]any{}}
	pc, err := c.cfg.Keys.Publication(ctx)
	if err != nil {
		return out, serviceDown(c, "settings", err)
	}
	out.Enabled = pc.Enabled
	if c.cfg.Listener != nil {
		addr, tlsOn, running := c.cfg.Listener()
		if addr != "" {
			out.Listener.Address = &addr
		}
		out.Listener.TLS, out.Listener.Running = tlsOn, running
	}
	logs, err := c.cfg.Store.Logs(ctx)
	if err != nil {
		return out, serviceDown(c, "logs", err)
	}
	for _, l := range logs {
		pl := api.LedgerPublicationLog{Kind: l.Kind, Origin: l.Origin, Size: l.Size, State: l.State}
		if l.SignedAt != nil {
			t := l.SignedAt.UTC()
			pl.LastCheckpointAt = &t
			if out.LastCheckpointAt == nil || t.After(*out.LastCheckpointAt) {
				tt := t
				out.LastCheckpointAt = &tt
			}
		}
		out.Logs = append(out.Logs, pl)
	}
	return out, nil
}
