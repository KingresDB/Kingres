-- SPDX-License-Identifier: AGPL-3.0-only
-- sketch_memory: the memory that the aggregate states of one backend hold,
-- the setting kingres_sketch.state_memory_limit that bounds it, and the
-- counter state_bytes() that must come back to zero on every path.
SET search_path = public, kingres_sketch;

-- nothing is held at rest; the default leaves ordinary queries alone: 1 GB.
-- The function comes first: the setting exists in a session once the library
-- is loaded there, which the first call of a function of the extension does
-- (the server of this test may or may not preload it).
SELECT state_bytes();
SHOW kingres_sketch.state_memory_limit;

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

-- the limit, for every aggregate state: a query that passes it fails with
-- 54000 and the counter is zero again at once, inside the same function (the
-- subtransaction of the exception block is aborted, so this is also the abort
-- of a subtransaction)
SET kingres_sketch.state_memory_limit = '2MB';
SHOW kingres_sketch.state_memory_limit;
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

-- who may set it: any role may lower it, only a superuser may raise it above
-- what the administrator gave it (the reset value), by SET and by ALTER ROLE
RESET kingres_sketch.state_memory_limit;
CREATE ROLE ks_mem_user NOLOGIN;
GRANT USAGE ON SCHEMA kingres_sketch TO ks_mem_user;
SET ROLE ks_mem_user;
SET kingres_sketch.state_memory_limit = '512MB';
SHOW kingres_sketch.state_memory_limit;
SET kingres_sketch.state_memory_limit = '2GB';
SHOW kingres_sketch.state_memory_limit;
RESET kingres_sketch.state_memory_limit;
SHOW kingres_sketch.state_memory_limit;
ALTER ROLE ks_mem_user SET kingres_sketch.state_memory_limit = '2GB';
RESET ROLE;
-- a superuser may raise it, for the session and for a role
SET kingres_sketch.state_memory_limit = '2GB';
SHOW kingres_sketch.state_memory_limit;
RESET kingres_sketch.state_memory_limit;
ALTER ROLE ks_mem_user SET kingres_sketch.state_memory_limit = '2GB';
SELECT setconfig FROM pg_db_role_setting WHERE setrole = 'ks_mem_user'::regrole;
ALTER ROLE ks_mem_user RESET kingres_sketch.state_memory_limit;
DROP OWNED BY ks_mem_user;
DROP ROLE ks_mem_user;

-- the range of the setting
SET kingres_sketch.state_memory_limit = '0';
RESET kingres_sketch.state_memory_limit;

-- the function: volatile, parallel restricted (a worker counts for itself)
SELECT p.provolatile, p.proparallel, p.prosecdef, p.proconfig
  FROM pg_proc p WHERE p.oid = 'kingres_sketch.state_bytes()'::regprocedure;
