-- ledger_stray: a row above the head (L8-08) and a digest row with another
-- hash (L9-02).  A ledger_tx row or a ledger_digest row that names a block
-- which does not exist, or a digest that names another hash than its block
-- has: who can write one (nobody but a superuser who asks), what ledger_head
-- and ledger_verify say, that the honest write path never leaves one, and the
-- way out of the README ("A row above the head"): ledger_remove_stray, which
-- keeps what it removes.  The statements of the README itself are run by
-- t/015_recipe.pl.  Works in databases of its own so that the chain is
-- known.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
SET search_path = public;
\set VERBOSITY terse
SELECT current_database() AS orig_db \gset
DROP DATABASE IF EXISTS kl_stray_t;
DROP DATABASE IF EXISTS kl_stray_e;
DROP ROLE IF EXISTS kl_stray_plain;
DROP ROLE IF EXISTS kl_stray_bulk;
DROP ROLE IF EXISTS kl_stray_svc;
CREATE ROLE kl_stray_plain;
CREATE ROLE kl_stray_bulk IN ROLE pg_write_all_data;
CREATE ROLE kl_stray_svc;
CREATE DATABASE kl_stray_t;
\c kl_stray_t
SET search_path = public;
\set VERBOSITY terse
CREATE EXTENSION kingres_ledger;
CREATE TABLE docs (id int PRIMARY KEY, body text);
SELECT kingres.ledger_enable('docs');
INSERT INTO docs VALUES (1, 'a'); SELECT kingres.ledger_build_block() AS built;
INSERT INTO docs VALUES (2, 'b'); SELECT kingres.ledger_build_block() AS built;
INSERT INTO docs VALUES (3, 'c'); SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest;
GRANT USAGE ON SCHEMA kingres TO kl_stray_plain, kl_stray_bulk, kl_stray_svc;
GRANT EXECUTE ON FUNCTION kingres.ledger_head() TO kl_stray_svc;
SELECT height FROM kingres.ledger_head();
SELECT count(*) AS verify_findings FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- Who can write such a row.  A role with privileges on the table cannot be
-- made (the hook refuses the grant); a role without them cannot; a member of
-- pg_write_all_data bypasses table privileges, and the guard refuses it.
--------------------------------------------------------------------------
SET ROLE kl_stray_plain;
INSERT INTO kingres.ledger_tx VALUES (900000001, 77, 0, 1, sha256('x'), 'x');
RESET ROLE;
GRANT INSERT ON kingres.ledger_tx TO kl_stray_plain;
SET ROLE kl_stray_bulk;
INSERT INTO kingres.ledger_tx VALUES (900000001, 77, 0, 1, sha256('x'), 'x');
INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
VALUES (now(), 'x', 88, sha256('y'), '{}');
RESET ROLE;
SELECT count(*) AS rows_planted FROM kingres.ledger_tx WHERE ledger_txid = 900000001;

-- A superuser who asks for it.
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_tx VALUES (900000001, 77, 0, 1, sha256('x'), 'x');
RESET kingres_ledger.allow_manual_write;

--------------------------------------------------------------------------
-- The error names the row and the table; ledger_verify says the same.
--------------------------------------------------------------------------
SELECT * FROM kingres.ledger_head();
SET ROLE kl_stray_svc;
SELECT * FROM kingres.ledger_head();
RESET ROLE;
SELECT * FROM kingres.ledger_verify();
-- Nothing but the one finding; ledger_blocks_entries does not read the row.
SELECT count(*) AS entries FROM kingres.ledger_blocks_entries(0, 100);
-- No SQL removes it, for any role, unless a superuser asks.
DELETE FROM kingres.ledger_tx WHERE ledger_txid = 900000001;
TRUNCATE kingres.ledger_tx;
SET session_replication_role = replica;
DELETE FROM kingres.ledger_tx WHERE ledger_txid = 900000001;
RESET session_replication_role;
SET ROLE kl_stray_bulk;
DELETE FROM kingres.ledger_tx WHERE ledger_txid = 900000001;
RESET ROLE;

--------------------------------------------------------------------------
-- The way out: ledger_remove_stray, for a superuser who turned the setting on.
--------------------------------------------------------------------------
SELECT kingres.ledger_remove_stray('a row written by hand');
SET kingres_ledger.allow_manual_write = on;
SET ROLE kl_stray_bulk;
SELECT kingres.ledger_remove_stray('a member cannot');
RESET ROLE;
SELECT kingres.ledger_remove_stray('  ');
SELECT kingres.ledger_remove_stray('a row written by hand') AS removed;
RESET kingres_ledger.allow_manual_write;
SELECT height FROM kingres.ledger_head();
-- Nothing is left of the row but what the operator kept, and ledger_verify says so.
SELECT check_name, object, left(detail, 40) AS detail FROM kingres.ledger_verify();
SELECT removal_id, source_table, removed_by = session_user AS by_the_caller, reason, row_data ->> 'block_height' AS block_height
  FROM kingres.ledger_removed ORDER BY removal_id;
-- A row that has no entries is not queued again (a block would hold a transaction
-- of nothing).
SELECT count(*) AS requeued FROM kingres.ledger_pending WHERE ledger_txid = 900000001;
SET ROLE kl_stray_bulk;
DELETE FROM kingres.ledger_pending;
DELETE FROM kingres.ledger_removed;
RESET ROLE;
-- The record is as protected as the other tables.
UPDATE kingres.ledger_removed SET reason = 'x';
DELETE FROM kingres.ledger_removed;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_removed;
RESET kingres_ledger.allow_manual_write;

--------------------------------------------------------------------------
-- A digest row above the head, and a digest of a block that is there with
-- another hash (L9-02): ledger_head stops on the newest digest, ledger_verify
-- reports every digest that is wrong, with its id.
--------------------------------------------------------------------------
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
VALUES (now(), 'x', 88, sha256('y'), '{}');
RESET kingres_ledger.allow_manual_write;
SELECT * FROM kingres.ledger_head();
SELECT check_name, object FROM kingres.ledger_verify() WHERE check_name <> 'operator_removal';
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
VALUES (now(), 'x', 0, sha256('z'), '{}');
RESET kingres_ledger.allow_manual_write;
SELECT * FROM kingres.ledger_head();
SELECT check_name, object FROM kingres.ledger_verify() WHERE check_name <> 'operator_removal';
SET kingres_ledger.allow_manual_write = on;
SELECT kingres.ledger_remove_stray('digests that name no block or another hash') AS removed;
RESET kingres_ledger.allow_manual_write;
SELECT height FROM kingres.ledger_head();
SELECT check_name, object FROM kingres.ledger_verify() WHERE check_name <> 'operator_removal';
SELECT count(*) AS removal_records FROM kingres.ledger_removed;
SELECT count(*) AS verify_findings FROM kingres.ledger_verify();

-- The chain goes on, and the removed rows are in none of it.
INSERT INTO docs VALUES (4, 'd');
SELECT kingres.ledger_build_block() IS NOT NULL AS built;
SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest;
SELECT height FROM kingres.ledger_head();
SELECT check_name FROM kingres.ledger_verify() WHERE check_name <> 'operator_removal';

--------------------------------------------------------------------------
-- An empty chain with a row that names a block.
--------------------------------------------------------------------------
\c postgres
SET search_path = public;
CREATE DATABASE kl_stray_e;
\c kl_stray_e
SET search_path = public;
\set VERBOSITY terse
CREATE EXTENSION kingres_ledger;
SELECT count(*) AS head_rows FROM kingres.ledger_head();
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
VALUES (now(), 'x', 5, sha256('y'), '{}');
RESET kingres_ledger.allow_manual_write;
SELECT * FROM kingres.ledger_head();
SELECT * FROM kingres.ledger_verify();

\c :orig_db
SET search_path = public;
DROP DATABASE kl_stray_t;
DROP DATABASE kl_stray_e;
DROP ROLE kl_stray_plain;
DROP ROLE kl_stray_bulk;
DROP ROLE kl_stray_svc;
