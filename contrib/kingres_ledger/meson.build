# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co

kingres_ledger_sources = files(
  'src/capture.c',
  'src/ed25519_check.c',
  'src/guard.c',
  'src/history.c',
  'src/kingres_ledger.c',
  'src/merkle.c',
  'src/proof.c',
  'src/protect.c',
  'src/row_hash.c',
  'src/sign.c',
  'src/worker.c',
  'src/write.c',
)

if host_system == 'windows'
  kingres_ledger_sources += rc_lib_gen.process(win32ver_rc, extra_args: [
    '--NAME', 'kingres_ledger',
    '--FILEDESC', 'kingres_ledger - tamper-evident ledger tables for KingresDB',])
endif

kingres_ledger_args = contrib_mod_args + {
  'dependencies': [ssl, contrib_mod_args['dependencies']],
}

kingres_ledger = shared_module('kingres_ledger',
  kingres_ledger_sources,
  include_directories: include_directories('src'),
  kwargs: kingres_ledger_args,
)
contrib_targets += kingres_ledger

install_data(
  'kingres_ledger--0.1.sql',
  'kingres_ledger--0.2.sql',
  'kingres_ledger--0.3.sql',
  'kingres_ledger--0.1--0.2.sql',
  'kingres_ledger--0.2--0.3.sql',
  'kingres_ledger--0.1--0.3.sql',
  'kingres_ledger.control',
  kwargs: contrib_data_args,
)

tests += {
  'name': 'kingres_ledger',
  'sd': meson.current_source_dir(),
  'bd': meson.current_build_dir(),
  'regress': {
    'sql': [
      'ledger_setup',
      'ledger_hash',
      'ledger_capture',
      'ledger_verify',
      'ledger_protect',
      'ledger_format',
      'ledger_review',
      'ledger_updatable',
      'ledger_records',
      'ledger_review2',
      'ledger_privs',
      'ledger_preimage',
      'ledger_head',
      'ledger_stray',
      'ledger_guard',
      'ledger_manual',
      'ledger_paths',
      'ledger_stops',
      'ledger_sign',
    ],
    'regress_args': ['--temp-config', files('kingres_ledger.conf')],
    # The tests need shared_preload_libraries = 'kingres_ledger', which a
    # plain "runningcheck" server does not have.
    'runningcheck': false,
  },
  'tap': {
    'tests': [
      't/001_concurrent_blocks.pl',
      't/002_restart_standby.pl',
      't/003_preload_and_hooks.pl',
      't/004_races.pl',
      't/005_environments.pl',
      't/006_guard_without_hook.pl',
      't/007_upgrade.pl',
      't/008_updatable_concurrency.pl',
      't/009_worker.pl',
      't/010_review2.pl',
      't/011_upgrade_0_3.pl',
      't/012_publish_read.pl',
      't/013_head_inflight.pl',
      't/014_guard.pl',
      't/015_recipe.pl',
      't/016_dump_upgrade.pl',
      't/017_guard_paths.pl',
      't/018_second_invariant.pl',
      't/019_ids_and_hold.pl',
      't/020_not_covered.pl',
      't/021_drop_database.pl',
      't/022_hold_bounds.pl',
      't/023_writers.pl',
      't/025_lock_order.pl',
      't/026_ids_and_cancel.pl',
      't/027_own_statement.pl',
      't/028_timer_survives.pl',
      't/029_enable_credit.pl',
      't/030_hold_property.pl',
      't/031_server_file_roles.pl',
      't/032_attest_policy.pl',
      't/033_attest_concurrency.pl',
      't/034_internal_tables.pl',
      't/035_no_openssl.pl',
      't/036_policy_switch.pl',
      't/037_attest_savepoints.pl',
      't/038_public_key_check.pl',
      't/039_signature_paths.pl',
      't/040_columnar_refused.pl',
      't/041_schema_spoof.pl',
      't/042_catalog_trust.pl',
      't/043_old_script_schema_check.pl',
    ],
  },
}
