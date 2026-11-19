// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgeranchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/membership"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// newLedgerAnchor builds the anchoring of the aggregate checkpoints (design 6, step 10 of the plan): the service
// with its credentials files below <ledger-keys-dir>/anchor, its four jobs on the scheduler, and the two internal
// calls that give a holder the credentials of a target. The alerts go through the same function as those of the
// signing keys.
func newLedgerAnchor(cfg config.Config, dir string, sys func(ctx context.Context, f func(c *pgx.Conn) error) error, sched *scheduler.Scheduler,
	members *membership.Manager, calls *callTable, self string, log *slog.Logger,
	alert func(ctx context.Context, kind, severity, subject, message string, raise bool)) (*ledgeranchor.Service, error) {
	creds, err := ledgeranchor.NewCredStore(dir, cfg.LedgerKeyPassphraseFile)
	if err != nil {
		return nil, err
	}
	holders := func(ctx context.Context) ([]string, error) {
		var hosts []string
		err := sys(ctx, func(c *pgx.Conn) error {
			rows, err := c.Query(ctx, `SELECT DISTINCT host FROM kingres.node WHERE group_id = 0 AND is_active ORDER BY host`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var h string
				if err := rows.Scan(&h); err != nil {
					return err
				}
				if h != self {
					hosts = append(hosts, h)
				}
			}
			return rows.Err()
		})
		return hosts, err
	}
	each := func(ctx context.Context, path string, body any) error {
		if members == nil {
			return nil
		}
		hs, err := holders(ctx)
		if err != nil {
			return err
		}
		var bad []string
		for _, h := range hs {
			if err := members.Client.Do(ctx, h, "POST", path, body, nil); err != nil {
				bad = append(bad, h)
			}
		}
		if len(bad) > 0 {
			return fmt.Errorf("not reached: %s", strings.Join(bad, ", "))
		}
		return nil
	}
	svc, err := ledgeranchor.New(ledgeranchor.Config{
		Store: ledgeranchor.PGStore{Sys: sys}, Creds: creds, Term: sched.Term, Log: log.With("component", "ledger-anchor"),
		Alert: alert, Limits: anchorLimits(os.Getenv, log),
		OpenAlerts: func(ctx context.Context) ([]ledgeranchor.OpenAlert, error) {
			var out []ledgeranchor.OpenAlert
			err := sys(ctx, func(c *pgx.Conn) error {
				rows, err := c.Query(ctx, `SELECT kind, subject, severity FROM kingres_cp.alert WHERE cleared_at IS NULL AND kind = ANY($1)`,
					[]string{ledgeranchor.AlertNotAnchored, ledgeranchor.AlertBehind, ledgeranchor.AlertFailed, ledgeranchor.AlertConflict})
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var a ledgeranchor.OpenAlert
					if err := rows.Scan(&a.Kind, &a.Subject, &a.Severity); err != nil {
						return err
					}
					out = append(out, a)
				}
				return rows.Err()
			})
			return out, err
		},
		Databases: func(ctx context.Context) ([]string, error) {
			var out []string
			err := sys(ctx, func(c *pgx.Conn) error {
				rows, err := c.Query(ctx, `SELECT database_name FROM kingres_cp.database_setting WHERE ledger_mode <> 'off' ORDER BY database_name`)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var d string
					if err := rows.Scan(&d); err != nil {
						return err
					}
					out = append(out, d)
				}
				return rows.Err()
			})
			return out, err
		},
		DatabaseExists: func(ctx context.Context, db string) (bool, error) {
			var ok bool
			err := sys(ctx, func(c *pgx.Conn) error {
				return c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1)`, db).Scan(&ok)
			})
			return ok, err
		},
		Distribute: func(ctx context.Context, id int64, kind string, file []byte) error {
			return each(ctx, "/ledger/anchor/credentials/install", ledgeranchor.InstallRequest{TargetID: id, Kind: kind, File: base64.StdEncoding.EncodeToString(file)})
		},
		Undistribute: func(ctx context.Context, id int64) error {
			return each(ctx, "/ledger/anchor/credentials/remove", ledgeranchor.RemoveRequest{TargetID: id})
		},
	})
	if err != nil {
		return nil, err
	}
	for _, j := range svc.Jobs() {
		if err := sched.Add(j); err != nil {
			return nil, err
		}
	}
	if calls != nil {
		agent := &ledgeranchor.Agent{Creds: creds}
		for _, key := range ledgeranchor.Calls {
			calls.Register(key, func(ctx context.Context, _ string, body []byte) (any, error) { return agent.Handle(ctx, key, body) })
		}
	}
	return svc, nil
}

// anchorLimits reads the limits of the retries of a record (design 6.6, duty 16 of internal/anchor): KINGRES_ANCHOR_MAX_RETRY_ATTEMPTS (an integer,
// default 200) and KINGRES_ANCHOR_MAX_RETRY_AGE_HOURS (an integer, default 168). They are the settings anchor.maxRetryAttempts and
// anchor.maxRetryAgeHours of the design; the settings schema of the contract (LedgerAnchorSettings) has no member for them, so they are
// environment variables of the daemon and not cluster settings (correction 189). A value that is not a positive integer is ignored with a warning.
func anchorLimits(getenv func(string) string, log *slog.Logger) anchor.Limits {
	var l anchor.Limits
	if v := getenv("KINGRES_ANCHOR_MAX_RETRY_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			l.MaxAttempts = n
		} else {
			log.Warn("KINGRES_ANCHOR_MAX_RETRY_ATTEMPTS is not a positive integer: the default is used")
		}
	}
	if v := getenv("KINGRES_ANCHOR_MAX_RETRY_AGE_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 24*365 {
			l.MaxAge = time.Duration(n) * time.Hour
		} else {
			log.Warn("KINGRES_ANCHOR_MAX_RETRY_AGE_HOURS is not a number of hours from 1 to 8760: the default is used")
		}
	}
	return l
}
