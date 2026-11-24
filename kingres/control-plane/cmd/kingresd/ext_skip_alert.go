// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// raiseExtensionSkipped is the handler of the cluster service for an extension that was left out of a database on this server because a tenant made the schema of the
// extension its own (review of g2, M2). The operation that found it (an added server) goes on; this alert is how an operator learns of it. It uses the existing
// kind extensionSchemaUntrusted (the schema of an extension in a database cannot be trusted), with severity warning and the subject server and database.
func raiseExtensionSkipped(svc *cluster.Service, host string, log *slog.Logger) func(ctx context.Context, database, extension, reason string) {
	return func(ctx context.Context, database, extension, reason string) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		c, err := svc.ServiceConn(ctx, sysdb.Database)
		if err != nil {
			log.Warn("the alert for a skipped extension could not be raised", "database", database, "extension", extension, "error", err)
			return
		}
		defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
		msg := fmt.Sprintf("Extension %s was not installed in database %s on server %s: %s.", extension, database, host, reason)
		if _, err := c.Exec(ctx, `INSERT INTO kingres_cp.alert (severity, kind, subject, message, raised_at)
			VALUES ('warning', 'extensionSchemaUntrusted', $1, $2, now()) ON CONFLICT (kind, subject) WHERE cleared_at IS NULL DO UPDATE SET message = $2`,
			"server:"+host+"|database:"+database, msg); err != nil {
			log.Warn("the alert for a skipped extension could not be raised", "database", database, "extension", extension, "error", err)
		}
	}
}
