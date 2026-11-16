-- ledger_stray: a row above the head (L8-08).  A ledger_tx row or a ledger_digest
-- row that names a block which does not exist: who can write one, what
-- ledger_head and ledger_verify say, that the honest write path never leaves
-- one, and the recovery of the README ("A row above the head").  Works in
-- databases of its own so that the chain is known.
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
-- pg_write_all_data can, because that role bypasses table privileges.
--------------------------------------------------------------------------
SET ROLE kl_stray_plain;
INSERT INTO kingres.ledger_tx VALUES (900000001, 77, 0, 1, sha256('x'), 'x');
RESET ROLE;
GRANT INSERT ON kingres.ledger_tx TO kl_stray_plain;

SET ROLE kl_stray_bulk;
INSERT INTO kingres.ledger_tx VALUES (900000001, 77, 0, 1, sha256('x'), 'x');
RESET ROLE;

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
-- No SQL removes it, for any role.
DELETE FROM kingres.ledger_tx WHERE ledger_txid = 900000001;
TRUNCATE kingres.ledger_tx;
SET session_replication_role = replica;
DELETE FROM kingres.ledger_tx WHERE ledger_txid = 900000001;
RESET session_replication_role;

--------------------------------------------------------------------------
-- The recovery of the README: switch the delete trigger of the table off in
-- the catalog (a new session sees it), remove the row, switch it on.
--------------------------------------------------------------------------
UPDATE pg_trigger SET tgenabled = 'D'
 WHERE tgrelid = 'kingres.ledger_tx'::regclass AND tgname = 'kingres_ledger_block_modify';
\c
SET search_path = public;
DELETE FROM kingres.ledger_tx
 WHERE block_height > coalesce((SELECT max(b.height) FROM kingres.ledger_blocks b), -1);
UPDATE pg_trigger SET tgenabled = 'A'
 WHERE tgrelid = 'kingres.ledger_tx'::regclass AND tgname = 'kingres_ledger_block_modify';
\c
SET search_path = public;
SELECT height FROM kingres.ledger_head();
SELECT count(*) AS verify_findings FROM kingres.ledger_verify();
DELETE FROM kingres.ledger_tx;

--------------------------------------------------------------------------
-- A digest row above the head.
--------------------------------------------------------------------------
SET ROLE kl_stray_bulk;
INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
VALUES (now(), 'x', 88, sha256('y'), '{}');
RESET ROLE;
SELECT * FROM kingres.ledger_head();
SELECT * FROM kingres.ledger_verify();
-- Recovery of a digest row: the same, on its own table.
UPDATE pg_trigger SET tgenabled = 'D'
 WHERE tgrelid = 'kingres.ledger_digest'::regclass AND tgname = 'kingres_ledger_block_modify';
\c
SET search_path = public;
DELETE FROM kingres.ledger_digest
 WHERE height > coalesce((SELECT max(b.height) FROM kingres.ledger_blocks b), -1);
UPDATE pg_trigger SET tgenabled = 'A'
 WHERE tgrelid = 'kingres.ledger_digest'::regclass AND tgname = 'kingres_ledger_block_modify';
\c
SET search_path = public;
SELECT height FROM kingres.ledger_head();
SELECT count(*) AS verify_findings FROM kingres.ledger_verify();

-- A digest of a block that is there, with another hash than the block has
-- (a chain rewritten after the digest): ledger_head stops on the newest digest;
-- ledger_verify does not compare digests, ledger_verify_digest does.
SET ROLE kl_stray_bulk;
INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
VALUES (now(), 'x', 0, sha256('z'), '{}');
RESET ROLE;
SELECT * FROM kingres.ledger_head();
SELECT count(*) AS verify_findings FROM kingres.ledger_verify();

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
SET ROLE kl_stray_bulk;
INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
VALUES (now(), 'x', 5, sha256('y'), '{}');
RESET ROLE;
SELECT * FROM kingres.ledger_head();
SELECT * FROM kingres.ledger_verify();

\c :orig_db
SET search_path = public;
DROP DATABASE kl_stray_t;
DROP DATABASE kl_stray_e;
DROP ROLE kl_stray_plain;
DROP ROLE kl_stray_bulk;
DROP ROLE kl_stray_svc;
