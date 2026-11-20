-- timepart_convert_more: what a conversion keeps of the partition, what it
-- refuses before it copies, and the rules that the first test of the
-- conversion (timepart_convert) did not pin: the enabled state of the cloned
-- triggers, storage parameters, statistics targets and the clustering index,
-- the privileges (a revoked privilege of the owner, a grant that another role
-- made), a constraint that is not validated, objects that depend on the
-- partition, a default partition, a publication of the schema, a name of
-- multibyte characters, the replica identity, the comments of constraints,
-- default privileges, another table access method, the grid in the plan of a
-- run, a superuser that has switched to the owner, and who may ask the
-- helper that knows about ledger and distributed tables.
--
-- The database encoding must be UTF8 (one name is made of two-byte letters).
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

SET client_min_messages = warning;
CREATE EXTENSION kingres_columnar;
DROP ROLE IF EXISTS tpw_owner, tpw_b, tpw_c, tpw_x;
RESET client_min_messages;
CREATE ROLE tpw_owner NOLOGIN;
CREATE ROLE tpw_b NOLOGIN;
CREATE ROLE tpw_c NOLOGIN;
CREATE ROLE tpw_x NOLOGIN;
CREATE SCHEMA tpw;
GRANT USAGE ON SCHEMA tpw TO tpw_b, tpw_c, tpw_x;
GRANT USAGE, CREATE ON SCHEMA tpw TO tpw_owner;
GRANT USAGE ON SCHEMA kingres TO tpw_owner, tpw_x;
GRANT EXECUTE ON PROCEDURE
    kingres.convert_partition(regclass, regclass, name, text[], jsonb),
    kingres.convert_old_partitions(regclass, timestamptz, name, text[], jsonb)
  TO tpw_owner;

CREATE FUNCTION tpw.digest(rel regclass) RETURNS text LANGUAGE plpgsql AS $$
DECLARE r text;
BEGIN
    EXECUTE format('SELECT count(*) || '' rows, '' || coalesce(md5(string_agg(t::text, ''|'' ORDER BY t::text)), ''-'') FROM %s t', rel) INTO r;
    RETURN r;
END $$;
CREATE FUNCTION tpw.methods(rel regclass) RETURNS TABLE (part name, am name)
LANGUAGE sql STABLE AS $$
    SELECT c.relname, a.amname
      FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_am a ON a.oid = c.relam
     WHERE i.inhparent = rel ORDER BY c.relname
$$;
CREATE FUNCTION tpw.acl(rel regclass) RETURNS text LANGUAGE sql STABLE AS $$
    SELECT CASE WHEN relacl IS NULL OR relacl = acldefault('r', relowner) THEN 'default'
                ELSE regexp_replace(relacl::text, relowner::regrole::text, 'OWNER', 'g') END
      FROM pg_class WHERE oid = rel
$$;
CREATE FUNCTION tpw.trg(rel regclass) RETURNS TABLE (tgname name, tgenabled "char")
LANGUAGE sql STABLE AS $$
    SELECT t.tgname, t.tgenabled FROM pg_trigger t WHERE t.tgrelid = rel AND NOT t.tgisinternal ORDER BY t.tgname
$$;
CREATE FUNCTION tpw.noop() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END';

------------------------------------------------------------------------
-- 1. What the new partition keeps: triggers cloned from the table in
--    their enabled state (disabled, replica, always), storage parameters,
--    statistics targets, the n_distinct option, the statistics target of an
--    index expression, the clustering index, the replica identity, the
--    comments of constraints, and no default privilege of the schema
------------------------------------------------------------------------
CREATE TABLE tpw.st (id int, ts timestamptz NOT NULL, dev int, note text, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
CREATE TRIGGER tr1 BEFORE INSERT ON tpw.st FOR EACH ROW EXECUTE FUNCTION tpw.noop();
CREATE TRIGGER tr2 BEFORE INSERT ON tpw.st FOR EACH ROW EXECUTE FUNCTION tpw.noop();
CREATE TRIGGER tr3 BEFORE INSERT ON tpw.st FOR EACH ROW EXECUTE FUNCTION tpw.noop();
CREATE TRIGGER tr4 BEFORE INSERT ON tpw.st FOR EACH ROW EXECUTE FUNCTION tpw.noop();
CREATE TABLE tpw.st_a PARTITION OF tpw.st FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE tpw.st_b PARTITION OF tpw.st FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
INSERT INTO tpw.st SELECT i, '2026-01-01 UTC'::timestamptz + (i % 50) * interval '1 day', i % 9, 'n' || i FROM generate_series(1, 400) i;
ALTER TABLE tpw.st OWNER TO tpw_owner;
ALTER TABLE tpw.st_a OWNER TO tpw_owner;
ALTER TABLE tpw.st_b OWNER TO tpw_owner;
-- enabled state of cloned triggers
ALTER TABLE tpw.st_a ENABLE REPLICA TRIGGER tr1;
ALTER TABLE tpw.st_a ENABLE ALWAYS TRIGGER tr2;
ALTER TABLE tpw.st_a DISABLE TRIGGER tr3;
-- storage parameters, statistics targets, options of a column
ALTER TABLE tpw.st_a SET (fillfactor = 70, autovacuum_enabled = false, parallel_workers = 3, toast.autovacuum_enabled = false);
ALTER TABLE tpw.st_a ALTER COLUMN note SET STATISTICS 500;
ALTER TABLE tpw.st_a ALTER COLUMN dev SET (n_distinct = 100);
-- an index with an expression, its statistics target, the clustering index
CREATE INDEX st_a_expr ON tpw.st_a ((dev + 1), ts);
ALTER INDEX tpw.st_a_expr ALTER COLUMN 1 SET STATISTICS 777;
ALTER TABLE tpw.st_a CLUSTER ON st_a_expr;
-- comments of constraints, of the partition's own key and of one made on it
ALTER TABLE tpw.st_a ADD CONSTRAINT st_a_uq UNIQUE (id, ts, dev);
COMMENT ON CONSTRAINT st_a_uq ON tpw.st_a IS 'unique of a';
COMMENT ON CONSTRAINT st_a_pkey ON tpw.st_a IS 'key of a';
COMMENT ON INDEX tpw.st_a_expr IS 'expression of a';
-- replica identity
ALTER TABLE tpw.st_a REPLICA IDENTITY FULL;
ALTER TABLE tpw.st_b REPLICA IDENTITY NOTHING;
-- a default privilege of the schema does not reach the new partition
ALTER DEFAULT PRIVILEGES FOR ROLE tpw_owner IN SCHEMA tpw GRANT SELECT ON TABLES TO tpw_x;
SELECT tpw.digest('tpw.st') AS st_digest \gset

CREATE FUNCTION tpw.kept(rel regclass) RETURNS TABLE (what text, value text) LANGUAGE sql STABLE AS $$
    SELECT 'triggers', string_agg(t.tgname::text || ':' || t.tgenabled::text, ' ' ORDER BY t.tgname)
      FROM pg_trigger t WHERE t.tgrelid = rel AND NOT t.tgisinternal
    UNION ALL
    SELECT 'reloptions', coalesce(reloptions::text, '-') FROM pg_class WHERE oid = rel
    UNION ALL
    SELECT 'toast reloptions', coalesce((SELECT t.reloptions::text FROM pg_class t WHERE t.oid = c.reltoastrelid), '-') FROM pg_class c WHERE c.oid = rel
    UNION ALL
    SELECT 'statistics ' || a.attname, coalesce(a.attstattarget::text, '-') || ' ' || coalesce((SELECT array_agg(o)::text FROM unnest(a.attoptions) o WHERE o NOT LIKE 'pg_attver=%'), '-')
      FROM pg_attribute a WHERE a.attrelid = rel AND a.attnum > 0 AND NOT a.attisdropped AND (a.attstattarget IS NOT NULL OR EXISTS (SELECT 1 FROM unnest(a.attoptions) o WHERE o NOT LIKE 'pg_attver=%'))
    UNION ALL
    SELECT 'index statistics ' || ic.relname, coalesce(a.attstattarget::text, '-')
      FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid JOIN pg_attribute a ON a.attrelid = ic.oid AND a.attnum = 1
     WHERE i.indrelid = rel AND a.attstattarget IS NOT NULL
    UNION ALL
    SELECT 'clustered on', coalesce((SELECT ic.relname FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid WHERE i.indrelid = rel AND i.indisclustered), '-')
    UNION ALL
    SELECT 'replica identity', relreplident::text FROM pg_class WHERE oid = rel
    UNION ALL
    SELECT 'comment on ' || c.conname, obj_description(c.oid, 'pg_constraint') FROM pg_constraint c WHERE c.conrelid = rel AND obj_description(c.oid, 'pg_constraint') IS NOT NULL
    UNION ALL
    SELECT 'comment on index ' || ic.relname, obj_description(ic.oid, 'pg_class') FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid WHERE i.indrelid = rel AND obj_description(ic.oid, 'pg_class') IS NOT NULL AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = ic.oid)
    UNION ALL
    SELECT 'privileges', tpw.acl(rel)
    ORDER BY 1
$$;
SELECT * FROM tpw.kept('tpw.st_a');
SELECT * FROM tpw.kept('tpw.st_b') WHERE what IN ('replica identity', 'privileges');
-- heap -> columnar
CALL kingres.convert_partition('tpw.st', 'tpw.st_a');
CALL kingres.convert_partition('tpw.st', 'tpw.st_b');
SELECT * FROM tpw.methods('tpw.st');
SELECT * FROM tpw.kept('tpw.st_a');
SELECT * FROM tpw.kept('tpw.st_b') WHERE what IN ('replica identity', 'privileges');
-- and back
CALL kingres.convert_partition('tpw.st', 'tpw.st_a', 'heap');
CALL kingres.convert_partition('tpw.st', 'tpw.st_b', 'heap');
SELECT * FROM tpw.methods('tpw.st');
SELECT * FROM tpw.kept('tpw.st_a');
SELECT * FROM tpw.kept('tpw.st_b') WHERE what IN ('replica identity', 'privileges');
SELECT tpw.digest('tpw.st') = :'st_digest' AS st_same_rows;
ALTER DEFAULT PRIVILEGES FOR ROLE tpw_owner IN SCHEMA tpw REVOKE SELECT ON TABLES FROM tpw_x;

------------------------------------------------------------------------
-- 2. Privileges: a privilege that the owner took from itself stays taken;
--    a grant that another role made is refused (the owner would become its
--    grantor)
------------------------------------------------------------------------
CREATE TABLE tpw.pv (id int, ts timestamptz NOT NULL, note text) PARTITION BY RANGE (ts);
CREATE TABLE tpw.pv_a PARTITION OF tpw.pv FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE tpw.pv_b PARTITION OF tpw.pv FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
INSERT INTO tpw.pv SELECT i, '2026-01-01 UTC'::timestamptz + (i % 50) * interval '1 day', 'n' || i FROM generate_series(1, 100) i;
ALTER TABLE tpw.pv OWNER TO tpw_owner;
ALTER TABLE tpw.pv_a OWNER TO tpw_owner;
ALTER TABLE tpw.pv_b OWNER TO tpw_owner;
GRANT SELECT ON tpw.pv_a TO tpw_b WITH GRANT OPTION;
REVOKE UPDATE, DELETE ON tpw.pv_a FROM tpw_owner;
SELECT tpw.acl('tpw.pv_a');
CALL kingres.convert_partition('tpw.pv', 'tpw.pv_a');
SELECT tpw.acl('tpw.pv_a');
CALL kingres.convert_partition('tpw.pv', 'tpw.pv_a', 'heap');
SELECT tpw.acl('tpw.pv_a');
-- B passes its right on: the grant of C is B's, not the owner's
GRANT SELECT ON tpw.pv_b TO tpw_b WITH GRANT OPTION;
SET SESSION AUTHORIZATION tpw_b;
GRANT SELECT ON tpw.pv_b TO tpw_c;
RESET SESSION AUTHORIZATION;
SELECT tpw.acl('tpw.pv_b');
CALL kingres.convert_partition('tpw.pv', 'tpw.pv_b');
-- the same for a column
REVOKE SELECT ON tpw.pv_b FROM tpw_b CASCADE;
GRANT UPDATE (note) ON tpw.pv_b TO tpw_b WITH GRANT OPTION;
SET SESSION AUTHORIZATION tpw_b;
GRANT UPDATE (note) ON tpw.pv_b TO tpw_c;
RESET SESSION AUTHORIZATION;
CALL kingres.convert_partition('tpw.pv', 'tpw.pv_b');
REVOKE UPDATE (note) ON tpw.pv_b FROM tpw_b CASCADE;
CALL kingres.convert_partition('tpw.pv', 'tpw.pv_b');
SELECT * FROM tpw.methods('tpw.pv');

------------------------------------------------------------------------
-- 3. A constraint that is not validated is refused
------------------------------------------------------------------------
CREATE TABLE tpw.nv (id int, ts timestamptz NOT NULL, note text) PARTITION BY RANGE (ts);
CREATE TABLE tpw.nv_a PARTITION OF tpw.nv FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
INSERT INTO tpw.nv SELECT i, '2026-01-01 UTC'::timestamptz + (i % 20) * interval '1 day', 'n' || i FROM generate_series(1, 100) i;
ALTER TABLE tpw.nv_a ADD CONSTRAINT nv_bad CHECK (id < 50) NOT VALID;
CALL kingres.convert_partition('tpw.nv', 'tpw.nv_a');
ALTER TABLE tpw.nv_a DROP CONSTRAINT nv_bad;
ALTER TABLE tpw.nv_a ADD CONSTRAINT nv_ok CHECK (id < 500) NOT VALID;
CALL kingres.convert_partition('tpw.nv', 'tpw.nv_a');
ALTER TABLE tpw.nv_a VALIDATE CONSTRAINT nv_ok;
CALL kingres.convert_partition('tpw.nv', 'tpw.nv_a');
SELECT part, am FROM tpw.methods('tpw.nv');

------------------------------------------------------------------------
-- 4. Objects that depend on the partition are found before the copy
------------------------------------------------------------------------
CREATE TABLE tpw.dp (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tpw.dp_a PARTITION OF tpw.dp FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
INSERT INTO tpw.dp SELECT i, '2026-01-05 UTC' FROM generate_series(1, 50) i;
CREATE TABLE tpw.other (id int, v text);
CREATE FUNCTION tpw.f_row(x tpw.dp_a) RETURNS int LANGUAGE sql AS 'SELECT 1';
CALL kingres.convert_partition('tpw.dp', 'tpw.dp_a');
DROP FUNCTION tpw.f_row(tpw.dp_a);
CREATE FUNCTION tpw.f_arr(x tpw.dp_a[]) RETURNS int LANGUAGE sql AS 'SELECT 1';
CALL kingres.convert_partition('tpw.dp', 'tpw.dp_a');
DROP FUNCTION tpw.f_arr(tpw.dp_a[]);
CREATE FUNCTION tpw.f_body() RETURNS bigint LANGUAGE sql BEGIN ATOMIC SELECT count(*) FROM tpw.dp_a; END;
CALL kingres.convert_partition('tpw.dp', 'tpw.dp_a');
DROP FUNCTION tpw.f_body();
CREATE POLICY other_p ON tpw.other USING (EXISTS (SELECT 1 FROM tpw.dp_a));
CALL kingres.convert_partition('tpw.dp', 'tpw.dp_a');
DROP POLICY other_p ON tpw.other;
ALTER TABLE tpw.other ADD COLUMN c text DEFAULT ('tpw.dp_a'::regclass)::text;
CALL kingres.convert_partition('tpw.dp', 'tpw.dp_a');
ALTER TABLE tpw.other DROP COLUMN c;
CALL kingres.convert_partition('tpw.dp', 'tpw.dp_a');
SELECT * FROM tpw.methods('tpw.dp');
-- the same through a policy: refused, not failed, and not copied again
SELECT kingres.timepart_set_policy('tpw.dp', '1 month', create_ahead => '1 month', columnar_after => '1 day');
CALL kingres.convert_partition('tpw.dp', 'tpw.dp_a', 'heap');
CREATE FUNCTION tpw.f_row2(x tpw.dp_a) RETURNS int LANGUAGE sql AS 'SELECT 1';
\o /dev/null
CALL kingres.timepart_run('tpw.dp', '2026-06-01 UTC', '{convert}');
\o
SELECT a.kind, a.partition_name, a.state, left(a.detail, 60) AS detail
  FROM kingres.timepart_run_action a
 WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'dp') AND a.kind = 'convert';
SELECT kingres.timepart_unset_policy('tpw.dp');
DROP FUNCTION tpw.f_row2(tpw.dp_a);

------------------------------------------------------------------------
-- 5. A default partition that would be scanned under the strong lock
------------------------------------------------------------------------
CREATE TABLE tpw.df (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tpw.df_a PARTITION OF tpw.df FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE tpw.df_d PARTITION OF tpw.df DEFAULT;
INSERT INTO tpw.df VALUES (1, '2026-01-05 UTC'), (2, '2027-01-05 UTC');
SET kingres_timepart.convert_default_max_mb = 0;
CALL kingres.convert_partition('tpw.df', 'tpw.df_a');
SET kingres_timepart.convert_default_max_mb = 1;
CALL kingres.convert_partition('tpw.df', 'tpw.df_a');
RESET kingres_timepart.convert_default_max_mb;
SELECT * FROM tpw.methods('tpw.df');
SET kingres_timepart.convert_default_max_mb = -1;
CALL kingres.convert_partition('tpw.df', 'tpw.df_a', 'heap');
RESET kingres_timepart.convert_default_max_mb;
CALL kingres.convert_partition('tpw.df', 'tpw.df_a', 'heap');

------------------------------------------------------------------------
-- 6. A publication of the schema would publish the copy of a columnar
--    partition under its temporary name (a publication of all tables cannot
--    be made while kingres_columnar is installed)
------------------------------------------------------------------------
CREATE TABLE tpw.pb (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tpw.pb_a PARTITION OF tpw.pb FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
INSERT INTO tpw.pb SELECT i, '2026-01-05 UTC' FROM generate_series(1, 30) i;
CALL kingres.convert_partition('tpw.pb', 'tpw.pb_a');
SET client_min_messages = error;
CREATE PUBLICATION tpw_pub_schema FOR TABLES IN SCHEMA tpw;
RESET client_min_messages;
CALL kingres.convert_partition('tpw.pb', 'tpw.pb_a', 'heap');
DROP PUBLICATION tpw_pub_schema;
CALL kingres.convert_partition('tpw.pb', 'tpw.pb_a', 'heap');
SELECT * FROM tpw.methods('tpw.pb');

------------------------------------------------------------------------
-- 7. A name of multibyte letters at the limit, with two indexes: the
--    temporary names of the indexes cannot be equal
------------------------------------------------------------------------
CREATE TABLE tpw.mb (id int, ts timestamptz NOT NULL, v int) PARTITION BY RANGE (ts);
SELECT format('CREATE TABLE tpw.%I PARTITION OF tpw.mb FOR VALUES FROM (''2026-01-01'') TO (''2026-02-01'')', repeat(chr(1575), 29)) AS ddl \gset
:ddl ;
SELECT format('INSERT INTO tpw.%I SELECT i, ''2026-01-05 UTC'', i FROM generate_series(1, 20) i', repeat(chr(1575), 29)) AS ddl \gset
:ddl ;
SELECT format('CREATE INDEX ON tpw.%I (v)', repeat(chr(1575), 29)) AS ddl \gset
:ddl ;
SELECT format('CREATE INDEX ON tpw.%I (id, v)', repeat(chr(1575), 29)) AS ddl \gset
:ddl ;
SELECT octet_length(c.relname) AS name_bytes,
       (SELECT count(*) FROM pg_index i WHERE i.indrelid = c.oid) AS indexes_before
  FROM pg_inherits h JOIN pg_class c ON c.oid = h.inhrelid WHERE h.inhparent = 'tpw.mb'::regclass;
SELECT c.oid::regclass::text AS part FROM pg_inherits h JOIN pg_class c ON c.oid = h.inhrelid WHERE h.inhparent = 'tpw.mb'::regclass \gset
CALL kingres.convert_partition('tpw.mb', :'part');
SELECT a.amname, (SELECT count(*) FROM pg_index i WHERE i.indrelid = c.oid) AS indexes_after, c.relname = repeat(chr(1575), 29) AS same_name
  FROM pg_inherits h JOIN pg_class c ON c.oid = h.inhrelid JOIN pg_am a ON a.oid = c.relam WHERE h.inhparent = 'tpw.mb'::regclass;
SELECT count(*) AS rows_after FROM tpw.mb;

------------------------------------------------------------------------
-- 8. Another table access method is not a target; the helpers of a run
------------------------------------------------------------------------
CREATE FUNCTION tpw.heap2_handler(internal) RETURNS table_am_handler LANGUAGE internal AS 'heap_tableam_handler';
CREATE ACCESS METHOD tpw_heap2 TYPE TABLE HANDLER tpw.heap2_handler;
SELECT kingres.timepart__am_kind('heap') AS heap_kind, kingres.timepart__am_kind('columnar') AS columnar_kind,
       kingres.timepart__am_kind('tpw_heap2') AS other_kind, kingres.timepart__am_kind('no_such_am') IS NULL AS unknown_is_null;
CREATE TABLE tpw.am (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tpw.am_a PARTITION OF tpw.am FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
INSERT INTO tpw.am VALUES (1, '2026-01-05 UTC');
CALL kingres.convert_partition('tpw.am', 'tpw.am_a', 'tpw_heap2');
CALL kingres.convert_old_partitions('tpw.am', '2026-03-01 UTC', 'tpw_heap2');
CALL kingres.convert_partition('tpw.am', 'tpw.am_a', 'no_such_am');
SELECT * FROM tpw.methods('tpw.am');

------------------------------------------------------------------------
-- 9. A partition off the grid of the policy is not converted by a run, however
--    old; convert_old_partitions converts it
------------------------------------------------------------------------
CREATE TABLE tpw.gr (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tpw.gr_a PARTITION OF tpw.gr FOR VALUES FROM ('2026-03-01') TO ('2026-03-02');
CREATE TABLE tpw.gr_b PARTITION OF tpw.gr FOR VALUES FROM ('2026-03-02') TO ('2026-03-02 12:00');
CREATE TABLE tpw.gr_c PARTITION OF tpw.gr FOR VALUES FROM ('2026-03-02 12:00') TO ('2026-03-03');
CREATE TABLE tpw.gr_d PARTITION OF tpw.gr FOR VALUES FROM ('2026-03-03') TO ('2026-03-04');
INSERT INTO tpw.gr SELECT i, '2026-03-01 UTC'::timestamptz + (i % 3) * interval '1 day' + (i % 24) * interval '1 hour' FROM generate_series(1, 60) i;
SELECT kingres.timepart_set_policy('tpw.gr', '1 day', create_ahead => '1 day', columnar_after => '2 days');
\o /dev/null
CALL kingres.timepart_run('tpw.gr', '2026-03-10 UTC', '{convert}');
\o
SELECT * FROM tpw.methods('tpw.gr');
SELECT a.kind, a.partition_name, a.state
  FROM kingres.timepart_run_action a
 WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'gr') ORDER BY a.seq;
CALL kingres.convert_old_partitions('tpw.gr', '2026-03-10 UTC');
SELECT * FROM tpw.methods('tpw.gr');
SELECT kingres.timepart_unset_policy('tpw.gr');

------------------------------------------------------------------------
-- 10. A superuser that has switched to the owner is not the owner: the
--     owner's code would run in the superuser's session
------------------------------------------------------------------------
CREATE TABLE tpw.sr (id int, ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE tpw.sr_a PARTITION OF tpw.sr FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
INSERT INTO tpw.sr VALUES (1, '2026-01-05 UTC');
ALTER TABLE tpw.sr OWNER TO tpw_owner;
ALTER TABLE tpw.sr_a OWNER TO tpw_owner;
SET ROLE tpw_owner;
CALL kingres.convert_partition('tpw.sr', 'tpw.sr_a');
CALL kingres.convert_old_partitions('tpw.sr', '2026-03-01 UTC');
RESET ROLE;
SELECT * FROM tpw.methods('tpw.sr');
-- the owner connected as itself, and the superuser as itself, convert
SET SESSION AUTHORIZATION tpw_owner;
CALL kingres.convert_partition('tpw.sr', 'tpw.sr_a');
RESET SESSION AUTHORIZATION;
SELECT * FROM tpw.methods('tpw.sr');
CALL kingres.convert_partition('tpw.sr', 'tpw.sr_a', 'heap');
SELECT * FROM tpw.methods('tpw.sr');

------------------------------------------------------------------------
-- 11. The helper that knows about ledger and distributed tables answers the
--     owner of the table and a superuser, nobody else
------------------------------------------------------------------------
SELECT 'tpw.sr'::regclass::oid AS sr_oid, 'tpw.sr_a'::regclass::oid AS sra_oid \gset
SET SESSION AUTHORIZATION tpw_x;
SELECT kingres.timepart__special_table(:sr_oid, :sra_oid);
RESET SESSION AUTHORIZATION;
SET SESSION AUTHORIZATION tpw_owner;
SELECT kingres.timepart__special_table(:sr_oid, :sra_oid) IS NULL AS owner_gets_null;
RESET SESSION AUTHORIZATION;
SELECT kingres.timepart__special_table(:sr_oid, :sra_oid) IS NULL AS superuser_gets_null;

------------------------------------------------------------------------
-- Clean up
------------------------------------------------------------------------
SET client_min_messages = warning;
DROP SCHEMA tpw CASCADE;
DROP ACCESS METHOD IF EXISTS tpw_heap2;
DROP EXTENSION kingres_columnar;
DROP OWNED BY tpw_owner, tpw_b, tpw_c, tpw_x;
DROP ROLE tpw_owner, tpw_b, tpw_c, tpw_x;
RESET client_min_messages;
