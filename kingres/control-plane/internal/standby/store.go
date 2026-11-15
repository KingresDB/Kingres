// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"errors"
	"fmt"

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
}

// Entry converts the record for the plan: Ready only while the standby is in the
// ready state (a standby that is being created or rebuilt must not be named in
// synchronous_standby_names).
func (r Record) Entry(primaryPort int) Entry {
	return Entry{ID: r.ID, Group: r.Group, Server: r.Server, PrimaryServer: r.PrimaryServer, PrimaryPort: primaryPort,
		Port: r.Port, Ready: r.State == stateReady, Generation: r.Generation}
}

// SysFunc runs f with a service connection to the system database (the leader's).
type SysFunc func(ctx context.Context, f func(c *pgx.Conn) error) error

const recordCols = `standby_id, group_id, server_id, primary_server, instance_id, coalesce(port, 0), state, needs_rebuild, generation, catalog_node_id`

func scanRecord(r pgx.Row) (Record, error) {
	var x Record
	err := r.Scan(&x.ID, &x.Group, &x.Server, &x.PrimaryServer, &x.InstanceID, &x.Port, &x.State, &x.NeedsRebuild, &x.Generation, &x.CatalogNode)
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
			needs_rebuild = CASE WHEN $2 = 'ready' THEN false ELSE needs_rebuild END, updated_at = now() WHERE standby_id = $1 AND state <> 'removing'`, id, state, port)
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
