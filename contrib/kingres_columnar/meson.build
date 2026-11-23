# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

kingres_columnar_sources = files(
  'src/kc_am.c',
  'src/kc_catalog.c',
  'src/kc_cscan.c',
  'src/kc_encode.c',
  'src/kc_scan.c',
  'src/kc_skip.c',
  'src/kc_storage.c',
  'src/kc_vacuum.c',
  'src/kc_write.c',
  'src/kingres_columnar.c',
)

if host_system == 'windows'
  kingres_columnar_sources += rc_lib_gen.process(win32ver_rc, extra_args: [
    '--NAME', 'kingres_columnar',
    '--FILEDESC', 'kingres_columnar - columnar table access method for KingresDB',])
endif

# lz4 and zstd are defined by the top-level meson.build; when the server was
# built without them the dependency objects are "not found" and the code
# compiles without those codecs (USE_LZ4 and USE_ZSTD come from pg_config.h).
kingres_columnar = shared_module('kingres_columnar',
  kingres_columnar_sources,
  include_directories: include_directories('src'),
  c_args: ['-Werror=implicit-function-declaration'],
  # contrib_mod_args has dependencies of its own, so the two codec libraries
  # are added to that entry (as pgcrypto does for its libraries).
  kwargs: contrib_mod_args + {
    'dependencies': [lz4, zstd, contrib_mod_args['dependencies']],
  },
)
contrib_targets += kingres_columnar

# The two modules that the TAP tests use (t/kcevents, t/kcprobe) are compiled here
# too, and never installed: a tree that cannot build them fails the build, loudly,
# where the tests would otherwise have to build them (t/KcProbe.pm does that again,
# with the installed headers, for a run out of tree).
shared_module('kcevents',
  files('t/kcevents/kcevents.c'),
  include_directories: include_directories('t/kcevents'),
  kwargs: pg_test_mod_args,
)
shared_module('kcprobe',
  files('t/kcprobe/kcprobe.c'),
  kwargs: pg_test_mod_args,
)

install_data(
  'kingres_columnar--0.1.sql',
  'kingres_columnar--0.2.sql',
  'kingres_columnar--0.1--0.2.sql',
  'kingres_columnar.control',
  kwargs: contrib_data_args,
)

tests += {
  'name': 'kingres_columnar',
  'sd': meson.current_source_dir(),
  'bd': meson.current_build_dir(),
  'regress': {
    'sql': [
      'columnar_setup',
      'columnar_basic',
      'columnar_types',
      'columnar_ddl',
      'columnar_refusals',
      'columnar_scan_plans',
      'columnar_stats',
      'columnar_vacuum',
      'columnar_analyze',
      'columnar_review',
      'columnar_parallel',
      'columnar_security',
      'columnar_reserve',
      'columnar_index',
      'columnar_unique',
      'columnar_compact_index',
      'upgrade',
    ],
    'regress_args': ['--temp-config', files('kingres_columnar.conf')],
  },
  'tap': {
    'tests': [
      't/001_crash_recovery.pl',
      't/002_standby.pl',
      't/003_backup_dump.pl',
      't/004_concurrent.pl',
      't/005_corruption.pl',
      't/006_compare.pl',
      't/007_snapshots.pl',
      't/008_array_validation.pl',
      't/009_serializable.pl',
      't/010_visibility.pl',
      't/011_memory.pl',
      't/012_publications_toast.pl',
      't/013_xid_wraparound.pl',
      't/014_buffers.pl',
      't/015_creation_compaction.pl',
      't/016_atomic_handover.pl',
      't/017_stripe_sizes.pl',
      't/018_maintenance_snapshots.pl',
      't/019_table_age.pl',
      't/020_limits_flags_check.pl',
      't/021_table_creation.pl',
      't/022_handover_window.pl',
      't/023_rewrite_cleanup.pl',
      't/024_commit_window.pl',
      't/025_memory_bound.pl',
      't/026_random_cursors.pl',
      't/027_ending_writes.pl',
      't/028_orphan_rows.pl',
      't/029_chain_parallel_backup.pl',
      't/030_late_stripes.pl',
      't/031_entry_points.pl',
      't/032_module_build.pl',
      't/033_standby_late.pl',
      't/034_decoding.pl',
      't/035_autovacuum_log.pl',
      't/036_check_view.pl',
      't/037_vacuum_truncate.pl',
      't/038_truncate_frozen_id.pl',
      't/039_alias_am.pl',
      't/040_check_lock_table.pl',
      't/041_vacuum_failsafe.pl',
      't/042_not_checked_memory.pl',
      't/043_parallel.pl',
      't/044_reserve_tids.pl',
      't/045_reserve_subxact.pl',
      't/046_reserve_crash.pl',
      't/047_reserve_concurrent.pl',
      't/048_reserve_standby.pl',
      't/049_upgrade_data.pl',
      't/050_reserve_check.pl',
      't/051_index_twin.pl',
      't/052_reserve_memory.pl',
      't/053_unique_race.pl',
      't/054_unique_crash.pl',
      't/055_unique_build.pl',
      't/056_index_vacuum.pl',
      't/057_unique_standby.pl',
      't/058_index_cursor.pl',
      't/059_compact_index.pl',
      't/060_compact_index_crash_standby.pl',
      't/061_logical.pl',
      't/062_free_extents.pl',
      't/063_index_old_snapshot.pl',
      't/064_decode_missing.pl',
      't/065_amcheck.pl',
      't/066_row_number_waste.pl',
      't/067_vacuum_compacts.pl',
      't/068_vacuum_giveback.pl',
      't/069_vacuum_merge_safe.pl',
      't/070_vacuum_order_and_skip.pl',
      't/071_schema_spoof.pl',
    ],
  },
}
