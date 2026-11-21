// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// RelinquishedMarker is the base name of the per group file that says "this
// server gave the lease of the group up (or is about to hand the group over): it
// does not renew it, and the primary of the group may not start here". It stays
// until the instance is a standby of the group or the hand-over is cancelled
// (Unrelinquish).
const RelinquishedMarker = "relinquished"

// Adoption kinds (design ha-phase5-part2.md, 3.1.1).
const (
	// AdoptFence renews the lease of a group this server does not run the primary of
	// and has no instance for: the fence holder of a failover keeps the old primary
	// from ever being granted the lease again while the new primary is chosen.
	// Losing it fences nothing.
	AdoptFence = "fence"
	// AdoptHolder renews the lease of a group whose primary this server is about to
	// run (the standby that will be promoted). Until Attach, losing it fences
	// nothing either: there is no primary to stop.
	AdoptHolder = "holder"
)

type adoption struct {
	kind     string
	attached bool
}

// AdoptionStatus is what the last renewal round of an adopted group found. A step
// that has to wait for a lease reads it, so that no second protocol is needed.
type AdoptionStatus struct {
	// Held says that a majority granted the lease in the last round and it is valid.
	Held bool `json:"held"`
	// Rounds counts the rounds since the adoption (0: none has finished).
	Rounds int `json:"rounds"`
	// WaitMs is how long until enough grantors would grant the lease, from the
	// refusals of the last round (0 when held or unknown).
	WaitMs int64 `json:"waitMs"`
	// RefusedBy are the servers that refused, with the holder they named.
	RefusedBy map[string]string `json:"refusedBy,omitempty"`
}

// Adopt makes this manager renew the lease of a group that the catalog does not
// name this server as the primary of (yet). It never changes the lease of a group
// the server runs the primary of. kind is AdoptFence or AdoptHolder. Adopting a
// relinquished group clears the marker of the earlier hand-over: the failover that adopts it again decides (FO-10).
func (m *Manager) Adopt(group int32, kind string) error {
	if kind != AdoptFence && kind != AdoptHolder {
		return fmt.Errorf("lease: unknown adoption kind %q", kind)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// A server that gave the group up (a transfer, a release, a switchover) adopts it again only by a decision of
	// the failover that asks (FO-10): the marker of the earlier hand-over is cleared, or a second failover of
	// the same group would be refused for ever. An instance that runs here for the group is not told it may start:
	// the start gate still waits for the lease.
	// The renewal of the group starts again, but the marker file STAYS (F2-04): it is what keeps a directory that
	// the role files still name primary for the group from being started by this server, which now holds the lease of
	// the group as the fence holder or as the holder-to-be. Only Attach (the instance of this adoption is the primary)
	// and Unrelinquish (a cancelled hand-over) remove it.
	delete(m.relinq, group)
	if cur := m.adopted[group]; cur != nil {
		if cur.attached {
			return nil
		}
		cur.kind = kind
		return nil
	}
	m.adopted[group] = &adoption{kind: kind}
	delete(m.groups, group) // a new adoption is judged from its own first round
	return nil
}

// Adopted tells whether the group is adopted and how.
func (m *Manager) Adopted(group int32) (kind string, attached, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.adopted[group]
	if a == nil {
		return "", false, false
	}
	return a.kind, a.attached, true
}

// AdoptionStatus answers from the last round of an adopted (or any renewed) group.
func (m *Manager) AdoptionStatus(group int32) AdoptionStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.groups[group]
	if st == nil {
		return AdoptionStatus{}
	}
	out := AdoptionStatus{Rounds: st.rounds, RefusedBy: map[string]string{}}
	out.Held = st.have && !st.lost && m.c.Clock.Mono() < st.validUntil-m.c.Margin
	for s, h := range st.refused {
		out.RefusedBy[s] = h
	}
	if !out.Held {
		out.WaitMs = st.waitMs
	}
	if len(out.RefusedBy) == 0 {
		out.RefusedBy = nil
	}
	return out
}

// Attach says that the instance of an adopted group now exists (the standby was
// promoted and its role file written): from then on losing the lease fences that
// instance like any primary's, and the watchdog covers it.
func (m *Manager) Attach(group int32) error {
	m.mu.Lock()
	a := m.adopted[group]
	if a == nil {
		m.mu.Unlock()
		return fmt.Errorf("lease: shard group %d is not adopted", group)
	}
	st := m.groups[group]
	if st == nil || !st.have || st.lost || m.c.Clock.Mono() >= st.validUntil-m.c.Margin {
		m.mu.Unlock()
		return fmt.Errorf("lease: the lease of shard group %d is not held: the instance is not attached", group)
	}
	a.attached = true
	m.mu.Unlock()
	if err := os.Remove(MarkerPath(m.c.Dir, RelinquishedMarker, group)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lease: clearing the relinquished marker of shard group %d: %w", group, err)
	}
	m.writeWatchdogFile(group)
	m.ensureWatchdog(group)
	return nil
}

// Unadopt forgets the adoption of a group: the lease is not renewed any more and
// runs out at the grantors (use Transfer or the grantors' release to hand it over
// at once). An adoption that is attached guards a promoted primary and is never
// forgotten here (F3-01): only the catalog following it ends it
// (groupsToRenewLocked). It returns false when it refused for that reason.
func (m *Manager) Unadopt(group int32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.adopted[group]; a != nil && a.attached {
		return false
	}
	delete(m.adopted, group)
	delete(m.groups, group)
	return true
}

// Relinquish stops renewing the lease of a group but keeps judging it: Check still
// fences the primary when the lease it holds runs out (validUntil minus the
// margin), so a server that is handing a group over and then fails to stop its
// primary does not keep acting beyond what it held. It writes the marker that
// keeps the primary of the group from starting here until Unrelinquish.
func (m *Manager) Relinquish(group int32) error {
	if err := pki.WriteFile(MarkerPath(m.c.Dir, RelinquishedMarker, group), []byte("relinquished\n"), 0o600); err != nil {
		return fmt.Errorf("lease: writing the relinquished marker: %w", err)
	}
	m.mu.Lock()
	m.relinq[group] = true
	m.mu.Unlock()
	return nil
}

// HoldBack writes the relinquished marker (the start gate then refuses to start the primary of the group) and does NOT stop renewing the lease. The clean
// stop of a planned switchover uses it: a fast shutdown can take longer than the lease has left (a logical walsender of a bucket move that has to send
// what is pending, a shutdown checkpoint on a slow disk), and a lease that stopped being renewed at its start lapses in the middle of it and is fenced,
// which turns a slow clean stop into an unclean one. The renewal ends with Relinquish (a stop that did not end cleanly: the lease then lapses and the
// fence stops what still runs) or ReleaseGroup (the clean stop: the lease is handed back at once).
func (m *Manager) HoldBack(group int32) error {
	if err := pki.WriteFile(MarkerPath(m.c.Dir, RelinquishedMarker, group), []byte("holding back\n"), 0o600); err != nil {
		return fmt.Errorf("lease: writing the relinquished marker: %w", err)
	}
	return nil
}

// Unrelinquish removes the marker and renews the lease of the group again (a
// cancelled switchover; a server that became a standby of the group).
func (m *Manager) Unrelinquish(group int32) error {
	m.mu.Lock()
	delete(m.relinq, group)
	m.mu.Unlock()
	if err := os.Remove(MarkerPath(m.c.Dir, RelinquishedMarker, group)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Relinquished tells whether the group is relinquished.
func (m *Manager) Relinquished(group int32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.relinq[group]
}

// loadRelinquished reads the markers of the lease directory (kingresd restarted
// between a demote and the end of the switchover must not renew what it gave away).
func (m *Manager) loadRelinquished() {
	files, _ := filepath.Glob(filepath.Join(m.c.Dir, RelinquishedMarker+".*"))
	for _, f := range files {
		var g int32
		if _, err := fmt.Sscanf(strings.TrimPrefix(filepath.Base(f), RelinquishedMarker+"."), "%d", &g); err == nil {
			m.relinq[g] = true
		}
	}
}

// TransferResult is what a hand-over found.
type TransferResult struct {
	// Granted is the number of grantors that moved the lease, Need the majority.
	Granted, Need int
}

// Transfer hands the lease of the group to server `to` without a free moment: the
// grantors that have this server as the holder make `to` the holder for a fresh
// duration and nobody else can be granted it before that runs out (grantor.go,
// Transfer). It succeeds when a majority did. The caller must have made sure that
// no primary runs here under this lease (a clean stop, or the group has no
// instance here: the fence holder of a failover): the lease is `to`'s from the
// moment a majority answered, and this manager then stops renewing the group, drops
// an adoption and does not hold the lease any more.
func (m *Manager) Transfer(ctx context.Context, group int32, to string) (TransferResult, error) {
	if err := m.primaryRunning(ctx, group); err != nil {
		return TransferResult{}, err
	}
	t, ok := m.topologyNow()
	if !ok {
		return TransferResult{}, errors.New("lease: the members of the cluster are not known")
	}
	members := t.Members
	need := majority(members)
	req := Request{Group: group, DurationMs: m.c.Duration.Milliseconds(), TransferTo: to}
	rctx, cancel := context.WithTimeout(ctx, m.c.Duration-m.c.Margin)
	defer cancel()
	results := make(chan bool, len(members))
	for _, s := range members {
		m.fl.add(group)
		go func() {
			defer m.fl.done(group)
			if s == m.c.Self {
				r, err := m.c.Grantor.Transfer(group, m.c.Self, to, m.c.Duration)
				results <- err == nil && transferDone(r, to)
				return
			}
			r, err := m.c.Voter.Grant(rctx, s, req)
			results <- err == nil && transferDone(r, to)
		}()
	}
	granted := 0
	for i := 0; i < len(members); i++ {
		if <-results {
			granted++
		}
		if granted >= need {
			break
		}
	}
	res := TransferResult{Granted: granted, Need: need}
	if granted < need {
		return res, fmt.Errorf("lease: only %d of the %d grantors needed moved the lease of shard group %d to %s (a grantor of an older version does not know the call)", granted, need, group, to)
	}
	// The lease is `to`'s. This server does not hold it and does not renew it any more. The
	// marker makes that survive a restart of kingresd.
	if err := pki.WriteFile(MarkerPath(m.c.Dir, RelinquishedMarker, group), []byte("transferred to "+to+"\n"), 0o600); err != nil {
		m.c.Log.Warn("writing the relinquished marker after a transfer", "group", group, "error", err)
	}
	m.mu.Lock()
	m.relinq[group] = true
	delete(m.adopted, group)
	delete(m.groups, group)
	m.mu.Unlock()
	return res, nil
}

// ReleaseGroup asks every grantor to delete the grant of the group that names this
// server (the lease is free at once). The caller has confirmed that no primary runs
// under the lease and that the fence marker is on disk: it is how a switchover does
// not wait for the old lease to run out. It succeeds when a majority did.
func (m *Manager) ReleaseGroup(ctx context.Context, group int32) (TransferResult, error) {
	if err := m.primaryRunning(ctx, group); err != nil {
		return TransferResult{}, err
	}
	// The group is marked first and the round in flight is waited for: after that no renewal of this group is on
	// its way to a grantor (FO-13). A release that did not reach a majority puts the mark back.
	// The marker is on disk before the first release request (as Transfer does at its end), so that a restart of
	// kingresd in the middle of a release does not renew, or start a primary, for a group that was given away (F2-03).
	markerBefore := markerExists(m.c.Dir, RelinquishedMarker, group)
	if err := pki.WriteFile(MarkerPath(m.c.Dir, RelinquishedMarker, group), []byte("released\n"), 0o600); err != nil {
		return TransferResult{}, fmt.Errorf("lease: writing the relinquished marker: %w", err)
	}
	m.mu.Lock()
	was := m.relinq[group]
	m.relinq[group] = true
	m.mu.Unlock()
	m.rmu.Lock()
	m.rmu.Unlock() //nolint:staticcheck // waiting for the round in flight is the point
	// A round ends when a majority answered: the request to the last grantor may still be on its way, and must not be
	// received after the release (FO-13).
	m.fl.wait(group)
	t, ok := m.topologyNow()
	if !ok {
		m.mu.Lock()
		if !was {
			delete(m.relinq, group)
		}
		if !markerBefore {
			_ = os.Remove(MarkerPath(m.c.Dir, RelinquishedMarker, group))
		}
		m.mu.Unlock()
		return TransferResult{}, errors.New("lease: the members of the cluster are not known")
	}
	members := t.Members
	need := majority(members)
	req := Request{Group: group, DurationMs: m.c.Duration.Milliseconds(), Release: true}
	rctx, cancel := context.WithTimeout(ctx, m.c.Duration-m.c.Margin)
	defer cancel()
	results := make(chan bool, len(members))
	for _, s := range members {
		m.fl.add(group)
		go func() {
			defer m.fl.done(group)
			if s == m.c.Self {
				r, err := m.c.Grantor.Release(group, m.c.Self)
				results <- err == nil && releaseDone(r)
				return
			}
			r, err := m.c.Voter.Grant(rctx, s, req)
			results <- err == nil && releaseDone(r)
		}()
	}
	granted := 0
	for i := 0; i < len(members); i++ {
		if <-results {
			granted++
		}
		if granted >= need {
			break
		}
	}
	res := TransferResult{Granted: granted, Need: need}
	if granted < need {
		m.mu.Lock()
		if !was {
			delete(m.relinq, group)
		}
		if !markerBefore {
			_ = os.Remove(MarkerPath(m.c.Dir, RelinquishedMarker, group))
		}
		m.mu.Unlock()
		return res, fmt.Errorf("lease: only %d of the %d grantors needed released the lease of shard group %d", granted, need, group)
	}
	m.mu.Lock()
	m.relinq[group] = true
	delete(m.adopted, group)
	delete(m.groups, group)
	m.mu.Unlock()
	return res, nil
}

func (m *Manager) topologyNow() (Topology, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.topo, m.haveT
}

// groupsToRenew is the topology's groups plus the adopted ones, without the
// relinquished ones, sorted. A group the catalog names this server as the primary
// of ends its adoption (the catalog caught up).
func (m *Manager) groupsToRenewLocked(t Topology) []int32 {
	set := map[int32]bool{}
	for _, g := range t.Groups {
		set[g] = true
		delete(m.adopted, g)
	}
	for g := range m.adopted {
		set[g] = true
	}
	var out []int32
	for g := range set {
		if !m.relinq[g] {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// transferDone reads the answer of a grantor to a transfer: the lease is `to`'s there when the holder it names is
// `to`, whether this call moved it or an earlier one did (a repeat after a lost answer, FO-06). A grantor of the
// version before the transfer treats the request as a plain grant for the caller and names the caller: that is
// not a transfer, whatever it says in Granted.
func transferDone(r Reply, to string) bool { return r.Holder == to }

// releaseDone reads the answer of a grantor to a release: the lease is free there, so no holder is named. A
// grantor of the version before the release answers a plain grant for the caller and names it (FO-06).
func releaseDone(r Reply) bool { return r.Granted && r.Holder == "" }

// primaryRunning refuses a hand-over of the lease while an instance of the group runs on this server (FO-07): the
// primary would go on acting with a lease that is another server's now, until the watchdog stopped it. The caller
// stops it first and confirms the stop. A group with no instance here (the fence holder of a failover) has
// nothing that runs.
func (m *Manager) primaryRunning(ctx context.Context, group int32) error {
	var insts []Instance
	if inst, have := m.instance(group); have {
		insts = append(insts, inst)
	}
	// An instance that may serve the group but cannot be attributed to it is treated as the primary (F2-06).
	insts = append(insts, m.unattributed(group)...)
	for _, inst := range insts {
		up, err := inst.Running(ctx)
		if err != nil {
			return fmt.Errorf("lease: the instance of shard group %d cannot be asked whether it runs: %w", group, err)
		}
		if up {
			return fmt.Errorf("lease: the instance of shard group %d runs on this server: stop it before its lease is handed over", group)
		}
	}
	return nil
}

// HeldBack tells whether anything on disk keeps the primary of the group from starting on this server: the marker of
// a hand-over, of a fence, or of a wait for the lease. A directory that is the former primary of a group and is held
// back is the one the old-primary rebuild may turn into a standby (design 6.2; the proof is that and more).
func (m *Manager) HeldBack(group int32) bool {
	return markerExists(m.c.Dir, RelinquishedMarker, group) || markerExists(m.c.Dir, FencedMarker, group) || waitMarkerExists(m.c.Dir, group)
}

// BecameStandby clears everything that held the old primary of the group back, once its directory is a standby of the
// group (design 6.2 step 5): the markers of a hand-over, of a fence and of a wait, the memory of all three, and the
// watchdog file of the instance, which says "not a primary" from now on (leaseFile is that file; empty: none). The
// caller has written the role file (standby) first: from then on the start gate does not look at the markers, and
// removing them only stops them from saying something that is not true. It is the one place that removes the markers
// of an old primary, besides a won lease and the withdrawal of a hand-over.
func (m *Manager) BecameStandby(group int32, leaseFile string) error {
	m.mu.Lock()
	delete(m.relinq, group)
	delete(m.adopted, group)
	delete(m.groups, group)
	delete(m.fenceFail, group)
	m.mu.Unlock()
	if leaseFile != "" {
		m.writeWatchdogOff(leaseFile)
	}
	var first error
	for _, base := range []string{RelinquishedMarker, FencedMarker, WaitMarker} {
		if err := os.Remove(MarkerPath(m.c.Dir, base, group)); err != nil && !errors.Is(err, os.ErrNotExist) && first == nil {
			first = err
		}
	}
	return first
}

// Resume is the rollback of a planned switchover that did not reach the promotion (design 5.3): the marker of the hand-over is removed
// and, when the primary of the group was stopped for the hand-over, the wait marker is written, so that the lease rounds start it again as
// soon as this server holds the lease (README decision 7: a primary with an enforced lease is not started before it holds one). It is
// idempotent and does nothing to a primary that runs.
func (m *Manager) Resume(ctx context.Context, group int32) error {
	if err := m.Unrelinquish(group); err != nil {
		return err
	}
	inst, have := m.instance(group)
	if !have {
		return nil
	}
	up, err := inst.Running(ctx)
	if err != nil {
		return err
	}
	if up {
		return nil
	}
	return pki.WriteFile(MarkerPath(m.c.Dir, WaitMarker, group), []byte("waiting for the group lease\n"), 0o600)
}
