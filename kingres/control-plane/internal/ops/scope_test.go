// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// TestDigestAndVerificationSayWhatTheyCover is CP2-08: a digest or a verification
// made by one server holds one chain; the other shard groups are named, and a
// database with one group is complete.
func TestDigestAndVerificationSayWhatTheyCover(t *testing.T) {
	r := digestRow{id: 3, at: time.Unix(1, 0), by: "kingresd", height: 7, hash: strings.Repeat("a", 64), hashVersion: 1, doc: []byte(`{}`)}
	d := toDigest("shop", r, 0, []int32{1, 2}, "")
	if d.Complete || len(d.UncoveredShardGroupIDs) != 2 || len(d.Chains) != 1 || d.AggregateHash != nil {
		t.Errorf("digest of a three-group database: %+v", d)
	}
	one := toDigest("shop", r, 0, nil, "alice")
	b, _ := json.Marshal(one)
	if !one.Complete || !strings.Contains(string(b), `"uncoveredShardGroupIds":[]`) {
		t.Errorf("digest of a one-group database: %s", b)
	}
	base := extVerification{id: 1, startedAt: time.Unix(1, 0), state: "clean"}
	v := toVerification("shop", verificationLink{id: 5, extID: 1, group: 0}, base, []int32{1})
	if v.State != "passed" || v.Complete || len(v.UncoveredShardGroupIDs) != 1 || v.UncoveredShardGroupIDs[0] != 1 {
		t.Errorf("verification of a two-group database: %+v", v)
	}
}

func TestPlainRead(t *testing.T) {
	for sql, want := range map[string]bool{
		"select 1":                       true,
		"  SELECT * from t":              true,
		"-- note\nselect 1":              true,
		"/* a */ /* b */ values (1)":     true,
		"table t":                        true,
		"show all":                       true,
		"update t set x = 1 returning *": false,
		"with a as (delete from t returning *) select * from a": false,
		"insert into t values (1) returning 1":                  false,
		"selectx(1)":                                            false,
		"/* open":                                               false,
		"":                                                      false,
	} {
		if got := plainRead(sql); got != want {
			t.Errorf("plainRead(%q) = %v, want %v", sql, got, want)
		}
	}
}

// TestOneRoleCannotHoldEveryWorkbenchSlot is CP2-15.
func TestOneRoleCannotHoldEveryWorkbenchSlot(t *testing.T) {
	var held []func()
	defer func() {
		for _, f := range held {
			f()
		}
	}()
	for i := 0; i < maxWorkbenchPerRole; i++ {
		rel, ok := acquireWorkbench("mallory")
		if !ok {
			t.Fatalf("slot %d refused", i)
		}
		held = append(held, rel)
	}
	if _, ok := acquireWorkbench("mallory"); ok {
		t.Error("a role got more than its share")
	}
	rel, ok := acquireWorkbench("alice")
	if !ok {
		t.Fatal("another role was locked out by mallory")
	}
	held = append(held, rel)
	// Fill the server with other roles: the global limit still holds.
	for i := 0; ; i++ {
		rel, ok := acquireWorkbench("filler" + string(rune('a'+i)))
		if !ok {
			break
		}
		held = append(held, rel)
		if i > maxWorkbench {
			t.Fatal("the global limit does not hold")
		}
	}
	held[0]()
	held = held[1:]
	if rel, ok := acquireWorkbench("mallory"); !ok {
		t.Error("a released slot is not available again")
	} else {
		rel()
	}
}

// TestOtherGroupsReadsTheShardCatalog runs the query on a real PostgreSQL: a database without
// the shard catalog has no other group, one with three groups has two besides the local one.
func TestOtherGroupsReadsTheShardCatalog(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set")
	}
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	c := in.Connect(t, "postgres")
	if got := otherGroups(ctx, c, 0); got == nil || len(got) != 0 {
		t.Fatalf("without a catalog: %v (want an empty, non-nil list)", got)
	}
	for _, sql := range []string{
		"CREATE SCHEMA kingres",
		"CREATE TABLE kingres.node (node_id int PRIMARY KEY, group_id int NOT NULL)",
		"INSERT INTO kingres.node VALUES (1, 0), (2, 0), (3, 1), (4, 2)",
	} {
		if _, err := c.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	got := otherGroups(ctx, c, 0)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("groups other than 0: %v", got)
	}
}
