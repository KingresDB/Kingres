// SPDX-License-Identifier: PostgreSQL

package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// Names of the states in ha.LeaseInfo.State.
const (
	StateHeld    = "held"
	StateLost    = "lost"
	StateGranted = "granted"
	StateNone    = "none"
)

// Audit action names of the lease events (contract 0.7.0 adds them to AuditAction).
const (
	AuditLeaseLost     = "leaseLost"
	AuditLeaseFenced   = "leaseFenced"
	AuditLeaseAcquired = "leaseAcquired"
)

// Topology is what the holder needs from the catalog.
type Topology struct {
	// Groups are the shard groups whose primary instance this server runs.
	Groups []int32 `json:"groups"`
	// Members are the server ids of every server of the cluster (this one
	// included): the majority is counted over them.
	Members []string `json:"members"`
}

// Source reads the topology (the local copy of the catalog).
type Source interface {
	Topology(ctx context.Context) (Topology, error)
}

// Voter asks another server's grantor.
type Voter interface {
	Grant(ctx context.Context, server string, req Request) (Reply, error)
}

// Instance is the primary instance the lease protects.
type Instance interface {
	Running(ctx context.Context) (bool, error)
	Start(ctx context.Context) error
	StopImmediate(ctx context.Context) error
}

// Config configures a Manager.
type Config struct {
	Self                    string
	Clock                   clock.Clock
	Duration, Renew, Margin time.Duration
	Grantor                 *Grantor
	Voter                   Voter
	Source                  Source
	Instance                Instance
	// Enforced tells whether losing the lease of the group stops the instance (a
	// group with a standby). It is asked at the moment of the decision.
	Enforced func(group int32) bool
	// Dir holds the fence marker and the cached topology (the lease directory).
	Dir string
	// WatchdogFile is the file the watchdog reads (empty: no watchdog).
	WatchdogFile string
	// EnsureWatchdog makes sure the watchdog runs next to the instance; called
	// after a successful renewal of an enforced group.
	EnsureWatchdog func()
	// Audit records an audited event (nil: none).
	Audit func(ctx context.Context, action string, group int32, detail string)
	Log   *slog.Logger
}

type groupState struct {
	// validUntil is the end of the lease on this server's monotonic clock: the
	// start of the renewal round that obtained it plus the duration.
	validUntil time.Duration
	have       bool
	// roundDone is set when the first round after start has finished, whatever
	// its outcome: until then the instance is not judged (see Check).
	roundDone bool
	lost      bool
}

// Manager is the holder side of the lease protocol. It implements ha.Leases.
type Manager struct {
	c Config

	mu     sync.Mutex
	groups map[int32]*groupState
	topo   Topology
	haveT  bool
	seq    uint64
	base   uint64 // distinguishes the sequence numbers of one process start from the next

	op sync.Mutex // serializes fencing and starting the instance
}

const (
	topologyFile = "topology.json"
	// FencedMarker is the name of the file whose presence means "this primary was
	// fenced and may not start without a lease". The watchdog writes it too.
	FencedMarker = "fenced"
)

// New builds a manager.
func New(c Config) (*Manager, error) {
	if c.Duration <= c.Margin || c.Renew <= 0 || c.Margin <= 0 {
		return nil, errors.New("lease: duration must exceed the margin, renew and margin must be positive")
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	if c.Enforced == nil {
		c.Enforced = func(int32) bool { return false }
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("lease: creating %s: %w", c.Dir, err)
	}
	m := &Manager{c: c, groups: map[int32]*groupState{}, base: uint64(time.Now().UnixNano())}
	if b, err := os.ReadFile(filepath.Join(c.Dir, topologyFile)); err == nil {
		var t Topology
		if json.Unmarshal(b, &t) == nil {
			m.topo, m.haveT = t, true
		}
	}
	return m, nil
}

// Gate is the start gate of the instance manager (instance.Manager.SetStartGate):
// an instance whose lease was lost does not start until the manager holds a
// lease again and removes the marker.
func Gate(dir string) func(id int) error {
	return func(int) error {
		if _, err := os.Stat(filepath.Join(dir, FencedMarker)); err == nil {
			return fmt.Errorf("%w: the lease of the group was lost", instance.ErrLeaseRequired)
		}
		return nil
	}
}

func majority(members []string) int { return len(members)/2 + 1 }

func (m *Manager) state(g int32) *groupState {
	s := m.groups[g]
	if s == nil {
		s = &groupState{}
		m.groups[g] = s
	}
	return s
}

// Renew runs one renewal round for every group this server runs the primary
// of. The lease obtained is valid from the START of the round, never from the
// moment the answers arrived.
func (m *Manager) Renew(ctx context.Context) {
	topo, err := m.c.Source.Topology(ctx)
	m.mu.Lock()
	if err == nil {
		changed := !sameTopology(m.topo, topo)
		m.topo, m.haveT = topo, true
		if changed {
			m.saveTopologyLocked()
		}
	}
	t, ok := m.topo, m.haveT
	m.mu.Unlock()
	if !ok {
		return
	}
	for _, g := range t.Groups {
		m.round(ctx, g, t.Members)
	}
}

func sameTopology(a, b Topology) bool {
	if len(a.Groups) != len(b.Groups) || len(a.Members) != len(b.Members) {
		return false
	}
	for i := range a.Groups {
		if a.Groups[i] != b.Groups[i] {
			return false
		}
	}
	for i := range a.Members {
		if a.Members[i] != b.Members[i] {
			return false
		}
	}
	return true
}

func (m *Manager) saveTopologyLocked() {
	sort.Slice(m.topo.Members, func(i, j int) bool { return m.topo.Members[i] < m.topo.Members[j] })
	b, _ := json.Marshal(m.topo)
	if err := pki.WriteFile(filepath.Join(m.c.Dir, topologyFile), b, 0o600); err != nil {
		m.c.Log.Warn("caching the lease topology", "error", err)
	}
}

func (m *Manager) round(ctx context.Context, group int32, members []string) {
	start := m.c.Clock.Mono()
	need := majority(members)
	req := Request{Group: group, DurationMs: m.c.Duration.Milliseconds()}
	rctx, cancel := context.WithTimeout(ctx, m.c.Duration-m.c.Margin)
	defer cancel()
	results := make(chan bool, len(members))
	for _, s := range members {
		go func() {
			if s == m.c.Self {
				r, err := m.c.Grantor.Grant(group, m.c.Self, m.c.Duration, false)
				results <- err == nil && r.Granted
				return
			}
			r, err := m.c.Voter.Grant(rctx, s, req)
			results <- err == nil && r.Granted
		}()
	}
	granted := 0
	ok := false
	for i := 0; i < len(members); i++ {
		if <-results {
			granted++
		}
		if granted >= need {
			ok = true
			break
		}
	}
	m.mu.Lock()
	st := m.state(group)
	st.roundDone = true
	wasLost := st.lost
	if ok && start+m.c.Duration > st.validUntil {
		st.validUntil, st.have = start+m.c.Duration, true
	}
	valid := st.have && m.c.Clock.Mono() < st.validUntil-m.c.Margin
	if ok && valid {
		st.lost = false
	}
	m.mu.Unlock()
	if !ok || !valid {
		return
	}
	m.acquired(ctx, group, wasLost)
}

// acquired runs after a round that left a valid lease.
func (m *Manager) acquired(ctx context.Context, group int32, wasLost bool) {
	m.writeWatchdogFile(group)
	m.op.Lock()
	defer m.op.Unlock()
	marker := filepath.Join(m.c.Dir, FencedMarker)
	if _, err := os.Stat(marker); err == nil {
		// The primary was fenced: the lease is held again, so it may start.
		_ = os.Remove(marker)
		if err := m.c.Instance.Start(ctx); err != nil {
			m.c.Log.Error("starting the primary after the lease was regained", "group", group, "error", err)
			_ = pki.WriteFile(marker, []byte("fenced\n"), 0o600) // not started: stay fenced
			return
		}
	}
	if wasLost {
		m.c.Log.Warn("the group lease is held again", "group", group)
		m.audit(ctx, AuditLeaseAcquired, group, "the lease was obtained again after it was lost")
	}
	if m.c.Enforced(group) && m.c.EnsureWatchdog != nil {
		m.writeWatchdogFile(group)
		m.c.EnsureWatchdog()
	}
}

// Check judges the leases against the monotonic clock and fences when an
// enforced lease has expired. It runs every few hundred milliseconds. Until the
// first renewal round after start has finished it judges nothing, so a restart
// of kingresd does not stop a healthy primary before the first round could
// succeed; that window is at most one round (duration minus margin), and a
// primary that was stopped while kingresd was away stays stopped (the fence
// marker).
func (m *Manager) Check(ctx context.Context) {
	m.mu.Lock()
	var expired []int32
	for g, st := range m.groups {
		if !st.roundDone || st.lost {
			continue
		}
		if !st.have || m.c.Clock.Mono() >= st.validUntil-m.c.Margin {
			st.lost = true
			expired = append(expired, g)
		}
	}
	m.mu.Unlock()
	for _, g := range expired {
		m.lose(ctx, g)
	}
}

func (m *Manager) lose(ctx context.Context, group int32) {
	enforced := m.c.Enforced(group)
	m.c.Log.Warn("the group lease was lost", "group", group, "enforced", enforced)
	m.audit(ctx, AuditLeaseLost, group, fmt.Sprintf("no majority renewed the lease in time; enforced=%t", enforced))
	if !enforced {
		return
	}
	if err := m.Fence(ctx, group); err != nil {
		m.c.Log.Error("fencing the primary", "group", group, "error", err)
	}
}

// Fence stops the primary instance with an immediate shutdown after writing the
// marker that keeps it from starting without a lease.
func (m *Manager) Fence(ctx context.Context, group int32) error {
	m.op.Lock()
	defer m.op.Unlock()
	if err := pki.WriteFile(filepath.Join(m.c.Dir, FencedMarker), []byte("fenced\n"), 0o600); err != nil {
		return fmt.Errorf("writing the fence marker: %w", err)
	}
	// The stop must not be cut short by the caller's context: a fence that
	// does not happen is the failure the whole protocol exists to prevent.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := m.c.Instance.StopImmediate(sctx); err != nil {
		return fmt.Errorf("stopping the primary: %w", err)
	}
	m.audit(ctx, AuditLeaseFenced, group, "the primary instance was stopped with an immediate shutdown")
	return nil
}

func (m *Manager) audit(ctx context.Context, action string, group int32, detail string) {
	if m.c.Audit != nil {
		m.c.Audit(ctx, action, group, detail)
	}
}

// Run renews every Renew and checks every few hundred milliseconds until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	checkEvery := min(100*time.Millisecond, m.c.Renew/4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(checkEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.Check(ctx)
			}
		}
	}()
	t := time.NewTicker(m.c.Renew)
	defer t.Stop()
	for {
		m.Renew(ctx)
		select {
		case <-ctx.Done():
			<-done
			return
		case <-t.C:
		}
	}
}

// Holds implements ha.Leases: true only while the lease is valid with the safety
// margin applied.
func (m *Manager) Holds(group int32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.groups[group]
	return st != nil && st.have && !st.lost && m.c.Clock.Mono() < st.validUntil-m.c.Margin
}

// Info implements ha.Leases: the leases this server holds or lost, and the
// leases it granted to others.
func (m *Manager) Info() []ha.LeaseInfo {
	m.mu.Lock()
	now := m.c.Clock.Mono()
	byGroup := map[int32]ha.LeaseInfo{}
	for g, st := range m.groups {
		li := ha.LeaseInfo{Group: g, Holder: m.c.Self, State: StateNone, Enforced: m.c.Enforced(g)}
		switch {
		case st.have && !st.lost && now < st.validUntil-m.c.Margin:
			li.State = StateHeld
			li.RemainingMillis = (st.validUntil - m.c.Margin - now).Milliseconds()
		case st.lost:
			li.State = StateLost
		}
		byGroup[g] = li
	}
	m.mu.Unlock()
	if m.c.Grantor != nil {
		for _, e := range m.c.Grantor.Snapshot() {
			if _, mine := byGroup[e.Group]; mine {
				continue
			}
			byGroup[e.Group] = ha.LeaseInfo{Group: e.Group, Holder: e.Holder, State: StateGranted, RemainingMillis: e.RemainingMs}
		}
	}
	out := make([]ha.LeaseInfo, 0, len(byGroup))
	for _, li := range byGroup {
		out = append(out, li)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out
}

var _ ha.Leases = (*Manager)(nil)
