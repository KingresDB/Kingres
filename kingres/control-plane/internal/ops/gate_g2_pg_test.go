// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
)

// L2 of the review of g2, as behaviour and not as source text: a tenant makes the schema kingres and a function kingres.partition_leaf_storage in it. The listing of
// the partitions of a distributed table runs as a superuser; it must not call the function of the tenant.
func TestForgedKingresSchemaIsNotCalledByThePartitionListing(t *testing.T) {
	_, c, o := newGuardInstance(t)
	ctx := context.Background()
	pgExecAll(t, c, "create role tenant", "create table public.proof (who text, su boolean)", "grant all on public.proof to public",
		"create schema kingres authorization tenant", "grant usage on schema kingres to public",
		`create function kingres.partition_leaf_storage(regclass) returns table (partition_name text, access_method text) language plpgsql security invoker as
		 $$ begin insert into public.proof select current_user, (select rolsuper from pg_roles where rolname = current_user); return query select 'p1'::text, 'columnar'::text; end $$`,
		"alter function kingres.partition_leaf_storage(regclass) owner to tenant", "create table public.t (id int)")
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if ok, err := extUsable(ctx, tx, extguard.Shard); err != nil || ok {
		t.Fatalf("a schema kingres that a tenant made is usable: %v %v", ok, err)
	}
	items := []api.TablePartition{{Name: "p1", Method: "range", Bound: "FOR VALUES FROM (1) TO (2)"}}
	o.fillDistPartitions(ctx, tx, "public", "t", items)
	if items[0].AccessMethod != nil {
		t.Errorf("the listing took the storage of a partition from the function of a tenant: %q", *items[0].AccessMethod)
	}
	var n int
	if err := tx.QueryRow(ctx, "select count(*) from public.proof").Scan(&n); err != nil || n != 0 {
		t.Errorf("the function of the tenant ran %d times (%v)", n, err)
	}
}
