# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co

kingres_shard_sources = files(
  'src/catalog.c',
  'src/dist_scan.c',
  'src/dist_table.c',
  'src/fanout.c',
  'src/fault.c',
  'src/forward_auth.c',
  'src/hashing.c',
  'src/kingres_shard.c',
  'src/node_admin.c',
  'src/node_conn.c',
  'src/planner.c',
  'src/recovery.c',
  'src/remote_scan.c',
  'src/shippable.c',
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
  'kingres_shard--0.2.sql',
  'kingres_shard--0.1--0.2.sql',
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
      'reference',
      'ddl_single',
      'upgrade',
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
      't/008_twophase.pl',
      't/009_recovery_worker.pl',
      't/010_atomic_catalog.pl',
      't/011_reference.pl',
      't/012_ddl_propagation.pl',
      't/013_dist_scan.pl',
      't/014_dist_write.pl',
      't/015_node_conninfo.pl',
      't/016_recovery_ordering.pl',
      't/017_settings_compare.pl',
      't/018_reference_writes.pl',
      't/019_forwarded_auth.pl',
      't/020_parallel_and_add_node.pl',
      't/021_owner_and_views.pl',
      't/022_timeouts_and_worker.pl',
      't/023_scoped_bypass.pl',
      't/024_low_findings.pl',
    ],
  },
}
