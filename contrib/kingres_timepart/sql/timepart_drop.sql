-- timepart_drop: what DROP EXTENSION removes and what it leaves.  It removes
-- the event triggers, the tables with the policies and the history, and the
-- functions; it leaves the schema kingres (the extension did not create it as
-- one of its members, so that the extensions that share it do not remove it
-- from each other) and every partition and table of the users.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only

SET timezone = 'UTC';
SET datestyle = ISO;
SET intervalstyle = postgres;
\set VERBOSITY default
\set SHOW_CONTEXT never

SELECT (SELECT count(*) FROM kingres.timepart_policy) > 0 AS had_policies,
       (SELECT count(*) FROM kingres.timepart_run) > 0 AS had_history;
CREATE TABLE tp.dropme (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('tp.dropme', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');

-- what the first installation was, to see that the next one is another
CREATE TEMP TABLE before_drop AS
SELECT kingres.timepart__stamp() AS stamp,
       (SELECT max(run_id) FROM kingres.timepart_run) AS max_run;

SET client_min_messages = warning;
DROP EXTENSION kingres_timepart;
RESET client_min_messages;

SELECT count(*) AS event_triggers FROM pg_event_trigger WHERE evtname LIKE 'kingres_timepart%';
SELECT count(*) AS tables_left FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relname LIKE 'timepart%';
SELECT count(*) AS functions_left FROM pg_proc WHERE pronamespace = 'kingres'::regnamespace AND proname LIKE '%timepart%';
SELECT count(*) AS functions_left FROM pg_proc WHERE pronamespace = 'kingres'::regnamespace AND proname IN ('create_time_partitions', 'drop_old_time_partitions', 'convert_partition');
SELECT to_regnamespace('kingres') IS NOT NULL AS schema_left;
SELECT count(*) AS partitions_kept FROM pg_inherits WHERE inhparent = 'tp.dropme'::regclass;

-- A new install starts empty and works on the tables that are still there.
CREATE EXTENSION kingres_timepart;
SELECT count(*) AS policies FROM kingres.timepart_policy;
SELECT kingres.timepart_set_policy('tp.dropme', '1 day', create_ahead => '1 day');
\o /dev/null
CALL kingres.timepart_run('tp.dropme', '2026-03-03 UTC');
\o
SELECT state FROM kingres.timepart_run;
-- the history and the policies of the first installation are gone with it:
-- the numbering starts again, and the installation has a stamp of its own
SELECT kingres.timepart__stamp() <> (SELECT stamp FROM before_drop) AS new_stamp,
       (SELECT max(run_id) FROM kingres.timepart_run) < (SELECT max_run FROM before_drop) AS numbering_restarted;
SELECT count(*) AS runs, count(*) FILTER (WHERE table_oid IS NOT NULL) AS runs_of_a_live_table FROM kingres.timepart_run;

-- CREATE EXTENSION where something of the user's already has the name of an
-- object of the extension, in the schema kingres: it ends in an error that
-- says which, and changes nothing (no extension, no other object of ours, no
-- event trigger, and what the user made is as it was).
DROP TABLE before_drop;
SET client_min_messages = warning;
DROP EXTENSION kingres_timepart;
RESET client_min_messages;

CREATE TABLE kingres.timepart_policy_data (x int);
INSERT INTO kingres.timepart_policy_data VALUES (42);
CREATE EXTENSION kingres_timepart;
SELECT count(*) AS extensions FROM pg_extension WHERE extname = 'kingres_timepart';
SELECT relname, relkind FROM pg_class WHERE relnamespace = 'kingres'::regnamespace ORDER BY 1;
SELECT count(*) AS event_triggers FROM pg_event_trigger WHERE evtname LIKE 'kingres_timepart%';
SELECT count(*) AS functions FROM pg_proc WHERE pronamespace = 'kingres'::regnamespace;
SELECT x FROM kingres.timepart_policy_data;
DROP TABLE kingres.timepart_policy_data;

-- a view, an index, a type of the user's
CREATE VIEW kingres.timepart_run AS SELECT 1 AS x;
CREATE EXTENSION kingres_timepart;
DROP VIEW kingres.timepart_run;
CREATE TABLE kingres.user_table (x int);
CREATE INDEX timepart_run_table_idx ON kingres.user_table (x);
CREATE EXTENSION kingres_timepart;
DROP TABLE kingres.user_table;
CREATE TYPE kingres.timepart_run_action_data AS (x int);
CREATE EXTENSION kingres_timepart;
DROP TYPE kingres.timepart_run_action_data;
-- A sequence of the user's with the name of the one behind run_id is no clash:
-- PostgreSQL names the sequence of the extension differently, and the dump
-- carries the one that belongs to the table.
CREATE SEQUENCE kingres.timepart_run_data_run_id_seq;
CREATE EXTENSION kingres_timepart;
SELECT pg_get_serial_sequence('kingres.timepart_run_data', 'run_id') AS sequence_of_the_extension;
SELECT count(*) AS config_sequences FROM pg_extension e, unnest(e.extconfig) AS t(oid), pg_class c
 WHERE e.extname = 'kingres_timepart' AND c.oid = t.oid AND c.relkind = 'S' AND c.relname = 'timepart_run_data_run_id_seq1';
SET client_min_messages = warning;
DROP EXTENSION kingres_timepart;
RESET client_min_messages;
SELECT count(*) AS user_sequence_left FROM pg_class WHERE relnamespace = 'kingres'::regnamespace AND relkind = 'S';
DROP SEQUENCE kingres.timepart_run_data_run_id_seq;
-- a function with the signature of one of ours; with another signature it is
-- an overload and stays
CREATE FUNCTION kingres.timepart_due(timestamptz) RETURNS int LANGUAGE sql AS 'SELECT 1';
CREATE EXTENSION kingres_timepart;
DROP FUNCTION kingres.timepart_due(timestamptz);
SELECT count(*) AS extensions FROM pg_extension WHERE extname = 'kingres_timepart';
SELECT count(*) AS objects_in_the_schema FROM pg_class WHERE relnamespace = 'kingres'::regnamespace;
SELECT count(*) AS functions_in_the_schema FROM pg_proc WHERE pronamespace = 'kingres'::regnamespace;
CREATE FUNCTION kingres.timepart_due(text) RETURNS int LANGUAGE sql AS 'SELECT 1';
CREATE EXTENSION kingres_timepart;
SELECT count(*) AS timepart_due_overloads FROM pg_proc WHERE pronamespace = 'kingres'::regnamespace AND proname = 'timepart_due';
SELECT kingres.timepart_due('x') AS user_function_works;
SET client_min_messages = warning;
DROP EXTENSION kingres_timepart;
RESET client_min_messages;
SELECT count(*) AS user_function_left FROM pg_proc WHERE pronamespace = 'kingres'::regnamespace;
DROP FUNCTION kingres.timepart_due(text);

-- and a clean install after all of that
CREATE EXTENSION kingres_timepart;
SELECT count(*) AS policies, (SELECT count(*) FROM kingres.timepart_run) AS runs FROM kingres.timepart_policy;
SELECT count(*) AS event_triggers FROM pg_event_trigger WHERE evtname LIKE 'kingres_timepart%' AND evtenabled = 'A';
SELECT kingres.timepart_set_policy('tp.dropme', '1 day', create_ahead => '1 day');
\o /dev/null
CALL kingres.timepart_run('tp.dropme', '2026-03-03 UTC');
\o
SELECT state FROM kingres.timepart_run;
