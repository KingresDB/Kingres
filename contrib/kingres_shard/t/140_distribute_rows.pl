# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/140_distribute_rows.pl (the "t/040 blocking" test of design/shard-phase9.md,
# increment 9.2): create_distributed_table() and create_reference_table() of
# tables that hold rows, on three nodes.
#
#   * every row is in exactly the group that owns its bucket, and nowhere else;
#     the table equals what it was, from every node;
#   * owner, privileges, comments, sequences, indexes and keys are the ones the
#     source had, compared object by object, on every node;
#   * what is blocked while the copy runs is blocked and nothing else: writers of
#     the table wait, readers do not, until the swap;
#   * a failure of any node at any point (an error at every send, wait and step, a
#     cancel, a crash of the origin, a node that is down) leaves the table exactly
#     as it was, with its rows, and nothing half-distributed on any node;
#   * the two-phase commit decides the end: a crash after the local commit ends in
#     a complete table.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_waiter start_psql finish_psql wait_for_fault_file);

my ($n1, $n2, $n3) = make_cluster('dr', 3);
my @nodes = ($n1, $n2, $n3);

# Roles are not synchronized between the nodes (phase 2a): make them everywhere.
$_->safe_psql('postgres', q{
	CREATE ROLE p9_owner; CREATE ROLE p9_reader; CREATE ROLE p9_writer; CREATE ROLE p9_grantor;
}) for @nodes;

# What a table looks like, object by object, as text; the same on every node when the copy carried everything.
my $fingerprint_sql = q{
CREATE FUNCTION public.fingerprint(t regclass) RETURNS SETOF text LANGUAGE sql AS $$
  SELECT 'column ' || a.attnum || ' ' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
         || ' notnull=' || a.attnotnull || ' identity=' || a.attidentity::text || ' generated=' || a.attgenerated::text
         || ' default=' || coalesce(pg_get_expr(d.adbin, d.adrelid), '-')
         || ' acl=' || coalesce(a.attacl::text, '-') || ' comment=' || coalesce(col_description(t, a.attnum), '-')
    FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
   WHERE a.attrelid = t AND a.attnum > 0 AND NOT a.attisdropped
  UNION ALL
  SELECT 'constraint ' || c.conname || ' ' || pg_get_constraintdef(c.oid) || ' comment=' || coalesce(obj_description(c.oid, 'pg_constraint'), '-')
    FROM pg_constraint c WHERE c.conrelid = t AND c.conparentid = 0
  UNION ALL
  SELECT 'index ' || ic.relname || ' ' || replace(pg_get_indexdef(i.indexrelid), ' ONLY ', ' ') || ' comment=' || coalesce(obj_description(i.indexrelid, 'pg_class'), '-')
    FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid WHERE i.indrelid = t
  UNION ALL
  SELECT 'table owner=' || c.relowner::regrole || ' acl=' || coalesce(c.relacl::text, '-') || ' comment=' || coalesce(obj_description(t, 'pg_class'), '-')
    FROM pg_class c WHERE c.oid = t
  UNION ALL
  SELECT 'sequence ' || s.seqrelid::regclass || ' type=' || s.seqtypid::regtype || ' start=' || s.seqstart || ' inc=' || s.seqincrement
         || ' min=' || s.seqmin || ' max=' || s.seqmax || ' cache=' || s.seqcache || ' cycle=' || s.seqcycle
         || ' owner=' || coalesce((SELECT d.refobjid::regclass || '.' || d.refobjsubid || '/' || d.deptype::text FROM pg_depend d
                                    WHERE d.classid = 'pg_class'::regclass AND d.objid = s.seqrelid
                                      AND d.refclassid = 'pg_class'::regclass LIMIT 1), '-')
    FROM pg_sequence s
   WHERE s.seqrelid IN (SELECT d.objid FROM pg_depend d WHERE d.classid = 'pg_class'::regclass
                         AND d.refclassid = 'pg_class'::regclass AND d.refobjid = t AND d.deptype IN ('a', 'i'))
$$;
-- the rows of the partitions of this node, and the rows that are in the wrong partition
CREATE FUNCTION public.local_rows(t regclass, key name) RETURNS TABLE (part text, bucket int, n bigint, wrong bigint)
LANGUAGE plpgsql AS $$
DECLARE r record;
BEGIN
  FOR r IN SELECT c.oid::regclass AS p, (regexp_match(c.relname, '_b([0-9]{4})$'))[1]::int AS b
             FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = t ORDER BY c.relname
  LOOP
    part := r.p::text; bucket := r.b;
    EXECUTE format('SELECT count(*), count(*) FILTER (WHERE kingres.bucket_for(%L::regclass, %I) <> %s) FROM ONLY %s', t, key, r.b, r.p)
       INTO n, wrong;
    RETURN NEXT;
  END LOOP;
END $$;
-- the largest value of a column in the partitions of this node
CREATE FUNCTION public.local_max(t regclass, col name) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE r record; m bigint := 0; v bigint;
BEGIN
  FOR r IN SELECT c.oid::regclass AS p FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = t
  LOOP
    EXECUTE format('SELECT coalesce(max(%I), 0) FROM ONLY %s', col, r.p) INTO v;
    m := greatest(m, v);
  END LOOP;
  RETURN m;
END $$;
};
$_->safe_psql('postgres', $fingerprint_sql) for @nodes;

sub fp
{
	my ($node, $t) = @_;
	return $node->safe_psql('postgres', "SELECT line FROM public.fingerprint('$t') line ORDER BY 1");
}

sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

sub no_leftovers
{
	my ($label, $table) = @_;
	for my $n (@nodes)
	{
		is(q1($n, "SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\\_shadow%' OR relname LIKE 'kingres\\_old%'"),
			'0', "$label: no shadow table on " . $n->name);
	}
	is(prepared_count($_), '0', "$label: nothing is prepared on " . $_->name) for @nodes;
}

# ---------------------------------------------------------------------------
# A reference table with rows, an owner, privileges and a comment: the copies
# on the other nodes carry them.
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.ref1 (id int PRIMARY KEY, name text);
	INSERT INTO public.ref1 SELECT g, 'r' || g FROM generate_series(1, 5) g;
	ALTER TABLE public.ref1 OWNER TO p9_owner;
	GRANT SELECT ON public.ref1 TO p9_reader;
	GRANT UPDATE (name) ON public.ref1 TO p9_writer;
	COMMENT ON TABLE public.ref1 IS 'the reference table';
	COMMENT ON COLUMN public.ref1.name IS 'the name';
});
my $ref1_before = fp($n1, 'public.ref1');
# a key from an ordinary table to a table that is not a reference table yet is refused (the key stays)
$n1->safe_psql('postgres', q{
	CREATE TABLE public.ref2 (id int PRIMARY KEY, r int REFERENCES public.ref1 (id), v text DEFAULT 'x');
});
{
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_reference_table('public.ref1')");
	like($err, qr/foreign key "ref2_r_fkey" of ordinary table "public.ref2" references the table/, 'a table that an ordinary table references is refused');
}
$n1->safe_psql('postgres', 'DROP TABLE public.ref2');
$n1->safe_psql('postgres', "SELECT kingres.create_reference_table('public.ref1')");
$n1->safe_psql('postgres', q{
	CREATE TABLE public.ref2 (id int PRIMARY KEY, r int REFERENCES public.ref1 (id), v text DEFAULT 'x');
	INSERT INTO public.ref2 SELECT g, 1 + g % 5 FROM generate_series(1, 20) g;
	ALTER TABLE public.ref2 OWNER TO p9_owner;
	GRANT SELECT, INSERT ON public.ref2 TO p9_reader WITH GRANT OPTION;
});
my $ref2_before = fp($n1, 'public.ref2');
$n1->safe_psql('postgres', "SELECT kingres.create_reference_table('public.ref2')");
for my $n (@nodes)
{
	is(q1($n, "SELECT count(*) FROM public.ref2"), '20', "reference table with a key: its rows are on " . $n->name);
	is(fp($n, 'public.ref1'), $ref1_before, "the reference table carries owner, privileges and comments on " . $n->name);
	is(fp($n, 'public.ref2'), $ref2_before, "the reference table with a key to a reference table is the same on " . $n->name);
}
{
	my (undef, undef, $err) = run_sql($n3, "INSERT INTO public.ref2 VALUES (100, 77, 'bad')");
	like($err, qr/violates foreign key constraint/, 'the key of a reference table is enforced on every node');
}

# ---------------------------------------------------------------------------
# A table with rows: the whole of what it has is carried.
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.items (
	    tenant int NOT NULL, id int NOT NULL, name text DEFAULT 'unnamed',
	    qty int CONSTRAINT items_qty_check CHECK (qty >= 0),
	    ser serial,
	    ident bigint GENERATED BY DEFAULT AS IDENTITY (START WITH 100 INCREMENT BY 5),
	    calc int GENERATED ALWAYS AS (qty * 2) STORED,
	    ref int REFERENCES public.ref1 (id),
	    big text, price money DEFAULT 1.5,
	    PRIMARY KEY (tenant, id));
	CREATE INDEX items_name_idx ON public.items (name);
	CREATE UNIQUE INDEX items_part_idx ON public.items (id, tenant) WHERE qty > 10;
	INSERT INTO public.items (tenant, id, name, qty, ref, big, price)
	  SELECT g % 7, g, CASE WHEN g % 10 = 0 THEN NULL ELSE 'n' || g END, g % 50, 1 + g % 5,
	         CASE WHEN g % 100 = 0 THEN repeat(md5(g::text), 400) END, (g * 3.5)::numeric::money
	    FROM generate_series(1, 6000) g;
	ALTER TABLE public.items OWNER TO p9_owner;
	GRANT SELECT ON public.items TO p9_reader;
	REVOKE UPDATE ON public.items FROM p9_owner;
	GRANT UPDATE (name), SELECT (qty) ON public.items TO p9_writer;
	GRANT INSERT, UPDATE ON public.items TO p9_grantor WITH GRANT OPTION;
	SET ROLE p9_grantor; GRANT INSERT ON public.items TO p9_writer; RESET ROLE;
	COMMENT ON TABLE public.items IS 'the items';
	COMMENT ON COLUMN public.items.name IS 'the name';
	COMMENT ON CONSTRAINT items_qty_check ON public.items IS 'qty is not negative';
	COMMENT ON CONSTRAINT items_pkey ON public.items IS 'the key';
	COMMENT ON INDEX public.items_name_idx IS 'by name';
	SELECT setval('public.items_ser_seq', 7000);
});
my $items_fp = fp($n1, 'public.items');
my $model_md5 = q1($n1, "SELECT md5(string_agg(t::text, ',' ORDER BY tenant, id)) FROM public.items t");
my $ident_state = q1($n1, "SELECT last_value || '/' || is_called FROM public.items_ident_seq");
my $ser_state = q1($n1, "SELECT last_value || '/' || is_called FROM public.items_ser_seq");
my @versions_before = map { q1($_, 'SELECT version FROM kingres.map_version') } @nodes;

$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('public.items', 'id', bucket_count => 12)");

for my $i (0 .. 2)
{
	my $n = $nodes[$i];
	cmp_ok(q1($n, 'SELECT version FROM kingres.map_version'), '>', $versions_before[$i], "the map version rose on " . $n->name);
	is(q1($n, "SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.items'::regclass"), '1', $n->name . " knows the table");
	is(q1($n, "SELECT count(*) FROM public.items"), '6000', "the table has 6000 rows seen from " . $n->name);
	is(q1($n, "SELECT md5(string_agg(t::text, ',' ORDER BY tenant, id)) FROM public.items t"), $model_md5,
		"the rows are the rows of the source, from " . $n->name);
}
# every row is in exactly the group that owns its bucket, and nowhere else
{
	my $total = 0;
	for my $i (0 .. 2)
	{
		my $n = $nodes[$i];
		my ($bad_owner, $wrong, $rows) = split /\|/, q1($n, qq{
			SELECT count(*) FILTER (WHERE (SELECT m.group_id FROM kingres.bucket_map m
			                                WHERE m.colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.items'::regclass)
			                                  AND m.bucket = r.bucket) <> $i)
			       || '|' || coalesce(sum(wrong), 0) || '|' || coalesce(sum(n), 0)
			  FROM public.local_rows('public.items', 'id') r});
		is($bad_owner, '0', $n->name . " holds only partitions of buckets of its group");
		is($wrong, '0', $n->name . " holds no row in the partition of another bucket");
		cmp_ok($rows, '>', 0, $n->name . " holds rows");
		$total += $rows;
	}
	is($total, 6000, 'the rows of the three nodes add up to the table: no row twice, none lost');
}
is(q1($n1, "SELECT count(*) FROM pg_class WHERE oid = 'public.items'::regclass AND relkind = 'p'"), '1', 'the table is partitioned by hash');
for my $n (@nodes)
{
	is(fp($n, 'public.items'), $items_fp, "owner, privileges, comments, keys, indexes, defaults and sequences are the source's on " . $n->name);
}
is(q1($n1, "SELECT last_value || '/' || is_called FROM public.items_ident_seq"), $ident_state, 'the identity sequence is where it was');
is(q1($n1, "SELECT last_value || '/' || is_called FROM public.items_ser_seq"), $ser_state, 'the serial sequence is where it was');
# each node's sequences are at least the largest value of its own rows
for my $i (0 .. 2)
{
	my $n = $nodes[$i];
	for my $pair (['ser', 'items_ser_seq'], ['ident', 'items_ident_seq'])
	{
		my ($col, $seq) = @$pair;
		my $held = q1($n, "SELECT public.local_max('public.items', '$col')");
		cmp_ok(q1($n, "SELECT last_value FROM public.$seq"), '>=', $held, "the sequence $seq of " . $n->name . " is not below the values of its own rows");
	}
}
# the table works: writes from every node, the keys are enforced
for my $i (0 .. 2)
{
	my $id = 20000 + $i;
	is(q1($nodes[$i], "INSERT INTO public.items (tenant, id, qty, ref) VALUES (1, $id, 3, 2) RETURNING calc"), '6',
		"an insert from " . $nodes[$i]->name . " works");
}
{
	my (undef, undef, $err) = run_sql($n2, "INSERT INTO public.items (tenant, id, qty, ref) VALUES (1, 20000, 3, 2)");
	like($err, qr/duplicate key value violates unique constraint/, 'the primary key is enforced');
	(undef, undef, $err) = run_sql($n3, "INSERT INTO public.items (tenant, id, qty, ref) VALUES (1, 20010, -1, 2)");
	like($err, qr/violates check constraint "items_qty_check"/, 'the check constraint is enforced');
	(undef, undef, $err) = run_sql($n2, "INSERT INTO public.items (tenant, id, qty, ref) VALUES (1, 20011, 1, 99)");
	like($err, qr/violates foreign key constraint "items_ref_fkey"/, 'the key to the reference table is enforced on the other nodes');
	(undef, undef, $err) = run_sql($n3, "DELETE FROM public.ref1 WHERE id = 1");
	like($err, qr/violates foreign key constraint/, 'the referenced rows are protected');
}
no_leftovers('after a table with rows', 'items');
is(q1($n1, "SELECT count(*) FROM kingres.table_operation"), '0', 'a blocking operation keeps no record');
is(q1($n1, "SELECT count(*) FROM kingres.check_cluster()"), '0', 'check_cluster() is clean');
$_->safe_psql('postgres', "DROP TABLE public.items") for ($n1);


# an identity column GENERATED ALWAYS is copied with its values
$n1->safe_psql('postgres', q{
	CREATE TABLE public.idt (id int PRIMARY KEY, g bigint GENERATED ALWAYS AS IDENTITY (START WITH 7 INCREMENT BY 3), v text);
	INSERT INTO public.idt (id, v) SELECT g, 'v' || g FROM generate_series(1, 300) g;
});
{
	q1($n1, "SELECT nextval('public.idt_g_seq') FROM generate_series(1, 3)");
	my $before = q1($n1, "SELECT md5(string_agg(t::text, ',' ORDER BY id)) || '/' || (SELECT last_value FROM public.idt_g_seq) FROM public.idt t");
	$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('public.idt', 'id', bucket_count => 12)");
	my $after = q1($n2, "SELECT md5(string_agg(t::text, ',' ORDER BY id)) || '/' || (SELECT last_value FROM public.idt_g_seq) FROM public.idt t");
	my ($rows) = q1($n1, "SELECT count(*) FROM public.idt");
	is($rows, '300', 'the table with a generated-always identity column is whole');
	is((split m{/}, $after)[0], (split m{/}, $before)[0], 'its identity values are the source\'s, seen from another node');
	is(q1($n1, "SELECT last_value FROM public.idt_g_seq"), (split m{/}, $before)[1], 'the identity sequence of the origin is where it was');
}
# the copy with an explicit destination, for the other kinds of destination (a table that is not in the catalog):
# to every node and to one node, from SQL
{
	$_->safe_psql('postgres', 'CREATE TABLE public.cp_every (id int PRIMARY KEY, v text); CREATE TABLE public.cp_one (id int PRIMARY KEY, v text)') for @nodes;
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.cp_src (id int PRIMARY KEY, v text, g int GENERATED ALWAYS AS (id * 2) STORED);
		INSERT INTO public.cp_src (id, v) SELECT g, 'v' || g FROM generate_series(1, 4000) g;
	});
	is(q1($n1, "SELECT kingres.internal_copy_routed('public.cp_src', 'public', 'cp_every', 'every_node')"), '12000',
		'the copy to every node stores every row on every node (4000 rows, 3 nodes)');
	is(q1($_, "SELECT count(*) || ',' || sum(id) FROM public.cp_every"), '4000,8002000', 'every node holds all the rows: ' . $_->name) for @nodes;
	is(q1($n1, "SELECT kingres.internal_copy_routed('public.cp_src', 'public', 'cp_one', 'one_node', NULL, NULL, 2)"), '4000',
		'the copy to one node stores every row there');
	is(q1($n1, "SELECT count(*) FROM public.cp_one"), '0', 'and not on the origin');
	is(q1($n2, "SELECT count(*) FROM public.cp_one"), '4000', 'but on the node asked for');
	is(q1($n3, "SELECT count(*) FROM public.cp_one"), '0', 'and not on the third node');
	is(prepared_count($_), '0', 'nothing is prepared after the copy on ' . $_->name) for @nodes;
	# a failed copy leaves nothing
	my (undef, undef, $err) = run_sql($n1, "SET kingres_shard.fault_injection = 'bulk_after_last_batch:error'; SELECT kingres.internal_copy_routed('public.cp_src', 'public', 'cp_one', 'one_node', NULL, NULL, 3)");
	like($err, qr/injected failure at bulk_after_last_batch/, 'a copy that fails at the end says so');
	is(q1($n3, "SELECT count(*) FROM public.cp_one"), '0', 'and the node that was written has nothing');
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.internal_copy_routed('public.cp_src', 'public', 'cp_one', 'one_node', NULL, NULL, 99)");
	like($err, qr/node 99 is not an active primary/, 'a node that does not exist is refused');
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.internal_copy_routed('public.cp_src', 'public', 'cp_one', 'sideways')");
	like($err, qr/unknown destination kind "sideways"/, 'an unknown kind of destination is refused');
	$_->safe_psql('postgres', 'DROP TABLE public.cp_every, public.cp_one') for @nodes;
	$n1->safe_psql('postgres', 'DROP TABLE public.cp_src');
}

# ---------------------------------------------------------------------------
# What is blocked while the copy runs, and nothing else.
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.blk (id int PRIMARY KEY, v text);
	INSERT INTO public.blk SELECT g, 'v' || g FROM generate_series(1, 3000) g;
	CREATE TABLE public.other (id int PRIMARY KEY);
});
{
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_blk_$$";
	append_to_file($gate, 'x');
	my $creator = start_psql($n1,
		"SET kingres_shard.fault_injection = 'distribute_after_shadow:wait:$gate'; "
		  . "SELECT kingres.create_distributed_table('public.blk', 'id', bucket_count => 12);");
	ok(wait_for_fault_file("$gate.reached", 60), 'the creation is at the end of the shadow, before the copy');
	is(q1($n1, "SELECT count(*) FROM public.blk"), '3000', 'a reader is not blocked while the copy runs');
	is(q1($n1, "INSERT INTO public.other VALUES (1) RETURNING id"), '1', 'a writer of another table is not blocked');
	my $writer = start_psql($n1, "INSERT INTO public.blk VALUES (5000, 'late');");
	ok(wait_waiter($n1, 1), 'a writer of the table waits');
	is(q1($n1, "SELECT count(*) FROM public.blk WHERE id = 5000"), '0', 'its row is not there yet');
	unlink $gate;
	my ($out, $err) = finish_psql($creator);
	is($err, '', 'the creation succeeded');
	($out, $err) = finish_psql($writer);
	is($err, '', 'the writer that waited went on after the commit') or diag($err);
	is(q1($n1, "SELECT count(*) FROM public.blk"), '3001', 'the row of the writer is in the new table');
	is(q1($n1, "SELECT count(*) FROM public.blk WHERE id = 5000 AND kingres.bucket_for('public.blk'::regclass, id) = " .
		"substring(tableoid::regclass::text from '_b([0-9]+)\$')::int"), '1', 'and in the partition of its bucket');
	no_leftovers('after the writer that waited', 'blk');
}
# readers go on until the swap, and wait from the swap to the commit
$n1->safe_psql('postgres', q{
	CREATE TABLE public.blk2 (id int PRIMARY KEY, v text);
	INSERT INTO public.blk2 SELECT g, 'v' || g FROM generate_series(1, 500) g;
});
{
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_blk2_$$";
	append_to_file($gate, 'x');
	my $creator = start_psql($n1,
		"SET kingres_shard.fault_injection = 'distribute_before_swap:wait:$gate'; "
		  . "SELECT kingres.create_distributed_table('public.blk2', 'id', bucket_count => 12);");
	ok(wait_for_fault_file("$gate.reached", 60), 'the creation is after the copy, before the swap');
	is(q1($n1, "SELECT count(*) FROM public.blk2"), '500', 'a reader still reads the old table after the copy');
	unlink $gate;
	finish_psql($creator);
	is(q1($n1, "SELECT count(*) FROM public.blk2"), '500', 'the rows are there after the swap');
}
$n1->safe_psql('postgres', q{
	CREATE TABLE public.blk3 (id int PRIMARY KEY, v text);
	INSERT INTO public.blk3 SELECT g, 'v' || g FROM generate_series(1, 500) g;
});
{
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_blk3_$$";
	append_to_file($gate, 'x');
	my $creator = start_psql($n1,
		"SET kingres_shard.fault_injection = 'distribute_after_swap:wait:$gate'; "
		  . "SELECT kingres.create_distributed_table('public.blk3', 'id', bucket_count => 12);");
	ok(wait_for_fault_file("$gate.reached", 60), 'the creation has swapped the table, and has not committed');
	my $reader = start_psql($n1, "SELECT count(*) FROM public.blk3;");
	ok(wait_waiter($n1, 1), 'a reader waits from the swap to the commit');
	unlink $gate;
	my ($out, $err) = finish_psql($creator);
	is($err, '', 'the creation committed');
	($out, $err) = finish_psql($reader);
	is($out, "500\n", 'the reader that waited sees the whole new table');
}

# ---------------------------------------------------------------------------
# A failure anywhere leaves the table as it was.
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.ft (tenant int NOT NULL, id int NOT NULL, v text DEFAULT 'd', s serial, PRIMARY KEY (tenant, id));
	INSERT INTO public.ft SELECT g % 5, g, 'v' || g FROM generate_series(1, 2500) g;
	CREATE INDEX ft_v_idx ON public.ft (v);
	ALTER TABLE public.ft OWNER TO p9_owner;
	GRANT SELECT ON public.ft TO p9_reader;
	SELECT setval('public.ft_s_seq', 4000);
});
my $ft_fp = fp($n1, 'public.ft');
my $ft_md5 = q1($n1, "SELECT md5(string_agg(t::text, ',' ORDER BY tenant, id)) FROM public.ft t");
my $ft_oid = q1($n1, "SELECT 'public.ft'::regclass::oid");
my $colocations = q1($n1, 'SELECT count(*) FROM kingres.colocation');
my $dist_tables = q1($n1, 'SELECT count(*) FROM kingres.dist_table');

sub assert_as_before
{
	my ($label) = @_;
	is(q1($n1, "SELECT relkind::text || ':' || oid FROM pg_class WHERE oid = 'public.ft'::regclass"), "r:$ft_oid", "$label: the table is the same plain table");
	is(q1($n1, "SELECT md5(string_agg(t::text, ',' ORDER BY tenant, id)) FROM public.ft t"), $ft_md5, "$label: it has its rows");
	is(fp($n1, 'public.ft'), $ft_fp, "$label: and its definition, owner, privileges and sequence");
	is(q1($n1, 'SELECT count(*) FROM kingres.colocation'), $colocations, "$label: no colocation group was kept");
	is(q1($n1, 'SELECT count(*) FROM kingres.dist_table'), $dist_tables, "$label: it is not in the catalog");
	for my $n (@nodes)
	{
		is(q1($n, "SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\\_shadow%' OR relname LIKE 'ft\\_b____' OR (relname = 'ft' AND relkind = 'p')"),
			'0', "$label: nothing of the new table is on " . $n->name);
	}
	is(prepared_count($_), '0', "$label: nothing is prepared on " . $_->name) for @nodes;
}

sub fail_at
{
	my ($point) = @_;
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:error';\n"
		. "SELECT kingres.create_distributed_table('public.ft', 'tenant', bucket_count => 12);");
	isnt($rc, 0, "an error at $point fails the creation");
	like($err, qr/injected failure at \Q$point\E/, "it is the injected error at $point");
	assert_as_before("after an error at $point");
}

# the steps of the creation, and every send, wait and step of the batch sender
fail_at($_) for qw(distribute_after_lock distribute_after_shadow copy_before_read bulk_before_probe
  bulk_before_first_batch bulk_before_send bulk_after_send bulk_before_wait bulk_after_first_batch
  bulk_before_last_flush bulk_between_groups bulk_after_last_batch bulk_before_commit copy_after_copy
  distribute_before_swap distribute_after_swap before_prepare after_first_prepare after_prepare);
# the local batch of the group of the origin
fail_at('bulk_before_local_batch');

# a cancel while the copy has not started
{
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_cancel_$$";
	append_to_file($gate, 'x');
	my $creator = start_psql($n1,
		"SET kingres_shard.fault_injection = 'distribute_after_shadow:wait:$gate'; "
		  . "SELECT kingres.create_distributed_table('public.ft', 'tenant', bucket_count => 12);");
	ok(wait_for_fault_file("$gate.reached", 60), 'the creation waits at the end of the shadow');
	is(q1($n1, "SELECT count(pg_cancel_backend(pid)) FROM pg_stat_activity WHERE query LIKE '%create_distributed_table%' AND pid <> pg_backend_pid()"), '1', 'the creation is cancelled');
	my ($out, $err) = finish_psql($creator);
	like($err, qr/canceling statement due to user request/, 'it reports the cancel');
	unlink $gate;
	assert_as_before('after a cancel');
}

# a node that is down
$n3->stop;
{
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.ft', 'tenant', bucket_count => 12);");
	isnt($rc, 0, 'the creation fails while a node is down');
	like($err, qr/node|connect/i, 'and says why');
	is(q1($n1, "SELECT relkind FROM pg_class WHERE oid = 'public.ft'::regclass"), 'r', 'the table is the plain table it was');
	is(q1($n1, "SELECT md5(string_agg(t::text, ',' ORDER BY tenant, id)) FROM public.ft t"), $ft_md5, 'with its rows');
	is(q1($n1, 'SELECT count(*) FROM kingres.dist_table'), $dist_tables, 'and not in the catalog');
}
$n3->start;
assert_as_before('after the node came back');

# a node that dies in the middle of the copy: the origin is held after the first batch, the node is killed, the copy goes on
{
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_kill3_$$";
	append_to_file($gate, 'x');
	my $creator = start_psql($n1,
		"SET kingres_shard.fault_injection = 'bulk_after_first_batch:wait:$gate'; "
		  . "SELECT kingres.create_distributed_table('public.ft', 'tenant', bucket_count => 12);");
	ok(wait_for_fault_file("$gate.reached", 60), 'the creation is held after the first batch of the copy');
	$n3->stop('immediate');
	unlink $gate;
	my ($out, $err) = finish_psql($creator);
	isnt($err, '', 'the creation fails when a node dies in the middle of the copy');
	like($err, qr/node|connect|terminat|server closed/i, 'and says that a node was lost');
	$n3->start;
	assert_as_before('after a node died in the middle of the copy');
}

# the origin dies in the middle of the copy (killed after the first batch)
{
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = 'bulk_after_first_batch:kill';\n"
		. "SELECT kingres.create_distributed_table('public.ft', 'tenant', bucket_count => 12);");
	isnt($rc, 0, 'the origin dies after the first batch');
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	assert_as_before('after the origin died in the middle of the copy');
}
# the origin dies after the prepare, before its own commit: everything is rolled back
{
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = 'after_prepare:kill';\n"
		. "SELECT kingres.create_distributed_table('public.ft', 'tenant', bucket_count => 12);");
	isnt($rc, 0, 'the origin dies after the prepare');
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
	is(prepared_count($n2) + prepared_count($n3), '2', 'both other nodes are left with a prepared transaction');
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2', 'recovery resolves both');
	assert_as_before('after the origin died before its commit');
}
# a copy that loses rows is not a copy: an event trigger of a superuser makes the shadow of this node drop rows
{
	$n1->safe_psql('postgres', q{
		CREATE FUNCTION public.drop_odd() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN IF NEW.id % 2 = 1 THEN RETURN NULL; END IF; RETURN NEW; END';
		CREATE FUNCTION public.sabotage() RETURNS event_trigger LANGUAGE plpgsql AS $$
		DECLARE r record;
		BEGIN
		  FOR r IN SELECT objid::regclass::text AS t FROM pg_event_trigger_ddl_commands() WHERE command_tag = 'CREATE TABLE' AND object_type = 'table'
		  LOOP
		    IF r.t ~ '^(public\.)?ft_b[0-9]{4}$' THEN
		      EXECUTE format('CREATE TRIGGER drop_odd BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION public.drop_odd()', r.t);
		    END IF;
		  END LOOP;
		END $$;
		CREATE EVENT TRIGGER sabotage ON ddl_command_end WHEN TAG IN ('CREATE TABLE') EXECUTE FUNCTION public.sabotage();
	});
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.ft', 'tenant', bucket_count => 12);");
	isnt($rc, 0, 'a copy that loses rows fails');
	like($err, qr/the copy of table "ft" is not complete: the table holds 2500 rows, 2500 were read and \d+ were stored/, 'and says how many rows were stored');
	assert_as_before('after a copy that lost rows');
	$n1->safe_psql('postgres', 'DROP EVENT TRIGGER sabotage; DROP FUNCTION public.sabotage(); DROP FUNCTION public.drop_odd()');
}

# a node that runs another version of the extension is named, and nothing changes
{
	$n3->safe_psql('postgres', "UPDATE pg_catalog.pg_extension SET extversion = '0.6' WHERE extname = 'kingres_shard'");
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.ft', 'tenant', bucket_count => 12);");
	isnt($rc, 0, 'the creation is refused while a node has another version');
	like($err, qr/node 3 \(\S+:\d+\) has kingres_shard 0\.6, a table operation needs 0.8 on every node/, 'the error names the node and the versions');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_reference_table('public.ft')");
	like($err, qr/node 3 \(\S+:\d+\) has kingres_shard 0\.6/, 'also for a reference table');
	$n3->safe_psql('postgres', "UPDATE pg_catalog.pg_extension SET extversion = '0.8' WHERE extname = 'kingres_shard'");
	assert_as_before('after the version handshake refused it');
}

# the origin dies after its commit, before the others: recovery commits them, and the table is whole
{
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = 'after_commit:kill';\n"
		. "SELECT kingres.create_distributed_table('public.ft', 'tenant', bucket_count => 12);");
	isnt($rc, 0, 'the origin dies after its own commit');
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
	is(prepared_count($n2) + prepared_count($n3), '2', 'both other nodes are left with a prepared transaction');
	is(q1($n1, "SELECT relkind FROM pg_class WHERE oid = 'public.ft'::regclass"), 'p', 'the origin has the new table');
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2', 'recovery commits both');
	is(prepared_count($_), '0', 'nothing is left prepared on ' . $_->name) for @nodes;
	for my $n (@nodes)
	{
		is(q1($n, "SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.ft'::regclass"), '1', $n->name . " knows the table");
		is(q1($n, "SELECT md5(string_agg(t::text, ',' ORDER BY tenant, id)) FROM public.ft t"), $ft_md5, "the whole table is read from " . $n->name);
		is(fp($n, 'public.ft'), $ft_fp, "the definition is the source's on " . $n->name);
	}
	my $total = 0;
	for my $i (0 .. 2)
	{
		my ($wrong, $rows) = split /\|/, q1($nodes[$i], "SELECT coalesce(sum(wrong), 0) || '|' || coalesce(sum(n), 0) FROM public.local_rows('public.ft', 'tenant')");
		is($wrong, '0', $nodes[$i]->name . ' has no row in a wrong bucket');
		$total += $rows;
	}
	is($total, 2500, 'the rows of the three nodes add up to the table after recovery');
	no_leftovers('after the recovery of a crash after the commit', 'ft');
	is(q1($n1, "SELECT count(*) FROM kingres.check_cluster()"), '0', 'check_cluster() is clean');
}

# a colocation group that a table operation claims, or a bucket that is being moved, excludes the creation, the DDL on the
# group and a move of its buckets, and the other way round
{
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.anchor (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('public.anchor', 'id', bucket_count => 12);
		CREATE TABLE public.joiner (id int PRIMARY KEY, v text);
		INSERT INTO public.joiner SELECT g, 'j' FROM generate_series(1, 50) g;
	});
	my $grp = q1($n1, "SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.anchor'::regclass");
	$n1->safe_psql('postgres', qq{
		INSERT INTO kingres.table_operation (kind, mode, phase, spec, started_at, updated_at) VALUES ('alter', 'online', 'copying', '{}', now(), now());
		INSERT INTO kingres.table_operation_lock (colocation_id, op_id) VALUES ($grp, (SELECT max(op_id) FROM kingres.table_operation));
	});
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.joiner', 'id', colocate_with => 'public.anchor')");
	like($err, qr/colocation group $grp is busy: a table operation uses it/, 'a creation in a claimed group is refused');
	(undef, undef, $err) = run_sql($n1, "ALTER TABLE public.anchor ADD COLUMN extra int");
	like($err, qr/colocation group $grp is used by table operation/, 'DDL on a table of a claimed group is refused');
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.internal_begin_move($grp, 0, 1)");
	like($err, qr/colocation group $grp is used by table operation/, 'the start of a bucket move in a claimed group is refused');
	is(q1($n1, "SELECT count(*) FROM kingres.bucket_move"), '0', 'and no move was recorded');
	$n1->safe_psql('postgres', 'DELETE FROM kingres.table_operation_lock; DELETE FROM kingres.table_operation');
	# the other way round: a bucket that is being moved refuses the creation
	$n1->safe_psql('postgres', qq{
		INSERT INTO kingres.bucket_move (colocation_id, bucket, source_group, target_group, phase, detail, started_at, updated_at)
		VALUES ($grp, 0, 0, 1, 'copying', 'test', now(), now());
	});
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.joiner', 'id', colocate_with => 'public.anchor')");
	like($err, qr/colocation group $grp is busy: a bucket is being moved/, 'a creation in a group with a move is refused');
	$n1->safe_psql('postgres', 'DELETE FROM kingres.bucket_move');
	# and with nothing in the way the table joins the group, rows and all
	$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('public.joiner', 'id', colocate_with => 'public.anchor')");
	is(q1($n3, "SELECT count(*) FROM public.joiner"), '50', 'a table with rows joins the group of another one');
	is(q1($n1, "SELECT count(DISTINCT colocation_id) FROM kingres.dist_table WHERE relid IN ('public.anchor'::regclass, 'public.joiner'::regclass)"), '1', 'in its colocation group');
}

# a foreign key to a colocated distributed table is carried (such a key exists on an ordinary table only when it was made
# with enable_routing = off: with several groups the hook refuses it at creation) and is enforced on every node
{
	$n1->safe_psql('postgres', q{
		INSERT INTO public.anchor SELECT g, 'a' || g FROM generate_series(1, 60) g;
		SET kingres_shard.enable_routing = off;
		CREATE TABLE public.kids (id int, parent int, PRIMARY KEY (id, parent));
		ALTER TABLE public.kids ADD CONSTRAINT kids_parent_fkey FOREIGN KEY (parent) REFERENCES public.anchor (id);
		RESET kingres_shard.enable_routing;
		SET session_replication_role = replica;
		INSERT INTO public.kids SELECT g, g FROM generate_series(1, 60) g;
		RESET session_replication_role;
	});
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.kids', 'id')");
	like($err, qr/foreign key "kids_parent_fkey" does not use the distribution column|another colocation group|distribution columns of both tables/, 'a key that does not use the distribution column of the table is refused');
	$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('public.kids', 'parent', colocate_with => 'public.anchor')");
	for my $n (@nodes)
	{
		is(q1($n, "SELECT count(*) FROM public.kids"), '60', 'the table with the key is whole from ' . $n->name);
		is(q1($n, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.kids'::regclass AND conname = 'kids_parent_fkey' AND conparentid = 0"), '1',
			'it has its key on ' . $n->name);
	}
	(undef, undef, $err) = run_sql($n2, "INSERT INTO public.kids VALUES (500, 9999)");
	like($err, qr/violates foreign key constraint "kids_parent_fkey"/, 'the key is enforced: a child without a parent is refused (from node 2)');
	is(q1($n3, "INSERT INTO public.kids VALUES (501, 7) RETURNING id"), '501', 'and a child with a parent is accepted (from node 3)');
	(undef, undef, $err) = run_sql($n3, "DELETE FROM public.anchor WHERE id = 7");
	like($err, qr/violates foreign key constraint "kids_parent_fkey(_[0-9]+)?"/, 'a parent with children cannot be deleted (from node 3)');
	is(q1($n1, "SELECT count(*) FROM kingres.check_cluster()"), '0', 'check_cluster() is clean');
	my $total = 0;
	for my $i (0 .. 2)
	{
		my ($wrong, $rows) = split /\|/, q1($nodes[$i], "SELECT coalesce(sum(wrong), 0) || '|' || coalesce(sum(n), 0) FROM public.local_rows('public.kids', 'parent')");
		is($wrong, '0', $nodes[$i]->name . ' holds no child in the bucket of another parent');
		$total += $rows;
	}
	is($total, 61, 'the rows of the three nodes add up (60 and the one added)');
}

# a table that is refused says so, and changes nothing
{
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.has_trg (id int PRIMARY KEY, k int);
		CREATE FUNCTION public.t_fn() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END';
		CREATE TRIGGER t1 BEFORE INSERT ON public.has_trg FOR EACH ROW EXECUTE FUNCTION public.t_fn();
		INSERT INTO public.has_trg VALUES (1, 1);
	});
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.has_trg', 'id')");
	like($err, qr/trigger "t1" is not supported yet/, 'a table with a trigger is refused');
	is(q1($n1, 'SELECT count(*) FROM public.has_trg'), '1', 'and keeps its row');
}

done_testing();
