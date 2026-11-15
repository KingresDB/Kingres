-- ledger_preimage: kingres.ledger_block_preimage, the 87 bytes a block hash
-- covers (the leaf of the chain log of the publication design).  Compared
-- with an independent layout written here with the byte senders of the
-- server, with the stored blocks of the database, and with ledger_block_hash.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
SET search_path = public;
\set VERBOSITY terse

-- The independent layout: tag 0x13, u16 version, u64 height, prev_hash,
-- tx_merkle_root, u32 tx_count, i64 microseconds since 2000-01-01 UTC.  The
-- microseconds come from extract(epoch ...), not from the extension.
CREATE FUNCTION pg_temp.ref_pre(v int2, h int8, p bytea, r bytea, n int4, t timestamptz)
RETURNS bytea LANGUAGE sql IMMUTABLE AS $$
    SELECT '\x13'::bytea || int2send(v) || int8send(h) || p || r || int4send(n)
           || int8send(round((extract(epoch FROM t) - 946684800) * 1000000)::int8)
$$;

CREATE TEMP TABLE cases (id int, v int2, h int8, p bytea, r bytea, n int4, t timestamptz);
INSERT INTO cases VALUES
  (1, 1, 0, decode(repeat('00', 32), 'hex'), decode(repeat('11', 32), 'hex'), 3,
      '2026-01-02 03:04:05.678901+00'),
  (2, 1, 1, sha256('p'), sha256('r'), 0, '2000-01-01 00:00:00+00'),
  (3, 1, 9223372036854775807, sha256('p2'), sha256('r2'), 2147483647, '2038-01-19 03:14:08.000001+00'),
  (4, 1, 4294967296, sha256('p3'), sha256('r3'), 1, '1999-12-31 23:59:59.999999+00'),
  (5, 1, 255, sha256('p4'), sha256('r4'), 256, '1970-01-01 00:00:00+00'),
  (6, 1, 70000, sha256('p5'), sha256('r5'), 65536, '2200-06-30 12:00:00.5+00');

SELECT c.id,
       octet_length(kingres.ledger_block_preimage(c.v, c.h, c.p, c.r, c.n, c.t)) AS len,
       kingres.ledger_block_preimage(c.v, c.h, c.p, c.r, c.n, c.t) = pg_temp.ref_pre(c.v, c.h, c.p, c.r, c.n, c.t) AS same_as_reference,
       sha256(kingres.ledger_block_preimage(c.v, c.h, c.p, c.r, c.n, c.t)) = kingres.ledger_block_hash(c.v, c.h, c.p, c.r, c.n, c.t) AS hashes_to_block_hash
FROM cases c ORDER BY c.id;

-- One value written out, so that a change of the layout cannot go unnoticed.
SELECT encode(kingres.ledger_block_preimage(1::smallint, 0, decode(repeat('00', 32), 'hex'),
              decode(repeat('11', 32), 'hex'), 3, '2000-01-01 00:00:01+00'), 'hex') AS pinned;

-- Every block this database holds: the stored hash is the hash of the preimage.
SELECT count(*) > 0 AS has_blocks,
       count(*) = count(*) FILTER (WHERE sha256(kingres.ledger_block_preimage(b.hash_version, b.height, b.prev_hash,
                                      b.tx_merkle_root, b.tx_count, b.closed_at)) = b.block_hash) AS all_preimages_hash_to_stored,
       count(*) = count(*) FILTER (WHERE kingres.ledger_block_preimage(b.hash_version, b.height, b.prev_hash,
                                      b.tx_merkle_root, b.tx_count, b.closed_at)
                              = pg_temp.ref_pre(b.hash_version, b.height, b.prev_hash,
                                      b.tx_merkle_root, b.tx_count, b.closed_at)) AS all_preimages_are_reference
FROM kingres.ledger_blocks b;

-- Arguments that are refused, with the messages of ledger_block_hash.
SELECT kingres.ledger_block_preimage(2::smallint, 0, sha256('a'), sha256('b'), 1, now());
SELECT kingres.ledger_block_preimage(1::smallint, -1, sha256('a'), sha256('b'), 1, now());
SELECT kingres.ledger_block_preimage(1::smallint, 0, sha256('a'), sha256('b'), -1, now());
SELECT kingres.ledger_block_preimage(1::smallint, 0, sha256('a'), sha256('b'), 1, 'infinity');
SELECT kingres.ledger_block_preimage(1::smallint, 0, sha256('a'), sha256('b'), 1, '-infinity');
SELECT kingres.ledger_block_preimage(1::smallint, 0, '\x00', sha256('b'), 1, now());
SELECT kingres.ledger_block_preimage(1::smallint, 0, sha256('a'), '\x00', 1, now());
-- STRICT: NULL in, NULL out.
SELECT kingres.ledger_block_preimage(1::smallint, NULL, sha256('a'), sha256('b'), 1, now()) IS NULL AS null_in_null_out;
-- The two functions refuse the same arguments the same way.
SELECT kingres.ledger_block_hash(2::smallint, 0, sha256('a'), sha256('b'), 1, now());
SELECT kingres.ledger_block_hash(1::smallint, 0, '\x00', sha256('b'), 1, now());

-- A role with no privileges may call it: it computes on its arguments only.
SELECT p.provolatile, p.proparallel, p.proisstrict, p.prosecdef,
       p.proacl IS NULL OR has_function_privilege('public', p.oid, 'execute') AS public_can_execute
FROM pg_proc p WHERE p.oid = 'kingres.ledger_block_preimage(smallint,bigint,bytea,bytea,int,timestamptz)'::regprocedure;
CREATE ROLE kl_pre_nobody;
GRANT USAGE ON SCHEMA public TO kl_pre_nobody;
SET ROLE kl_pre_nobody;
SELECT octet_length(kingres.ledger_block_preimage(1::smallint, 0, sha256('a'), sha256('b'), 1, now())) AS len_as_nobody;
RESET ROLE;
DROP OWNED BY kl_pre_nobody;
DROP ROLE kl_pre_nobody;
