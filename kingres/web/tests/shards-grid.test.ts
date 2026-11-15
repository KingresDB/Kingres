// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
import { describe, expect, it } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import BucketGrid from '@/components/BucketGrid.vue'
import ShardsPage from '@/pages/ShardsPage.vue'
import type { Bucket } from '@/api/types'
import { renderComponent } from './helpers'

function bucket(n: number, group: number, extra: Partial<Bucket> = {}): Bucket {
  return {
    bucket: n,
    shardGroupId: group,
    state: 'active',
    primaryServerId: `10.0.0.${10 + group}`,
    replicaServerIds: [`10.0.0.${10 + (group % 3) + 1}`],
    sizeBytes: 1024,
    movingToShardGroupId: null,
    ...extra,
  }
}

describe('BucketGrid', () => {
  const buckets = [
    bucket(0, 1),
    bucket(1, 2),
    bucket(2, 3),
    bucket(3, 1),
    bucket(4, 2, { state: 'moving', movingToShardGroupId: 3 }),
    bucket(5, 3, { state: 'orphan' }),
  ]

  it('renders one cell per bucket, coloured and numbered by the owning group', () => {
    render(BucketGrid, { props: { buckets, groupIds: [1, 2, 3] } })
    const cells = screen.getAllByTestId('bucket-cell')
    expect(cells).toHaveLength(6)
    expect(cells.map((c) => c.getAttribute('data-group'))).toEqual(['1', '2', '3', '1', '2', '3'])
    // Colour is taken from the group's position in groupIds, and the number is in the cell.
    expect(cells[0]?.getAttribute('style')).toContain('var(--g-1)')
    expect(cells[1]?.getAttribute('style')).toContain('var(--g-2)')
    expect(cells[2]?.getAttribute('style')).toContain('var(--g-3)')
    expect(cells[0]).toHaveTextContent('1')
    expect(cells[2]).toHaveTextContent('3')
  })

  it('marks moving and orphan buckets with a symbol, not only a colour', () => {
    render(BucketGrid, { props: { buckets, groupIds: [1, 2, 3] } })
    const cells = screen.getAllByTestId('bucket-cell')
    expect(cells[4]).toHaveAttribute('data-state', 'moving')
    expect(cells[4]).toHaveTextContent('→')
    expect(cells[4]).toHaveAttribute('title', expect.stringContaining('moving to group 3'))
    expect(cells[5]).toHaveAttribute('data-state', 'orphan')
    expect(cells[5]).toHaveTextContent('✕')
  })

  it('offers a table with the counts per shard group for screen readers', () => {
    render(BucketGrid, { props: { buckets, groupIds: [1, 2, 3] } })
    const group = screen.getByRole('group', { name: /bucket map with 6 buckets/i })
    expect(group).toBeInTheDocument()
    const table = screen.getByRole('table', { name: /buckets per shard group/i })
    const row2 = within(table).getByRole('row', { name: /group 2/i })
    expect(
      within(row2)
        .getAllByRole('cell')
        .map((c) => c.textContent),
    ).toEqual(['2', '1', '0'])
    const row3 = within(table).getByRole('row', { name: /group 3/i })
    expect(
      within(row3)
        .getAllByRole('cell')
        .map((c) => c.textContent),
    ).toEqual(['2', '0', '1'])
  })

  it('dims buckets that do not match the filter', () => {
    render(BucketGrid, {
      props: { buckets, groupIds: [1, 2, 3], isDimmed: (b: Bucket) => b.shardGroupId !== 1 },
    })
    const dim = screen.getAllByTestId('bucket-cell').filter((c) => c.classList.contains('dim'))
    expect(dim).toHaveLength(4)
  })
})

describe('Shards page', () => {
  it('draws the 128-bucket map of the sharded database and filters by server', async () => {
    const user = userEvent.setup()
    await renderComponent(ShardsPage)
    await waitFor(() => expect(screen.getAllByTestId('bucket-cell')).toHaveLength(128))

    const cells = screen.getAllByTestId('bucket-cell')
    const groups = new Set(cells.map((c) => c.getAttribute('data-group')))
    expect([...groups].sort()).toEqual(['1', '2', '3'])

    // Highlight server 1: it is primary of group 1 and standby of group 3 only.
    await screen.findByRole('option', { name: /10.0.0.11/ })
    await user.selectOptions(screen.getByLabelText(/highlight server/i), '10.0.0.11')
    const bright = screen
      .getAllByTestId('bucket-cell')
      .filter((c) => !c.classList.contains('dim'))
      .map((c) => c.getAttribute('data-group'))
    expect(bright.length).toBeGreaterThan(0)
    expect(new Set(bright)).toEqual(new Set(['1', '3']))

    // Per-server counts come from the bucket-distribution endpoint.
    expect(await screen.findByText('Buckets per server')).toBeInTheDocument()
  })

  it('finds a bucket by number and shows its owner', async () => {
    const user = userEvent.setup()
    await renderComponent(ShardsPage)
    await waitFor(() => expect(screen.getAllByTestId('bucket-cell')).toHaveLength(128))
    await user.type(screen.getByLabelText(/find bucket number/i), '7')
    await user.click(screen.getByRole('button', { name: /^find$/i }))
    const detail = await screen.findByTestId('bucket-detail')
    expect(detail).toHaveTextContent('Bucket 7')
    expect(detail).toHaveTextContent(/shard group \d/i)
  })
})
