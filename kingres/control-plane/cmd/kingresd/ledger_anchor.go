// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgeranchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/membership"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
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
	// a server that has just got a passphrase file holds its own files in clear: they are sealed, keeping generation and stamp (P7I-02)
	if n, serr := creds.SealPlainFiles(); serr != nil {
		log.Warn("the credentials files in clear could not all be sealed", "sealed", n, "error", serr.Error())
	} else if n > 0 {
		log.Info("the credentials files in clear were sealed", "files", n)
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
				// the reason goes with the name: a holder that refuses a file (it seals its files and this server does not, a number that is far
				// above its own) is told apart from a holder that did not answer (P7I-02)
				reason := err.Error()
				if len(reason) > 160 {
					reason = reason[:160]
				}
				bad = append(bad, h+" ("+reason+")")
			}
		}
		if len(bad) > 0 {
			return fmt.Errorf("not reached or refused: %s", strings.Join(bad, ", "))
		}
		return nil
	}
	// fetch asks the other holders, one after the other, for the credentials file of a target this server lacks (P7A-03). It answers
	// ErrNoCredentials only when every holder answered that it has none; a holder that could not be asked makes it ErrHoldersUnreached,
	// so that the records of the target wait and are not failed for good (P7R-08).
	fetch := func(ctx context.Context, id int64) ([]byte, error) {
		if members == nil {
			return nil, ledgeranchor.ErrNoCredentials
		}
		hs, err := holders(ctx)
		if err != nil {
			return nil, err
		}
		return ledgeranchor.FetchFirst(ctx, hs, func(ctx context.Context, h string) ([]byte, ledgeranchor.HolderAnswer) {
			var out ledgeranchor.FetchResponse
			if err := members.Client.Do(ctx, h, "POST", "/ledger/anchor/credentials/fetch", ledgeranchor.FetchRequest{TargetID: id}, &out); err != nil {
				return nil, fetchAnswerOf(err)
			}
			raw, err := base64.StdEncoding.Strict().DecodeString(out.File)
			out.File = ""
			if err != nil || len(raw) == 0 {
				return nil, ledgeranchor.HolderUnreached
			}
			return raw, ledgeranchor.HolderHasFile
		})
	}
	// fetchFrom asks one holder, by name, for its file: the holder that says its file is newer than the one of this server (P7F-01).
	fetchFrom := func(ctx context.Context, host string, id int64) ([]byte, error) {
		if members == nil {
			return nil, ledgeranchor.ErrNoCredentials
		}
		var out ledgeranchor.FetchResponse
		if err := members.Client.Do(ctx, host, "POST", "/ledger/anchor/credentials/fetch", ledgeranchor.FetchRequest{TargetID: id}, &out); err != nil {
			return nil, err
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(out.File)
		out.File = ""
		if err != nil || len(raw) == 0 {
			return nil, errors.New("the answer holds no file")
		}
		return raw, nil
	}
	// holderReport asks every other holder which files it has: the holders that lack a file of ids, and the files of targets that are gone
	holderReport := func(ctx context.Context, ids []int64) (ledgeranchor.HolderReport, error) {
		rep := ledgeranchor.NewHolderReport()
		if members == nil {
			return rep, nil
		}
		hs, err := holders(ctx)
		if err != nil {
			return rep, err
		}
		for _, h := range hs {
			var out ledgeranchor.ListResponse
			if err := members.Client.Do(ctx, h, "POST", "/ledger/anchor/credentials/list", struct{}{}, &out); err != nil {
				rep.Unreached = append(rep.Unreached, h)
				continue
			}
			rep.Add(h, out, ids)
		}
		return rep, nil
	}
	svc, err := ledgeranchor.New(ledgeranchor.Config{
		Fetch: fetch, FetchFrom: fetchFrom, Holders: holderReport,
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
			return each(ctx, "/ledger/anchor/credentials/install", ledgeranchor.InstallRequest{TargetID: id, Kind: kind, File: base64.StdEncoding.EncodeToString(file), Known: ledgeranchor.KnownGeneration(ctx)})
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

// fetchAnswerOf classifies the error of a request for a credentials file: the answer "not found" of a holder (404 not_found) says that the
// holder was asked and has none; every other error (the holder did not answer, answered 503 or 422) says nothing about the file.
func fetchAnswerOf(err error) ledgeranchor.HolderAnswer {
	var ae *peerclient.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound && ae.Code == "not_found" && ae.Message == ledgeranchor.FetchNoneMessage {
		return ledgeranchor.HolderHasNone
	}
	return ledgeranchor.HolderUnreached
}
