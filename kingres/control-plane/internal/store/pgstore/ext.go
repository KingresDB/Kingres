// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pgstore

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
)

// extTTL is how long the answer of extguard is kept for a database. A role that is not a superuser cannot create or rename anything in the schema of an
// extension (checks 2 and 3 of extguard), so it cannot make a trusted schema untrusted or the reverse by adding objects. It can still change what it owns: the
// owner of a table that moved can drop the orphan, and the owner of a split trigger function can change its attributes (extguard then answers untrusted at once).
// A short memory therefore delays such a change by at most this time, and spares a busy listing one catalog query per statement.
const extTTL = 3 * time.Second

type extKey struct {
	pool *pgxpool.Pool
	ext  string
}

type extAnswer struct {
	at   time.Time
	info extguard.Info
	err  error
}

// extState is the memory of the answers.
type extState struct {
	mu sync.Mutex
	m  map[extKey]extAnswer
}

// extInfo verifies the extension in the database of p (extguard.Verify) with a short memory. An untrusted extension is logged and returned as the
// *extguard.UntrustedError; an error of the query is returned as it is and is not remembered.
func (s *Store) extInfo(ctx context.Context, p *pgxpool.Pool, ext string) (extguard.Info, error) {
	k := extKey{p, ext}
	now := s.now()
	s.ext.mu.Lock()
	a, ok := s.ext.m[k]
	s.ext.mu.Unlock()
	if ok && now.Sub(a.at) < extTTL {
		return a.info, a.err
	}
	info, err := extguard.Verify(ctx, p, ext)
	if err != nil {
		if _, untrusted := extguard.IsUntrusted(err); !untrusted {
			return info, s.wrap("checking the extension "+ext, err)
		}
		s.opts.Logger.Warn("the schema of a KingresDB extension cannot be trusted; nothing in it is read", "extension", ext, "error", err)
	}
	if err == nil && !info.Present {
		// "Not installed" is never remembered: the control plane installs the extension itself (enabling sharding, creating a database) and
		// the next read, a moment later, must see it. Only the answers about an installed schema are kept.
		return info, nil
	}
	s.ext.mu.Lock()
	if s.ext.m == nil || len(s.ext.m) > 256 {
		s.ext.m = map[extKey]extAnswer{}
	}
	s.ext.m[k] = extAnswer{now, info, err}
	s.ext.mu.Unlock()
	return info, err
}

// extReady reports whether the extension is installed and trusted in the database of p. An absent extension and an untrusted one are both "not available" for
// the readers that can do without it (the monitor raises the alert for the untrusted one); only an error of the query is an error.
func (s *Store) extReady(ctx context.Context, p *pgxpool.Pool, ext string) (bool, error) {
	info, err := s.extInfo(ctx, p, ext)
	if err != nil {
		if _, untrusted := extguard.IsUntrusted(err); untrusted {
			return false, nil
		}
		return false, err
	}
	return info.Present, nil
}
