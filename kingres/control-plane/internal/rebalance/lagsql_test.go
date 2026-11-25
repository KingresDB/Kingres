// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package rebalance

import (
	"strings"
	"testing"
)

// The lag is read on the primary of a shard group with a statement that names pg_catalog views only: nothing of the schema kingres of a user database is read
// or called there, which is why the read needs no extguard (the call that finds the node runs on the system database, see standbyLagOf).
func TestLagSQLReadsNoObjectOfTheSchemaKingres(t *testing.T) {
	q := lagSQL("pg_catalog.pg_stat_replication", "pg_catalog.pg_replication_slots")
	if strings.Contains(q, "kingres") {
		t.Errorf("the lag statement names an object of kingres: %s", q)
	}
}
