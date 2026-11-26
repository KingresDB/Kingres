// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// F2-16: the recovery of a bucket move asks the extension kingres.internal_move_alive(bigint) whether the session of a
// move is alive; a test against a stub of that function cannot tell if the real one has that signature, and the
// recovery reads any error as "not alive". This test calls the real function on a real map primary, with the
// service role the recovery uses: it must answer without an error, and false for a move that does not exist.
func TestTheRealExtensionAnswersTheMoveAliveCallOfTheRecovery(t *testing.T) {
	c := clustertest.Form(t, 1)
	n := c.Leader()
	conn := n.ServicePG("kingres_system")
	defer func() { _ = conn.Close(context.Background()) }()
	var alive bool
	if err := conn.QueryRow(context.Background(), `SELECT kingres.internal_move_alive($1::bigint)`, int64(987654321)).Scan(&alive); err != nil {
		t.Fatalf("the extension's internal_move_alive(bigint) cannot be called as the recovery calls it: %v", err)
	}
	if alive {
		t.Fatal("a move that does not exist is reported alive")
	}
}
