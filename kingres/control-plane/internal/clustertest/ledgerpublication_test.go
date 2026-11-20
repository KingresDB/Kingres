// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// The collector on real servers (design ledger-phase7.md, steps 5 and 6): two servers are two shard groups, each with a ledger table of its own and
// so a chain of its own. The leader reads both, publishes a chain log for each and the aggregate of the database, serves the files over the
// listener of each server (the follower's through the leader), and the standalone verifier accepts what was published.

type pubCluster struct {
	c     *clustertest.Cluster
	ports []int
}

func (p *pubCluster) base(i int) string {
	return fmt.Sprintf("http://%s:%d", p.c.Nodes[i].Host, p.ports[i])
}

func formPublishing(t *testing.T, dbs ...string) *pubCluster {
	t.Helper()
	p := &pubCluster{ports: make([]int, 2)}
	p.c = clustertest.FormWithNodeExtra(t, 2, func(nd *clustertest.Node) []string {
		p.ports[nd.Index] = pgtest.FreePort(t)
		t.Cleanup(func() { pgtest.ReleasePorts(p.ports[nd.Index]) })
		return []string{"-ledger-publication-listen", fmt.Sprintf("%s:%d", nd.Host, p.ports[nd.Index]), "-monitor-interval", "3s"}
	}, func(leader *clustertest.Node) {
		for _, db := range dbs {
			mkDBLedger(t, leader, db)
		}
	})
	return p
}

// startPublishing makes the first signing key, turns the publication on with the shortest interval, and waits for the scheduler of the leader.
func (p *pubCluster) startPublishing(t *testing.T) {
	t.Helper()
	leader := p.c.Nodes[0]
	if done := startKeyTask(t, leader, "/ledger/signing/keys", nil); done["state"] != "done" {
		t.Fatalf("the first key: %v", done)
	}
	until(t, 60*time.Second, "the settings to be accepted", func() bool {
		st, b := leader.Do("PUT", "/ledger/settings", map[string]any{"publication": map[string]any{"enabled": true, "intervalSeconds": 10}}, nil)
		if st != 200 {
			t.Logf("settings: %d %v", st, b)
		}
		return st == 200
	})
}

// ledgerTable makes a table with a ledger on the group of the node (through the service role of that node's own instance, which is the
// primary of its group) and inserts n rows.
func ledgerTable(t *testing.T, n *clustertest.Node, db, table string, rows int) {
	t.Helper()
	ctx := context.Background()
	c := n.ServicePG(db)
	defer func() { _ = c.Close(ctx) }()
	for _, q := range []string{fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v text)", table), fmt.Sprintf("SELECT kingres.ledger_enable('public.%s'::regclass, 'append_only')", table)} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	insertRows(t, n, db, table, 0, rows)
}

// insertRows adds rows, each in a transaction of its own followed by a block.
func insertRows(t *testing.T, n *clustertest.Node, db, table string, from, count int) {
	t.Helper()
	ctx := context.Background()
	c := n.ServicePG(db)
	defer func() { _ = c.Close(ctx) }()
	for i := from; i < from+count; i++ {
		if _, err := c.Exec(ctx, fmt.Sprintf("INSERT INTO %s VALUES ($1, 'x')", table), i); err != nil {
			t.Fatal(err)
		}
		// close the block now (the block builder of the extension does it every ten seconds): one block for each row, so that the head the
		// collector reads moves by the number of rows
		if _, err := c.Exec(ctx, "SELECT kingres.ledger_generate_digest()"); err != nil {
			t.Fatal(err)
		}
	}
}

type pubLog struct {
	Kind, Origin, State string
	Size                int64
}

func publicationLogs(t *testing.T, n *clustertest.Node) []pubLog {
	t.Helper()
	body := n.Expect(200, "GET", "/ledger/publication", nil)
	var out []pubLog
	for _, l := range anyList(body["logs"]) {
		m := asMap(l)
		out = append(out, pubLog{Kind: m["kind"].(string), Origin: m["origin"].(string), State: m["state"].(string), Size: int64(m["size"].(float64))})
	}
	return out
}

func logOf(logs []pubLog, kind string, n int) (pubLog, bool) {
	i := 0
	for _, l := range logs {
		if l.Kind == kind {
			if i == n {
				return l, true
			}
			i++
		}
	}
	return pubLog{}, false
}

// logPath is the part of an origin below the prefix: c/<chain>/e<epoch> or a/<id>.
var logPathRE = regexp.MustCompile(`/((?:c/[0-9a-f]{64}/e[0-9]+)|(?:a/[0-9a-f]{64}))$`)

func logPath(origin string) string {
	m := logPathRE.FindStringSubmatch(origin)
	if m == nil {
		return ""
	}
	return m[1]
}

func httpGet(t *testing.T, url string) (int, []byte) {
	t.Helper()
	// A 503 is the listener's own "try again" (the limit of requests, or a store that is busy just after a restart): a reader tries again.
	for attempt := 0; ; attempt++ {
		resp, err := (&http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || attempt >= 20 {
			return resp.StatusCode, b
		}
		time.Sleep(time.Second)
	}
}

// download copies the files a reader of the latest checkpoint of the log may ask for, over HTTP, into dir (the layout of a publication).
func download(t *testing.T, base, lp, dir string) (size uint64, note string) {
	t.Helper()
	st, cp := httpGet(t, base+"/"+lp+"/checkpoint")
	if st != 200 {
		t.Fatalf("checkpoint of %s: %d %s", lp, st, cp)
	}
	n, err := ledgerlog.ParseNote(cp)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ledgerlog.ParseCheckpoint(n.Text)
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel string, data []byte) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(lp+"/checkpoint", cp)
	keys, err := ledgerlog.Required(c.Size)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		st, d := httpGet(t, base+"/"+lp+"/"+k.Path())
		if st != 200 {
			t.Fatalf("%s/%s: %d", lp, k.Path(), st)
		}
		write(lp+"/"+k.Path(), d)
	}
	return c.Size, string(cp)
}

func verifierBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("KINGRES_VERIFY_BIN")
	if bin == "" {
		t.Skip("KINGRES_VERIFY_BIN does not name a built kingres-verify")
	}
	return bin
}

// verifyPublication runs the verifier's checkpoint command on a log of the downloaded publication.
func verifyPublication(t *testing.T, p *pubCluster, node int, lp string, minSize uint64) {
	t.Helper()
	bin := verifierBinary(t)
	// KINGRES_PUBLICATION_FIXTURES names a directory that keeps what the verifier read: the layout of the publication of a real two-group
	// database and its trust.json, which the verifier tests of step 7 use as fixtures.
	dir := os.Getenv("KINGRES_PUBLICATION_FIXTURES")
	if !strings.HasPrefix(t.Name(), "TestLedgerPublicationOfADatabaseOnTwoGroups") {
		dir = "" // only the test of the fixture writes into it (the logs of the kill tests are other logs)
	}
	if dir == "" {
		dir = t.TempDir()
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	size, _ := download(t, p.base(node), lp, dir)
	if size < minSize {
		t.Fatalf("%s has %d entries, expected at least %d", lp, size, minSize)
	}
	tf := filepath.Join(dir, "trust.json")
	st, raw := rawGet(t, p.c.Nodes[0], "/ledger/signing/trust-file")
	if st != 200 {
		t.Fatalf("trust file: %d", st)
	}
	if err := os.WriteFile(tf, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	flag := ""
	parts := strings.Split(lp, "/")
	if parts[0] == "c" {
		flag = "chain:" + parts[1] + ":" + parts[2]
	} else {
		flag = "aggregate:" + parts[1]
	}
	out, err := exec.Command(bin, "checkpoint", "--trust", tf, "--source", dir, "--log", flag, "--now", time.Now().UTC().Format(time.RFC3339), //nolint:gosec // the verifier under test
		"--min-size", fmt.Sprint(minSize)).CombinedOutput()
	if err != nil {
		t.Fatalf("kingres-verify checkpoint %s: %v\n%s", flag, err, out)
	}
	if !strings.Contains(string(out), "VERIFIED") {
		t.Fatalf("kingres-verify checkpoint %s did not say VERIFIED:\n%s", flag, out)
	}
}

func rawGet(t *testing.T, n *clustertest.Node, path string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", n.Base()+path, nil)
	req.Header.Set("Authorization", "Bearer "+n.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// pgTiles reads the tiles of a log from the system database (the test's own view of what is stored).
type pgTiles struct {
	c  *pgx.Conn
	id int64
}

func (g pgTiles) GetTile(ctx context.Context, k ledgerlog.TileKey) ([]byte, error) {
	var d []byte
	err := g.c.QueryRow(ctx, `SELECT data FROM kingres_cp.ledger_tile WHERE log_id = $1 AND level = $2 AND idx = $3 AND width = $4`, g.id, k.Level, int64(k.Index), k.Width).Scan(&d) //nolint:gosec // a small index
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ledgerlog.ErrNotFound
	}
	return d, err
}

// auditStored audits every log that is not stopped against its latest stored checkpoint: every tile and bundle, against the root.
func auditStored(t *testing.T, n *clustertest.Node) {
	t.Helper()
	ctx := context.Background()
	c := n.ServicePG("kingres_system")
	defer func() { _ = c.Close(ctx) }()
	rows, err := c.Query(ctx, `SELECT l.log_id, l.origin, k.size, k.root FROM kingres_cp.ledger_log l JOIN LATERAL
		(SELECT size, root FROM kingres_cp.ledger_checkpoint WHERE log_id = l.log_id ORDER BY size DESC LIMIT 1) k ON true WHERE l.state <> 'diverged'`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id     int64
		origin string
		size   int64
		root   []byte
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.origin, &r.size, &r.root); err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	rows.Close()
	if len(rs) == 0 {
		t.Fatal("no log with a checkpoint")
	}
	for _, r := range rs {
		var root ledgerlog.Hash
		copy(root[:], r.root)
		if err := ledgerlog.Audit(ctx, pgTiles{c, r.id}, uint64(r.size), root); err != nil { //nolint:gosec // a stored size
			t.Errorf("audit of %s at %d: %v", r.origin, r.size, err)
		}
	}
}

func TestLedgerPublicationOfADatabaseOnTwoGroups(t *testing.T) {
	p := formPublishing(t, "pub_db")
	c := p.c
	leader, follower := c.Nodes[0], c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	ledgerTable(t, leader, "pub_db", "t0", 5)
	ledgerTable(t, follower, "pub_db", "t1", 3)
	p.startPublishing(t)

	// a digest asked of the FOLLOWER (forwarded to the leader): both groups, complete, with the aggregate
	d := follower.Expect(201, "POST", "/databases/pub_db/ledger/digests", nil)
	chains := anyList(d["chains"])
	if len(chains) != 2 || d["complete"] != true || len(anyList(d["uncoveredShardGroupIds"])) != 0 {
		t.Fatalf("digest: %v", d)
	}
	groups := map[float64]bool{}
	for _, ch := range chains {
		m := asMap(ch)
		groups[m["shardGroupId"].(float64)] = true
		if m["document"] == nil || m["blockHash"] == nil {
			t.Errorf("chain %v", m)
		}
	}
	if len(groups) != 2 {
		t.Errorf("the chains are of groups %v", groups)
	}
	if d["aggregateHash"] == nil || d["aggregateSize"] == nil || d["aggregateSize"].(float64) < 1 {
		t.Fatalf("no aggregate in the digest: %v", d)
	}
	logs := publicationLogs(t, follower)
	agg, ok := logOf(logs, "aggregate", 0)
	if !ok || agg.Size < 1 {
		t.Fatalf("logs %v", logs)
	}
	ch0, _ := logOf(logs, "chain", 0)
	ch1, ok1 := logOf(logs, "chain", 1)
	if !ok1 || ch0.Size < 1 || ch1.Size < 1 || strings.Contains(ch0.Origin+ch1.Origin+agg.Origin, "pub_db") {
		t.Fatalf("logs %v", logs)
	}
	// the aggregate through the API
	a := follower.Expect(200, "GET", "/databases/pub_db/ledger/aggregate", nil)
	if len(anyList(a["members"])) != 2 || a["complete"] != true || !strings.Contains(a["statement"].(string), "global snapshot") {
		t.Fatalf("aggregate %v", a)
	}
	if got := a["root"].(string); got != d["aggregateHash"] && int64(a["size"].(float64)) == int64(d["aggregateSize"].(float64)) {
		t.Errorf("the digest's aggregate hash %v is not the root %v of the aggregate of that size", d["aggregateHash"], got)
	}
	list := leader.Expect(200, "GET", "/databases/pub_db/ledger/aggregates?limit=1", nil)
	if len(anyList(list["items"])) != 1 {
		t.Errorf("aggregates %v", list)
	}
	if st, _ := leader.Do("GET", "/databases/pub_db/ledger/aggregate?size=999999", nil, nil); st != 404 {
		t.Errorf("a size that was not published: %d", st)
	}

	// the files over the listener of each server: the leader's reads its store, the follower's asks the leader; they agree
	for _, l := range []pubLog{ch0, ch1, agg} {
		lp := logPath(l.Origin)
		if lp == "" {
			t.Fatalf("origin %q", l.Origin)
		}
		st0, b0 := httpGet(t, p.base(0)+"/"+lp+"/checkpoint")
		st1, b1 := httpGet(t, p.base(1)+"/"+lp+"/checkpoint")
		if st0 != 200 || st1 != 200 || string(b0) != string(b1) || !strings.HasPrefix(string(b0), l.Origin+"\n") {
			t.Errorf("%s: %d %d\n%s\n%s", lp, st0, st1, b0, b1)
		}
		if st, _ := httpGet(t, p.base(1)+"/"+lp+"/tile/9/000"); st != 404 {
			t.Errorf("a tile that does not exist: %d", st)
		}
	}
	if st, _ := httpGet(t, p.base(0)+"/c/"+strings.Repeat("0", 64)+"/e1/checkpoint"); st != 404 {
		t.Errorf("an unknown chain: %d", st)
	}
	if st, body := httpGet(t, p.base(1)+"/keyring.json"); st != 200 || !strings.Contains(string(body), "format") {
		t.Errorf("keyring: %d %s", st, body)
	}
	if st, _ := httpGet(t, p.base(0)+"/events/000000"); st != 200 {
		t.Errorf("event 0: %d", st)
	}

	// the standalone verifier accepts every log, read over the follower's listener
	for _, l := range []pubLog{ch0, ch1, agg} {
		verifyPublication(t, p, 1, logPath(l.Origin), uint64(l.Size))
	}
	auditStored(t, leader)

	// one record of the WORM kind against a fake store, then the whole publication of this first moment (item 2 of the round)
	s3 := newFakeS3()
	defer s3.close()
	anchorOne(t, p, "pub_db", s3, 1)
	savePublication(t, p, 1, fixtureDir(t, "first"), "pub_db")

	// the chain of group 0 grows: its log and the aggregate follow at the next round, and the verifier accepts the larger log
	insertRows(t, leader, "pub_db", "t0", 100, 300)
	until(t, 2*time.Minute, "the chain logs and then the aggregate to grow", func() bool {
		ls := publicationLogs(t, leader)
		l, _ := logOf(ls, "chain", 0)
		l1, _ := logOf(ls, "chain", 1)
		a2, _ := logOf(ls, "aggregate", 0)
		// the aggregate follows the chain logs in the same round: wait for it too
		return l.Size+l1.Size > ch0.Size+ch1.Size+100 && a2.Size > agg.Size
	})
	logs = publicationLogs(t, leader)
	for i, kind := range []string{"chain", "chain", "aggregate"} {
		n := i
		if kind == "aggregate" {
			n = 0
		}
		l, _ := logOf(logs, kind, n)
		verifyPublication(t, p, 0, logPath(l.Origin), uint64(l.Size))
	}
	if agg2, _ := logOf(logs, "aggregate", 0); agg2.Size <= agg.Size {
		t.Errorf("the aggregate did not grow: %d after %d", agg2.Size, agg.Size)
	}
	auditStored(t, leader)

	// the whole publication of this moment is kept (events, key ring, logs.json, every log, the evidence of an anchored record): the second of two
	// successive states, for the verifier's fixture
	anchorOne(t, p, "pub_db", s3, 2)
	savePublication(t, p, 0, fixtureDir(t, "second"), "pub_db")

	// the follower's group becomes unreadable: the digest is incomplete and says which group, the aggregate flags the member stale; then it is read again
	follower.Isolate(leader)
	leader.Isolate(follower)
	var d2 map[string]any
	until(t, 60*time.Second, "the cut to be in force (a digest that is incomplete)", func() bool {
		st, b := leader.Do("POST", "/databases/pub_db/ledger/digests", nil, nil)
		d2 = b
		return st == 201 && b["complete"] == false
	})
	if d2["complete"] != false || len(anyList(d2["uncoveredShardGroupIds"])) != 1 || len(anyList(d2["chains"])) != 1 {
		t.Fatalf("the digest with a group cut off: %v", d2)
	}
	a2 := leader.Expect(200, "GET", "/databases/pub_db/ledger/aggregate", nil)
	var stale int
	for _, m := range anyList(a2["members"]) {
		if f := anyList(asMap(m)["flags"]); len(f) == 1 && f[0] == "stale" {
			stale++
		}
	}
	if a2["complete"] != false || stale != 1 {
		t.Errorf("aggregate with a group cut off: %v", a2)
	}
	follower.Heal()
	leader.Heal()
	until(t, 60*time.Second, "the cut off group to be read again", func() bool {
		st, d3 := leader.Do("POST", "/databases/pub_db/ledger/digests", nil, nil)
		return st == 201 && d3["complete"] == true
	})
}

// A kill -9 of the leader at a write of the round: the next process repairs it, and what is published is whole and verified by the verifier.
func TestLedgerRoundSurvivesAKillAtEachKindOfWrite(t *testing.T) {
	p := formPublishing(t, "kill_db")
	c := p.c
	leader, follower := c.Nodes[0], c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	ledgerTable(t, leader, "kill_db", "t0", 2)
	ledgerTable(t, follower, "kill_db", "t1", 2)
	p.startPublishing(t)
	until(t, 2*time.Minute, "the first publication", func() bool {
		_, ok := logOf(publicationLogs(t, leader), "aggregate", 0)
		return ok
	})
	next := 10
	for _, kind := range []string{"tile", "frontier", "checkpoint", "pending"} {
		before := publicationLogs(t, leader)
		leader.SetFaults(fault.Spec{LedgerExitKind: kind, LedgerExitSkip: 0})
		insertRows(t, leader, "kill_db", "t0", next, 20)
		insertRows(t, follower, "kill_db", "t1", next, 20)
		next += 20
		if !leader.WaitExit(3 * time.Minute) {
			t.Fatalf("the leader was not killed at a write of kind %s in three minutes; publication %v; log tail: %s", kind, publicationLogs(t, leader), leader.LogTail(6000))
		}
		leader.SetFaults(fault.Spec{})
		os.Remove(leader.FaultDir() + "/" + fault.FileName)
		leader.Start()
		until(t, 3*time.Minute, "the restarted leader accepts a sign-in", func() bool { return leader.Login(clustertest.AdminUser, clustertest.AdminPass) })
		until(t, 3*time.Minute, "the leader to answer again", func() bool {
			st, _ := leader.Do("GET", "/ledger/publication", nil, nil)
			return st == 200
		})
		until(t, 4*time.Minute, "the publication to catch up after the kill at "+kind, func() bool {
			now := publicationLogs(t, leader)
			var a, b int64
			for _, l := range before {
				a += l.Size
			}
			for _, l := range now {
				b += l.Size
			}
			return len(now) == len(before) && b > a
		})
		for _, l := range publicationLogs(t, leader) {
			if l.State != "active" {
				t.Fatalf("after a kill at a write of kind %s the log %s is %s", kind, l.Origin, l.State)
			}
		}
		auditStored(t, leader)
	}
	logs := publicationLogs(t, leader)
	for _, l := range logs {
		verifyPublication(t, p, 0, logPath(l.Origin), uint64(l.Size))
	}
}

func TestLedgerPublicationEndsWhenADatabaseIsDroppedAndWhenItVanishes(t *testing.T) {
	// the daemons read the time a database must be missing before its publication ends; the default is 30 minutes (P8-03)
	t.Setenv("KINGRES_LEDGER_MISSING_FOR", "20s")
	p := formPublishing(t, "drop_db", "vanish_db")
	c := p.c
	leader, follower := c.Nodes[0], c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	for _, db := range []string{"drop_db", "vanish_db"} {
		ledgerTable(t, leader, db, "t0", 3)
		ledgerTable(t, follower, db, "t1", 3)
	}
	p.startPublishing(t)
	for _, db := range []string{"drop_db", "vanish_db"} {
		d := leader.Expect(201, "POST", "/databases/"+db+"/ledger/digests", nil)
		if d["complete"] != true || len(anyList(d["chains"])) != 2 {
			t.Fatalf("%s: %v", db, d)
		}
	}
	logsBefore := publicationLogs(t, leader)
	if len(logsBefore) != 6 {
		t.Fatalf("logs %v", logsBefore)
	}

	// dropDatabase: the final round, the events, the retired leaf; the old files stay served
	task := follower.Expect(202, "DELETE", "/databases/drop_db", nil)
	done := follower.WaitTaskDone(clustertest.TaskID(task), 4*time.Minute)
	if done["state"] != "done" {
		t.Fatalf("drop: %v", done)
	}
	a := leader.Expect(200, "GET", "/databases/drop_db/ledger/aggregate", nil)
	retired := 0
	for _, m := range anyList(a["members"]) {
		if f := anyList(asMap(m)["flags"]); len(f) == 1 && f[0] == "retired" {
			retired++
		}
	}
	if retired != 2 || len(anyList(a["members"])) != 2 {
		t.Fatalf("the last aggregate of the dropped database: %v", a)
	}
	events := ledgerEvents(t, leader)
	if n := countContaining(events, "reason: dropped"); n != 2 {
		t.Errorf("%d chain-retired events with the reason dropped: %v", n, events)
	}
	closed := 0
	for _, l := range publicationLogs(t, leader) {
		if l.Kind == "chain" && l.State == "closed" {
			closed++
		}
	}
	if closed != 2 {
		t.Errorf("%d closed chain logs", closed)
	}
	for _, l := range logsBefore {
		if st, _ := httpGet(t, p.base(1)+"/"+logPath(l.Origin)+"/checkpoint"); st != 200 {
			t.Errorf("the checkpoint of %s is no longer served: %d", l.Origin, st)
		}
	}

	// vanish_db is dropped behind the control plane's back, from SQL on both groups: it ends with an event that says so, not as a divergence
	ctx := context.Background()
	for _, n := range []*clustertest.Node{leader, follower} {
		pg := n.ServicePG("postgres")
		if _, err := pg.Exec(ctx, "DROP DATABASE vanish_db WITH (FORCE)"); err != nil {
			t.Fatal(err)
		}
		_ = pg.Close(ctx)
	}
	until(t, 3*time.Minute, "the publication of the vanished database to end", func() bool {
		return countContaining(ledgerEvents(t, leader), "reason: database-missing") == 2
	})
	for _, l := range publicationLogs(t, leader) {
		if l.Kind == "chain" && l.State == "diverged" {
			t.Errorf("%s is diverged", l.Origin)
		}
	}
	al := leader.Expect(200, "GET", "/alerts", nil)
	for _, it := range anyList(al["items"]) {
		if asMap(it)["kind"] == "ledgerChainDiverged" {
			t.Errorf("a vanished database raised %v", it)
		}
	}
	// the leaf that flags the members retired follows the events by a moment
	var ab map[string]any
	until(t, time.Minute, "the aggregate of the vanished database to say retired", func() bool {
		ab = leader.Expect(200, "GET", "/databases/vanish_db/ledger/aggregate", nil)
		n := 0
		for _, m := range anyList(ab["members"]) {
			if f := anyList(asMap(m)["flags"]); len(f) > 0 && f[len(f)-1] == "retired" {
				n++
			}
		}
		return n == 2
	})
	retired = 0
	for _, m := range anyList(ab["members"]) {
		if f := anyList(asMap(m)["flags"]); len(f) > 0 && f[len(f)-1] == "retired" {
			retired++
		}
	}
	if retired != 2 {
		t.Errorf("the aggregate of the vanished database: %v", ab)
	}
}

func ledgerEvents(t *testing.T, n *clustertest.Node) []string {
	t.Helper()
	ctx := context.Background()
	c := n.ServicePG("kingres_system")
	defer func() { _ = c.Close(ctx) }()
	rows, err := c.Query(ctx, `SELECT convert_from(note, 'UTF8') FROM kingres_cp.ledger_event ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func countContaining(list []string, sub string) int {
	n := 0
	for _, s := range list {
		if strings.Contains(s, sub) && strings.Contains(s, "type: chain-retired") {
			n++
		}
	}
	return n
}

var _ = hex.EncodeToString

// A group that loses the tail of its chain (an older backup restored, or blocks lost at an asynchronous failover) is a divergence: its log stops with
// the old head, nothing more is signed for it, the alert is critical, its member in the aggregate says diverged with the last good head, and the other
// group goes on. The tail is cut from the real tables of the extension, with its triggers off, as an attacker with catalog access would.
func TestLedgerPublicationStopsWhenAGroupLosesItsTail(t *testing.T) {
	p := formPublishing(t, "div_db")
	c := p.c
	leader, follower := c.Nodes[0], c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	ledgerTable(t, leader, "div_db", "t0", 4)
	ledgerTable(t, follower, "div_db", "t1", 4)
	p.startPublishing(t)
	insertRows(t, follower, "div_db", "t1", 10, 12)
	d := leader.Expect(201, "POST", "/databases/div_db/ledger/digests", nil)
	if d["complete"] != true {
		t.Fatalf("%v", d)
	}
	before := publicationLogs(t, leader)
	var group1 pubLog
	for _, l := range before {
		if l.Kind == "chain" && l.Size > 12 {
			group1 = l
		}
	}
	if group1.Origin == "" {
		t.Fatalf("no chain with more than 12 blocks: %v", before)
	}
	pubBefore := func() string {
		st, b := httpGet(t, p.base(0)+"/"+logPath(group1.Origin)+"/checkpoint")
		return fmt.Sprint(st) + string(b)
	}
	cpBefore := pubBefore()

	ctx := context.Background()
	sc := follower.ServicePG("div_db")
	defer func() { _ = sc.Close(ctx) }()
	for _, q := range []string{
		"SET kingres_ledger.allow_manual_write = on",
		"UPDATE pg_trigger SET tgenabled = 'D' WHERE tgname IN ('kingres_ledger_capture', 'kingres_ledger_block_modify', 'kingres_ledger_block_truncate')",
		"DELETE FROM kingres.ledger_digest WHERE height > 5",
		"DELETE FROM kingres.ledger_tx WHERE block_height > 5",
		"DELETE FROM kingres.ledger_blocks WHERE height > 5",
		"UPDATE pg_trigger SET tgenabled = 'A' WHERE tgname IN ('kingres_ledger_capture', 'kingres_ledger_block_modify', 'kingres_ledger_block_truncate')",
	} {
		if _, err := sc.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	insertRows(t, leader, "div_db", "t0", 100, 3) // the other group goes on
	until(t, 3*time.Minute, "the log of the group that lost its tail to be stopped", func() bool {
		for _, l := range publicationLogs(t, leader) {
			if l.Origin == group1.Origin {
				return l.State == "diverged"
			}
		}
		return false
	})
	for _, l := range publicationLogs(t, leader) {
		if l.Origin == group1.Origin && l.Size != group1.Size {
			t.Errorf("the size of the diverged log changed from %d to %d", group1.Size, l.Size)
		}
	}
	if got := pubBefore(); got != cpBefore {
		t.Errorf("the checkpoint of the diverged log changed:\n%s\n%s", cpBefore, got)
	}
	until(t, 60*time.Second, "the alert ledgerChainDiverged", func() bool {
		al := leader.Expect(200, "GET", "/alerts", nil)
		for _, it := range anyList(al["items"]) {
			if m := asMap(it); m["kind"] == "ledgerChainDiverged" && m["severity"] == "critical" {
				return true
			}
		}
		return false
	})
	until(t, 60*time.Second, "the aggregate to say diverged", func() bool {
		a := leader.Expect(200, "GET", "/databases/div_db/ledger/aggregate", nil)
		for _, m := range anyList(a["members"]) {
			for _, f := range anyList(asMap(m)["flags"]) {
				if f == "diverged" {
					return a["complete"] == false
				}
			}
		}
		return false
	})
	// the other group's log kept growing
	var grown bool
	for _, l := range publicationLogs(t, leader) {
		if l.Kind == "chain" && l.Origin != group1.Origin && l.State == "active" && l.Size > 4 {
			grown = true
		}
	}
	if !grown {
		t.Errorf("the other chain did not go on: %v", publicationLogs(t, leader))
	}
	auditStored(t, leader)
	verifyPublication(t, p, 1, logPath(group1.Origin), uint64(group1.Size))
}
