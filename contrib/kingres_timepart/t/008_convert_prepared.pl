# contrib/kingres_timepart/t/008_convert_prepared.pl
#
# A prepared transaction (PREPARE TRANSACTION) that holds a row of the
# partition, or only a lock on its parent, during a conversion (design 10.4,
# 10.7).  The prepared transaction keeps its locks until it is ended, so the
# conversion cannot lock the partition (a row of the partition: lock_timeout)
# or the parent (only a read of it: swap_lock_timeout); either way nothing
# changes, the row is neither lost nor doubled, and the conversion works after
# COMMIT PREPARED or ROLLBACK PREPARED, also across a crash and a restart
# with the transaction still prepared, and through the policy.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('conv_prepared');
$node->init;
$node->append_conf('postgresql.conf', q{
fsync = off
shared_preload_libraries = 'kingres_columnar'
max_prepared_transactions = 5
kingres_timepart.trust_table_owners = on
kingres_timepart.create_lock_timeout_ms = 300
});
$node->start;

$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE EXTENSION kingres_timepart;
ALTER DATABASE postgres SET search_path = public;
ALTER DATABASE postgres SET timezone = 'UTC';
});
$node->safe_psql('postgres', q{
CREATE TABLE ev (id bigint, ts timestamptz NOT NULL, dev int, payload text) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('ev', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
INSERT INTO ev SELECT i, '2026-03-01 UTC'::timestamptz + (i % 3) * interval '1 day' + (i % 86400) * interval '1 second',
       i % 11, md5(i::text)
  FROM generate_series(1, 30000) i;
});

sub q1
{
	my ($sql) = @_;
	return $node->safe_psql('postgres', $sql);
}

sub digest
{
	return q1(q{SELECT count(*) || '|' || coalesce(sum(hashtext(t::text)::numeric), 0) FROM ev t});
}

sub am
{
	return q1(q{SELECT a.amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.relname = 'ev_p20260301'});
}

sub strays
{
	return q1(q{SELECT count(*) FROM pg_class WHERE relname LIKE '%\_\_kc'});
}

my $convert = q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'columnar')};

# Try a conversion that must fail: the error text, and nothing changed.
sub fails
{
	my ($sql, $like, $what) = @_;
	my ($out, $err);
	my $ret = $node->psql('postgres', $sql, stdout => \$out, stderr => \$err);
	isnt($ret, 0, "$what: the conversion did not go through");
	like($err, $like, "$what: with the expected error");
	is(am(), 'heap', "$what: the partition is still heap");
	is(strays(), '0', "$what: no stray table");
	return $err;
}

my $base = digest();
my $c0 = (split /\|/, $base)[0];

# ---- 1. A prepared transaction that holds a row of the partition (by name).
my $a = $node->background_psql('postgres');
$a->query_safe(q{BEGIN; INSERT INTO ev_p20260301 VALUES (9000001, '2026-03-01 10:00 UTC', 1, 'prepared a'); PREPARE TRANSACTION 'a';});
$a->quit;
is(q1(q{SELECT count(*) FROM pg_prepared_xacts}), '1', '1: one transaction is prepared');
fails($convert, qr/lock timeout/, '1');
is(digest(), $base, '1: the row of the prepared transaction is not there yet, nothing else changed');
q1(q{COMMIT PREPARED 'a'});
is((split /\|/, digest())[0], $c0 + 1, '1: after COMMIT PREPARED the row is there');
my $with_a = digest();
q1($convert);
is(am(), 'columnar', '1: the conversion works then');
is(digest(), $with_a, '1: and the row came with it, once');
is(q1(q{SELECT count(*) FROM ev WHERE id = 9000001}), '1', '1: exactly once');
q1(q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'heap')});

# ---- 2. The same through the parent; rolled back.
$a = $node->background_psql('postgres');
$a->query_safe(q{BEGIN; INSERT INTO ev VALUES (9000002, '2026-03-01 11:00 UTC', 2, 'prepared b'); PREPARE TRANSACTION 'b';});
$a->quit;
fails($convert, qr/lock timeout/, '2');
q1(q{ROLLBACK PREPARED 'b'});
is(digest(), $with_a, '2: after ROLLBACK PREPARED the row is not there');
q1($convert);
is(am(), 'columnar', '2: the conversion works then');
is(digest(), $with_a, '2: with the rows that are there');
q1(q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'heap')});

# ---- 3. A prepared transaction that only read the parent: the copy goes through, the swap cannot lock the parent.
$a = $node->background_psql('postgres');
$a->query_safe(q{BEGIN; SELECT count(*) FROM ev; PREPARE TRANSACTION 'c';});
$a->quit;
fails($convert, qr/swap_lock_timeout/, '3');
is(digest(), $with_a, '3: the same rows');
q1(q{COMMIT PREPARED 'c'});
q1($convert);
is(am(), 'columnar', '3: after it ends, the conversion works');
is(digest(), $with_a, '3: with the same rows');
q1(q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'heap')});

# ---- 4. A crash and a restart with the transaction still prepared.
$a = $node->background_psql('postgres');
$a->query_safe(q{BEGIN; INSERT INTO ev_p20260301 VALUES (9000004, '2026-03-01 12:00 UTC', 4, 'prepared d'); PREPARE TRANSACTION 'd';});
$a->quit;
fails($convert, qr/lock timeout/, '4, before the crash');
$node->stop('immediate');
$node->start;
is(q1(q{SELECT count(*) FROM pg_prepared_xacts}), '1', '4: the transaction is still prepared after the restart');
fails($convert, qr/lock timeout/, '4, after the restart');
q1(q{COMMIT PREPARED 'd'});
my $with_d = digest();
is((split /\|/, $with_d)[0], $c0 + 2, '4: after COMMIT PREPARED the row is there');
q1($convert);
is(am(), 'columnar', '4: the conversion works then');
is(digest(), $with_d, '4: with the row, once');
q1(q{CALL kingres.convert_partition('ev', 'ev_p20260301', 'heap')});

# ---- 5. Through the policy: the action fails with lock_timeout, the next run converts.
q1(q{SELECT kingres.timepart_set_policy('ev', '1 day', create_ahead => '1 day', columnar_after => '12 hours')});
my $asof = q{'2026-03-02 12:00 UTC'};
$a = $node->background_psql('postgres');
$a->query_safe(q{BEGIN; INSERT INTO ev_p20260301 VALUES (9000005, '2026-03-01 13:00 UTC', 5, 'prepared e'); PREPARE TRANSACTION 'e';});
$a->quit;
q1(qq{CALL kingres.timepart_run('ev', $asof, '{convert}')});
is(q1(q{SELECT a.state || ':' || left(a.detail, 13) FROM kingres.timepart_run_action a
	WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run) AND a.kind = 'convert'}),
	'failed:lock_timeout:', '5: the action is failed, lock_timeout');
is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}), 'partial', '5: the run is partial');
is(am(), 'heap', '5: the partition is heap');
q1(q{COMMIT PREPARED 'e'});
my $with_e = digest();
q1(qq{CALL kingres.timepart_run('ev', $asof, '{convert}')});
is(q1(q{SELECT state FROM kingres.timepart_run ORDER BY run_id DESC LIMIT 1}), 'done', '5: the next run is done');
is(am(), 'columnar', '5: the partition is columnar');
is(digest(), $with_e, '5: with every row, once');
is(q1(q{SELECT kingres.columnar_check_clean()}), 't', 'the columnar files and the stripe catalog are clean');

$node->stop;
done_testing();
