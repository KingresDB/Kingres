# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/043_reference_paths.pl
#
# Review findings S3-01, S3-02 and S3-13, and the rule behind them: a reference
# table is written on every copy or not at all, whatever path the write takes.
# Every path that can reach a reference table is listed here with the decision
# the extension takes (propagated to every copy, or refused with a clear
# error), and each decision is tested; the copies are compared on every node
# afterwards.  kingres.check_cluster() must report a divergence made by hand.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql keys_of_group);

my ($n1, $n2, $n3) = make_cluster('rp', 3);
my @nodes = ($n1, $n2, $n3);

$n1->safe_psql('postgres', q{
	CREATE TABLE public.logr (k int PRIMARY KEY, v text);
	CREATE TABLE public.logr2 (k int PRIMARY KEY, v text);
	CREATE TABLE public.d (id int PRIMARY KEY, v text);
	SELECT kingres.create_reference_table('public.logr');
	SELECT kingres.create_reference_table('public.logr2');
	SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 12);
});
my $coloc = $n1->safe_psql('postgres',
	q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.d'::regclass});
my ($k_local) = keys_of_group($n1, $coloc, 0, 1, 1);
my ($k_remote) = keys_of_group($n1, $coloc, 1, 1, 1);
my ($k_remote3) = keys_of_group($n1, $coloc, 2, 1, 1);

$_->safe_psql('postgres', q{
	CREATE FUNCTION public.wr(k int) RETURNS text LANGUAGE plpgsql VOLATILE AS
	  $$ BEGIN INSERT INTO public.logr VALUES (k, 'wr'); RETURN 'w'; END $$;
	CREATE FUNCTION public.wr_sql(k int) RETURNS text LANGUAGE sql VOLATILE AS
	  $$ INSERT INTO public.logr VALUES (k, 'sql') RETURNING 's' $$;
	CREATE FUNCTION public.wr_dyn(k int) RETURNS text LANGUAGE plpgsql VOLATILE AS
	  $$ BEGIN EXECUTE format('INSERT INTO public.logr VALUES (%s, %L)', k, 'dyn'); RETURN 'd'; END $$;
	CREATE TABLE public.ot (i int);
}) for @nodes;

sub table_text
{
	my ($node, $table) = @_;
	return $node->safe_psql('postgres',
		"SELECT coalesce(string_agg(t::text, ',' ORDER BY t::text), '') FROM public.$table t");
}

sub same_everywhere
{
	my ($label, @tables) = @_;
	for my $t (@tables)
	{
		my $want = table_text($n1, $t);
		is(table_text($_, $t), $want, "$label: $t is the same on " . $_->name) for ($n2, $n3);
	}
}

sub refused
{
	my ($label, $node, $sql, $re) = @_;
	my ($rc, $out, $err) = run_sql($node, $sql);
	isnt($rc, 0, "$label: refused");
	like($err, $re, "$label: the message says why");
	return $err;
}

# ---------------------------------------------------------------------------
# S3-01: a function called by a statement that is run for another node.
# ---------------------------------------------------------------------------
# Rows to find, so that a routed SELECT or UPDATE really calls the function.
$n1->safe_psql('postgres', "INSERT INTO public.d VALUES ($k_remote, 'seed')");
$n1->safe_psql('postgres', "INSERT INTO public.d VALUES ($k_remote3, 'seed')");

my $forwarded_re = qr/reference table "logr" cannot be written/;
refused('plpgsql function in a routed INSERT', $n1,
	"INSERT INTO public.d VALUES ($k_remote, public.wr($k_remote))", $forwarded_re);
refused('SQL function in a routed INSERT', $n1,
	"INSERT INTO public.d VALUES ($k_remote, public.wr_sql($k_remote))", $forwarded_re);
refused('dynamic SQL in a routed INSERT', $n1,
	"INSERT INTO public.d VALUES ($k_remote, public.wr_dyn($k_remote))", $forwarded_re);
refused('function in a routed SELECT', $n1,
	"SELECT public.wr($k_remote) FROM public.d WHERE id = $k_remote", $forwarded_re);
refused('function in a routed UPDATE', $n1,
	"UPDATE public.d SET v = public.wr($k_remote) WHERE id = $k_remote", $forwarded_re);
refused('function in a routed DO block statement', $n1,
	"DO \$\$ BEGIN INSERT INTO public.d VALUES ($k_remote3, public.wr($k_remote3)); END \$\$", $forwarded_re);
is($n2->safe_psql('postgres', 'SELECT count(*) FROM public.d'), '2', 'S3-01: the routed statements changed nothing');
same_everywhere('S3-01 refused writes', 'logr');
is(table_text($n1, 'logr'), '', 'S3-01: no copy was written');

# The same function called by a statement that runs here is a write of its own:
# it is fanned out like any other statement, on every copy.
$n1->safe_psql('postgres', "INSERT INTO public.d VALUES ($k_local, public.wr($k_local))");
same_everywhere('function in a local statement', 'logr');
is(table_text($n2, 'logr'), "($k_local,wr)", 'the function in a local statement wrote every copy');

# A trigger on a bucket partition (made by hand: DDL on partitions is refused),
# which a routed statement fires on the node that owns the bucket.
{
	my $part = $n2->safe_psql('postgres', qq{
		SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		 WHERE i.inhparent = 'public.d'::regclass
		   AND c.relname = format('d_b%s', lpad(kingres.bucket_for('public.d'::regclass, $k_remote)::text, 4, '0'))});
	$n2->safe_psql('postgres', qq{
		SET kingres_shard.enable_routing = off;
		CREATE FUNCTION public.trg_wr() RETURNS trigger LANGUAGE plpgsql AS
		  \$\$ BEGIN INSERT INTO public.logr VALUES (NEW.id, 'trg'); RETURN NEW; END \$\$;
		CREATE TRIGGER wr_trg AFTER INSERT ON public.$part FOR EACH ROW EXECUTE FUNCTION public.trg_wr();
	});
	$n1->safe_psql('postgres', "DELETE FROM public.d WHERE id = $k_remote");
	refused('trigger on a bucket partition', $n1,
		"INSERT INTO public.d VALUES ($k_remote, 'x')", $forwarded_re);
	is($n2->safe_psql('postgres', 'SELECT count(*) FROM public.d'), '2', 'the statement with the trigger changed nothing');
	same_everywhere('S3-01 trigger', 'logr');
	$n2->safe_psql('postgres', "SET kingres_shard.enable_routing = off; DROP TRIGGER wr_trg ON public.$part");
}

# An ordinary table's trigger or rule, fired by a statement that runs here.
$n1->safe_psql('postgres', q{
	CREATE FUNCTION public.ot_trg() RETURNS trigger LANGUAGE plpgsql AS
	  $$ BEGIN INSERT INTO public.logr VALUES (NEW.i, 'ot'); RETURN NEW; END $$;
	CREATE TRIGGER ot_t AFTER INSERT ON public.ot FOR EACH ROW EXECUTE FUNCTION public.ot_trg();
	CREATE TABLE public.orule (i int);
	CREATE RULE orule_r AS ON INSERT TO public.orule DO ALSO INSERT INTO public.logr2 VALUES (NEW.i, 'rule');
});
$n1->safe_psql('postgres', 'INSERT INTO public.ot VALUES (501), (502)');
# A rule's action reads the table the rule is on (through NEW), which the write to
# a reference table may not: the statement is refused, whole.
refused('rule action writing a reference table', $n1, 'INSERT INTO public.orule VALUES (601)',
	qr/cannot be run on every node/);
same_everywhere('trigger on an ordinary table, rule', 'logr', 'logr2');
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.orule'), '0', 'the refused rule statement changed nothing');
like(table_text($n3, 'logr'), qr/\(501,ot\),\(502,ot\)/, 'the trigger of an ordinary table wrote every copy');

# ---------------------------------------------------------------------------
# Other paths.
# ---------------------------------------------------------------------------
# A data-modifying CTE.
refused('data-modifying CTE', $n1,
	'WITH x AS (INSERT INTO public.logr VALUES (700, $$cte$$) RETURNING k) SELECT * FROM x', qr/WITH clause or a subquery/);
refused('data-modifying CTE in a write', $n1,
	'WITH x AS (INSERT INTO public.logr2 VALUES (700, $$cte$$) RETURNING k) INSERT INTO public.logr SELECT k, $$c$$ FROM x',
	qr/cannot be run on every node|WITH clause|reads a table/);
same_everywhere('CTE', 'logr', 'logr2');

# MERGE and COPY FROM are refused, TRUNCATE is propagated.
refused('MERGE', $n1,
	q{MERGE INTO public.logr t USING (VALUES (1, 'm')) s(k, v) ON t.k = s.k WHEN NOT MATCHED THEN INSERT VALUES (s.k, s.v)},
	qr/MERGE into a reference table is not supported/);
refused('COPY FROM', $n1, q{COPY public.logr FROM STDIN}, qr/not supported/);
same_everywhere('MERGE and COPY', 'logr');
$n1->safe_psql('postgres', q{INSERT INTO public.logr2 VALUES (800, 'a'), (801, 'b'); TRUNCATE public.logr2});
same_everywhere('TRUNCATE', 'logr2');
is(table_text($n3, 'logr2'), '', 'TRUNCATE emptied every copy');

# INSERT ... SELECT between reference tables is propagated; from another source it is refused.
$n1->safe_psql('postgres', q{INSERT INTO public.logr2 SELECT k + 5000, v FROM public.logr});
same_everywhere('INSERT ... SELECT', 'logr', 'logr2');
refused('INSERT ... SELECT from an ordinary table', $n1,
	q{INSERT INTO public.logr2 SELECT i + 9000, 'o' FROM public.ot}, qr/cannot be run on every node|not a reference table/);

# Prepared statements, custom and generic plans.
$n1->safe_psql('postgres', q{TRUNCATE public.logr2});
{
	my $s = $n2->background_psql('postgres', on_error_stop => 0);
	$s->query_safe('SET plan_cache_mode = force_generic_plan');
	$s->query_safe(q{PREPARE ins AS INSERT INTO public.logr2 VALUES ($1, 'p')});
	$s->query_safe('EXECUTE ins(1)');
	$s->query_safe('EXECUTE ins(2)');
	$s->query_safe('SET plan_cache_mode = force_custom_plan');
	$s->query_safe('EXECUTE ins(3)');
	$s->quit;
	same_everywhere('prepared statements', 'logr2');
	is(table_text($n3, 'logr2'), '(1,p),(2,p),(3,p)', 'prepared writes reached every copy');
}

# FK cascades between reference tables run once on every copy.
$n1->safe_psql('postgres', q{
	CREATE TABLE public.rp (id int PRIMARY KEY);
	CREATE TABLE public.rc (id int PRIMARY KEY, p int);
	CREATE TABLE public.rn (id int PRIMARY KEY, p int);
	SELECT kingres.create_reference_table('public.rp');
	SELECT kingres.create_reference_table('public.rc');
	SELECT kingres.create_reference_table('public.rn');
	ALTER TABLE public.rc ADD CONSTRAINT rc_fk FOREIGN KEY (p) REFERENCES public.rp ON DELETE CASCADE ON UPDATE CASCADE;
	ALTER TABLE public.rn ADD CONSTRAINT rn_fk FOREIGN KEY (p) REFERENCES public.rp ON DELETE SET NULL ON UPDATE SET NULL;
});
$n1->safe_psql('postgres', q{
	INSERT INTO public.rp VALUES (1), (2), (3);
	INSERT INTO public.rc VALUES (10, 1), (20, 2), (30, 3);
	INSERT INTO public.rn VALUES (10, 1), (20, 2), (30, 3);
});
for my $n ($n2, $n1)
{
	my $name = $n->name;
	$n->safe_psql('postgres', 'DELETE FROM public.rp WHERE id = ' . ($n == $n2 ? 1 : 2));
	same_everywhere("cascade delete from $name", 'rp', 'rc', 'rn');
}
$n3->safe_psql('postgres', 'UPDATE public.rp SET id = 33 WHERE id = 3');
same_everywhere('cascade update from node 3', 'rp', 'rc', 'rn');
is($n2->safe_psql('postgres', 'SELECT p FROM public.rc WHERE id = 30'), '33', 'ON UPDATE CASCADE reached every copy');

# ---------------------------------------------------------------------------
# S3-02: a plan made while the table was an ordinary table.
# ---------------------------------------------------------------------------
$_->safe_psql('postgres', q{CREATE TABLE IF NOT EXISTS public.tt (id int PRIMARY KEY, v int)}) for ($n1);
$n1->safe_psql('postgres', q{INSERT INTO public.tt VALUES (1, 0)});
{
	my $s = $n1->background_psql('postgres', on_error_stop => 0);
	$s->query_safe('SET plan_cache_mode = force_generic_plan');
	$s->query_safe('PREPARE u AS UPDATE public.tt SET v = v + 1 WHERE id = 1');
	$s->query_safe('EXECUTE u');
	$s->query_safe(q{CREATE FUNCTION public.bump() RETURNS void LANGUAGE plpgsql AS $$ BEGIN UPDATE public.tt SET v = v + 10 WHERE id = 1; END $$});
	$s->query_safe('SELECT public.bump()');
	$n1->safe_psql('postgres', q{SELECT kingres.create_reference_table('public.tt')});
	my ($ok1, $err1) = run_in($s, 'EXECUTE u');
	my ($ok2, $err2) = run_in($s, 'SELECT public.bump()');
	$s->quit;
	is(table_text($n2, 'tt'), table_text($n1, 'tt'), 'S3-02: node 2 holds the same copy');
	is(table_text($n3, 'tt'), table_text($n1, 'tt'), 'S3-02: node 3 holds the same copy');
	ok($ok1, 'S3-02: the cached prepared statement is planned again: ' . ($err1 // ''));
	ok($ok2, 'S3-02: the cached plan of a function is planned again: ' . ($err2 // ''));
	is(table_text($n1, 'tt'), '(1,22)', 'S3-02: both ran, and on every copy');
}

# A plan made by a node that did not know the table was a reference table.
{
	my $s = $n3->background_psql('postgres', on_error_stop => 0);
	$s->query_safe('SET plan_cache_mode = force_generic_plan');
	$s->query_safe('PREPARE w AS UPDATE public.tt SET v = v + 100 WHERE id = 1');
	$s->query_safe('EXECUTE w');
	$s->query_safe('EXECUTE w');
	$s->quit;
	same_everywhere('generic plan on another node', 'tt');
}

# ---------------------------------------------------------------------------
# Divergence is detectable: check_cluster compares the copies.
# ---------------------------------------------------------------------------
is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'reference_table_differs'}),
	'0', 'check_cluster: the copies agree');
$n2->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; UPDATE public.logr2 SET v = 'tampered' WHERE k = 1});
my $rep = $n1->safe_psql('postgres',
	q{SELECT check_name || '|' || coalesce(object, '') || '|' || coalesce(node_id::text, '') FROM kingres.check_cluster() WHERE check_name = 'reference_table_differs'});
like($rep, qr/^reference_table_differs\|public\.logr2\|/m, 'check_cluster reports a table whose copies differ (same row count, other content)');
$n3->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; DELETE FROM public.logr2 WHERE k = 2});
$rep = $n1->safe_psql('postgres',
	q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'reference_table_differs' AND object = 'public.logr2'});
cmp_ok($rep, '>=', 1, 'check_cluster reports the table again when a row is missing');
is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.check_cluster() WHERE object <> 'public.logr2' AND check_name = 'reference_table_differs'}),
	'0', 'no other table is reported');

done_testing();

# Run one statement in a background session; (success, error text).
sub run_in
{
	my ($s, $sql) = @_;
	my $before = length($s->{stderr} // '');
	$s->query($sql);
	my $after = $s->{stderr} // '';
	return (length($after) == $before ? 1 : 0, substr($after, $before));
}
