# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/144_online_distribute.pl: create_distributed_table_concurrently() and
# create_reference_table_concurrently() (design/shard-phase9.md, increment 9.3).
#
# Three nodes.  A table with rows, an owner, privileges, comments, a serial column, an identity column,
# a key to a reference table and indexes becomes distributed while it stays in use; the phases are also run
# one by one with kingres.table_operation_advance().  The rows are checked against the digest of the old
# copy, against the buckets of their keys and with the scan; the map version rose; nothing is left.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2, $n3) = make_cluster('od', 3);
my @nodes = ($n1, $n2, $n3);

$_->safe_psql('postgres', q{
	CREATE ROLE p93_owner; CREATE ROLE p93_reader; CREATE ROLE p93_writer;
}) for @nodes;

sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

# what a table looks like, object by object
my $fingerprint_sql = q{
CREATE FUNCTION public.fingerprint(t regclass) RETURNS SETOF text LANGUAGE sql AS $$
  SELECT 'column ' || a.attnum || ' ' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
         || ' notnull=' || a.attnotnull || ' identity=' || a.attidentity::text
         || ' default=' || coalesce(pg_get_expr(d.adbin, d.adrelid), '-')
         || ' acl=' || coalesce(a.attacl::text, '-') || ' comment=' || coalesce(col_description(t, a.attnum), '-')
    FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
   WHERE a.attrelid = t AND a.attnum > 0 AND NOT a.attisdropped
  UNION ALL
  SELECT 'constraint ' || c.conname || ' ' || pg_get_constraintdef(c.oid) || ' validated=' || c.convalidated
    FROM pg_constraint c WHERE c.conrelid = t AND c.conparentid = 0
  UNION ALL
  SELECT 'index ' || ic.relname || ' ' || replace(pg_get_indexdef(i.indexrelid), ' ONLY ', ' ')
    FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid WHERE i.indrelid = t
  UNION ALL
  SELECT 'table owner=' || c.relowner::regrole || ' acl=' || coalesce(c.relacl::text, '-') || ' comment=' || coalesce(obj_description(t, 'pg_class'), '-')
    FROM pg_class c WHERE c.oid = t
$$;
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
};
$_->safe_psql('postgres', $fingerprint_sql) for @nodes;

sub fp { my ($node, $t) = @_; return q1($node, "SELECT line FROM public.fingerprint('$t') line ORDER BY 1"); }

sub no_leftovers
{
	my ($label) = @_;
	for my $n (@nodes)
	{
		is(q1($n, "SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\\_shadow%' OR relname LIKE 'tlog\\_%'"),
			'0', "$label: no shadow or log table on " . $n->name);
	}
	is(q1($n1, "SELECT count(*) FROM pg_trigger WHERE tgname LIKE 'kingres\\_capture%' OR tgname LIKE 'kingres\\_notrunc%'"), '0',
		"$label: no capture trigger");
	is(prepared_count($_), '0', "$label: nothing is prepared on " . $_->name) for @nodes;
	is(q1($n1, "SELECT count(*) FROM kingres.table_operation_lock"), '0', "$label: no group is claimed");
}

# every row of the table is in the partition of its bucket, on the node of that bucket, once
sub rows_in_place
{
	my ($label, $t, $key, $expect) = @_;
	my ($total, $wrong) = (0, 0);
	for my $n (@nodes)
	{
		my $r = q1($n, "SELECT coalesce(sum(n), 0) || ',' || coalesce(sum(wrong), 0) FROM public.local_rows('$t', '$key')");
		my ($a, $b) = split /,/, $r;
		$total += $a;
		$wrong += $b;
	}
	is($total, $expect, "$label: $expect rows in the partitions of the nodes");
	is($wrong, 0, "$label: none of them is in the partition of another bucket");
}

# ---------------------------------------------------------------------------
# A reference table the new table points to, and the table itself
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.regions (id int PRIMARY KEY, name text);
	INSERT INTO public.regions SELECT g, 'r' || g FROM generate_series(1, 5) g;
	SELECT kingres.create_reference_table('public.regions');

	CREATE TABLE public.orders (
		tenant int NOT NULL,
		id bigint NOT NULL,
		region int REFERENCES public.regions (id),
		note text DEFAULT 'none',
		qty int CHECK (qty >= 0),
		s serial,
		seq_id int GENERATED BY DEFAULT AS IDENTITY,
		PRIMARY KEY (tenant, id)
	);
	CREATE UNIQUE INDEX orders_uq ON public.orders (tenant, s);
	CREATE INDEX orders_note_idx ON public.orders (note) WHERE qty > 5;
	INSERT INTO public.orders (tenant, id, region, qty) SELECT g % 17, g, 1 + g % 5, g % 11 FROM generate_series(1, 3000) g;
	ALTER TABLE public.orders OWNER TO p93_owner;
	GRANT SELECT ON public.orders TO p93_reader;
	GRANT UPDATE (note) ON public.orders TO p93_writer;
	COMMENT ON TABLE public.orders IS 'the orders';
	COMMENT ON COLUMN public.orders.note IS 'a note';
});
my $before_fp = fp($n1, 'public.orders');
my $digest_before = q1($n1, "SELECT n_rows || ',' || n_hash FROM kingres.internal_table_digest('public.orders')");
$_->safe_psql('postgres', q{
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
}) for @nodes;
my $version_before = q1($n1, 'SELECT version FROM kingres.map_version');

# ---------------------------------------------------------------------------
# the refusals come before anything is recorded
# ---------------------------------------------------------------------------
{
	my (undef, undef, $err) = run_sql($n1, "BEGIN; SELECT kingres.create_distributed_table_concurrently('public.orders', 'tenant'); COMMIT;");
	like($err, qr/cannot run inside a transaction block/, 'the online variant refuses a transaction block');
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table_concurrently('public.orders', 'nocolumn')");
	like($err, qr/column|does not exist/i, 'a column that does not exist is refused');
	$n1->safe_psql('postgres', 'CREATE TABLE public.nokey (a int, b text)');
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table_concurrently('public.nokey', 'a')");
	like($err, qr/primary key or a unique index over NOT NULL columns/, 'a table without a key is refused, with the way out');
	$n1->safe_psql('postgres', 'DROP TABLE public.nokey');
	$n2->safe_psql('postgres', 'CREATE TABLE public.on_n2 (a int PRIMARY KEY)');
	(undef, undef, $err) = run_sql($n2, "SELECT kingres.create_distributed_table_concurrently('public.on_n2', 'a')");
	like($err, qr/map group/i, 'only the map group primary runs it');
	is(q1($n1, 'SELECT count(*) FROM kingres.table_operation'), '0', 'a refusal leaves no record');
}

# ---------------------------------------------------------------------------
# the online variant, in one call
# ---------------------------------------------------------------------------
my $op = q1($n1, "SELECT kingres.create_distributed_table_concurrently('public.orders', 'tenant', bucket_count => 12)");
like($op, qr/^\d+$/, 'it returns the id of the operation');
is(q1($n1, "SELECT phase || '|' || coalesce(verified, '-') FROM kingres.table_operation WHERE op_id = $op"), 'done|ok', 'the operation is done and verified');
is(q1($n1, "SELECT method || ',' || dist_column FROM kingres.dist_table WHERE relid = 'public.orders'::regclass"), 'hash,tenant', 'the table is distributed');
cmp_ok(q1($n1, 'SELECT version FROM kingres.map_version'), '>', $version_before, 'the map version rose');
is(q1($n1, 'SELECT count(*) FROM public.orders'), '3000', 'the scan sees every row');
rows_in_place('after the online variant', 'public.orders', 'tenant', 3000);
for my $n (@nodes)
{
	is(q1($n, "SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.orders'::regclass"), '1', 'the table is in the catalog of ' . $n->name);
}
is(fp($n1, 'public.orders'), $before_fp, 'columns, defaults, constraints, indexes, owner, privileges and comments are the ones of the old table (map group)')
  or diag(fp($n1, 'public.orders'));
for my $n ($n2, $n3)
{
	my $theirs = fp($n, 'public.orders');
	my $mine = $before_fp;
	$mine =~ s/ validated=false/ validated=true/g;
	is($theirs, $mine, 'the same on ' . $n->name) or diag($theirs);
}
is(q1($n1, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.orders'::regclass AND contype = 'f' AND NOT convalidated"), '0',
	'the key to the reference table is validated');
# the digest of the new table equals the digest of the old one
{
	my @parts = map { q1($_, "SELECT n_rows || ',' || n_hash FROM kingres.internal_table_digest('public.orders')") } @nodes;
	my $sum = q1($n1, "SELECT sum(split_part(x, ',', 1)::bigint) || ',' || sum(split_part(x, ',', 2)::numeric) FROM unnest(ARRAY['" . join("','", @parts) . "']) x");
	is($sum, $digest_before, 'the rows of the nodes together are the rows the old table had');
}
# the old copy is kept, out of the way
my $old = q1($n1, "SELECT old_name FROM kingres.table_operation_item WHERE op_id = $op");
like($old, qr/^kingres_old_\d+$/, 'the old copy has a name that carries its OID');
is(q1($n1, "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'kingres' AND c.relname = '$old'"), '1', 'the old copy is in the schema kingres');
is(q1($n1, "SELECT count(*) FROM kingres.$old"), '3000', 'and holds the rows');
is(q1($n1, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'kingres.$old'::regclass AND contype = 'f'"), '0', 'the old copy holds no foreign key (it must not hold up the table it pointed at)');
is(q1($n1, "SELECT count(*) FROM pg_trigger WHERE tgrelid = 'kingres.$old'::regclass AND NOT tgisinternal"), '0', 'and no capture trigger');
is(q1($n1, "SELECT has_table_privilege('p93_reader', 'kingres.$old', 'SELECT') OR has_table_privilege('p93_writer', 'kingres.$old', 'UPDATE') OR has_table_privilege('public', 'kingres.$old', 'SELECT')"), 'f', 'only its owner can read it');
# the sequences go on from the largest values on each node
$n1->safe_psql('postgres', "INSERT INTO public.orders (tenant, id, region, qty) VALUES (3, 100000, 2, 1)");
is(q1($n1, "SELECT s IS NOT NULL AND seq_id IS NOT NULL FROM public.orders WHERE id = 100000"), 't', 'a row gets its serial and identity values after the switch');
for my $n (@nodes)
{
	is(q1($n, "SELECT (SELECT last_value FROM public.orders_s_seq) >= public.local_max('public.orders', 's')"), 't', 'the serial sequence is at least the largest value of the node: ' . $n->name);
	is(q1($n, "SELECT (SELECT last_value FROM public.orders_seq_id_seq) >= public.local_max('public.orders', 'seq_id')"), 't', 'the identity sequence is at least the largest value of the node: ' . $n->name);
}
is(q1($n1, "SELECT count(*) FROM pg_depend d JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S' WHERE d.refobjid = 'public.orders'::regclass AND d.deptype = 'a'"), '1', 'the serial sequence belongs to the new table');
# a statement that was planned before the swap works afterwards
is(q1($n1, "SELECT count(*) FROM kingres.table_operations WHERE op_id = $op AND phase = 'done'"), '1', 'the view shows the operation');
is(q1($n1, "SELECT kingres.cleanup_table_operations(true)"), '1', 'cleanup with force drops the old copy');
is(q1($n1, "SELECT count(*) FROM pg_class WHERE relname = '$old'"), '0', 'it is gone');
no_leftovers('after the online variant');
is(q1($n1, "SELECT count(*) FROM kingres.check_cluster()"), '0', 'check_cluster() is clean');

# ---------------------------------------------------------------------------
# the options and the values of the sequences are carried (identity and serial, not the defaults)
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.seqt (tenant int NOT NULL, id int GENERATED BY DEFAULT AS IDENTITY (START 5 INCREMENT 3 MINVALUE 2 MAXVALUE 1000 CACHE 4 CYCLE),
	                          s serial, v text, PRIMARY KEY (tenant, id));
	ALTER SEQUENCE seqt_s_seq INCREMENT BY 7 MINVALUE 3 MAXVALUE 500 START 9 RESTART WITH 9 CACHE 3 CYCLE;
	INSERT INTO public.seqt (tenant, v) SELECT g, 'x' FROM generate_series(1, 8) g;
});
{
	my $opt_sql = q{SELECT string_agg(c.relname || ':' || s.seqstart || ',' || s.seqincrement || ',' || s.seqmin || ',' || s.seqmax || ',' || s.seqcache || ',' || s.seqcycle, ';' ORDER BY c.relname)
	                  FROM pg_sequence s JOIN pg_class c ON c.oid = s.seqrelid WHERE c.relname LIKE 'seqt\_%'};
	my $val_sql = q{SELECT (SELECT last_value || ',' || is_called FROM public.seqt_id_seq) || ';' || (SELECT last_value || ',' || is_called FROM public.seqt_s_seq)};
	my $opts = q1($n1, $opt_sql);
	my $vals = q1($n1, $val_sql);
	q1($n1, "SELECT kingres.create_distributed_table_concurrently('public.seqt', 'tenant', colocate_with => 'public.orders')");
	is(q1($n1, $opt_sql), $opts, 'the options of the sequences are the same at the origin');
	is(q1($n1, $val_sql), $vals, 'and so are their values, so that no value is given twice there');
	is(q1($_, $opt_sql), $opts, 'the options of the sequences are the same on ' . $_->name) for ($n2, $n3);
	$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
}

# ---------------------------------------------------------------------------
# the phases one by one
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.items (tenant int NOT NULL, id int NOT NULL, v text, PRIMARY KEY (tenant, id));
	INSERT INTO public.items SELECT g % 7, g, 'v' || g FROM generate_series(1, 500) g;
});
$op = q1($n1, "SELECT kingres.begin_table_operation('distribute', 'public.items', '{\"column\": \"tenant\", \"colocate_with\": \"" . q1($n1, "SELECT 'public.orders'::regclass::oid") . "\"}')");
is(q1($n1, "SELECT phase FROM kingres.table_operation WHERE op_id = $op"), 'preparing', 'a recorded operation is in phase preparing');
is(q1($n1, "SELECT colocation_id FROM kingres.table_operation_lock WHERE op_id = $op"), q1($n1, "SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.orders'::regclass"), 'it claims the group it joins');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'shadow')"), 'copying', 'the shadow phase ends in copying');
is(q1($n1, "SELECT count(*) FROM pg_trigger WHERE tgname = 'kingres_capture_$op'"), '1', 'the capture trigger exists');
{
	my (undef, undef, $err) = run_sql($n1, "TRUNCATE public.items");
	like($err, qr/is being rebuilt by table operation $op/, 'TRUNCATE is refused while the operation runs');
	(undef, undef, $err) = run_sql($n1, "ALTER TABLE public.items ADD COLUMN x int");
	like($err, qr/is being rebuilt by table operation $op/, 'DDL on the source is refused');
	(undef, undef, $err) = run_sql($n1, "DROP TABLE public.items");
	like($err, qr/is being rebuilt by table operation $op/, 'DROP of the source is refused');
}
$n1->safe_psql('postgres', "INSERT INTO public.items VALUES (1, 1000, 'late'); UPDATE public.items SET v = 'upd' WHERE id = 5; DELETE FROM public.items WHERE id = 6");
is(q1($n1, "SELECT count(*) FROM kingres.tlog_${op}_1"), '3', 'three changes are logged');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'copying')"), 'catching_up', 'the copy phase ends in catching_up');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'catching_up')"), 'switching', 'the replay ends in switching');
is(q1($n1, "SELECT count(*) FROM kingres.tlog_${op}_1"), '0', 'the log is empty');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'switching')"), 'cleaning', 'the switch ends in cleaning');
is(q1($n1, "SELECT kingres.table_operation_advance($op, 'cleaning')"), 'done', 'cleaning ends in done');
is(q1($n1, "SELECT count(*) FROM public.items"), '500', 'the scan sees 500 rows (500 - 1 deleted + 1 inserted)');
is(q1($n1, "SELECT v FROM public.items WHERE id = 5"), 'upd', 'the update is there');
is(q1($n1, "SELECT count(*) FROM public.items WHERE id = 6"), '0', 'the delete is there');
is(q1($n1, "SELECT count(*) FROM public.items WHERE id = 1000"), '1', 'the insert is there');
is(q1($n1, "SELECT d.colocation_id FROM kingres.dist_table d WHERE d.relid = 'public.items'::regclass"),
   q1($n1, "SELECT d.colocation_id FROM kingres.dist_table d WHERE d.relid = 'public.orders'::regclass"), 'the table joined the group of orders');
rows_in_place('after the phases', 'public.items', 'tenant', 500);
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
no_leftovers('after the phases');

# ---------------------------------------------------------------------------
# the reference variant
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.codes (code text PRIMARY KEY, label text, n int);
	INSERT INTO public.codes SELECT 'c' || g, 'label ' || g, g FROM generate_series(1, 200) g;
	ALTER TABLE public.codes OWNER TO p93_owner;
	GRANT SELECT ON public.codes TO p93_reader;
	COMMENT ON TABLE public.codes IS 'codes';
});
$op = q1($n1, "SELECT kingres.create_reference_table_concurrently('public.codes')");
is(q1($n1, "SELECT phase || '|' || verified FROM kingres.table_operation WHERE op_id = $op"), 'done|ok', 'the reference operation is done and verified');
is(q1($n1, "SELECT method FROM kingres.dist_table WHERE relid = 'public.codes'::regclass"), 'reference', 'the table is a reference table');
for my $n (@nodes)
{
	is(q1($n, "SELECT count(*) || ',' || sum(n) FROM public.codes"), '200,20100', 'every node holds the 200 rows: ' . $n->name);
}
is(fp($n2, 'public.codes'), fp($n1, 'public.codes'), 'the copies carry owner, privileges and comments');
$n1->safe_psql('postgres', "INSERT INTO public.codes VALUES ('new', 'n', 1)");
is(q1($n3, "SELECT count(*) FROM public.codes WHERE code = 'new'"), '1', 'a write reaches every node');
$n1->safe_psql('postgres', 'SELECT kingres.cleanup_table_operations(true)');
no_leftovers('after the reference variant');
is(q1($n1, "SELECT count(*) FROM kingres.check_cluster()"), '0', 'check_cluster() is clean at the end');

done_testing();
