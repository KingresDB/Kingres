// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/vue'
import { axe } from 'vitest-axe'
import BrandLogo from '@/components/BrandLogo.vue'
import { resetMockState } from '@/mocks/http'
import { renderApp } from './helpers'

const webRoot = process.cwd()
const brandSvg = (name: string) => readFileSync(join(webRoot, '../brand/svg', name), 'utf8')

describe('BrandLogo', () => {
  it.each(['mark', 'horizontal', 'stacked'] as const)(
    'renders the %s variant with the accessible name KingresDB',
    (variant) => {
      render(BrandLogo, { props: { variant, size: 40 } })
      const logo = screen.getByRole('img', { name: 'KingresDB' })
      expect(logo).toHaveAttribute('data-variant', variant)
      expect(logo).toHaveAttribute('height', '40')
    },
  )

  it('keeps the proportions of the supplied files', () => {
    render(BrandLogo, { props: { variant: 'horizontal', size: 34 } })
    // viewBox 1006 x 340 at height 34 is 100.6 wide.
    expect(screen.getByRole('img', { name: 'KingresDB' })).toHaveAttribute('width', '101')
  })

  it('is hidden from assistive technology when decorative', () => {
    const { container } = render(BrandLogo, { props: { variant: 'stacked', decorative: true } })
    expect(screen.queryByRole('img', { name: 'KingresDB' })).not.toBeInTheDocument()
    const svg = container.querySelector('svg')
    expect(svg).toHaveAttribute('aria-hidden', 'true')
    expect(svg).not.toHaveAttribute('aria-label')
  })

  it('uses unique gradient and clip-path ids per instance', () => {
    const { container } = render({
      components: { BrandLogo },
      template:
        '<div><BrandLogo variant="mark" /><BrandLogo variant="horizontal" /><BrandLogo variant="stacked" /></div>',
    })
    const ids = [...container.querySelectorAll('[id]')].map((e) => e.id)
    expect(ids).toHaveLength(6)
    expect(new Set(ids).size).toBe(6)
    // Every reference points at an id inside its own svg.
    for (const svg of container.querySelectorAll('svg')) {
      const own = [...svg.querySelectorAll('[id]')].map((e) => e.id)
      for (const ref of svg.innerHTML.matchAll(/url\(#([^)]+)\)/g)) expect(own).toContain(ref[1])
    }
  })

  it('draws exactly the paths of the supplied SVG files, without a background', () => {
    const { container } = render(BrandLogo, { props: { variant: 'horizontal' } })
    const source = brandSvg('kingres-db-horizontal-color-light.svg')
    const drawn = [...container.querySelectorAll('path')].map((p) => p.getAttribute('d') ?? '')
    expect(drawn.length).toBeGreaterThanOrEqual(5)
    for (const d of drawn) expect(source).toContain(`d="${d}"`)
    expect(container.querySelector('svg > rect')).toBeNull()
    // The shield keeps the gold gradient of the brand.
    const stops = [...container.querySelectorAll('stop')].map((s) => s.getAttribute('stop-color'))
    expect(stops).toEqual(['#F6D365', '#C8962E'])
  })

  it('takes the theme-dependent parts from CSS custom properties, not from fixed colours', () => {
    const vue = readFileSync(join(webRoot, 'src/components/BrandLogo.vue'), 'utf8')
    expect(vue).toContain('fill: var(--logo-crown)')
    expect(vue).toContain('fill: var(--logo-db)')
    const tokens = readFileSync(join(webRoot, 'src/styles/tokens.css'), 'utf8')
    // Light = color-light (ink + deep gold); dark = color-dark (cream + light gold).
    expect(tokens).toMatch(/--logo-crown: var\(--brand-ink\)/)
    expect(tokens).toMatch(/--logo-db: var\(--brand-gold-deep\)/)
    expect(tokens).toMatch(/--logo-crown: var\(--brand-cream\)/)
    expect(tokens).toMatch(/--logo-db: var\(--brand-gold-light\)/)
  })
})

describe('where the logo appears', () => {
  it('the top bar has the horizontal lockup, named KingresDB, linking to the dashboard', async () => {
    await renderApp('/servers')
    const link = await screen.findByRole('link', { name: 'KingresDB' })
    expect(link).toHaveAttribute('href', '/dashboard')
    expect(within(link).getByRole('img', { name: 'KingresDB' })).toHaveAttribute(
      'data-variant',
      'horizontal',
    )
  })

  it('the sign-in page has the stacked logo above the form, as decoration next to the heading', async () => {
    const { container } = await renderApp('/login', { user: null })
    const heading = await screen.findByRole('heading', { name: /sign in to kingresdb/i })
    const logo = container.querySelector('svg[data-variant="stacked"]')
    expect(logo).not.toBeNull()
    expect(logo).toHaveAttribute('aria-hidden', 'true')
    // The logo comes before the form in the document.
    const form = heading.closest('form') as HTMLElement
    expect(logo!.compareDocumentPosition(form) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    expect(await axe(container)).toHaveNoViolations()
  })

  it('shows a notice on the sign-in page when the session has expired', async () => {
    const { router } = await renderApp('/login?expired=1', { user: null })
    expect(router.currentRoute.value.query.expired).toBe('1')
    expect(await screen.findByText(/your session has expired/i)).toBeInTheDocument()
  })

  it('every step of the first-run wizard has the logo', async () => {
    resetMockState({ uninitialized: true })
    const { container } = await renderApp('/', { user: null })
    await screen.findByLabelText(/setup token/i)
    expect(container.querySelector('svg[data-variant="horizontal"]')).not.toBeNull()
    expect(screen.getByRole('img', { name: 'KingresDB' })).toBeInTheDocument()
    expect(await axe(container)).toHaveNoViolations()
  })

  it('the not-found page shows the mark as decoration', async () => {
    const { container } = await renderApp('/no/such/page')
    await screen.findByRole('heading', { level: 1, name: 'Page not found' })
    expect(container.querySelector('main svg[data-variant="mark"]')).toHaveAttribute(
      'aria-hidden',
      'true',
    )
    // The top bar still has the one logo that names the product.
    expect(screen.getAllByRole('img', { name: 'KingresDB' })).toHaveLength(1)
    expect(await axe(container)).toHaveNoViolations()
  })

  it('the not-allowed page shows the mark', async () => {
    const { container } = await renderApp('/security', { user: 'ops' })
    await screen.findByRole('heading', { level: 1, name: 'Not allowed' })
    await waitFor(() =>
      expect(container.querySelector('main svg[data-variant="mark"]')).not.toBeNull(),
    )
    expect(await axe(container)).toHaveNoViolations()
  })
})
