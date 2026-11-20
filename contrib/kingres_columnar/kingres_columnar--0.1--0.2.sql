-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_columnar 0.1 to 0.2: the stripe catalog gains the state "reserved"
-- (increment C6a).  A stripe row that is reserved is a claim on row numbers that a
-- writer made when its first row went into a buffer, and that the writer replaces
-- with the stripe's row when it writes the stripe.  Every row of a 0.1 catalog is
-- a written stripe, which is what the default says, so nothing is lost and no
-- row is rewritten (a column with a constant default is not stored in the old
-- rows).  Tables written by 0.1 stay readable and writable: their files are
-- format version 2, which this version reads as it reads version 3 (a table
-- reserves row numbers only if its file is of version 3, see the README).

\echo Use "ALTER EXTENSION kingres_columnar UPDATE TO '0.2'" to load this file. \quit

ALTER TABLE columnar_stripe ADD COLUMN reserved boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN columnar_stripe.reserved IS
    'A claim on row numbers, not yet a stripe: a writer that reserves row numbers inserts the row when its first row goes in and replaces it with the stripe row when it writes the stripe. Readers skip it (version 0.2).';

-- The cross-check of the indexes of one columnar table against its stripes (increment C6c).
CREATE FUNCTION columnar_check_indexes(rel regclass)
    RETURNS TABLE (check_name text, relation regclass, detail text)
    AS 'MODULE_PATHNAME', 'kingres_columnar_check_indexes'
    LANGUAGE C STRICT SECURITY INVOKER
    SET search_path = pg_catalog, kingres, pg_temp;
REVOKE ALL ON FUNCTION columnar_check_indexes(regclass) FROM PUBLIC;

-- Foreign keys TO a columnar table are refused too (increment C6c: a columnar table can have a
-- primary key now, and a key that references it needs a row lock on the referenced row).
CREATE OR REPLACE FUNCTION columnar_check_ddl()
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
        -- Columnar tables can be published (0.2, C7: the rows are decoded from the
        -- flush of a stripe).  What must never be published is the stripe catalog,
        -- whose rows describe files of this server; a publication of all tables or
        -- of the schema kingres would include it (pg_get_publication_tables says
        -- what a publication includes: all tables, a schema, the listed tables, the
        -- exceptions of 19), and a subscriber would apply its rows.
        IF EXISTS (SELECT 1
                     FROM pg_catalog.pg_publication p,
                          LATERAL pg_catalog.pg_get_publication_tables(p.pubname) pt
                    WHERE pt.relid = 'kingres.columnar_stripe'::pg_catalog.regclass) THEN
            RAISE EXCEPTION 'a publication would include the stripe catalog of kingres_columnar, kingres.columnar_stripe'
                USING ERRCODE = '0A000',
                      HINT = 'List the tables to publish (CREATE PUBLICATION ... FOR TABLE ...) instead of publishing all tables or the schema kingres.';
        END IF;
    END IF;

    IF cardinality(pubs) > 0 THEN
        IF EXISTS (SELECT 1
                     FROM pg_catalog.pg_publication p,
                          LATERAL pg_catalog.pg_get_publication_tables(p.pubname) pt
                    WHERE p.oid = ANY (pubs)
                      AND pt.relid = 'kingres.columnar_stripe'::pg_catalog.regclass)
           AND EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                        WHERE c.relkind IN ('r', 'm')
                          AND kingres.columnar_is_columnar_am(c.relam)) THEN
            RAISE EXCEPTION 'a publication would include the stripe catalog of kingres_columnar, kingres.columnar_stripe'
                USING ERRCODE = '0A000',
                      HINT = 'List the tables to publish (CREATE PUBLICATION ... FOR TABLE ...) instead of publishing all tables or the schema kingres.';
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
