// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The test vectors of the value checker (`src/lib/config-value.ts`): one table that the unit test of the checker, the
// mock API and the edit dialog are all held to. The control plane keeps the same cases for its own checker (and compares
// every accepted value with PostgreSQL), so a case that is added here is a case to add there. A vector with a
// `normalized` text is accepted and written back that way; one with null is refused with some message.
import type { ValueSpec } from '@/lib/config-value'

export const SPECS = {
  bool: { type: 'bool', unit: null, min: null, max: null, enumValues: null },
  count: { type: 'integer', unit: null, min: '1', max: '100', enumValues: null },
  /** work_mem: kB, 64 to 2147483647. */
  kb: { type: 'integer', unit: 'kB', min: '64', max: '2147483647', enumValues: null },
  /** shared_buffers: pages of 8 kB, 16 to 1073741823. */
  pages: { type: 'integer', unit: '8kB', min: '16', max: '1073741823', enumValues: null },
  /** statement_timeout: ms. */
  ms: { type: 'integer', unit: 'ms', min: '0', max: '2147483647', enumValues: null },
  /** authentication_timeout: s, 1 to 600. */
  sec: { type: 'integer', unit: 's', min: '1', max: '600', enumValues: null },
  /** wal_summary_keep_time: min. */
  min: { type: 'integer', unit: 'min', min: '0', max: '35791394', enumValues: null },
  /** log_min_duration_statement: ms, -1 disables. */
  msOff: { type: 'integer', unit: 'ms', min: '-1', max: '2147483647', enumValues: null },
  fraction: { type: 'real', unit: null, min: '0', max: '1', enumValues: null },
  mode: {
    type: 'enum',
    unit: null,
    min: null,
    max: null,
    enumValues: ['local', 'remote_write', 'remote_apply', 'on', 'off'],
  },
  text: { type: 'string', unit: null, min: null, max: null, enumValues: null },
} satisfies Record<string, ValueSpec>

export type SpecName = keyof typeof SPECS

export interface Vector {
  spec: SpecName
  input: string
  /** The normalized text when the value is accepted; null when it is refused. */
  normalized: string | null
  /** The value in the base unit of the parameter, when accepted. */
  base?: string
}

const ok = (spec: SpecName, input: string, normalized: string, base?: string): Vector =>
  base === undefined ? { spec, input, normalized } : { spec, input, normalized, base }
const no = (spec: SpecName, input: string): Vector => ({ spec, input, normalized: null })

export const VECTORS: Vector[] = [
  // booleans: the narrow set, any letter case
  ok('bool', 'on', 'on'),
  ok('bool', 'OFF', 'off'),
  ok('bool', 'Yes', 'on'),
  ok('bool', 'no', 'off'),
  ok('bool', 'true', 'on'),
  ok('bool', 'FALSE', 'off'),
  ok('bool', '1', 'on'),
  ok('bool', '0', 'off'),
  no('bool', 'o'),
  no('bool', 'of'),
  no('bool', 't'),
  no('bool', 'y'),
  no('bool', ' on'),
  no('bool', 'on '),
  no('bool', ''),
  no('bool', '2'),
  // integers without a unit
  ok('count', '5', '5', '5'),
  ok('count', '+5', '5'),
  ok('count', '100', '100'),
  ok('count', '1', '1'),
  no('count', '0'),
  no('count', '101'),
  no('count', '-5'),
  no('count', '007'),
  no('count', '010'),
  no('count', '0x10'),
  no('count', '1e2'),
  no('count', '1.5'),
  no('count', '5MB'),
  no('count', '5 '),
  no('count', ' 5'),
  no('count', ''),
  no('count', 'five'),
  no('count', '9999999999999999999999'),
  // sizes in kB
  ok('kb', '4096', '4MB', '4096'),
  ok('kb', '4MB', '4MB', '4096'),
  ok('kb', '4 MB', '4MB', '4096'),
  ok('kb', '64kB', '64kB', '64'),
  ok('kb', '65536B', '64kB', '64'),
  ok('kb', '1GB', '1GB', '1048576'),
  ok('kb', '1.5MB', '1536kB', '1536'),
  ok('kb', '1TB', '1TB', '1073741824'),
  ok('kb', '100', '100kB', '100'),
  no('kb', '63kB'),
  no('kb', '64B'),
  no('kb', '0.5kB'),
  no('kb', '2TB'),
  no('kb', '4mb'),
  no('kb', '4MiB'),
  no('kb', '0x40kB'),
  no('kb', '1e3kB'),
  no('kb', '4 xb'),
  no('kb', 'MB'),
  // sizes in pages of 8 kB
  ok('pages', '128MB', '128MB', '16384'),
  ok('pages', '16384', '128MB', '16384'),
  ok('pages', '1GB', '1GB', '131072'),
  ok('pages', '16', '128kB', '16'),
  ok('pages', '136kB', '136kB', '17'),
  no('pages', '8kB'),
  no('pages', '129kB'),
  no('pages', '0'),
  // times in ms
  ok('ms', '1s', '1s', '1000'),
  ok('ms', '1000', '1s', '1000'),
  ok('ms', '1500', '1500ms', '1500'),
  ok('ms', '1.5s', '1500ms', '1500'),
  ok('ms', '1min', '1min', '60000'),
  ok('ms', '1h', '1h', '3600000'),
  ok('ms', '1d', '1d', '86400000'),
  ok('ms', '1000us', '1ms', '1'),
  ok('ms', '0', '0', '0'),
  no('ms', '-1'),
  no('ms', '100us'),
  no('ms', '25d'),
  no('ms', '1.0005s'),
  no('ms', '1S'),
  // times in s
  ok('sec', '600', '10min', '600'),
  ok('sec', '1min', '1min', '60'),
  ok('sec', '90s', '90s', '90'),
  ok('sec', '2000ms', '2s', '2'),
  no('sec', '601'),
  no('sec', '0'),
  no('sec', '1500ms'),
  // times in min
  ok('min', '1h', '1h', '60'),
  ok('min', '90', '90min', '90'),
  no('min', '30s'),
  // a setting that switches itself off with -1
  ok('msOff', '-1', '-1', '-1'),
  ok('msOff', '0', '0', '0'),
  no('msOff', '-2'),
  // reals
  ok('fraction', '0.5', '0.5'),
  ok('fraction', '0.50', '0.5'),
  ok('fraction', '1', '1'),
  ok('fraction', '0', '0'),
  no('fraction', '1.1'),
  no('fraction', '-0.1'),
  no('fraction', '.5'),
  no('fraction', '5.'),
  no('fraction', '01'),
  no('fraction', '1e-1'),
  no('fraction', '0x1'),
  no('fraction', 'abc'),
  no('fraction', ''),
  // enums: any letter case, the spelling of the server comes back
  ok('mode', 'ON', 'on'),
  ok('mode', 'Remote_Write', 'remote_write'),
  ok('mode', 'local', 'local'),
  no('mode', 'true'),
  no('mode', 'maybe'),
  no('mode', ' on'),
  no('mode', ''),
  // strings
  ok('text', 'x', 'x'),
  ok('text', '', ''),
  ok('text', 'café', 'café'),
  ok('text', "x' include 'y", "x' include 'y"),
  ok('text', 'a'.repeat(4096), 'a'.repeat(4096)),
  no('text', 'a'.repeat(4097)),
  no('text', 'é'.repeat(2049)),
  no('text', 'a\nb'),
  no('text', 'a\rb'),
  no('text', 'a\u0000b'),
  no('text', '\ud800'),
]
