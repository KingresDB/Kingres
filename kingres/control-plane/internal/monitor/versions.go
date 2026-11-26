// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package monitor

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// VersionSource gives the manager leader the versions of the servers.
type VersionSource interface {
	// Local returns the versions of this server.
	Local(ctx context.Context) (ServerVersions, error)
	// Others lists the other active servers.
	Others(ctx context.Context) ([]string, error)
	// Ask asks one of them for its versions.
	Ask(ctx context.Context, server string) (ServerVersions, error)
}

// CheckVersions compares the versions of KingresDB, the PostgreSQL build and
// the extensions of the servers that answer and raises or clears versionMismatch
// (the alert of the cluster, kept on the server that runs the check, the manager
// leader). A server that does not answer is left out; with fewer than two
// answers nothing is raised and nothing is cleared.
func CheckVersions(ctx context.Context, c *pgx.Conn, src VersionSource, now time.Time) error {
	local, err := src.Local(ctx)
	if err != nil {
		return err
	}
	all := []ServerVersions{local}
	ids, err := src.Others(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		v, err := src.Ask(actx, id)
		cancel()
		if err != nil {
			continue
		}
		v.Server = id
		all = append(all, v)
	}
	if len(all) < 2 {
		return nil
	}
	return ReconcileScope(ctx, c, "cluster", EvaluateVersions(all), map[string]bool{"versionMismatch": true}, now)
}
