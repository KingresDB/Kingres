# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/005_session_settings.pl
#   Review 2026-11-14, S-C: a forwarded statement must mean the same on the
#   remote node whatever the session settings of either node are.  The same
#   statement is run for a key of the local group and for a key of the remote
#   group under non-default DateStyle, IntervalStyle, extra_float_digits,
#   standard_conforming_strings, bytea_output, TimeZone and a remote node of
#   another encoding; the two rows must be identical, and so must what a
#   forwarded read returns.

use strict;
use warnings FATAL => 'all';
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

# Node 1 is UTF8, node 2 is LATIN1: the text of a forwarded statement is
# converted between the two.
sub make_node
{
	my ($name, $encoding) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init(extra => [ "--encoding=$encoding", '--locale=C' ]);
	$node->append_conf('postgresql.conf',
		"shared_preload_libraries = 'kingres_shard'");
	$node->start;
	return $node;
}

my $n1 = make_node('c1', 'UTF8');
my $n2 = make_node('c2', 'LATIN1');

$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$n1->safe_psql('postgres',
	sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));
$n1->safe_psql('postgres',
	sprintf("SELECT kingres.add_node('%s', %d)", $n2->host, $n2->port));
$n1->safe_psql('postgres', q{
	CREATE TABLE public.ses (id int PRIMARY KEY, d date, ts timestamptz, ts0 timestamp,
	                         i interval, f float8, n numeric, s text, b bytea, tm time,
	                         d2 date, d3 date);
	SELECT kingres.create_distributed_table('public.ses', 'id', bucket_count => 8);
});

# Keys of the local group (0) and of the remote group (1), as seen from node 1.
sub keys_of
{
	my ($group, $from, $count) = @_;
	my $list = $n1->safe_psql('postgres', qq{
		SELECT string_agg(k::text, ',' ORDER BY k) FROM (
		  SELECT k FROM generate_series($from, $from + 1000) k
		   WHERE (SELECT m.group_id FROM kingres.bucket_map m
		           WHERE m.colocation_id = 1
		             AND m.bucket = kingres.bucket_for(1, k)) = $group
		   LIMIT $count) s});
	return split /,/, $list;
}
my @local_keys  = keys_of(0, 1, 20);
my @remote_keys = keys_of(1, 1, 20);

# One row of values; every column can be overridden.
sub insert_sql
{
	my ($id, %o) = @_;
	my %v = (
		d   => q{'2026-03-04'},
		ts  => q{'2026-03-04 10:00:00+00'},
		ts0 => q{'2026-03-04 10:00:00'},
		i   => q{'1 year 2 mons 3 days 04:05:06'},
		f   => q{'0.1'::float8},
		n   => q{'12345.6789'::numeric},
		s   => q{'plain'},
		b   => q{'\x00ff'::bytea},
		tm  => q{'10:11:12'},
		%o,
	);
	return "INSERT INTO public.ses (id, d, ts, ts0, i, f, n, s, b, tm) VALUES ($id, "
	  . join(', ', @v{qw(d ts ts0 i f n s b tm)}) . ");";
}

# What is stored, read directly on the node that owns the row, in a fixed
# output format.
sub stored_row
{
	my ($node, $id) = @_;
	return $node->safe_psql('postgres', qq{
		SET client_encoding = 'UTF8'; SET timezone = 'UTC'; SET extra_float_digits = 3;
		SET datestyle = ISO; SET intervalstyle = postgres; SET bytea_output = hex;
		SELECT d, ts, ts0, i, f, n, encode(convert_to(s, 'UTF8'), 'hex'), encode(b, 'hex'),
		       tm, d2, d3
		  FROM public.ses WHERE id = $id});
}

my $next = 0;

# Run the same statements for a key of each group under the given settings and
# compare what is stored and what a read returns.
sub check_case
{
	my ($label, $settings, %o) = @_;
	my ($kl, $kr) = ($local_keys[$next], $remote_keys[$next]);
	$next++;
	my $extra = $o{after} // '';
	(my $extra_l = $extra) =~ s/%ID%/$kl/g;
	(my $extra_r = $extra) =~ s/%ID%/$kr/g;

	my ($rc, $out, $err);
	my $sql = "$settings\n" . insert_sql($kl, %{ $o{values} // {} }) . "\n"
	  . insert_sql($kr, %{ $o{values} // {} }) . "\n" . $extra_l . "\n" . $extra_r . "\n";
	($rc, $out, $err) = run_sql($n1, $sql);
	is($err, '', "$label: the statements run for a local and a remote key");

	is(stored_row($n2, $kr), stored_row($n1, $kl),
		"$label: the row written through the remote path equals the row written locally");

	my $read = $o{read} // 'd, ts, ts0, i, f, n, s, b, tm, d2, d3';
	($rc, $out, $err) = run_sql($n1, "$settings\n"
		  . "SELECT $read FROM public.ses WHERE id = $kl;\n"
		  . "SELECT $read FROM public.ses WHERE id = $kr;\n");
	my @rows = split /\n/, $out;
	is($rows[1], $rows[0],
		"$label: a forwarded read returns what the same read of a local row returns");
	return;
}

sub run_sql
{
	my ($node, $sql, %extra) = @_;
	my ($stdout, $stderr);
	my $rc = $node->psql('postgres', $sql, stdout => \$stdout,
		stderr => \$stderr, %extra);
	return ($rc, $stdout, $stderr);
}

# ---------------------------------------------------------------------------
# Settings that a remote session must not inherit from the origin, nor leave
# to chance: what a forwarded statement itself sees.
# ---------------------------------------------------------------------------
{
	my $kr = $remote_keys[19];
	$n1->safe_psql('postgres', "INSERT INTO public.ses (id) VALUES ($kr)");
	my ($rc, $out, $err) = run_sql($n1, qq{
		SET datestyle = 'SQL, DMY'; SET intervalstyle = sql_standard;
		SET extra_float_digits = -3; SET standard_conforming_strings = off;
		SET timezone = 'Asia/Tokyo'; SET bytea_output = escape;
		SELECT current_setting('datestyle'), current_setting('intervalstyle'),
		       current_setting('extra_float_digits'),
		       current_setting('standard_conforming_strings'),
		       current_setting('client_encoding'), current_setting('timezone'),
		       current_setting('search_path'), current_setting('bytea_output'),
		       current_setting('kingres_shard.forwarded')
		  FROM public.ses WHERE id = $kr;
		});
	is($err, '', 'the settings probe runs');
	is($out, 'ISO, MDY|postgres|3|on|UTF8|Asia/Tokyo|pg_catalog|hex|on',
		'a forwarded statement runs in a session with fixed output settings and the local TimeZone');
}

# ---------------------------------------------------------------------------
# The same statement for a local and a remote key.
# ---------------------------------------------------------------------------
check_case('datestyle SQL, DMY',
	q{SET datestyle = 'SQL, DMY'; SET timezone = 'Asia/Kolkata';},
	values => {
		d   => q{'03/04/2026'},
		ts  => q{'04/05/2026 10:11:12'},
		ts0 => q{'05/06/2026 07:08:09'},
	});
check_case('datestyle German, DMY',
	q{SET datestyle = 'German, DMY';},
	values => { d => q{'03.04.2026'}, ts0 => q{'05.06.2026 07:08:09'} });
check_case('datestyle Postgres, MDY',
	q{SET datestyle = 'Postgres, MDY';},
	values => { d => q{'Mar 04 2026'} });
check_case('intervalstyle sql_standard',
	q{SET intervalstyle = sql_standard;},
	values => { i => q{'1-2 3 4:05:06'} });
check_case('intervalstyle iso_8601',
	q{SET intervalstyle = iso_8601;},
	values => { i => q{'P1Y2M3DT4H5M6S'} });
check_case('intervalstyle postgres_verbose',
	q{SET intervalstyle = postgres_verbose;},
	values => { i => q{'@ 1 year 2 mons 3 days 4 hours 5 mins 6 secs'} });
check_case('extra_float_digits 0',
	q{SET extra_float_digits = 0;},
	values => { f => q{'0.30000000000000004'::float8}, n => q{'0.30000000000000004'::numeric} });
check_case('extra_float_digits -3',
	q{SET extra_float_digits = -3;},
	values => { f => q{'0.30000000000000004'::float8} });
check_case('standard_conforming_strings off',
	q{SET standard_conforming_strings = off;},
	values => { s => q{E'back\\\\slash ''q'' \\\\n'}, b => q{E'\\\\x00ff'::bytea} });
check_case('standard_conforming_strings on, backslashes',
	q{SET standard_conforming_strings = on;},
	values => { s => q{'back\slash ''q'' \n'} });
check_case('bytea_output escape',
	q{SET bytea_output = escape;},
	values => { b => q{'\x5c27ff00'::bytea} });
check_case('bytea_output escape and standard_conforming_strings off',
	q{SET bytea_output = escape; SET standard_conforming_strings = off;},
	values => { b => q{E'\\\\x5c27ff00'::bytea}, s => q{E'\\\\\\\\'} });
check_case('non-ASCII text under a LATIN1 remote node',
	q{SET standard_conforming_strings = on;},
	values => { s => q{U&'caf\00e9 \00fc \00df'} });
check_case('TimeZone Pacific/Kiritimati',
	q{SET timezone = 'Pacific/Kiritimati';},
	values => { ts => q{'2026-03-05 05:30:00'}, ts0 => q{'2026-03-05 05:30:00'}, tm => q{'23:59:59'} },
	after => q{UPDATE public.ses SET d2 = ts::date WHERE id = %ID%;});
check_case('TimeZone America/Los_Angeles',
	q{SET timezone = 'America/Los_Angeles';},
	values => { ts => q{'2026-03-05 20:30:00'} },
	after => q{UPDATE public.ses SET d2 = ts::date WHERE id = %ID%;});
check_case('everything at once',
	q{SET datestyle = 'SQL, DMY'; SET intervalstyle = sql_standard; SET extra_float_digits = 0;
	  SET standard_conforming_strings = off; SET bytea_output = escape;
	  SET timezone = 'Asia/Tokyo';},
	values => {
		d   => q{'03/04/2026'},
		ts  => q{'04/05/2026 10:11:12'},
		i   => q{'1-2 3 4:05:06'},
		f   => q{'0.30000000000000004'::float8},
		s   => q{E'back\\\\slash ''q'' \\\\n'},
		b   => q{E'\\\\x5c27ff00'::bytea},
	},
	after => q{UPDATE public.ses SET d2 = ts::date, n = n + 0.5 WHERE id = %ID%;});

# A distributed table whose name is not ASCII, between nodes of two encodings
# (the name is in the statement text, in the DDL script of create_distributed_table
# and in the catalog snapshot).
{
	my ($kl, $kr) = ($local_keys[$next], $remote_keys[$next]);
	$next++;
	$n1->safe_psql('postgres', q{CREATE TABLE public.U&"t\00e1bla" (id int PRIMARY KEY, v text)});
	$n1->safe_psql('postgres', q{SELECT kingres.create_distributed_table(
		format('%I.%I', 'public', U&'t\00e1bla')::regclass, 'id', bucket_count => 8)});
	my ($rc, $out, $err) = run_sql($n1, qq{
		INSERT INTO public.U&"t\\00e1bla" VALUES ($kl, U&'caf\\00e9');
		INSERT INTO public.U&"t\\00e1bla" VALUES ($kr, U&'caf\\00e9');});
	is($err, '', 'rows of a table with a non-ASCII name are written through both paths');
	($rc, $out, $err) = run_sql($n1, qq{
		SELECT v FROM public.U&"t\\00e1bla" WHERE id = $kl;
		SELECT v FROM public.U&"t\\00e1bla" WHERE id = $kr;});
	my @rows = split /\n/, $out;
	is($rows[1], $rows[0], 'and read back the same through both paths');
	is(scalar(@rows), 2, 'and both rows are there');
}

# A TimeZone that changes inside one session and inside one transaction, with
# a cached remote connection in between.  Writes go to one group per
# transaction, so the local and the remote key get one transaction each.
{
	my ($kl, $kr) = ($local_keys[$next], $remote_keys[$next]);
	$next++;
	my ($rc, $out, $err) = run_sql($n1,
		insert_sql($kl, ts => q{'2026-03-05 05:30:00+00'}) . "\n"
		  . insert_sql($kr, ts => q{'2026-03-05 05:30:00+00'}));
	is($err, '', 'the rows of the TimeZone transaction test are written');
	for my $k ($kl, $kr)
	{
		($rc, $out, $err) = run_sql($n1, join("\n",
			"BEGIN;",
			"SET LOCAL timezone = 'Pacific/Kiritimati';",
			"UPDATE public.ses SET d2 = ts::date WHERE id = $k;",
			"SET LOCAL timezone = 'America/Los_Angeles';",
			"UPDATE public.ses SET d3 = ts::date WHERE id = $k;",
			"SAVEPOINT s1;",
			"SET LOCAL timezone = 'Pacific/Kiritimati';",
			"UPDATE public.ses SET d3 = ts::date WHERE id = $k;",
			"ROLLBACK TO s1;",
			"UPDATE public.ses SET d3 = ts::date WHERE id = $k;",
			"COMMIT;",
		));
		is($err, '', "TimeZone changes inside a transaction with a savepoint run for key $k");
	}
	is(stored_row($n2, $kr), stored_row($n1, $kl),
		'the TimeZone of the moment is used for every statement of a transaction');
	like(stored_row($n2, $kr), qr/\|2026-03-05\|2026-03-04$/,
		'the remote row has the dates of the two time zones');
}

foreach my $n ($n1, $n2)
{
	$n->stop;
}

done_testing();
