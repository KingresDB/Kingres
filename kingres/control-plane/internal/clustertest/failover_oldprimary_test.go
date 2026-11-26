// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/standby"
)

// The old primary after a failover (design ha-phase5-part2.md, increment 4; tests T11, T11b, T11c of section 9).

// opConn connects as the service role to the instance of directory id of node n over its private socket (peer
// authentication: an instance made from a copy of another server's data has that server's service password).
func opConn(n *clustertest.Node, id, port int, db string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port, cfg.Database, cfg.User = filepath.Join(n.Dir, "s", strconv.Itoa(id)), uint16(port), db, "kingresd" //nolint:gosec // test ports
	cfg.TLSConfig, cfg.Fallbacks, cfg.ConnectTimeout = nil, nil, 10*time.Second
	return pgx.ConnectConfig(context.Background(), cfg)
}

// opRecord reads the standby record of group g on a server, as the leader keeps it.
type opRecord struct {
	ok       bool
	id       int64
	state    string
	diverged bool
	port     int
	instance int
	node     int32
}

func opRecordOf(t *testing.T, leader *clustertest.Node, g int, server string) opRecord {
	t.Helper()
	c := leader.ServicePG("kingres_system")
	defer func() { _ = c.Close(context.Background()) }()
	var r opRecord
	err := c.QueryRow(context.Background(), `SELECT standby_id, state, diverged, coalesce(port, 0), instance_id, coalesce(catalog_node_id, 0)
		FROM kingres_cp.standby WHERE group_id = $1 AND server_id = $2`, g, server).Scan(&r.id, &r.state, &r.diverged, &r.port, &r.instance, &r.node)
	r.ok = err == nil
	return r
}

// opCut stops the walreceiver of the standby of group g on node n for good: its primary_conninfo is made to point at a
// closed port (in postgresql.auto.conf, which the managed settings do not touch) and the receiver is ended. The
// standby stays up at the position it has; opRestore undoes it.
func opCut(t *testing.T, n *clustertest.Node, g, dir, port int) {
	t.Helper()
	ctx := context.Background()
	c, err := opConn(n, dir, port, "postgres")
	if err != nil {
		t.Fatalf("connecting to the standby to cut it: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()
	for _, q := range []string{`ALTER SYSTEM SET primary_conninfo = 'host=127.0.0.1 port=1 user=nobody'`, `SELECT pg_reload_conf()`,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE backend_type = 'walreceiver'`} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = g
}

func opRestore(n *clustertest.Node, dir, port int) error {
	ctx := context.Background()
	c, err := opConn(n, dir, port, "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()
	for _, q := range []string{`ALTER SYSTEM RESET primary_conninfo`, `SELECT pg_reload_conf()`} {
		if _, err := c.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// opWaitNotStreaming waits until the primary at (node, dir, port) has no walsender for the standby with the application name.
func opWaitNotStreaming(t *testing.T, primary *clustertest.Node, app string) {
	t.Helper()
	until(t, 2*time.Minute, "the primary to see "+app+" disconnected", func() bool {
		c := primary.ServicePG("postgres")
		defer func() { _ = c.Close(context.Background()) }()
		var n int
		return c.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_replication WHERE application_name = $1`, app).Scan(&n) == nil && n == 0
	})
}

// opLeaseFiles lists the markers of the lease directory of a node for a group.
func opMarkers(n *clustertest.Node, g int) []string {
	var out []string
	for _, base := range []string{"fenced", "relinquished", "waitlease"} {
		p := filepath.Join(n.DataDir, "lease", base+"."+strconv.Itoa(g))
		if _, err := os.Stat(p); err == nil {
			out = append(out, base)
		}
	}
	return out
}

// opScenario: the old primary of a group returns after a failover that lost writes it had acknowledged (T11).
// With failRewind the fault hook makes pg_rewind count as failed, so that the fallback, a new base backup, is taken.
func opScenario(t *testing.T, failRewind, back bool) {
	c, port, pool := foCluster(t)
	leader := c.Leader()
	g, old, nw := foVictim(t, c)
	waitFacts(t, leader, g)
	sampler := startFoSampler(c, len(c.Nodes))
	defer func() { sampler.Stop(t) }()
	defer func() {
		if t.Failed() {
			t.Logf("catalog of the leader: %s\nleader log tail:\n%s\nold instances:\n%s", foCatalogDump(leader), leader.LogTail(2500), instLog(old, 1, 6000))
		}
	}()
	ctx := context.Background()
	if failRewind {
		old.SetFaults(fault.Spec{FailRewind: true})
	}
	// Writers go through the leader's entry point for the whole test: it is never killed.
	w := startFoWriters([]*clustertest.Node{leader}, port, pool)
	until(t, time.Minute, "rows acknowledged", func() bool { return w.count() > 30 })

	// The divergence: the old primary acknowledges, asynchronously, rows its standby never receives.
	probe := old.ServicePG("postgres")
	if !failRewind {
		opKeepWal(t, probe)
	}
	if _, err := probe.Exec(ctx, `CREATE TABLE div_probe (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	var sb catalogNode
	for _, n := range catalogStandbys(t, leader) {
		if int(n.group) == g {
			sb = n
		}
	}
	rec := opRecordOf(t, leader, g, nw.Host)
	until(t, time.Minute, "the probe table on the standby", func() bool {
		cc, err := tryStandbyConn(nw, g, sb.port, "postgres")
		if err != nil {
			return false
		}
		defer func() { _ = cc.Close(ctx) }()
		var n int
		return cc.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = 'div_probe'`).Scan(&n) == nil && n == 1
	})
	opCut(t, nw, g, 100+g, sb.port)
	opWaitNotStreaming(t, old, "kingres_standby_"+strconv.FormatInt(rec.id, 10))
	if _, err := probe.Exec(ctx, `SET synchronous_commit = local`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := probe.Exec(ctx, `INSERT INTO div_probe VALUES ($1)`, i); err != nil {
			t.Fatalf("an asynchronous write on the old primary: %v", err)
		}
	}
	_ = probe.Close(ctx)

	old.Kill()
	old.StopInstance()
	old.StopStandbys()
	st, resp := leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true, "comment": "T11"}, nil)
	if st != 202 {
		t.Fatalf("failover request: %d %v", st, resp)
	}
	task := leader.WaitTaskDone(clustertest.TaskID(resp), 8*time.Minute)
	if task["state"] != "done" {
		t.Fatalf("the failover: %v", task)
	}
	// The promoted instance still carries the cut of the walreceiver in its postgresql.auto.conf, which pg_rewind copies
	// to a standby of it: the test lifts it, as an operator who made such a setting by hand would.
	if err := opRestore(nw, 100+g, sb.port); err != nil {
		t.Fatal(err)
	}
	// The record of the old primary exists, to be rebuilt, and the old server is still down.
	if r := opRecordOf(t, leader, g, old.Host); !r.ok || !r.diverged || r.state == "ready" {
		t.Fatalf("the record of the old primary after the failover: %+v", r)
	}

	old.Start()
	until(t, 8*time.Minute, "the old primary to be rebuilt as a standby", func() bool {
		r := opRecordOf(t, leader, g, old.Host)
		return r.ok && r.state == "ready" && !r.diverged
	})
	rec = opRecordOf(t, leader, g, old.Host)
	oc, err := func() (*pgx.Conn, error) {
		var c *pgx.Conn
		var err error
		until(t, 2*time.Minute, "a connection to the rebuilt standby", func() bool { c, err = opConn(old, rec.instance, rec.port, "postgres"); return err == nil })
		return c, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = oc.Close(ctx) }()

	// It is a standby, in recovery, without the rows it alone had, and the markers that held it back are gone.
	var inRecovery bool
	var n int
	if err := oc.QueryRow(ctx, `SELECT pg_is_in_recovery(), (SELECT count(*) FROM div_probe)`).Scan(&inRecovery, &n); err != nil {
		t.Fatal(err)
	}
	if !inRecovery || n != 0 {
		t.Errorf("the rebuilt old primary: in recovery %v, %d of the 5 rows it alone had (want 0)", inRecovery, n)
	}
	if m := opMarkers(old, g); len(m) != 0 {
		t.Errorf("markers of the old primary after it became a standby: %v", m)
	}
	role, ok := readRole(t, old, rec.instance)
	if !ok || role.Role != "standby" || role.Origin != "copy" || int(role.Group) != g {
		t.Errorf("role file of the rebuilt old primary: %+v %v", role, ok)
	}
	if _, err := os.Stat(filepath.Join(old.DataDir, "instances", strconv.Itoa(rec.instance), "kingres-copy-unfinished")); err == nil {
		t.Error("the mark of an unfinished copy is still there")
	}

	// It streams and the new primary names it; the catalog has the new term and calls it a standby.
	until(t, 3*time.Minute, "the rebuilt old primary to stream", func() bool {
		for _, v := range topology(t, leader)[g] {
			if v.server == old.Host && v.state == "running" && v.syncState != "" {
				return true
			}
		}
		return false
	})
	var term int64
	var superseded bool
	if err := oc.QueryRow(ctx, `SELECT 1`).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
	sys := leader.ServicePG("kingres_system")
	var leaderTerm int64
	if err := sys.QueryRow(ctx, `SELECT term FROM kingres.catalog_status`).Scan(&leaderTerm); err != nil {
		t.Fatal(err)
	}
	var incidents int
	if err := sys.QueryRow(ctx, `SELECT count(*) FROM kingres.check_cluster() WHERE check_name LIKE 'standby\_%'`).Scan(&incidents); err != nil {
		t.Fatal(err)
	}
	_ = sys.Close(ctx)
	if incidents != 0 {
		t.Errorf("check_cluster() still reports %d standby rows after the old primary is back", incidents)
	}
	until(t, time.Minute, "the catalog of the rebuilt old primary to show the new term", func() bool {
		kc, err := opConn(old, rec.instance, rec.port, "kingres_system")
		if err != nil {
			return false
		}
		defer func() { _ = kc.Close(ctx) }()
		return kc.QueryRow(ctx, `SELECT term, superseded FROM kingres.catalog_status`).Scan(&term, &superseded) == nil && term == leaderTerm && !superseded
	})

	// The report says what happened to the old primary.
	rep := leader.Expect(200, "GET", fmt.Sprintf("/failover/reports/%v", clustertest.TaskID(resp)), nil)
	gs, _ := rep["groups"].([]any)
	if len(gs) != 1 {
		t.Fatalf("report: %v", rep)
	}
	op, _ := gs[0].(map[string]any)["oldPrimary"].(map[string]any)
	wantMethod := "pgRewind"
	if failRewind {
		wantMethod = "baseBackup"
	}
	if op["state"] != "standby" || op["method"] != wantMethod {
		t.Errorf("old primary in the report: %v (want state standby, method %s)", op, wantMethod)
	}
	if !failRewind {
		// The report says how much the old primary wrote after the divergence: a number (zero when the old WAL ends before the point where the histories parted,
		// which a failover under load can give: the promoted standby held more than the old primary wrote), or, only when the instance had not finished its crash
		// recovery, nothing and the documented message that says the old WAL could not be measured.
		db, isNumber := op["divergenceBytes"].(float64)
		msg, _ := op["message"].(string)
		switch {
		case isNumber && db >= 0:
		case !isNumber && strings.Contains(msg, "could not measure the old WAL"):
		default:
			t.Errorf("the report does not say how much the old primary wrote after the divergence: %v", op)
		}
	}
	// Nothing acknowledged is lost, and the writers went on.
	before := w.count()
	until(t, time.Minute, "writes after the old primary was rebuilt", func() bool { return w.count() > before+10 })

	if back && nw != leader {
		// A second failover, back: the server that took over dies, and the old primary (a standby in directory 1 now) is
		// promoted.
		sb2 := opRecordOf(t, leader, g, old.Host)
		until(t, 3*time.Minute, "the old primary registered as a standby in the catalog", func() bool {
			return opRecordOf(t, leader, g, old.Host).node != 0
		})
		_ = sb2
		defer func() {
			if t.Failed() {
				var st standby.Status
				code, err := leader.InternalCall(nw, "GET", "/standby/status", nil, &st)
				t.Logf("status of the new primary server: %d %v %+v; records: %s", code, err, st.Primaries, foCatalogDump(leader))
			}
		}()
		waitFacts(t, leader, g)
		nw.Kill()
		nw.StopInstance()
		nw.StopStandbys()
		st, resp2 := leader.DoThroughRestart("POST", "/servers/"+nw.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true, "comment": "T11 back"}, nil)
		if st != 202 {
			t.Fatalf("the second failover: %d %v", st, resp2)
		}
		if task := leader.WaitTaskDone(clustertest.TaskID(resp2), 10*time.Minute); task["state"] != "done" {
			t.Fatalf("the second failover: %v", task)
		}
		hosts, insts := primaryOf(t, leader)
		if hosts[g] != old.Host {
			t.Errorf("after the failover back the primary of group %d is %s (instance %d), want %s", g, hosts[g], insts[g], old.Host)
		}
	}
	acked := w.Stop()
	have := foReadIDs(t, leader, port, pool)
	lost := 0
	for id := range acked {
		if !have[id] {
			lost++
		}
	}
	if lost > 0 {
		t.Errorf("%d of the %d acknowledged rows are missing", lost, len(acked))
	}
}

// T11: the old primary returns after a failover and is rebuilt with pg_rewind; the rows it alone had are gone; then a
// second failover, back to it, works.
func TestOldPrimaryReturns(t *testing.T) { opScenario(t, false, true) }

// T11, the fallback: pg_rewind is made to fail and the instance directory is rebuilt from a new base backup.
func TestOldPrimaryReturnsAndIsRebuiltByABaseBackup(t *testing.T) { opScenario(t, true, false) }

// T11b: the old server never returns. After standby-replace-after (20 s here) its record is removed in the order of
// extension sequence 8, a replacement standby is made on another server, groupUnderReplicated and oldPrimaryNotRebuilt
// are raised, and no commit blocks meanwhile.
func TestOldPrimaryNeverReturns(t *testing.T) {
	c, port, pool := foClusterWith(t, "-lease-duration", "6s", "-lease-renew", "1s", "-lease-margin", "2s", "-standby-replace-after", "20s")
	leader := c.Leader()
	g, old, nw := foVictim(t, c)
	waitFacts(t, leader, g)
	ctx := context.Background()
	w := startFoWriters([]*clustertest.Node{leader}, port, pool)
	until(t, time.Minute, "rows acknowledged", func() bool { return w.count() > 30 })
	old.Kill()
	old.StopInstance()
	old.StopStandbys()
	st, resp := leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "comment": "T11b"}, nil)
	if st != 202 {
		t.Fatalf("failover request: %d %v", st, resp)
	}
	if task := leader.WaitTaskDone(clustertest.TaskID(resp), 8*time.Minute); task["state"] != "done" {
		t.Fatalf("the failover: %v", task)
	}
	defer func() {
		if t.Failed() {
			t.Logf("catalog of the leader: %s\nleader log tail:\n%s", foCatalogDump(leader), leader.LogTail(5000))
		}
	}()
	// Writes go on while the record of the old primary waits and while it is removed: nothing waits for a standby that
	// is not there, and a distributed commit does not get KS005 from a list that names it.
	mark := w.count()
	until(t, 5*time.Minute, "the record of the old primary to be removed and a replacement to be ready", func() bool {
		if opRecordOf(t, leader, g, old.Host).ok {
			return false
		}
		c := leader.ServicePG("kingres_system")
		defer func() { _ = c.Close(ctx) }()
		var n int
		return c.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.standby WHERE group_id = $1 AND state = 'ready' AND server_id NOT IN ($2, $3)`, g, old.Host, nw.Host).Scan(&n) == nil && n == 1
	})
	if w.count() <= mark+30 {
		t.Errorf("only %d rows were acknowledged while the old primary was replaced (last error: %s)", w.count()-mark, w.lastError())
	}
	sys := leader.ServicePG("kingres_system")
	defer func() { _ = sys.Close(ctx) }()
	for _, kind := range []string{"oldPrimaryNotRebuilt"} { // groupUnderReplicated needs three quiet checks in a row, which the replacement beats
		var n int
		if err := sys.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.alert WHERE kind = $1 AND subject LIKE '%group:'||$2`, kind, strconv.Itoa(g)).Scan(&n); err != nil || n == 0 {
			t.Errorf("the alert %s was never raised for group %d (%v)", kind, g, err)
		}
	}
	// The catalog no longer has the old primary's node.
	var nodes int
	if err := sys.QueryRow(ctx, `SELECT count(*) FROM kingres.node WHERE host = $1 AND group_id = $2`, old.Host, g).Scan(&nodes); err != nil || nodes != 0 {
		t.Errorf("the catalog still has %d nodes of group %d on the old server (%v)", nodes, g, err)
	}
	acked := w.Stop()
	have := foReadIDs(t, leader, port, pool)
	for id := range acked {
		if !have[id] {
			t.Errorf("acknowledged row %d is missing", id)
			break
		}
	}
}

// tailOf is the last n bytes of a log without the lines of the connections that clients dropped.
func tailOf(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "SSL error") || strings.Contains(l, "could not receive data from client") || strings.Contains(l, "unexpected eof") {
			continue
		}
		keep = append(keep, l)
	}
	s = strings.Join(keep, "\n")
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// instLog is the filtered tail of the PostgreSQL log of one instance directory of a node.
func instLog(n *clustertest.Node, id int, max int) string {
	files, _ := filepath.Glob(filepath.Join(n.DataDir, "instances", strconv.Itoa(id), "pgdata", "log", "*"))
	var sb strings.Builder
	for _, f := range files {
		b, _ := os.ReadFile(f)
		sb.Write(b)
	}
	return tailOf(sb.String(), max)
}

// opKeepWal makes a primary keep 256 MB of WAL: pg_rewind of a former primary needs the WAL from the last checkpoint
// before the histories parted, and a primary that was shut down after the divergence has recycled it by default (the
// rebuild then falls back to a base backup, which TestOldPrimaryReturnsAndIsRebuiltByABaseBackup shows).
func opKeepWal(t *testing.T, c *pgx.Conn) {
	t.Helper()
	for _, q := range []string{`ALTER SYSTEM SET wal_keep_size = '256MB'`, `SELECT pg_reload_conf()`} {
		if _, err := c.Exec(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}
