// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"errors"
	"testing"
)

// markFails is a cluster whose extension cannot say where its records stand.
type markFails struct{ *fakeCluster }

func (markFails) Mark(context.Context, Move) (int64, error) {
	return 0, errors.New("the map group primary cannot be asked")
}

// P4D-03: the mark is taken before the row is made and is written with it. A mark that cannot be had leaves no row
// at all (a row without a mark would be matched to a record by time), and no call is made.
func TestAMoveWhoseMarkCannotBeHadLeavesNoRow(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1, 1: 1}, map[int32]int32{0: 2, 1: 2})
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	l := testLoop(markFails{c}, r)
	_ = l.Moves(ctxWithFence(), Options{}) // ends in the safety stop after the tries
	moves, _ := r.Moves(context.Background())
	if len(moves) != 0 {
		t.Fatalf("%d rows were made for moves whose mark could not be had: %+v", len(moves), moves)
	}
	if n := len(c.runs); n != 0 {
		t.Fatalf("%d calls were made without a mark", n)
	}
}
