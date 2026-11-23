-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_shard--0.8--0.9.sql
--   Upgrade of the kingres_shard extension from version 0.8 to 0.9: columnar storage in the sharding
--   extension (design/analytics-phase12-storage.md, section 9; increments S1, S2 and S3) and the
--   count_distinct_mode rewrite (design/analytics-phase12-queries.md, Q-S5).
--   The result is identical to installing kingres_shard--0.9.sql (the "upgrade" regression test compares
--   the two).  kingres_shard--0.9.sql is the 0.8 install script with this script appended; regenerate it
--   with mk-install-script.sh when this file changes.

\echo Use "ALTER EXTENSION kingres_shard UPDATE TO '0.9'" to load this file. \quit

-- ---------------------------------------------------------------------------
-- The version handshake
-- ---------------------------------------------------------------------------

-- internal_check_node_versions(): asks for 0.9.
CREATE OR REPLACE FUNCTION kingres.internal_check_node_versions()
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    n pg_catalog.record;
    v pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR n IN SELECT nd.node_id, nd.host, nd.port FROM kingres.node nd
              WHERE nd.is_active AND nd.role OPERATOR(pg_catalog.=) 'primary' ORDER BY nd.node_id
    LOOP
        BEGIN
            v := kingres.internal_exec_scalar(n.node_id,
                'SELECT COALESCE((SELECT e.extversion FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) ''kingres_shard''), '''')', 20000);
        EXCEPTION WHEN OTHERS THEN
            -- The node may be reachable and refuse the claim of the session of this node (it runs
            -- another version of the library): ask for the version without a claim, so that the
            -- error names both versions (review finding S6-06).
            v := kingres.internal_remote_extension_version(n.host, n.port);
            IF v IS NULL THEN
                RAISE EXCEPTION 'node % (%:%) cannot be asked for its kingres_shard version: %', n.node_id, n.host, n.port, SQLERRM
                    USING ERRCODE = 'object_not_in_prerequisite_state';
            END IF;
        END;
        IF v IS DISTINCT FROM '0.9' THEN
            RAISE EXCEPTION 'node % (%:%) has kingres_shard %, a bucket move needs 0.9 on every node',
                n.node_id, n.host, n.port, COALESCE(NULLIF(v, ''), '(not installed)')
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Run ALTER EXTENSION kingres_shard UPDATE on that node, after the new library is loaded there.';
        END IF;
    END LOOP;
END;
$$;

-- ---------------------------------------------------------------------------
-- S1: the access method of a table is carried (design/analytics-phase12-storage.md, 9.1)
-- ---------------------------------------------------------------------------

-- The access method of a table as the clause that names it: ' USING "<am>"' for a table whose access method is not heap, the empty
-- string for heap (and for a partitioned table that has none).  A distributed table's parent carries the access method that its
-- bucket partitions inherit (CREATE TABLE ... PARTITION OF takes the parent's), so a columnar table stays columnar on every node.
CREATE FUNCTION kingres.internal_am_clause(p_relid regclass)
    RETURNS pg_catalog.text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT COALESCE((SELECT pg_catalog.format(' USING %I', a.amname)
                   FROM pg_catalog.pg_class c JOIN pg_catalog.pg_am a ON a.oid OPERATOR(pg_catalog.=) c.relam
                  WHERE c.oid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND a.amname OPERATOR(pg_catalog.<>) 'heap'), '');
$$;
REVOKE ALL ON FUNCTION kingres.internal_am_clause(regclass) FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- The options of a columnar table are carried (review P12S-03)
-- ---------------------------------------------------------------------------

-- The options that kingres_columnar keeps for a table (compression, level, rows per stripe, rows per chunk) as the arguments of
-- columnar_set_options(): "'zstd', 3, 150000, 10000".  For a partitioned table the options of its first columnar leaf (by name) on this node.
-- NULL when the table is not columnar, kingres_columnar is not installed, or no such leaf is here.
CREATE FUNCTION kingres.internal_columnar_args(p_relid regclass)
    RETURNS pg_catalog.text
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_rel  pg_catalog.regclass := p_relid;
    v_kind pg_catalog."char";
    v_out  pg_catalog.text;
BEGIN
    IF pg_catalog.to_regprocedure('kingres.columnar_options(regclass)') IS NULL THEN
        RETURN NULL;
    END IF;
    SELECT c.relkind INTO v_kind FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid;
    IF v_kind OPERATOR(pg_catalog.=) 'p' THEN
        SELECT c.oid::pg_catalog.regclass INTO v_rel
          FROM pg_catalog.pg_partition_tree(p_relid) pt
          JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) pt.relid
          JOIN pg_catalog.pg_am a ON a.oid OPERATOR(pg_catalog.=) c.relam
         WHERE pt.isleaf AND a.amname OPERATOR(pg_catalog.=) 'columnar'
         ORDER BY c.relname LIMIT 1;
        IF NOT FOUND THEN
            RETURN NULL;
        END IF;
    ELSIF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_am a ON a.oid OPERATOR(pg_catalog.=) c.relam
                       WHERE c.oid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid AND a.amname OPERATOR(pg_catalog.=) 'columnar') THEN
        RETURN NULL;
    END IF;
    EXECUTE 'SELECT pg_catalog.format(''%L, %s, %s, %s'', o.compression, o.compression_level, o.stripe_row_count, o.chunk_row_count) FROM kingres.columnar_options($1) o'
       INTO v_out USING v_rel;
    RETURN v_out;
END;
$$;
REVOKE ALL ON FUNCTION kingres.internal_columnar_args(regclass) FROM PUBLIC;

-- internal_table_ddl(): the statement that makes the table (and its shadow) names the access method.
CREATE OR REPLACE FUNCTION kingres.internal_table_ddl(p_relid regclass, p_column name, p_shadow bool DEFAULT false)
    RETURNS TABLE (ord pg_catalog.int4, scope pg_catalog.text, stmt pg_catalog.text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    c        pg_catalog.pg_class%ROWTYPE;
    nsp      pg_catalog.text;
    qname    pg_catalog.text;
    shname   pg_catalog.text;
    qtarget  pg_catalog.text;
    coldefs  pg_catalog.text;
    r        pg_catalog.record;
    q        pg_catalog.record;
    n        pg_catalog.int4 := 0;
    swaps    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    posts    pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    locals   pg_catalog.text[] := ARRAY[]::pg_catalog.text[];
    p        pg_catalog.text;
    owner    pg_catalog.text;
    k        pg_catalog.int4 := 0;
    cname    pg_catalog.text;
    iname    pg_catalog.text;
    idef     pg_catalog.text;
    idseq    pg_catalog.text;
    state    pg_catalog.record;
BEGIN
    SELECT * INTO c FROM pg_class WHERE oid = p_relid;
    SELECT nspname INTO nsp FROM pg_namespace WHERE oid = c.relnamespace;
    qname := format('%I.%I', nsp, c.relname);
    shname := format('kingres_shadow_%s', p_relid::pg_catalog.oid);
    qtarget := CASE WHEN p_shadow THEN format('%I.%I', nsp, shname) ELSE qname END;

    IF p_column IS NOT NULL AND octet_length(c.relname) + 6 > 63 THEN
        RAISE EXCEPTION 'table name "%" is too long', c.relname
            USING ERRCODE = 'invalid_name',
                  DETAIL = 'Bucket partition names append 6 characters (_bNNNN) to the table name.';
    END IF;

    n := n + 1; ord := n; scope := 'remote_pre';
    stmt := format('CREATE SCHEMA IF NOT EXISTS %I', nsp);
    RETURN NEXT;

    FOR r IN
        SELECT format('%I.%I', sn.nspname, s.relname) AS qseq,
               (SELECT ro.rolname FROM pg_roles ro WHERE ro.oid = s.relowner) AS seqowner,
               a.attname,
               format_type(sq.seqtypid, NULL) AS seqtype,
               sq.seqstart, sq.seqincrement, sq.seqmax, sq.seqmin, sq.seqcache, sq.seqcycle
          FROM pg_depend d
          JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
          JOIN pg_namespace sn ON sn.oid = s.relnamespace
          JOIN pg_sequence sq ON sq.seqrelid = s.oid
          JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
         WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
           AND d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
           AND d.refobjid = p_relid AND d.deptype = 'a'
         ORDER BY a.attnum
    LOOP
        n := n + 1; ord := n; scope := 'remote_pre';
        stmt := format('CREATE SEQUENCE IF NOT EXISTS %s AS %s INCREMENT BY %s MINVALUE %s MAXVALUE %s START WITH %s CACHE %s %s',
                       r.qseq, r.seqtype, r.seqincrement, r.seqmin, r.seqmax, r.seqstart, r.seqcache,
                       CASE WHEN r.seqcycle THEN 'CYCLE' ELSE 'NO CYCLE' END);
        RETURN NEXT;
        IF r.seqowner IS DISTINCT FROM current_user THEN
            -- a sequence must have the owner of the table it belongs to
            n := n + 1; ord := n; scope := 'remote_pre';
            stmt := format('ALTER SEQUENCE %s OWNER TO %I', r.qseq, r.seqowner);
            RETURN NEXT;
        END IF;
        n := n + 1; ord := n; scope := 'local_pre';
        stmt := format('ALTER SEQUENCE %s OWNED BY NONE', r.qseq);
        RETURN NEXT;
        swaps := swaps || format('ALTER SEQUENCE %s OWNED BY %s.%I', r.qseq, qname, r.attname);
        posts := posts || format('SELECT kingres.internal_advance_sequence(%L::pg_catalog.regclass, %L::pg_catalog.regclass, %L)',
                                 r.qseq, qname, r.attname);
    END LOOP;

    -- The identity columns' sequences belong to the table (an internal dependency),
    -- so the new table makes its own: with the options of the old one, and under the
    -- old name after the swap.
    SELECT string_agg(
             format('%I %s%s%s%s%s', a.attname, format_type(a.atttypid, a.atttypmod),
                    CASE WHEN a.attcollation <> t.typcollation
                         THEN ' COLLATE ' || format('%I.%I', cn.nspname, co.collname)
                         ELSE '' END,
                    CASE a.attidentity
                         WHEN 'a' THEN ' GENERATED ALWAYS AS IDENTITY'
                         WHEN 'd' THEN ' GENERATED BY DEFAULT AS IDENTITY'
                         ELSE '' END
                    || CASE WHEN a.attidentity <> '' THEN
                         (SELECT format(' (INCREMENT BY %s MINVALUE %s MAXVALUE %s START WITH %s CACHE %s %s)',
                                        sq.seqincrement, sq.seqmin, sq.seqmax, sq.seqstart, sq.seqcache,
                                        CASE WHEN sq.seqcycle THEN 'CYCLE' ELSE 'NO CYCLE' END)
                            FROM pg_sequence sq
                           WHERE sq.seqrelid = pg_get_serial_sequence(qname, a.attname::pg_catalog.text)::pg_catalog.regclass)
                         ELSE '' END,
                    CASE WHEN a.attgenerated = 's'
                         THEN format(' GENERATED ALWAYS AS (%s) STORED', pg_get_expr(ad.adbin, ad.adrelid))
                         WHEN a.attgenerated = 'v'
                         THEN format(' GENERATED ALWAYS AS (%s) VIRTUAL', pg_get_expr(ad.adbin, ad.adrelid))
                         WHEN ad.adbin IS NOT NULL AND a.attidentity = ''
                         THEN ' DEFAULT ' || pg_get_expr(ad.adbin, ad.adrelid)
                         ELSE '' END,
                    CASE WHEN a.attnotnull THEN coalesce(format(' CONSTRAINT %I NOT NULL', nn.conname), ' NOT NULL') ELSE '' END),
             ', ' ORDER BY a.attnum)
      INTO coldefs
      FROM pg_attribute a
      JOIN pg_type t ON t.oid = a.atttypid
      LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
      LEFT JOIN pg_collation co ON co.oid = a.attcollation
      LEFT JOIN pg_namespace cn ON cn.oid = co.collnamespace
      LEFT JOIN pg_constraint nn ON nn.conrelid = a.attrelid AND nn.contype = 'n' AND nn.conkey[1] = a.attnum
     WHERE a.attrelid = p_relid AND a.attnum > 0 AND NOT a.attisdropped;

    n := n + 1; ord := n; scope := 'both';
    -- A NULL column means a reference table: a plain table, a full copy per node.
    IF p_column IS NULL THEN
        stmt := format('CREATE TABLE %s (%s)%s%s', qtarget, coldefs, kingres.internal_am_clause(p_relid), COALESCE(NULLIF(' ' || kingres.internal_with_clause(kingres.internal_reloptions_of(p_relid)), ' '), ''));
    ELSE
        stmt := format('CREATE TABLE %s (%s) PARTITION BY HASH (%I)%s', qtarget, coldefs, p_column, kingres.internal_am_clause(p_relid));
    END IF;
    RETURN NEXT;
    -- the options of a columnar table (compression, stripe and chunk size) are kept for a plain table; the bucket partitions of a distributed
    -- table get theirs from the script that makes them
    IF p_column IS NULL AND kingres.internal_columnar_args(p_relid) IS NOT NULL THEN
        n := n + 1; ord := n; scope := 'both';
        stmt := format('SELECT kingres.columnar_set_options(%L::pg_catalog.regclass, %s)', qtarget, kingres.internal_columnar_args(p_relid));
        RETURN NEXT;
    END IF;

    -- The identity columns of the new table: their sequences get the old names and
    -- the values the rows need.
    FOR r IN
        SELECT a.attname, a.attnum
          FROM pg_attribute a
         WHERE a.attrelid = p_relid AND a.attnum > 0 AND NOT a.attisdropped AND a.attidentity <> ''
         ORDER BY a.attnum
    LOOP
        idseq := pg_get_serial_sequence(qname, r.attname::pg_catalog.text);
        IF p_shadow THEN
            swaps := swaps || format('SELECT kingres.internal_rename_identity(%L::pg_catalog.regclass, %L, %L)',
                                     qtarget, r.attname, split_part(idseq, '.', 2));
        END IF;
        posts := posts || format('SELECT kingres.internal_advance_sequence(pg_catalog.pg_get_serial_sequence(%L, %L)::pg_catalog.regclass, %L::pg_catalog.regclass, %L)',
                                 qname, r.attname, qname, r.attname);
        EXECUTE format('SELECT last_value, is_called FROM %s', idseq) INTO state;
        locals := locals || format('SELECT kingres.internal_advance_sequence(pg_catalog.pg_get_serial_sequence(%L, %L)::pg_catalog.regclass, %L::pg_catalog.regclass, %L, %s, %L)',
                                   qname, r.attname, qname, r.attname, state.last_value, state.is_called);
    END LOOP;

    FOR r IN
        SELECT k2.conname, pg_get_constraintdef(k2.oid) AS def, k2.contype
          FROM pg_constraint k2
         WHERE k2.conrelid = p_relid AND k2.contype IN ('p', 'u', 'c')
         ORDER BY CASE k2.contype WHEN 'p' THEN 0 WHEN 'u' THEN 1 ELSE 2 END, k2.conname
    LOOP
        n := n + 1; ord := n; scope := 'both';
        IF p_shadow AND r.contype IN ('p', 'u') THEN
            k := k + 1;
            cname := format('kingres_shadow_%s_c%s', p_relid::pg_catalog.oid, k);
            stmt := format('ALTER TABLE %s ADD CONSTRAINT %I %s', qtarget, cname, r.def);
            swaps := swaps || format('ALTER TABLE %s RENAME CONSTRAINT %I TO %I', qtarget, cname, r.conname);
        ELSE
            stmt := format('ALTER TABLE %s ADD CONSTRAINT %I %s', qtarget, r.conname, r.def);
        END IF;
        RETURN NEXT;
    END LOOP;

    -- Foreign keys of the table itself (not the copies in its partitions): they are
    -- added after every table exists, on the new table once it has its name.
    FOR r IN
        SELECT k2.conname, pg_get_constraintdef(k2.oid) AS def
          FROM pg_constraint k2
         WHERE k2.conrelid = p_relid AND k2.contype = 'f' AND k2.conparentid = 0
         ORDER BY k2.conname
    LOOP
        n := n + 1; ord := n; scope := 'fk';
        stmt := format('ALTER TABLE %s ADD CONSTRAINT %I %s', qname, r.conname, r.def);
        RETURN NEXT;
    END LOOP;

    k := 0;
    FOR r IN
        SELECT replace(pg_get_indexdef(i.indexrelid), ' ON ONLY ', ' ON ') AS def, ic.relname AS iname
          FROM pg_index i
          JOIN pg_class ic ON ic.oid = i.indexrelid
         WHERE i.indrelid = p_relid
           AND NOT EXISTS (SELECT 1 FROM pg_constraint k2
                            WHERE k2.conindid = i.indexrelid AND k2.conrelid = p_relid
                              AND k2.contype IN ('p', 'u', 'x'))
         ORDER BY i.indexrelid
    LOOP
        n := n + 1; ord := n; scope := 'both';
        IF p_shadow THEN
            k := k + 1;
            iname := format('kingres_shadow_%s_i%s', p_relid::pg_catalog.oid, k);
            idef := replace(r.def, format(' INDEX %I ON %s USING ', r.iname, qname),
                                   format(' INDEX %I ON %s USING ', iname, qtarget));
            IF idef = r.def THEN
                RAISE EXCEPTION 'cannot rewrite the definition of index "%": %', r.iname, r.def
                    USING ERRCODE = 'internal_error';
            END IF;
            stmt := idef;
            swaps := swaps || format('ALTER INDEX %I.%I RENAME TO %I', nsp, iname, r.iname);
        ELSE
            stmt := r.def;
        END IF;
        RETURN NEXT;
    END LOOP;

    SELECT rolname INTO owner FROM pg_roles WHERE oid = c.relowner;
    IF owner IS DISTINCT FROM current_user THEN
        n := n + 1; ord := n; scope := 'both';
        stmt := format('ALTER TABLE %s OWNER TO %I', qtarget, owner);
        RETURN NEXT;
    END IF;

    -- After the copy: the names of the shadow's constraints, indexes and identity
    -- sequences become the old names, then the table takes its name, then the
    -- sequences that columns own move to it.
    IF p_shadow THEN
        FOR p IN SELECT s FROM unnest(swaps) s WHERE s LIKE 'ALTER TABLE % RENAME CONSTRAINT %'
                                                  OR s LIKE 'ALTER INDEX % RENAME TO %'
                                                  OR s LIKE 'SELECT kingres.internal_rename_identity%'
        LOOP
            n := n + 1; ord := n; scope := 'swap';
            stmt := p;
            RETURN NEXT;
        END LOOP;
        n := n + 1; ord := n; scope := 'swap';
        stmt := format('ALTER TABLE %s RENAME TO %I', qtarget, c.relname);
        RETURN NEXT;
    END IF;
    FOR p IN SELECT s FROM unnest(swaps) s WHERE s LIKE 'ALTER SEQUENCE % OWNED BY %'
    LOOP
        n := n + 1; ord := n;
        scope := CASE WHEN p_shadow THEN 'swap' ELSE 'both' END;
        stmt := p;
        RETURN NEXT;
    END LOOP;

    -- Sequence values, then the privileges and comments of the old table.
    FOREACH p IN ARRAY posts LOOP
        n := n + 1; ord := n; scope := 'post';
        stmt := p;
        RETURN NEXT;
    END LOOP;
    FOREACH p IN ARRAY locals LOOP
        n := n + 1; ord := n; scope := 'post_local';
        stmt := p;
        RETURN NEXT;
    END LOOP;

    IF p_shadow OR p_column IS NULL THEN
        n := n + 1; ord := n; scope := 'post';
        stmt := format('SELECT kingres.internal_reset_acl(%L::pg_catalog.regclass, %L::pg_catalog.bool)', qname, c.relacl IS NULL);
        RETURN NEXT;
        -- table privileges: the owner's own entries first, so that the entries other
        -- grantors made find their grant option
        IF c.relacl IS NOT NULL THEN
            FOR q IN
                SELECT x.grantor, x.grantee, x.is_grantable,
                       string_agg(x.privilege_type, ', ' ORDER BY x.privilege_type) AS privs
                  FROM aclexplode(c.relacl) x
                 GROUP BY x.grantor, x.grantee, x.is_grantable
                 ORDER BY (x.grantor = c.relowner) DESC, x.grantor, x.grantee, x.is_grantable
            LOOP
                IF q.grantor <> c.relowner THEN
                    n := n + 1; ord := n; scope := 'post';
                    stmt := format('SET LOCAL ROLE %I', (SELECT rolname FROM pg_roles WHERE oid = q.grantor));
                    RETURN NEXT;
                END IF;
                n := n + 1; ord := n; scope := 'post';
                stmt := format('GRANT %s ON TABLE %s TO %s%s', q.privs, qname,
                               CASE WHEN q.grantee = 0 THEN 'PUBLIC'
                                    ELSE format('%I', (SELECT rolname FROM pg_roles WHERE oid = q.grantee)) END,
                               CASE WHEN q.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END);
                RETURN NEXT;
                IF q.grantor <> c.relowner THEN
                    n := n + 1; ord := n; scope := 'post';
                    stmt := 'RESET ROLE';
                    RETURN NEXT;
                END IF;
            END LOOP;
        END IF;
        -- column privileges
        FOR r IN
            SELECT a.attname, a.attacl FROM pg_attribute a
             WHERE a.attrelid = p_relid AND a.attnum > 0 AND NOT a.attisdropped AND a.attacl IS NOT NULL
             ORDER BY a.attnum
        LOOP
            FOR q IN
                SELECT x.grantor, x.grantee, x.is_grantable,
                       string_agg(format('%s (%I)', x.privilege_type, r.attname), ', ' ORDER BY x.privilege_type) AS privs
                  FROM aclexplode(r.attacl) x
                 GROUP BY x.grantor, x.grantee, x.is_grantable
                 ORDER BY (x.grantor = c.relowner) DESC, x.grantor, x.grantee, x.is_grantable
            LOOP
                IF q.grantor <> c.relowner THEN
                    n := n + 1; ord := n; scope := 'post';
                    stmt := format('SET LOCAL ROLE %I', (SELECT rolname FROM pg_roles WHERE oid = q.grantor));
                    RETURN NEXT;
                END IF;
                n := n + 1; ord := n; scope := 'post';
                stmt := format('GRANT %s ON TABLE %s TO %s%s', q.privs, qname,
                               CASE WHEN q.grantee = 0 THEN 'PUBLIC'
                                    ELSE format('%I', (SELECT rolname FROM pg_roles WHERE oid = q.grantee)) END,
                               CASE WHEN q.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END);
                RETURN NEXT;
                IF q.grantor <> c.relowner THEN
                    n := n + 1; ord := n; scope := 'post';
                    stmt := 'RESET ROLE';
                    RETURN NEXT;
                END IF;
            END LOOP;
        END LOOP;
        -- comments: the table, its columns, its constraints and its indexes
        FOR r IN
            SELECT d.objsubid, d.description FROM pg_description d
             WHERE d.classoid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objoid = p_relid
             ORDER BY d.objsubid
        LOOP
            n := n + 1; ord := n; scope := 'post';
            IF r.objsubid = 0 THEN
                stmt := format('COMMENT ON TABLE %s IS %L', qname, r.description);
            ELSE
                stmt := format('COMMENT ON COLUMN %s.%I IS %L', qname,
                               (SELECT a.attname FROM pg_attribute a WHERE a.attrelid = p_relid AND a.attnum = r.objsubid),
                               r.description);
            END IF;
            RETURN NEXT;
        END LOOP;
        FOR r IN
            SELECT k2.conname, d.description FROM pg_constraint k2
              JOIN pg_description d ON d.classoid = 'pg_catalog.pg_constraint'::pg_catalog.regclass AND d.objoid = k2.oid
             WHERE k2.conrelid = p_relid AND k2.contype IN ('p', 'u', 'c', 'f') AND k2.conparentid = 0
             ORDER BY k2.conname
        LOOP
            n := n + 1; ord := n; scope := 'post';
            stmt := format('COMMENT ON CONSTRAINT %I ON %s IS %L', r.conname, qname, r.description);
            RETURN NEXT;
        END LOOP;
        FOR r IN
            SELECT ic.relname, d.description FROM pg_index i
              JOIN pg_class ic ON ic.oid = i.indexrelid
              JOIN pg_description d ON d.classoid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objoid = i.indexrelid
             WHERE i.indrelid = p_relid
             ORDER BY ic.relname
        LOOP
            n := n + 1; ord := n; scope := 'post';
            stmt := format('COMMENT ON INDEX %I.%I IS %L', nsp, r.relname, r.description);
            RETURN NEXT;
        END LOOP;
    END IF;
END;
$$;

-- internal_partition_layout(): one more column, the access method of each partition, so that the leaves of a user-partitioned table
-- keep the storage they had.
DROP FUNCTION kingres.internal_partition_layout(regclass);
CREATE FUNCTION kingres.internal_partition_layout(p_relid regclass)
    RETURNS TABLE (partby pg_catalog.text, ord pg_catalog.int4, name pg_catalog.name, bound pg_catalog.text, am pg_catalog.name, opts pg_catalog.text)
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.pg_get_partkeydef(p_relid::pg_catalog.oid),
       (pg_catalog.row_number() OVER (ORDER BY pc.relname))::pg_catalog.int4,
       pc.relname,
       pg_catalog.regexp_replace(pg_catalog.pg_get_expr(pc.relpartbound, pc.oid), '^FOR VALUES ', ''),
       COALESCE((SELECT a.amname FROM pg_catalog.pg_am a WHERE a.oid OPERATOR(pg_catalog.=) pc.relam), 'heap'::pg_catalog.name),
       kingres.internal_columnar_args(pc.oid::pg_catalog.regclass)
  FROM pg_catalog.pg_inherits i
  JOIN pg_catalog.pg_class pc ON pc.oid OPERATOR(pg_catalog.=) i.inhrelid
 WHERE i.inhparent OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid
   AND EXISTS (SELECT 1 FROM pg_catalog.pg_partitioned_table t WHERE t.partrelid OPERATOR(pg_catalog.=) p_relid::pg_catalog.oid)
 ORDER BY pc.relname;
$$;
REVOKE ALL ON FUNCTION kingres.internal_partition_layout(regclass) FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- The report of a distribution knows the access method (S1, S2)
-- ---------------------------------------------------------------------------

-- The report of 0.8 is kept under another name and the report is a wrapper that adds the rows about the storage of the table; the
-- callers (internal_raise_blockers, the control plane) call the report by its name.  The rows added, for a table that is not
-- distributed yet and whose access method is not heap:
--   columnar_reference_table (blocker, distribute_reference): a reference table is heap in this version (design 9.5);
--   access_method_missing (blocker, distribute): a node lacks the access method, so the table could not be made there;
--   columnar_storage (note, distribute): what the table becomes;
--   columnar_online (blocker, every kind with mode online): the online variants capture changes with a row trigger, which columnar does not support.
ALTER FUNCTION kingres.table_change_report(text, regclass, jsonb) RENAME TO internal_table_change_report_base;

CREATE FUNCTION kingres.table_change_report(kind text, relation regclass,
                                            options jsonb DEFAULT '{}'::jsonb)
    RETURNS TABLE (severity text, code text, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
#variable_conflict use_variable
DECLARE
    v_am    pg_catalog.name;
    v_rel   pg_catalog.text;
    v_pfx   pg_catalog.text;
    nd      pg_catalog.record;
    probe   pg_catalog.text;
    local_id pg_catalog.int4;
    v_bud   pg_catalog.record;
BEGIN
    RETURN QUERY SELECT b.severity, b.code, b.object, b.detail
                   FROM kingres.internal_table_change_report_base(kind, relation, options) b;
    -- one more user partition must keep the table within kingres_shard.max_partition_leaves (create_partition and attach_partition refuse it)
    IF kind IN ('create_partition', 'attach_partition') THEN
        SELECT * INTO v_bud FROM kingres.partition_leaf_budget(relation);
        IF FOUND AND v_bud.buckets::pg_catalog.int8 OPERATOR(pg_catalog.*) (v_bud.partitions OPERATOR(pg_catalog.+) 1)::pg_catalog.int8 OPERATOR(pg_catalog.>) v_bud.max_leaves::pg_catalog.int8 THEN
            RETURN QUERY SELECT 'blocker'::pg_catalog.text, 'leaf_budget'::pg_catalog.text, relation::pg_catalog.regclass::pg_catalog.text,
                                pg_catalog.format('cannot change table "%s": it would have %s leaves (%s buckets, %s partitions); kingres_shard.max_partition_leaves is %s',
                                                  relation::pg_catalog.regclass::pg_catalog.text,
                                                  v_bud.buckets::pg_catalog.int8 OPERATOR(pg_catalog.*) (v_bud.partitions OPERATOR(pg_catalog.+) 1)::pg_catalog.int8,
                                                  v_bud.buckets, v_bud.partitions OPERATOR(pg_catalog.+) 1, v_bud.max_leaves);
        END IF;
        RETURN;
    END IF;
    IF kind NOT IN ('distribute', 'distribute_reference', 'undistribute', 'alter') THEN
        RETURN;
    END IF;
    SELECT a.amname, pg_catalog.format('%I.%I', n.nspname, k.relname) INTO v_am, v_rel
      FROM pg_catalog.pg_class k
      JOIN pg_catalog.pg_am a ON a.oid OPERATOR(pg_catalog.=) k.relam
      JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) k.relnamespace
     WHERE k.oid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND k.relkind OPERATOR(pg_catalog.=) ANY (ARRAY['r', 'p']::pg_catalog."char"[])
       AND a.amname OPERATOR(pg_catalog.<>) 'heap'
       AND (kind OPERATOR(pg_catalog.<>) ALL (ARRAY['distribute', 'distribute_reference'])
            OR NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) relation));
    IF v_am IS NULL THEN
        RETURN;
    END IF;
    -- the online variants capture the changes of the source with a row trigger, which a columnar table does not support
    IF options OPERATOR(pg_catalog.->>) 'mode' OPERATOR(pg_catalog.=) 'online' THEN
        RETURN QUERY SELECT 'blocker'::pg_catalog.text, 'columnar_online'::pg_catalog.text, v_rel,
                            pg_catalog.format('cannot change table "%s": the online variants capture changes with a row trigger, which the access method "%s" does not support; use the blocking variant', relation::pg_catalog.regclass::pg_catalog.text, v_am);
    END IF;
    IF kind OPERATOR(pg_catalog.=) ANY (ARRAY['undistribute', 'alter']) THEN
        RETURN;
    END IF;
    v_pfx := CASE kind WHEN 'distribute' THEN pg_catalog.format('cannot distribute table "%s": ', relation::pg_catalog.regclass::pg_catalog.text)
                       ELSE pg_catalog.format('cannot make table "%s" a reference table: ', relation::pg_catalog.regclass::pg_catalog.text) END;
    IF kind OPERATOR(pg_catalog.=) 'distribute_reference' THEN
        RETURN QUERY SELECT 'blocker'::pg_catalog.text, 'columnar_reference_table'::pg_catalog.text, v_rel,
                            v_pfx OPERATOR(pg_catalog.||) pg_catalog.format('the table uses the access method "%s"; a reference table is a heap table in this version', v_am);
        RETURN;
    END IF;
    SELECT l.node_id INTO local_id FROM kingres.local_node l;
    FOR nd IN SELECT n.node_id, n.host, n.port FROM kingres.node n
               WHERE n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active
                 AND n.node_id OPERATOR(pg_catalog.<>) COALESCE(local_id, 0)
               ORDER BY n.node_id
    LOOP
        BEGIN
            probe := kingres.internal_exec_scalar(nd.node_id,
                pg_catalog.format('SELECT CASE WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_am a WHERE a.amname OPERATOR(pg_catalog.=) %L AND a.amtype OPERATOR(pg_catalog.=) ''t'') THEN ''yes'' ELSE ''no'' END', v_am),
                (SELECT s.setting::pg_catalog.int4 OPERATOR(pg_catalog.*) 2000 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout'));
            IF probe OPERATOR(pg_catalog.=) 'no' THEN
                RETURN QUERY SELECT 'blocker'::pg_catalog.text, 'access_method_missing'::pg_catalog.text, v_am::pg_catalog.text,
                                    v_pfx OPERATOR(pg_catalog.||) pg_catalog.format('the access method "%s" does not exist on node %s; create the extension that provides it there first', v_am, nd.node_id);
            END IF;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            -- an unreachable node is reported by the report of 0.8 already
            NULL;
        END;
    END LOOP;
    RETURN QUERY SELECT 'note'::pg_catalog.text, 'columnar_storage'::pg_catalog.text, v_rel,
                        pg_catalog.format('the table keeps its access method "%s": every bucket partition is made with it and has REPLICA IDENTITY FULL; UPDATE and DELETE are refused by the access method on every shard group', v_am);
END;
$$;

COMMENT ON FUNCTION kingres.table_change_report(text, regclass, jsonb) IS
    'What a change of a table would meet: blockers, warnings and notes, without changing anything (the access method of the table included)';
COMMENT ON FUNCTION kingres.internal_table_change_report_base(text, regclass, jsonb) IS
    'The report of 0.8, which table_change_report() extends with the rows about the storage of the table';
REVOKE ALL ON FUNCTION kingres.table_change_report(text, regclass, jsonb), kingres.internal_table_change_report_base(text, regclass, jsonb) FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- S2: a bucket move refuses a columnar relation (design 9.3, 9.7)
-- ---------------------------------------------------------------------------

-- internal_move_inspect(): on the source, a bucket partition or a leaf of a user-partitioned bucket partition that is not heap stops the
-- move in "preparing", before anything is made, with a stable text, unless the access method is columnar and the installed kingres_columnar decodes
-- its changes (0.2, increment S3): then the answer carries the access method of the bucket partition for the copy.
CREATE OR REPLACE FUNCTION kingres.internal_move_inspect(p_role text, p_bucket int, p_bucket_count int, p_tables jsonb)
    RETURNS jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    t         pg_catalog.record;
    v_ntab    pg_catalog.int4 := pg_catalog.jsonb_array_length(p_tables);
    v_sync    pg_catalog.int4 := LEAST(pg_catalog.current_setting('max_sync_workers_per_subscription')::pg_catalog.int4, v_ntab + 1);
    v_free    pg_catalog.int4;
    v_node    pg_catalog.int4;
    v_out     pg_catalog.jsonb := '[]'::pg_catalog.jsonb;
    v_part    pg_catalog.text;
    v_oid     pg_catalog.oid;
    v_ident   pg_catalog.text;
    v_pk      pg_catalog.bool;
    v_col     pg_catalog.text;
    v_origins pg_catalog.text;
    v_columnar pg_catalog.text;
    v_amname pg_catalog.text;
    v_colver pg_catalog.text;
    v_bam    pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    SELECT l.node_id INTO v_node FROM kingres.local_node l;

    IF pg_catalog.current_setting('wal_level') OPERATOR(pg_catalog.<>) 'logical' THEN
        RAISE EXCEPTION 'wal_level is "%" on node %, a bucket move needs wal_level = logical', pg_catalog.current_setting('wal_level'), v_node
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  HINT = 'Set wal_level = logical on every node and restart them.';
    END IF;
    IF pg_catalog.current_setting('max_logical_replication_workers')::pg_catalog.int4 < 1 + v_sync THEN
        RAISE EXCEPTION 'max_logical_replication_workers is %, node % needs at least %',
            pg_catalog.current_setting('max_logical_replication_workers'), v_node, 1 + v_sync
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  HINT = 'Raise max_logical_replication_workers (and max_worker_processes) and restart the node.';
    END IF;

    IF p_role OPERATOR(pg_catalog.=) 'source' THEN
        SELECT pg_catalog.current_setting('max_replication_slots')::pg_catalog.int4 - pg_catalog.count(*)
          INTO v_free FROM pg_catalog.pg_replication_slots;
        IF v_free < 1 + v_sync THEN
            RAISE EXCEPTION 'node % has % free replication slots (max_replication_slots = %), a move needs %',
                v_node, v_free, pg_catalog.current_setting('max_replication_slots'), 1 + v_sync
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Raise max_replication_slots and restart the node, or finish the moves that hold slots.';
        END IF;
        SELECT pg_catalog.current_setting('max_wal_senders')::pg_catalog.int4 - pg_catalog.count(*)
          INTO v_free FROM pg_catalog.pg_stat_replication;
        IF v_free < 1 + v_sync THEN
            RAISE EXCEPTION 'node % has % free WAL senders (max_wal_senders = %), a move needs %',
                v_node, v_free, pg_catalog.current_setting('max_wal_senders'), 1 + v_sync
                USING ERRCODE = 'object_not_in_prerequisite_state',
                      HINT = 'Raise max_wal_senders and restart the node.';
        END IF;
    ELSE
        v_origins := pg_catalog.current_setting('max_active_replication_origins', true);
        IF v_origins IS NOT NULL THEN
            SELECT v_origins::pg_catalog.int4 - pg_catalog.count(*) INTO v_free FROM pg_catalog.pg_replication_origin;
            IF v_free < 1 + v_sync THEN
                RAISE EXCEPTION 'node % has % free replication origins (max_active_replication_origins = %), a move needs %',
                    v_node, v_free, v_origins, 1 + v_sync
                    USING ERRCODE = 'object_not_in_prerequisite_state',
                          HINT = 'Raise max_active_replication_origins and restart the node.';
            END IF;
        END IF;
    END IF;

    FOR t IN SELECT x.schema_name, x.table_name
               FROM pg_catalog.jsonb_to_recordset(p_tables) AS x(schema_name text, table_name text)
    LOOP
        v_part := kingres.internal_bucket_part_name(t.table_name, p_bucket);
        v_oid := pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, v_part));
        IF p_role OPERATOR(pg_catalog.=) 'source' THEN
            IF v_oid IS NULL OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
                                             WHERE c.oid OPERATOR(pg_catalog.=) v_oid AND c.relispartition) THEN
                RAISE EXCEPTION 'bucket partition "%.%" is not attached on node %', t.schema_name, v_part, v_node
                    USING ERRCODE = 'object_not_in_prerequisite_state';
            END IF;
            SELECT c.relreplident::pg_catalog.text INTO v_ident FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) v_oid;
            SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_index i
                            WHERE i.indrelid OPERATOR(pg_catalog.=) v_oid AND i.indisprimary) INTO v_pk;
            IF v_ident OPERATOR(pg_catalog.=) 'n' OR (v_ident OPERATOR(pg_catalog.=) 'd' AND NOT v_pk) THEN
                RAISE EXCEPTION 'table "%.%" has no replica identity', t.schema_name, t.table_name
                    USING ERRCODE = 'object_not_in_prerequisite_state',
                          DETAIL = 'Logical replication needs a primary key or REPLICA IDENTITY FULL to replicate updates and deletes of the bucket.',
                          HINT = pg_catalog.format('Add a primary key, or set REPLICA IDENTITY FULL on the bucket partitions of %I.%I on the node that has them (ALTER TABLE is not propagated for it: superuser, kingres_shard.enable_routing = off).', t.schema_name, t.table_name);
            END IF;
            -- S3: a relation that is not heap moves when its changes can be decoded: the copy is made with its access method (the
            -- answer carries it), the initial copy is the stripe-wise COPY and the changes are the decoded inserts of kingres_columnar 0.2.
            -- every relation of the tree is looked at, not the first: one that is neither heap nor columnar stops the move
            SELECT pg_catalog.format('%I.%I', ln.nspname, lc.relname), a.amname INTO v_columnar, v_amname
              FROM pg_catalog.pg_partition_tree(v_oid) pt
              JOIN pg_catalog.pg_class lc ON lc.oid OPERATOR(pg_catalog.=) pt.relid
              JOIN pg_catalog.pg_namespace ln ON ln.oid OPERATOR(pg_catalog.=) lc.relnamespace
              JOIN pg_catalog.pg_am a ON a.oid OPERATOR(pg_catalog.=) lc.relam
             WHERE lc.relkind OPERATOR(pg_catalog.=) 'r' AND a.amname OPERATOR(pg_catalog.<>) 'heap' AND a.amname OPERATOR(pg_catalog.<>) 'columnar'
             ORDER BY lc.relname LIMIT 1;
            IF v_columnar IS NOT NULL THEN
                RAISE EXCEPTION 'bucket move refused: relation "%" uses the access method "%", which a bucket move does not carry', v_columnar, v_amname
                    USING ERRCODE = '0A000';
            END IF;
            SELECT pg_catalog.format('%I.%I', ln.nspname, lc.relname) INTO v_columnar
              FROM pg_catalog.pg_partition_tree(v_oid) pt
              JOIN pg_catalog.pg_class lc ON lc.oid OPERATOR(pg_catalog.=) pt.relid
              JOIN pg_catalog.pg_namespace ln ON ln.oid OPERATOR(pg_catalog.=) lc.relnamespace
              JOIN pg_catalog.pg_am a ON a.oid OPERATOR(pg_catalog.=) lc.relam
             WHERE lc.relkind OPERATOR(pg_catalog.=) 'r' AND a.amname OPERATOR(pg_catalog.=) 'columnar'
             ORDER BY lc.relname LIMIT 1;
            IF v_columnar IS NOT NULL THEN
                SELECT e.extversion INTO v_colver FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_columnar';
                IF v_colver IS NULL OR pg_catalog.string_to_array(v_colver, '.')::pg_catalog.int4[] OPERATOR(pg_catalog.<) ARRAY[0, 2] THEN
                    RAISE EXCEPTION 'bucket move refused: relation "%" has columnar storage, and the installed kingres_columnar cannot decode its changes', v_columnar
                        USING ERRCODE = '0A000',
                              HINT = 'Move buckets of this table after kingres_columnar is upgraded to a version that supports logical decoding (0.2), or convert the table to row storage';
                END IF;
            END IF;
            SELECT a.amname INTO v_bam FROM pg_catalog.pg_class c JOIN pg_catalog.pg_am a ON a.oid OPERATOR(pg_catalog.=) c.relam
             WHERE c.oid OPERATOR(pg_catalog.=) v_oid AND c.relkind OPERATOR(pg_catalog.=) 'r' AND a.amname OPERATOR(pg_catalog.<>) 'heap';
            IF v_ident OPERATOR(pg_catalog.=) 'i' THEN
                RAISE EXCEPTION 'table "%.%" uses REPLICA IDENTITY USING INDEX, which a bucket move does not support yet', t.schema_name, t.table_name
                    USING ERRCODE = 'feature_not_supported',
                          HINT = 'Use a primary key or REPLICA IDENTITY FULL.';
            END IF;
            SELECT a.attname INTO v_col FROM pg_catalog.pg_attribute a
             WHERE a.attrelid OPERATOR(pg_catalog.=) v_oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped
               AND a.attidentity OPERATOR(pg_catalog.<>) '' LIMIT 1;
            IF FOUND THEN
                RAISE EXCEPTION 'column "%" of table "%.%" is an identity column, which a bucket move does not support yet', v_col, t.schema_name, t.table_name
                    USING ERRCODE = 'feature_not_supported';
            END IF;
            v_out := v_out OPERATOR(pg_catalog.||) (pg_catalog.jsonb_build_object(
                'schema_name', t.schema_name, 'table_name', t.table_name, 'replident', v_ident)
                OPERATOR(pg_catalog.||) kingres.internal_move_tree(v_oid)
                OPERATOR(pg_catalog.||) CASE WHEN v_bam IS NULL THEN '{}'::pg_catalog.jsonb ELSE pg_catalog.jsonb_build_object('am', v_bam, 'opts', kingres.internal_columnar_args(v_oid::pg_catalog.regclass)) END);
        ELSE
            IF v_oid IS NOT NULL THEN
                RAISE EXCEPTION 'relation "%.%" already exists on node %', t.schema_name, v_part, v_node
                    USING ERRCODE = 'duplicate_table',
                          HINT = 'It may be left over from an earlier move; see kingres.check_cluster().';
            END IF;
            IF pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, t.table_name)) IS NULL THEN
                RAISE EXCEPTION 'table "%.%" does not exist on node %', t.schema_name, t.table_name, v_node
                    USING ERRCODE = 'undefined_table';
            END IF;
            v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object(
                'schema_name', t.schema_name, 'table_name', t.table_name);
        END IF;
    END LOOP;
    RETURN v_out;
END;
$$;

-- internal_split_inspect(): the refusal of a table that is not heap says why, in the words of the design (9.7): a split needs a primary
-- key, row locks and update triggers, which a columnar table does not have.
CREATE OR REPLACE FUNCTION kingres.internal_split_inspect(p_colocation int, p_bucket int)
    RETURNS pg_catalog.jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    t      pg_catalog.record;
    v_rel  pg_catalog.regclass;
    v_part pg_catalog.regclass;
    v_pk   pg_catalog.text[];
    v_cols pg_catalog.text[];
    v_dist pg_catalog.name;
    v_pm   pg_catalog.text;
    v_kind pg_catalog."char";
    v_am   pg_catalog.oid;
    v_opts pg_catalog.text[];
    v_rows pg_catalog.float8;
    v_out  pg_catalog.jsonb := '[]'::pg_catalog.jsonb;
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR t IN SELECT * FROM kingres.internal_move_tables(p_colocation) ORDER BY ord LOOP
        v_rel := pg_catalog.format('%I.%I', t.schema_name, t.table_name)::pg_catalog.regclass;
        SELECT d.dist_column, d.partition_method INTO v_dist, v_pm FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_rel;
        IF v_pm IS NOT NULL THEN
            RAISE EXCEPTION 'table % is partitioned by the user: a split of its buckets is not supported yet', v_rel USING ERRCODE = 'feature_not_supported';
        END IF;
        v_part := pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, kingres.internal_bucket_part_name(t.table_name, p_bucket)));
        IF v_part IS NULL THEN
            RAISE EXCEPTION 'the partition of bucket % of table % is not on this node', p_bucket, v_rel USING ERRCODE = 'object_not_in_prerequisite_state';
        END IF;
        SELECT c.relkind, c.relam, c.reloptions, c.reltuples INTO v_kind, v_am, v_opts, v_rows FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) v_part::pg_catalog.oid;
        IF v_kind OPERATOR(pg_catalog.<>) 'r' THEN
            RAISE EXCEPTION 'the partition % is itself partitioned: a split of its bucket is not supported yet', v_part USING ERRCODE = 'feature_not_supported';
        END IF;
        IF v_am OPERATOR(pg_catalog.<>) (SELECT a.oid FROM pg_catalog.pg_am a WHERE a.amname OPERATOR(pg_catalog.=) 'heap') THEN
            RAISE EXCEPTION 'bucket split refused: table "%" is columnar; a split needs a primary key, row locks and update triggers on the table, which columnar tables do not have', v_rel
                USING ERRCODE = 'feature_not_supported',
                      HINT = 'Convert the table to row storage first.';
        END IF;
        SELECT pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY k.ord) INTO v_pk
          FROM pg_catalog.pg_index i
          CROSS JOIN LATERAL pg_catalog.unnest(i.indkey::pg_catalog.int2[]) WITH ORDINALITY k(attnum, ord)
          JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) i.indrelid AND a.attnum OPERATOR(pg_catalog.=) k.attnum
         WHERE i.indrelid OPERATOR(pg_catalog.=) v_rel::pg_catalog.oid AND i.indisprimary;
        IF v_pk IS NULL THEN
            RAISE EXCEPTION 'table % has no primary key: a split matches the rows of the children by key', v_rel USING ERRCODE = 'object_not_in_prerequisite_state';
        END IF;
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) v_rel::pg_catalog.oid AND a.attnum OPERATOR(pg_catalog.>) 0
                      AND NOT a.attisdropped AND (a.attgenerated OPERATOR(pg_catalog.<>) '' OR a.attidentity OPERATOR(pg_catalog.<>) '')) THEN
            RAISE EXCEPTION 'table % has a generated or an identity column: a bucket of such a table cannot be split yet', v_rel USING ERRCODE = 'feature_not_supported';
        END IF;
        SELECT pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY a.attnum) INTO v_cols
          FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) v_rel::pg_catalog.oid AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped;
        v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object(
                    'ord', t.ord, 'schema', t.schema_name, 'table', t.table_name,
                    'part', kingres.internal_bucket_part_name(t.table_name, p_bucket), 'dist', v_dist,
                    'pk', pg_catalog.to_jsonb(v_pk), 'cols', pg_catalog.to_jsonb(v_cols),
                    'reloptions', COALESCE(pg_catalog.to_jsonb(v_opts), 'null'::pg_catalog.jsonb),
                    'rows', CASE WHEN v_rows OPERATOR(pg_catalog.>) 0 THEN v_rows::pg_catalog.int8 ELSE 0 END));
    END LOOP;
    RETURN v_out;
END;
$$;

-- ---------------------------------------------------------------------------
-- Phase 7 step 25: a ledger table is refused explicitly (SQLSTATE KS016)
-- ---------------------------------------------------------------------------

-- internal_raise_blockers(): the blockers of a call become one error, as before; when one of them is a ledger table (or the history table of
-- one), the error is its own: SQLSTATE KS016, the message says what the table is and that it stays where it is, and the detail lists every
-- blocker with its code.  The calls of create_distributed_table() and create_reference_table() raise it, the online variants too.
CREATE OR REPLACE FUNCTION kingres.internal_raise_blockers(p_kind text, p_relid regclass, p_options jsonb)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_rows  pg_catalog.text[];
    v_codes pg_catalog.text[];
    v_n     pg_catalog.int4;
    v_state pg_catalog.text := 'feature_not_supported';
    v_detail pg_catalog.text;
BEGIN
    SELECT pg_catalog.array_agg(t.detail ORDER BY t.o), pg_catalog.array_agg(t.code ORDER BY t.o)
      INTO v_rows, v_codes
      FROM (SELECT r.detail, r.code, pg_catalog.row_number() OVER () AS o
              FROM kingres.table_change_report(p_kind, p_relid, p_options) r
             WHERE r.severity OPERATOR(pg_catalog.=) 'blocker') t;
    v_n := COALESCE(pg_catalog.cardinality(v_rows), 0);
    IF v_n OPERATOR(pg_catalog.=) 0 THEN
        RETURN;
    END IF;
    v_detail := (SELECT pg_catalog.string_agg(pg_catalog.format('%s: %s', v_codes[i], v_rows[i]), E'\n' ORDER BY i)
                   FROM pg_catalog.generate_subscripts(v_rows, 1) AS i);
    IF 'ledger_table' OPERATOR(pg_catalog.=) ANY (v_codes) THEN
        RAISE EXCEPTION '%', pg_catalog.format('cannot %s: table "%s" is a ledger table (or the history table of one) and stays where it is',
                                               CASE WHEN p_kind OPERATOR(pg_catalog.=) 'distribute_reference' THEN 'make it a reference table' ELSE 'distribute it' END,
                                               p_relid)
            USING ERRCODE = 'KS016',
                  DETAIL = v_detail,
                  HINT = 'A ledger table keeps its chain, its capture trigger and its protection on one node.  Distribute a table that is not under the ledger, or keep this one as an ordinary table.';
    END IF;
    IF v_n OPERATOR(pg_catalog.=) 1 THEN
        v_state := CASE v_codes[1]
                       WHEN 'already_distributed' THEN 'duplicate_object'
                       WHEN 'not_ordinary_table' THEN 'wrong_object_type'
                       WHEN 'column_missing' THEN 'undefined_column'
                       WHEN 'unique_without_distribution_column' THEN 'invalid_table_definition'
                       WHEN 'name_too_long' THEN 'invalid_name'
                       WHEN 'not_on_map_group' THEN 'object_not_in_prerequisite_state'
                       WHEN 'group_busy' THEN 'object_in_use'
                       ELSE 'feature_not_supported' END;
        RAISE EXCEPTION '%', v_rows[1] USING ERRCODE = v_state;
    END IF;
    RAISE EXCEPTION '% problems prevent the change of table "%"', v_n, p_relid
        USING ERRCODE = 'feature_not_supported',
              DETAIL = v_detail,
              HINT = 'kingres.table_change_report() lists them with the warnings and notes.';
END;
$$;

-- check_cluster(): a ledger table (or the history table of one) that is also a distributed table or a reference table.  ledger_enable runs on
-- one node; a reference table has a copy on every node, so a ledger on one of them covers one copy only, and a distributed table cannot be one.
-- Node-local: the names of the tables of this node that kingres_ledger registered and that the catalog of the cluster distributes, as a JSON array.
CREATE FUNCTION kingres.internal_ledger_distributed()
    RETURNS pg_catalog.jsonb
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_out pg_catalog.jsonb;
    v_hist pg_catalog.text := '';
BEGIN
    IF pg_catalog.to_regclass('kingres.ledger_table') IS NULL THEN
        RETURN '[]'::pg_catalog.jsonb;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
                WHERE a.attrelid OPERATOR(pg_catalog.=) 'kingres.ledger_table'::pg_catalog.regclass
                  AND a.attname OPERATOR(pg_catalog.=) 'history_relid' AND NOT a.attisdropped) THEN
        v_hist := ' UNION SELECT t.history_relid::pg_catalog.oid FROM kingres.ledger_table t WHERE t.history_relid IS NOT NULL';
    END IF;
    EXECUTE pg_catalog.format(
        'SELECT COALESCE(pg_catalog.jsonb_agg(x.o::pg_catalog.regclass::pg_catalog.text ORDER BY x.o::pg_catalog.regclass::pg_catalog.text), ''[]''::pg_catalog.jsonb) '
        'FROM (SELECT t.relid::pg_catalog.oid AS o FROM kingres.ledger_table t%s) x '
        'WHERE EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.method OPERATOR(pg_catalog.<>) ''schema'' '
        'AND (d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) x.o OR d.relid::pg_catalog.oid OPERATOR(pg_catalog.=) COALESCE(pg_catalog.pg_partition_root(x.o::pg_catalog.regclass)::pg_catalog.oid, 0::pg_catalog.oid)))',
        v_hist)
       INTO v_out;
    RETURN v_out;
END;
$$;
COMMENT ON FUNCTION kingres.internal_ledger_distributed() IS 'Node-local: the ledger tables of this node that are distributed tables or reference tables, as a JSON array';
REVOKE ALL ON FUNCTION kingres.internal_ledger_distributed() FROM PUBLIC;

CREATE OR REPLACE FUNCTION kingres.internal_check_ledger(p_skip int[] DEFAULT '{}')
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    n pg_catalog.record;
    r pg_catalog.jsonb;
    e pg_catalog.text;
BEGIN
    IF pg_catalog.to_regclass('kingres.dist_schema') IS NULL THEN
        RETURN;
    END IF;
    FOR n IN SELECT nd.node_id AS id FROM kingres.node nd WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active
                AND NOT (nd.node_id OPERATOR(pg_catalog.=) ANY (p_skip)) ORDER BY nd.node_id LOOP
        BEGIN
            r := kingres.internal_node_exec(n.id, 'SELECT kingres.internal_ledger_misplaced()::pg_catalog.text');
            r := (((r OPERATOR(pg_catalog.->) 0) OPERATOR(pg_catalog.->>) 0))::pg_catalog.jsonb;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            CONTINUE;
        END;
        FOR e IN SELECT x FROM pg_catalog.jsonb_array_elements_text(r) x LOOP
            check_name := 'ledger_on_non_owner'; node_id := n.id; object := e;
            detail := 'the table is a ledger table of this node, and its schema is distributed and held by another shard group: its rows are on the owner, so this ledger covers none of them';
            RETURN NEXT;
        END LOOP;
        BEGIN
            r := kingres.internal_node_exec(n.id, 'SELECT kingres.internal_ledger_distributed()::pg_catalog.text');
            r := (((r OPERATOR(pg_catalog.->) 0) OPERATOR(pg_catalog.->>) 0))::pg_catalog.jsonb;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            CONTINUE;
        END;
        FOR e IN SELECT x FROM pg_catalog.jsonb_array_elements_text(r) x LOOP
            check_name := 'ledger_distributed_table'; node_id := n.id; object := e;
            detail := 'the table is a ledger table of this node and also a distributed or reference table: a ledger covers the rows of one node only, so this one proves nothing about the table as a whole (kingres_shard refuses to distribute a ledger table; the ledger was enabled afterwards)';
            RETURN NEXT;
        END LOOP;
    END LOOP;
END;
$$;

-- internal_move_tree(): the answer for a user-partitioned bucket also carries the columnar options of each leaf.
CREATE OR REPLACE FUNCTION kingres.internal_move_tree(p_oid oid)
    RETURNS jsonb
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.jsonb_build_object('reloptions', pg_catalog.to_jsonb(kingres.internal_table_options(c.oid))) OPERATOR(pg_catalog.||) CASE WHEN c.relkind OPERATOR(pg_catalog.=) 'p'
            THEN pg_catalog.jsonb_build_object(
                     'partby', pg_catalog.pg_get_partkeydef(c.oid),
                     'leaves', COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                                                'name', lc.relname, 'bound', pg_catalog.pg_get_expr(lc.relpartbound, lc.oid), 'am', COALESCE(a.amname, 'heap'), 'opts', kingres.internal_columnar_args(lc.oid::pg_catalog.regclass))
                                            ORDER BY lc.relname)
                                           FROM pg_catalog.pg_inherits i JOIN pg_catalog.pg_class lc ON lc.oid OPERATOR(pg_catalog.=) i.inhrelid
                                           LEFT JOIN pg_catalog.pg_am a ON a.oid OPERATOR(pg_catalog.=) lc.relam
                                          WHERE i.inhparent OPERATOR(pg_catalog.=) c.oid), '[]'::pg_catalog.jsonb))
            ELSE '{}'::pg_catalog.jsonb END
  FROM pg_catalog.pg_class c WHERE c.oid OPERATOR(pg_catalog.=) p_oid;
$$;

-- S3: internal_create_move_tables(): the copy of a bucket partition is made with the access method of the source partition (design 11.6).
CREATE OR REPLACE FUNCTION kingres.internal_create_move_tables(p_move bigint, p_bucket int, p_bucket_count int, p_tables jsonb)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    t       pg_catalog.record;
    v_part  pg_catalog.text;
    v_col   pg_catalog.text;
    v_owner pg_catalog.text;
    v_parent pg_catalog.regclass;
    l       pg_catalog.record;
BEGIN
    PERFORM kingres.internal_require_superuser();
    FOR t IN SELECT x.schema_name, x.table_name, x.replident, x.partby, x.leaves, x.reloptions, x.am, x.opts
               FROM pg_catalog.jsonb_to_recordset(p_tables) AS x(schema_name text, table_name text, replident text, partby text, leaves jsonb, reloptions text[], am text, opts text)
    LOOP
        v_part := kingres.internal_bucket_part_name(t.table_name, p_bucket);
        v_parent := pg_catalog.format('%I.%I', t.schema_name, t.table_name)::pg_catalog.regclass;
        SELECT d.dist_column INTO v_col FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) v_parent;
        SELECT r.rolname INTO v_owner FROM pg_catalog.pg_class c JOIN pg_catalog.pg_roles r ON r.oid OPERATOR(pg_catalog.=) c.relowner
         WHERE c.oid OPERATOR(pg_catalog.=) v_parent::pg_catalog.oid;
        IF pg_catalog.to_regclass(pg_catalog.format('%I.%I', t.schema_name, v_part)) IS NOT NULL THEN
            CONTINUE;	-- made by an earlier attempt of this move
        END IF;
        -- LIKE copies neither the storage parameters nor the access method: they are those of the bucket partition on the source
        EXECUTE pg_catalog.format('CREATE TABLE %I.%I (LIKE %I.%I INCLUDING ALL)%s%s %s',
                                  t.schema_name, v_part, t.schema_name, t.table_name,
                                  CASE WHEN t.partby IS NOT NULL THEN ' PARTITION BY ' OPERATOR(pg_catalog.||) t.partby ELSE '' END,
                                  CASE WHEN t.am IS NOT NULL THEN pg_catalog.format(' USING %I', t.am) ELSE '' END,
                                  kingres.internal_with_clause(t.reloptions));
        IF t.opts IS NOT NULL AND t.partby IS NULL THEN
            EXECUTE pg_catalog.format('SELECT kingres.columnar_set_options(%L::pg_catalog.regclass, %s)', pg_catalog.format('%I.%I', t.schema_name, v_part), t.opts);
        END IF;
        IF t.partby IS NOT NULL THEN
            -- a user-partitioned bucket partition: the copy is the same tree, a leaf per user partition with its bound and its access method
            FOR l IN SELECT y.name, y.bound, y.am, y.opts FROM pg_catalog.jsonb_to_recordset(t.leaves) AS y(name text, bound text, am text, opts text) ORDER BY y.name LOOP
                EXECUTE pg_catalog.format('CREATE TABLE %I.%I PARTITION OF %I.%I %s USING %I', t.schema_name, l.name, t.schema_name, v_part, l.bound, l.am);
                IF l.am OPERATOR(pg_catalog.<>) 'heap' THEN
                    -- a leaf that is not heap names its rows for a later move the way the bucket partitions of the table do
                    EXECUTE pg_catalog.format('ALTER TABLE %I.%I REPLICA IDENTITY FULL', t.schema_name, l.name);
                END IF;
                IF l.opts IS NOT NULL THEN
                    EXECUTE pg_catalog.format('SELECT kingres.columnar_set_options(%L::pg_catalog.regclass, %s)', pg_catalog.format('%I.%I', t.schema_name, l.name), l.opts);
                END IF;
            END LOOP;
        END IF;
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I ADD CONSTRAINT ks_bucket_check CHECK '
                                  '(pg_catalog.satisfies_hash_partition(%L::pg_catalog.regclass, %s, %s, %I))',
                                  t.schema_name, v_part, v_parent::pg_catalog.text, p_bucket_count, p_bucket, v_col);
        IF t.replident OPERATOR(pg_catalog.=) 'f' THEN
            EXECUTE pg_catalog.format('ALTER TABLE %I.%I REPLICA IDENTITY FULL', t.schema_name, v_part);
        END IF;
        EXECUTE pg_catalog.format('ALTER TABLE %I.%I OWNER TO %I', t.schema_name, v_part, v_owner);
        IF t.partby IS NOT NULL THEN
            FOR l IN SELECT y.name FROM pg_catalog.jsonb_to_recordset(t.leaves) AS y(name text) LOOP
                EXECUTE pg_catalog.format('ALTER TABLE %I.%I OWNER TO %I', t.schema_name, l.name, v_owner);
            END LOOP;
        END IF;
        EXECUTE pg_catalog.format('COMMENT ON TABLE %I.%I IS %L', t.schema_name, v_part,
                                  'kingres_shard bucket move ' OPERATOR(pg_catalog.||) p_move::pg_catalog.text);
    END LOOP;
END;
$$;

-- ---------------------------------------------------------------------------
-- The leftovers of phase 11 (design/shard-phase11.md 9.9 and 9.10): the schemas in the rebalance plan and the drain, and the schema checks of
-- check_cluster.  internal_plan_inputs() and check_cluster() are replaced; the functions of the block below are new.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION kingres.internal_plan_inputs(p_strategy text, p_extra jsonb, p_exclude int[], p_colos int[])
    RETURNS jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_groups pg_catalog.jsonb;
    v_items  pg_catalog.jsonb;
    v_min    pg_catalog.int8;
    v_sizes  pg_catalog.jsonb := '{}';
    v_pinned pg_catalog.jsonb := '[]';
    g        pg_catalog.record;
    v_node   pg_catalog.int4;
BEGIN
    IF p_strategy IS NULL OR p_strategy NOT IN ('by_bucket_count', 'by_disk_size') THEN
        RAISE EXCEPTION 'unknown rebalance strategy "%" (by_bucket_count or by_disk_size)', p_strategy
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_extra IS NOT NULL AND pg_catalog.jsonb_typeof(p_extra) IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'extra_load must be a JSON object of group id to load' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    v_min := (SELECT s.setting::pg_catalog.int8 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.min_bucket_cost') * 1024;

    SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
               'id', x.group_id, 'avail', x.node_id IS NOT NULL,
               'recv', x.node_id IS NOT NULL AND x.should AND NOT (x.group_id OPERATOR(pg_catalog.=) ANY (COALESCE(p_exclude, '{}'::pg_catalog.int4[]))),
               'w', COALESCE(x.weight, 1),
               'extra', CASE WHEN x.node_id IS NOT NULL AND x.should AND NOT (x.group_id OPERATOR(pg_catalog.=) ANY (COALESCE(p_exclude, '{}'::pg_catalog.int4[])))
                             THEN COALESCE((p_extra OPERATOR(pg_catalog.->>) x.group_id::pg_catalog.text)::pg_catalog.float8, 0) ELSE 0 END)
               ORDER BY x.group_id), '[]'::pg_catalog.jsonb)
      INTO v_groups
      FROM (SELECT gr.group_id,
                   (SELECT n.node_id FROM kingres.node n WHERE n.group_id OPERATOR(pg_catalog.=) gr.group_id AND n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active) AS node_id,
                   COALESCE((SELECT n.should_have_shards FROM kingres.node n WHERE n.group_id OPERATOR(pg_catalog.=) gr.group_id AND n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active), false) AS should,
                   (SELECT n.weight FROM kingres.node n WHERE n.group_id OPERATOR(pg_catalog.=) gr.group_id AND n.role OPERATOR(pg_catalog.=) 'primary' AND n.is_active) AS weight
              FROM (SELECT n.group_id FROM kingres.node n UNION SELECT m.group_id FROM kingres.bucket_map m) gr) x;

    IF p_strategy OPERATOR(pg_catalog.=) 'by_disk_size' THEN
        PERFORM kingres.internal_require_superuser();
        -- the sizes of the bucket partitions of every table of every colocation group, from the node that owns them now
        FOR g IN SELECT DISTINCT m.group_id FROM kingres.bucket_map m ORDER BY m.group_id LOOP
            v_node := kingres.internal_group_node(g.group_id);
            IF v_node IS NULL THEN
                CONTINUE;           -- a group without a primary is not planned
            END IF;
            BEGIN
                v_sizes := v_sizes OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_object(g.group_id::pg_catalog.text,
                    (kingres.internal_node_exec(v_node,
                        'SELECT COALESCE(pg_catalog.jsonb_object_agg(s.k, s.sz), ''{}''::pg_catalog.jsonb)::pg_catalog.text FROM ('
                        'SELECT d.colocation_id::pg_catalog.text OPERATOR(pg_catalog.||) '':'' OPERATOR(pg_catalog.||) b.bucket::pg_catalog.text AS k, '
                        'pg_catalog.sum(b.partition_size)::pg_catalog.int8 AS sz '
                        'FROM kingres.buckets b JOIN kingres.dist_table d ON d.relid OPERATOR(pg_catalog.=) b.table_name '
                        'WHERE b.partition_size IS NOT NULL GROUP BY 1 '
                        -- the one bucket of a distributed schema is its tables (P10R-09): the size is the tables' own, where the node holds them
                        'UNION ALL SELECT d.colocation_id::pg_catalog.text OPERATOR(pg_catalog.||) '':0'', pg_catalog.sum(pg_catalog.pg_total_relation_size(d.relid))::pg_catalog.int8 '
                        'FROM kingres.dist_table d WHERE d.method OPERATOR(pg_catalog.=) ''schema'' GROUP BY d.colocation_id) s',
                        10000) OPERATOR(pg_catalog.->) 0 OPERATOR(pg_catalog.->>) 0)::pg_catalog.jsonb);
            EXCEPTION WHEN OTHERS THEN
                RAISE EXCEPTION 'the sizes of the buckets of shard group % cannot be read (%): a plan by disk size does not guess', g.group_id, SQLERRM
                    USING ERRCODE = 'object_not_in_prerequisite_state';
            END;
        END LOOP;
    END IF;

    -- The schemas that a plan must not move (a ledger table pins them, design 9.9): the owner knows.  Only a superuser can ask the nodes; for any other
    -- caller the plan does not know the pins, and the move of a pinned schema is refused when it is tried.  A group that does not answer is not planned either.
    IF pg_catalog.current_setting('is_superuser') OPERATOR(pg_catalog.=) 'on' THEN
        FOR g IN SELECT DISTINCT m.group_id FROM kingres.bucket_map m JOIN kingres.colocation c ON c.colocation_id OPERATOR(pg_catalog.=) m.colocation_id
                  WHERE c.kind OPERATOR(pg_catalog.=) 'schema' ORDER BY m.group_id LOOP
            v_node := kingres.internal_group_node(g.group_id);
            IF v_node IS NULL THEN
                CONTINUE;
            END IF;
            BEGIN
                v_pinned := v_pinned OPERATOR(pg_catalog.||)
                    (((kingres.internal_node_exec(v_node, 'SELECT kingres.internal_pinned_schemas()::pg_catalog.text', 10000) OPERATOR(pg_catalog.->) 0) OPERATOR(pg_catalog.->>) 0))::pg_catalog.jsonb;
            EXCEPTION WHEN OTHERS THEN
                NULL;
            END;
        END LOOP;
    END IF;

    SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
               'c', q.colocation_id, 'b', q.bucket, 'owner', q.owner, 'phys', q.group_id,
               'mov', q.mov, 'size', q.size,
               'cost', CASE WHEN p_strategy OPERATOR(pg_catalog.=) 'by_disk_size' THEN (CASE WHEN COALESCE(q.size, 0) OPERATOR(pg_catalog.>) v_min THEN q.size ELSE v_min END) ELSE 1 END)
               ORDER BY q.colocation_id, q.bucket), '[]'::pg_catalog.jsonb)
      INTO v_items
      FROM (SELECT m.colocation_id, m.bucket, m.group_id,
                   COALESCE((SELECT b.target_group FROM kingres.bucket_move b
                              WHERE b.colocation_id OPERATOR(pg_catalog.=) m.colocation_id AND b.bucket OPERATOR(pg_catalog.=) m.bucket
                                AND b.phase IN ('preparing', 'copying', 'catching_up', 'switching')), m.group_id) AS owner,
                   m.state OPERATOR(pg_catalog.=) 'active' AND (p_colos IS NULL OR m.colocation_id OPERATOR(pg_catalog.=) ANY (p_colos))
                   -- the bucket of a distributed schema is an item like any other (design 9.9), unless a ledger table pins the schema: it counts in the
                   -- load of its group and stays
                   AND NOT (v_pinned OPERATOR(pg_catalog.@>) pg_catalog.jsonb_build_array(m.colocation_id)) AS mov,
                   (v_sizes OPERATOR(pg_catalog.->) m.group_id::pg_catalog.text OPERATOR(pg_catalog.->>) (m.colocation_id::pg_catalog.text OPERATOR(pg_catalog.||) ':' OPERATOR(pg_catalog.||) m.bucket::pg_catalog.text))::pg_catalog.int8 AS size
              FROM kingres.bucket_map m) q;
    RETURN pg_catalog.jsonb_build_object('groups', v_groups, 'items', v_items);
END;
$$;

CREATE OR REPLACE FUNCTION kingres.check_cluster(p_prepared_older_than interval DEFAULT interval '5 minutes')
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    b      pg_catalog.record;
    v_skip pg_catalog.int4[] := '{}';
BEGIN
    -- the base check counts the buckets of a group against its bucket_count; the layout check does it for groups with split buckets too.
    -- The checks of the layout and of the splits ask every node: they leave out a node that the base check found not answering (each call
    -- to such a node costs a node_timeout, and check_cluster has a bound, README "Bounds").
    FOR b IN SELECT x.check_name, x.node_id, x.object, x.detail FROM kingres.internal_check_cluster_base(p_prepared_older_than) x LOOP
        IF b.check_name OPERATOR(pg_catalog.=) 'node_unreachable' AND b.node_id IS NOT NULL THEN
            v_skip := v_skip OPERATOR(pg_catalog.||) b.node_id;
        END IF;
        IF b.check_name OPERATOR(pg_catalog.<>) 'bucket_map_incomplete' THEN
            check_name := b.check_name; node_id := b.node_id; object := b.object; detail := b.detail;
            RETURN NEXT;
        END IF;
    END LOOP;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_layout() t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_layout_partitions(v_skip) t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_splits(v_skip) t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_ledger(v_skip) t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_schemas(v_skip) t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_table_operations(v_skip) t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_partitions() t;
    RETURN QUERY SELECT t.check_name, t.node_id, t.object, t.detail FROM kingres.internal_check_leaf_conversions() t;
END;
$$;

-- =========================================================================
-- PHASE 11 (distributed schemas), the leftovers of steps 6 and 15 of the design: the schema items of the rebalance and the drain (9.9) and the checks
-- of check_cluster (9.10)   [begin]
-- =========================================================================

-- Node-local: the colocation groups of the distributed schemas that a ledger table pins on this node, as a JSON array.  The plan of a rebalance
-- asks the owners (a pinned schema is not an item of the plan, 9.9).
CREATE FUNCTION kingres.internal_pinned_schemas()
    RETURNS pg_catalog.jsonb
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT COALESCE(pg_catalog.jsonb_agg(s.colocation_id ORDER BY s.colocation_id), '[]'::pg_catalog.jsonb)
  FROM kingres.dist_schema s
 WHERE kingres.internal_schema_pinned(s.colocation_id) IS NOT NULL;
$$;

-- Node-local: the distributed schemas whose bucket this node's group owns, as a JSON array of {colocation_id, size, blocker}: the bytes of their tables
-- (tables, indexes, TOAST) and what keeps them from moving (a ledger pin, an unlogged table, a table with neither a key nor a full replica identity), or null.
CREATE FUNCTION kingres.internal_schemas_report()
    RETURNS pg_catalog.jsonb
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
           'colocation_id', s.colocation_id,
           'size', (SELECT COALESCE(pg_catalog.sum(pg_catalog.pg_total_relation_size(d.relid)), 0)
                      FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND d.method OPERATOR(pg_catalog.=) 'schema'),
           'blocker', kingres.internal_schema_precheck(s.colocation_id)) ORDER BY s.colocation_id), '[]'::pg_catalog.jsonb)
  FROM kingres.dist_schema s
  JOIN kingres.bucket_map b ON b.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND b.bucket OPERATOR(pg_catalog.=) 0
  JOIN kingres.node n ON n.group_id OPERATOR(pg_catalog.=) b.group_id AND n.node_id OPERATOR(pg_catalog.=) (SELECT l.node_id FROM kingres.local_node l);
$$;

-- The distributed schemas that a shard group holds, with their sizes and what would keep each from moving: what a drain of the group has to move, and what
-- would stop it (design 9.9, the preview of a drain).  A group that does not answer gives its schemas with a null size and the reason in blocker.
-- Superusers only (it asks the owner).
CREATE FUNCTION kingres.schemas_on_group(p_group int)
    RETURNS TABLE (schema_name text, colocation_id int, state text, size_bytes bigint, blocker text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_node   pg_catalog.int4 := kingres.internal_group_node(p_group);
    v_rep    pg_catalog.jsonb;
    v_err    pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF v_node IS NULL THEN
        v_err := pg_catalog.format('shard group %s has no active primary: its schemas cannot be asked', p_group);
    ELSE
        BEGIN
            v_rep := (((kingres.internal_node_exec(v_node, 'SELECT kingres.internal_schemas_report()::pg_catalog.text', 10000) OPERATOR(pg_catalog.->) 0) OPERATOR(pg_catalog.->>) 0))::pg_catalog.jsonb;
        EXCEPTION WHEN OTHERS THEN
            v_err := pg_catalog.format('shard group %s did not answer: %s', p_group, SQLERRM);
        END;
    END IF;
    RETURN QUERY
    SELECT ns.nspname::pg_catalog.text, s.colocation_id, m.state,
           (r.size)::pg_catalog.int8,
           COALESCE(v_err, r.blocker)
      FROM kingres.dist_schema s
      JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) s.nsp::pg_catalog.oid
      JOIN kingres.bucket_map m ON m.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND m.bucket OPERATOR(pg_catalog.=) 0
      LEFT JOIN pg_catalog.jsonb_to_recordset(COALESCE(v_rep, '[]'::pg_catalog.jsonb)) AS r(colocation_id int, size int8, blocker text)
             ON r.colocation_id OPERATOR(pg_catalog.=) s.colocation_id
     WHERE m.group_id OPERATOR(pg_catalog.=) p_group
     ORDER BY ns.nspname;
END;
$$;

-- Node-local, for check_cluster (design 9.10): the digest of what this node says about the distributed schemas in its catalog, by names: each schema
-- with the group and the state of its bucket, the placement epoch of its colocation group and the number of its schema tables.  The map group primary
-- compares its own digest with the one of every node.
CREATE FUNCTION kingres.internal_schema_catalog_digest()
    RETURNS text
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(COALESCE(pg_catalog.string_agg(
           pg_catalog.format('%s|%s|%s|%s|%s|%s', ns.nspname, s.colocation_id, b.group_id, b.state, c.placement_epoch,
                             (SELECT pg_catalog.count(*) FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND d.method OPERATOR(pg_catalog.=) 'schema')),
           ';' ORDER BY ns.nspname), ''), 'UTF8')), 'hex')
  FROM kingres.dist_schema s
  JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) s.nsp::pg_catalog.oid
  JOIN kingres.bucket_map b ON b.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND b.bucket OPERATOR(pg_catalog.=) 0
  JOIN kingres.colocation c ON c.colocation_id OPERATOR(pg_catalog.=) s.colocation_id;
$$;

-- Node-local, for check_cluster: what is wrong on this node with the distributed schemas, given what the map group primary expects
-- (p_expect: {"digest": ..., "schemas": [{"nsp", "owner", "busy": [groups], "tables": [{"q", "fp"}]}]}).  A JSON array of {check, object, detail}.
CREATE FUNCTION kingres.internal_check_schemas_local(p_expect pg_catalog.jsonb)
    RETURNS pg_catalog.jsonb
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_group pg_catalog.int4;
    v_out   pg_catalog.jsonb := '[]'::pg_catalog.jsonb;
    e       pg_catalog.record;
    t       pg_catalog.record;
    v_rel   pg_catalog.regclass;
    v_has   pg_catalog.bool;
    v_fp    pg_catalog.text;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM pg_catalog.set_config('kingres_shard.enable_routing', 'off', true);
    SELECT n.group_id INTO v_group FROM kingres.local_node l JOIN kingres.node n ON n.node_id OPERATOR(pg_catalog.=) l.node_id;
    FOR e IN SELECT * FROM pg_catalog.jsonb_to_recordset(p_expect OPERATOR(pg_catalog.->) 'schemas') AS x(nsp text, owner int, busy jsonb, tables jsonb) LOOP
        IF pg_catalog.to_regnamespace(pg_catalog.quote_ident(e.nsp)) IS NULL THEN
            v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object('check', 'schema_missing', 'object', e.nsp,
                        'detail', 'the schema is a distributed schema of the map group and does not exist on this node'));
            CONTINUE;
        END IF;
        FOR t IN SELECT * FROM pg_catalog.jsonb_to_recordset(e.tables) AS y(q text, fp text) LOOP
            v_rel := pg_catalog.to_regclass(t.q);
            IF v_rel IS NULL THEN
                v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object('check', 'schema_table_missing', 'object', t.q,
                            'detail', 'a table of a distributed schema does not exist on this node'));
                CONTINUE;
            END IF;
            v_fp := kingres.internal_table_fingerprint(v_rel);
            IF v_fp IS DISTINCT FROM t.fp THEN
                v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object('check', 'schema_structure_differs', 'object', t.q,
                            'detail', 'the structure of the table on this node is not the structure on the map group primary (columns, constraints, indexes, triggers)'));
            END IF;
            -- the empty copy of a table that another group holds: no row, except while a move of the schema is on its way to or from this group
            IF v_group OPERATOR(pg_catalog.<>) e.owner AND NOT (e.busy OPERATOR(pg_catalog.@>) pg_catalog.jsonb_build_array(v_group)) THEN
                EXECUTE pg_catalog.format('SELECT EXISTS (SELECT 1 FROM %s)', v_rel) INTO v_has;
                IF v_has THEN
                    v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object('check', 'stray_schema_rows', 'object', t.q,
                                'detail', pg_catalog.format('the table holds rows on shard group %s, and the schema is held by shard group %s: a statement that reached the empty copy wrote there', v_group, e.owner)));
                END IF;
            END IF;
        END LOOP;
    END LOOP;
    IF kingres.internal_schema_catalog_digest() IS DISTINCT FROM (p_expect OPERATOR(pg_catalog.->>) 'digest') THEN
        v_out := v_out OPERATOR(pg_catalog.||) pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object('check', 'schema_catalog_differs', 'object', NULL,
                    'detail', 'the rows of dist_schema and of the schema tables of dist_table (schema, colocation group, group, state, placement epoch, number of tables) differ from the map group primary'));
    END IF;
    RETURN v_out;
END;
$$;

-- check_cluster on the map group primary (design 9.10): what every active primary says about the distributed schemas.  A structural check: it does
-- not compare the rows of a copy with its source (the check of a move does not either, review S4-10).
CREATE FUNCTION kingres.internal_check_schemas(p_skip int[] DEFAULT '{}')
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_expect pg_catalog.jsonb;
    n        pg_catalog.record;
    r        pg_catalog.jsonb;
    f        pg_catalog.record;
BEGIN
    IF pg_catalog.to_regclass('kingres.dist_schema') IS NULL OR NOT EXISTS (SELECT 1 FROM kingres.dist_schema) THEN
        RETURN;
    END IF;
    SELECT pg_catalog.jsonb_build_object(
               'digest', kingres.internal_schema_catalog_digest(),
               'schemas', COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                   'nsp', ns.nspname, 'owner', b.group_id,
                   'busy', (SELECT COALESCE(pg_catalog.jsonb_agg(DISTINCT g.grp), '[]'::pg_catalog.jsonb)
                              FROM kingres.bucket_move mv
                              CROSS JOIN LATERAL (VALUES (mv.source_group), (mv.target_group)) g(grp)
                             WHERE mv.colocation_id OPERATOR(pg_catalog.=) s.colocation_id
                               AND mv.phase IN ('preparing', 'copying', 'catching_up', 'switching', 'cleaning')),
                   'tables', (SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
                                         'q', d.relid::pg_catalog.regclass::pg_catalog.text, 'fp', kingres.internal_table_fingerprint(d.relid)) ORDER BY d.relid::pg_catalog.oid), '[]'::pg_catalog.jsonb)
                                FROM kingres.dist_table d WHERE d.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND d.method OPERATOR(pg_catalog.=) 'schema')
                   ) ORDER BY ns.nspname), '[]'::pg_catalog.jsonb))
      INTO v_expect
      FROM kingres.dist_schema s
      JOIN pg_catalog.pg_namespace ns ON ns.oid OPERATOR(pg_catalog.=) s.nsp::pg_catalog.oid
      JOIN kingres.bucket_map b ON b.colocation_id OPERATOR(pg_catalog.=) s.colocation_id AND b.bucket OPERATOR(pg_catalog.=) 0;
    FOR n IN SELECT nd.node_id AS id FROM kingres.node nd WHERE nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active
                AND NOT (nd.node_id OPERATOR(pg_catalog.=) ANY (p_skip)) ORDER BY nd.node_id LOOP
        BEGIN
            r := kingres.internal_node_exec(n.id, pg_catalog.format('SELECT kingres.internal_check_schemas_local(%L::pg_catalog.jsonb)::pg_catalog.text', v_expect::pg_catalog.text), 60000);
            r := (((r OPERATOR(pg_catalog.->) 0) OPERATOR(pg_catalog.->>) 0))::pg_catalog.jsonb;
        EXCEPTION WHEN OTHERS OR query_canceled THEN
            CONTINUE;
        END;
        FOR f IN SELECT x."check" AS c, x.object AS o, x.detail AS d FROM pg_catalog.jsonb_to_recordset(r) AS x("check" text, object text, detail text) LOOP
            check_name := f.c; node_id := n.id; object := f.o; detail := f.d;
            RETURN NEXT;
        END LOOP;
    END LOOP;
END;
$$;

COMMENT ON FUNCTION kingres.internal_pinned_schemas() IS 'Node-local: the colocation groups of the distributed schemas that a ledger table pins here';
COMMENT ON FUNCTION kingres.internal_schemas_report() IS 'Node-local: the distributed schemas that this node''s group owns, with sizes and what keeps each from moving';
COMMENT ON FUNCTION kingres.schemas_on_group(int) IS 'The distributed schemas of a shard group with their sizes and what would keep each from moving (the preview of a drain); superusers only';
COMMENT ON FUNCTION kingres.internal_schema_catalog_digest() IS 'Node-local: the digest of the distributed schemas of this node''s catalog, by names';
COMMENT ON FUNCTION kingres.internal_check_schemas_local(jsonb) IS 'Node-local: what is wrong here with the distributed schemas, given what the map group primary expects';
REVOKE ALL ON FUNCTION
    kingres.internal_pinned_schemas(),
    kingres.internal_schemas_report(),
    kingres.schemas_on_group(int),
    kingres.internal_schema_catalog_digest(),
    kingres.internal_check_schemas_local(jsonb),
    kingres.internal_check_schemas(int[])
    FROM PUBLIC;

-- =========================================================================
-- PHASE 11, the leftovers                                                    [end]
-- =========================================================================

-- =========================================================================
-- Time partitions of distributed tables (design/analytics-phase12-storage.md, section 11; increments St-D1a and St-D1b)
-- =========================================================================
--
-- kingres_timepart 0.3 runs a time partitioning policy on a user-partitioned distributed table with the partition functions of
-- phase 9 (create_partition, drop_partition, detach_partition).  What it needs of this extension is here: a leaf budget that the
-- partition functions enforce, the storage of the leaves of a table on every node, and the claim of a colocation group that
-- keeps a bucket move and the other table operations away while the leaves of one partition are converted.

-- The leaves of a user-partitioned table: its buckets times its partitions, against kingres_shard.max_partition_leaves.
CREATE FUNCTION kingres.partition_leaf_budget(parent regclass)
    RETURNS TABLE (buckets int, partitions int, leaves bigint, max_leaves int)
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT b.n, p.n, (b.n::pg_catalog.int8 OPERATOR(pg_catalog.*) p.n::pg_catalog.int8),
       COALESCE(NULLIF(pg_catalog.current_setting('kingres_shard.max_partition_leaves', true), '')::pg_catalog.int4, 8192)
  FROM (SELECT pg_catalog.count(*)::pg_catalog.int4 AS n
          FROM kingres.bucket_map m JOIN kingres.dist_table d ON d.colocation_id OPERATOR(pg_catalog.=) m.colocation_id
         WHERE d.relid OPERATOR(pg_catalog.=) partition_leaf_budget.parent) b,
       (SELECT pg_catalog.count(*)::pg_catalog.int4 AS n FROM kingres.dist_partition x
         WHERE x.parent OPERATOR(pg_catalog.=) partition_leaf_budget.parent) p
 WHERE EXISTS (SELECT 1 FROM kingres.dist_table d2 WHERE d2.relid OPERATOR(pg_catalog.=) partition_leaf_budget.parent AND d2.partition_method IS NOT NULL);
$$;
COMMENT ON FUNCTION kingres.partition_leaf_budget(regclass) IS
    'The buckets, the user partitions, the leaves (their product) and kingres_shard.max_partition_leaves of a user-partitioned distributed table';

-- One more user partition must keep the table within kingres_shard.max_partition_leaves (create_distributed_table checks the same for the
-- partitions the table has when it is distributed).
CREATE FUNCTION kingres.internal_check_leaf_budget(p_parent regclass)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_b pg_catalog.record;
BEGIN
    SELECT * INTO v_b FROM kingres.partition_leaf_budget(p_parent);
    IF FOUND AND v_b.buckets::pg_catalog.int8 OPERATOR(pg_catalog.*) (v_b.partitions OPERATOR(pg_catalog.+) 1)::pg_catalog.int8 OPERATOR(pg_catalog.>) v_b.max_leaves::pg_catalog.int8 THEN
        RAISE EXCEPTION 'table "%" would have % leaves (% buckets, % partitions); kingres_shard.max_partition_leaves is %',
            p_parent, v_b.buckets::pg_catalog.int8 OPERATOR(pg_catalog.*) (v_b.partitions OPERATOR(pg_catalog.+) 1)::pg_catalog.int8, v_b.buckets, v_b.partitions OPERATOR(pg_catalog.+) 1, v_b.max_leaves
            USING ERRCODE = 'program_limit_exceeded',
                  HINT = 'Drop or detach old partitions, use a wider partition interval, or raise kingres_shard.max_partition_leaves.';
    END IF;
END;
$$;

-- create_partition and attach_partition check the budget before they make a leaf.
CREATE OR REPLACE FUNCTION kingres.create_partition(parent regclass, partition_name text, bound text, if_not_exists bool DEFAULT false)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    SET client_min_messages = warning
    AS $$
DECLARE
    c pg_catalog.record;
BEGIN
    SELECT * INTO c FROM kingres.internal_partition_context(parent);
    PERFORM kingres.internal_fault_point('partition_after_lock');
    IF partition_name IS NULL OR pg_catalog.octet_length(partition_name) OPERATOR(pg_catalog.>) 57 THEN
        RAISE EXCEPTION 'partition name "%" is too long or missing: leaf names append 6 characters (_bNNNN), at most 57 bytes', partition_name USING ERRCODE = 'invalid_name';
    END IF;
    IF EXISTS (SELECT 1 FROM kingres.dist_partition p WHERE p.parent OPERATOR(pg_catalog.=) create_partition.parent AND p.partition_name OPERATOR(pg_catalog.=) create_partition.partition_name) THEN
        IF if_not_exists THEN
            RETURN;
        END IF;
        RAISE EXCEPTION 'partition "%" of table "%" already exists', partition_name, parent USING ERRCODE = 'duplicate_table';
    END IF;
    IF pg_catalog.to_regclass(pg_catalog.format('%I.%I', c.nsp, partition_name)) IS NOT NULL THEN
        RAISE EXCEPTION 'relation "%" already exists', partition_name USING ERRCODE = 'duplicate_table';
    END IF;
    PERFORM kingres.internal_check_leaf_budget(parent);
    PERFORM kingres.internal_leaf_everywhere('create', c.colocation, c.nsp, c.rname, partition_name::pg_catalog.name, bound);
    PERFORM kingres.internal_fault_point('partition_after_leaves');
    INSERT INTO kingres.dist_partition (parent, partition_name, bound) VALUES (parent, partition_name, bound);
    PERFORM kingres.sync_catalog();
END;
$$;

CREATE OR REPLACE FUNCTION kingres.attach_partition(parent regclass, partition regclass, bound text)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    SET client_min_messages = warning
    AS $$
DECLARE
    c        pg_catalog.record;
    v_dt     kingres.dist_table%ROWTYPE;
    v_name   pg_catalog.name;
    v_nsp    pg_catalog.name;
    v_cnt    pg_catalog.int8;
    v_own    pg_catalog.text;
BEGIN
    SELECT * INTO c FROM kingres.internal_partition_context(parent);
    PERFORM kingres.internal_fault_point('partition_after_lock');
    SELECT * INTO v_dt FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) partition;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'table "%" is not a distributed table of the group of "%"', partition, parent USING ERRCODE = 'wrong_object_type',
              HINT = 'Create the partition with create_partition() and insert the rows into the parent.';
    END IF;
    IF v_dt.method OPERATOR(pg_catalog.<>) 'hash' OR v_dt.colocation_id OPERATOR(pg_catalog.<>) c.colocation OR v_dt.dist_column OPERATOR(pg_catalog.<>) c.dist_column OR v_dt.partition_method IS NOT NULL THEN
        RAISE EXCEPTION 'table "%" must be a distributed table of the colocation group of "%", on the same distribution column, and not partitioned', partition, parent USING ERRCODE = 'wrong_object_type';
    END IF;
    SELECT n.nspname, cl.relname INTO v_nsp, v_name FROM pg_catalog.pg_class cl JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) cl.relnamespace
     WHERE cl.oid OPERATOR(pg_catalog.=) partition::pg_catalog.oid;
    IF v_nsp OPERATOR(pg_catalog.<>) c.nsp THEN
        RAISE EXCEPTION 'the partition must be in the schema of the table' USING ERRCODE = 'invalid_schema_name';
    END IF;
    -- A bucket move makes the leaves again from the definition of the table and of its partitions: an index, a constraint, a default, a
    -- storage parameter, an owner or privileges that the table has of its own would be lost at the first move (P9C-03)
    SELECT pg_catalog.string_agg(m.what, '; ' ORDER BY m.what) INTO v_own FROM kingres.internal_attach_differences(parent, partition) m(what);
    IF v_own IS NOT NULL THEN
        RAISE EXCEPTION 'table "%" has objects of its own that table "%" does not have: %', partition, parent, v_own
            USING ERRCODE = 'feature_not_supported',
                  DETAIL = 'A bucket move creates the leaves of a partition from the definition of the partitioned table, so they would be lost.',
                  HINT = 'Give the table the same indexes and constraints, owner and privileges as the partitioned table, or create the partition with create_partition() and copy the rows.';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k WHERE k.contype OPERATOR(pg_catalog.=) 'f'
                AND (k.conrelid OPERATOR(pg_catalog.=) partition::pg_catalog.oid OR k.confrelid OPERATOR(pg_catalog.=) partition::pg_catalog.oid) AND k.conparentid OPERATOR(pg_catalog.=) 0) THEN
        RAISE EXCEPTION 'table "%" has foreign keys: it cannot be attached in this version', partition USING ERRCODE = 'feature_not_supported';
    END IF;
    IF pg_catalog.octet_length(v_name) OPERATOR(pg_catalog.>) 57 THEN
        RAISE EXCEPTION 'partition name "%" is too long', v_name USING ERRCODE = 'invalid_name';
    END IF;
    PERFORM kingres.internal_check_leaf_budget(parent);
    PERFORM kingres.internal_leaf_everywhere('attach', c.colocation, c.nsp, c.rname, v_name, bound);
    PERFORM kingres.internal_leaf_everywhere('drop_parent', c.colocation, c.nsp, c.rname, v_name, NULL);
    PERFORM kingres.internal_fault_point('partition_after_leaves');
    DELETE FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) partition;
    INSERT INTO kingres.dist_partition (parent, partition_name, bound) VALUES (parent, v_name, bound);
    PERFORM kingres.sync_catalog();
END;
$$;

-- The access method of every leaf of a table on the node that holds it (null for a leaf that is missing there): the way to see that
-- a partition is heap, columnar or mixed after a conversion that stopped half way.  Asks every node that holds a leaf; superusers only.
CREATE FUNCTION kingres.partition_leaf_storage(parent regclass)
    RETURNS TABLE (partition_name text, node_id int, bucket int, leaf text, access_method name)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_nsp   pg_catalog.name;
    n       pg_catalog.record;
    x       pg_catalog.record;
    r       pg_catalog.jsonb;
    v_names pg_catalog.text[];
    v_nobucket pg_catalog.int4;
    v_nogroup  pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    IF NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) partition_leaf_storage.parent AND d.partition_method IS NOT NULL) THEN
        RAISE EXCEPTION 'table "%" is not a user-partitioned distributed table', parent USING ERRCODE = 'wrong_object_type';
    END IF;
    SELECT s.nspname INTO v_nsp FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace s ON s.oid OPERATOR(pg_catalog.=) c.relnamespace
     WHERE c.oid OPERATOR(pg_catalog.=) parent::pg_catalog.oid;
    -- the leaves of a bucket whose group has no active primary cannot be read: say so, and do not leave them out of the answer
    SELECT m.bucket, m.group_id INTO v_nobucket, v_nogroup
      FROM kingres.dist_table d JOIN kingres.bucket_map m ON m.colocation_id OPERATOR(pg_catalog.=) d.colocation_id
     WHERE d.relid OPERATOR(pg_catalog.=) partition_leaf_storage.parent
       AND NOT EXISTS (SELECT 1 FROM kingres.node nd WHERE nd.group_id OPERATOR(pg_catalog.=) m.group_id AND nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active)
     ORDER BY m.bucket LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'bucket % of table "%" belongs to shard group %, which has no active primary: its leaves cannot be read', v_nobucket, parent, v_nogroup
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    FOR n IN SELECT DISTINCT nd.node_id AS id
               FROM kingres.dist_table d
               JOIN kingres.bucket_map m ON m.colocation_id OPERATOR(pg_catalog.=) d.colocation_id
               JOIN kingres.node nd ON nd.group_id OPERATOR(pg_catalog.=) m.group_id AND nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active
              WHERE d.relid OPERATOR(pg_catalog.=) partition_leaf_storage.parent
              ORDER BY nd.node_id
    LOOP
        SELECT pg_catalog.array_agg(p.partition_name::pg_catalog.text OPERATOR(pg_catalog.||) '_b' OPERATOR(pg_catalog.||) pg_catalog.lpad(m.bucket::pg_catalog.text, 4, '0'))
          INTO v_names
          FROM kingres.dist_partition p
          JOIN kingres.dist_table d ON d.relid OPERATOR(pg_catalog.=) p.parent
          JOIN kingres.bucket_map m ON m.colocation_id OPERATOR(pg_catalog.=) d.colocation_id
          JOIN kingres.node nd ON nd.group_id OPERATOR(pg_catalog.=) m.group_id AND nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active AND nd.node_id OPERATOR(pg_catalog.=) n.id
         WHERE p.parent OPERATOR(pg_catalog.=) partition_leaf_storage.parent;
        IF v_names IS NULL THEN
            CONTINUE;
        END IF;
        r := ((kingres.internal_node_exec(n.id, pg_catalog.format(
                  'SELECT COALESCE(pg_catalog.jsonb_object_agg(c.relname, a.amname), ''{}''::pg_catalog.jsonb)::pg_catalog.text '
                  'FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace s ON s.oid OPERATOR(pg_catalog.=) c.relnamespace '
                  'LEFT JOIN pg_catalog.pg_am a ON a.oid OPERATOR(pg_catalog.=) c.relam '
                  'WHERE s.nspname OPERATOR(pg_catalog.=) %L AND c.relkind OPERATOR(pg_catalog.=) ''r'' AND c.relispartition AND c.relname OPERATOR(pg_catalog.=) ANY (%L::pg_catalog.text[])',
                  v_nsp, v_names), 60000) OPERATOR(pg_catalog.->) 0) OPERATOR(pg_catalog.->>) 0)::pg_catalog.jsonb;
        FOR x IN SELECT p.partition_name AS pn, m.bucket AS b,
                        p.partition_name::pg_catalog.text OPERATOR(pg_catalog.||) '_b' OPERATOR(pg_catalog.||) pg_catalog.lpad(m.bucket::pg_catalog.text, 4, '0') AS rel
                   FROM kingres.dist_partition p
                   JOIN kingres.dist_table d ON d.relid OPERATOR(pg_catalog.=) p.parent
                   JOIN kingres.bucket_map m ON m.colocation_id OPERATOR(pg_catalog.=) d.colocation_id
                   JOIN kingres.node nd ON nd.group_id OPERATOR(pg_catalog.=) m.group_id AND nd.role OPERATOR(pg_catalog.=) 'primary' AND nd.is_active AND nd.node_id OPERATOR(pg_catalog.=) n.id
                  WHERE p.parent OPERATOR(pg_catalog.=) partition_leaf_storage.parent
                  ORDER BY p.partition_name, m.bucket
        LOOP
            partition_name := x.pn::pg_catalog.text;
            node_id := n.id;
            bucket := x.b;
            leaf := pg_catalog.format('%I.%I', v_nsp, x.rel);
            access_method := (r OPERATOR(pg_catalog.->>) x.rel)::pg_catalog.name;
            RETURN NEXT;
        END LOOP;
    END LOOP;
END;
$$;
COMMENT ON FUNCTION kingres.partition_leaf_storage(regclass) IS
    'The access method of every leaf of a user-partitioned distributed table, read on the node that holds it (superusers only)';

-- ---------------------------------------------------------------------------
-- The claim of a colocation group for the conversion of the leaves of one partition (St-D1b)
-- ---------------------------------------------------------------------------
--
-- A leaf is replaced on its node by a copy in another access method.  While the leaves of one partition are converted, no bucket of the
-- group may move (the copy on the target and the publication name the leaves), no table operation may copy the tables of the group, and
-- no partition may be created, dropped or detached: those refuse an object-in-use error as long as table_operation_lock holds the
-- group.  The claim is a record of kind convert_leaf in table_operation (phase done, so that the table operations that resume and clean
-- up what is unfinished leave it alone), and a row of table_operation_lock, which every one of those checks already reads.
ALTER TABLE kingres.table_operation DROP CONSTRAINT table_operation_kind_check;
ALTER TABLE kingres.table_operation ADD CONSTRAINT table_operation_kind_check
    CHECK (kind IN ('distribute', 'distribute_reference', 'undistribute', 'alter', 'convert_leaf'));

-- Claim the group of a table for the conversion of the leaves of one of its partitions; returns the number of the claim.  A claim that
-- nobody renewed for six hours is taken over.  Run on the map group primary, committed at once by the session that calls it (kingres_timepart
-- calls it through kingres.internal_node_exec on the node itself); the claim stays until end_leaf_conversion().
CREATE FUNCTION kingres.begin_leaf_conversion(parent regclass, partition_name text, target text DEFAULT 'columnar', holder_pid int DEFAULT NULL)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_dt kingres.dist_table%ROWTYPE;
    v_id pg_catalog.int8;
    v_op pg_catalog.int8;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    PERFORM kingres.internal_require_version((SELECT e.extversion FROM pg_catalog.pg_extension e WHERE e.extname OPERATOR(pg_catalog.=) 'kingres_shard'));
    PERFORM pg_catalog.set_config('lock_timeout', '5s', true);
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    SELECT * INTO v_dt FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) begin_leaf_conversion.parent;
    IF NOT FOUND OR v_dt.partition_method IS NULL THEN
        RAISE EXCEPTION 'table "%" is not a user-partitioned distributed table', parent USING ERRCODE = 'wrong_object_type';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM kingres.dist_partition p WHERE p.parent OPERATOR(pg_catalog.=) begin_leaf_conversion.parent AND p.partition_name OPERATOR(pg_catalog.=) begin_leaf_conversion.partition_name) THEN
        RAISE EXCEPTION 'partition "%" of table "%" does not exist', partition_name, parent USING ERRCODE = 'undefined_table';
    END IF;
    IF target IS NULL OR target NOT IN ('columnar', 'heap') THEN
        RAISE EXCEPTION 'the storage of a leaf is columnar or heap, not "%"', target USING ERRCODE = 'invalid_parameter_value';
    END IF;
    -- a claim of a conversion that stopped without releasing it
    FOR v_op IN SELECT l.op_id FROM kingres.table_operation_lock l JOIN kingres.table_operation o ON o.op_id OPERATOR(pg_catalog.=) l.op_id
                 WHERE l.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id AND o.kind OPERATOR(pg_catalog.=) 'convert_leaf'
                   AND (o.updated_at OPERATOR(pg_catalog.<) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) interval '6 hours')
                        OR COALESCE(kingres.internal_leaf_holder_gone(o.op_id), false))
    LOOP
        PERFORM kingres.end_leaf_conversion(v_op, 'the claim was not renewed for six hours, or its session is gone');
    END LOOP;
    IF EXISTS (SELECT 1 FROM kingres.table_operation_lock l WHERE l.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id)
       OR EXISTS (SELECT 1 FROM kingres.bucket_move m WHERE m.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching'))
       OR EXISTS (SELECT 1 FROM kingres.bucket_map b WHERE b.colocation_id OPERATOR(pg_catalog.=) v_dt.colocation_id AND b.state OPERATOR(pg_catalog.<>) 'active') THEN
        RAISE EXCEPTION 'colocation group % is busy: a table operation or a bucket move uses it', v_dt.colocation_id USING ERRCODE = 'object_in_use',
              HINT = 'See kingres.table_operations and kingres.bucket_move: a bucket move is cancelled with kingres.cancel_bucket_move(), a table operation with kingres.cancel_table_operation(), and a claim of a conversion that nobody runs any more with kingres.end_leaf_conversion().';
    END IF;
    INSERT INTO kingres.table_operation (kind, mode, phase, spec, detail, colocation_id, started_at, updated_at)
    VALUES ('convert_leaf', 'online', 'done',
            pg_catalog.jsonb_build_object('relid', parent::pg_catalog.oid, 'table', parent::pg_catalog.text, 'partition', partition_name, 'target', target,
                                          'holder_pid', holder_pid,
                                          'holder_start', (SELECT a.backend_start FROM pg_catalog.pg_stat_activity a WHERE a.pid OPERATOR(pg_catalog.=) begin_leaf_conversion.holder_pid)),
            pg_catalog.format('the leaves of partition %s are converted to %s', partition_name, target),
            v_dt.colocation_id, pg_catalog.clock_timestamp(), pg_catalog.clock_timestamp())
    RETURNING op_id INTO v_id;
    INSERT INTO kingres.table_operation_lock (colocation_id, op_id) VALUES (v_dt.colocation_id, v_id);
    RETURN v_id;
END;
$$;

-- Is the session that holds a claim gone?  holder_pid and holder_start name the backend that runs the conversion; a claim made without them has no holder to miss.
CREATE FUNCTION kingres.internal_leaf_holder_gone(p_op bigint)
    RETURNS bool
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT (t.spec OPERATOR(pg_catalog.->>) 'holder_pid') IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_stat_activity a
                        WHERE a.pid OPERATOR(pg_catalog.=) (t.spec OPERATOR(pg_catalog.->>) 'holder_pid')::pg_catalog.int4
                          AND a.backend_start OPERATOR(pg_catalog.=) (t.spec OPERATOR(pg_catalog.->>) 'holder_start')::pg_catalog.timestamptz)
  FROM kingres.table_operation t WHERE t.op_id OPERATOR(pg_catalog.=) p_op AND t.kind OPERATOR(pg_catalog.=) 'convert_leaf';
$$;

-- check_cluster: a claim of a conversion that is stale, not renewed for the six hours after which the next claim takes it over, or held by a
-- session that is gone (holder_pid names the backend that runs the conversion, with its start time; none given means only the time counts).
CREATE FUNCTION kingres.internal_check_leaf_conversions()
    RETURNS TABLE (check_name text, node_id int, object text, detail text)
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    o pg_catalog.record;
    v_gone pg_catalog.bool;
BEGIN
    IF pg_catalog.to_regclass('kingres.table_operation_lock') IS NULL THEN
        RETURN;
    END IF;
    FOR o IN SELECT t.op_id, t.updated_at, t.spec, t.colocation_id
               FROM kingres.table_operation t JOIN kingres.table_operation_lock l ON l.op_id OPERATOR(pg_catalog.=) t.op_id
              WHERE t.kind OPERATOR(pg_catalog.=) 'convert_leaf' AND t.finished_at IS NULL ORDER BY t.op_id
    LOOP
        v_gone := COALESCE(kingres.internal_leaf_holder_gone(o.op_id), false);
        IF o.updated_at OPERATOR(pg_catalog.<) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) interval '6 hours') OR v_gone THEN
            check_name := 'leaf_conversion_stale'; node_id := NULL;
            object := pg_catalog.format('leaf conversion %s of %s', o.op_id, o.spec OPERATOR(pg_catalog.->>) 'table');
            detail := pg_catalog.format('the claim of colocation group %s for the conversion of partition %s was last renewed at %s%s and keeps bucket moves and partition changes away; kingres.end_leaf_conversion(%s) releases it',
                                        o.colocation_id, o.spec OPERATOR(pg_catalog.->>) 'partition', o.updated_at,
                                        CASE WHEN v_gone THEN ' and the session that holds it is gone' ELSE '' END, o.op_id);
            RETURN NEXT;
        END IF;
    END LOOP;
END;
$$;

-- The claim shows that the conversion lives: kingres_timepart renews it after each leaf.
CREATE FUNCTION kingres.renew_leaf_conversion(op_id bigint)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    PERFORM kingres.internal_require_superuser();
    UPDATE kingres.table_operation o SET updated_at = pg_catalog.clock_timestamp()
     WHERE o.op_id OPERATOR(pg_catalog.=) renew_leaf_conversion.op_id AND o.kind OPERATOR(pg_catalog.=) 'convert_leaf';
END;
$$;

-- Release the claim (idempotent); the reason, if any, is kept in the record.
CREATE FUNCTION kingres.end_leaf_conversion(op_id bigint, reason text DEFAULT NULL)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
BEGIN
    PERFORM kingres.internal_require_superuser();
    DELETE FROM kingres.table_operation_lock l
     WHERE l.op_id OPERATOR(pg_catalog.=) end_leaf_conversion.op_id
       AND EXISTS (SELECT 1 FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) l.op_id AND o.kind OPERATOR(pg_catalog.=) 'convert_leaf');
    UPDATE kingres.table_operation o
       SET finished_at = COALESCE(o.finished_at, pg_catalog.clock_timestamp()), updated_at = pg_catalog.clock_timestamp(),
           detail = COALESCE(end_leaf_conversion.reason, o.detail)
     WHERE o.op_id OPERATOR(pg_catalog.=) end_leaf_conversion.op_id AND o.kind OPERATOR(pg_catalog.=) 'convert_leaf';
END;
$$;

COMMENT ON FUNCTION kingres.begin_leaf_conversion(regclass, text, text, int) IS
    'Claim the colocation group of a user-partitioned table for the conversion of the leaves of one partition (map group primary, superuser)';
COMMENT ON FUNCTION kingres.renew_leaf_conversion(bigint) IS 'Renew the claim of a leaf conversion (a claim not renewed for six hours is taken over)';
COMMENT ON FUNCTION kingres.end_leaf_conversion(bigint, text) IS 'Release the claim of a leaf conversion; also the way to remove one that a stopped conversion left behind';

REVOKE ALL ON FUNCTION
    kingres.internal_check_leaf_budget(regclass),
    kingres.partition_leaf_storage(regclass),
    kingres.begin_leaf_conversion(regclass, text, text, int),
    kingres.internal_check_leaf_conversions(),
    kingres.internal_leaf_holder_gone(bigint),
    kingres.renew_leaf_conversion(bigint),
    kingres.end_leaf_conversion(bigint, text)
    FROM PUBLIC;

-- internal_begin_move(): the refusal that a claim of a conversion causes names the action that fits it.
CREATE OR REPLACE FUNCTION kingres.internal_begin_move(p_colocation int, p_bucket int, p_target int)
    RETURNS bigint
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_source pg_catalog.int4;
    v_state  pg_catalog.text;
    v_count  pg_catalog.int4;
    v_id     pg_catalog.int8;
    v_op     pg_catalog.int8;
BEGIN
    PERFORM kingres.internal_require_superuser();
    LOCK TABLE kingres.map_version IN EXCLUSIVE MODE;
    PERFORM kingres.internal_require_map_group();
    PERFORM kingres.internal_check_node_versions();

    SELECT c.bucket_count INTO v_count FROM kingres.colocation c WHERE c.colocation_id OPERATOR(pg_catalog.=) p_colocation;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'colocation group % does not exist', p_colocation USING ERRCODE = 'undefined_object';
    END IF;
    SELECT l.op_id INTO v_op FROM kingres.table_operation_lock l WHERE l.colocation_id OPERATOR(pg_catalog.=) p_colocation;
    IF FOUND THEN
        RAISE EXCEPTION 'colocation group % is used by table operation %', p_colocation, v_op
            USING ERRCODE = 'object_in_use',
                  DETAIL = 'A bucket cannot move while a table operation copies the tables of its group.',
                  HINT = CASE WHEN EXISTS (SELECT 1 FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op AND o.kind OPERATOR(pg_catalog.=) 'convert_leaf')
                              THEN pg_catalog.format('The leaves of a partition are being converted; wait for the conversion to end. A claim that nobody holds any more is reported by kingres.check_cluster() and released by kingres.cancel_table_operation(%s) or kingres.end_leaf_conversion(%s).', v_op, v_op)
                              ELSE 'Wait for the operation to finish, or cancel it; see kingres.table_operations.' END;
    END IF;
    SELECT m.group_id, m.state INTO v_source, v_state FROM kingres.bucket_map m
     WHERE m.colocation_id OPERATOR(pg_catalog.=) p_colocation AND m.bucket OPERATOR(pg_catalog.=) p_bucket;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'bucket % of colocation group % does not exist (it has % buckets)', p_bucket, p_colocation, v_count
            USING ERRCODE = 'undefined_object';
    END IF;
    IF EXISTS (SELECT 1 FROM kingres.bucket_move m
                WHERE m.colocation_id OPERATOR(pg_catalog.=) p_colocation AND m.bucket OPERATOR(pg_catalog.=) p_bucket
                  AND m.phase IN ('preparing', 'copying', 'catching_up', 'switching')) THEN
        RAISE EXCEPTION 'bucket % of colocation group % is already being moved', p_bucket, p_colocation
            USING ERRCODE = 'object_in_use',
                  HINT = 'See kingres.bucket_move; a move can be cancelled with kingres.cancel_bucket_move().';
    END IF;
    IF v_state OPERATOR(pg_catalog.<>) 'active' THEN
        RAISE EXCEPTION 'bucket % of colocation group % is in state "%"', p_bucket, p_colocation, v_state
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF p_target OPERATOR(pg_catalog.=) v_source THEN
        RAISE EXCEPTION 'bucket % of colocation group % is already on shard group %', p_bucket, p_colocation, p_target
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF kingres.internal_group_node(p_target) IS NULL OR kingres.internal_group_node(v_source) IS NULL THEN
        RAISE EXCEPTION 'shard group % or % has no active primary', v_source, p_target
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM kingres.node n
                    WHERE n.group_id OPERATOR(pg_catalog.=) p_target AND n.role OPERATOR(pg_catalog.=) 'primary'
                      AND n.is_active AND n.should_have_shards) THEN
        RAISE EXCEPTION 'shard group % does not accept buckets (should_have_shards is false)', p_target
            USING ERRCODE = 'object_not_in_prerequisite_state',
                  HINT = 'Use kingres.set_node_property() to change it.';
    END IF;

    INSERT INTO kingres.bucket_move (colocation_id, bucket, source_group, target_group, phase,
                                     detail, started_at, updated_at)
    VALUES (p_colocation, p_bucket, v_source, p_target, 'preparing', 'starting',
            pg_catalog.clock_timestamp(), pg_catalog.clock_timestamp())
    RETURNING move_id INTO v_id;
    UPDATE kingres.bucket_move m SET slot_name = pg_catalog.format('ks_move_%s', v_id) WHERE m.move_id OPERATOR(pg_catalog.=) v_id;
    UPDATE kingres.bucket_map m SET state = 'moving'
     WHERE m.colocation_id OPERATOR(pg_catalog.=) p_colocation AND m.bucket OPERATOR(pg_catalog.=) p_bucket;
    PERFORM kingres.sync_catalog();
    RETURN v_id;
END;
$$;


-- cancel_table_operation() also releases the claim of a conversion of leaves that nobody holds any more.
CREATE OR REPLACE FUNCTION kingres.cancel_table_operation(op_id bigint)
    RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
DECLARE
    v_op    pg_catalog.int8 := op_id;
    v_phase pg_catalog.text;
    v_self  pg_catalog.int4;
    v_ms    pg_catalog.int4;
    v_res   pg_catalog.text;
    i       pg_catalog.int4;
BEGIN
    PERFORM kingres.internal_require_superuser();
    PERFORM kingres.internal_require_map_group();
    SELECT o.phase INTO v_phase FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'table operation % does not exist', v_op USING ERRCODE = 'undefined_object';
    END IF;
    -- the claim of a conversion of leaves (kingres_timepart): released when the session that holds it is gone or it was not renewed for the lease; a live
    -- conversion is stopped by cancelling its session (pg_cancel_backend of holder_pid), which releases the claim itself
    IF EXISTS (SELECT 1 FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op AND o.kind OPERATOR(pg_catalog.=) 'convert_leaf') THEN
        IF NOT EXISTS (SELECT 1 FROM kingres.table_operation_lock l WHERE l.op_id OPERATOR(pg_catalog.=) v_op) THEN
            RETURN;
        END IF;
        IF kingres.internal_leaf_holder_gone(v_op)
           OR EXISTS (SELECT 1 FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op AND o.updated_at OPERATOR(pg_catalog.<) (pg_catalog.clock_timestamp() OPERATOR(pg_catalog.-) interval '6 hours')) THEN
            PERFORM kingres.end_leaf_conversion(v_op, 'cancelled: nobody holds the claim any more');
            RETURN;
        END IF;
        RAISE EXCEPTION 'table operation % is a conversion of leaves that its session still holds', v_op
            USING ERRCODE = 'object_in_use',
                  HINT = 'Cancel the session that runs it (see holder_pid in the spec of the record), or release the claim with kingres.end_leaf_conversion() if you know that nothing runs.';
    END IF;
    IF v_phase IN ('cleaning', 'done') THEN
        RAISE NOTICE 'table operation % has switched: nothing is undone', v_op;
        RETURN;
    END IF;
    IF v_phase IN ('failed', 'cancelled') THEN
        RETURN;
    END IF;
    SELECT l.node_id INTO v_self FROM kingres.local_node l;
    v_ms := (SELECT s.setting::pg_catalog.int4 FROM pg_catalog.pg_settings s WHERE s.name OPERATOR(pg_catalog.=) 'kingres_shard.node_timeout') * 1000;
    PERFORM kingres.internal_exec_scalar(v_self, pg_catalog.format('SELECT kingres.internal_op_request_cancel(%s)', v_op), v_ms);
    IF kingres.internal_op_alive(v_op) THEN
        -- the session that runs it undoes it at the end of its chunk
        FOR i IN 1 .. (v_ms OPERATOR(pg_catalog./) 100) * 4 LOOP
            PERFORM pg_catalog.pg_sleep(0.1);
            SELECT o.phase INTO v_phase FROM kingres.table_operation o WHERE o.op_id OPERATOR(pg_catalog.=) v_op;
            EXIT WHEN v_phase IN ('failed', 'cancelled', 'cleaning', 'done');
        END LOOP;
        IF v_phase NOT IN ('failed', 'cancelled', 'cleaning', 'done') THEN
            RAISE NOTICE 'table operation % was asked to stop; the session that runs it has not yet noticed', v_op;
        END IF;
        RETURN;
    END IF;
    v_res := kingres.internal_exec_scalar(v_self,
        pg_catalog.format('SELECT kingres.internal_op_undo(%s, ''cancelled'', %L)', v_op, 'cancelled on request'), v_ms * 6);
    IF v_res OPERATOR(pg_catalog.=) 'incomplete' THEN
        RAISE NOTICE 'table operation % was cancelled but a node could not be cleaned; kingres.resume_table_operations() removes what is left', v_op;
    END IF;
END;
$$;
