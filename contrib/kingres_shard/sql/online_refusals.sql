-- sql/online_refusals.sql
--   Increment 9.3 of phase 9 (design/shard-phase9.md, "The rebuild engine"): what the online variants refuse
--   before they touch anything, with the text of each refusal.  The operations themselves need several nodes
--   and run in the TAP tests (t/144 to t/149); here nothing is recorded, nothing is created.
--
-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
\set SHOW_CONTEXT never
SET search_path = public;
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('localhost', current_setting('port')::int);

CREATE TABLE nokey (a int, b text);
CREATE TABLE haskey (a int PRIMARY KEY, b text);
CREATE TABLE partkey (a int, b int, PRIMARY KEY (a, b));
CREATE FUNCTION trgf() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TABLE withtrigger (a int PRIMARY KEY);
CREATE TRIGGER t1 BEFORE INSERT ON withtrigger FOR EACH ROW EXECUTE FUNCTION trgf();
INSERT INTO haskey VALUES (1, 'x');

-- no key to follow the changes by: only the blocking variant
SELECT kingres.create_distributed_table_concurrently('nokey', 'a');
SELECT kingres.create_reference_table_concurrently('nokey');
-- not inside a transaction block (it commits in sessions of its own)
BEGIN;
SELECT kingres.create_distributed_table_concurrently('haskey', 'a');
ROLLBACK;
BEGIN;
SELECT kingres.create_reference_table_concurrently('haskey');
ROLLBACK;
BEGIN;
SELECT kingres.begin_table_operation('distribute', 'haskey', '{"column": "a"}');
ROLLBACK;
-- the report's blockers, all of them, before anything is recorded
SELECT kingres.begin_table_operation('distribute', 'withtrigger', '{"column": "a"}');
SELECT kingres.begin_table_operation('distribute', 'haskey', '{"column": "nope"}');
SELECT kingres.begin_table_operation('distribute', 'nokey', '{"column": "nope"}');
-- operations that this version does not have
SELECT kingres.begin_table_operation('undistribute', 'haskey', '{}');
SELECT kingres.begin_table_operation('alter', 'haskey', '{}');
SELECT kingres.begin_table_operation('nonsense', 'haskey', '{}');
-- an operation that does not exist, a phase that does not exist
SELECT kingres.table_operation_advance(999, 'copying');
SELECT kingres.table_operation_advance(999, 'nonsense');
SELECT kingres.cancel_table_operation(999);
-- nothing was recorded, nothing is claimed
SELECT count(*) AS operations FROM kingres.table_operation;
SELECT count(*) AS claims FROM kingres.table_operation_lock;
SELECT count(*) AS colocation_groups FROM kingres.colocation;
-- resume and cleanup have nothing to do
SELECT kingres.cleanup_table_operations();
-- a role that is not a superuser cannot call any of them
CREATE ROLE ks_online_user;
SET ROLE ks_online_user;
SELECT kingres.create_distributed_table_concurrently('haskey', 'a');
SELECT kingres.begin_table_operation('distribute', 'haskey', '{"column": "a"}');
SELECT kingres.table_operation_advance(1, 'copying');
SELECT kingres.cancel_table_operation(1);
SELECT kingres.resume_table_operations();
SELECT kingres.cleanup_table_operations();
SELECT * FROM kingres.table_operation;
RESET ROLE;
-- the capture trigger function cannot be made a trigger by an ordinary role
GRANT ALL ON haskey TO ks_online_user;
SET ROLE ks_online_user;
CREATE TRIGGER cap AFTER INSERT ON haskey FOR EACH ROW EXECUTE FUNCTION kingres.internal_capture_trigger('x', 'x', 'x');
RESET ROLE;
-- and the function refuses a log that is not one of the extension's, whoever made the trigger
CREATE TRIGGER cap AFTER INSERT ON haskey FOR EACH ROW EXECUTE FUNCTION kingres.internal_capture_trigger('public.haskey; DROP TABLE haskey', 'a', 'x');
INSERT INTO haskey VALUES (2, 'y');
DROP TRIGGER cap ON haskey;
SELECT count(*) AS rows_left FROM haskey;

DROP OWNED BY ks_online_user;
DROP ROLE ks_online_user;
DROP TABLE nokey, haskey, partkey, withtrigger;
DROP FUNCTION trgf();
DROP EXTENSION kingres_shard;
