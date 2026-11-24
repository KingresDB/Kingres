// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co
//
// Contract 0.31.0: the report of a rebalance names the distributed schema that a move carried, as the plan and the status do.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { screen, within } from '@testing-library/vue'
import { endpoints } from '@/api/endpoints'
import RebalanceReportDialog from '@/components/RebalanceReportDialog.vue'
import { renderComponent } from './helpers'

afterEach(() => vi.restoreAllMocks())

describe('the report of a rebalance with a distributed schema', () => {
  it('shows the schema by its name as text, and the bucket of a hash group by its number', async () => {
    vi.spyOn(endpoints, 'getRebalanceReport').mockResolvedValue({
      id: 9,
      outcome: 'completed',
      startedAt: '2026-11-24T10:00:00Z',
      finishedAt: '2026-11-24T10:05:00Z',
      stoppedBy: null,
      error: null,
      totals: { planned: 2, done: 2, failed: 0, skipped: 0, bytesMoved: 10 },
      placementBefore: [],
      placementAfter: [],
      moves: [
        {
          databaseName: 'shop',
          colocationId: 4,
          bucket: 0,
          fromShardGroupId: 1,
          toShardGroupId: 2,
          outcome: 'done',
          kind: 'schema',
          schemaName: '<img src=x onerror=alert(1)>',
        },
        {
          databaseName: 'shop',
          colocationId: 1,
          bucket: 17,
          fromShardGroupId: 1,
          toShardGroupId: 2,
          outcome: 'done',
        },
      ],
    } as never)
    await renderComponent(RebalanceReportDialog, { reportId: 9 })
    const cells = await screen.findAllByText(/^Schema/)
    expect(cells).toHaveLength(1)
    const row = cells[0]!.closest('tr') as HTMLElement
    expect(within(row).getByText('<img src=x onerror=alert(1)>')).toBeInTheDocument()
    expect(document.querySelector('img[src="x"]')).toBeNull()
    expect(screen.getByText('17')).toBeInTheDocument()
  })
})
