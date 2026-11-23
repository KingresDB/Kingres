-- kingres_ledger--0.3.sql
--
-- Install script of the kingres_ledger extension, version 0.3: the data
-- model, the C primitives and the SQL orchestration functions of the ledger
-- described in kingres/docs/design/ledger-mvp.md (append-only tables) and
-- kingres/docs/design/ledger-0.2.md (updatable tables with a history table,
-- the block-building workers, the records for operators).
--
-- Version 0.3 part A adds three read-only functions for tile publication
-- (kingres/docs/design/ledger-phase7.md, 8.1) and the write guard (src/guard.c):
-- the ledger tables are written by the code of the extension only, through
-- the C entry points that compute what they write (ledger_build_block and the
-- other functions of src/write.c, the capture trigger), and by a superuser who
-- turned allow_manual_write on.  Hash format 1 and every earlier object are as in 0.2.
--
-- Part B adds client signatures (src/sign.c; design 3.6 to 3.8 and 8.1): three
-- tables that are ledger tables themselves (ledger_client_key,
-- ledger_client_key_revocation, ledger_attestation), registered by
-- ledger_register_internal while this script runs, the functions that write them,
-- two columns of ledger_table (signature_policy, signature_required_from), the
-- checks of ledger_verify and the columns of ledger_status that go with them.
--
-- kingres_ledger--0.1--0.2.sql and kingres_ledger--0.2--0.3.sql upgrade the
-- earlier versions to the objects this script creates; the tests
-- t/007_upgrade.pl and t/011_upgrade_0_3.pl compare the results.  Anything
-- changed here must be changed there, in the same order for column
-- positions (new columns are appended last in all).
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- Schema: the control file sets "schema = kingres" and "relocatable =
-- false".  CREATE EXTENSION then creates the schema when it does not exist
-- and uses it when it does (for example because kingres_shard is already
-- installed), and in both cases the schema is not a member of this
-- extension, so DROP EXTENSION never removes a schema that another extension
-- also uses.  Every object created here is qualified with the schema name
-- and its name starts with "ledger_".
--
-- Conventions for every function of this script:
--   * Functions that look up any object (a table, a function, an operator)
--     carry "SET search_path = pg_catalog, pg_temp".  pg_temp comes last, so
--     a caller cannot shadow anything by creating a temporary object; every
--     reference to an object of the extension is qualified with "kingres.".
--     The only functions without a search_path are the C primitives that
--     compute on their arguments and look nothing up; the test ledger_privs
--     keeps that list.
--   * SECURITY DEFINER is used only where the function has to do something
--     the caller may not: each such function says why in the comment above
--     it.  All of them are owned by the role that created the extension,
--     which has to be a superuser.
--   * Nothing here relies on a row alias or a variable name that a column of
--     a user table could shadow: queries over user tables are written with
--     the alias "kl_r" and "kl_r.*", which names the whole row whatever the
--     columns are called.

\echo Use "CREATE EXTENSION kingres_ledger" to load this file. \quit

-- The schema is the extension's own: when it exists already (a role may have made it in advance, and "schema = ..." in the control file uses an existing
-- schema) it must be owned by a superuser and give CREATE to no role that is not a superuser.  Otherwise that role would own what the service role, a
-- superuser, reads and runs (the same check as extguard in the control plane, which refuses such a schema at run time too).
DO $kgchk$
DECLARE
    v_schema pg_catalog.text := '@extschema@';
    v_owner  pg_catalog.name;
BEGIN
    SELECT r.rolname INTO v_owner
      FROM pg_catalog.pg_namespace n JOIN pg_catalog.pg_roles r ON r.oid OPERATOR(pg_catalog.=) n.nspowner
     WHERE n.nspname OPERATOR(pg_catalog.=) v_schema AND NOT r.rolsuper;
    IF v_owner IS NOT NULL THEN
        RAISE EXCEPTION 'the schema % already exists and is owned by %, which is not a superuser', v_schema, v_owner
            USING ERRCODE = 'insufficient_privilege',
                  HINT = 'Drop the schema, or make a superuser its owner, before CREATE EXTENSION.';
    END IF;
    IF EXISTS (SELECT 1
                 FROM pg_catalog.pg_namespace n,
                      LATERAL pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) a
                WHERE n.nspname OPERATOR(pg_catalog.=) v_schema
                  AND a.privilege_type OPERATOR(pg_catalog.=) 'CREATE'
                  AND (a.grantee OPERATOR(pg_catalog.=) 0
                       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles g WHERE g.oid OPERATOR(pg_catalog.=) a.grantee AND g.rolsuper))) THEN
        RAISE EXCEPTION 'the schema % grants CREATE to a role that is not a superuser (or to PUBLIC)', v_schema
            USING ERRCODE = 'insufficient_privilege',
                  HINT = 'Revoke CREATE on the schema before CREATE EXTENSION.';
    END IF;
END
$kgchk$;

-- Callers need USAGE on the schema to call the public functions.
GRANT USAGE ON SCHEMA kingres TO PUBLIC;

--------------------------------------------------------------------------
-- Data model
--------------------------------------------------------------------------

-- Tables under ledger control.  table_id is the stable identity used inside
-- hashes; a regclass OID is not stable across dump and restore.
CREATE TABLE kingres.ledger_table (
    table_id   int GENERATED ALWAYS AS IDENTITY,
    relid      regclass NOT NULL,
    mode       text NOT NULL,
    enabled_at timestamptz NOT NULL DEFAULT now(),
    -- The history table of an updatable table; NULL for an append-only one.
    -- This column must stay the fifth: protect.c reads it by position.
    history_relid regclass,
    -- The signature policy (0.3, part B): the sixth and seventh columns.  The
    -- policy only goes from optional to required.  signature_required_from is
    -- the largest transaction identifier that is not asked for an attestation:
    -- ledger_set_signature_policy reads it under the lock that orders the switch
    -- with the writers of the table (the identifier that the next transaction
    -- gets, less one), and ledger_verify asks for an attestation of the entries
    -- of the transactions above it.  Not covered by a hash.
    signature_policy text NOT NULL DEFAULT 'optional',
    signature_required_from bigint,
    CONSTRAINT ledger_table_pkey PRIMARY KEY (table_id),
    CONSTRAINT ledger_table_relid_key UNIQUE (relid),
    CONSTRAINT ledger_table_history_relid_key UNIQUE (history_relid),
    CONSTRAINT ledger_table_mode_check CHECK (mode IN ('append_only', 'updatable')),
    CONSTRAINT ledger_table_history_check
        CHECK ((mode = 'updatable') = (history_relid IS NOT NULL)),
    CONSTRAINT ledger_table_signature_policy_check
        CHECK (signature_policy IN ('optional', 'required')),
    CONSTRAINT ledger_table_signature_from_check
        CHECK ((signature_policy = 'required') = (signature_required_from IS NOT NULL))
);

-- The identity of this chain: one row, random, written when the extension is
-- created.  The previous-block hash of block 0 is derived from it, and every
-- digest and proof carries it.
CREATE TABLE kingres.ledger_chain (
    singleton  boolean NOT NULL DEFAULT true,
    chain_id   bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ledger_chain_pkey PRIMARY KEY (singleton),
    CONSTRAINT ledger_chain_singleton_check CHECK (singleton),
    CONSTRAINT ledger_chain_id_check CHECK (octet_length(chain_id) = 32)
);

-- One row per row inserted into a ledger table.
CREATE TABLE kingres.ledger_entry (
    ledger_txid bigint NOT NULL,
    seq         int    NOT NULL,
    table_id    int    NOT NULL,
    op          "char" NOT NULL,
    row_hash    bytea  NOT NULL,
    CONSTRAINT ledger_entry_pkey PRIMARY KEY (ledger_txid, seq),
    CONSTRAINT ledger_entry_table_id_fkey
        FOREIGN KEY (table_id) REFERENCES kingres.ledger_table (table_id),
    CONSTRAINT ledger_entry_op_check CHECK (op IN ('I', 'D')),
    CONSTRAINT ledger_entry_row_hash_check CHECK (octet_length(row_hash) = 32)
);
-- Lookup of an entry by row hash (inclusion proofs).
CREATE INDEX ledger_entry_row_hash_idx
    ON kingres.ledger_entry (table_id, row_hash);

-- Transactions that have entries but are not in a block yet.  This is a work
-- queue, not part of the ledger: the block builder deletes from it.
CREATE TABLE kingres.ledger_pending (
    ledger_txid bigint NOT NULL,
    user_name   name   NOT NULL,
    -- When the transaction wrote its first entry; NULL for a row that was
    -- queued by version 0.1.  Not covered by any hash: it only lets
    -- ledger_status show how long the oldest transaction has waited.
    queued_at   timestamptz DEFAULT clock_timestamp(),
    CONSTRAINT ledger_pending_pkey PRIMARY KEY (ledger_txid)
);

-- One row per transaction, written by the block builder.
CREATE TABLE kingres.ledger_tx (
    ledger_txid  bigint NOT NULL,
    block_height bigint NOT NULL,
    tx_index     int    NOT NULL,
    entry_count  int    NOT NULL,
    merkle_root  bytea  NOT NULL,
    user_name    name   NOT NULL,
    CONSTRAINT ledger_tx_pkey PRIMARY KEY (ledger_txid),
    CONSTRAINT ledger_tx_block_height_tx_index_key
        UNIQUE (block_height, tx_index)
);

CREATE TABLE kingres.ledger_blocks (
    height         bigint      NOT NULL,
    hash_version   smallint    NOT NULL,
    prev_hash      bytea       NOT NULL,
    tx_merkle_root bytea       NOT NULL,
    tx_count       int         NOT NULL,
    closed_at      timestamptz NOT NULL,
    block_hash     bytea       NOT NULL,
    CONSTRAINT ledger_blocks_pkey PRIMARY KEY (height)
);

-- Every digest this database has generated.  A convenience record, NOT a
-- root of trust: a digest proves something only when a copy was kept outside
-- the database.
CREATE TABLE kingres.ledger_digest (
    digest_id    bigint GENERATED ALWAYS AS IDENTITY,
    generated_at timestamptz NOT NULL,
    generated_by name        NOT NULL,
    height       bigint      NOT NULL,
    block_hash   bytea       NOT NULL,
    digest       jsonb       NOT NULL,
    CONSTRAINT ledger_digest_pkey PRIMARY KEY (digest_id),
    CONSTRAINT ledger_digest_block_hash_check CHECK (octet_length(block_hash) = 32)
);

-- Every verification run through ledger_verify_recorded and its outcome.
CREATE TABLE kingres.ledger_verification (
    verification_id bigint GENERATED ALWAYS AS IDENTITY,
    started_at      timestamptz NOT NULL,
    finished_at     timestamptz,
    started_by      name        NOT NULL,
    from_height     bigint      NOT NULL,
    to_height       bigint,
    state           text        NOT NULL,
    problem_count   int,
    blocks_checked  bigint,
    rows_checked    bigint,
    problems        jsonb,
    CONSTRAINT ledger_verification_pkey PRIMARY KEY (verification_id),
    CONSTRAINT ledger_verification_state_check
        CHECK (state IN ('running', 'clean', 'problems', 'failed'))
);

-- The rows that an operator took out of the ledger with ledger_remove_stray,
-- as they were, with who did it, when and why.  Append-only like the others;
-- ledger_verify reports every row of it (operator_removal) for as long as it
-- exists, so a removal is never silent.  Not readable by PUBLIC: the rows are
-- copies of ledger rows.
CREATE TABLE kingres.ledger_removed (
    removal_id   bigint GENERATED ALWAYS AS IDENTITY,
    removed_at   timestamptz NOT NULL,
    removed_by   name        NOT NULL,
    reason       text        NOT NULL,
    source_table name        NOT NULL,
    row_data     jsonb       NOT NULL,
    action       text        NOT NULL DEFAULT 'removed',
    CONSTRAINT ledger_removed_pkey PRIMARY KEY (removal_id),
    CONSTRAINT ledger_removed_source_check
        CHECK (source_table IN ('ledger_table', 'ledger_chain', 'ledger_entry', 'ledger_tx',
                                'ledger_blocks', 'ledger_digest', 'ledger_verification',
                                'ledger_client_key', 'ledger_client_key_revocation',
                                'ledger_attestation')),
    CONSTRAINT ledger_removed_action_check
        CHECK (action IN ('removed', 'inserted'))
);

-- The largest digest identifier that ledger_remove_stray took out (ledger_generate_digest
-- never gives it again), read under the lock of the builders: an index, so that the
-- read does not grow with every manual write and every removal (L15-09).  The expression
-- uses built-in operators only.
CREATE INDEX ledger_removed_digest_idx ON kingres.ledger_removed
    ((((row_data OPERATOR(pg_catalog.->>) 'digest_id'))::pg_catalog.int8))
    WHERE source_table OPERATOR(pg_catalog.=) 'ledger_digest'
      AND action OPERATOR(pg_catalog.=) 'removed';

COMMENT ON TABLE kingres.ledger_table IS
    'Tables under ledger control; table_id is the identity used inside hashes.';
COMMENT ON TABLE kingres.ledger_chain IS
    'The random identity of this chain, one row; block 0 derives its prev_hash from it.';
COMMENT ON TABLE kingres.ledger_entry IS
    'One row per row inserted into a ledger table; append-only.';
COMMENT ON TABLE kingres.ledger_pending IS
    'Committed transactions with ledger entries that are not in a block yet; work queue of the block builder.';
COMMENT ON TABLE kingres.ledger_tx IS
    'One row per ledger transaction, written by the block builder; append-only.';
COMMENT ON TABLE kingres.ledger_blocks IS
    'The hash chain of ledger blocks; append-only.';
COMMENT ON TABLE kingres.ledger_digest IS
    'Every digest this database generated; a convenience record, not a root of trust: keep a copy outside the database.';
COMMENT ON TABLE kingres.ledger_verification IS
    'Every verification run through ledger_verify_recorded and its outcome; append-only.';
COMMENT ON TABLE kingres.ledger_removed IS
    'Rows that an operator removed from the ledger tables with ledger_remove_stray, as they were (action removed), and the rows that a superuser inserted by hand with kingres_ledger.allow_manual_write on (action inserted); append-only, and reported by ledger_verify as operator_removal and manual_write.';

-- Who may read what.  The tables that only describe the chain itself (the
-- registered tables, the chain identity and the block headers) are readable
-- by everybody.  The entries, the work queue and the transactions are not:
-- they hold the row hashes (a hash of a row with little entropy can be
-- confirmed by guessing the row), the identities of the tables and the names
-- of the roles that wrote, and when.  An auditor role needs
--   GRANT SELECT ON kingres.ledger_entry, kingres.ledger_pending,
--                   kingres.ledger_tx, kingres.ledger_verification TO auditor;
-- only to read them directly; the verification functions below run with the
-- rights of the extension owner and need only EXECUTE.
--
-- ledger_digest holds what a digest holds, which is block-level data (the
-- chain identity, the height and hash of a block, when the digest was made),
-- and is readable by PUBLIC except the column generated_by, which names the
-- role that made it.  ledger_verification is not readable by PUBLIC: the
-- problems of a run name tables and row hashes and transaction ids.
REVOKE ALL ON kingres.ledger_table, kingres.ledger_chain,
    kingres.ledger_entry, kingres.ledger_pending, kingres.ledger_tx,
    kingres.ledger_blocks, kingres.ledger_digest, kingres.ledger_verification,
    kingres.ledger_removed
    FROM PUBLIC;
GRANT SELECT ON kingres.ledger_table, kingres.ledger_chain,
    kingres.ledger_blocks
    TO PUBLIC;
GRANT SELECT (digest_id, generated_at, height, block_hash, digest)
    ON kingres.ledger_digest TO PUBLIC;

-- The identity of the chain: 32 random bytes (two random UUIDs, hashed).
INSERT INTO kingres.ledger_chain (chain_id)
VALUES (pg_catalog.sha256(
            pg_catalog.uuid_send(pg_catalog.gen_random_uuid())
            || pg_catalog.uuid_send(pg_catalog.gen_random_uuid())));

--------------------------------------------------------------------------
-- C primitives: hashing
--------------------------------------------------------------------------

-- Pure functions of their arguments carry no search_path setting: they look
-- up no object by name.

CREATE FUNCTION kingres.ledger_leaf_hash(data bytea) RETURNS bytea
    AS 'MODULE_PATHNAME', 'kingres_ledger_leaf_hash'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_leaf_hash(bytea) IS
    'RFC 6962 leaf hash: SHA-256(0x00 || data).';

CREATE FUNCTION kingres.ledger_node_hash(left_hash bytea, right_hash bytea)
    RETURNS bytea
    AS 'MODULE_PATHNAME', 'kingres_ledger_node_hash'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_node_hash(bytea, bytea) IS
    'RFC 6962 inner node hash: SHA-256(0x01 || left || right).';

CREATE FUNCTION kingres.ledger_merkle_accum(internal, bytea) RETURNS internal
    AS 'MODULE_PATHNAME', 'kingres_ledger_merkle_accum'
    LANGUAGE C PARALLEL UNSAFE;

CREATE FUNCTION kingres.ledger_merkle_final(internal) RETURNS bytea
    AS 'MODULE_PATHNAME', 'kingres_ledger_merkle_final'
    LANGUAGE C PARALLEL UNSAFE;

CREATE AGGREGATE kingres.ledger_merkle_root(bytea) (
    SFUNC = kingres.ledger_merkle_accum,
    STYPE = internal,
    FINALFUNC = kingres.ledger_merkle_final
);
COMMENT ON AGGREGATE kingres.ledger_merkle_root(bytea) IS
    'RFC 6962 Merkle root over leaf hashes; write ledger_merkle_root(x ORDER BY ...).';

CREATE FUNCTION kingres.ledger_merkle_path(leaf_hashes bytea[], leaf_index int)
    RETURNS bytea[]
    AS 'MODULE_PATHNAME', 'kingres_ledger_merkle_path'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_merkle_path(bytea[], int) IS
    'RFC 6962 audit path of the leaf with the given zero-based index.';

CREATE FUNCTION kingres.ledger_tx_leaf(seq int, table_id int, op "char",
                                        row_hash bytea)
    RETURNS bytea
    AS 'MODULE_PATHNAME', 'kingres_ledger_tx_leaf'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_tx_leaf(int, int, "char", bytea) IS
    'Leaf hash of an entry in a transaction tree: leaf data is tag 0x11, u32 seq, u32 table_id, u8 op, row_hash.';

CREATE FUNCTION kingres.ledger_block_tx_leaf(ledger_txid bigint,
                                              entry_count int,
                                              merkle_root bytea,
                                              user_name name)
    RETURNS bytea
    AS 'MODULE_PATHNAME', 'kingres_ledger_block_tx_leaf'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_block_tx_leaf(bigint, int, bytea, name) IS
    'Leaf hash of a transaction in a block tree: leaf data is tag 0x12, u64 ledger_txid, u32 entry_count, merkle_root, u16 length of user_name, user_name in UTF-8.';

CREATE FUNCTION kingres.ledger_block_hash(hash_version smallint, height bigint,
                                           prev_hash bytea,
                                           tx_merkle_root bytea,
                                           tx_count int,
                                           closed_at timestamptz)
    RETURNS bytea
    AS 'MODULE_PATHNAME', 'kingres_ledger_block_hash'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_block_hash(smallint, bigint, bytea, bytea, int, timestamptz) IS
    'Block hash, hash format version 1: SHA-256 of tag 0x13, u16 hash_version, u64 height, prev_hash, tx_merkle_root, u32 tx_count, i64 closed_at in microseconds.';

CREATE FUNCTION kingres.ledger_genesis_hash(chain_id bytea) RETURNS bytea
    AS 'MODULE_PATHNAME', 'kingres_ledger_genesis_hash'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_genesis_hash(bytea) IS
    'The prev_hash of block 0 of the chain with this identity: SHA-256(0x14 || chain_id).';

-- Reads kingres.ledger_table through SPI with fully qualified names.  It
-- runs SQL, so it has a fixed search_path.
CREATE FUNCTION kingres.ledger_row_hash(record) RETURNS bytea
    AS 'MODULE_PATHNAME', 'kingres_ledger_row_hash'
    LANGUAGE C STABLE STRICT
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_row_hash(record) IS
    'Leaf hash of a row of a ledger table, e.g. SELECT kingres.ledger_row_hash(a.*) FROM accounts a; write a.* so that a column named a cannot be taken for the row.';

-- Row hash of a row of the history table of an updatable ledger table: the
-- hash of the old version of the row that the entry D carries.  Looks up the
-- registry through SPI with fully qualified names, so it has a fixed
-- search_path.
CREATE FUNCTION kingres.ledger_history_row_hash(record) RETURNS bytea
    AS 'MODULE_PATHNAME', 'kingres_ledger_history_row_hash'
    LANGUAGE C STABLE STRICT
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_history_row_hash(record) IS
    'Leaf hash of a row of the history table of an updatable ledger table, e.g. SELECT kingres.ledger_history_row_hash(h.*) FROM accounts_ledger_history h; ledger_txid, ledger_seq and ledger_op are not part of the hash.';

CREATE FUNCTION kingres.ledger_hook_active() RETURNS boolean
    AS 'MODULE_PATHNAME', 'kingres_ledger_hook_active'
    LANGUAGE C STABLE;
COMMENT ON FUNCTION kingres.ledger_hook_active() IS
    'True when the protection hook is installed, that is, when the library is in shared_preload_libraries.';

CREATE FUNCTION kingres.ledger_binary_upgrade() RETURNS boolean
    AS 'MODULE_PATHNAME', 'kingres_ledger_binary_upgrade'
    LANGUAGE C STABLE;
COMMENT ON FUNCTION kingres.ledger_binary_upgrade() IS
    'True in a server that pg_upgrade started to restore a schema (binary upgrade mode); the event triggers do nothing there.';

CREATE FUNCTION kingres.ledger_outer_user() RETURNS name
    AS 'MODULE_PATHNAME', 'kingres_ledger_outer_user'
    LANGUAGE C STABLE;

-- Locks a table by OID in SHARE ROW EXCLUSIVE mode for the role that called
-- ledger_enable, after checking that this role owns it.  Called by
-- ledger_enable only.
CREATE FUNCTION kingres.ledger_lock_table(relation regclass) RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_ledger_lock_table'
    LANGUAGE C VOLATILE STRICT
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_lock_table(regclass) IS
    'Internal: lock a table by OID for ledger_enable, after an ownership check of the calling role.';

-- Adds to the history table the columns that its ledger table gained.  Makes
-- the change through the catalog API, not as a statement, so that no hook,
-- event trigger or user function runs while it is done; only the event trigger
-- ledger_guard calls it.  It looks up objects, so it has a fixed search_path.
CREATE FUNCTION kingres.ledger_history_sync(relation regclass, history regclass)
    RETURNS boolean
    AS 'MODULE_PATHNAME', 'kingres_ledger_history_sync'
    LANGUAGE C VOLATILE STRICT
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_history_sync(regclass, regclass) IS
    'Internal: add the columns of an updatable ledger table that its history table lacks.';

-- Sets the privileges on a history table to what the rule says: the owner of
-- the history table (the extension owner) and the owner of the ledger table
-- can read it, nobody else has anything, whatever default privileges or the
-- former owner of the ledger table had.  Also through the catalog API.  Called
-- by ledger_enable and by the event trigger ledger_guard; returns true when it
-- changed something.
CREATE FUNCTION kingres.ledger_history_acl_sync(relation regclass, history regclass)
    RETURNS boolean
    AS 'MODULE_PATHNAME', 'kingres_ledger_history_acl_sync'
    LANGUAGE C VOLATILE STRICT
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_history_acl_sync(regclass, regclass) IS
    'Internal: make the privileges on a history table those of the owner of its ledger table and of the extension owner, and nobody else.';

-- What the launcher and the workers know about the current database.  Read by
-- the view ledger_status, which runs as the owner.
CREATE FUNCTION kingres.ledger_worker_state(OUT state text, OUT visits bigint,
                                             OUT failures bigint,
                                             OUT consecutive_failures int,
                                             OUT last_attempt timestamptz,
                                             OUT last_success timestamptz,
                                             OUT retry_after timestamptz,
                                             OUT last_error text)
    RETURNS record
    AS 'MODULE_PATHNAME', 'kingres_ledger_worker_state'
    LANGUAGE C VOLATILE
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_worker_state() IS
    'Internal: the outcome of the last visits of the block-building worker to this database (state, counts, times of the last attempt, success and next attempt after a failure, and the last error); read by ledger_status.';

--------------------------------------------------------------------------
-- C primitives: write capture
--------------------------------------------------------------------------

-- Trigger function of every ledger table.  SECURITY DEFINER because ordinary
-- roles have no write privilege on the ledger tables and must not get one:
-- the trigger writes kingres.ledger_entry and kingres.ledger_pending on
-- their behalf.  Not executable by PUBLIC, so nobody else can attach it to a
-- table of their own to forge entries.
CREATE FUNCTION kingres.ledger_capture() RETURNS trigger
    AS 'MODULE_PATHNAME', 'kingres_ledger_capture'
    LANGUAGE C VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_capture() IS
    'AFTER row trigger of a ledger table (INSERT; for an updatable table also UPDATE and DELETE): records the row hashes in kingres.ledger_entry and the old versions in the history table.';

CREATE FUNCTION kingres.ledger_block_modify() RETURNS trigger
    AS 'MODULE_PATHNAME', 'kingres_ledger_block_modify'
    LANGUAGE C VOLATILE
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_block_modify() IS
    'Trigger function that rejects UPDATE, DELETE and TRUNCATE on append-only ledger tables.';

-- The last step of ledger_enable (src/write.c): registers a table that
-- ledger_enable has set up and enters the rows that it already holds.  What it
-- writes it computes from the catalogs (the table_id of the capture trigger,
-- which the registry's sequence must have issued, the mode of that trigger, the
-- history table) and the table itself, and it refuses a table that
-- ledger_table_defects finds anything wrong with, so it takes no content from
-- its caller.  SECURITY DEFINER because it writes the ledger tables; not
-- executable by PUBLIC, and the caller must own the table.
CREATE FUNCTION kingres.ledger_register_table(relation regclass) RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_ledger_register_table'
    LANGUAGE C VOLATILE STRICT SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_register_table(regclass) IS
    'Internal: register a table that ledger_enable has set up and enter the rows it already holds; for ledger_enable only.';

REVOKE ALL ON FUNCTION kingres.ledger_capture() FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_register_table(regclass) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_lock_table(regclass) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_history_sync(regclass, regclass) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_history_acl_sync(regclass, regclass) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_worker_state() FROM PUBLIC;

--------------------------------------------------------------------------
-- The tables of the ledger are append-only for every role
--------------------------------------------------------------------------

-- All triggers are created ENABLE ALWAYS so that session_replication_role =
-- replica does not skip them.  ledger_pending is a work queue: the block
-- builder deletes from it, so only UPDATE and TRUNCATE are blocked there.

CREATE TRIGGER kingres_ledger_block_modify
    BEFORE UPDATE OR DELETE ON kingres.ledger_table
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate
    BEFORE TRUNCATE ON kingres.ledger_table
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();

CREATE TRIGGER kingres_ledger_block_modify
    BEFORE UPDATE OR DELETE ON kingres.ledger_chain
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate
    BEFORE TRUNCATE ON kingres.ledger_chain
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();

CREATE TRIGGER kingres_ledger_block_modify
    BEFORE UPDATE OR DELETE ON kingres.ledger_entry
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate
    BEFORE TRUNCATE ON kingres.ledger_entry
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();

CREATE TRIGGER kingres_ledger_block_modify
    BEFORE UPDATE ON kingres.ledger_pending
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate
    BEFORE TRUNCATE ON kingres.ledger_pending
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();

CREATE TRIGGER kingres_ledger_block_modify
    BEFORE UPDATE OR DELETE ON kingres.ledger_tx
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate
    BEFORE TRUNCATE ON kingres.ledger_tx
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();

CREATE TRIGGER kingres_ledger_block_modify
    BEFORE UPDATE OR DELETE ON kingres.ledger_blocks
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate
    BEFORE TRUNCATE ON kingres.ledger_blocks
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();

CREATE TRIGGER kingres_ledger_block_modify
    BEFORE UPDATE OR DELETE ON kingres.ledger_digest
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate
    BEFORE TRUNCATE ON kingres.ledger_digest
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();

CREATE TRIGGER kingres_ledger_block_modify
    BEFORE UPDATE OR DELETE ON kingres.ledger_verification
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate
    BEFORE TRUNCATE ON kingres.ledger_verification
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();

ALTER TABLE kingres.ledger_table ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_table ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;
ALTER TABLE kingres.ledger_chain ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_chain ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;
ALTER TABLE kingres.ledger_entry ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_entry ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;
ALTER TABLE kingres.ledger_pending ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_pending ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;
ALTER TABLE kingres.ledger_tx ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_tx ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;
ALTER TABLE kingres.ledger_blocks ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_blocks ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;
ALTER TABLE kingres.ledger_digest ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_digest ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;
ALTER TABLE kingres.ledger_verification ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_verification ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;

CREATE TRIGGER kingres_ledger_block_modify
    BEFORE UPDATE OR DELETE ON kingres.ledger_removed
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate
    BEFORE TRUNCATE ON kingres.ledger_removed
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();
ALTER TABLE kingres.ledger_removed ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_removed ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;

-- The guard trigger (src/guard.c).  The ledger tables take an insert, and the
-- queue a delete, only from the code of the extension.  The executor hook
-- refuses every other statement before a row is built; this trigger is for the
-- writers that never reach it: the logical replication apply worker and the
-- table synchronisation worker.  ENABLE ALWAYS, like the others, so that
-- session_replication_role = replica does not skip it.
CREATE TRIGGER kingres_ledger_guard_write
    BEFORE INSERT ON kingres.ledger_table
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_guard_write
    BEFORE INSERT ON kingres.ledger_chain
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_guard_write
    BEFORE INSERT ON kingres.ledger_entry
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_guard_write
    BEFORE INSERT ON kingres.ledger_tx
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_guard_write
    BEFORE INSERT ON kingres.ledger_blocks
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_guard_write
    BEFORE INSERT ON kingres.ledger_digest
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_guard_write
    BEFORE INSERT ON kingres.ledger_verification
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_guard_write
    BEFORE INSERT ON kingres.ledger_removed
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_guard_write
    BEFORE INSERT OR DELETE ON kingres.ledger_pending
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
ALTER TABLE kingres.ledger_table ENABLE ALWAYS TRIGGER kingres_ledger_guard_write;
ALTER TABLE kingres.ledger_chain ENABLE ALWAYS TRIGGER kingres_ledger_guard_write;
ALTER TABLE kingres.ledger_entry ENABLE ALWAYS TRIGGER kingres_ledger_guard_write;
ALTER TABLE kingres.ledger_pending ENABLE ALWAYS TRIGGER kingres_ledger_guard_write;
ALTER TABLE kingres.ledger_tx ENABLE ALWAYS TRIGGER kingres_ledger_guard_write;
ALTER TABLE kingres.ledger_blocks ENABLE ALWAYS TRIGGER kingres_ledger_guard_write;
ALTER TABLE kingres.ledger_digest ENABLE ALWAYS TRIGGER kingres_ledger_guard_write;
ALTER TABLE kingres.ledger_verification ENABLE ALWAYS TRIGGER kingres_ledger_guard_write;
ALTER TABLE kingres.ledger_removed ENABLE ALWAYS TRIGGER kingres_ledger_guard_write;

--------------------------------------------------------------------------
-- What a ledger table and the ledger catalog must look like
--------------------------------------------------------------------------

-- Whether an object belongs to this extension.  Used instead of names to
-- recognise the functions, triggers and tables of the extension.
CREATE FUNCTION kingres.ledger_is_member(p_classid oid, p_objid oid)
    RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, pg_temp
AS $fn$
    SELECT EXISTS (
        SELECT 1
          FROM pg_catalog.pg_depend d
          JOIN pg_catalog.pg_extension x ON x.oid = d.refobjid
         WHERE d.classid = p_classid AND d.objid = p_objid
           AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
           AND d.deptype = 'e'
           AND x.extname = 'kingres_ledger')
$fn$;
COMMENT ON FUNCTION kingres.ledger_is_member(oid, oid) IS
    'Whether the object (catalog oid, object oid) is a member of the extension.';

-- Why a column type cannot be hashed in a stable way, or NULL when it can.
-- Used by ledger_enable and by the event trigger, so that ADD COLUMN passes
-- exactly the checks that ledger_enable applies.
CREATE FUNCTION kingres.ledger_type_problem(p_typid oid) RETURNS text
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    typ  oid := p_typid;
    ti   record;
    prob text;
BEGIN
    LOOP
        SELECT t.typtype, t.typsend::pg_catalog.oid AS typsend, t.typelem,
               t.typcategory, t.typbasetype, t.typname
          INTO ti
          FROM pg_catalog.pg_type t WHERE t.oid = typ;
        IF NOT FOUND THEN
            RETURN 'has a type that does not exist';
        END IF;
        IF ti.typtype = 'd' THEN
            typ := ti.typbasetype;
            CONTINUE;
        END IF;
        IF ti.typtype = 'c' THEN
            -- The binary form of a composite value contains type OIDs,
            -- which are not stable across dump and restore.
            RETURN 'has a composite type, which is not supported yet';
        END IF;
        IF ti.typsend = 0 THEN
            RETURN pg_catalog.format('has type %s without a binary send function', ti.typname);
        END IF;
        IF ti.typcategory = 'A' AND ti.typelem >= 16384 THEN
            -- array_send writes the element type OID.
            RETURN 'is an array of a user-defined type, which is not supported yet';
        END IF;
        IF ti.typcategory = 'A' AND ti.typelem <> 0 THEN
            -- array_send calls the send function of the element type.
            prob := kingres.ledger_type_problem(ti.typelem);
            IF prob IS NOT NULL THEN
                RETURN 'is an array whose element type is not supported (' || prob || ')';
            END IF;
        END IF;
        RETURN NULL;
    END LOOP;
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_type_problem(oid) IS
    'Why a column of this type cannot be put under ledger control, or NULL.';

-- Whether a table has the named trigger of the extension, in the expected
-- shape: enabled ALWAYS, of the expected type, without a column list, WHEN
-- condition or transition tables, and a trigger function that is a member of
-- the extension with the expected C symbol.  The function is recognised by
-- membership and C symbol, never by its name.
CREATE FUNCTION kingres.ledger_trigger_ok(p_relid oid, p_name name,
                                           p_symbol text, p_secdef boolean,
                                           p_type int, p_args bytea)
    RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, pg_temp
AS $fn$
    SELECT EXISTS (
        SELECT 1
          FROM pg_catalog.pg_trigger g
          JOIN pg_catalog.pg_proc p ON p.oid = g.tgfoid
         WHERE g.tgrelid = p_relid AND g.tgname = p_name
           AND g.tgenabled = 'A' AND NOT g.tgisinternal
           AND g.tgtype = p_type
           AND g.tgqual IS NULL AND g.tgattr::pg_catalog.text = ''
           AND g.tgoldtable IS NULL AND g.tgnewtable IS NULL
           AND (p_args IS NULL OR g.tgargs = p_args)
           AND p.prolang = (SELECT l.oid FROM pg_catalog.pg_language l WHERE l.lanname = 'c')
           AND p.prosrc = p_symbol
           AND p.prosecdef = p_secdef
           AND kingres.ledger_is_member('pg_catalog.pg_proc'::pg_catalog.regclass, p.oid))
$fn$;
COMMENT ON FUNCTION kingres.ledger_trigger_ok(oid, name, text, boolean, int, bytea) IS
    'Whether a table has the named trigger of the extension, in the expected shape.';

-- The problems of one table under ledger control: one row per problem.
-- Trigger types: 5 = AFTER INSERT row, 27 = BEFORE UPDATE OR DELETE row,
-- 34 = BEFORE TRUNCATE statement.
CREATE FUNCTION kingres.ledger_table_defects(p_relid oid, p_table_id int)
    RETURNS TABLE (check_name text, detail text)
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_rel  pg_catalog.pg_class%ROWTYPE;
    v_col  record;
    v_prob text;
    v_mode text;
    v_hist oid;
BEGIN
    SELECT k.* INTO v_rel FROM pg_catalog.pg_class k WHERE k.oid = p_relid;
    IF NOT FOUND OR v_rel.relkind <> 'r' THEN
        RETURN QUERY SELECT 'table_missing'::text,
            'the table with oid ' || p_relid || ' no longer exists or is not an ordinary table';
        RETURN;
    END IF;

    IF v_rel.relpersistence <> 'p' THEN
        RETURN QUERY SELECT 'table_state'::text,
            'the table is not a permanent logged table'::text;
    END IF;
    IF v_rel.relispartition THEN
        RETURN QUERY SELECT 'inheritance'::text,
            'the table is a partition'::text;
    END IF;
    RETURN QUERY
    SELECT 'inheritance'::text,
           'the table inherits from table ' || i.inhparent::pg_catalog.regclass::pg_catalog.text
      FROM pg_catalog.pg_inherits i WHERE i.inhrelid = p_relid;
    RETURN QUERY
    SELECT 'inheritance'::text,
           'table ' || i.inhrelid::pg_catalog.regclass::pg_catalog.text
           || ' is a child of this table, so rows can be read through it that have no ledger entry'
      FROM pg_catalog.pg_inherits i WHERE i.inhparent = p_relid
     ORDER BY i.inhrelid;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_rewrite r WHERE r.ev_class = p_relid) THEN
        RETURN QUERY SELECT 'table_state'::text, 'the table has rules'::text;
    END IF;

    SELECT t.mode, t.history_relid::pg_catalog.oid INTO v_mode, v_hist
      FROM kingres.ledger_table t WHERE t.table_id = p_table_id;
    v_mode := COALESCE(v_mode, 'append_only');

    IF v_mode = 'updatable' THEN
        -- AFTER INSERT OR UPDATE OR DELETE row trigger, and no blocking
        -- trigger for updates and deletes.
        IF NOT kingres.ledger_trigger_ok(p_relid, 'kingres_ledger_capture',
                   'kingres_ledger_capture', true, 29,
                   pg_catalog.convert_to(p_table_id::pg_catalog.text, 'UTF8') || pg_catalog.decode('00', 'hex')) THEN
            RETURN QUERY SELECT 'trigger'::text,
                'trigger kingres_ledger_capture is missing, disabled or not ENABLE ALWAYS'::text;
        END IF;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_trigger g
                    WHERE g.tgrelid = p_relid AND g.tgname = 'kingres_ledger_block_modify') THEN
            RETURN QUERY SELECT 'trigger'::text,
                'trigger kingres_ledger_block_modify exists on an updatable table'::text;
        END IF;
        FOR v_col IN
            SELECT a.attname FROM pg_catalog.pg_attribute a
             WHERE a.attrelid = p_relid AND a.attnum > 0 AND NOT a.attisdropped
               AND a.attname IN ('ledger_txid', 'ledger_seq', 'ledger_op')
        LOOP
            RETURN QUERY SELECT 'column'::text,
                pg_catalog.format('column "%s" has the name of a column of the history table', v_col.attname);
        END LOOP;
        RETURN QUERY SELECT h.check_name, h.detail
          FROM kingres.ledger_history_defects(v_hist, p_relid) h;
    ELSE
        IF NOT kingres.ledger_trigger_ok(p_relid, 'kingres_ledger_capture',
                   'kingres_ledger_capture', true, 5,
                   pg_catalog.convert_to(p_table_id::pg_catalog.text, 'UTF8') || pg_catalog.decode('00', 'hex')) THEN
            RETURN QUERY SELECT 'trigger'::text,
                'trigger kingres_ledger_capture is missing, disabled or not ENABLE ALWAYS'::text;
        END IF;
        IF NOT kingres.ledger_trigger_ok(p_relid, 'kingres_ledger_block_modify',
                   'kingres_ledger_block_modify', false, 27, NULL) THEN
            RETURN QUERY SELECT 'trigger'::text,
                'trigger kingres_ledger_block_modify is missing, disabled or not ENABLE ALWAYS'::text;
        END IF;
    END IF;
    IF NOT kingres.ledger_trigger_ok(p_relid, 'kingres_ledger_block_truncate',
               'kingres_ledger_block_modify', false, 34, NULL) THEN
        RETURN QUERY SELECT 'trigger'::text,
            'trigger kingres_ledger_block_truncate is missing, disabled or not ENABLE ALWAYS'::text;
    END IF;

    FOR v_col IN
        SELECT a.attname, a.atttypid, a.attgenerated
          FROM pg_catalog.pg_attribute a
         WHERE a.attrelid = p_relid AND a.attnum > 0 AND NOT a.attisdropped
         ORDER BY a.attnum
    LOOP
        IF v_col.attgenerated <> '' THEN
            RETURN QUERY SELECT 'column'::text,
                pg_catalog.format('column "%s" is a generated column, which is not supported', v_col.attname);
        END IF;
        v_prob := kingres.ledger_type_problem(v_col.atttypid);
        IF v_prob IS NOT NULL THEN
            RETURN QUERY SELECT 'column'::text,
                pg_catalog.format('column "%s" %s', v_col.attname, v_prob);
        END IF;
    END LOOP;
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_table_defects(oid, int) IS
    'The problems of one table under ledger control: missing triggers, inheritance, rules, persistence, generated columns and column types.';

-- The problems of the history table of an updatable ledger table: it exists
-- and is an ordinary permanent table, has the two blocking triggers and no
-- other trigger, rules, policies, constraints, indexes or children, and its
-- columns are exactly those of the ledger table (same names, types and type
-- modifiers, in the order of the table) plus ledger_txid bigint, ledger_seq
-- integer and ledger_op "char", wherever those three are.
CREATE FUNCTION kingres.ledger_history_defects(p_hist oid, p_relid oid)
    RETURNS TABLE (check_name text, detail text)
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_rel  pg_catalog.pg_class%ROWTYPE;
    v_name text;
    v_towner oid;
BEGIN
    IF p_hist IS NULL THEN
        RETURN QUERY SELECT 'history_missing'::text, 'the table has no history table'::text;
        RETURN;
    END IF;
    SELECT k.* INTO v_rel FROM pg_catalog.pg_class k WHERE k.oid = p_hist;
    IF NOT FOUND OR v_rel.relkind <> 'r' THEN
        RETURN QUERY SELECT 'history_missing'::text,
            'the history table with oid ' || p_hist || ' no longer exists or is not an ordinary table';
        RETURN;
    END IF;
    v_name := pg_catalog.format('%I.%I', (SELECT n.nspname FROM pg_catalog.pg_namespace n WHERE n.oid = v_rel.relnamespace), v_rel.relname);

    IF v_rel.relpersistence <> 'p' THEN
        RETURN QUERY SELECT 'history_state'::text, v_name || ' is not a permanent logged table';
    END IF;
    IF v_rel.relrowsecurity OR v_rel.relforcerowsecurity
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_policy p WHERE p.polrelid = p_hist) THEN
        RETURN QUERY SELECT 'history_state'::text, v_name || ' has row security or policies';
    END IF;
    IF v_rel.relispartition
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhrelid = p_hist OR i.inhparent = p_hist) THEN
        RETURN QUERY SELECT 'history_state'::text, v_name || ' takes part in inheritance or partitioning';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_rewrite r WHERE r.ev_class = p_hist) THEN
        RETURN QUERY SELECT 'history_state'::text, v_name || ' has rules';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_constraint c WHERE c.conrelid = p_hist)
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid = p_hist) THEN
        RETURN QUERY SELECT 'history_state'::text, v_name || ' has constraints or indexes';
    END IF;
    IF NOT kingres.ledger_trigger_ok(p_hist, 'kingres_ledger_block_modify',
               'kingres_ledger_block_modify', false, 27, NULL) THEN
        RETURN QUERY SELECT 'history_trigger'::text,
            'trigger kingres_ledger_block_modify of ' || v_name || ' is missing, disabled or not ENABLE ALWAYS';
    END IF;
    IF NOT kingres.ledger_trigger_ok(p_hist, 'kingres_ledger_block_truncate',
               'kingres_ledger_block_modify', false, 34, NULL) THEN
        RETURN QUERY SELECT 'history_trigger'::text,
            'trigger kingres_ledger_block_truncate of ' || v_name || ' is missing, disabled or not ENABLE ALWAYS';
    END IF;
    IF NOT kingres.ledger_trigger_ok(p_hist, 'kingres_ledger_block_insert',
               'kingres_ledger_block_modify', false, 7, NULL) THEN
        RETURN QUERY SELECT 'history_trigger'::text,
            'trigger kingres_ledger_block_insert of ' || v_name || ' is missing, disabled or not ENABLE ALWAYS';
    END IF;
    RETURN QUERY
    SELECT 'history_trigger'::text, 'unexpected trigger ' || g.tgname || ' on ' || v_name
      FROM pg_catalog.pg_trigger g
     WHERE g.tgrelid = p_hist AND NOT g.tgisinternal
       AND g.tgname NOT IN ('kingres_ledger_block_modify', 'kingres_ledger_block_truncate',
                            'kingres_ledger_block_insert');

    -- Who may read it: the owner of the history table and the owner of the
    -- ledger table, on the table and on no column, and nobody else.
    SELECT k.relowner INTO v_towner FROM pg_catalog.pg_class k WHERE k.oid = p_relid;
    RETURN QUERY
    SELECT 'history_privileges'::text,
           pg_catalog.format('%s: %s has the privilege %s, which the rule gives only to the owner of the table and the owner of the history table',
                             v_name,
                             CASE x.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(x.grantee)::pg_catalog.text END,
                             x.privilege_type)
      FROM pg_catalog.aclexplode(COALESCE(v_rel.relacl, pg_catalog.acldefault('r', v_rel.relowner))) x
     WHERE x.grantee <> v_rel.relowner
       AND NOT (x.grantee = v_towner AND x.grantor = v_rel.relowner
                AND x.privilege_type = 'SELECT' AND NOT x.is_grantable);
    IF v_towner <> v_rel.relowner
       AND NOT EXISTS (SELECT 1
                         FROM pg_catalog.aclexplode(COALESCE(v_rel.relacl, pg_catalog.acldefault('r', v_rel.relowner))) x
                        WHERE x.grantee = v_towner AND x.grantor = v_rel.relowner
                          AND x.privilege_type = 'SELECT' AND NOT x.is_grantable) THEN
        RETURN QUERY SELECT 'history_privileges'::text,
            v_name || ': the owner of the table cannot read the history table';
    END IF;
    RETURN QUERY
    SELECT 'history_privileges'::text,
           pg_catalog.format('%s: column "%s" has column privileges', v_name, a.attname)
      FROM pg_catalog.pg_attribute a
     WHERE a.attrelid = p_hist AND a.attnum > 0 AND NOT a.attisdropped
       AND a.attacl IS NOT NULL;

    -- Columns of the table that the history table lacks, or has with another
    -- type, and columns of the history table that do not belong there.
    RETURN QUERY
    SELECT 'history_columns'::text,
           pg_catalog.format('%s: column "%s" of the table is missing or has another type', v_name, a.attname)
      FROM pg_catalog.pg_attribute a
     WHERE a.attrelid = p_relid AND a.attnum > 0 AND NOT a.attisdropped
       AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute h
                        WHERE h.attrelid = p_hist AND h.attnum > 0 AND NOT h.attisdropped
                          AND h.attname = a.attname AND h.atttypid = a.atttypid
                          AND h.atttypmod = a.atttypmod);
    RETURN QUERY
    SELECT 'history_columns'::text,
           pg_catalog.format('%s: column "%s" does not belong to the history table', v_name, h.attname)
      FROM pg_catalog.pg_attribute h
     WHERE h.attrelid = p_hist AND h.attnum > 0 AND NOT h.attisdropped
       AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
                        WHERE a.attrelid = p_relid AND a.attnum > 0 AND NOT a.attisdropped
                          AND a.attname = h.attname)
       AND NOT ((h.attname = 'ledger_txid' AND h.atttypid = 'pg_catalog.int8'::pg_catalog.regtype)
             OR (h.attname = 'ledger_seq' AND h.atttypid = 'pg_catalog.int4'::pg_catalog.regtype)
             OR (h.attname = 'ledger_op' AND h.atttypid = 'pg_catalog."char"'::pg_catalog.regtype));
    FOR v_name IN
        SELECT m.n FROM (VALUES ('ledger_txid', 'pg_catalog.int8'),
                                ('ledger_seq', 'pg_catalog.int4'),
                                ('ledger_op', 'pg_catalog."char"')) AS m(n, t)
         WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute h
                            WHERE h.attrelid = p_hist AND h.attnum > 0 AND NOT h.attisdropped
                              AND h.attname = m.n AND h.atttypid = m.t::pg_catalog.regtype)
    LOOP
        RETURN QUERY SELECT 'history_columns'::text,
            pg_catalog.format('the history table lacks column "%s"', v_name);
    END LOOP;
    -- The relative order of the table's columns decides the row hash.
    IF (SELECT pg_catalog.array_agg(a.attname ORDER BY a.attnum)
          FROM pg_catalog.pg_attribute a
         WHERE a.attrelid = p_relid AND a.attnum > 0 AND NOT a.attisdropped)
       IS DISTINCT FROM
       (SELECT pg_catalog.array_agg(h.attname ORDER BY h.attnum)
          FROM pg_catalog.pg_attribute h
         WHERE h.attrelid = p_hist AND h.attnum > 0 AND NOT h.attisdropped
           AND h.attname NOT IN ('ledger_txid', 'ledger_seq', 'ledger_op')) THEN
        RETURN QUERY SELECT 'history_columns'::text,
            'the columns of the history table are not those of the table in the same order';
    END IF;
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_history_defects(oid, oid) IS
    'The problems of the history table of an updatable ledger table: state, triggers and columns.';

-- The problems of the ledger catalog tables and of the event triggers that
-- guard everything: the append-only triggers, no children, rules, policies
-- or row security, no foreign triggers.  The object is reported as
-- kingres.<table>.
CREATE FUNCTION kingres.ledger_catalog_defects()
    RETURNS TABLE (check_name text, object text, detail text)
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_spec record;
    v_oid  oid;
    v_rel  pg_catalog.pg_class%ROWTYPE;
    v_evt  text;
BEGIN
    -- (table, trigger type of kingres_ledger_block_modify)
    FOR v_spec IN
        SELECT s.tbl, s.modtype, s.guardtype
          FROM (VALUES ('ledger_table', 27, 7), ('ledger_chain', 27, 7),
                       ('ledger_entry', 27, 7), ('ledger_pending', 19, 15),
                       ('ledger_tx', 27, 7), ('ledger_blocks', 27, 7),
                       ('ledger_digest', 27, 7), ('ledger_verification', 27, 7),
                       ('ledger_removed', 27, 7), ('ledger_client_key', 27, 7),
                       ('ledger_client_key_revocation', 27, 7),
                       ('ledger_attestation', 27, 7)) AS s(tbl, modtype, guardtype)
    LOOP
        v_oid := pg_catalog.to_regclass(pg_catalog.format('kingres.%I', v_spec.tbl))::pg_catalog.oid;
        IF v_oid IS NULL
           OR NOT kingres.ledger_is_member('pg_catalog.pg_class'::pg_catalog.regclass, v_oid) THEN
            RETURN QUERY SELECT 'table_missing'::text, 'kingres.' || v_spec.tbl,
                'the table of the ledger is missing or is not a member of the extension';
            CONTINUE;
        END IF;
        SELECT k.* INTO v_rel FROM pg_catalog.pg_class k WHERE k.oid = v_oid;
        IF v_rel.relkind <> 'r' OR v_rel.relpersistence <> 'p' THEN
            RETURN QUERY SELECT 'table_state'::text, 'kingres.' || v_spec.tbl,
                'the table is not a permanent ordinary table';
        END IF;
        IF v_rel.relrowsecurity OR v_rel.relforcerowsecurity
           OR EXISTS (SELECT 1 FROM pg_catalog.pg_policy p WHERE p.polrelid = v_oid) THEN
            RETURN QUERY SELECT 'table_state'::text, 'kingres.' || v_spec.tbl,
                'the table has row security or policies';
        END IF;
        IF v_rel.relispartition
           OR EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i
                       WHERE i.inhrelid = v_oid OR i.inhparent = v_oid) THEN
            RETURN QUERY SELECT 'inheritance'::text, 'kingres.' || v_spec.tbl,
                'the table takes part in inheritance or partitioning';
        END IF;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_rewrite r WHERE r.ev_class = v_oid) THEN
            RETURN QUERY SELECT 'table_state'::text, 'kingres.' || v_spec.tbl,
                'the table has rules';
        END IF;
        IF NOT kingres.ledger_trigger_ok(v_oid, 'kingres_ledger_block_modify',
                   'kingres_ledger_block_modify', false, v_spec.modtype, NULL) THEN
            RETURN QUERY SELECT 'trigger'::text, 'kingres.' || v_spec.tbl,
                'trigger kingres_ledger_block_modify is missing, disabled or not ENABLE ALWAYS';
        END IF;
        IF NOT kingres.ledger_trigger_ok(v_oid, 'kingres_ledger_block_truncate',
                   'kingres_ledger_block_modify', false, 34, NULL) THEN
            RETURN QUERY SELECT 'trigger'::text, 'kingres.' || v_spec.tbl,
                'trigger kingres_ledger_block_truncate is missing, disabled or not ENABLE ALWAYS';
        END IF;
        IF NOT kingres.ledger_trigger_ok(v_oid, 'kingres_ledger_guard_write',
                   'kingres_ledger_block_modify', false, v_spec.guardtype, NULL) THEN
            RETURN QUERY SELECT 'trigger'::text, 'kingres.' || v_spec.tbl,
                'trigger kingres_ledger_guard_write is missing, disabled or not ENABLE ALWAYS';
        END IF;
        RETURN QUERY
        SELECT 'trigger'::text, 'kingres.' || v_spec.tbl,
               'unexpected trigger ' || g.tgname
          FROM pg_catalog.pg_trigger g
         WHERE g.tgrelid = v_oid AND NOT g.tgisinternal
           AND NOT kingres.ledger_is_member('pg_catalog.pg_proc'::pg_catalog.regclass, g.tgfoid);
    END LOOP;

    -- The event triggers that enforce the rules at the point of action.
    FOREACH v_evt IN ARRAY ARRAY['ddl_command_end', 'sql_drop', 'table_rewrite']
    LOOP
        IF NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_event_trigger e
             WHERE e.evtevent = v_evt AND e.evtenabled = 'A' AND e.evttags IS NULL
               AND kingres.ledger_is_member('pg_catalog.pg_event_trigger'::pg_catalog.regclass, e.oid)
               AND kingres.ledger_is_member('pg_catalog.pg_proc'::pg_catalog.regclass, e.evtfoid)) THEN
            RETURN QUERY SELECT 'event_trigger'::text, 'event ' || v_evt,
                'the event trigger of the extension is missing, disabled or not ENABLE ALWAYS';
        END IF;
    END LOOP;
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_catalog_defects() IS
    'The problems of the ledger catalog tables and of the event triggers of the extension.';

-- Every problem that the event trigger and ledger_verify look for in the
-- state of the ledger tables.
CREATE FUNCTION kingres.ledger_all_defects()
    RETURNS TABLE (check_name text, object text, detail text)
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_lt  record;
    v_obj text;
BEGIN
    FOR v_lt IN
        SELECT t.table_id, t.relid::pg_catalog.oid AS relid
          FROM kingres.ledger_table t ORDER BY t.table_id
    LOOP
        SELECT pg_catalog.format('%I.%I', n.nspname, c.relname) INTO v_obj
          FROM pg_catalog.pg_class c
          JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
         WHERE c.oid = v_lt.relid;
        IF v_obj IS NULL THEN
            v_obj := 'table_id ' || v_lt.table_id;
        END IF;
        RETURN QUERY
        SELECT d.check_name, v_obj, d.detail
          FROM kingres.ledger_table_defects(v_lt.relid, v_lt.table_id) d;
    END LOOP;
    RETURN QUERY SELECT * FROM kingres.ledger_catalog_defects();
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_all_defects() IS
    'Every problem in the state of the ledger tables and of the ledger catalog.';

--------------------------------------------------------------------------
-- The event trigger: the rules, enforced at the point of action
--------------------------------------------------------------------------

-- The ProcessUtility hook looks at a statement before it runs, and has to
-- resolve names to do so; a rename that happens in between could make it look
-- at another table than the statement changes.  This event trigger looks at
-- what the statement did, by OID, and raises an error, which rolls the
-- statement back, when
--   * a ledger table, one of its columns, or a column of a ledger catalog
--     table was dropped, whatever dependency path led there (DROP ...
--     CASCADE included);
--   * a ledger table is rewritten;
--   * after any DDL command a ledger table or the ledger catalog is no
--     longer what ledger_enable made it: a trigger missing, disabled, of the
--     wrong shape or with another function, a child or parent table, a
--     partition, rules, an unlogged table, a generated column, a column of a
--     type that ledger_enable rejects.
-- SECURITY DEFINER, because it must see the ledger tables whatever the role
-- that runs the DDL may read, and so that an administrator who tightens the
-- privileges on the ledger tables cannot make every DDL statement of every
-- role fail.  It writes nothing.
CREATE FUNCTION kingres.ledger_guard() RETURNS event_trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    d record;
BEGIN
    -- pg_upgrade creates the members of the extension one at a time, in a server
    -- of its own: the event triggers would find each other missing.
    IF kingres.ledger_binary_upgrade() THEN
        RETURN;
    END IF;
    IF TG_EVENT = 'sql_drop' THEN
        FOR d IN
            SELECT o.object_type, o.object_identity
              FROM pg_catalog.pg_event_trigger_dropped_objects() o
             WHERE o.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
               AND o.object_type IN ('table', 'table column')
               AND (EXISTS (SELECT 1 FROM kingres.ledger_table t
                             WHERE t.relid::pg_catalog.oid = o.objid
                                OR t.history_relid::pg_catalog.oid = o.objid)
                    OR (o.object_type = 'table column'
                        AND kingres.ledger_is_member('pg_catalog.pg_class'::pg_catalog.regclass, o.objid)))
        LOOP
            RAISE EXCEPTION 'cannot drop % %: it belongs to a ledger table or to the ledger',
                d.object_type, d.object_identity
                USING ERRCODE = 'KL002',
                      HINT = 'Ledger tables are append-only; the ledger can only be switched off outside the database. A DROP ... CASCADE that reaches a column or a table of the ledger is rejected.';
        END LOOP;
    ELSIF TG_EVENT = 'table_rewrite' THEN
        IF EXISTS (SELECT 1 FROM kingres.ledger_table t
                    WHERE t.relid::pg_catalog.oid = pg_catalog.pg_event_trigger_table_rewrite_oid()
                       OR t.history_relid::pg_catalog.oid = pg_catalog.pg_event_trigger_table_rewrite_oid())
           OR kingres.ledger_is_member('pg_catalog.pg_class'::pg_catalog.regclass,
                                        pg_catalog.pg_event_trigger_table_rewrite_oid()) THEN
            RAISE EXCEPTION 'cannot rewrite table %: it is a ledger table or part of the ledger',
                pg_event_trigger_table_rewrite_oid()::pg_catalog.regclass
                USING ERRCODE = 'KL002',
                      HINT = 'A rewrite changes how the rows are stored; the ledger tables cannot be rewritten by ALTER TABLE.';
        END IF;
    ELSE
        -- ADD COLUMN on an updatable table: the history table follows it, in
        -- the same transaction and before the checks below.
        FOR d IN
            SELECT t.relid::pg_catalog.oid AS relid, t.history_relid::pg_catalog.oid AS hist
              FROM kingres.ledger_table t
             WHERE t.history_relid IS NOT NULL
               AND EXISTS (SELECT 1 FROM pg_catalog.pg_event_trigger_ddl_commands() c
                            WHERE c.command_tag = 'ALTER TABLE'
                              AND c.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
                              AND c.objid = t.relid::pg_catalog.oid)
        LOOP
            PERFORM kingres.ledger_history_sync(d.relid::pg_catalog.regclass,
                                                d.hist::pg_catalog.regclass);
        END LOOP;
        -- The privileges on every history table follow the owner of its
        -- table (ALTER TABLE ... OWNER TO, REASSIGN OWNED, GRANT ... ON ALL
        -- TABLES IN SCHEMA); a table that is already right costs a cache
        -- lookup.
        FOR d IN
            SELECT t.relid::pg_catalog.oid AS relid, t.history_relid::pg_catalog.oid AS hist
              FROM kingres.ledger_table t
             WHERE t.history_relid IS NOT NULL
        LOOP
            PERFORM kingres.ledger_history_acl_sync(d.relid::pg_catalog.regclass,
                                                    d.hist::pg_catalog.regclass);
        END LOOP;
        FOR d IN SELECT x.check_name, x.object, x.detail
                   FROM kingres.ledger_all_defects() x LIMIT 1
        LOOP
            RAISE EXCEPTION 'the statement leaves the ledger broken (%: %: %)',
                d.check_name, d.object, d.detail
                USING ERRCODE = 'KL002',
                      HINT = 'Ledger tables are append-only; the statement was rolled back.';
        END LOOP;
    END IF;
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_guard() IS
    'Event trigger function: rejects, at the point of action, every statement that drops, rewrites or breaks a ledger table or the ledger.';

--------------------------------------------------------------------------
-- Putting a table under ledger control
--------------------------------------------------------------------------

-- ledger_enable is SECURITY DEFINER because it writes the ledger tables and
-- creates triggers on a table that the ledger owner does not own; it
-- therefore checks by hand that the role that called it owns the table.  It
-- is not executable by PUBLIC: putting a table under ledger control cannot be
-- undone, so it is an administrative decision.  The administrator grants
--   GRANT EXECUTE ON FUNCTION kingres.ledger_enable(regclass, text) TO role;
-- to the roles that may make that decision for the tables they own.
--
-- The table is locked first, by OID, and everything after that works from
-- the OID.  The statement runs in READ COMMITTED, so each statement sees
-- every row that was committed before the lock was granted.  The triggers are
-- created and armed first and the table is registered last, so that the
-- event trigger, which checks every registered table, never sees a table
-- that is half set up; the hook treats the table as protected from the
-- moment its capture trigger is ENABLE ALWAYS.
CREATE FUNCTION kingres.ledger_enable(relation regclass,
                                       mode text DEFAULT 'append_only')
    RETURNS void
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    rel   pg_catalog.pg_class%ROWTYPE;
    nsp   text;
    col   record;
    prob  text;
    tid   int;
    supr  boolean;
    hname text;
    hcols text;
    hid   oid;
    old_mode text;
    owner_name name;
    droppers text;
    potential text;
    by_owner boolean;
BEGIN
    IF NOT kingres.ledger_hook_active() THEN
        RAISE EXCEPTION 'kingres_ledger must be loaded through shared_preload_libraries'
            USING ERRCODE = 'KL003',
                  DETAIL = 'Without the library loaded at server start nothing stops a privileged user from switching the ledger off.',
                  HINT = 'Add kingres_ledger to shared_preload_libraries and restart the server.';
    END IF;
    IF relation IS NULL THEN
        RAISE EXCEPTION 'relation must not be NULL' USING ERRCODE = '22004';
    END IF;
    IF mode IS DISTINCT FROM 'append_only' AND mode IS DISTINCT FROM 'updatable' THEN
        RAISE EXCEPTION 'ledger mode "%" is not supported yet', mode
            USING ERRCODE = '0A000',
                  HINT = 'The modes are "append_only" and "updatable".';
    END IF;
    IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'ledger_enable requires the READ COMMITTED isolation level'
            USING ERRCODE = '0A000',
                  DETAIL = 'A snapshot taken before the table was locked would not show every committed row, and the rows it misses could never be entered.';
    END IF;
    SELECT r.rolsuper INTO supr FROM pg_catalog.pg_roles r WHERE r.rolname = current_user;
    IF NOT COALESCE(supr, false) THEN
        RAISE EXCEPTION 'the owner of the kingres_ledger extension must be a superuser'
            USING ERRCODE = '42501',
                  DETAIL = 'ledger_enable creates triggers on tables owned by other roles.';
    END IF;
    IF relation::pg_catalog.oid < 16384 THEN
        RAISE EXCEPTION 'table "%" is in a system schema and cannot be a ledger table', relation::pg_catalog.text
            USING ERRCODE = '0A000';
    END IF;

    -- Own the table (checked for the calling role) and lock it, by OID,
    -- before anything is read: concurrent writers wait, rows inserted before
    -- this point are entered below, rows inserted after it are captured by
    -- the trigger.
    PERFORM kingres.ledger_lock_table(relation);

    SELECT c.* INTO rel FROM pg_catalog.pg_class c WHERE c.oid = relation;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'relation with OID % does not exist', relation::pg_catalog.oid
            USING ERRCODE = '42P01';
    END IF;
    SELECT n.nspname INTO nsp FROM pg_catalog.pg_namespace n WHERE n.oid = rel.relnamespace;

    IF rel.relkind = 'p' THEN
        RAISE EXCEPTION 'partitioned table "%" cannot be a ledger table yet', rel.relname
            USING ERRCODE = '0A000';
    END IF;
    IF rel.relkind <> 'r' THEN
        RAISE EXCEPTION '"%" is not an ordinary table', rel.relname
            USING ERRCODE = '42809';
    END IF;
    IF rel.relpersistence <> 'p' THEN
        RAISE EXCEPTION 'temporary and unlogged table "%" cannot be a ledger table', rel.relname
            USING ERRCODE = '0A000';
    END IF;
    IF nsp = 'kingres' THEN
        -- A role named like the schema puts it first in the default
        -- search_path, so this is easy to hit by accident.
        RAISE EXCEPTION 'table "%" is in schema "kingres", which is reserved for KingresDB objects', rel.relname
            USING ERRCODE = '0A000',
                  HINT = 'Create the table in another schema, or schema-qualify its name.';
    END IF;
    IF nsp = 'information_schema' OR nsp LIKE 'pg!_%' ESCAPE '!' THEN
        RAISE EXCEPTION 'table "%" is in a system schema and cannot be a ledger table', rel.relname
            USING ERRCODE = '0A000';
    END IF;
    IF rel.relispartition OR rel.relhassubclass
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i
                   WHERE i.inhrelid = rel.oid OR i.inhparent = rel.oid) THEN
        RAISE EXCEPTION 'table "%" takes part in inheritance and cannot be a ledger table yet', rel.relname
            USING ERRCODE = '0A000';
    END IF;
    IF rel.relhasrules OR EXISTS (SELECT 1 FROM pg_catalog.pg_rewrite r WHERE r.ev_class = rel.oid) THEN
        RAISE EXCEPTION 'table "%" has rules and cannot be a ledger table yet', rel.relname
            USING ERRCODE = '0A000';
    END IF;

    -- Every column must be hashable in a stable way.  Generated columns,
    -- stored or virtual, are rejected: the value of a virtual column is not
    -- stored (a whole-row value expands it, a stored tuple does not), and a
    -- stored one depends on an expression that a dependency cascade or a
    -- later ALTER can drop, which would silently drop the column and change
    -- every hash.
    FOR col IN
        SELECT a.attname, a.atttypid, a.attgenerated
        FROM pg_catalog.pg_attribute a
        WHERE a.attrelid = rel.oid AND a.attnum > 0 AND NOT a.attisdropped
        ORDER BY a.attnum
    LOOP
        IF col.attgenerated <> '' THEN
            RAISE EXCEPTION 'column "%" is a generated column, which is not supported', col.attname
                USING ERRCODE = '0A000';
        END IF;
        prob := kingres.ledger_type_problem(col.atttypid);
        IF prob IS NOT NULL THEN
            RAISE EXCEPTION 'column "%" %', col.attname, prob
                USING ERRCODE = '0A000';
        END IF;
    END LOOP;

    SELECT lt.mode INTO old_mode FROM kingres.ledger_table lt WHERE lt.relid = rel.oid;
    IF FOUND THEN
        IF old_mode <> mode THEN
            RAISE EXCEPTION 'table "%" is a ledger table of mode "%"; converting it to "%" is not supported yet',
                rel.relname, old_mode, mode
                USING ERRCODE = '0A000',
                      HINT = 'The mode of a ledger table is permanent.';
        END IF;
        RAISE EXCEPTION 'table "%" is already under ledger control', rel.relname
            USING ERRCODE = '42710';
    END IF;

    IF mode = 'updatable' THEN
        -- The history table has three columns of its own, so the table must
        -- not use their names, and its name must fit.
        FOR col IN
            SELECT a.attname FROM pg_catalog.pg_attribute a
             WHERE a.attrelid = rel.oid AND a.attnum > 0 AND NOT a.attisdropped
               AND a.attname IN ('ledger_txid', 'ledger_seq', 'ledger_op')
        LOOP
            RAISE EXCEPTION 'column "%" of table "%" has the name of a column of the history table', col.attname, rel.relname
                USING ERRCODE = '0A000',
                      HINT = 'Rename the column before putting the table under ledger control in mode "updatable".';
        END LOOP;
        hname := rel.relname || '_ledger_history';
        IF pg_catalog.octet_length(hname) > 63 THEN
            RAISE EXCEPTION 'the name of the history table of "%" would be longer than 63 bytes', rel.relname
                USING ERRCODE = '0A000',
                      HINT = 'Rename the table to a shorter name.';
        END IF;
        IF pg_catalog.to_regclass(pg_catalog.format('%I.%I', nsp, hname)) IS NOT NULL THEN
            RAISE EXCEPTION 'relation "%" already exists', hname
                USING ERRCODE = '42P07';
        END IF;
    END IF;

    -- The next free identifier of the registry.  It is taken under the lock
    -- of the registry (ledger_lock_table took it, after the lock of the
    -- table), and not from the sequence of the identity column: a role that
    -- holds nextval and setval on every sequence (a member of
    -- pg_write_all_data) could move it, and ledger_enable would fail for
    -- everybody.  ledger_register_table checks that the identifier is this one.
    SELECT COALESCE(pg_catalog.max(lt.table_id), 0) + 1 INTO tid
      FROM kingres.ledger_table lt;

    IF mode = 'updatable' THEN
        -- The history table: the columns of the table (names and types, in
        -- order), no constraints, no defaults, no identity, and the three
        -- columns of its own.  It belongs to the extension owner; the owner
        -- of the table may read it.
        SELECT pg_catalog.string_agg(
                   pg_catalog.format('%I %s', a.attname,
                                     pg_catalog.format_type(a.atttypid, a.atttypmod)),
                   ', ' ORDER BY a.attnum)
          INTO hcols
          FROM pg_catalog.pg_attribute a
         WHERE a.attrelid = rel.oid AND a.attnum > 0 AND NOT a.attisdropped;
        EXECUTE pg_catalog.format(
            'CREATE TABLE %I.%I (%s, ledger_txid bigint, ledger_seq integer, ledger_op "char") USING heap',
            nsp, hname, hcols);
        SELECT c.oid INTO hid FROM pg_catalog.pg_class c
         WHERE c.relnamespace = rel.relnamespace AND c.relname = hname;
        EXECUTE pg_catalog.format(
            'CREATE TRIGGER kingres_ledger_block_modify BEFORE UPDATE OR DELETE ON %I.%I FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify()',
            nsp, hname);
        EXECUTE pg_catalog.format(
            'CREATE TRIGGER kingres_ledger_block_truncate BEFORE TRUNCATE ON %I.%I FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify()',
            nsp, hname);
        -- Nothing but the ledger inserts into the history table, and the ledger
        -- does it through the table access method, which fires no trigger.
        EXECUTE pg_catalog.format(
            'CREATE TRIGGER kingres_ledger_block_insert BEFORE INSERT ON %I.%I FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify()',
            nsp, hname);
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I ENABLE ALWAYS TRIGGER kingres_ledger_block_modify', nsp, hname);
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate', nsp, hname);
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I ENABLE ALWAYS TRIGGER kingres_ledger_block_insert', nsp, hname);
        -- Who may read the old versions: the owner of the table and the
        -- extension owner, nobody else, whatever default privileges say.
        PERFORM kingres.ledger_history_acl_sync(rel.oid::pg_catalog.regclass,
                                                hid::pg_catalog.regclass);

        EXECUTE pg_catalog.format(
            'CREATE TRIGGER kingres_ledger_capture AFTER INSERT OR UPDATE OR DELETE ON %I.%I FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture(%L)',
            nsp, rel.relname, tid::text);
        EXECUTE pg_catalog.format(
            'CREATE TRIGGER kingres_ledger_block_truncate BEFORE TRUNCATE ON %I.%I FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify()',
            nsp, rel.relname);
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate', nsp, rel.relname);
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I ENABLE ALWAYS TRIGGER kingres_ledger_capture', nsp, rel.relname);
    ELSE
        EXECUTE pg_catalog.format(
            'CREATE TRIGGER kingres_ledger_capture AFTER INSERT ON %I.%I FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture(%L)',
            nsp, rel.relname, tid::text);
        EXECUTE pg_catalog.format(
            'CREATE TRIGGER kingres_ledger_block_modify BEFORE UPDATE OR DELETE ON %I.%I FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify()',
            nsp, rel.relname);
        EXECUTE pg_catalog.format(
            'CREATE TRIGGER kingres_ledger_block_truncate BEFORE TRUNCATE ON %I.%I FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify()',
            nsp, rel.relname);

        -- The order matters for the protection hook: the capture trigger is
        -- armed last, which is what lets these statements through.
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I ENABLE ALWAYS TRIGGER kingres_ledger_block_modify', nsp, rel.relname);
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate', nsp, rel.relname);
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I ENABLE ALWAYS TRIGGER kingres_ledger_capture', nsp, rel.relname);
    END IF;

    -- Say what the ledger cannot do: a role that can drop this database drops
    -- the ledger with it (a DROP DATABASE run from another database goes
    -- through no hook of a library loaded in this one).  The roles are those
    -- that PostgreSQL's dropdb lets through, for a role that is not a
    -- superuser: the current user must be the owner of the database or have
    -- the privileges of the owner (object_ownercheck, has_privs_of_role:
    -- membership with INHERIT, directly or through a chain), and the current
    -- user is what SET ROLE made it, so a role that can SET ROLE (membership
    -- with SET, directly or through a chain) to such a role, or to a
    -- superuser, can drop it as well.  A database owned by pg_database_owner
    -- belongs, in each database a session is connected to, to the owner of that
    -- database: every role that owns a database of the cluster can drop it, so
    -- the owners of the databases stand in for the owner in the rule
    -- (pg_database_owner, the role 6171, is never asked about: its membership is
    -- relative to the database of the session).  Second, the roles that cannot
    -- drop it now and can make themselves able to without a superuser: a role
    -- with ADMIN OPTION, directly or through a chain, on a role that can (a
    -- CREATEROLE role has it on every role it created), grants itself that role
    -- with INHERIT and SET.
    -- Third, the roles that hold the power of a superuser over the files and
    -- the programs of the server: a member (directly, through a chain, or by
    -- SET ROLE) of pg_execute_server_program (COPY ... TO PROGRAM runs any
    -- command as the operating-system user of the server, dropdb included) or
    -- of pg_write_server_files (COPY ... TO a file writes any file that user
    -- can write, the files of the cluster included).  They count as
    -- superusers for this purpose and are listed with the first, and the roles
    -- with ADMIN OPTION on those two roles with the second (L19-05).  Only a
    -- database whose owner is a superuser role without members that are not
    -- superusers, and with no role administering such a member, and that has
    -- no member of those two roles, is out of reach of every role that is not
    -- a superuser (README, "What a role that is not a superuser can stop or
    -- remove", exception 6; L13-03, L15-01, L17-02, L17-03, L19-05).
    WITH RECURSIVE own(oid) AS (
        SELECT d.datdba FROM pg_catalog.pg_database d
         WHERE d.datname = pg_catalog.current_database()
           AND d.datdba <> 6171::pg_catalog.oid
        UNION
        SELECT d2.datdba FROM pg_catalog.pg_database d, pg_catalog.pg_database d2
         WHERE d.datname = pg_catalog.current_database()
           AND d.datdba = 6171::pg_catalog.oid
           AND d2.datdba <> 6171::pg_catalog.oid
    ), drop_now(oid) AS (
        SELECT r.oid FROM pg_catalog.pg_roles r
         WHERE NOT r.rolsuper
           AND EXISTS (SELECT 1 FROM pg_catalog.pg_roles x
                        WHERE (x.oid = r.oid OR pg_catalog.pg_has_role(r.oid, x.oid, 'SET'))
                          AND (x.rolsuper
                               OR EXISTS (SELECT 1 FROM own o
                                           WHERE pg_catalog.pg_has_role(x.oid, o.oid, 'USAGE'))
                               OR (r.oid >= 16384::pg_catalog.oid
                                   AND (pg_catalog.pg_has_role(x.oid, 'pg_execute_server_program', 'USAGE')
                                        OR pg_catalog.pg_has_role(x.oid, 'pg_write_server_files', 'USAGE')))))
    ), can_make(oid) AS (
        SELECT dn.oid FROM drop_now dn
        UNION
        SELECT r2.oid FROM pg_catalog.pg_roles r2
         WHERE NOT r2.rolsuper AND r2.oid >= 16384::pg_catalog.oid
           AND (pg_catalog.pg_has_role(r2.oid, 'pg_execute_server_program', 'MEMBER WITH ADMIN OPTION')
                OR pg_catalog.pg_has_role(r2.oid, 'pg_write_server_files', 'MEMBER WITH ADMIN OPTION'))
        UNION
        SELECT r.oid FROM pg_catalog.pg_roles r, can_make c
         WHERE NOT r.rolsuper
           AND pg_catalog.pg_has_role(r.oid, c.oid, 'MEMBER WITH ADMIN OPTION')
    )
    SELECT pg_catalog.string_agg(r.rolname::pg_catalog.text, ', ' ORDER BY r.rolname)
               FILTER (WHERE r.oid IN (SELECT dn.oid FROM drop_now dn)),
           pg_catalog.string_agg(r.rolname::pg_catalog.text, ', ' ORDER BY r.rolname)
               FILTER (WHERE r.oid IN (SELECT cm.oid FROM can_make cm)
                         AND r.oid NOT IN (SELECT dn.oid FROM drop_now dn)),
           (SELECT d.datdba = 6171::pg_catalog.oid
              FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database())
      INTO droppers, potential, by_owner
      FROM pg_catalog.pg_roles r;
    IF droppers IS NOT NULL AND by_owner THEN
        RAISE WARNING 'database "%" can be dropped, and the ledger with it, by roles that are not superusers: %',
            pg_catalog.current_database(), droppers
            USING DETAIL = 'The database is owned by pg_database_owner, which is whoever owns the database a session is connected to: every role that owns a database of this cluster can drop it.',
                  HINT = 'Make a superuser role that has no members (or only superusers) the owner of every database that holds a ledger (ALTER DATABASE ... OWNER TO), do not grant it to a role that is not a superuser, and keep the digests outside the database. kingres.ledger_status lists the roles.';
    ELSIF droppers IS NOT NULL THEN
        RAISE WARNING 'database "%" can be dropped, and the ledger with it, by roles that are not superusers: %',
            pg_catalog.current_database(), droppers
            USING HINT = 'Make a superuser role that has no members (or only superusers) the owner of every database that holds a ledger (ALTER DATABASE ... OWNER TO), do not grant it to a role that is not a superuser, and keep the digests outside the database. kingres.ledger_status lists the roles.';
    END IF;
    IF potential IS NOT NULL THEN
        RAISE WARNING 'database "%" can also be dropped, and the ledger with it, by roles that are not superusers after they grant themselves a role they administer (ADMIN OPTION; a role with CREATEROLE has it on the roles it created): %',
            pg_catalog.current_database(), potential
            USING HINT = 'Take the ADMIN OPTION away from them (REVOKE ... FROM), or the roles they administer from the owner of the database. kingres.ledger_status lists the roles in database_potential_droppers.';
    END IF;

    -- The registration and the entry of the rows that exist already are
    -- writes of ledger tables.  The C function makes them, computing what it
    -- writes from the state that was just made (src/write.c).
    PERFORM kingres.ledger_register_table(rel.oid::pg_catalog.regclass);
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_enable(regclass, text) IS
    'Put a table under ledger control, for good, in mode append_only (the default) or updatable (with a history table); existing rows are entered by the calling transaction. Needs READ COMMITTED, ownership of the table and EXECUTE, which is not granted to PUBLIC.';

--------------------------------------------------------------------------
-- Blocks and digests
--------------------------------------------------------------------------

-- Closes a block from the pending transactions (src/write.c).  Only one
-- builder runs at a time: they serialise on a lock on the extension's own
-- object with a sub-object number that no statement uses, which no role can
-- request but through this code.  Neither a table lock (pg_maintain and the
-- members of pg_write_all_data can take them) nor an advisory lock (every role
-- can) would do: a role that holds one could stop all block building.
-- READ COMMITTED is required: the builder must see the block written by the
-- previous builder, which a REPEATABLE READ snapshot taken earlier would not.
--
-- SECURITY DEFINER because it writes the ledger tables, which no other role
-- may write.
CREATE FUNCTION kingres.ledger_build_block()
    RETURNS bigint
    AS 'MODULE_PATHNAME', 'kingres_ledger_build_block'
    LANGUAGE C VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_build_block() IS
    'Close a block from the pending transactions; returns the new height, or NULL when nothing was pending.';

-- SECURITY DEFINER because it calls the block builder and reads the ledger.
CREATE FUNCTION kingres.ledger_generate_digest()
    RETURNS jsonb
    AS 'MODULE_PATHNAME', 'kingres_ledger_generate_digest'
    LANGUAGE C VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_generate_digest() IS
    'Close a block if needed, record and return the digest (chain identity, height and block hash) of the latest block as JSON; NULL when the ledger is empty (nothing is recorded then).';

-- The way out for a row that stops ledger_head and that no honest write
-- leaves (README, "A row above the head"): a transaction row that names a block
-- above the head (the end of the chain was removed), and a recorded digest that
-- names a block that is not there or another hash than the block has.  What it
-- removes it first copies into kingres.ledger_removed, as it was; the
-- transactions of the removed blocks are queued again (their entries are still
-- there), so the next block is built from them and no entry is left without a
-- block.  It is an act of a superuser who asked for it: the session must have
-- kingres_ledger.allow_manual_write on, and the caller must be a superuser
-- (a superuser acting as another role is not).  No trigger is switched off and
-- no other session is affected: the setting is the only thing that opens the
-- tables, for this session.
--
-- SECURITY DEFINER only so that the owner's search_path and the table lock are
-- the same whoever the superuser is; the checks below are on the caller.
CREATE FUNCTION kingres.ledger_remove_stray(reason text)
    RETURNS bigint
    AS 'MODULE_PATHNAME', 'kingres_ledger_remove_stray'
    LANGUAGE C VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_remove_stray(text) IS
    'Superuser, with kingres_ledger.allow_manual_write on in the session: copy to ledger_removed and remove the transaction rows above the head (their transactions are queued again) and the recorded digests that name a missing block or another hash; returns the number of rows removed.';

--------------------------------------------------------------------------
-- Verification
--------------------------------------------------------------------------

CREATE FUNCTION kingres.ledger_timestamptz_us(ts timestamptz) RETURNS bigint
    AS 'MODULE_PATHNAME', 'kingres_ledger_timestamptz_us'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_timestamptz_us(timestamptz) IS
    'Microseconds since 2000-01-01 00:00:00 UTC, the value the block hash covers.';

-- Checks the proof using only its content and a trusted digest; reads no
-- table.  Malformed proofs return false.
CREATE FUNCTION kingres.ledger_verify_proof(proof jsonb, digest jsonb)
    RETURNS boolean
    AS 'MODULE_PATHNAME', 'kingres_ledger_verify_proof'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_verify_proof(jsonb, jsonb) IS
    'Check an inclusion proof against a trusted digest of the same chain; reads no table. It proves that an entry with the row hash in the proof is in the ledger; recompute the row hash from the row you hold, or use the four-argument form.';

-- The same, and in addition the table id and the row hash that the caller
-- computed from the row it holds (kingres.ledger_row_hash of that row, on a
-- server that has the table) must be the ones in the proof.
CREATE FUNCTION kingres.ledger_verify_proof(proof jsonb, digest jsonb,
                                             expected_table_id int,
                                             expected_row_hash bytea)
    RETURNS boolean
    AS 'MODULE_PATHNAME', 'kingres_ledger_verify_proof'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_verify_proof(jsonb, jsonb, int, bytea) IS
    'Check an inclusion proof against a trusted digest, and that it is a proof of an I entry for this table id and this row hash.';

-- The same, for an entry of the given operation: I proves that a version of
-- a row was entered, D that it was removed.
CREATE FUNCTION kingres.ledger_verify_proof(proof jsonb, digest jsonb,
                                             expected_table_id int,
                                             expected_row_hash bytea,
                                             expected_op "char")
    RETURNS boolean
    AS 'MODULE_PATHNAME', 'kingres_ledger_verify_proof'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_verify_proof(jsonb, jsonb, int, bytea, "char") IS
    'Check an inclusion proof against a trusted digest, and that it is a proof of an entry of operation expected_op (I or D) for this table id and this row hash.';

-- Whether a digest that was exported earlier matches the stored chain.  The
-- whole chain from block 0 up to the digest height is recomputed, so a
-- rewritten chain (even one that is internally consistent) fails because its
-- hashes differ from the digest.  A digest of another chain is rejected.
--
-- Not executable by PUBLIC: it reads the whole chain, which costs time.  It
-- runs with the rights of the caller, who needs SELECT on ledger_blocks and
-- ledger_chain (both are readable by PUBLIC).  Every input gives true or
-- false, never an error.
CREATE FUNCTION kingres.ledger_verify_digest(digest jsonb) RETURNS boolean
    LANGUAGE plpgsql
    STABLE
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    dheight bigint;
    dnum    numeric;
    dhash   bytea;
    dchain  bytea;
    chain   bytea;
    ok      boolean;
BEGIN
    IF digest IS NULL OR pg_catalog.jsonb_typeof(digest) <> 'object' THEN
        RETURN false;
    END IF;
    BEGIN
        IF pg_catalog.jsonb_typeof(digest -> 'height') IS DISTINCT FROM 'number'
           OR pg_catalog.jsonb_typeof(digest -> 'block_hash') IS DISTINCT FROM 'string'
           OR pg_catalog.jsonb_typeof(digest -> 'chain_id') IS DISTINCT FROM 'string' THEN
            RETURN false;
        END IF;
        dnum := (digest -> 'height')::numeric;
        dhash := pg_catalog.decode(digest ->> 'block_hash', 'hex');
        dchain := pg_catalog.decode(digest ->> 'chain_id', 'hex');
    EXCEPTION WHEN OTHERS THEN
        RETURN false;
    END;
    -- An integer in the range of bigint; checked before it is converted.
    IF dnum IS NULL OR dhash IS NULL OR dchain IS NULL
       OR dnum < 0 OR dnum > 9223372036854775807
       OR dnum <> pg_catalog.trunc(dnum)
       OR pg_catalog.octet_length(dhash) <> 32
       OR pg_catalog.octet_length(dchain) <> 32 THEN
        RETURN false;
    END IF;
    dheight := dnum::bigint;

    SELECT c.chain_id INTO chain FROM kingres.ledger_chain c;
    IF NOT FOUND OR chain IS DISTINCT FROM dchain THEN
        RETURN false;
    END IF;

    SELECT pg_catalog.count(*) - 1 = dheight
           AND pg_catalog.bool_and(
                   -- CASE, because AND does not guarantee evaluation order
                   -- and the hash function rejects malformed input.
                   CASE WHEN c.height = c.expected
                             AND c.hash_version = 1
                             AND pg_catalog.octet_length(c.prev_hash) = 32
                             AND pg_catalog.octet_length(c.tx_merkle_root) = 32
                             AND c.tx_count >= 0
                             AND c.closed_at > '-infinity'::pg_catalog.timestamptz
                             AND c.closed_at < 'infinity'::pg_catalog.timestamptz
                        THEN c.prev_hash = COALESCE(c.lag_hash, kingres.ledger_genesis_hash(chain))
                             AND (c.lag_at IS NULL OR c.closed_at >= c.lag_at)
                             AND c.block_hash = kingres.ledger_block_hash(
                                   c.hash_version, c.height, c.prev_hash,
                                   c.tx_merkle_root, c.tx_count, c.closed_at)
                        ELSE false END)
           AND pg_catalog.bool_or(c.height = dheight AND c.block_hash = dhash)
      INTO ok
      FROM (SELECT b.*,
                   pg_catalog.lag(b.block_hash) OVER (ORDER BY b.height) AS lag_hash,
                   pg_catalog.lag(b.closed_at) OVER (ORDER BY b.height) AS lag_at,
                   pg_catalog.row_number() OVER (ORDER BY b.height) - 1 AS expected
              FROM kingres.ledger_blocks b
             WHERE b.height <= dheight) c;
    RETURN COALESCE(ok, false);
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_verify_digest(jsonb) IS
    'Whether a digest exported earlier matches the stored chain of blocks and belongs to this chain. Not executable by PUBLIC.';

-- Recomputes everything in the range and returns one row per problem.
--
-- Each check is one statement so that it sees one snapshot; run the function
-- in a REPEATABLE READ transaction to get one snapshot for all checks.  The
-- comparison of each ledger table with its entries is not restricted to the
-- range, because rows have no block of their own.
--
-- SECURITY DEFINER because it reads every ledger table and every ledger
-- catalog table, which the caller may not be allowed to read; the caller
-- needs EXECUTE only.  Row security is switched off so that a role without
-- BYPASSRLS fails loudly instead of silently seeing fewer rows.  A table
-- that cannot be checked (a hostile or broken one) is reported as a finding
-- for that table and the verification goes on with the next one.
CREATE FUNCTION kingres.ledger_verify(from_height bigint DEFAULT 0,
                                       to_height bigint DEFAULT NULL)
    RETURNS TABLE (check_name text, object text, detail text)
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    SET row_security = off
AS $fn$
DECLARE
    lo       bigint := COALESCE(from_height, 0);
    hi       bigint;
    chain    bytea;
    genesis  bytea;
    lt       record;
    nsp      text;
    rname    text;
    obj      text;
    hobj     text;
    findings text[];
BEGIN
    SELECT COALESCE(to_height, pg_catalog.max(b.height), -1) INTO hi
      FROM kingres.ledger_blocks b;
    IF lo < 0 OR (to_height IS NOT NULL AND to_height < lo) THEN
        RAISE EXCEPTION 'invalid height range % .. %', from_height, to_height
            USING ERRCODE = '22023';
    END IF;

    -- The identity of the chain, and the hash that block 0 must start from.
    SELECT c.chain_id INTO chain FROM kingres.ledger_chain c;
    IF NOT FOUND OR pg_catalog.octet_length(chain) <> 32 THEN
        RETURN QUERY SELECT 'chain_id'::text, 'kingres.ledger_chain'::text,
            'the identity of the chain is missing or malformed'::text;
        chain := NULL;
    ELSE
        genesis := kingres.ledger_genesis_hash(chain);
    END IF;

    -- Heights must be dense from 0.
    RETURN QUERY
    SELECT 'block_height'::text, 'block ' || s.height,
           'expected height ' || s.expected || ' at this position of the chain'
      FROM (SELECT b.height,
                   pg_catalog.row_number() OVER (ORDER BY b.height) - 1 AS expected
              FROM kingres.ledger_blocks b) s
     WHERE s.height <> s.expected;

    -- Malformed values that the hash functions would reject.
    RETURN QUERY
    SELECT 'malformed'::text, 'block ' || b.height,
           'block header has an unsupported hash version or wrongly sized values'
      FROM kingres.ledger_blocks b
     WHERE b.height BETWEEN lo AND hi
       AND (b.hash_version <> 1 OR pg_catalog.octet_length(b.prev_hash) <> 32
            OR pg_catalog.octet_length(b.tx_merkle_root) <> 32
            OR pg_catalog.octet_length(b.block_hash) <> 32 OR b.tx_count < 0
            OR b.closed_at <= '-infinity'::pg_catalog.timestamptz
            OR b.closed_at >= 'infinity'::pg_catalog.timestamptz);
    RETURN QUERY
    SELECT 'malformed'::text, 'tx ' || t.ledger_txid,
           'transaction row has wrongly sized or negative values'
      FROM kingres.ledger_tx t
     WHERE t.block_height BETWEEN lo AND hi
       AND (pg_catalog.octet_length(t.merkle_root) <> 32 OR t.entry_count < 0
            OR t.ledger_txid < 0);
    RETURN QUERY
    SELECT 'malformed'::text, 'tx ' || e.ledger_txid || ' seq ' || e.seq,
           'entry has a wrongly sized row hash, an unknown operation or a negative number'
      FROM kingres.ledger_entry e
     WHERE pg_catalog.octet_length(e.row_hash) <> 32 OR e.op NOT IN ('I', 'D')
        OR e.seq < 0 OR e.table_id < 0 OR e.ledger_txid < 0;

    -- Every block links to its predecessor; block 0 starts from the chain
    -- identity.
    RETURN QUERY
    SELECT 'block_prev_hash'::text, 'block ' || b.height,
           CASE WHEN b.height = 0 THEN 'the first block must have the hash derived from the chain identity as prev_hash'
                WHEN p.height IS NULL THEN 'previous block ' || (b.height - 1) || ' is missing'
                ELSE 'prev_hash does not equal the hash of block ' || (b.height - 1) END
      FROM kingres.ledger_blocks b
      LEFT JOIN kingres.ledger_blocks p ON p.height = b.height - 1
     WHERE b.height BETWEEN lo AND hi
       AND b.height >= 0
       AND b.prev_hash IS DISTINCT FROM (CASE WHEN b.height = 0 THEN genesis ELSE p.block_hash END);

    -- The time of a block is never earlier than the time of the block before.
    RETURN QUERY
    SELECT 'block_time'::text, 'block ' || s.height,
           'closed_at is earlier than closed_at of the previous block'
      FROM (SELECT b.height, b.closed_at,
                   pg_catalog.lag(b.closed_at) OVER (ORDER BY b.height) AS prev_at
              FROM kingres.ledger_blocks b) s
     WHERE s.height BETWEEN lo AND hi AND s.closed_at < s.prev_at;

    -- A block closed later than the time of this verification (by more than
    -- the skew the builder tolerates, one hour when that setting is 0): the
    -- clock was ahead, or the row was written by hand.  Block times never go
    -- back, so every later block carries that time or a later one; one row
    -- says where it starts and how many blocks are affected.
    RETURN QUERY
    SELECT 'block_time'::text, 'block ' || f.height,
           'closed_at is later than the time of this verification; ' || f.n
           || ' block(s) from here on are closed in the future (the clock was ahead, or the block was written by hand)'
      FROM (SELECT pg_catalog.min(b.height) AS height, pg_catalog.count(*) AS n
              FROM kingres.ledger_blocks b
             WHERE b.height BETWEEN lo AND hi
               AND b.closed_at > pg_catalog.clock_timestamp()
                   + COALESCE(NULLIF((SELECT s.setting::pg_catalog.int8 FROM pg_catalog.pg_settings s
                                       WHERE s.name = 'kingres_ledger.max_clock_skew'), 0),
                              3600000) * interval '1 millisecond') f
     WHERE f.n > 0;

    -- The builder closes a block for at least one transaction, and a
    -- transaction exists for at least one entry; a row that says otherwise was
    -- not written by the builder.
    RETURN QUERY
    SELECT 'block_empty'::text, 'block ' || b.height,
           'the block has no transaction, which the builder never writes'
      FROM kingres.ledger_blocks b
     WHERE b.height BETWEEN lo AND hi AND b.tx_count = 0;
    RETURN QUERY
    SELECT 'tx_empty'::text, 'tx ' || t.ledger_txid,
           'the transaction has no entry, which the builder never writes'
      FROM kingres.ledger_tx t
     WHERE t.block_height BETWEEN lo AND hi AND t.entry_count = 0;

    -- Every block hash is the hash of its header.
    RETURN QUERY
    SELECT 'block_hash'::text, 'block ' || b.height,
           'stored block_hash ' || pg_catalog.encode(b.block_hash, 'hex')
           || ' differs from the recomputed hash'
      FROM kingres.ledger_blocks b
     WHERE b.height BETWEEN lo AND hi
       AND CASE WHEN b.hash_version = 1 AND pg_catalog.octet_length(b.prev_hash) = 32
                     AND pg_catalog.octet_length(b.tx_merkle_root) = 32
                     AND b.tx_count >= 0 AND b.height >= 0
                     AND b.closed_at > '-infinity'::pg_catalog.timestamptz
                     AND b.closed_at < 'infinity'::pg_catalog.timestamptz
                THEN b.block_hash IS DISTINCT FROM kingres.ledger_block_hash(
                         b.hash_version, b.height, b.prev_hash, b.tx_merkle_root,
                         b.tx_count, b.closed_at)
                ELSE false END;

    -- Every block transaction root, transaction count and tx_index.
    RETURN QUERY
    SELECT 'block_tx_root'::text, 'block ' || b.height,
           CASE WHEN r.cnt <> b.tx_count
                THEN 'tx_count is ' || b.tx_count || ' but the block has ' || r.cnt || ' transactions'
                WHEN r.cnt > 0 AND (r.min_idx <> 0 OR r.max_idx <> r.cnt - 1)
                THEN 'tx_index values are not 0..' || (r.cnt - 1)
                ELSE 'tx_merkle_root ' || pg_catalog.encode(b.tx_merkle_root, 'hex')
                     || ' differs from the recomputed root' END
      FROM kingres.ledger_blocks b
     CROSS JOIN LATERAL (
           SELECT pg_catalog.count(*) AS cnt,
                  pg_catalog.min(t.tx_index) AS min_idx,
                  pg_catalog.max(t.tx_index) AS max_idx,
                  kingres.ledger_merkle_root(
                      kingres.ledger_block_tx_leaf(t.ledger_txid, t.entry_count, t.merkle_root, t.user_name)
                      ORDER BY t.tx_index) AS root
             FROM kingres.ledger_tx t
            WHERE t.block_height = b.height
              AND pg_catalog.octet_length(t.merkle_root) = 32
              AND t.entry_count >= 0 AND t.ledger_txid >= 0) r
     WHERE b.height BETWEEN lo AND hi
       AND (r.cnt <> b.tx_count
            OR (r.cnt > 0 AND (r.min_idx <> 0 OR r.max_idx <> r.cnt - 1))
            OR r.root IS DISTINCT FROM b.tx_merkle_root);

    -- Every transaction root and entry count against the entries.
    RETURN QUERY
    SELECT 'tx_root'::text, 'tx ' || t.ledger_txid || ' (block ' || t.block_height || ')',
           CASE WHEN r.cnt <> t.entry_count
                THEN 'entry_count is ' || t.entry_count || ' but ' || r.cnt || ' entries exist'
                ELSE 'merkle_root ' || pg_catalog.encode(t.merkle_root, 'hex')
                     || ' differs from the recomputed root' END
      FROM kingres.ledger_tx t
     CROSS JOIN LATERAL (
           SELECT pg_catalog.count(*) AS cnt,
                  kingres.ledger_merkle_root(
                      kingres.ledger_tx_leaf(e.seq, e.table_id, e.op, e.row_hash)
                      ORDER BY e.seq) AS root
             FROM kingres.ledger_entry e
            WHERE e.ledger_txid = t.ledger_txid
              AND pg_catalog.octet_length(e.row_hash) = 32
              AND e.seq >= 0 AND e.table_id >= 0) r
     WHERE t.block_height BETWEEN lo AND hi
       AND (r.cnt <> t.entry_count
            OR r.root IS DISTINCT FROM t.merkle_root);

    -- Transactions that point to no block, and transactions queued twice.
    RETURN QUERY
    SELECT 'tx_block'::text, 'tx ' || t.ledger_txid,
           'transaction refers to block ' || t.block_height || ', which does not exist'
      FROM kingres.ledger_tx t
     WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_blocks b WHERE b.height = t.block_height);
    -- The same for the recorded digests (0.3): a digest that names a block that
    -- does not exist, a digest whose hash is not the hash of the block it
    -- names, and a digest document that does not say what the row says.  Every
    -- digest is checked, whatever the range, because these are the findings
    -- ledger_head stops on: an operator who got its error finds the row here,
    -- with its id.  A digest that was kept outside the database is compared
    -- with ledger_verify_digest.
    RETURN QUERY
    SELECT 'digest_block'::text, 'digest ' || d.digest_id,
           'digest refers to block ' || d.height || ', which does not exist'
      FROM kingres.ledger_digest d
      LEFT JOIN kingres.ledger_blocks b ON b.height = d.height
     WHERE b.height IS NULL;
    RETURN QUERY
    SELECT 'digest_hash'::text, 'digest ' || d.digest_id,
           'digest names block ' || d.height || ' with hash ' || pg_catalog.encode(d.block_hash, 'hex')
           || ', but the block has hash ' || pg_catalog.encode(b.block_hash, 'hex')
      FROM kingres.ledger_digest d
      JOIN kingres.ledger_blocks b ON b.height = d.height
     WHERE d.block_hash IS DISTINCT FROM b.block_hash;
    RETURN QUERY
    SELECT 'digest_json'::text, 'digest ' || d.digest_id,
           'the digest document does not agree with the height, the hash or the chain identity'::text
      FROM kingres.ledger_digest d
     WHERE d.digest ->> 'height' IS DISTINCT FROM d.height::pg_catalog.text
        OR d.digest ->> 'block_hash' IS DISTINCT FROM pg_catalog.encode(d.block_hash, 'hex')
        OR (chain IS NOT NULL
            AND d.digest ->> 'chain_id' IS DISTINCT FROM pg_catalog.encode(chain, 'hex'));

    -- What an operator removed with ledger_remove_stray: one finding for every
    -- row that was taken out, for as long as the record exists.
    RETURN QUERY
    SELECT 'operator_removal'::pg_catalog.text, 'removal ' || r.removal_id,
           'a row of ' || r.source_table || ' was removed by ' || r.removed_by
           || ' at ' || r.removed_at || ': ' || r.reason
      FROM kingres.ledger_removed r
     WHERE r.action = 'removed'
     ORDER BY r.removal_id;
    -- What a superuser inserted by hand (allow_manual_write): one finding for
    -- every table, for as long as the record exists; the record, with the row as
    -- it was, who and when, is in kingres.ledger_removed.
    RETURN QUERY
    SELECT 'manual_write'::pg_catalog.text, 'kingres.' || r.source_table,
           pg_catalog.count(*) || ' row(s) were inserted by hand with kingres_ledger.allow_manual_write on (see kingres.ledger_removed)'
      FROM kingres.ledger_removed r
     WHERE r.action = 'inserted'
     GROUP BY r.source_table
     ORDER BY r.source_table;
    RETURN QUERY
    SELECT 'pending_in_block'::text, 'tx ' || p.ledger_txid,
           'transaction is both pending and in block ' || t.block_height
      FROM kingres.ledger_pending p
      JOIN kingres.ledger_tx t USING (ledger_txid);

    -- Every entry belongs to a transaction in a block or to a pending one.
    RETURN QUERY
    SELECT 'entry_orphan'::text, 'tx ' || e.ledger_txid,
           pg_catalog.count(*) || ' entries belong to no block and to no pending transaction'
      FROM kingres.ledger_entry e
     WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_tx t WHERE t.ledger_txid = e.ledger_txid)
       AND NOT EXISTS (SELECT 1 FROM kingres.ledger_pending p WHERE p.ledger_txid = e.ledger_txid)
     GROUP BY e.ledger_txid;
    RETURN QUERY
    SELECT 'entry_table'::text, 'tx ' || e.ledger_txid || ' seq ' || e.seq,
           'entry refers to unknown table_id ' || e.table_id
      FROM kingres.ledger_entry e
     WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_table t WHERE t.table_id = e.table_id);

    -- For each ledger table: its state (triggers, no children, no rules, ...)
    -- and the multiset of row hashes of the current rows equals the multiset
    -- of its entries.  A failure of one table's checks is a finding for that
    -- table; the others are still checked.
    FOR lt IN SELECT t.table_id, t.relid::pg_catalog.oid AS relid, t.mode,
                     t.history_relid::pg_catalog.oid AS history_relid
                FROM kingres.ledger_table t ORDER BY t.table_id
    LOOP
        SELECT n.nspname, c.relname INTO nsp, rname
          FROM pg_catalog.pg_class c
          JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
         WHERE c.oid = lt.relid AND c.relkind = 'r';
        IF NOT FOUND THEN
            RETURN QUERY SELECT 'table_missing'::text, 'table_id ' || lt.table_id,
                'the table with oid ' || lt.relid || ' no longer exists';
            CONTINUE;
        END IF;
        obj := pg_catalog.format('%I.%I', nsp, rname);

        BEGIN
            RETURN QUERY
            SELECT d.check_name, obj, d.detail
              FROM kingres.ledger_table_defects(lt.relid, lt.table_id) d;

            -- "kl_r.*" is the whole row whatever the columns are called.
            -- The entries that are still in force: every I counts one, every D
            -- takes one away (an append-only table has no D).
            EXECUTE pg_catalog.format($q$
                SELECT pg_catalog.array_agg(m.d ORDER BY m.d)
                  FROM (SELECT 'row hash ' || pg_catalog.encode(COALESCE(r.h, e.h), 'hex') || ': '
                               || COALESCE(r.n, 0) || ' row(s) in the table, '
                               || COALESCE(e.n, 0) || ' entry(ies) in the ledger' AS d
                          FROM (SELECT kingres.ledger_row_hash(kl_r.*) AS h, pg_catalog.count(*) AS n
                                  FROM ONLY %I.%I AS kl_r GROUP BY 1) r
                          FULL JOIN (SELECT le.row_hash AS h,
                                            pg_catalog.sum(CASE le.op WHEN 'I' THEN 1 WHEN 'D' THEN -1 ELSE 0 END) AS n
                                       FROM kingres.ledger_entry le
                                      WHERE le.table_id = %L GROUP BY 1) e ON e.h = r.h
                         WHERE COALESCE(r.n, 0) <> COALESCE(e.n, 0)) m
                $q$, nsp, rname, lt.table_id)
              INTO findings;
            IF findings IS NOT NULL THEN
                RETURN QUERY SELECT 'table_rows'::text, obj, f.d
                               FROM pg_catalog.unnest(findings) AS f(d);
            END IF;

            IF lt.mode = 'updatable' THEN
                -- Every D entry has exactly one history row (same transaction,
                -- sequence number and row hash, recomputed from the history
                -- row), and every history row has its D entry.
                SELECT pg_catalog.format('%I.%I', n.nspname, c.relname) INTO hobj
                  FROM pg_catalog.pg_class c
                  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
                 WHERE c.oid = lt.history_relid AND c.relkind = 'r';
                IF NOT FOUND THEN
                    RETURN QUERY SELECT 'history_missing'::text, obj,
                        'the history table no longer exists';
                ELSE
                    EXECUTE pg_catalog.format($q$
                        SELECT pg_catalog.array_agg(m.d ORDER BY m.d)
                          FROM (SELECT CASE
                                         WHEN h.n IS NULL THEN 'D entry of tx ' || e.ledger_txid || ' seq ' || e.seq || ' has no history row'
                                         WHEN e.ledger_txid IS NULL THEN 'history row of tx ' || COALESCE(h.ledger_txid::pg_catalog.text, 'NULL') || ' seq ' || COALESCE(h.ledger_seq::pg_catalog.text, 'NULL') || ' has no D entry'
                                         WHEN h.n > 1 THEN 'D entry of tx ' || e.ledger_txid || ' seq ' || e.seq
                                              || ' has ' || h.n || ' history rows'
                                         WHEN h.h IS DISTINCT FROM pg_catalog.encode(e.row_hash, 'hex') THEN 'history row of tx ' || e.ledger_txid || ' seq ' || e.seq
                                              || ' does not hash to the row hash of its D entry'
                                         WHEN h.bad_op THEN 'history row of tx ' || e.ledger_txid || ' seq ' || e.seq
                                              || ' has an operation other than D'
                                       END AS d
                                  FROM (SELECT le.ledger_txid, le.seq, le.row_hash
                                          FROM kingres.ledger_entry le
                                         WHERE le.table_id = %L AND le.op = 'D') e
                                  FULL JOIN (SELECT kl_h.ledger_txid, kl_h.ledger_seq,
                                                    pg_catalog.count(*) AS n,
                                                    pg_catalog.min(pg_catalog.encode(kingres.ledger_history_row_hash(kl_h.*), 'hex')) AS h,
                                                    pg_catalog.bool_or(kl_h.ledger_op IS DISTINCT FROM 'D') AS bad_op
                                               FROM ONLY %s AS kl_h
                                              GROUP BY 1, 2) h
                                    ON h.ledger_txid = e.ledger_txid AND h.ledger_seq = e.seq) m
                         WHERE m.d IS NOT NULL
                        $q$, lt.table_id, hobj)
                      INTO findings;
                    IF findings IS NOT NULL THEN
                        RETURN QUERY SELECT 'history_rows'::text, obj, f.d
                                       FROM pg_catalog.unnest(findings) AS f(d);
                    END IF;
                END IF;
            ELSE
                -- An append-only table has no D entries.
                IF EXISTS (SELECT 1 FROM kingres.ledger_entry le
                            WHERE le.table_id = lt.table_id AND le.op = 'D') THEN
                    RETURN QUERY SELECT 'entry_op'::text, obj,
                        'the table is append-only but the ledger has D entries for it';
                END IF;
            END IF;
        EXCEPTION WHEN OTHERS THEN
            RETURN QUERY SELECT 'table_error'::text, obj,
                'the table cannot be checked: ' || SQLERRM;
        END;
    END LOOP;

    -- Client signatures (0.3, part B).  The three tables are ledger tables and
    -- are compared with their entries like every other ledger table, so a
    -- deleted, changed or inserted attestation, key or revocation is a table_rows
    -- finding.  These checks are about what the rows say.  A key or an
    -- attestation row whose signature does not verify is for the verifier
    -- (kingres-verify attestation): the signatures are checked from the proof of
    -- an entry, one at a time.
    RETURN QUERY
    SELECT 'attestation_duplicate'::text,
           'key ' || pg_catalog.encode(a.key_id, 'hex') || ' nonce ' || pg_catalog.encode(a.nonce, 'hex'),
           pg_catalog.count(*) || ' attestations use the same client key and nonce'
      FROM kingres.ledger_attestation a
     GROUP BY a.key_id, a.nonce
    HAVING pg_catalog.count(*) > 1;
    RETURN QUERY
    SELECT 'attestation_duplicate'::text,
           'tx ' || a.ledger_txid || ' seq ' || a.seq,
           pg_catalog.count(*) || ' attestations of the same entry'
      FROM kingres.ledger_attestation a
     GROUP BY a.ledger_txid, a.seq
    HAVING pg_catalog.count(*) > 1;
    RETURN QUERY
    SELECT 'client_key_unknown'::text,
           'tx ' || a.ledger_txid || ' seq ' || a.seq,
           'the attestation names client key ' || pg_catalog.encode(a.key_id, 'hex')
           || ', which is not registered'
      FROM kingres.ledger_attestation a
     WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_client_key k WHERE k.key_id = a.key_id);
    RETURN QUERY
    SELECT 'client_key_unknown'::text,
           'key ' || pg_catalog.encode(r.key_id, 'hex'),
           'a revocation names a client key that is not registered'
      FROM kingres.ledger_client_key_revocation r
     WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_client_key k WHERE k.key_id = r.key_id);
    RETURN QUERY
    SELECT 'client_key_id'::text,
           'key ' || pg_catalog.encode(k.key_id, 'hex'),
           'the key identifier is not the first 16 bytes of the SHA-256 of the public key'
      FROM kingres.ledger_client_key k
     WHERE k.key_id IS DISTINCT FROM pg_catalog.substr(pg_catalog.sha256(k.public_key), 1, 16);
    RETURN QUERY
    SELECT 'attestation_orphan'::text,
           'tx ' || a.ledger_txid || ' seq ' || a.seq,
           'the attestation is for an entry that does not exist'
      FROM kingres.ledger_attestation a
     WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_entry e
                        WHERE e.ledger_txid = a.ledger_txid AND e.seq = a.seq);
    -- An entry of a table with the signature policy required that has no
    -- attestation, for the transactions with an identifier above the date of the
    -- switch (signature_required_from).  The switch waits for every open writer
    -- of the table and the date is taken under the lock that it holds, so a
    -- transaction that wrote the table and committed before the switch has a
    -- lower identifier and is not asked for one.  A transaction that began
    -- before the date and wrote the table after the switch has a lower
    -- identifier too; it cannot commit without attestations (the check at
    -- commit), and is not looked for here.  The policy is not covered by a
    -- hash, so a superuser who switched it off by editing the catalog is not
    -- seen (README).
    RETURN QUERY
    SELECT 'unattested_entry'::text,
           'tx ' || e.ledger_txid || ' seq ' || e.seq,
           'the entry of ledger table_id ' || e.table_id
           || ' has no attestation, and the table has had the signature policy required since transaction '
           || t.signature_required_from
      FROM kingres.ledger_entry e
      JOIN kingres.ledger_table t ON t.table_id = e.table_id
     WHERE t.signature_policy = 'required'
       AND t.signature_required_from < e.ledger_txid
       AND NOT EXISTS (SELECT 1 FROM kingres.ledger_attestation a
                        WHERE a.ledger_txid = e.ledger_txid AND a.seq = e.seq)
     ORDER BY e.ledger_txid, e.seq;

    -- A capture trigger of the extension on a table that is not registered
    -- (for example in a database that was restored from a dump, which keeps
    -- the triggers of the tables but not the ledger).
    RETURN QUERY
    SELECT 'trigger'::text, pg_catalog.format('%I.%I', n.nspname, c.relname),
           'the capture trigger of the ledger is on a table that is not registered in kingres.ledger_table'
      FROM pg_catalog.pg_trigger g
      JOIN pg_catalog.pg_proc p ON p.oid = g.tgfoid
      JOIN pg_catalog.pg_class c ON c.oid = g.tgrelid
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE p.prosrc = 'kingres_ledger_capture'
       AND p.prolang = (SELECT l.oid FROM pg_catalog.pg_language l WHERE l.lanname = 'c')
       AND kingres.ledger_is_member('pg_catalog.pg_proc'::pg_catalog.regclass, p.oid)
       AND NOT EXISTS (SELECT 1 FROM kingres.ledger_table t WHERE t.relid::pg_catalog.oid = g.tgrelid)
     ORDER BY 2;

    -- The ledger catalog tables and the event triggers.
    RETURN QUERY SELECT d.check_name, d.object, d.detail
                   FROM kingres.ledger_catalog_defects() d;
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_verify(bigint, bigint) IS
    'Recompute the ledger in the given block range; one row per problem, no rows when consistent. Not executable by PUBLIC.';

-- Proof that one row of a ledger table is in the ledger: the entry's audit
-- path in its transaction tree, the transaction's audit path in its block, the
-- block header and the headers of the blocks after it up to the latest block.
--
-- SECURITY DEFINER because it reads the entries and the transactions, which
-- the caller may not be allowed to read; the caller needs EXECUTE only, and
-- whoever has it can learn the row hashes of the table.
CREATE FUNCTION kingres.ledger_inclusion_proof(relation regclass, row_hash bytea,
                                             op "char")
    RETURNS jsonb
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    tid        int;
    chain      bytea;
    e          kingres.ledger_entry%ROWTYPE;
    t          kingres.ledger_tx%ROWTYPE;
    b          kingres.ledger_blocks%ROWTYPE;
    tip        kingres.ledger_blocks%ROWTYPE;
    entry_idx  int;
    entry_n    int;
    tx_idx     int;
    tx_n       int;
    leaves     bytea[];
    epath      bytea[];
    tpath      bytea[];
    chn        jsonb;
    proof      jsonb;
BEGIN
    IF op IS NULL OR op NOT IN ('I', 'D') THEN
        RAISE EXCEPTION 'operation must be I or D'
            USING ERRCODE = '22023';
    END IF;
    SELECT lt.table_id INTO tid FROM kingres.ledger_table lt WHERE lt.relid = relation;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'table "%" is not under ledger control', relation
            USING ERRCODE = '42704';
    END IF;
    SELECT c.chain_id INTO chain FROM kingres.ledger_chain c;

    SELECT le.* INTO e
      FROM kingres.ledger_entry le
      JOIN kingres.ledger_tx lx ON lx.ledger_txid = le.ledger_txid
     WHERE le.table_id = tid AND le.row_hash = ledger_inclusion_proof.row_hash
       AND le.op = ledger_inclusion_proof.op
     ORDER BY le.ledger_txid, le.seq
     LIMIT 1;
    IF NOT FOUND THEN
        IF EXISTS (SELECT 1 FROM kingres.ledger_entry le
                    WHERE le.table_id = tid AND le.row_hash = ledger_inclusion_proof.row_hash
                      AND le.op = ledger_inclusion_proof.op) THEN
            RAISE EXCEPTION 'the row is not in a block yet'
                USING ERRCODE = '55000',
                      HINT = 'Call kingres.ledger_build_block() or kingres.ledger_generate_digest() first.';
        END IF;
        IF ledger_inclusion_proof.op = 'I' THEN
            RAISE EXCEPTION 'no ledger entry with this row hash for table "%"', relation
                USING ERRCODE = '02000';
        END IF;
        RAISE EXCEPTION 'no ledger entry of operation % with this row hash for table "%"',
            ledger_inclusion_proof.op, relation
            USING ERRCODE = '02000';
    END IF;

    SELECT lx.* INTO t FROM kingres.ledger_tx lx WHERE lx.ledger_txid = e.ledger_txid;
    SELECT lb.* INTO b FROM kingres.ledger_blocks lb WHERE lb.height = t.block_height;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'block % of the transaction is missing', t.block_height
            USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
    END IF;

    -- Path inside the transaction.
    SELECT pg_catalog.array_agg(kingres.ledger_tx_leaf(x.seq, x.table_id, x.op, x.row_hash) ORDER BY x.seq),
           pg_catalog.count(*) FILTER (WHERE x.seq < e.seq)::int
      INTO leaves, entry_idx
      FROM kingres.ledger_entry x WHERE x.ledger_txid = e.ledger_txid;
    entry_n := pg_catalog.cardinality(leaves);
    epath := kingres.ledger_merkle_path(leaves, entry_idx);

    -- Path inside the block.
    SELECT pg_catalog.array_agg(kingres.ledger_block_tx_leaf(x.ledger_txid, x.entry_count, x.merkle_root, x.user_name)
                                ORDER BY x.tx_index),
           pg_catalog.count(*) FILTER (WHERE x.tx_index < t.tx_index)::int
      INTO leaves, tx_idx
      FROM kingres.ledger_tx x WHERE x.block_height = b.height;
    tx_n := pg_catalog.cardinality(leaves);
    tpath := kingres.ledger_merkle_path(leaves, tx_idx);

    -- Headers of the blocks after this one, up to the latest.
    SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
               'height', x.height,
               'hash_version', x.hash_version,
               'prev_hash', pg_catalog.encode(x.prev_hash, 'hex'),
               'tx_merkle_root', pg_catalog.encode(x.tx_merkle_root, 'hex'),
               'tx_count', x.tx_count,
               'closed_at_us', kingres.ledger_timestamptz_us(x.closed_at),
               'block_hash', pg_catalog.encode(x.block_hash, 'hex'))
               ORDER BY x.height)
      INTO chn
      FROM kingres.ledger_blocks x WHERE x.height > b.height;

    proof := pg_catalog.jsonb_build_object(
        'format', 1,
        'chain_id', pg_catalog.encode(chain, 'hex'),
        'relation', relation::text,
        'table_id', e.table_id,
        'ledger_txid', e.ledger_txid,
        'seq', e.seq,
        'op', e.op::text,
        'row_hash', pg_catalog.encode(e.row_hash, 'hex'),
        'entry_index', entry_idx,
        'entry_count', entry_n,
        'entry_path', (SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.encode(p, 'hex') ORDER BY o), '[]'::jsonb)
                         FROM pg_catalog.unnest(epath) WITH ORDINALITY AS u(p, o)),
        'tx_root', pg_catalog.encode(t.merkle_root, 'hex'),
        'user_name', t.user_name::text,
        'tx_index', tx_idx,
        'tx_path', (SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.encode(p, 'hex') ORDER BY o), '[]'::jsonb)
                      FROM pg_catalog.unnest(tpath) WITH ORDINALITY AS u(p, o)),
        'block', pg_catalog.jsonb_build_object(
            'height', b.height,
            'hash_version', b.hash_version,
            'prev_hash', pg_catalog.encode(b.prev_hash, 'hex'),
            'tx_merkle_root', pg_catalog.encode(b.tx_merkle_root, 'hex'),
            'tx_count', b.tx_count,
            'closed_at_us', kingres.ledger_timestamptz_us(b.closed_at),
            'block_hash', pg_catalog.encode(b.block_hash, 'hex')),
        'chain', COALESCE(chn, '[]'::jsonb));

    -- Self-check against the latest block: a proof built from an
    -- inconsistent ledger must not be handed out.
    SELECT lb.* INTO tip FROM kingres.ledger_blocks lb ORDER BY lb.height DESC LIMIT 1;
    IF NOT kingres.ledger_verify_proof(
               proof,
               pg_catalog.jsonb_build_object('chain_id', pg_catalog.encode(chain, 'hex'),
                                             'height', tip.height,
                                             'block_hash', pg_catalog.encode(tip.block_hash, 'hex'))) THEN
        RAISE EXCEPTION 'the stored ledger is inconsistent; no proof can be built'
            USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
    END IF;
    RETURN proof;
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_inclusion_proof(regclass, bytea, "char") IS
    'Proof for the (first) entry of operation op (I, the default, or D) with this row hash: the chain identity, both Merkle paths, the block header and the headers up to the latest block. A proof for D proves that the version was removed in that transaction. Not executable by PUBLIC.';

-- The form of 0.1: the proof of the (first) I entry.  SECURITY DEFINER so
-- that a role that was granted EXECUTE on this signature before 0.2 keeps
-- what it had without being granted the three-argument form.
CREATE FUNCTION kingres.ledger_inclusion_proof(relation regclass, row_hash bytea)
    RETURNS jsonb
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
    SELECT kingres.ledger_inclusion_proof(relation, row_hash, 'I'::pg_catalog."char")
$fn$;
COMMENT ON FUNCTION kingres.ledger_inclusion_proof(regclass, bytea) IS
    'Proof for the (first) I entry with this row hash; the same as the three-argument form with operation I. Not executable by PUBLIC.';
-- Everything that reads the chain or writes the ledger is an administrative
-- action.  The functions that write the ledger or scan the chain are not
-- executable by PUBLIC; GRANT EXECUTE to the roles of the KingresDB agent,
-- and to an auditor role (see the README).
REVOKE ALL ON FUNCTION kingres.ledger_enable(regclass, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_build_block() FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_generate_digest() FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_verify_digest(jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_verify(bigint, bigint) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_remove_stray(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_inclusion_proof(regclass, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_inclusion_proof(regclass, bytea, "char") FROM PUBLIC;

--------------------------------------------------------------------------
-- Client signatures (0.3, part B): the tables, the functions
--------------------------------------------------------------------------
-- kingres/docs/design/ledger-phase7.md, 3.6 to 3.8 and 8.1; src/sign.c.  A
-- client attestation is an Ed25519 signature by a key the client holds over
-- one ledger entry of one row version.  The registry of the keys, the
-- revocations and the attestations are rows of three tables of the extension
-- that are ledger tables themselves: registered in ledger_table as append-only
-- tables by ledger_register_internal below, entered into the chain by the
-- capture trigger, covered by blocks, proofs and ledger_verify, and written by
-- the code of the extension only (the guard refuses every other statement that
-- writes them, whatever the role holds).  Nothing here changes hash format 1.

CREATE TABLE kingres.ledger_client_key (
    key_id        bytea       NOT NULL,
    algorithm     text        NOT NULL,
    public_key    bytea       NOT NULL,
    role_name     name,
    label         text        NOT NULL,
    registered_by name        NOT NULL,
    registered_at timestamptz NOT NULL,
    CONSTRAINT ledger_client_key_pkey PRIMARY KEY (key_id),
    CONSTRAINT ledger_client_key_key_id_check CHECK (octet_length(key_id) = 16),
    CONSTRAINT ledger_client_key_algorithm_check CHECK (algorithm = 'ed25519'),
    CONSTRAINT ledger_client_key_public_key_check CHECK (octet_length(public_key) = 32),
    CONSTRAINT ledger_client_key_label_check CHECK (char_length(label) <= 200)
);

CREATE TABLE kingres.ledger_client_key_revocation (
    key_id     bytea       NOT NULL,
    reason     text        NOT NULL,
    revoked_by name        NOT NULL,
    revoked_at timestamptz NOT NULL,
    CONSTRAINT ledger_client_key_revocation_pkey PRIMARY KEY (key_id),
    CONSTRAINT ledger_client_key_revocation_key_id_check CHECK (octet_length(key_id) = 16),
    CONSTRAINT ledger_client_key_revocation_reason_check CHECK (char_length(reason) <= 200)
);

CREATE TABLE kingres.ledger_attestation (
    ledger_txid bigint NOT NULL,
    seq         int    NOT NULL,
    key_id      bytea  NOT NULL,
    nonce       bytea  NOT NULL,
    signature   bytea  NOT NULL,
    CONSTRAINT ledger_attestation_pkey PRIMARY KEY (ledger_txid, seq),
    CONSTRAINT ledger_attestation_key_nonce_key UNIQUE (key_id, nonce),
    CONSTRAINT ledger_attestation_key_id_check CHECK (octet_length(key_id) = 16),
    CONSTRAINT ledger_attestation_nonce_check CHECK (octet_length(nonce) = 16),
    CONSTRAINT ledger_attestation_signature_check CHECK (octet_length(signature) = 64)
);

COMMENT ON TABLE kingres.ledger_client_key IS
    'Registered client keys (Ed25519): key_id is the first 16 bytes of the SHA-256 of the public key; role_name NULL lets any role attest with it. A ledger table itself, written by ledger_client_key_register only.';
COMMENT ON TABLE kingres.ledger_client_key_revocation IS
    'Revocations of client keys, one row per revoked key. A ledger table itself, written by ledger_client_key_revoke only.';
COMMENT ON TABLE kingres.ledger_attestation IS
    'One row per attested ledger entry (ledger_txid, seq): the client key, a nonce and the Ed25519 signature. A ledger table itself, written by ledger_attest only.';

-- Not readable by PUBLIC, like the entries: the service role and the
-- administrators are granted SELECT (README).
REVOKE ALL ON kingres.ledger_client_key, kingres.ledger_client_key_revocation,
    kingres.ledger_attestation FROM PUBLIC;

-- The same triggers as the other ledger tables: no update, delete or
-- truncate, and the guard trigger for the writers that never reach the
-- executor hook.  The capture trigger is armed by ledger_register_internal.
CREATE TRIGGER kingres_ledger_block_modify
    BEFORE UPDATE OR DELETE ON kingres.ledger_client_key
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate
    BEFORE TRUNCATE ON kingres.ledger_client_key
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_guard_write
    BEFORE INSERT ON kingres.ledger_client_key
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_modify
    BEFORE UPDATE OR DELETE ON kingres.ledger_client_key_revocation
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate
    BEFORE TRUNCATE ON kingres.ledger_client_key_revocation
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_guard_write
    BEFORE INSERT ON kingres.ledger_client_key_revocation
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_modify
    BEFORE UPDATE OR DELETE ON kingres.ledger_attestation
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_block_truncate
    BEFORE TRUNCATE ON kingres.ledger_attestation
    FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();
CREATE TRIGGER kingres_ledger_guard_write
    BEFORE INSERT ON kingres.ledger_attestation
    FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
ALTER TABLE kingres.ledger_client_key ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_client_key ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;
ALTER TABLE kingres.ledger_client_key ENABLE ALWAYS TRIGGER kingres_ledger_guard_write;
ALTER TABLE kingres.ledger_client_key_revocation ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_client_key_revocation ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;
ALTER TABLE kingres.ledger_client_key_revocation ENABLE ALWAYS TRIGGER kingres_ledger_guard_write;
ALTER TABLE kingres.ledger_attestation ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;
ALTER TABLE kingres.ledger_attestation ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;
ALTER TABLE kingres.ledger_attestation ENABLE ALWAYS TRIGGER kingres_ledger_guard_write;

-- Registers one of the three tables above as an append-only ledger table:
-- arms its capture trigger with the next identifier of the registry and writes
-- the row of the registry; for this script only (src/sign.c).  The tables
-- hold no row, so no entry is written, and an upgrade writes none.  It takes
-- no content from its caller; not executable by anybody else.
CREATE FUNCTION kingres.ledger_register_internal(relation regclass) RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_ledger_register_internal'
    LANGUAGE C VOLATILE STRICT
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_register_internal(regclass) IS
    'Internal: register a table of client signatures as a ledger table; callable by the install and upgrade scripts of the extension only.';
REVOKE ALL ON FUNCTION kingres.ledger_register_internal(regclass) FROM PUBLIC;

SELECT kingres.ledger_register_internal('kingres.ledger_client_key'::regclass);
SELECT kingres.ledger_register_internal('kingres.ledger_client_key_revocation'::regclass);
SELECT kingres.ledger_register_internal('kingres.ledger_attestation'::regclass);

-- The message a client signs, byte exact (README, "Client signatures"): the
-- tag "kingres-ledger/client-signature/v1" and a zero byte, the chain
-- identity, u32 table_id, the operation byte, the row hash, the key
-- identifier and the nonce.  A pure function, for a client that wants to
-- compare its own encoding.
CREATE FUNCTION kingres.ledger_attestation_message(chain_id bytea, table_id int,
                                                    op "char", row_hash bytea,
                                                    key_id bytea, nonce bytea)
    RETURNS bytea
    AS 'MODULE_PATHNAME', 'kingres_ledger_attestation_message'
    LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION kingres.ledger_attestation_message(bytea, int, "char", bytea, bytea, bytea) IS
    'The 136 bytes a client signs for one entry: the tag kingres-ledger/client-signature/v1, a zero byte, chain_id (32 bytes), u32 table_id, the operation byte (I or D), row_hash (32), key_id (16) and nonce (16).';

-- SECURITY DEFINER because they write the ledger tables above, which no other
-- role may write; the caller needs EXECUTE only, which an administrator
-- grants (PUBLIC has none).  They take no lock of the extension, so they run
-- inside the transaction of the caller, as ledger_attest must.
CREATE FUNCTION kingres.ledger_client_key_register(public_key bytea, role_name name,
                                                    label text)
    RETURNS bytea
    AS 'MODULE_PATHNAME', 'kingres_ledger_client_key_register'
    LANGUAGE C VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_client_key_register(bytea, name, text) IS
    'Register an Ed25519 client key (32 bytes); returns its key_id, the first 16 bytes of the SHA-256 of the public key. role_name NULL lets any role attest with it. Not executable by PUBLIC.';

CREATE FUNCTION kingres.ledger_client_key_revoke(key_id bytea, reason text)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_ledger_client_key_revoke'
    LANGUAGE C VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_client_key_revoke(bytea, text) IS
    'Revoke a client key. Attestations made in blocks before the revocation stay valid. Not executable by PUBLIC.';

CREATE FUNCTION kingres.ledger_attest(relation regclass, row_hash bytea, op "char",
                                       key_id bytea, nonce bytea, signature bytea)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_ledger_attest'
    LANGUAGE C VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_attest(regclass, bytea, "char", bytea, bytea, bytea) IS
    'Attest one ledger entry of the current transaction with a registered client key: the Ed25519 signature must verify over ledger_attestation_message. KL011 no such entry, KL012 key unknown, revoked or not allowed for this role, KL013 bad signature, KL014 nonce reused; 0A000 on a server built without OpenSSL. Not executable by PUBLIC.';

CREATE FUNCTION kingres.ledger_set_signature_policy(relation regclass, policy text)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_ledger_set_signature_policy'
    LANGUAGE C VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_set_signature_policy(regclass, text) IS
    'Switch a ledger table from the signature policy optional to required, for good: a transaction that writes an entry of the table without attesting it fails at commit (KL010). The caller owns the table. Not executable by PUBLIC.';

REVOKE ALL ON FUNCTION kingres.ledger_client_key_register(bytea, name, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_client_key_revoke(bytea, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_attest(regclass, bytea, "char", bytea, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_set_signature_policy(regclass, text) FROM PUBLIC;

--------------------------------------------------------------------------
-- Records for operators
--------------------------------------------------------------------------

-- Runs ledger_verify and records the run in kingres.ledger_verification;
-- returns the id of the record.  The record is written when the run is over:
-- PostgreSQL has no autonomous transaction, so a "running" row that survives
-- a failure of the caller's transaction cannot be written (the table allows
-- the state, but nothing writes it, because the table is append-only).  A
-- failure of ledger_verify itself is caught and recorded with the state
-- 'failed' and the error message; the function then returns normally and
-- warns.  If the caller's own transaction rolls back, the record is gone with
-- it.  blocks_checked is the number of blocks in the range; rows_checked the
-- number of ledger entries of the transactions in those blocks.  The
-- problems are the rows of ledger_verify, at most 100 of them (the count is
-- exact).
--
-- SECURITY DEFINER because it writes kingres.ledger_verification, which no
-- other role may write; the caller needs EXECUTE only, which PUBLIC does not
-- have.  started_by is the role that called it.
CREATE FUNCTION kingres.ledger_verify_recorded(from_height bigint DEFAULT 0,
                                                to_height bigint DEFAULT NULL)
    RETURNS bigint
    AS 'MODULE_PATHNAME', 'kingres_ledger_verify_recorded'
    LANGUAGE C VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_verify_recorded(bigint, bigint) IS
    'Run ledger_verify and record the run (state, counts, the first 100 problems) in kingres.ledger_verification; returns the id. A failure of the verification itself is recorded as state failed. Not executable by PUBLIC.';

-- One row for the management panel.  The view runs with the rights of its
-- owner, so a role that may read it sees the numbers whatever it may read of
-- the ledger tables; it is therefore not readable by PUBLIC (the number of
-- pending transactions tells how active the database is).
CREATE VIEW kingres.ledger_status AS
SELECT (SELECT pg_catalog.count(*) FROM kingres.ledger_table t
         WHERE t.mode = 'append_only'
           AND NOT kingres.ledger_is_member('pg_catalog.pg_class'::pg_catalog.regclass,
                                            t.relid::pg_catalog.oid))
           AS append_only_tables,
       (SELECT pg_catalog.count(*) FROM kingres.ledger_table t WHERE t.mode = 'updatable')
           AS updatable_tables,
       lb.height AS latest_block_height,
       lb.block_hash AS latest_block_hash,
       lb.closed_at AS latest_block_closed_at,
       (SELECT pg_catalog.count(*) FROM kingres.ledger_pending p) AS pending_transactions,
       (SELECT pg_catalog.clock_timestamp() - pg_catalog.min(p.queued_at)
          FROM kingres.ledger_pending p) AS oldest_pending_age,
       ld.digest_id AS last_digest_id,
       ld.generated_at AS last_digest_at,
       ld.height AS last_digest_height,
       lv.verification_id AS last_verification_id,
       lv.finished_at AS last_verification_at,
       lv.state AS last_verification_state,
       lv.problem_count AS last_verification_problems,
       ws.state AS worker_state,
       ws.consecutive_failures AS worker_consecutive_failures,
       ws.retry_after AS worker_retry_after,
       ws.last_error AS worker_last_error,
       dbo.rolname AS database_owner,
       dbo.rolsuper AS database_owner_is_superuser,
       dbd.droppers AS database_droppers,
       (pg_catalog.cardinality(dbd.droppers) > 0) AS database_droppable_by_non_superuser,
       dbd.potential AS database_potential_droppers,
       (SELECT pg_catalog.count(*) FROM kingres.ledger_client_key k
         WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_client_key_revocation r
                            WHERE r.key_id = k.key_id)) AS client_keys_active,
       (SELECT pg_catalog.count(*) FROM kingres.ledger_attestation) AS attestations,
       (SELECT pg_catalog.count(*) FROM kingres.ledger_table t
         WHERE t.signature_policy = 'required') AS required_tables
  FROM (SELECT 1) one
  CROSS JOIN LATERAL kingres.ledger_worker_state() ws
  CROSS JOIN (SELECT r.rolname, r.rolsuper
                FROM pg_catalog.pg_database d JOIN pg_catalog.pg_roles r ON r.oid = d.datdba
               WHERE d.datname = pg_catalog.current_database()) dbo
  CROSS JOIN (WITH RECURSIVE own(oid) AS (
                  SELECT d.datdba FROM pg_catalog.pg_database d
                   WHERE d.datname = pg_catalog.current_database()
                     AND d.datdba <> 6171::pg_catalog.oid
                  UNION
                  SELECT d2.datdba FROM pg_catalog.pg_database d, pg_catalog.pg_database d2
                   WHERE d.datname = pg_catalog.current_database()
                     AND d.datdba = 6171::pg_catalog.oid
                     AND d2.datdba <> 6171::pg_catalog.oid
              ), drop_now(oid) AS (
                  SELECT r.oid FROM pg_catalog.pg_roles r
                   WHERE NOT r.rolsuper
                     AND EXISTS (SELECT 1 FROM pg_catalog.pg_roles x
                                  WHERE (x.oid = r.oid OR pg_catalog.pg_has_role(r.oid, x.oid, 'SET'))
                                    AND (x.rolsuper
                                         OR EXISTS (SELECT 1 FROM own o
                                                     WHERE pg_catalog.pg_has_role(x.oid, o.oid, 'USAGE'))
                                         OR (r.oid >= 16384::pg_catalog.oid
                                             AND (pg_catalog.pg_has_role(x.oid, 'pg_execute_server_program', 'USAGE')
                                                  OR pg_catalog.pg_has_role(x.oid, 'pg_write_server_files', 'USAGE')))))
              ), can_make(oid) AS (
                  SELECT dn.oid FROM drop_now dn
                  UNION
                  SELECT r2.oid FROM pg_catalog.pg_roles r2
                   WHERE NOT r2.rolsuper AND r2.oid >= 16384::pg_catalog.oid
                     AND (pg_catalog.pg_has_role(r2.oid, 'pg_execute_server_program', 'MEMBER WITH ADMIN OPTION')
                          OR pg_catalog.pg_has_role(r2.oid, 'pg_write_server_files', 'MEMBER WITH ADMIN OPTION'))
                  UNION
                  SELECT r.oid FROM pg_catalog.pg_roles r, can_make c
                   WHERE NOT r.rolsuper
                     AND pg_catalog.pg_has_role(r.oid, c.oid, 'MEMBER WITH ADMIN OPTION')
              )
              SELECT COALESCE(pg_catalog.array_agg(r.rolname ORDER BY r.rolname)
                                  FILTER (WHERE r.oid IN (SELECT dn.oid FROM drop_now dn)),
                              '{}'::pg_catalog.name[]) AS droppers,
                     COALESCE(pg_catalog.array_agg(r.rolname ORDER BY r.rolname)
                                  FILTER (WHERE r.oid IN (SELECT cm.oid FROM can_make cm)
                                            AND r.oid NOT IN (SELECT dn.oid FROM drop_now dn)),
                              '{}'::pg_catalog.name[]) AS potential
                FROM pg_catalog.pg_roles r) dbd
  LEFT JOIN LATERAL (SELECT b.height, b.block_hash, b.closed_at
                       FROM kingres.ledger_blocks b
                      ORDER BY b.height DESC LIMIT 1) lb ON true
  LEFT JOIN LATERAL (SELECT d.digest_id, d.generated_at, d.height
                       FROM kingres.ledger_digest d
                      ORDER BY d.digest_id DESC LIMIT 1) ld ON true
  LEFT JOIN LATERAL (SELECT v.verification_id, v.finished_at, v.state, v.problem_count
                       FROM kingres.ledger_verification v
                      WHERE v.finished_at IS NOT NULL
                      ORDER BY v.verification_id DESC LIMIT 1) lv ON true;
COMMENT ON VIEW kingres.ledger_status IS
    'One row: the number of append-only and updatable ledger tables, the latest block, the pending transactions and the age of the oldest, the last digest and the last finished verification, and the state of the block-building worker for this database (ok, failed with the delay before the next attempt and the last error, ...), and the owner of the database with whether it is a superuser, and the roles that are not superusers and can drop the database, and the ledger with it (database_droppers: the owner if it is not a superuser, every role with the privileges of the owner, every role that can SET ROLE to one of those or to a superuser; computed as PostgreSQL does; for a database owned by pg_database_owner, every role that owns a database of the cluster is taken as the owner), and the roles that cannot now and can make themselves able to without a superuser (database_potential_droppers: ADMIN OPTION, directly or through a chain, on a role that can drop it; a role with CREATEROLE has it on the roles it created; the members of pg_execute_server_program and pg_write_server_files count as superusers here, and the roles with ADMIN OPTION on those two as the second kind), and the client signatures (client_keys_active: registered keys that are not revoked; attestations: rows of ledger_attestation; required_tables: tables with the signature policy required). The tables of the ledger itself (the three of client signatures) are not counted in append_only_tables. Not readable by PUBLIC.';

REVOKE ALL ON FUNCTION kingres.ledger_verify_recorded(bigint, bigint) FROM PUBLIC;
REVOKE ALL ON kingres.ledger_status FROM PUBLIC;

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
--
-- Why the refusal on a row above the head is right, and for ever (until an
-- operator acts, see the README, "A row above the head"):
--   * No honest path leaves such a row.  The builder writes a block, its
--     transaction rows and the digest in one transaction, and a statement
--     snapshot (a STABLE function reads the snapshot of the calling query,
--     and a standby replays a commit whole) shows all of them or none.  The
--     check was run against writers, builders and digest generators on a
--     primary and on a hot standby that was replaying, with no false alarm.
--   * Only a superuser who turned kingres_ledger.allow_manual_write on can
--     plant one (src/guard.c); a member of pg_write_all_data is refused like
--     every other role.  A row that was planted before the guard existed, or by
--     a superuser, makes the publisher stop here, and it stays stopped until a
--     superuser removes the row on purpose with ledger_remove_stray, which
--     keeps a copy: a row above the head may be the only trace of a block that
--     was removed.
--
-- What the error tells a role that has EXECUTE and nothing else (a review
-- note): the number of a block, a transaction id and a digest id, in the
-- damaged case only.  That is no row data.  The role may read the head of the
-- chain anyway, and a publisher needs the text to tell the operator which row
-- to look at, so the details stay in the message.
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
    tx       record;
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
    -- The newest transaction row, by the unique index (block_height,
    -- tx_index), and the newest digest row, by its key: two index probes.
    SELECT t.block_height AS th, t.ledger_txid AS tid INTO tx
      FROM kingres.ledger_tx t
     ORDER BY t.block_height DESC, t.tx_index DESC
     LIMIT 1;
    SELECT d.digest_id AS did, d.height AS dh, d.block_hash AS dhash INTO dg
      FROM kingres.ledger_digest d
     ORDER BY d.digest_id DESC
     LIMIT 1;
    IF b.bh IS NULL THEN
        -- No block at all: empty, unless something still records a block.
        IF tx.th IS NOT NULL THEN
            RAISE EXCEPTION 'the chain has no block, but the row of transaction % in kingres.ledger_tx names block %',
                tx.tid, tx.th
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify(); see the README, "A row above the head".';
        END IF;
        IF dg.dh IS NOT NULL THEN
            RAISE EXCEPTION 'the chain has no block, but digest % in kingres.ledger_digest names block %',
                dg.did, dg.dh
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify(); see the README, "A row above the head".';
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
    IF tx.th > b.bh THEN
        RAISE EXCEPTION 'the head is block %, but the row of transaction % in kingres.ledger_tx names block %: the end of the chain was removed, or the row was written by hand',
            b.bh, tx.tid, tx.th
            USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify(); see the README, "A row above the head".';
    END IF;
    IF dg.dh IS NOT NULL THEN
        IF dg.dh > b.bh THEN
            RAISE EXCEPTION 'the head is block %, but digest % in kingres.ledger_digest names block %: the end of the chain was removed, or the row was written by hand',
                b.bh, dg.did, dg.dh
                USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify(); see the README, "A row above the head".';
        END IF;
        SELECT x.block_hash INTO dg_hash FROM kingres.ledger_blocks x WHERE x.height = dg.dh;
        IF dg_hash IS DISTINCT FROM dg.dhash THEN
            RAISE EXCEPTION 'block % is not the block that digest % in kingres.ledger_digest names', dg.dh, dg.did
                USING ERRCODE = 'XX001', HINT = 'The chain was rewritten after the digest was recorded, or the digest row was written by hand; see the README, "A row above the head".';
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

--------------------------------------------------------------------------
-- The event triggers go last: every statement after them is checked, and the
-- checks need the whole extension to be in place.  They are ENABLE ALWAYS so
-- that session_replication_role = replica does not skip them.
--------------------------------------------------------------------------
CREATE EVENT TRIGGER kingres_ledger_guard_end ON ddl_command_end
    EXECUTE FUNCTION kingres.ledger_guard();
CREATE EVENT TRIGGER kingres_ledger_guard_drop ON sql_drop
    EXECUTE FUNCTION kingres.ledger_guard();
CREATE EVENT TRIGGER kingres_ledger_guard_rewrite ON table_rewrite
    EXECUTE FUNCTION kingres.ledger_guard();
ALTER EVENT TRIGGER kingres_ledger_guard_end ENABLE ALWAYS;
ALTER EVENT TRIGGER kingres_ledger_guard_drop ENABLE ALWAYS;
ALTER EVENT TRIGGER kingres_ledger_guard_rewrite ENABLE ALWAYS;
