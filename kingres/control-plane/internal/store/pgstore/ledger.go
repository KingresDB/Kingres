// SPDX-License-Identifier: PostgreSQL

package pgstore

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// zeroHash is the prev_hash of block 0 in the ledger design (32 zero bytes
// in hex); the API reports it as null.
var zeroHash = strings.Repeat("0", 64)

// ledgerStatus reads the status of the ledger chain on this instance.
//
// This increment reads only the chain of the instance the DSN points at; a
// sharded database has one chain per shard group, and reading the others
// needs the connections of the agent.
func (s *Store) ledgerStatus(ctx context.Context, h *dbHandle) (api.LedgerStatus, error) {
	st := api.LedgerStatus{
		DatabaseName: h.name,
		Chains:       []api.LedgerChainStatus{},
		Complete:     true, UncoveredShardGroupIDs: []int32{},
	}
	zero, off := 0, "off"
	st.Mode = &off              // no ledger catalog: the ledger is certainly off
	st.LedgerTableCount = &zero // and there are no ledger tables
	ok, err := s.regclassExists(ctx, h.pool, "kingres.ledger_blocks")
	if err != nil || !ok {
		return st, err
	}
	// A ledger catalog exists: the mode is whatever the control plane recorded,
	// and unknown (null) for databases it did not create.
	st.Mode = nil
	if rec, err := s.dbSettings(ctx); err == nil {
		if r, ok := rec[h.name]; ok {
			mode := r.ledgerMode
			st.Mode, st.DigestIntervalSeconds = &mode, r.digest
		}
	}
	if has, err := s.regclassExists(ctx, h.pool, "kingres.ledger_table"); err != nil {
		return st, err
	} else if has {
		var n int
		if err := h.pool.QueryRow(ctx, "SELECT count(*) FROM kingres.ledger_table").Scan(&n); err != nil {
			return st, s.wrap("reading kingres.ledger_table", err)
		}
		st.LedgerTableCount = &n
	}
	chain := api.LedgerChainStatus{ShardGroupID: s.chainGroup(ctx, h)}
	var (
		height int64
		hash   string
		at     time.Time
	)
	err = h.pool.QueryRow(ctx, `SELECT height, encode(block_hash, 'hex'), closed_at
		FROM kingres.ledger_blocks ORDER BY height DESC LIMIT 1`).Scan(&height, &hash, &at)
	switch {
	case err == nil:
		at = at.UTC()
		chain.LatestBlockHeight, chain.LatestBlockHash, chain.LatestBlockAt = &height, &hash, &at
	case !isNoRows(err):
		return st, s.wrap("reading kingres.ledger_blocks", err)
	}
	// kingres_ledger 0.2 records verifications and has the status view; 0.1
	// has neither, and the verification fields then stay null ("not available").
	if has, err := s.regclassExists(ctx, h.pool, "kingres.ledger_status"); err != nil {
		return st, err
	} else if has {
		var at *time.Time
		var state *string
		var tables int
		err := h.pool.QueryRow(ctx, `SELECT append_only_tables + updatable_tables, last_verification_at, last_verification_state
			FROM kingres.ledger_status`).Scan(&tables, &at, &state)
		if err != nil {
			return st, s.wrap("reading kingres.ledger_status", err)
		}
		st.LedgerTableCount = &tables
		never := "never"
		vs := &never
		if state != nil {
			m := "failed"
			if *state == "clean" {
				m = "passed"
			}
			vs = &m
			u := at.UTC()
			st.LastVerifiedAt = &u
			var to *int64
			if err := h.pool.QueryRow(ctx, "SELECT max(to_height) FROM kingres.ledger_verification WHERE state = 'clean'").Scan(&to); err == nil {
				chain.LastVerifiedBlockHeight = to
			}
		}
		st.VerificationState, chain.VerificationState = vs, vs
	}
	st.Chains = append(st.Chains, chain)
	st.UncoveredShardGroupIDs = s.otherGroups(ctx, h, chain.ShardGroupID)
	st.Complete = len(st.UncoveredShardGroupIDs) == 0
	return st, nil
}

// chainGroup is the catalog group of the local chain (0, the map group, when
// the node is not in the catalog).
func (s *Store) chainGroup(ctx context.Context, h *dbHandle) int32 {
	return s.localGroup(ctx, h)
}

// LedgerStatus implements store.Store.
func (s *Store) LedgerStatus(ctx context.Context, db string) (api.LedgerStatus, error) {
	h, err := s.requireDatabase(ctx, db)
	if err != nil {
		return api.LedgerStatus{}, err
	}
	return s.ledgerStatus(ctx, h)
}

// LedgerStatuses implements store.Store.
func (s *Store) LedgerStatuses(ctx context.Context) ([]api.LedgerStatus, error) {
	names, err := s.databaseNames(ctx)
	if err != nil {
		return nil, err
	}
	out := []api.LedgerStatus{}
	for _, n := range names {
		h, err := s.requireDatabase(ctx, n)
		if err != nil {
			return nil, err
		}
		st, err := s.ledgerStatus(ctx, h)
		if err != nil {
			s.opts.Logger.Debug("skipping database in ledger status", "database", n, "error", err)
			continue
		}
		if st.Mode == nil || *st.Mode != "off" {
			out = append(out, st)
		}
	}
	return out, nil
}

// LedgerBlocks implements store.Store.
func (s *Store) LedgerBlocks(ctx context.Context, db string, shardGroupID *int32, p store.Page) (api.LedgerBlockPage, error) {
	empty := api.LedgerBlockPage{Items: []api.LedgerBlock{}}
	h, err := s.requireDatabase(ctx, db)
	if err != nil {
		return empty, err
	}
	before, bounded, err := store.DecodeKey(p.Cursor)
	if err != nil {
		return empty, err
	}
	sharded, err := s.regclassExists(ctx, h.pool, "kingres.bucket_map")
	if err != nil {
		return empty, err
	}
	if sharded && shardGroupID == nil {
		var n int
		if err := h.pool.QueryRow(ctx, "SELECT count(*) FROM kingres.colocation").Scan(&n); err != nil {
			return empty, s.wrap("reading kingres.colocation", err)
		}
		if n > 0 {
			return empty, &store.ArgError{Field: "shardGroupId", Message: "required for a sharded database"}
		}
	}
	group := s.chainGroup(ctx, h)
	if shardGroupID != nil && *shardGroupID != group {
		return empty, nil // the chains of other shard groups are not readable from here
	}
	ok, err := s.regclassExists(ctx, h.pool, "kingres.ledger_blocks")
	if err != nil || !ok {
		return empty, err
	}
	var bound any
	if bounded {
		bound = before
	}
	rows, err := h.pool.Query(ctx, `SELECT height, encode(prev_hash, 'hex'), encode(tx_merkle_root, 'hex'),
			encode(block_hash, 'hex'), closed_at, hash_version, tx_count
		FROM kingres.ledger_blocks WHERE $1::bigint IS NULL OR height < $1
		ORDER BY height DESC LIMIT $2`, bound, p.Limit+1)
	if err != nil {
		return empty, s.wrap("reading kingres.ledger_blocks", err)
	}
	blocks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.LedgerBlock, error) {
		b := api.LedgerBlock{ShardGroupID: group}
		var prev string
		var at time.Time
		err := r.Scan(&b.Height, &prev, &b.TxMerkleRoot, &b.BlockHash, &at, &b.HashVersion, &b.TransactionCount)
		b.Timestamp = at.UTC()
		if prev != zeroHash {
			b.PrevHash = &prev
		}
		return b, err
	})
	if err != nil {
		return empty, s.wrap("reading kingres.ledger_blocks", err)
	}
	page := api.LedgerBlockPage{Items: blocks}
	if len(blocks) > p.Limit {
		next := store.EncodeKey(blocks[p.Limit-1].Height)
		page.NextCursor = &next
		page.Items = blocks[:p.Limit]
	}
	return page, nil
}

// otherGroups lists the shard groups of the cluster other than the one whose
// chain was read: their chains are not part of the status (contract 0.7.0).
func (s *Store) otherGroups(ctx context.Context, h *dbHandle, local int32) []int32 {
	out := []int32{}
	if ok, err := s.regclassExists(ctx, h.pool, "kingres.node"); err != nil || !ok {
		return out
	}
	rows, err := h.pool.Query(ctx, `SELECT DISTINCT group_id FROM kingres.node WHERE group_id <> $1 ORDER BY group_id`, local)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var g int32
		if rows.Scan(&g) == nil {
			out = append(out, g)
		}
	}
	return out
}
