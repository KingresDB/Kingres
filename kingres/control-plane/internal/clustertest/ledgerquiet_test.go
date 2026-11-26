// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// quietCounts is what a quiet database is measured by: the blocks and the recorded digests of each group, the sizes of the published logs and the
// number of events.
type quietCounts struct {
	blocks, digests [2]int64
	logs            map[string]int64
	events          int64
}

func countQuiet(t *testing.T, p *pubCluster, db string) quietCounts {
	t.Helper()
	ctx := context.Background()
	q := quietCounts{logs: map[string]int64{}}
	for i, n := range p.c.Nodes {
		c := n.ServicePG(db)
		if err := c.QueryRow(ctx, `SELECT count(*) FROM kingres.ledger_blocks`).Scan(&q.blocks[i]); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRow(ctx, `SELECT count(*) FROM kingres.ledger_digest`).Scan(&q.digests[i]); err != nil {
			t.Fatal(err)
		}
		_ = c.Close(ctx)
	}
	for _, l := range publicationLogs(t, p.c.Nodes[0]) {
		q.logs[l.Origin] = l.Size
	}
	s := p.c.Nodes[0].ServicePG("kingres_system")
	defer func() { _ = s.Close(ctx) }()
	if err := s.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.ledger_event`).Scan(&q.events); err != nil {
		t.Fatal(err)
	}
	return q
}

// The 305 blocks of the fixture (item 1 of the round, correction 190). The chain of group 0 of the publication test had 305 blocks for the 5 rows of
// its first table: the helper insertRows closes a block with ledger_generate_digest after EVERY row, so the 5 rows made 5 blocks, and the 300 rows that
// the test adds later made 300 more (5 + 300 = 305). Neither the builder nor the collector made one of them. This test measures what a quiet
// database does: rows are written in a way that leaves the builder to its schedule (one INSERT per row, no digest), the builder is given time to put
// them into blocks, and from then on nothing is written for a time (KINGRES_QUIET_MINUTES, 3 by default, 10 for the measurement of the report) with
// the publication running at its shortest interval. The blocks, the digests, the sizes of the logs and the events must not change by one.
func TestAQuietLedgerDatabaseStaysQuiet(t *testing.T) {
	minutes := 3
	if v, err := strconv.Atoi(os.Getenv("KINGRES_QUIET_MINUTES")); err == nil && v > 0 {
		minutes = v
	}
	p := formPublishing(t, "quiet_db")
	leader, follower := p.c.Nodes[0], p.c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	ctx := context.Background()
	for _, n := range []*clustertest.Node{leader, follower} {
		// the database server of a node that has only just been created may not listen yet (seen once in five runs)
		until(t, time.Minute, "the database server of the node to accept connections", func() bool {
			pw, err := os.ReadFile(filepath.Join(n.DataDir, "service-credentials"))
			if err != nil {
				return false
			}
			pc, err := n.PG(ctx, "kingresd", strings.TrimSpace(string(pw)), "quiet_db")
			if err != nil {
				return false
			}
			_ = pc.Close(ctx)
			return true
		})
		c := n.ServicePG("quiet_db")
		for _, q := range []string{"CREATE TABLE q (id int PRIMARY KEY, v text)", "SELECT kingres.ledger_enable('public.q'::regclass, 'append_only')"} {
			if _, err := c.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 5; i++ { // five rows, five transactions, no digest: the builder's own schedule
			if _, err := c.Exec(ctx, "INSERT INTO q VALUES ($1, 'x')", i); err != nil {
				t.Fatal(err)
			}
		}
		_ = c.Close(ctx)
	}
	p.startPublishing(t)
	var start quietCounts
	until(t, 3*time.Minute, "the builder to put the rows into blocks and the publication to follow", func() bool {
		start = countQuiet(t, p, "quiet_db")
		agg := int64(0)
		for o, s := range start.logs {
			if len(o) > 0 && s > agg {
				agg = s
			}
		}
		return start.blocks[0] >= 1 && start.blocks[1] >= 1 && len(start.logs) == 3
	})
	time.Sleep(25 * time.Second) // one more look of the builder and of two rounds, so that "start" is the settled state
	start = countQuiet(t, p, "quiet_db")
	t.Logf("blocks for 5 rows with no digest: group 0 %d, group 1 %d (the builder batches the pending transactions of a visit into one block)", start.blocks[0], start.blocks[1])
	time.Sleep(time.Duration(minutes) * time.Minute)
	end := countQuiet(t, p, "quiet_db")
	t.Logf("after %d quiet minutes: blocks %v -> %v, digests %v -> %v, logs %v -> %v, events %d -> %d", minutes, start.blocks, end.blocks, start.digests, end.digests, start.logs, end.logs, start.events, end.events)
	if start.blocks != end.blocks || start.digests != end.digests || start.events != end.events {
		t.Errorf("a quiet database changed: blocks %v -> %v, digests %v -> %v, events %d -> %d", start.blocks, end.blocks, start.digests, end.digests, start.events, end.events)
	}
	for o, s := range start.logs {
		if end.logs[o] != s {
			t.Errorf("the log %s grew without a write: %d -> %d", o, s, end.logs[o])
		}
	}
	if len(end.logs) != len(start.logs) {
		t.Errorf("logs %v -> %v", start.logs, end.logs)
	}
}
