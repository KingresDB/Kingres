-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- sketch_memory: the memory that the aggregate states of one backend hold,
-- the setting kingres_sketch.state_memory_limit that bounds it, and the
-- counter state_bytes() that must come back to zero on every path.
SET search_path = public, kingres_sketch;

-- nothing is held at rest; the default leaves ordinary queries alone: a ceiling
-- of 1 GB (set by the administrator) and no limit of the role's own.
-- The function comes first: the setting exists in a session once the library
-- is loaded there, which the first call of a function of the extension does
-- (the server of this test may or may not preload it).
SELECT state_bytes();
SHOW kingres_sketch.state_memory_ceiling;
SHOW kingres_sketch.state_memory_limit;
SELECT state_limit();

-- a completed query leaves nothing behind
SELECT sum(topn_total(t)) FROM (
  SELECT g, topn_add_agg(i % 500) AS t
    FROM (SELECT i, i % 40 AS g FROM generate_series(1, 20000) i) s GROUP BY g) q;
SELECT state_bytes();

-- a state is counted while it lives: a cursor that is left open holds the
-- states of its groups, closing it gives them back
BEGIN;
DECLARE c CURSOR FOR
  SELECT topn_total(t) FROM (SELECT g, topn_add_agg(i::text) AS t
    FROM (SELECT i, i % 40 AS g FROM generate_series(1, 20000) i) s GROUP BY g) q;
FETCH 1 FROM c;
SELECT state_bytes() > 0 AS held_by_the_open_cursor;
CLOSE c;
SELECT state_bytes() AS after_close;
COMMIT;
SELECT state_bytes();

-- all the states of one aggregate node live in one memory context, not one
-- context per state (which cost more than a kilobyte each and made a hash
-- aggregate of many groups slow: PostgreSQL walks the child contexts of the
-- hash table on every new group).  50000 small groups hold a few megabytes.
BEGIN;
SET LOCAL enable_sort = off;
SET LOCAL work_mem = '256MB';
DECLARE c CURSOR FOR
  SELECT count(s) FROM (SELECT g, hll_add_agg(g) AS s FROM generate_series(1, 50000) g GROUP BY g) q;
FETCH 1 FROM c;
SELECT state_bytes() > 0 AS held, state_bytes() < 25 * 1024 * 1024 AS under_25mb;
SELECT count(*) AS state_contexts FROM pg_backend_memory_contexts WHERE name = 'kingres_sketch aggregate states';
CLOSE c;
SELECT state_bytes() AS after_close;
COMMIT;

-- the other ways an aggregate runs give everything back: a sorted aggregate
-- (its context is reset after every group), DISTINCT, GROUPING SETS, a window
SET enable_hashagg = off;
SELECT count(*) FROM (SELECT g % 100, hll_cardinality(hll_add_agg(g)) FROM generate_series(1, 20000) g GROUP BY 1) q;
SELECT state_bytes() AS sorted_aggregate;
RESET enable_hashagg;
SELECT hll_cardinality(hll_add_agg(DISTINCT g % 10)) FROM generate_series(1, 1000) g;
SELECT state_bytes() AS distinct_aggregate;
SELECT count(*) FROM (SELECT g % 3 AS a, g % 5 AS b, topn_total(topn_add_agg(g % 7)) FROM generate_series(1, 3000) g
  GROUP BY GROUPING SETS ((a), (b), (a, b), ())) q;
SELECT state_bytes() AS grouping_sets;
SELECT count(*) FROM (SELECT hll_cardinality(hll_add_agg(g) OVER (PARTITION BY g % 10 ORDER BY g)) FROM generate_series(1, 2000) g) q;
SELECT state_bytes() AS window_aggregate;

-- the limit, for every aggregate state: a query that passes it fails with
-- 54000 and the counter is zero again at once, inside the same function (the
-- subtransaction of the exception block is aborted, so this is also the abort
-- of a subtransaction)
SET kingres_sketch.state_memory_limit = '2MB';
SHOW kingres_sketch.state_memory_limit;
SELECT state_limit();
DO $$
DECLARE
  q text[] := ARRAY[
    'SELECT sum(topn_total(t)) FROM (SELECT topn_add_agg(repeat(''x'', 200) || i::text, 20000) AS t FROM generate_series(1, 20000) i GROUP BY i % 4) q',
    'SELECT sum(hll_cardinality(h)) FROM (SELECT hll_add_agg(i, 18) AS h FROM generate_series(1, 1000000) i GROUP BY i % 10) q',
    'SELECT sum(tdigest_count(d)) FROM (SELECT tdigest_agg(i::float8, 10000) AS d FROM generate_series(1, 200000) i GROUP BY i % 40) q',
    'SELECT sum(p) FROM (SELECT approx_percentile(0.5, i::float8, 10000) AS p FROM generate_series(1, 200000) i GROUP BY i % 40) q'];
  s text;
BEGIN
  FOREACH s IN ARRAY q LOOP
    BEGIN
      EXECUTE s;
      RAISE NOTICE 'no error';
    EXCEPTION WHEN program_limit_exceeded THEN
      RAISE NOTICE 'sqlstate %, message: %, bytes after the error: %', SQLSTATE,
        regexp_replace(SQLERRM, '[0-9]+', 'N', 'g'), kingres_sketch.state_bytes();
    END;
  END LOOP;
END $$;
SELECT state_bytes();

-- a query under the limit still runs under a low limit
SELECT topn_total(topn_add_agg(i % 10)) FROM generate_series(1, 20000) i;
SELECT state_bytes();

-- the union (combine) path: the states of the groups and the merged state
-- are counted; an error in the middle leaves nothing
CREATE TEMP TABLE parts AS
  SELECT g, topn_add_agg(repeat('y', 100) || i::text, 500) AS t
    FROM (SELECT i, i % 4 AS g FROM generate_series(1, 4000) i) s GROUP BY g;
SET kingres_sketch.state_memory_limit = '1GB';
SELECT topn_total(topn_union_agg(t)) FROM parts;
SET kingres_sketch.state_memory_limit = '64kB';
DO $$ BEGIN
  PERFORM topn_total(topn_union_agg(t)) FROM parts;
EXCEPTION WHEN program_limit_exceeded THEN
  RAISE NOTICE 'union: sqlstate %, bytes after the error: %', SQLSTATE, kingres_sketch.state_bytes();
END $$;
SELECT state_bytes();

-- an error under a savepoint: the counter is zero after ROLLBACK TO, and the
-- SET LOCAL of the subtransaction is gone with it
\set VERBOSITY sqlstate
BEGIN;
SAVEPOINT s1;
SET LOCAL kingres_sketch.state_memory_limit = '2MB';
SELECT sum(topn_total(t)) FROM (SELECT topn_add_agg(repeat('x', 200) || i::text, 20000) AS t FROM generate_series(1, 20000) i GROUP BY i % 4) q;
ROLLBACK TO s1;
SELECT state_bytes() AS after_rollback_to_savepoint;
ROLLBACK;
\set VERBOSITY default
SELECT state_bytes();

-- who may set what.  The limit in force is the smaller of the ceiling, which
-- only a superuser sets (the setting is PGC_SUSET, so PostgreSQL itself
-- refuses a role at every place a value can enter), and the role's own
-- limit, which any role may set.  A role that sets its own limit above the
-- ceiling changes nothing: state_limit() is still the ceiling.
RESET kingres_sketch.state_memory_limit;

-- the working memory of a call that is not an aggregate (a function on a
-- stored value, a cast, an input function) is not counted, but a value that
-- alone passes the limit is refused before it is detoasted or unpacked
CREATE TEMP TABLE bigval AS
  SELECT topn_add_agg(repeat('x', 500) || i::text, 20000) AS v FROM generate_series(1, 20000) i;
SELECT length(v::bytea) > 5000000 AS value_over_5mb FROM bigval;
SET kingres_sketch.state_memory_limit = '2MB';
DO $$
DECLARE
  q text[] := ARRAY[
    'SELECT topn_total(v) FROM bigval',
    'SELECT topn_total(topn_union(v, v)) FROM bigval',
    'SELECT topn_total(topn_union_agg(v)) FROM bigval',
    'SELECT sketch_kind(v::bytea) FROM bigval',
    'SELECT topn_total((v::bytea)::topn) FROM bigval',
    'SELECT topn_total((v::text)::topn) FROM bigval'];
  s text;
BEGIN
  FOREACH s IN ARRAY q LOOP
    BEGIN
      EXECUTE s;
      RAISE NOTICE 'no error: %', s;
    EXCEPTION WHEN program_limit_exceeded THEN
      RAISE NOTICE 'refused (%): %', SQLSTATE, s;
    END;
  END LOOP;
END $$;
RESET kingres_sketch.state_memory_limit;
SELECT topn_total(v) FROM bigval;
SELECT state_bytes();
DROP TABLE bigval;

CREATE ROLE ks_mem_user NOLOGIN;
GRANT USAGE ON SCHEMA kingres_sketch TO ks_mem_user;
GRANT CREATE ON SCHEMA public TO ks_mem_user;
SET ROLE ks_mem_user;
SET kingres_sketch.state_memory_limit = '512MB';
SELECT state_limit();
SET kingres_sketch.state_memory_limit = '2GB';
SHOW kingres_sketch.state_memory_limit;
SELECT state_limit() AS stays_at_the_ceiling;
RESET kingres_sketch.state_memory_limit;
-- the ceiling, every way a role can try to set it
SET kingres_sketch.state_memory_ceiling = '2GB';
SET LOCAL kingres_sketch.state_memory_ceiling = '2GB';
SELECT set_config('kingres_sketch.state_memory_ceiling', '2GB', false);
SELECT set_config('kingres_sketch.state_memory_ceiling', '2GB', true);
ALTER ROLE ks_mem_user SET kingres_sketch.state_memory_ceiling = '2GB';
ALTER ROLE ks_mem_user SET kingres_sketch.state_memory_limit = '2GB';
-- the database of this test has another name in a tree build
SELECT current_database() AS ks_mem_db \gset
ALTER ROLE ks_mem_user IN DATABASE :"ks_mem_db" SET kingres_sketch.state_memory_ceiling = '2GB';
CREATE FUNCTION public.ks_mem_f() RETURNS bigint LANGUAGE sql
  SET kingres_sketch.state_memory_ceiling = '2GB' AS 'SELECT kingres_sketch.state_limit()';
-- a function SET clause for the role's own limit is allowed and cannot raise anything
CREATE FUNCTION public.ks_mem_g() RETURNS bigint LANGUAGE sql
  SET kingres_sketch.state_memory_limit = '2GB' AS 'SELECT kingres_sketch.state_limit()';
SELECT public.ks_mem_g();
RESET ALL;
SET search_path = public, kingres_sketch;
SELECT state_limit();
RESET ROLE;
-- a superuser may raise the ceiling, for the session and for a role; it
-- stays in force for the role's next session, which the TAP test opens
SET kingres_sketch.state_memory_ceiling = '2GB';
SELECT state_limit();
RESET kingres_sketch.state_memory_ceiling;
ALTER ROLE ks_mem_user SET kingres_sketch.state_memory_ceiling = '2GB';
SELECT setconfig FROM pg_db_role_setting WHERE setrole = 'ks_mem_user'::regrole;
ALTER ROLE ks_mem_user RESET kingres_sketch.state_memory_ceiling;
-- GRANT SET ON PARAMETER lets a role set the ceiling, here and nowhere else
GRANT SET ON PARAMETER kingres_sketch.state_memory_ceiling TO ks_mem_user;
SET ROLE ks_mem_user;
SET kingres_sketch.state_memory_ceiling = '2GB';
SELECT state_limit();
RESET ALL;
SET search_path = public, kingres_sketch;
RESET ROLE;
REVOKE SET ON PARAMETER kingres_sketch.state_memory_ceiling FROM ks_mem_user;
SET ROLE ks_mem_user;
SET kingres_sketch.state_memory_ceiling = '3GB';
RESET ROLE;
-- a SECURITY DEFINER function of a superuser runs with the session's
-- settings: the role cannot raise what the function sees
CREATE FUNCTION public.ks_mem_def() RETURNS bigint LANGUAGE sql SECURITY DEFINER
  AS 'SELECT kingres_sketch.state_limit()';
GRANT EXECUTE ON FUNCTION public.ks_mem_def() TO ks_mem_user;
SET ROLE ks_mem_user;
SET kingres_sketch.state_memory_limit = '64MB';
SELECT public.ks_mem_def();
RESET ALL;
SET search_path = public, kingres_sketch;
RESET ROLE;
DROP FUNCTION public.ks_mem_def();
DROP FUNCTION public.ks_mem_g();
DROP OWNED BY ks_mem_user;
DROP ROLE ks_mem_user;

-- the range of the setting
SET kingres_sketch.state_memory_limit = '-2';
RESET kingres_sketch.state_memory_limit;

-- the functions: volatile, parallel restricted (a worker counts for itself)
SELECT p.proname, p.provolatile, p.proparallel, p.prosecdef, p.proconfig
  FROM pg_proc p WHERE p.oid IN ('kingres_sketch.state_bytes()'::regprocedure, 'kingres_sketch.state_limit()'::regprocedure)
  ORDER BY 1;
