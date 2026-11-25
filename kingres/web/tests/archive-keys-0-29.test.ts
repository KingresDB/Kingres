// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

import { describe, expect, it } from 'vitest'

import { exportDatabaseRequest, tableKey } from '@/lib/archive'

describe('the tables that an export leaves out are named as pairs', () => {
  it('does not take two tables for one when a name has a dot', () => {
    const a = tableKey({ schema: 'a.b', name: 'c' })
    const b = tableKey({ schema: 'a', name: 'b.c' })
    expect(a).not.toBe(b)
    const request = exportDatabaseRequest({
      dataMode: 'all',
      excluded: [a, b],
      acknowledgeLargeObjectsLost: false,
      retentionHours: 24,
    })
    expect(request.excludeTables).toEqual([
      { schema: 'a.b', table: 'c' },
      { schema: 'a', table: 'b.c' },
    ])
  })
})
