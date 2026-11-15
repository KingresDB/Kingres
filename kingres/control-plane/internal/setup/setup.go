// SPDX-License-Identifier: AGPL-3.0-only

// Package setup owns the node state machine (uninitialized, standalone,
// clustered) and the one-time setup token.
//
// The state is persisted in the data directory (node-state.json) with an
// atomic replace, so a crash never leaves a half-written file. The setup
// token is generated on the first start while the node is uninitialized and
// written to a file that only the owner can read; it is compared in constant
// time and is never logged.
package setup

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// State is the lifecycle state of a node.
type State string

// The node states of the contract (schema NodeState).
const (
	Uninitialized State = "uninitialized"
	Standalone    State = "standalone"
	Clustered     State = "clustered"
)

// File names inside the data directory.
const (
	StateFileName = "node-state.json"
	TokenFileName = "setup-token"
)

// ErrInvalidTransition is returned for a state change the state machine does
// not allow.
var ErrInvalidTransition = errors.New("invalid node state transition")

// ErrTokenConsumed is returned by VerifyToken when the node is no longer
// uninitialized and the token has been removed.
var ErrTokenConsumed = errors.New("setup token is no longer valid")

// allowed lists the permitted transitions of the state machine.
var allowed = map[State][]State{
	Uninitialized: {Standalone, Clustered},
	Standalone:    {Clustered},
	Clustered:     {Standalone},
}

// Persisted is the content of node-state.json.
type Persisted struct {
	// State is the node state.
	State State `json:"state"`
	// ClusterID is the UUID of the cluster once the node belongs to one.
	ClusterID string `json:"clusterId,omitempty"`
	// ClusterName is the cluster name.
	ClusterName string `json:"clusterName,omitempty"`
	// ServerID is the server id once the node belongs to a cluster.
	ServerID *string `json:"serverId,omitempty"`
	// SetupTask is the task started by init-cluster or join-cluster; it stays
	// after the task finished and after the node left uninitialized.
	SetupTask *api.SetupTaskSummary `json:"setupTask,omitempty"`
	// ChangedAt is when the state last changed.
	ChangedAt time.Time `json:"changedAt"`
}

// Manager holds the node state and the setup token.
type Manager struct {
	dir string
	now func() time.Time

	mu        sync.RWMutex
	persisted Persisted
	tokenHash [sha256.Size]byte // digest of the token, zero when none exists
	hasToken  bool
}

// Open loads (or creates) the node state in dir and, while the node is
// uninitialized, loads (or generates) the setup token. now supplies the
// clock; pass time.Now in production.
func Open(dir string, now func() time.Time) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data directory: %w", err)
	}
	m := &Manager{dir: dir, now: now}
	if err := m.loadState(); err != nil {
		return nil, err
	}
	if m.persisted.State == Uninitialized {
		if err := m.loadOrCreateToken(); err != nil {
			return nil, err
		}
	} else {
		// A token file left over from an earlier state must not stay valid.
		_ = os.Remove(m.TokenPath())
	}
	return m, nil
}

// StatePath returns the path of the state file.
func (m *Manager) StatePath() string { return filepath.Join(m.dir, StateFileName) }

// TokenPath returns the path of the setup token file.
func (m *Manager) TokenPath() string { return filepath.Join(m.dir, TokenFileName) }

func (m *Manager) loadState() error {
	data, err := os.ReadFile(m.StatePath())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		m.persisted = Persisted{State: Uninitialized, ChangedAt: m.now().UTC()}
		return m.writeState()
	case err != nil:
		return fmt.Errorf("reading node state: %w", err)
	}
	var p Persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("parsing %s: %w", m.StatePath(), err)
	}
	switch p.State {
	case Uninitialized, Standalone, Clustered:
	default:
		return fmt.Errorf("%s: unknown node state %q", m.StatePath(), p.State)
	}
	m.persisted = p
	return nil
}

// writeState replaces the state file atomically. Callers hold m.mu or are
// in Open.
func (m *Manager) writeState() error {
	data, err := json.MarshalIndent(m.persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding node state: %w", err)
	}
	return writeFileAtomic(m.StatePath(), append(data, '\n'))
}

func (m *Manager) loadOrCreateToken() error {
	data, err := os.ReadFile(m.TokenPath())
	switch {
	case err == nil:
		tok := trimToken(data)
		if len(tok) >= 32 {
			m.tokenHash = sha256.Sum256(tok)
			m.hasToken = true
			return nil
		}
		// A truncated file is not trusted: replace it.
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("reading setup token: %w", err)
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generating setup token: %w", err)
	}
	tok := []byte(hex.EncodeToString(raw))
	if err := writeFileAtomic(m.TokenPath(), append(tok, '\n')); err != nil {
		return fmt.Errorf("writing setup token: %w", err)
	}
	m.tokenHash = sha256.Sum256(tok)
	m.hasToken = true
	return nil
}

func trimToken(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

// State returns the current node state.
func (m *Manager) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.persisted.State
}

// Snapshot returns the persisted node data.
func (m *Manager) Snapshot() Persisted {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.persisted
}

// VerifyToken reports whether presented is the setup token. It compares the
// SHA-256 digests in constant time, so neither the content nor the length of
// the real token leaks through timing. It returns ErrTokenConsumed when the
// node is not uninitialized.
func (m *Manager) VerifyToken(presented string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.persisted.State != Uninitialized || !m.hasToken {
		return false, ErrTokenConsumed
	}
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(got[:], m.tokenHash[:]) == 1, nil
}

// Transition moves the node to a new state and persists it. Moving out of
// uninitialized consumes the setup token: the token file is removed.
// clusterID, clusterName and serverID are stored when non-empty.
func (m *Manager) Transition(to State, clusterID, clusterName string, serverID *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	from := m.persisted.State
	ok := false
	for _, s := range allowed[from] {
		if s == to {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("%w: %s to %s", ErrInvalidTransition, from, to)
	}
	next := m.persisted
	next.State = to
	next.ChangedAt = m.now().UTC()
	if clusterID != "" {
		next.ClusterID = clusterID
	}
	if clusterName != "" {
		next.ClusterName = clusterName
	}
	if serverID != nil {
		next.ServerID = serverID
	}
	prev := m.persisted
	m.persisted = next
	if err := m.writeState(); err != nil {
		m.persisted = prev
		return err
	}
	if from == Uninitialized {
		m.hasToken = false
		m.tokenHash = [sha256.Size]byte{}
		if err := os.Remove(m.TokenPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing consumed setup token: %w", err)
		}
	}
	return nil
}

// Reset returns a clustered node to uninitialized after the cluster removed it
// (design D5): the cluster identity is forgotten and a new setup token is
// generated, so the server can join a cluster again.
func (m *Manager) Reset() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.persisted.State == Uninitialized {
		return nil
	}
	if m.persisted.State != Clustered {
		return fmt.Errorf("%w: %s to uninitialized", ErrInvalidTransition, m.persisted.State)
	}
	prev := m.persisted
	m.persisted = Persisted{State: Uninitialized, ChangedAt: m.now().UTC()}
	if err := m.writeState(); err != nil {
		m.persisted = prev
		return err
	}
	return m.loadOrCreateToken()
}

// SetSetupTask records the summary of the setup task so that GET /setup/state
// can report it; it is persisted with the node state.
func (m *Manager) SetSetupTask(t api.SetupTaskSummary) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.persisted
	m.persisted.SetupTask = &t
	if err := m.writeState(); err != nil {
		m.persisted = prev
		return err
	}
	return nil
}

// writeFileAtomic writes data to a temporary file in the
// same directory, syncs it and renames it over path. os.CreateTemp creates
// the file with O_EXCL and mode 0600 (owner only) on Unix, so it is never
// readable by others, not even briefly. On Windows the directory ACL applies.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("writing temporary file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("syncing temporary file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing temporary file: %w", err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}
