// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.7.0: `LedgerStatus.complete` and `uncoveredShardGroupIds`. The chains of a status are those of the shard
// group of the answering server; the Ledger page says so the way it does for digests and verifications.
import { describe, expect, it } from 'vitest'
import { screen, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { mockConfig } from '@/mocks/state'
import { getState } from '@/mocks/http'
import { coverageOf, statusCoverageText } from '@/lib/ledger-coverage'
import { renderApp } from './helpers'

describe('the coverage of a ledger status', () => {
  it('names the shard groups the status does not cover, and says it is not the status of the whole database', async () => {
    await renderApp('/ledger')
    const note = await screen.findByTestId('status-coverage')
    expect(note).toHaveTextContent(
      /chains below are those of the shard group of the server that answered/i,
    )
    expect(note).toHaveTextContent(/shard groups 2, 3/)
    expect(note).toHaveTextContent(/not the status of the whole database/i)
    // Only the chain of the group of the answering server is in the table.
    const chains = within(screen.getByRole('table', { name: 'Chains' }))
    expect(chains.getByText('Group 1')).toBeInTheDocument()
    expect(chains.queryByText('Group 2')).toBeNull()
    // The passed state is still only the state of the latest verification.
    expect(screen.getByText(/is the state of the latest verification only/i)).toBeInTheDocument()
  })

  it('offers the groups the status does not cover as chains whose blocks can be asked for, marked as such', async () => {
    await renderApp('/ledger')
    await screen.findByTestId('status-coverage')
    const chain = screen.getByLabelText('Chain')
    const options = within(chain)
      .getAllByRole('option')
      .map((o) => o.textContent?.replace(/\s+/g, ' ').trim())
    expect(options).toEqual([
      'Shard group 1',
      'Shard group 2 (not in the status above)',
      'Shard group 3 (not in the status above)',
    ])
  })

  it('says nothing when the status covers every group', async () => {
    mockConfig.ledgerStatusCoverage = 'all'
    await renderApp('/ledger')
    const chains = within(await screen.findByRole('table', { name: 'Chains' }))
    expect(chains.getByText('Group 3')).toBeInTheDocument()
    expect(screen.queryByTestId('status-coverage')).toBeNull()
  })

  it('does not claim coverage when an older server sends neither member', async () => {
    mockConfig.ledgerStatusCoverage = 'absent'
    await renderApp('/ledger')
    const note = await screen.findByTestId('status-coverage')
    expect(note).toHaveTextContent(/did not say whether the chains below are all the chains/i)
  })

  it('a database that is not sharded is complete with its one chain', async () => {
    const reports = getState().databases.find((d) => d.info.name === 'reports')!
    reports.info.ledger = { mode: 'appendOnly', digestIntervalSeconds: 3600 }
    const user = userEvent.setup()
    await renderApp('/ledger')
    await screen.findByTestId('status-coverage')
    await user.selectOptions(screen.getByLabelText('Database'), 'reports')
    await screen.findByText(/status of reports/i)
    expect(screen.queryByTestId('status-coverage')).toBeNull()
    expect(
      within(screen.getByRole('table', { name: 'Chains' })).getByText('Group 0'),
    ).toBeInTheDocument()
  })

  it('readCoverage: a list that names groups wins over complete: true', () => {
    expect(coverageOf({ complete: true, uncoveredShardGroupIds: [4] })).toEqual({
      state: 'incomplete',
      uncovered: [4],
    })
    expect(coverageOf({ complete: true, uncoveredShardGroupIds: [] })).toEqual({
      state: 'complete',
    })
    expect(coverageOf({})).toEqual({ state: 'unknown' })
    expect(statusCoverageText({ state: 'complete' })).toBe('')
  })
})
