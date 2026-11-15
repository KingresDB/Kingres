// SPDX-License-Identifier: AGPL-3.0-only

package monitor

import (
	"context"
	"crypto/x509"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/serverinfo"
)

// Source gives the runner what it checks.
type Source interface {
	// ServiceConn opens a service connection to db.
	ServiceConn(ctx context.Context, db string) (*pgx.Conn, error)
	// InstanceRunning reports whether the managed instance runs.
	InstanceRunning(ctx context.Context) (bool, error)
}

// Runner runs the health checks and the sampler.
type Runner struct {
	Src      Source
	Ring     *Ring
	Host     string
	DataDir  string
	Interval time.Duration
	Log      *slog.Logger
	// System is the system database name (alerts live there).
	System string
	// Cert reads this server's certificate (nil: not checked); CA returns the
	// certificate of the CA this server holds (nil: none); RenewFraction is the
	// part of its lifetime at which the certificate is renewed.
	// Leases returns the group leases of this server (nil: no lease manager).
	Leases        func() []ha.LeaseInfo
	Cert          func() (*x509.Certificate, error)
	CA            func() *x509.Certificate
	RenewFraction float64
	// LedgerPendingMaxAge overrides the age of the oldest pending ledger
	// transaction that raises ledgerPendingTooOld (0: LedgerPendingMaxAge).
	LedgerPendingMaxAge time.Duration
	// Probes are extra checks run in every round: each returns the conditions that exist
	// and the alert kinds it evaluated (only those may be cleared).
	Probes []func(ctx context.Context, now time.Time) ([]Condition, map[string]bool)

	tracker *PendingTracker
	round   int

	downSince time.Time
	lastXact  map[string]float64
	lastAt    time.Time
}

// Run checks and samples every Interval until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	if r.Interval == 0 {
		r.Interval = 10 * time.Second
	}
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		r.Once(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func f(v float64) *float64 { return &v }

// Once performs one round of checks and one sample.
func (r *Runner) Once(ctx context.Context, now time.Time) {
	up, err := r.Src.InstanceRunning(ctx)
	if err != nil {
		r.Log.Warn("checking the instance", "error", err)
		return
	}
	rd := Readings{Host: r.Host, InstanceUp: up}
	res := serverinfo.Resources(map[string]string{"data": r.DataDir}, nil)
	if len(res.Disks) == 1 && res.Disks[0].TotalBytes != nil && *res.Disks[0].TotalBytes > 0 {
		used := float64(*res.Disks[0].UsedBytes) * 100 / float64(*res.Disks[0].TotalBytes)
		rd.DiskUsedPercent = &used
		r.Ring.Add(Series{Metric: "diskUsedBytes", ServerID: r.Host}, now, f(float64(*res.Disks[0].UsedBytes)))
	}
	if res.Memory.TotalBytes != nil && res.Memory.UsedBytes != nil && *res.Memory.TotalBytes > 0 {
		avail := float64(*res.Memory.TotalBytes-*res.Memory.UsedBytes) * 100 / float64(*res.Memory.TotalBytes)
		rd.MemAvailPercent = &avail
		r.Ring.Add(Series{Metric: "memoryUsedBytes", ServerID: r.Host}, now, f(float64(*res.Memory.UsedBytes)))
	}
	if res.CPU.UsagePercent != nil {
		r.Ring.Add(Series{Metric: "cpuUsagePercent", ServerID: r.Host}, now, res.CPU.UsagePercent)
	}
	if !up {
		if r.downSince.IsZero() {
			r.downSince = now
		}
		return // nothing can be stored or measured in PostgreSQL now
	}
	c, err := r.Src.ServiceConn(ctx, r.System)
	if err != nil {
		r.Log.Warn("monitor connection", "error", err)
		return
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if !r.downSince.IsZero() {
		if err := RecordOutage(ctx, c, r.Host, r.downSince, now); err != nil {
			r.Log.Warn("recording an outage", "error", err)
		}
		r.downSince = time.Time{}
	}
	r.sample(ctx, c, now, &rd)
	r.certificates(now, &rd)
	if r.round%3 == 0 {
		r.ledger(ctx, c, now, &rd)
	}
	if rep, err := ReadReplication(ctx, c); err != nil {
		r.Log.Warn("reading the replication tasks", "error", err)
	} else {
		rd.ReplicationChecked, rd.Replication, rd.Now = true, rep, now
	}
	if r.Leases != nil {
		rd.LeasesChecked = true
		for _, l := range r.Leases() {
			if l.State == "held" || l.State == "lost" {
				rd.Leases = append(rd.Leases, LeaseReading{Group: l.Group, Lost: l.State == "lost", Enforced: l.Enforced})
			}
		}
	}
	r.round++
	conds, evaluated := Evaluate(rd)
	if !rd.LedgerChecked {
		delete(evaluated, "ledgerPendingTooOld")
	}
	// Probes add the conditions of features that know their own state (standbys): their
	// kinds are evaluated with the others, so that one Reconcile raises and clears all.
	for _, p := range r.Probes {
		extra, kinds := p(ctx, now)
		conds = append(conds, extra...)
		for k := range kinds {
			evaluated[k] = true
		}
	}
	if err := Reconcile(ctx, c, r.Host, conds, evaluated, now); err != nil {
		r.Log.Warn("updating alerts", "error", err)
	}
	var v string
	if err := c.QueryRow(ctx, "SELECT version()").Scan(&v); err == nil && !strings.Contains(v, "KingresDB") {
		// The contract has no alert kind for this; it is an operator error.
		r.Log.Error("the PostgreSQL instance is not a KingresDB build: version() does not contain KingresDB")
	}
}

func (r *Runner) sample(ctx context.Context, c *pgx.Conn, now time.Time, rd *Readings) {
	var conns int
	if err := c.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'").Scan(&conns); err == nil {
		r.Ring.Add(Series{Metric: "connections", ServerID: r.Host}, now, f(float64(conns)))
	}
	var lagS, lagB *float64
	if err := c.QueryRow(ctx, "SELECT max(extract(epoch FROM replay_lag))::float8, max(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn))::float8 FROM pg_stat_replication").Scan(&lagS, &lagB); err == nil {
		rd.LagSeconds = lagS
		r.Ring.Add(Series{Metric: "replicationLagSeconds", ServerID: r.Host}, now, lagS)
		r.Ring.Add(Series{Metric: "replicationLagBytes", ServerID: r.Host}, now, lagB)
	}
	rows, err := c.Query(ctx, `SELECT d.datname, pg_database_size(d.datname)::float8, (s.xact_commit + s.xact_rollback)::float8
		FROM pg_database d JOIN pg_stat_database s ON s.datid = d.oid WHERE NOT d.datistemplate AND d.datname <> $1`, r.System)
	if err != nil {
		return
	}
	defer rows.Close()
	cur := map[string]float64{}
	for rows.Next() {
		var name string
		var size, x float64
		if rows.Scan(&name, &size, &x) != nil {
			continue
		}
		r.Ring.Add(Series{Metric: "databaseSizeBytes", Database: name}, now, f(size))
		cur[name] = x
		if prev, ok := r.lastXact[name]; ok && now.After(r.lastAt) && x >= prev {
			r.Ring.Add(Series{Metric: "transactionsPerSecond", Database: name}, now, f((x-prev)/now.Sub(r.lastAt).Seconds()))
		}
	}
	r.lastXact, r.lastAt = cur, now
}
