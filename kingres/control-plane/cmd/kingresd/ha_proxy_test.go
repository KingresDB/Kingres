// SPDX-License-Identifier: PostgreSQL

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clientproxy"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
)

// TestProxyCountersAreMetrics: the counters of the entry points reach the metrics
// ring, so that /metrics shows them under kingres_client_* and kingres_client_standby_*.
func TestProxyCountersAreMetrics(t *testing.T) {
	r := monitor.NewRing(10, 100)
	now := time.Now()
	addProxyMetrics(r, "10.0.0.1", "client", clientproxy.Stats{Accepted: 5, Active: 2, RefusedOverCap: 1, ConnectFailures: 3, ClosedOnMove: 4, BytesIn: 100, BytesOut: 200}, now)
	addProxyMetrics(r, "10.0.0.1", "clientStandby", clientproxy.Stats{Accepted: 1}, now)
	var out bytes.Buffer
	r.WritePrometheus(&out)
	for _, want := range []string{
		`kingres_client_connections_accepted{server="10.0.0.1"} 5`,
		`kingres_client_connections_active{server="10.0.0.1"} 2`,
		`kingres_client_connections_refused{server="10.0.0.1"} 1`,
		`kingres_client_connect_failures{server="10.0.0.1"} 3`,
		`kingres_client_connections_moved{server="10.0.0.1"} 4`,
		`kingres_client_bytes_in{server="10.0.0.1"} 100`,
		`kingres_client_bytes_out{server="10.0.0.1"} 200`,
		`kingres_client_standby_connections_accepted{server="10.0.0.1"} 1`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestEntryPort(t *testing.T) {
	if entryPort("10.0.0.1:5432") != 5432 || entryPort("") != 0 || entryPort("x") != 0 {
		t.Fatal("entryPort")
	}
}
