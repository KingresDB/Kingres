-- kingres_ledger--0.1.sql
--
-- Install script of the kingres_ledger extension, version 0.1: the data
-- model, the C primitives and the SQL orchestration functions of the ledger
-- described in kingres/docs/design/ledger-mvp.md.
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

-- The schema of the extension must not be one that a role that is not a superuser holds or can create objects in: that role would own what the service role, a
-- superuser, reads and runs.  The check is the same in every script of the extension, the install scripts of the older versions and the upgrade scripts too (a
-- schema that was made, or given away, after the install is met by ALTER EXTENSION ... UPDATE).  A schema that does not exist yet is made by the script.
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
                  HINT = 'Drop the schema, or make a superuser its owner, before CREATE EXTENSION or ALTER EXTENSION ... UPDATE.';
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
                  HINT = 'Revoke CREATE on the schema before CREATE EXTENSION or ALTER EXTENSION ... UPDATE.';
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
    CONSTRAINT ledger_table_pkey PRIMARY KEY (table_id),
    CONSTRAINT ledger_table_relid_key UNIQUE (relid),
    CONSTRAINT ledger_table_mode_check CHECK (mode IN ('append_only'))
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
    CONSTRAINT ledger_entry_op_check CHECK (op IN ('I')),
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

-- Who may read what.  The tables that only describe the chain itself (the
-- registered tables, the chain identity and the block headers) are readable
-- by everybody.  The entries, the work queue and the transactions are not:
-- they hold the row hashes (a hash of a row with little entropy can be
-- confirmed by guessing the row), the identities of the tables and the names
-- of the roles that wrote, and when.  An auditor role needs
--   GRANT SELECT ON kingres.ledger_entry, kingres.ledger_pending,
--                   kingres.ledger_tx TO auditor;
-- only to read them directly; the verification functions below run with the
-- rights of the extension owner and need only EXECUTE.
REVOKE ALL ON kingres.ledger_table, kingres.ledger_chain,
    kingres.ledger_entry, kingres.ledger_pending, kingres.ledger_tx,
    kingres.ledger_blocks
    FROM PUBLIC;
GRANT SELECT ON kingres.ledger_table, kingres.ledger_chain,
    kingres.ledger_blocks
    TO PUBLIC;

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

CREATE FUNCTION kingres.ledger_hook_active() RETURNS boolean
    AS 'MODULE_PATHNAME', 'kingres_ledger_hook_active'
    LANGUAGE C STABLE;
COMMENT ON FUNCTION kingres.ledger_hook_active() IS
    'True when the protection hook is installed, that is, when the library is in shared_preload_libraries.';

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
    'AFTER INSERT row trigger of a ledger table: records the row hash in kingres.ledger_entry.';

CREATE FUNCTION kingres.ledger_block_modify() RETURNS trigger
    AS 'MODULE_PATHNAME', 'kingres_ledger_block_modify'
    LANGUAGE C VOLATILE
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_block_modify() IS
    'Trigger function that rejects UPDATE, DELETE and TRUNCATE on append-only ledger tables.';

-- SECURITY DEFINER because it writes the ledger tables; the callers are
-- ledger_enable and nobody else.
CREATE FUNCTION kingres.ledger_record_row(table_id int, row_data record)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_ledger_record_row'
    LANGUAGE C VOLATILE STRICT SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp;
COMMENT ON FUNCTION kingres.ledger_record_row(int, record) IS
    'Internal: enter an existing row of a ledger table into the ledger.';

REVOKE ALL ON FUNCTION kingres.ledger_capture() FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_record_row(int, record) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_lock_table(regclass) FROM PUBLIC;

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

    IF NOT kingres.ledger_trigger_ok(p_relid, 'kingres_ledger_capture',
               'kingres_ledger_capture', true, 5,
               pg_catalog.convert_to(p_table_id::pg_catalog.text, 'UTF8') || '\x00'::pg_catalog.bytea) THEN
        RETURN QUERY SELECT 'trigger'::text,
            'trigger kingres_ledger_capture is missing, disabled or not ENABLE ALWAYS'::text;
    END IF;
    IF NOT kingres.ledger_trigger_ok(p_relid, 'kingres_ledger_block_modify',
               'kingres_ledger_block_modify', false, 27, NULL) THEN
        RETURN QUERY SELECT 'trigger'::text,
            'trigger kingres_ledger_block_modify is missing, disabled or not ENABLE ALWAYS'::text;
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
        SELECT s.tbl, s.modtype
          FROM (VALUES ('ledger_table', 27), ('ledger_chain', 27),
                       ('ledger_entry', 27), ('ledger_pending', 19),
                       ('ledger_tx', 27), ('ledger_blocks', 27)) AS s(tbl, modtype)
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
    IF TG_EVENT = 'sql_drop' THEN
        FOR d IN
            SELECT o.object_type, o.object_identity
              FROM pg_catalog.pg_event_trigger_dropped_objects() o
             WHERE o.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
               AND o.object_type IN ('table', 'table column')
               AND (EXISTS (SELECT 1 FROM kingres.ledger_table t WHERE t.relid::pg_catalog.oid = o.objid)
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
                    WHERE t.relid::pg_catalog.oid = pg_catalog.pg_event_trigger_table_rewrite_oid())
           OR kingres.ledger_is_member('pg_catalog.pg_class'::pg_catalog.regclass,
                                        pg_catalog.pg_event_trigger_table_rewrite_oid()) THEN
            RAISE EXCEPTION 'cannot rewrite table %: it is a ledger table or part of the ledger',
                pg_event_trigger_table_rewrite_oid()::pg_catalog.regclass
                USING ERRCODE = 'KL002',
                      HINT = 'A rewrite changes how the rows are stored; the ledger tables cannot be rewritten by ALTER TABLE.';
        END IF;
    ELSE
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
    IF mode IS DISTINCT FROM 'append_only' THEN
        RAISE EXCEPTION 'ledger mode "%" is not supported yet', mode
            USING ERRCODE = '0A000',
                  HINT = 'Only mode "append_only" exists in this version.';
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
    IF nsp = 'information_schema' OR nsp LIKE 'pg\_%' THEN
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

    IF EXISTS (SELECT 1 FROM kingres.ledger_table lt WHERE lt.relid = rel.oid) THEN
        RAISE EXCEPTION 'table "%" is already under ledger control', rel.relname
            USING ERRCODE = '42710';
    END IF;

    tid := pg_catalog.nextval(
        pg_catalog.pg_get_serial_sequence('kingres.ledger_table', 'table_id')::pg_catalog.regclass)::int;

    EXECUTE pg_catalog.format(
        'CREATE TRIGGER kingres_ledger_capture AFTER INSERT ON %I.%I FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture(%L)',
        nsp, rel.relname, tid::text);
    EXECUTE pg_catalog.format(
        'CREATE TRIGGER kingres_ledger_block_modify BEFORE UPDATE OR DELETE ON %I.%I FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify()',
        nsp, rel.relname);
    EXECUTE pg_catalog.format(
        'CREATE TRIGGER kingres_ledger_block_truncate BEFORE TRUNCATE ON %I.%I FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify()',
        nsp, rel.relname);

    -- The order matters for the protection hook: the capture trigger is armed
    -- last, which is what lets these statements through.
    EXECUTE pg_catalog.format('ALTER TABLE %I.%I ENABLE ALWAYS TRIGGER kingres_ledger_block_modify', nsp, rel.relname);
    EXECUTE pg_catalog.format('ALTER TABLE %I.%I ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate', nsp, rel.relname);
    EXECUTE pg_catalog.format('ALTER TABLE %I.%I ENABLE ALWAYS TRIGGER kingres_ledger_capture', nsp, rel.relname);

    INSERT INTO kingres.ledger_table (table_id, relid, mode)
    OVERRIDING SYSTEM VALUE
    VALUES (tid, rel.oid, mode);

    -- Rows that already exist are entered by this transaction.  "kl_r.*" is
    -- the whole row whatever the columns of the table are called.
    EXECUTE pg_catalog.format(
        'SELECT kingres.ledger_record_row(%L::int, kl_r.*) FROM ONLY %I.%I AS kl_r',
        tid, nsp, rel.relname);
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_enable(regclass, text) IS
    'Put a table under ledger control, for good; existing rows are entered by the calling transaction. Needs READ COMMITTED, ownership of the table and EXECUTE, which is not granted to PUBLIC.';

--------------------------------------------------------------------------
-- Blocks and digests
--------------------------------------------------------------------------

-- Closes a block from the pending transactions.  Only one builder runs at a
-- time: they serialise on a table lock on kingres.ledger_chain, which only
-- the extension owner can take (the lock mode conflicts with every writer,
-- and no other role has a privilege that allows it); an advisory lock would
-- be available to every role, which could then stop all block building.
-- READ COMMITTED is required: the builder must see the block written by the
-- previous builder, which a REPEATABLE READ snapshot taken earlier would not.
--
-- SECURITY DEFINER because it writes the ledger tables, which no other role
-- may write.
CREATE FUNCTION kingres.ledger_build_block() RETURNS bigint
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    max_tx   int;
    txids    bigint[];
    h        bigint;
    prev     bytea;
    prev_at  timestamptz;
    chain    bytea;
    txroot   bytea;
    n        int;
    closed   timestamptz;
    bh       bytea;
BEGIN
    IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'ledger_build_block requires the READ COMMITTED isolation level'
            USING ERRCODE = '0A000';
    END IF;

    LOCK TABLE kingres.ledger_chain IN EXCLUSIVE MODE;

    max_tx := pg_catalog.current_setting('kingres_ledger.max_block_transactions')::int;

    -- Pending transactions are committed ones: their pending row became
    -- visible together with their entries.
    SELECT pg_catalog.array_agg(p.ledger_txid ORDER BY p.ledger_txid)
      INTO txids
      FROM (SELECT lp.ledger_txid
              FROM kingres.ledger_pending lp
             ORDER BY lp.ledger_txid
             LIMIT max_tx) p;
    IF txids IS NULL THEN
        RETURN NULL;
    END IF;

    SELECT c.chain_id INTO chain FROM kingres.ledger_chain c;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'the identity of the chain is missing'
            USING ERRCODE = 'XX001', HINT = 'Run kingres.ledger_verify().';
    END IF;

    SELECT b.height + 1, b.block_hash, b.closed_at INTO h, prev, prev_at
      FROM kingres.ledger_blocks b ORDER BY b.height DESC LIMIT 1;
    IF NOT FOUND THEN
        h := 0;
        prev := kingres.ledger_genesis_hash(chain);
        prev_at := NULL;
    END IF;

    INSERT INTO kingres.ledger_tx
           (ledger_txid, block_height, tx_index, entry_count, merkle_root, user_name)
    SELECT p.ledger_txid, h,
           (pg_catalog.row_number() OVER (ORDER BY p.ledger_txid) - 1)::int,
           r.cnt, r.root, p.user_name
      FROM kingres.ledger_pending p
     CROSS JOIN LATERAL (
           SELECT pg_catalog.count(*)::int AS cnt,
                  kingres.ledger_merkle_root(
                      kingres.ledger_tx_leaf(e.seq, e.table_id, e.op, e.row_hash)
                      ORDER BY e.seq) AS root
             FROM kingres.ledger_entry e
            WHERE e.ledger_txid = p.ledger_txid) r
     WHERE p.ledger_txid = ANY (txids);

    SELECT pg_catalog.count(*)::int,
           kingres.ledger_merkle_root(
               kingres.ledger_block_tx_leaf(t.ledger_txid, t.entry_count, t.merkle_root, t.user_name)
               ORDER BY t.tx_index)
      INTO n, txroot
      FROM kingres.ledger_tx t
     WHERE t.block_height = h;

    -- The time of a block is the server's clock, but it never goes back: a
    -- clock that was set back, or a standby promoted with a clock that is
    -- behind, gives the time of the previous block.
    closed := pg_catalog.clock_timestamp();
    IF prev_at IS NOT NULL AND closed < prev_at THEN
        closed := prev_at;
    END IF;
    bh := kingres.ledger_block_hash(1::smallint, h, prev, txroot, n, closed);

    INSERT INTO kingres.ledger_blocks
           (height, hash_version, prev_hash, tx_merkle_root, tx_count, closed_at, block_hash)
    VALUES (h, 1, prev, txroot, n, closed, bh);

    DELETE FROM kingres.ledger_pending WHERE ledger_txid = ANY (txids);
    RETURN h;
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_build_block() IS
    'Close a block from the pending transactions; returns the new height, or NULL when nothing was pending.';

-- SECURITY DEFINER because it calls the block builder and reads the ledger.
CREATE FUNCTION kingres.ledger_generate_digest() RETURNS jsonb
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    b     kingres.ledger_blocks%ROWTYPE;
    chain bytea;
BEGIN
    PERFORM kingres.ledger_build_block();
    SELECT * INTO b FROM kingres.ledger_blocks lb ORDER BY lb.height DESC LIMIT 1;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    SELECT c.chain_id INTO chain FROM kingres.ledger_chain c;
    RETURN pg_catalog.jsonb_build_object(
        'database', pg_catalog.current_database(),
        'chain_id', pg_catalog.encode(chain, 'hex'),
        'hash_version', b.hash_version,
        'height', b.height,
        'block_hash', pg_catalog.encode(b.block_hash, 'hex'),
        'block_closed_at', pg_catalog.to_char(b.closed_at AT TIME ZONE 'UTC',
                                              'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
        'generated_at', pg_catalog.to_char(pg_catalog.clock_timestamp() AT TIME ZONE 'UTC',
                                           'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'));
END
$fn$;
COMMENT ON FUNCTION kingres.ledger_generate_digest() IS
    'Close a block if needed and return the digest (chain identity, height and block hash) of the latest block as JSON; NULL when the ledger is empty.';

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
    'Check an inclusion proof against a trusted digest, and that it is a proof for this table id and this row hash.';

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
     WHERE pg_catalog.octet_length(e.row_hash) <> 32 OR e.op <> 'I'
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
    FOR lt IN SELECT t.table_id, t.relid::pg_catalog.oid AS relid
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
            EXECUTE pg_catalog.format($q$
                SELECT pg_catalog.array_agg(m.d ORDER BY m.d)
                  FROM (SELECT 'row hash ' || pg_catalog.encode(COALESCE(r.h, e.h), 'hex') || ': '
                               || COALESCE(r.n, 0) || ' row(s) in the table, '
                               || COALESCE(e.n, 0) || ' entry(ies) in the ledger' AS d
                          FROM (SELECT kingres.ledger_row_hash(kl_r.*) AS h, pg_catalog.count(*) AS n
                                  FROM ONLY %I.%I AS kl_r GROUP BY 1) r
                          FULL JOIN (SELECT le.row_hash AS h, pg_catalog.count(*) AS n
                                       FROM kingres.ledger_entry le
                                      WHERE le.table_id = %L GROUP BY 1) e ON e.h = r.h
                         WHERE COALESCE(r.n, 0) <> COALESCE(e.n, 0)) m
                $q$, nsp, rname, lt.table_id)
              INTO findings;
            IF findings IS NOT NULL THEN
                RETURN QUERY SELECT 'table_rows'::text, obj, f.d
                               FROM pg_catalog.unnest(findings) AS f(d);
            END IF;
        EXCEPTION WHEN OTHERS THEN
            RETURN QUERY SELECT 'table_error'::text, obj,
                'the table cannot be checked: ' || SQLERRM;
        END;
    END LOOP;

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
CREATE FUNCTION kingres.ledger_inclusion_proof(relation regclass, row_hash bytea)
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
     ORDER BY le.ledger_txid, le.seq
     LIMIT 1;
    IF NOT FOUND THEN
        IF EXISTS (SELECT 1 FROM kingres.ledger_entry le
                    WHERE le.table_id = tid AND le.row_hash = ledger_inclusion_proof.row_hash) THEN
            RAISE EXCEPTION 'the row is not in a block yet'
                USING ERRCODE = '55000',
                      HINT = 'Call kingres.ledger_build_block() or kingres.ledger_generate_digest() first.';
        END IF;
        RAISE EXCEPTION 'no ledger entry with this row hash for table "%"', relation
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
COMMENT ON FUNCTION kingres.ledger_inclusion_proof(regclass, bytea) IS
    'Proof that a row is in the ledger: the chain identity, both Merkle paths, the block header and the headers up to the latest block. Not executable by PUBLIC.';

-- Everything that reads the chain or writes the ledger is an administrative
-- action.  The functions that write the ledger or scan the chain are not
-- executable by PUBLIC; GRANT EXECUTE to the roles of the KingresDB agent,
-- and to an auditor role (see the README).
REVOKE ALL ON FUNCTION kingres.ledger_enable(regclass, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_build_block() FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_generate_digest() FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_verify_digest(jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_verify(bigint, bigint) FROM PUBLIC;
REVOKE ALL ON FUNCTION kingres.ledger_inclusion_proof(regclass, bytea) FROM PUBLIC;

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
