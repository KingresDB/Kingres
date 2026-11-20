# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/150_distribute_races.pl: the table lifecycle against concurrent transactions, and the refusals that must come
# before anything is done (review of increments 9.1 and 9.2, findings P9A-01 to P9A-10).
#
#   P9A-01  a change that a transaction committed while the call waited for the lock is in the new table: the
#           values are compared, not only the counts (UPDATE, DELETE, DELETE with INSERT, INSERT; a reference
#           table; the online engine)
#   P9A-02  a statement queued behind the call (RENAME COLUMN, ADD COLUMN, COPY) is decided after the wait: its
#           effect is on every node
#   P9A-03 .. P9A-07  rules, row types, NOT VALID checks, defaults that call an unowned sequence, collations and
#           schema privileges of other nodes, enum distribution columns, owners of functions, invalid indexes,
#           FORCE ROW LEVEL SECURITY: each is a code of the report and a refusal before anything changes; the
#           grants of default privileges are not added; the attribute options are a warning
#   P9A-10  tests for the mutants of the review (C04 C05 C09 S09 S13 S16 S17)

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node make_cluster run_sql prepared_count wait_waiter start_psql finish_psql);

my ($n1, $n2) = make_cluster('dr', 2);
my @nodes = ($n1, $n2);

sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

$_->safe_psql('postgres', 'CREATE ROLE p10_mal; CREATE ROLE p10_nobody; CREATE ROLE p10_dp; CREATE ROLE p10_own;') for @nodes;

# ------------------------------------------------------------------------------------------------------------
# P9A-01: the writer that commits while the call waits
# ------------------------------------------------------------------------------------------------------------
sub race
{
	my ($name, $setup, $bsql, $call, $check) = @_;

	$n1->safe_psql('postgres', $setup);
	my $b = $n1->background_psql('postgres', on_error_stop => 0);
	$b->query_safe("BEGIN;\n$bsql");
	my $a = start_psql($n1, $call);
	ok(wait_waiter($n1, 1), "$name: the call waits for the writer");
	$b->query_safe('COMMIT;');
	$b->quit;
	my ($out, $err) = finish_psql($a);
	is($err, '', "$name: the call succeeded") or diag($err);
	$check->($name);
}

sub tbl { return "CREATE TABLE public.$_[0] (id int PRIMARY KEY, v text); INSERT INTO public.$_[0] SELECT g, 'orig' FROM generate_series(1, 10) g;" }


race('update', tbl('r_up'), "UPDATE public.r_up SET v = 'B' WHERE id <= 3;",
	"SELECT kingres.create_distributed_table('public.r_up', 'id', bucket_count => 4)",
	sub {
		is(q1($_, "SELECT count(*) || '|' || count(*) FILTER (WHERE v = 'B') FROM public.r_up"), '10|3',
			"$_[0]: the three committed updates are in the new table, seen from " . $_->name) for @nodes;
	});

race('delete', tbl('r_del'), "DELETE FROM public.r_del WHERE id IN (2, 3);",
	"SELECT kingres.create_distributed_table('public.r_del', 'id', bucket_count => 4)",
	sub {
		is(q1($_, "SELECT count(*) || '|' || count(*) FILTER (WHERE id IN (2, 3)) FROM public.r_del"), '8|0',
			"$_[0]: the two committed deletes are in the new table, seen from " . $_->name) for @nodes;
	});

race('delete and insert', tbl('r_di'), "DELETE FROM public.r_di WHERE id = 1; INSERT INTO public.r_di VALUES (100, 'B');",
	"SELECT kingres.create_distributed_table('public.r_di', 'id', bucket_count => 4)",
	sub {
		is(q1($_, "SELECT count(*) || '|' || count(*) FILTER (WHERE id = 1) || '|' || count(*) FILTER (WHERE id = 100 AND v = 'B') FROM public.r_di"),
			'10|0|1', "$_[0]: the delete and the insert are both in the new table, seen from " . $_->name) for @nodes;
	});

race('insert', tbl('r_in'), "INSERT INTO public.r_in VALUES (100, 'B');",
	"SELECT kingres.create_distributed_table('public.r_in', 'id', bucket_count => 4)",
	sub {
		is(q1($_, "SELECT count(*) || '|' || count(*) FILTER (WHERE id = 100) FROM public.r_in"), '11|1',
			"$_[0]: the committed insert is in the new table, seen from " . $_->name) for @nodes;
	});

race('reference update', tbl('r_ref'), "UPDATE public.r_ref SET v = 'B' WHERE id <= 3; DELETE FROM public.r_ref WHERE id = 10;",
	"SELECT kingres.create_reference_table('public.r_ref')",
	sub {
		is(q1($_, "SELECT count(*) || '|' || count(*) FILTER (WHERE v = 'B') FROM public.r_ref"), '9|3',
			"$_[0]: the committed update and delete are in the copy on " . $_->name) for @nodes;
	});

race('online update', tbl('r_on'), "UPDATE public.r_on SET v = 'B' WHERE id <= 3; DELETE FROM public.r_on WHERE id = 10; INSERT INTO public.r_on VALUES (100, 'B');",
	"SELECT kingres.create_distributed_table_concurrently('public.r_on', 'id', bucket_count => 4)",
	sub {
		is(q1($_, "SELECT count(*) || '|' || count(*) FILTER (WHERE v = 'B') FROM public.r_on"), '10|4',
			"$_[0]: the online engine has the committed changes, seen from " . $_->name) for @nodes;
	});

race('online reference', tbl('r_onr'), "UPDATE public.r_onr SET v = 'B' WHERE id <= 3;",
	"SELECT kingres.create_reference_table_concurrently('public.r_onr')",
	sub {
		is(q1($_, "SELECT count(*) || '|' || count(*) FILTER (WHERE v = 'B') FROM public.r_onr"), '10|3',
			"$_[0]: the online reference copy has the committed update on " . $_->name) for @nodes;
	});

# a transaction that holds one snapshot cannot give the call a snapshot after the lock: refused
{
	$n1->safe_psql('postgres', tbl('r_rr'));
	my (undef, undef, $err) = run_sql($n1, "BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT 1; SELECT kingres.create_distributed_table('public.r_rr', 'id'); COMMIT;");
	like($err, qr/REPEATABLE READ or SERIALIZABLE/, 'a REPEATABLE READ transaction is refused with the reason');
	(undef, undef, $err) = run_sql($n1, "BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT 1; SELECT kingres.create_reference_table('public.r_rr'); COMMIT;");
	like($err, qr/REPEATABLE READ or SERIALIZABLE/, 'also for a reference table');
	is(q1($n1, "SELECT relkind || count(*) FROM public.r_rr, pg_class WHERE pg_class.oid = 'public.r_rr'::regclass GROUP BY relkind"), 'r10', 'the table is unchanged');
}

# ------------------------------------------------------------------------------------------------------------
# P9A-02: statements queued behind the call
# ------------------------------------------------------------------------------------------------------------
$n1->safe_psql('postgres', "CREATE TABLE public.qq (id int PRIMARY KEY, v text); INSERT INTO public.qq SELECT g, 'v' FROM generate_series(1, 20) g;");
{
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_qq_$$";
	append_to_file($gate, 'x');
	my $creator = start_psql($n1,
		"SET kingres_shard.fault_injection = 'distribute_after_lock:wait:$gate'; "
		  . "SELECT kingres.create_distributed_table('public.qq', 'id', bucket_count => 6);");
	ok(KingresShardTest::wait_for_fault_file("$gate.reached", 60), 'the call holds the lock');
	my @queued = (
		start_psql($n1, 'ALTER TABLE public.qq RENAME COLUMN v TO w;'),
		start_psql($n1, 'ALTER TABLE public.qq ADD COLUMN extra int;'),
		start_psql($n1, "COPY public.qq (id) FROM PROGRAM 'seq 101 105';"),
	);
	ok(wait_waiter($n1, 3), 'the three statements wait for the table');
	unlink $gate;
	my ($out, $err) = finish_psql($creator);
	is($err, '', 'the call succeeded') or diag($err);
	for my $q (@queued)
	{
		($out, $err) = finish_psql($q);
		is($err, '', 'a queued statement succeeded after the call') or diag($err);
	}
	for my $n (@nodes)
	{
		is(q1($n, q{SELECT count(*) || '|' || count(DISTINCT cols) || '|' || min(cols) FROM (
				SELECT c.oid, string_agg(a.attname, ',' ORDER BY a.attnum) AS cols
				  FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
				 WHERE c.relname ~ '^qq(_b[0-9]{4})?$' GROUP BY c.oid) x}) =~ s/^\d+\|//r,
			'1|id,w,extra', 'the parent and every partition on ' . $n->name . ' have the columns that the queued statements made');
		is(q1($n, 'SELECT count(*) FROM public.qq'), '25', 'the rows of the queued COPY are there, seen from ' . $n->name);
	}
	# each row of the COPY is in the partition of its bucket (the partitions are read directly: a scan that spans groups has no tableoid)
	my ($right, $all) = (0, 0);
	for my $n (@nodes)
	{
		for my $p (split /\n/, q1($n, q{SELECT relname FROM pg_class WHERE relname ~ '^qq_b[0-9]{4}$' ORDER BY 1}))
		{
			my ($bucket) = $p =~ /_b(\d+)$/;
			$all += q1($n, "SELECT count(*) FROM public.$p WHERE id > 100");
			$right += q1($n, "SELECT count(*) FROM public.$p WHERE id > 100 AND kingres.bucket_for('public.qq'::regclass, id) = " . int($bucket));
		}
	}
	is("$right/$all", '5/5', 'and each of them is in the partition of its bucket');
}

# ------------------------------------------------------------------------------------------------------------
# P9A-03 .. P9A-05, P9A-07: what the report refuses, before anything changes
# ------------------------------------------------------------------------------------------------------------
sub codes
{
	my ($kind, $rel, $opts) = @_;
	$opts //= '{"column":"id"}';
	return q1($n1, "SELECT coalesce(string_agg(severity || ':' || code, ',' ORDER BY code), '') FROM kingres.table_change_report('$kind', '$rel', '$opts')");
}

sub blockers
{
	my ($rel) = @_;
	return q1($n1, "SELECT coalesce(string_agg(code, ',' ORDER BY code), '') FROM kingres.table_change_report('distribute', '$rel', '{\"column\":\"id\"}') WHERE severity = 'blocker'");
}

sub warnings_of
{
	my ($rel) = @_;
	return q1($n1, "SELECT coalesce(string_agg(detail, '; ' ORDER BY detail), '') FROM kingres.table_change_report('distribute', '$rel', '{\"column\":\"id\"}') WHERE severity = 'warning'");
}

sub untouched
{
	my ($name, $t) = @_;
	is(q1($n1, "SELECT relkind FROM pg_class WHERE oid = 'public.$t'::regclass"), 'r', "$name: the table is still the plain table");
	is(q1($_, "SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\\_shadow%' OR relname ~ '^${t}_b[0-9]{4}\$'"), '0',
		"$name: nothing is left on " . $_->name) for @nodes;
	is(prepared_count($_), '0', "$name: nothing is prepared on " . $_->name) for @nodes;
}

sub refused
{
	my ($name, $t, $code, $setup, %o) = @_;

	$n1->safe_psql('postgres', $setup);
	like(codes('distribute', "public.$t"), qr/blocker:$code/, "$name: the report has the blocker $code");
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.$t', 'id')");
	like($err, qr/$code:|$o{message}/s, "$name: create_distributed_table refuses it") or diag($err);
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table_concurrently('public.$t', 'id')");
	like($err, qr/$code:|$o{message}/s, "$name: so does the online function") or diag($err);
	untouched($name, $t);
}

refused('rule', 't_rule', 'rule', q{
	CREATE TABLE public.t_rule (id int PRIMARY KEY, v text); CREATE TABLE public.t_audit (id int);
	INSERT INTO public.t_rule VALUES (1, 'a');
	CREATE RULE t_rule_audit AS ON INSERT TO public.t_rule DO ALSO INSERT INTO public.t_audit VALUES (NEW.id);}, message => 'rule "t_rule_audit"');

refused('row type', 't_rt', 'dependent_object', q{
	CREATE TABLE public.t_rt (id int PRIMARY KEY, v text); CREATE TABLE public.t_rt_user (x public.t_rt);
	INSERT INTO public.t_rt VALUES (1, 'a');}, message => 'has the row type of the table');

refused('NOT VALID check', 't_nv', 'check_not_valid', q{
	CREATE TABLE public.t_nv (id int PRIMARY KEY, v int); INSERT INTO public.t_nv VALUES (1, -5);
	ALTER TABLE public.t_nv ADD CONSTRAINT nv CHECK (v > 0) NOT VALID;}, message => 'is not validated');

refused('unowned sequence', 't_us', 'default_sequence_not_owned', q{
	CREATE SEQUENCE public.t_us_seq; CREATE TABLE public.t_us (id int PRIMARY KEY, n bigint DEFAULT nextval('public.t_us_seq'));
	INSERT INTO public.t_us (id) VALUES (1);}, message => 'which no column of the table owns');

$n1->safe_psql('postgres', "CREATE COLLATION public.coll_only (provider = libc, locale = 'C')");
refused('collation of one node', 't_co', 'collation_missing', q{
	CREATE TABLE public.t_co (id int PRIMARY KEY, v text COLLATE public.coll_only); INSERT INTO public.t_co VALUES (1, 'a');},
	message => 'do not exist on node');
$n2->safe_psql('postgres', "CREATE COLLATION public.coll_only (provider = libc, locale = 'C')");
is(blockers('public.t_co'), '', 'the collation made on the other node too: the report is empty');

# an owner with CREATE on the schema on this node only
$n1->safe_psql('postgres', 'CREATE SCHEMA sp10; GRANT CREATE, USAGE ON SCHEMA sp10 TO p10_own');
$n2->safe_psql('postgres', 'CREATE SCHEMA sp10');
{
	$n1->safe_psql('postgres', 'CREATE TABLE sp10.t_sp (id int PRIMARY KEY); INSERT INTO sp10.t_sp VALUES (1); ALTER TABLE sp10.t_sp OWNER TO p10_own');
	like(codes('distribute', 'sp10.t_sp'), qr/blocker:schema_privilege/, 'schema privilege: the report has the blocker');
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('sp10.t_sp', 'id')");
	like($err, qr/schema_privilege:|does not have CREATE on schema/s, 'schema privilege: the call refuses before anything is done') or diag($err);
	$n2->safe_psql('postgres', 'GRANT CREATE, USAGE ON SCHEMA sp10 TO p10_own');
	is(blockers('sp10.t_sp'), '', 'schema privilege: granted on the other node too, the report is empty');
	$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('sp10.t_sp', 'id')");
	is(q1($n2, 'SELECT count(*) FROM sp10.t_sp'), '1', 'schema privilege: the table with its owner is distributed once the privilege is everywhere');
}

$n1->safe_psql('postgres', q{CREATE TYPE public.color10 AS ENUM ('red', 'green')});
$n2->safe_psql('postgres', q{CREATE TYPE public.color10 AS ENUM ('green', 'red')});
$n1->safe_psql('postgres', q{CREATE TABLE public.t_en (id public.color10 PRIMARY KEY, v text); INSERT INTO public.t_en VALUES ('red', 'a')});
like(codes('distribute', 'public.t_en'), qr/blocker:enum_distribution_column/, 'enum: the report has the blocker');
{
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.t_en', 'id')");
	like($err, qr/enum_distribution_column:|is an enum/s, 'enum: the call refuses and says why') or diag($err);
	untouched('enum', 't_en');
}

{
	$n1->safe_psql('postgres', 'CREATE TABLE public.t_nm (id int PRIMARY KEY); INSERT INTO public.t_nm VALUES (1)');
	my $oid = q1($n1, "SELECT 'public.t_nm'::regclass::oid");
	$n2->safe_psql('postgres', "CREATE TABLE public.kingres_shadow_$oid (a int)");
	like(codes('distribute', 'public.t_nm'), qr/blocker:name_in_use/, 'name on another node: the report has the blocker');
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.t_nm', 'id')");
	like($err, qr/name_in_use:|exist on node/s, 'name on another node: the call refuses') or diag($err);
	$n2->safe_psql('postgres', "DROP TABLE public.kingres_shadow_$oid");
	is(blockers('public.t_nm'), '', 'name on another node: gone, the report is empty');
}

# P9A-05: a function of an owner who is not a superuser is not run by the copy
$n1->safe_psql('postgres', q{
	CREATE FUNCTION public.p10_untrusted(text) RETURNS bool LANGUAGE sql IMMUTABLE AS 'SELECT true';
	ALTER FUNCTION public.p10_untrusted(text) OWNER TO p10_mal;
	CREATE FUNCTION public.p10_trusted(text) RETURNS bool LANGUAGE sql IMMUTABLE AS 'SELECT true';});
refused('function owner', 't_fo', 'function_owner_not_superuser', q{
	CREATE TABLE public.t_fo (id int PRIMARY KEY, v text CHECK (public.p10_untrusted(v))); INSERT INTO public.t_fo VALUES (1, 'a');},
	message => 'who is not a superuser');
$n1->safe_psql('postgres', q{CREATE TABLE public.t_ft (id int PRIMARY KEY, v text CHECK (public.p10_trusted(v))); INSERT INTO public.t_ft VALUES (1, 'a')});
is(blockers('public.t_ft'), '', 'function owner: a function of a superuser is accepted');

# P9A-07: invalid index, FORCE ROW LEVEL SECURITY
refused('invalid index', 't_ii', 'invalid_index', q{
	CREATE TABLE public.t_ii (id int PRIMARY KEY, v text); CREATE INDEX t_ii_v ON public.t_ii (v);
	UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'public.t_ii_v'::regclass;}, message => 'is not valid');
refused('force row level security', 't_fr', 'row_level_security', q{
	CREATE TABLE public.t_fr (id int PRIMARY KEY, v text); ALTER TABLE public.t_fr FORCE ROW LEVEL SECURITY;}, message => 'row-level security');

# an exclusion constraint (mutant S09): its own code, and only it
refused('exclusion constraint', 't_ex', 'exclusion_constraint', q{
	CREATE TABLE public.t_ex (id int PRIMARY KEY, v int, EXCLUDE (v WITH =));}, message => 'exclusion constraint');
is(blockers('public.t_ex'), 'exclusion_constraint', 'the exclusion constraint is the only blocker of that table');

# attribute options are a warning
$n1->safe_psql('postgres', q{CREATE TABLE public.t_ao (id int PRIMARY KEY, v text); ALTER TABLE public.t_ao ALTER COLUMN v SET (n_distinct = 100); INSERT INTO public.t_ao VALUES (1, 'a')});
like(warnings_of('public.t_ao'), qr/attribute options of column "v"/, 'attribute options: the report warns that they are not carried');
$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('public.t_ao', 'id')");

# P9A-06: the state of a serial sequence is at least the largest value of the column, and the next value is above every row
$n1->safe_psql('postgres', q{CREATE TABLE public.t_se (id serial PRIMARY KEY, v text); INSERT INTO public.t_se (v) SELECT 'a' FROM generate_series(1, 5);
	DELETE FROM public.t_se WHERE id > 2});
$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('public.t_se', 'id')");
is(q1($n1, q{INSERT INTO public.t_se (v) VALUES ('later') RETURNING id > 2}), 't', 'serial: the next value is above the rows that were copied (the state is advanced to the largest value of the column, as the README says)');

# ------------------------------------------------------------------------------------------------------------
# P9A-07: the grants of default privileges are not added to the new table (and the PUBLIC grant of the old one stays)
# ------------------------------------------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.t_acl (id int PRIMARY KEY, v text);
	CREATE TABLE public.t_acl_pub (id int PRIMARY KEY, v text);
	CREATE TABLE public.t_acl_own (id int PRIMARY KEY, v text);
	INSERT INTO public.t_acl VALUES (1, 'a'); INSERT INTO public.t_acl_pub VALUES (1, 'a'); INSERT INTO public.t_acl_own VALUES (1, 'a');
	GRANT SELECT ON public.t_acl_pub TO PUBLIC;
	GRANT SELECT ON public.t_acl TO p10_nobody;
	REVOKE INSERT, UPDATE ON public.t_acl_own FROM CURRENT_USER;
	ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO p10_dp;
	ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO PUBLIC;});
sub acl_of
{
	my ($t) = @_;
	return q1($n1, "SELECT coalesce((SELECT string_agg(s, ',' ORDER BY s) FROM (SELECT (CASE WHEN e.grantee = 0 THEN 'PUBLIC' ELSE e.grantee::regrole::text END) || ':' || e.privilege_type || ':' || e.is_grantable || ':' || e.grantor::regrole::text AS s FROM aclexplode(coalesce(c.relacl, acldefault('r', c.relowner))) e) q), 'NULL') FROM pg_class c WHERE c.oid = 'public.$t'::regclass");
}
my %acl_before = map { $_ => acl_of($_) } qw(t_acl t_acl_pub t_acl_own);
$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('public.$_', 'id')") for qw(t_acl t_acl_pub t_acl_own);
$n1->safe_psql('postgres', 'ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM p10_dp; ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT ON TABLES FROM PUBLIC');
for my $t (qw(t_acl t_acl_pub t_acl_own))
{
	is(acl_of($t), $acl_before{$t},
		"privileges: the ACL of $t is the old ACL, nothing added by default privileges");
}
# the shadow of a table that has no default privileges to take back has the default ACL (NULL): a privilege that the owner
# took from himself must still be gone, and the privileges of a default ACL stay the same (the new ACL is written out: the
# entries of the owner are explicit, which is the same set of privileges)
$n1->safe_psql('postgres', q{
	CREATE TABLE public.t_acl_o2 (id int PRIMARY KEY); INSERT INTO public.t_acl_o2 VALUES (1);
	REVOKE UPDATE, DELETE ON public.t_acl_o2 FROM CURRENT_USER;
	CREATE TABLE public.t_acl_nul (id int PRIMARY KEY); INSERT INTO public.t_acl_nul VALUES (1);});
{
	my %b = map { $_ => acl_of($_) } qw(t_acl_o2 t_acl_nul);
	is(q1($n1, "SELECT relacl IS NULL FROM pg_class WHERE oid = 'public.t_acl_nul'::regclass"), 't', 'a table that was never granted anything has the default ACL');
	$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('public.$_', 'id')") for qw(t_acl_o2 t_acl_nul);
	is(acl_of($_), $b{$_}, "privileges: the ACL of $_ is the old one, with no default privileges to take back") for qw(t_acl_o2 t_acl_nul);
}
is(q1($n1, "SELECT has_table_privilege('p10_dp', 'public.t_acl', 'SELECT') OR has_table_privilege('p10_dp', 'public.t_acl', 'INSERT')"), 'f',
	'privileges: the role of the default privileges has nothing on the new table');
is(q1($n1, "SELECT has_table_privilege('p10_nobody', 'public.t_acl', 'SELECT')"), 't', 'privileges: the carried grant is there');
is(q1($n1, "SELECT has_table_privilege('p10_dp', 'public.t_acl_pub', 'SELECT')"), 't', 'privileges: PUBLIC keeps its grant on the table that had one');

# ------------------------------------------------------------------------------------------------------------
# P9A-10: the survivors of the review
# ------------------------------------------------------------------------------------------------------------
# C09: a statement after a call that added keys, in the same session, is still seen by the hook (and goes everywhere)
$n1->safe_psql('postgres', q{
	CREATE TABLE public.k_ref (id int PRIMARY KEY);
	INSERT INTO public.k_ref VALUES (1);});
$n1->safe_psql('postgres', "SELECT kingres.create_reference_table('public.k_ref')");
$n1->safe_psql('postgres', q{
	CREATE TABLE public.k_child (id int PRIMARY KEY, p int REFERENCES public.k_ref (id));
	INSERT INTO public.k_child VALUES (1, 1);});
{
	my ($rc, $out, $err) = run_sql($n1, q{
		SELECT kingres.create_distributed_table('public.k_child', 'id', bucket_count => 4);
		ALTER TABLE public.k_child ADD COLUMN after_call int;});
	is($rc, 0, 'C09: the statement after the call runs') or diag($err);
	for my $n (@nodes)
	{
		is(q1($n, q{SELECT count(*) > 0 AND count(*) = count(a.attname) FROM pg_class c
				LEFT JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'after_call' AND NOT a.attisdropped
				WHERE c.relname ~ '^k_child(_b[0-9]{4})?$'}), 't',
			'C09: the added column is on the parent and on every partition of ' . $n->name);
	}
}

# C04 (check_identity in the copy) has no test: it guards a destination table that is replaced between the probe and the insert, which
# nothing in the call can do to its own private shadow (see the notes of the review round).

# C05: money keeps its value under another lc_monetary (needs a locale that the system has)
SKIP:
{
	my $loc = q1($n1, q{SELECT coalesce((SELECT collname FROM pg_collation WHERE collname ~* '^(en_US|de_DE).utf-?8$' ORDER BY collname LIMIT 1), '')});
	skip('no locale with a different money format on this system', 2) if $loc eq '';
	$n1->safe_psql('postgres', q{CREATE TABLE public.mm (id int PRIMARY KEY, m money); INSERT INTO public.mm SELECT g, (g * 1234.56)::numeric::money FROM generate_series(1, 20) g});
	my $sum = q1($n1, q{SELECT sum(m::numeric) FROM public.mm});
	my ($rc, $out, $err) = run_sql($n1, "SET lc_monetary = '$loc'; SELECT kingres.create_distributed_table('public.mm', 'id', bucket_count => 4);");
	is($rc, 0, 'C05: the call runs under another lc_monetary') or diag($err);
	is(q1($n1, q{SELECT sum(m::numeric) FROM public.mm}), $sum, 'C05: the money values are unchanged');
}

# S13: a node that got the catalog by a sync knows the colocation ids already used
{
	my $max = q1($n1, 'SELECT max(colocation_id) FROM kingres.colocation');
	is(q1($n2, 'SELECT max(colocation_id) FROM kingres.colocation'), $max, 'S13: the catalog is on the other node');
	is(q1($n2, 'SELECT last_value >= ' . $max . ' AND is_called FROM kingres.colocation_id_seq'), 't',
		'S13: the sequence of colocation ids on the other node is at the highest id of the catalog');
}

# S16: the sequence of colocation ids is closed to roles without privilege
is(q1($n1, "SELECT has_sequence_privilege('p10_nobody', 'kingres.colocation_id_seq', 'USAGE') OR has_sequence_privilege('p10_nobody', 'kingres.colocation_id_seq', 'SELECT') OR has_sequence_privilege('p10_nobody', 'kingres.colocation_id_seq', 'UPDATE')"),
	'f', 'S16: a role without privilege cannot use the sequence of colocation ids');
{
	my (undef, undef, $err) = run_sql($n1, "SET ROLE p10_nobody; SELECT nextval('kingres.colocation_id_seq')");
	like($err, qr/permission denied/, 'S16: nextval is refused to such a role');
}

# S16 again, where it matters: default privileges that give every new sequence to PUBLIC must not reach the sequence of
# colocation ids (the extension revokes it explicitly)
{
	my $fresh = make_node('s16');
	$fresh->safe_psql('postgres', 'CREATE ROLE p10_nobody; ALTER DEFAULT PRIVILEGES GRANT ALL ON SEQUENCES TO PUBLIC; CREATE EXTENSION kingres_shard');
	is($fresh->safe_psql('postgres', "SELECT has_sequence_privilege('p10_nobody', 'kingres.colocation_id_seq', 'USAGE') OR has_sequence_privilege('p10_nobody', 'kingres.colocation_id_seq', 'UPDATE')"),
		'f', 'S16: with default privileges that grant sequences to PUBLIC, the sequence of colocation ids is still closed');
	$fresh->stop;
}

for my $n (@nodes)
{
	is(prepared_count($n), '0', 'nothing is prepared on ' . $n->name);
}

done_testing();
