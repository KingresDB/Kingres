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
    RETURNS TABLE (partby pg_catalog.text, ord pg_catalog.int4, name pg_catalog.name, bound pg_catalog.text, am pg_catalog.name)
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, kingres, pg_temp
    AS $$
SELECT pg_catalog.pg_get_partkeydef(p_relid::pg_catalog.oid),
       (pg_catalog.row_number() OVER (ORDER BY pc.relname))::pg_catalog.int4,
       pc.relname,
       pg_catalog.regexp_replace(pg_catalog.pg_get_expr(pc.relpartbound, pc.oid), '^FOR VALUES ', ''),
       COALESCE((SELECT a.amname FROM pg_catalog.pg_am a WHERE a.oid OPERATOR(pg_catalog.=) pc.relam), 'heap'::pg_catalog.name)
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
--   columnar_storage (note, distribute): what the table becomes.
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
BEGIN
    RETURN QUERY SELECT b.severity, b.code, b.object, b.detail
                   FROM kingres.internal_table_change_report_base(kind, relation, options) b;
    IF kind NOT IN ('distribute', 'distribute_reference') THEN
        RETURN;
    END IF;
    SELECT a.amname, pg_catalog.format('%I.%I', n.nspname, k.relname) INTO v_am, v_rel
      FROM pg_catalog.pg_class k
      JOIN pg_catalog.pg_am a ON a.oid OPERATOR(pg_catalog.=) k.relam
      JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) k.relnamespace
     WHERE k.oid OPERATOR(pg_catalog.=) relation::pg_catalog.oid AND k.relkind OPERATOR(pg_catalog.=) ANY (ARRAY['r', 'p']::pg_catalog."char"[])
       AND a.amname OPERATOR(pg_catalog.<>) 'heap'
       AND NOT EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid OPERATOR(pg_catalog.=) relation);
    IF v_am IS NULL THEN
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
