-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- columnar_reserve: row numbers reserved at insert (increment C6a, design 5.3
-- and 7.8), with the developer setting that switches the reservation on.  A row's
-- TID exists when the insert returns it and is the TID the row has after the
-- commit, whatever happened to the buffer in between (a flush by size, a flush
-- that a read caused, a savepoint, PREPARE TRANSACTION); no two rows share one;
-- the catalog row of the stripe exists from the first insert; and with the
-- setting off nothing changes.  The runs of several sessions, crashes, standbys
-- and the check functions are in t/044 to t/050.
SET search_path = public;

-- the rows every table below holds, as a fingerprint
CREATE FUNCTION fp(tbl regclass) RETURNS text LANGUAGE plpgsql AS $$
DECLARE r text;
BEGIN
  EXECUTE format('SELECT count(*) || '','' || coalesce(sum(a), 0) || '','' || coalesce(sum(hashtext(b)), 0) FROM %s', tbl) INTO r;
  RETURN r;
END $$;

-- a heap table that keeps what the inserts returned: the row and its TID
CREATE TABLE ret (tbl text, a int, t tid);
-- the stripes of a table as (first row, rows), in order
CREATE FUNCTION stripes(tbl regclass) RETURNS text LANGUAGE sql AS $$
  SELECT coalesce(string_agg('(' || first_row || ',' || row_count || ')', ' ' ORDER BY stripe_id), '-')
    FROM kingres.columnar_stripes(tbl) $$;

-- the setting is for superusers, and off
SHOW kingres_columnar.reserve_row_numbers;
CREATE ROLE reserve_user;
SET ROLE reserve_user;
SET kingres_columnar.reserve_row_numbers = on;
RESET ROLE;
DROP ROLE reserve_user;

CREATE TABLE r1 (a int, b text) USING columnar;
SELECT kingres.columnar_set_options('r1', stripe_row_count => 1000, chunk_row_count => 100);
CREATE TABLE h1 (a int, b text);
-- a table of the format of 0.1 would not reserve: this one is new
SELECT kingres.columnar_creating_xid('r1') <> 0 AS made_by_this_version;

-- With the setting off the TID of a row that is only buffered is the same
-- meaningless one for every row, as in 0.1.
BEGIN;
INSERT INTO r1 SELECT g, 'off' || g FROM generate_series(1, 3) g;
SELECT count(*) AS rows, count(*) FILTER (WHERE ctid::text LIKE '(4294967293,%') AS fake_tids FROM r1;
SELECT count(*) AS reserved_rows FROM kingres.columnar_stripe WHERE reserved;
COMMIT;
SELECT stripes('r1');
TRUNCATE r1;

SET kingres_columnar.reserve_row_numbers = on;

-- 1. TIDs: the TID that INSERT returns is the TID after the flush by size
-- (two full stripes), inside the transaction and after the commit.
BEGIN;
WITH i AS (INSERT INTO r1 SELECT g, 'v' || g FROM generate_series(1, 2500) g RETURNING a, ctid)
  INSERT INTO ret SELECT 'r1', a, ctid FROM i;
-- the catalog row of the last buffer is there, reserved, before anything is written for it
SELECT count(*) AS reserved_rows, sum(row_count) AS reserved_numbers FROM kingres.columnar_stripe WHERE reserved;
SELECT count(*) AS rows, count(DISTINCT t) AS distinct_tids,
       (SELECT count(*) FROM r1 JOIN ret USING (a) WHERE ret.tbl = 'r1' AND r1.ctid::text = ret.t::text) AS same_inside
  FROM ret WHERE tbl = 'r1';
COMMIT;
SELECT count(*) AS rows, count(DISTINCT t) AS distinct_tids,
       (SELECT count(*) FROM r1 JOIN ret USING (a) WHERE ret.tbl = 'r1' AND r1.ctid::text = ret.t::text) AS same_after_commit
  FROM ret WHERE tbl = 'r1';
SELECT min(t) AS first_tid, max(t) AS last_tid FROM ret WHERE tbl = 'r1';
-- two stripes of 1000 and the last one of 500; the unused 500 numbers were given back
SELECT stripes('r1');
SELECT count(*) AS reserved_rows FROM kingres.columnar_stripe WHERE reserved;
SELECT * FROM kingres.columnar_check();
-- the next transaction goes on from the last row
BEGIN;
WITH i AS (INSERT INTO r1 VALUES (2501, 'next') RETURNING a, ctid) INSERT INTO ret SELECT 'r1', a, ctid FROM i;
COMMIT;
SELECT t AS tid_of_next_row FROM ret WHERE tbl = 'r1' AND a = 2501;
SELECT stripes('r1');
SELECT path, hits FROM kingres.columnar_write_paths() WHERE path IN ('reserved', 'tail_returned', 'tail_gap') ORDER BY path;

-- 2. a flush that a read causes (a parallel plan) writes the rows that are there;
-- the TIDs of the rows do not change, the rows after it go on in a new reservation
TRUNCATE r1;
DELETE FROM ret;
SELECT count(*) > 0 AS reset FROM kingres.columnar_write_paths(true);
BEGIN;
WITH i AS (INSERT INTO r1 SELECT g, 'p' || g FROM generate_series(1, 300) g RETURNING a, ctid)
  INSERT INTO ret SELECT 'r1', a, ctid FROM i;
SET LOCAL debug_parallel_query = on;
SET LOCAL parallel_setup_cost = 0;
SET LOCAL parallel_tuple_cost = 0;
SELECT count(*) FROM r1;
RESET debug_parallel_query;
WITH i AS (INSERT INTO r1 SELECT g, 'p' || g FROM generate_series(301, 450) g RETURNING a, ctid)
  INSERT INTO ret SELECT 'r1', a, ctid FROM i;
SELECT stripes('r1');
SELECT count(*) AS rows, count(DISTINCT t) AS distinct_tids,
       (SELECT count(*) FROM r1 JOIN ret USING (a) WHERE ret.tbl = 'r1' AND r1.ctid::text = ret.t::text) AS same_inside
  FROM ret WHERE tbl = 'r1';
COMMIT;
SELECT stripes('r1');
SELECT count(*) AS rows, count(DISTINCT t) AS distinct_tids,
       (SELECT count(*) FROM r1 JOIN ret USING (a) WHERE ret.tbl = 'r1' AND r1.ctid::text = ret.t::text) AS same_after_commit
  FROM ret WHERE tbl = 'r1';
SELECT * FROM kingres.columnar_check();

-- 3. savepoints: a committing subtransaction writes its own buffer as its own stripe
-- and keeps the numbers; a rolled back one leaves a gap and no row
TRUNCATE r1;
DELETE FROM ret;
BEGIN;
INSERT INTO r1 VALUES (1, 'top');
SAVEPOINT s1;
INSERT INTO r1 VALUES (2, 's1');
SAVEPOINT s2;
INSERT INTO r1 VALUES (3, 's2');
ROLLBACK TO s2;
INSERT INTO r1 VALUES (4, 's1 again');
RELEASE s1;
INSERT INTO r1 VALUES (5, 'top again');
SELECT a, b, ctid FROM r1 ORDER BY ctid;
COMMIT;
SELECT a, b, ctid FROM r1 ORDER BY ctid;
SELECT stripes('r1');
SELECT * FROM kingres.columnar_check();

-- 4. PREPARE TRANSACTION: the TIDs of the rows are those of COMMIT PREPARED
TRUNCATE r1;
DELETE FROM ret;
BEGIN;
WITH i AS (INSERT INTO r1 SELECT g, 'q' || g FROM generate_series(1, 1200) g RETURNING a, ctid)
  INSERT INTO ret SELECT 'r1', a, ctid FROM i;
SAVEPOINT s;
WITH i AS (INSERT INTO r1 SELECT g, 'q' || g FROM generate_series(1201, 1300) g RETURNING a, ctid)
  INSERT INTO ret SELECT 'r1', a, ctid FROM i;
RELEASE s;
PREPARE TRANSACTION 'reserve_1';
SELECT count(*) AS reserved_rows FROM kingres.columnar_stripe WHERE reserved;
SELECT count(*) AS visible_rows FROM r1;
COMMIT PREPARED 'reserve_1';
SELECT count(*) AS rows, count(DISTINCT t) AS distinct_tids,
       (SELECT count(*) FROM r1 JOIN ret USING (a) WHERE ret.tbl = 'r1' AND r1.ctid::text = ret.t::text) AS same_after_commit_prepared
  FROM ret WHERE tbl = 'r1';
SELECT stripes('r1');
-- a prepared transaction that is rolled back leaves nothing
BEGIN;
INSERT INTO r1 SELECT g, 'gone' FROM generate_series(1, 50) g;
PREPARE TRANSACTION 'reserve_2';
ROLLBACK PREPARED 'reserve_2';
SELECT count(*) AS rows FROM r1;
SELECT count(*) AS reserved_rows FROM kingres.columnar_stripe WHERE reserved;
SELECT * FROM kingres.columnar_check();

-- 5. the same rows as a heap table
TRUNCATE r1;
INSERT INTO r1 SELECT g, 'w' || g FROM generate_series(1, 5000) g;
INSERT INTO h1 SELECT g, 'w' || g FROM generate_series(1, 5000) g;
SELECT fp('r1') = fp('h1') AS same_rows, fp('r1');
-- a rolled back insert leaves no row, its numbers are never used by another row
BEGIN;
WITH i AS (INSERT INTO r1 SELECT g, 'rb' FROM generate_series(1, 10) g RETURNING ctid) SELECT min(ctid) AS rolled_back_first FROM i;
ROLLBACK;
WITH i AS (INSERT INTO r1 VALUES (5001, 'after') RETURNING ctid) SELECT ctid AS after_rollback FROM i;
INSERT INTO h1 VALUES (5001, 'after');
SELECT fp('r1') = fp('h1') AS same_rows_after_rollback;
SELECT * FROM kingres.columnar_check();
SELECT kingres.columnar_reconcile();

-- 6. with the setting off again nothing is reserved (a table of the format of
-- 0.1, which never reserves, is in t/049_upgrade_data.pl)
RESET kingres_columnar.reserve_row_numbers;
DROP TABLE r1, h1, ret;
DROP FUNCTION fp(regclass), stripes(regclass);
