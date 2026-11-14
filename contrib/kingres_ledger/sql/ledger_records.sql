-- ledger_records: the records for operators: ledger_digest,
-- ledger_verification, ledger_verify_recorded and the view ledger_status;
-- their protection and privileges.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: PostgreSQL
SET search_path = public;
\set VERBOSITY terse

DROP ROLE IF EXISTS kl_r_agent;
DROP ROLE IF EXISTS kl_r_reader;
CREATE ROLE kl_r_agent;
CREATE ROLE kl_r_reader;
GRANT USAGE ON SCHEMA public TO kl_r_agent, kl_r_reader;

--------------------------------------------------------------------------
-- ledger_digest
--------------------------------------------------------------------------
SELECT count(*) AS digests_before FROM kingres.ledger_digest \gset
INSERT INTO docs VALUES (700, 'for a digest', NULL);
SELECT kingres.ledger_generate_digest() AS dig \gset
SELECT count(*) - :digests_before AS recorded FROM kingres.ledger_digest;
-- The record is exactly what was returned.
SELECT d.digest = :'dig'::jsonb AS digest_is_what_was_returned,
       d.height = (:'dig'::jsonb ->> 'height')::bigint AS height_matches,
       encode(d.block_hash, 'hex') = :'dig'::jsonb ->> 'block_hash' AS hash_matches,
       d.generated_by = current_user AS generated_by_matches,
       d.generated_at = (:'dig'::jsonb ->> 'generated_at')::timestamptz AS time_matches
FROM kingres.ledger_digest d ORDER BY d.digest_id DESC LIMIT 1;
-- A digest without new transactions is recorded again.
SELECT kingres.ledger_generate_digest() AS dig2 \gset
SELECT count(*) - :digests_before AS recorded_after_two FROM kingres.ledger_digest;
SELECT count(DISTINCT digest_id) = count(*) AS ids_are_unique,
       kingres.ledger_verify_digest(:'dig'::jsonb) AS the_recorded_digest_verifies,
       kingres.ledger_verify_digest((SELECT digest FROM kingres.ledger_digest ORDER BY digest_id DESC LIMIT 1)) AS the_latest_record_verifies
FROM kingres.ledger_digest;
-- Another role generates one (it needs EXECUTE)
GRANT EXECUTE ON FUNCTION kingres.ledger_generate_digest() TO kl_r_agent;
SET ROLE kl_r_agent;
SELECT kingres.ledger_generate_digest() IS NOT NULL AS agent_generated;
RESET ROLE;
SELECT generated_by FROM kingres.ledger_digest ORDER BY digest_id DESC LIMIT 1;

--------------------------------------------------------------------------
-- ledger_verification and ledger_verify_recorded
--------------------------------------------------------------------------
SELECT count(*) AS runs_before FROM kingres.ledger_verification \gset
SELECT kingres.ledger_verify_recorded() AS vid \gset
SELECT state, problem_count, problems IS NULL AS no_problems, started_by = current_user AS by_caller,
       finished_at >= started_at AS finished, from_height, to_height = (SELECT max(height) FROM kingres.ledger_blocks) AS to_the_tip,
       blocks_checked = (SELECT count(*) FROM kingres.ledger_blocks) AS all_blocks,
       rows_checked = (SELECT sum(entry_count) FROM kingres.ledger_tx) AS all_entries
FROM kingres.ledger_verification WHERE verification_id = :vid;
SELECT count(*) - :runs_before AS runs_recorded FROM kingres.ledger_verification;
-- a range
SELECT kingres.ledger_verify_recorded(1, 2) AS vid \gset
SELECT state, from_height, to_height, blocks_checked FROM kingres.ledger_verification WHERE verification_id = :vid;
-- ledger_verify itself records nothing
SELECT count(*) AS before FROM kingres.ledger_verification \gset
SELECT count(*) AS problems FROM kingres.ledger_verify();
SELECT count(*) - :before AS recorded_by_ledger_verify FROM kingres.ledger_verification;
-- invalid range: an error and no record
SELECT kingres.ledger_verify_recorded(5, 2);
SELECT kingres.ledger_verify_recorded(-1);
SELECT count(*) - :before AS recorded_by_invalid_calls FROM kingres.ledger_verification;
-- privileges
SET ROLE kl_r_reader;
SELECT kingres.ledger_verify_recorded();
RESET ROLE;
GRANT EXECUTE ON FUNCTION kingres.ledger_verify_recorded(bigint, bigint) TO kl_r_agent;
SET ROLE kl_r_agent;
SELECT kingres.ledger_verify_recorded() AS vid \gset
RESET ROLE;
SELECT started_by, state FROM kingres.ledger_verification WHERE verification_id = :vid;

--------------------------------------------------------------------------
-- Problems are recorded, and capped
--------------------------------------------------------------------------
\set trig_off 'UPDATE pg_trigger SET tgenabled = ''D'' WHERE tgname LIKE ''kingres_ledger_%'' AND tgrelid IN (SELECT oid FROM pg_class WHERE relname IN (''docs'', ''ledger_digest''))'
\set trig_on 'UPDATE pg_trigger SET tgenabled = ''A'' WHERE tgname LIKE ''kingres_ledger_%'' AND tgrelid IN (SELECT oid FROM pg_class WHERE relname IN (''docs'', ''ledger_digest''))'
:trig_off;
\c
SET search_path = public;
-- 120 rows without entries: 120 problems, 100 of them kept
INSERT INTO docs SELECT 1000 + g, 'smuggled', NULL FROM generate_series(1, 120) g;
SELECT kingres.ledger_verify_recorded() AS vid \gset
:trig_on;
\c
SET search_path = public;
SELECT state, problem_count, jsonb_array_length(problems) AS kept, problems -> 0 ->> 'check' AS first_check,
       (problems -> 0 ->> 'object') AS first_object,
       finished_at IS NOT NULL AS finished
FROM kingres.ledger_verification WHERE verification_id = :vid;
:trig_off;
\c
SET search_path = public;
DELETE FROM docs WHERE id >= 1000;
SELECT count(*) AS problems_after_cleanup FROM kingres.ledger_verify();
:trig_on;
\c
SET search_path = public;
SELECT count(*) AS problems_after_restore FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- ledger_status
--------------------------------------------------------------------------
SELECT count(*) AS rows FROM kingres.ledger_status;
SELECT append_only_tables = (SELECT count(*) FROM kingres.ledger_table WHERE mode = 'append_only') AS append_only_count,
       updatable_tables = (SELECT count(*) FROM kingres.ledger_table WHERE mode = 'updatable') AS updatable_count,
       latest_block_height = (SELECT max(height) FROM kingres.ledger_blocks) AS latest_block,
       latest_block_hash = (SELECT block_hash FROM kingres.ledger_blocks ORDER BY height DESC LIMIT 1) AS latest_hash,
       pending_transactions, oldest_pending_age IS NULL AS nothing_waiting,
       last_digest_id = (SELECT max(digest_id) FROM kingres.ledger_digest) AS last_digest,
       last_verification_id = (SELECT max(verification_id) FROM kingres.ledger_verification) AS last_verification,
       last_verification_state, last_verification_problems
FROM kingres.ledger_status;
INSERT INTO docs VALUES (701, 'pending', NULL);
SELECT pg_sleep(0.05);
SELECT pending_transactions, oldest_pending_age > interval '0' AS age_is_positive,
       oldest_pending_age < interval '1 minute' AS age_is_small FROM kingres.ledger_status;
SELECT kingres.ledger_build_block() IS NOT NULL AS built;
SELECT pending_transactions, oldest_pending_age FROM kingres.ledger_status;
-- not readable by PUBLIC (it shows how active the database is); the grant works
SET ROLE kl_r_reader;
SELECT count(*) FROM kingres.ledger_status;
RESET ROLE;
GRANT SELECT ON kingres.ledger_status TO kl_r_reader;
SET ROLE kl_r_reader;
SELECT count(*) AS reader_sees_the_status FROM kingres.ledger_status;
SELECT count(*) > 0 AS reader_reads_digests FROM kingres.ledger_digest;
SELECT count(*) > 0 AS reader_reads_verifications FROM kingres.ledger_verification;
RESET ROLE;

--------------------------------------------------------------------------
-- Protection of the new tables and of the view
--------------------------------------------------------------------------
\set VERBOSITY sqlstate
-- append-only for every role, the owner included
UPDATE kingres.ledger_digest SET height = 0;
DELETE FROM kingres.ledger_digest;
TRUNCATE kingres.ledger_digest;
UPDATE kingres.ledger_verification SET state = 'clean';
DELETE FROM kingres.ledger_verification;
TRUNCATE kingres.ledger_verification;
-- no role can write them through SQL
GRANT INSERT ON kingres.ledger_digest TO kl_r_agent;
GRANT UPDATE ON kingres.ledger_verification TO kl_r_agent;
GRANT ALL ON kingres.ledger_digest TO kl_r_agent;
REVOKE ALL ON kingres.ledger_verification FROM CURRENT_USER;
GRANT SELECT ON kingres.ledger_digest TO kl_r_agent;
SET ROLE kl_r_agent;
INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
VALUES (now(), 'x', 0, sha256('x'), '{}');
RESET ROLE;
-- DDL
DROP TABLE kingres.ledger_digest;
DROP TABLE kingres.ledger_verification CASCADE;
ALTER TABLE kingres.ledger_digest ADD COLUMN x int;
ALTER TABLE kingres.ledger_verification DROP COLUMN problems;
ALTER TABLE kingres.ledger_digest RENAME COLUMN digest TO d;
ALTER TABLE kingres.ledger_verification DISABLE TRIGGER ALL;
ALTER TABLE kingres.ledger_digest DISABLE TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_digest SET SCHEMA public;
CREATE INDEX ON kingres.ledger_digest (height);
CREATE TRIGGER x AFTER INSERT ON kingres.ledger_digest FOR EACH ROW EXECUTE FUNCTION trg_after('x');
CREATE RULE r AS ON UPDATE TO kingres.ledger_digest DO INSTEAD NOTHING;
CREATE POLICY p ON kingres.ledger_digest USING (true);
-- the view and the function
DROP VIEW kingres.ledger_status;
CREATE OR REPLACE VIEW kingres.ledger_status AS SELECT 1 AS x;
ALTER VIEW kingres.ledger_status RENAME TO other;
ALTER TABLE kingres.ledger_status ADD COLUMN y int;
ALTER VIEW kingres.ledger_status SET SCHEMA public;
GRANT UPDATE ON kingres.ledger_status TO kl_r_agent;
CREATE OR REPLACE FUNCTION kingres.ledger_verify_recorded(from_height bigint DEFAULT 0, to_height bigint DEFAULT NULL)
RETURNS bigint LANGUAGE sql AS 'SELECT 1::bigint';
ALTER FUNCTION kingres.ledger_verify_recorded(bigint, bigint) SECURITY INVOKER;
ALTER FUNCTION kingres.ledger_verify_recorded(bigint, bigint) RENAME TO x;
DROP FUNCTION kingres.ledger_verify_recorded(bigint, bigint);
\set VERBOSITY terse
SELECT count(*) AS problems FROM kingres.ledger_verify();

-- The record of a verification that found problems stays: it is append-only too
SELECT count(*) > 0 AS the_problem_record_is_still_there FROM kingres.ledger_verification WHERE state = 'problems';
