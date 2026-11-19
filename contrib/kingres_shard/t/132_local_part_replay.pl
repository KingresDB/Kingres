# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/132_local_part_replay.pl
#
# Review of steps 3 and 4 (P2C-05): the mark of the local part (fanout.c) is consumed by the first planning of the
# statement that SPI runs, so a statement that SQL code inside the local part runs is planned and routed as any
# statement.  The CHECK function of the table reads the exact text of the local part from PG_EXCEPTION_CONTEXT
# ("SQL statement ...") and executes it itself, during the execution of the local part:
#   (the fan-out UPDATE cannot replay itself, see below)
#   B. the local batch of a router INSERT: the replay (with parameters of its own) is routed, its rows are stored.
# The one-line mutant that leaves the mark armed after the first planning makes the replay a local part (A: the
# replay updates this node's rows only; B: rows of other groups fail with "no partition").

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('rp', 3);
my ($n1, $n2, $n3) = @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.mine2 (id int PRIMARY KEY, v int);
	SELECT kingres.create_distributed_table('public.mine2', 'id', bucket_count => 12);
	INSERT INTO public.mine2 SELECT g, g FROM generate_series(1, 40) g;
});
$_->safe_psql('postgres', q{
	CREATE TABLE public.rp_log (kind text, n bigint);
	CREATE FUNCTION public.rp_replay(x int) RETURNS boolean LANGUAGE plpgsql AS $f$
	DECLARE ctx text; q text; n bigint;
	BEGIN
	  IF coalesce(current_setting('rp.armed', true), '') <> 'on'
	     OR coalesce(current_setting('kingres_shard.forwarded', true), '') = 'on' THEN
	    RETURN true;
	  END IF;
	  PERFORM set_config('rp.armed', 'off', false);
	  BEGIN PERFORM 1/0; EXCEPTION WHEN OTHERS THEN GET STACKED DIAGNOSTICS ctx = PG_EXCEPTION_CONTEXT; END;
	  q := substring(ctx from '.*SQL statement "(.*)"');
	  IF q IS NULL THEN
	    INSERT INTO public.rp_log VALUES ('no text ' || ctx, 0);
	  ELSIF q ~ 'UPDATE' THEN
	    EXECUTE q;
	    GET DIAGNOSTICS n = ROW_COUNT;
	    INSERT INTO public.rp_log VALUES ('update', n);
	  ELSIF q ~ 'unnest' THEN
	    EXECUTE q USING ARRAY['3001', '3002', '3003']::text[], ARRAY['1', '1', '1']::text[];
	    GET DIAGNOSTICS n = ROW_COUNT;
	    INSERT INTO public.rp_log VALUES ('batch', n);
	  ELSE
	    INSERT INTO public.rp_log VALUES ('other: ' || left(q, 60), 0);
	  END IF;
	  RETURN true;
	END $f$}) for @nodes;
$n1->safe_psql('postgres', q{ALTER TABLE public.mine2 ADD CONSTRAINT rp_c CHECK (public.rp_replay(v))});

# A. the local part of a fan-out UPDATE: an UPDATE of the rows that are being updated cannot replay itself (the
# replay modifies the tuple the outer command is updating: "already modified by an operation triggered by the current
# command"), so the fan-out write is covered by t/118 (a statement of the function over the same table, routed) and not here.

# B. the local batch of a router INSERT replays its own text (with parameters of its own)
my ($rc, $out, $err) = run_sql($n1, q{SET rp.armed = on; INSERT INTO public.mine2 SELECT g, g FROM generate_series(1001, 1040) g});
is($rc, 0, 'B: the INSERT whose CHECK replays the text of its local batch runs') or diag($err);
is($n1->safe_psql('postgres', q{SELECT string_agg(kind || ':' || n, ',') FROM public.rp_log}), 'batch:3', 'B: the replay found the text and inserted its three rows');
is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.mine2 WHERE id IN (3001, 3002, 3003)}), 3, 'B: the replay was routed: its rows are stored, wherever their buckets are');
is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.mine2 WHERE id BETWEEN 1001 AND 1040}), 40, 'B: and the statement stored its own rows once');
is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.mine2}), 40 + 40 + 3, 'the table holds exactly the rows of both statements');
is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;
done_testing();
