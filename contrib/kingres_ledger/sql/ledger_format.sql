-- ledger_format: the hash format hardening of format version 1 -- domain tags,
-- the chain identity, block time and the binding of proofs to a table --
-- checked against an independent implementation written here in SQL with
-- sha256(), over the ledger that the earlier tests built.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: PostgreSQL
SET search_path = public;
\set VERBOSITY terse

-- Change one hex digit of a string.
CREATE FUNCTION pg_temp.flip(s text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE substr(s, 1, 1) WHEN '0' THEN '1' ELSE '0' END || substr(s, 2)
$$;

-- Independent RFC 6962 tree hash over leaf DATA (not leaf hashes).
CREATE FUNCTION pg_temp.ref_mth(d bytea[], lo int, hi int) RETURNS bytea
LANGUAGE plpgsql AS $$
DECLARE
    n int := hi - lo;
    k int := 1;
BEGIN
    IF n = 0 THEN
        RETURN sha256(''::bytea);
    ELSIF n = 1 THEN
        RETURN sha256('\x00'::bytea || d[lo + 1]);
    END IF;
    WHILE k * 2 < n LOOP
        k := k * 2;
    END LOOP;
    RETURN sha256('\x01'::bytea || pg_temp.ref_mth(d, lo, lo + k)
                  || pg_temp.ref_mth(d, lo + k, hi));
END $$;

-- Microseconds since 2000-01-01 00:00:00 UTC of a timestamptz.
CREATE FUNCTION pg_temp.us(ts timestamptz) RETURNS bigint
LANGUAGE sql IMMUTABLE AS $$
    SELECT ((extract(epoch FROM ts) - 946684800) * 1000000)::numeric::bigint
$$;

--------------------------------------------------------------------------
-- The whole ledger, recomputed from scratch with the documented formats
--------------------------------------------------------------------------
SELECT kingres.ledger_generate_digest() IS NOT NULL AS digest_built;

-- Entry leaves: tag 0x11 || u32 seq || u32 table_id || u8 op || row_hash;
-- transaction roots: tree over them in seq order.
SELECT count(*) AS transactions, bool_and(t.merkle_root = r.root) AS tx_roots_match
FROM kingres.ledger_tx t
CROSS JOIN LATERAL (
    SELECT pg_temp.ref_mth(array_agg('\x11'::bytea || int4send(e.seq) || int4send(e.table_id)
                                     || '\x49'::bytea || e.row_hash ORDER BY e.seq),
                           0, count(*)::int) AS root
    FROM kingres.ledger_entry e WHERE e.ledger_txid = t.ledger_txid) r;

-- Block leaves: tag 0x12 || u64 ledger_txid || u32 entry_count || merkle_root ||
-- u16 length || user_name; block roots over them in tx_index order.
SELECT count(*) AS blocks, bool_and(b.tx_merkle_root = r.root) AS block_roots_match
FROM kingres.ledger_blocks b
CROSS JOIN LATERAL (
    SELECT pg_temp.ref_mth(array_agg('\x12'::bytea || int8send(t.ledger_txid) || int4send(t.entry_count)
                                     || t.merkle_root
                                     || int2send(octet_length(convert_to(t.user_name::text, 'UTF8'))::int2)
                                     || convert_to(t.user_name::text, 'UTF8') ORDER BY t.tx_index),
                           0, count(*)::int) AS root
    FROM kingres.ledger_tx t WHERE t.block_height = b.height) r;

-- Block hashes: tag 0x13 || u16 version || u64 height || prev_hash || root ||
-- u32 tx_count || i64 closed_at.
SELECT count(*) AS blocks,
       bool_and(b.block_hash = sha256('\x13'::bytea || int2send(b.hash_version) || int8send(b.height)
                                      || b.prev_hash || b.tx_merkle_root || int4send(b.tx_count)
                                      || int8send(pg_temp.us(b.closed_at)))) AS block_hashes_match
FROM kingres.ledger_blocks b;

-- The chain: block 0 starts from SHA-256(0x14 || chain_id), every other block
-- from the hash of its predecessor.
SELECT bool_and(b.prev_hash = CASE WHEN b.height = 0
                                   THEN sha256('\x14'::bytea || c.chain_id)
                                   ELSE (SELECT p.block_hash FROM kingres.ledger_blocks p
                                          WHERE p.height = b.height - 1) END) AS chain_links_match
FROM kingres.ledger_blocks b, kingres.ledger_chain c;

-- Row hashes: tag 0x10 || u16 version || u32 table_id || u16 column count ||
-- (u16 name length, name, u32 value length, value) for each non-NULL column.
SELECT count(*) AS rows_checked,
       bool_and(kingres.ledger_row_hash(d.*) = sha256('\x00'::bytea || '\x10'::bytea || '\x0001'::bytea
           || int4send((SELECT table_id FROM kingres.ledger_table WHERE relid = 'docs'::regclass))
           || int2send((1 + (d.body IS NOT NULL)::int + (d.created IS NOT NULL)::int)::int2)
           || int2send(2::int2) || convert_to('id', 'UTF8') || int4send(4) || int4send(d.id)
           || CASE WHEN d.body IS NULL THEN ''::bytea
                   ELSE int2send(4::int2) || convert_to('body', 'UTF8')
                        || int4send(octet_length(convert_to(d.body, 'UTF8')))
                        || convert_to(d.body, 'UTF8') END
           || CASE WHEN d.created IS NULL THEN ''::bytea
                   ELSE int2send(7::int2) || convert_to('created', 'UTF8') || int4send(4)
                        || int4send((d.created - date '2000-01-01')) END)) AS row_hashes_match
FROM docs d;


--------------------------------------------------------------------------
-- Chain identity
--------------------------------------------------------------------------
SELECT kingres.ledger_generate_digest() AS dg \gset
SELECT (:'dg'::jsonb ->> 'chain_id') = encode((SELECT chain_id FROM kingres.ledger_chain), 'hex')
       AS digest_carries_chain_id;
SELECT kingres.ledger_verify_digest(:'dg') AS own_digest;
SELECT kingres.ledger_verify_digest(jsonb_set(:'dg'::jsonb, '{chain_id}',
           to_jsonb(pg_temp.flip(:'dg'::jsonb ->> 'chain_id')))) AS other_chain;
SELECT kingres.ledger_verify_digest((:'dg'::jsonb) - 'chain_id') AS no_chain_id;
SELECT kingres.ledger_verify_digest(jsonb_set(:'dg'::jsonb, '{chain_id}', '"00"')) AS short_chain_id;
SELECT kingres.ledger_verify_digest(jsonb_set(:'dg'::jsonb, '{chain_id}', '17')) AS numeric_chain_id;

-- A proof from the first block carries the chain identity inside its hashes
-- (the prev_hash of block 0): another chain id, even one that the digest
-- agrees with, is rejected.
SELECT e.row_hash AS r0 FROM kingres.ledger_entry e
JOIN kingres.ledger_tx t USING (ledger_txid)
JOIN kingres.ledger_table lt USING (table_id)
WHERE t.block_height = 0 AND lt.relid = 'accounts'::regclass
ORDER BY e.ledger_txid, e.seq LIMIT 1 \gset
SELECT kingres.ledger_inclusion_proof('accounts', :'r0') AS p0 \gset
SELECT (:'p0'::jsonb #>> '{block,height}')::int AS block_of_proof;
SELECT (:'p0'::jsonb ->> 'chain_id') = (:'dg'::jsonb ->> 'chain_id') AS proof_carries_chain_id;
SELECT kingres.ledger_verify_proof(:'p0'::jsonb, :'dg'::jsonb) AS block_0_proof;
SELECT kingres.ledger_verify_proof(
           jsonb_set(:'p0'::jsonb, '{chain_id}', to_jsonb(pg_temp.flip(:'p0'::jsonb ->> 'chain_id'))),
           jsonb_set(:'dg'::jsonb, '{chain_id}', to_jsonb(pg_temp.flip(:'dg'::jsonb ->> 'chain_id'))))
       AS block_0_proof_with_both_chain_ids_changed;
SELECT kingres.ledger_verify_proof(
           jsonb_set(:'p0'::jsonb, '{chain_id}', to_jsonb(pg_temp.flip(:'p0'::jsonb ->> 'chain_id'))),
           :'dg'::jsonb) AS proof_of_another_chain;

--------------------------------------------------------------------------
-- A proof is a proof for one table and one row
--------------------------------------------------------------------------
SELECT kingres.ledger_inclusion_proof('docs', kingres.ledger_row_hash(d.*)) AS pd,
       (SELECT table_id FROM kingres.ledger_table WHERE relid = 'docs'::regclass) AS docs_id,
       (SELECT table_id FROM kingres.ledger_table WHERE relid = 'accounts'::regclass) AS accounts_id,
       kingres.ledger_row_hash(d.*) AS docs_hash
FROM docs d WHERE d.id = 4 \gset
SELECT (:'pd'::jsonb ->> 'table_id')::int = :docs_id AS proof_names_the_table;
SELECT kingres.ledger_verify_proof(:'pd'::jsonb, :'dg'::jsonb) AS plain_form;
SELECT kingres.ledger_verify_proof(:'pd'::jsonb, :'dg'::jsonb, :docs_id, :'docs_hash') AS right_table_and_row;
SELECT kingres.ledger_verify_proof(:'pd'::jsonb, :'dg'::jsonb, :accounts_id, :'docs_hash') AS other_table;
SELECT kingres.ledger_verify_proof(:'pd'::jsonb, :'dg'::jsonb, :docs_id, sha256('another row')) AS other_row;
SELECT kingres.ledger_verify_proof(:'pd'::jsonb, :'dg'::jsonb, :docs_id, '\x00') AS short_row_hash;
-- The table id is covered by the hashes: a proof for the row of one table does
-- not pass for the same bytes in another table.
SELECT kingres.ledger_verify_proof(
           jsonb_set(:'pd'::jsonb, '{table_id}', to_jsonb(:accounts_id)),
           :'dg'::jsonb, :accounts_id, :'docs_hash') AS table_id_edited;
--------------------------------------------------------------------------
-- Block time never goes back
--------------------------------------------------------------------------
-- Move the time of the latest block one hour into the future, the way a
-- clock that was ahead would have, and recompute its hash.  The next block
-- must not be earlier.
SELECT max(height) AS last_height FROM kingres.ledger_blocks \gset
UPDATE pg_trigger SET tgenabled = 'D'
WHERE tgname IN ('kingres_ledger_capture', 'kingres_ledger_block_modify', 'kingres_ledger_block_truncate');
\c
SET search_path = public;
UPDATE kingres.ledger_blocks b
SET closed_at = closed_at + interval '1 hour',
    block_hash = kingres.ledger_block_hash(b.hash_version, b.height, b.prev_hash, b.tx_merkle_root,
                                            b.tx_count, b.closed_at + interval '1 hour')
WHERE b.height = :last_height;
UPDATE pg_trigger SET tgenabled = 'A'
WHERE tgname IN ('kingres_ledger_capture', 'kingres_ledger_block_modify', 'kingres_ledger_block_truncate');
\c
SET search_path = public;
SELECT count(*) AS problems_with_a_future_block FROM kingres.ledger_verify();
INSERT INTO docs VALUES (500, 'after a clock that was ahead', NULL);
SELECT kingres.ledger_build_block() AS new_height \gset
SELECT b.closed_at = p.closed_at AS clamped_to_previous_time,
       b.closed_at >= p.closed_at AS never_earlier
FROM kingres.ledger_blocks b JOIN kingres.ledger_blocks p ON p.height = b.height - 1
WHERE b.height = :new_height;
SELECT count(*) AS problems_after_clamp FROM kingres.ledger_verify();
SELECT kingres.ledger_generate_digest() AS dg2 \gset
SELECT kingres.ledger_verify_digest(:'dg2') AS digest_of_clamped_chain;

-- A block that is earlier than its predecessor is reported, and a digest of
-- such a chain does not verify, even though every hash is consistent.
UPDATE pg_trigger SET tgenabled = 'D'
WHERE tgname IN ('kingres_ledger_capture', 'kingres_ledger_block_modify', 'kingres_ledger_block_truncate');
\c
SET search_path = public;
UPDATE kingres.ledger_blocks b
SET closed_at = closed_at - interval '2 hours',
    block_hash = kingres.ledger_block_hash(b.hash_version, b.height, b.prev_hash, b.tx_merkle_root,
                                            b.tx_count, b.closed_at - interval '2 hours')
WHERE b.height = (SELECT max(height) FROM kingres.ledger_blocks);
UPDATE pg_trigger SET tgenabled = 'A'
WHERE tgname IN ('kingres_ledger_capture', 'kingres_ledger_block_modify', 'kingres_ledger_block_truncate');
\c
SET search_path = public;
SELECT check_name, regexp_replace(object, '[0-9]+', 'N') AS object, detail FROM kingres.ledger_verify();
SELECT kingres.ledger_verify_digest(
           jsonb_set(:'dg2'::jsonb, '{block_hash}',
                     to_jsonb(encode((SELECT block_hash FROM kingres.ledger_blocks
                                       ORDER BY height DESC LIMIT 1), 'hex')))) AS digest_of_backwards_chain;
-- Put the time back where it was (an hour ahead of the previous block).
UPDATE pg_trigger SET tgenabled = 'D'
WHERE tgname IN ('kingres_ledger_capture', 'kingres_ledger_block_modify', 'kingres_ledger_block_truncate');
\c
SET search_path = public;
UPDATE kingres.ledger_blocks b
SET closed_at = closed_at + interval '2 hours',
    block_hash = kingres.ledger_block_hash(b.hash_version, b.height, b.prev_hash, b.tx_merkle_root,
                                            b.tx_count, b.closed_at + interval '2 hours')
WHERE b.height = (SELECT max(height) FROM kingres.ledger_blocks);
UPDATE pg_trigger SET tgenabled = 'A'
WHERE tgname IN ('kingres_ledger_capture', 'kingres_ledger_block_modify', 'kingres_ledger_block_truncate');
\c
SET search_path = public;
SELECT count(*) AS problems_after_repair FROM kingres.ledger_verify();
SELECT kingres.ledger_verify_digest(:'dg2') AS digest_after_repair;
