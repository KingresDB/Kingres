-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- sketch_setup: install the extension; check where it lives and the
-- self-test line that is compared between nodes.
CREATE EXTENSION kingres_sketch;

SELECT e.extname, e.extversion, n.nspname, e.extrelocatable
  FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
 WHERE e.extname = 'kingres_sketch';

SET search_path = public, kingres_sketch;
-- one field per line: the version, the hash of each type, and the bytes of
-- the fixed hll, tdigest (alone, united, read) and topn (alone, united)
SELECT regexp_split_to_table(self_test(), ' ') AS self_test;

-- the three types and the casts to bytea
SELECT t.typname, t.typlen, t.typstorage
  FROM pg_type t
 WHERE t.typnamespace = 'kingres_sketch'::regnamespace AND t.typtype = 'b'
   AND t.typname IN ('hll', 'topn', 'tdigest')
 ORDER BY t.typname;
