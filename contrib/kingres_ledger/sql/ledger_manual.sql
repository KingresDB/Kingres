-- ledger_manual: what a superuser's hand leaves in the ledger, and what the
-- ledger says about it (L9-01, second half; L9-14).  A superuser who turned
-- allow_manual_write on can write a valid successor block with a time that
-- the clock never had; the rules that keep that time from becoming the time
-- of every later block:
--   * the builder refuses to build on a block that is ahead of its clock by
--     more than kingres_ledger.max_clock_skew (error KL004, nothing is lost);
--   * ledger_verify reports a block closed in the future whatever the setting;
--   * only a superuser can lift the limit;
--   * a block with no transaction and a transaction with no entry, which the
--     builder never writes, are reported.
-- And a pending row that is deleted by hand leaves entries that belong to no
-- block: ledger_verify reports them, for good.  Works in a database of its own.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
SET search_path = public;
\set VERBOSITY terse
SELECT current_database() AS orig_db \gset
DROP DATABASE IF EXISTS kl_manual_t;
DROP ROLE IF EXISTS kl_m_plain;
CREATE ROLE kl_m_plain;
CREATE DATABASE kl_manual_t;
\c kl_manual_t
SET search_path = public;
\set VERBOSITY terse
CREATE EXTENSION kingres_ledger;
CREATE TABLE docs (id int PRIMARY KEY, body text);
SELECT kingres.ledger_enable('docs');
INSERT INTO docs VALUES (1, 'a');
SELECT kingres.ledger_build_block() AS built;
SELECT count(*) AS findings FROM kingres.ledger_verify();
SHOW kingres_ledger.max_clock_skew;

-- A valid successor written by hand: right link, right hash, no transaction,
-- closed in the year 2100.  Without the setting even a superuser is refused.
SELECT height + 1 AS nh, block_hash AS ph FROM kingres.ledger_blocks ORDER BY height DESC LIMIT 1 \gset
SELECT '2100-01-01 00:00:00+00'::timestamptz AS nt \gset
SELECT sha256('') AS e0 \gset
SELECT kingres.ledger_block_hash(1::smallint, :nh, :'ph', :'e0', 0, :'nt') AS bh \gset
INSERT INTO kingres.ledger_blocks VALUES (:nh, 1, :'ph', :'e0', 0, :'nt', :'bh');
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_blocks VALUES (:nh, 1, :'ph', :'e0', 0, :'nt', :'bh');
RESET kingres_ledger.allow_manual_write;
-- ledger_head answers (the block is a valid head); ledger_verify says what it is.
SELECT height FROM kingres.ledger_head();
SELECT check_name, object, detail FROM kingres.ledger_verify() ORDER BY 1, 2;

-- An honest transaction, and the builder: refused, nothing lost.
INSERT INTO docs VALUES (2, 'b');
SELECT kingres.ledger_build_block();
SELECT count(*) AS pending_transactions FROM kingres.ledger_pending;
SELECT count(*) AS blocks FROM kingres.ledger_blocks;
-- The limit is the superuser's.
SET ROLE kl_m_plain;
SET kingres_ledger.max_clock_skew = 0;
RESET ROLE;
-- A superuser who has looked at the clock lifts the limit (0: none); the
-- builder goes on, and the new block carries the time of the one before.
SET kingres_ledger.max_clock_skew = 0;
SELECT kingres.ledger_build_block() IS NOT NULL AS built_without_a_limit;
RESET kingres_ledger.max_clock_skew;
SELECT b.closed_at = '2100-01-01 00:00:00+00'::timestamptz AS carries_the_time
  FROM kingres.ledger_blocks b ORDER BY b.height DESC LIMIT 1;
-- The report counts the blocks that carry the time, and is the same whatever
-- the limit is.
SELECT check_name, object, detail FROM kingres.ledger_verify() WHERE check_name = 'block_time';
SET kingres_ledger.max_clock_skew = 0;
SELECT check_name, object FROM kingres.ledger_verify() WHERE check_name = 'block_time';
RESET kingres_ledger.max_clock_skew;

--------------------------------------------------------------------------
-- A transaction with no entry, written by hand, and a pending row deleted by
-- hand (L9-14).
--------------------------------------------------------------------------
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_tx VALUES (900000001, 1, 5, 0, sha256(''), 'x');
RESET kingres_ledger.allow_manual_write;
SELECT check_name, object FROM kingres.ledger_verify() WHERE check_name IN ('tx_empty', 'block_tx_root');
INSERT INTO docs VALUES (3, 'c');
SELECT count(*) AS pending_before FROM kingres.ledger_pending;
-- A member of pg_write_all_data cannot delete it; a superuser with the setting can.
CREATE ROLE kl_m_bulk IN ROLE pg_write_all_data;
SET ROLE kl_m_bulk;
DELETE FROM kingres.ledger_pending;
RESET ROLE;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_pending;
RESET kingres_ledger.allow_manual_write;
-- The entries of the lost transaction belong to no block and to no pending
-- transaction: reported, for good.  The row is in the table; the ledger no
-- longer covers it by a block.
SELECT check_name, count(*) AS n FROM kingres.ledger_verify() WHERE check_name = 'entry_orphan' GROUP BY 1;

\c :orig_db
SET search_path = public;
DROP DATABASE kl_manual_t;
DROP ROLE kl_m_plain;
DROP ROLE kl_m_bulk;
