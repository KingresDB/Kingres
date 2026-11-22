// SPDX-License-Identifier: AGPL-3.0-only
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
    // 16 in 0.4.0, plus certificateRenewalFailed (0.5.0), replicationPending (0.6.0) and, in 0.7.0, leaseLost,
    // groupWithoutSynchronousStandby and groupUnderReplicated.
    // 0.10.0 added seven for failover: failoverNeedsDecision, failoverBlocked, failoverDataLoss, splitPromotion,
    // oldPrimaryRunning, oldPrimaryNotStopped and oldPrimaryNotRebuilt: 29.
    // 0.11.0 added nine for the ledger of phase 7: ledgerNotAnchored, ledgerAnchorBehind, ledgerAnchorFailed,
    // ledgerAnchorConflict, ledgerChainDiverged, ledgerGroupUnread, ledgerPublicationBehind, ledgerSignerUnavailable and
    // ledgerDigestOverdue: 38.
    // 0.13.0 added certificateAuthorityUnavailable: 39.
    // 0.16.0 added rebalanceFailed: 40.
    expect(Object.keys(ALERT_KIND_LABEL)).toHaveLength(40)
    // 18 in 0.4.0, plus the two certificate kinds (0.5.0), standby.reconcile (0.7.0), config.apply and config.restart (0.8.0).
    // 0.11.0 added eight: ledger.key.create, .rotate, .retire, .revoke, .distribute, ledger.bundle.create, ledger.epoch.start
    // and ledger.upgrade: 31.
    // 0.14.0 added table.undistribute, table.alter and table.partition: 34.
    // 0.16.0 added bucket.split, schema.distribute, schema.move and schema.undistribute: 38.
    expect(Object.keys(TASK_KIND_LABEL)).toHaveLength(38)
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
