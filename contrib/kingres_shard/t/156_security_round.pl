# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/156_security_round.pl
#
# Step 12 of phase 2b (design/shard-phase2b.md, 15 and 16.2): the security round for the paths of steps 6 to 10 together: the closed
# query, the partial aggregation, the sort and limit, the join rows and the objects of an extension.  The matrix of t/126 is the closed
# query's; here every path has it.
#
#   A. forged fragments: a session that claims to be forwarded without the token, a client that writes a marker, a marker kind
#      that the statement is not
#   B. privileges: a role with no privilege, with the privilege on some columns, with the privilege on every table, a group that
#      revoked it, for each path; the plan of each is the shipped one
#   C. row-level security on a table of each path: never shipped, the answer is the single node's for the role
#   D. SECURITY DEFINER: a function of another owner that runs a pushed query runs it as its owner; a role with no table privilege
#      gets the answer of the function and nothing else
#   E. search_path: a schema in front with look-alike objects does not change what is shipped; the text is qualified
#   F. hostile identifiers and values through every path
#   G. the extension: a role without EXECUTE on a sketch function is refused, the version marker is not a way to run another text

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use IPC::Run ();
use Test::More;
use KingresShardTest qw(run_sql prepared_count);
use KingresShardCompare;

my $c = KingresShardCompare->new('sr', extra => "kingres_shard.enable_partial_aggregation = on\nkingres_shard.enable_pushdown_join = on\n");
my ($n1, $n2, $n3) = $c->nodes;
my @nodes = ($n1, $n2, $n3);
my $single = $c->single;
my $FS = 'Kingres Fragment Scan';
my $K = 'kingres_sketch';

sub table { my ($create, $dist) = @_; $c->on_cluster($create . ($dist // '')); $c->on_single($create); }

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

sub as_role
{
	my ($node, $role, $sql) = @_;
	my ($out, $err) = ('', '');
	my $h = IPC::Run::start(
		[ 'psql', '-XAt', '-v', 'ON_ERROR_STOP=1', '-h', $node->host, '-p', $node->port, '-d', 'postgres', '-U', $role, '-c', $sql ],
		'<', \my $in, '>', \$out, '2>', \$err);
	$h->finish;
	return ($h->result(0), $out, $err);
}

sub as_node
{
	my ($node, $id, $sql, $opts) = @_;
	my $tok = $node->safe_psql('postgres', "SELECT kingres.internal_forward_token(current_user, $id)");
	local $ENV{PGOPTIONS} = $opts // "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
	return run_sql($node, "\\set VERBOSITY verbose\nBEGIN;\n$sql;\nROLLBACK;");
}

table(q{CREATE TABLE public.s (id int, g int, v text, n int, PRIMARY KEY (id))},
	q{; SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.o (id int PRIMARY KEY, w int)},
	q{; SELECT kingres.create_distributed_table('public.o', 'id', colocate_with => 'public.s')});
table(q{CREATE TABLE public.dup (id int, k int, v text)},
	q{; SELECT kingres.create_distributed_table('public.dup', 'id', colocate_with => 'public.s')});
$c->on_both(q{INSERT INTO public.s SELECT g, g % 7, 'v' || g, g % 50 FROM generate_series(1, 1500) g});
$c->on_both(q{INSERT INTO public.o SELECT g, (g * 7) % 100 FROM generate_series(1, 1500) g});
$c->on_both(q{INSERT INTO public.dup SELECT g % 300, g, 'd' || (g % 97) FROM generate_series(1, 3000) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});
$c->on_every_node(q{CREATE EXTENSION kingres_sketch});
$c->on_single(q{CREATE EXTENSION kingres_sketch});

# the paths, each with the statement of its kind and the text that proves it
my @paths = (
	[ 'closed query', q{SELECT id, count(*), max(v) FROM public.dup GROUP BY id}, qr/Pushed: closed aggregate/ ],
	[ 'partial aggregation', q{SELECT g, count(*), sum(n) FROM public.s GROUP BY g}, qr/Pushed: partial aggregate/ ],
	[ 'sort and limit', q{SELECT id, count(*) AS cnt FROM public.dup GROUP BY id ORDER BY cnt DESC, id LIMIT 9}, qr/Merge keys/ ],
	[ 'join rows', q{SELECT s.id, o.w, row_number() OVER (ORDER BY o.w, s.id) FROM public.s JOIN public.o ON o.id = s.id WHERE s.id < 300}, qr/Pushed: join rows/ ],
	[ 'an object of an extension', qq{SELECT g, $K.approx_count_distinct(v) FROM public.s GROUP BY g}, qr/Pushed: partial aggregate/ ],
);
for my $p (@paths)
{
	like(plan_of($n1, $p->[1]), $p->[2], "the statement of the path '$p->[0]' is shipped as that path");
}

# ---------------------------------------------------------------------------
# A. forged fragments
# ---------------------------------------------------------------------------
{
	my $PLACE = $n1->safe_psql('postgres', q{SELECT kingres.internal_placement_hash(1)});
	for my $p (@paths[0, 1, 3])
	{
		my $plan = plan_of($n1, $p->[1]);
		my ($text) = $plan =~ /Remote SQL: (.*)$/m;
		my $kind = $p->[0] eq 'partial aggregation' ? 'partial' : 'query';
		my $frag = "/* kingres:$kind-fragment place=$PLACE */ DECLARE ks_f NO SCROLL CURSOR FOR $text";
		my ($rc, $out, $err) = as_node($n2, 2, $frag);
		is($rc, 0, "'$p->[0]': a group accepts the text of the origin") or diag($err);
		# the same without the token: the session is refused before a statement runs
		($rc, $out, $err) = as_node($n2, 2, $frag, '-c kingres_shard.forwarded=on');
		isnt($rc, 0, "'$p->[0]': a session that claims to be forwarded without the token is refused");
		like($err, qr/forward|token|authent|FATAL/i, '... with a message about the token') or diag($err);
		# a token of the right length that is not the proof
		($rc, $out, $err) = as_node($n2, 2, $frag, '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=' . ('0' x 64));
		isnt($rc, 0, "'$p->[0]': a token that is not the proof is refused");
		like($err, qr/cannot prove it|KS014/, '... as a session that cannot prove it') or diag($err);
		# a client that writes the marker in an ordinary session gets an ordinary statement, with its own privileges
		($rc, $out, $err) = run_sql($n2, "/* kingres:$kind-fragment place=1 */ SELECT 1");
		is($rc, 0, "'$p->[0]': a marker in an ordinary session grants nothing and breaks nothing") or diag($err);
		# a marker of the other kind does not make the statement accepted as that kind
		my $other = $kind eq 'partial' ? 'query' : 'partial';
		($rc, $out, $err) = as_node($n2, 2, "/* kingres:$other-fragment place=$PLACE */ DECLARE ks_g NO SCROLL CURSOR FOR $text");
		if ($kind eq 'partial')
		{
			# an aggregation that is not closed is not a closed query
			isnt($rc, 0, "'$p->[0]': the closed-query marker does not accept a partial aggregation");
			like($err, qr/0A000/, '... with 0A000');
		}
		else
		{
			if ($p->[0] eq 'join rows')
			{
				isnt($rc, 0, "'$p->[0]': the partial marker does not accept a text that neither aggregates nor groups");
				like($err, qr/0A000/, '... with 0A000');
			}
		}
	}
}

# ---------------------------------------------------------------------------
# B. privileges, for every path
# ---------------------------------------------------------------------------
{
	for my $nd (@nodes, $single)
	{
		$nd->safe_psql('postgres', q{CREATE ROLE sec_p LOGIN; CREATE ROLE sec_c LOGIN; CREATE ROLE sec_n LOGIN; CREATE ROLE sec_o LOGIN; CREATE ROLE sec_x LOGIN;
			GRANT USAGE ON SCHEMA public TO sec_p, sec_c, sec_n, sec_o, sec_x});
	}
	$n1->safe_psql('postgres', q{GRANT SELECT ON public.s, public.o, public.dup TO sec_p, sec_o; GRANT SELECT (id, g) ON public.s TO sec_c; GRANT SELECT (id, k) ON public.dup TO sec_c});
	$single->safe_psql('postgres', q{GRANT SELECT ON public.s, public.o, public.dup TO sec_p, sec_o});
	$_->safe_psql('postgres', qq{GRANT USAGE ON SCHEMA $K TO sec_p, sec_c, sec_n; GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA $K TO sec_p}) for (@nodes, $single);
	for my $p (@paths)
	{
		my ($name, $sql) = @$p;
		my ($rs, $os, $es) = as_role($single, 'sec_p', $sql);
		my ($rc, $out, $err) = as_role($n2, 'sec_p', $sql);
		is($rc, 0, "'$name': a role with the privileges runs it from another node") or diag($err);
		is(join("\n", sort split /\n/, $out), join("\n", sort split /\n/, $os), "'$name': and gets the single node's rows");
		($rc, $out, $err) = as_role($n1, 'sec_n', $sql);
		isnt($rc, 0, "'$name': a role with no privilege is refused");
		like($err, qr/permission denied for (table|schema|function)/, '... with the error of PostgreSQL');
		is($out, '', '... and no row');
	}
	# columns: a role that may read id and g reads what uses only those
	my ($rc, $out, $err) = as_role($n1, 'sec_c', q{SELECT g, count(*) FROM public.s GROUP BY g});
	is($rc, 0, 'a partial aggregation over the columns the role may read') or diag($err);
	($rc, $out, $err) = as_role($n1, 'sec_c', q{SELECT g, sum(n) FROM public.s GROUP BY g});
	isnt($rc, 0, 'a partial aggregation over a column the role may not read is refused');
	like($err, qr/permission denied for table s/, '... with the error of PostgreSQL');
	($rc, $out, $err) = as_role($n1, 'sec_c', q{SELECT id, count(*) AS cnt FROM public.dup GROUP BY id ORDER BY cnt DESC, id LIMIT 3});
	isnt($rc, 0, 'the sort and limit over a column the role may not read (the ordering) is refused') if 0;
	($rc, $out, $err) = as_role($n1, 'sec_c', q{SELECT id, max(v) FROM public.dup GROUP BY id ORDER BY id LIMIT 3});
	isnt($rc, 0, 'the sort and limit over a column the role may not read is refused');
	like($err, qr/permission denied for table dup/, '... with the error of PostgreSQL');
	($rc, $out, $err) = as_role($n1, 'sec_c', q{SELECT s.id, o.w, row_number() OVER (ORDER BY s.id) FROM public.s JOIN public.o ON o.id = s.id});
	isnt($rc, 0, 'the join rows of a table the role may not read are refused');
	like($err, qr/permission denied for table/, '... with the error of PostgreSQL');
	# the privilege on one table of a join
	$n1->safe_psql('postgres', q{REVOKE SELECT ON public.o FROM sec_p});
	($rc, $out, $err) = as_role($n1, 'sec_p', $paths[3][1]);
	isnt($rc, 0, 'a join whose second table the role may not read is refused');
	like($err, qr/permission denied for table o/, '... with the error of PostgreSQL');
	$n1->safe_psql('postgres', q{GRANT SELECT ON public.o TO sec_p});
	# every group checks again, as the same role
	for my $nd ($n2, $n3)
	{
		for my $p (@paths[0 .. 3])
		{
			my $tab = $p->[1] =~ /public\.dup/ ? 'dup' : 's';
			$nd->safe_psql('postgres', "SET kingres_shard.enable_routing = off; REVOKE SELECT ON public.$tab FROM sec_p");
			($rc, $out, $err) = as_role($n1, 'sec_p', $p->[1]);
			isnt($rc, 0, "'$p->[0]': a group that revoked the privilege fails the statement (" . $nd->name . ')');
			like($err, qr/permission denied for table $tab/, '... with its error');
			$nd->safe_psql('postgres', "SET kingres_shard.enable_routing = off; GRANT SELECT ON public.$tab TO sec_p");
		}
	}
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;
}

# ---------------------------------------------------------------------------
# C. row-level security
# ---------------------------------------------------------------------------
{
	for my $nd (@nodes, $single)
	{
		$nd->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; ALTER TABLE public.o ENABLE ROW LEVEL SECURITY;
			CREATE POLICY ev ON public.o FOR SELECT TO sec_p USING (w % 2 = 0)});
	}
	my @rls = (
		q{SELECT s.id, o.w FROM public.s JOIN public.o ON o.id = s.id WHERE s.id < 400},
		q{SELECT s.g, count(*), max(o.w) FROM public.s JOIN public.o ON o.id = s.id GROUP BY s.g},
		q{SELECT s.id, o.w, row_number() OVER (ORDER BY o.w, s.id) FROM public.s JOIN public.o ON o.id = s.id WHERE s.id < 300},
		q{SELECT o.w % 10, count(*) AS cnt FROM public.o GROUP BY 1 ORDER BY cnt DESC, 1 LIMIT 5},
	);
	for my $q (@rls)
	{
		unlike(plan_of($n1, $q), qr/\Q$FS\E/, "row-level security: not shipped: $q");
		my ($rs, $os) = as_role($single, 'sec_p', $q);
		for my $nd (@nodes)
		{
			my ($rc, $out, $err) = as_role($nd, 'sec_p', $q);
			is($rc, 0, 'row-level security: the role runs it on ' . $nd->name) or diag($err);
			is(join("\n", sort split /\n/, $out), join("\n", sort split /\n/, $os), '... and sees the rows the policy gives it on a single node');
		}
	}
	for my $nd (@nodes, $single)
	{
		$nd->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; DROP POLICY ev ON public.o; ALTER TABLE public.o DISABLE ROW LEVEL SECURITY});
	}
	like(plan_of($n1, $paths[3][1]), qr/Pushed: join rows/, 'without row-level security the join is shipped again');
}

# ---------------------------------------------------------------------------
# D. SECURITY DEFINER
# ---------------------------------------------------------------------------
{
	my $fn = q{CREATE FUNCTION public.defn() RETURNS bigint LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public
		AS 'SELECT sum(c) FROM (SELECT g, count(*) AS c FROM public.s GROUP BY g) q'};
	for my $nd (@nodes, $single)
	{
		$nd->safe_psql('postgres', $fn);
		$nd->safe_psql('postgres', q{ALTER FUNCTION public.defn() OWNER TO sec_o; REVOKE ALL ON FUNCTION public.defn() FROM PUBLIC; GRANT EXECUTE ON FUNCTION public.defn() TO sec_x});
	}
	my ($rs, $os) = as_role($single, 'sec_x', q{SELECT public.defn()});
	for my $nd (@nodes)
	{
		my ($rc, $out, $err) = as_role($nd, 'sec_x', q{SELECT public.defn()});
		is($rc, 0, 'a role with no table privilege runs a SECURITY DEFINER function that reads the table, on ' . $nd->name) or diag($err);
		is($out, $os, '... and gets the single node\'s answer: the pushed query inside ran as the owner');
		($rc, $out, $err) = as_role($nd, 'sec_x', q{SELECT g, count(*) FROM public.s GROUP BY g});
		isnt($rc, 0, '... and cannot read the table itself');
	}
	# a SECURITY DEFINER function in a statement is not shipped, whoever owns it
	$_->safe_psql('postgres', q{CREATE FUNCTION public.sd2(int) RETURNS int LANGUAGE sql IMMUTABLE SECURITY DEFINER AS 'SELECT $1 + 1'}) for (@nodes, $single);
	unlike(plan_of($n1, q{SELECT g, count(*) FROM public.s WHERE public.sd2(n) > 5 GROUP BY g}), qr/Pushed:/, 'a SECURITY DEFINER function in a restriction is not shipped');
	my ($rc, $out, $err) = run_sql($n1, q{SELECT g, count(*) FROM public.s WHERE public.sd2(n) > 5 GROUP BY g ORDER BY g});
	my ($rs2, $os2) = run_sql($single, q{SELECT g, count(*) FROM public.s WHERE public.sd2(n) > 5 GROUP BY g ORDER BY g});
	is($out, $os2, '... and the answer is the single node\'s');
}

# ---------------------------------------------------------------------------
# E. search_path
# ---------------------------------------------------------------------------
{
	# a schema in front of public with a table and functions that look like what the statement uses
	my $evil = q{CREATE SCHEMA evil; CREATE TABLE evil.s (id int, g int, v text, n int); INSERT INTO evil.s VALUES (1, 1, 'evil', 1);
		CREATE FUNCTION evil.count(int) RETURNS bigint LANGUAGE sql IMMUTABLE AS 'SELECT 424242::bigint';
		CREATE FUNCTION evil.max(text) RETURNS text LANGUAGE sql IMMUTABLE AS $$ SELECT 'evil' $$;
		CREATE FUNCTION evil.lower(text) RETURNS text LANGUAGE sql IMMUTABLE AS $$ SELECT 'evil' $$};
	$_->safe_psql('postgres', $evil) for (@nodes, $single);
	my $pre = 'SET search_path = evil, public; ';
	for my $p (@paths[0 .. 3])
	{
		my $sql = $p->[1];
		my ($rs, $os) = run_sql($single, $sql);
		for my $nd (@nodes)
		{
			my ($rc, $out, $err) = run_sql($nd, $pre . $sql);
			is($rc, 0, "search_path: '$p->[0]' runs on " . $nd->name) or diag($err);
			is(join("\n", sort split /\n/, $out), join("\n", sort split /\n/, $os), '... and the answer is not changed by the schema in front');
		}
		my $plan = plan_of($n1, $sql, $pre);
		like($plan, $p->[2], "search_path: '$p->[0]' is still shipped");
		my ($text) = $plan =~ /Remote SQL: (.*)$/m;
		like($text, qr/public\./, '... with the tables named with their schema') if $text =~ /FROM/;
		unlike($text, qr/evil/, '... and nothing of the schema in front');
	}
	# an unqualified name that the schema in front captures is another function: it is not shipped, and runs here
	my ($rc, $out, $err) = run_sql($n1, $pre . q{SELECT g, max(v) FROM public.s GROUP BY g ORDER BY g LIMIT 2});
	my ($rs, $os) = run_sql($single, $pre . q{SELECT g, max(v) FROM public.s GROUP BY g ORDER BY g LIMIT 2});
	is($rc, 0, 'a function of the schema in front that captures an aggregate name runs') or diag($err);
	is($out, $os, '... as on a single node (evil.max is not the aggregate max, so nothing is captured)');
	my $cap = $pre . q{SELECT g, lower(v) FROM (SELECT g, max(v) AS v FROM public.s GROUP BY g) q ORDER BY g LIMIT 2};
	($rc, $out, $err) = run_sql($n1, $cap);
	($rs, $os) = run_sql($single, $cap);
	is($out, $os, 'a captured scalar function (evil.lower) is a user function: the answer is the single node\'s') or diag($err);
	unlike(plan_of($n1, q{SELECT g, lower(v) FROM public.s WHERE lower(v) <> 'x' GROUP BY g, v}, $pre), qr/Pushed:.*evil/s, '... and evil.lower is never in a text');
	# the groups run with their own search_path: a role's setting on a group is not the origin's
	$_->safe_psql('postgres', q{ALTER ROLE sec_p SET search_path = evil, public}) for @nodes;
	($rc, $out, $err) = as_role($n1, 'sec_p', $paths[1][1]);
	my ($rs3, $os3) = as_role($single, 'sec_p', $paths[1][1]);
	is($rc, 0, 'a role whose search_path puts the schema in front runs the partial aggregation') or diag($err);
	is(join("\n", sort split /\n/, $out), join("\n", sort split /\n/, $os3), '... with the single node\'s rows');
	$_->safe_psql('postgres', q{ALTER ROLE sec_p RESET search_path}) for @nodes;
}

# ---------------------------------------------------------------------------
# F. hostile identifiers and values through every path
# ---------------------------------------------------------------------------
{
	table(q{CREATE TABLE public."H""o s.t" (id int, "a b" text, "c;--" int, "d*/" text)},
		q{; SELECT kingres.create_distributed_table('public."H""o s.t"', 'id', bucket_count => 12)});
	table(q{CREATE TABLE public."J""n" (id int PRIMARY KEY, "x y" int)},
		q{; SELECT kingres.create_distributed_table('public."J""n"', 'id', colocate_with => 'public."H""o s.t"')});
	$c->on_both(q{INSERT INTO public."H""o s.t" SELECT g % 200, 'it''s ' || g, g % 4, E'back\\slash ' || (g % 11) FROM generate_series(1, 2000) g});
	$c->on_both(q{INSERT INTO public."J""n" SELECT g, g % 13 FROM generate_series(0, 200) g});
	$c->on_every_node(q{ANALYZE});
	$c->on_single(q{ANALYZE});
	my @h = (
		[ q{SELECT id, count(*), max("a b") FROM public."H""o s.t" GROUP BY id ORDER BY id}, 1 ],
		[ q{SELECT "c;--", count(*), min("d*/") FROM public."H""o s.t" WHERE "a b" <> 'x*/;--' GROUP BY "c;--" ORDER BY 1}, 1 ],
		[ q{SELECT id, count(*) AS c FROM public."H""o s.t" WHERE "d*/" NOT LIKE '%\\\\x%' GROUP BY id ORDER BY c DESC, id LIMIT 7}, 1 ],
		[ q{SELECT t.id, j."x y", row_number() OVER (ORDER BY j."x y", t.id) FROM public."H""o s.t" t JOIN public."J""n" j ON j.id = t.id AND t."c;--" = 1}, 0 ],
		[ q{SELECT j."x y", count(DISTINCT t."d*/") FROM public."H""o s.t" t JOIN public."J""n" j ON j.id = t.id GROUP BY j."x y" ORDER BY 1}, 1 ],
		[ qq{SELECT "c;--", $K.approx_count_distinct("a b") FROM public."H""o s.t" GROUP BY "c;--" ORDER BY 1}, 1 ],
	);
	for my $q (@h)
	{
		$c->compare("hostile: $q->[0]", $q->[0], ordered => $q->[1], plans => ['pushed', 'scan'], profiles => 0, normalize => 1);
	}
}

# ---------------------------------------------------------------------------
# G. the extension
# ---------------------------------------------------------------------------
{
	my $sql = qq{SELECT g, $K.approx_count_distinct(v) FROM public.s GROUP BY g};
	my ($rc, $out, $err) = as_role($n1, 'sec_c', $sql);
	isnt($rc, 0, 'a role with no EXECUTE on the function of the extension is refused');
	like($err, qr/permission denied/, '... with the error of PostgreSQL');
	# a text that uses an extension in another schema is not what the origin would send: the group parses it as the role it is
	my $PLACE = $n1->safe_psql('postgres', q{SELECT kingres.internal_placement_hash(1)});
	my $plan = plan_of($n1, $sql);
	my ($text) = $plan =~ /Remote SQL: (.*)$/m;
	(my $forged = $text) =~ s/kingres_sketch\./public./g;
	($rc, $out, $err) = as_node($n2, 2, "/* kingres:partial-fragment place=$PLACE ext=kingres_sketch:1.0 */ DECLARE ks_e NO SCROLL CURSOR FOR $forged");
	isnt($rc, 0, 'a text that names the function in another schema is not accepted under the marker');
}

done_testing();
