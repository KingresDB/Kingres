# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

# No C code: the extension is SQL and PL/pgSQL only.

install_data(
  'kingres_timepart--0.1.sql',
  'kingres_timepart--0.1--0.2.sql',
  'kingres_timepart--0.2.sql',
  'kingres_timepart.control',
  kwargs: contrib_data_args,
)

tests += {
  'name': 'kingres_timepart',
  'sd': meson.current_source_dir(),
  'bd': meson.current_build_dir(),
  'regress': {
    'sql': [
      'timepart_setup',
      'timepart_grid',
      'timepart_create',
      'timepart_policy',
      'timepart_run',
      'timepart_retention',
      'timepart_names',
      'timepart_hostile',
      'timepart_security',
      'timepart_history',
      'timepart_privilege',
      'timepart_forged',
      'timepart_owner_code',
      'timepart_convert',
      'timepart_drop',
      'upgrade',
    ],
    'regress_args': ['--temp-config', files('kingres_timepart.conf')],
    # The extension needs no preloaded library, but timepart_convert writes
    # columnar tables (kingres_columnar is preloaded by the settings file),
    # the tests set kingres_timepart.* settings and the in-tree run uses its
    # own server.
    'runningcheck': false,
  },
  'tap': {
    'tests': [
      't/001_locks.pl',
      't/002_interrupt.pl',
      't/003_dump_restore.pl',
      't/004_owner_and_races.pl',
      't/005_convert_concurrent.pl',
      't/006_convert_crash.pl',
      't/007_convert_standby.pl',
      't/008_convert_prepared.pl',
    ],
  },
}
