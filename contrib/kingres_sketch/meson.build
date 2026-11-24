# Portions Copyright (c) 2026, RooyeKhat Media Co

kingres_sketch_sources = files(
  'src/hll.c',
  'src/kingres_sketch.c',
  'src/sketch_hash.c',
  'src/tdigest.c',
  'src/topn.c',
)

if host_system == 'windows'
  kingres_sketch_sources += rc_lib_gen.process(win32ver_rc, extra_args: [
    '--NAME', 'kingres_sketch',
    '--FILEDESC', 'kingres_sketch - approximate aggregates (hll, topn, tdigest) for KingresDB',])
endif

kingres_sketch = shared_module('kingres_sketch',
  kingres_sketch_sources,
  include_directories: include_directories('src'),
  c_args: ['-Werror=implicit-function-declaration'],
  kwargs: contrib_mod_args,
)
contrib_targets += kingres_sketch

install_data(
  'kingres_sketch--1.0.sql',
  'kingres_sketch.control',
  kwargs: contrib_data_args,
)

tests += {
  'name': 'kingres_sketch',
  'sd': meson.current_source_dir(),
  'bd': meson.current_build_dir(),
  'regress': {
    'sql': [
      'sketch_setup',
      'sketch_hash',
      'sketch_hll',
      'sketch_topn',
      'sketch_tdigest',
      'sketch_io',
      'sketch_security',
      'sketch_memory',
    ],
  },
  'tap': {
    'tests': [
      't/001_accuracy_hll.pl',
      't/002_accuracy_topn.pl',
      't/003_accuracy_tdigest.pl',
      't/004_parallel.pl',
      't/005_dump_copy.pl',
      't/006_fuzz.pl',
      't/007_portable_sketches.pl',
      't/008_memory.pl',
      't/009_memory_ceiling.pl',
      't/010_schema_spoof.pl',
    ],
  },
}
