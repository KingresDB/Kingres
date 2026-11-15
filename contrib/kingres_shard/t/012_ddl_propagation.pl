# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# DDL propagation (design/shard-phase2a.md, section 4): every supported
# statement on a distributed or reference table is visible on all nodes and in
# all bucket partitions; the statements that are not supported are refused and
# leave nothing behind; DDL on a node that is not the map group primary is
# refused; the propagation is part of the distributed transaction.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node make_cluster keys_of_group run_sql prepared_count);

my ($n1, $n2, $n3) = make_cluster('dd', 3);
my @nodes = ($n1, $n2, $n3);

$n1->safe_psql('postgres', q{
	CREATE ROLE ddl_role;
	CREATE TABLE public.d1 (id int PRIMARY KEY, a text, b int);
	CREATE TABLE public.d2 (id int PRIMARY KEY, c text);
	CREATE TABLE public.ref1 (id int PRIMARY KEY, name text);
	CREATE TABLE public.loc (i int);
	SELECT kingres.create_distributed_table('public.d1', 'id', bucket_count => 12);
	SELECT kingres.create_distributed_table('public.d2', 'id', colocate_with => 'public.d1');
	SELECT kingres.create_reference_table('public.ref1');
	INSERT INTO public.ref1 VALUES (1, 'r1'), (2, 'r2');
});
$_->safe_psql('postgres', 'CREATE ROLE ddl_role') for ($n2, $n3);
$n1->safe_psql('postgres', "INSERT INTO public.d1 VALUES ($_, 'a$_', $_)") for (1 .. 12);

# The output of a query on every node, which must be the same.
sub same_all
{
	my ($sql, $label) = @_;
	my @out = map { $_->safe_psql('postgres', $sql) } @nodes;
	is($out[1], $out[0], "$label: node 2 equals node 1");
	is($out[2], $out[0], "$label: node 3 equals node 1");
	return $out[0];
}

# Every partition of table $1 on this node has the property of the query $2
# (with %s for the partition's OID).
sub all_partitions
{
	my ($table, $cond, $label) = @_;
	my @out = map {
		$_->safe_psql('postgres',
			"SELECT count(*) > 0 AND bool_and($cond) FROM pg_inherits i, LATERAL (SELECT i.inhrelid AS oid) p WHERE i.inhparent = 'public.$table'::regclass")
	} @nodes;
	is(join(',', @out), 't,t,t', "$label: in every bucket partition on every node");
}

sub columns_of
{
	my ($table) = @_;
	return "SELECT string_agg(attname || ':' || format_type(atttypid, atttypmod), ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid = 'public.$table'::regclass AND attnum > 0 AND NOT attisdropped";
}

sub ddl_ok
{
	my ($node, $sql, $label) = @_;
	my ($rc, $out, $err) = run_sql($node, $sql);
	is($err, '', $label);
}

sub ddl_refused
{
	my ($node, $sql, $re, $label) = @_;
	my ($rc, $out, $err) = run_sql($node, $sql);
	like($err, $re, $label);
}

# ---------------------------------------------------------------------------
# ALTER TABLE.
# ---------------------------------------------------------------------------
ddl_ok($n1, 'ALTER TABLE public.d1 ADD COLUMN x int', 'ADD COLUMN');
is(same_all(columns_of('d1'), 'ADD COLUMN'), 'id:integer,a:text,b:integer,x:integer', 'the column is added');
all_partitions('d1', "EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = p.oid AND a.attname = 'x')", 'the new column');

ddl_ok($n1, "ALTER TABLE public.d1 ADD COLUMN y text DEFAULT 'dflt' NOT NULL", 'ADD COLUMN with a constant default');
is(same_all("SELECT y FROM public.d1 WHERE id = 1", 'default value of the added column'), 'dflt', 'rows got the default');
ddl_ok($n1, "ALTER TABLE public.d1 ADD COLUMN z numeric DEFAULT 1.5 CHECK (z > 0)", 'ADD COLUMN with a default and a check');

my $before = $n1->safe_psql('postgres', columns_of('d1'));
ddl_refused($n1, 'ALTER TABLE public.d1 ADD COLUMN t timestamptz DEFAULT now()', qr/not a constant/, 'a default that is not a constant is refused');
ddl_refused($n1, 'ALTER TABLE public.d1 ADD COLUMN s serial', qr/serial type/, 'a serial column is refused');
ddl_refused($n1, 'ALTER TABLE public.d1 ADD COLUMN i int GENERATED ALWAYS AS IDENTITY', qr/identity/, 'an identity column is refused');
is(same_all(columns_of('d1'), 'after refused ADD COLUMN'), $before, 'nothing was added by the refused statements');

ddl_ok($n1, 'ALTER TABLE public.d1 DROP COLUMN x', 'DROP COLUMN');
is(same_all(columns_of('d1'), 'DROP COLUMN'), 'id:integer,a:text,b:integer,y:text,z:numeric', 'the column is dropped');
ddl_refused($n1, 'ALTER TABLE public.d1 DROP COLUMN id', qr/ALTER TABLE is not supported on distributed tables yet.*distribution column/s, 'the distribution column cannot be dropped');
ddl_refused($n1, 'ALTER TABLE public.d1 DROP COLUMN b CASCADE', qr/CASCADE/, 'DROP COLUMN CASCADE is refused');
ddl_refused($n1, 'ALTER TABLE public.d1 ALTER COLUMN id TYPE bigint', qr/not supported on distributed tables yet/, 'the type of the distribution column cannot change');
ddl_refused($n1, 'ALTER TABLE public.d1 ALTER COLUMN b TYPE bigint', qr/ALTER TABLE is not supported on distributed tables yet/, 'ALTER COLUMN TYPE is refused');

ddl_ok($n1, "ALTER TABLE public.d1 ALTER COLUMN b SET DEFAULT 77", 'SET DEFAULT');
is(same_all("SELECT pg_get_expr(adbin, adrelid) FROM pg_attrdef WHERE adrelid = 'public.d1'::regclass AND adnum = 3", 'SET DEFAULT'), '77', 'the default is set');
ddl_ok($n1, "ALTER TABLE public.d1 ALTER COLUMN b DROP DEFAULT", 'DROP DEFAULT');
is(same_all("SELECT count(*) FROM pg_attrdef WHERE adrelid = 'public.d1'::regclass AND adnum = 3", 'DROP DEFAULT'), '0', 'the default is gone');
ddl_ok($n1, "ALTER TABLE public.d1 ALTER COLUMN b SET NOT NULL", 'SET NOT NULL');
is(same_all("SELECT attnotnull FROM pg_attribute WHERE attrelid = 'public.d1'::regclass AND attname = 'b'", 'SET NOT NULL'), 't', 'NOT NULL is set');
all_partitions('d1', "(SELECT attnotnull FROM pg_attribute a WHERE a.attrelid = p.oid AND a.attname = 'b')", 'NOT NULL');
ddl_ok($n1, "ALTER TABLE public.d1 ALTER COLUMN b DROP NOT NULL", 'DROP NOT NULL');
is(same_all("SELECT attnotnull FROM pg_attribute WHERE attrelid = 'public.d1'::regclass AND attname = 'b'", 'DROP NOT NULL'), 'f', 'NOT NULL is gone');

ddl_ok($n1, "ALTER TABLE public.d1 ADD CONSTRAINT d1_b_pos CHECK (b >= 0)", 'ADD CONSTRAINT CHECK');
ddl_ok($n1, "ALTER TABLE public.d1 ADD CONSTRAINT d1_id_a_key UNIQUE (id, a)", 'ADD CONSTRAINT UNIQUE with the distribution column');
is(same_all("SELECT string_agg(conname, ',' ORDER BY conname) FROM pg_constraint WHERE conrelid = 'public.d1'::regclass AND contype IN ('c', 'p', 'u')", 'constraints'),
	'd1_b_pos,d1_id_a_key,d1_pkey,d1_z_check', 'the constraints are on every node');
ddl_refused($n1, "ALTER TABLE public.d1 ADD CONSTRAINT d1_a_key UNIQUE (a)", qr/unique constraint.*partition|distribution|partition key/i, 'a unique constraint without the distribution column is refused (by PostgreSQL, before anything is sent)');
ddl_refused($n1, "ALTER TABLE public.d1 ADD CONSTRAINT d1_x EXCLUDE USING btree (id WITH =)", qr/not supported yet/, 'an exclusion constraint is refused');
ddl_ok($n1, "ALTER TABLE public.d1 DROP CONSTRAINT d1_b_pos", 'DROP CONSTRAINT');
is(same_all("SELECT count(*) FROM pg_constraint WHERE conname = 'd1_b_pos'", 'DROP CONSTRAINT'), '0', 'the constraint is gone');
ddl_ok($n1, "ALTER TABLE public.d1 ADD CONSTRAINT d1_b_nv CHECK (b >= 0) NOT VALID", 'ADD CONSTRAINT NOT VALID');
ddl_ok($n1, "ALTER TABLE public.d1 VALIDATE CONSTRAINT d1_b_nv", 'VALIDATE CONSTRAINT');
is(same_all("SELECT convalidated FROM pg_constraint WHERE conname = 'd1_b_nv' AND conrelid = 'public.d1'::regclass", 'VALIDATE'), 't', 'the constraint is validated everywhere');
ddl_ok($n1, "ALTER TABLE public.d1 DROP CONSTRAINT d1_b_nv", 'DROP CONSTRAINT again');

# Foreign keys.
ddl_ok($n1, "ALTER TABLE public.d2 ADD CONSTRAINT d2_id_fk FOREIGN KEY (id) REFERENCES public.d1 (id)", 'a foreign key between colocated tables on the distribution column');
is(same_all("SELECT count(*) FROM pg_constraint WHERE conname = 'd2_id_fk' AND conparentid = 0", 'FK'), '1', 'the foreign key is on every node');
$n1->safe_psql('postgres', "INSERT INTO public.d2 VALUES (1, 'child')");
ddl_refused($n1, "INSERT INTO public.d2 VALUES (999, 'orphan')", qr/foreign key|violates/, 'the foreign key is enforced');
ddl_ok($n1, "ALTER TABLE public.d1 ADD COLUMN r int", 'a column for a foreign key to a reference table');
ddl_ok($n1, "ALTER TABLE public.d1 ADD CONSTRAINT d1_r_fk FOREIGN KEY (r) REFERENCES public.ref1 (id)", 'a foreign key from a distributed table to a reference table');
ddl_refused($n1, "ALTER TABLE public.ref1 ADD COLUMN q int; ALTER TABLE public.ref1 ADD CONSTRAINT ref1_fk FOREIGN KEY (q) REFERENCES public.d1 (id)", qr/to a distributed table is possible only from a colocated/, 'a foreign key from a reference table to a distributed table is refused');
ddl_refused($n1, "ALTER TABLE public.loc ADD COLUMN q int; ALTER TABLE public.loc ADD CONSTRAINT loc_fk FOREIGN KEY (q) REFERENCES public.d1 (id)", qr/to a distributed table is possible only from a colocated/, 'a foreign key from an ordinary table to a distributed table is refused');
ddl_refused($n1, "ALTER TABLE public.d2 ADD CONSTRAINT d2_c_fk FOREIGN KEY (c) REFERENCES public.d1 (a)", qr/must contain the distribution column/, 'a foreign key between distributed tables off the distribution column is refused');
ddl_refused($n1, "ALTER TABLE public.d2 ADD CONSTRAINT d2_loc_fk FOREIGN KEY (id) REFERENCES public.loc (i)", qr/can reference only a reference table or a colocated/, 'a foreign key to an ordinary table is refused');

# Rename.
ddl_ok($n1, 'ALTER TABLE public.d1 RENAME COLUMN a TO a2', 'RENAME COLUMN');
is(same_all(columns_of('d1'), 'RENAME COLUMN'), 'id:integer,a2:text,b:integer,y:text,z:numeric,r:integer', 'the column is renamed');
ddl_refused($n1, 'ALTER TABLE public.d1 RENAME COLUMN id TO id2', qr/not supported on distributed tables yet/, 'the distribution column cannot be renamed');
ddl_ok($n1, 'ALTER TABLE public.d1 RENAME COLUMN a2 TO a', 'RENAME COLUMN back');
ddl_ok($n1, 'ALTER TABLE public.d2 RENAME TO d2r', 'RENAME TO');
is(same_all("SELECT count(*) FROM pg_class WHERE relname = 'd2r' AND relkind = 'p'", 'RENAME TO'), '1', 'the table has the new name on every node');
is(same_all("SELECT count(*) FROM pg_inherits i WHERE i.inhparent = 'public.d2r'::regclass AND (SELECT relname FROM pg_class WHERE oid = i.inhrelid) !~ '^d2r_b[0-9]{4}\$'", 'partitions'), '0', 'every partition has a name derived from the new name');
is($n2->safe_psql('postgres', "SELECT count(*) FROM pg_class WHERE relname ~ '^d2_b' AND relkind = 'r'"), '0', 'no partition kept the old name');
is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.buckets WHERE table_name = 'public.d2r'::regclass AND partition_size IS NOT NULL"),
	$n1->safe_psql('postgres', "SELECT count(*) FROM pg_class WHERE relname ~ '^d2r_b[0-9]{4}\$' AND relkind = 'r'"),
	'the bucket view finds the renamed partitions');
is($n1->safe_psql('postgres', "SELECT val FROM (SELECT c AS val FROM public.d2r WHERE id = 1) s"), 'child', 'the renamed table works');
ddl_ok($n1, 'ALTER TABLE public.d2r RENAME TO d2', 'RENAME TO back');
ddl_refused($n1, 'ALTER TABLE public.d1_b0000 RENAME TO oops', qr/not supported on distributed tables yet/, 'a bucket partition cannot be renamed');
ddl_refused($n1, 'ALTER TABLE public.d1 RENAME TO ' . ('x' x 60), qr/too long/, 'a name too long for the partition names is refused');

# Owner.
ddl_ok($n1, 'ALTER TABLE public.d1 OWNER TO ddl_role', 'OWNER TO');
is(same_all("SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'public.d1'::regclass", 'OWNER TO'), 'ddl_role', 'the owner changed everywhere');
ddl_ok($n1, 'ALTER TABLE public.d1 OWNER TO CURRENT_USER', 'OWNER TO back');

# Other ALTER TABLE forms are refused.
for my $sql ('ALTER TABLE public.d1 SET SCHEMA public', 'ALTER TABLE public.d1 ENABLE ROW LEVEL SECURITY',
	'ALTER TABLE public.d1 SET (fillfactor = 70)', 'ALTER TABLE public.d1 SET TABLESPACE pg_default')
{
	ddl_refused($n1, $sql, qr/not supported/, "refused: $sql");
}
is(same_all(columns_of('d1'), 'after the refused forms'), 'id:integer,a:text,b:integer,y:text,z:numeric,r:integer', 'nothing changed');
ddl_ok($n1, 'ALTER TABLE public.d1 ADD COLUMN gx int GENERATED ALWAYS AS (b + 1) STORED', 'ADD COLUMN with a generated column');
is(same_all("SELECT gx FROM public.d1 WHERE id = 1", 'generated'), '2', 'the generated column is computed on every node');
ddl_ok($n1, 'ALTER TABLE public.d1 DROP COLUMN gx', 'and dropped');

# ---------------------------------------------------------------------------
# Indexes.
# ---------------------------------------------------------------------------
ddl_ok($n1, 'CREATE INDEX d1_b_idx ON public.d1 (b)', 'CREATE INDEX');
is(same_all("SELECT count(*) FROM pg_class WHERE relname = 'd1_b_idx'", 'CREATE INDEX'), '1', 'the index is on every node');
all_partitions('d1', "EXISTS (SELECT 1 FROM pg_index x JOIN pg_attribute a ON a.attrelid = x.indexrelid WHERE x.indrelid = p.oid AND a.attname = 'b')", 'the new index');
ddl_ok($n1, 'CREATE UNIQUE INDEX d1_id_b_uidx ON public.d1 (id, b)', 'CREATE UNIQUE INDEX with the distribution column');
ddl_refused($n1, 'CREATE UNIQUE INDEX d1_a_uidx ON public.d1 (a)', qr/unique index|partition|distribution/i, 'a unique index without the distribution column is refused');
ddl_refused($n1, 'CREATE INDEX CONCURRENTLY d1_c_idx ON public.d1 (b)', qr/not supported on distributed tables yet/, 'CREATE INDEX CONCURRENTLY is refused');
ddl_refused($n1, 'CREATE INDEX d1_p_idx ON public.d1_b0000 (b)', qr/not supported on distributed tables yet/, 'an index on a bucket partition is refused');
ddl_ok($n1, 'CREATE INDEX ref1_name_idx ON public.ref1 (name)', 'CREATE INDEX on a reference table');
is(same_all("SELECT count(*) FROM pg_class WHERE relname = 'ref1_name_idx'", 'index of a reference table'), '1', 'it is on every node');
ddl_ok($n1, 'DROP INDEX d1_b_idx', 'DROP INDEX');
is(same_all("SELECT count(*) FROM pg_class WHERE relname = 'd1_b_idx'", 'DROP INDEX'), '0', 'the index is gone everywhere');
ddl_refused($n1, 'DROP INDEX CONCURRENTLY d1_id_b_uidx', qr/not supported on distributed tables yet/, 'DROP INDEX CONCURRENTLY is refused');
ddl_ok($n1, 'DROP INDEX d1_id_b_uidx, ref1_name_idx', 'DROP INDEX of several indexes');

# ---------------------------------------------------------------------------
# Privileges and comments.
# ---------------------------------------------------------------------------
ddl_ok($n1, 'GRANT SELECT ON public.d1 TO ddl_role', 'GRANT');
is(same_all("SELECT has_table_privilege('ddl_role', 'public.d1', 'SELECT')", 'GRANT'), 't', 'the privilege is granted everywhere');
ddl_ok($n1, 'REVOKE SELECT ON public.d1 FROM ddl_role', 'REVOKE');
is(same_all("SELECT has_table_privilege('ddl_role', 'public.d1', 'SELECT')", 'REVOKE'), 'f', 'the privilege is revoked everywhere');
ddl_ok($n1, 'GRANT INSERT, UPDATE ON public.ref1 TO ddl_role', 'GRANT on a reference table');
is(same_all("SELECT has_table_privilege('ddl_role', 'public.ref1', 'INSERT')", 'GRANT on ref'), 't', 'granted everywhere');
ddl_refused($n1, 'GRANT SELECT ON public.d1, public.loc TO ddl_role', qr/Name only distributed or reference tables/, 'a GRANT that mixes distributed and ordinary tables is refused');
ddl_refused($n1, 'GRANT SELECT ON ALL TABLES IN SCHEMA public TO ddl_role', qr/name the tables/, 'GRANT ON ALL TABLES IN SCHEMA is refused');
ddl_ok($n1, "COMMENT ON TABLE public.d1 IS 'a distributed table'", 'COMMENT ON TABLE');
ddl_ok($n1, "COMMENT ON COLUMN public.d1.b IS 'the b column'", 'COMMENT ON COLUMN');
is(same_all("SELECT obj_description('public.d1'::regclass) || '/' || col_description('public.d1'::regclass, 3)", 'COMMENT'), 'a distributed table/the b column', 'the comments are on every node');

# ---------------------------------------------------------------------------
# TRUNCATE.
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', "INSERT INTO public.d1 (id, a, y) VALUES (101, 'x', 'y')");
ddl_refused($n1, 'TRUNCATE public.d1, public.loc', qr/Name only distributed or reference tables/, 'TRUNCATE that mixes distributed and ordinary tables is refused');
ddl_refused($n1, 'TRUNCATE public.d1_b0000', qr/not supported on distributed tables yet/, 'TRUNCATE of a bucket partition is refused');
ddl_ok($n1, 'TRUNCATE public.d2', 'TRUNCATE d2');
ddl_ok($n1, 'SET client_min_messages = warning; TRUNCATE public.d1 CASCADE', 'TRUNCATE d1 CASCADE');
is(same_all("SELECT count(*) FROM public.d1 WHERE id = 101", 'rows'), '0', 'no row is left on any node');
$n1->safe_psql('postgres', "INSERT INTO public.d1 (id, a, y, r) VALUES ($_, 'x', 'y', 1)") for (1 .. 6);
ddl_ok($n1, 'SET client_min_messages = warning; TRUNCATE public.ref1 CASCADE', 'TRUNCATE of a reference table (cascading to the table that references it)');
is(same_all('SELECT count(*) FROM public.ref1', 'TRUNCATE ref1'), '0', 'the reference table is empty everywhere');
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.d1 WHERE id = 1'), '0', 'the cascade reached the distributed table');

# ---------------------------------------------------------------------------
# A node that is not the map group primary refuses; a query string with
# several statements is refused.
# ---------------------------------------------------------------------------
ddl_refused($n2, 'ALTER TABLE public.d1 ADD COLUMN nm int', qr/must run on the map group primary/, 'DDL on a non-map node is refused');
ddl_refused($n3, 'DROP TABLE public.d1', qr/must run on the map group primary/, 'DROP TABLE on a non-map node is refused');
ddl_refused($n2, 'TRUNCATE public.ref1', qr/must run on the map group primary/, 'TRUNCATE of a reference table on a non-map node is refused');
ddl_refused($n1, "ALTER TABLE public.d1 ADD COLUMN m1 int \\; ALTER TABLE public.d1 ADD COLUMN m2 int;", qr/one of several statements in one query string/, 'a query string with several statements is refused');
is(same_all(columns_of('d1'), 'after the refusals'), 'id:integer,a:text,b:integer,y:text,z:numeric,r:integer', 'nothing changed');

# Statements that are not DDL on distributed tables are not affected.
ddl_ok($n3, 'CREATE TABLE public.local3 (i int); ALTER TABLE public.local3 ADD COLUMN j int; DROP TABLE public.local3', 'ordinary DDL on a non-map node works');

# ---------------------------------------------------------------------------
# Names resolve with the search_path of the session; literals are read with its
# DateStyle.
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', q{
		CREATE SCHEMA ddl_s;
		CREATE TABLE ddl_s.d3 (id int PRIMARY KEY, v text);
		SELECT kingres.create_distributed_table('ddl_s.d3', 'id', colocate_with => 'public.d1');
	});
	ddl_ok($n1, "SET search_path = ddl_s, pg_catalog; ALTER TABLE d3 ADD COLUMN w int", 'DDL by an unqualified name under a search_path');
	is(same_all("SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid = 'ddl_s.d3'::regclass AND attnum > 0", 'search_path'), 'id,v,w', 'the column was added to ddl_s.d3 everywhere');
	ddl_ok($n1, "SET datestyle = 'DMY'; ALTER TABLE public.d1 ADD COLUMN dd date DEFAULT '02/03/2020'", 'a date literal under another DateStyle');
	is(same_all("SELECT pg_get_expr(adbin, adrelid) FROM pg_attrdef WHERE adrelid = 'public.d1'::regclass AND adnum = (SELECT attnum FROM pg_attribute WHERE attrelid = 'public.d1'::regclass AND attname = 'dd')", 'date literal'),
		"'2020-03-02'::date", 'the date means the same on every node');
	ddl_ok($n1, 'ALTER TABLE public.d1 DROP COLUMN dd', 'drop it again');
}

# ---------------------------------------------------------------------------
# DROP TABLE.
# ---------------------------------------------------------------------------
ddl_refused($n1, 'DROP TABLE public.d1', qr/cannot drop table d1 because other objects depend on it/, 'DROP TABLE fails when a foreign key depends on the table');
ddl_ok($n1, 'DROP TABLE public.d2', 'DROP TABLE');
is(same_all("SELECT count(*) FROM pg_class WHERE relname ~ '^d2(_b[0-9]{4})?\$'", 'DROP TABLE'), '0', 'the table and its partitions are gone everywhere');
is(same_all("SELECT count(*) FROM kingres.dist_table", 'catalog after DROP TABLE'), '3', 'the catalog rows are gone everywhere');
ddl_ok($n1, 'SET client_min_messages = warning; DROP TABLE public.ref1 CASCADE', 'DROP TABLE of a reference table');
is(same_all("SELECT count(*) FROM kingres.dist_table", 'catalog after DROP TABLE ref1'), '2', 'the reference table left the catalog everywhere');
ddl_refused($n1, 'DROP TABLE public.d1, public.loc', qr/Name only distributed or reference tables/, 'DROP TABLE that mixes distributed and ordinary tables is refused');
ddl_refused($n1, 'DROP TABLE public.d1_b0000', qr/not supported on distributed tables yet/, 'DROP TABLE of a bucket partition is refused');

# ---------------------------------------------------------------------------
# Rejected statements.
# ---------------------------------------------------------------------------
for my $sql (
	'CREATE TRIGGER t1 BEFORE INSERT ON public.d1 FOR EACH ROW EXECUTE FUNCTION suppress_redundant_updates_trigger()',
	'CREATE RULE r1 AS ON INSERT TO public.d1 DO ALSO NOTIFY d1',
	'CREATE POLICY p1 ON public.d1 USING (true)',
	'CLUSTER public.d1 USING d1_pkey',
	"COPY public.d1 (id) FROM '/dev/null'",
	'CREATE STATISTICS st1 ON id, b FROM public.d1',
	'ALTER TABLE public.d1 ATTACH PARTITION public.loc FOR VALUES WITH (MODULUS 4, REMAINDER 0)',
	'ALTER TABLE public.d1 DETACH PARTITION public.d1_b0000',
	'CREATE TABLE public.kid () INHERITS (public.d1)',
	'ALTER INDEX public.d1_pkey RENAME TO d1_pkey2',
	)
{
	ddl_refused($n1, $sql, qr/is not supported on distributed tables yet/, "refused: $sql");
}
is(same_all("SELECT count(*) FROM pg_trigger WHERE tgrelid = 'public.d1'::regclass AND NOT tgisinternal", 'triggers'), '0', 'no trigger was created');

# ---------------------------------------------------------------------------
# DDL is part of the distributed transaction: a rollback undoes it everywhere,
# and a crash is resolved by recovery.
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n1, 'BEGIN; ALTER TABLE public.d1 ADD COLUMN rb int; ALTER TABLE public.d1 ADD COLUMN rb2 int; ROLLBACK;');
	is($err, '', 'a transaction with DDL rolled back');
	is(same_all("SELECT count(*) FROM pg_attribute WHERE attrelid = 'public.d1'::regclass AND attname LIKE 'rb%'", 'rollback'), '0', 'the rolled back DDL is gone everywhere');
	is(prepared_count($_), '0', "nothing is prepared on $_->name") for @nodes;

	($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = 'after_prepare:kill'; ALTER TABLE public.d1 ADD COLUMN cr int;");
	isnt($rc, 0, 'the origin dies after the DDL was prepared');
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2', 'recovery rolls back the prepared DDL');
	is(same_all("SELECT count(*) FROM pg_attribute WHERE attrelid = 'public.d1'::regclass AND attname = 'cr'", 'recovered rollback'), '0', 'the column does not exist anywhere');

	($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = 'after_commit:kill'; ALTER TABLE public.d1 ADD COLUMN cc int;");
	isnt($rc, 0, 'the origin dies after the commit');
	$n1->stop('immediate', fail_ok => 1);
	$n1->start;
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2', 'recovery commits the prepared DDL');
	is(same_all("SELECT count(*) FROM pg_attribute WHERE attrelid = 'public.d1'::regclass AND attname = 'cc'", 'recovered commit'), '1', 'the column exists everywhere');
}

# ---------------------------------------------------------------------------
# A node added after the DDL gets the tables as they are now.
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.ref2 (id int PRIMARY KEY, name text);
		SELECT kingres.create_reference_table('public.ref2');
		INSERT INTO public.ref2 VALUES (1, 'x');
		ALTER TABLE public.d1 ADD CONSTRAINT d1_r2_fk FOREIGN KEY (r) REFERENCES public.ref2 (id);
	});
	my $n4 = make_node('dd4');
	$n1->safe_psql('postgres', sprintf("SELECT kingres.add_node('%s', %d)", $n4->host, $n4->port));
	is($n4->safe_psql('postgres', columns_of('d1')), $n1->safe_psql('postgres', columns_of('d1')), 'the new node has the columns as they are now');
	is($n4->safe_psql('postgres', "SELECT string_agg(conname, ',' ORDER BY conname) FROM pg_constraint WHERE conrelid = 'public.d1'::regclass AND contype = 'f'"),
		$n1->safe_psql('postgres', "SELECT string_agg(conname, ',' ORDER BY conname) FROM pg_constraint WHERE conrelid = 'public.d1'::regclass AND contype = 'f'"),
		'and its foreign keys');
	is($n4->safe_psql('postgres', "SELECT name FROM public.ref2"), 'x', 'and the rows of the reference table');
	ddl_ok($n1, 'ALTER TABLE public.d1 ADD COLUMN after4 int', 'DDL reaches the new node too');
	is($n4->safe_psql('postgres', "SELECT count(*) FROM pg_attribute WHERE attrelid = 'public.d1'::regclass AND attname = 'after4'"), '1', 'the new node has the column');
}

done_testing();
