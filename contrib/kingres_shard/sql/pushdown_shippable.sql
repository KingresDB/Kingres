-- sql/pushdown_shippable.sql
--   Step 5 of phase 2b (design/shard-phase2b.md, 5.1 to 5.4 and 16.1): the shippability matrix in
--   the multi-relation mode of the walker.  One statement per rule, each shipped or not as the
--   design states, and the same matrix again under hostile session settings (the decision must
--   not depend on them).  Later steps add the matrix of the other paths and of extension objects.
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- The path under test is the colocated INSERT ... SELECT, whose statement has to pass the walker
-- as a whole (shippable.c); a statement that does not is planned by the row router.  The verdict
-- of a case is the shape of its plan.  The second group is fictitious (as in bulk_plans).
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('localhost', current_setting('port')::int);

CREATE TYPE mood AS ENUM ('sad', 'ok');
CREATE TABLE s (id int PRIMARY KEY, a int, t text, ts timestamp, tz timestamptz, f float8, m money,
                b bytea, dt date, n numeric, e mood, c char(3), tc text COLLATE "C", arr int[], iv interval);
CREATE TABLE d (id int PRIMARY KEY, a int, t text, ts timestamp, tz timestamptz, f float8, m money,
                b bytea, dt date, n numeric, e mood, c char(3), tc text COLLATE "C", arr int[], iv interval);
CREATE TABLE r (k int PRIMARY KEY, l text);
CREATE FUNCTION my_imm(int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT $1';
CREATE FUNCTION my_stable(int) RETURNS int LANGUAGE sql STABLE AS 'SELECT $1';
CREATE FUNCTION my_volatile(int) RETURNS int LANGUAGE sql VOLATILE AS 'SELECT $1';
SELECT kingres.create_distributed_table('s', 'id', bucket_count => 8);
SELECT kingres.create_distributed_table('d', 'id', colocate_with => 's');
SELECT kingres.create_reference_table('r');
INSERT INTO kingres.node (node_id, group_id, host, port, role)
  VALUES (2, 1, '127.0.0.1', 1, 'primary');
UPDATE kingres.bucket_map SET group_id = 1 WHERE bucket % 2 = 1;

-- The verdict of a statement: shipped whole, or something else.
CREATE FUNCTION verdict(q text) RETURNS text LANGUAGE plpgsql AS $$
DECLARE l text; r text := 'other';
BEGIN
  FOR l IN EXECUTE 'EXPLAIN (COSTS OFF) ' || q LOOP
    IF l LIKE '%Kingres Fan-out Write%' THEN r := 'shipped'; END IF;
    IF l LIKE '%Kingres Row Router%' THEN r := 'router'; END IF;
  END LOOP;
  RETURN r;
EXCEPTION WHEN feature_not_supported THEN
  RETURN 'refused';
END $$;

-- an expression in the restriction of the source, and one in its select list
CREATE FUNCTION in_where(cond text) RETURNS text LANGUAGE sql AS
  $$ SELECT verdict('INSERT INTO d (id) SELECT id FROM s WHERE ' || cond) $$;
CREATE FUNCTION in_list(col text, expr text) RETURNS text LANGUAGE sql AS
  $$ SELECT verdict('INSERT INTO d (id, ' || col || ') SELECT id, ' || expr || ' FROM s') $$;

CREATE TABLE matrix (rule text, what text, verdict text, PRIMARY KEY (rule, what));
CREATE FUNCTION run_matrix() RETURNS void LANGUAGE plpgsql AS $$
DECLARE c record;
BEGIN
  FOR c IN SELECT * FROM (VALUES
    -- rule 1: functions and operators are built in and immutable
    ('1 immutable built-in', 'a + 1 > 0'),
    ('1 immutable built-in', 'abs(a) > 0'),
    ('1 immutable built-in', 'upper(t) = ''A'''),
    ('1 immutable built-in', 'ts + interval ''1 day'' > ts'),
    ('1 immutable built-in', 'dt + 1 > dt'),
    ('1 stable built-in', 'tz > now()'),
    ('1 stable built-in', 'dt < current_date'),
    ('1 stable built-in', 'to_char(tz, ''HH'') = ''01'''),
    ('1 stable built-in', 'tz + interval ''1 day'' > tz'),
    ('1 volatile built-in', 'random() < 2'),
    ('1 user function', 'my_imm(a) > 0'),
    ('1 user function', 'my_stable(a) > 0'),
    ('1 user function', 'my_volatile(a) > 0'),
    ('1 libm', 'sin(f) > 0'),
    ('1 libm', 'sqrt(f) > 0'),
    -- rule 2: types
    ('2 money', 'm > 0::money'),
    ('2 money', 'm IS NOT NULL'),
    ('2 enum', 'e = ''ok'''),
    ('2 array of a built-in', 'arr[1] > 0'),
    ('2 bytea', 'b = decode(''0001'', ''hex'')'),
    -- rule 3: collations
    ('3 default', 't < ''m'''),
    ('3 C on the column', 'tc < ''m'''),
    ('3 C explicit', 't < ''m'' COLLATE "C"'),
    ('3 POSIX explicit', 't < ''m'' COLLATE "POSIX"'),
    -- rule 4: conversions through the text form
    ('4 CoerceViaIO', 'a::text = ''1'''),
    ('4 CoerceViaIO', 'tz::text = ''x'''),
    ('4 CoerceViaIO', 'f::text = ''1'''),
    ('4 array coercion', 'arr::bigint[] = ''{1}'''),
    -- rule 5: system columns
    ('5 system column', 'ctid IS NOT NULL'),
    ('5 system column', 'tableoid IS NOT NULL'),
    -- rule 6: reg* constants
    ('6 reg constant', 'a = ''s''::regclass::oid::int'),
    ('6 reg constant', 'a = ''pg_catalog.int4pl''::regproc::oid::int'),
    -- rule 7: node kinds
    ('7 case', 'CASE WHEN a > 1 THEN true ELSE false END'),
    ('7 coalesce', 'coalesce(a, 1) > 0'),
    ('7 row', '(a, t) = (1, ''x'')'),
    ('7 field select', '(s).a > 0'),
    ('7 whole row', 's IS NOT NULL'),
    ('7 sub-select', 'a IN (SELECT k FROM r)'),
    ('7 sub-select', 'EXISTS (SELECT 1 FROM d WHERE d.id = s.id)'),
    ('7 current_user', 'current_user = ''x'''),
    ('7 session_user', 'session_user = ''x'''),
    ('7 json', 'json_scalar(a)::text <> ''x'''),
    ('7 array operator', 'arr @> ARRAY[1]'),
    ('7 in list', 'a IN (1, 2, 3)'),
    ('7 any array', 'a = ANY (ARRAY[1, 2])'),
    ('7 between', 'a BETWEEN 1 AND 3'),
    ('7 is distinct', 'a IS DISTINCT FROM 3'),
    ('7 nullif', 'nullif(a, 3) IS NULL'),
    ('7 greatest', 'greatest(a, 3) > 3'),
    ('7 boolean test', '(a > 1) IS TRUE'),
    -- 5.3: other parts of the environment
    ('5.3 text of a float', 'f = 1.5'),
    ('5.3 interval constant', 'iv > interval ''2 days 3 hours'''),
    ('5.3 date constant', 'dt > date ''2026-01-01'''),
    ('5.3 timestamp constant', 'ts > timestamp ''2026-01-01 12:00:00'''),
    ('5.3 timestamptz constant', 'tz > timestamptz ''2026-01-01 12:00:00+05'''),
    ('5.3 bytea constant', 'b <> decode(''dead'', ''hex'')')
  ) AS v(rule, cond)
  LOOP
    INSERT INTO matrix VALUES (c.rule, c.cond, in_where(c.cond)) ON CONFLICT DO NOTHING;
  END LOOP;
  -- select-list expressions
  FOR c IN SELECT * FROM (VALUES
    ('L immutable', 'a', 'a + 1'),
    ('L immutable', 't', 'upper(t) || ''x'''),
    ('L immutable', 'ts', 'ts + interval ''1 day'''),
    ('L stable', 'tz', 'now()'),
    ('L stable', 'tz', 'tz + interval ''1 hour'''),
    ('L volatile', 'f', 'random()'),
    ('L user function', 'a', 'my_imm(a)'),
    ('L money', 'm', 'm'),
    ('L enum', 'e', 'e'),
    ('L cast via text', 't', 'a::text'),
    ('L cast numeric', 'n', 'a::numeric'),
    ('L cast bigint', 'a', 'a::bigint::int'),
    ('L array', 'arr', 'ARRAY[a, a]'),
    ('L case', 'a', 'CASE WHEN a > 1 THEN 1 ELSE 2 END'),
    ('L concat of text', 't', 't || t'),
    ('L bytea', 'b', 'b || b'),
    ('L char', 'c', 'c'),
    ('L whole row', 't', '(s.*)::text')
  ) AS v(rule, col, expr)
  LOOP
    INSERT INTO matrix VALUES (c.rule, c.expr, in_list(c.col, c.expr)) ON CONFLICT DO NOTHING;
  END LOOP;
END $$;

-- ---------------------------------------------------------------------------
-- The matrix, with the default settings of the session
-- ---------------------------------------------------------------------------
SELECT run_matrix();
SELECT rule, what, verdict FROM matrix ORDER BY rule, what;

-- ---------------------------------------------------------------------------
-- The same under hostile session settings: no verdict changes
-- ---------------------------------------------------------------------------
CREATE TABLE matrix0 AS SELECT * FROM matrix;
TRUNCATE matrix;
SET DateStyle = 'German, DMY';
SET IntervalStyle = 'sql_standard';
SET extra_float_digits = -3;
SET bytea_output = 'escape';
SET TimeZone = 'Asia/Tokyo';
SET lc_monetary = 'C';
SET search_path = pg_temp, public;
SELECT run_matrix();
SELECT count(*) AS cases, count(*) FILTER (WHERE m0.verdict = m.verdict) AS same_verdict
  FROM matrix m JOIN matrix0 m0 USING (rule, what);
RESET ALL;

-- ---------------------------------------------------------------------------
-- The statement as a whole: the target list, ON CONFLICT and RETURNING go through the same walker
-- ---------------------------------------------------------------------------
SELECT verdict('INSERT INTO d (id, a) SELECT id, a FROM s ON CONFLICT (id) DO UPDATE SET a = excluded.a + 1') AS do_update_immutable,
       verdict('INSERT INTO d (id, a) SELECT id, a FROM s ON CONFLICT (id) DO UPDATE SET a = my_imm(excluded.a)') AS do_update_user_function,
       verdict('INSERT INTO d (id, a) SELECT id, a FROM s ON CONFLICT (id) DO UPDATE SET a = excluded.a WHERE d.tz > now()') AS do_update_where_stable,
       verdict('INSERT INTO d (id, a) SELECT id, a FROM s ON CONFLICT (id) DO UPDATE SET a = (SELECT 1)') AS do_update_sub_select,
       verdict('INSERT INTO d (id, a) SELECT id, a FROM s ON CONFLICT (id) DO UPDATE SET a = excluded.a WHERE d.a > 0') AS do_update_where,
       verdict('INSERT INTO d (id, a) SELECT id, a FROM s RETURNING a + 1') AS returning_immutable,
       verdict('INSERT INTO d (id, a) SELECT id, a FROM s RETURNING random()') AS returning_volatile,
       verdict('INSERT INTO d (id, a) SELECT id, a FROM s RETURNING d.*') AS returning_star_with_money_and_enum,
       verdict('INSERT INTO d (id, a) SELECT id, a FROM s RETURNING (d.*)::text') AS returning_whole_row,
       verdict('INSERT INTO d (id, m) SELECT id, m FROM s') AS money_column,
       verdict('INSERT INTO d (id, e) SELECT id, e FROM s') AS enum_column,
       verdict('INSERT INTO d (id, tc) SELECT id, tc FROM s') AS c_collation_column;

-- ---------------------------------------------------------------------------
-- Clean up
-- ---------------------------------------------------------------------------
UPDATE kingres.bucket_map SET group_id = 0;
DELETE FROM kingres.node WHERE node_id = 2;
DROP TABLE d, s, r, matrix, matrix0;
DROP FUNCTION run_matrix();
DROP FUNCTION in_where(text);
DROP FUNCTION in_list(text, text);
DROP FUNCTION verdict(text);
DROP FUNCTION my_imm(int);
DROP FUNCTION my_stable(int);
DROP FUNCTION my_volatile(int);
DROP TYPE mood;
DROP EXTENSION kingres_shard;
