# Portions Copyright (c) 2026, RooyeKhat Media Co

kingres_ledger_sources = files(
  'src/capture.c',
  'src/history.c',
  'src/kingres_ledger.c',
  'src/merkle.c',
  'src/proof.c',
  'src/protect.c',
  'src/row_hash.c',
  'src/worker.c',
)

if host_system == 'windows'
  kingres_ledger_sources += rc_lib_gen.process(win32ver_rc, extra_args: [
    '--NAME', 'kingres_ledger',
    '--FILEDESC', 'kingres_ledger - tamper-evident ledger tables for KingresDB',])
endif

kingres_ledger = shared_module('kingres_ledger',
  kingres_ledger_sources,
  include_directories: include_directories('src'),
  kwargs: contrib_mod_args,
)
contrib_targets += kingres_ledger

install_data(
  'kingres_ledger--0.1.sql',
  'kingres_ledger--0.2.sql',
  'kingres_ledger--0.1--0.2.sql',
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
    ],
  },
}
