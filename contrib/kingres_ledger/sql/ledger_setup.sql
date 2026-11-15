-- ledger_setup: switch the block-building workers off for the pg_regress
-- tests, which build blocks by hand and count them.  The tests run against a
-- server that was started with shared_preload_libraries = 'kingres_ledger'
-- only (kingres/dev/ext.sh), where the launcher is running with its default
-- interval; the temporary instance of "make check" gets the same setting
-- from kingres_ledger.conf.  The TAP tests cover the workers.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
ALTER SYSTEM SET kingres_ledger.block_interval = 0;
SELECT pg_reload_conf();
SELECT pg_sleep(1);
\c
SHOW kingres_ledger.block_interval;
