-- ledger_verify: clean verification, digests, inclusion proofs and every
-- tamper case, performed the way an attacker with catalog access would:
-- the protection triggers are switched off by editing pg_trigger directly
-- and the session is reconnected so that the change takes effect.
SET search_path = public;
\set VERBOSITY terse

-- Report of ledger_verify with the values that change from run to run
-- (transaction ids, hashes) masked.
CREATE FUNCTION public.vreport(lo bigint DEFAULT 0, hi bigint DEFAULT NULL)
RETURNS TABLE (check_name text, object text, detail text)
LANGUAGE sql AS $$
    SELECT v.check_name,
           regexp_replace(v.object, 'tx [0-9]+', 'tx N', 'g'),
           regexp_replace(v.detail, '[0-9a-f]{64}', '<hash>', 'g')
    FROM kingres.ledger_verify(lo, hi) v
    ORDER BY 1, 2, 3
$$;

-- Change one hex digit of a string.
CREATE FUNCTION pg_temp.flip(s text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE substr(s, 1, 1) WHEN '0' THEN '1' ELSE '0' END || substr(s, 2)
$$;

--------------------------------------------------------------------------
-- The state left by the previous test verifies cleanly
--------------------------------------------------------------------------
SELECT count(*) AS problems FROM kingres.ledger_verify();
SELECT count(*) AS problems_first_block FROM kingres.ledger_verify(0, 0);
SELECT * FROM kingres.ledger_verify(5, 2);
SELECT * FROM kingres.ledger_verify(-1);

--------------------------------------------------------------------------
-- New data: several transactions, one with several entries
--------------------------------------------------------------------------
CREATE TABLE docs (id int PRIMARY KEY, body text, created date);
SELECT kingres.ledger_enable('docs');
INSERT INTO docs VALUES (1, 'one', '2026-01-01');
BEGIN;
INSERT INTO docs SELECT g, 'bulk ' || g, '2026-02-01' FROM generate_series(2, 6) g;
COMMIT;
INSERT INTO docs VALUES (7, 'seven', NULL);
SELECT kingres.ledger_build_block() IS NOT NULL AS block_built;
INSERT INTO docs VALUES (8, 'eight', NULL);
SELECT kingres.ledger_generate_digest() AS d1 \gset
INSERT INTO docs VALUES (9, 'nine', NULL);
SELECT kingres.ledger_generate_digest() AS d2 \gset
SELECT count(*) AS problems FROM kingres.ledger_verify();
SELECT count(*) AS problems_in_range FROM kingres.ledger_verify(1, 2);

-- A row that is not in a block yet, and a row that is unknown.
INSERT INTO docs VALUES (10, 'ten', NULL);
SELECT kingres.ledger_inclusion_proof('docs', kingres.ledger_row_hash(d)) FROM docs d WHERE id = 10;
SELECT kingres.ledger_inclusion_proof('docs', sha256('nothing'));
SELECT kingres.ledger_inclusion_proof('accounts_v', sha256('nothing'));
SELECT kingres.ledger_inclusion_proof('pg_class', sha256('nothing'));
SELECT kingres.ledger_generate_digest() AS d3 \gset
SELECT count(*) AS problems FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- Digests
--------------------------------------------------------------------------
SELECT kingres.ledger_verify_digest(:'d1') AS d1_ok,
       kingres.ledger_verify_digest(:'d2') AS d2_ok,
       kingres.ledger_verify_digest(:'d3') AS d3_ok;
SELECT kingres.ledger_verify_digest(jsonb_set(:'d3'::jsonb, '{block_hash}',
           to_jsonb(pg_temp.flip(:'d3'::jsonb ->> 'block_hash')))) AS wrong_hash_accepted,
       kingres.ledger_verify_digest(jsonb_set(:'d3'::jsonb, '{height}',
           to_jsonb(((:'d3'::jsonb ->> 'height')::int + 1)))) AS future_height_accepted,
       kingres.ledger_verify_digest(jsonb_set(:'d3'::jsonb, '{height}',
           to_jsonb(((:'d3'::jsonb ->> 'height')::int - 1)))) AS other_height_accepted,
       kingres.ledger_verify_digest(jsonb_set(:'d3'::jsonb, '{chain_id}',
           to_jsonb(pg_temp.flip(:'d3'::jsonb ->> 'chain_id')))) AS other_chain_accepted,
       kingres.ledger_verify_digest((:'d3'::jsonb) - 'chain_id') AS no_chain_id_accepted,
       kingres.ledger_verify_digest('{}') AS empty_accepted,
       kingres.ledger_verify_digest('[]') AS array_accepted,
       kingres.ledger_verify_digest('{"height": "x", "block_hash": "00"}') AS garbage_accepted,
       kingres.ledger_verify_digest('{"height": 0, "block_hash": "zz"}') AS not_hex_accepted,
       kingres.ledger_verify_digest(NULL) AS null_result;

--------------------------------------------------------------------------
-- Inclusion proofs
--------------------------------------------------------------------------
-- Every row has a proof, and it verifies against the latest digest.
SELECT count(*) AS rows, count(*) FILTER (WHERE
           kingres.ledger_verify_proof(
               kingres.ledger_inclusion_proof('docs', kingres.ledger_row_hash(d)),
               :'d3'::jsonb)) AS verified_against_d3
FROM docs d;

-- An older digest verifies the rows that were in the ledger at that time.
SELECT d.id,
       kingres.ledger_verify_proof(
           kingres.ledger_inclusion_proof('docs', kingres.ledger_row_hash(d)), :'d1'::jsonb) AS in_d1,
       kingres.ledger_verify_proof(
           kingres.ledger_inclusion_proof('docs', kingres.ledger_row_hash(d)), :'d2'::jsonb) AS in_d2
FROM docs d ORDER BY d.id;

-- Round trip and corruption of a proof for a row in the middle of a
-- transaction with five entries; the block has more than one transaction.
SELECT kingres.ledger_inclusion_proof('docs', kingres.ledger_row_hash(d)) AS p
FROM docs d WHERE id = 4 \gset
SELECT jsonb_array_length(:'p'::jsonb -> 'entry_path') AS entry_path_length,
       (:'p'::jsonb ->> 'entry_count')::int AS entry_count,
       (:'p'::jsonb -> 'block' ->> 'tx_count')::int > 1 AS block_has_several_txs,
       jsonb_array_length(:'p'::jsonb -> 'chain') > 0 AS chain_not_empty;

WITH p AS (SELECT :'p'::jsonb AS j, :'d3'::jsonb AS d)
SELECT name, kingres.ledger_verify_proof(j, d) AS accepted
FROM (
    SELECT 'original' AS name, j, d FROM p
    UNION ALL SELECT 'row_hash', jsonb_set(j, '{row_hash}', to_jsonb(pg_temp.flip(j ->> 'row_hash'))), d FROM p
    UNION ALL SELECT 'entry_path[0]', jsonb_set(j, '{entry_path,0}', to_jsonb(pg_temp.flip(j #>> '{entry_path,0}'))), d FROM p
    UNION ALL SELECT 'entry_path[last]', jsonb_set(j, ARRAY['entry_path', (jsonb_array_length(j -> 'entry_path') - 1)::text],
                         to_jsonb(pg_temp.flip(j #>> ARRAY['entry_path', (jsonb_array_length(j -> 'entry_path') - 1)::text]))), d FROM p
    UNION ALL SELECT 'entry_path shortened', jsonb_set(j, '{entry_path}', (j -> 'entry_path') - 0), d FROM p
    UNION ALL SELECT 'entry_path extended', jsonb_set(j, '{entry_path}', (j -> 'entry_path') || to_jsonb(repeat('00', 32))), d FROM p
    UNION ALL SELECT 'entry_index', jsonb_set(j, '{entry_index}', to_jsonb((j ->> 'entry_index')::int + 1)), d FROM p
    UNION ALL SELECT 'entry_count', jsonb_set(j, '{entry_count}', to_jsonb((j ->> 'entry_count')::int + 1)), d FROM p
    UNION ALL SELECT 'seq', jsonb_set(j, '{seq}', to_jsonb((j ->> 'seq')::int + 1)), d FROM p
    UNION ALL SELECT 'table_id', jsonb_set(j, '{table_id}', to_jsonb((j ->> 'table_id')::int + 1)), d FROM p
    UNION ALL SELECT 'op', jsonb_set(j, '{op}', '"D"'), d FROM p
    UNION ALL SELECT 'ledger_txid', jsonb_set(j, '{ledger_txid}', to_jsonb((j ->> 'ledger_txid')::bigint + 1)), d FROM p
    UNION ALL SELECT 'tx_root', jsonb_set(j, '{tx_root}', to_jsonb(pg_temp.flip(j ->> 'tx_root'))), d FROM p
    UNION ALL SELECT 'user_name', jsonb_set(j, '{user_name}', '"mallory"'), d FROM p
    UNION ALL SELECT 'user_name empty', jsonb_set(j, '{user_name}', '""'), d FROM p
    UNION ALL SELECT 'user_name removed', j - 'user_name', d FROM p
    UNION ALL SELECT 'user_name not a string', jsonb_set(j, '{user_name}', '5'), d FROM p
    UNION ALL SELECT 'tx_index', jsonb_set(j, '{tx_index}', to_jsonb((j ->> 'tx_index')::int + 1)), d FROM p
    UNION ALL SELECT 'tx_path[0]', jsonb_set(j, '{tx_path,0}', to_jsonb(pg_temp.flip(j #>> '{tx_path,0}'))), d FROM p
    UNION ALL SELECT 'tx_path shortened', jsonb_set(j, '{tx_path}', (j -> 'tx_path') - 0), d FROM p
    UNION ALL SELECT 'block.height', jsonb_set(j, '{block,height}', to_jsonb((j #>> '{block,height}')::int + 1)), d FROM p
    UNION ALL SELECT 'block.prev_hash', jsonb_set(j, '{block,prev_hash}', to_jsonb(pg_temp.flip(j #>> '{block,prev_hash}'))), d FROM p
    UNION ALL SELECT 'block.tx_merkle_root', jsonb_set(j, '{block,tx_merkle_root}', to_jsonb(pg_temp.flip(j #>> '{block,tx_merkle_root}'))), d FROM p
    UNION ALL SELECT 'block.tx_count', jsonb_set(j, '{block,tx_count}', to_jsonb((j #>> '{block,tx_count}')::int + 1)), d FROM p
    UNION ALL SELECT 'block.closed_at_us', jsonb_set(j, '{block,closed_at_us}', to_jsonb((j #>> '{block,closed_at_us}')::bigint + 1)), d FROM p
    UNION ALL SELECT 'block.block_hash', jsonb_set(j, '{block,block_hash}', to_jsonb(pg_temp.flip(j #>> '{block,block_hash}'))), d FROM p
    UNION ALL SELECT 'block.hash_version', jsonb_set(j, '{block,hash_version}', '2'), d FROM p
    UNION ALL SELECT 'chain[0].prev_hash', jsonb_set(j, '{chain,0,prev_hash}', to_jsonb(pg_temp.flip(j #>> '{chain,0,prev_hash}'))), d FROM p
    UNION ALL SELECT 'chain[0].closed_at_us', jsonb_set(j, '{chain,0,closed_at_us}', to_jsonb((j #>> '{chain,0,closed_at_us}')::bigint + 1)), d FROM p
    UNION ALL SELECT 'chain removed', jsonb_set(j, '{chain}', '[]'), d FROM p
    UNION ALL SELECT 'chain missing', j - 'chain', d FROM p
    UNION ALL SELECT 'entry_path missing', j - 'entry_path', d FROM p
    UNION ALL SELECT 'entry_path not hex', jsonb_set(j, '{entry_path,0}', '"zz"'), d FROM p
    UNION ALL SELECT 'block missing', j - 'block', d FROM p
    UNION ALL SELECT 'proof is an array', '[]'::jsonb, d FROM p
    UNION ALL SELECT 'digest hash', j, jsonb_set(d, '{block_hash}', to_jsonb(pg_temp.flip(d ->> 'block_hash'))) FROM p
    UNION ALL SELECT 'digest height + 1', j, jsonb_set(d, '{height}', to_jsonb((d ->> 'height')::int + 1)) FROM p
    UNION ALL SELECT 'digest below block', j, jsonb_set(d, '{height}', to_jsonb((j #>> '{block,height}')::int - 1)) FROM p
    UNION ALL SELECT 'digest without hash', j, d - 'block_hash' FROM p
    UNION ALL SELECT 'proof chain_id', jsonb_set(j, '{chain_id}', to_jsonb(pg_temp.flip(j ->> 'chain_id'))), d FROM p
    UNION ALL SELECT 'proof chain_id removed', j - 'chain_id', d FROM p
    UNION ALL SELECT 'digest chain_id', j, jsonb_set(d, '{chain_id}', to_jsonb(pg_temp.flip(d ->> 'chain_id'))) FROM p
    UNION ALL SELECT 'digest chain_id removed', j, d - 'chain_id' FROM p
    UNION ALL SELECT 'relation (informational, not authenticated)', jsonb_set(j, '{relation}', '"other.table"'), d FROM p
) m;

-- The same proof checked against the digest of the block it is in.
SELECT kingres.ledger_verify_proof(:'p'::jsonb,
           jsonb_build_object('chain_id', :'p'::jsonb ->> 'chain_id',
                              'height', (:'p'::jsonb #>> '{block,height}')::int,
                              'block_hash', :'p'::jsonb #>> '{block,block_hash}')) AS against_own_block;

-- Proofs for rows that are the only entry of their transaction, and for the
-- first and last entry of a transaction.
SELECT d.id, kingres.ledger_verify_proof(
           kingres.ledger_inclusion_proof('docs', kingres.ledger_row_hash(d)), :'d3'::jsonb) AS ok
FROM docs d WHERE id IN (1, 2, 6, 7) ORDER BY id;

-- A proof for a table that is not a ledger table.
SELECT kingres.ledger_inclusion_proof('pg_class', sha256('x'));

--------------------------------------------------------------------------
-- Tampering, with the triggers bypassed as an attacker with catalog access
-- would.  Saved copies allow the ledger to be restored after each case.
--------------------------------------------------------------------------
CREATE TABLE saved_docs AS TABLE docs;
CREATE TABLE saved_entry AS TABLE kingres.ledger_entry;
CREATE TABLE saved_tx AS TABLE kingres.ledger_tx;
CREATE TABLE saved_blocks AS TABLE kingres.ledger_blocks;
CREATE TABLE saved_pending AS TABLE kingres.ledger_pending;

CREATE FUNCTION public.restore_all() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    TRUNCATE public.docs;
    INSERT INTO public.docs SELECT * FROM public.saved_docs;
    TRUNCATE kingres.ledger_entry;
    INSERT INTO kingres.ledger_entry SELECT * FROM public.saved_entry;
    TRUNCATE kingres.ledger_tx;
    INSERT INTO kingres.ledger_tx SELECT * FROM public.saved_tx;
    TRUNCATE kingres.ledger_blocks;
    INSERT INTO kingres.ledger_blocks SELECT * FROM public.saved_blocks;
    TRUNCATE kingres.ledger_pending;
    INSERT INTO kingres.ledger_pending SELECT * FROM public.saved_pending;
END $$;

\set trig_off 'UPDATE pg_trigger SET tgenabled = ''D'' WHERE tgname IN (''kingres_ledger_capture'', ''kingres_ledger_block_modify'', ''kingres_ledger_block_truncate'')'
\set trig_on 'UPDATE pg_trigger SET tgenabled = ''A'' WHERE tgname IN (''kingres_ledger_capture'', ''kingres_ledger_block_modify'', ''kingres_ledger_block_truncate'')'

-- The bypass itself works: with the triggers off, an update succeeds.
UPDATE docs SET body = 'blocked' WHERE id = 1;
:trig_off;
\c
SET search_path = public;
UPDATE docs SET body = 'one' WHERE id = 1;
:trig_on;
\c
SET search_path = public;
SELECT count(*) AS problems FROM kingres.ledger_verify();

-- 1. A modified row.
:trig_off;
\c
SET search_path = public;
UPDATE docs SET body = 'tampered' WHERE id = 3;
:trig_on;
\c
SET search_path = public;
SELECT * FROM public.vreport();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;
SELECT count(*) AS problems_after_restore FROM kingres.ledger_verify();

-- 2. A deleted row.
:trig_off;
\c
SET search_path = public;
DELETE FROM docs WHERE id = 7;
:trig_on;
\c
SET search_path = public;
SELECT * FROM public.vreport();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 3. A row inserted without an entry.
:trig_off;
\c
SET search_path = public;
INSERT INTO docs VALUES (99, 'smuggled', NULL);
:trig_on;
\c
SET search_path = public;
SELECT * FROM public.vreport();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 4. A modified entry (and no change of the data).
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_entry SET row_hash = sha256('other')
WHERE (ledger_txid, seq) = (SELECT ledger_txid, seq FROM kingres.ledger_entry
                            WHERE table_id = (SELECT table_id FROM kingres.ledger_table WHERE relid = 'docs'::regclass)
                            ORDER BY ledger_txid, seq LIMIT 1);
:trig_on;
\c
SET search_path = public;
SELECT * FROM public.vreport();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 5. A deleted entry.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_entry
WHERE (ledger_txid, seq) = (SELECT ledger_txid, seq FROM kingres.ledger_entry
                            WHERE table_id = (SELECT table_id FROM kingres.ledger_table WHERE relid = 'docs'::regclass)
                            ORDER BY ledger_txid DESC, seq DESC LIMIT 1);
:trig_on;
\c
SET search_path = public;
SELECT * FROM public.vreport();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 6. A modified block.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
-- One second earlier: the block is then also earlier than its predecessor,
-- which the block time check reports at block 1, outside the range that is
-- checked from block 2 on.
UPDATE kingres.ledger_blocks SET closed_at = closed_at - interval '1 second' WHERE height = 1;
:trig_on;
\c
SET search_path = public;
SELECT * FROM public.vreport();
SELECT count(*) AS problems_outside_range FROM kingres.ledger_verify(2);
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 7. A modified transaction root, and a modified transaction count of a block.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_tx SET merkle_root = sha256('forged')
WHERE ledger_txid = (SELECT min(ledger_txid) FROM kingres.ledger_tx);
UPDATE kingres.ledger_blocks SET tx_count = tx_count + 1 WHERE height = 0;
:trig_on;
\c
SET search_path = public;
SELECT * FROM public.vreport();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 7b. The user recorded for a transaction in a block is covered by the hashes.
SELECT ledger_txid AS first_tx FROM kingres.ledger_tx ORDER BY ledger_txid LIMIT 1 \gset
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_tx SET user_name = 'mallory' WHERE ledger_txid = :first_tx;
:trig_on;
\c
SET search_path = public;
SELECT * FROM public.vreport();
SELECT kingres.ledger_verify_digest(:'d3') AS digest_still_matches_stored_blocks;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;
SELECT count(*) AS problems_after_restore FROM kingres.ledger_verify();

-- 7c. The user recorded in the work queue, before the block is built, is NOT
-- covered: this is inside the documented window before a block exists.  The
-- tampered name ends up in the block and is hashed from then on.
INSERT INTO docs VALUES (12, 'twelve', NULL);
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_pending SET user_name = 'mallory';
:trig_on;
\c
SET search_path = public;
SELECT count(*) AS problems_with_tampered_pending_user FROM kingres.ledger_verify();
SELECT kingres.ledger_build_block() IS NOT NULL AS built;
SELECT count(*) AS problems_after_build FROM kingres.ledger_verify();
SELECT user_name FROM kingres.ledger_tx ORDER BY ledger_txid DESC LIMIT 1;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 8. A transaction removed from the work queue before it is in a block, and an
-- entry that belongs to no transaction.
INSERT INTO docs VALUES (11, 'eleven', NULL);
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_pending;
INSERT INTO kingres.ledger_entry
SELECT 1000000000, 0, table_id, 'I', sha256('ghost') FROM kingres.ledger_table WHERE relid = 'with_enum'::regclass;
:trig_on;
\c
SET search_path = public;
SELECT * FROM public.vreport();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;
SELECT count(*) AS problems_after_cleanup FROM kingres.ledger_verify();

-- 9. A transaction in a block and in the work queue at the same time.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_pending
SELECT ledger_txid, user_name FROM kingres.ledger_tx ORDER BY ledger_txid LIMIT 1;
:trig_on;
\c
SET search_path = public;
SELECT * FROM public.vreport();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;

-- 10. A trigger left disabled is reported by the verification.
:trig_off;
\c
SET search_path = public;
SELECT check_name, object, detail FROM kingres.ledger_verify()
WHERE object IN ('public.docs', 'kingres.ledger_blocks') ORDER BY 1, 2, 3;
:trig_on;
\c
SET search_path = public;
UPDATE pg_trigger SET tgenabled = 'O'
WHERE tgname = 'kingres_ledger_capture' AND tgrelid = 'docs'::regclass;
\c
SET search_path = public;
SELECT check_name, object, detail FROM kingres.ledger_verify();
UPDATE pg_trigger SET tgenabled = 'A'
WHERE tgname = 'kingres_ledger_capture' AND tgrelid = 'docs'::regclass;
\c
SET search_path = public;
SELECT count(*) AS problems_after_rearm FROM kingres.ledger_verify();

-- 11. The whole chain rewritten consistently from block 1 on: the ledger is
-- internally consistent, so ledger_verify finds nothing, but the digest that
-- was exported earlier no longer matches.
SELECT kingres.ledger_verify_digest(:'d3') AS digest_before_rewrite;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
DO $$
DECLARE
    h bigint;
    prev bytea;
    b kingres.ledger_blocks%ROWTYPE;
BEGIN
    -- Every block from 1 on moves by the same amount, so that block times stay
    -- in order and the rewritten chain is consistent.
    UPDATE kingres.ledger_blocks SET closed_at = closed_at + interval '1 second' WHERE height >= 1;
    FOR h IN SELECT height FROM kingres.ledger_blocks WHERE height >= 1 ORDER BY height LOOP
        SELECT block_hash INTO prev FROM kingres.ledger_blocks WHERE height = h - 1;
        UPDATE kingres.ledger_blocks SET prev_hash = prev WHERE height = h;
        SELECT * INTO b FROM kingres.ledger_blocks WHERE height = h;
        UPDATE kingres.ledger_blocks
        SET block_hash = kingres.ledger_block_hash(b.hash_version, b.height, b.prev_hash,
                                                    b.tx_merkle_root, b.tx_count, b.closed_at)
        WHERE height = h;
    END LOOP;
END $$;
:trig_on;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
-- The digests that the database recorded name the old hashes: ledger_verify
-- reports them (digest_hash).  An attacker who rewrites the chain has to remove
-- them too; a delete by hand with the setting on is recorded and reported
-- (operator_removal, L17-06), so the attacker takes them out with the triggers
-- off, as for the rewrite above.  Only a digest kept outside the database
-- cannot be taken away.
SELECT count(*) AS problems_with_stale_recorded_digests FROM kingres.ledger_verify();
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_digest;
:trig_on;
\c
SET search_path = public;
SELECT count(*) AS problems_after_consistent_rewrite FROM kingres.ledger_verify();
SELECT kingres.ledger_verify_digest(:'d1') AS d1_after_rewrite,
       kingres.ledger_verify_digest(:'d2') AS d2_after_rewrite,
       kingres.ledger_verify_digest(:'d3') AS d3_after_rewrite;
-- Block 0 was not rewritten, so a digest of block 0 still matches.
SELECT kingres.ledger_verify_digest(jsonb_build_object('chain_id', encode((SELECT chain_id FROM kingres.ledger_chain), 'hex'),
           'height', 0, 'block_hash',
           encode((SELECT block_hash FROM kingres.ledger_blocks WHERE height = 0), 'hex')))
       AS digest_of_block_0;
-- Proofs built from the rewritten chain do not verify against the old digest.
SELECT kingres.ledger_verify_proof(
           kingres.ledger_inclusion_proof('docs', kingres.ledger_row_hash(d)), :'d3'::jsonb) AS old_digest_accepts_rewritten_proof
FROM docs d WHERE id = 4;
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
SELECT public.restore_all();
:trig_on;
\c
SET search_path = public;
SELECT count(*) AS problems_after_restore FROM kingres.ledger_verify();
SELECT kingres.ledger_verify_digest(:'d3') AS digest_after_restore;
