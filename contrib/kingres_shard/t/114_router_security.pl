# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/114_router_security.pl
#
# Steps 3 and 4 of phase 2b (design/shard-phase2b.md, 11.2, 11.5, 15 and 16.4): the router part of the
# design's 066 (the privileges of the caller on every node) and the batch and log part of its 067.
#
#   A. the privilege matrix of INSERT and COPY FROM on a live cluster: what the origin refuses before it
#      sends anything, what a role with the privileges of some columns can do, and that every group
#      checks the privilege again as the same role (a revoke on one group fails the statement);
#   B. a statement that carries the marker of an insert fragment is an ordinary statement in an ordinary
#      session (nothing is trusted because of a comment);
#   C. the statement text of a batch holds no value (log of the groups), the number of batches is the
#      number of rows over the size of a batch, and the errors of a batch name the batch and no value;
#   D. hostile identifiers and type names: the statements are made of quoted identifiers.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run ();
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2, $n3) = make_cluster('rs', 3, extra => "log_statement = 'all'\n");
my @nodes = ($n1, $n2, $n3);

sub all_nodes
{
	my ($sql) = @_;
	$_->safe_psql('postgres', $sql) for @nodes;
}

# roles exist on every node (roles are not distributed); the grants on a distributed table are
all_nodes(q{CREATE ROLE sec_w LOGIN; CREATE ROLE sec_c LOGIN; CREATE ROLE sec_r LOGIN; CREATE ROLE sec_n LOGIN;
	GRANT USAGE ON SCHEMA public TO sec_w, sec_c, sec_r, sec_n});
$n1->safe_psql('postgres', q{
	CREATE TABLE public.sec (id int PRIMARY KEY, v text, n int DEFAULT 7);
	SELECT kingres.create_distributed_table('public.sec', 'id', bucket_count => 12);
	CREATE TABLE public.secret (id int, v text);
	INSERT INTO public.secret VALUES (1, 's1'), (2, 's2');
	GRANT INSERT, SELECT ON public.sec TO sec_w;
	GRANT INSERT (id, v) ON public.sec TO sec_c;
	GRANT SELECT ON public.sec TO sec_r;
	CREATE TABLE public.srcok (id int, v text);
	INSERT INTO public.srcok SELECT g, 'ok' || g FROM generate_series(1, 30) g;
	GRANT SELECT ON public.srcok TO sec_w, sec_c;
});
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');

sub as_role
{
	my ($node, $role, $sql, $data) = @_;
	my ($out, $err) = ('', '');
	$data //= '';
	my $h = IPC::Run::start(
		[ 'psql', '-XAt', '-v', 'ON_ERROR_STOP=1', '-h', $node->host, '-p', $node->port, '-d', 'postgres', '-U', $role, '-c', $sql ],
		'<', \$data, '>', \$out, '2>', \$err);
	$h->finish;
	return ($h->result(0), $out, $err);
}

sub total { return $n1->safe_psql('postgres', 'SELECT count(*) FROM public.sec') }

sub unchanged
{
	my ($label, $want) = @_;
	is(total(), $want, "$label: no row is left");
	is(prepared_count($_), 0, "$label: nothing is prepared on " . $_->name) for @nodes;
}

# ---------------------------------------------------------------------------
# A. the privilege matrix
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = as_role($n1, 'sec_w', q{INSERT INTO public.sec (id, v) VALUES (1, 'a'), (2, 'b'), (3, 'c'), (4, 'd'), (5, 'e'), (6, 'f')});
	is($rc, 0, 'a role with INSERT and SELECT inserts several rows') or diag($err);
	($rc, $out, $err) = as_role($n1, 'sec_w', q{INSERT INTO public.sec (id, v) SELECT id + 100, v FROM public.srcok});
	is($rc, 0, '... and runs INSERT ... SELECT') or diag($err);
	is(total(), 36, 'thirty-six rows');

	($rc, $out, $err) = as_role($n2, 'sec_c', q{INSERT INTO public.sec (id, v) VALUES (201, 'a'), (202, 'b'), (203, 'c'), (204, 'd'), (205, 'e'), (206, 'f')});
	is($rc, 0, 'a role with INSERT on two columns inserts them (from another node)') or diag($err);
	is($n1->safe_psql('postgres', q{SELECT string_agg(DISTINCT n::text, ',') FROM public.sec WHERE id BETWEEN 201 AND 206}), '7',
		'... and the groups apply the constant default of the column it has no privilege on');
	($rc, $out, $err) = as_role($n1, 'sec_c', q{COPY public.sec (id, v) FROM STDIN}, join('', map { "$_\tcopy\n" } (211 .. 216)));
	is($rc, 0, '... and loads two columns with COPY') or diag($err);
	is(total(), 48, 'forty-eight rows');

	my $now = total();
	for my $case (
		[ 'sec_c', q{INSERT INTO public.sec (id, v, n) VALUES (301, 'a', 1), (302, 'b', 2)}, qr/permission denied for table sec/, 'a column without INSERT' ],
		[ 'sec_c', q{INSERT INTO public.sec (id, v) VALUES (301, 'a'), (302, 'b') RETURNING id}, qr/permission denied for table sec/, 'RETURNING a column without SELECT' ],
		[ 'sec_r', q{INSERT INTO public.sec (id, v) VALUES (301, 'a'), (302, 'b')}, qr/permission denied for table sec/, 'a role that can only read' ],
		[ 'sec_r', q{INSERT INTO public.sec (id, v) SELECT id + 300, v FROM public.srcok}, qr/permission denied for table sec/, 'a role that can only read, INSERT ... SELECT' ],
		[ 'sec_n', q{INSERT INTO public.sec (id, v) VALUES (301, 'a'), (302, 'b')}, qr/permission denied for table sec/, 'a role with no privilege' ],
		[ 'sec_w', q{INSERT INTO public.sec (id, v) SELECT id + 300, v FROM public.secret}, qr/permission denied for table secret/, 'a source the role may not read' ],
	)
	{
		my ($role, $sql, $re, $name) = @$case;
		($rc, $out, $err) = as_role($n1, $role, $sql);
		isnt($rc, 0, "$name: refused");
		like($err, $re, '... with the error of PostgreSQL');
		unchanged($name, $now);
	}
	# COPY
	for my $case (
		[ 'sec_c', q{COPY public.sec (id, v, n) FROM STDIN}, "301\ta\t1\n", qr/permission denied for table sec/, 'COPY a column without INSERT' ],
		[ 'sec_r', q{COPY public.sec (id, v) FROM STDIN}, "301\ta\n", qr/permission denied for table sec/, 'COPY by a role that can only read' ],
		[ 'sec_n', q{COPY public.sec (id, v) FROM STDIN}, "301\ta\n", qr/permission denied for table sec/, 'COPY by a role with no privilege' ],
		[ 'sec_w', q{COPY public.sec (id, v) FROM '/etc/hostname'}, '', qr/permission denied to COPY from a file/, 'COPY from a file without pg_read_server_files' ],
		[ 'sec_w', q{COPY public.sec (id, v) FROM PROGRAM 'true'}, '', qr/permission denied to COPY to or from an external program/, 'COPY from a program without pg_execute_server_program' ],
	)
	{
		my ($role, $sql, $data, $re, $name) = @$case;
		($rc, $out, $err) = as_role($n1, $role, $sql, $data);
		isnt($rc, 0, "$name: refused");
		like($err, $re, '... with the error of PostgreSQL');
		# the checks of DoCopy are repeated at the origin: the refusal is the origin's, before any batch
		unlike($err, qr/bulk insert of/, '... by the origin, before any batch was built');
		unchanged($name, $now);
	}
	# with the role the statement is allowed, the file is read (an empty file loads nothing)
	all_nodes(q{GRANT pg_read_server_files TO sec_w});
	($rc, $out, $err) = as_role($n1, 'sec_w', q{COPY public.sec (id, v) FROM '/dev/null'});
	is($rc, 0, 'COPY from a file with pg_read_server_files') or diag($err);
	all_nodes(q{REVOKE pg_read_server_files FROM sec_w});

	# every group checks the privilege again, as the same role
	my $i = 0;
	for my $nd ($n2, $n3)
	{
		$nd->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; REVOKE INSERT ON public.sec FROM sec_w});
		($rc, $out, $err) = as_role($n1, 'sec_w', q{INSERT INTO public.sec (id, v) SELECT g + 400, 'x' FROM generate_series(1, 90) g});
		isnt($rc, 0, 'INSERT fails when a group has revoked the privilege (' . $nd->name . ')');
		like($err, qr/permission denied for table sec|permission denied for table sec_b/, '... with the error of that group');
		unchanged('after the revoke on ' . $nd->name, $now);
		($rc, $out, $err) = as_role($n1, 'sec_w', q{COPY public.sec (id, v) FROM STDIN}, join('', map { ($_ + 500) . "\tx\n" } (1 .. 90)));
		isnt($rc, 0, 'COPY fails when a group has revoked the privilege (' . $nd->name . ')');
		unchanged('after the COPY and the revoke on ' . $nd->name, $now);
		$nd->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; GRANT INSERT ON public.sec TO sec_w});
		$i++;
	}
	($rc, $out, $err) = as_role($n1, 'sec_w', q{INSERT INTO public.sec (id, v) SELECT g + 400, 'x' FROM generate_series(1, 90) g});
	is($rc, 0, 'with the privilege back the same statement works') or diag($err);
	is(total(), $now + 90, '... and inserts its rows');
	$n1->safe_psql('postgres', q{DELETE FROM public.sec WHERE id > 400});
}

# ---------------------------------------------------------------------------
# B. a marker is a comment
# ---------------------------------------------------------------------------
{
	my $now = total();
	my $marker = '/* kingres:insert-fragment place=0 */ ';
	# the wire statement of the sender, sent by hand in an ordinary session with the parameters of the extended protocol
	my ($out, $err) = ('', '');
	my $script = $marker . q{INSERT INTO public.sec (id, v) SELECT u.id::integer, u.v::pg_catalog.text FROM unnest($1::pg_catalog.text[], $2::pg_catalog.text[]) AS u(id, v) } .
		qq{\\bind '{801,802,803,804,805,806,807,808}' '{a,b,c,d,e,f,g,h}' \\g\n};
	my $h = IPC::Run::start([ 'psql', '-XAt', '-v', 'ON_ERROR_STOP=1', '-h', $n2->host, '-p', $n2->port, '-d', 'postgres', '-U', 'sec_w', '-f', '-' ],
		'<', \$script, '>', \$out, '2>', \$err);
	$h->finish;
	my $rc = $h->result(0);
	diag($err) if $rc != 0;
	is(total(), $now + 8, 'the statement of a fragment, sent by a user, inserts its rows');
	my $wrong = 0;
	my $i = 0;
	for my $nd (@nodes)
	{
		my $out2 = $nd->safe_psql('postgres', qq{SET kingres_shard.enable_routing = off;
			SELECT count(*) FROM public.sec k JOIN kingres.bucket_map m ON m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, k.id)
			 WHERE k.id BETWEEN 801 AND 808 AND m.group_id <> $i});
		$wrong += $out2;
		$i++;
	}
	is($wrong, 0, '... in the groups that own them (the marker is not a reason to store a row where it came in)');
	$n1->safe_psql('postgres', q{DELETE FROM public.sec WHERE id > 800});
}

# ---------------------------------------------------------------------------
# C. batches and the log
# ---------------------------------------------------------------------------
{
	my @offset = map { -s $_->logfile } @nodes;
	my ($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.insert_batch_rows = 10;
		INSERT INTO public.sec (id, v) SELECT g + 1000, 'SENTINEL_VALUE_' || g FROM generate_series(1, 200) g});
	is($rc, 0, 'a bulk insert of two hundred rows in batches of ten') or diag($err);
	my @counts;
	my $i = 0;
	for my $nd (@nodes)
	{
		my $log = slurp_file($nd->logfile, $offset[$i]);
		my @frag = grep { /insert-fragment/ } split /\n/, $log;
		push @counts, scalar(@frag);
		my $rows = $nd->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.sec WHERE v LIKE 'SENTINEL%'});
		if ($i > 0)
		{
			my @statements = grep { /LOG:\s+(execute|statement|duration)/ } @frag;
			is(scalar(@frag) > 0 ? 1 : 0, 1, 'the log of ' . $nd->name . ' shows the batches');
			is(scalar(grep { /SENTINEL/ } @frag), 0, '... and no value is in the text of a statement');
			is(scalar(grep { /insert-fragment place=-?\d+ \*\/ INSERT INTO public\.sec \(id, v\) SELECT u\.id::integer, u.v::pg_catalog.text FROM unnest\(\$1::pg_catalog\.text\[\], \$2::pg_catalog\.text\[\]\) AS u\(id, v\)/ } @frag),
				scalar(@frag), '... which are all of the shape of 11.3');
			is($counts[-1], int(($rows + 9) / 10), "... and their number is the rows of the group over the size of a batch ($rows rows)");
		}
		$i++;
	}
	# insert_batch_bytes: a batch is sent when its text reaches the size
	@offset = map { -s $_->logfile } @nodes;
	($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.insert_batch_bytes = '16kB'; SET kingres_shard.insert_batch_rows = 100000;
		INSERT INTO public.sec (id, v) SELECT g + 3000, repeat('b', 4000) FROM generate_series(1, 120) g});
	is($rc, 0, 'wide rows with batches of sixteen kilobytes') or diag($err);
	$i = 0;
	for my $nd (@nodes)
	{
		my $log = slurp_file($nd->logfile, $offset[$i]);
		my $nfrag = scalar(grep { /insert-fragment/ } split /\n/, $log);
		my $rows = $nd->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.sec WHERE id BETWEEN 3001 AND 3120});
		# a batch is sent when it holds 16 kB: the fifth row of 4000 bytes; so a batch holds five rows
		is($nfrag, $i == 0 ? 0 : int(($rows + 4) / 5), "the batches of " . $nd->name . " are of five rows of 4000 bytes ($rows rows)") if $i > 0;
		$i++;
	}
	# an error names the batch and no value
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.sec (id, v) SELECT g + 1000, 'SENTINEL_AGAIN' FROM generate_series(1, 20) g});
	isnt($rc, 0, 'a duplicate key fails');
	like($err, qr/bulk insert of public\.sec, batch \d+ to group \d/, '... naming the batch');
	unlike($err, qr/SENTINEL_AGAIN/, '... and no value of a column that is not the key');
	$n1->safe_psql('postgres', q{DELETE FROM public.sec WHERE id > 400});
}

# ---------------------------------------------------------------------------
# D. hostile identifiers
# ---------------------------------------------------------------------------
{
	all_nodes(q{CREATE SCHEMA "We ird"; CREATE TYPE "We ird"."My ""Type""" AS ENUM ('a b', 'c,d', 'e"f')});
	$n1->safe_psql('postgres', q{CREATE TABLE "We ird"."Ta""ble" (id int PRIMARY KEY, "col,1" text, "select" int DEFAULT 5, "x y" "We ird"."My ""Type""", U&"\00C4\00D6" text);
		SELECT kingres.create_distributed_table('"We ird"."Ta""ble"', 'id', bucket_count => 12)});
	my ($rc, $out, $err) = run_sql($n1, q{INSERT INTO "We ird"."Ta""ble" (id, "col,1", "x y", U&"\00C4\00D6") VALUES (1, 'a', 'a b', U&'\00E4'), (2, 'b', 'c,d', U&'\00F6'), (3, 'c', 'e"f', NULL), (4, NULL, NULL, 'x'), (5, 'e', 'a b', 'y'), (6, 'f', 'c,d', 'z') RETURNING id, "select"});
	is($rc, 0, 'INSERT into a table, columns and a type with hostile names') or diag($err);
	is(scalar(() = $out =~ /\|5/g), 6, '... returns the rows with the default');
	my ($o, $e) = ('', '');
	my $data = "11\tq\ta b\n12\tr\tc,d\n13\ts\te\"f\n14\tt\ta b\n";
	my $h = IPC::Run::start([ 'psql', '-XAtq', '-v', 'ON_ERROR_STOP=1', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres', '-c',
		q{COPY "We ird"."Ta""ble" (id, "col,1", "x y") FROM STDIN} ], '<', \$data, '>', \$o, '2>', \$e);
	$h->finish;
	is($h->result(0), 0, 'COPY into the same table') or diag($e);
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM "We ird"."Ta""ble"}), 10, '... both are there');
	is($n1->safe_psql('postgres', q{SELECT string_agg("x y"::text, '/' ORDER BY id) FROM "We ird"."Ta""ble" WHERE id IN (2, 3)}), 'c,d/e"f', 'the values of the type with a hostile name arrive intact');
}

done_testing();
