# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co

kingres_shard_sources = files(
  'src/catalog.c',
  'src/dist_table.c',
  'src/hashing.c',
  'src/kingres_shard.c',
  'src/node_admin.c',
  'src/node_conn.c',
  'src/planner.c',
  'src/remote_scan.c',
  'src/utility.c',
)

if host_system == 'windows'
  kingres_shard_sources += rc_lib_gen.process(win32ver_rc, extra_args: [
    '--NAME', 'kingres_shard',
    '--FILEDESC', 'kingres_shard - KingresDB sharding extension',])
endif

kingres_shard = shared_module('kingres_shard',
  kingres_shard_sources,
  include_directories: include_directories('src'),
  kwargs: contrib_mod_args + {
    'dependencies': contrib_mod_args['dependencies'] + [libpq],
  },
)
contrib_targets += kingres_shard

install_data(
  'kingres_shard.control',
  'kingres_shard--0.1.sql',
  kwargs: contrib_data_args,
)

tests += {
  'name': 'kingres_shard',
  'sd': meson.current_source_dir(),
  'bd': meson.current_build_dir(),
  'regress': {
    'sql': [
      'version',
      'catalog',
      'bucket_for',
      'distribute',
      'routing_single',
      'security',
      'utility_guard',
      'routing_decisions',
    ],
    'regress_args': ['--temp-config', files('kingres_shard.conf')],
    # The tests need shared_preload_libraries = 'kingres_shard', which a
    # plain "runningcheck" server does not have.
    'runningcheck': false,
  },
  'tap': {
    'tests': [
      't/001_cluster.pl',
      't/002_routing.pl',
      't/003_hooks.pl',
      't/004_security.pl',
      't/005_session_settings.pl',
      't/006_ddl_and_concurrency.pl',
      't/007_nested_contexts.pl',
    ],
  },
}
