# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# The comparison harness: the main correctness guard of phase 2a, extended for
# phase 2b (design/shard-phase2b.md, 16.2).  It builds a three-node cluster and,
# next to it, a single ordinary node; the test loads the same data into both,
# and the harness runs a list of queries on both and checks that the results
# are identical.  Later phases add queries to the list in the test file
# (t/013_dist_scan.pl), not to this module.
#
# Usage:
#   my $c = KingresShardCompare->new('prefix');
#   $c->on_cluster($sql);       # a statement on node 1 of the cluster (its DDL
#                               # and routed writes)
#   $c->on_every_node($sql);    # on each of the three nodes (local tables, functions)
#   $c->on_single($sql);        # on the single node
#   $c->on_both($sql);          # on the cluster's first node and the single node
#   $c->compare($name, $sql, ordered => 1);  # the query's output on each node
#                                            # of the cluster equals the single node's
#
# Output is compared line by line; unless ordered is true the lines are sorted
# first (the query has no ORDER BY the result could be compared by).  A NULL is
# printed as <NULL>, so a NULL never equals an empty string or the text NULL.
#
# What compare() takes besides name, sql and ordered:
#
#   nodes => [0, 1]      the nodes of the cluster to compare (default: all three)
#   profiles => 0        skip the four session profiles (@PROFILES)
#   expect_error => qr/  every node must fail with a message that matches
#   plans => [...]       run the query on every node under each plan profile and
#                        require its result to equal the single node's and the
#                        result of the 'scan' profile (the row-moving scan).  A
#                        profile is a name of %PLAN_PROFILES or a reference to a
#                        hash { name => ..., settings => [[guc, value], ...],
#                        sql => 'SET ...' }; a setting whose GUC does not exist
#                        on the node is skipped (the extension has no switch for
#                        it yet), 'sql' is run as it is
#   path => 'Kingres Fragment Scan'
#                        (or a list) run EXPLAIN under the 'pushed' profile on
#                        every node and require that the plan names the custom
#                        scan, so a test cannot pass because nothing was pushed
#   normalize => 1       round a float4 or float8 column to 9 significant
#                        digits (the columns are found with \gdesc); or a hash
#                        { floats => 'auto' | [columns], sort_arrays => [columns],
#                        sort_strings => { column => delimiter } } to sort the
#                        elements of an array_agg or string_agg result; columns
#                        are counted from 1.  On a difference the md5 of the
#                        whole normalized output is printed
#
# Other helpers: compare_dml() (rows affected by a statement), copy_in() and
# copy_in_both() (COPY FROM STDIN), compare_tables() (a whole table, by COPY
# (SELECT ...) TO), generator() and compare_generated() (a seeded generator of
# queries, KS_SEED).  The harness is tested by t/099_harness_selftest.pl:
# expect_failures() runs an assertion that must fail and shows that it does.

package KingresShardCompare;

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use Digest::MD5 qw(md5_hex);
use IPC::Run ();
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

# Session settings under which compare() repeats every query (review finding
# S2-03): a restriction shipped to another node must mean what it means in the
# session of the user, so the whole list is run on the cluster and on the single
# node under each of these profiles too.  Pass profiles => 0 to compare() to
# skip them.
our @PROFILES = (
	q{SET DateStyle = 'SQL, DMY'; SET extra_float_digits = 0},
	q{SET DateStyle = 'German, DMY'; SET IntervalStyle = 'sql_standard'; SET bytea_output = 'escape'},
	q{SET DateStyle = 'Postgres, MDY'; SET IntervalStyle = 'postgres_verbose'; SET extra_float_digits = -3; SET TimeZone = 'America/New_York'},
	q{SET TimeZone = 'Asia/Tokyo'; SET IntervalStyle = 'iso_8601'; SET lc_monetary = 'C'; SET lc_numeric = 'C'; SET lc_time = 'C'},
);

# Plan profiles (design/shard-phase2b.md, 16.2): settings of the cluster nodes
# under which a query is run to compare its plans' results with each other.
#   scan       the row-moving scan of phase 2a (ADR 0022 rule 3: every pushed
#              answer is compared with it)
#   pushed     the defaults; the profile of the path assertion
#   nopartial  no partial aggregation
# A test that needs another profile (for example one without pruning, which has
# no setting of its own in section 13 of the design) passes a hash instead of a
# name.  A setting is applied only on a node that has the GUC: until a step of
# the design adds a switch, the profile that uses it is the defaults.
our %PLAN_PROFILES = (
	scan      => { settings => [ [ 'kingres_shard.enable_pushdown', 'off' ] ] },
	pushed    => { settings => [] },
	nopartial => { settings => [ [ 'kingres_shard.enable_partial_aggregation', 'off' ] ] },
);

sub profile_script
{
	my ($sql) = @_;
	return join('', map { "DISCARD ALL;\n\\echo \@\@profile $_\n$PROFILES[$_];\n$sql\n;\n" } (0 .. $#PROFILES));
}

# The output of a profile script, split by profile (a reference to a hash).
sub split_profiles
{
	my ($out) = @_;
	my @parts = split /^\@\@profile (\d+)(?:\n|\z)/m, $out, -1;
	shift @parts;				# text before the first marker
	my %by;
	while (@parts) { my ($n, $text) = splice(@parts, 0, 2); $by{$n} = $text; }
	return \%by;
}

sub new
{
	my ($class, $prefix, %opt) = @_;
	my @nodes = make_cluster($prefix . 'c', 3, %opt);
	my $single = PostgreSQL::Test::Cluster->new($prefix . 's');
	$single->init;
	$single->start;
	return bless { nodes => \@nodes, single => $single, ncompared => 0,
				   failed => 0, failed_names => [], expecting => 0, guc_cache => {} }, $class;
}

sub nodes { return @{ $_[0]{nodes} } }
sub single { return $_[0]{single} }

sub on_cluster
{
	my ($self, $sql) = @_;
	return $self->{nodes}[0]->safe_psql('postgres', $sql);
}

sub on_every_node
{
	my ($self, $sql) = @_;
	$_->safe_psql('postgres', $sql) for @{ $self->{nodes} };
}

sub on_single
{
	my ($self, $sql) = @_;
	return $self->{single}->safe_psql('postgres', $sql);
}

sub on_both
{
	my ($self, $sql) = @_;
	$self->on_cluster($sql);
	$self->on_single($sql);
}

# A script of statements, one per line, run on the cluster and on the single node.
sub script_on_both
{
	my ($self, $script) = @_;
	$self->on_cluster($script);
	$self->on_single($script);
}

# ---------------------------------------------------------------------------
# Assertions.  Every check of the harness goes through these, so that the
# harness can count the failures it reports (expect_failures) while the output
# and the names of the tests stay what Test::More makes of them.
# ---------------------------------------------------------------------------
sub _is
{
	my ($self, $got, $want, $name) = @_;
	my $pass = defined $got ? (defined $want && $got eq $want) : !defined $want;
	return $self->_assert($pass, $name, sub { is($got, $want, $name) });
}

sub _isnt
{
	my ($self, $got, $want, $name) = @_;
	my $same = defined $got ? (defined $want && $got eq $want) : !defined $want;
	return $self->_assert(!$same, $name, sub { isnt($got, $want, $name) });
}

sub _like
{
	my ($self, $got, $re, $name) = @_;
	return $self->_assert((defined $got && $got =~ $re) ? 1 : 0, $name, sub { like($got, $re, $name) });
}

sub _ok
{
	my ($self, $ok, $name) = @_;
	return $self->_assert($ok ? 1 : 0, $name, sub { ok($ok, $name) });
}

# Run an assertion.  While expect_failures() runs a call, an assertion that
# fails is marked as expected (TODO), so it does not fail the test file; one
# that passes is a plain pass.
sub _assert
{
	my ($self, $pass, $name, $call) = @_;
	my $tb = Test::More->builder;
	local $Test::Builder::Level = $Test::Builder::Level + 2;
	my $expected = !$pass && $self->{expecting};
	$tb->todo_start('expected failure') if $expected;
	my $ok = $call->();
	$tb->todo_end if $expected;
	return $self->_record($ok, $name);
}

sub _record
{
	my ($self, $ok, $name) = @_;
	if (!$ok)
	{
		$self->{failed}++;
		push @{ $self->{failed_names} }, $name;
	}
	return $ok;
}

# The number of assertions of the harness that have failed so far.
sub failures { return $_[0]{failed} }

# Run $code, which must make the harness report at least one failure (and, with
# names => qr/.../, one whose name matches).  The failures it reports are
# marked as expected (TODO), so they do not fail the test file; the test that
# follows fails if the code did not report one.  This is how a test shows that a helper
# fails when it should.
sub expect_failures
{
	my ($self, $label, $code, %opt) = @_;
	my $before = $self->{failed};
	my $nnames = scalar @{ $self->{failed_names} };
	$self->{expecting}++;
	my $died = !eval { $code->(); 1 };
	my $err = $@;
	$self->{expecting}--;
	my $reported = $self->{failed} - $before;
	my @new = @{ $self->{failed_names} }[ $nnames .. $#{ $self->{failed_names} } ];
	my $ok = !$died && $reported >= ($opt{min} // 1);
	$ok &&= scalar(grep { $_ =~ $opt{names} } @new) > 0 if defined $opt{names};
	$ok &&= $reported == $opt{exactly} if defined $opt{exactly};
	my $name = "the harness reports a failure: $label";
	$self->_assert($ok, $name, sub { ok($ok, $name) }) or diag(
		($died ? "the code died: $err" : '') . "failures reported: $reported; names: " . join('; ', @new));
	return $reported;
}

# ---------------------------------------------------------------------------
# Normalization.
# ---------------------------------------------------------------------------

# Split the text form of an array ({a,b,"c d"}) into its elements.
sub _array_elements
{
	my ($text) = @_;
	return ($text) unless $text =~ /^\{(.*)\}$/s;
	my $inner = $1;
	my @el;
	while ($inner =~ /\G\s*("(?:[^"\\]|\\.)*"|[^,]*)(?:,|\z)/gc)
	{
		push @el, $1;
		last if pos($inner) >= length($inner);
	}
	return @el;
}

sub _round_float
{
	my ($v) = @_;
	return $v unless $v =~ /^-?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?$/;
	return $v if $v =~ /^-0(?:\.0*)?(?:[eE].*)?$/;			# a negative zero stays one
	return sprintf('%.9g', $v);
}

# The output of a query after the normalization of a spec (see _norm_spec).
# Without a spec the output is only sorted, unless it is ordered.
sub normalize
{
	my ($out, $ordered, $spec) = @_;
	my @lines = split /\n/, $out, -1;
	if ($spec && $spec->{ncols})
	{
		my %float = map { $_ => 1 } @{ $spec->{floats} || [] };
		my %arr = map { $_ => 1 } @{ $spec->{sort_arrays} || [] };
		my %str = %{ $spec->{sort_strings} || {} };
		for my $line (@lines)
		{
			my @f = split /\|/, $line, $spec->{ncols};
			next if @f != $spec->{ncols};						# a value with a newline: left alone
			for my $c (keys %float) { $f[ $c - 1 ] = _round_float($f[ $c - 1 ]); }
			for my $c (keys %arr)
			{
				$f[ $c - 1 ] = '{' . join(',', sort(_array_elements($f[ $c - 1 ]))) . '}'
				  if $f[ $c - 1 ] =~ /^\{/;
			}
			for my $c (keys %str)
			{
				my $d = $str{$c};
				$f[ $c - 1 ] = join($d, sort(split(/\Q$d\E/, $f[ $c - 1 ], -1)));
			}
			$line = join('|', @f);
		}
	}
	@lines = sort @lines unless $ordered;
	return join("\n", @lines);
}

# The normalization a call asks for, as a spec for normalize(); undef for none.
# The columns of the query are found on the single node with \gdesc.
sub _norm_spec
{
	my ($self, $sql, $want) = @_;
	return undef unless $want;
	my %spec = ref($want) eq 'HASH' ? %$want : (floats => 'auto');
	(my $stmt = $sql) =~ s/[;\s]+\z//;
	my ($rc, $out, $err) = run_sql($self->{single}, "$stmt \\gdesc");
	die "could not describe the columns of the query: $err" if $rc != 0;
	my @types = map { (split /\|/, $_, 2)[1] } grep { length } split /\n/, $out;
	$spec{ncols} = scalar @types;
	if (defined $spec{floats} && $spec{floats} eq 'auto')
	{
		$spec{floats} = [ grep { $types[ $_ - 1 ] =~ /^(?:real|double precision)$/ } (1 .. @types) ];
	}
	return \%spec;
}

# The line printed when two outputs differ: the md5 of each whole normalized
# output, so that two runs of a failing test can be compared at a glance.
sub _hash_line
{
	my ($self, $got, $want) = @_;
	my $line = "normalized output md5: cluster node " . md5_hex($got) . ", single node " . md5_hex($want);
	$self->{last_hash_line} = $line;
	return $line;
}

# ---------------------------------------------------------------------------
# Plan profiles and the path assertion.
# ---------------------------------------------------------------------------
sub _profile
{
	my ($profile) = @_;
	return { name => $profile, %{ $PLAN_PROFILES{$profile} } } if !ref($profile) && exists $PLAN_PROFILES{$profile};
	return { name => ($profile->{name} // 'custom'), %$profile } if ref($profile) eq 'HASH';
	die "unknown plan profile: " . (ref($profile) ? ref($profile) : $profile);
}

# Does node $i have the setting?  (A GUC the extension has not defined yet is not
# in pg_settings.)
sub _guc_exists
{
	my ($self, $i, $guc) = @_;
	my $cache = ($self->{guc_cache}{$i} //= {});
	return $cache->{$guc} //= ($self->{nodes}[$i]->safe_psql('postgres',
			"SELECT count(*) FROM pg_settings WHERE name = '$guc'") == 1 ? 1 : 0);
}

# The statements that put node $i under a plan profile.
sub profile_sql
{
	my ($self, $i, $profile) = @_;
	my $p = _profile($profile);
	my $sql = '';
	for my $s (@{ $p->{settings} || [] })
	{
		$sql .= "SET $s->[0] = '$s->[1]';\n" if $self->_guc_exists($i, $s->[0]);
	}
	$sql .= "$p->{sql};\n" if defined $p->{sql};
	return $sql;
}

# The profile settings that are not defined on node 1: what a test of a step
# needs to say it relies on (a typo in a profile would otherwise make it the
# defaults).
sub missing_profile_gucs
{
	my ($self, @profiles) = @_;
	my @missing;
	for my $profile (@profiles)
	{
		for my $s (@{ _profile($profile)->{settings} || [] })
		{
			push @missing, $s->[0] unless $self->_guc_exists(0, $s->[0]);
		}
	}
	return @missing;
}

# The psql script of a query: NULL is shown as <NULL>.
sub _script
{
	my ($sql) = @_;
	return "\\pset null '<NULL>'\n$sql";
}

# Run $sql on the single node and on every node of the cluster and compare.
sub compare
{
	my ($self, $name, $sql, %opt) = @_;
	my $ordered = $opt{ordered} ? 1 : 0;
	my @which = defined $opt{nodes} ? @{ $opt{nodes} } : (0 .. 2);
	my $spec = $self->_norm_spec($sql, $opt{normalize});
	my $norm = sub { return normalize($_[0], $ordered, $spec) };
	my ($rs, $os, $es) = run_sql($self->{single}, _script($sql));

	if ($opt{expect_error})
	{
		for my $i (@which)
		{
			my ($rd, $od, $ed) = run_sql($self->{nodes}[$i], _script($sql));
			$self->_like($ed, $opt{expect_error}, "$name: node " . ($i + 1) . " reports the error");
		}
		return;
	}

	$self->_is($rs, 0, "$name: the single node runs the query") or diag($es);
	my $want = $norm->($os);
	for my $i (@which)
	{
		my ($rd, $od, $ed) = run_sql($self->{nodes}[$i], _script($sql));
		$self->_is($rd, 0, "$name: node " . ($i + 1) . " runs the query") or diag($ed);
		my $got = $norm->($od);
		$self->_is($got, $want, "$name: the result on node " . ($i + 1) . " equals the single node's")
		  or diag($self->_hash_line($got, $want));
		$self->{ncompared}++;
	}

	if (!defined $opt{profiles} || $opt{profiles})
	{
		my $script = profile_script(_script($sql));
		my ($rsp, $osp, $esp) = run_sql($self->{single}, $script);
		$self->_is($rsp, 0, "$name: the single node runs the query under the session profiles") or diag($esp);
		my $wantp = split_profiles($osp);
		for my $i (@which)
		{
			my ($rdp, $odp, $edp) = run_sql($self->{nodes}[$i], $script);
			$self->_is($rdp, 0, "$name: node " . ($i + 1) . " runs the query under the session profiles") or diag($edp);
			my $got = split_profiles($odp);
			for my $p (0 .. $#PROFILES)
			{
				my $g = $norm->($got->{$p} // '<missing>');
				my $w = $norm->($wantp->{$p} // '');
				$self->_is($g, $w, "$name: node " . ($i + 1) . " under session profile $p equals the single node's")
				  or diag($self->_hash_line($g, $w));
			}
		}
	}

	$self->_compare_plans($name, $sql, \@which, $norm, $want, $opt{plans}) if $opt{plans};
	$self->_assert_path($name, $sql, \@which, $opt{path}) if defined $opt{path};
	return $os;
}

# The result under each plan profile equals the single node's and the 'scan'
# profile's, on every node.
sub _compare_plans
{
	my ($self, $name, $sql, $which, $norm, $want, $plans) = @_;
	for my $i (@$which)
	{
		my $n = $i + 1;
		my ($rc, $out, $err) = run_sql($self->{nodes}[$i], _script($self->profile_sql($i, 'scan') . $sql));
		$self->_is($rc, 0, "$name: node $n runs the query under the plan profile scan") or diag($err);
		my $scan = $norm->($out);
		for my $plan (@$plans)
		{
			my $p = _profile($plan);
			my $pn = $p->{name};
			my ($rp, $op, $ep) = run_sql($self->{nodes}[$i], _script($self->profile_sql($i, $p) . $sql));
			$self->_is($rp, 0, "$name: node $n runs the query under the plan profile $pn") or diag($ep);
			my $got = $norm->($op);
			$self->_is($got, $want, "$name: node $n under the plan profile $pn equals the single node's")
			  or diag($self->_hash_line($got, $want));
			$self->_is($got, $scan, "$name: node $n under the plan profile $pn equals the scan's")
			  or diag($self->_hash_line($got, $scan));
		}
	}
}

# EXPLAIN under the 'pushed' profile names the custom scan that is asserted.
sub _assert_path
{
	my ($self, $name, $sql, $which, $path) = @_;
	my @paths = ref($path) eq 'ARRAY' ? @$path : ($path);
	for my $i (@$which)
	{
		my ($rc, $plan, $err) = run_sql($self->{nodes}[$i],
			_script($self->profile_sql($i, 'pushed') . "EXPLAIN (COSTS OFF) $sql"));
		$self->_is($rc, 0, "$name: node " . ($i + 1) . " explains the query") or diag($err);
		for my $p (@paths)
		{
			$self->_ok(index($plan, $p) >= 0, "$name: node " . ($i + 1) . " plans the query with $p")
			  or diag("the plan is:\n$plan");
		}
	}
}

# Run a data-modifying statement on the single node and on one node of the
# cluster (the node $opt{node}, default 0) and compare the number of rows it
# reports.  The caller then compares the tables with compare().
sub compare_dml
{
	my ($self, $name, $dml, %opt) = @_;
	my $node = $self->{nodes}[ $opt{node} // 0 ];
	my $pre = $opt{settings} ? "$opt{settings}\n" : '';
	my $do = "${pre}DO \$\$ DECLARE ks_rows bigint; BEGIN $dml; GET DIAGNOSTICS ks_rows = ROW_COUNT; RAISE NOTICE 'ROWS=%', ks_rows; END \$\$";
	# A statement that only the cluster refuses is not run on the single node (it would change its data).
	my ($rs, $os, $es) = ($opt{expect_error} && !$opt{single_fails}) ? (0, "", "") : run_sql($self->{single}, $do);
	my ($rd, $od, $ed) = run_sql($node, $do);
	if ($opt{expect_error})
	{
		$self->_isnt($rd, 0, "$name: the cluster refuses the statement");
		$self->_isnt($rs, 0, "$name: the single node refuses it too") if $opt{single_fails};
		$self->_like($ed, $opt{expect_error}, "$name: the error");
		return;
	}
	$self->_is($rs, 0, "$name: the single node runs the statement") or diag($es);
	$self->_is($rd, 0, "$name: the cluster runs the statement") or diag($ed);
	my ($cs) = $es =~ /ROWS=(\d+)/;
	my ($cd) = $ed =~ /ROWS=(\d+)/;
	$self->_is($cd, $cs, "$name: the statement affected the same number of rows ($cs)");
	return $cs;
}

# ---------------------------------------------------------------------------
# Write helpers.
# ---------------------------------------------------------------------------

# COPY $table FROM STDIN on a node, the data coming from $data.  The statement
# is the top-level statement of its own session (a DO block cannot hold a COPY,
# and a statement in a DO block is planned as a nested one).  Options:
#   columns => ['a', 'b']       the column list
#   options => 'FORMAT csv, HEADER true'   the text inside WITH (...)
# Returns (exit status of psql, stdout, stderr); a refused or failing COPY is a
# status other than 0 and a message in stderr.
sub copy_in
{
	my ($node, $table, $data, %opt) = @_;
	my $cols = $opt{columns} ? ' (' . join(', ', @{ $opt{columns} }) . ')' : '';
	my $with = defined $opt{options} ? " WITH ($opt{options})" : '';
	my ($out, $err) = ('', '');
	my $h = IPC::Run::start(
		[ 'psql', '-XAtq', '-v', 'ON_ERROR_STOP=1', '-h', $node->host, '-p', $node->port,
		  '-d', 'postgres', '-c', "COPY $table$cols FROM STDIN$with" ],
		'<', \$data, '>', \$out, '2>', \$err);
	$h->finish;
	return ($h->result(0), $out, $err);
}

# copy_in() on a node of the cluster ($opt{node}, default 0) and on the single
# node; returns the results of both as ([cluster], [single]).
sub copy_in_both
{
	my ($self, $table, $data, %opt) = @_;
	my @c = copy_in($self->{nodes}[ delete($opt{node}) // 0 ], $table, $data, %opt);
	my @s = copy_in($self->{single}, $table, $data, %opt);
	return (\@c, \@s);
}

# The whole table on every node of the cluster equals the single node's, after
# a write.  The rows are read with COPY (SELECT ...) TO STDOUT (text format, so
# a NULL is \N and a string that looks like one is escaped).  Options: name,
# order_by (an ORDER BY list; the order is then compared), query (another
# SELECT than SELECT * FROM table), options (inside WITH (...) of the COPY),
# nodes (the nodes of the cluster to compare, default all three).
sub compare_tables
{
	my ($self, $table, %opt) = @_;
	my $name = $opt{name} // $table;
	my $ordered = defined $opt{order_by} ? 1 : 0;
	my $q = $opt{query} // ("SELECT * FROM $table" . ($ordered ? " ORDER BY $opt{order_by}" : ''));
	my $copy = "COPY ($q) TO STDOUT" . (defined $opt{options} ? " WITH ($opt{options})" : '');
	my ($rs, $os, $es) = run_sql($self->{single}, $copy);
	$self->_is($rs, 0, "$name: the single node copies the table out") or diag($es);
	my $want = normalize($os, $ordered);
	for my $i (defined $opt{nodes} ? @{ $opt{nodes} } : (0 .. 2))
	{
		my ($rd, $od, $ed) = run_sql($self->{nodes}[$i], $copy);
		$self->_is($rd, 0, "$name: node " . ($i + 1) . " copies the table out") or diag($ed);
		my $got = normalize($od, $ordered);
		$self->_is($got, $want, "$name: node " . ($i + 1) . " holds the same rows as the single node")
		  or diag($self->_hash_line($got, $want));
	}
	return $os;
}

# ---------------------------------------------------------------------------
# The generator of queries.
# ---------------------------------------------------------------------------

# A seeded generator.  Its templates are hashes
#   { name => 'filter', sql => 'SELECT {cols} FROM {table} WHERE {pred}',
#     ordered => 0, slots => { cols => [...], table => [...], pred => [...] } }
# Every {slot} of the text is replaced by one of the alternatives of the slot
# (a string, which may hold slots again, or a code reference that is called
# with the generator).  The same seed gives the same queries on every machine
# and every version of perl; KS_SEED in the environment sets it, and the seed
# is printed in the output of the test so that a failure can be reproduced.
sub generator
{
	my ($self, %opt) = @_;
	my $g = KingresShardCompare::Generator->new(%opt);
	note("KS_SEED=" . $g->seed);
	return $g;
}

# Run $count generated queries through compare(); %opt goes to compare().
sub compare_generated
{
	my ($self, $g, $count, %opt) = @_;
	my @q = $g->queries($count);
	my $i = 0;
	for my $q (@q)
	{
		$i++;
		my $text = $q->{sql};
		$text = substr($text, 0, 100) . '...' if length($text) > 100;
		$self->compare("generated $i ($q->{name}): $text", $q->{sql},
			ordered => $q->{ordered}, %opt);
	}
	return scalar @q;
}

package KingresShardCompare::Generator;

use strict;
use warnings FATAL => 'all';

sub new
{
	my ($class, %opt) = @_;
	my $seed = $opt{seed} // $ENV{KS_SEED};
	$seed = (time() ^ ($$ << 8)) % 2147483648 unless defined $seed && length $seed;
	die "the seed of the generator must be a number: $seed" unless $seed =~ /^\d+$/;
	return bless { seed => $seed + 0, state => ($seed + 0) % 2147483648,
				   templates => $opt{templates} || [] }, $class;
}

sub seed { return $_[0]{seed} }

# A number from 0 to n - 1.  A linear congruential generator of our own (the
# state fits 31 bits, the product 62), so the sequence does not depend on the
# random number generator of the perl that runs the test.
sub next_int
{
	my ($self, $n) = @_;
	my $v = 0;
	for (1 .. 2)
	{
		$self->{state} = ($self->{state} * 1103515245 + 12345) % 2147483648;
		$v = $v * 32768 + int($self->{state} / 65536);
	}
	return $v % $n;
}

sub pick
{
	my ($self, $list) = @_;
	return $list->[ $self->next_int(scalar @$list) ];
}

# One query: { name, sql, ordered }.
sub query
{
	my ($self) = @_;
	die "the generator has no templates" unless @{ $self->{templates} };
	my $t = $self->pick($self->{templates});
	my $sql = $t->{sql};
	for (1 .. 8)
	{
		last unless $sql =~ /\{(\w+)\}/;
		$sql =~ s{\{(\w+)\}}{
			my $alts = $t->{slots}{$1} or die "template $t->{name} has no slot $1";
			my $v = $self->pick($alts);
			ref($v) eq 'CODE' ? $v->($self) : $v;
		}ge;
	}
	die "template $t->{name} leaves a slot open: $sql" if $sql =~ /\{\w+\}/;
	return { name => $t->{name}, sql => $sql, ordered => $t->{ordered} ? 1 : 0 };
}

sub queries
{
	my ($self, $n) = @_;
	return map { $self->query } (1 .. $n);
}

package KingresShardCompare;

1;
