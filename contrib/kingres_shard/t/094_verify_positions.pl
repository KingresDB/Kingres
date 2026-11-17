# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/094_verify_positions.pl
#
# Review S11-03: kingres.switchover_verify() aborted the backend (an assertion of the WAL reader)
# for every position from 0/18 to 0/1FF8, the first WAL page.  The invariant: the function is
# total.  For every pg_lsn value it returns t or f, or raises KS006; it never ends the backend.
#
# The sweep: every 8-byte position of the first two pages of segment 0 (which never exists) and
# of the first two pages of the first real segment, around a segment boundary, through the page
# that holds the end of the WAL of a node that is in use and the 200 bytes around it, in segments
# that are in the future and far away, and a few thousand random positions.  The node runs with
# assertions (the installation of the test does), on a primary and on a standby.  The answer of
# every call is looked at: an error of another class than KS006 fails the file, and so does a "t"
# (the position of no shutdown checkpoint of this node is ever proven).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster make_standby wait_catchup run_sql);

my ($node) = make_cluster('vp', 1, replication => 1);
$node->safe_psql('postgres', 'CREATE TABLE public.t1 (a int); INSERT INTO public.t1 SELECT generate_series(1, 300)');
$node->safe_psql('postgres', 'CHECKPOINT');
my $sb = make_standby($node, 'vp_s');
wait_catchup($node, $sb);

is($node->safe_psql('postgres', "SELECT current_setting('wal_segment_size')"), '16MB',
	'the segments are 16MB (the positions below are computed for that)');

# One DO block per set: each position is one call inside its own subtransaction.  __ARRAY__ is an
# expression that yields the positions.  The result comes back as a NOTICE.
my $sweep_do = <<'EOS';
DO $do$
DECLARE
	positions pg_lsn[] := __ARRAY__;
	p pg_lsn;
	r boolean;
	n_f int := 0;
	n_t int := 0;
	n_err int := 0;
	bad text := '';
BEGIN
	FOREACH p IN ARRAY positions LOOP
		BEGIN
			r := kingres.switchover_verify(p, 'shut down');
			IF r THEN
				n_t := n_t + 1;
				bad := bad || ' t at ' || p;
			ELSE
				n_f := n_f + 1;
			END IF;
		EXCEPTION WHEN OTHERS THEN
			IF SQLSTATE = 'KS006' THEN
				n_err := n_err + 1;
			ELSE
				bad := bad || ' ' || SQLSTATE || ' at ' || p;
			END IF;
		END;
	END LOOP;
	RAISE NOTICE 'RESULT false=% true=% KS006=% of % ;%', n_f, n_t, n_err, array_length(positions, 1), bad;
END $do$;
EOS

sub lsn_array
{
	my ($sql) = @_;
	return "(SELECT array_agg(p) FROM ($sql) s(p))";
}

my $big = '1099511627776';		# 2^40
my @sets = (
	[ 'the first two pages of segment 0',
	  lsn_array("SELECT '0/0'::pg_lsn + g FROM generate_series(0, 16376, 8) g") ],
	[ 'the first two pages of the first segment',
	  lsn_array("SELECT '0/1000000'::pg_lsn + g FROM generate_series(0, 16376, 8) g") ],
	[ 'both sides of a segment boundary',
	  lsn_array("SELECT '0/2000000'::pg_lsn + g FROM generate_series(-96, 96, 8) g") ],
	[ 'the page that holds the end of the WAL',
	  lsn_array("SELECT (END_LSN - (END_LSN - '0/0'::pg_lsn) % 8192) + g FROM generate_series(0, 8184, 8) g") ],
	[ 'the 200 bytes around the end of the WAL',
	  lsn_array("SELECT (END_LSN - (END_LSN - '0/0'::pg_lsn) % 8) + g FROM generate_series(-200, 200, 8) g") ],
	[ 'future and far segments',
	  lsn_array("SELECT '0/3000000'::pg_lsn + g FROM generate_series(0, 16376, 8) g UNION ALL SELECT '0/10000000'::pg_lsn + g FROM generate_series(0, 64, 8) g UNION ALL SELECT '7FFFFFFF/FFFFFFF0'::pg_lsn - g FROM generate_series(0, 64, 8) g UNION ALL SELECT 'FFFFFFFF/FFFFFFF8'::pg_lsn - g FROM generate_series(0, 64, 8) g") ],
	[ 'three thousand random positions',
	  lsn_array("SELECT '0/0'::pg_lsn + (((random() * $big)::bigint / 8) * 8) FROM generate_series(1, 3000)") ],
	[ 'three thousand random positions below the end of the WAL',
	  lsn_array("SELECT '0/0'::pg_lsn + (((random() * (END_LSN - '0/0'::pg_lsn))::bigint / 8) * 8) FROM generate_series(1, 3000)") ],
);

foreach my $who ([ 'the primary', $node ], [ 'the standby', $sb ])
{
	my ($label, $n) = @$who;
	# the end of the WAL: what the primary has written (a standby cannot ask for it)
	my $end = $node->safe_psql('postgres', 'SELECT pg_current_wal_lsn()');
	foreach my $set (@sets)
	{
		my ($name, $arr) = @$set;
		(my $arr_sql = $arr) =~ s/END_LSN/'$end'::pg_lsn/g;
		(my $code = $sweep_do) =~ s/__ARRAY__/$arr_sql/;
		my ($rc, $out, $err) = run_sql($n, $code);
		is($rc, 0, "$label: $name: the server answered every call");
		my ($counts, $bad) = $err =~ /RESULT ([^;]*);(.*)/ ? ($1, $2) : ('no result', "no result: $err");
		$bad =~ s/^\s+|\s+$//g;
		is($bad, '', "$label: $name: nothing but false and KS006 came back ($counts)");
	}
	is($n->safe_psql('postgres', 'SELECT 1'), '1', "$label: and the server is still up");
}

done_testing();
