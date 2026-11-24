// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// The alert extensionSkipped (contract 0.30.0): raised by the handler of the skip, cleared by the handler of a later successful apply of that extension, and cleared when
// the database is dropped.
func TestExtensionSkippedAlertIsRaisedAndCleared(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	sys := alertSys(func(ctx context.Context, f func(c *pgx.Conn) error) error {
		c := e.serviceConn(t, sysdb.Database)
		defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
		return f(c)
	})
	log := slog.Default()
	raise, clear := raiseExtensionSkipped(sys, "h1", log), clearExtensionSkipped(sys, "h1", log)
	ctx := context.Background()
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "dbskip", "sharding": map[string]any{"enabled": false}, "replication": map[string]any{"durability": "async"}})
	open := func() string {
		sc := e.serviceConn(t, sysdb.Database)
		defer func() { _ = sc.Close(ctx) }()
		return rowsText(t, sc, "select severity, subject, message from kingres_cp.alert where kind = 'extensionSkipped' and cleared_at is null order by subject")
	}
	if got := open(); !isNone(got) {
		t.Fatalf("alerts before: %s", got)
	}
	raise(ctx, "dbskip", "pgcrypto", "schema x of extension pgcrypto is owned by intruder on this server and by o on the leader")
	got := open()
	for _, want := range []string{"warning", "server:h1|database:dbskip", "Extension pgcrypto was not installed in database dbskip on server h1", "owned by intruder on this server and by o on the leader"} {
		if !strings.Contains(got, want) {
			t.Errorf("the alert does not hold %q: %s", want, got)
		}
	}
	// Another extension of the same database replaces the text (one open alert per subject); the clearing of the first one then leaves it.
	raise(ctx, "dbskip", "hstore", "schema y of extension hstore is owned by intruder on this server and by o on the leader")
	clear(ctx, "dbskip", "pgcrypto")
	if got := open(); !strings.Contains(got, "Extension hstore was not installed") {
		t.Errorf("the apply of another extension cleared the alert: %s", got)
	}
	// An apply of an extension on another database or server does not clear it.
	clear(ctx, "other", "hstore")
	clearOther := clearExtensionSkipped(sys, "h2", log)
	clearOther(ctx, "dbskip", "hstore")
	if got := open(); isNone(got) {
		t.Errorf("the apply of the extension elsewhere cleared the alert")
	}
	clear(ctx, "dbskip", "hstore")
	if got := open(); !isNone(got) {
		t.Errorf("the apply of the extension on this server did not clear the alert: %s", got)
	}
	// A dropped database clears its alerts.
	raise(ctx, "dbskip", "pgcrypto", "schema x of extension pgcrypto is owned by intruder on this server and by o on the leader")
	if got := open(); isNone(got) {
		t.Fatal("the alert was not raised again")
	}
	e.mustTask(t, "DELETE", "/databases/dbskip", nil)
	deadline := time.Now().Add(30 * time.Second)
	for !isNone(open()) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if got := open(); !isNone(got) {
		t.Errorf("the alert of a dropped database is open: %s", got)
	}
}

// isNone says that the rows are empty (rowsText writes nothing or an empty list).
func isNone(rows string) bool { return rows == "" || rows == "[]" }
