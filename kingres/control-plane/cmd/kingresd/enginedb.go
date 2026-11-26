// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
)

// swapDB is the database of the task store: a pool that is replaced when the system database moves (the new manager leader of a
// failover of the map group serves it from the promoted instance, design ha-phase5-part2.md 3.4). A statement in flight keeps the
// pool it started on; the old pool is closed after a grace period.
type swapDB struct {
	mu   sync.RWMutex
	pool *pgxpool.Pool
	dsn  string
}

func newSwapDB(pool *pgxpool.Pool, dsn string) *swapDB { return &swapDB{pool: pool, dsn: dsn} }

func (s *swapDB) cur() *pgxpool.Pool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pool
}

func (s *swapDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return s.cur().Exec(ctx, sql, args...)
}

func (s *swapDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return s.cur().Query(ctx, sql, args...)
}

func (s *swapDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return s.cur().QueryRow(ctx, sql, args...)
}

// Reconnect points the store at dsn (a no-op when it already is).
func (s *swapDB) Reconnect(ctx context.Context, dsn string) error {
	s.mu.RLock()
	same := s.dsn == dsn
	s.mu.RUnlock()
	if same {
		return nil
	}
	p, err := cluster.NewSafePool(ctx, dsn)
	if err != nil {
		return err
	}
	s.mu.Lock()
	old := s.pool
	s.pool, s.dsn = p, dsn
	s.mu.Unlock()
	go func() { time.Sleep(30 * time.Second); old.Close() }()
	return nil
}

// Close closes the current pool.
func (s *swapDB) Close() { s.cur().Close() }
