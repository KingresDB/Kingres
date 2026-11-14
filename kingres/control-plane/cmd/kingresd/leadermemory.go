// SPDX-License-Identifier: PostgreSQL

package main

import (
	"context"
	"sync"

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
}

func (m *leaderMemory) set(id string) {
	m.mu.Lock()
	m.id = id
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
