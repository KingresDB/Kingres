-- ledger_head: kingres.ledger_head and kingres.ledger_blocks_entries, the
-- reads of the chain for tile publication: an empty ledger, one block, many,
-- the bounds of a page, malformed stored blocks (XX001), privileges, and that
-- neither builds a block nor writes anything.  Works in a database of its own
-- so that the chain is known and can be empty.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
SET search_path = public;
\set VERBOSITY terse
SELECT current_database() AS orig_db \gset
DROP DATABASE IF EXISTS kl_head_t;
CREATE DATABASE kl_head_t;
\c kl_head_t
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
\set VERBOSITY terse
CREATE EXTENSION kingres_ledger;

--------------------------------------------------------------------------
-- An empty ledger
--------------------------------------------------------------------------
SELECT count(*) AS head_rows FROM kingres.ledger_head();
SELECT count(*) AS entries_rows FROM kingres.ledger_blocks_entries(0, 10);
SELECT count(*) AS entries_rows_far FROM kingres.ledger_blocks_entries(1000, 10);

-- Pending transactions are not blocks: the functions build nothing.
CREATE TABLE docs (id int PRIMARY KEY, body text);
SELECT kingres.ledger_enable('docs');
INSERT INTO docs VALUES (1, 'pending');
BEGIN READ ONLY;
SELECT count(*) AS head_rows_with_pending FROM kingres.ledger_head();
SELECT count(*) AS entries_rows_with_pending FROM kingres.ledger_blocks_entries(0, 10);
COMMIT;
SELECT (SELECT count(*) FROM kingres.ledger_pending) AS pending,
       (SELECT count(*) FROM kingres.ledger_blocks) AS blocks,
       (SELECT count(*) FROM kingres.ledger_digest) AS digests;

--------------------------------------------------------------------------
-- One block, then five
--------------------------------------------------------------------------
SELECT kingres.ledger_build_block() AS built;
SELECT h.height, octet_length(h.block_hash) AS hash_len,
       h.chain_id = (SELECT c.chain_id FROM kingres.ledger_chain c) AS chain_ok,
       h.block_hash = (SELECT b.block_hash FROM kingres.ledger_blocks b WHERE b.height = h.height) AS hash_ok,
       h.closed_at_us = kingres.ledger_timestamptz_us((SELECT b.closed_at FROM kingres.ledger_blocks b WHERE b.height = h.height)) AS time_ok
FROM kingres.ledger_head() h;
SELECT e.height, octet_length(e.entry) AS len,
       sha256(e.entry) = (SELECT b.block_hash FROM kingres.ledger_blocks b WHERE b.height = e.height) AS hashes_to_block
FROM kingres.ledger_blocks_entries(0, 10) e ORDER BY e.height;

INSERT INTO docs VALUES (2, 'b'); SELECT kingres.ledger_build_block() AS built;
INSERT INTO docs VALUES (3, 'c'); SELECT kingres.ledger_build_block() AS built;
INSERT INTO docs VALUES (4, 'd'); SELECT kingres.ledger_build_block() AS built;
INSERT INTO docs VALUES (5, 'e'); SELECT kingres.ledger_build_block() AS built;
SELECT height FROM kingres.ledger_head();
-- Nothing pending: the head is the same on every call and nothing is recorded.
SELECT (SELECT count(*) FROM kingres.ledger_digest) AS digests,
       (SELECT count(*) FROM kingres.ledger_blocks) AS blocks;

--------------------------------------------------------------------------
-- Pages: the bounds
--------------------------------------------------------------------------
-- The text of the error of ledger_head with the transaction id (the id of a real
-- transaction, not the same on every run) replaced.
CREATE FUNCTION public.head_err() RETURNS text LANGUAGE plpgsql AS $$
BEGIN
    PERFORM * FROM kingres.ledger_head();
    RETURN 'no error';
EXCEPTION WHEN OTHERS THEN
    RETURN regexp_replace(SQLERRM, 'transaction [0-9]+', 'transaction N');
END $$;
CREATE FUNCTION public.page(f bigint, m int) RETURNS text LANGUAGE sql AS $$
    SELECT coalesce(string_agg(e.height::text, ',' ORDER BY e.height), '(none)')
    FROM kingres.ledger_blocks_entries(f, m) e
$$;
SELECT public.page(0, 100) AS all_blocks,
       public.page(0, 1) AS first_block,
       public.page(0, 2) AS first_two,
       public.page(2, 2) AS middle,
       public.page(3, 100) AS from_3,
       public.page(4, 1) AS last_block,
       public.page(4, 100) AS last_block_big_limit,
       public.page(5, 10) AS one_past_head,
       public.page(9223372036854775807, 10) AS far_past_head,
       public.page(0, 0) AS zero_count,
       public.page(2147483646, 0) AS zero_count_far,
       public.page(0, 2147483647) AS huge_count;
-- Pages join without a gap or overlap.
SELECT public.page(0, 2) || ' | ' || public.page(2, 2) || ' | ' || public.page(4, 2) AS pages;
-- The order is by height whatever the physical order of the table is.
SELECT array_agg(e.height) = ARRAY[0,1,2,3,4]::bigint[] AS in_order FROM kingres.ledger_blocks_entries(0, 10) e;
-- Negative arguments are errors; NULL gives nothing.
SELECT * FROM kingres.ledger_blocks_entries(-1, 5);
SELECT * FROM kingres.ledger_blocks_entries(0, -1);
SELECT * FROM kingres.ledger_blocks_entries(-9223372036854775807 - 1, 5);
SELECT * FROM kingres.ledger_blocks_entries(NULL, 5);
SELECT * FROM kingres.ledger_blocks_entries(0, NULL);
-- Entries are what ledger_block_preimage returns for the stored fields.
SELECT count(*) AS entries_equal_preimage
FROM kingres.ledger_blocks_entries(0, 10) e
JOIN kingres.ledger_blocks b ON b.height = e.height
WHERE e.entry = kingres.ledger_block_preimage(b.hash_version, b.height, b.prev_hash, b.tx_merkle_root, b.tx_count, b.closed_at);

--------------------------------------------------------------------------
-- Privileges
--------------------------------------------------------------------------
SELECT p.proname, p.prosecdef, p.provolatile,
       has_function_privilege('public', p.oid, 'execute') AS public_can_execute,
       p.proconfig
FROM pg_proc p
WHERE p.proname IN ('ledger_head', 'ledger_blocks_entries', 'ledger_block_preimage')
  AND p.pronamespace = 'kingres'::regnamespace
ORDER BY 1;
CREATE ROLE kl_head_nobody;
CREATE ROLE kl_head_service;
GRANT USAGE ON SCHEMA public TO kl_head_nobody, kl_head_service;
GRANT EXECUTE ON FUNCTION kingres.ledger_head(), kingres.ledger_blocks_entries(bigint, int) TO kl_head_service;
SET ROLE kl_head_nobody;
SELECT * FROM kingres.ledger_head();
SELECT * FROM kingres.ledger_blocks_entries(0, 1);
RESET ROLE;
SET ROLE kl_head_service;
-- The service role cannot read the tables that hold more than the head.
SELECT count(*) FROM kingres.ledger_entry;
SELECT height FROM kingres.ledger_head();
SELECT count(*) AS entries FROM kingres.ledger_blocks_entries(0, 10);
-- It can call nothing that writes.
SELECT kingres.ledger_build_block();
RESET ROLE;

--------------------------------------------------------------------------
-- Stored blocks that are not what they claim to be, with the triggers
-- bypassed as an attacker with catalog access would (see ledger_verify).
--------------------------------------------------------------------------
CREATE TABLE saved_blocks AS TABLE kingres.ledger_blocks;
CREATE TABLE saved_chain AS TABLE kingres.ledger_chain;
CREATE TABLE saved_tx AS TABLE kingres.ledger_tx;
CREATE FUNCTION public.restore_all() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    TRUNCATE kingres.ledger_blocks;
    INSERT INTO kingres.ledger_blocks SELECT * FROM public.saved_blocks;
    TRUNCATE kingres.ledger_chain;
    INSERT INTO kingres.ledger_chain SELECT * FROM public.saved_chain;
    TRUNCATE kingres.ledger_tx;
    INSERT INTO kingres.ledger_tx SELECT * FROM public.saved_tx;
    TRUNCATE kingres.ledger_digest;
END $$;
\set trig_off 'UPDATE pg_trigger SET tgenabled = ''D'' WHERE tgname IN (''kingres_ledger_capture'', ''kingres_ledger_block_modify'', ''kingres_ledger_block_truncate'')'
\set trig_on 'UPDATE pg_trigger SET tgenabled = ''A'' WHERE tgname IN (''kingres_ledger_capture'', ''kingres_ledger_block_modify'', ''kingres_ledger_block_truncate'')'

-- 1. A block whose transaction count was changed: its stored hash no longer matches.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_blocks SET tx_count = tx_count + 1 WHERE height = 2;
:trig_on;
\c
SET search_path = public;
SELECT public.page(0, 2) AS before_the_bad_block;
SELECT public.page(0, 3) AS reaching_the_bad_block;
SELECT public.page(2, 1) AS starting_at_the_bad_block;
SELECT public.page(3, 5) AS after_the_bad_block;
SELECT height FROM kingres.ledger_head();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;
SELECT public.page(0, 100) AS restored;

-- 2. A block whose stored hash was replaced: it fails, and so does the
-- block after it, whose prev_hash no longer matches.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_blocks SET block_hash = sha256('forged') WHERE height = 3;
:trig_on;
\c
SET search_path = public;
SELECT public.page(0, 3) AS before_the_bad_block;
SELECT public.page(3, 1) AS the_bad_block;
SELECT public.page(4, 1) AS the_block_after;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 3. A block that was deleted from the middle: a gap, found whichever side the page starts.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_blocks WHERE height = 2;
:trig_on;
\c
SET search_path = public;
SELECT public.page(0, 2) AS before_the_gap;
SELECT public.page(0, 4) AS across_the_gap;
SELECT public.page(3, 2) AS after_the_gap;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 4. A stored hash version that is not 1, and a stored root of the wrong length.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_blocks SET hash_version = 2 WHERE height = 1;
:trig_on;
\c
SET search_path = public;
SELECT public.page(1, 1) AS version_two;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
UPDATE kingres.ledger_blocks SET tx_merkle_root = '\x00' WHERE height = 4;
:trig_on;
\c
SET search_path = public;
SELECT public.page(4, 1) AS short_root;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 5. The first block no longer follows from the chain identity.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_chain SET chain_id = sha256('another chain');
:trig_on;
\c
SET search_path = public;
SELECT public.page(0, 1) AS first_block_other_chain;
SELECT public.page(1, 1) AS second_block_other_chain;
SELECT height FROM kingres.ledger_head();
-- 6. The chain identity is missing: an error, never an empty answer.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_chain;
:trig_on;
\c
SET search_path = public;
SELECT * FROM kingres.ledger_head();
SELECT public.page(0, 1) AS no_chain_identity;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;
SELECT public.page(0, 100) AS restored_again;
SELECT count(*) AS problems FROM kingres.ledger_verify();

-- 7. The end of the chain was removed from ledger_blocks and from nothing
-- else.  A read of the blocks alone cannot see it (entries stops at the new
-- end, as for any shorter chain), but the transactions of the removed blocks
-- are still recorded and ledger_head says so.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_blocks WHERE height = 4;
:trig_on;
\c
SET search_path = public;
SELECT public.page(0, 100) AS entries_after_losing_the_head;
SELECT public.head_err() AS head;
SELECT (SELECT count(*) FROM kingres.ledger_verify()) > 0 AS verify_sees_it;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_blocks WHERE height >= 2;
:trig_on;
\c
SET search_path = public;
SELECT public.head_err() AS head;
-- All the blocks removed: the head is not "empty", it is an error.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_blocks;
:trig_on;
\c
SET search_path = public;
SELECT public.head_err() AS head;
SELECT public.page(0, 100) AS entries_after_losing_everything;
-- What is not detected: the same removal together with the transaction rows
-- of the removed blocks, which someone who can write the ledger tables can
-- do.  The shorter chain is then a valid chain, and the answer is the new head.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
DELETE FROM kingres.ledger_blocks WHERE height = 4;
DELETE FROM kingres.ledger_tx WHERE block_height = 4;
:trig_on;
\c
SET search_path = public;
SELECT height FROM kingres.ledger_head();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 8. A digest that was recorded remembers the block it named.
SELECT (kingres.ledger_generate_digest() ->> 'height')::bigint AS digest_height;
SELECT height FROM kingres.ledger_head();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_blocks WHERE height = 4;
DELETE FROM kingres.ledger_tx WHERE block_height = 4;
:trig_on;
\c
SET search_path = public;
SELECT * FROM kingres.ledger_head();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;
SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest_again;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_digest SET block_hash = sha256('not the block');
:trig_on;
\c
SET search_path = public;
SELECT * FROM kingres.ledger_head();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;
SELECT height FROM kingres.ledger_head();

-- 9. What ledger_head checks of the head itself.
-- A head with a changed field: its hash no longer matches.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_blocks SET tx_count = tx_count + 1 WHERE height = 4;
:trig_on;
\c
SET search_path = public;
SELECT * FROM kingres.ledger_head();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
-- A head with the right hash for a link that is wrong.
DELETE FROM kingres.ledger_blocks WHERE height = 4;
INSERT INTO kingres.ledger_blocks
SELECT 4, 1, sha256('wrong link'), b.tx_merkle_root, b.tx_count, b.closed_at,
       sha256(kingres.ledger_block_preimage(1::smallint, 4, sha256('wrong link'), b.tx_merkle_root, b.tx_count, b.closed_at))
FROM public.saved_blocks b WHERE b.height = 4;
:trig_on;
\c
SET search_path = public;
SELECT * FROM kingres.ledger_head();
SELECT public.page(4, 1) AS wrong_link_through_entries;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
-- Block 0 gone while the head is above it.
DELETE FROM kingres.ledger_blocks WHERE height = 0;
:trig_on;
\c
SET search_path = public;
SELECT * FROM kingres.ledger_head();
SELECT public.page(1, 2) AS from_one_without_block_zero;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 10. The chain identity is checked on every call, not only from block 0.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_chain SET chain_id = sha256('another chain');
:trig_on;
\c
SET search_path = public;
SELECT public.page(3, 1) AS middle_block_other_chain;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 11. A height that is there twice (the primary key made non-unique, as
-- someone with catalog access could): the message names that block.
:trig_off;
\c
SET search_path = public;
UPDATE pg_index SET indisunique = false, indisprimary = false
 WHERE indexrelid = 'kingres.ledger_blocks_pkey'::regclass;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_blocks SELECT * FROM public.saved_blocks WHERE height = 3;
:trig_on;
\c
SET search_path = public;
SELECT public.page(2, 5) AS duplicated_height;
SELECT public.page(0, 100) AS duplicated_height_from_zero;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_blocks WHERE height = 3;
INSERT INTO kingres.ledger_blocks SELECT * FROM public.saved_blocks WHERE height = 3;
UPDATE pg_index SET indisunique = true, indisprimary = true
 WHERE indexrelid = 'kingres.ledger_blocks_pkey'::regclass;
:trig_on;
\c
SET search_path = public;
SELECT public.page(0, 100) AS restored_after_duplicate;

-- 12. A fully valid successor written by someone who can write the table is
-- returned as valid: nothing in the database can tell it from a block the
-- builder wrote.  Only a record kept outside the database (what the
-- publisher published, a digest) shows it.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_blocks
SELECT 5, 1, b.block_hash, b.tx_merkle_root, 0, b.closed_at,
       sha256(kingres.ledger_block_preimage(1::smallint, 5, b.block_hash, b.tx_merkle_root, 0, b.closed_at))
FROM public.saved_blocks b WHERE b.height = 4;
:trig_on;
\c
SET search_path = public;
SELECT public.page(4, 5) AS forged_valid_successor;
SELECT height FROM kingres.ledger_head();
SELECT count(*) > 0 AS verify_sees_the_successor FROM kingres.ledger_verify();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;
SELECT count(*) AS problems_after_all FROM kingres.ledger_verify();

-- 13. PUBLIC's read access to the catalog tables is the operator's to
-- withdraw (L7-09): the statement works, the owner's privileges stay
-- protected, a role that was granted EXECUTE on the two reading functions
-- keeps using them, and writing to a ledger table still works.
CREATE ROLE kl_head_writer;
GRANT USAGE ON SCHEMA public TO kl_head_writer;
GRANT INSERT ON docs TO kl_head_writer;
REVOKE SELECT ON kingres.ledger_blocks FROM PUBLIC;
REVOKE SELECT ON kingres.ledger_chain, kingres.ledger_table FROM public;
REVOKE SELECT ON kingres.ledger_digest FROM PUBLIC;
SELECT c.relname, has_table_privilege('public', c.oid, 'select') AS public_can_select
FROM pg_class c WHERE c.oid IN ('kingres.ledger_blocks'::regclass, 'kingres.ledger_chain'::regclass,
                                'kingres.ledger_table'::regclass)
ORDER BY 1;
SET ROLE kl_head_service;
SELECT count(*) FROM kingres.ledger_blocks;
SELECT count(*) FROM kingres.ledger_chain;
SELECT height FROM kingres.ledger_head();
SELECT count(*) AS entries FROM kingres.ledger_blocks_entries(0, 10);
SELECT count(*) AS entries_from_two FROM kingres.ledger_blocks_entries(2, 10);
RESET ROLE;
SET ROLE kl_head_nobody;
SELECT count(*) FROM kingres.ledger_blocks;
SELECT * FROM kingres.ledger_blocks_entries(0, 1);
RESET ROLE;
-- The ledger still takes writes from a role that cannot read its tables.
SET ROLE kl_head_writer;
INSERT INTO docs VALUES (100, 'written by a role that cannot read the ledger');
RESET ROLE;
SELECT kingres.ledger_build_block() IS NOT NULL AS built_after_the_write;
-- The owner's own privileges stay: revoking them would stop the functions.
REVOKE SELECT ON kingres.ledger_blocks FROM CURRENT_USER;
REVOKE ALL ON kingres.ledger_blocks FROM PUBLIC, CURRENT_USER;
-- All tables of the schema at once is allowed for PUBLIC, and so is granting again.
REVOKE ALL ON ALL TABLES IN SCHEMA kingres FROM PUBLIC;
GRANT SELECT ON kingres.ledger_blocks, kingres.ledger_chain, kingres.ledger_table TO PUBLIC;
SELECT has_table_privilege('public', 'kingres.ledger_blocks', 'select') AS public_can_select_again;
SELECT count(*) AS problems_at_the_end FROM kingres.ledger_verify();

-- Clean up the database and the roles.
\c :orig_db
DROP DATABASE kl_head_t;
DROP ROLE kl_head_nobody;
DROP ROLE kl_head_service;
DROP ROLE kl_head_writer;
