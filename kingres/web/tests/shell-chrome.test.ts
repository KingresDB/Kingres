// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The parts of the console that are on every page: the top bar and the copyright line.
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { screen, within } from '@testing-library/vue'
import { axe } from 'vitest-axe'
import { resetMockState } from '@/mocks/http'
import { renderApp } from './helpers'

const webRoot = process.cwd()

function expectCopyright(): void {
  const footer = screen.getByRole('contentinfo')
  expect(footer).toHaveTextContent(/Copyright © 2026 RooyeKhat Media Co\./)
  const link = within(footer).getByRole('link', { name: /kingresdb\.com/ })
  expect(link).toHaveAttribute('href', 'https://kingresdb.com')
  // A new tab that gets neither a handle on this page nor its address.
  expect(link).toHaveAttribute('target', '_blank')
  expect(link).toHaveAttribute('rel', 'noopener noreferrer')
  expect(link).toHaveAccessibleName(/opens in a new tab/)
}

describe('the copyright line', () => {
  it('is on the pages of the console', async () => {
    const { container } = await renderApp('/servers')
    await screen.findByRole('heading', { level: 1, name: 'Servers' })
    expectCopyright()
    expect(await axe(container)).toHaveNoViolations()
  })

  it('is on the sign-in page', async () => {
    await renderApp('/login', { user: null })
    await screen.findByRole('heading', { name: /sign in to kingresdb/i })
    expectCopyright()
  })

  it('is on the first-run wizard', async () => {
    resetMockState({ uninitialized: true })
    await renderApp('/', { user: null })
    await screen.findByLabelText(/setup token/i)
    expectCopyright()
  })
})

describe('the top bar', () => {
  it('names the menu button, the theme switch, the user with the role, and sign out', async () => {
    const { container } = await renderApp('/servers')
    await screen.findByRole('heading', { level: 1, name: 'Servers' })
    const bar = container.querySelector('header.topbar') as HTMLElement
    expect(bar).not.toBeNull()
    const menu = within(bar).getByRole('button', { name: 'Menu' })
    expect(menu).toHaveAttribute('aria-controls', 'side-nav')
    expect(menu).toHaveAttribute('aria-expanded', 'false')
    expect(within(bar).getByRole('button', { name: /switch theme/i })).toBeInTheDocument()
    expect(within(bar).getByRole('button', { name: 'Sign out' })).toBeInTheDocument()
    expect(bar).toHaveTextContent('admin')
    expect(bar).toHaveTextContent('kingres_admin')
  })

  it('takes its colours from tokens that the dark theme does not redefine', () => {
    const tokens = readFileSync(join(webRoot, 'src/styles/tokens.css'), 'utf8')
    // Defined once: a second definition would be a theme changing the colour of the bar.
    expect(tokens.match(/--topbar-bg:/g)).toHaveLength(1)
    expect(tokens.match(/--topbar-fg:/g)).toHaveLength(1)
    const shell = readFileSync(join(webRoot, 'src/layouts/ShellLayout.vue'), 'utf8')
    expect(shell).toContain('background: var(--topbar-bg)')
    // On black the logo uses its colouring for dark backgrounds in both themes.
    expect(shell).toContain('--logo-crown: var(--brand-cream)')
  })
})
