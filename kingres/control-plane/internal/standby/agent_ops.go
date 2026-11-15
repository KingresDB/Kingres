// SPDX-License-Identifier: PostgreSQL

package standby

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/user"
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
	id := instance.StandbyDirID(e.Group)
	if a.mgr().Exists(id) {
		if m, err := a.metaOf(id); err == nil {
			if m.ID != e.ID {
				return CreateResponse{}, ErrConflict
			}
			return a.startExisting(ctx, id, m)
		}
		// An unfinished copy: remove it (the slot stays, it is reused below).
		if err := a.mgr().Remove(ctx, id); err != nil {
			return CreateResponse{}, fmt.Errorf("standby: removing an unfinished copy: %w", err)
		}
	}
	slot := SlotName(e.ID)
	if err := a.ensureSlot(ctx, e, slot); err != nil {
		return CreateResponse{}, err
	}
	port, err := a.freePort()
	if err != nil {
		return CreateResponse{}, err
	}
	conninfo, err := a.Svc.ReplicationConninfo(e.PrimaryServer, e.PrimaryPort, AppName(e.ID), "")
	if err != nil {
		return CreateResponse{}, fmt.Errorf("standby: building the replication connection: %w", err)
	}
	base, err := a.mgr().Settings(cluster.NodeID)
	if err != nil {
		return CreateResponse{}, fmt.Errorf("standby: reading the settings of the primary instance: %w", err)
	}
	trust, err := a.mgr().Trust(cluster.NodeID)
	if err != nil {
		return CreateResponse{}, fmt.Errorf("standby: reading the trust of the primary instance: %w", err)
	}
	s := base
	s.Port = port
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
	if err := a.mgr().InitStandby(ctx, instance.StandbySpec{ID: id, Settings: s, Trust: trust, Conninfo: conninfo, Slot: slot}); err != nil {
		return CreateResponse{}, fmt.Errorf("standby: creating the standby of group %d: %w", e.Group, err)
	}
	if _, err := a.syncAccess(ctx, id); err != nil {
		_ = a.mgr().Remove(ctx, id)
		return CreateResponse{}, fmt.Errorf("standby: writing the access rules of the standby: %w", err)
	}
	e.Port = port
	if err := a.writeMeta(id, e); err != nil {
		return CreateResponse{}, fmt.Errorf("standby: recording the standby: %w", err)
	}
	return a.startExisting(ctx, id, e)
}

// startExisting starts the standby instance and waits for it to accept connections.
func (a *Agent) startExisting(ctx context.Context, id int, e Entry) (CreateResponse, error) {
	mk, err := a.mgr().Managed(id)
	if err != nil {
		return CreateResponse{}, err
	}
	if err := a.mgr().Start(ctx, id); err != nil {
		return CreateResponse{}, fmt.Errorf("standby: starting the standby instance: %w", err)
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
	id := instance.StandbyDirID(e.Group)
	if a.mgr().Exists(id) {
		if m, err := a.metaOf(id); err == nil && m.ID != e.ID {
			return ErrConflict // the directory holds another standby: not this one's to remove
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
	if err := validEntry(e); err != nil {
		return CreateResponse{}, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	id := instance.StandbyDirID(e.Group)
	if diverged && a.mgr().Exists(id) {
		if err := a.rewindLocked(ctx, id, e); err == nil {
			a.audit(ctx, "standbyInstanceRebuilt", strconv.FormatInt(e.ID, 10), audit.OutcomeSuccess, map[string]any{"group": e.Group, "server": a.Self, "method": "pg_rewind"})
			return a.startExisting(ctx, id, e)
		} else {
			a.Log.Info("standby: pg_rewind was not possible, taking a new base backup", "standby", e.ID, "error", err)
		}
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

func (a *Agent) rewindLocked(ctx context.Context, id int, e Entry) error {
	if err := a.mgr().Stop(ctx, id); err != nil {
		return err
	}
	conninfo, err := a.Svc.ReplicationConninfo(e.PrimaryServer, e.PrimaryPort, AppName(e.ID), "postgres")
	if err != nil {
		return err
	}
	return a.mgr().Rewind(ctx, id, conninfo)
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
	for _, e := range a.localStandbys() {
		// A standby the list does not name was removed while this server could not be
		// reached (or by an administrator's task that this server missed): it goes.
		if plan.Version > 0 && !in[e.ID] {
			if err := a.removeLocked(ctx, e); err != nil {
				a.Log.Warn("standby: removing a standby that is not in the list", "standby", e.ID, "error", err)
			}
			continue
		}
		id := instance.StandbyDirID(e.Group)
		up, err := a.mgr().Running(ctx, id)
		if err != nil {
			continue
		}
		if !up {
			if err := a.mgr().Start(ctx, id); err != nil {
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
	return a.mgr().SyncAccess(ctx, cluster.NodeID, id, u.Username)
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
	for _, e := range a.localStandbys() {
		id := instance.StandbyDirID(e.Group)
		ls := LocalStatus{ID: e.ID, Group: e.Group, Port: e.Port, Generation: e.Generation}
		ls.Running, _ = a.mgr().Running(ctx, id)
		if ls.Running {
			a.readReceiver(ctx, id, &ls)
		}
		st.Standbys = append(st.Standbys, ls)
	}
	a.mu.Lock()
	if a.primary != nil {
		p := *a.primary
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
