// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/gather"
)

func items(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["items"].([]any)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

func missingOf(body map[string]any) map[string]string {
	out := map[string]string{}
	m, _ := body["missingServers"].([]any)
	for _, x := range m {
		e := x.(map[string]any)
		out[e["serverId"].(string)] = e["reason"].(string)
	}
	return out
}

// Logs, alerts and metrics answer for the whole cluster from any server; paging
// is stable across servers; an acknowledgement finds the server that holds the
// alert; a server that is down is named; the answering server checks the role again.
func TestClusterWideLogsAlertsAndMetricsFromAnyServer(t *testing.T) {
	c := sharedCluster(t)
	ctx := context.Background()
	hosts := []string{c.Nodes[0].Host, c.Nodes[1].Host, c.Nodes[2].Host}

	// One alert on each server, in the table that server keeps itself.
	for i, n := range c.Nodes {
		sys := n.ServicePG("kingres_system")
		if _, err := sys.Exec(ctx, `INSERT INTO kingres_cp.alert (severity, kind, subject, message, raised_at)
			VALUES ('warning', 'taskFailed', $1, $2, now() + $3 * interval '1 second')`, "server:"+n.Host, "cluster-wide test alert of "+n.Host, i); err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
	}
	mine := func(list []map[string]any) map[string]map[string]any {
		got := map[string]map[string]any{}
		for _, a := range list {
			if m, _ := a["message"].(string); strings.HasPrefix(m, "cluster-wide test alert of ") {
				got[strings.TrimPrefix(m, "cluster-wide test alert of ")] = a
			}
		}
		return got
	}
	ids := map[float64]bool{}
	for _, n := range c.Nodes {
		body := n.Expect(200, "GET", "/alerts?limit=200", nil)
		got := mine(items(t, body))
		if len(got) != 3 || body["partial"] != false {
			t.Fatalf("node %d lists %d of the 3 alerts (partial %v): %v", n.Index, len(got), body["partial"], body)
		}
		for h, a := range got {
			if a["serverId"] != h {
				t.Errorf("node %d: the alert of %s says server %v", n.Index, h, a["serverId"])
			}
			ids[a["id"].(float64)] = true
		}
	}
	if len(ids) != 3 {
		t.Errorf("alert ids are not unique across servers: %v", ids)
	}
	for id := range ids {
		if id >= 1<<53 {
			t.Errorf("alert id %v does not fit a JSON number of a browser", id)
		}
	}
	// Paging with limit 1 asks every server again for each page and still sees every alert once.
	var paged []string
	cursor := ""
	for i := 0; i < 400; i++ {
		q := "/alerts?limit=1"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		body := c.Nodes[i%3].Expect(200, "GET", q, nil)
		for _, a := range items(t, body) {
			paged = append(paged, a["message"].(string))
		}
		next, _ := body["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	seen := map[string]int{}
	for _, m := range paged {
		seen[m]++
	}
	for _, h := range hosts {
		if seen["cluster-wide test alert of "+h] != 1 {
			t.Errorf("paging saw the alert of %s %d times", h, seen["cluster-wide test alert of "+h])
		}
	}
	// A cursor of the old kind is refused, and so is one of another list.
	if st, _ := c.Nodes[0].Do("GET", "/alerts?cursor=MQ", nil, nil); st != 400 {
		t.Errorf("old cursor: %d", st)
	}
	if st, _ := c.Nodes[0].Do("GET", "/alerts?cursor="+url.QueryEscape(gather.EncodeLogCursor(gather.LogKey{Time: time.Now(), Server: "x"})), nil, nil); st != 400 {
		t.Errorf("a log cursor for the alerts: %d", st)
	}

	// Acknowledge the alert of the third server through the first.
	list := mine(items(t, c.Nodes[0].Expect(200, "GET", "/alerts?limit=200", nil)))
	target := list[hosts[2]]
	id := int64(target["id"].(float64))
	ack := c.Nodes[0].Expect(200, "PUT", fmt.Sprintf("/alerts/%d/acknowledgement", id), nil)
	if ack["state"] != "acknowledged" || ack["acknowledgedBy"] != clustertest.AdminUser || ack["id"].(float64) != target["id"].(float64) {
		t.Errorf("acknowledged alert: %v", ack)
	}
	for _, n := range c.Nodes {
		if got := mine(items(t, n.Expect(200, "GET", "/alerts?state=acknowledged&limit=200", nil))); got[hosts[2]] == nil {
			t.Errorf("node %d does not list the acknowledged alert", n.Index)
		}
	}
	// The server that holds the alert audited it, once; the asking server did not.
	if got := countAudit(t, c.Nodes[2], "acknowledgeAlert", ""); got != 1 {
		t.Errorf("%d acknowledgeAlert events on the server that holds the alert", got)
	}
	if got := countAudit(t, c.Nodes[0], "acknowledgeAlert", ""); got != 0 {
		t.Errorf("%d acknowledgeAlert events on the asking server", got)
	}
	// An alert of a server that does not exist, and the resolved one.
	if st, out := c.Nodes[1].Do("PUT", fmt.Sprintf("/alerts/%d/acknowledgement", int64(999)<<32|5), nil, nil); st != 404 {
		t.Errorf("unknown server: %d %v", st, out)
	}

	// Logs: every server writes its own; the list holds all of them, paged without gaps.
	deadline := time.Now().Add(60 * time.Second)
	var servers map[string]bool
	for {
		servers = map[string]bool{}
		body := c.Nodes[1].Expect(200, "GET", "/logs?limit=500", nil)
		for _, e := range items(t, body) {
			if s, _ := e["serverId"].(string); s != "" {
				servers[s] = true
			}
		}
		if len(servers) == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if len(servers) != 3 {
		t.Errorf("the cluster log has entries of %d servers: %v", len(servers), servers)
	}
	var last time.Time
	count := 0
	cursor = ""
	for i := 0; i < 400; i++ {
		q := "/logs?limit=7"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		body := c.Nodes[i%3].Expect(200, "GET", q, nil)
		for _, e := range items(t, body) {
			ts, err := time.Parse(time.RFC3339Nano, e["timestamp"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if !last.IsZero() && ts.After(last) {
				t.Fatalf("the log is not newest first: %s after %s", ts, last)
			}
			last = ts
			count++
		}
		next, _ := body["nextCursor"].(string)
		if next == "" || count > 300 {
			break
		}
		cursor = next
	}
	if count < 10 {
		t.Errorf("paged only %d entries", count)
	}
	if body := c.Nodes[0].Expect(200, "GET", "/logs?serverId="+c.Nodes[2].Host+"&limit=500", nil); true {
		for _, e := range items(t, body) {
			if e["serverId"] != c.Nodes[2].Host {
				t.Errorf("an entry of %v with serverId=%s", e["serverId"], c.Nodes[2].Host)
			}
		}
	}

	// Metrics: one series per server, from any server (the sampler runs every 10 s).
	deadline = time.Now().Add(90 * time.Second)
	for {
		q := fmt.Sprintf("/metrics/query?metric=connections&from=%s&to=%s&step=60&groupBy=server",
			time.Now().Add(-10*time.Minute).UTC().Format(time.RFC3339), time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
		body := c.Nodes[1].Expect(200, "GET", q, nil)
		labels := map[string]bool{}
		for _, s := range anyList(body["series"]) {
			labels[s.(map[string]any)["labels"].(map[string]any)["serverId"].(string)] = true
		}
		if len(labels) == 3 && body["partial"] == false {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics of %d servers: %v", len(labels), body)
		}
		time.Sleep(2 * time.Second)
	}

	// The role is checked on the receiving server and again on the answering one.
	mkRole(t, c.Leader(), "logwatch", "A-Long-Password-1!", "kingres_ops")
	if st, _ := c.Nodes[0].Do("GET", "/logs", nil, map[string]string{"Authorization": "Bearer " + sessionToken(t, c.Nodes[0], "logwatch", "A-Long-Password-1!")}); st != 200 {
		t.Fatalf("logwatch cannot read the logs: %d", st)
	}
	tok := sessionToken(t, c.Nodes[0], "logwatch", "A-Long-Password-1!")
	if _, err := c.Nodes[2].ServicePG("postgres").Exec(ctx, "ALTER ROLE logwatch NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(30 * time.Second) // the answering server caches the check for 3 seconds
	for {
		st, body := c.Nodes[0].Do("GET", "/logs?limit=50", nil, map[string]string{"Authorization": "Bearer " + tok})
		m := missingOf(body)
		if st == 200 && body["partial"] == true && m[c.Nodes[2].Host] == "refused" && len(m) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server that no longer accepts the role is not reported: %d %v", st, body)
		}
		time.Sleep(time.Second)
	}
	// The same role as a reader of alerts only.
	mkRole(t, c.Leader(), "alertreader", "A-Long-Password-1!", "kingres_readonly")
	rtok := sessionToken(t, c.Nodes[1], "alertreader", "A-Long-Password-1!")
	if st, _ := c.Nodes[1].Do("GET", "/logs", nil, map[string]string{"Authorization": "Bearer " + rtok}); st != 403 {
		t.Errorf("a readonly role reads the logs: %d", st)
	}
	if st, _ := c.Nodes[1].Do("GET", "/alerts", nil, map[string]string{"Authorization": "Bearer " + rtok}); st != 200 {
		t.Errorf("a readonly role cannot read the alerts: %d", st)
	}
}

// A server that is down is named in the answer, with the reason; the rest of the
// cluster still answers. (Last, because it stops a server for a while.)
func TestClusterWideReadsNameTheServerThatIsDown(t *testing.T) {
	c := sharedCluster(t)
	down := c.Nodes[2]
	down.Stop()
	t.Cleanup(func() {
		if !down.Running() {
			down.Start()
			waitLogin(t, down)
		}
	})
	for _, q := range []string{"/logs?limit=20", "/alerts?limit=20"} {
		st, body := c.Nodes[1].Do("GET", q, nil, nil)
		m := missingOf(body)
		if st != 200 || body["partial"] != true || m[down.Host] == "" || len(m) != 1 {
			t.Errorf("%s with a server down: %d %v", q, st, body)
		}
		if m[down.Host] != gather.ReasonUnreachable && m[down.Host] != gather.ReasonTimeout {
			t.Errorf("%s: reason %q", q, m[down.Host])
		}
	}
	from, to := time.Now().Add(-5*time.Minute).UTC().Format(time.RFC3339), time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	st, body := c.Nodes[0].Do("GET", "/metrics/query?metric=connections&groupBy=server&step=60&from="+from+"&to="+to, nil, nil)
	if st != 200 || body["partial"] != true || missingOf(body)[down.Host] == "" {
		t.Errorf("metrics with a server down: %d %v", st, body)
	}
	down.Start()
	waitLogin(t, down)
	st, body = c.Nodes[1].Do("GET", "/logs?limit=20", nil, nil)
	if st != 200 || body["partial"] != false {
		t.Errorf("after the server is back: %d %v", st, body["missingServers"])
	}
}

// anyList reads a JSON array member; a missing or mistyped member is an empty list, so a test
// whose request failed reports that (the status and the answer) instead of panicking.
func anyList(v any) []any {
	l, _ := v.([]any)
	return l
}
