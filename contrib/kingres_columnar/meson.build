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

install_data(
  'kingres_columnar--0.1.sql',
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
      'columnar_security',
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
    ],
  },
}
