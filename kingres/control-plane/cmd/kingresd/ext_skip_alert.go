// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// The alert extensionSkipped (contract 0.30.0, severity warning, subject "server:<id>|database:<name>|extension:<name>", one alert per skipped extension): an extension of a database was left out on this server because
// a role of the tenant made a schema of the extension its own (review of g2, M2). The operation that found it (an added server) goes on; this alert is how an operator
// learns of it. It is raised by the cluster service when it applies the database, and cleared when a later apply of the extension on this server succeeds or the
// database is dropped (ops, dropDB).

// alertSys runs f on a connection to the catalog database of this server.
type alertSys func(ctx context.Context, f func(c *pgx.Conn) error) error

func serviceAlertSys(svc *cluster.Service) alertSys {
	return func(ctx context.Context, f func(c *pgx.Conn) error) error {
		c, err := svc.ServiceConn(ctx, sysdb.Database)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
		return f(c)
	}
}

// extensionSkippedSubject: every skipped extension has its own alert, so a second one does not overwrite the text of the first. The contract reads the last segment as the aspect.
func extensionSkippedSubject(host, database, extension string) string {
	return "server:" + host + "|database:" + database + "|extension:" + api.SubjectEscape(extension)
}

// extensionSkippedMessage is the text of the alert: the extension first then the reason, which names the schema and both owners.
func extensionSkippedMessage(host, database, extension, reason string) string {
	return fmt.Sprintf("Extension %s was not installed in database %s on server %s: %s.", extension, database, host, reason)
}

// raiseExtensionSkipped is the handler of the cluster service for an extension that was left out.
func raiseExtensionSkipped(sys alertSys, host string, log *slog.Logger) func(ctx context.Context, database, extension, reason string) {
	return func(ctx context.Context, database, extension, reason string) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err := sys(ctx, func(c *pgx.Conn) error {
			_, e := c.Exec(ctx, `INSERT INTO kingres_cp.alert (severity, kind, subject, message, raised_at) VALUES ('warning', $1, $2, $3, now())
				ON CONFLICT (kind, subject) WHERE cleared_at IS NULL DO UPDATE SET message = $3`,
				sysdb.AlertExtensionSkipped, extensionSkippedSubject(host, database, extension), extensionSkippedMessage(host, database, extension, reason))
			return e
		})
		if err != nil {
			log.Warn("the alert for a skipped extension could not be raised", "database", database, "extension", extension, "error", err)
		}
	}
}

// clearExtensionSkipped is the handler of the cluster service for an extension that was installed: it clears the alert about this extension on this server.
func clearExtensionSkipped(sys alertSys, host string, log *slog.Logger) func(ctx context.Context, database, extension string) {
	return func(ctx context.Context, database, extension string) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err := sys(ctx, func(c *pgx.Conn) error {
			_, e := c.Exec(ctx, `UPDATE kingres_cp.alert SET cleared_at = now() WHERE kind = $1 AND subject = $2 AND cleared_at IS NULL`,
				sysdb.AlertExtensionSkipped, extensionSkippedSubject(host, database, extension))
			return e
		})
		if err != nil {
			log.Warn("the alert for a skipped extension could not be cleared", "database", database, "extension", extension, "error", err)
		}
	}
}
