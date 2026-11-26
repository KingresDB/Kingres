// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package monitor_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/gather"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

func kindsOf(conds []monitor.Condition) map[string]monitor.Condition {
	m := map[string]monitor.Condition{}
	for _, c := range conds {
		m[c.Kind+"|"+c.Subject] = c
	}
	return m
}

func first(c []monitor.Condition, _ map[string]bool) []monitor.Condition { return c }

// certificateExpiring: raised when the certificate of the server has less than
// half of the part of its lifetime at which the renewal job renews it (the job
// has not managed to), critical when it has less than an eighth or has expired;
// cleared when the certificate is younger again. The cluster CA is checked on
// the server that holds it, in absolute terms: it is not rotated.
func TestEvaluateCertificateExpiring(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	rd := func(rem float64, notAfter time.Time) monitor.Readings {
		return monitor.Readings{Host: "h1", InstanceUp: true,
			Certificates: []monitor.CertificateReading{{Remaining: rem, NotAfter: notAfter, RenewFraction: 0.3, Now: now}}}
	}
	for _, tc := range []struct {
		name string
		rem  float64
		end  time.Time
		want string // "" none
	}{
		{"fresh", 0.9, now.Add(24 * time.Hour), ""},
		{"inside the renewal window, the job is still working", 0.2, now.Add(24 * time.Hour), ""},
		{"less than half the window left", 0.14, now.Add(24 * time.Hour), "warning"},
		{"less than an eighth of the window", 0.03, now.Add(24 * time.Hour), "critical"},
		{"expired", 0, now.Add(-time.Minute), "critical"},
	} {
		conds, ev := monitor.Evaluate(rd(tc.rem, tc.end))
		if !ev["certificateExpiring"] {
			t.Errorf("%s: the kind was not evaluated", tc.name)
		}
		got := ""
		if c, ok := kindsOf(conds)["certificateExpiring|server:h1"]; ok {
			got = c.Severity
			if !strings.Contains(c.Message, "h1") || !strings.Contains(c.Message, tc.end.UTC().Format(time.RFC3339)) {
				t.Errorf("%s: message %q", tc.name, c.Message)
			}
		}
		if got != tc.want {
			t.Errorf("%s: severity %q, want %q", tc.name, got, tc.want)
		}
	}
	// The CA.
	ca := func(left time.Duration) monitor.Readings {
		return monitor.Readings{Host: "h1", InstanceUp: true,
			Certificates: []monitor.CertificateReading{{Remaining: 0.5, NotAfter: now.Add(left), RenewFraction: 0.3, CA: true, Now: now}}}
	}
	if conds, _ := monitor.Evaluate(ca(5 * 365 * 24 * time.Hour)); len(conds) != 0 {
		t.Errorf("a young CA: %v", conds)
	}
	if c := kindsOf(first(monitor.Evaluate(ca(300 * 24 * time.Hour))))["certificateExpiring|server:h1|ca"]; c.Severity != "warning" {
		t.Errorf("a CA with 300 days left: %+v", c)
	}
	if c := kindsOf(first(monitor.Evaluate(ca(20 * 24 * time.Hour))))["certificateExpiring|server:h1|ca"]; c.Severity != "critical" {
		t.Errorf("a CA with 20 days left: %+v", c)
	}
	// Not measured: neither raised nor evaluated, so never cleared.
	if _, ev := monitor.Evaluate(monitor.Readings{Host: "h1", InstanceUp: true}); ev["certificateExpiring"] || ev["ledgerPendingTooOld"] {
		t.Errorf("unmeasured kinds are evaluated: %v", ev)
	}
}

func TestPendingTrackerCountsTheAgeOfTheOldestPendingEntry(t *testing.T) {
	tr := monitor.NewPendingTracker()
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	m := func(v int64) *int64 { return &v }
	if age := tr.Observe("shop", m(100), 5, t0); age != 0 {
		t.Errorf("first sight: %v", age)
	}
	if age := tr.Observe("shop", m(100), 7, t0.Add(10*time.Minute)); age != 10*time.Minute {
		t.Errorf("the same oldest entry after 10 minutes: %v", age)
	}
	// The oldest entry went into a block: the age starts again for the next one.
	if age := tr.Observe("shop", m(140), 3, t0.Add(11*time.Minute)); age != 0 {
		t.Errorf("progress: %v", age)
	}
	if age := tr.Observe("shop", m(140), 3, t0.Add(20*time.Minute)); age != 9*time.Minute {
		t.Errorf("age after progress: %v", age)
	}
	// Nothing pending: forgotten.
	if age := tr.Observe("shop", nil, 0, t0.Add(21*time.Minute)); age != 0 {
		t.Errorf("empty queue: %v", age)
	}
	if age := tr.Observe("shop", m(140), 1, t0.Add(30*time.Minute)); age != 0 {
		t.Errorf("an entry after the queue was empty is a new entry: %v", age)
	}
	// Databases are tracked separately.
	if age := tr.Observe("other", m(1), 1, t0.Add(31*time.Minute)); age != 0 {
		t.Errorf("another database: %v", age)
	}
}

func TestEvaluateLedgerPendingTooOld(t *testing.T) {
	rd := func(age time.Duration) monitor.Readings {
		return monitor.Readings{Host: "h1", InstanceUp: true, LedgerChecked: true,
			LedgerPending: []monitor.LedgerPendingReading{{Database: "shop", Count: 4, Age: age}, {Database: "quiet", Count: 0}}}
	}
	conds, ev := monitor.Evaluate(rd(time.Minute))
	if len(conds) != 0 || !ev["ledgerPendingTooOld"] {
		t.Errorf("young entries: %v %v", conds, ev)
	}
	c := kindsOf(first(monitor.Evaluate(rd(monitor.LedgerPendingMaxAge + time.Second))))["ledgerPendingTooOld|server:h1|database:shop"]
	if c.Severity != "warning" || !strings.Contains(c.Message, "shop") || !strings.Contains(c.Message, "4") {
		t.Errorf("old entries: %+v", c)
	}
	if c := kindsOf(first(monitor.Evaluate(rd(2 * time.Hour))))["ledgerPendingTooOld|server:h1|database:shop"]; c.Severity != "critical" {
		t.Errorf("very old entries: %+v", c)
	}
}

func TestEvaluateVersions(t *testing.T) {
	same := []monitor.ServerVersions{
		{Server: "a", KingresDB: "0.2.0", PostgreSQL: "18.7", Extensions: map[string]string{"kingres_shard": "0.4", "kingres_ledger": "0.2"}},
		{Server: "b", KingresDB: "0.2.0", PostgreSQL: "18.7", Extensions: map[string]string{"kingres_shard": "0.4", "kingres_ledger": "0.2"}},
	}
	if c := monitor.EvaluateVersions(same); len(c) != 0 {
		t.Errorf("equal versions: %v", c)
	}
	if c := monitor.EvaluateVersions(same[:1]); len(c) != 0 {
		t.Errorf("one server: %v", c)
	}
	diff := append([]monitor.ServerVersions(nil), same...)
	diff = append(diff, monitor.ServerVersions{Server: "c", KingresDB: "0.3.0", PostgreSQL: "18.7", Extensions: map[string]string{"kingres_shard": "0.5", "kingres_ledger": "0.2"}})
	conds := monitor.EvaluateVersions(diff)
	if len(conds) != 1 || conds[0].Kind != "versionMismatch" || conds[0].Subject != "cluster" || conds[0].Severity != "warning" {
		t.Fatalf("different versions: %+v", conds)
	}
	m := conds[0].Message
	for _, want := range []string{"KingresDB 0.2.0 on a, b", "0.3.0 on c", "kingres_shard", "0.5 on c"} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q: %s", want, m)
		}
	}
	if strings.Contains(m, "kingres_ledger") || strings.Contains(m, "PostgreSQL") {
		t.Errorf("message names what is equal: %s", m)
	}
}

// Alerts of one kind for several subjects of a server (a database) are raised
// separately, cleared separately and listed with the server and the database;
// the keyset listing is newest first and resumes after a position.
func TestAlertSubjectsAndKeysetListing(t *testing.T) {
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	c := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, c); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	both := []monitor.Condition{
		{Kind: "ledgerPendingTooOld", Severity: "warning", Subject: "server:h1|database:shop", Message: "shop is late"},
		{Kind: "ledgerPendingTooOld", Severity: "warning", Subject: "server:h1|database:blog", Message: "blog is late"},
	}
	ev := map[string]bool{"ledgerPendingTooOld": true}
	if err := monitor.Reconcile(ctx, c, "h1", both, ev, now); err != nil {
		t.Fatal(err)
	}
	// Conditions that belong to another server are refused.
	if err := monitor.Reconcile(ctx, c, "h2", both[:1], ev, now.Add(time.Second)); err == nil {
		t.Error("conditions that belong to another server were accepted")
	}
	// One database recovers.
	if err := monitor.Reconcile(ctx, c, "h1", both[:1], ev, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	open, err := monitor.ListAfter(ctx, c, "open", "", nil, 10, 3)
	if err != nil || len(open) != 1 {
		t.Fatalf("open: %+v %v", open, err)
	}
	a := open[0]
	if a.Kind != "ledgerPendingTooOld" || a.ServerID == nil || *a.ServerID != "h1" || a.DatabaseName == nil || *a.DatabaseName != "shop" || a.Title != "Ledger entries pending for too long" {
		t.Errorf("alert %+v", a)
	}
	ord, local := gather.SplitAlertID(a.ID)
	if ord != 3 || local <= 0 {
		t.Errorf("the id %d is not made of the ordinal 3 and the local id", a.ID)
	}
	// The cluster-wide kinds: no server.
	if err := monitor.ReconcileScope(ctx, c, "cluster", []monitor.Condition{{Kind: "versionMismatch", Severity: "warning", Subject: "cluster", Message: "versions differ"}},
		map[string]bool{"versionMismatch": true}, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	mm, _ := monitor.ListAfter(ctx, c, "open", "", nil, 10, 3)
	var vm bool
	for _, x := range mm {
		if x.Kind == "versionMismatch" {
			vm = x.ServerID == nil && x.DatabaseName == nil && x.Title == "KingresDB versions differ"
		}
	}
	if !vm {
		t.Errorf("versionMismatch alert: %+v", mm)
	}
	if err := monitor.ReconcileScope(ctx, c, "cluster", nil, map[string]bool{"versionMismatch": true}, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, x := range func() []string {
		var k []string
		l, _ := monitor.ListAfter(ctx, c, "open", "", nil, 10, 3)
		for _, y := range l {
			k = append(k, y.Kind)
		}
		return k
	}() {
		if x == "versionMismatch" {
			t.Error("versionMismatch was not cleared")
		}
	}
	// Keyset listing: newest first, resumes after a position, open and resolved.
	for i, k := range []string{"x1", "x2", "x3", "x4"} {
		if err := monitor.Reconcile(ctx, c, "h1", []monitor.Condition{{Kind: "serverSuspect", Severity: "info", Subject: "server:h1|database:" + k, Message: k}},
			map[string]bool{"serverSuspect": true}, now.Add(time.Duration(10+i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	all, err := monitor.ListAfter(ctx, c, "", "", nil, 20, 3)
	if err != nil || len(all) != 7 {
		t.Fatalf("all: %d %v", len(all), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].RaisedAt.After(all[i-1].RaisedAt) || (all[i].RaisedAt.Equal(all[i-1].RaisedAt) && all[i].ID > all[i-1].ID) {
			t.Errorf("not newest first at %d: %v %v", i, all[i-1].RaisedAt, all[i].RaisedAt)
		}
	}
	var got []int64
	var after *gather.AlertKey
	for {
		page, err := monitor.ListAfter(ctx, c, "", "", after, 3, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range page {
			got = append(got, x.ID)
		}
		if len(page) < 3 {
			break
		}
		last := page[len(page)-1]
		after = &gather.AlertKey{Time: last.RaisedAt, ID: last.ID}
	}
	if len(got) != len(all) {
		t.Fatalf("paged %d alerts, listed %d", len(got), len(all))
	}
	for i := range got {
		if got[i] != all[i].ID {
			t.Errorf("page order differs at %d", i)
		}
	}
	// Acknowledging works with the local id.
	ack, err := monitor.Acknowledge(ctx, c, local, "alice", now)
	if err != nil || ack.State != "acknowledged" {
		t.Errorf("acknowledge: %+v %v", ack, err)
	}
}

type fakeVersions struct {
	local  monitor.ServerVersions
	others map[string]*monitor.ServerVersions // nil value: the server does not answer
}

func (f *fakeVersions) Local(context.Context) (monitor.ServerVersions, error) { return f.local, nil }
func (f *fakeVersions) Others(context.Context) ([]string, error) {
	var ids []string
	for id := range f.others {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}
func (f *fakeVersions) Ask(_ context.Context, id string) (monitor.ServerVersions, error) {
	if v := f.others[id]; v != nil {
		return *v, nil
	}
	return monitor.ServerVersions{}, errors.New("no answer")
}

// versionMismatch: raised once while the versions differ (checked again, still
// one alert), not changed while fewer than two servers answer, cleared when the
// servers agree.
func TestVersionMismatchAlertLifecycle(t *testing.T) {
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	c := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, c); err != nil {
		t.Fatal(err)
	}
	v := func(server, kv, shard string) monitor.ServerVersions {
		return monitor.ServerVersions{Server: server, KingresDB: kv, PostgreSQL: "18.7", Extensions: map[string]string{"kingres_shard": shard}}
	}
	b := v("b", "0.2.0", "0.4")
	src := &fakeVersions{local: v("a", "0.2.0", "0.4"), others: map[string]*monitor.ServerVersions{"b": &b}}
	open := func() int {
		page, err := monitor.ListAfter(ctx, c, "open", "", nil, 50, 1)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, a := range page {
			if a.Kind == "versionMismatch" {
				n++
			}
		}
		return n
	}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	if err := monitor.CheckVersions(ctx, c, src, now); err != nil || open() != 0 {
		t.Fatalf("equal versions: %v, %d open", err, open())
	}
	b.Extensions["kingres_shard"] = "0.5"
	for i := 0; i < 3; i++ {
		if err := monitor.CheckVersions(ctx, c, src, now.Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if open() != 1 {
		t.Fatalf("different versions: %d open alerts, want exactly 1 (deduplicated)", open())
	}
	// The other server does not answer: nothing changes.
	src.others["b"] = nil
	if err := monitor.CheckVersions(ctx, c, src, now.Add(10*time.Minute)); err != nil || open() != 1 {
		t.Fatalf("one server alone: %v, %d open", err, open())
	}
	// The servers agree again.
	src.others["b"] = &b
	b.Extensions["kingres_shard"] = "0.4"
	if err := monitor.CheckVersions(ctx, c, src, now.Add(11*time.Minute)); err != nil || open() != 0 {
		t.Fatalf("agreement: %v, %d open", err, open())
	}
	if page, _ := monitor.ListAfter(ctx, c, "resolved", "", nil, 50, 1); len(page) != 1 || page[0].ServerID != nil {
		t.Errorf("resolved: %+v", page)
	}
}

func TestEvaluateExtensionSchemaUntrusted(t *testing.T) {
	rd := monitor.Readings{Host: "h1", InstanceUp: true, ExtensionsChecked: true, ExtensionsUntrusted: []monitor.ExtensionReading{
		{Database: "shop", Extension: "kingres_ledger", Schema: "kingres", Reason: "schemaOwner"},
		{Database: "shop", Extension: "kingres_shard", Schema: "kingres", Reason: "foreignObject", Detail: "view kingres.ledger_pending"},
		{Database: "blog", Extension: "kingres_sketch", Schema: "kingres_sketch", Reason: "schemaCreate"},
	}}
	conds, ev := monitor.Evaluate(rd)
	if !ev["extensionSchemaUntrusted"] {
		t.Fatalf("the kind is evaluated when the extensions were checked: %v", ev)
	}
	m := kindsOf(conds)
	shop := m["extensionSchemaUntrusted|server:h1|database:shop"]
	if shop.Severity != "critical" || !strings.Contains(shop.Message, "kingres_ledger") || !strings.Contains(shop.Message, "kingres_shard") || !strings.Contains(shop.Message, "view kingres.ledger_pending") {
		t.Errorf("shop: %+v", shop)
	}
	if blog := m["extensionSchemaUntrusted|server:h1|database:blog"]; blog.Severity != "critical" || !strings.Contains(blog.Message, "kingres_sketch") {
		t.Errorf("blog: %+v", blog)
	}
	if len(conds) != 2 {
		t.Errorf("one alert per database: %v", conds)
	}
	// Trusted everywhere: evaluated and empty, so an open alert clears; not checked: not evaluated, so it neither raises nor clears.
	if c, ev := monitor.Evaluate(monitor.Readings{Host: "h1", InstanceUp: true, ExtensionsChecked: true}); len(c) != 0 || !ev["extensionSchemaUntrusted"] {
		t.Errorf("trusted: %v %v", c, ev)
	}
	if _, ev := monitor.Evaluate(monitor.Readings{Host: "h1", InstanceUp: true}); ev["extensionSchemaUntrusted"] {
		t.Errorf("not checked: %v", ev)
	}
	found := false
	for _, k := range monitor.Managed {
		found = found || k == "extensionSchemaUntrusted"
	}
	if !found {
		t.Error("the kind is not in Managed")
	}
}
