// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/leader"
)

// leaderMemory remembers the last manager leader the catalog named. The catalog
// is read from this server's own instance, so while that instance is stopped on
// purpose (stopThisServerInstance) it cannot name the leader; the internal API
// then authorizes the calls of the leader, and this server records the tasks of
// its start, with the last one it knew. There is no failover, and the leader
// checks every call itself (a server that is not the leader refuses), so a
// remembered leader cannot make anything possible that the leader does not allow.
type leaderMemory struct {
	mu sync.Mutex
	id string
	at time.Time
}

// remembered is how long a server that cannot read the leader from its catalog (for
// example one whose catalog was just emptied by its own removal, which the leader
// then tells to reset itself) still accepts the calls of the leader it last verified.
// It replaces the accident of a two-second cache, which the removal of the standbys of
// the server made too short (the leader answers every call itself, so a stale
// memory cannot make anything possible that the leader refuses).
const remembered = 60 * time.Second

// recent returns the leader last verified if that was less than remembered ago.
func (m *leaderMemory) recent() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.id != "" && time.Since(m.at) < remembered {
		return m.id
	}
	return ""
}

func (m *leaderMemory) set(id string) {
	m.mu.Lock()
	m.id = id
	m.at = time.Now()
	m.mu.Unlock()
}

func (m *leaderMemory) get() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.id
}

// resolve reads the leader and remembers it; when it cannot be read it returns
// the remembered one.
func (m *leaderMemory) resolve(ctx context.Context, r *leader.Resolver, self string) (id string, isSelf bool, err error) {
	id, err = r.Get(ctx)
	if err == nil && id != "" {
		m.set(id)
		return id, id == self, nil
	}
	if last := m.get(); last != "" {
		return last, last == self, nil
	}
	return "", false, err
}
