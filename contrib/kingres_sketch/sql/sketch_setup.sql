-- sketch_setup: install the extension; check where it lives and the
-- self-test line that is compared between nodes.
CREATE EXTENSION kingres_sketch;

SELECT e.extname, e.extversion, n.nspname, e.extrelocatable
  FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
 WHERE e.extname = 'kingres_sketch';

SET search_path = public, kingres_sketch;
SELECT self_test();

-- the three types and the casts to bytea
SELECT t.typname, t.typlen, t.typstorage
  FROM pg_type t
 WHERE t.typnamespace = 'kingres_sketch'::regnamespace AND t.typtype = 'b'
   AND t.typname IN ('hll', 'topn', 'tdigest')
 ORDER BY t.typname;
