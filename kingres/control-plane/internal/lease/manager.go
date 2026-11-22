// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	// InstanceFor returns the primary instance of a group this server runs; it replaces
	// Instance when set (after a failover a server runs primaries that are not
	// instance 1). false: no instance of this server serves the group now.
	InstanceFor func(group int32) (Instance, bool)
	// Unattributed returns the instances that may be the primary of the group but that the role files cannot
	// attribute to it (a role file that cannot be read, two directories claiming the group, a role file that names
	// another group than the catalog): the lease fails closed for them (F2-06). A fence stops them, a hand-over of
	// the lease is refused while one runs.
	Unattributed func(group int32) []Instance
	// Enforced tells whether losing the lease of the group stops the instance (a
	// group with a standby). It is asked at the moment of the decision.
	Enforced func(group int32) bool
	// OnFenceFailed, when set, is called every time a stop of a primary whose lease is lost fails (the alert
	// oldPrimaryNotStopped of the design, 6.4).
	OnFenceFailed func(group int32, err error)
	// Dir holds the fence marker and the cached topology (the lease directory).
	Dir string
	// WatchdogFile is the file the watchdog reads (empty: no watchdog).
	WatchdogFile string
	// WatchdogFileFor and EnsureWatchdogFor are the per group forms of the two fields
	// around this one: each primary instance has its own lease file and its own watchdog.
	WatchdogFileFor   func(group int32) string
	EnsureWatchdogFor func(group int32)
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
	// lastFence is when a fence of this group was last tried while the lease stayed lost (F2-05).
	lastFence  time.Duration
	triedFence bool
	// rounds counts the finished rounds; refused and waitMs are what the last round that did not
	// reach a majority learned (AdoptionStatus).
	rounds  int
	refused map[string]string
	waitMs  int64
	// What the log says when the lease is lost, so that a loss is explained by what the rounds saw: when the last round started
	// and how long it took (monotonic), how many grantors answered yes, and when a round last reached a majority.
	lastStart, lastTook, lastOKStart time.Duration
	lastGranted                      int
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

	// wmu is held while the watchdog file of a group is written, from the reading of the state to the end of the write (F6-05); it is taken before mu.
	wmu sync.Mutex

	op sync.Mutex // serializes fencing and starting the instance

	// rmu is held by a renewal round of a group while it runs; a release takes it after the group is marked as
	// given up, so that a renewal sent before the release cannot be received by a grantor after it (FO-13).
	rmu sync.Mutex

	// fl counts the renewal requests that are on their way to a grantor (flights.go).
	fl flights

	// starting is set while the instance is being started after the lease was won;
	// the start runs beside the renewal rounds, so that the lease is renewed while
	// PostgreSQL starts or recovers. wg tracks it (tests wait on it).
	starting map[int32]bool
	// fencing is set from the moment a fence of the group begins until its stop has ended. A round that wins the
	// lease in that time must not start the instance: the start would find it still running (and do nothing),
	// the stop would then end it, and the markers the start removed would leave nothing to start it again.
	fencing map[int32]bool
	wg      sync.WaitGroup
	// enf is what the lease manager last knew about which groups are enforced
	// (loaded lazily from the lease directory, see recordEnforced).
	enf map[int32]bool
	// adopted are the groups renewed although the catalog does not name this server as
	// their primary (adopt.go); relinq the groups whose lease was given up and is not renewed.
	adopted map[int32]*adoption
	relinq  map[int32]bool
	// ambWarned is when the warning about an unattributed instance of the group was last logged.
	ambWarned map[int32]time.Duration
	// fenceFail is the set of groups whose last fence could not stop an instance (the alert oldPrimaryNotStopped, F2-05).
	fenceFail map[int32]bool
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
	m := &Manager{c: c, groups: map[int32]*groupState{}, starting: map[int32]bool{}, fencing: map[int32]bool{}, adopted: map[int32]*adoption{}, relinq: map[int32]bool{}, base: uint64(time.Now().UnixNano())}
	m.loadRelinquished()
	if b, err := os.ReadFile(filepath.Join(c.Dir, topologyFile)); err == nil {
		var t Topology
		if json.Unmarshal(b, &t) == nil {
			m.topo, m.haveT = t, true
		}
	}
	return m, nil
}

// Gate is the start gate of the instance manager (instance.Manager.SetStartGate):
// an instance whose lease was lost, or that waits for its lease since kingresd
// started (the wait marker), does not start until the manager holds a lease and
// removes the marker. A standby instance this server runs for another server's
// group belongs to that group's lease, not to this one, and is never held back.
func Gate(dir string, primaryGroup func(id int) (group int32, primary bool)) func(id int) error {
	return func(id int) error {
		group, primary := primaryGroup(id)
		if !primary {
			return nil
		}
		if markerExists(dir, RelinquishedMarker, group) {
			return fmt.Errorf("%w: this server gave the lease of the group up", instance.ErrLeaseRequired)
		}
		if markerExists(dir, FencedMarker, group) {
			return fmt.Errorf("%w: the lease of the group was lost", instance.ErrLeaseRequired)
		}
		if waitMarkerExists(dir, group) {
			return fmt.Errorf("%w: the lease of the group is not held yet", instance.ErrLeaseRequired)
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
	// The catalog is read within one renewal period: a catalog that does not answer (its instance is busy, or starting again) must not
	// keep the rounds from being asked, the cached topology serves (as for any error), or the lease runs out because nothing asked for it.
	tctx, tcancel := context.WithTimeout(ctx, max(m.c.Renew, 500*time.Millisecond))
	topo, err := m.c.Source.Topology(tctx)
	tcancel()
	m.mu.Lock()
	if err == nil {
		changed := !sameTopology(m.topo, topo)
		migrateLegacyMarkers(m.c.Dir, topo.Groups)
		m.topo, m.haveT = topo, true
		if changed {
			m.saveTopologyLocked()
		}
	}
	t, ok := m.topo, m.haveT
	var groups []int32
	if ok {
		groups = m.groupsToRenewLocked(t)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	for _, g := range groups {
		m.rmu.Lock()
		// The list was taken before the rounds began; a group that was released or handed over meanwhile is not
		// asked for (F2-02). The check is made with the round lock held, which ReleaseGroup waits for.
		m.warnUnattributed(g)
		if m.mayRenew(g, t) {
			m.round(ctx, g, t.Members)
		}
		m.rmu.Unlock()
		// A group that is not enforced keeps its watchdog file fresh whether or not the round succeeded (FO-01).
		if wf := m.watchdogFile(g); wf != "" && !m.c.Enforced(g) {
			m.writeWatchdogIdle(wf)
		}
	}
}

// touchIdle refreshes the watchdog file of every group of this server that is not enforced.
func (m *Manager) touchIdle() {
	m.mu.Lock()
	t := m.topo
	groups := append([]int32(nil), t.Groups...)
	for g := range m.adopted {
		groups = append(groups, g)
	}
	m.mu.Unlock()
	for _, g := range groups {
		if wf := m.watchdogFile(g); wf != "" && !m.c.Enforced(g) {
			m.writeWatchdogIdle(wf)
		}
	}
}

// mayRenew says whether a round for the group may be sent now: not for a group that was relinquished, and not for one
// that is neither in the catalog's list nor adopted any more.
func (m *Manager) mayRenew(g int32, t Topology) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.relinq[g] {
		return false
	}
	for _, x := range t.Groups {
		if x == g {
			return true
		}
	}
	return m.adopted[g] != nil
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
	type vote struct {
		server  string
		granted bool
		reply   Reply
	}
	results := make(chan vote, len(members))
	for _, s := range members {
		m.fl.add(group)
		go func() {
			defer m.fl.done(group)
			if s == m.c.Self {
				r, err := m.c.Grantor.Grant(group, m.c.Self, m.c.Duration, false)
				results <- vote{s, err == nil && r.Granted, r}
				return
			}
			r, err := m.c.Voter.Grant(rctx, s, req)
			results <- vote{s, err == nil && r.Granted, r}
		}()
	}
	granted := 0
	ok := false
	refused := map[string]string{}
	var waits []int64
	for i := 0; i < len(members); i++ {
		v := <-results
		if v.granted {
			granted++
		} else if v.reply.Holder != "" {
			refused[v.server] = v.reply.Holder
			waits = append(waits, v.reply.WaitMs)
		}
		if granted >= need {
			ok = true
			break
		}
	}
	var waitMs int64
	if !ok && len(waits) >= need-granted && need-granted > 0 {
		sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
		waitMs = waits[need-granted-1]
	}
	m.mu.Lock()
	st := m.state(group)
	st.roundDone = true
	st.rounds++
	st.lastStart, st.lastTook, st.lastGranted = start, m.c.Clock.Mono()-start, granted
	if ok {
		st.lastOKStart = start
		st.refused, st.waitMs = nil, 0
	} else {
		st.refused, st.waitMs = refused, waitMs
	}
	wasLost := st.lost
	if ok && start+m.c.Duration > st.validUntil {
		st.validUntil, st.have = start+m.c.Duration, true
	}
	valid := st.have && m.c.Clock.Mono() < st.validUntil-m.c.Margin
	if ok && valid {
		st.lost = false
		delete(m.fenceFail, group) // the lease is held again: nothing to stop
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
	enforced := m.c.Enforced(group)
	m.recordEnforced(group, enforced)
	m.mu.Lock()
	if m.starting[group] {
		m.mu.Unlock()
		return // the start that is under way finishes the work below
	}
	if m.fencing[group] {
		// A fence is stopping the primary right now. Its marker is on disk, so the next round that holds the lease
		// starts the instance once the stop has ended; starting now would race with the stop.
		m.mu.Unlock()
		return
	}
	if Waiting(m.c.Dir, group) {
		// The primary was fenced, or kingresd found it stopped at its start while the
		// lease is enforced: the lease is held, so it may start. This runs beside the
		// next rounds: a start with crash recovery can take longer than a lease.
		m.starting[group] = true
		m.wg.Add(1)
		m.mu.Unlock()
		go func() {
			defer m.wg.Done()
			m.startPrimary(ctx, group, wasLost)
		}()
		return
	}
	m.mu.Unlock()
	m.afterAcquired(ctx, group, wasLost)
}

// startPrimary removes the markers, starts the instance and finishes what
// acquired does for a running one. A failed start puts the markers back, so that
// nothing but this path starts the primary and the next round tries again.
func (m *Manager) startPrimary(ctx context.Context, group int32, wasLost bool) {
	defer func() { m.mu.Lock(); delete(m.starting, group); m.mu.Unlock() }()
	m.op.Lock()
	marks := map[string]bool{}
	for _, n := range []string{FencedMarker, WaitMarker} {
		if markerExists(m.c.Dir, n, group) {
			marks[MarkerPath(m.c.Dir, n, group)] = true
			removeMarker(m.c.Dir, n, group)
		}
	}
	if len(marks) > 0 {
		inst, have := m.instance(group)
		var err error
		if !have {
			err = fmt.Errorf("no instance of this server serves shard group %d", group)
		} else {
			err = inst.Start(ctx)
		}
		if err == nil && markerExists(m.c.Dir, FencedMarker, group) {
			// A fence arrived while the start ran (it did not wait for it): the instance that came up must not
			// stay up. The fence wrote its marker before it looked at the instance.
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			_ = inst.StopImmediate(sctx)
			cancel()
			m.op.Unlock()
			return
		}
		if err != nil {
			m.c.Log.Error("starting the primary after the lease was won", "group", group, "error", err)
			for p := range marks {
				_ = pki.WriteFile(p, []byte("waiting for the group lease\n"), 0o600) // not started: stay held back
			}
			m.op.Unlock()
			return
		}
		m.c.Log.Info("the group lease is held: the primary instance was started", "group", group)
	}
	m.op.Unlock()
	m.afterAcquired(ctx, group, wasLost)
}

func (m *Manager) afterAcquired(ctx context.Context, group int32, wasLost bool) {
	if wasLost {
		m.c.Log.Warn("the group lease is held again", "group", group)
		m.audit(ctx, AuditLeaseAcquired, group, "the lease was obtained again after it was lost")
	}
	if m.c.Enforced(group) {
		m.writeWatchdogFile(group)
		m.ensureWatchdog(group)
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
	var expired, refence []int32
	now := m.c.Clock.Mono()
	for g, st := range m.groups {
		if st.roundDone && st.lost {
			// The lease is over and was not won again: the stop is repeated until it succeeds, and whether the group is
			// enforced is asked every time (a group that became enforced after its lease ran out must not keep a primary
			// that runs with no lease, F2-05).
			if !st.triedFence || now-st.lastFence >= FenceRetry {
				st.triedFence, st.lastFence = true, now
				refence = append(refence, g)
			}
			continue
		}
		if !st.roundDone || st.lost {
			continue
		}
		if !st.have && waitMarkerExists(m.c.Dir, g) {
			// This kingresd has not started the primary yet because it holds no
			// lease: there is nothing to fence and nothing was lost, the lease is
			// simply not held yet (state "none"). The first round that wins it starts
			// the instance.
			continue
		}
		if !st.have || m.c.Clock.Mono() >= st.validUntil-m.c.Margin {
			st.lost = true
			st.triedFence, st.lastFence = true, now
			expired = append(expired, g)
		}
	}
	m.mu.Unlock()
	for _, g := range expired {
		m.lose(ctx, g)
	}
	for _, g := range refence {
		m.refence(ctx, g)
	}
}

// FenceRetry is how often the stop of a primary whose lease is lost is tried again while it still runs.
const FenceRetry = 5 * time.Second

// refence stops the primary of a group whose lease is still lost, when it runs and the group is enforced now.
func (m *Manager) refence(ctx context.Context, group int32) {
	if a := m.adoptionOf(group); a != nil && !a.attached {
		return
	}
	m.mu.Lock()
	busy := m.fencing[group]
	m.mu.Unlock()
	if busy || !m.c.Enforced(group) {
		return
	}
	var insts []Instance
	if inst, have := m.instance(group); have {
		insts = append(insts, inst)
	}
	insts = append(insts, m.unattributed(group)...)
	anyUp := false
	for _, inst := range insts {
		if up, err := inst.Running(ctx); err != nil || up {
			anyUp = true
		}
	}
	if !anyUp {
		return
	}
	if err := m.Fence(ctx, group); err != nil {
		m.c.Log.Error("fencing the primary again", "group", group, "error", err)
		if m.c.OnFenceFailed != nil {
			m.c.OnFenceFailed(group, err)
		}
	}
}

func (m *Manager) lose(ctx context.Context, group int32) {
	if a := m.adoptionOf(group); a != nil && !a.attached {
		// A lease that guards no instance (the fence holder of a failover, or the server that is
		// about to promote a standby): there is nothing to stop and no marker to write.
		m.c.Log.Warn("an adopted group lease was lost", "group", group, "kind", a.kind)
		return
	}
	// Nothing runs here that the lease protects: no instance serves the group and none may be its primary. The group is
	// still in the cached topology of a server whose old primary was rebuilt as a standby while its catalog could not be
	// read; a marker written now would hold the next primary of the group back for ever.
	if _, have := m.instance(group); !have && len(m.unattributed(group)) == 0 {
		m.c.Log.Warn("a group lease was lost and no instance of this server serves the group: nothing to fence", "group", group)
		return
	}
	enforced := m.c.Enforced(group)
	m.mu.Lock()
	var why []any
	if st := m.groups[group]; st != nil {
		now := m.c.Clock.Mono()
		why = []any{"rounds", st.rounds, "lastRoundStartedAgo", (now - st.lastStart).Round(time.Millisecond), "lastRoundTook", st.lastTook.Round(time.Millisecond),
			"lastRoundGranted", st.lastGranted, "lastMajorityStartedAgo", (now - st.lastOKStart).Round(time.Millisecond), "refusedBy", st.refused}
	}
	m.mu.Unlock()
	m.c.Log.Warn("the group lease was lost", append([]any{"group", group, "enforced", enforced}, why...)...)
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
	m.mu.Lock()
	m.fencing[group] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.fencing, group); m.mu.Unlock() }()
	// A fence never waits behind anything (ST-04): not behind m.op, which a start of the primary holds
	// across its Start, and StopImmediate takes no lifecycle lock. A start that is under way is stopped
	// again by the check at its end (startPrimary).
	if err := pki.WriteFile(FencedPath(m.c.Dir, group), []byte("fenced\n"), 0o600); err != nil {
		return fmt.Errorf("writing the fence marker: %w", err)
	}
	// The instance that serves the group, and every instance that may (F2-06): all of them are stopped.
	var insts []Instance
	if inst, have := m.instance(group); have {
		insts = append(insts, inst)
	}
	insts = append(insts, m.unattributed(group)...)
	var first error
	stopped := false
	for _, inst := range insts {
		if up, err := inst.Running(ctx); err == nil && !up {
			// Nothing runs that could be stopped: the marker keeps it from starting
			// without a lease, and no stop is claimed in the audit log.
			continue
		}
		// The stop must not be cut short by the caller's context: a fence that
		// does not happen is the failure the whole protocol exists to prevent.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := inst.StopImmediate(sctx)
		cancel()
		if err != nil {
			if first == nil {
				first = fmt.Errorf("stopping the primary: %w", err)
			}
			continue
		}
		stopped = true
	}
	if stopped {
		m.audit(ctx, AuditLeaseFenced, group, "the primary instance was stopped with an immediate shutdown")
	}
	m.mu.Lock()
	if first != nil {
		if m.fenceFail == nil {
			m.fenceFail = map[int32]bool{}
		}
		m.fenceFail[group] = true
	} else {
		delete(m.fenceFail, group)
	}
	m.mu.Unlock()
	return first
}

// FenceFailures lists the groups whose instance a fence could not stop and whose lease has not been won again since
// (the alert oldPrimaryNotStopped; the stop is repeated every FenceRetry).
func (m *Manager) FenceFailures() []int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []int32
	for g := range m.fenceFail {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// unattributed is Config.Unattributed, nil-safe.
func (m *Manager) unattributed(group int32) []Instance {
	if m.c.Unattributed == nil {
		return nil
	}
	return m.c.Unattributed(group)
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
	// The file of a group that is not enforced is refreshed by a loop of its own (F2-07), never behind a round: a pass
	// of rounds that get no answer lasts one round time per group, and a file written only after each round would
	// run out meanwhile and make the watchdog stop a healthy single-copy primary. The rule: a group without a standby
	// is stopped for a lapsed lease by nothing, only for a kingresd that stopped running.
	idleDone := make(chan struct{})
	go func() {
		defer close(idleDone)
		it := time.NewTicker(min(m.c.Renew, time.Second))
		defer it.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-it.C:
				m.touchIdle()
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
			<-idleDone
			m.wg.Wait()
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

// Confirming tells whether an enforced group whose primary this server runs is not
// yet confirmed by a lease: it has none that is valid and the manager has not yet
// judged it lost. That is the time before the first renewal round of this kingresd
// has finished: a restarted kingresd finds the instance running, but the watchdog
// still counts down the lease the previous kingresd renewed and fences the
// instance when it runs out before the first round has written a new one. Until
// the round is done the instance may disappear at any moment, so the API must not
// sign anybody in against it (the window is at most one round, duration minus
// margin).
//
// It is not true while a renewal fails but the lease still holds: the instance
// runs until the lease ends, and a follower that lost its leader for a few seconds
// must go on serving. When the lease ends the manager marks the group lost and
// fences the instance (see Check); from then on the instance is simply not there,
// which the requests meet in the usual way.
func (m *Manager) Confirming() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.haveT {
		return false // unknown groups
	}
	for _, g := range m.topo.Groups {
		if waitMarkerExists(m.c.Dir, g) {
			continue // an instance that is held back anyway
		}
		st := m.groups[g]
		if st != nil && (st.lost || (st.have && m.c.Clock.Mono() < st.validUntil-m.c.Margin)) {
			continue
		}
		if m.c.Enforced(g) || (st == nil && slices.Contains(readEnforced(m.c.Dir).Groups, g)) {
			return true
		}
	}
	return false
}

// instance is the primary instance of the group on this server.
func (m *Manager) instance(group int32) (Instance, bool) {
	if m.c.InstanceFor != nil {
		return m.c.InstanceFor(group)
	}
	return m.c.Instance, m.c.Instance != nil
}

func (m *Manager) watchdogFile(group int32) string {
	if m.c.WatchdogFileFor != nil {
		return m.c.WatchdogFileFor(group)
	}
	return m.c.WatchdogFile
}

func (m *Manager) ensureWatchdog(group int32) {
	switch {
	case m.c.EnsureWatchdogFor != nil:
		m.c.EnsureWatchdogFor(group)
	case m.c.EnsureWatchdog != nil:
		m.c.EnsureWatchdog()
	}
}

func (m *Manager) adoptionOf(group int32) *adoption {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.adopted[group]
}

// warnUnattributed logs, once a minute per group, an instance that may be the primary of the group and that the role
// files cannot attribute to it (F2-06). The alert kinds of the contract have none for it; the log line is what there is.
func (m *Manager) warnUnattributed(group int32) {
	if m.c.Unattributed == nil || len(m.c.Unattributed(group)) == 0 {
		return
	}
	now := m.c.Clock.Mono()
	m.mu.Lock()
	last, seen := m.ambWarned[group]
	if seen && now-last < time.Minute {
		m.mu.Unlock()
		return
	}
	if m.ambWarned == nil {
		m.ambWarned = map[int32]time.Duration{}
	}
	m.ambWarned[group] = now
	m.mu.Unlock()
	m.c.Log.Error("an instance of this server may be the primary of the group and cannot be attributed to it by its role file: it is held by the start gate, stopped by a fence and counted as running by a hand-over of the lease", "group", group)
}
