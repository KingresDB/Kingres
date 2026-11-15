// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.4.0 added alert kinds (ledgerPendingTooOld, versionMismatch, certificateExpiring), the task
// kind replicate.apply and the inconsistency kind `other`. Each has words; an unknown future kind is
// shown as sent, as text.
import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/vue'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { alertKindLabel, taskKindLabel, ALERT_KIND_LABEL, TASK_KIND_LABEL } from '@/lib/status'
import { renderApp } from './helpers'

describe('labels', () => {
  it('has words for the kinds added in 0.4.0', () => {
    for (const k of ['ledgerPendingTooOld', 'versionMismatch', 'certificateExpiring'])
      expect(alertKindLabel(k)).not.toBe(k)
    expect(taskKindLabel('replicate.apply')).not.toBe('replicate.apply')
    expect(Object.keys(ALERT_KIND_LABEL)).toHaveLength(17) // 16 in 0.4.0, plus certificateRenewalFailed of the 0.5.0 draft
    expect(Object.keys(TASK_KIND_LABEL)).toHaveLength(20) // 18 in 0.4.0, plus the two certificate kinds of the 0.5.0 draft
  })
  it('shows an unknown kind as sent', () => {
    expect(alertKindLabel('somethingNew')).toBe('somethingNew')
    expect(taskKindLabel('x.y')).toBe('x.y')
  })
})

describe('Alerts page', () => {
  it('names a certificateExpiring alert in words', async () => {
    mockServer.use(
      http.get('*/api/v1/alerts', () =>
        HttpResponse.json({
          items: [
            {
              id: 99,
              kind: 'certificateExpiring',
              severity: 'warning',
              state: 'open',
              title: 'Certificate of 10.0.0.13 expires in 9 days',
              message: 'Replace it.',
              serverId: '10.0.0.13',
              databaseName: null,
              raisedAt: '2026-11-15T00:00:00Z',
            },
          ],
          nextCursor: null,
        }),
      ),
    )
    await renderApp('/alerts')
    expect(await screen.findByText('Server certificate expiring')).toBeInTheDocument()
  })
})
