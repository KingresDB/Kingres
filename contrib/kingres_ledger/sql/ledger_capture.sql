-- ledger_capture: enabling tables, write capture, rollback behavior, blocks
-- and digests.

-- A previous run against the same cluster may have left roles behind; the
-- database itself is recreated by pg_regress.
DROP ROLE IF EXISTS kl_owner;
DROP ROLE IF EXISTS kl_other;
CREATE ROLE kl_owner;
CREATE ROLE kl_other;
-- The bootstrap superuser of a test cluster may be named like the schema of the
-- extension, which would put that schema first in the default search_path.
SET search_path = public;
\set VERBOSITY terse
GRANT USAGE ON SCHEMA public TO kl_owner, kl_other;
GRANT CREATE ON SCHEMA public TO kl_owner;

-- The schema belongs to no extension, and the functions are in place.
SELECT count(*) AS schema_is_extension_member
FROM pg_depend d JOIN pg_namespace n ON n.oid = d.objid
WHERE d.classid = 'pg_namespace'::regclass AND n.nspname = 'kingres'
  AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e';

--------------------------------------------------------------------------
-- ledger_enable: argument and table checks
--------------------------------------------------------------------------
CREATE TABLE accounts (id int PRIMARY KEY, owner_name text NOT NULL, balance numeric(12,2) NOT NULL);
INSERT INTO accounts VALUES (1, 'alice', 100.00), (2, 'bob', 50.50);

SELECT kingres.ledger_enable('accounts', 'bogus');
SELECT kingres.ledger_enable(NULL);
CREATE TEMP TABLE tmp_t (a int);
SELECT kingres.ledger_enable('tmp_t');
CREATE VIEW accounts_v AS SELECT * FROM accounts;
SELECT kingres.ledger_enable('accounts_v');
CREATE TABLE parted (a int) PARTITION BY LIST (a);
SELECT kingres.ledger_enable('parted');
CREATE TYPE pair AS (x int, y int);
CREATE TABLE with_composite (id int, p pair);
SELECT kingres.ledger_enable('with_composite');
CREATE TYPE color AS ENUM ('red', 'green');
CREATE TABLE with_enum_array (id int, c color[]);
SELECT kingres.ledger_enable('with_enum_array');
CREATE TABLE with_enum (id int, c color);
SELECT kingres.ledger_enable('with_enum');
SELECT kingres.ledger_enable('pg_class');
SELECT kingres.ledger_enable('kingres.ledger_entry');

-- ledger_enable is not executable by PUBLIC (enabling is permanent, so it is an
-- administrative decision).  The administrator grants it to the roles that may
-- make that decision; they still have to own the table.
SET ROLE kl_other;
SELECT kingres.ledger_enable('accounts');
RESET ROLE;
GRANT EXECUTE ON FUNCTION kingres.ledger_enable(regclass, text) TO kl_owner, kl_other;

-- Only the owner (or a member of the owning role) may enable a table.
ALTER TABLE with_enum OWNER TO kl_owner;
SET ROLE kl_other;
SELECT kingres.ledger_enable('accounts');
RESET ROLE;

-- Enabling is transactional: a rolled back ledger_enable leaves nothing.
BEGIN;
SELECT kingres.ledger_enable('accounts');
SELECT count(*) AS entries_in_enabling_tx FROM kingres.ledger_entry;
ROLLBACK;
SELECT count(*) AS tables, (SELECT count(*) FROM kingres.ledger_entry) AS entries,
       (SELECT count(*) FROM kingres.ledger_pending) AS pending
FROM kingres.ledger_table;
SELECT tgname FROM pg_trigger WHERE tgrelid = 'accounts'::regclass AND NOT tgisinternal;

-- Enable a table that already has rows: they are entered by this transaction.
SELECT kingres.ledger_enable('accounts');
SELECT kingres.ledger_enable('accounts');
SELECT t.relid::regclass AS relid, t.mode,
       (SELECT count(*) FROM kingres.ledger_entry) AS entries,
       (SELECT count(DISTINCT ledger_txid) FROM kingres.ledger_entry) AS transactions,
       (SELECT count(*) FROM kingres.ledger_pending) AS pending
FROM kingres.ledger_table t;

-- The triggers are installed and armed ENABLE ALWAYS.
SELECT tgname, tgenabled
FROM pg_trigger WHERE tgrelid = 'accounts'::regclass AND NOT tgisinternal
ORDER BY tgname;

-- Every existing row has an entry with its row hash.
SELECT count(*) AS rows_with_entry
FROM accounts a JOIN kingres.ledger_entry e ON e.row_hash = kingres.ledger_row_hash(a);

--------------------------------------------------------------------------
-- Capture of inserts
--------------------------------------------------------------------------
CREATE TEMP TABLE marks (label text, txid bigint);

-- Autocommit insert: one transaction per statement.
INSERT INTO accounts VALUES (3, 'carol', 10);
-- One transaction, several rows: sequence numbers are ordered.
BEGIN;
INSERT INTO marks VALUES ('multi', pg_current_xact_id()::text::bigint);
INSERT INTO accounts VALUES (4, 'dave', 1), (5, 'erin', 2);
INSERT INTO accounts VALUES (6, 'frank', 3);
COMMIT;
SELECT count(*) AS entries_of_multi, min(seq) AS first_seq, max(seq) AS last_seq
FROM kingres.ledger_entry e JOIN marks m ON m.txid = e.ledger_txid
WHERE m.label = 'multi';
SELECT count(*) AS pending_of_multi
FROM kingres.ledger_pending p JOIN marks m ON m.txid = p.ledger_txid WHERE m.label = 'multi';

-- The entry hash is the row hash, and the user is recorded.
SELECT count(*) AS hashes_match
FROM accounts a JOIN kingres.ledger_entry e ON e.row_hash = kingres.ledger_row_hash(a)
WHERE a.id >= 3;
SELECT DISTINCT user_name = current_user AS user_recorded FROM kingres.ledger_pending;

-- COPY goes through the same trigger.
BEGIN;
INSERT INTO marks VALUES ('copy', pg_current_xact_id()::text::bigint);
COPY accounts FROM stdin;
7	gina	7
8	hank	8
\.
COMMIT;
SELECT count(*) AS entries_of_copy
FROM kingres.ledger_entry e JOIN marks m ON m.txid = e.ledger_txid WHERE m.label = 'copy';

-- session_replication_role = replica does not skip the capture trigger.
SET session_replication_role = replica;
BEGIN;
INSERT INTO marks VALUES ('replica', pg_current_xact_id()::text::bigint);
INSERT INTO accounts VALUES (9, 'ivan', 9);
COMMIT;
RESET session_replication_role;
SELECT count(*) AS entries_of_replica
FROM kingres.ledger_entry e JOIN marks m ON m.txid = e.ledger_txid WHERE m.label = 'replica';

--------------------------------------------------------------------------
-- Rollback and subtransactions
--------------------------------------------------------------------------
CREATE TEMP TABLE counts AS
SELECT (SELECT count(*) FROM kingres.ledger_entry) AS entries,
       (SELECT count(*) FROM kingres.ledger_pending) AS pending;

-- A rolled back transaction leaves no entry and no pending row.
BEGIN;
INSERT INTO accounts VALUES (100, 'rolled back', 1);
ROLLBACK;
SELECT (SELECT count(*) FROM kingres.ledger_entry) - entries AS new_entries,
       (SELECT count(*) FROM kingres.ledger_pending) - pending AS new_pending
FROM counts;

-- The first entry is written in a subtransaction that is rolled back: the
-- transaction must still be queued when it later inserts again.
BEGIN;
INSERT INTO marks VALUES ('sub1', pg_current_xact_id()::text::bigint);
SAVEPOINT a;
INSERT INTO accounts VALUES (101, 'in rolled back sub', 1);
ROLLBACK TO a;
INSERT INTO accounts VALUES (102, 'after rollback to', 1);
COMMIT;
SELECT count(*) AS entries, min(e.seq) AS only_seq,
       (SELECT count(*) FROM kingres.ledger_pending p WHERE p.ledger_txid = m.txid) AS queued
FROM kingres.ledger_entry e JOIN marks m ON m.txid = e.ledger_txid
WHERE m.label = 'sub1' GROUP BY m.txid;

-- The pending row is written in a released subtransaction and the outer
-- transaction rolls back to an earlier savepoint, then writes again.
BEGIN;
INSERT INTO marks VALUES ('sub2', pg_current_xact_id()::text::bigint);
SAVEPOINT outer_sp;
SAVEPOINT inner_sp;
INSERT INTO accounts VALUES (103, 'in released sub', 1);
RELEASE SAVEPOINT inner_sp;
ROLLBACK TO outer_sp;
INSERT INTO accounts VALUES (104, 'after outer rollback', 1);
COMMIT;
SELECT count(*) AS entries,
       (SELECT count(*) FROM kingres.ledger_pending p WHERE p.ledger_txid = m.txid) AS queued
FROM kingres.ledger_entry e JOIN marks m ON m.txid = e.ledger_txid
WHERE m.label = 'sub2' GROUP BY m.txid;

-- An error caught inside a PL/pgSQL block (an implicit subtransaction).
BEGIN;
INSERT INTO marks VALUES ('sub3', pg_current_xact_id()::text::bigint);
DO $$
BEGIN
    BEGIN
        INSERT INTO accounts VALUES (105, 'dup', 1);
        INSERT INTO accounts VALUES (105, 'dup again', 1);
    EXCEPTION WHEN unique_violation THEN
        NULL;
    END;
    INSERT INTO accounts VALUES (106, 'after caught error', 1);
END $$;
COMMIT;
SELECT count(*) AS entries,
       (SELECT count(*) FROM kingres.ledger_pending p WHERE p.ledger_txid = m.txid) AS queued
FROM kingres.ledger_entry e JOIN marks m ON m.txid = e.ledger_txid
WHERE m.label = 'sub3' GROUP BY m.txid;
SELECT count(*) AS rows_without_entry
FROM accounts a
WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_entry e WHERE e.row_hash = kingres.ledger_row_hash(a));
SELECT count(*) AS entries_without_row
FROM kingres.ledger_entry e
WHERE NOT EXISTS (SELECT 1 FROM accounts a WHERE e.row_hash = kingres.ledger_row_hash(a));

-- Two-phase commit: the transaction is queued only when it commits.
SELECT count(*) AS pending_before FROM kingres.ledger_pending \gset
BEGIN;
INSERT INTO accounts VALUES (110, 'prepared', 1);
PREPARE TRANSACTION 'kl_2pc_commit';
SELECT count(*) - :pending_before AS pending_added_while_prepared FROM kingres.ledger_pending;
COMMIT PREPARED 'kl_2pc_commit';
SELECT count(*) - :pending_before AS pending_added_after_commit FROM kingres.ledger_pending;
BEGIN;
INSERT INTO accounts VALUES (111, 'prepared and rolled back', 1);
PREPARE TRANSACTION 'kl_2pc_rollback';
ROLLBACK PREPARED 'kl_2pc_rollback';
SELECT count(*) AS rows_without_entry
FROM accounts a
WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_entry e WHERE e.row_hash = kingres.ledger_row_hash(a));
SELECT count(*) AS entries_without_row
FROM kingres.ledger_entry e
WHERE NOT EXISTS (SELECT 1 FROM accounts a WHERE e.row_hash = kingres.ledger_row_hash(a));

--------------------------------------------------------------------------
-- Append-only enforcement by the triggers
--------------------------------------------------------------------------
DO $$
DECLARE
    stmt text;
BEGIN
    FOREACH stmt IN ARRAY ARRAY[
        'UPDATE accounts SET balance = 0',
        'DELETE FROM accounts',
        'TRUNCATE accounts',
        'DELETE FROM kingres.ledger_entry',
        'TRUNCATE kingres.ledger_tx',
        'UPDATE kingres.ledger_table SET mode = mode',
        'UPDATE kingres.ledger_pending SET user_name = user_name',
        'UPDATE kingres.ledger_chain SET chain_id = chain_id',
        'DELETE FROM kingres.ledger_chain',
        'TRUNCATE kingres.ledger_chain',
        'INSERT INTO kingres.ledger_chain (singleton, chain_id) VALUES (false, decode(repeat(''00'', 32), ''hex''))',
        'INSERT INTO kingres.ledger_chain (chain_id) VALUES (decode(repeat(''00'', 32), ''hex''))']
    LOOP
        BEGIN
            EXECUTE stmt;
            RAISE NOTICE '% -> no error', stmt;
        EXCEPTION WHEN OTHERS THEN
            RAISE NOTICE '% -> % %', stmt, SQLSTATE, SQLERRM;
        END;
    END LOOP;
END $$;

-- The blocking triggers also hold under session_replication_role = replica.
SET session_replication_role = replica;
UPDATE accounts SET balance = 0;
DELETE FROM accounts;
TRUNCATE accounts;
RESET session_replication_role;

-- INSERT ... ON CONFLICT DO UPDATE would modify a row: rejected as well.
INSERT INTO accounts VALUES (1, 'alice', 1) ON CONFLICT (id) DO UPDATE SET balance = 1;
-- ... and DO NOTHING is allowed and captures nothing.
SELECT count(*) AS entries_before FROM kingres.ledger_entry \gset
INSERT INTO accounts VALUES (1, 'alice', 1) ON CONFLICT (id) DO NOTHING;
SELECT count(*) - :entries_before AS entries_added FROM kingres.ledger_entry;

--------------------------------------------------------------------------
-- Blocks
--------------------------------------------------------------------------
SELECT count(*) > 0 AS has_pending FROM kingres.ledger_pending;
SET kingres_ledger.max_block_transactions = 3;
SELECT kingres.ledger_build_block() AS first_height;
-- Block 0 starts from the identity of the chain, not from zeros.
SELECT height, tx_count,
       prev_hash = kingres.ledger_genesis_hash((SELECT chain_id FROM kingres.ledger_chain))
           AS prev_is_derived_from_chain_id,
       prev_hash = decode(repeat('00', 32), 'hex') AS prev_is_zero,
       hash_version
FROM kingres.ledger_blocks;
RESET kingres_ledger.max_block_transactions;
SELECT count(*) AS txs_in_block_0, min(tx_index) AS first_index, max(tx_index) AS last_index,
       bool_and(lag_txid IS NULL OR ledger_txid > lag_txid) AS ordered_by_txid
FROM (SELECT t.*, lag(ledger_txid) OVER (ORDER BY tx_index) AS lag_txid
      FROM kingres.ledger_tx t WHERE block_height = 0) s;
SELECT count(*) > 0 AS others_still_pending FROM kingres.ledger_pending;
SELECT count(*) AS pending_also_in_a_block
FROM kingres.ledger_pending p JOIN kingres.ledger_tx t USING (ledger_txid);

-- The next block continues the chain.
SELECT kingres.ledger_build_block() AS second_height;
SELECT b.height, b.prev_hash = a.block_hash AS chained
FROM kingres.ledger_blocks b JOIN kingres.ledger_blocks a ON a.height = b.height - 1;
SELECT count(*) AS pending_left FROM kingres.ledger_pending;

-- Nothing pending: no block.
SELECT kingres.ledger_build_block() AS nothing_to_do;

-- Every transaction with entries is in exactly one block.
SELECT count(*) AS entry_transactions,
       count(t.ledger_txid) AS in_a_block
FROM (SELECT DISTINCT ledger_txid FROM kingres.ledger_entry) e
LEFT JOIN kingres.ledger_tx t USING (ledger_txid);

-- Recomputed per-transaction roots and the block hash, independent of the builder.
SELECT count(*) AS txs_checked,
       bool_and(t.merkle_root = (
           SELECT kingres.ledger_merkle_root(
                      kingres.ledger_leaf_hash(
                          '\x11'::bytea || int4send(e.seq) || int4send(e.table_id) || '\x49'::bytea || e.row_hash)
                      ORDER BY e.seq)
           FROM kingres.ledger_entry e WHERE e.ledger_txid = t.ledger_txid)) AS roots_match
FROM kingres.ledger_tx t;

--------------------------------------------------------------------------
-- Digest
--------------------------------------------------------------------------
INSERT INTO accounts VALUES (200, 'digest row', 1);
SELECT kingres.ledger_generate_digest() AS digest \gset
SELECT jsonb_typeof(:'digest'::jsonb) AS digest_type,
       (:'digest'::jsonb) ? 'height' AS has_height,
       (:'digest'::jsonb) ? 'block_hash' AS has_block_hash,
       (:'digest'::jsonb)->>'database' = current_database() AS database_matches,
       (:'digest'::jsonb)->>'block_hash' = encode((SELECT block_hash FROM kingres.ledger_blocks
                                                     ORDER BY height DESC LIMIT 1), 'hex') AS hash_matches_latest,
       ((:'digest'::jsonb)->>'height')::bigint = (SELECT max(height) FROM kingres.ledger_blocks) AS height_matches_latest;
SELECT count(*) AS pending_after_digest FROM kingres.ledger_pending;
-- A second digest without new writes does not create a block.
SELECT count(*) AS blocks_before FROM kingres.ledger_blocks \gset
SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest_again;
SELECT count(*) - :blocks_before AS blocks_added FROM kingres.ledger_blocks;

-- Block building needs READ COMMITTED.
BEGIN ISOLATION LEVEL REPEATABLE READ;
SELECT kingres.ledger_build_block();
ROLLBACK;

-- Writers cannot call the administrative functions.
SET ROLE kl_other;
SELECT kingres.ledger_build_block();
SELECT kingres.ledger_generate_digest();
SELECT kingres.ledger_record_row(1, ROW(1));
RESET ROLE;

-- An owner role can enable its table and write to it as itself.
SET ROLE kl_owner;
SELECT kingres.ledger_enable('with_enum');
INSERT INTO with_enum VALUES (1, 'green');
RESET ROLE;
REVOKE EXECUTE ON FUNCTION kingres.ledger_enable(regclass, text) FROM kl_owner, kl_other;
SELECT user_name FROM kingres.ledger_pending ORDER BY ledger_txid DESC LIMIT 1;
SELECT count(*) AS enum_entries FROM kingres.ledger_entry e
JOIN kingres.ledger_table t USING (table_id) WHERE t.relid = 'with_enum'::regclass;

-- Now that blocks, transactions and entries exist, every statement that
-- would modify them is rejected, for every role.
DO $$
DECLARE
    stmt text;
BEGIN
    FOREACH stmt IN ARRAY ARRAY[
        'UPDATE kingres.ledger_blocks SET height = height',
        'DELETE FROM kingres.ledger_blocks',
        'TRUNCATE kingres.ledger_blocks',
        'UPDATE kingres.ledger_tx SET tx_index = tx_index',
        'DELETE FROM kingres.ledger_tx',
        'UPDATE kingres.ledger_entry SET seq = seq',
        'DELETE FROM kingres.ledger_table',
        'TRUNCATE kingres.ledger_table, kingres.ledger_entry']
    LOOP
        BEGIN
            EXECUTE stmt;
            RAISE NOTICE '% -> no error', stmt;
        EXCEPTION WHEN OTHERS THEN
            RAISE NOTICE '% -> % %', stmt, SQLSTATE, SQLERRM;
        END;
    END LOOP;
END $$;
