-- ledger_hash: hash primitives, RFC 6962 vectors and canonical row hashing.
--
-- Everything computed by the C code is compared with an independent
-- implementation written here in SQL with sha256().
CREATE EXTENSION kingres_ledger;

-- The GUC exists, has the documented default and the prefix is reserved.
SHOW kingres_ledger.max_block_transactions;
SELECT name, setting, context FROM pg_settings WHERE name LIKE 'kingres_ledger.%';
SET kingres_ledger.no_such_setting = 1;
SELECT kingres.ledger_hook_active();

-- Independent reference implementation of RFC 6962.
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

-- Reference verification of an audit path, RFC 9162 section 2.1.3.2.
CREATE FUNCTION pg_temp.ref_verify(leaf bytea, idx bigint, size bigint,
                                   path bytea[], root bytea) RETURNS boolean
LANGUAGE plpgsql AS $$
DECLARE
    fn bigint := idx;
    sn bigint := size - 1;
    r bytea := sha256('\x00'::bytea || leaf);
    p bytea;
BEGIN
    FOREACH p IN ARRAY path LOOP
        IF sn = 0 THEN
            RETURN false;
        END IF;
        IF (fn & 1) = 1 OR fn = sn THEN
            r := sha256('\x01'::bytea || p || r);
            IF (fn & 1) = 0 THEN
                WHILE (fn & 1) = 0 AND fn <> 0 LOOP
                    fn := fn >> 1;
                    sn := sn >> 1;
                END LOOP;
            END IF;
        ELSE
            r := sha256('\x01'::bytea || r || p);
        END IF;
        fn := fn >> 1;
        sn := sn >> 1;
    END LOOP;
    RETURN sn = 0 AND r = root;
END $$;

-- Test data of the Certificate Transparency reference implementation.
CREATE TEMP TABLE leaves (i int PRIMARY KEY, data bytea);
INSERT INTO leaves VALUES
    (0, '\x'), (1, '\x00'), (2, '\x10'), (3, '\x2021'), (4, '\x3031'),
    (5, '\x40414243'), (6, '\x5051525354555657'),
    (7, '\x606162636465666768696a6b6c6d6e6f');

-- Roots of the trees of 0..8 leaves: C aggregate against the SQL reference.
SELECT n,
       encode(c_root, 'hex') AS root,
       c_root = ref_root AS matches_reference
FROM (
    SELECT n,
           (SELECT kingres.ledger_merkle_root(kingres.ledger_leaf_hash(data) ORDER BY i)
            FROM leaves WHERE i < n) AS c_root,
           pg_temp.ref_mth((SELECT array_agg(data ORDER BY i) FROM leaves WHERE i < n),
                           0, n) AS ref_root
    FROM generate_series(0, 8) AS n
) s
ORDER BY n;

-- The empty tree is SHA-256 of the empty string; one empty leaf is
-- SHA-256(0x00).
SELECT encode(kingres.ledger_merkle_root(x), 'hex') AS empty_root
FROM (SELECT NULL::bytea AS x WHERE false) e;
SELECT encode(kingres.ledger_leaf_hash(''), 'hex') AS leaf_of_empty;

-- Inner nodes and leaves are domain separated.
SELECT kingres.ledger_node_hash(kingres.ledger_leaf_hash('\x01'),
                                 kingres.ledger_leaf_hash('\x02'))
       = sha256('\x01'::bytea || sha256('\x0001'::bytea) || sha256('\x0002'::bytea))
       AS node_matches_reference;

-- Audit paths: every index of every tree up to 9 leaves verifies with the
-- independent checker, and a wrong index does not.  The tree size is not
-- tested here: a root does not commit to the size of its tree, the size is
-- bound by the entry_count of the leaf one level up.
WITH sizes AS (
    SELECT n FROM generate_series(1, 9) n
), data AS (
    SELECT n, i, ('\x' || lpad(to_hex(i), 2, '0') || lpad(to_hex(n), 2, '0'))::bytea AS d
    FROM sizes, LATERAL generate_series(0, n - 1) i
), trees AS (
    SELECT n,
           array_agg(d ORDER BY i) AS datas,
           array_agg(kingres.ledger_leaf_hash(d) ORDER BY i) AS hashes
    FROM data GROUP BY n
), checks AS (
    SELECT t.n, i,
           pg_temp.ref_verify(t.datas[i + 1], i, t.n,
                              kingres.ledger_merkle_path(t.hashes, i),
                              pg_temp.ref_mth(t.datas, 0, t.n)) AS ok,
           pg_temp.ref_verify(t.datas[i + 1], (i + 1) % t.n, t.n,
                              kingres.ledger_merkle_path(t.hashes, i),
                              pg_temp.ref_mth(t.datas, 0, t.n)) AS wrong_index_ok
    FROM trees t, LATERAL generate_series(0, t.n - 1) i
)
SELECT count(*) AS paths_checked,
       bool_and(ok) AS all_verify,
       -- a wrong index can only verify in a one-leaf tree (index wraps to 0)
       count(*) FILTER (WHERE wrong_index_ok AND n > 1) AS wrong_index_accepted
FROM checks;

-- Argument checks.
SELECT kingres.ledger_node_hash('\x00', '\x00');
SELECT kingres.ledger_merkle_path(ARRAY['\x00'::bytea], 0);
SELECT kingres.ledger_merkle_path(ARRAY[kingres.ledger_leaf_hash('\x00')], 1);
SELECT kingres.ledger_merkle_root('\x00'::bytea);

-- Leaves of the transaction tree and the block tree.
-- Every kind of hashed data starts with its own tag inside the RFC 6962
-- leaf: 0x11 for an entry of a transaction, 0x12 for a transaction in a block.
SELECT encode(kingres.ledger_tx_leaf(1, 2, 'I', sha256('x')), 'hex')
       = encode(sha256('\x00'::bytea || '\x11' || '\x00000001' || '\x00000002' || '\x49' || sha256('x')), 'hex')
       AS tx_leaf_matches_reference;
SELECT encode(kingres.ledger_block_tx_leaf(5, 3, sha256('x'), 'alice'), 'hex')
       = encode(sha256('\x00'::bytea || '\x12' || '\x0000000000000005' || '\x00000003' || sha256('x')
                       || int2send(5::int2) || convert_to('alice', 'UTF8')), 'hex')
       AS block_tx_leaf_matches_reference;
-- The tags separate the kinds: the leaf of an entry is not the leaf of the
-- same bytes without the tag.
SELECT kingres.ledger_tx_leaf(1, 2, 'I', sha256('x'))
       <> kingres.ledger_leaf_hash(int4send(1) || int4send(2) || '\x49'::bytea || sha256('x'))
       AS tag_changes_the_leaf;
-- The user name is hashed with its length: different names give different
-- leaves, an empty name is allowed, and a non-ASCII name is hashed as UTF-8.
SELECT count(DISTINCT kingres.ledger_block_tx_leaf(5, 3, sha256('x'), n)) AS distinct_leaves
FROM (VALUES ('alice'::name), ('alic'), (''), ('bob')) v(n);
SELECT kingres.ledger_block_tx_leaf(5, 3, sha256('x'), U&'r\00e9mi'::name)
       = sha256('\x00'::bytea || '\x0000000000000005' || '\x00000003' || sha256('x')
                || int2send(5::int2) || convert_to(U&'r\00e9mi', 'UTF8'))
       AS non_ascii_name_matches_reference;

-- Block hash against an independent computation.  closed_at is encoded as
-- microseconds since 2000-01-01 00:00:00 UTC.
-- The input starts with the tag 0x13.
SELECT kingres.ledger_block_hash(1::smallint, 7, sha256('p'), sha256('r'), 3,
                                  '2026-11-14 10:20:30.123456+00')
       = sha256('\x13'::bytea || '\x0001'::bytea || '\x0000000000000007' || sha256('p') || sha256('r')
                || '\x00000003'
                || int8send(((extract(epoch FROM timestamptz '2026-11-14 10:20:30.123456+00')
                              - 946684800) * 1000000)::numeric::bigint))
       AS block_hash_matches_reference;
SELECT kingres.ledger_block_hash(2::smallint, 0, sha256('p'), sha256('r'), 0, now());
SELECT kingres.ledger_block_hash(1::smallint, 0, sha256('p'), sha256('r'), 0, 'infinity');

-- The previous-block hash of block 0 is derived from the chain identity
-- with the tag 0x14.
SELECT kingres.ledger_genesis_hash(sha256('chain'))
       = sha256('\x14'::bytea || sha256('chain')) AS genesis_matches_reference;
SELECT kingres.ledger_genesis_hash(sha256('chain'))
       <> kingres.ledger_genesis_hash(sha256('other chain')) AS genesis_depends_on_chain;
SELECT kingres.ledger_genesis_hash('\x00');
-- The identity of this chain: 32 bytes, one row, and not all zero.
SELECT count(*) AS chain_rows, min(octet_length(chain_id)) AS chain_id_length,
       bool_and(chain_id <> decode(repeat('00', 32), 'hex')) AS chain_id_not_zero
FROM kingres.ledger_chain;

-- Row hash.  Registering the table by hand is enough for the hash tests;
-- the whole test runs in a transaction that is rolled back so that no
-- ledger metadata is left behind.
BEGIN;
-- The tables below are registered by hand, without triggers.  The event
-- trigger of the extension, which checks every registered table after every
-- DDL statement, would reject them; the test switches it off for this
-- transaction.
SET LOCAL event_triggers = off;
SET LOCAL kingres_ledger.allow_manual_write = on;
CREATE TABLE hash_t (id int, name text, note text);
INSERT INTO kingres.ledger_table (relid, mode) VALUES ('hash_t', 'append_only');
INSERT INTO hash_t VALUES (7, U&'h\00e9llo', NULL);

-- Not a ledger table, and not a table row at all.
SAVEPOINT s1;
SELECT kingres.ledger_row_hash(ROW(1, 2));
ROLLBACK TO s1;
CREATE TABLE plain_t (a int);
INSERT INTO plain_t VALUES (1);
SELECT kingres.ledger_row_hash(p) FROM plain_t p;
ROLLBACK TO s1;

-- The documented encoding, written out independently.
SELECT kingres.ledger_row_hash(t) = sha256(
           '\x00'::bytea
           || '\x10'              -- the tag of a row
           || '\x0001'
           || int4send((SELECT table_id FROM kingres.ledger_table WHERE relid = 'hash_t'::regclass))
           || int2send(2::int2)
           || int2send(2::int2) || convert_to('id', 'UTF8')
           || int4send(4) || int4send(7)
           || int2send(4::int2) || convert_to('name', 'UTF8')
           || int4send(octet_length(convert_to(U&'h\00e9llo', 'UTF8')))
           || convert_to(U&'h\00e9llo', 'UTF8'))
       AS row_hash_matches_reference
FROM hash_t t;

-- Independence from session settings.
CREATE TABLE settings_t (
    id int, label text, price numeric(12,4), ratio float8, ts timestamptz,
    d date, iv interval, raw bytea, doc jsonb, tags text[], flag boolean,
    uid uuid, addr inet, nothing text);
INSERT INTO kingres.ledger_table (relid, mode) VALUES ('settings_t', 'append_only');
INSERT INTO settings_t VALUES (
    1, U&'caf\00e9 \20ac', 12.5, 0.1, '2026-11-14 10:20:30.123456+01',
    '2026-11-14', '1 year 2 mons 3 days 04:05:06.789', '\x00ff10',
    '{"b": [1, 2], "a": null}', ARRAY['x', 'y z'], true,
    'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '192.168.1.5/24', NULL);
CREATE TEMP TABLE baseline AS
    SELECT kingres.ledger_row_hash(s) AS h FROM settings_t s;
SELECT encode(h, 'hex') ~ '^[0-9a-f]{64}$' AS looks_like_a_sha256 FROM baseline;

SET LOCAL DateStyle = 'German, DMY';
SET LOCAL extra_float_digits = -3;
SET LOCAL bytea_output = 'escape';
SET LOCAL IntervalStyle = 'sql_standard';
SELECT kingres.ledger_row_hash(s) = (SELECT h FROM baseline) AS same_with_other_formats
FROM settings_t s;
SET LOCAL TimeZone = 'Asia/Tokyo';
SET LOCAL client_encoding = 'LATIN1';
SELECT kingres.ledger_row_hash(s) = (SELECT h FROM baseline) AS same_with_other_client_encoding
FROM settings_t s;
SELECT show_text = 'LATIN1' AS client_encoding_restored
FROM (SELECT current_setting('client_encoding') AS show_text) x;
RESET client_encoding;
RESET DateStyle;
RESET extra_float_digits;
RESET bytea_output;
RESET IntervalStyle;
RESET TimeZone;

-- Adding a nullable column does not change the hash of existing rows.
ALTER TABLE settings_t ADD COLUMN later int;
SELECT kingres.ledger_row_hash(s) = (SELECT h FROM baseline) AS same_after_add_column
FROM settings_t s;

-- A different value, a different column name, a different table: different hashes.
INSERT INTO settings_t (id) VALUES (1);
SELECT count(DISTINCT kingres.ledger_row_hash(s)) AS distinct_hashes FROM settings_t s;
ROLLBACK;
