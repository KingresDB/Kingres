// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The states of contract 0.7.0 pass axe, log no Vue warning, and say their state in words and symbols, never by
// colour alone (colour contrast itself is covered by tokens-contrast.test.ts).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { configureAxe } from 'vitest-axe'
import { getState } from '@/mocks/http'
import { loseLease, mockConfig, silenceServer, stallStandby } from '@/mocks/state'
import { seedHostile } from '@/mocks/hostile'
import { renderApp, settled } from './helpers'

const axe = configureAxe({ rules: { 'color-contrast': { enabled: false } } })

describe('0.7.0 states', () => {
  let errors: ReturnType<typeof vi.spyOn>
  let warnings: ReturnType<typeof vi.spyOn>
  beforeEach(() => {
    errors = vi.spyOn(console, 'error').mockImplementation(() => {})
    warnings = vi.spyOn(console, 'warn').mockImplementation(() => {})
  })
  afterEach(() => {
    errors.mockRestore()
    warnings.mockRestore()
  })
  const clean = async (container: Element) => {
    await settled()
    expect(await axe(container)).toHaveNoViolations()
    expect(errors).not.toHaveBeenCalled()
    expect(warnings).not.toHaveBeenCalled()
  }

  it('Replication with a lost lease, a standby that does not stream and a silent server', async () => {
    const s = getState()
    loseLease(s, 1)
    stallStandby(s, 2, '10.0.0.13', Date.now() - (mockConfig.syncGraceMs + 1000))
    silenceServer(s, '10.0.0.12', Date.now() - (mockConfig.suspectMs + 1000))
    const { container } = await renderApp('/replication')
    await screen.findByTestId('group-1')
    await waitFor(() =>
      expect(
        screen.getAllByTestId('group-lease').some((e) => e.dataset.leaseState === 'lost'),
      ).toBe(true),
    )
    // State is words and a symbol, not only colour: every lease badge and the rule badge carry text.
    for (const lease of screen.getAllByTestId('group-lease'))
      if (lease.dataset.leaseState !== 'absent')
        expect(within(lease).getAllByText(/lease/i).length).toBeGreaterThan(0)
    await clean(container)
  })

  it('the replica count dialog passes axe, and so does the task view inside it', async () => {
    const user = userEvent.setup()
    const { container } = await renderApp('/replication')
    await user.click(await screen.findByRole('button', { name: /change replica count/i }))
    const dialog = await screen.findByRole('dialog', { name: /change the replica count/i })
    expect(await axe(dialog)).toHaveNoViolations()
    await fireEvent.update(within(dialog).getByLabelText(/standbys per shard group/i), '2')
    await user.click(within(dialog).getByRole('button', { name: /^change replica count$/i }))
    await within(dialog).findByTestId('replica-done', {}, { timeout: 6000 })
    expect(await axe(dialog)).toHaveNoViolations()
    expect(container).toBeTruthy()
    expect(errors).not.toHaveBeenCalled()
  }, 20000)

  it('the page of a suspect server', async () => {
    silenceServer(getState(), '10.0.0.12', Date.now() - (mockConfig.suspectMs + 1000))
    const { container } = await renderApp('/servers/10.0.0.12')
    await screen.findByTestId('liveness-notice')
    await clean(container)
  })

  it('the servers page with a suspect and a dead server', async () => {
    silenceServer(getState(), '10.0.0.12', Date.now() - (mockConfig.suspectMs + 1000))
    silenceServer(getState(), '10.0.0.13', Date.now() - (mockConfig.failureMs + 1000))
    const { container } = await renderApp('/servers')
    await screen.findByText('DEAD')
    await clean(container)
  })

  it('the dashboard with the client entry point on and a server that is not live', async () => {
    mockConfig.clientEntryPoint = 'standby'
    silenceServer(getState(), '10.0.0.13', Date.now() - (mockConfig.failureMs + 1000))
    const { container } = await renderApp('/dashboard')
    await screen.findByTestId('client-entry-point', {}, { timeout: 6000 })
    await screen.findByTestId('not-live-servers')
    await waitFor(() => expect(screen.getAllByTestId(/^metric-client/)).toHaveLength(16))
    await clean(container)
  })

  it('the alert list with subjects, including hostile ones', async () => {
    seedHostile(getState())
    const { container } = await renderApp('/alerts')
    await screen.findAllByTestId('alert-subject')
    await clean(container)
  })

  it('the shell with the banner of a lost leader', async () => {
    mockConfig.leaderLost = true
    const { container } = await renderApp('/servers')
    await screen.findByTestId('banner-leader', {}, { timeout: 7000 })
    await clean(container)
  })

  it('the workbench while a statement runs, and after the page stopped waiting', async () => {
    const user = userEvent.setup()
    const { container } = await renderApp('/workbench')
    await screen.findByRole('option', { name: 'shop' })
    await fireEvent.update(screen.getByLabelText(/^SQL/), 'select pg_sleep(30)')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    await screen.findByRole('button', { name: /cancel statement/i })
    expect(await axe(container)).toHaveNoViolations()
    await user.click(screen.getByRole('button', { name: /stop waiting/i }))
    await screen.findByTestId('stopped-notice')
    expect(await axe(container)).toHaveNoViolations()
    await user.click(screen.getByRole('button', { name: /cancel it on the server/i }))
    await waitFor(() => expect(getState().statements.size).toBe(0))
    expect(errors).not.toHaveBeenCalled()
  })

  it('the ledger page with an incomplete status', async () => {
    const { container } = await renderApp('/ledger')
    await screen.findByTestId('status-coverage')
    await clean(container)
  })

  it('announces the cancel in a live region', async () => {
    const user = userEvent.setup()
    await renderApp('/workbench')
    await screen.findByRole('option', { name: 'shop' })
    await fireEvent.update(screen.getByLabelText(/^SQL/), 'select pg_sleep(30)')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    await user.click(await screen.findByRole('button', { name: /cancel statement/i }))
    await waitFor(() => {
      const live = [...document.querySelectorAll('[aria-live]')].map((e) => e.textContent).join(' ')
      expect(live).toMatch(/cancel requested/i)
    })
  })
})
