-- sketch_security: what the extension installs, and that none of it is
-- SECURITY DEFINER or outside the schema.
SELECT count(*) AS functions_not_in_schema
  FROM pg_depend d JOIN pg_proc p ON p.oid = d.objid
 WHERE d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
   AND d.refobjid = (SELECT oid FROM pg_extension WHERE extname = 'kingres_sketch')
   AND d.classid = 'pg_proc'::regclass AND p.pronamespace <> 'kingres_sketch'::regnamespace;

SELECT count(*) AS types_not_in_schema
  FROM pg_depend d JOIN pg_type t ON t.oid = d.objid
 WHERE d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
   AND d.refobjid = (SELECT oid FROM pg_extension WHERE extname = 'kingres_sketch')
   AND d.classid = 'pg_type'::regclass AND t.typnamespace <> 'kingres_sketch'::regnamespace;

SELECT count(*) AS public_objects FROM pg_proc WHERE pronamespace = 'public'::regnamespace;

-- every function: C language, not SECURITY DEFINER, immutable, parallel safe,
-- no per-function settings (the C code looks nothing up by name)
SELECT count(*) AS functions,
       count(*) FILTER (WHERE prolang <> (SELECT oid FROM pg_language WHERE lanname = 'c')) AS not_c,
       count(*) FILTER (WHERE prosecdef) AS security_definer,
       count(*) FILTER (WHERE provolatile <> 'i') AS not_immutable,
       count(*) FILTER (WHERE proparallel <> 's') AS not_parallel_safe,
       count(*) FILTER (WHERE proconfig IS NOT NULL) AS with_settings
  FROM pg_proc WHERE pronamespace = 'kingres_sketch'::regnamespace AND prokind = 'f'
   AND proname <> 'state_bytes';

-- every aggregate is parallel safe and has the functions partial aggregation needs
SELECT p.proname, pg_get_function_identity_arguments(p.oid) AS args,
       a.aggtranstype::regtype AS stype,
       a.aggcombinefn <> 0 AS has_combine, a.aggserialfn <> 0 AS has_serial,
       a.aggdeserialfn <> 0 AS has_deserial, p.proparallel AS parallel
  FROM pg_aggregate a JOIN pg_proc p ON p.oid = a.aggfnoid
 WHERE p.pronamespace = 'kingres_sketch'::regnamespace
 ORDER BY p.proname, args;

-- the types have no operators, so a sketch is no key and no GROUP BY column
SELECT count(*) AS operators_on_sketch_types
  FROM pg_operator
 WHERE oprleft IN ('kingres_sketch.hll'::regtype, 'kingres_sketch.topn'::regtype, 'kingres_sketch.tdigest'::regtype)
    OR oprright IN ('kingres_sketch.hll'::regtype, 'kingres_sketch.topn'::regtype, 'kingres_sketch.tdigest'::regtype);
CREATE TABLE sketch_key_test (s kingres_sketch.hll);
CREATE INDEX ON sketch_key_test (s);
SELECT s FROM sketch_key_test GROUP BY s;
DROP TABLE sketch_key_test;

-- the extension can be dropped and created again
DROP EXTENSION kingres_sketch;
SELECT count(*) AS left_in_schema FROM pg_proc WHERE pronamespace = 'kingres_sketch'::regnamespace;
CREATE EXTENSION kingres_sketch;
SELECT kingres_sketch.hll_cardinality(kingres_sketch.hll_empty());
