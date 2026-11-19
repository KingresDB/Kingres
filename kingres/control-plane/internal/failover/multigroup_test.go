// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"testing"
)

// A server that hosted two primaries: the second group is promoted after the first, and the term of the catalog has risen by
// the first promotion. The destructive calls of the second group expect that term, not the one the run read at its start
// (found by the first cluster test of a failover of two groups: the second group ended "superseded" by its own run).
func TestTheTermTheSecondGroupExpectsIncludesThePromotionOfTheFirst(t *testing.T) {
	ctx := context.Background()
	rows := newMemRows()
	in := Input{ServerID: "s1", Groups: []int32{1, 2}}
	if err := rows.Begin(ctx, in, 4, []GroupRow{{Group: 1, Seq: 0}, {Group: 2, Seq: 1}}); err != nil {
		t.Fatal(err)
	}
	after := int64(5)
	if err := rows.Set(ctx, 1, GroupSet{TermAfter: &after}); err != nil {
		t.Fatal(err)
	}
	first := (&stepCtx{in: in, rows: rows, group: 1})
	second := (&stepCtx{in: in, rows: rows, group: 2})
	if run, err := first.runRow(ctx); err != nil || run.ExpectedTerm != 4 {
		t.Fatalf("group 1 expects %d (%v), want 4", run.ExpectedTerm, err)
	}
	if run, err := second.runRow(ctx); err != nil || run.ExpectedTerm != 5 {
		t.Fatalf("group 2 expects %d (%v), want 5", run.ExpectedTerm, err)
	}
}
