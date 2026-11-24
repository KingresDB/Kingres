// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The Extensions tab of Configuration: whether `kingres_columnar` and `pg_stat_statements` are preloaded on each server (the value of
// `shared_preload_libraries`), and the note that the contract has no install action and no per-database install state.
import { describe, expect, it } from 'vitest'
import { screen, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { renderApp } from './helpers'

describe('the Extensions tab', () => {
  it('shows both libraries as preloaded on every server, and says what the API does not offer', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('preload=visible'), getState())
    await renderApp('/configuration')
    await user.click(await screen.findByRole('tab', { name: /^extensions$/i }))
    const panel = await screen.findByTestId('extensions-panel')
    for (const lib of ['kingres_columnar', 'pg_stat_statements']) {
      const card = await within(panel).findByTestId(`ext-${lib}`)
      expect(within(card).getByTestId(`ext-${lib}-state`)).toHaveTextContent(
        /preloaded on every server/i,
      )
      expect(within(card).getByTestId(`ext-${lib}-10.0.0.11`)).toHaveTextContent(/yes/i)
    }
    const note = within(panel).getByTestId('ext-install-note')
    expect(note).toHaveTextContent(/no action that installs them/i)
    expect(within(note).getByRole('link', { name: /databases/i })).toHaveAttribute(
      'href',
      '/databases',
    )
  })

  it('is reachable by the address of the tab', async () => {
    applySwitches(new URLSearchParams('preload=visible'), getState())
    await renderApp('/configuration?tab=extensions')
    expect(await screen.findByTestId('extensions-panel')).toBeInTheDocument()
  })
})

describe('without the switch', () => {
  it('says that the API does not report the preloaded libraries, because PostgreSQL hides them', async () => {
    await renderApp('/configuration?tab=extensions')
    const card = await screen.findByTestId('ext-pg_stat_statements')
    expect(within(card).getByTestId('ext-pg_stat_statements-state')).toHaveTextContent(
      /not reported by the API/i,
    )
    expect(screen.getByTestId('ext-hidden-note')).toHaveTextContent(/hides the value/i)
  })

  it('shows a library that a server does not preload as such', async () => {
    applySwitches(new URLSearchParams('preload=visible'), getState())
    const rec = getState().config.servers['10.0.0.12']
    if (rec)
      rec.run['shared_preload_libraries'] = {
        value: 'kingres_shard,kingres_ledger',
        source: 'managed',
      }
    await renderApp('/configuration?tab=extensions')
    const card = await screen.findByTestId('ext-pg_stat_statements')
    expect(within(card).getByTestId('ext-pg_stat_statements-state')).toHaveTextContent(
      /not preloaded on every server/i,
    )
    expect(within(card).getByTestId('ext-pg_stat_statements-10.0.0.12')).toHaveTextContent(/no/i)
  })
})
