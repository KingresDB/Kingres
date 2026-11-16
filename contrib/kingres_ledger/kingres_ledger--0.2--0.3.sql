-- kingres_ledger--0.2--0.3.sql
--
-- Upgrade of the kingres_ledger extension from version 0.2 to 0.3 (part A):
-- three functions that read the chain for tile publication,
-- kingres.ledger_block_preimage, kingres.ledger_head and
-- kingres.ledger_blocks_entries.  The result is the same set of objects,
-- privileges and protection as installing kingres_ledger--0.3.sql directly;
-- the test t/011_upgrade_0_3.pl compares the two.  No table, block, digest
-- or proof is touched, no entry is written, and hash format 1 is unchanged.
--
-- Load the new library first (restart the server): the new functions need
-- the 0.3 library.  A 0.3 library works with the 0.2 SQL.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only

\echo Use "ALTER EXTENSION kingres_ledger UPDATE TO '0.3'" to load this file. \quit

--------------------------------------------------------------------------
-- Reading the chain for publication (0.3, part A)
--------------------------------------------------------------------------
-- What the control plane needs to build a tile log over the blocks
-- (kingres/docs/design/ledger-phase7.md, sections 4 and 8.1): the 87 bytes a
-- block hash covers, the head of the chain, and a bounded page of the block
-- preimages.  None of these writes anything or builds a block; each reads
-- the snapshot of the statement that calls it (the snapshot of the calling
-- query, because they are STABLE) and is safe to call while the block
-- builder and writers run: the builder holds only a table lock that does not
-- block readers, and a block row becomes visible whole and in height order.
-- On a standby they work and read what has been replayed.

CREATE FUNCTION kingres.ledger_block_preimage(hash_version smallint, height bigint,
                                               prev_hash bytea,
                                               tx_merkle_root bytea,
                                               tx_count int,
                                               closed_at timestamptz)
    RETURNS bytea
    AS 'MODULE_PATHNAME', 'kingres_ledger_block_preimage'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_block_preimage(smallint, bigint, bytea, bytea, int, timestamptz) IS
    'The 87 bytes ledger_block_hash hashes (hash format version 1): tag 0x13, u16 hash_version, u64 height, prev_hash, tx_merkle_root, u32 tx_count, i64 closed_at in microseconds. Same arguments and checks as ledger_block_hash; block_hash = SHA-256 of the result.';

-- SECURITY DEFINER so that the answer does not depend on what the caller may
-- read of the ledger tables; the caller needs EXECUTE only, which PUBLIC does
-- not have.  The chain identity, the head and the records it is compared
-- with are read by one statement snapshot.  An empty ledger gives no row; a
-- missing chain identity is an error, not an empty answer, so that nobody
-- mistakes damage for emptiness.
--
-- What the answer is checked against (error XX001 when any check fails; a
-- head that is checked is still only what this database says, see the README,
-- "Reading the chain for publication"):
--   * the head block itself: stored hash version, lengths, and a block hash
--     recomputed from its fields, not the stored one passed through;
--   * the chain identity against block 0: the genesis hash of the identity
--     must be the link of block 0;
--   * the stored hash of the block before the head against the head's link;
--   * the records that outlive a deleted block: the newest transaction row
--     (ledger_tx.block_height) and the newest recorded digest
--     (ledger_digest.height) must not be above the head, and a recorded
--     digest must name the block that is there.  That catches the removal of
--     the last blocks from ledger_blocks alone.  It does not catch someone who
--     removes the same rows from the other tables too.
CREATE FUNCTION kingres.ledger_head()
    RETURNS TABLE (chain_id bytea, height bigint, block_hash bytea, closed_at_us bigint)
    LANGUAGE plpgsql
    STABLE
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    chain    bytea;
    b        record;
    first_pv bytea;
    prev_h   bytea;
    tx_top   bigint;
    dg       record;
    dg_hash  bytea;
BEGIN
    SELECT c.chain_id INTO chain FROM kingres.ledger_chain c;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'the identity of the chain is missing'
            USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
    END IF;
    SELECT lb.height AS bh, lb.hash_version AS hv, lb.prev_hash AS ph,
           lb.tx_merkle_root AS tr, lb.tx_count AS tc, lb.closed_at AS ca,
           lb.block_hash AS bhash
      INTO b
      FROM kingres.ledger_blocks lb
     ORDER BY lb.height DESC
     LIMIT 1;
    SELECT pg_catalog.max(t.block_height) INTO tx_top FROM kingres.ledger_tx t;
    SELECT d.height AS dh, d.block_hash AS dhash INTO dg
      FROM kingres.ledger_digest d
     ORDER BY d.digest_id DESC
     LIMIT 1;
    IF b.bh IS NULL THEN
        -- No block at all: empty, unless something still records a block.
        IF tx_top IS NOT NULL THEN
            RAISE EXCEPTION 'the chain has no block, but transactions of the ledger refer to block %', tx_top
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
        END IF;
        IF dg.dh IS NOT NULL THEN
            RAISE EXCEPTION 'the chain has no block, but a digest was recorded at block %', dg.dh
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
        END IF;
        RETURN;
    END IF;
    IF b.hv IS DISTINCT FROM 1
       OR pg_catalog.octet_length(b.ph) IS DISTINCT FROM 32
       OR pg_catalog.octet_length(b.tr) IS DISTINCT FROM 32
       OR pg_catalog.octet_length(b.bhash) IS DISTINCT FROM 32
       OR b.tc < 0 OR NOT pg_catalog.isfinite(b.ca)
       OR pg_catalog.sha256(kingres.ledger_block_preimage(b.hv, b.bh, b.ph, b.tr, b.tc, b.ca))
          IS DISTINCT FROM b.bhash THEN
        RAISE EXCEPTION 'the head, block %, is malformed or does not match its stored block hash', b.bh
            USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
    END IF;
    IF b.bh = 0 THEN
        first_pv := b.ph;
    ELSE
        SELECT g.prev_hash INTO first_pv FROM kingres.ledger_blocks g WHERE g.height = 0;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'block 0 is missing'
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
        END IF;
        SELECT p.block_hash INTO prev_h FROM kingres.ledger_blocks p WHERE p.height = b.bh - 1;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'block % is missing', b.bh - 1
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
        END IF;
        IF b.ph IS DISTINCT FROM prev_h THEN
            RAISE EXCEPTION 'the head, block %, does not follow the block before it', b.bh
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
        END IF;
    END IF;
    IF first_pv IS DISTINCT FROM kingres.ledger_genesis_hash(chain) THEN
        RAISE EXCEPTION 'the identity of the chain does not match block 0'
            USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
    END IF;
    IF tx_top > b.bh THEN
        RAISE EXCEPTION 'the head is block %, but transactions of the ledger refer to block %: the end of the chain was removed',
            b.bh, tx_top
            USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
    END IF;
    IF dg.dh IS NOT NULL THEN
        IF dg.dh > b.bh THEN
            RAISE EXCEPTION 'the head is block %, but a digest was recorded at block %: the end of the chain was removed',
                b.bh, dg.dh
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
        END IF;
        SELECT x.block_hash INTO dg_hash FROM kingres.ledger_blocks x WHERE x.height = dg.dh;
        IF dg_hash IS DISTINCT FROM dg.dhash THEN
            RAISE EXCEPTION 'block % is not the block a recorded digest names', dg.dh
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
        END IF;
    END IF;
    RETURN QUERY
    SELECT chain, b.bh, b.bhash, kingres.ledger_timestamptz_us(b.ca);
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_head() IS
    'The latest block: chain identity, height, block hash and close time in microseconds since 2000-01-01 UTC. No row for an empty ledger. Checks the head (hash recomputed, link to the block before, chain identity against block 0) and that no transaction row or recorded digest names a block above it, error XX001 otherwise; it cannot detect a chain that someone rewrote or shortened together with those other rows. Builds no block and records nothing. Not executable by PUBLIC.';

-- SECURITY DEFINER, like ledger_head, so that EXECUTE is all a service role
-- needs: an operator may REVOKE SELECT on the block headers and the chain
-- identity from PUBLIC (the README says publication is the operator's choice)
-- and the roles granted EXECUTE keep working.  It reads only
-- ledger_blocks and ledger_chain and writes nothing.  Every block it returns
-- is checked here: the stored hash version, lengths and block hash, the height
-- (no gap, no repeat), and the link to the block before (the genesis hash for
-- block 0), with SHA-256 from pg_catalog, not the extension's own hash.
-- When the range does not start at block 0, the block before it is read too
-- and its hash is recomputed from its fields, so that the link into the range
-- is checked against a hash that was itself checked; the chain identity is
-- compared with block 0 on every call that returns a row.  The first failure
-- raises XX001 and no row is returned, so a publisher that signs what it was
-- given never signs a block or a link that is wrong within what was read.
-- What it cannot see: blocks removed from the end (it reads only
-- ledger_blocks, see ledger_head for the check that catches that), and a
-- successor block that is fully valid.  The result holds at most
-- least(max_count, 10000) rows, in height order, starting at from_height; a
-- range past the head, or an empty chain, gives no rows; max_count = 0 gives
-- no rows; a negative argument is an error (22023); NULL gives no rows.
CREATE FUNCTION kingres.ledger_blocks_entries(from_height bigint, max_count int)
    RETURNS TABLE (height bigint, entry bytea)
    LANGUAGE plpgsql
    STABLE
    STRICT
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    r        record;
    pb       record;
    lim      int;
    expect   bigint;
    prev     bytea;
    pre      bytea;
    chain    bytea;
    first_pv bytea;
BEGIN
    IF from_height < 0 OR max_count < 0 THEN
        RAISE EXCEPTION 'from_height and max_count must not be negative'
            USING ERRCODE = '22023';
    END IF;
    lim := CASE WHEN max_count > 10000 THEN 10000 ELSE max_count END;
    IF lim = 0 THEN
        RETURN;
    END IF;
    expect := from_height;
    FOR r IN
        SELECT b.height AS bh, b.hash_version AS hv, b.prev_hash AS ph,
               b.tx_merkle_root AS tr, b.tx_count AS tc, b.closed_at AS ca,
               b.block_hash AS bhash
          FROM kingres.ledger_blocks b
         WHERE b.height >= from_height
         ORDER BY b.height
         LIMIT lim
    LOOP
        IF r.bh > expect THEN
            RAISE EXCEPTION 'block % is missing (found block %)', expect, r.bh
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
        ELSIF r.bh < expect THEN
            RAISE EXCEPTION 'block % is present more than once', r.bh
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
        END IF;
        IF expect = from_height THEN
            -- The chain identity, on every call: block 0 links to the genesis
            -- hash of the identity.
            SELECT c.chain_id INTO chain FROM kingres.ledger_chain c;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'the identity of the chain is missing'
                    USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
            END IF;
            IF from_height = 0 THEN
                first_pv := r.ph;
                prev := kingres.ledger_genesis_hash(chain);
            ELSE
                SELECT g.prev_hash INTO first_pv FROM kingres.ledger_blocks g WHERE g.height = 0;
                IF NOT FOUND THEN
                    RAISE EXCEPTION 'block 0 is missing'
                        USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
                END IF;
                -- The link into the range: the block before it, whose hash is
                -- recomputed from its fields and not taken as stored.
                SELECT b.hash_version AS hv, b.prev_hash AS ph, b.tx_merkle_root AS tr,
                       b.tx_count AS tc, b.closed_at AS ca, b.block_hash AS bhash
                  INTO pb
                  FROM kingres.ledger_blocks b
                 WHERE b.height = from_height - 1;
                IF NOT FOUND THEN
                    RAISE EXCEPTION 'block % is missing', from_height - 1
                        USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
                END IF;
                IF pb.hv IS DISTINCT FROM 1
                   OR pg_catalog.octet_length(pb.ph) IS DISTINCT FROM 32
                   OR pg_catalog.octet_length(pb.tr) IS DISTINCT FROM 32
                   OR pg_catalog.octet_length(pb.bhash) IS DISTINCT FROM 32
                   OR pb.tc < 0 OR NOT pg_catalog.isfinite(pb.ca)
                   OR pg_catalog.sha256(kingres.ledger_block_preimage(pb.hv, from_height - 1,
                                                                      pb.ph, pb.tr, pb.tc, pb.ca))
                      IS DISTINCT FROM pb.bhash THEN
                    RAISE EXCEPTION 'block % is malformed or does not match its stored block hash', from_height - 1
                        USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
                END IF;
                prev := pb.bhash;
            END IF;
            IF first_pv IS DISTINCT FROM kingres.ledger_genesis_hash(chain) THEN
                RAISE EXCEPTION 'the identity of the chain does not match block 0'
                    USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
            END IF;
        END IF;
        IF r.hv IS DISTINCT FROM 1
           OR pg_catalog.octet_length(r.ph) IS DISTINCT FROM 32
           OR pg_catalog.octet_length(r.tr) IS DISTINCT FROM 32
           OR pg_catalog.octet_length(r.bhash) IS DISTINCT FROM 32
           OR r.tc < 0 OR NOT pg_catalog.isfinite(r.ca)
           OR r.ph IS DISTINCT FROM prev THEN
            RAISE EXCEPTION 'block % is malformed or does not follow the block before it', r.bh
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
        END IF;
        pre := kingres.ledger_block_preimage(r.hv, r.bh, r.ph, r.tr, r.tc, r.ca);
        IF pg_catalog.sha256(pre) IS DISTINCT FROM r.bhash THEN
            RAISE EXCEPTION 'block % does not match its stored block hash', r.bh
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
        END IF;
        prev := r.bhash;
        expect := expect + 1;
        height := r.bh;
        entry := pre;
        RETURN NEXT;
    END LOOP;
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_blocks_entries(bigint, int) IS
    'The block hash preimages (87 bytes each, the leaves of the chain log) of at most least(max_count, 10000) blocks from from_height, in height order. Each block is checked against its stored hash and its place in the chain, the block before the range is recomputed, and the chain identity is compared with block 0; error XX001 if anything is wrong within what is read. It cannot see blocks removed from the end or a fully valid successor: compare the head from ledger_head with what you published. Empty for a range past the head or an empty chain. Not executable by PUBLIC.';

REVOKE ALL ON FUNCTION kingres.ledger_head() FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_blocks_entries(bigint, int) FROM PUBLIC;
