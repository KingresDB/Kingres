# contrib/kingres_columnar/t/KcReserve.pm
#
# What the tests of the reservation of row numbers (C6a, t/044 to t/050) share:
# a node with the extension, the helper functions they install, and the checks
# every one of them makes after what it did to a table: the rows are those of
# the heap twin, no two rows share a TID, every row has the TID its insert
# returned, no reserved stripe row is left, and columnar_check has nothing to
# report.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

package KcReserve;

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More ();

use Exporter 'import';
our @EXPORT_OK = qw(new_node install_helpers rsql q1 fp same_as_twin tids_ok
  check_empty stripes_str wait_waiting reserve_on);

# the statement every session of a test that reserves starts with
sub reserve_on { return "SET kingres_columnar.reserve_row_numbers = on;\n"; }

# A cluster with the library preloaded (and the extra modules in $opts{preload}),
# the extension installed, and the helper functions of install_helpers.
sub new_node
{
	my ($name, %opts) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);

	$node->init(%{ $opts{init} || {} });
	$node->append_conf('postgresql.conf',
		"shared_preload_libraries = '" . ($opts{preload} // 'kingres_columnar') . "'\n"
		  . "max_prepared_transactions = 10\nautovacuum = off\n"
		  . ($opts{conf} // ''));
	$node->start;
	install_helpers($node);
	return $node;
}

# SQL functions the tests use, in the database "postgres" of the node:
#   fp(tbl)               count, sum and a hash of the rows of a table, one string
#   stripes_str(tbl)      the stripes of a table as "(first row,rows)" in order
#   meta_next_row(tbl)    the next row number the metapage will hand out
#   dead_reserved(tbl)    unused
sub install_helpers
{
	my ($node) = @_;

	$node->safe_psql('postgres', q{
		CREATE EXTENSION kingres_columnar;
		CREATE EXTENSION pageinspect;
		CREATE FUNCTION fp(tbl regclass) RETURNS text LANGUAGE plpgsql AS $$
		DECLARE r text;
		BEGIN
		  EXECUTE format('SELECT count(*) || '','' || coalesce(sum(a), 0) || '','' || coalesce(sum(hashtext(b)), 0) FROM %s', tbl) INTO r;
		  RETURN r;
		END $$;
		CREATE FUNCTION stripes_str(tbl regclass) RETURNS text LANGUAGE sql AS $$
		  SELECT coalesce(string_agg('(' || first_row || ',' || row_count || ')', ' ' ORDER BY stripe_id), '-')
		    FROM kingres.columnar_stripes(tbl) $$;
		CREATE FUNCTION meta_next_row(tbl regclass) RETURNS bigint LANGUAGE sql AS $$
		  SELECT sum(get_byte(p, 48 + i)::bigint << (8 * i))
		    FROM (SELECT get_raw_page(tbl::text, 0) p) s, generate_series(0, 7) i $$;
	});
}

# a script with the setting on; returns (status, stdout, stderr)
sub rsql
{
	my ($node, $sql, %opts) = @_;
	return $node->psql('postgres', reserve_on() . $sql, on_error_stop => 1, %opts);
}

sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

sub fp { my ($node, $tbl) = @_; return q1($node, "SELECT fp('$tbl')"); }

# the rows of a columnar table are those of its heap twin
sub same_as_twin
{
	my ($node, $col, $heap, $what) = @_;
	Test::More::is(fp($node, $col), fp($node, $heap), "$what: the rows are those of the heap twin");
}

# No two rows share a TID, and every row has the TID that its insert returned
# (the table $ret holds (a, t) of the rows that survive: it is a heap table written
# by the same transaction).  Returns the number of rows.
sub tids_ok
{
	my ($node, $tbl, $ret, $what) = @_;
	my ($n, $dist, $match) = split /\|/, q1($node, qq{
		SELECT count(*), count(DISTINCT ctid::text),
		       (SELECT count(*) FROM $tbl x JOIN $ret r USING (a) WHERE x.ctid::text = r.t::text)
		  FROM $tbl});
	Test::More::is($dist, $n, "$what: no two rows share a TID ($n rows)");
	Test::More::is($match, $n, "$what: every row has the TID its insert returned");
	return $n;
}

# columnar_check has nothing to report, and no reserved row is left for anybody to see
sub check_empty
{
	my ($node, $what) = @_;
	Test::More::is(q1($node, "SELECT coalesce(string_agg(check_name || ' ' || coalesce(detail, ''), '; '), '') FROM kingres.columnar_check()"),
		'', "$what: columnar_check has nothing to report");
	Test::More::is(q1($node, "SELECT count(*) FROM kingres.columnar_stripe WHERE reserved"),
		0, "$what: no reserved stripe row is left");
}

sub stripes_str { my ($node, $tbl) = @_; return q1($node, "SELECT stripes_str('$tbl')"); }

# wait until $n sessions other than the caller wait in a gate (wait event PgSleep)
sub wait_waiting
{
	my ($node, $n) = @_;
	for (my $i = 0; $i < 600; $i++)
	{
		return 1 if q1($node, "SELECT count(*) FROM pg_stat_activity WHERE wait_event = 'PgSleep' AND pid <> pg_backend_pid()") >= $n;
		select(undef, undef, undef, 0.05);
	}
	return 0;
}

1;
