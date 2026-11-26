// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package rebalance

import (
	"context"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
)

// ST-06: StartMove and AddSkipped are repeated by retryRows when the answer of a committed write was lost: the second
// execution must find what the first made, not add a second row (one move would count twice in the report).
func TestARepeatedStartMoveAndAddSkippedDoNotDuplicateRows(t *testing.T) {
	e, lease := newGuardEnv(t)
	ctx := fence.With(context.Background(), fence.Token{Task: e.id, Epoch: lease.Epoch})
	rows := &PGRows{Conns: e, TaskID: e.id}
	if _, err := rows.CreateRun(ctx, Options{}, nil); err != nil {
		t.Fatal(err)
	}
	m := Move{Database: "shop", Colocation: 0, Bucket: 3, From: 1, To: 2}
	a, err := rows.StartMove(ctx, m, "s1", "s2", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := rows.StartMove(ctx, m, "s1", "s2", 1, 0) // the same attempt, repeated
	if err != nil || a != b {
		t.Fatalf("a repeated start made seq %d after %d (%v)", b, a, err)
	}
	m2 := Move{Database: "shop", Colocation: 0, Bucket: 4, From: 1, To: 2}
	for i := 0; i < 2; i++ {
		if err := rows.AddSkipped(ctx, m2, "s1", "s2", "target_busy", "a held target"); err != nil {
			t.Fatal(err)
		}
	}
	var n, skipped int
	if err := e.sys.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE state = 'skipped') FROM kingres_cp.rebalance_move WHERE task_id = $1`, e.id).Scan(&n, &skipped); err != nil {
		t.Fatal(err)
	}
	if n != 2 || skipped != 1 {
		t.Fatalf("%d rows (%d skipped), want 2 (1 skipped): a repeated write made a second row", n, skipped)
	}
	// A new attempt of a failed move is still a new attempt (the update branch), not a repeat.
	if err := rows.FinishMove(ctx, a, "failed", "x", "", nil); err != nil {
		t.Fatal(err)
	}
	if c, err := rows.StartMove(ctx, m, "s1", "s2", 2, 0); err != nil || c != a {
		t.Fatalf("the next attempt must reuse the row: %d %v", c, err)
	}
}
