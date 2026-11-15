// SPDX-License-Identifier: AGPL-3.0-only

package monitor

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// certificates fills the readings of the certificates of this server: its own
// server certificate (the one the renewal job renews) and, on the server that
// holds it, the certificate of the cluster CA.
func (r *Runner) certificates(now time.Time, rd *Readings) {
	if r.Cert == nil {
		return
	}
	cert, err := r.Cert()
	if err != nil || cert == nil {
		return // no certificate: not a member of a cluster, nothing to check
	}
	frac := r.RenewFraction
	if frac <= 0 {
		frac = pki.DefaultRenewFraction
	}
	rd.Certificates = []CertificateReading{{Remaining: pki.RemainingFraction(cert, now), NotAfter: cert.NotAfter, RenewFraction: frac, Now: now}}
	if r.CA != nil {
		if ca := r.CA(); ca != nil {
			rd.Certificates = append(rd.Certificates, CertificateReading{Remaining: pki.RemainingFraction(ca, now), NotAfter: ca.NotAfter, CA: true, Now: now})
		}
	}
}

// ledger reads the ledger queue of every database that has one (a table
// kingres.ledger_pending) and fills the readings; when a database could not be
// read the check is not marked as done, so that it neither raises nor clears.
func (r *Runner) ledger(ctx context.Context, sys *pgx.Conn, now time.Time, rd *Readings) {
	if r.tracker == nil {
		r.tracker = NewPendingTracker()
	}
	rows, err := sys.Query(ctx, `SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate AND datname <> $1 ORDER BY 1`, r.System)
	if err != nil {
		return
	}
	dbs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return
	}
	var out []LedgerPendingReading
	for _, db := range dbs {
		reading, ok, err := r.pending(ctx, db, now)
		if err != nil {
			return
		}
		if ok {
			out = append(out, reading)
		}
	}
	rd.LedgerChecked, rd.LedgerPending, rd.LedgerMaxAge = true, out, r.LedgerPendingMaxAge
}

// pending reads the queue of one database; ok is false when it has no ledger.
func (r *Runner) pending(ctx context.Context, db string, now time.Time) (LedgerPendingReading, bool, error) {
	c, err := r.Src.ServiceConn(ctx, db)
	if err != nil {
		return LedgerPendingReading{}, false, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var has bool
	if err := c.QueryRow(ctx, `SELECT to_regclass('kingres.ledger_pending') IS NOT NULL`).Scan(&has); err != nil || !has {
		return LedgerPendingReading{}, false, err
	}
	var oldest *int64
	var count int64
	if err := c.QueryRow(ctx, `SELECT min(ledger_txid), count(*) FROM kingres.ledger_pending`).Scan(&oldest, &count); err != nil {
		return LedgerPendingReading{}, false, err
	}
	return LedgerPendingReading{Database: db, Count: count, Age: r.tracker.Observe(db, oldest, count, now)}, true, nil
}
