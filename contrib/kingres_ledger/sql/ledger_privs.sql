-- ledger_privs: the privilege model and the catalog hygiene of the functions of
-- the extension.  Scans pg_proc for the members of the extension and fails (a
-- row in the output) when one of them breaks a rule:
--   * every function that looks anything up has "SET search_path = pg_catalog,
--     pg_temp", with pg_temp last; the only functions without a search_path
--     are the C primitives that compute on their arguments, listed below;
--   * SECURITY DEFINER is used only by the functions that have to write or
--     read what the caller may not;
--   * every reference to an object of the extension in a SQL or PL/pgSQL body
--     is qualified with the schema;
--   * what PUBLIC may execute and read.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
SET search_path = public;
\set VERBOSITY terse

CREATE TEMP VIEW members AS
SELECT p.*
FROM pg_proc p
JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid
                AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
JOIN pg_extension e ON e.oid = d.refobjid AND e.extname = 'kingres_ledger';

SELECT count(*) > 30 AS functions_found FROM members;

-- C primitives that look nothing up and run no SQL.
CREATE TEMP TABLE pure_primitives (name text PRIMARY KEY);
INSERT INTO pure_primitives VALUES
    ('ledger_leaf_hash'), ('ledger_node_hash'), ('ledger_merkle_accum'),
    ('ledger_merkle_final'), ('ledger_merkle_path'), ('ledger_tx_leaf'),
    ('ledger_block_tx_leaf'), ('ledger_block_hash'), ('ledger_block_preimage'),
    ('ledger_genesis_hash'),
    ('ledger_timestamptz_us'), ('ledger_verify_proof'), ('ledger_hook_active'),
    ('ledger_outer_user');

-- A function without a fixed search_path that is not on the list.
SELECT m.oid::regprocedure AS function_without_fixed_search_path
FROM members m
WHERE m.prokind <> 'a'
  AND NOT EXISTS (SELECT 1 FROM unnest(m.proconfig) c WHERE c = 'search_path=pg_catalog, pg_temp')
  AND m.proname NOT IN (SELECT name FROM pure_primitives)
ORDER BY 1;
-- Any search_path of the extension that does not end with pg_temp.
SELECT m.oid::regprocedure AS search_path_not_ending_with_pg_temp
FROM members m, unnest(m.proconfig) c
WHERE c LIKE 'search_path=%' AND c !~ ', pg_temp$'
ORDER BY 1;
-- The exempt functions really are plain C functions without privileges of their own.
SELECT m.oid::regprocedure AS exempt_function_that_is_not_plain_c
FROM members m JOIN pure_primitives pp ON pp.name = m.proname
WHERE m.prolang <> (SELECT oid FROM pg_language WHERE lanname = 'c') OR m.prosecdef
ORDER BY 1;
-- Every listed primitive exists (a typo in the list would hide a function).
SELECT pp.name AS listed_but_missing
FROM pure_primitives pp WHERE NOT EXISTS (SELECT 1 FROM members m WHERE m.proname = pp.name);

-- The functions that run as the extension owner, and only those.
SELECT m.proname, pg_get_function_identity_arguments(m.oid) AS args
FROM members m WHERE m.prosecdef ORDER BY 1, 2;

-- Unqualified references in the SQL and PL/pgSQL functions: a table of the
-- extension after FROM, JOIN, INTO, UPDATE or TABLE, or a call of a function
-- of the extension, without "kingres.".  Comments are not looked at.
SELECT m.oid::regprocedure AS function_with_unqualified_reference
FROM members m
WHERE m.prolang <> (SELECT oid FROM pg_language WHERE lanname = 'c')
  AND (regexp_replace(m.prosrc, '--[^\n]*', '', 'g')
           ~* '\y(from|join|into|update|table)\s+(only\s+)?ledger_'
       OR regexp_replace(m.prosrc, '--[^\n]*', '', 'g')
           ~ '(^|[^.a-z_"])ledger_[a-z_]+\s*\(')
ORDER BY 1;

-- The default privilege model: which function PUBLIC can execute.
SELECT m.proname,
       pg_get_function_identity_arguments(m.oid) AS args,
       (m.proacl IS NULL OR EXISTS (SELECT 1 FROM aclexplode(m.proacl) a
                                     WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE')) AS public_can_execute
FROM members m
WHERE m.prokind <> 'a'
ORDER BY 1, 2;

-- ... and which table PUBLIC can read or write.
SELECT c.relname,
       (c.relacl IS NULL OR EXISTS (SELECT 1 FROM aclexplode(c.relacl) a
                                     WHERE a.grantee = 0 AND a.privilege_type = 'SELECT')) AS public_can_select,
       EXISTS (SELECT 1 FROM aclexplode(c.relacl) a
                WHERE a.grantee = 0 AND a.privilege_type <> 'SELECT') AS public_can_do_more
FROM pg_class c
JOIN pg_depend d ON d.classid = 'pg_class'::regclass AND d.objid = c.oid
                AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
JOIN pg_extension e ON e.oid = d.refobjid AND e.extname = 'kingres_ledger'
WHERE c.relkind = 'r'
ORDER BY 1;

-- The event triggers of the extension are in place and ENABLE ALWAYS.
SELECT e.evtevent, e.evtenabled
FROM pg_event_trigger e
WHERE EXISTS (SELECT 1 FROM members m WHERE m.oid = e.evtfoid)
ORDER BY 1;

--------------------------------------------------------------------------
-- A caller cannot shadow anything the functions use
--------------------------------------------------------------------------
-- Temporary objects with the names of the objects of the extension, and of
-- the functions it calls, in front of everything in the search path.
CREATE TEMP TABLE ledger_chain (singleton boolean, chain_id bytea);
INSERT INTO ledger_chain VALUES (true, '\x00');
CREATE TEMP TABLE ledger_blocks (height bigint);
CREATE FUNCTION pg_temp.encode(bytea, text) RETURNS text LANGUAGE sql AS $$ SELECT 'hijacked' $$;
CREATE FUNCTION pg_temp.count(anyelement) RETURNS bigint LANGUAGE sql AS $$ SELECT 42::bigint $$;
CREATE FUNCTION pg_temp.format(text, VARIADIC text[]) RETURNS text LANGUAGE sql AS $$ SELECT 'hijacked' $$;
SET search_path = pg_temp, public;
SELECT count(*) AS problems_with_shadows FROM kingres.ledger_verify();
INSERT INTO docs VALUES (600, 'shadow', NULL);
SELECT kingres.ledger_generate_digest() ->> 'chain_id' = encode((SELECT c.chain_id FROM kingres.ledger_chain c), 'hex')
       AS digest_uses_the_real_chain_id;
SELECT kingres.ledger_verify_digest(kingres.ledger_generate_digest()) AS digest_verifies;
SELECT kingres.ledger_inclusion_proof('docs', kingres.ledger_row_hash(d.*)) IS NOT NULL AS proof_built
FROM docs d WHERE d.id = 600;
SET search_path = public;
DROP TABLE pg_temp.ledger_chain;
DROP TABLE pg_temp.ledger_blocks;
