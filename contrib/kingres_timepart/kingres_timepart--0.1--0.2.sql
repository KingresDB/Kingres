-- kingres_timepart--0.1--0.2.sql
--
-- Upgrade of kingres_timepart from 0.1 to 0.2: conversion of partitions to
-- columnar storage and back (increment T2 of the design, section 10.4).
-- convert_partition and convert_old_partitions, stubs in 0.1, are
-- implemented, and the convert phase of a run executes the conversion.  The
-- tables and views are unchanged.  Everything here is CREATE OR REPLACE of a
-- function whose signature is the same as in 0.1 (its result and its
-- privileges stay), or a new function; the result is the catalog that
-- kingres_timepart--0.2.sql makes (the regression test "upgrade" compares
-- the two).
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only

\echo Use "ALTER EXTENSION kingres_timepart UPDATE TO '0.2'" to load this file. \quit


-- New functions

-- The options of a policy or of a call, as the arguments of
-- kingres.columnar_set_options, checked.  Null gives four nulls (the table
-- keeps the settings of the session that makes it).  The keys are the
-- arguments of columnar_set_options; the ranges are those of the settings
-- kingres_columnar.* (columnar_set_options itself checks again).
CREATE FUNCTION kingres.timepart__columnar_options(
    p_opts jsonb,
    OUT o_compression text, OUT o_level integer, OUT o_stripe integer,
    OUT o_chunk integer)
LANGUAGE plpgsql IMMUTABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_key  text;
    v_val  jsonb;
    v_num  numeric;
    v_lo   numeric;
    v_hi   numeric;
BEGIN
    IF p_opts IS NULL THEN
        RETURN;
    END IF;
    IF jsonb_typeof(p_opts) <> 'object' THEN
        RAISE EXCEPTION 'columnar_options must be a JSON object'
            USING ERRCODE = '22023';
    END IF;
    FOR v_key, v_val IN SELECT e.key, e.value FROM jsonb_each(p_opts) e
    LOOP
        IF v_key = 'compression' THEN
            IF jsonb_typeof(v_val) <> 'string'
               OR (v_val #>> '{}') NOT IN ('none', 'pglz', 'lz4', 'zstd', 'auto') THEN
                RAISE EXCEPTION 'columnar_options: compression must be one of none, pglz, lz4, zstd, auto'
                    USING ERRCODE = '22023';
            END IF;
            o_compression := v_val #>> '{}';
        ELSIF v_key IN ('compression_level', 'stripe_row_count', 'chunk_row_count') THEN
            IF jsonb_typeof(v_val) <> 'number' THEN
                RAISE EXCEPTION 'columnar_options: % must be a number', v_key
                    USING ERRCODE = '22023';
            END IF;
            v_num := (v_val #>> '{}')::numeric;
            v_lo := CASE v_key WHEN 'compression_level' THEN 1
                               WHEN 'stripe_row_count' THEN 1000 ELSE 100 END;
            v_hi := CASE v_key WHEN 'compression_level' THEN 19
                               WHEN 'stripe_row_count' THEN 10000000 ELSE 100000 END;
            IF v_num <> trunc(v_num) OR v_num < v_lo OR v_num > v_hi THEN
                RAISE EXCEPTION 'columnar_options: % is out of range', v_key
                    USING ERRCODE = '22023',
                          HINT = 'compression_level 1 to 19, stripe_row_count 1000 to 10000000, chunk_row_count 100 to 100000, all whole numbers.';
            END IF;
            IF v_key = 'compression_level' THEN
                o_level := v_num::integer;
            ELSIF v_key = 'stripe_row_count' THEN
                o_stripe := v_num::integer;
            ELSE
                o_chunk := v_num::integer;
            END IF;
        ELSE
            RAISE EXCEPTION 'columnar_options: unknown option "%"', v_key
                USING ERRCODE = '22023',
                      HINT = 'The options are compression, compression_level, stripe_row_count and chunk_row_count.';
        END IF;
    END LOOP;
END;
$$;

-- What kind of table access method a name is: 'heap', 'columnar' (by its
-- handler, as kingres_columnar itself decides, never by the name), 'other', or
-- null when there is no table access method of that name.
CREATE FUNCTION kingres.timepart__am_kind(p_am name)
RETURNS text
LANGUAGE sql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
    SELECT CASE
             WHEN a.amhandler = 'pg_catalog.heap_tableam_handler'::pg_catalog.regproc
                  THEN 'heap'
             WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_proc p
                           WHERE p.oid = a.amhandler
                             AND p.prosrc = 'kingres_columnar_handler'
                             AND p.probin LIKE '%kingres_columnar')
                  THEN 'columnar'
             ELSE 'other' END
      FROM pg_catalog.pg_am a
     WHERE a.amname = p_am AND a.amtype = 't'
$$;

-- Is the table, or its partition, one that another extension owns: a ledger
-- table (kingres_ledger) or a distributed table (kingres_shard)?  SECURITY
-- DEFINER because the catalogs of those extensions are private to them; it
-- decides nothing about the caller and returns a word, or null.
CREATE FUNCTION kingres.timepart__special_table(p_parent oid, p_part oid)
RETURNS text
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_hit boolean;
BEGIN
    IF pg_catalog.to_regclass('kingres.ledger_table') IS NOT NULL THEN
        EXECUTE 'SELECT EXISTS (SELECT 1 FROM kingres.ledger_table t WHERE t.relid::pg_catalog.oid IN ($1, $2))'
            INTO v_hit USING p_parent, p_part;
        IF v_hit THEN
            RETURN 'ledger';
        END IF;
    END IF;
    IF pg_catalog.to_regclass('kingres.dist_table') IS NOT NULL THEN
        EXECUTE 'SELECT EXISTS (SELECT 1 FROM kingres.dist_table t WHERE t.relid::pg_catalog.oid IN ($1, $2))'
            INTO v_hit USING p_parent, p_part;
        IF v_hit THEN
            RETURN 'distributed';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;

-- The reason a partition cannot be converted to the access method p_target, or
-- null when it can (10.4 step 1).  Reads the catalogs only and changes
-- nothing; the first reason found is returned, as a sentence that names the
-- object.  The conversion copies the rows and drops the old partition, so
-- everything that would be lost with it, or that the new table cannot carry,
-- is a reason: the user's objects are never dropped silently.
CREATE FUNCTION kingres.timepart__convert_refusal(
    p_parent oid, p_part oid, p_target name, p_sort text[], p_options jsonb)
RETURNS text
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
SET DateStyle = 'ISO, MDY'
AS $$
DECLARE
    v_kind   text;
    v_cur    text;
    v_c      record;
    v_powner oid;
    v_pn     text := p_part::pg_catalog.regclass::text;
    v_parn   text := p_parent::pg_catalog.regclass::text;
    v_x      text;
    v_s      text;
BEGIN
    v_kind := kingres.timepart__am_kind(p_target);
    IF v_kind IS NULL THEN
        RETURN format('"%s" is not a table access method', p_target);
    END IF;
    IF v_kind = 'other' THEN
        RETURN format('access method "%s" is not supported: convert to columnar or to heap', p_target);
    END IF;

    SELECT c.relkind, c.relpersistence, c.relowner, c.relhassubclass,
           c.relrowsecurity, c.relnamespace, a.amname,
           pg_get_expr(c.relpartbound, c.oid) AS bound
      INTO v_c
      FROM pg_inherits i
      JOIN pg_class c ON c.oid = i.inhrelid
      LEFT JOIN pg_am a ON a.oid = c.relam
     WHERE i.inhparent = p_parent AND i.inhrelid = p_part;
    IF NOT FOUND THEN
        RETURN format('%s is not a partition of %s', v_pn, v_parn);
    END IF;
    IF v_c.relkind = 'p' THEN
        RETURN format('%s is a partitioned table: only leaf partitions are converted', v_pn);
    END IF;
    IF v_c.relkind <> 'r' THEN
        RETURN format('%s is not an ordinary table', v_pn);
    END IF;
    IF v_c.bound = 'DEFAULT' THEN
        RETURN format('%s is the default partition', v_pn);
    END IF;
    v_cur := kingres.timepart__am_kind(v_c.amname);
    IF v_cur = v_kind THEN
        RETURN format('%s is already stored with the access method "%s"', v_pn, v_c.amname);
    END IF;
    IF v_c.relpersistence <> 'p' THEN
        RETURN format('%s is a temporary or unlogged table', v_pn);
    END IF;
    -- The copy reads the partition after it has locked it; under REPEATABLE
    -- READ or SERIALIZABLE the snapshot of the transaction can be older than
    -- the lock, and a row committed in between would be left behind and
    -- dropped with the old partition.
    IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN
        RETURN format('a conversion needs the transaction isolation level read committed, not %s',
                      pg_catalog.current_setting('transaction_isolation'));
    END IF;
    SELECT c.relowner INTO v_powner FROM pg_class c WHERE c.oid = p_parent;
    IF v_c.relowner <> v_powner THEN
        RETURN format('%s is owned by "%s", the table by "%s"; the owner of a partition must be the owner of the table',
                      v_pn, pg_get_userbyid(v_c.relowner), pg_get_userbyid(v_powner));
    END IF;
    v_x := kingres.timepart__special_table(p_parent, p_part);
    IF v_x = 'ledger' THEN
        RETURN format('%s belongs to a ledger table', v_pn);
    ELSIF v_x = 'distributed' THEN
        RETURN format('%s belongs to a distributed table, which this version does not convert', v_pn);
    END IF;

    -- What would be lost with the old partition.
    SELECT t.tgname INTO v_x FROM pg_trigger t
     WHERE t.tgrelid = p_part AND NOT t.tgisinternal AND t.tgparentid = 0
     ORDER BY t.tgname LIMIT 1;
    IF v_x IS NOT NULL THEN
        RETURN format('%s has the trigger "%s" of its own, which the copy would not carry', v_pn, v_x);
    END IF;
    SELECT r.rulename INTO v_x FROM pg_rewrite r
     WHERE r.ev_class = p_part AND r.rulename <> '_RETURN'
     ORDER BY r.rulename LIMIT 1;
    IF v_x IS NOT NULL THEN
        RETURN format('%s has the rule "%s"', v_pn, v_x);
    END IF;
    IF v_c.relrowsecurity OR EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = p_part) THEN
        RETURN format('%s has row-level security', v_pn);
    END IF;
    SELECT pb.pubname INTO v_x FROM pg_publication_rel r
      JOIN pg_publication pb ON pb.oid = r.prpubid
     WHERE r.prrelid = p_part ORDER BY pb.pubname LIMIT 1;
    IF v_x IS NOT NULL THEN
        RETURN format('%s is in the publication "%s"', v_pn, v_x);
    END IF;
    SELECT string_agg(DISTINCT format('%I.%I', n.nspname, vc.relname), ', ') INTO v_x
      FROM pg_depend d
      JOIN pg_rewrite rw ON rw.oid = d.objid
      JOIN pg_class vc ON vc.oid = rw.ev_class
      JOIN pg_namespace n ON n.oid = vc.relnamespace
     WHERE d.classid = 'pg_rewrite'::pg_catalog.regclass
       AND d.refclassid = 'pg_class'::pg_catalog.regclass
       AND d.refobjid = p_part AND vc.oid <> p_part;
    IF v_x IS NOT NULL THEN
        RETURN format('%s is used by the view or rule of %s', v_pn, v_x);
    END IF;
    SELECT format('%I on %s', k.conname, k.conrelid::pg_catalog.regclass::text) INTO v_x
      FROM pg_constraint k
     WHERE k.contype = 'f' AND k.confrelid = p_part AND k.conparentid = 0
     ORDER BY k.conname LIMIT 1;
    IF v_x IS NOT NULL THEN
        RETURN format('%s is referenced by the foreign key %s', v_pn, v_x);
    END IF;
    SELECT format('%I', s.relname) INTO v_x
      FROM pg_depend d JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
     WHERE d.classid = 'pg_class'::pg_catalog.regclass AND d.refclassid = 'pg_class'::pg_catalog.regclass
       AND d.refobjid = p_part AND d.deptype = 'a'
     ORDER BY s.relname LIMIT 1;
    IF v_x IS NOT NULL THEN
        RETURN format('%s owns the sequence %s, which the drop of the old partition would remove', v_pn, v_x);
    END IF;
    SELECT format('%I', e.stxname) INTO v_x FROM pg_statistic_ext e
     WHERE e.stxrelid = p_part ORDER BY e.stxname LIMIT 1;
    IF v_x IS NOT NULL THEN
        RETURN format('%s has the extended statistics object %s', v_pn, v_x);
    END IF;
    SELECT a.attname INTO v_x FROM pg_attribute a
     WHERE a.attrelid = p_part AND a.attnum > 0 AND NOT a.attisdropped
       AND a.attidentity <> ''
     ORDER BY a.attnum LIMIT 1;
    IF v_x IS NOT NULL THEN
        RETURN format('%s has the identity column "%s"', v_pn, v_x);
    END IF;

    IF v_kind = 'columnar' THEN
        -- What a columnar table cannot have (kingres_columnar README, "What
        -- is refused"), found before the attach would fail on it.
        SELECT format('%s', i.indexrelid::pg_catalog.regclass::text) INTO v_x
          FROM pg_index i WHERE i.indrelid = p_parent
         ORDER BY i.indexrelid LIMIT 1;
        IF v_x IS NOT NULL THEN
            RETURN format('%s has the index %s, and the columnar partition would need a matching index: columnar tables have no indexes in this version',
                          v_parn, v_x);
        END IF;
        SELECT format('%s', i.indexrelid::pg_catalog.regclass::text) INTO v_x
          FROM pg_index i WHERE i.indrelid = p_part
         ORDER BY i.indexrelid LIMIT 1;
        IF v_x IS NOT NULL THEN
            RETURN format('%s has the index %s: columnar tables have no indexes in this version', v_pn, v_x);
        END IF;
        SELECT format('%I on %s', k.conname, k.conrelid::pg_catalog.regclass::text) INTO v_x
          FROM pg_constraint k
         WHERE k.contype = 'f'
           AND (k.conrelid IN (p_part, p_parent) OR k.confrelid IN (p_part, p_parent))
         ORDER BY k.conparentid <> 0, k.conname LIMIT 1;
        IF v_x IS NOT NULL THEN
            RETURN format('the foreign key %s concerns %s or %s: columnar tables have no foreign keys', v_x, v_pn, v_parn);
        END IF;
        SELECT format('"%s" on %s', t.tgname, t.tgrelid::pg_catalog.regclass::text) INTO v_x
          FROM pg_trigger t
         WHERE t.tgrelid IN (p_part, p_parent) AND NOT t.tgisinternal
           AND (t.tgtype & 1) <> 0 AND (t.tgtype & 2) = 0 AND (t.tgtype & 64) = 0
         ORDER BY t.tgparentid <> 0, t.tgname LIMIT 1;
        IF v_x IS NOT NULL THEN
            RETURN format('the AFTER ROW trigger %s cannot be on a columnar table', v_x);
        END IF;
        SELECT pb.pubname INTO v_x FROM pg_publication pb
         WHERE pb.puballtables
            OR EXISTS (SELECT 1 FROM pg_publication_rel r
                        WHERE r.prpubid = pb.oid AND r.prrelid = p_parent)
            OR EXISTS (SELECT 1 FROM pg_publication_namespace pn
                        WHERE pn.pnpubid = pb.oid AND pn.pnnspid = v_c.relnamespace)
         ORDER BY pb.pubname LIMIT 1;
        IF v_x IS NOT NULL THEN
            RETURN format('the publication "%s" would include the columnar partition: columnar tables are not replicated logically yet', v_x);
        END IF;
    END IF;

    -- What the copy reads and sorts by.
    IF NOT EXISTS (SELECT 1 FROM pg_attribute a
                    WHERE a.attrelid = p_part AND a.attnum > 0
                      AND NOT a.attisdropped AND a.attgenerated = '') THEN
        RETURN format('%s has no column that can be copied', v_pn);
    END IF;
    IF p_sort IS NOT NULL THEN
        IF cardinality(p_sort) = 0 THEN
            RETURN 'sort_by must name at least one column';
        END IF;
        FOREACH v_s IN ARRAY p_sort LOOP
            IF v_s IS NULL OR NOT EXISTS (
                   SELECT 1 FROM pg_attribute a
                    WHERE a.attrelid = p_part AND a.attname = v_s
                      AND a.attnum > 0 AND NOT a.attisdropped) THEN
                RETURN format('the sort column "%s" is not a column of %s', coalesce(v_s, 'NULL'), v_pn);
            END IF;
        END LOOP;
    END IF;
    IF p_options IS NOT NULL THEN
        IF v_kind <> 'columnar' THEN
            RETURN 'options apply to columnar tables only';
        END IF;
        BEGIN
            PERFORM kingres.timepart__columnar_options(p_options);
        EXCEPTION WHEN invalid_parameter_value THEN
            RETURN SQLERRM;
        END;
    END IF;
    RETURN NULL;
END;
$$;

-- The statements that replace one partition by a copy in another access method
-- (10.4, "Convert one partition"): after the checks, one DO statement that
-- the caller executes as the owner of the table (timepart__exec_direct or
-- timepart__exec_as), so that the whole conversion is one transaction of the
-- caller and the owner's privileges apply to all of it.  The names are
-- quoted here, once, and handed to the block as literals; the block holds
-- no name of its own.  What it does, in order:
--
--   1. SHARE ROW EXCLUSIVE on the partition: its writers wait, its readers go
--      on (the design says SHARE; this mode also keeps out a plain CREATE
--      INDEX, which SHARE lets in, and a second conversion).
--   2. A new table next to it (LIKE the partition, with the check of the
--      bound in its definition so that the attach below scans nothing), in
--      the new access method, with the options, comment, tablespace and
--      replica identity of the old one.
--   3. The copy: the columns named one by one, ordered by sort_by (the
--      partition column by default for columnar).
--   4. The check: the two tables have the same number of rows, or the block
--      fails with verification_failed and nothing has happened.
--   5. The privileges of the old partition on the new one.
--   6. The swap: no wait at all if a session that holds the parent waits for
--      the partition (a cycle); else a short wait (the smaller of 500 ms and
--      half of deadlock_timeout) for ACCESS EXCLUSIVE on the parent, then detach,
--      attach, drop, rename and drop of the helper check.  If the lock is not
--      granted in time the block fails with swap_lock_timeout and the copy is
--      rolled back with everything else.
CREATE FUNCTION kingres.timepart__convert_stmts(
    p_parent oid, p_part oid, p_target name, p_sort text[], p_options jsonb)
RETURNS text[]
LANGUAGE plpgsql
SET search_path = pg_catalog, kingres, pg_temp
SET DateStyle = 'ISO, MDY'
AS $$
DECLARE
    v_info    record;
    v_reason  text;
    v_kind    text;
    v_c       record;
    v_pname   name;
    v_base    text;
    v_tmp     text;
    v_schema  name;
    v_m       text[];
    v_lit     text;
    v_parts   text[] := '{}';
    v_side    text[] := ARRAY['', ''];
    v_bound   text;
    v_bound_cl text := '';
    v_check   text := 'timepart_convert_bound';
    v_cols    text;
    v_order   text := '';
    v_tblspc  text := '';
    v_opts    record;
    v_swap    integer;
    v_dl      integer;
    v_body    text;
    i         integer;
BEGIN
    SELECT * INTO v_info FROM kingres.timepart__key_info(p_parent);
    PERFORM kingres.timepart__require_owner(
        current_user, v_info.r_owner_oid,
        format('%I.%I', v_info.r_schema, v_info.r_table));
    v_reason := kingres.timepart__convert_refusal(
        p_parent, p_part, p_target, p_sort, p_options);
    IF v_reason IS NOT NULL THEN
        RAISE EXCEPTION 'conversion_refused: %', v_reason
            USING ERRCODE = '0A000';
    END IF;
    v_kind := kingres.timepart__am_kind(p_target);

    SELECT n.nspname, c.relname, pg_get_expr(c.relpartbound, c.oid) AS bound,
           c.reltablespace INTO v_c
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = p_part;
    v_schema := v_c.nspname;
    v_pname := v_c.relname;

    -- The name of the new table: the old name and "__kc", cut to fit.
    v_base := v_pname;
    WHILE octet_length(v_base) > 59 LOOP
        v_base := left(v_base, char_length(v_base) - 1);
    END LOOP;
    v_tmp := v_base || '__kc';
    IF kingres.timepart__oid(v_schema, v_tmp::name) IS NOT NULL THEN
        RAISE EXCEPTION 'name_conflict: relation %.% already exists and is not the expected partition of %.%',
            quote_ident(v_schema), quote_ident(v_tmp),
            quote_ident(v_info.r_schema), quote_ident(v_info.r_table)
            USING ERRCODE = '42P07';
    END IF;

    -- The bound of the partition, as literals; the check of the new table is
    -- made from them.  (MINVALUE and MAXVALUE leave their side unchecked.)
    v_m := regexp_match(v_c.bound, '^FOR VALUES FROM \((.*)\) TO \((.*)\)$');
    IF v_m IS NULL THEN
        RAISE EXCEPTION 'conversion_refused: cannot read the bound of %: %',
            p_part::pg_catalog.regclass::text, v_c.bound USING ERRCODE = '0A000';
    END IF;
    FOR i IN 1..2 LOOP
        IF v_m[i] IN ('MINVALUE', 'MAXVALUE') THEN
            v_side[i] := v_m[i];
        ELSE
            v_lit := (regexp_match(v_m[i], '^''([^'']*)''$'))[1];
            IF v_lit IS NULL THEN
                RAISE EXCEPTION 'conversion_refused: cannot read the bound of %: %',
                    p_part::pg_catalog.regclass::text, v_c.bound USING ERRCODE = '0A000';
            END IF;
            v_side[i] := quote_literal(v_lit);
            v_parts := v_parts || CASE WHEN i = 1
                THEN format('%I >= %s', v_info.r_keycol, v_side[i])
                ELSE format('%I < %s', v_info.r_keycol, v_side[i]) END;
        END IF;
    END LOOP;
    v_bound := format('FOR VALUES FROM (%s) TO (%s)', v_side[1], v_side[2]);
    IF cardinality(v_parts) > 0 THEN
        v_bound_cl := format(', CONSTRAINT %I CHECK (%s)', v_check,
                             array_to_string(v_parts, ' AND '));
    END IF;

    SELECT string_agg(quote_ident(a.attname), ', ' ORDER BY a.attnum) INTO v_cols
      FROM pg_attribute a
     WHERE a.attrelid = p_part AND a.attnum > 0 AND NOT a.attisdropped
       AND a.attgenerated = '';
    IF p_sort IS NOT NULL THEN
        SELECT ' ORDER BY ' || string_agg(quote_ident(s.x), ', ' ORDER BY s.n) INTO v_order
          FROM unnest(p_sort) WITH ORDINALITY s(x, n);
    ELSIF v_kind = 'columnar' THEN
        v_order := ' ORDER BY ' || quote_ident(v_info.r_keycol);
    END IF;
    IF v_c.reltablespace <> 0 THEN
        SELECT ' TABLESPACE ' || quote_ident(t.spcname) INTO v_tblspc
          FROM pg_tablespace t WHERE t.oid = v_c.reltablespace;
    END IF;
    IF v_kind = 'columnar' THEN
        SELECT * INTO v_opts FROM kingres.timepart__columnar_options(p_options);
    ELSE
        SELECT NULL::text AS o_compression, NULL::integer AS o_level,
               NULL::integer AS o_stripe, NULL::integer AS o_chunk INTO v_opts;
    END IF;
    -- A writer of the partition holds the parent in ROW EXCLUSIVE while it
    -- waits for our lock on the partition, and we wait for the parent: a
    -- cycle that only a timeout breaks.  The wait is below the deadlock
    -- timeout, so the conversion gives way, not the user's statement.
    SELECT s.setting::integer INTO v_dl FROM pg_settings s WHERE s.name = 'deadlock_timeout';
    v_swap := greatest(1, least(500, coalesce(v_dl, 1000) / 2));

    v_body := format($tpl$
DECLARE
    v_parent   text := %1$L;
    v_part     text := %2$L;
    v_new      text := %3$L;
    v_schema   text := %4$L;
    v_old_name text := %5$L;
    v_am       text := %6$L;
    v_cols     text := %7$L;
    v_order    text := %8$L;
    v_bound_cl text := %9$L;
    v_bound    text := %10$L;
    v_check    text := %11$L;
    v_tblspc   text := %12$L;
    v_o_comp   text := %13$L;
    v_o_level  integer := %14$L;
    v_o_stripe integer := %15$L;
    v_o_chunk  integer := %16$L;
    v_swap     integer := %17$s;
    v_columnar boolean := %18$s;
    v_part_oid oid := %19$s;
    v_parent_oid oid := %20$s;
    v_prev_lt  text := pg_catalog.current_setting('lock_timeout');
    v_owner    oid;
    v_old_cnt  bigint;
    v_new_cnt  bigint;
    v_text     text;
    v_ident    "char";
    r          record;
BEGIN
    EXECUTE 'LOCK TABLE ' || v_part || ' IN SHARE ROW EXCLUSIVE MODE';
    EXECUTE 'CREATE TABLE ' || v_new || ' (LIKE ' || v_part
         || ' INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING COMMENTS'
         || ' INCLUDING STORAGE INCLUDING GENERATED INCLUDING COMPRESSION'
         || v_bound_cl || ') USING ' || pg_catalog.quote_ident(v_am) || v_tblspc;
    IF v_columnar AND (v_o_comp IS NOT NULL OR v_o_level IS NOT NULL
                       OR v_o_stripe IS NOT NULL OR v_o_chunk IS NOT NULL) THEN
        PERFORM kingres.columnar_set_options(v_new::pg_catalog.regclass, v_o_comp,
                                             v_o_level, v_o_stripe, v_o_chunk);
    END IF;
    SELECT d.description INTO v_text FROM pg_catalog.pg_description d
     WHERE d.classoid = 'pg_catalog.pg_class'::pg_catalog.regclass
       AND d.objoid = v_part_oid AND d.objsubid = 0;
    IF FOUND THEN
        EXECUTE 'COMMENT ON TABLE ' || v_new || ' IS ' || pg_catalog.quote_literal(v_text);
    END IF;
    SELECT c.relreplident, c.relowner INTO v_ident, v_owner
      FROM pg_catalog.pg_class c WHERE c.oid = v_part_oid;
    IF v_ident = 'f' THEN
        EXECUTE 'ALTER TABLE ' || v_new || ' REPLICA IDENTITY FULL';
    ELSIF v_ident = 'n' THEN
        EXECUTE 'ALTER TABLE ' || v_new || ' REPLICA IDENTITY NOTHING';
    END IF;

    EXECUTE 'INSERT INTO ' || v_new || ' (' || v_cols || ') SELECT ' || v_cols
         || ' FROM ' || v_part || v_order;

    EXECUTE 'SELECT pg_catalog.count(*) FROM ' || v_part INTO v_old_cnt;
    EXECUTE 'SELECT pg_catalog.count(*) FROM ' || v_new INTO v_new_cnt;
    IF v_old_cnt IS DISTINCT FROM v_new_cnt THEN
        RAISE EXCEPTION 'verification_failed: %% has %% rows, its copy has %%',
            v_part, v_old_cnt, v_new_cnt USING ERRCODE = 'KT001';
    END IF;

    -- the privileges of the old partition, table and column level
    FOR r IN SELECT DISTINCT x.grantee
               FROM pg_catalog.pg_class c, LATERAL pg_catalog.aclexplode(c.relacl) x
              WHERE c.oid = v_new::pg_catalog.regclass AND x.grantee <> v_owner
    LOOP
        EXECUTE 'REVOKE ALL ON TABLE ' || v_new || ' FROM '
             || CASE WHEN r.grantee = 0 THEN 'PUBLIC'
                     ELSE pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(r.grantee)) END;
    END LOOP;
    FOR r IN SELECT x.grantee, x.privilege_type, x.is_grantable
               FROM pg_catalog.pg_class c,
                    LATERAL pg_catalog.aclexplode(coalesce(c.relacl, pg_catalog.acldefault('r', c.relowner))) x
              WHERE c.oid = v_part_oid AND x.grantee <> c.relowner
    LOOP
        EXECUTE 'GRANT ' || r.privilege_type || ' ON TABLE ' || v_new || ' TO '
             || CASE WHEN r.grantee = 0 THEN 'PUBLIC'
                     ELSE pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(r.grantee)) END
             || CASE WHEN r.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END;
    END LOOP;
    FOR r IN SELECT a.attname, x.grantee, x.privilege_type, x.is_grantable
               FROM pg_catalog.pg_attribute a, LATERAL pg_catalog.aclexplode(a.attacl) x
              WHERE a.attrelid = v_part_oid AND a.attnum > 0 AND NOT a.attisdropped
                AND x.grantee <> v_owner
    LOOP
        EXECUTE 'GRANT ' || r.privilege_type || ' (' || pg_catalog.quote_ident(r.attname)
             || ') ON TABLE ' || v_new || ' TO '
             || CASE WHEN r.grantee = 0 THEN 'PUBLIC'
                     ELSE pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(r.grantee)) END
             || CASE WHEN r.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END;
    END LOOP;

    -- the swap.  A session that holds the parent (a writer through it, a
    -- reader inside a transaction) and waits for a lock on the partition,
    -- which we hold, is a cycle with the lock we are about to ask for.  It would
    -- be broken by our timeout below, or, if the session has waited longer than
    -- deadlock_timeout, by the deadlock detector running in that session,
    -- which would then be the one that fails.  So do not ask: the conversion
    -- gives way at once.
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_locks w JOIN pg_catalog.pg_locks h ON h.pid = w.pid
                WHERE w.locktype = 'relation' AND w.database = (SELECT d.oid FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database())
                  AND w.relation = v_part_oid AND NOT w.granted
                  AND h.locktype = 'relation' AND h.database = w.database
                  AND h.relation = v_parent_oid AND h.granted) THEN
        RAISE EXCEPTION 'swap_lock_timeout: a session that holds %% waits for the partition %%; the copy was discarded and the next run tries again',
            v_parent, v_part USING ERRCODE = '55P03';
    END IF;
    PERFORM pg_catalog.set_config('lock_timeout', v_swap::pg_catalog.text || 'ms', true);
    BEGIN
        EXECUTE 'LOCK TABLE ' || v_parent || ' IN ACCESS EXCLUSIVE MODE';
    EXCEPTION WHEN lock_not_available THEN
        RAISE EXCEPTION 'swap_lock_timeout: %% could not be locked within %% ms; the copy was discarded and the next run tries again',
            v_parent, v_swap USING ERRCODE = '55P03';
    END;
    EXECUTE 'ALTER TABLE ' || v_parent || ' DETACH PARTITION ' || v_part;
    EXECUTE 'ALTER TABLE ' || v_parent || ' ATTACH PARTITION ' || v_new || ' ' || v_bound;
    EXECUTE 'DROP TABLE ' || v_part;
    EXECUTE 'ALTER TABLE ' || v_new || ' RENAME TO ' || pg_catalog.quote_ident(v_old_name);
    IF v_bound_cl <> '' THEN
        EXECUTE 'ALTER TABLE ' || pg_catalog.quote_ident(v_schema) || '.'
             || pg_catalog.quote_ident(v_old_name)
             || ' DROP CONSTRAINT ' || pg_catalog.quote_ident(v_check);
    END IF;
    PERFORM pg_catalog.set_config('lock_timeout', v_prev_lt, true);
END
$tpl$,
        format('%I.%I', v_info.r_schema, v_info.r_table),
        format('%I.%I', v_schema, v_pname),
        format('%I.%I', v_schema, v_tmp),
        v_schema, v_pname, p_target, v_cols, v_order, v_bound_cl, v_bound,
        v_check, v_tblspc, v_opts.o_compression, v_opts.o_level,
        v_opts.o_stripe, v_opts.o_chunk, v_swap, (v_kind = 'columnar')::text,
        p_part::oid, p_parent::oid);
    RETURN ARRAY[format('DO %L', v_body)];
END;
$$;

-- After the transaction of a conversion has committed: give every conversion
-- of the run that was done and not yet noted its size afterwards, the rows and
-- the time it took, in the history, and return the partitions, so that the
-- caller can analyze them.  Nothing of this can be known inside the
-- conversion's own transaction: the stripes of a columnar table are written
-- when the transaction commits.  SECURITY DEFINER, because the history is
-- written by the extension; the caller is judged as in timepart__step_end.
CREATE FUNCTION kingres.timepart__convert_note(
    p_run bigint, p_caller_oid oid, p_caller_super boolean)
RETURNS TABLE (part_schema name, part_name name)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_run   kingres.timepart_run_data%ROWTYPE;
    v_a     kingres.timepart_run_action_data%ROWTYPE;
    v_oid   oid;
    v_size  bigint;
    v_rows  bigint;
    v_ratio float8;
    v_text  text;
BEGIN
    SELECT r.* INTO v_run FROM kingres.timepart_run_data r
     WHERE r.run_id = p_run FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'run % does not exist', p_run USING ERRCODE = '42704';
    END IF;
    PERFORM kingres.timepart__require_driver(
        CASE WHEN v_run.stamp = kingres.timepart__stamp()
             THEN v_run.table_owner_oid ELSE 0::pg_catalog.oid END,
        p_caller_oid, p_caller_super);
    FOR v_a IN SELECT a.* FROM kingres.timepart_run_action_data a
                WHERE a.run_id = p_run AND a.kind = 'convert'
                  AND a.state = 'done' AND a.bytes_after IS NULL
                ORDER BY a.seq
    LOOP
        v_oid := kingres.timepart__oid(v_a.partition_schema, v_a.partition_name);
        v_size := coalesce(pg_total_relation_size(v_oid), 0);
        v_rows := NULL;
        v_ratio := NULL;
        IF v_oid IS NOT NULL
           AND pg_catalog.to_regprocedure('kingres.columnar_info(regclass)') IS NOT NULL
           AND kingres.timepart__am_kind(
                   (SELECT am.amname FROM pg_class c JOIN pg_am am ON am.oid = c.relam
                     WHERE c.oid = v_oid)) = 'columnar' THEN
            EXECUTE 'SELECT i.rows, i.compression_ratio FROM kingres.columnar_info($1) i'
                INTO v_rows, v_ratio USING v_oid::pg_catalog.regclass;
        END IF;
        v_text := format('%s%s, %s s',
                         CASE WHEN v_rows IS NULL THEN ''
                              ELSE format('%s rows, compression ratio %s, ', v_rows,
                                          coalesce(round(v_ratio::numeric, 2)::text, 'n/a')) END,
                         format('%s bytes before, %s after', v_a.bytes_before, v_size),
                         round(extract(epoch FROM coalesce(v_a.finished_at, now()) - v_a.started_at)::numeric, 1));
        UPDATE kingres.timepart_run_action_data a
           SET bytes_after = v_size,
               detail = coalesce(a.detail || ': ', '') || v_text
         WHERE a.run_id = v_a.run_id AND a.seq = v_a.seq;
        part_schema := v_a.partition_schema;
        part_name := v_a.partition_name;
        RETURN NEXT;
    END LOOP;
END;
$$;

-- What follows the commit of a conversion, in a transaction of its own
-- (10.4 step 8): note the result in the history and ANALYZE the new
-- partitions.  A failure to analyze (a lock that is not granted in time) is
-- a warning: the conversion is complete without statistics, which autovacuum
-- makes later.  SECURITY INVOKER: the ANALYZE is the caller's.
CREATE FUNCTION kingres.timepart__convert_after(p_run bigint)
RETURNS integer
LANGUAGE plpgsql
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_self  oid;
    v_super boolean;
    v_part  record;
    v_n     integer := 0;
BEGIN
    SELECT r.oid, r.rolsuper INTO v_self, v_super
      FROM pg_roles r WHERE r.rolname = current_user;
    PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
    FOR v_part IN SELECT n.part_schema, n.part_name
                    FROM kingres.timepart__convert_note(p_run, v_self, v_super) n
    LOOP
        BEGIN
            EXECUTE format('ANALYZE %I.%I', v_part.part_schema, v_part.part_name);
            v_n := v_n + 1;
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'ANALYZE of %.% was not done: %',
                quote_ident(v_part.part_schema), quote_ident(v_part.part_name), SQLERRM;
        END;
    END LOOP;
    RETURN v_n;
END;
$$;

-- The conversion needs kingres_columnar: the error names it.  (Replaces
-- timepart__no_columnar of 0.1, which was only executable by superusers.)
CREATE FUNCTION kingres.timepart__need_columnar()
RETURNS void
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_extension e
                    WHERE e.extname = 'kingres_columnar') THEN
        RAISE EXCEPTION 'converting a partition needs the extension kingres_columnar, which is not installed'
            USING ERRCODE = '0A000',
                  HINT = 'Install kingres_columnar in this database; the run skips the conversion step until then.';
    END IF;
END;
$$;

-- Functions that change; the signature of each is the one of 0.1

-- The checks on the values of a policy that do not depend on who sets it.
CREATE OR REPLACE FUNCTION kingres.timepart__validate_policy(
    p_keytype text, p_every interval, p_tz text, p_ahead interval,
    p_retention interval, p_action text, p_col_after interval,
    p_col_sort text[], p_col_options jsonb, p_run_every interval)
RETURNS void
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_parts record;
BEGIN
    PERFORM kingres.timepart__check_tz(p_tz);
    SELECT * INTO v_parts FROM kingres.timepart_interval_parts(p_every);
    IF p_keytype = 'date' AND v_parts.unit = 'hour' THEN
        RAISE EXCEPTION 'partition_every in hours needs a timestamp or timestamptz column, not date'
            USING ERRCODE = '22023';
    END IF;
    IF p_ahead IS NULL OR p_ahead < interval '0' THEN
        RAISE EXCEPTION 'create_ahead must not be negative' USING ERRCODE = '22023';
    END IF;
    IF p_run_every IS NULL OR p_run_every <= interval '0' THEN
        RAISE EXCEPTION 'run_every must be positive' USING ERRCODE = '22023';
    END IF;
    IF p_action IS NULL OR p_action NOT IN ('drop', 'detach') THEN
        RAISE EXCEPTION 'retention_action must be drop or detach'
            USING ERRCODE = '22023';
    END IF;
    IF p_retention IS NOT NULL AND p_retention < p_every THEN
        RAISE EXCEPTION 'retention must be at least one partition_every'
            USING ERRCODE = '22023',
                  HINT = 'Retention drops whole partitions only.';
    END IF;
    IF p_col_after IS NOT NULL AND p_col_after < interval '0' THEN
        RAISE EXCEPTION 'columnar_after must not be negative' USING ERRCODE = '22023';
    END IF;
    IF p_col_after IS NOT NULL AND p_retention IS NOT NULL
       AND p_col_after > p_retention THEN
        RAISE EXCEPTION 'columnar_after must not be longer than retention'
            USING ERRCODE = '22023';
    END IF;
    IF p_col_options IS NOT NULL
       AND jsonb_typeof(p_col_options) <> 'object' THEN
        RAISE EXCEPTION 'columnar_options must be a JSON object'
            USING ERRCODE = '22023';
    END IF;
    PERFORM kingres.timepart__columnar_options(p_col_options);
    IF p_col_sort IS NOT NULL
       AND (cardinality(p_col_sort) = 0
            OR array_position(p_col_sort, NULL::text) IS NOT NULL) THEN
        RAISE EXCEPTION 'columnar_sort must be a non-empty list of column names'
            USING ERRCODE = '22023';
    END IF;
END;
$$;

-- The first half of an action, SECURITY DEFINER: the checks, the claim of the
-- table, the plan from the catalog at the run's as_of, and the actions that
-- need no DDL (misaligned partitions, the conversion when kingres_columnar is
-- not installed), which it records.  It returns what the caller needs to run the DDL: the
-- action to take and who runs it (o_mode: "direct" when the caller is the owner
-- and drives from its own session, "frame" when a superuser runs the owner's
-- code in its session and trusts that, "recorded" when the action was recorded
-- here and there is nothing to run, "none" when the phase has nothing left).
-- What the caller is (p_caller_oid, p_caller_super, p_caller_plain: it has not
-- changed role) comes from the caller; a caller that lies about it can only
-- be told to run statements it could run anyway.  The caller is judged by
-- session_user: a superuser, or the owner of the table.
CREATE OR REPLACE FUNCTION kingres.timepart__step_begin(
    p_run bigint, p_phase text,
    p_caller_oid oid, p_caller_super boolean, p_caller_plain boolean,
    OUT o_more boolean, OUT o_mode text, OUT o_kind text,
    OUT o_owner_oid oid, OUT o_parent oid, OUT o_keytype text, OUT o_am name,
    OUT o_part_schema name, OUT o_part_name name, OUT o_part_oid oid,
    OUT o_from timestamptz, OUT o_to timestamptz, OUT o_bytes bigint,
    OUT o_retries integer, OUT o_sleep_min integer, OUT o_sleep_max integer)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, kingres, pg_temp
SET DateStyle = 'ISO, MDY'
AS $$
DECLARE
    v_run     kingres.timepart_run_data%ROWTYPE;
    v_pol     kingres.timepart_policy_data%ROWTYPE;
    v_info    record;
    v_row     record;
    v_kinds   text[];
    v_seq     integer;
    v_ncols   integer;
    v_have    boolean;
    v_owner_super boolean;
BEGIN
    o_more := false; o_mode := 'none';
    IF p_phase IS NULL OR p_phase NOT IN ('create', 'convert', 'retire') THEN
        RAISE EXCEPTION 'phase must be create, convert or retire'
            USING ERRCODE = '22023';
    END IF;
    -- A key share lock only: the claim of the table orders the steps of its runs,
    -- and a rename or a drop of the table has its event trigger change the rows
    -- of the run, which a stronger lock of this step would hold up (a deadlock
    -- with the DDL that waits for the table that this step is about to read).
    SELECT r.* INTO v_run FROM kingres.timepart_run_data r
     WHERE r.run_id = p_run FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'run % does not exist', p_run USING ERRCODE = '42704';
    END IF;
    -- the owner recorded in the run is meant by its OID when the run was made
    -- here; a run from a backup is a superuser's to drive
    PERFORM kingres.timepart__require_driver(
        CASE WHEN v_run.stamp = kingres.timepart__stamp()
             THEN v_run.table_owner_oid ELSE 0::pg_catalog.oid END,
        p_caller_oid, p_caller_super);
    IF v_run.state <> 'running' THEN
        RETURN;
    END IF;
    -- One step per table at a time, whatever session runs it: a scheduler may
    -- use another connection for each step.  Steps re-plan from the catalog, so
    -- steps of two runs that follow each other are safe; they must not overlap.
    PERFORM kingres.timepart__claim(v_run.schema_name, v_run.table_name,
                                    'another step of a run of table %.% is in progress');
    o_retries := kingres.timepart__setting_int(
                     'kingres_timepart.lock_retries', 5, 1, 20);
    o_sleep_min := kingres.timepart__setting_int(
                     'kingres_timepart.retry_sleep_min_ms', 200, 0, 60000);
    o_sleep_max := kingres.timepart__setting_int(
                     'kingres_timepart.retry_sleep_max_ms', 2000, 0, 60000);
    IF o_sleep_max < o_sleep_min THEN
        o_sleep_max := o_sleep_min;
    END IF;

    BEGIN
        -- Reading the bounds of the partitions takes a lock on them: the wait is
        -- bounded from the first statement on.
        PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout(
            CASE WHEN p_phase = 'retire' THEN 'retire' ELSE 'create' END), true);
        o_parent := kingres.timepart__oid(v_run.schema_name, v_run.table_name)::oid;
        IF o_parent IS NULL THEN
            RAISE EXCEPTION 'table %.% does not exist',
                quote_ident(v_run.schema_name), quote_ident(v_run.table_name)
                USING ERRCODE = '42P01';
        END IF;
        SELECT p.* INTO v_pol FROM kingres.timepart_policy_data p
         WHERE p.schema_name = v_run.schema_name
           AND p.table_name = v_run.table_name;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'table %.% has no time partitioning policy',
                quote_ident(v_run.schema_name), quote_ident(v_run.table_name)
                USING ERRCODE = '42704';
        END IF;
        -- the policy is acted on only if it was set or adopted here, and is as
        -- it was then (see the seal)
        IF NOT kingres.timepart__is_sealed(v_pol.schema_name, v_pol.table_name) THEN
            RAISE EXCEPTION 'policy_unverified: the policy of %.% was neither set nor adopted in this installation; nothing acts on it',
                quote_ident(v_run.schema_name), quote_ident(v_run.table_name)
                USING ERRCODE = '42501';
        END IF;
        SELECT * INTO v_info FROM kingres.timepart__key_info(o_parent);
        IF v_info.r_owner_oid IS DISTINCT FROM v_pol.table_owner_oid THEN
            RAISE EXCEPTION 'owner_changed: the owner of %.% is now "%", the policy was set for "%"',
                quote_ident(v_run.schema_name), quote_ident(v_run.table_name),
                v_info.r_owner,
                kingres.timepart__role_name(v_pol.stamp, v_pol.table_owner_oid,
                                            v_pol.table_owner)
                USING ERRCODE = '42501';
        END IF;
        IF v_pol.parent_oid <> o_parent THEN
            UPDATE kingres.timepart_policy_data p SET parent_oid = o_parent
             WHERE p.schema_name = v_pol.schema_name
               AND p.table_name = v_pol.table_name;
        END IF;
        o_owner_oid := v_info.r_owner_oid;
        o_keytype := v_info.r_keytype;
        SELECT a.amname INTO o_am FROM pg_am a WHERE a.oid = v_info.r_am;

        SELECT coalesce(max(a.seq), 0) + 1 INTO v_seq
          FROM kingres.timepart_run_action_data a WHERE a.run_id = p_run;

        -- The conversion needs kingres_columnar: without it the step says so
        -- once per run and goes on (the run is not failed).
        IF p_phase = 'convert'
           AND NOT EXISTS (SELECT 1 FROM pg_extension e
                            WHERE e.extname = 'kingres_columnar') THEN
            SELECT count(*) INTO v_ncols
              FROM kingres.timepart__plan_rows(
                       o_parent, v_pol.partition_every, v_pol.timezone,
                       v_pol.create_ahead, v_pol.retention,
                       v_pol.retention_action, v_pol.columnar_after,
                       v_run.as_of) pr
             WHERE pr.kind = 'convert';
            SELECT EXISTS (SELECT 1 FROM kingres.timepart_run_action_data a
                            WHERE a.run_id = p_run AND a.kind = 'convert')
              INTO v_have;
            IF v_ncols = 0 OR v_have THEN
                RETURN;
            END IF;
            INSERT INTO kingres.timepart_run_action_data (
                run_id, seq, kind, partition_schema, partition_name, state,
                detail, finished_at)
            VALUES (p_run, v_seq, 'convert', v_run.schema_name,
                    v_run.table_name, 'skipped',
                    format('conversion needs kingres_columnar, which is not installed; %s partition(s) older than columnar_after were not converted', v_ncols),
                    now());
            o_more := true; o_mode := 'recorded';
            RETURN;
        END IF;

        v_kinds := CASE p_phase WHEN 'create' THEN ARRAY['misaligned', 'create']
                                WHEN 'convert' THEN ARRAY['convert']
                                ELSE ARRAY['drop', 'detach'] END;
        SELECT pr.* INTO v_row
          FROM kingres.timepart__plan_rows(
                   o_parent, v_pol.partition_every, v_pol.timezone,
                   v_pol.create_ahead, v_pol.retention, v_pol.retention_action,
                   v_pol.columnar_after, v_run.as_of) pr
         WHERE pr.kind = ANY (v_kinds)
           AND NOT EXISTS (SELECT 1 FROM kingres.timepart_run_action_data a
                            WHERE a.run_id = p_run AND a.kind = pr.kind
                              AND a.partition_schema = pr.part_schema
                              AND a.partition_name = pr.part_name)
         ORDER BY pr.seq LIMIT 1;
        IF NOT FOUND THEN
            RETURN;
        END IF;

        IF v_row.kind = 'misaligned' THEN
            INSERT INTO kingres.timepart_run_action_data (
                run_id, seq, kind, partition_schema, partition_name,
                range_from, range_to, state, detail, finished_at)
            VALUES (p_run, v_seq, 'misaligned', v_row.part_schema,
                    v_row.part_name, v_row.range_from, v_row.range_to,
                    'skipped', v_row.reason || '; left alone', now());
            o_more := true; o_mode := 'recorded';
            RETURN;
        END IF;

        -- Who runs the owner's code, and where (see timepart__exec_as).
        SELECT r.rolsuper INTO v_owner_super FROM pg_roles r WHERE r.oid = o_owner_oid;
        IF p_caller_plain AND p_caller_oid = o_owner_oid THEN
            o_mode := 'direct';
        ELSIF p_caller_super AND (coalesce(v_owner_super, false)
                                  OR kingres.timepart__trust_owners()) THEN
            o_mode := 'frame';
        ELSE
            RAISE EXCEPTION 'owner_code_not_trusted: the code of the owner of %.% would run in the session of "%"',
                quote_ident(v_run.schema_name), quote_ident(v_run.table_name),
                pg_get_userbyid(p_caller_oid)
                USING ERRCODE = '42501',
                      HINT = 'Drive the maintenance of this table as its owner, or set kingres_timepart.trust_table_owners = on in the session of a superuser if the owners of the tables are trusted as the superuser is.';
        END IF;

        o_kind := v_row.kind;
        o_part_schema := v_row.part_schema;
        o_part_name := v_row.part_name;
        o_part_oid := v_row.part_oid;
        o_from := v_row.range_from;
        o_to := v_row.range_to;
        o_bytes := CASE WHEN v_row.part_oid IS NULL THEN NULL
                        ELSE pg_total_relation_size(v_row.part_oid) END;
        o_more := true;
        RETURN;
    EXCEPTION WHEN OTHERS THEN
        -- The run itself failed (not one action): record it.
        UPDATE kingres.timepart_run_data r
           SET state = 'failed', finished_at = now(),
               error = SQLSTATE || ': ' || SQLERRM
         WHERE r.run_id = p_run;
        o_more := false; o_mode := 'none';
        RETURN;
    END;
END;
$$;

-- The second half: record what one action came to.
CREATE OR REPLACE FUNCTION kingres.timepart__step_end(
    p_run bigint, p_kind text, p_part_schema name, p_part_name name,
    p_from timestamptz, p_to timestamptz, p_state text, p_detail text,
    p_bytes bigint,
    p_caller_oid oid, p_caller_super boolean)
RETURNS void
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_run kingres.timepart_run_data%ROWTYPE;
BEGIN
    SELECT r.* INTO v_run FROM kingres.timepart_run_data r
     WHERE r.run_id = p_run FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'run % does not exist', p_run USING ERRCODE = '42704';
    END IF;
    PERFORM kingres.timepart__require_driver(
        CASE WHEN v_run.stamp = kingres.timepart__stamp()
             THEN v_run.table_owner_oid ELSE 0::pg_catalog.oid END,
        p_caller_oid, p_caller_super);
    IF v_run.state <> 'running' THEN
        RAISE EXCEPTION 'run % is not running', p_run USING ERRCODE = '55000';
    END IF;
    INSERT INTO kingres.timepart_run_action_data (
        run_id, seq, kind, partition_schema, partition_name, range_from,
        range_to, state, detail, bytes_before, finished_at)
    SELECT p_run, coalesce(max(a.seq), 0) + 1, p_kind, p_part_schema,
           p_part_name, p_from, p_to, p_state, p_detail,
           CASE WHEN p_kind IN ('drop', 'detach', 'convert') THEN p_bytes END,
           CASE WHEN p_kind = 'convert' THEN clock_timestamp() ELSE now() END
      FROM kingres.timepart_run_action_data a WHERE a.run_id = p_run;
END;
$$;

-- Execute the next outstanding action of one phase and record it; return
-- false when the phase has nothing left.  A driver that wants a time limit for
-- an action issues each call as a statement of its own after SET
-- statement_timeout (see timepart_action_timeout): when the limit strikes while
-- the action runs, the action is recorded as failed with detail
-- statement_timeout and the run goes on.  (A timer cannot be started from
-- inside a running statement, so timepart_run and timepart_run_phase, which
-- are one statement, can only be bounded as a whole.)  It re-plans from the
-- catalog at the run's as_of every time, so it is idempotent: after an
-- interruption the same call does what is left.  An action that failed or was
-- skipped in this run is not tried again in it.  Each call is one transaction
-- of the caller (timepart_run_phase commits after each).  The DDL runs as the
-- owner of the table: directly when the caller is the owner, else in the
-- session of the superuser that calls, see timepart__exec_as.  Lock waits are
-- bounded by the settings kingres_timepart.create_lock_timeout_ms /
-- retire_lock_timeout_ms with up to kingres_timepart.lock_retries attempts
-- separated by a random sleep between retry_sleep_min_ms and
-- retry_sleep_max_ms.  SECURITY INVOKER, because the DDL must run as the
-- caller; the records are written by timepart__step_begin and
-- timepart__step_end.
CREATE OR REPLACE FUNCTION kingres.timepart_run_action(p_run bigint, p_phase text)
RETURNS boolean
LANGUAGE plpgsql
SET search_path = pg_catalog, kingres, pg_temp
SET DateStyle = 'ISO, MDY'
AS $$
DECLARE
    v_s       record;
    v_self    oid;
    v_super   boolean;
    v_stmts   text[];
    v_state   text;
    v_detail  text;
    v_attempt integer;
    v_tries   integer;
    v_info    record;
    v_sort    text[];
    v_opts    jsonb;
BEGIN
    SELECT r.oid, r.rolsuper INTO v_self, v_super
      FROM pg_roles r WHERE r.rolname = current_user;
    SELECT * INTO v_s FROM kingres.timepart__step_begin(
        p_run, p_phase, v_self, v_super, session_user = current_user);
    IF NOT v_s.o_more THEN
        RETURN false;
    END IF;
    IF v_s.o_mode = 'recorded' THEN
        RETURN true;
    END IF;

    v_state := 'failed';
    -- A conversion copies a whole partition: it is tried once in a run (a
    -- lock that is not granted in time is tried again by the next run, not
    -- by a second copy).
    v_tries := CASE WHEN v_s.o_kind = 'convert' THEN 1 ELSE v_s.o_retries END;
    FOR v_attempt IN 1..v_tries LOOP
        BEGIN
            -- The statements and the lock timeout are prepared here; only the
            -- DDL runs as the table's owner.
            IF v_s.o_kind = 'create' THEN
                PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
                v_stmts := kingres.timepart__create_stmts(
                    v_s.o_parent, v_s.o_part_schema, v_s.o_part_name,
                    kingres.timepart__bound_literal(v_s.o_keytype, v_s.o_from),
                    kingres.timepart__bound_literal(v_s.o_keytype, v_s.o_to),
                    v_s.o_am);
            ELSIF v_s.o_kind = 'convert' THEN
                -- The sort and the options are the policy's; the checks, the
                -- statements and the lock waits of the conversion are
                -- timepart__convert_stmts's.
                PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
                SELECT * INTO v_info FROM kingres.timepart__key_info(v_s.o_parent);
                SELECT p.columnar_sort, p.columnar_options INTO v_sort, v_opts
                  FROM kingres.timepart_policy p
                 WHERE p.schema_name = v_info.r_schema
                   AND p.table_name = v_info.r_table;
                v_stmts := kingres.timepart__convert_stmts(
                    v_s.o_parent, v_s.o_part_oid, 'columnar'::name, v_sort, v_opts);
            ELSE
                PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('retire'), true);
                v_stmts := kingres.timepart__retire_stmts(
                    v_s.o_parent, v_s.o_part_oid, v_s.o_kind = 'detach');
            END IF;
            IF v_s.o_mode = 'direct' THEN
                PERFORM kingres.timepart__exec_direct(v_stmts);
            ELSE
                PERFORM kingres.timepart__exec_as(v_s.o_owner_oid, v_stmts);
            END IF;
            v_state := 'done';
            v_detail := CASE WHEN v_s.o_kind = 'convert' THEN 'converted to columnar' END;
            EXIT;
        EXCEPTION
            WHEN lock_not_available THEN
                v_state := 'failed';
                IF v_s.o_kind = 'convert' AND SQLERRM LIKE 'swap_lock_timeout:%' THEN
                    v_detail := SQLERRM;
                ELSE
                    v_detail := 'lock_timeout: ' || SQLERRM
                                || format(' (attempt %s of %s)', v_attempt, v_tries);
                END IF;
            WHEN query_canceled THEN
                -- The statement_timeout of the driver's statement, or a
                -- cancel by an operator: only the first is an action's
                -- failure.
                IF NOT kingres.timepart__timed_out() THEN
                    RAISE;
                END IF;
                v_state := 'failed';
                v_detail := 'statement_timeout: ' || SQLERRM;
                EXIT;
            WHEN OTHERS THEN
                IF v_s.o_kind = 'convert' AND SQLERRM LIKE 'conversion_refused:%' THEN
                    -- Not a failure of the run: the partition cannot be
                    -- converted as it is (timepart_check reports it).
                    v_state := 'skipped';
                    v_detail := SQLERRM;
                ELSE
                    v_state := 'failed';
                    v_detail := SQLSTATE || ': ' || SQLERRM;
                END IF;
                EXIT;
        END;
        IF v_attempt < v_tries THEN
            PERFORM pg_sleep((v_s.o_sleep_min + random() * (v_s.o_sleep_max - v_s.o_sleep_min)) / 1000.0);
        END IF;
    END LOOP;

    PERFORM kingres.timepart__step_end(
        p_run, v_s.o_kind, v_s.o_part_schema, v_s.o_part_name, v_s.o_from,
        v_s.o_to, v_state, v_detail, v_s.o_bytes, v_self, v_super);
    RETURN true;
END;
$$;

-- Run one phase of a run: create, convert or retire.  One transaction per
-- action; it cannot run inside a transaction block.  Nothing here may be
-- redirected by the caller's search_path: the first statement, and the first
-- after every COMMIT, sets the path of the transaction (a procedure with a SET
-- clause cannot COMMIT), and every function called is schema-qualified.
CREATE OR REPLACE PROCEDURE kingres.timepart_run_phase(run_id bigint, phase text)
LANGUAGE plpgsql
AS $$
DECLARE
    v_more boolean;
BEGIN
    PERFORM pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true);
    LOOP
        v_more := kingres.timepart_run_action(run_id, phase);
        COMMIT;
        PERFORM pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true);
        -- After a conversion has committed: its result in the history and
        -- the ANALYZE of the new partition, in a transaction of their own.
        IF phase OPERATOR(pg_catalog.=) 'convert' THEN
            PERFORM kingres.timepart__convert_after(run_id);
            COMMIT;
            PERFORM pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true);
        END IF;
        EXIT WHEN NOT v_more;
    END LOOP;
END;
$$;

-- Close a run: the final state, the next run time and the pruning of old runs.  The state is "failed" when the
-- run itself failed, "partial" when an action failed, otherwise "done"
-- (skipped actions are not failures).  next_run_at is as_of + run_every, or
-- as_of + least(run_every, 5 minutes) when an action failed on a lock
-- timeout.
CREATE OR REPLACE FUNCTION kingres.timepart__finish_run(run_id bigint, p_caller_oid oid,
                                          p_caller_super boolean)
RETURNS void
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_run     kingres.timepart_run_data%ROWTYPE;
    v_nfailed integer;
    v_locked  boolean;
    v_state   text;
BEGIN
    PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
    SELECT r.* INTO v_run FROM kingres.timepart_run_data r
     WHERE r.run_id = timepart__finish_run.run_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'run % does not exist', run_id USING ERRCODE = '42704';
    END IF;
    PERFORM kingres.timepart__require_driver(
        CASE WHEN v_run.stamp = kingres.timepart__stamp()
             THEN v_run.table_owner_oid ELSE 0::pg_catalog.oid END,
        p_caller_oid, p_caller_super);
    SELECT count(*) FILTER (WHERE a.state = 'failed'),
           coalesce(bool_or(a.state = 'failed'
                            AND (a.detail LIKE 'lock_timeout%'
                                 OR a.detail LIKE 'swap_lock_timeout%')), false)
      INTO v_nfailed, v_locked
      FROM kingres.timepart_run_action_data a
     WHERE a.run_id = timepart__finish_run.run_id;
    -- A run that could not even read the partitions because of a lock (55P03)
    -- is retried soon, like an action that timed out.
    v_locked := v_locked OR coalesce(v_run.error LIKE '55P03:%', false);
    v_state := CASE WHEN v_run.state = 'failed' THEN 'failed'
                    WHEN v_nfailed > 0 THEN 'partial'
                    ELSE 'done' END;
    UPDATE kingres.timepart_run_data r
       SET state = v_state, finished_at = coalesce(r.finished_at, now())
     WHERE r.run_id = timepart__finish_run.run_id;
    -- The owner of the table can hold a lock on its policy row: then the
    -- policy stays due, and the run is closed all the same.
    BEGIN
        UPDATE kingres.timepart_policy_data p
           SET next_run_at = kingres.timepart__add(
                   v_run.as_of,
                   CASE WHEN v_locked THEN least(p.run_every, interval '5 minutes')
                        ELSE p.run_every END,
                   'UTC', 1)
         WHERE p.schema_name = v_run.schema_name AND p.table_name = v_run.table_name;
    EXCEPTION WHEN lock_not_available THEN
        NULL;
    END;
    PERFORM kingres.timepart_prune_runs(v_run.schema_name, v_run.table_name,
                                        v_run.as_of);
END;
$$;

-- Problems of the tables that have a policy: ahead_low, last_run_failed,
-- retire_blocked, convert_blocked, policy_orphan, policy_unverified,
-- default_partition_present, misaligned and lock_timeout.  A superuser sees
-- every table, another role sees the tables it owns.  p_as_of is the time
-- "ahead" is measured from.
CREATE OR REPLACE FUNCTION kingres.timepart_check(
    min_ahead interval DEFAULT NULL, as_of timestamptz DEFAULT now())
RETURNS TABLE (schema_name name, table_name name, problem text, detail text)
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_super  boolean;
    v_pol    kingres.timepart_policy%ROWTYPE;
    v_oid    oid;
    v_info   record;
    v_ok     boolean;
    v_tz     text;
    v_last   timestamptz;
    v_need   timestamptz;
    v_run    kingres.timepart_run%ROWTYPE;
    v_mis    text;
BEGIN
    -- Reading the bounds of the partitions takes locks: bound the wait, so that
    -- a long transaction on the table cannot hold a monitor up.
    PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
    SELECT coalesce(bool_or(r.rolsuper), false) INTO v_super
      FROM pg_roles r WHERE r.rolname = current_user;
    FOR v_pol IN SELECT p.* FROM kingres.timepart_policy p
                  ORDER BY p.schema_name, p.table_name
    LOOP
        BEGIN
            v_oid := kingres.timepart__oid(v_pol.schema_name, v_pol.table_name)::oid;
            v_ok := false;
            IF v_oid IS NOT NULL THEN
                BEGIN
                    SELECT * INTO v_info FROM kingres.timepart__key_info(v_oid);
                    v_ok := true;
                EXCEPTION WHEN lock_not_available THEN
                    RAISE;
                WHEN OTHERS THEN
                    v_ok := false;
                END;
            END IF;
            IF NOT v_ok THEN
                IF v_super THEN
                    schema_name := v_pol.schema_name; table_name := v_pol.table_name;
                    problem := 'policy_orphan';
                    detail := 'the table does not exist or is no longer a suitable partitioned table';
                    RETURN NEXT;
                END IF;
                CONTINUE;
            END IF;
            IF NOT pg_has_role(current_user, v_info.r_owner_oid, 'USAGE') THEN
                CONTINUE;
            END IF;

            IF NOT kingres.timepart__is_sealed(v_pol.schema_name, v_pol.table_name) THEN
                schema_name := v_pol.schema_name; table_name := v_pol.table_name;
                problem := 'policy_unverified';
                detail := 'the policy was neither set nor adopted in this installation; nothing acts on it';
                RETURN NEXT;
                CONTINUE;
            END IF;

            IF v_info.r_default IS NOT NULL THEN
                schema_name := v_pol.schema_name; table_name := v_pol.table_name;
                problem := 'default_partition_present';
                detail := format('default partition %I', v_info.r_default);
                RETURN NEXT;
            END IF;

            v_tz := CASE WHEN v_info.r_keytype = 'timestamptz'
                         THEN v_pol.timezone ELSE 'UTC' END;
            SELECT max(c.hi) INTO v_last
              FROM kingres.timepart__children(v_oid) c WHERE NOT c.is_default;
            v_need := kingres.timepart__add(
                kingres.timepart__eff(as_of, v_info.r_keytype, v_pol.timezone),
                coalesce(min_ahead, v_pol.create_ahead / 2), v_tz, 1);
            IF v_last IS NULL OR v_last < v_need THEN
                schema_name := v_pol.schema_name; table_name := v_pol.table_name;
                problem := 'ahead_low';
                detail := CASE WHEN v_last IS NULL THEN 'the table has no partitions'
                               ELSE format('the last partition ends at %s, before %s',
                                           v_last, v_need) END;
                RETURN NEXT;
            END IF;

            SELECT string_agg(format('%I', m.part_name), ', ' ORDER BY m.lo)
              INTO v_mis
              FROM kingres.timepart__misaligned(v_oid, v_pol.partition_every, v_tz) m;
            IF v_mis IS NOT NULL THEN
                schema_name := v_pol.schema_name; table_name := v_pol.table_name;
                problem := 'misaligned';
                detail := 'partitions with bounds off the grid: ' || v_mis;
                RETURN NEXT;
            END IF;

            SELECT r.* INTO v_run FROM kingres.timepart_run r
             WHERE r.table_oid = v_oid
               AND r.state <> 'running'
             ORDER BY r.run_id DESC LIMIT 1;
            IF FOUND AND v_run.state IN ('failed', 'partial') THEN
                schema_name := v_pol.schema_name; table_name := v_pol.table_name;
                problem := 'last_run_failed';
                detail := format('the last run (as of %s) ended %s%s', v_run.as_of,
                                 v_run.state, coalesce(': ' || v_run.error, ''));
                RETURN NEXT;
                -- Actions of that run that could not retire a partition: a view or
                -- a foreign key that needs it, a role that does not own it.
                SELECT string_agg(format('%I (%s)', a.partition_name, left(a.detail, 60)),
                                  '; ' ORDER BY a.seq)
                  INTO v_mis
                  FROM kingres.timepart_run_action a
                 WHERE a.run_id = v_run.run_id AND a.state = 'failed'
                   AND a.kind IN ('drop', 'detach');
                IF v_mis IS NOT NULL THEN
                    schema_name := v_pol.schema_name; table_name := v_pol.table_name;
                    problem := 'retire_blocked';
                    detail := 'partitions that could not be retired: ' || v_mis;
                    RETURN NEXT;
                END IF;
            END IF;
            -- Partitions that the last run could not convert: refused (a
            -- foreign key, an index, a trigger ...), failed (a lock that was
            -- not granted in time, a writer that kept the parent busy), or
            -- not tried because kingres_columnar is not installed.
            IF v_run.run_id IS NOT NULL THEN
                SELECT string_agg(format('%I (%s)', a.partition_name, left(a.detail, 80)),
                                  '; ' ORDER BY a.seq)
                  INTO v_mis
                  FROM kingres.timepart_run_action a
                 WHERE a.run_id = v_run.run_id AND a.kind = 'convert'
                   AND a.state IN ('failed', 'skipped');
                IF v_mis IS NOT NULL THEN
                    schema_name := v_pol.schema_name; table_name := v_pol.table_name;
                    problem := 'convert_blocked';
                    detail := 'partitions that could not be converted: ' || v_mis;
                    RETURN NEXT;
                END IF;
            END IF;
        EXCEPTION WHEN lock_not_available THEN
            schema_name := v_pol.schema_name; table_name := v_pol.table_name;
            problem := 'lock_timeout';
            detail := 'the partitions could not be read before the lock timeout';
            RETURN NEXT;
        WHEN OTHERS THEN
            schema_name := v_pol.schema_name; table_name := v_pol.table_name;
            problem := 'check_failed';
            detail := left(SQLSTATE || ': ' || SQLERRM, 200);
            RETURN NEXT;
        END;
    END LOOP;
END;
$$;

-- The owner of a table drives the maintenance of its own tables from its own
-- session (see timepart__exec_as): these two give a role the EXECUTE that
-- takes and drop it again.  The role can then run a table of its own through
-- timepart_run, or step by step through timepart_due, timepart_begin_run,
-- timepart_run_phase or timepart_run_action, and timepart_finish_run; the
-- functions check that the table is the caller's.  Superuser only.
CREATE OR REPLACE FUNCTION kingres.timepart_grant_driver(p_role name)
RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, kingres, pg_temp
AS $$
BEGIN
    PERFORM kingres.timepart__require_superuser();
    EXECUTE pg_catalog.format('GRANT USAGE ON SCHEMA kingres TO %I', p_role);
    EXECUTE pg_catalog.format(
        'GRANT EXECUTE ON FUNCTION '
        'kingres.timepart_due(timestamptz), '
        'kingres.timepart_begin_run(regclass, timestamptz, text), '
        'kingres.timepart__begin_run(regclass, timestamptz, text, oid, boolean), '
        'kingres.timepart_run_action(bigint, text), '
        'kingres.timepart__step_begin(bigint, text, oid, boolean, boolean), '
        'kingres.timepart__step_end(bigint, text, name, name, timestamptz, timestamptz, text, text, bigint, oid, boolean), '
        'kingres.timepart_finish_run(bigint), '
        'kingres.timepart__finish_run(bigint, oid, boolean), '
        'kingres.timepart__convert_note(bigint, oid, boolean), '
        'kingres.timepart__convert_after(bigint) TO %I', p_role);
    EXECUTE pg_catalog.format(
        'GRANT EXECUTE ON PROCEDURE '
        'kingres.timepart_run_phase(bigint, text), '
        'kingres.timepart_run(regclass, timestamptz, text[], bigint) TO %I', p_role);
END;
$$;

CREATE OR REPLACE FUNCTION kingres.timepart_revoke_driver(p_role name)
RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, kingres, pg_temp
AS $$
BEGIN
    PERFORM kingres.timepart__require_superuser();
    EXECUTE pg_catalog.format(
        'REVOKE EXECUTE ON FUNCTION '
        'kingres.timepart_due(timestamptz), '
        'kingres.timepart_begin_run(regclass, timestamptz, text), '
        'kingres.timepart__begin_run(regclass, timestamptz, text, oid, boolean), '
        'kingres.timepart_run_action(bigint, text), '
        'kingres.timepart__step_begin(bigint, text, oid, boolean, boolean), '
        'kingres.timepart__step_end(bigint, text, name, name, timestamptz, timestamptz, text, text, bigint, oid, boolean), '
        'kingres.timepart_finish_run(bigint), '
        'kingres.timepart__finish_run(bigint, oid, boolean), '
        'kingres.timepart__convert_note(bigint, oid, boolean), '
        'kingres.timepart__convert_after(bigint) FROM %I', p_role);
    EXECUTE pg_catalog.format(
        'REVOKE EXECUTE ON PROCEDURE '
        'kingres.timepart_run_phase(bigint, text), '
        'kingres.timepart_run(regclass, timestamptz, text[], bigint) FROM %I', p_role);
END;
$$;

-- Replace one leaf partition by a copy in another access method (10.4).  The
-- partition is checked, copied, verified and swapped in one transaction (see
-- timepart__convert_stmts); a failure at any point leaves the old partition
-- as it was.  The transaction commits, and the new partition is analyzed in a
-- transaction of its own.  It cannot run inside a transaction block, and the
-- search path is set as in timepart_run.  Called by the owner of the table, as
-- itself, or by a superuser (the owner's code then runs in the superuser's
-- session, which timepart__exec_as refuses unless the owner is a superuser or
-- the session trusts the owners of its tables).
CREATE OR REPLACE PROCEDURE kingres.convert_partition(
    parent regclass, part regclass,
    access_method name DEFAULT 'columnar', sort_by text[] DEFAULT NULL,
    options jsonb DEFAULT NULL)
LANGUAGE plpgsql
AS $$
DECLARE
    v_info   record;
    v_stmts  text[];
    v_self   oid;
    v_super  boolean;
    v_pschema name;
    v_pname   name;
BEGIN
    PERFORM pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true);
    -- Refuse a transaction block before anything is done.
    COMMIT;
    PERFORM pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true);
    IF parent IS NULL OR part IS NULL OR access_method IS NULL THEN
        RAISE EXCEPTION 'parent, part and access_method must not be null'
            USING ERRCODE = '22004';
    END IF;
    IF kingres.timepart__am_kind(access_method) IS DISTINCT FROM 'heap' THEN
        PERFORM kingres.timepart__need_columnar();
    END IF;
    PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
    SELECT r.oid, r.rolsuper INTO v_self, v_super
      FROM pg_catalog.pg_roles r WHERE r.rolname OPERATOR(pg_catalog.=) current_user;
    SELECT * INTO v_info FROM kingres.timepart__key_info(parent::pg_catalog.oid);
    SELECT n.nspname, c.relname INTO v_pschema, v_pname
      FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = part::pg_catalog.oid;
    v_stmts := kingres.timepart__convert_stmts(
        parent::pg_catalog.oid, part::pg_catalog.oid, access_method, sort_by, options);
    IF v_self = v_info.r_owner_oid AND session_user = current_user THEN
        PERFORM kingres.timepart__exec_direct(v_stmts);
    ELSIF v_super THEN
        PERFORM kingres.timepart__exec_as(v_info.r_owner_oid, v_stmts);
    ELSE
        RAISE EXCEPTION 'permission denied: only the owner of the table itself or a superuser may convert its partitions'
            USING ERRCODE = '42501';
    END IF;
    COMMIT;
    PERFORM pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true);
    PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
    BEGIN
        EXECUTE pg_catalog.format('ANALYZE %I.%I', v_pschema, v_pname);
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'ANALYZE of %.% was not done: %',
            pg_catalog.quote_ident(v_pschema), pg_catalog.quote_ident(v_pname), SQLERRM;
    END;
END;
$$;

-- convert_partition for every partition whose upper bound is at or before
-- older_than and whose access method differs, oldest first, one transaction
-- each.  Partitions committed before a failure stay converted; the first
-- partition that is refused or fails ends the call with its error (the policy
-- path, timepart_run, records each partition and goes on).  A timestamp or
-- date key is read as UTC wall-clock time here, as in
-- drop_old_time_partitions called with its default zone.
CREATE OR REPLACE PROCEDURE kingres.convert_old_partitions(
    parent regclass, older_than timestamptz,
    access_method name DEFAULT 'columnar', sort_by text[] DEFAULT NULL,
    options jsonb DEFAULT NULL)
LANGUAGE plpgsql
AS $$
DECLARE
    v_info   record;
    v_kind   text;
    v_limit  timestamptz;
    v_parts  oid[];
    v_oid    oid;
BEGIN
    PERFORM pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true);
    -- Refuse a transaction block before anything is done.
    COMMIT;
    PERFORM pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true);
    IF parent IS NULL OR older_than IS NULL OR access_method IS NULL THEN
        RAISE EXCEPTION 'parent, older_than and access_method must not be null'
            USING ERRCODE = '22004';
    END IF;
    IF older_than IN ('infinity', '-infinity') THEN
        RAISE EXCEPTION 'older_than must be a finite time' USING ERRCODE = '22008';
    END IF;
    v_kind := kingres.timepart__am_kind(access_method);
    IF v_kind IS DISTINCT FROM 'heap' THEN
        PERFORM kingres.timepart__need_columnar();
    END IF;
    IF v_kind IS NULL OR v_kind OPERATOR(pg_catalog.=) 'other' THEN
        RAISE EXCEPTION 'conversion_refused: "%" is not a supported table access method (use columnar or heap)',
            access_method USING ERRCODE = '0A000';
    END IF;
    PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
    SELECT * INTO v_info FROM kingres.timepart__key_info(parent::pg_catalog.oid);
    PERFORM kingres.timepart__require_owner(
        current_user, v_info.r_owner_oid,
        pg_catalog.format('%I.%I', v_info.r_schema, v_info.r_table));
    v_limit := kingres.timepart__eff(older_than, v_info.r_keytype, 'UTC');
    SELECT coalesce(pg_catalog.array_agg(c.part_oid ORDER BY c.hi), '{}') INTO v_parts
      FROM kingres.timepart__children(parent::pg_catalog.oid) c
     WHERE NOT c.is_default AND c.hi < 'infinity' AND c.hi <= v_limit
       AND kingres.timepart__am_kind(c.am_name) IS DISTINCT FROM v_kind;
    FOREACH v_oid IN ARRAY v_parts LOOP
        CALL kingres.convert_partition(parent, v_oid::pg_catalog.regclass,
                                       access_method, sort_by, options);
        PERFORM pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true);
    END LOOP;
END;
$$;

-- timepart__no_columnar of 0.1 (executable by superusers only) is replaced by
-- timepart__need_columnar, which every role that may convert can call
DROP FUNCTION kingres.timepart__no_columnar();

-- New functions are not executable by PUBLIC
REVOKE ALL ON FUNCTION
    kingres.timepart__convert_note(bigint, oid, boolean),
    kingres.timepart__convert_after(bigint)
    FROM PUBLIC;

-- A role that was given the right to drive its own maintenance
-- (timepart_grant_driver) is given the two functions the convert phase of a
-- run now calls.
DO $upgrade$
DECLARE
    v_role oid;
BEGIN
    FOR v_role IN
        SELECT DISTINCT x.grantee
          FROM pg_catalog.pg_proc p, LATERAL pg_catalog.aclexplode(p.proacl) x
         WHERE p.oid = 'kingres.timepart_run_action(bigint, text)'::pg_catalog.regprocedure
           AND x.grantee <> 0 AND x.privilege_type = 'EXECUTE'
           AND x.grantee <> p.proowner
    LOOP
        EXECUTE pg_catalog.format(
            'GRANT EXECUTE ON FUNCTION kingres.timepart__convert_note(bigint, oid, boolean), '
            'kingres.timepart__convert_after(bigint) TO %I', pg_catalog.pg_get_userbyid(v_role));
    END LOOP;
END
$upgrade$;

COMMENT ON PROCEDURE kingres.convert_partition(regclass, regclass, name, text[], jsonb) IS
    'Replace a leaf partition by a copy in another access method (columnar or heap), in one transaction; needs kingres_columnar; the owner of the table or a superuser; cannot run inside a transaction block';
COMMENT ON PROCEDURE kingres.convert_old_partitions(regclass, timestamptz, name, text[], jsonb) IS
    'convert_partition for every partition whose upper bound is at or before a time, oldest first; needs kingres_columnar; cannot run inside a transaction block';
