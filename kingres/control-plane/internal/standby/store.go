// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// Record states.
const (
	stateCreating   = "creating"
	stateReady      = "ready"
	stateRebuilding = "rebuilding"
	stateRemoving   = "removing"
)

// Record is a row of kingres_cp.standby: the leader's authoritative record of one
// standby.
type Record struct {
	ID            int64
	Group         int32
	Server        string
	PrimaryServer string
	InstanceID    int
	Port          int
	State         string
	NeedsRebuild  bool
	Generation    int64
	CatalogNode   *int32
	// Diverged says that the instance may hold writes the primary does not have (an old primary after a failover, a
	// standby that is ahead of its new primary): it is rebuilt with pg_rewind, else a base backup. WaitingSince is when
	// the leader first found its server silent (nil: it answers).
	Diverged     bool
	WaitingSince *time.Time
	CreatedAt    time.Time
}

// Entry converts the record for the plan: Ready only while the standby is in the
// ready state (a standby that is being created or rebuilt must not be named in
// synchronous_standby_names).
func (r Record) Entry(primaryPort int) Entry {
	return Entry{ID: r.ID, Group: r.Group, Server: r.Server, PrimaryServer: r.PrimaryServer, PrimaryPort: primaryPort,
		Port: r.Port, Ready: r.State == stateReady, Generation: r.Generation, CatalogNode: r.catalogNode(), InstanceID: r.InstanceID}
}

// SysFunc runs f with a service connection to the system database (the leader's).
type SysFunc func(ctx context.Context, f func(c *pgx.Conn) error) error

const recordCols = `standby_id, group_id, server_id, primary_server, instance_id, coalesce(port, 0), state, needs_rebuild, generation, catalog_node_id, diverged, waiting_since, created_at`

func scanRecord(r pgx.Row) (Record, error) {
	var x Record
	err := r.Scan(&x.ID, &x.Group, &x.Server, &x.PrimaryServer, &x.InstanceID, &x.Port, &x.State, &x.NeedsRebuild, &x.Generation, &x.CatalogNode, &x.Diverged, &x.WaitingSince, &x.CreatedAt)
	return x, err
}

type store struct{ sys SysFunc }

func (s store) list(ctx context.Context) ([]Record, error) {
	var out []Record
	err := s.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT `+recordCols+` FROM kingres_cp.standby ORDER BY standby_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRecord(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("standby: reading kingres_cp.standby: %w", err)
	}
	return out, nil
}

// ensure returns the record of (group, server), creating it in the state
// creating when it does not exist. A record in the state removing is not reused:
// the removal has to finish first.
func (s store) ensure(ctx context.Context, group int32, server, primary string) (Record, error) {
	var r Record
	err := s.sys(ctx, func(c *pgx.Conn) error {
		if _, err := c.Exec(ctx, `INSERT INTO kingres_cp.standby (group_id, server_id, primary_server, instance_id, slot_name)
			VALUES ($1, $2, $3, $4, 'pending') ON CONFLICT (group_id, server_id) DO NOTHING`, group, server, primary, instance.StandbyDirID(group)); err != nil {
			return err
		}
		var err error
		r, err = scanRecord(c.QueryRow(ctx, `UPDATE kingres_cp.standby SET slot_name = $4 || standby_id::text, primary_server = $3, updated_at = now()
			WHERE group_id = $1 AND server_id = $2 AND state <> 'removing' RETURNING `+recordCols, group, server, primary, slotPrefix))
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("the standby is being removed")
		}
		return err
	})
	return r, wrapErr("ensuring the record of a standby", err)
}

func (s store) get(ctx context.Context, id int64) (Record, bool, error) {
	var r Record
	found := true
	err := s.sys(ctx, func(c *pgx.Conn) error {
		var err error
		r, err = scanRecord(c.QueryRow(ctx, `SELECT `+recordCols+` FROM kingres_cp.standby WHERE standby_id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return nil
		}
		return err
	})
	return r, found, wrapErr("reading a standby record", err)
}

// setState changes the state (and the port when it is not 0); ready clears
// needs_rebuild.
func (s store) setState(ctx context.Context, id int64, state string, port int) error {
	return wrapErr("updating a standby record", s.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.standby SET state = $2, port = CASE WHEN $3 > 0 THEN $3 ELSE port END,
			needs_rebuild = CASE WHEN $2 = 'ready' THEN false ELSE needs_rebuild END,
			diverged = CASE WHEN $2 = 'ready' THEN false ELSE diverged END,
			waiting_since = CASE WHEN $2 = 'ready' THEN NULL ELSE waiting_since END,
			updated_at = now() WHERE standby_id = $1 AND state <> 'removing'`, id, state, port)
		return err
	}))
}

// bumpGeneration starts a rebuild: the generation grows and the standby leaves the
// ready state.
func (s store) bumpGeneration(ctx context.Context, id int64) (Record, error) {
	var r Record
	err := s.sys(ctx, func(c *pgx.Conn) error {
		var err error
		r, err = scanRecord(c.QueryRow(ctx, `UPDATE kingres_cp.standby SET generation = generation + 1, state = 'rebuilding', needs_rebuild = false, updated_at = now()
			WHERE standby_id = $1 RETURNING `+recordCols, id))
		return err
	})
	return r, wrapErr("starting a rebuild", err)
}

func (s store) markRemoving(ctx context.Context, id int64) error {
	return wrapErr("marking a standby for removal", s.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.standby SET state = 'removing', updated_at = now() WHERE standby_id = $1`, id)
		return err
	}))
}

func (s store) delete(ctx context.Context, id int64) error {
	return wrapErr("deleting a standby record", s.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `DELETE FROM kingres_cp.standby WHERE standby_id = $1`, id)
		return err
	}))
}

func (s store) flagRebuild(ctx context.Context, id int64) error {
	return wrapErr("flagging a standby for a rebuild", s.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.standby SET needs_rebuild = true WHERE standby_id = $1`, id)
		return err
	}))
}

// version reads the current plan version; bump takes the next one.
func (s store) version(ctx context.Context) (int64, error) {
	var v int64
	err := s.sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM kingres_cp.standby_plan_version`).Scan(&v)
	})
	return v, wrapErr("reading the plan version", err)
}

func (s store) bump(ctx context.Context) (int64, error) {
	var v int64
	err := s.sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT nextval('kingres_cp.standby_plan_version')`).Scan(&v)
	})
	return v, wrapErr("taking a plan version", err)
}

func wrapErr(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("standby: %s: %w", what, err)
}

func (r Record) catalogNode() int32 {
	if r.CatalogNode == nil {
		return 0
	}
	return *r.CatalogNode
}

// setCatalogNode stores the node id the catalog gave the standby (0 clears it).
func (s store) setCatalogNode(ctx context.Context, id int64, node int32) error {
	return wrapErr("storing the catalog node of a standby", s.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.standby SET catalog_node_id = nullif($2, 0), updated_at = now() WHERE standby_id = $1`, id, node)
		return err
	}))
}

// setWaiting marks, or unmarks, that the server of a standby that is not ready does not answer the leader: the time
// is kept at the first check that finds it silent (the standby-replace-after rule counts from it) and cleared when
// the server answers.
func (s store) setWaiting(ctx context.Context, id int64, waiting bool) error {
	return wrapErr("recording that a server does not answer", s.sys(ctx, func(c *pgx.Conn) error {
		if waiting {
			_, err := c.Exec(ctx, `UPDATE kingres_cp.standby SET waiting_since = coalesce(waiting_since, now()) WHERE standby_id = $1 AND waiting_since IS NULL`, id)
			return err
		}
		_, err := c.Exec(ctx, `UPDATE kingres_cp.standby SET waiting_since = NULL WHERE standby_id = $1 AND waiting_since IS NOT NULL`, id)
		return err
	}))
}

// setInstance records the directory a standby turned out to be in (the old primary's: the failover did not know it).
func (s store) setInstance(ctx context.Context, id int64, instanceID int) error {
	return wrapErr("storing the instance of a standby", s.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.standby SET instance_id = $2, updated_at = now() WHERE standby_id = $1 AND instance_id <> $2`, id, instanceID)
		return err
	}))
}

// recordOldPrimary stores what the rebuild of the old primary found and did in the newest failover row of the group
// that names the server as its old server (the report reads it). A row of a run that is gone is not an error.
func (s store) recordOldPrimary(ctx context.Context, group int32, server string, report []byte) error {
	return wrapErr("storing the report of an old primary", s.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.failover_group SET old_primary_report = $3::jsonb, old_primary = 'standby'
			WHERE (task_id, group_id) = (SELECT task_id, group_id FROM kingres_cp.failover_group
				WHERE group_id = $1 AND old_server = $2 ORDER BY task_id DESC LIMIT 1)`, group, server, string(report))
		return err
	}))
}
