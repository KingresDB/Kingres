-- SPDX-License-Identifier: AGPL-3.0-only
--
-- ledger_stops: the second invariant (L11-01 to L11-05).  A role that is not a
-- superuser cannot stop the block builder or the capture of rows for longer
-- than one of its own statements runs, and cannot remove the ledger or the
-- protection of a ledger table.  This test runs the roles that have everything
-- but the attribute (a member of the owner role with INHERIT and no SET,
-- pg_maintain, pg_write_all_data), and the owner of an application table, against
-- every statement that reaches a ledger object through a path that the statement
-- does not name: a view, a cascade, a dependency.  The locks that a role would
-- keep in a transaction are the concern of t/018_second_invariant.pl, which needs
-- a second session; what this file shows is that each of those statements is
-- refused or harmless, and the rule for the owner of a ledger table.
-- Works in a database of its own.
SET search_path = public;
\set VERBOSITY terse
SELECT current_database() AS orig_db \gset
SELECT current_user AS owner_name \gset
DROP DATABASE IF EXISTS kl_stops_t;
DROP DATABASE IF EXISTS kl_stops_e;
DROP ROLE IF EXISTS kl_s_inh;
DROP ROLE IF EXISTS kl_s_inh2;
DROP ROLE IF EXISTS kl_s_maint;
DROP ROLE IF EXISTS kl_s_wad;
DROP ROLE IF EXISTS kl_s_app;
DROP ROLE IF EXISTS kl_s_aud;
CREATE ROLE kl_s_inh LOGIN;
GRANT :"owner_name" TO kl_s_inh WITH INHERIT TRUE, SET FALSE;
CREATE ROLE kl_s_inh2 LOGIN;
GRANT :"owner_name" TO kl_s_inh2 WITH INHERIT TRUE, SET FALSE;
CREATE ROLE kl_s_maint LOGIN IN ROLE pg_maintain;
CREATE ROLE kl_s_wad LOGIN IN ROLE pg_write_all_data;
CREATE ROLE kl_s_app LOGIN;
CREATE ROLE kl_s_aud LOGIN;
CREATE DATABASE kl_stops_t;
\c kl_stops_t
SET search_path = public;
\set VERBOSITY terse
CREATE EXTENSION kingres_ledger;
GRANT USAGE ON SCHEMA kingres TO PUBLIC;
GRANT CREATE ON SCHEMA public TO kl_s_inh, kl_s_inh2, kl_s_maint, kl_s_wad, kl_s_app, kl_s_aud;
GRANT EXECUTE ON FUNCTION kingres.ledger_build_block(), kingres.ledger_generate_digest() TO kl_s_aud;

CREATE TABLE docs (id int PRIMARY KEY, body text);
ALTER TABLE docs OWNER TO kl_s_app;
CREATE TABLE upd (id int PRIMARY KEY, body text);
ALTER TABLE upd OWNER TO kl_s_app;
CREATE TABLE plain (id int PRIMARY KEY);
GRANT ALL ON plain TO PUBLIC;
SELECT kingres.ledger_enable('docs');
SELECT kingres.ledger_enable('upd', 'updatable');
GRANT SELECT ON docs TO PUBLIC;
GRANT kl_s_aud TO kl_s_app;
INSERT INTO docs VALUES (1, 'a'), (2, 'b');
INSERT INTO upd VALUES (1, 'a');
SELECT kingres.ledger_build_block() AS block;
SELECT count(*) AS findings FROM kingres.ledger_verify();

CREATE TABLE s_results (ord serial, who text, form text, outcome text);
GRANT ALL ON s_results TO PUBLIC;
GRANT ALL ON SEQUENCE s_results_ord_seq TO PUBLIC;

-- A statement of the caller, tried inside a block that undoes it; the outcome is
-- "ran" or the SQLSTATE of its error (KL002 is the protection, KL001 the
-- append-only error, 42501 a privilege).
CREATE FUNCTION public.s_try(who text, form text, stmt text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE res text;
BEGIN
    BEGIN
        EXECUTE stmt;
        RAISE EXCEPTION 'undo' USING ERRCODE = 'U0001';
    EXCEPTION
        WHEN SQLSTATE 'U0001' THEN res := 'ran';
        WHEN OTHERS THEN res := SQLSTATE;
    END;
    INSERT INTO public.s_results (who, form, outcome) VALUES (who, form, res);
END $$;
GRANT EXECUTE ON FUNCTION public.s_try(text, text, text) TO PUBLIC;

--------------------------------------------------------------------------
-- L11-01: a lock reaches a ledger table through a view
--------------------------------------------------------------------------
-- The views are made by the roles themselves (the lock is taken with the
-- privileges of the view's owner), and are views over a ledger table, over a
-- view, with a subquery, with a join, and over the status view.
\c - kl_s_maint
SET search_path = public;
CREATE VIEW v_blocks AS SELECT * FROM kingres.ledger_blocks;
CREATE VIEW v_pend AS SELECT * FROM kingres.ledger_pending;
CREATE VIEW vv_pend AS SELECT * FROM v_pend;
CREATE VIEW vvv_pend AS SELECT * FROM vv_pend;
CREATE VIEW v_sub AS SELECT id FROM docs WHERE id IN (SELECT height FROM kingres.ledger_blocks);
CREATE VIEW v_join AS SELECT d.id FROM docs d JOIN kingres.ledger_tx t ON t.tx_index = d.id;
CREATE VIEW v_status AS SELECT * FROM kingres.ledger_status;
CREATE VIEW v_cte AS WITH c AS (SELECT * FROM kingres.ledger_entry) SELECT count(*) FROM c;
CREATE VIEW v_user AS SELECT * FROM docs;
CREATE VIEW v_harmless AS SELECT * FROM plain;
\c - kl_s_wad
SET search_path = public;
CREATE VIEW w_blocks AS SELECT * FROM kingres.ledger_blocks;
CREATE VIEW w_pend AS SELECT * FROM kingres.ledger_pending;
CREATE VIEW ww_pend AS SELECT * FROM w_pend;
CREATE VIEW w_user AS SELECT * FROM docs;
CREATE VIEW w_harmless AS SELECT * FROM plain;

\c - :owner_name
SET search_path = public;
CREATE FUNCTION public.s_run_locks() RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    r text := CASE current_user WHEN 'kl_s_maint' THEN 'pg_maintain' ELSE 'pg_write_all_data' END;
    v text;
    m text;
BEGIN
    PERFORM public.s_try(r, 'direct: lock ledger_blocks access exclusive', 'LOCK TABLE kingres.ledger_blocks IN ACCESS EXCLUSIVE MODE');
    PERFORM public.s_try(r, 'direct: lock ledger_pending row exclusive', 'LOCK TABLE kingres.ledger_pending IN ROW EXCLUSIVE MODE');
    PERFORM public.s_try(r, 'direct: lock ledger_blocks access share', 'LOCK TABLE kingres.ledger_blocks IN ACCESS SHARE MODE');
    FOREACH v IN ARRAY CASE current_user
        WHEN 'kl_s_maint' THEN ARRAY['v_blocks', 'v_pend', 'vv_pend', 'vvv_pend', 'v_sub', 'v_join', 'v_status', 'v_cte']
        ELSE ARRAY['w_blocks', 'w_pend', 'ww_pend'] END
    LOOP
        FOREACH m IN ARRAY ARRAY['ACCESS EXCLUSIVE', 'EXCLUSIVE', 'SHARE ROW EXCLUSIVE', 'SHARE', 'ROW EXCLUSIVE', 'ROW SHARE']
        LOOP
            PERFORM public.s_try(r, 'view ' || v || ': lock ' || lower(m), format('LOCK TABLE %I IN %s MODE', v, m));
        END LOOP;
        PERFORM public.s_try(r, 'view ' || v || ': lock only, access exclusive', format('LOCK TABLE ONLY %I IN ACCESS EXCLUSIVE MODE', v));
        PERFORM public.s_try(r, 'view ' || v || ': lock access share', format('LOCK TABLE %I IN ACCESS SHARE MODE', v));
        PERFORM public.s_try(r, 'view ' || v || ': lock among others', format('LOCK TABLE plain, %I IN ACCESS EXCLUSIVE MODE', v));
    END LOOP;
    PERFORM public.s_try(r, 'view of a user table: lock access exclusive',
                         format('LOCK TABLE %I IN ACCESS EXCLUSIVE MODE', CASE current_user WHEN 'kl_s_maint' THEN 'v_user' ELSE 'w_user' END));
    PERFORM public.s_try(r, 'view of an ordinary table: lock access exclusive',
                         format('LOCK TABLE %I IN ACCESS EXCLUSIVE MODE', CASE current_user WHEN 'kl_s_maint' THEN 'v_harmless' ELSE 'w_harmless' END));
    PERFORM public.s_try(r, 'ordinary table: lock access exclusive', 'LOCK TABLE plain IN ACCESS EXCLUSIVE MODE');
    -- Row locks, through a view and through a cursor.
    FOREACH m IN ARRAY ARRAY['UPDATE', 'NO KEY UPDATE', 'SHARE', 'KEY SHARE']
    LOOP
        PERFORM public.s_try(r, 'direct: select for ' || lower(m), format('SELECT * FROM kingres.ledger_pending FOR %s', m));
        PERFORM public.s_try(r, 'view: select for ' || lower(m),
                             format('SELECT * FROM %I FOR %s', CASE current_user WHEN 'kl_s_maint' THEN 'vv_pend' ELSE 'ww_pend' END, m));
    END LOOP;
    PERFORM public.s_try(r, 'cursor over a view: for update',
                         format('DECLARE c CURSOR FOR SELECT * FROM %I FOR UPDATE', CASE current_user WHEN 'kl_s_maint' THEN 'vv_pend' ELSE 'ww_pend' END));
    -- ALTER of a view locks the view, not what it reads.
    PERFORM public.s_try(r, 'alter view: set a security barrier',
                         format('ALTER VIEW %I SET (security_barrier = true)', CASE current_user WHEN 'kl_s_maint' THEN 'v_blocks' ELSE 'w_blocks' END));
    PERFORM public.s_try(r, 'alter table of a view: rename',
                         format('ALTER TABLE %I RENAME TO %I', CASE current_user WHEN 'kl_s_maint' THEN 'v_blocks' ELSE 'w_blocks' END, 'zz_renamed'));
    -- Maintenance as a statement of its own is honest; the others are refused in a block.
    PERFORM public.s_try(r, 'analyze ledger_blocks', 'ANALYZE kingres.ledger_blocks');
    PERFORM public.s_try(r, 'reindex ledger_blocks in a function', 'REINDEX TABLE kingres.ledger_blocks');
    PERFORM public.s_try(r, 'cluster ledger_blocks in a function', 'CLUSTER kingres.ledger_blocks USING ledger_blocks_pkey');
END $$;
\c - kl_s_maint
SET search_path = public;
SELECT public.s_run_locks();
\c - kl_s_wad
SET search_path = public;
SELECT public.s_run_locks();
\c - :owner_name
SET search_path = public;
SELECT who, form, outcome FROM s_results ORDER BY ord;
TRUNCATE s_results;

--------------------------------------------------------------------------
-- L11-02: the ledger is dropped through something it depends on
--------------------------------------------------------------------------
-- Every statement below is run by the owner role's inheriting member (not a
-- superuser) and by a superuser; each is undone if it ran.  Afterwards the
-- extension, its tables and the capture triggers are all there.
CREATE FUNCTION public.s_run_drops() RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    r text := CASE WHEN (SELECT rolsuper FROM pg_roles WHERE rolname = current_user) THEN 'superuser' ELSE 'inheriting member' END;
BEGIN
    PERFORM public.s_try(r, 'drop extension kingres_ledger', 'DROP EXTENSION kingres_ledger');
    PERFORM public.s_try(r, 'drop extension kingres_ledger cascade', 'DROP EXTENSION kingres_ledger CASCADE');
    PERFORM public.s_try(r, 'drop extension plpgsql cascade', 'DROP EXTENSION plpgsql CASCADE');
    PERFORM public.s_try(r, 'drop language plpgsql cascade', 'DROP LANGUAGE plpgsql CASCADE');
    PERFORM public.s_try(r, 'drop schema kingres cascade', 'DROP SCHEMA kingres CASCADE');
    PERFORM public.s_try(r, 'drop schema public cascade', 'DROP SCHEMA public CASCADE');
    PERFORM public.s_try(r, 'drop owned by the owner role', format('DROP OWNED BY %I', current_setting('kl.owner')));
    PERFORM public.s_try(r, 'drop owned by the owner of a ledger table', 'DROP OWNED BY kl_s_app');
    PERFORM public.s_try(r, 'drop table kingres.ledger_blocks cascade', 'DROP TABLE kingres.ledger_blocks CASCADE');
    PERFORM public.s_try(r, 'drop table docs cascade', 'DROP TABLE docs CASCADE');
    PERFORM public.s_try(r, 'drop view ledger_status', 'DROP VIEW kingres.ledger_status');
    PERFORM public.s_try(r, 'drop function ledger_capture cascade', 'DROP FUNCTION kingres.ledger_capture() CASCADE');
    PERFORM public.s_try(r, 'drop function ledger_guard cascade', 'DROP FUNCTION kingres.ledger_guard() CASCADE');
    PERFORM public.s_try(r, 'drop event trigger', 'DROP EVENT TRIGGER kingres_ledger_guard_end');
    PERFORM public.s_try(r, 'drop trigger capture', 'DROP TRIGGER kingres_ledger_capture ON docs');
    PERFORM public.s_try(r, 'alter extension drop table', 'ALTER EXTENSION kingres_ledger DROP TABLE kingres.ledger_chain');
    PERFORM public.s_try(r, 'alter extension drop function', 'ALTER EXTENSION kingres_ledger DROP FUNCTION kingres.ledger_capture()');
    PERFORM public.s_try(r, 'alter extension add table', 'ALTER EXTENSION kingres_ledger ADD TABLE plain');
    PERFORM public.s_try(r, 'alter extension set schema', 'ALTER EXTENSION kingres_ledger SET SCHEMA public');
    PERFORM public.s_try(r, 'function depends on extension', 'ALTER FUNCTION kingres.ledger_capture() DEPENDS ON EXTENSION plpgsql');
    PERFORM public.s_try(r, 'capture trigger depends on extension', 'ALTER TRIGGER kingres_ledger_capture ON docs DEPENDS ON EXTENSION plpgsql');
    PERFORM public.s_try(r, 'catalog trigger depends on extension', 'ALTER TRIGGER kingres_ledger_block_modify ON kingres.ledger_blocks DEPENDS ON EXTENSION plpgsql');
    PERFORM public.s_try(r, 'create a trigger of its own with the capture function', 'CREATE TRIGGER s_t AFTER INSERT ON plain FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture(''1'')');
    PERFORM public.s_try(r, 'alter event trigger disable', 'ALTER EVENT TRIGGER kingres_ledger_guard_drop DISABLE');
    PERFORM public.s_try(r, 'alter event trigger owner', 'ALTER EVENT TRIGGER kingres_ledger_guard_end OWNER TO kl_s_inh');
    PERFORM public.s_try(r, 'alter function owner', 'ALTER FUNCTION kingres.ledger_capture() OWNER TO kl_s_inh');
    PERFORM public.s_try(r, 'alter function set schema', 'ALTER FUNCTION kingres.ledger_capture() SET SCHEMA public');
    PERFORM public.s_try(r, 'alter table owner (catalog)', 'ALTER TABLE kingres.ledger_chain OWNER TO kl_s_inh');
    PERFORM public.s_try(r, 'alter table set schema (catalog)', 'ALTER TABLE kingres.ledger_chain SET SCHEMA public');
    PERFORM public.s_try(r, 'reassign owned', 'REASSIGN OWNED BY kl_s_app TO kl_s_inh');
    PERFORM public.s_try(r, 'create table inheriting a catalog table', 'CREATE TABLE s_child () INHERITS (kingres.ledger_blocks)');
    PERFORM public.s_try(r, 'create a rule on a catalog table', 'CREATE RULE s_r AS ON INSERT TO kingres.ledger_blocks DO INSTEAD NOTHING');
    PERFORM public.s_try(r, 'create a policy on a catalog table', 'CREATE POLICY s_p ON kingres.ledger_blocks USING (true)');
    PERFORM public.s_try(r, 'truncate a catalog table', 'TRUNCATE kingres.ledger_blocks');
    PERFORM public.s_try(r, 'truncate a catalog table cascade', 'TRUNCATE kingres.ledger_blocks CASCADE');
    PERFORM public.s_try(r, 'truncate a ledger table', 'TRUNCATE docs');
    PERFORM public.s_try(r, 'foreign key to the queue (create table)', 'CREATE TABLE s_fk (x bigint REFERENCES kingres.ledger_pending (ledger_txid))');
    PERFORM public.s_try(r, 'foreign key to the blocks (table constraint)', 'CREATE TABLE s_fk (x bigint, FOREIGN KEY (x) REFERENCES kingres.ledger_blocks (height))');
    PERFORM public.s_try(r, 'foreign key to the blocks (add constraint)', 'CREATE TABLE s_fk (x bigint); ALTER TABLE s_fk ADD FOREIGN KEY (x) REFERENCES kingres.ledger_blocks (height)');
    PERFORM public.s_try(r, 'foreign key to the blocks (add column)', 'CREATE TABLE s_fk (y int); ALTER TABLE s_fk ADD COLUMN x bigint REFERENCES kingres.ledger_blocks (height)');
    PERFORM public.s_try(r, 'foreign key to the chain (named constraint)', 'CREATE TABLE s_fk (x bytea CONSTRAINT s_c REFERENCES kingres.ledger_chain (chain_id))');
    PERFORM public.s_try(r, 'foreign key to an ordinary table', 'CREATE TABLE s_fk (x int REFERENCES plain (id))');
END $$;
SELECT set_config('kl.owner', :'owner_name', false) \gset
\c - kl_s_inh
SET search_path = public;
SELECT set_config('kl.owner', :'owner_name', false) \gset
SELECT public.s_run_drops();
\c - :owner_name
SET search_path = public;
SELECT set_config('kl.owner', :'owner_name', false) \gset
SELECT public.s_run_drops();
SELECT who, form, outcome FROM s_results ORDER BY ord;
TRUNCATE s_results;
SELECT (SELECT count(*) FROM pg_extension WHERE extname = 'kingres_ledger') AS extensions,
       (SELECT count(*) FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'ledger!_%' ESCAPE '!' AND relkind = 'r') AS catalog_tables,
       (SELECT count(*) FROM pg_trigger WHERE tgname = 'kingres_ledger_capture' AND NOT tgisinternal) AS capture_triggers,
       (SELECT count(*) FROM pg_event_trigger) AS event_triggers;
SELECT count(*) AS findings FROM kingres.ledger_verify();
-- The tables still refuse what they refused.
SELECT kingres.ledger_enable('plain');
UPDATE docs SET body = 'x' WHERE id = 1;
DELETE FROM docs;

-- A cascade that reaches nothing of the ledger is still an ordinary statement,
-- and so is the drop of the extension when no table is under ledger control.
CREATE DATABASE kl_stops_e;
\c kl_stops_e
SET search_path = public;
CREATE EXTENSION kingres_ledger;
DROP EXTENSION kingres_ledger;
CREATE EXTENSION kingres_ledger;
DROP EXTENSION plpgsql CASCADE;
SELECT count(*) AS extensions_left FROM pg_extension WHERE extname IN ('kingres_ledger', 'plpgsql');
\c kl_stops_t
SET search_path = public;

--------------------------------------------------------------------------
-- The owner of a ledger table: one rule.  It may do to its table what changes
-- neither what is recorded nor how it is recorded; it may not do what removes
-- or weakens the capture, or changes a row that was hashed.
--------------------------------------------------------------------------
CREATE FUNCTION public.s_run_owner() RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['docs', 'upd']
    LOOP
        -- what it may do
        PERFORM public.s_try('owner', t || ': rename the table', format('ALTER TABLE %I RENAME TO %I', t, t || '_r'));
        PERFORM public.s_try('owner', t || ': add a nullable column', format('ALTER TABLE %I ADD COLUMN n int', t));
        PERFORM public.s_try('owner', t || ': set a default', format('ALTER TABLE %I ALTER COLUMN body SET DEFAULT ''d''', t));
        PERFORM public.s_try('owner', t || ': set not null', format('ALTER TABLE %I ALTER COLUMN body SET NOT NULL', t));
        PERFORM public.s_try('owner', t || ': add a check constraint', format('ALTER TABLE %I ADD CONSTRAINT s_chk CHECK (id > 0)', t));
        PERFORM public.s_try('owner', t || ': add a foreign key to an ordinary table', format('ALTER TABLE %I ADD FOREIGN KEY (id) REFERENCES plain (id) NOT VALID', t));
        PERFORM public.s_try('owner', t || ': drop its primary key', format('ALTER TABLE %I DROP CONSTRAINT %I', t, t || '_pkey'));
        PERFORM public.s_try('owner', t || ': create an index', format('CREATE INDEX s_i ON %I (body)', t));
        PERFORM public.s_try('owner', t || ': drop an index', format('CREATE INDEX s_i2 ON %I (body); DROP INDEX s_i2', t));
        PERFORM public.s_try('owner', t || ': create its own trigger', format('CREATE TRIGGER s_trg AFTER INSERT ON %I FOR EACH ROW EXECUTE FUNCTION public.s_noop()', t));
        PERFORM public.s_try('owner', t || ': enable row level security', format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t));
        PERFORM public.s_try('owner', t || ': create a policy', format('CREATE POLICY s_pol ON %I USING (true)', t));
        PERFORM public.s_try('owner', t || ': set a storage parameter', format('ALTER TABLE %I SET (fillfactor = 70)', t));
        PERFORM public.s_try('owner', t || ': grant', format('GRANT SELECT ON %I TO PUBLIC', t));
        PERFORM public.s_try('owner', t || ': lock it', format('LOCK TABLE %I IN ACCESS EXCLUSIVE MODE', t));
        PERFORM public.s_try('owner', t || ': vacuum-class maintenance (analyze)', format('ANALYZE %I', t));
        PERFORM public.s_try('owner', t || ': change the owner', format('ALTER TABLE %I OWNER TO kl_s_aud', t));
        PERFORM public.s_try('owner', t || ': create a view over it', format('CREATE VIEW s_view_%s AS SELECT * FROM %I', t, t));
        PERFORM public.s_try('owner', t || ': create a table that references it', format('CREATE TABLE s_ref (x int REFERENCES %I (id))', t));
        -- what it may not do
        PERFORM public.s_try('owner', t || ': drop the table', format('DROP TABLE %I', t));
        PERFORM public.s_try('owner', t || ': drop the schema it is in', 'DROP SCHEMA public CASCADE');
        PERFORM public.s_try('owner', t || ': drop its capture trigger', format('DROP TRIGGER kingres_ledger_capture ON %I', t));
        PERFORM public.s_try('owner', t || ': disable its capture trigger', format('ALTER TABLE %I DISABLE TRIGGER kingres_ledger_capture', t));
        PERFORM public.s_try('owner', t || ': disable all its triggers', format('ALTER TABLE %I DISABLE TRIGGER ALL', t));
        PERFORM public.s_try('owner', t || ': disable user triggers', format('ALTER TABLE %I DISABLE TRIGGER USER', t));
        PERFORM public.s_try('owner', t || ': enable its trigger for replicas only', format('ALTER TABLE %I ENABLE REPLICA TRIGGER kingres_ledger_capture', t));
        PERFORM public.s_try('owner', t || ': rename its capture trigger', format('ALTER TRIGGER kingres_ledger_capture ON %I RENAME TO s_x', t));
        PERFORM public.s_try('owner', t || ': replace its capture trigger', format('CREATE OR REPLACE TRIGGER kingres_ledger_capture AFTER INSERT ON %I FOR EACH ROW EXECUTE FUNCTION public.s_noop()', t));
        PERFORM public.s_try('owner', t || ': make its trigger depend on an extension', format('ALTER TRIGGER kingres_ledger_capture ON %I DEPENDS ON EXTENSION plpgsql', t));
        PERFORM public.s_try('owner', t || ': drop a column', format('ALTER TABLE %I DROP COLUMN body', t));
        PERFORM public.s_try('owner', t || ': change a column type', format('ALTER TABLE %I ALTER COLUMN body TYPE varchar(5)', t));
        PERFORM public.s_try('owner', t || ': rename a column', format('ALTER TABLE %I RENAME COLUMN body TO b', t));
        PERFORM public.s_try('owner', t || ': add a column with a default', format('ALTER TABLE %I ADD COLUMN n int DEFAULT 0', t));
        PERFORM public.s_try('owner', t || ': add a not null column', format('ALTER TABLE %I ADD COLUMN n int NOT NULL', t));
        PERFORM public.s_try('owner', t || ': add a generated column', format('ALTER TABLE %I ADD COLUMN n int GENERATED ALWAYS AS (id) STORED', t));
        PERFORM public.s_try('owner', t || ': set unlogged', format('ALTER TABLE %I SET UNLOGGED', t));
        PERFORM public.s_try('owner', t || ': move to another schema', format('ALTER TABLE %I SET SCHEMA kingres', t));
        PERFORM public.s_try('owner', t || ': create a rule', format('CREATE RULE s_rule AS ON INSERT TO %I DO INSTEAD NOTHING', t));
        PERFORM public.s_try('owner', t || ': make a child of it', format('CREATE TABLE s_ch () INHERITS (%I)', t));
        PERFORM public.s_try('owner', t || ': attach it as a partition', format('CREATE TABLE s_part (id int, body text) PARTITION BY RANGE (id); ALTER TABLE s_part ATTACH PARTITION %I FOR VALUES FROM (0) TO (100)', t));
        PERFORM public.s_try('owner', t || ': truncate it', format('TRUNCATE %I', t));
        PERFORM public.s_try('owner', t || ': set session_replication_role', 'SET session_replication_role = replica');
        IF t = 'upd' THEN
            PERFORM public.s_try('owner', t || ': drop its history table', 'DROP TABLE upd_ledger_history');
        END IF;
    END LOOP;
END $$;
CREATE FUNCTION public.s_noop() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$;
GRANT EXECUTE ON FUNCTION public.s_noop() TO PUBLIC;
\c - kl_s_app
SET search_path = public;
SELECT public.s_run_owner();
\c - :owner_name
SET search_path = public;
SELECT who, form, outcome FROM s_results ORDER BY ord;
TRUNCATE s_results;
-- ...and the table captures what it wrote, whatever the owner did.
SELECT count(*) AS findings FROM kingres.ledger_verify();
INSERT INTO docs VALUES (3, 'c');
SELECT kingres.ledger_build_block() AS block;
SELECT count(*) AS findings FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- L11-05: the schema, its privileges and the registry's sequence
--------------------------------------------------------------------------
CREATE FUNCTION public.s_run_schema() RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    r text := CASE WHEN (SELECT rolsuper FROM pg_roles WHERE rolname = current_user) THEN 'superuser' ELSE 'inheriting member' END;
BEGIN
    PERFORM public.s_try(r, 'revoke usage on schema kingres from public', 'REVOKE USAGE ON SCHEMA kingres FROM PUBLIC');
    PERFORM public.s_try(r, 'grant create on schema kingres', 'GRANT CREATE ON SCHEMA kingres TO kl_s_app');
    PERFORM public.s_try(r, 'alter schema kingres owner', 'ALTER SCHEMA kingres OWNER TO kl_s_app');
    PERFORM public.s_try(r, 'alter schema kingres rename', 'ALTER SCHEMA kingres RENAME TO kingres_x');
    PERFORM public.s_try(r, 'restart the registry sequence', 'ALTER SEQUENCE kingres.ledger_table_table_id_seq RESTART WITH 1');
    PERFORM public.s_try(r, 'alter the registry sequence increment', 'ALTER SEQUENCE kingres.ledger_table_table_id_seq INCREMENT BY 5');
    PERFORM public.s_try(r, 'alter the owner of the registry sequence', 'ALTER SEQUENCE kingres.ledger_table_table_id_seq OWNER TO kl_s_app');
    PERFORM public.s_try(r, 'grant update on the registry sequence', 'GRANT UPDATE ON SEQUENCE kingres.ledger_table_table_id_seq TO kl_s_app');
    PERFORM public.s_try(r, 'grant usage on all sequences of the schema', 'GRANT USAGE ON ALL SEQUENCES IN SCHEMA kingres TO kl_s_app');
    PERFORM public.s_try(r, 'grant select on the registry sequence', 'GRANT SELECT ON SEQUENCE kingres.ledger_table_table_id_seq TO kl_s_app');
    PERFORM public.s_try(r, 'create a function in the schema', 'CREATE FUNCTION kingres.s_f() RETURNS int LANGUAGE sql AS ''SELECT 1''');
    PERFORM public.s_try(r, 'create a table in the schema', 'CREATE TABLE kingres.s_t (x int)');
END $$;
\c - kl_s_inh
SET search_path = public;
SELECT public.s_run_schema();
\c - :owner_name
SET search_path = public;
SELECT public.s_run_schema();
SELECT who, form, outcome FROM s_results ORDER BY ord;
TRUNCATE s_results;
-- A sequence that is set back (setval is a function: nothing sees it) makes the
-- next registration fail on the key of the registry; nothing collides, nothing
-- is forged, and the table that was being enabled is not left half made.
SELECT last_value AS seq_last FROM kingres.ledger_table_table_id_seq \gset
\c - kl_s_inh
SET search_path = public;
SELECT setval('kingres.ledger_table_table_id_seq', 1) AS reset;
CREATE TABLE s_after (id int);
\c - :owner_name
SET search_path = public;
SELECT kingres.ledger_enable('s_after');
SELECT count(*) AS registered FROM kingres.ledger_table WHERE relid = 's_after'::regclass;
SELECT count(*) AS leftover_triggers FROM pg_trigger WHERE tgrelid = 's_after'::regclass;
SELECT setval('kingres.ledger_table_table_id_seq', :seq_last) AS restored;
SELECT kingres.ledger_enable('s_after');
SELECT count(*) AS findings FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- L11-03: what a role that may call a builder can hold
--------------------------------------------------------------------------
INSERT INTO docs VALUES (4, 'd');
\c - kl_s_aud
SET search_path = public;
SELECT kingres.ledger_build_block() IS NOT NULL AS built_alone;
BEGIN;
SELECT kingres.ledger_build_block();
ROLLBACK;
BEGIN;
SELECT kingres.ledger_generate_digest();
ROLLBACK;
BEGIN;
SAVEPOINT s;
SELECT kingres.ledger_generate_digest();
ROLLBACK;
SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest_alone;
-- A function that calls the builder is one statement.
CREATE FUNCTION public.s_build() RETURNS bigint LANGUAGE sql AS $$ SELECT kingres.ledger_build_block() $$;
SELECT public.s_build() IS NULL AS built_in_a_function;
\c - :owner_name
SET search_path = public;
-- A superuser may, as the worker does.
BEGIN;
SELECT kingres.ledger_build_block() IS NULL AS superuser_in_a_block;
-- The lock is the one the builders wait on: a lock on the extension's own
-- object, with the sub-object number that no statement uses, in EXCLUSIVE mode.
SELECT l.mode, l.granted FROM pg_locks l
 WHERE l.locktype = 'object' AND l.classid = 'pg_extension'::regclass
   AND l.objsubid = 19276 AND l.pid = pg_backend_pid();
ROLLBACK;

--------------------------------------------------------------------------
-- L11-04: the checks of ledger_register_table that no test could fail
--------------------------------------------------------------------------
-- A registration that is complete in every way but ownership: the table is made
-- by hand by one member of the owner role (the identity was issued, the
-- triggers are armed, capture last) and another member, who does not own it,
-- asks for it to be registered.
BEGIN; CREATE TABLE s_gap (id int); SELECT kingres.ledger_enable('s_gap') IS NULL AS enabled; ROLLBACK;
SELECT last_value AS gap_id FROM kingres.ledger_table_table_id_seq \gset
\c - kl_s_inh
SET search_path = public;
CREATE TABLE s_hand (id int);
INSERT INTO s_hand VALUES (1);
CREATE TRIGGER kingres_ledger_capture AFTER INSERT ON s_hand FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture(:'gap_id');
CREATE TRIGGER kingres_ledger_block_modify BEFORE UPDATE OR DELETE ON s_hand FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate BEFORE TRUNCATE ON s_hand FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();
ALTER TABLE s_hand ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE s_hand ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;
ALTER TABLE s_hand ENABLE ALWAYS TRIGGER kingres_ledger_capture;
-- An unlogged table, made the same way: the check of the persistence gives its own answer.
BEGIN;
CREATE UNLOGGED TABLE s_unl (id int);
CREATE TRIGGER kingres_ledger_capture AFTER INSERT ON s_unl FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture(:'gap_id');
ALTER TABLE s_unl ENABLE ALWAYS TRIGGER kingres_ledger_capture;
SELECT kingres.ledger_register_table('s_unl'::regclass);
ROLLBACK;
\c - kl_s_inh2
SET search_path = public;
SELECT kingres.ledger_register_table('s_hand'::regclass);
SELECT count(*) AS registered_by_the_other FROM kingres.ledger_table WHERE relid = 's_hand'::regclass;
\c - kl_s_inh
SET search_path = public;
SELECT kingres.ledger_register_table('s_hand'::regclass);
SELECT count(*) AS registered_by_the_owner, (SELECT count(*) FROM kingres.ledger_entry WHERE table_id = :gap_id) AS entries
  FROM kingres.ledger_table WHERE relid = 's_hand'::regclass;
\c - :owner_name
SET search_path = public;
-- The row that the table already held was entered by the registration, as
-- ledger_enable enters it, and a row inserted afterwards is captured at once.
INSERT INTO s_hand VALUES (2);
SELECT count(*) AS entries_after FROM kingres.ledger_entry WHERE table_id = :gap_id;
SELECT kingres.ledger_build_block() IS NOT NULL AS built;
SELECT count(*) AS findings FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- A write by hand leaves a record; the writes of the extension do not, whatever
-- the setting says (M22)
--------------------------------------------------------------------------
SET kingres_ledger.allow_manual_write = on;
INSERT INTO docs VALUES (10, 'under the setting');
SELECT kingres.ledger_build_block() IS NOT NULL AS built_under_the_setting;
SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest_under_the_setting;
SELECT kingres.ledger_verify_recorded() > 0 AS recorded_under_the_setting;
RESET kingres_ledger.allow_manual_write;
SELECT count(*) AS manual_records FROM kingres.ledger_removed WHERE action = 'inserted';
SELECT count(*) AS findings FROM kingres.ledger_verify();
