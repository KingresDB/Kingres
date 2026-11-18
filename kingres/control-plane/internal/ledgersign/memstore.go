// SPDX-License-Identifier: AGPL-3.0-only

package ledgersign

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// MemStore is a Store in memory with the rules of the database store: a write
// names the term and the revision. It is for the tests of this package and of the
// packages that wire it; the daemon uses PGStore.
type MemStore struct {
	mu   sync.Mutex
	snap Snapshot
	// Now is the clock of the "database" (default time.Now).
	Now func() time.Time
	// Origins are the answer of LogOrigins.
	Origins []string
	// MaxSize is the answer of MaxCheckpointSize.
	MaxSize uint64
	// Hook, when set, is called inside Commit after the checks and before the change is kept; an
	// error from it aborts the commit (a crash between the checks and the write).
	Hook func(ch Change) error
	// Commits counts the changes kept.
	Commits int
}

// NewMemStore returns an empty store whose term is 0.
func NewMemStore() *MemStore {
	return &MemStore{snap: Snapshot{Cluster: ClusterRow{PublicationInterval: 60, AnchorInterval: 3600, AnchorMinimum: 1}}}
}

// SetTerm sets the stored term (what scheduler.BumpTerm does in the database) and returns it.
func (m *MemStore) SetTerm(t int64) {
	m.mu.Lock()
	m.snap.Term = t
	m.mu.Unlock()
}

// BumpTerm increments the stored term and returns it.
func (m *MemStore) BumpTerm(context.Context) (scheduler.Term, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snap.Term++
	return scheduler.Term(m.snap.Term), nil
}

func clone(s Snapshot) Snapshot {
	out := s
	out.Cluster.ClusterID = append([]byte(nil), s.Cluster.ClusterID...)
	if s.Cluster.ClusterID == nil {
		out.Cluster.ClusterID = nil
	}
	out.Cluster.TrustFile = append([]byte(nil), s.Cluster.TrustFile...)
	if s.Cluster.TrustFile == nil {
		out.Cluster.TrustFile = nil
	}
	out.Cluster.Holders = append([]string(nil), s.Cluster.Holders...)
	out.Keys = make([]KeyRow, len(s.Keys))
	for i, k := range s.Keys {
		k.Public = append(ed25519.PublicKey(nil), k.Public...)
		out.Keys[i] = k
	}
	out.Events = make([]EventRow, len(s.Events))
	for i, e := range s.Events {
		e.Note = append([]byte(nil), e.Note...)
		out.Events[i] = e
	}
	return out
}

// Load implements Store.
func (m *MemStore) Load(context.Context) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return clone(m.snap), nil
}

// Commit implements Store.
func (m *MemStore) Commit(_ context.Context, term scheduler.Term, expect int64, ch Change) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snap.Term != int64(term) {
		return scheduler.ErrDeposed
	}
	if m.snap.Revision != expect {
		return ErrConflict
	}
	next := clone(m.snap)
	if ch.Cluster != nil {
		next.Cluster = *ch.Cluster
	}
	for _, k := range ch.Keys {
		replaced := false
		for i := range next.Keys {
			if string(next.Keys[i].Public) == string(k.Public) {
				next.Keys[i] = k
				replaced = true
			}
		}
		if !replaced {
			next.Keys = append(next.Keys, k)
		}
	}
	for _, e := range ch.Events {
		if e.Seq != int64(len(next.Events)) {
			return fmt.Errorf("ledgersign: event %d is not the next one (%d)", e.Seq, len(next.Events))
		}
		for _, o := range next.Events {
			if e.Subject != "" && o.Type == e.Type && o.Subject == e.Subject {
				return fmt.Errorf("ledgersign: an event %s of %s exists already", e.Type, e.Subject)
			}
		}
		next.Events = append(next.Events, e)
	}
	if m.Hook != nil {
		if err := m.Hook(ch); err != nil {
			return err
		}
	}
	next.Revision++
	m.snap = clone(next)
	m.Commits++
	return nil
}

// DBTime implements Store.
func (m *MemStore) DBTime(context.Context) (time.Time, error) {
	if m.Now != nil {
		return m.Now(), nil
	}
	return time.Now(), nil
}

// LogOrigins implements Store.
func (m *MemStore) LogOrigins(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.Origins...), nil
}

// MaxCheckpointSize implements Store.
func (m *MemStore) MaxCheckpointSize(context.Context) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.MaxSize, nil
}

// Overwrite replaces the stored keys without any check (a test that damages the store).
func (m *MemStore) Overwrite(f func(s *Snapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&m.snap)
}
