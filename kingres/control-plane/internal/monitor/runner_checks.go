// SPDX-License-Identifier: AGPL-3.0-only

package monitor

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
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
	var untrusted []ExtensionReading
	for _, db := range dbs {
		reading, ok, bad, err := r.pending(ctx, db, now)
		if err != nil {
			return
		}
		untrusted = append(untrusted, bad...)
		if ok {
			out = append(out, reading)
		}
	}
	// The system database of the control plane: the service role owns it and nobody else can create there; the readers of kingres_cp and kingres in it do not
	// check the extension, so what they rely on is this check (extguard.SystemDatabaseProblems).
	sysProblems, err := extguard.SystemDatabaseProblems(ctx, sys)
	if err != nil {
		return
	}
	for _, p := range sysProblems {
		untrusted = append(untrusted, ExtensionReading{Database: r.System, Extension: "the system database", Schema: p.Object, Reason: string(p.Reason)})
	}
	rd.LedgerChecked, rd.LedgerPending, rd.LedgerMaxAge = true, out, r.LedgerPendingMaxAge
	rd.ExtensionsChecked, rd.ExtensionsUntrusted = true, untrusted
}

// pending reads the queue of one database; ok is false when it has no ledger. Before anything of the schema kingres is read, every KingresDB extension of the
// database is checked (extguard): a database owner can make a schema of that name, with a view whose function would run as the service role, a superuser. An
// extension whose schema cannot be trusted is returned in untrusted (the monitor raises extensionSchemaUntrusted for it) and nothing of it is read.
func (r *Runner) pending(ctx context.Context, db string, now time.Time) (reading LedgerPendingReading, ok bool, untrusted []ExtensionReading, err error) {
	c, err := r.Src.ServiceConn(ctx, db)
	if err != nil {
		return LedgerPendingReading{}, false, nil, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	res, err := extguard.VerifyAll(ctx, c, extguard.All...)
	if err != nil {
		return LedgerPendingReading{}, false, nil, err
	}
	var ledger extguard.Info
	var ledgerErr error
	for _, x := range res {
		if u, bad := extguard.IsUntrusted(x.Err); bad {
			untrusted = append(untrusted, ExtensionReading{Database: db, Extension: u.Extension, Schema: u.Schema, Reason: string(u.Reason), Detail: u.Detail})
		}
		if x.Info.Name == extguard.Ledger {
			ledger, ledgerErr = x.Info, x.Err
		}
	}
	if ledgerErr != nil || !ledger.Present {
		return LedgerPendingReading{}, false, untrusted, nil
	}
	// The schema is the extension's alone (nothing in it that is not a member of an extension), so the table is the extension's, if it exists.
	var has bool
	if err := c.QueryRow(ctx, `SELECT pg_catalog.to_regclass('kingres.ledger_pending') IS NOT NULL`).Scan(&has); err != nil || !has {
		return LedgerPendingReading{}, false, untrusted, err
	}
	var oldest *int64
	var count int64
	if err := c.QueryRow(ctx, `SELECT min(ledger_txid), count(*) FROM kingres.ledger_pending`).Scan(&oldest, &count); err != nil {
		return LedgerPendingReading{}, false, untrusted, err
	}
	return LedgerPendingReading{Database: db, Count: count, Age: r.tracker.Observe(db, oldest, count, now)}, true, untrusted, nil
}
