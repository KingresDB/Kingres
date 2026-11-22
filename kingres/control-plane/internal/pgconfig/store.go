// SPDX-License-Identifier: AGPL-3.0-only

package pgconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// SysFunc runs f with a service connection to the system database (the leader's).
type SysFunc func(ctx context.Context, f func(c *pgx.Conn) error) error

// valueRow is a row of kingres_cp.config_value.
type valueRow struct {
	Level, Scope, Name, Value, By string
	Revision                      int64
	At                            time.Time
}

// srv is an active server with its server group ("" when none).
type srv struct{ ID, Group string }

type store struct{ sys SysFunc }

func (s store) values(ctx context.Context) ([]valueRow, error) {
	var out []valueRow
	err := s.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT level, scope_id, name, value, revision, updated_by, updated_at FROM kingres_cp.config_value ORDER BY level, scope_id, name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v valueRow
			if err := rows.Scan(&v.Level, &v.Scope, &v.Name, &v.Value, &v.Revision, &v.By, &v.At); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pgconfig: reading kingres_cp.config_value: %w", err)
	}
	return out, nil
}

// revision is the number of the last accepted change (0 before the first).
func (s store) revision(ctx context.Context) (int64, error) {
	var rev int64
	err := s.sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM kingres_cp.config_revision`).Scan(&rev)
	})
	return rev, err
}

func (s store) servers(ctx context.Context) ([]srv, error) {
	var out []srv
	err := s.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT server_id, coalesce(server_group, '') FROM kingres_cp.server WHERE status = 'active' ORDER BY server_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x srv
			if err := rows.Scan(&x.ID, &x.Group); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, err
}

func (s store) groups(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	err := s.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT name FROM kingres_cp.server_group`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			out[n] = true
		}
		return rows.Err()
	})
	return out, err
}

// standbyPair is a standby instance and the server that runs its primary.
type standbyPair struct {
	Group         int32
	Server        string
	InstanceID    int
	PrimaryServer string
}

func (s store) standbys(ctx context.Context) ([]standbyPair, error) {
	var out []standbyPair
	err := s.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT group_id, server_id, instance_id, primary_server FROM kingres_cp.standby WHERE state = 'ready' ORDER BY group_id, server_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p standbyPair
			if err := rows.Scan(&p.Group, &p.Server, &p.InstanceID, &p.PrimaryServer); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// groupPrimaries returns, for each shard group, the server that runs its primary now (what the standby check of the
// leader last saw). After a failover that is the server of the promoted standby, not the one that ran the primary
// when the standby was created (P13R-06).
func (s store) groupPrimaries(ctx context.Context) (map[int32]string, error) {
	out := map[int32]string{}
	err := s.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT group_id, coalesce(primary_server, '') FROM kingres_cp.group_replication_state`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g int32
			var srv string
			if err := rows.Scan(&g, &srv); err != nil {
				return err
			}
			if srv != "" {
				out[g] = srv
			}
		}
		return rows.Err()
	})
	return out, err
}

func (s store) setting(ctx context.Context, key string, def int) int {
	var v []byte
	err := s.sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT value FROM kingres_cp.cluster_setting WHERE key = $1`, key).Scan(&v)
	})
	if err != nil {
		return def
	}
	var n int
	if json.Unmarshal(v, &n) != nil {
		return def
	}
	return n
}

// serverState is a row of kingres_cp.config_server_state.
type serverState struct {
	Server   string
	Revision int64
	State    string
	// Errors are the failed parameters with PostgreSQL's text.
	Errors map[string]string
	Detail string
}

type stateDetail struct {
	Errors map[string]string `json:"errors,omitempty"`
	Detail string            `json:"detail,omitempty"`
}

func (s store) states(ctx context.Context) (map[string]serverState, error) {
	out := map[string]serverState{}
	err := s.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT server_id, applied_revision, state, detail FROM kingres_cp.config_server_state`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st serverState
			var d []byte
			if err := rows.Scan(&st.Server, &st.Revision, &st.State, &d); err != nil {
				return err
			}
			var sd stateDetail
			_ = json.Unmarshal(d, &sd)
			st.Errors, st.Detail = sd.Errors, sd.Detail
			out[st.Server] = st
		}
		return rows.Err()
	})
	return out, err
}

func (s store) saveState(ctx context.Context, st serverState) error {
	d, _ := json.Marshal(stateDetail{Errors: st.Errors, Detail: st.Detail})
	return s.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `INSERT INTO kingres_cp.config_server_state (server_id, applied_revision, state, detail) VALUES ($1, $2, $3, $4)
			ON CONFLICT (server_id) DO UPDATE SET applied_revision = EXCLUDED.applied_revision, state = EXCLUDED.state, detail = EXCLUDED.detail, updated_at = now()`,
			st.Server, st.Revision, st.State, d)
		return err
	})
}

// removedDatabaseNames are the parameters that were set for a database once and have no value now:
// every apply resets them (RESET of a parameter that is not set is harmless), so a server that was
// away when a value was removed catches up.
func (s store) removedDatabaseNames(ctx context.Context) (map[string][]string, error) {
	out := map[string][]string{}
	err := s.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT DISTINCT h.scope_id, h.name FROM kingres_cp.config_change h
			WHERE h.level = 'database' AND h.new_value IS NULL
			AND NOT EXISTS (SELECT 1 FROM kingres_cp.config_value v WHERE v.level = 'database' AND v.scope_id = h.scope_id AND v.name = h.name)`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var db, n string
			if err := rows.Scan(&db, &n); err != nil {
				return err
			}
			out[db] = append(out[db], n)
		}
		return rows.Err()
	})
	return out, err
}

// changeRow is a row of kingres_cp.config_change joined with its task.
type changeRow struct {
	ID, Revision    int64
	TaskID, Reverts *int64
	Level, Scope    string
	Name            string
	Old, New        *string
	By              string
	At              time.Time
	TaskState       *string
}

func (s store) changes(ctx context.Context, name, level, scope string, after int64, limit int) ([]changeRow, error) {
	var out []changeRow
	err := s.sys(ctx, func(c *pgx.Conn) error {
		q := `SELECT h.change_id, h.revision, h.task_id, h.reverts, h.level, h.scope_id, h.name, h.old_value, h.new_value, h.changed_by, h.changed_at, t.state
			FROM kingres_cp.config_change h LEFT JOIN kingres_cp.task t ON t.id = h.task_id WHERE true`
		args := []any{}
		add := func(cond string, v any) {
			args = append(args, v)
			q += " AND " + cond + "$" + strconv.Itoa(len(args))
		}
		if name != "" {
			add("h.name = ", name)
		}
		if level != "" {
			add("h.level = ", level)
		}
		if scope != "" {
			add("h.scope_id = ", scope)
		}
		if after > 0 {
			add("h.change_id < ", after)
		}
		args = append(args, limit)
		q += " ORDER BY h.change_id DESC LIMIT $" + strconv.Itoa(len(args))
		rows, err := c.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r changeRow
			if err := rows.Scan(&r.ID, &r.Revision, &r.TaskID, &r.Reverts, &r.Level, &r.Scope, &r.Name, &r.Old, &r.New, &r.By, &r.At, &r.TaskState); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

var errNoChange = errors.New("no such change")

func (s store) change(ctx context.Context, id int64) (changeRow, error) {
	rows, err := s.sys2(ctx, id)
	if err != nil {
		return changeRow{}, err
	}
	if len(rows) == 0 {
		return changeRow{}, errNoChange
	}
	return rows[0], nil
}

func (s store) sys2(ctx context.Context, id int64) ([]changeRow, error) {
	var out []changeRow
	err := s.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT h.change_id, h.revision, h.task_id, h.reverts, h.level, h.scope_id, h.name, h.old_value, h.new_value, h.changed_by, h.changed_at, t.state
			FROM kingres_cp.config_change h LEFT JOIN kingres_cp.task t ON t.id = h.task_id WHERE h.change_id = $1`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r changeRow
			if err := rows.Scan(&r.ID, &r.Revision, &r.TaskID, &r.Reverts, &r.Level, &r.Scope, &r.Name, &r.Old, &r.New, &r.By, &r.At, &r.TaskState); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// latestChange returns the id of the newest change of (level, scope, name).
func (s store) latestChange(ctx context.Context, level, scope, name string) (int64, error) {
	var id int64
	err := s.sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT coalesce(max(change_id), 0) FROM kingres_cp.config_change WHERE level = $1 AND scope_id = $2 AND name = $3`, level, scope, name).Scan(&id)
	})
	return id, err
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lastRevision is the revision of the latest change of a value at a level and scope; 0 when it was never changed.
func (s store) lastRevision(ctx context.Context, level, scope, name string) (int64, error) {
	var rev int64
	err := s.sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT coalesce(max(revision), 0) FROM kingres_cp.config_change WHERE level = $1 AND scope_id = $2 AND name = $3`, level, scope, name).Scan(&rev)
	})
	return rev, err
}
