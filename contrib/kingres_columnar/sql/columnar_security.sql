-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- columnar_security: what the extension installs and who may do what with it.
SET search_path = public;

-- every function of the extension: the language, who runs it, the pinned search_path, who may execute it
SELECT p.proname, pg_get_function_identity_arguments(p.oid) AS args, l.lanname AS lang,
       p.prosecdef AS security_definer,
       coalesce(p.proconfig::text, '') AS config,
       (p.proacl IS NULL OR EXISTS (SELECT 1 FROM aclexplode(p.proacl) a WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE')) AS public_execute
  FROM pg_depend d JOIN pg_proc p ON p.oid = d.objid JOIN pg_language l ON l.oid = p.prolang
 WHERE d.classid = 'pg_proc'::regclass AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
   AND d.refobjid = (SELECT oid FROM pg_extension WHERE extname = 'kingres_columnar')
 ORDER BY p.proname, args;
-- every function that runs with the rights of its owner or without a pinned search_path is listed above;
-- none may be missing the pin
SELECT count(*) AS functions_without_pinned_search_path
  FROM pg_depend d JOIN pg_proc p ON p.oid = d.objid
 WHERE d.classid = 'pg_proc'::regclass AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
   AND d.refobjid = (SELECT oid FROM pg_extension WHERE extname = 'kingres_columnar')
   AND p.proname <> 'columnar_handler'
   AND NOT EXISTS (SELECT 1 FROM unnest(p.proconfig) c WHERE c LIKE 'search_path=%pg_temp');
-- the objects of the extension
SELECT classid::regclass AS catalog, count(*) AS objects
  FROM pg_depend d WHERE d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
   AND d.refobjid = (SELECT oid FROM pg_extension WHERE extname = 'kingres_columnar')
 GROUP BY classid ORDER BY 1::text;
-- the stripe catalog: nobody but its owner has a privilege
SELECT relname, relacl IS NULL AS default_acl, (SELECT count(*) FROM aclexplode(relacl) a WHERE a.grantee <> c.relowner) AS other_grantees
  FROM pg_class c WHERE oid = 'kingres.columnar_stripe'::regclass;
SELECT has_table_privilege('public', 'kingres.columnar_stripe', 'SELECT') AS public_select,
       has_table_privilege('public', 'kingres.columnar_stripe', 'INSERT') AS public_insert;

CREATE ROLE regress_kc_owner;
CREATE ROLE regress_kc_reader;
CREATE ROLE regress_kc_writer;
CREATE ROLE regress_kc_other;
GRANT CREATE ON SCHEMA public TO regress_kc_owner;

-- the schema of the extension is not open to everybody by default
REVOKE USAGE ON SCHEMA kingres FROM PUBLIC;
SET ROLE regress_kc_owner;
CREATE TABLE s1 (id int, s text) USING columnar;
INSERT INTO s1 SELECT i, 'x' FROM generate_series(1, 100) i;
SELECT kingres.columnar_set_options('s1', compression => 'pglz');
RESET ROLE;
GRANT USAGE ON SCHEMA kingres TO regress_kc_owner, regress_kc_reader, regress_kc_writer, regress_kc_other;

-- the owner
SET ROLE regress_kc_owner;
SELECT kingres.columnar_set_options('s1', compression => 'pglz', stripe_row_count => 5000);
SELECT kingres.columnar_set_options('s1', compression => 'pglz', stripe_row_count => 5000, chunk_row_count => 1000);
SELECT compression, stripe_row_count, chunk_row_count FROM kingres.columnar_options('s1');
SELECT stripes, rows FROM kingres.columnar_info('s1');
SELECT count(*) FROM kingres.columnar_stripes('s1');
SELECT kingres.columnar_compact('s1');
-- what the owner must not reach
SELECT * FROM kingres.columnar_stripe;
INSERT INTO kingres.columnar_stripe VALUES (999999, 1, 0, 1, 1, 1, 1, 1, 1, 1, 0, '\x', '\x');
DELETE FROM kingres.columnar_stripe;
SELECT * FROM kingres.columnar_check();
SELECT kingres.columnar_reconcile();
SELECT kingres.columnar_on_drop();
SELECT kingres.columnar_check_ddl();
RESET ROLE;
-- columnar_check also checks the user, whoever may execute it
GRANT EXECUTE ON FUNCTION kingres.columnar_check(boolean) TO regress_kc_owner;
SET ROLE regress_kc_owner;
SELECT * FROM kingres.columnar_check();
SELECT * FROM kingres.columnar_check(true);
RESET ROLE;
REVOKE EXECUTE ON FUNCTION kingres.columnar_check(boolean) FROM regress_kc_owner;
SELECT * FROM kingres.columnar_check();

-- a role with SELECT can read and nothing else
GRANT SELECT ON s1 TO regress_kc_reader;
GRANT INSERT ON s1 TO regress_kc_writer;
SET ROLE regress_kc_reader;
SELECT count(*), sum(id) FROM s1;
SELECT count(*) FROM s1 WHERE id < 50;
EXPLAIN (costs off) SELECT id FROM s1 WHERE id < 50;
INSERT INTO s1 VALUES (1, 'reader');
COPY s1 FROM STDIN;
1	reader
\.
TRUNCATE s1;
SELECT kingres.columnar_set_options('s1', compression => 'none');
SELECT kingres.columnar_compact('s1');
VACUUM s1;
ANALYZE s1;
CREATE TABLE reader_t (a int) USING columnar;
RESET ROLE;
-- a role with INSERT can write, by INSERT, INSERT SELECT and COPY, and cannot read
SET ROLE regress_kc_writer;
INSERT INTO s1 VALUES (101, 'writer');
INSERT INTO s1 SELECT 102, 'writer' FROM generate_series(1, 3);
COPY s1 FROM STDIN;
110	writer copy
\.
SELECT count(*) FROM s1;
SELECT * FROM s1 WHERE id = 101;
UPDATE s1 SET s = 'u';
DELETE FROM s1;
SELECT kingres.columnar_set_options('s1', compression => 'none');
RESET ROLE;
SELECT count(*), count(*) FILTER (WHERE s LIKE 'writer%') AS by_writer FROM s1;
-- a role without privileges on the table
SET ROLE regress_kc_other;
SELECT count(*) FROM s1;
INSERT INTO s1 VALUES (1, 'other');
SELECT kingres.columnar_set_options('s1', compression => 'none');
SELECT kingres.columnar_compact('s1');
SELECT * FROM kingres.columnar_stripe;
RESET ROLE;

-- ownership: the new owner may change the options
ALTER TABLE s1 OWNER TO regress_kc_other;
SET ROLE regress_kc_other;
SELECT kingres.columnar_set_options('s1', compression => 'none');
SELECT compression FROM kingres.columnar_options('s1');
SELECT kingres.columnar_compact('s1') >= 0 AS compacted;
RESET ROLE;
SET ROLE regress_kc_owner;
SELECT kingres.columnar_set_options('s1', compression => 'lz4');
RESET ROLE;
ALTER TABLE s1 OWNER TO regress_kc_owner;

-- a table owner who is not a superuser: the checks of the event triggers still apply,
-- whatever the search_path of the session is
SET ROLE regress_kc_owner;
CREATE TABLE public.pg_trigger (tgrelid oid, tgisinternal bool, tgtype int);
CREATE TABLE public.pg_constraint (conrelid oid, confrelid oid, contype "char");
SET search_path = public, pg_catalog;
CREATE TABLE evil_pk (id int PRIMARY KEY);
CREATE TABLE evil_fk (id int REFERENCES evil_pk (id)) USING columnar;
CREATE TABLE evil_t (id int) USING columnar;
CREATE FUNCTION evil_trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
CREATE TRIGGER evil_after AFTER INSERT ON evil_t FOR EACH ROW EXECUTE FUNCTION evil_trg();
CREATE TRIGGER evil_stmt AFTER INSERT ON evil_t FOR EACH STATEMENT EXECUTE FUNCTION evil_trg();
INSERT INTO evil_t VALUES (1);
SET search_path = public;
-- dropping the tables of a non-superuser cleans the stripe catalog
SELECT kingres.columnar_identity('evil_t'::regclass) AS evil_filenode \gset
RESET ROLE;
SELECT count(*) AS stripe_rows FROM kingres.columnar_stripe WHERE rel_uid = :evil_filenode;
SET ROLE regress_kc_owner;
DROP TABLE evil_t, evil_pk;
RESET ROLE;
SELECT count(*) AS stripe_rows FROM kingres.columnar_stripe WHERE rel_uid = :evil_filenode;
SET ROLE regress_kc_owner;
DROP TABLE public.pg_trigger, public.pg_constraint;
DROP FUNCTION evil_trg();
RESET ROLE;

-- DROP OWNED and DROP ROLE with columnar tables
SELECT kingres.columnar_identity('s1'::regclass) AS s1_filenode \gset
SELECT count(*) > 0 AS has_stripes FROM kingres.columnar_stripe WHERE rel_uid = :s1_filenode;
DROP OWNED BY regress_kc_owner;
SELECT count(*) AS stripe_rows FROM kingres.columnar_stripe WHERE rel_uid = :s1_filenode;
SELECT to_regclass('s1') IS NULL AS s1_dropped;
DROP OWNED BY regress_kc_reader, regress_kc_writer, regress_kc_other;
REVOKE CREATE ON SCHEMA public FROM regress_kc_owner;
DROP ROLE regress_kc_owner;
DROP ROLE regress_kc_reader;
DROP ROLE regress_kc_writer;
DROP ROLE regress_kc_other;
SELECT count(*) AS catalog_rows FROM kingres.columnar_stripe;

-- DROP EXTENSION, in a database of its own
SELECT current_database() AS maindb \gset
CREATE DATABASE regress_kc_drop;
\c regress_kc_drop
CREATE EXTENSION kingres_columnar;
CREATE TABLE e1 (a int) USING columnar;
INSERT INTO e1 VALUES (1);
CREATE TABLE e2 (a int);
DROP EXTENSION kingres_columnar;
DROP EXTENSION kingres_columnar CASCADE;
SELECT to_regclass('e1') IS NULL AS e1_dropped, to_regclass('e2') IS NOT NULL AS e2_kept;
SELECT count(*) AS event_triggers FROM pg_event_trigger;
SELECT count(*) AS access_methods FROM pg_am WHERE amname = 'columnar';
CREATE EXTENSION kingres_columnar;
CREATE TABLE e1 (a int) USING columnar;
INSERT INTO e1 VALUES (1), (2);
SELECT count(*) FROM e1;
SELECT count(*) AS catalog_rows FROM kingres.columnar_stripe;
\c :maindb
DROP DATABASE regress_kc_drop;
