# contrib/kingres_timepart/t/007_convert_standby.pl
#
# A streaming standby sees a conversion as the primary does (design 10.4,
# 10.7): a reader on the standby, across the swap, never sees zero or two
# copies of the partition; after the standby has replayed, the table, the
# partition's access method and the rows of the columnar stripes are the
# primary's; conversion back and a conversion by the policy replay the same
# way; and after a promotion the standby, now the primary, converts a
# partition itself.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use IPC::Run ();
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(usleep);

my $primary = PostgreSQL::Test::Cluster->new('conv_primary');
$primary->init(allows_streaming => 1);
$primary->append_conf('postgresql.conf', q{
fsync = off
shared_preload_libraries = 'kingres_columnar'
kingres_timepart.trust_table_owners = on
});
$primary->start;

$primary->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE EXTENSION kingres_timepart;
ALTER DATABASE postgres SET search_path = public;
ALTER DATABASE postgres SET timezone = 'UTC';
});
$primary->safe_psql('postgres', q{
CREATE TABLE ev (id bigint, ts timestamptz NOT NULL, dev int, payload text) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('ev', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
INSERT INTO ev SELECT i, '2026-03-01 UTC'::timestamptz + (i % 3) * interval '1 day' + (i % 86400) * interval '1 second',
       i % 11, md5(i::text) || repeat('z', i % 30)
  FROM generate_series(1, 600000) i;
});

my $backup = 'conv_backup';
$primary->backup($backup);
my $standby = PostgreSQL::Test::Cluster->new('conv_standby');
$standby->init_from_backup($primary, $backup, has_streaming => 1);
$standby->start;
$primary->wait_for_catchup($standby);

sub digest_sql
{
	my ($rel) = @_;
	return qq{SELECT count(*) || '|' || coalesce(sum(hashtext(t::text)::numeric), 0) FROM $rel t};
}

sub both
{
	my ($sql) = @_;
	$primary->wait_for_catchup($standby);
	return ($primary->safe_psql('postgres', $sql), $standby->safe_psql('postgres', $sql));
}

sub methods_sql
{
	return q{SELECT string_agg(c.relname || ':' || a.amname, ',' ORDER BY c.relname)
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_am a ON a.oid = c.relam
		WHERE i.inhparent = 'ev'::regclass};
}

my ($p, $s) = both(digest_sql('ev'));
is($s, $p, 'start: the standby has the rows of the primary');
is($standby->safe_psql('postgres', q{SELECT pg_is_in_recovery()}), 't', 'start: it is a standby');
my $base = $p;

# ---- 1. A reader on the standby across the swap.
my $cnt = $primary->safe_psql('postgres', q{SELECT count(*) || '|' || sum(id) FROM ev});
my $in = qq{SET timezone = 'UTC';\nSELECT 'r:' || count(*) || '|' || sum(id) FROM ev t\n\\watch 0.003\n};
my ($out, $err) = ('', '');
my $reader = IPC::Run::start(
	[ 'psql', '-XAtq', '-d', $standby->connstr('postgres'), '-f', '-' ],
	'<', \$in, '>', \$out, '2>', \$err, IPC::Run::timeout(300));
$reader->pump_nb;
usleep(300_000);
$primary->safe_psql('postgres', q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'columnar', '{dev,id}')});
$primary->wait_for_catchup($standby);
usleep(300_000);
$standby->safe_psql('postgres', q{SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE '%FROM ev t%' AND pid <> pg_backend_pid()});
eval { $reader->finish };
my %seen = map { $_ => 1 } grep { /^r:/ } split /\n/, $out;
cmp_ok(scalar(keys %seen) + 0, '==', 1, '1: the reader on the standby saw one count and sum all the time')
	or diag join("\n", sort keys %seen);
is((keys %seen)[0] // '', 'r:' . $cnt, '1: and they are the right ones');
my $reads = () = $out =~ /^r:/mg;
cmp_ok($reads, '>', 6, "1: it read many times ($reads)");

# ---- 2. After the replay the standby is the primary.
($p, $s) = both(methods_sql());
is($p, 'ev_p20260301:columnar,ev_p20260302:heap,ev_p20260303:heap', '2: the primary: the first partition is columnar');
is($s, $p, '2: and the standby says the same');
($p, $s) = both(digest_sql('ev'));
is($p, $base, '2: the primary has every row');
is($s, $base, '2: and the standby has every row');
($p, $s) = both(q{SELECT i.rows || '|' || i.stripes FROM kingres.columnar_info('ev_p20260301') i});
is($s, $p, '2: the rows and stripes of the columnar partition are the same on the standby');
($p, $s) = both(digest_sql('ev_p20260301'));
is($s, $p, '2: and the rows of that partition');
($p, $s) = both(q{SELECT count(*) FROM pg_class WHERE relname LIKE '%\_\_kc'});
is("$p|$s", '0|0', '2: no stray table on either');

# ---- 3. Back to heap, and by the policy.
$primary->safe_psql('postgres', q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'heap')});
($p, $s) = both(methods_sql());
is($p, 'ev_p20260301:heap,ev_p20260302:heap,ev_p20260303:heap', '3: back to heap on the primary');
is($s, $p, '3: and on the standby');
($p, $s) = both(digest_sql('ev'));
is("$p|$s", "$base|$base", '3: the same rows on both');
$primary->safe_psql('postgres', q{SELECT kingres.timepart_set_policy('ev', '1 day', create_ahead => '1 day', columnar_after => '12 hours')});
$primary->safe_psql('postgres', q{CALL kingres.timepart_run('ev', '2026-03-03 12:00 UTC', '{convert}')});
($p, $s) = both(methods_sql());
is($p, 'ev_p20260301:columnar,ev_p20260302:columnar,ev_p20260303:heap', '3: the policy converted two partitions on the primary');
is($s, $p, '3: the standby says the same');
($p, $s) = both(digest_sql('ev'));
is("$p|$s", "$base|$base", '3: the same rows on both');
($p, $s) = both(q{SELECT string_agg(a.partition_name || ':' || a.state || ':' || (a.bytes_after > 0), ',' ORDER BY a.seq) FROM kingres.timepart_run_action a WHERE a.kind = 'convert'});
is($s, $p, '3: the history of the run is on the standby too');

# ---- 4. A promotion: the standby converts a partition itself.
$primary->stop;
$standby->promote;
$standby->poll_query_until('postgres', q{SELECT NOT pg_is_in_recovery()});
is($standby->safe_psql('postgres', digest_sql('ev')), $base, '4: the promoted standby has every row');
$standby->safe_psql('postgres', q{CALL kingres.convert_partition('ev', 'ev_p20260303', 'columnar')});
is($standby->safe_psql('postgres', methods_sql()), 'ev_p20260301:columnar,ev_p20260302:columnar,ev_p20260303:columnar', '4: it converted the third partition');
is($standby->safe_psql('postgres', digest_sql('ev')), $base, '4: with every row');
is($standby->safe_psql('postgres', q{SELECT kingres.columnar_check_clean()}), 't', '4: and the columnar files are clean');
$standby->safe_psql('postgres', q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'heap')});
is($standby->safe_psql('postgres', digest_sql('ev')), $base, '4: and back to heap, the same rows');

$standby->stop;
done_testing();
