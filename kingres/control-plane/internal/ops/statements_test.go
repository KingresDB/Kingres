// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"log/slog"
	"testing"
)

// A database that kingresd makes carries pg_stat_statements, next to the storage extensions; a package that lacks it still makes the database.
func TestInstallAnalyticsInstallsStatStatements(t *testing.T) {
	fe := newFakeExt()
	fe.available[ExtStatStatements] = "1.12"
	o := &Ops{ServerHost: "s1", extBackendOverride: fe, Log: slog.New(slog.DiscardHandler)}
	if err := o.installAnalytics(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{ExtColumnar, ExtTimepart, ExtStatStatements} {
		if fe.installed[e] == "" {
			t.Errorf("%s was not installed: %v", e, fe.installed)
		}
	}
	if fe.installed[ExtSketch] != "" {
		t.Error("kingres_sketch is installed by its own task, not when a database is made")
	}
	// the extension is the one of the closed list that the other servers install on request
	if !analyticsExtensions[ExtStatStatements] {
		t.Error("pg_stat_statements is not in the extensions that the internal API installs")
	}
}

func TestInstallAnalyticsWithoutStatStatementsInThePackage(t *testing.T) {
	fe := newFakeExt() // the package has no pg_stat_statements
	o := &Ops{ServerHost: "s1", extBackendOverride: fe, Log: slog.New(slog.DiscardHandler)}
	if err := o.installAnalytics(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	if fe.installed[ExtStatStatements] != "" || fe.installed[ExtColumnar] == "" {
		t.Errorf("installed %v", fe.installed)
	}
}
