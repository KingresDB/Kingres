-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_columnar 0.2: the columnar table access method.

\echo Use "CREATE EXTENSION kingres_columnar" to load this file. \quit

-- The stripe catalog: one row per stripe, keyed by the identity of the
-- stored content (an id in the metapage of the relation file, which a copy of
-- the file keeps and a rewrite or TRUNCATE replaces).  Ordinary heap rows: a stripe is visible
-- to a snapshot exactly when its row is.  Nobody but the extension owner has
-- privileges on it; the access method reads and writes it with the heap API.
CREATE TABLE columnar_stripe (
    rel_uid       bigint   NOT NULL,
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
    maintenance   boolean  NOT NULL DEFAULT false,
    reserved      boolean  NOT NULL DEFAULT false,
    CONSTRAINT columnar_stripe_pkey PRIMARY KEY (rel_uid, stripe_id)
);
REVOKE ALL ON TABLE columnar_stripe FROM PUBLIC;
COMMENT ON TABLE columnar_stripe IS
    'Stripes of columnar tables, keyed by the identity of the stored content. Maintained by the access method.';
COMMENT ON COLUMN columnar_stripe.reserved IS
    'A claim on row numbers, not yet a stripe: a writer that reserves row numbers inserts the row when its first row goes in and replaces it with the stripe row when it writes the stripe. Readers skip it (version 0.2).';

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
                   compression_ratio float8, unreferenced_bytes bigint)
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

-- The cross-check of the indexes of one columnar table against its stripes: a visible
-- row with no entry in an index is damage (index_entry_missing); an entry with no row is
-- expected until the next VACUUM and is not reported.  It reads every index of the table.
CREATE FUNCTION columnar_check_indexes(rel regclass)
    RETURNS TABLE (check_name text, relation regclass, detail text)
    AS 'MODULE_PATHNAME', 'kingres_columnar_check_indexes'
    LANGUAGE C STRICT SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

-- The strict form of columnar_check: true only when every columnar table was
-- checked and nothing was found.  false for any row columnar_check() would
-- return, a table that could not be locked at once (not_checked) included.
CREATE FUNCTION columnar_check_clean()
    RETURNS boolean
    AS 'MODULE_PATHNAME', 'kingres_columnar_check_clean'
    LANGUAGE C STRICT SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION columnar_identity(rel regclass)
    RETURNS bigint
    AS 'MODULE_PATHNAME', 'kingres_columnar_identity'
    LANGUAGE C STRICT SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION columnar_creating_xid(rel regclass)
    RETURNS xid
    AS 'MODULE_PATHNAME', 'kingres_columnar_creating_xid'
    LANGUAGE C STRICT SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION columnar_table_xid(rel regclass)
    RETURNS xid
    AS 'MODULE_PATHNAME', 'kingres_columnar_table_xid'
    LANGUAGE C STRICT SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

CREATE FUNCTION columnar_reconcile()
    RETURNS int
    AS 'MODULE_PATHNAME', 'kingres_columnar_reconcile'
    LANGUAGE C STRICT SECURITY DEFINER
    SET search_path = pg_catalog, kingres, pg_temp;

-- How often each path of the write side was taken in this session (a
-- diagnostic, and what the tests of the paths read): rows written at once after
-- the commit-time flush or between the phases of a hand-over, buffers written
-- under memory pressure, by a committing subtransaction or because they held a
-- full stripe, writes refused at the ceiling or at a moment where the server
-- forbids an error, and so on.  With reset the counters start again.
CREATE FUNCTION columnar_write_paths(reset boolean DEFAULT false)
    RETURNS TABLE (path text, hits bigint)
    AS 'MODULE_PATHNAME', 'kingres_columnar_write_paths'
    LANGUAGE C STRICT SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

-- The clean-up of the files a command replaced or dropped, at the end of the
-- command.  The access method notes such a file itself, when the relation that
-- holds it is dropped (an object access hook: no event trigger is needed for
-- that), and removes the stripe rows of the noted files at the latest when the
-- transaction commits or prepares.  The event triggers below only call this
-- function so that it happens at the end of the command and not at the commit.
CREATE FUNCTION columnar_finish_rewrites()
    RETURNS int
    AS 'MODULE_PATHNAME', 'kingres_columnar_finish_rewrites'
    LANGUAGE C STRICT SECURITY DEFINER
    SET search_path = pg_catalog, kingres, pg_temp;

-- Whether an access method is the extension's, decided in C by what its
-- handler returns, the way every C path decides it.  The guard below asks it,
-- so that an access method over another SQL function with the extension's C
-- entry point (a second CREATE FUNCTION over the same symbol) is guarded like
-- the one over columnar_handler.
CREATE FUNCTION columnar_is_columnar_am(amoid oid)
    RETURNS boolean
    AS 'MODULE_PATHNAME', 'kingres_columnar_is_columnar_am'
    LANGUAGE C STRICT STABLE SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;

-- Remove the stripe rows of the files a DROP took away, at the end of the
-- command: only the files the command dropped are looked at (the access method
-- noted them when the relations were dropped), so the cost does not depend on
-- how many columnar tables exist.  Without this trigger (event triggers off,
-- the trigger disabled, single-user mode) the same rows are removed when the
-- transaction commits.
CREATE FUNCTION columnar_on_drop()
    RETURNS event_trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, kingres, pg_temp
AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_event_trigger_dropped_objects()
                WHERE object_type IN ('table', 'materialized view')) THEN
        PERFORM kingres.columnar_finish_rewrites();
    END IF;
END;
$$;

-- Objects the access method cannot support are refused when the command that
-- creates them ends.  The trigger looks only at the relations (and
-- publications) the command that fired it created or changed, including the
-- tables inside CREATE SCHEMA, so DDL that does not concern a columnar table
-- is never affected, whatever else exists in the database.  Where a command
-- reaches the access method itself the access method refuses too (a row
-- cannot be fetched by TID; indexes cannot be built).
--
--  * foreign keys on or to a columnar table, and row-level AFTER triggers on
--    one: they lock or re-read rows by their TID, which a columnar row does
--    not have until its stripe is written;
--  * columnar tables in a publication, and a publication that would include
--    one: columnar rows are not decoded for logical replication yet, so a
--    subscriber would silently miss them.
CREATE FUNCTION columnar_check_ddl()
    RETURNS event_trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    r       record;
    rels    oid[] := '{}';
    pubs    oid[] := '{}';
    cols    oid[];
BEGIN
    FOR r IN SELECT classid, objid FROM pg_event_trigger_ddl_commands() LOOP
        IF r.classid = 'pg_catalog.pg_class'::regclass THEN
            rels := rels || r.objid;
        ELSIF r.classid = 'pg_catalog.pg_namespace'::regclass THEN
            rels := rels || ARRAY(SELECT oid FROM pg_catalog.pg_class
                                   WHERE relnamespace = r.objid
                                     AND relkind IN ('r', 'p', 'm'));
        ELSIF r.classid = 'pg_catalog.pg_trigger'::regclass THEN
            rels := rels || ARRAY(SELECT tgrelid FROM pg_catalog.pg_trigger
                                   WHERE oid = r.objid);
        ELSIF r.classid = 'pg_catalog.pg_publication'::regclass THEN
            pubs := pubs || r.objid;
        ELSIF r.classid = 'pg_catalog.pg_publication_rel'::regclass THEN
            pubs := pubs || ARRAY(SELECT prpubid FROM pg_catalog.pg_publication_rel
                                   WHERE oid = r.objid);
        ELSIF r.classid = 'pg_catalog.pg_publication_namespace'::regclass THEN
            pubs := pubs || ARRAY(SELECT pnpubid FROM pg_catalog.pg_publication_namespace
                                   WHERE oid = r.objid);
        END IF;
    END LOOP;

    IF cardinality(rels) > 0 THEN
        -- the tables the keys of these relations point to (a foreign key TO a columnar table
        -- needs a row lock on the referenced row, which a columnar row cannot give)
        rels := rels || ARRAY(SELECT DISTINCT c.confrelid FROM pg_catalog.pg_constraint c
                               WHERE c.contype = 'f' AND c.conrelid = ANY (rels));
        -- partitions of the partitioned tables the command named
        rels := rels || ARRAY(SELECT t.relid
                                FROM unnest(rels) x,
                                     LATERAL pg_catalog.pg_partition_tree(x) t);
        -- only the columnar ones matter
        cols := ARRAY(SELECT DISTINCT c.oid
                        FROM pg_catalog.pg_class c
                       WHERE c.oid = ANY (rels)
                         AND c.relkind IN ('r', 'm')
                         AND kingres.columnar_is_columnar_am(c.relam));
    ELSE
        cols := '{}';
    END IF;

    IF cardinality(cols) > 0 THEN
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_constraint c
                    WHERE c.contype = 'f'
                      AND (c.conrelid = ANY (cols) OR c.confrelid = ANY (cols))) THEN
            RAISE EXCEPTION 'foreign keys on or to columnar tables are not supported'
                USING ERRCODE = '0A000',
                      HINT = 'Keep the referencing and the referenced table in row storage.';
        END IF;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t
                    WHERE t.tgrelid = ANY (cols) AND NOT t.tgisinternal
                      AND (t.tgtype & 1) <> 0 AND (t.tgtype & 2) = 0) THEN
            RAISE EXCEPTION 'row-level AFTER triggers on columnar tables are not supported'
                USING ERRCODE = '0A000',
                      HINT = 'Use a BEFORE ROW or a statement-level trigger, or keep the table in row storage.';
        END IF;
        -- What a publication publishes is what the server says it does
        -- (pg_get_publication_tables: all tables, a schema, the listed
        -- tables, the exceptions of 19, the partitions or their root with
        -- publish_via_partition_root), so ask it, and test the table and
        -- every ancestor of it against that list.
        IF EXISTS (SELECT 1
                     FROM unnest(cols) AS col(relid),
                          pg_catalog.pg_publication p,
                          LATERAL pg_catalog.pg_get_publication_tables(p.pubname) pt
                    WHERE pt.relid = col.relid
                       OR pt.relid IN (SELECT an.relid
                                         FROM pg_catalog.pg_partition_ancestors(col.relid) an)) THEN
            RAISE EXCEPTION 'columnar tables are not replicated logically yet: a publication would include a columnar table'
                USING ERRCODE = '0A000',
                      HINT = 'Keep tables that are published in row storage, or leave the columnar table out of the publication.';
        END IF;
    END IF;

    IF cardinality(pubs) > 0 THEN
        -- The relations listed are the ones published under their own name
        -- (partitions) or, with publish_via_partition_root, the roots of
        -- partitioned tables: the rows that are published are those of the
        -- listed relation and of everything below it.
        IF EXISTS (SELECT 1
                     FROM pg_catalog.pg_publication p,
                          LATERAL pg_catalog.pg_get_publication_tables(p.pubname) pt,
                          LATERAL (SELECT pt.relid
                                   UNION ALL
                                   SELECT t.relid
                                     FROM pg_catalog.pg_partition_tree(pt.relid) t) x(relid)
                    WHERE p.oid = ANY (pubs)
                      AND EXISTS (SELECT 1
                                    FROM pg_catalog.pg_class c
                                   WHERE c.oid = x.relid
                                     AND kingres.columnar_is_columnar_am(c.relam))) THEN
            RAISE EXCEPTION 'columnar tables are not replicated logically yet: the publication would include a columnar table'
                USING ERRCODE = '0A000',
                      HINT = 'Leave columnar tables out of publications.';
        END IF;
    END IF;

    -- A rewriting ALTER TABLE (a type change, a change of access method in
    -- either direction) replaces the table's file and leaves the stripe rows
    -- of the old one, which nothing can read any more.  The access method
    -- notes the old file itself (when the transient table that holds it is
    -- dropped) and removes its rows at the latest at commit; this removes them
    -- with the command, so that columnar_check has nothing to report until the
    -- next VACUUM.  Only the files the command replaced are looked at, so the
    -- cost does not depend on how many columnar tables exist.
    IF TG_TAG = 'ALTER TABLE' THEN
        PERFORM columnar_finish_rewrites();
    END IF;
END;
$$;

CREATE EVENT TRIGGER kingres_columnar_on_drop ON sql_drop
    EXECUTE FUNCTION columnar_on_drop();
CREATE EVENT TRIGGER kingres_columnar_check_ddl ON ddl_command_end
    WHEN TAG IN ('CREATE TABLE', 'ALTER TABLE', 'CREATE TRIGGER', 'CREATE SCHEMA',
                 'CREATE TABLE AS', 'SELECT INTO', 'CREATE MATERIALIZED VIEW',
                 'CREATE PUBLICATION', 'ALTER PUBLICATION')
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
REVOKE ALL ON FUNCTION columnar_check_indexes(regclass) FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_check_clean() FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_reconcile() FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_write_paths(boolean) FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_finish_rewrites() FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_is_columnar_am(oid) FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_on_drop() FROM PUBLIC;
REVOKE ALL ON FUNCTION columnar_check_ddl() FROM PUBLIC;
-- The functions that act on one table check ownership themselves.
GRANT EXECUTE ON FUNCTION columnar_identity(regclass) TO PUBLIC;
GRANT EXECUTE ON FUNCTION columnar_creating_xid(regclass) TO PUBLIC;
GRANT EXECUTE ON FUNCTION columnar_table_xid(regclass) TO PUBLIC;
GRANT EXECUTE ON FUNCTION columnar_options(regclass), columnar_info(regclass),
    columnar_stripes(regclass), columnar_set_options(regclass, text, int, int, int),
    columnar_compact(regclass, int) TO PUBLIC;

GRANT USAGE ON SCHEMA kingres TO PUBLIC;
