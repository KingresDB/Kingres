// SPDX-License-Identifier: PostgreSQL

// Package monitor runs the periodic health checks (design D6) that raise and
// clear alerts in kingres_cp.alert, and samples metrics into a bounded ring
// buffer (metrics.go). Alert kinds are those of the contract: instance down
// (serverDown), diskSpaceLow, memoryPressure and replicationLagHigh.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// Thresholds of the checks.
const (
	DiskWarnPercent  = 90.0
	DiskCritPercent  = 95.0
	MemoryLowPercent = 10.0 // available memory below this share
	LagWarnSeconds   = 60.0
)

// Managed lists the alert kinds the checks own; others are never cleared by them.
var Managed = []string{"serverDown", "diskSpaceLow", "memoryPressure", "replicationLagHigh"}

// Readings are the inputs of the checks; nil means "not measured", which
// neither raises nor clears an alert.
type Readings struct {
	// Host is the server id of this server.
	Host string
	// InstanceUp is false when the managed instance is not running.
	InstanceUp bool
	// DiskUsedPercent, MemAvailPercent and LagSeconds are the measured values.
	DiskUsedPercent, MemAvailPercent, LagSeconds *float64
}

// Condition is a problem that exists now.
type Condition struct {
	Kind, Severity, Subject, Message string
}

// Evaluate returns the problems the readings show and the kinds that were
// actually evaluated (only those may be cleared).
func Evaluate(r Readings) (conds []Condition, evaluated map[string]bool) {
	subject := "server:" + r.Host
	evaluated = map[string]bool{"serverDown": true}
	if !r.InstanceUp {
		conds = append(conds, Condition{"serverDown", "critical", subject, "The PostgreSQL instance of server " + r.Host + " is not running."})
	}
	if r.DiskUsedPercent != nil {
		evaluated["diskSpaceLow"] = true
		switch p := *r.DiskUsedPercent; {
		case p >= DiskCritPercent:
			conds = append(conds, Condition{"diskSpaceLow", "critical", subject, fmt.Sprintf("The data disk of server %s is %.0f%% full.", r.Host, p)})
		case p >= DiskWarnPercent:
			conds = append(conds, Condition{"diskSpaceLow", "warning", subject, fmt.Sprintf("The data disk of server %s is %.0f%% full.", r.Host, p)})
		}
	}
	if r.MemAvailPercent != nil {
		evaluated["memoryPressure"] = true
		if p := *r.MemAvailPercent; p < MemoryLowPercent {
			conds = append(conds, Condition{"memoryPressure", "warning", subject, fmt.Sprintf("Only %.0f%% of the memory of server %s is available.", p, r.Host)})
		}
	}
	if r.LagSeconds != nil {
		evaluated["replicationLagHigh"] = true
		if l := *r.LagSeconds; l > LagWarnSeconds {
			conds = append(conds, Condition{"replicationLagHigh", "warning", subject, fmt.Sprintf("A standby of server %s is %.0f seconds behind.", r.Host, l)})
		}
	}
	return conds, evaluated
}

// Reconcile makes the alert table match the conditions: a condition without an
// open alert raises one (severity and message of an open alert are updated), an
// open alert of an evaluated kind without a condition is resolved. An open
// alert survives a restart of kingresd.
func Reconcile(ctx context.Context, c *pgx.Conn, host string, conds []Condition, evaluated map[string]bool, now time.Time) error {
	subject := "server:" + host
	have := map[string]bool{}
	for _, cd := range conds {
		have[cd.Kind] = true
		_, err := c.Exec(ctx, `INSERT INTO kingres_cp.alert (severity, kind, subject, message, raised_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (kind, subject) WHERE cleared_at IS NULL DO UPDATE SET severity = EXCLUDED.severity, message = EXCLUDED.message`,
			cd.Severity, cd.Kind, cd.Subject, cd.Message, now)
		if err != nil {
			return err
		}
	}
	for kind := range evaluated {
		if have[kind] {
			continue
		}
		if _, err := c.Exec(ctx, "UPDATE kingres_cp.alert SET cleared_at = $3 WHERE kind = $1 AND subject = $2 AND cleared_at IS NULL", kind, subject, now); err != nil {
			return err
		}
	}
	return nil
}

// RecordOutage stores an outage that ended: the instance was down from since
// until now, a time in which nothing could be written to the database.
func RecordOutage(ctx context.Context, c *pgx.Conn, host string, since, now time.Time) error {
	_, err := c.Exec(ctx, `INSERT INTO kingres_cp.alert (severity, kind, subject, message, raised_at, cleared_at)
		VALUES ('critical', 'serverDown', $1, $2, $3, $4)`, "server:"+host,
		"The PostgreSQL instance of server "+host+" was not running.", since, now)
	return err
}

// ErrNotFound and ErrResolved are the outcomes of Acknowledge.
var (
	ErrNotFound = errors.New("alert not found")
	ErrResolved = errors.New("alert already resolved")
)

const alertCols = `id, kind, severity, subject, message, raised_at, acknowledged_at, acknowledged_by, cleared_at`

var titles = map[string]string{
	"serverDown": "Server down", "diskSpaceLow": "Disk space low", "memoryPressure": "Memory pressure",
	"replicationLagHigh": "Replication lag high",
}

func scanAlert(r pgx.Row) (api.Alert, error) {
	var a api.Alert
	var subject string
	var raised time.Time
	var ack, cleared *time.Time
	if err := r.Scan(&a.ID, &a.Kind, &a.Severity, &subject, &a.Message, &raised, &ack, &a.AcknowledgedBy, &cleared); err != nil {
		return a, err
	}
	a.Title = titles[a.Kind]
	if a.Title == "" {
		a.Title = a.Kind
	}
	if len(subject) > 7 && subject[:7] == "server:" {
		id := subject[7:]
		a.ServerID = &id
	}
	a.RaisedAt = raised.UTC()
	switch {
	case cleared != nil:
		a.State = "resolved"
	case ack != nil:
		a.State = "acknowledged"
	default:
		a.State = "open"
	}
	if ack != nil {
		u := ack.UTC()
		a.AcknowledgedAt = &u
	}
	if cleared != nil {
		u := cleared.UTC()
		a.ResolvedAt = &u
	}
	return a, nil
}

// List returns alerts newest first with a keyset cursor on the id.
func List(ctx context.Context, c *pgx.Conn, state, severity string, p store.Page) (api.AlertPage, error) {
	page := api.AlertPage{Items: []api.Alert{}}
	before, bounded, err := store.DecodeKey(p.Cursor)
	if err != nil {
		return page, err
	}
	var bound any
	if bounded {
		bound = before
	}
	rows, err := c.Query(ctx, `SELECT `+alertCols+` FROM kingres_cp.alert
		WHERE ($1::bigint IS NULL OR id < $1) AND ($2 = '' OR severity = $2)
		  AND ($3 = '' OR ($3 = 'resolved' AND cleared_at IS NOT NULL)
		       OR ($3 = 'acknowledged' AND cleared_at IS NULL AND acknowledged_at IS NOT NULL)
		       OR ($3 = 'open' AND cleared_at IS NULL AND acknowledged_at IS NULL))
		ORDER BY id DESC LIMIT $4`, bound, severity, state, p.Limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, a)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > p.Limit {
		next := store.EncodeKey(page.Items[p.Limit-1].ID)
		page.NextCursor = &next
		page.Items = page.Items[:p.Limit]
	}
	return page, nil
}

// Acknowledge marks an open alert acknowledged by user; an acknowledged alert
// is returned as it is, a resolved one is ErrResolved.
func Acknowledge(ctx context.Context, c *pgx.Conn, id int64, user string, now time.Time) (api.Alert, error) {
	var cleared *time.Time
	if err := c.QueryRow(ctx, "SELECT cleared_at FROM kingres_cp.alert WHERE id = $1", id).Scan(&cleared); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return api.Alert{}, ErrNotFound
		}
		return api.Alert{}, err
	}
	if cleared != nil {
		return api.Alert{}, ErrResolved
	}
	if _, err := c.Exec(ctx, "UPDATE kingres_cp.alert SET acknowledged_at = $2, acknowledged_by = $3 WHERE id = $1 AND acknowledged_at IS NULL", id, now, user); err != nil {
		return api.Alert{}, err
	}
	return scanAlert(c.QueryRow(ctx, "SELECT "+alertCols+" FROM kingres_cp.alert WHERE id = $1", id))
}

// OpenCounts returns the number of open alerts by severity.
func OpenCounts(ctx context.Context, c *pgx.Conn) (map[string]int, error) {
	rows, err := c.Query(ctx, "SELECT severity, count(*) FROM kingres_cp.alert WHERE cleared_at IS NULL GROUP BY 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}
