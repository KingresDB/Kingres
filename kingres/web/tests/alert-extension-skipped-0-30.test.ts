// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

import { describe, expect, it } from 'vitest'

import { ALERT_KIND_LABEL, alertKindHint, alertKindLabel } from '@/lib/status'

describe('the alert kind of 0.30.0', () => {
  it('extensionSkipped has words and an explanation', () => {
    expect(ALERT_KIND_LABEL.extensionSkipped).toBe(
      'An extension was not installed in a database on a server',
    )
    expect(alertKindLabel('extensionSkipped')).not.toBe('extensionSkipped')
    const hint = alertKindHint('extensionSkipped') ?? ''
    expect(hint).toMatch(/owner/i)
    expect(hint).toMatch(/dropped/i)
  })
})
