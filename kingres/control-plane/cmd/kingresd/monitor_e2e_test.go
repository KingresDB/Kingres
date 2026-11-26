// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMetricsAndAlertsEndpoints(t *testing.T) {
	e := shared(t)
	// The sampler runs at start; wait for the first server-scoped samples.
	var res map[string]any
	deadline := time.Now().Add(60 * time.Second)
	for {
		q := fmt.Sprintf("/metrics/query?metric=connections&from=%s&to=%s&step=60&groupBy=server",
			time.Now().Add(-10*time.Minute).UTC().Format(time.RFC3339), time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
		_, res, _ = e.call("GET", q, nil, nil)
		if s, _ := res["series"].([]any); len(s) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no samples: %v", res)
		}
		time.Sleep(500 * time.Millisecond)
	}
	valid(t, "queryMetrics", "200", res)
	if res["unit"] != "count" {
		t.Errorf("unit %v", res["unit"])
	}
	for _, bad := range []string{
		"/metrics/query?metric=nope&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z&step=60",
		"/metrics/query?metric=connections&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z",
		"/metrics/query?metric=connections&from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z&step=60",
	} {
		if st, _, _ := e.call("GET", bad, nil, nil); st != 400 && st != 422 {
			t.Errorf("%s: %d", bad, st)
		}
	}
	if st, _, _ := e.call("GET", "/metrics/query?metric=connections&from=2026-01-01T00:00:00Z&to=2026-06-01T00:00:00Z&step=1", nil, nil); st != 422 {
		t.Errorf("more than 5000 points: %d", st)
	}
	// Alerts: nothing is wrong on a healthy test machine (or whatever is open is well formed).
	al := e.expect(t, 200, "GET", "/alerts?limit=10", nil)
	valid(t, "listAlerts", "200", al)
	if st, _, _ := e.call("PUT", "/alerts/987654/acknowledgement", nil, nil); st != 404 {
		t.Errorf("unknown alert: %d", st)
	}
	ro := e.accountWithRole(t, "mon_ro", "kingres_readonly")
	if st, _, _ := e.as(ro).call("PUT", "/alerts/1/acknowledgement", nil, nil); st != 403 {
		t.Errorf("acknowledge as readonly: %d", st)
	}
	stats := e.accountWithRole(t, "mon_stats", "kingres_stats")
	if st, _, _ := e.as(stats).call("GET", "/alerts", nil, nil); st != 403 {
		t.Errorf("alerts as stats: %d", st)
	}

	// The Prometheus endpoint needs authentication.
	get := func(token string) (int, string) {
		req, _ := http.NewRequest("GET", strings.TrimSuffix(e.base, "/api/v1")+"/metrics", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, _ := get(""); st != 401 {
		t.Errorf("/metrics without credentials: %d", st)
	}
	if st, _ := get("not-a-token"); st != 401 {
		t.Errorf("/metrics with a wrong token: %d", st)
	}
	st, body := get(stats)
	if st != 200 || !strings.Contains(body, "# TYPE kingres_connections gauge") {
		t.Errorf("/metrics with a stats token: %d %.200s", st, body)
	}
	if st, _ := get(ro); st != 200 {
		t.Errorf("/metrics with a readonly token (above stats): %d", st)
	}
	e.expect(t, 204, "DELETE", "/roles/mon_ro", nil)
	e.expect(t, 204, "DELETE", "/roles/mon_stats", nil)
}
