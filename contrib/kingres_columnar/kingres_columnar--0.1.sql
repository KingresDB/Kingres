-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_columnar 0.1: the columnar table access method.

\echo Use "CREATE EXTENSION kingres_columnar" to load this file. \quit

-- The stripe catalog: one row per stripe, keyed by the relfilenode of the
-- relation the stripe is stored in.  Ordinary heap rows: a stripe is visible
-- to a snapshot exactly when its row is.  Nobody but the extension owner has
-- privileges on it; the access method reads and writes it with the heap API.
CREATE TABLE columnar_stripe (
    filenode      oid      NOT NULL,
    stripe_id     bigint   NOT NULL,
    first_row     bigint   NOT NULL,
    row_count     int      NOT NULL,
    chunk_groups  int      NOT NULL,
    natts         smallint NOT NULL,
    first_block   bigint   NOT NULL,
    block_count   int      NOT NULL,
    blob_len      bigint   NOT NULL,
    raw_bytes     bigint   NOT NULL,
    footer_offset bigint   NOT NULL,
    cid_points    bytea    NOT NULL,
    col_stats     bytea    NOT NULL,
    CONSTRAINT columnar_stripe_pkey PRIMARY KEY (filenode, stripe_id)
);
REVOKE ALL ON TABLE columnar_stripe FROM PUBLIC;
COMMENT ON TABLE columnar_stripe IS
    'Stripes of columnar tables, keyed by relfilenode. Maintained by the access method.';

CREATE FUNCTION columnar_handler(internal)
    RETURNS table_am_handler
    AS 'MODULE_PATHNAME', 'kingres_columnar_handler'
    LANGUAGE C STRICT;

CREATE ACCESS METHOD columnar TYPE TABLE HANDLER columnar_handler;
COMMENT ON ACCESS METHOD columnar IS 'KingresDB columnar storage: append-only, compressed, column-wise';

CREATE FUNCTION columnar_set_options(rel regclass,
                                     compression text DEFAULT NULL,
                                     compression_level int DEFAULT NULL,
                                     stripe_row_count int DEFAULT NULL,
                                     chunk_row_count int DEFAULT NULL)
    RETURNS void
    AS 'MODULE_PATHNAME', 'kingres_columnar_set_options'
    LANGUAGE C SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION columnar_options(rel regclass)
    RETURNS TABLE (compression text, compression_level int,
                   stripe_row_count int, chunk_row_count int)
    AS 'MODULE_PATHNAME', 'kingres_columnar_options'
    LANGUAGE C STRICT SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION columnar_info(rel regclass)
    RETURNS TABLE (stripes bigint, chunk_groups bigint, rows bigint,
                   bytes_on_disk bigint, bytes_uncompressed bigint,
                   compression_ratio float8)
    AS 'MODULE_PATHNAME', 'kingres_columnar_info'
    LANGUAGE C STRICT SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION columnar_stripes(rel regclass)
    RETURNS TABLE (stripe_id bigint, first_row bigint, row_count int,
                   chunk_groups int, bytes_on_disk bigint,
                   bytes_uncompressed bigint, written_by_xmin xid)
    AS 'MODULE_PATHNAME', 'kingres_columnar_stripes'
    LANGUAGE C STRICT SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION columnar_compact(rel regclass, min_rows int DEFAULT NULL)
    RETURNS int
    AS 'MODULE_PATHNAME', 'kingres_columnar_compact'
    LANGUAGE C SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION columnar_check(fix boolean DEFAULT false)
    RETURNS TABLE (check_name text, relation regclass, detail text)
    AS 'MODULE_PATHNAME', 'kingres_columnar_check'
    LANGUAGE C STRICT SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION columnar_reconcile()
    RETURNS int
    AS 'MODULE_PATHNAME', 'kingres_columnar_reconcile'
    LANGUAGE C STRICT SECURITY DEFINER
    SET search_path = pg_catalog, kingres, pg_temp;

-- Remove the stripe rows of files no relation uses any more after a DROP.
CREATE FUNCTION columnar_on_drop()
    RETURNS event_trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, kingres, pg_temp
AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_event_trigger_dropped_objects()
                WHERE object_type IN ('table', 'materialized view')) THEN
        PERFORM kingres.columnar_reconcile();
    END IF;
END;
$$;

-- Foreign keys on or to a columnar table, and row-level AFTER triggers on one,
-- are refused: they lock or re-read rows by their TID, which a columnar row
-- does not have until its stripe is written.
CREATE FUNCTION columnar_check_ddl()
    RETURNS event_trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, kingres, pg_temp
AS $$
BEGIN
    IF EXISTS (SELECT 1
                 FROM pg_catalog.pg_constraint c
                 JOIN pg_catalog.pg_class r ON r.oid IN (c.conrelid, c.confrelid)
                 JOIN pg_catalog.pg_am a ON a.oid = r.relam
                WHERE c.contype = 'f' AND a.amname = 'columnar') THEN
        RAISE EXCEPTION 'foreign keys on or to columnar tables are not supported'
            USING ERRCODE = '0A000',
                  HINT = 'Keep the referencing and the referenced table in row storage.';
    END IF;
    IF EXISTS (SELECT 1
                 FROM pg_catalog.pg_trigger t
                 JOIN pg_catalog.pg_class r ON r.oid = t.tgrelid
                 JOIN pg_catalog.pg_am a ON a.oid = r.relam
                WHERE a.amname = 'columnar' AND NOT t.tgisinternal
                  AND (t.tgtype & 1) <> 0 AND (t.tgtype & 2) = 0) THEN
        RAISE EXCEPTION 'row-level AFTER triggers on columnar tables are not supported'
            USING ERRCODE = '0A000',
                  HINT = 'Use a BEFORE ROW or a statement-level trigger, or keep the table in row storage.';
    END IF;
END;
$$;

CREATE EVENT TRIGGER kingres_columnar_on_drop ON sql_drop
    EXECUTE FUNCTION columnar_on_drop();
CREATE EVENT TRIGGER kingres_columnar_check_ddl ON ddl_command_end
    WHEN TAG IN ('CREATE TABLE', 'ALTER TABLE', 'CREATE TRIGGER')
    EXECUTE FUNCTION columnar_check_ddl();
ALTER EVENT TRIGGER kingres_columnar_on_drop ENABLE ALWAYS;
ALTER EVENT TRIGGER kingres_columnar_check_ddl ENABLE ALWAYS;

REVOKE ALL ON FUNCTION columnar_handler(internal) FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_set_options(regclass, text, int, int, int) FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_options(regclass) FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_info(regclass) FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_stripes(regclass) FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_compact(regclass, int) FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_check(boolean) FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_reconcile() FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_on_drop() FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_check_ddl() FROM PUBLIC;
-- The functions that act on one table check ownership themselves.
GRANT EXECUTE ON FUNCTION columnar_options(regclass), columnar_info(regclass),
    columnar_stripes(regclass), columnar_set_options(regclass, text, int, int, int),
    columnar_compact(regclass, int) TO PUBLIC;

GRANT USAGE ON SCHEMA kingres TO PUBLIC;
