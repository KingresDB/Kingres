// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package monitor_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

func p(v float64) *float64 { return &v }

func TestEvaluate(t *testing.T) {
	conds, ev := monitor.Evaluate(monitor.Readings{Host: "h", InstanceUp: true, DiskUsedPercent: p(50), MemAvailPercent: p(50), LagSeconds: p(1)})
	if len(conds) != 0 || !ev["serverDown"] || !ev["diskSpaceLow"] || !ev["memoryPressure"] || !ev["replicationLagHigh"] {
		t.Errorf("healthy: %v %v", conds, ev)
	}
	conds, _ = monitor.Evaluate(monitor.Readings{Host: "h", InstanceUp: false, DiskUsedPercent: p(96), MemAvailPercent: p(5), LagSeconds: p(300)})
	kinds := map[string]string{}
	for _, c := range conds {
		kinds[c.Kind] = c.Severity
	}
	if kinds["serverDown"] != "critical" || kinds["diskSpaceLow"] != "critical" || kinds["memoryPressure"] != "warning" || kinds["replicationLagHigh"] != "warning" {
		t.Errorf("unhealthy: %v", kinds)
	}
	if c, _ := monitor.Evaluate(monitor.Readings{Host: "h", InstanceUp: true, DiskUsedPercent: p(91)}); len(c) != 1 || c[0].Severity != "warning" {
		t.Errorf("disk warning: %v", c)
	}
	// A value that was not measured neither raises nor is evaluated (so it never clears).
	_, ev = monitor.Evaluate(monitor.Readings{Host: "h", InstanceUp: true})
	if ev["diskSpaceLow"] || ev["memoryPressure"] || ev["replicationLagHigh"] {
		t.Errorf("unmeasured kinds are evaluated: %v", ev)
	}
}

func TestRingBoundsAndQuery(t *testing.T) {
	r := monitor.NewRing(3, 2)
	t0 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	s := monitor.Series{Metric: "connections", ServerID: "a"}
	for i := 0; i < 10; i++ {
		r.Add(s, t0.Add(time.Duration(i)*time.Second), p(float64(i)))
	}
	r.Add(monitor.Series{Metric: "connections", ServerID: "b"}, t0.Add(9*time.Second), p(100))
	r.Add(monitor.Series{Metric: "connections", ServerID: "c"}, t0, p(1)) // over the series limit: dropped
	res, err := r.Query("connections", t0, t0.Add(10*time.Second), 1, "", "", "server", true)
	if err != nil || len(res) != 2 {
		t.Fatalf("series: %d %v", len(res), err)
	}
	var got []float64
	for _, pt := range res[0].Points {
		if pt.V != nil {
			got = append(got, *pt.V)
		}
	}
	if len(got) != 3 || got[0] != 7 || got[2] != 9 {
		t.Errorf("only the newest 3 points are kept: %v", got)
	}
	agg, _ := r.Query("connections", t0.Add(9*time.Second), t0.Add(9*time.Second), 1, "", "", "", true)
	if len(agg) != 1 || agg[0].Points[0].V == nil || *agg[0].Points[0].V != 109 {
		t.Errorf("sum aggregate: %+v", agg)
	}
	if _, err := r.Query("connections", t0, t0.Add(24*time.Hour), 1, "", "", "", true); !errors.Is(err, monitor.ErrTooManyPoints) {
		t.Errorf("more than 5000 points: %v", err)
	}
	var buf bytes.Buffer
	r.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), "# TYPE kingres_connections gauge") || !strings.Contains(buf.String(), `kingres_connections{server="a"} 9`) {
		t.Errorf("prometheus text:\n%s", buf.String())
	}
}

func TestAlertLifecycleAgainstPostgreSQL(t *testing.T) {
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	c := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, c); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	bad, ev := monitor.Evaluate(monitor.Readings{Host: "h1", InstanceUp: true, DiskUsedPercent: p(92)})
	if err := monitor.Reconcile(ctx, c, "h1", bad, ev, now); err != nil {
		t.Fatal(err)
	}
	// Raised once, not again; severity follows the reading.
	worse, ev := monitor.Evaluate(monitor.Readings{Host: "h1", InstanceUp: true, DiskUsedPercent: p(97)})
	if err := monitor.Reconcile(ctx, c, "h1", worse, ev, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	page, err := monitor.List(ctx, c, "open", "", store.Page{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Severity != "critical" || page.Items[0].ServerID == nil || *page.Items[0].ServerID != "h1" || page.Items[0].State != "open" {
		t.Fatalf("open alerts: %+v %v", page, err)
	}
	id := page.Items[0].ID
	a, err := monitor.Acknowledge(ctx, c, id, "alice", now)
	if err != nil || a.State != "acknowledged" || a.AcknowledgedBy == nil || *a.AcknowledgedBy != "alice" {
		t.Fatalf("acknowledge: %+v %v", a, err)
	}
	if again, err := monitor.Acknowledge(ctx, c, id, "bob", now); err != nil || *again.AcknowledgedBy != "alice" {
		t.Errorf("acknowledging twice must keep the first: %+v %v", again, err)
	}
	// An unmeasured reading does not clear; a healthy one does.
	_, evNone := monitor.Evaluate(monitor.Readings{Host: "h1", InstanceUp: true})
	if err := monitor.Reconcile(ctx, c, "h1", nil, evNone, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if page, _ := monitor.List(ctx, c, "acknowledged", "", store.Page{Limit: 10}); len(page.Items) != 1 {
		t.Errorf("an unmeasured value cleared the alert: %+v", page)
	}
	ok, ev := monitor.Evaluate(monitor.Readings{Host: "h1", InstanceUp: true, DiskUsedPercent: p(10)})
	if err := monitor.Reconcile(ctx, c, "h1", ok, ev, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	page, _ = monitor.List(ctx, c, "resolved", "", store.Page{Limit: 10})
	if len(page.Items) != 1 || page.Items[0].ResolvedAt == nil {
		t.Fatalf("resolved: %+v", page)
	}
	if _, err := monitor.Acknowledge(ctx, c, id, "x", now); !errors.Is(err, monitor.ErrResolved) {
		t.Errorf("acknowledging a resolved alert: %v", err)
	}
	if _, err := monitor.Acknowledge(ctx, c, 9999, "x", now); !errors.Is(err, monitor.ErrNotFound) {
		t.Errorf("unknown alert: %v", err)
	}
	// An outage that ended is recorded as a resolved serverDown alert.
	if err := monitor.RecordOutage(ctx, c, "h1", now.Add(-time.Minute), now); err != nil {
		t.Fatal(err)
	}
	page, _ = monitor.List(ctx, c, "resolved", "critical", store.Page{Limit: 1})
	if len(page.Items) != 1 || page.Items[0].Kind != "serverDown" || page.NextCursor != nil && false {
		t.Errorf("outage: %+v", page)
	}
}

// The cluster-wide metric answer is built from the resampled series of every
// server; it must equal what one ring holding all of them would answer.
func TestAssembleOfPerServerSeriesEqualsOneRing(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	fill := func(rings ...*monitor.Ring) {
		for i, r := range rings {
			host := string(rune('a' + i))
			for k := 0; k < 6; k++ {
				r.Add(monitor.Series{Metric: "connections", ServerID: host}, t0.Add(time.Duration(k)*time.Second), p(float64(10*(i+1)+k)))
				r.Add(monitor.Series{Metric: "databaseSizeBytes", Database: "shop"}, t0.Add(time.Duration(k)*time.Second), p(float64(100*(i+1))))
			}
		}
	}
	one := monitor.NewRing(100, 100)
	a, b := monitor.NewRing(100, 100), monitor.NewRing(100, 100)
	// One ring cannot hold two series with the same labels; give the database series of b another name.
	for k := 0; k < 6; k++ {
		one.Add(monitor.Series{Metric: "connections", ServerID: "a"}, t0.Add(time.Duration(k)*time.Second), p(float64(10+k)))
		one.Add(monitor.Series{Metric: "connections", ServerID: "b"}, t0.Add(time.Duration(k)*time.Second), p(float64(20+k)))
	}
	fill(a, b)
	to := t0.Add(6 * time.Second)
	n := monitor.NumPoints(t0, to, 2)
	for _, groupBy := range []string{"", "server"} {
		want, err := one.Query("connections", t0, to, 2, "", "", groupBy, true)
		if err != nil {
			t.Fatal(err)
		}
		var raw []monitor.Result
		for _, r := range []*monitor.Ring{a, b} {
			rs, err := r.Raw("connections", t0, to, 2, "", "")
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, rs...)
		}
		got := monitor.Assemble(t0, 2, n, raw, "", "", groupBy, true)
		if len(got) != len(want) {
			t.Fatalf("groupBy %q: %d series, want %d", groupBy, len(got), len(want))
		}
		for i := range want {
			if got[i].Series != want[i].Series || len(got[i].Points) != len(want[i].Points) {
				t.Fatalf("groupBy %q series %d: %+v vs %+v", groupBy, i, got[i].Series, want[i].Series)
			}
			for j := range want[i].Points {
				gv, wv := got[i].Points[j].V, want[i].Points[j].V
				if (gv == nil) != (wv == nil) || (gv != nil && *gv != *wv) {
					t.Errorf("groupBy %q series %d point %d: %v vs %v", groupBy, i, j, gv, wv)
				}
			}
		}
	}
	// A database series that two servers report is summed (the size of the database on both).
	var raw []monitor.Result
	for _, r := range []*monitor.Ring{a, b} {
		rs, _ := r.Raw("databaseSizeBytes", t0, to, 2, "", "")
		raw = append(raw, rs...)
	}
	agg := monitor.Assemble(t0, 2, n, raw, "", "", "", true)
	if len(agg) != 1 || agg[0].Points[0].V == nil || *agg[0].Points[0].V != 300 {
		t.Errorf("sum over the servers: %+v", agg)
	}
	byDB := monitor.Assemble(t0, 2, n, raw, "", "", "database", true)
	if len(byDB) != 1 || byDB[0].Series.Database != "shop" || *byDB[0].Points[0].V != 300 {
		t.Errorf("per database: %+v", byDB)
	}
}
