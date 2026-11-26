// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// ErrConflict means the directory of the standby's group on this server holds
// another standby.
var ErrConflict = errors.New("standby: this server already runs another standby of the group")

// Create makes the standby of e on this server, or finishes making it: the
// physical slot is created on the primary first (so the primary keeps the WAL the
// copy needs), then pg_basebackup copies the primary over the replication
// connection, then the instance is started in recovery. Every part checks what
// exists, so a task step that runs again after an interruption continues; a base
// backup that did not finish (no metadata file) is thrown away and made again.
func (a *Agent) Create(ctx context.Context, e Entry) (CreateResponse, error) {
	if err := validEntry(e); err != nil {
		return CreateResponse{}, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	return a.createLocked(ctx, e)
}

func (a *Agent) createLocked(ctx context.Context, e Entry) (CreateResponse, error) {
	if !a.Svc.Enabled() || !a.mgr().Exists(cluster.NodeID) {
		return CreateResponse{}, errors.New("standby: this server has no instance")
	}
	id := e.dir()
	if a.mgr().Exists(id) {
		if m, err := a.metaOf(id); err == nil {
			if m.ID != e.ID {
				return CreateResponse{}, ErrConflict
			}
			return a.startExisting(ctx, id, m)
		}
		// Not a complete standby: remove it (the slot stays, it is reused below) when it is an unfinished copy by the
		// positive mark of InitStandby (F2-08) or a standby proved by its role file and standby.signal (FO-04); anything else stays.
		if err := a.provedStandby(ctx, id, a.mgr().CopyUnfinished(id)); err != nil {
			return CreateResponse{}, err
		}
		if err := a.mgr().Remove(ctx, id); err != nil {
			return CreateResponse{}, fmt.Errorf("standby: removing an unfinished copy: %w", err)
		}
	}
	slot := SlotName(e.ID)
	if err := a.ensureSlot(ctx, e, slot); err != nil {
		return CreateResponse{}, err
	}
	conninfo, err := a.Svc.ReplicationConninfo(e.PrimaryServer, e.PrimaryPort, AppName(e.ID), "")
	if err != nil {
		return CreateResponse{}, fmt.Errorf("standby: building the replication connection: %w", err)
	}
	base, err := a.mgr().Settings(a.Svc.AccessSourceID())
	if err != nil {
		return CreateResponse{}, fmt.Errorf("standby: reading the settings of the primary instance: %w", err)
	}
	trust, err := a.mgr().Trust(a.Svc.AccessSourceID())
	if err != nil {
		return CreateResponse{}, fmt.Errorf("standby: reading the trust of the primary instance: %w", err)
	}
	s := base
	s.Port = 0 // chosen after the copy (StandbySpec.PickPort)
	s.Extra = maps.Clone(base.Extra)
	if s.Extra == nil {
		s.Extra = map[string]string{}
	}
	// The standby's own synchronous_standby_names is the primary's business; a copy of it
	// would only matter after a promotion, which sets its own.
	delete(s.Extra, "synchronous_standby_names")
	s.Extra["hot_standby"] = "on"
	s.Extra["primary_conninfo"] = conninfo
	s.Extra["primary_slot_name"] = slot
	if err := a.mgr().InitStandby(ctx, instance.StandbySpec{ID: id, Settings: s, Trust: trust, Conninfo: conninfo, Slot: slot, PickPort: a.freePort}); err != nil {
		return CreateResponse{}, fmt.Errorf("standby: creating the standby of group %d: %w", e.Group, err)
	}
	if _, err := a.syncAccess(ctx, id); err != nil {
		_ = a.mgr().Remove(ctx, id)
		return CreateResponse{}, fmt.Errorf("standby: writing the access rules of the standby: %w", err)
	}
	mk, merr := a.mgr().Managed(id)
	if merr != nil {
		_ = a.mgr().Remove(ctx, id)
		return CreateResponse{}, fmt.Errorf("standby: reading the port of the standby: %w", merr)
	}
	e.Port = mk.Port
	if err := a.writeMeta(id, e); err != nil {
		return CreateResponse{}, fmt.Errorf("standby: recording the standby: %w", err)
	}
	// The copy is complete: the mark InitStandby wrote first goes last (F2-08).
	if err := a.mgr().ClearCopyUnfinished(id); err != nil {
		return CreateResponse{}, fmt.Errorf("standby: clearing the mark of an unfinished copy: %w", err)
	}
	return a.startExisting(ctx, id, e)
}

// startExisting starts the standby instance and waits for it to accept connections.
func (a *Agent) startExisting(ctx context.Context, id int, e Entry) (CreateResponse, error) {
	if _, err := a.mgr().Managed(id); err != nil {
		return CreateResponse{}, err
	}
	if err := a.startStandbyInstance(ctx, id, e); err != nil {
		return CreateResponse{}, fmt.Errorf("standby: starting the standby instance: %w", err)
	}
	mk, err := a.mgr().Managed(id) // after the start: it may have chosen another port
	if err != nil {
		return CreateResponse{}, err
	}
	a.audit(ctx, "standbyInstanceStarted", strconv.FormatInt(e.ID, 10), audit.OutcomeSuccess, map[string]any{"group": e.Group, "server": a.Self, "phase": "created"})
	return CreateResponse{Port: mk.Port}, nil
}

// ensureSlot creates the physical slot of the standby on its primary unless it
// exists; it reserves WAL at once so that the base backup can never miss any.
func (a *Agent) ensureSlot(ctx context.Context, e Entry, slot string) error {
	c, err := a.Svc.RemoteConn(ctx, e.PrimaryServer, e.PrimaryPort, "postgres")
	if err != nil {
		return fmt.Errorf("standby: connecting to the primary of group %d: %w", e.Group, errors.New("the primary cannot be reached"))
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	_, err = c.Exec(ctx, `SELECT pg_create_physical_replication_slot($1, true)
		WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = $1)`, slot)
	if err != nil {
		return errors.New("standby: creating the replication slot on the primary failed")
	}
	return nil
}

// dropSlot drops the slot of a standby on its primary, ending the walsender that
// still uses it; a slot that does not exist is fine.
func (a *Agent) dropSlot(ctx context.Context, c *pgx.Conn, slot string) error {
	_, err := c.Exec(ctx, `SELECT pg_terminate_backend(active_pid) FROM pg_replication_slots WHERE slot_name = $1 AND active_pid IS NOT NULL`, slot)
	if err != nil {
		return err
	}
	for i := 0; i < 20; i++ {
		_, err = c.Exec(ctx, `SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = $1`, slot)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return err
}

// Remove stops and deletes the standby instance of e and drops its slot on the
// primary (best effort: the primary's own supervision also drops slots of
// standbys that are not in the plan any more). A standby that does not exist is
// removed already.
func (a *Agent) Remove(ctx context.Context, e Entry) error {
	if err := validEntry(e); err != nil {
		return err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	return a.removeLocked(ctx, e)
}

func (a *Agent) removeLocked(ctx context.Context, e Entry) error {
	id := e.dir()
	if a.mgr().Exists(id) {
		m, merr := a.metaOf(id)
		if merr == nil && m.ID != e.ID {
			return ErrConflict // the directory holds another standby: not this one's to remove
		}
		// Only an instance proved to be a standby is deleted (FO-04): a promoted standby whose role file is not
		// written yet, or is broken, is a primary with its data.
		if err := a.provedStandby(ctx, id, a.mgr().CopyUnfinished(id)); err != nil {
			return err
		}
		if err := a.mgr().Remove(ctx, id); err != nil {
			return fmt.Errorf("standby: removing the instance: %w", err)
		}
	}
	if c, err := a.Svc.RemoteConn(ctx, e.PrimaryServer, e.PrimaryPort, "postgres"); err == nil {
		if err := a.dropSlot(ctx, c, SlotName(e.ID)); err != nil {
			a.Log.Warn("standby: dropping the slot failed; the primary drops it later", "standby", e.ID, "error", err)
		}
		_ = c.Close(context.WithoutCancel(ctx))
	}
	a.audit(ctx, "standbyInstanceRemoved", strconv.FormatInt(e.ID, 10), audit.OutcomeSuccess, map[string]any{"group": e.Group, "server": a.Self})
	return nil
}

// Rebuild makes the standby again. A standby whose history may have left the
// primary's (diverged) is first tried with pg_rewind, which copies only what
// differs; when that is not possible, and for a standby that fell behind its slot,
// the instance is deleted, the slot made again and a new base backup taken.
func (a *Agent) Rebuild(ctx context.Context, e Entry, diverged bool) (CreateResponse, error) {
	return a.rebuild(ctx, e, diverged, 0)
}

// rebuild is Rebuild with the catalog term the leader asks under: the old primary of a group is rebuilt only when the
// term is newer than the one its role file was written under (oldprimary.go).
func (a *Agent) rebuild(ctx context.Context, e Entry, diverged bool, term int64) (CreateResponse, error) {
	if err := validEntry(e); err != nil {
		return CreateResponse{}, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	id := a.dirFor(e, diverged)
	if a.mgr().Exists(id) && e.Generation > 0 {
		// ST-03: the call is repeated when its answer was lost. A standby that is of the generation asked for was
		// rebuilt by the first execution: a second one is a no-op (it only makes sure the instance runs).
		if m, err := a.metaOf(id); err == nil && m.ID == e.ID && m.Generation >= e.Generation {
			return a.startExisting(ctx, id, m)
		}
	}
	if a.mgr().Exists(id) {
		// The old primary of the group is a primary on disk: it is rebuilt only when it is proved to be one (a role file
		// that says primary of this group, stopped, not held by a lease of this server, marked, a newer term).
		former, err := a.enterFormer(ctx, id, e, term)
		if err != nil {
			return CreateResponse{}, err
		}
		if former {
			return a.rebuildFormerLocked(ctx, id, e, term)
		}
		if err := a.provedStandby(ctx, id, a.mgr().CopyUnfinished(id)); err != nil {
			return CreateResponse{}, err
		}
	}
	if diverged && a.mgr().Exists(id) {
		// A standby that is ahead of its new primary or on another timeline: the same steps as the old primary, with
		// the proof of a standby (the role file and the recovery or standby.signal).
		return a.rebuildFormerLocked(ctx, id, e, term)
	}
	if err := a.removeLocked(ctx, e); err != nil {
		return CreateResponse{}, err
	}
	res, err := a.createLocked(ctx, e)
	if err != nil {
		return res, err
	}
	a.audit(ctx, "standbyInstanceRebuilt", strconv.FormatInt(e.ID, 10), audit.OutcomeSuccess, map[string]any{"group": e.Group, "server": a.Self, "method": "base backup"})
	return res, nil
}

// superviseStandbys keeps the standby instances of this server running and in
// step with the instance of the primary: pg_hba.conf and pg_ident.conf are copied
// from it when they differ (network rules and the certificate map change under
// the instance), and a changed certificate, CA or revocation list makes a running
// standby reload (it connects to its primary with the certificate files). A
// create, remove or rebuild in progress is left alone.
func (a *Agent) superviseStandbys(ctx context.Context) {
	if !a.opMu.TryLock() {
		return
	}
	defer a.opMu.Unlock()
	reload := a.certificatesChanged()
	plan := a.local.snapshot()
	in := map[int64]bool{}
	for _, p := range plan.Standbys {
		in[p.ID] = true
	}
	for _, lsb := range a.localStandbys() {
		e, id := lsb.Entry, lsb.Dir
		// A standby the list does not name was removed while this server could not be
		// reached (or by an administrator's task that this server missed): it goes.
		if plan.Version > 0 && !in[e.ID] {
			if err := a.removeLocked(ctx, e); err != nil {
				a.Log.Warn("standby: removing a standby that is not in the list", "standby", e.ID, "error", err)
			}
			continue
		}
		// Running or not, the port in the meta file is the port of the instance.
		if ne, merr := a.reconcileMetaPort(id, e); merr != nil {
			a.Log.Warn("standby: recording the port of a standby", "group", e.Group, "error", merr)
		} else {
			e = ne
		}
		up, err := a.mgr().Running(ctx, id)
		if err != nil {
			continue
		}
		if !up {
			if err := a.startStandbyInstance(ctx, id, e); err != nil {
				a.Log.Warn("standby: starting a standby instance", "group", e.Group, "error", err)
			}
			continue
		}
		changed, err := a.syncAccess(ctx, id)
		if err != nil {
			a.Log.Warn("standby: copying the access rules", "group", e.Group, "error", err)
		}
		if changed {
			continue // the standby was reloaded
		}
		if reload {
			if err := a.mgr().Reload(ctx, id); err != nil {
				a.Log.Warn("standby: reloading a standby instance", "group", e.Group, "error", err)
			}
		}
	}
}

// syncAccess brings the network rules of a standby in line with this server's
// primary instance (instance.Manager.SyncAccess).
func (a *Agent) syncAccess(ctx context.Context, id int) (bool, error) {
	u, err := user.Current()
	if err != nil {
		return false, err
	}
	return a.mgr().SyncAccess(ctx, a.Svc.AccessSourceID(), id, u.Username)
}

// certificatesChanged reports whether the certificate, CA or CRL file of this
// server changed since the last look (the first look only records them).
func (a *Agent) certificatesChanged() bool {
	changed := false
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range []string{pki.ServerCertFile, pki.CACertFile, pki.CRLFile} {
		st, err := os.Stat(a.Svc.PKIFile(f))
		if err != nil {
			continue
		}
		cur := fileStamp{st.ModTime(), st.Size()}
		if old, ok := a.access[f]; ok && old != cur {
			changed = true
		}
		a.access[f] = cur
	}
	return changed
}

// Status is the answer of GET /standby/status.
func (a *Agent) Status(ctx context.Context) Status {
	st := Status{Server: a.Self, PlanVersion: a.local.snapshot().Version, Standbys: []LocalStatus{}}
	if !a.Svc.Enabled() {
		return st
	}
	for _, lsb := range a.localStandbys() {
		e, id := lsb.Entry, lsb.Dir
		ls := LocalStatus{ID: e.ID, Group: e.Group, Port: e.Port, Generation: e.Generation}
		ls.Running, _ = a.mgr().Running(ctx, id)
		if ls.Running {
			a.readReceiver(ctx, id, &ls)
		}
		st.Standbys = append(st.Standbys, ls)
	}
	a.mu.Lock()
	st.Rounds = a.rounds
	for _, g := range a.primaryGroups() {
		st.Primaries = append(st.Primaries, *a.primaries[g])
	}
	if len(st.Primaries) > 0 {
		p := st.Primaries[0]
		st.Primary = &p
	}
	a.mu.Unlock()
	return st
}

// readReceiver reads the walreceiver state of a running standby.
func (a *Agent) readReceiver(ctx context.Context, id int, ls *LocalStatus) {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c, err := a.Svc.ServiceConnAt(cctx, id, ls.Port, "postgres")
	if err != nil {
		return
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var status string
	var lag *int64
	if err := c.QueryRow(cctx, `SELECT coalesce((SELECT status FROM pg_stat_wal_receiver LIMIT 1), 'stopped'),
		pg_wal_lsn_diff(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn())::bigint`).Scan(&status, &lag); err != nil {
		return
	}
	ls.Receiving, ls.ReplayLagBytes = status, lag
}

// ErrNotProvedStandby means that an instance directory is not proved to be a standby, so it is neither deleted nor
// rebuilt (FO-04): a promoted standby whose role file is not written yet, or is broken, would be deleted with its data.
var ErrNotProvedStandby = errors.New("standby: the instance is not proved to be a standby")

// provedStandby returns nil only when the instance of directory id is proved to be a standby by more than the
// default of its id: the role file exists, is valid and says standby, AND the instance is in recovery when it runs
// (pg_is_in_recovery), or carries standby.signal when it does not. An absent or broken file is never a licence.
// unfinished is for the directory of a copy that was never completed (it has no standby meta file): a missing role
// file and a missing standby.signal are then expected, but a role file that says primary or is broken, and a running
// instance that is not in recovery, still refuse.
func (a *Agent) provedStandby(ctx context.Context, id int, unfinished bool) error {
	r, exists, err := a.mgr().RoleOf(id)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotProvedStandby, err)
	}
	if unfinished && !exists {
		exists, r = true, instance.Role{Role: instance.RoleStandby} // a half-made copy: no role file yet
	}
	if !exists || !r.IsStandby() {
		return fmt.Errorf("%w: its role file does not say standby", ErrNotProvedStandby)
	}
	up, err := a.mgr().Running(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotProvedStandby, err)
	}
	if !up && unfinished {
		return nil // a half-made copy that does not run (no standby.signal yet) is thrown away
	}
	if !up {
		data, err := a.mgr().DataDir(id)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrNotProvedStandby, err)
		}
		if _, err := os.Stat(filepath.Join(data, "standby.signal")); err != nil {
			return fmt.Errorf("%w: it is stopped and has no standby.signal", ErrNotProvedStandby)
		}
		return nil
	}
	inRecovery, err := a.askRecovery(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: it runs and cannot be asked: %w", ErrNotProvedStandby, err)
	}
	if !inRecovery {
		return fmt.Errorf("%w: it is not in recovery", ErrNotProvedStandby)
	}
	return nil
}

// askRecovery asks the running instance of directory id whether it is in recovery (a test sets Agent.AskRecovery).
func (a *Agent) askRecovery(ctx context.Context, id int) (bool, error) {
	if a.AskRecovery != nil {
		return a.AskRecovery(ctx, id)
	}
	mk, err := a.mgr().Managed(id)
	if err != nil {
		return false, err
	}
	c, err := a.Svc.ServiceConnAt(ctx, id, mk.Port, "postgres")
	if err != nil {
		return false, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var inRecovery bool
	if err := c.QueryRow(ctx, "SELECT pg_catalog.pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return false, err
	}
	return inRecovery, nil
}

// LockLifecycle takes the lock of the agent's create, remove and rebuild for the caller: the promotion of a standby runs
// under it, so that the proof that a directory is a standby and its removal (or the promotion that makes it a primary)
// are never interleaved (F2-08). It waits until ctx ends.
func (a *Agent) LockLifecycle(ctx context.Context) (unlock func(), err error) {
	for !a.opMu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return a.opMu.Unlock, nil
}

// startStandbyInstance starts the standby instance of directory id. A standby that has never run and whose port could
// not be bound (another program took it) is given another port once by the instance manager; the standby's meta file
// is written with the port the instance has afterwards, whether or not the start worked, before anything reports it.
func (a *Agent) startStandbyInstance(ctx context.Context, id int, e Entry) error {
	err := a.mgr().StartStandby(ctx, id, a.freePort)
	// The marker has the port the instance has: the meta file follows it, whether or not the start worked.
	if _, werr := a.reconcileMetaPort(id, e); werr != nil && err == nil {
		err = werr
	}
	return err
}

// reconcileMetaPort makes the port in the meta file of a standby the port of its marker (instance.Manager.Managed), which
// is the authority: a re-pick of the port, a crash in the middle of it, or an older version may have left the two apart.
// It returns the entry with the port the instance has.
func (a *Agent) reconcileMetaPort(id int, e Entry) (Entry, error) {
	return syncMetaPort(func() (int, bool) {
		mk, err := a.mgr().Managed(id)
		return mk.Port, err == nil
	}, id, e, a.writeMeta)
}

// syncMetaPort is reconcileMetaPort with the marker's port (and whether there is a marker) given by portOf.
func syncMetaPort(portOf func() (int, bool), id int, e Entry, write func(int, Entry) error) (Entry, error) {
	port, ok := portOf()
	if !ok || port == 0 || port == e.Port {
		return e, nil
	}
	e.Port = port
	if err := write(id, e); err != nil {
		return e, fmt.Errorf("standby: recording the port %d of a standby: %w", port, err)
	}
	return e, nil
}
