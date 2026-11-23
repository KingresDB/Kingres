-- kingres_timepart--0.2--0.3.sql
--
-- Upgrade of kingres_timepart from 0.2 to 0.3: time partitioning policies on
-- user-partitioned distributed tables (increments St-D1a and St-D1b of the
-- design, section 11).  A policy of a distributed table creates its
-- partitions ahead, retires the old ones and converts them to columnar
-- storage with the partition functions of kingres_shard 0.9 and the leaf
-- conversion of this version, on every node.  The tables and views are
-- unchanged; the functions that read the partitions of a table understand a
-- distributed one, and the functions below are new.  The result is the
-- catalog that kingres_timepart--0.3.sql makes (the regression test
-- "upgrade" compares the two).
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only

\echo Use "ALTER EXTENSION kingres_timepart UPDATE TO '0.3'" to load this file. \quit

------------------------------------------------------------------------
-- Functions whose body changes (the signature is the same as in 0.2)
------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION kingres.timepart__key_info(
    p_rel oid,
    OUT r_schema name, OUT r_table name, OUT r_owner_oid oid,
    OUT r_owner name, OUT r_keycol name, OUT r_keytype text,
    OUT r_persistence "char", OUT r_am oid, OUT r_default name)
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_c    record;
    v_pt   record;
    v_att  record;
    v_name text;
    v_dist record;
BEGIN
    SELECT c.relkind, c.relpersistence, c.relowner, c.relam, n.nspname,
           c.relname, pg_get_userbyid(c.relowner) AS ownername
      INTO v_c
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = p_rel;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'relation with OID % does not exist', p_rel
            USING ERRCODE = '42P01';
    END IF;
    v_name := format('%I.%I', v_c.nspname, v_c.relname);
    IF v_c.relkind <> 'p' THEN
        RAISE EXCEPTION '% is not a partitioned table', v_name
            USING ERRCODE = '42809';
    END IF;
    IF v_c.relpersistence = 't' THEN
        RAISE EXCEPTION 'temporary table "%" is not supported', v_c.relname
            USING ERRCODE = '0A000';
    END IF;
    SELECT * INTO v_dist FROM kingres.timepart__dist(p_rel);
    IF v_dist.r_dist THEN
        -- a user-partitioned distributed table: hash on top, the user's
        -- partitions below; the partition key is in the catalog of kingres_shard
        IF v_dist.r_method <> 'range' THEN
            RAISE EXCEPTION '% is not partitioned by RANGE', v_name
                USING ERRCODE = '0A000';
        END IF;
        SELECT a.attname, a.atttypid INTO v_att
          FROM pg_attribute a
         WHERE a.attrelid = p_rel AND a.attnum > 0 AND NOT a.attisdropped
           AND quote_ident(a.attname) = regexp_replace(v_dist.r_key, '^[(](.*)[)]$', '\1');
        IF NOT FOUND THEN
            RAISE EXCEPTION '% must be partitioned by exactly one column, which is not an expression', v_name
                USING ERRCODE = '0A000';
        END IF;
    ELSE
        SELECT pt.partstrat, pt.partnatts, pt.partattrs[0] AS attnum
          INTO v_pt FROM pg_partitioned_table pt WHERE pt.partrelid = p_rel;
        IF v_pt.partstrat <> 'r' THEN
            RAISE EXCEPTION '% is not partitioned by RANGE', v_name
                USING ERRCODE = '0A000';
        END IF;
        IF v_pt.partnatts <> 1 OR v_pt.attnum = 0 THEN
            RAISE EXCEPTION '% must be partitioned by exactly one column, which is not an expression', v_name
                USING ERRCODE = '0A000';
        END IF;
        SELECT a.attname, a.atttypid INTO v_att
          FROM pg_attribute a
         WHERE a.attrelid = p_rel AND a.attnum = v_pt.attnum;
    END IF;
    r_keytype := CASE v_att.atttypid
        WHEN 'pg_catalog.timestamptz'::regtype THEN 'timestamptz'
        WHEN 'pg_catalog.timestamp'::regtype THEN 'timestamp'
        WHEN 'pg_catalog.date'::regtype THEN 'date'
    END;
    IF r_keytype IS NULL THEN
        RAISE EXCEPTION 'partition column "%" of % must be of type date, timestamp or timestamptz',
            v_att.attname, v_name USING ERRCODE = '0A000';
    END IF;
    r_schema := v_c.nspname;
    r_table := v_c.relname;
    r_owner_oid := v_c.relowner;
    r_owner := v_c.ownername;
    r_keycol := v_att.attname;
    r_persistence := v_c.relpersistence;
    r_am := v_c.relam;
    IF v_dist.r_dist THEN
        EXECUTE 'SELECT p.partition_name FROM kingres.dist_partition p '
                'WHERE p.parent::pg_catalog.oid = $1 AND upper(btrim(p.bound)) = ''DEFAULT'''
            INTO r_default USING p_rel;
    ELSE
        SELECT c2.relname INTO r_default
          FROM pg_inherits i JOIN pg_class c2 ON c2.oid = i.inhrelid
         WHERE i.inhparent = p_rel AND pg_get_expr(c2.relpartbound, c2.oid) = 'DEFAULT';
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION kingres.timepart__children(p_parent oid)
RETURNS TABLE (part_oid oid, part_schema name, part_name name,
               lo timestamptz, hi timestamptz, is_default boolean,
               am_name name)
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
SET DateStyle = 'ISO, MDY'
AS $$
DECLARE
    v_keytype text;
    r         record;
    v_m       text[];
    v_lo      timestamptz;
    v_hi      timestamptz;
    v_dist    boolean;
    v_schema  name;
BEGIN
    SELECT k.r_keytype, k.r_schema INTO v_keytype, v_schema FROM kingres.timepart__key_info(p_parent) k;
    v_dist := (SELECT d.r_dist FROM kingres.timepart__dist(p_parent) d);
    -- The partitions of a distributed table are the rows of dist_partition: logical
    -- objects without a relation or an OID, one leaf per bucket; the bound is the
    -- text that was given to create_partition.
    FOR r IN
        SELECT c.oid AS o, n.nspname AS s, c.relname AS nm,
               pg_get_expr(c.relpartbound, c.oid) AS b, a.amname AS am
          FROM pg_inherits i
          JOIN pg_class c ON c.oid = i.inhrelid
          JOIN pg_namespace n ON n.oid = c.relnamespace
          LEFT JOIN pg_am a ON a.oid = c.relam
         WHERE i.inhparent = p_parent AND NOT v_dist
        UNION ALL
        SELECT NULL::oid, v_schema, p.partition_name,
               CASE WHEN upper(btrim(p.bound)) = 'DEFAULT' THEN 'DEFAULT'
                    ELSE 'FOR VALUES ' || btrim(p.bound) END,
               NULL::name
          FROM kingres.timepart__dist_bounds(p_parent) p
         WHERE v_dist
    LOOP
        part_oid := r.o;
        part_schema := r.s;
        part_name := r.nm;
        am_name := r.am;
        IF r.b = 'DEFAULT' THEN
            lo := NULL;
            hi := NULL;
            is_default := true;
            RETURN NEXT;
            CONTINUE;
        END IF;
        v_m := regexp_match(r.b, '^FOR VALUES\s+FROM\s*\((.*)\)\s*TO\s*\((.*)\)\s*$', 'i');
        IF v_m IS NULL THEN
            RAISE EXCEPTION 'cannot read the bound of partition %.%: %',
                r.s, r.nm, r.b USING ERRCODE = '0A000';
        END IF;
        FOR i IN 1..2 LOOP
            DECLARE
                v_lit text := (regexp_match(btrim(v_m[i]), '^''([^'']*)'''))[1];
                v_val timestamptz;
            BEGIN
                IF v_lit IS NULL THEN
                    IF btrim(v_m[i]) !~* '^(MINVALUE|MAXVALUE)$' THEN
                        RAISE EXCEPTION 'cannot read the bound of partition %.%: %',
                            r.s, r.nm, r.b USING ERRCODE = '0A000';
                    END IF;
                    v_val := CASE WHEN btrim(v_m[i]) ILIKE 'MINVALUE' THEN '-infinity'
                                  ELSE 'infinity' END;
                ELSIF v_keytype = 'timestamptz' THEN
                    v_val := v_lit::timestamptz;
                ELSIF v_keytype = 'timestamp' THEN
                    v_val := v_lit::timestamp AT TIME ZONE 'UTC';
                ELSE
                    v_val := v_lit::date::timestamp AT TIME ZONE 'UTC';
                END IF;
                IF i = 1 THEN v_lo := v_val; ELSE v_hi := v_val; END IF;
            END;
        END LOOP;
        lo := v_lo;
        hi := v_hi;
        is_default := false;
        RETURN NEXT;
    END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION kingres.timepart__create_rows(p_parent oid, p_every interval,
                                              p_tz text, p_k_from bigint,
                                              p_k_to bigint)
RETURNS TABLE (part_schema name, part_name name,
               lo timestamptz, hi timestamptz)
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_info record;
    v_los  timestamptz[];
    v_his  timestamptz[];
    v_cnt  integer;
    j      integer := 1;
    k      bigint;
    v_s    timestamptz;
    v_e    timestamptz;
    v_max  integer;
BEGIN
    IF p_k_to - p_k_from + 1 > 10000 THEN
        RAISE EXCEPTION 'too many partitions in one call: % cells',
            p_k_to - p_k_from + 1
            USING ERRCODE = '54000',
                  HINT = 'Create at most 10000 partitions per call; use a wider partition_every or a shorter range.';
    END IF;
    SELECT * INTO v_info FROM kingres.timepart__key_info(p_parent);
    v_max := CASE WHEN (SELECT d.r_dist FROM kingres.timepart__dist(p_parent) d)
                  THEN 57 ELSE 63 END;
    SELECT coalesce(array_agg(c.lo ORDER BY c.lo), '{}'),
           coalesce(array_agg(c.hi ORDER BY c.lo), '{}')
      INTO v_los, v_his
      FROM kingres.timepart__children(p_parent) c
     WHERE NOT c.is_default;
    v_cnt := coalesce(cardinality(v_los), 0);
    v_s := kingres.timepart_cell_start(p_every, p_tz, p_k_from);
    FOR k IN p_k_from..p_k_to LOOP
        v_e := kingres.timepart_cell_start(p_every, p_tz, k + 1);
        -- Existing partitions do not overlap each other, so they are sorted
        -- by upper bound too; the first one whose upper bound is after the
        -- start of the cell is the only one that can touch it.
        WHILE j <= v_cnt AND v_his[j] <= v_s LOOP
            j := j + 1;
        END LOOP;
        -- A zone that skipped a whole local day gives a cell that starts and
        -- ends at one instant: there is nothing to create for it.
        IF v_e > v_s AND NOT (j <= v_cnt AND v_los[j] < v_e) THEN
            part_schema := v_info.r_schema;
            part_name := kingres.timepart__part_name(v_info.r_table, p_every,
                                                     p_tz, v_s, v_max);
            lo := v_s;
            hi := v_e;
            RETURN NEXT;
        END IF;
        v_s := v_e;
    END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION kingres.timepart__plan_rows(
    p_parent oid, p_every interval, p_tz text, p_ahead interval,
    p_retention interval, p_retention_action text,
    p_columnar_after interval, p_as_of timestamptz)
RETURNS TABLE (seq integer, kind text, part_oid oid, part_schema name,
               part_name name, range_from timestamptz, range_to timestamptz,
               reason text)
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_info    record;
    v_tz      text;
    v_asof    timestamptz;
    v_cur_k   bigint;
    v_first_k bigint;
    v_last_k  bigint;
    v_min_lo  timestamptz;
    v_thr_ret timestamptz;
    v_thr_col timestamptz;
    r         record;
    v_seq     integer := 0;
    v_dist    boolean;
    v_kinds   jsonb;
BEGIN
    IF p_as_of IS NULL OR p_as_of IN ('infinity', '-infinity') THEN
        RAISE EXCEPTION 'as_of must be a finite time' USING ERRCODE = '22008';
    END IF;
    SELECT * INTO v_info FROM kingres.timepart__key_info(p_parent);
    v_dist := (SELECT d.r_dist FROM kingres.timepart__dist(p_parent) d);
    v_tz := CASE WHEN v_info.r_keytype = 'timestamptz' THEN p_tz ELSE 'UTC' END;
    v_asof := kingres.timepart__eff(p_as_of, v_info.r_keytype, p_tz);
    v_thr_ret := CASE WHEN p_retention IS NULL THEN NULL
                      ELSE kingres.timepart__add(v_asof, p_retention, v_tz, -1) END;
    v_thr_col := CASE WHEN p_columnar_after IS NULL THEN NULL
                      ELSE kingres.timepart__add(v_asof, p_columnar_after, v_tz, -1) END;

    -- Partitions whose bounds are not on the grid are reported and left alone.
    FOR r IN SELECT * FROM kingres.timepart__misaligned(p_parent, p_every, v_tz) c
    LOOP
        v_seq := v_seq + 1;
        seq := v_seq; kind := 'misaligned';
        part_oid := r.part_oid; part_schema := r.part_schema;
        part_name := r.part_name;
        range_from := r.lo; range_to := r.hi;
        reason := 'bounds are not on the grid of the policy';
        RETURN NEXT;
    END LOOP;

    -- Partitions to create.
    SELECT min(c.lo) INTO v_min_lo
      FROM kingres.timepart__children(p_parent) c
     WHERE NOT c.is_default AND c.lo > '-infinity';
    v_cur_k := kingres.timepart_cell_of(p_every, v_tz, v_asof);
    v_first_k := v_cur_k;
    IF v_min_lo IS NOT NULL THEN
        v_first_k := least(v_cur_k,
                           kingres.timepart_cell_of(p_every, v_tz, v_min_lo));
    END IF;
    IF v_thr_ret IS NOT NULL THEN
        v_first_k := greatest(v_first_k,
                              kingres.timepart_cell_of(p_every, v_tz, v_thr_ret));
    END IF;
    v_last_k := greatest(
        v_cur_k,
        kingres.timepart_cell_of(
            p_every, v_tz,
            kingres.timepart__add(v_asof, p_ahead, v_tz, 1)
            - interval '1 microsecond'));
    FOR r IN SELECT * FROM kingres.timepart__create_rows(
                 p_parent, p_every, v_tz, v_first_k, v_last_k) c
    LOOP
        v_seq := v_seq + 1;
        seq := v_seq; kind := 'create';
        part_oid := NULL; part_schema := r.part_schema;
        part_name := r.part_name;
        range_from := r.lo; range_to := r.hi;
        reason := 'missing partition';
        RETURN NEXT;
    END LOOP;

    -- Partitions to convert.
    IF v_thr_col IS NOT NULL THEN
        -- the storage of the leaves of a distributed table is read on the nodes
        -- (a partition is columnar when every leaf is; mixed while a
        -- conversion is half done, and then converted again)
        IF v_dist THEN
            SELECT coalesce(jsonb_object_agg(k.partition_name, k.kind), '{}')
              INTO v_kinds FROM kingres.timepart__dist_kinds(p_parent) k;
        END IF;
        FOR r IN SELECT * FROM kingres.timepart__children(p_parent) c
                  WHERE NOT c.is_default AND c.hi < 'infinity'
                    AND c.hi <= v_thr_col
                    AND kingres.timepart__on_grid(p_every, v_tz, c.lo)
                    AND kingres.timepart__on_grid(p_every, v_tz, c.hi)
                    AND coalesce(v_kinds ->> c.part_name::text, c.am_name::text)
                        IS DISTINCT FROM 'columnar'
                    AND NOT (v_thr_ret IS NOT NULL AND c.hi <= v_thr_ret)
                  ORDER BY c.hi
        LOOP
            v_seq := v_seq + 1;
            seq := v_seq; kind := 'convert';
            part_oid := r.part_oid; part_schema := r.part_schema;
            part_name := r.part_name;
            range_from := r.lo; range_to := r.hi;
            reason := 'older than columnar_after';
            RETURN NEXT;
        END LOOP;
    END IF;

    -- Partitions to retire.
    IF v_thr_ret IS NOT NULL THEN
        FOR r IN SELECT * FROM kingres.timepart__children(p_parent) c
                  WHERE NOT c.is_default AND c.hi < 'infinity'
                    AND c.hi <= v_thr_ret
                    AND kingres.timepart__on_grid(p_every, v_tz, c.lo)
                    AND kingres.timepart__on_grid(p_every, v_tz, c.hi)
                  ORDER BY c.hi
        LOOP
            v_seq := v_seq + 1;
            seq := v_seq;
            kind := CASE WHEN p_retention_action = 'detach' THEN 'detach'
                         ELSE 'drop' END;
            part_oid := r.part_oid; part_schema := r.part_schema;
            part_name := r.part_name;
            range_from := r.lo; range_to := r.hi;
            reason := 'older than retention';
            RETURN NEXT;
        END LOOP;
    END IF;
END;
$$;

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
    v_dist    boolean;
    v_bound   text;
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
    -- A distributed table is changed with the partition functions of
    -- kingres_shard, on every node: the caller must be a superuser.
    v_dist := (SELECT d.r_dist FROM kingres.timepart__dist(v_s.o_parent) d);
    IF v_dist AND NOT v_super THEN
        PERFORM kingres.timepart__step_end(
            p_run, v_s.o_kind, v_s.o_part_schema, v_s.o_part_name, v_s.o_from,
            v_s.o_to, 'failed',
            'permission denied: the partitions of a distributed table are changed by a superuser',
            v_s.o_bytes, v_self, v_super);
        RETURN true;
    END IF;
    -- A conversion copies a whole partition: it is tried once in a run (a
    -- lock that is not granted in time is tried again by the next run, not
    -- by a second copy).
    v_tries := CASE WHEN v_s.o_kind = 'convert' THEN 1 ELSE v_s.o_retries END;
    FOR v_attempt IN 1..v_tries LOOP
        BEGIN
            -- The statements and the lock timeout are prepared here; only the
            -- DDL runs as the table's owner.
            IF v_dist THEN
                -- the operations of kingres_shard (map group primary): each is one
                -- two-phase transaction on every node, refused with object_in_use
                -- while a bucket move or a table operation claims the group
                PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
                IF v_s.o_kind = 'create' THEN
                    v_bound := format('FROM (%L) TO (%L)',
                        kingres.timepart__bound_literal(v_s.o_keytype, v_s.o_from),
                        kingres.timepart__bound_literal(v_s.o_keytype, v_s.o_to));
                    PERFORM kingres.create_partition(v_s.o_parent::regclass,
                                                     v_s.o_part_name::text, v_bound, true);
                ELSIF v_s.o_kind = 'convert' THEN
                    SELECT * INTO v_info FROM kingres.timepart__key_info(v_s.o_parent);
                    SELECT p.columnar_sort, p.columnar_options INTO v_sort, v_opts
                      FROM kingres.timepart_policy p
                     WHERE p.schema_name = v_info.r_schema
                       AND p.table_name = v_info.r_table;
                    v_detail := kingres.timepart__dist_convert(
                        v_s.o_parent, v_s.o_part_name, 'columnar'::name, v_sort, v_opts);
                ELSIF v_s.o_kind = 'detach' THEN
                    PERFORM kingres.detach_partition(v_s.o_parent::regclass, v_s.o_part_name::text);
                ELSE
                    PERFORM kingres.drop_partition(v_s.o_parent::regclass, v_s.o_part_name::text, true);
                END IF;
                v_state := 'done';
                v_detail := CASE WHEN v_s.o_kind = 'convert' THEN 'converted to columnar: ' || v_detail END;
                EXIT;
            END IF;
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
            WHEN object_in_use THEN
                -- a colocation group that a bucket move, a table operation or a
                -- conversion uses: retry_later.  The action is tried again with
                -- a growing wait, and a run that still finds it busy is partial
                -- and comes back within five minutes.
                v_state := 'failed';
                v_detail := 'lock_timeout: retry_later: ' || SQLERRM
                            || format(' (attempt %s of %s)', v_attempt, v_tries);
            WHEN lock_not_available THEN
                v_state := 'failed';
                IF v_s.o_kind = 'convert' AND (SQLERRM LIKE 'swap_lock_timeout:%'
                                               OR SQLERRM LIKE 'schema_changed:%') THEN
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
            -- a distributed table waits longer at each attempt (the group is
            -- busy for as long as a move or a conversion takes)
            PERFORM pg_sleep(least(60000, (v_s.o_sleep_min + random() * (v_s.o_sleep_max - v_s.o_sleep_min))
                                          * CASE WHEN v_dist THEN power(2, v_attempt - 1) ELSE 1 END) / 1000.0);
        END IF;
    END LOOP;

    PERFORM kingres.timepart__step_end(
        p_run, v_s.o_kind, v_s.o_part_schema, v_s.o_part_name, v_s.o_from,
        v_s.o_to, v_state, v_detail, v_s.o_bytes, v_self, v_super);
    RETURN true;
END;
$$;

CREATE OR REPLACE FUNCTION kingres.timepart_set_policy(
    rel regclass, partition_every interval,
    create_ahead interval DEFAULT '7 days',
    retention interval DEFAULT NULL,
    retention_action text DEFAULT 'drop',
    columnar_after interval DEFAULT NULL,
    columnar_sort text[] DEFAULT NULL,
    columnar_options jsonb DEFAULT NULL,
    timezone text DEFAULT 'UTC',
    run_every interval DEFAULT '1 hour',
    enabled boolean DEFAULT true)
RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_info   record;
BEGIN
    SELECT * INTO v_info FROM kingres.timepart__key_info(rel::oid);
    PERFORM kingres.timepart__require_owner(
        current_user, v_info.r_owner_oid,
        format('%I.%I', v_info.r_schema, v_info.r_table));
    IF v_info.r_default IS NOT NULL THEN
        RAISE EXCEPTION 'table %.% has a default partition "%"',
            quote_ident(v_info.r_schema), quote_ident(v_info.r_table),
            v_info.r_default
            USING ERRCODE = '0A000',
                  HINT = 'ATTACH PARTITION scans the default partition under a strong lock, and DETACH ... CONCURRENTLY is refused; create partitions far enough ahead instead.';
    END IF;
    PERFORM kingres.timepart__validate_policy(
        v_info.r_keytype, partition_every, timezone, create_ahead, retention,
        retention_action, columnar_after, columnar_sort, columnar_options,
        run_every);
    -- a distributed table has a leaf for every bucket of every partition
    PERFORM kingres.timepart__dist_budget(rel::oid, partition_every, create_ahead,
                                          retention);

    -- A role writes through the view, and the trigger pins what it must not
    -- choose (the owner, the stamp, who made the policy).  A superuser, which
    -- the trigger trusts, writes the table itself, with the same values.
    IF EXISTS (SELECT 1 FROM pg_roles r
                WHERE r.rolname = current_user AND r.rolsuper) THEN
        -- the one write of a superuser that asks for a seal: this function is
        -- the documented interface
        PERFORM kingres.timepart__mark_seal(v_info.r_schema, v_info.r_table);
        INSERT INTO kingres.timepart_policy_data AS p (
            schema_name, table_name, parent_oid, partition_every, timezone,
            create_ahead, retention, retention_action, columnar_after,
            columnar_sort, columnar_options, run_every, enabled, table_owner,
            table_owner_oid, created_by, created_by_oid, stamp)
        VALUES (
            v_info.r_schema, v_info.r_table, rel::oid, partition_every, timezone,
            create_ahead, retention, retention_action, columnar_after,
            columnar_sort, columnar_options, run_every, enabled, v_info.r_owner,
            v_info.r_owner_oid, current_user,
            (SELECT r.oid FROM pg_roles r WHERE r.rolname = current_user),
            kingres.timepart__stamp())
        ON CONFLICT (schema_name, table_name) DO UPDATE SET
            parent_oid = EXCLUDED.parent_oid,
            partition_every = EXCLUDED.partition_every,
            timezone = EXCLUDED.timezone,
            create_ahead = EXCLUDED.create_ahead,
            retention = EXCLUDED.retention,
            retention_action = EXCLUDED.retention_action,
            columnar_after = EXCLUDED.columnar_after,
            columnar_sort = EXCLUDED.columnar_sort,
            columnar_options = EXCLUDED.columnar_options,
            run_every = EXCLUDED.run_every,
            enabled = EXCLUDED.enabled,
            table_owner = EXCLUDED.table_owner,
            table_owner_oid = EXCLUDED.table_owner_oid,
            stamp = EXCLUDED.stamp,
            updated_at = clock_timestamp();
    ELSE
        INSERT INTO kingres.timepart_policy AS p (
            schema_name, table_name, parent_oid, partition_every, timezone,
            create_ahead, retention, retention_action, columnar_after,
            columnar_sort, columnar_options, run_every, enabled)
        VALUES (
            v_info.r_schema, v_info.r_table, rel::oid, partition_every, timezone,
            create_ahead, retention, retention_action, columnar_after,
            columnar_sort, columnar_options, run_every, enabled)
        ON CONFLICT (schema_name, table_name) DO UPDATE SET
            parent_oid = EXCLUDED.parent_oid,
            partition_every = EXCLUDED.partition_every,
            timezone = EXCLUDED.timezone,
            create_ahead = EXCLUDED.create_ahead,
            retention = EXCLUDED.retention,
            retention_action = EXCLUDED.retention_action,
            columnar_after = EXCLUDED.columnar_after,
            columnar_sort = EXCLUDED.columnar_sort,
            columnar_options = EXCLUDED.columnar_options,
            run_every = EXCLUDED.run_every,
            enabled = EXCLUDED.enabled,
            updated_at = clock_timestamp();
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION kingres.create_time_partitions(
    rel regclass, partition_every interval, end_at timestamptz,
    start_from timestamptz DEFAULT now(), access_method name DEFAULT NULL,
    timezone text DEFAULT 'UTC')
RETURNS integer
LANGUAGE plpgsql
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_info   record;
    v_parts  record;
    v_tz     text;
    v_first  bigint;
    v_last   bigint;
    v_n      integer := 0;
    v_am     name := access_method;
    r        record;
BEGIN
    -- Reading the bounds of the partitions takes a lock on them, so the wait
    -- is bounded from the first statement on.
    PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
    SELECT * INTO v_info FROM kingres.timepart__key_info(rel::oid);
    PERFORM kingres.timepart__require_owner(
        current_user, v_info.r_owner_oid,
        format('%I.%I', v_info.r_schema, v_info.r_table));
    IF v_info.r_default IS NOT NULL THEN
        RAISE EXCEPTION 'table %.% has a default partition "%"',
            quote_ident(v_info.r_schema), quote_ident(v_info.r_table),
            v_info.r_default
            USING ERRCODE = '0A000',
                  HINT = 'ATTACH PARTITION scans the default partition under a strong lock; create partitions far enough ahead instead.';
    END IF;
    PERFORM kingres.timepart__check_tz(timezone);
    SELECT * INTO v_parts FROM kingres.timepart_interval_parts(partition_every);
    IF v_info.r_keytype = 'date' AND v_parts.unit = 'hour' THEN
        RAISE EXCEPTION 'partition_every in hours needs a timestamp or timestamptz column, not date'
            USING ERRCODE = '22023';
    END IF;
    IF end_at IS NULL OR start_from IS NULL
       OR end_at IN ('infinity', '-infinity')
       OR start_from IN ('infinity', '-infinity') THEN
        RAISE EXCEPTION 'start_from and end_at must be finite times'
            USING ERRCODE = '22008';
    END IF;
    v_tz := CASE WHEN v_info.r_keytype = 'timestamptz' THEN timezone ELSE 'UTC' END;
    v_first := kingres.timepart_cell_of(
        partition_every, v_tz,
        kingres.timepart__eff(start_from, v_info.r_keytype, timezone));
    v_last := kingres.timepart_cell_of(
        partition_every, v_tz,
        kingres.timepart__eff(end_at, v_info.r_keytype, timezone));
    IF v_last < v_first THEN
        RAISE EXCEPTION 'end_at is before start_from' USING ERRCODE = '22023';
    END IF;
    IF v_am IS NULL THEN
        SELECT a.amname INTO v_am FROM pg_am a WHERE a.oid = v_info.r_am;
    END IF;
    IF (SELECT d.r_dist FROM kingres.timepart__dist(rel::oid) d) THEN
        -- a distributed table: one leaf per bucket on every node, heap
        IF access_method IS NOT NULL AND access_method <> 'heap' THEN
            RAISE EXCEPTION 'the partitions of a distributed table are created as heap'
                USING ERRCODE = '0A000',
                      HINT = 'Convert them with convert_distributed_partition.';
        END IF;
        FOR r IN SELECT * FROM kingres.timepart__create_rows(
                     rel::oid, partition_every, v_tz, v_first, v_last) c
        LOOP
            PERFORM kingres.create_partition(
                rel, r.part_name::text,
                format('FROM (%L) TO (%L)',
                       kingres.timepart__bound_literal(v_info.r_keytype, r.lo),
                       kingres.timepart__bound_literal(v_info.r_keytype, r.hi)),
                false);
            v_n := v_n + 1;
        END LOOP;
        RETURN v_n;
    END IF;
    FOR r IN SELECT * FROM kingres.timepart__create_rows(
                 rel::oid, partition_every, v_tz, v_first, v_last) c
    LOOP
        PERFORM kingres.timepart_create_one(
            rel::oid, r.part_schema, r.part_name,
            kingres.timepart__bound_literal(v_info.r_keytype, r.lo),
            kingres.timepart__bound_literal(v_info.r_keytype, r.hi), v_am);
        v_n := v_n + 1;
    END LOOP;
    RETURN v_n;
END;
$$;

CREATE OR REPLACE FUNCTION kingres.drop_old_time_partitions(
    rel regclass, older_than timestamptz, detach_only boolean DEFAULT false,
    timezone text DEFAULT 'UTC')
RETURNS integer
LANGUAGE plpgsql
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_info   record;
    v_limit  timestamptz;
    v_n      integer := 0;
    r        record;
BEGIN
    PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('retire'), true);
    SELECT * INTO v_info FROM kingres.timepart__key_info(rel::oid);
    PERFORM kingres.timepart__require_owner(
        current_user, v_info.r_owner_oid,
        format('%I.%I', v_info.r_schema, v_info.r_table));
    PERFORM kingres.timepart__check_tz(timezone);
    IF older_than IS NULL OR older_than IN ('infinity', '-infinity') THEN
        RAISE EXCEPTION 'older_than must be a finite time' USING ERRCODE = '22008';
    END IF;
    v_limit := kingres.timepart__eff(older_than, v_info.r_keytype, timezone);
    FOR r IN SELECT * FROM kingres.timepart__children(rel::oid) c
              WHERE NOT c.is_default AND c.hi < 'infinity' AND c.hi <= v_limit
              ORDER BY c.hi
    LOOP
        IF r.part_oid IS NULL THEN
            -- a partition of a distributed table
            IF coalesce(detach_only, false) THEN
                PERFORM kingres.detach_partition(rel, r.part_name::text);
            ELSE
                PERFORM kingres.drop_partition(rel, r.part_name::text);
            END IF;
        ELSE
            PERFORM kingres.timepart_retire_one(rel::oid, r.part_oid,
                                                coalesce(detach_only, false));
        END IF;
        v_n := v_n + 1;
    END LOOP;
    RETURN v_n;
END;
$$;

CREATE OR REPLACE FUNCTION kingres.timepart_partitions(rel regclass)
RETURNS TABLE (partition regclass, range_from timestamptz,
               range_to timestamptz, access_method name, size_bytes bigint,
               row_estimate bigint, compression_ratio float8,
               attached boolean)
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_info   record;
BEGIN
    -- Reading the bounds of the partitions takes locks: bound the wait, so that
    -- a long transaction on the table cannot hold a monitor up.
    PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
    SELECT * INTO v_info FROM kingres.timepart__key_info(rel::oid);
    IF (SELECT d.r_dist FROM kingres.timepart__dist(rel::oid) d) THEN
        RAISE EXCEPTION '%.% is a distributed table: its partitions are not tables of this database',
            quote_ident(v_info.r_schema), quote_ident(v_info.r_table)
            USING ERRCODE = '42809',
                  HINT = 'Use timepart_dist_partitions.';
    END IF;
    IF NOT (pg_has_role(current_user, v_info.r_owner_oid, 'USAGE')
            OR has_table_privilege(current_user, rel::oid, 'SELECT')) THEN
        RAISE EXCEPTION 'permission denied for table %',
            format('%I.%I', v_info.r_schema, v_info.r_table)
            USING ERRCODE = '42501';
    END IF;
    RETURN QUERY
    SELECT format('%I.%I', c.part_schema, c.part_name)::regclass,
           c.lo, c.hi, c.am_name,
           pg_total_relation_size(c.part_oid),
           (SELECT CASE WHEN cl.reltuples < 0 THEN NULL
                        ELSE cl.reltuples::bigint END
              FROM pg_class cl WHERE cl.oid = c.part_oid),
           NULL::float8, true
      FROM kingres.timepart__children(rel::oid) c
     WHERE NOT c.is_default
     ORDER BY c.lo;
    RETURN QUERY
    SELECT DISTINCT ON (a.partition_schema, a.partition_name)
           kingres.timepart__oid(a.partition_schema, a.partition_name)::regclass,
           a.range_from, a.range_to,
           (SELECT am.amname FROM pg_class cl JOIN pg_am am ON am.oid = cl.relam
             WHERE cl.oid = kingres.timepart__oid(a.partition_schema, a.partition_name)),
           pg_total_relation_size(kingres.timepart__oid(a.partition_schema, a.partition_name)),
           (SELECT CASE WHEN cl.reltuples < 0 THEN NULL
                        ELSE cl.reltuples::bigint END
              FROM pg_class cl
             WHERE cl.oid = kingres.timepart__oid(a.partition_schema, a.partition_name)),
           NULL::float8, false
      FROM kingres.timepart_run_action a
      JOIN kingres.timepart_run r ON r.run_id = a.run_id
     WHERE r.table_oid = rel::oid
       AND a.kind = 'detach' AND a.state = 'done'
       AND kingres.timepart__oid(a.partition_schema, a.partition_name) IS NOT NULL
       AND has_table_privilege(current_user,
                               kingres.timepart__oid(a.partition_schema, a.partition_name)::oid, 'SELECT')
       AND NOT EXISTS (
           SELECT 1 FROM pg_inherits i
            WHERE i.inhrelid = kingres.timepart__oid(a.partition_schema, a.partition_name)::oid)
     ORDER BY a.partition_schema, a.partition_name, a.run_id DESC;
END;
$$;

CREATE OR REPLACE FUNCTION kingres.timepart__convert_note(
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
        -- the leaves of a distributed partition are analyzed on their nodes
        IF v_oid IS NULL AND v_run.table_oid IS NOT NULL
           AND (SELECT d.r_dist FROM kingres.timepart__dist(v_run.table_oid) d) THEN
            UPDATE kingres.timepart_run_action_data a
               SET bytes_after = 0
             WHERE a.run_id = v_a.run_id AND a.seq = v_a.seq;
            CONTINUE;
        END IF;
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

DROP FUNCTION kingres.timepart__convert_stmts(oid, oid, name, text[], jsonb);
CREATE FUNCTION kingres.timepart__convert_stmts(
    p_parent oid, p_part oid, p_target name, p_sort text[], p_options jsonb,
    p_dist boolean DEFAULT false)
RETURNS text[]
LANGUAGE plpgsql
SET search_path = pg_catalog, kingres, pg_temp
SET DateStyle = 'ISO, MDY'
AS $$
DECLARE
    v_plan    jsonb;
    v_kind    text;
    v_opts    record;
    v_swap    integer;
    v_dl      integer;
    v_body    text;
BEGIN
    -- The checks and the plan, before any lock: a refusal is raised here, with
    -- nothing locked.  The block reads them again under its lock.
    v_plan := kingres.timepart__convert_plan(p_parent, p_part, p_target, p_sort, p_options);
    v_kind := kingres.timepart__am_kind(p_target);
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
    v_am       text := %1$L;
    v_sort     text[] := %2$L;
    v_options  jsonb := %3$L;
    v_o_comp   text := %4$L;
    v_o_level  integer := %5$L;
    v_o_stripe integer := %6$L;
    v_o_chunk  integer := %7$L;
    v_swap     integer := %8$s;
    v_columnar boolean := %9$s;
    v_part_oid oid := %10$s;
    v_parent_oid oid := %11$s;
    v_dist     boolean := %12$s;
    v_prev_lt  text := pg_catalog.current_setting('lock_timeout');
    v_plan     jsonb;
    v_parent   text;
    v_part     text;
    v_new      text;
    v_schema   text;
    v_old_name text;
    v_cols     text;
    v_order    text;
    v_bound_cl text;
    v_bound    text;
    v_check    text;
    v_tblspc   text;
    v_ix       jsonb;
    v_fp       jsonb;
    v_dts      text := pg_catalog.current_setting('default_tablespace');
    v_trg      jsonb;
    v_final    text;
    v_owner    oid;
    v_old_cnt  bigint;
    v_new_cnt  bigint;
    v_text     text;
    v_k        text;
    v_v        text;
    v_ident    "char";
    i          integer;
    r          record;
BEGIN
    -- a partition that is gone: the cast of its OID to a name would give the
    -- digits, and LOCK TABLE of them is a syntax error
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c WHERE c.oid = v_part_oid) THEN
        RAISE EXCEPTION 'schema_changed: the partition with OID %% was dropped before the conversion could lock it; the next run tries again',
            v_part_oid USING ERRCODE = '55P03';
    END IF;
    BEGIN
        EXECUTE 'LOCK TABLE ' || v_part_oid::pg_catalog.regclass::pg_catalog.text
             || ' IN SHARE ROW EXCLUSIVE MODE';
    EXCEPTION WHEN undefined_table OR syntax_error THEN
        RAISE EXCEPTION 'schema_changed: the partition with OID %% was renamed or dropped while the conversion waited for its lock; the next run tries again',
            v_part_oid USING ERRCODE = '55P03';
    END;
    -- the lock that is held is the one on the partition, not on a table that
    -- took its name while this statement waited
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_locks l
                    WHERE l.pid = pg_catalog.pg_backend_pid() AND l.locktype = 'relation'
                      AND l.database = (SELECT d.oid FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database())
                      AND l.relation = v_part_oid AND l.mode = 'ShareRowExclusiveLock' AND l.granted) THEN
        RAISE EXCEPTION 'schema_changed: the partition with OID %% was renamed or dropped while the conversion waited for its lock, and its name is another table now; the next run tries again',
            v_part_oid USING ERRCODE = '55P03';
    END IF;
    -- everything the catalog says about the partition, now that nothing can
    -- change on it; a refusal or an error here is raised as it would be above
    v_plan := kingres.timepart__convert_plan(v_parent_oid, v_part_oid, v_am::pg_catalog.name, v_sort, v_options);
    v_parent   := v_plan ->> 'parent';
    v_part     := v_plan ->> 'part';
    v_new      := v_plan ->> 'newq';
    v_schema   := v_plan ->> 'schema';
    v_old_name := v_plan ->> 'old';
    v_cols     := v_plan ->> 'cols';
    v_order    := v_plan ->> 'order';
    v_bound_cl := v_plan ->> 'bound_cl';
    v_bound    := v_plan ->> 'bound';
    v_check    := v_plan ->> 'check';
    v_tblspc   := v_plan ->> 'tblspc';
    v_ix       := v_plan -> 'ix';
    v_fp       := v_plan -> 'fp';

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
    -- a columnar leaf of a distributed table has REPLICA IDENTITY FULL: the logical
    -- replication of a bucket move decodes its inserts with it
    IF v_ident = 'f' OR (v_columnar AND v_dist) THEN
        EXECUTE 'ALTER TABLE ' || v_new || ' REPLICA IDENTITY FULL';
    ELSIF v_ident = 'n' THEN
        EXECUTE 'ALTER TABLE ' || v_new || ' REPLICA IDENTITY NOTHING';
    END IF;
    -- The statistics target and the options of each column, which LIKE does not
    -- carry (the storage parameters of the table follow the indexes, below).
    FOR r IN SELECT a.attname, a.attstattarget, a.attoptions
               FROM pg_catalog.pg_attribute a
              WHERE a.attrelid = v_part_oid AND a.attnum > 0 AND NOT a.attisdropped
              ORDER BY a.attnum
    LOOP
        IF r.attstattarget IS NOT NULL AND r.attstattarget <> -1 THEN
            BEGIN
                EXECUTE 'ALTER TABLE ' || v_new || ' ALTER COLUMN ' || pg_catalog.quote_ident(r.attname)
                     || ' SET STATISTICS ' || r.attstattarget::pg_catalog.text;
            EXCEPTION WHEN OTHERS THEN
                RAISE WARNING 'the statistics target of the column %% of %% was not carried to the new table: %%',
                    r.attname, v_part, SQLERRM;
            END;
        END IF;
        IF r.attoptions IS NOT NULL THEN
            FOR i IN 1 .. pg_catalog.array_length(r.attoptions, 1) LOOP
                v_k := pg_catalog.split_part(r.attoptions[i], '=', 1);
                v_v := pg_catalog.substr(r.attoptions[i], pg_catalog.strpos(r.attoptions[i], '=') + 1);
                -- the version stamp of the column is the server's own, and
                -- the new column has its own
                CONTINUE WHEN v_k = 'pg_attver';
                BEGIN
                    EXECUTE 'ALTER TABLE ' || v_new || ' ALTER COLUMN ' || pg_catalog.quote_ident(r.attname)
                         || ' SET (' || pg_catalog.quote_ident(v_k) || ' = ' || pg_catalog.quote_literal(v_v) || ')';
                EXCEPTION WHEN OTHERS THEN
                    RAISE WARNING 'the option %% of the column %% of %% was not carried to the new table: %%',
                        r.attoptions[i], r.attname, v_part, SQLERRM;
                END;
            END LOOP;
        END IF;
    END LOOP;

    EXECUTE 'INSERT INTO ' || v_new || ' (' || v_cols || ') SELECT ' || v_cols
         || ' FROM ' || v_part || v_order;

    EXECUTE 'SELECT pg_catalog.count(*) FROM ' || v_part INTO v_old_cnt;
    EXECUTE 'SELECT pg_catalog.count(*) FROM ' || v_new INTO v_new_cnt;
    IF v_old_cnt IS DISTINCT FROM v_new_cnt THEN
        RAISE EXCEPTION 'verification_failed: %% has %% rows, its copy has %%',
            v_part, v_old_cnt, v_new_cnt USING ERRCODE = 'KT001';
    END IF;

    -- the indexes, after the load, with plain CREATE INDEX (under the lock of
    -- the partition, so the rows cannot change); a duplicate fails here
    FOR r IN SELECT x ->> 'create' AS stmt, x ->> 'spc' AS spc
               FROM pg_catalog.jsonb_array_elements(v_ix) WITH ORDINALITY t(x, n) ORDER BY n
    LOOP
        PERFORM pg_catalog.set_config('default_tablespace', coalesce(r.spc, v_dts), true);
        EXECUTE r.stmt;
    END LOOP;
    PERFORM pg_catalog.set_config('default_tablespace', v_dts, true);

    -- The storage parameters of the table (and of its TOAST table), which LIKE
    -- does not carry, after the load and the indexes: parallel_workers would
    -- have a columnar table refuse a parallel index build of rows that the
    -- transaction has not written yet.  The target access method may not
    -- accept one of them: that one is a warning, the rest are carried.
    FOR r IN SELECT o.o AS opt
               FROM pg_catalog.pg_class c, LATERAL pg_catalog.unnest(c.reloptions) o(o)
              WHERE c.oid = v_part_oid
    LOOP
        v_k := pg_catalog.split_part(r.opt, '=', 1);
        v_v := pg_catalog.substr(r.opt, pg_catalog.strpos(r.opt, '=') + 1);
        BEGIN
            EXECUTE 'ALTER TABLE ' || v_new || ' SET ('
                 || CASE WHEN pg_catalog.strpos(v_k, '.') > 0
                         THEN pg_catalog.quote_ident(pg_catalog.split_part(v_k, '.', 1)) || '.'
                              || pg_catalog.quote_ident(pg_catalog.split_part(v_k, '.', 2))
                         ELSE pg_catalog.quote_ident(v_k) END
                 || ' = ' || pg_catalog.quote_literal(v_v) || ')';
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'the storage parameter %% of %% was not carried to the new table: %%',
                r.opt, v_part, SQLERRM;
        END;
    END LOOP;
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
    -- the privileges that the owner took from itself stay taken
    FOR r IN SELECT d.privilege_type
               FROM pg_catalog.pg_class c,
                    LATERAL pg_catalog.aclexplode(pg_catalog.acldefault('r', c.relowner)) d
              WHERE c.oid = v_part_oid AND c.relacl IS NOT NULL AND d.grantee = c.relowner
                AND NOT EXISTS (SELECT 1 FROM pg_catalog.aclexplode(c.relacl) x
                                 WHERE x.grantee = c.relowner AND x.privilege_type = d.privilege_type)
    LOOP
        EXECUTE 'REVOKE ' || r.privilege_type || ' ON TABLE ' || v_new || ' FROM '
             || pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(v_owner));
    END LOOP;

    -- the enabled state of the triggers that the partition has from the
    -- table: the attach clones them enabled, and the old state is put back below
    SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('name', t.tgname, 'en', t.tgenabled::pg_catalog.text)
                                ORDER BY t.tgname) INTO v_trg
      FROM pg_catalog.pg_trigger t
     WHERE t.tgrelid = v_part_oid AND NOT t.tgisinternal AND t.tgparentid <> 0
       AND t.tgenabled <> 'O';

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
    -- What the partition carries and a change does not need the table's lock
    -- for (privileges, names, comments and options of the indexes) is read
    -- again, now that the parent is locked: a change that committed during the
    -- copy or the wait would be dropped with the old partition, so the
    -- conversion gives way and the next run tries again.
    IF kingres.timepart__convert_fp(v_part_oid) IS DISTINCT FROM v_fp THEN
        RAISE EXCEPTION 'schema_changed: the privileges, or the names, comments or options of the indexes, of %% were changed while the conversion copied it; the copy was discarded and the next run tries again',
            v_part USING ERRCODE = '55P03';
    END IF;
    EXECUTE 'ALTER TABLE ' || v_parent || ' DETACH PARTITION ' || v_part;
    EXECUTE 'ALTER TABLE ' || v_parent || ' ATTACH PARTITION ' || v_new || ' ' || v_bound;
    EXECUTE 'DROP TABLE ' || v_part;
    EXECUTE 'ALTER TABLE ' || v_new || ' RENAME TO ' || pg_catalog.quote_ident(v_old_name);
    v_final := pg_catalog.quote_ident(v_schema) || '.' || pg_catalog.quote_ident(v_old_name);
    IF v_bound_cl <> '' THEN
        EXECUTE 'ALTER TABLE ' || v_final
             || ' DROP CONSTRAINT ' || pg_catalog.quote_ident(v_check);
    END IF;
    -- the names, and the comments, of the indexes and constraints
    FOR r IN SELECT x FROM pg_catalog.jsonb_array_elements(v_ix) t(x)
    LOOP
        IF r.x ->> 'con' IS NOT NULL THEN
            EXECUTE 'ALTER TABLE ' || v_final || ' RENAME CONSTRAINT '
                 || pg_catalog.quote_ident(r.x ->> 'tmp') || ' TO ' || pg_catalog.quote_ident(r.x ->> 'con');
            IF r.x ->> 'cdesc' IS NOT NULL THEN
                EXECUTE 'COMMENT ON CONSTRAINT ' || pg_catalog.quote_ident(r.x ->> 'con') || ' ON '
                     || v_final || ' IS ' || pg_catalog.quote_literal(r.x ->> 'cdesc');
            END IF;
        ELSE
            EXECUTE 'ALTER INDEX ' || pg_catalog.quote_ident(v_schema) || '.'
                 || pg_catalog.quote_ident(r.x ->> 'tmp') || ' RENAME TO ' || pg_catalog.quote_ident(r.x ->> 'name');
        END IF;
        IF r.x ->> 'idesc' IS NOT NULL THEN
            EXECUTE 'COMMENT ON INDEX ' || pg_catalog.quote_ident(v_schema) || '.'
                 || pg_catalog.quote_ident(coalesce(r.x ->> 'con', r.x ->> 'name'))
                 || ' IS ' || pg_catalog.quote_literal(r.x ->> 'idesc');
        END IF;
        -- the statistics targets of the expression columns of the index
        FOR i IN 0 .. pg_catalog.jsonb_array_length(r.x -> 'stats') - 1 LOOP
            BEGIN
                EXECUTE 'ALTER INDEX ' || pg_catalog.quote_ident(v_schema) || '.'
                     || pg_catalog.quote_ident(coalesce(r.x ->> 'con', r.x ->> 'name'))
                     || ' ALTER COLUMN ' || ((r.x -> 'stats' -> i) ->> 'n')
                     || ' SET STATISTICS ' || ((r.x -> 'stats' -> i) ->> 't');
            EXCEPTION WHEN OTHERS THEN
                RAISE WARNING 'a statistics target of the index %% was not carried to the new table: %%',
                    coalesce(r.x ->> 'con', r.x ->> 'name'), SQLERRM;
            END;
        END LOOP;
        -- the index that the table is clustered on
        IF (r.x ->> 'cluster')::pg_catalog.bool THEN
            BEGIN
                EXECUTE 'ALTER TABLE ' || v_final || ' CLUSTER ON '
                     || pg_catalog.quote_ident(coalesce(r.x ->> 'con', r.x ->> 'name'));
            EXCEPTION WHEN OTHERS THEN
                RAISE WARNING 'the clustering index %% was not carried to the new table: %%',
                    coalesce(r.x ->> 'con', r.x ->> 'name'), SQLERRM;
            END;
        END IF;
    END LOOP;
    -- the enabled state of the cloned triggers
    FOR r IN SELECT x FROM pg_catalog.jsonb_array_elements(coalesce(v_trg, '[]'::pg_catalog.jsonb)) t(x)
    LOOP
        EXECUTE 'ALTER TABLE ' || v_final
             || CASE r.x ->> 'en' WHEN 'D' THEN ' DISABLE TRIGGER '
                                  WHEN 'R' THEN ' ENABLE REPLICA TRIGGER '
                                  ELSE ' ENABLE ALWAYS TRIGGER ' END
             || pg_catalog.quote_ident(r.x ->> 'name');
    END LOOP;
    PERFORM pg_catalog.set_config('lock_timeout', v_prev_lt, true);
END
$tpl$,
        p_target, p_sort::text, p_options::text, v_opts.o_compression, v_opts.o_level,
        v_opts.o_stripe, v_opts.o_chunk, v_swap, (v_kind = 'columnar')::text,
        p_part::oid, p_parent::oid, p_dist::text);
    RETURN ARRAY[format('DO %L', v_body)];
END;
$$;


------------------------------------------------------------------------
-- Distributed tables (increments St-D1a and St-D1b, section 11)
------------------------------------------------------------------------
--
-- A user-partitioned distributed table of kingres_shard (hash on top, the user's range partitions below) has a
-- policy like any other table.  Its partitions are logical objects, rows of kingres.dist_partition with one leaf per
-- bucket on the node that owns the bucket; a run creates, retires and converts them with the partition functions of
-- kingres_shard, on the map group primary, as a superuser.  Nothing here needs kingres_shard to be installed: a
-- database without it has no distributed table.

-- Is the table a range-partitioned distributed table of kingres_shard?  The partition key is as pg_get_partkeydef
-- gives it, without the method.
CREATE FUNCTION kingres.timepart__dist(
    p_rel oid,
    OUT r_dist boolean, OUT r_method text, OUT r_key text,
    OUT r_colocation integer)
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
BEGIN
    r_dist := false;
    IF pg_catalog.to_regclass('kingres.dist_table') IS NULL
       OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
                       WHERE a.attrelid = pg_catalog.to_regclass('kingres.dist_table')
                         AND a.attname = 'partition_method' AND NOT a.attisdropped) THEN
        RETURN;
    END IF;
    EXECUTE 'SELECT true, d.partition_method, d.partition_key, d.colocation_id '
            'FROM kingres.dist_table d '
            'WHERE d.relid::pg_catalog.oid = $1 AND d.method = ''hash'' '
            'AND d.partition_method IS NOT NULL'
        INTO r_dist, r_method, r_key, r_colocation USING p_rel;
    r_dist := coalesce(r_dist, false);
END;
$$;

-- The partitions of a distributed table with the bound as it was given to create_partition; nothing without kingres_shard.
CREATE FUNCTION kingres.timepart__dist_bounds(p_parent oid)
RETURNS TABLE (partition_name name, bound text)
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
BEGIN
    IF pg_catalog.to_regclass('kingres.dist_partition') IS NULL THEN
        RETURN;
    END IF;
    RETURN QUERY EXECUTE
        'SELECT p.partition_name, p.bound FROM kingres.dist_partition p '
        'WHERE p.parent::pg_catalog.oid = $1'
        USING p_parent;
END;
$$;

-- timepart_part_name with the length limit as an argument: 63 bytes for a partition of an ordinary table, 57 for one
-- of a distributed table (its leaves append _bNNNN).
CREATE FUNCTION kingres.timepart__part_name(p_base name, p_every interval,
                                            p_tz text, p_start timestamptz,
                                            p_max integer)
RETURNS name
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_unit   text;
    v_n      integer;
    v_tail   text;
    v_base   text := p_base::text;
BEGIN
    SELECT u.unit, u.n INTO v_unit, v_n
      FROM kingres.timepart_interval_parts(p_every) u;
    v_tail := '_p' || CASE v_unit
        WHEN 'hour'  THEN to_char(p_start AT TIME ZONE 'UTC', 'YYYYMMDDHH24')
        WHEN 'day'   THEN to_char(p_start AT TIME ZONE p_tz, 'YYYYMMDD')
        WHEN 'week'  THEN to_char(p_start AT TIME ZONE p_tz, 'YYYYMMDD')
        WHEN 'month' THEN to_char(p_start AT TIME ZONE p_tz, 'YYYYMM')
        ELSE              to_char(p_start AT TIME ZONE p_tz, 'YYYY')
    END;
    WHILE octet_length(v_base) + octet_length(v_tail) > p_max LOOP
        v_base := left(v_base, char_length(v_base) - 1);
    END LOOP;
    RETURN (v_base || v_tail)::name;
END;
$$;

CREATE OR REPLACE FUNCTION kingres.timepart_part_name(p_base name, p_every interval,
                                           p_tz text, p_start timestamptz)
RETURNS name
LANGUAGE sql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
    SELECT kingres.timepart__part_name(p_base, p_every, p_tz, p_start, 63)
$$;

-- The storage of the partitions of a distributed table, read on the nodes that hold the leaves: per partition the
-- number of leaves, how many of them are columnar and the kind (heap, columnar, mixed, or other when a leaf is
-- missing or has another access method).  SECURITY DEFINER because asking the nodes is for superusers; the caller
-- must be a superuser or have the privileges of the owner of the table, as in timepart__special_table.
CREATE FUNCTION kingres.timepart__dist_kinds(p_parent oid)
RETURNS TABLE (partition_name name, kind text, leaves integer,
               columnar_leaves integer)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, kingres, pg_temp
AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles r
                    WHERE r.rolname = session_user AND r.rolsuper)
       AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                        WHERE c.oid = p_parent
                          AND pg_catalog.pg_has_role(session_user, c.relowner, 'USAGE')) THEN
        RAISE EXCEPTION 'permission denied: only the owner of the table or a superuser may ask'
            USING ERRCODE = '42501';
    END IF;
    IF NOT (SELECT d.r_dist FROM kingres.timepart__dist(p_parent) d) THEN
        RAISE EXCEPTION 'relation with OID % is not a distributed table', p_parent
            USING ERRCODE = '42809';
    END IF;
    RETURN QUERY EXECUTE
        'SELECT s.partition_name::pg_catalog.name, '
        'CASE WHEN count(*) = count(*) FILTER (WHERE s.access_method = ''columnar'') THEN ''columnar'' '
        '     WHEN count(*) = count(*) FILTER (WHERE s.access_method = ''heap'') THEN ''heap'' '
        '     WHEN count(*) FILTER (WHERE s.access_method = ''columnar'') > 0 THEN ''mixed'' '
        '     ELSE ''other'' END, '
        'count(*)::pg_catalog.int4, '
        '(count(*) FILTER (WHERE s.access_method = ''columnar''))::pg_catalog.int4 '
        'FROM kingres.partition_leaf_storage($1::pg_catalog.oid::pg_catalog.regclass) s '
        'GROUP BY s.partition_name'
        USING p_parent;
END;
$$;

-- The partitions of a distributed table: the bounds, the number of leaves and their storage (heap, columnar, or
-- mixed while a conversion is half done).  The counterpart of timepart_partitions, whose partitions are tables.
CREATE FUNCTION kingres.timepart_dist_partitions(rel regclass)
RETURNS TABLE (partition_name name, range_from timestamptz,
               range_to timestamptz, access_method text, leaves integer,
               columnar_leaves integer)
LANGUAGE plpgsql
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_info record;
BEGIN
    PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
    SELECT * INTO v_info FROM kingres.timepart__key_info(rel::oid);
    IF NOT (SELECT d.r_dist FROM kingres.timepart__dist(rel::oid) d) THEN
        RAISE EXCEPTION '%.% is not a distributed table; see timepart_partitions',
            quote_ident(v_info.r_schema), quote_ident(v_info.r_table)
            USING ERRCODE = '42809';
    END IF;
    IF NOT (pg_has_role(current_user, v_info.r_owner_oid, 'USAGE')
            OR has_table_privilege(current_user, rel::oid, 'SELECT')) THEN
        RAISE EXCEPTION 'permission denied for table %',
            format('%I.%I', v_info.r_schema, v_info.r_table)
            USING ERRCODE = '42501';
    END IF;
    RETURN QUERY
    SELECT c.part_name, c.lo, c.hi, k.kind, k.leaves, k.columnar_leaves
      FROM kingres.timepart__children(rel::oid) c
      LEFT JOIN kingres.timepart__dist_kinds(rel::oid) k
        ON k.partition_name = c.part_name
     WHERE NOT c.is_default
     ORDER BY c.lo;
END;
$$;

-- The leaves one policy needs in a steady state against kingres_shard.max_partition_leaves: the buckets times the
-- partitions of the retention (one year when there is none) and of the lookahead.  A refusal of the policy, with the
-- numbers; create_partition refuses the partition that would pass the limit all the same.
CREATE FUNCTION kingres.timepart__dist_budget(
    p_rel oid, p_every interval, p_ahead interval, p_retention interval)
RETURNS void
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_b     record;
    v_parts bigint;
BEGIN
    IF NOT (SELECT d.r_dist FROM kingres.timepart__dist(p_rel) d) THEN
        RETURN;
    END IF;
    EXECUTE 'SELECT b.buckets, b.max_leaves '
            'FROM kingres.partition_leaf_budget($1::pg_catalog.oid::pg_catalog.regclass) b'
        INTO v_b USING p_rel;
    v_parts := ceil(extract(epoch FROM coalesce(p_retention, interval '1 year') + p_ahead)
                    / extract(epoch FROM p_every))::bigint + 1;
    IF v_b.buckets::bigint * v_parts > v_b.max_leaves THEN
        RAISE EXCEPTION 'the policy needs about % leaves in a steady state (% buckets, % partitions); kingres_shard.max_partition_leaves is %',
            v_b.buckets::bigint * v_parts, v_b.buckets, v_parts, v_b.max_leaves
            USING ERRCODE = '54000',
                  HINT = 'Use a wider partition_every, a shorter retention or create_ahead, or raise kingres_shard.max_partition_leaves.';
    END IF;
END;
$$;

-- Convert the leaves of one partition of a distributed table, on the nodes that hold them (11.4).  The colocation
-- group is claimed first (kingres.begin_leaf_conversion, committed at once through a session of its own), so that no
-- bucket moves, no table operation copies the group and no partition is created or dropped meanwhile; then each
-- leaf that is not in the target storage yet is converted on its node by kingres.convert_leaf, in a session of its
-- own that commits; the claim is renewed after each leaf and released at the end, also when a leaf fails.  The leaves
-- converted before a failure stay converted (the partition is then mixed, and the next run goes on).  Returns what
-- was done.  A superuser, on the map group primary.
CREATE FUNCTION kingres.timepart__dist_convert(
    p_parent oid, p_partition name, p_target name, p_sort text[],
    p_options jsonb)
RETURNS text
LANGUAGE plpgsql
SET search_path = pg_catalog, kingres, pg_temp
AS $$
DECLARE
    v_self    integer;
    v_op      bigint;
    v_leaves  jsonb;
    v_leaf    record;
    v_node    integer;
    v_bp      text;
    v_have    text;
    v_trust   boolean := kingres.timepart__trust_owners();
    v_ms      integer := kingres.timepart__setting_int(
                             'kingres_timepart.dist_leaf_timeout_ms', 14400000, 1000, 86400000);
    v_done    integer := 0;
    v_had     integer := 0;
    v_kind    text := kingres.timepart__am_kind(p_target);
    v_msg     text;
    v_state   text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname = current_user AND r.rolsuper) THEN
        RAISE EXCEPTION 'permission denied: the leaves of a distributed table are converted by a superuser'
            USING ERRCODE = '42501';
    END IF;
    IF v_kind IS NULL OR v_kind = 'other' THEN
        RAISE EXCEPTION 'conversion_refused: "%" is not a supported table access method (use columnar or heap)', p_target
            USING ERRCODE = '0A000';
    END IF;
    IF v_kind <> 'heap' THEN
        PERFORM kingres.timepart__need_columnar();
    END IF;
    SELECT jsonb_agg(to_jsonb(s) ORDER BY s.node_id, s.bucket) INTO v_leaves
      FROM kingres.partition_leaf_storage(p_parent::regclass) s
     WHERE s.partition_name = p_partition::text;
    IF v_leaves IS NULL THEN
        RAISE EXCEPTION 'partition "%" of table "%" does not exist', p_partition, p_parent::regclass
            USING ERRCODE = '42704';
    END IF;
    -- every node that holds a leaf can convert it (kingres_timepart 0.3 and, for columnar, kingres_columnar)
    FOR v_node IN SELECT DISTINCT (x ->> 'node_id')::integer
                    FROM jsonb_array_elements(v_leaves) x ORDER BY 1
    LOOP
        v_have := kingres.internal_exec_scalar(v_node,
            'SELECT (pg_catalog.to_regprocedure(''kingres.convert_leaf(regclass,regclass,name,text[],jsonb,boolean)'') IS NOT NULL)::pg_catalog.text '
            '|| '','' || (EXISTS (SELECT 1 FROM pg_catalog.pg_extension e WHERE e.extname = ''kingres_columnar''))::pg_catalog.text',
            60000);
        IF v_have LIKE 'false,%' THEN
            RAISE EXCEPTION 'conversion_refused: node % does not have kingres_timepart 0.3 (kingres.convert_leaf) in this database', v_node
                USING ERRCODE = '0A000',
                      HINT = 'Install or update kingres_timepart on every node of the cluster.';
        END IF;
        IF v_kind = 'columnar' AND v_have LIKE '%,false' THEN
            RAISE EXCEPTION 'conversion_refused: node % does not have kingres_columnar in this database', v_node
                USING ERRCODE = '0A000',
                      HINT = 'Install kingres_columnar on every node of the cluster.';
        END IF;
    END LOOP;

    v_self := (SELECT l.node_id FROM kingres.local_node l);
    v_op := kingres.internal_exec_scalar(v_self,
        format('SELECT kingres.begin_leaf_conversion(%s::pg_catalog.oid::pg_catalog.regclass, %L, %L, %s)',
               p_parent::oid, p_partition, p_target, pg_catalog.pg_backend_pid()), 60000)::bigint;
    BEGIN
        FOR v_leaf IN
            SELECT x.partition_name, x.node_id, x.bucket, x.leaf, x.access_method
              FROM jsonb_to_recordset(v_leaves)
                   AS x(partition_name text, node_id integer, bucket integer,
                        leaf text, access_method text)
             ORDER BY x.node_id, x.bucket
        LOOP
            IF v_leaf.access_method IS NULL THEN
                RAISE EXCEPTION 'conversion_refused: the leaf % of bucket % is missing on node %',
                    v_leaf.leaf, v_leaf.bucket, v_leaf.node_id USING ERRCODE = '0A000';
            END IF;
            IF v_leaf.access_method = p_target::text THEN
                v_had := v_had + 1;
                CONTINUE;
            END IF;
            v_bp := format('%I.%I',
                           (SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                             WHERE c.oid = p_parent),
                           (SELECT c.relname FROM pg_class c WHERE c.oid = p_parent)
                               || '_b' || lpad(v_leaf.bucket::text, 4, '0'));
            PERFORM kingres.internal_node_exec(v_leaf.node_id,
                format('CALL kingres.convert_leaf(%L::pg_catalog.regclass, %L::pg_catalog.regclass, %L, %L::pg_catalog.text[], %L::pg_catalog.jsonb, %L::pg_catalog.bool)',
                       v_bp, v_leaf.leaf, p_target, p_sort, p_options, v_trust),
                v_ms);
            v_done := v_done + 1;
            PERFORM kingres.internal_exec_scalar(v_self,
                format('SELECT kingres.renew_leaf_conversion(%s)::pg_catalog.text', v_op), 60000);
        END LOOP;
    EXCEPTION WHEN OTHERS OR query_canceled THEN
        v_msg := SQLERRM;
        v_state := SQLSTATE;
        BEGIN
            PERFORM kingres.internal_exec_scalar(v_self,
                format('SELECT kingres.end_leaf_conversion(%s, %L)::pg_catalog.text', v_op,
                       left('stopped: ' || v_msg, 200)), 60000);
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            NULL;
        END;
        RAISE EXCEPTION '%', v_msg USING ERRCODE = v_state;
    END;
    PERFORM kingres.internal_exec_scalar(v_self,
        format('SELECT kingres.end_leaf_conversion(%s, %L)::pg_catalog.text', v_op,
               format('%s leaves converted to %s', v_done, p_target)), 60000);
    RETURN format('%s leaves converted to %s on %s nodes, %s already were',
                  v_done, p_target,
                  (SELECT count(DISTINCT (x ->> 'node_id')) FROM jsonb_array_elements(v_leaves) x), v_had);
END;
$$;

-- Convert one leaf of a distributed table on the node that holds it: the conversion of convert_partition (10.4), with
-- the bucket partition as the parent.  A superuser, called by timepart__dist_convert on every node that holds a leaf.
-- The swap is DDL on a bucket partition, which the hook of kingres_shard refuses for a user, so the transaction of the
-- swap runs with kingres_shard.enable_routing off (a setting of superusers); the new leaf of a columnar conversion has
-- REPLICA IDENTITY FULL, which the logical replication of a bucket move needs.  trust_owners is the setting
-- kingres_timepart.trust_table_owners of the session that asks.  Cannot run inside a transaction block.
CREATE PROCEDURE kingres.convert_leaf(
    parent regclass, leaf regclass,
    access_method name DEFAULT 'columnar', sort_by text[] DEFAULT NULL,
    options jsonb DEFAULT NULL, trust_owners boolean DEFAULT false)
LANGUAGE plpgsql
AS $$
DECLARE
    v_info    record;
    v_stmts   text[];
    v_self    oid;
    v_super   boolean;
    v_is      boolean;
    v_pschema name;
    v_pname   name;
BEGIN
    PERFORM pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true);
    -- Refuse a transaction block before anything is done.
    COMMIT;
    PERFORM pg_catalog.set_config('search_path', 'pg_catalog, pg_temp', true);
    IF parent IS NULL OR leaf IS NULL OR access_method IS NULL THEN
        RAISE EXCEPTION 'parent, leaf and access_method must not be null'
            USING ERRCODE = '22004';
    END IF;
    SELECT r.oid, r.rolsuper INTO v_self, v_super
      FROM pg_catalog.pg_roles r WHERE r.rolname OPERATOR(pg_catalog.=) current_user;
    IF NOT v_super THEN
        RAISE EXCEPTION 'permission denied: only a superuser may convert the leaves of a distributed table'
            USING ERRCODE = '42501';
    END IF;
    IF pg_catalog.to_regclass('kingres.dist_table') IS NULL THEN
        RAISE EXCEPTION '% is not a bucket partition of a distributed table', parent
            USING ERRCODE = '42809';
    END IF;
    EXECUTE 'SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i '
            'JOIN kingres.dist_table d ON d.relid::pg_catalog.oid = i.inhparent '
            'WHERE i.inhrelid = $1::pg_catalog.oid)'
        INTO v_is USING parent;
    IF NOT v_is THEN
        RAISE EXCEPTION '% is not a bucket partition of a distributed table', parent
            USING ERRCODE = '42809';
    END IF;
    IF kingres.timepart__am_kind(access_method) IS DISTINCT FROM 'heap' THEN
        PERFORM kingres.timepart__need_columnar();
    END IF;
    PERFORM pg_catalog.set_config('lock_timeout', kingres.timepart__lock_timeout('create'), true);
    SELECT * INTO v_info FROM kingres.timepart__key_info(parent::pg_catalog.oid);
    SELECT n.nspname, c.relname INTO v_pschema, v_pname
      FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
     WHERE c.oid OPERATOR(pg_catalog.=) leaf::pg_catalog.oid;
    v_stmts := kingres.timepart__convert_stmts(
        parent::pg_catalog.oid, leaf::pg_catalog.oid, access_method, sort_by, options, true);
    IF trust_owners THEN
        PERFORM pg_catalog.set_config('kingres_timepart.trust_table_owners', 'on', true);
    END IF;
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
    IF v_self OPERATOR(pg_catalog.=) v_info.r_owner_oid AND session_user OPERATOR(pg_catalog.=) current_user THEN
        PERFORM kingres.timepart__exec_direct(v_stmts);
    ELSE
        PERFORM kingres.timepart__exec_as(v_info.r_owner_oid, v_stmts);
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

-- convert_partition for a partition of a distributed table, for use by hand: every leaf that is not in the target
-- storage yet, on its node (see timepart__dist_convert).  A superuser, on the map group primary; returns what was done.
CREATE FUNCTION kingres.convert_distributed_partition(
    parent regclass, partition_name name,
    access_method name DEFAULT 'columnar', sort_by text[] DEFAULT NULL,
    options jsonb DEFAULT NULL)
RETURNS text
LANGUAGE plpgsql
SET search_path = pg_catalog, kingres, pg_temp
AS $$
BEGIN
    IF NOT (SELECT d.r_dist FROM kingres.timepart__dist(parent::oid) d) THEN
        RAISE EXCEPTION '% is not a distributed table', parent
            USING ERRCODE = '42809';
    END IF;
    RETURN kingres.timepart__dist_convert(parent::oid, partition_name,
                                          access_method, sort_by, options);
END;
$$;

------------------------------------------------------------------------
-- Privileges and comments
------------------------------------------------------------------------

REVOKE ALL ON FUNCTION
    kingres.timepart__dist_convert(oid, name, name, text[], jsonb),
    kingres.timepart_dist_partitions(regclass),
    kingres.convert_distributed_partition(regclass, name, name, text[], jsonb)
    FROM PUBLIC;
REVOKE ALL ON PROCEDURE
    kingres.convert_leaf(regclass, regclass, name, text[], jsonb, boolean)
    FROM PUBLIC;

COMMENT ON FUNCTION kingres.timepart_dist_partitions(regclass) IS
    'The partitions of a distributed table with bounds, number of leaves and storage (heap, columnar or mixed)';
COMMENT ON PROCEDURE kingres.convert_leaf(regclass, regclass, name, text[], jsonb, boolean) IS
    'Convert one leaf of a distributed table on the node that holds it (a superuser; called for every leaf of a partition by the policy); cannot run inside a transaction block';
