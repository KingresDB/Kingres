// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
import { afterEach, describe, expect, it } from 'vitest'
import { createMockState, mockConfig } from '@/mocks/state'
import { applySwitches } from '@/mocks/switches'

describe('the table switches of the mock', () => {
  const before = { ms: mockConfig.tableUndoMs, fails: mockConfig.tableUndoFails }
  afterEach(() => {
    mockConfig.tableUndoMs = before.ms
    mockConfig.tableUndoFails = before.fails
  })

  it('tables=undo:<ms> sets how long the undo of a cancelled task takes', () => {
    applySwitches(new URLSearchParams('tables=undo:731'), createMockState())
    expect(mockConfig.tableUndoMs).toBe(731)
  })

  it('tables=undo:fail sets the failure and leaves the duration alone', () => {
    applySwitches(new URLSearchParams('tables=undo:fail'), createMockState())
    expect(mockConfig.tableUndoFails).toBe(true)
    expect(mockConfig.tableUndoMs).toBe(before.ms)
  })
})
