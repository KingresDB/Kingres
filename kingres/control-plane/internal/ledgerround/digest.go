// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// readProblem is the problem a caller of a read of one database gets when no group could be read: the first group's reason.
func readProblem(res Result) error {
	for _, g := range res.Groups {
		if g.Err == nil {
			continue
		}
		switch g.Err.Code {
		case CodeNoLedger:
			return problem.New(problem.Conflict, "The ledger is not enabled on this database.")
		case CodeNoDatabase:
			return problem.New(problem.NotFound, "The database does not exist.")
		case CodeLedgerOld:
			return problem.New(problem.UnsupportedOp, g.Err.Message)
		}
	}
	return problem.New(problem.ServiceUnavailable, "No shard group could be read now.").WithRetryAfter(5)
}

// digestChains lists the chains of the groups that were read, ascending by group: the digest of each, as the extension made it.
func digestChains(res Result) ([]api.LedgerDigestChain, int) {
	var out []api.LedgerDigestChain
	hv := 1
	for _, g := range res.Groups {
		d := g.Resp.Digest
		if g.Err != nil || g.Resp.Empty || d == nil {
			continue
		}
		out = append(out, api.LedgerDigestChain{ShardGroupID: g.Group.ID, BlockHeight: d.Height, BlockHash: d.BlockHash, Document: d.Document})
		if d.HashVersion > hv {
			hv = d.HashVersion
		}
	}
	return out, hv
}

// Digest runs a full round of the database (design 5.4: every shard group is read with the operation that records a digest and closes the
// open block, the chain logs and the aggregate are updated when publication is on) and returns the digest of every group read. A group
// that could not be read is named in uncoveredShardGroupIds and the digest is incomplete; it is never skipped as complete. The digest is
// stored as a view (listLedgerDigests lists them).
func (c *Collector) Digest(ctx context.Context, db, actor string) (api.LedgerDigest, error) {
	l := c.lock(db)
	l.Lock()
	defer l.Unlock()
	res, err := c.round(ctx, db, OpDigest, nil)
	switch {
	case errors.Is(err, ErrNotLeader):
		return api.LedgerDigest{}, problem.New(problem.ServiceUnavailable, "This server is not yet the manager leader; try again in a moment.").WithRetryAfter(5)
	case err != nil:
		var pe *problem.Problem
		if errors.As(err, &pe) {
			return api.LedgerDigest{}, err
		}
		c.log.Warn("generating a digest", "database", db, "error", err)
		return api.LedgerDigest{}, problem.New(problem.ServiceUnavailable, "The digest could not be made now.").WithRetryAfter(5)
	}
	chains, hv := digestChains(res)
	if len(chains) == 0 {
		if !res.Answered() {
			return api.LedgerDigest{}, readProblem(res)
		}
		return api.LedgerDigest{}, problem.New(problem.Conflict, "The ledger is empty: there is nothing to build a digest of.")
	}
	raw, err := json.Marshal(chains)
	if err != nil {
		return api.LedgerDigest{}, err
	}
	if actor == "" {
		actor = "system"
	}
	unc := res.Uncovered()
	v := ledgerpub.DigestView{Database: db, GeneratedAt: c.cfg.Clock().UTC(), GeneratedBy: actor, HashVersion: hv, Chains: raw,
		Complete: len(unc) == 0, Uncovered: unc}
	if res.Published && res.AggregateSize > 0 {
		n := res.AggregateSize
		v.AggregateSize, v.AggregateHash = &n, res.AggregateHash
	}
	id, err := c.cfg.Store.PutDigest(ctx, res.Term, v)
	if err != nil {
		c.log.Warn("storing a digest", "database", db, "error", err)
		return api.LedgerDigest{}, problem.New(problem.ServiceUnavailable, "The digest could not be stored now.").WithRetryAfter(5)
	}
	v.ID = id
	return viewDigest(v), nil
}

// viewDigest maps a stored view to the contract.
func viewDigest(v ledgerpub.DigestView) api.LedgerDigest {
	var chains []api.LedgerDigestChain
	if err := json.Unmarshal(v.Chains, &chains); err != nil || chains == nil {
		chains = []api.LedgerDigestChain{}
	}
	by := v.GeneratedBy
	unc := v.Uncovered
	if unc == nil {
		unc = []int32{}
	}
	d := api.LedgerDigest{ID: v.ID, DatabaseName: v.Database, GeneratedAt: v.GeneratedAt.UTC(), GeneratedBy: &by, HashVersion: v.HashVersion,
		Chains: chains, Anchors: []api.DigestAnchor{}, Complete: v.Complete, UncoveredShardGroupIDs: unc}
	if v.AggregateSize != nil {
		n := *v.AggregateSize
		d.AggregateSize = &n
		if len(v.AggregateHash) > 0 {
			h := hex.EncodeToString(v.AggregateHash)
			d.AggregateHash = &h
		}
	}
	return d
}

// Digests lists the recorded digests of a database, newest first (listLedgerDigests).
func (c *Collector) Digests(ctx context.Context, db string, p store.Page) (api.LedgerDigestPage, error) {
	page := api.LedgerDigestPage{Items: []api.LedgerDigest{}}
	before, _, err := store.DecodeKey(p.Cursor)
	if err != nil {
		return page, problem.Validation("The cursor is not valid.", problem.FieldError{Field: "cursor", Message: "not a cursor issued by this server", Code: "invalid_cursor"})
	}
	views, err := c.cfg.Store.Digests(ctx, db, before, p.Limit+1)
	if err != nil {
		return page, problem.New(problem.ServiceUnavailable, "The digests cannot be read now.").WithRetryAfter(5)
	}
	if len(views) == 0 && p.Cursor == "" {
		// nothing to list: an empty list for a database that exists, a 404 for one that does not
		ok, err := c.known(ctx, db)
		if err != nil {
			return page, problem.New(problem.ServiceUnavailable, "The digests cannot be read now.").WithRetryAfter(5)
		}
		if !ok {
			return page, problem.New(problem.NotFound, "No such database.")
		}
	}
	if len(views) > p.Limit {
		next := store.EncodeKey(views[p.Limit-1].ID)
		page.NextCursor = &next
		views = views[:p.Limit]
	}
	for _, v := range views {
		page.Items = append(page.Items, viewDigest(v))
	}
	return page, nil
}
