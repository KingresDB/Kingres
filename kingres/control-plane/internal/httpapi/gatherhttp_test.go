// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/gather"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
)

// A small cluster in one process: every "server" is a Server of its own with its
// own log directory and metric ring; the gatherer of each calls ServeGathered of
// the others, as the internal API does after it verified the peer's certificate.
type gsrv struct {
	id      string
	ordinal int
	env     *env
	ring    *monitor.Ring
	// down: cannot be reached; slow: answers after the deadline; refuse: answers 500.
	down, slow, refuse bool
}

type gcluster struct {
	t       *testing.T
	servers []*gsrv
}

type fakeGatherer struct {
	c    *gcluster
	self *gsrv
}

func (f fakeGatherer) Self(context.Context) (gather.Peer, error) {
	return gather.Peer{ID: f.self.id, Ordinal: f.self.ordinal}, nil
}

func (f fakeGatherer) Others(context.Context) ([]gather.Peer, error) {
	var out []gather.Peer
	for _, s := range f.c.servers {
		if s != f.self {
			out = append(out, gather.Peer{ID: s.id, Ordinal: s.ordinal})
		}
	}
	return out, nil
}

func (f fakeGatherer) Ask(ctx context.Context, server string, req internalapi.ForwardRequest, _ int64) (internalapi.ForwardResponse, error) {
	for _, s := range f.c.servers {
		if s.id != server {
			continue
		}
		switch {
		case s.down:
			return internalapi.ForwardResponse{}, gather.ErrUnreachable
		case s.slow:
			<-ctx.Done()
			return internalapi.ForwardResponse{}, ctx.Err()
		case s.refuse:
			return internalapi.ForwardResponse{}, gather.ErrRefused
		}
		return s.env.srv.ServeGathered(ctx, f.self.id, req)
	}
	return internalapi.ForwardResponse{}, gather.ErrUnreachable
}

const logTimeFormat = "2006-01-02 15:04:05.000"

// writeLog writes the log of a server: lines at the given seconds after t0.
func writeLog(t *testing.T, dir string, t0 time.Time, lines map[int]string) {
	t.Helper()
	var b []byte
	for sec := 0; sec < 100; sec++ {
		if msg, ok := lines[sec]; ok {
			b = append(b, fmt.Sprintf("%s UTC [1] LOG:  %s\n", t0.Add(time.Duration(sec)*time.Second).In(time.Local).Format(logTimeFormat), msg)...)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "postgresql-test.log"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// newGCluster builds n servers named s1..sn; lines[i] are the log lines of s(i+1).
func newGCluster(t *testing.T, lines []map[int]string, timeout time.Duration) *gcluster {
	t.Helper()
	// The test clock is not the log clock: the lines are written in the local time zone.
	t0 := time.Date(2026, 11, 14, 9, 0, 0, 0, time.Local)
	c := &gcluster{t: t}
	for i := range lines {
		s := &gsrv{id: fmt.Sprintf("s%d", i+1), ordinal: i + 1, ring: monitor.NewRing(100, 100)}
		dir := t.TempDir()
		writeLog(t, dir, t0, lines[i])
		s.env = newEnv(t, func(d *Deps) {
			d.LogDir = func() string { return dir }
			d.Metrics = s.ring
			d.GatherTimeout = timeout
		})
		id := s.id
		if err := s.env.node.Transition(setup.Clustered, "7d1f2c0e-3b2a-4d58-9c51-2f6f0a1b9e11", "prod", &id); err != nil {
			t.Fatal(err)
		}
		c.servers = append(c.servers, s)
	}
	for _, s := range c.servers {
		s.env.srv.deps.Gatherer = fakeGatherer{c, s}
		// Every server knows the users of the others' sessions (revalidation, CP-07).
		for _, roles := range [][]string{{auth.RoleOps}, {auth.RoleReadonly}, {auth.RoleAdmin}, {auth.RoleStats}} {
			s.env.testUser(roles)
		}
	}
	return c
}

func (s *gsrv) get(path string, roles ...string) (int, map[string]any) {
	w := s.env.do(req{method: "GET", path: path, bearer: s.env.token(roles...)})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func messages(items []any) []string {
	var m []string
	for _, it := range items {
		m = append(m, it.(map[string]any)["message"].(string))
	}
	return m
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestClusterLogsMergeEveryServerInOneOrderAndPageStably(t *testing.T) {
	c := newGCluster(t, []map[int]string{
		{5: "s1-a", 9: "s1-b", 20: "s1-c"},
		{5: "s2-a", 7: "s2-b", 20: "s2-c"},
		{1: "s3-a", 9: "s3-b"},
	}, time.Second)
	want := []string{"s2-c", "s1-c", "s3-b", "s1-b", "s2-b", "s2-a", "s1-a", "s3-a"}
	// One page.
	st, body := c.servers[0].get("/api/v1/logs?limit=50", auth.RoleOps)
	if st != 200 || !equal(messages(body["items"].([]any)), want) || body["partial"] != false || body["missingServers"] != nil || body["nextCursor"] != nil {
		t.Fatalf("one page: %d %v", st, body)
	}
	// From any server the same answer; entries say which server wrote them.
	for _, s := range c.servers {
		_, b := s.get("/api/v1/logs?limit=50", auth.RoleOps)
		if !equal(messages(b["items"].([]any)), want) {
			t.Errorf("%s answers %v", s.id, messages(b["items"].([]any)))
		}
		for _, it := range b["items"].([]any) {
			m := it.(map[string]any)
			if m["serverId"] != m["message"].(string)[:2] {
				t.Errorf("entry %v is from the wrong server", m)
			}
		}
	}
	// Pages of three, each asked of another server: the same sequence, no entry twice or lost.
	var got []string
	cursor := ""
	for i := 0; i < 10; i++ {
		q := "/api/v1/logs?limit=3"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		st, b := c.servers[i%3].get(q, auth.RoleOps)
		if st != 200 {
			t.Fatalf("page %d: %d %v", i, st, b)
		}
		got = append(got, messages(b["items"].([]any))...)
		next, _ := b["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if !equal(got, want) {
		t.Errorf("paged: %v\nwant:  %v", got, want)
	}
	// Filters: the level, the text, a server.
	if _, b := c.servers[1].get("/api/v1/logs?q=S1-", auth.RoleOps); !equal(messages(b["items"].([]any)), []string{"s1-c", "s1-b", "s1-a"}) {
		t.Errorf("text filter: %v", b)
	}
	if _, b := c.servers[1].get("/api/v1/logs?serverId=s3", auth.RoleOps); !equal(messages(b["items"].([]any)), []string{"s3-b", "s3-a"}) {
		t.Errorf("server filter: %v", b)
	}
	if _, b := c.servers[2].get("/api/v1/logs?serverId=nonexistent", auth.RoleOps); len(b["items"].([]any)) != 0 || b["partial"] != false {
		t.Errorf("unknown server: %v", b)
	}
	// The server-local list stays local.
	if _, b := c.servers[0].get("/api/v1/this-server/logs", auth.RoleOps); !equal(messages(b["items"].([]any)), []string{"s1-c", "s1-b", "s1-a"}) {
		t.Errorf("this-server logs: %v", b)
	}
	// A cursor that is not ours, or that belongs to another kind of list.
	for _, bad := range []string{"nope", "e30", gather.EncodeAlertCursor(gather.AlertKey{Time: time.Now(), ID: 5})} {
		if st, _ := c.servers[0].get("/api/v1/logs?cursor="+bad, auth.RoleOps); st != 400 {
			t.Errorf("cursor %q: %d", bad, st)
		}
	}
}

// A server that cannot be reached, is slow, refuses or is not allowed is named in
// the answer, with its reason; the others still answer, and the answer comes
// within about the deadline.
func TestClusterLogsNameTheServersThatDidNotAnswer(t *testing.T) {
	c := newGCluster(t, []map[int]string{{5: "s1-a"}, {6: "s2-a"}, {7: "s3-a"}, {8: "s4-a"}}, 300*time.Millisecond)
	c.servers[1].down = true
	c.servers[2].slow = true
	c.servers[3].refuse = true
	start := time.Now()
	st, body := c.servers[0].get("/api/v1/logs", auth.RoleOps)
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %v with a 300 ms deadline", took)
	}
	if st != 200 || !equal(messages(body["items"].([]any)), []string{"s1-a"}) || body["partial"] != true {
		t.Fatalf("%d %v", st, body)
	}
	missing := map[string]string{}
	for _, m := range body["missingServers"].([]any) {
		mm := m.(map[string]any)
		missing[mm["serverId"].(string)] = mm["reason"].(string)
	}
	want := map[string]string{"s2": gather.ReasonUnreachable, "s3": gather.ReasonTimeout, "s4": gather.ReasonRefused}
	if len(missing) != 3 || missing["s2"] != want["s2"] || missing["s3"] != want["s3"] || missing["s4"] != want["s4"] {
		t.Errorf("missing servers %v, want %v", missing, want)
	}
	// A server that recovers is part of the next answer.
	c.servers[1].down = false
	_, body = c.servers[0].get("/api/v1/logs", auth.RoleOps)
	if len(body["items"].([]any)) != 2 || len(body["missingServers"].([]any)) != 2 {
		t.Errorf("after one server came back: %v", body)
	}
}

// The role is checked on the receiving server and again on the answering one:
// an identity that claims more than the answering server's catalog says gets no
// part of that server in the answer.
func TestClusterLogsAnsweringServerChecksTheRoleAgain(t *testing.T) {
	c := newGCluster(t, []map[int]string{{5: "s1-a"}, {6: "s2-a"}}, time.Second)
	// kingres_readonly may not read logs at all: refused by the receiving server.
	if st, _ := c.servers[0].get("/api/v1/logs", auth.RoleReadonly); st != http.StatusForbidden {
		t.Errorf("readonly on the receiving server: %d", st)
	}
	// The answering server revokes the cluster role of the user: it refuses its part.
	user := c.servers[0].env.testUser([]string{auth.RoleOps})
	c.servers[1].env.fa.mu.Lock()
	c.servers[1].env.fa.users[user] = fakeUser{password: "-", roles: []string{auth.RoleReadonly}}
	c.servers[1].env.fa.mu.Unlock()
	c.servers[1].env.srv.RoleChanged(user)
	st, body := c.servers[0].get("/api/v1/logs", auth.RoleOps)
	if st != 200 || !equal(messages(body["items"].([]any)), []string{"s1-a"}) || body["partial"] != true {
		t.Fatalf("%d %v", st, body)
	}
	if m := body["missingServers"].([]any)[0].(map[string]any); m["serverId"] != "s2" || m["reason"] != gather.ReasonRefused {
		t.Errorf("missing %v", m)
	}
	// An operation that is not gatherable, or an identity that is not valid, is refused outright.
	srv := c.servers[1].env.srv
	for name, fr := range map[string]internalapi.ForwardRequest{
		"another operation": {OperationID: "createRole", Identity: internalapi.ForwardIdentity{Username: "x", ClusterRoles: []string{auth.RoleAdmin}}},
		"no user":           {OperationID: "listLogs"},
		"unknown role":      {OperationID: "listLogs", Identity: internalapi.ForwardIdentity{Username: "x", ClusterRoles: []string{"root"}}},
	} {
		if _, err := srv.ServeGathered(context.Background(), "s1", fr); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestClusterMetricsCombineTheServers(t *testing.T) {
	c := newGCluster(t, []map[int]string{{}, {}, {}}, 400*time.Millisecond)
	t0 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	for i, s := range c.servers {
		for k := 0; k < 4; k++ {
			s.ring.Add(monitor.Series{Metric: "connections", ServerID: s.id}, t0.Add(time.Duration(k)*time.Second), f(float64(10*(i+1))))
			s.ring.Add(monitor.Series{Metric: "databaseSizeBytes", Database: "shop"}, t0.Add(time.Duration(k)*time.Second), f(float64(100*(i+1))))
		}
	}
	q := func(s *gsrv, extra string) map[string]any {
		st, b := s.get("/api/v1/metrics/query?metric=connections&from="+t0.Format(time.RFC3339)+"&to="+t0.Add(4*time.Second).Format(time.RFC3339)+"&step=4"+extra, auth.RoleStats)
		if st != 200 {
			t.Fatalf("%d %v", st, b)
		}
		return b
	}
	value := func(b map[string]any, i int) float64 {
		return b["series"].([]any)[i].(map[string]any)["points"].([]any)[0].(map[string]any)["value"].(float64)
	}
	// The cluster aggregate (a sum for connections) from any server.
	for _, s := range c.servers {
		if b := q(s, ""); len(b["series"].([]any)) != 1 || value(b, 0) != 60 || b["partial"] != false {
			t.Errorf("%s aggregate: %v", s.id, b)
		}
	}
	// One series per server.
	b := q(c.servers[1], "&groupBy=server")
	if len(b["series"].([]any)) != 3 {
		t.Fatalf("per server: %v", b)
	}
	for i, id := range []string{"s1", "s2", "s3"} {
		ser := b["series"].([]any)[i].(map[string]any)
		if ser["labels"].(map[string]any)["serverId"] != id || value(b, i) != float64(10*(i+1)) {
			t.Errorf("series %d: %v", i, ser)
		}
	}
	// One server.
	if b := q(c.servers[0], "&serverId=s3"); len(b["series"].([]any)) != 1 || value(b, 0) != 30 {
		t.Errorf("serverId=s3: %v", b)
	}
	// A database that every server reports is summed.
	st, db := c.servers[2].get("/api/v1/metrics/query?metric=databaseSizeBytes&from="+t0.Format(time.RFC3339)+"&to="+t0.Add(4*time.Second).Format(time.RFC3339)+"&step=4&groupBy=database", auth.RoleStats)
	if st != 200 || len(db["series"].([]any)) != 1 || value(db, 0) != 600 {
		t.Errorf("database size: %d %v", st, db)
	}
	// A server that is down: the sum of the others, and it is named.
	c.servers[2].down = true
	b = q(c.servers[0], "")
	if value(b, 0) != 30 || b["partial"] != true || b["missingServers"].([]any)[0].(map[string]any)["serverId"] != "s3" {
		t.Errorf("one server down: %v", b)
	}
}

// What a server answers is checked: another server's name, a wrong number of
// points or text with control characters never reach the client as they are.
func TestGatheredAnswersAreCleaned(t *testing.T) {
	p := gather.Peer{ID: "s2", Ordinal: 2}
	logs := cleanPeerLogs(p, []gather.RankedLog{
		{Entry: api.LogEntry{Timestamp: time.Now(), Level: "bogus", Component: "nope", Message: "a\x1b[31mred\nline"}},
		{Entry: api.LogEntry{Message: "no timestamp"}},
	}, 10)
	if len(logs) != 1 || logs[0].Entry.Level != "info" || logs[0].Entry.Component != "postgres" || *logs[0].Entry.ServerID != "s2" {
		t.Fatalf("logs %+v", logs)
	}
	for _, r := range logs[0].Entry.Message {
		if r < ' ' {
			t.Errorf("control character in %q", logs[0].Entry.Message)
		}
	}
	alerts := cleanPeerAlerts(p, []api.Alert{
		{ID: gather.AlertID(2, 5), RaisedAt: time.Now(), Message: "ok\x00"},
		{ID: gather.AlertID(3, 5), RaisedAt: time.Now(), Message: "an id of another server"},
		{ID: gather.AlertID(2, 6)},
	}, 10)
	if len(alerts) != 1 || alerts[0].ID != gather.AlertID(2, 5) || alerts[0].Message != "ok�" {
		t.Errorf("alerts %+v", alerts)
	}
	from := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	body := []byte(`{"series":[{"server":"s9","points":[{"timestamp":"2026-11-14T10:00:00Z","value":1}]}]}`)
	res, ok := cleanPeerSeries(p, body, "connections", from, 1, 1, "")
	if !ok || len(res) != 1 || res[0].Series.ServerID != "s2" || res[0].Series.Metric != "connections" {
		t.Errorf("series %+v %v", res, ok)
	}
	if _, ok := cleanPeerSeries(p, body, "connections", from, 1, 3, ""); ok {
		t.Error("an answer with the wrong number of points was accepted")
	}
	if _, ok := cleanPeerSeries(p, []byte(`{`), "connections", from, 1, 1, ""); ok {
		t.Error("an answer that is not JSON was accepted")
	}
}
