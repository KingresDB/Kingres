# contrib/kingres_columnar/t/045_reserve_subxact.pl
#
# C6a, decision 3: while a buffer holds a reservation of row numbers, a committing
# subtransaction writes that buffer as a stripe of its own and does not merge it into
# its parent's; a rolled back subtransaction leaves no row and its numbers are never
# used by another row.  Model-based: random programs of inserts, savepoints
# (released, or rolled back to, with inserts after the rollback), PL/pgSQL blocks with
# an EXCEPTION clause (ending well, or raising and being caught), nested three levels
# deep, ended by COMMIT, ROLLBACK or PREPARE TRANSACTION with COMMIT PREPARED.  A model
# built from the program says which rows survive and how many stripes the
# transaction leaves (one for every subtransaction that survives and wrote a row, and
# one for the top level if it wrote one); the table must hold exactly those rows, be
# equal to its heap twin, have no two rows with one TID, every row with the TID its
# insert returned, stripes whose number ranges do not overlap, no reserved row left,
# and columnar_check nothing to report.  The seed is printed; KC_SEED reruns it.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1 rsql fp reserve_on);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = new_node('rsub');
my $seed = $ENV{KC_SEED} // (time() ^ ($$ << 8)) & 0x7fffffff;
srand($seed);
note("seed $seed");

my $next_id = 1;
my $next_sp = 1;

# --- the programs ----------------------------------------------------------
# a node is one of
#   {t => 'ins', ids => [...]}
#   {t => 'sp',  out => 'release'|'rollback', ch => [...], post => [ids]}   SQL savepoint
#   {t => 'do',  ch => [...]}                  a DO block, in the current subtransaction
#   {t => 'ex',  out => 'ok'|'raise', ch => [...]}   a block with an EXCEPTION clause
sub gen_ins
{
	my $n = rand() < 0.1 ? 40 + int(rand(100)) : 1 + int(rand(3));
	return { t => 'ins', ids => [ map { $next_id++ } 1 .. $n ] };
}

sub gen_children
{
	my ($depth, $ctx) = @_;
	my @c;
	my $n = 1 + int(rand(4));
	for (1 .. $n)
	{
		my $p = rand();
		if ($p < 0.45 || $depth >= 3) { push @c, gen_ins(); }
		elsif ($p < 0.80)
		{
			if ($ctx eq 'sql')
			{
				my $out = rand() < 0.5 ? 'release' : 'rollback';
				push @c, { t => 'sp', out => $out, ch => gen_children($depth + 1, 'sql'),
					post => ($out eq 'rollback' && rand() < 0.6) ? [ map { $next_id++ } 1 .. (1 + int(rand(3))) ] : [] };
			}
			else
			{
				push @c, { t => 'ex', out => (rand() < 0.5 ? 'ok' : 'raise'), ch => gen_children($depth + 1, 'pl') };
			}
		}
		elsif ($ctx eq 'sql')
		{
			push @c, { t => 'do', ch => gen_children($depth, 'pl') };
		}
		else { push @c, gen_ins(); }
	}
	return \@c;
}

# --- the model ---------------------------------------------------------------
# An instance has direct rows, rows of the surviving instances inside it, and the
# stripes they leave.  Returns {direct => [...], ids => [...], stripes => n} for
# the children of one instance (a DO block is part of the instance it is in).
sub model_children
{
	my ($nodes, $acc) = @_;
	for my $n (@$nodes)
	{
		if ($n->{t} eq 'ins') { push @{ $acc->{direct} }, @{ $n->{ids} }; }
		elsif ($n->{t} eq 'do') { model_children($n->{ch}, $acc); }
		elsif ($n->{t} eq 'sp' || $n->{t} eq 'ex')
		{
			my $kept = ($n->{t} eq 'sp') ? ($n->{out} eq 'release') : ($n->{out} eq 'ok');
			if ($kept)
			{
				my $sub = { direct => [], ids => [], stripes => 0 };
				model_children($n->{ch}, $sub);
				push @{ $acc->{ids} }, @{ $sub->{direct} }, @{ $sub->{ids} };
				$acc->{stripes} += $sub->{stripes} + (@{ $sub->{direct} } ? 1 : 0);
			}
			elsif ($n->{t} eq 'sp' && @{ $n->{post} })
			{
				# after ROLLBACK TO the savepoint goes on as a new subtransaction
				push @{ $acc->{ids} }, @{ $n->{post} };
				$acc->{stripes} += 1;
			}
		}
	}
}

# --- the rendering ------------------------------------------------------------
my ($T, $TH, $TR);					# the table of the case, its twin, the TIDs

sub values_of { return join(',', map { "($_, 't$_')" } @{ $_[0] }); }

sub ins_sql
{
	my ($ids) = @_;
	my $v = values_of($ids);
	return "WITH i AS (INSERT INTO $T VALUES $v RETURNING a, ctid), j AS (INSERT INTO $TH VALUES $v) INSERT INTO $TR SELECT a, ctid FROM i;";
}

sub render_pl
{
	my ($nodes) = @_;
	my @s;
	for my $n (@$nodes)
	{
		if ($n->{t} eq 'ins') { push @s, ins_sql($n->{ids}); }
		elsif ($n->{t} eq 'ex')
		{
			push @s, 'BEGIN ' . render_pl($n->{ch}) .
			  ($n->{out} eq 'raise' ? " RAISE EXCEPTION 'boom';" : '') .
			  ' EXCEPTION WHEN OTHERS THEN NULL; END;';
		}
	}
	return join(' ', @s);
}

sub render_sql
{
	my ($nodes) = @_;
	my @s;
	for my $n (@$nodes)
	{
		if ($n->{t} eq 'ins') { push @s, ins_sql($n->{ids}); }
		elsif ($n->{t} eq 'do') { push @s, 'DO $$ BEGIN ' . render_pl($n->{ch}) . ' END $$;'; }
		elsif ($n->{t} eq 'sp')
		{
			my $name = 'sp' . $next_sp++;
			push @s, "SAVEPOINT $name;";
			push @s, render_sql($n->{ch});
			if ($n->{out} eq 'rollback')
			{
				push @s, "ROLLBACK TO $name;";
				push @s, ins_sql($_) for map { [$_] } @{ $n->{post} };
			}
			push @s, "RELEASE $name;";
		}
	}
	return join("\n", @s);
}

# --- the cases -----------------------------------------------------------------
my $CASES = $ENV{KC_CASES} // 150;
my @bad;
my %stat = (cases => 0, prepared => 0, committed => 0, subs => 0, deep => 0, stripes => 0);

for my $case (1 .. $CASES)
{
	$T = "m$case"; $TH = "mh$case"; $TR = "mr$case";
	$next_sp = 1;
	q1($node, "CREATE TABLE $T (a int, b text) USING columnar; CREATE TABLE $TH (a int, b text); CREATE TABLE $TR (a int, t tid);");

	my $prog = gen_children(0, 'sql');
	my $end = rand();
	my $mode = $end < 0.65 ? 'commit' : $end < 0.82 ? 'rollback' : 'prepare';
	my $acc = { direct => [], ids => [], stripes => 0 };
	model_children($prog, $acc);
	my @want_ids = sort { $a <=> $b } (@{ $acc->{direct} }, @{ $acc->{ids} });
	my $want_stripes = $acc->{stripes} + (@{ $acc->{direct} } ? 1 : 0);
	if ($mode eq 'rollback') { @want_ids = (); $want_stripes = 0; }
	$stat{subs} += () = render_sql($prog) =~ /SAVEPOINT|BEGIN (?!ATOMIC)/g;

	my $script = "BEGIN;\n" . render_sql($prog) . "\n" .
	  ($mode eq 'commit' ? 'COMMIT;' : $mode eq 'rollback' ? 'ROLLBACK;' : "PREPARE TRANSACTION 'c$case';");
	my ($rc, $out, $err) = rsql($node, $script);
	if ($rc != 0)
	{
		push @bad, "case $case: the program failed: $err";
		diag("seed $seed\n$script");
		last;
	}
	q1($node, "COMMIT PREPARED 'c$case'") if $mode eq 'prepare';
	$stat{cases}++;
	$stat{prepared}++ if $mode eq 'prepare';
	$stat{committed}++ if $mode ne 'rollback';

	my $got_ids = q1($node, "SELECT coalesce(string_agg(a::text, ',' ORDER BY a), '') FROM $T");
	my ($same, $n, $dist, $match, $res, $stripes, $overlap, $sumrows) = split /\|/, q1($node, qq{
		SELECT fp('$T') = fp('$TH'),
		       (SELECT count(*) FROM $T),
		       (SELECT count(DISTINCT ctid::text) FROM $T),
		       (SELECT count(*) FROM $T x JOIN $TR r USING (a) WHERE x.ctid::text = r.t::text),
		       (SELECT count(*) FROM kingres.columnar_stripe WHERE reserved),
		       (SELECT count(*) FROM kingres.columnar_stripes('$T')),
		       (SELECT count(*) FROM (SELECT first_row, row_count, lead(first_row) OVER (ORDER BY first_row) nf
		                                FROM kingres.columnar_stripes('$T')) s WHERE first_row + row_count > nf),
		       (SELECT coalesce(sum(row_count), 0) FROM kingres.columnar_stripes('$T'))});
	my $check = q1($node, "SELECT count(*) FROM kingres.columnar_check()");
	my @p;
	push @p, "rows [$got_ids] want [" . join(',', @want_ids) . "]" if $got_ids ne join(',', @want_ids);
	push @p, "twin differs" if $same ne 't';
	push @p, "TIDs shared ($n rows, $dist TIDs)" if $n != $dist;
	push @p, "TIDs differ from the ones returned ($match of $n)" if $n != $match;
	push @p, "$res reserved rows left" if $res != 0;
	push @p, "stripes $stripes want $want_stripes" if $stripes != $want_stripes;
	push @p, "$overlap stripes overlap in their row numbers" if $overlap != 0;
	push @p, "stripes hold $sumrows rows of $n" if $sumrows != $n;
	push @p, "columnar_check reports $check rows" if $check != 0;
	$stat{stripes} += $stripes;
	if (@p)
	{
		push @bad, "case $case ($mode): " . join('; ', @p);
		diag("seed $seed\n$script");
		last;
	}
	q1($node, "DROP TABLE $T, $TH, $TR");
}
is(scalar(@bad), 0, "$stat{cases} random programs of savepoints and exception blocks ($stat{prepared} prepared, $stat{committed} committed): rows, stripes, TIDs and the catalog are those of the model") or diag(join("\n", @bad));
cmp_ok($stat{subs}, '>', 50, "the programs had $stat{subs} subtransactions");
cmp_ok($stat{stripes}, '>', 50, "and left $stat{stripes} stripes");

$node->stop;
done_testing();
