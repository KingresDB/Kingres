// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co
import { describe, expect, it } from 'vitest'
import { mapGroupPauseHint } from '@/lib/failover'

describe('the pause of a hard failover of the map group (0.28.0)', () => {
  it('names the server that takes the repeated request', () => {
    const m =
      'The failover of the map group could lose data (possible): x Repeat the request with allowDataLoss set to true if you accept it. Send the request to server 10.0.0.12, which runs the bootstrap; the old leader was stopped.'
    expect(mapGroupPauseHint(m)).toEqual({ server: '10.0.0.12' })
  })
  it('is null for every other message', () => {
    expect(mapGroupPauseHint('waiting for the old lease')).toBeNull()
    expect(mapGroupPauseHint(null)).toBeNull()
    expect(mapGroupPauseHint(undefined)).toBeNull()
  })
})
