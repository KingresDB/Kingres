# contrib/kingres_columnar/t/065_amcheck.pl
#
# C13-04: amcheck can verify the B-tree indexes of a columnar table, including the form that
# compares the index with the table (heapallindexed): it reads the table through the access
# method's index build with a concurrent index description, which is refused for CREATE INDEX
# CONCURRENTLY and REINDEX CONCURRENTLY (they would build an index while writers run) and
# accepted for a read of an index that is valid and ready.
#  1. bt_index_check and bt_index_parent_check, with and without heapallindexed, pass on
#     unique, composite, partial and expression indexes, after inserts, rollbacks, a
#     compaction and a VACUUM.
#  2. amcheck finds an index that lacks the entries of rows (the index was not ready while
#     they went in): the check with heapallindexed fails and names the missing entry.
#  3. CREATE INDEX CONCURRENTLY and REINDEX CONCURRENTLY are still refused, with their own message.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = new_node('amck', conf => "allow_system_table_mods = on\n");
my ($rc, $out, $err) = $node->psql('postgres', "CREATE EXTENSION amcheck");
if ($rc != 0)
{
	$node->stop;
	plan skip_all => 'the amcheck module is not installed in this installation';
}

q1($node, q{
	CREATE TABLE m (k int, g int, v text) USING columnar;
	SELECT kingres.columnar_set_options('m', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE UNIQUE INDEX m_k ON m (k);
	CREATE INDEX m_gk ON m (g DESC, k);
	CREATE INDEX m_p ON m (k) WHERE g = 3;
	CREATE INDEX m_e ON m ((k % 100), v);
});
q1($node, "INSERT INTO m SELECT $_ * 1000 + g, g % 7, 'v' || g FROM generate_series(1, 1000) g") for 0 .. 5;
q1($node, "DO \$\$ BEGIN FOR i IN 1..50 LOOP BEGIN INSERT INTO m VALUES (900000 + i, 1, 'x'); RAISE EXCEPTION 'u'; EXCEPTION WHEN raise_exception THEN NULL; END; END LOOP; END \$\$");
q1($node, "SELECT kingres.columnar_compact('m', 100000)");
q1($node, "VACUUM m");

sub check
{
	my ($what, $sql) = @_;
	my ($rc, $out, $err) = $node->psql('postgres', $sql);
	is($rc, 0, $what) or diag($err);
}

for my $ix ('m_k', 'm_gk', 'm_p', 'm_e')
{
	check("bt_index_check($ix)", "SELECT bt_index_check('$ix'::regclass)");
	check("bt_index_check($ix, heapallindexed)", "SELECT bt_index_check('$ix'::regclass, true)");
	check("bt_index_parent_check($ix, heapallindexed)", "SELECT bt_index_parent_check('$ix'::regclass, true)");
}
# a transaction that has unwritten rows of its own: amcheck sees them (they are flushed first)
check('heapallindexed in a transaction that wrote rows', "BEGIN; INSERT INTO m SELECT 2000000 + g, 2, 'own' FROM generate_series(1, 300) g; SELECT bt_index_check('m_k'::regclass, true); COMMIT");

# 2. damage: rows that went in while the index was not ready have no entry
q1($node, "UPDATE pg_index SET indisready = false, indisvalid = false WHERE indexrelid = 'm_k'::regclass");
$node->safe_psql('postgres', "SELECT 1");
q1($node, "INSERT INTO m SELECT 3000000 + g, 1, 'lost' FROM generate_series(1, 10) g");
q1($node, "UPDATE pg_index SET indisready = true, indisvalid = true WHERE indexrelid = 'm_k'::regclass");
($rc, $out, $err) = $node->psql('postgres', "SELECT bt_index_check('m_k'::regclass)");
is($rc, 0, '2: without heapallindexed the structure of the damaged index is fine');
($rc, $out, $err) = $node->psql('postgres', "SELECT bt_index_check('m_k'::regclass, true)");
isnt($rc, 0, '2: heapallindexed finds that rows have no entry');
like($err, qr/lacks matching index tuple|heap tuple.*index/i, '2: and says so');
q1($node, "REINDEX INDEX m_k");
check('2: after REINDEX the check passes again', "SELECT bt_index_check('m_k'::regclass, true)");

# 3. the concurrent builds stay refused, with their own words
($rc, $out, $err) = $node->psql('postgres', "CREATE INDEX CONCURRENTLY m_c ON m (v)");
isnt($rc, 0, '3: CREATE INDEX CONCURRENTLY is refused');
like($err, qr/CREATE INDEX CONCURRENTLY and REINDEX CONCURRENTLY are not supported/, '3: with its message');
is(q1($node, "SELECT count(*) FROM pg_indexes WHERE indexname = 'm_c'"), 0, '3: and leaves no index');
($rc, $out, $err) = $node->psql('postgres', "REINDEX INDEX CONCURRENTLY m_gk");
isnt($rc, 0, '3: REINDEX CONCURRENTLY is refused');
check('3: the index it named is untouched', "SELECT bt_index_check('m_gk'::regclass, true)");

$node->stop;
done_testing();
