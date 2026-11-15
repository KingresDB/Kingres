-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- columnar_setup: install the extension; the access method, the settings and
-- their defaults.
CREATE EXTENSION kingres_columnar;
SET search_path = public;

SELECT e.extname, e.extversion, n.nspname, e.extrelocatable
  FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
 WHERE e.extname = 'kingres_columnar';

-- the access method
SELECT a.amname, a.amtype, p.pronamespace::regnamespace AS handler_schema, p.proname AS handler
  FROM pg_am a JOIN pg_proc p ON p.oid = a.amhandler WHERE a.amname = 'columnar';

-- the library is preloaded by the test server
SHOW shared_preload_libraries;

-- the settings of the extension and their defaults
SELECT name, setting, unit, vartype, enumvals, context, boot_val,
       CASE WHEN name NOT LIKE '%memory' THEN min_val END AS min_val,
       CASE WHEN name NOT LIKE '%memory' THEN max_val END AS max_val
  FROM pg_settings WHERE name LIKE 'kingres\_columnar.%' ORDER BY name;

SHOW kingres_columnar.compression;
SHOW kingres_columnar.compression_level;
SHOW kingres_columnar.stripe_row_count;
SHOW kingres_columnar.chunk_row_count;
SHOW kingres_columnar.write_buffer_memory;
SHOW kingres_columnar.enable_custom_scan;
SHOW kingres_columnar.enable_chunk_skipping;

-- values outside the range are refused
SET kingres_columnar.stripe_row_count = 999;
SET kingres_columnar.stripe_row_count = 10000001;
SET kingres_columnar.chunk_row_count = 99;
SET kingres_columnar.chunk_row_count = 100001;
SET kingres_columnar.compression_level = 0;
SET kingres_columnar.compression_level = 20;
SET kingres_columnar.compression = 'gzip';
SET kingres_columnar.compression = 'none';
SET kingres_columnar.compression = 'auto';
RESET kingres_columnar.compression;
SET kingres_columnar.no_such_setting = 1;

-- a new table gets the defaults
CREATE TABLE setup_t (a int) USING columnar;
SELECT compression IN ('zstd', 'lz4', 'pglz') AS real_codec, compression_level,
       stripe_row_count, chunk_row_count
  FROM kingres.columnar_options('setup_t');
SELECT c.relname, a.amname, c.relkind, c.relpersistence
  FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 'setup_t'::regclass;
DROP TABLE setup_t;

-- the schema of the extension, the catalog and its privileges
SELECT relname, relkind, has_table_privilege('public', oid, 'SELECT') AS public_select
  FROM pg_class WHERE oid = 'kingres.columnar_stripe'::regclass;
SELECT count(*) AS catalog_rows FROM kingres.columnar_stripe;

-- the event triggers are always enabled
SELECT evtname, evtevent, evtenabled FROM pg_event_trigger
 WHERE evtname LIKE 'kingres\_columnar%' ORDER BY evtname;
