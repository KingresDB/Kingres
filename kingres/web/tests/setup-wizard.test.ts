// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
import { describe, expect, it } from 'vitest'
import { screen, waitFor } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { mockServer } from '@/mocks/server'
import { http, HttpResponse } from 'msw'
import { getState, resetMockState } from '@/mocks/http'
import { renderApp } from './helpers'

async function passToken(user: ReturnType<typeof userEvent.setup>, token = 'demo-setup-token') {
  await user.type(await screen.findByLabelText(/setup token/i), token)
  await user.click(screen.getByRole('button', { name: /continue/i }))
}

describe('First-run wizard', () => {
  it('is shown instead of any other page while the server is uninitialized', async () => {
    resetMockState({ uninitialized: true })
    const { router } = await renderApp('/servers', { user: null })
    await waitFor(() => expect(router.currentRoute.value.name).toBe('setup'))
    expect(await screen.findByRole('heading', { name: /set up this server/i })).toBeInTheDocument()
    expect(screen.getByLabelText(/setup token/i)).toBeInTheDocument()
  })

  it('rejects a wrong setup token on the first step, before any form is shown', async () => {
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await passToken(user, 'wrong-token')
    expect(await screen.findByText(/setup token rejected/i)).toBeInTheDocument()
    // Still on the token step: nothing to create or join is offered.
    expect(screen.getByLabelText(/setup token/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^create new cluster$/i })).not.toBeInTheDocument()
  })

  it('creates a cluster: token check, settings, progress from /setup/state, hand-off to sign-in', async () => {
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    const { router } = await renderApp('/', { user: null })

    // GET /tasks/{id} must never be used during setup (it needs a session).
    const taskReads: string[] = []
    mockServer.use(
      http.get('*/api/v1/tasks/:id', ({ request }) => {
        taskReads.push(request.url)
        return HttpResponse.json({ code: 'node_uninitialized' }, { status: 503 })
      }),
    )

    await passToken(user)
    await user.click(await screen.findByRole('button', { name: /^create new cluster$/i }))

    // Client-side validation first.
    await user.click(await screen.findByRole('button', { name: /^create cluster$/i }))
    expect(await screen.findByText(/use letters, digits/i)).toBeInTheDocument()
    expect(
      screen.getByText(/at least 12 characters\./i, { selector: '.error' }),
    ).toBeInTheDocument()

    await user.type(screen.getByLabelText(/cluster name/i), 'prod-eu')
    await user.type(screen.getByLabelText(/^password/i), 'a-very-long-password')
    await user.type(screen.getByLabelText(/repeat password/i), 'a-very-long-password')
    await user.click(screen.getByRole('button', { name: /^create cluster$/i }))

    expect(await screen.findByRole('progressbar', { name: /task progress/i })).toBeInTheDocument()
    await user.click(await screen.findByRole('button', { name: /continue to sign in/i }))

    await waitFor(() => expect(router.currentRoute.value.name).toBe('login'))
    expect(getState().node.state).toBe('clustered')
    expect(getState().clusterName).toBe('prod-eu')
    expect(taskReads).toEqual([])
    expect(
      await screen.findByRole('heading', { name: /sign in to kingresdb/i }),
    ).toBeInTheDocument()
  })

  it('shows a failed setup with its error and lets the user submit again', async () => {
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await passToken(user)
    await user.click(await screen.findByRole('button', { name: /^create new cluster$/i }))
    await user.type(screen.getByLabelText(/cluster name/i), 'failme')
    await user.type(screen.getByLabelText(/^password/i), 'a-very-long-password')
    await user.type(screen.getByLabelText(/repeat password/i), 'a-very-long-password')
    await user.click(screen.getByRole('button', { name: /^create cluster$/i }))

    expect((await screen.findAllByText(/data directory is not writable/i)).length).toBeGreaterThan(
      0,
    )
    expect(getState().node.state).toBe('uninitialized')

    await user.click(screen.getByRole('button', { name: /back to the settings/i }))
    const name = await screen.findByLabelText(/cluster name/i)
    await user.clear(name)
    await user.type(name, 'prod-eu')
    await user.click(screen.getByRole('button', { name: /^create cluster$/i }))
    expect(await screen.findByRole('button', { name: /continue to sign in/i })).toBeInTheDocument()
    expect(getState().node.state).toBe('clustered')
  })

  it('joins an existing cluster', async () => {
    resetMockState({ uninitialized: true })
    const user = userEvent.setup()
    await renderApp('/', { user: null })
    await passToken(user)
    await user.click(await screen.findByRole('button', { name: /^join existing cluster$/i }))

    await user.type(screen.getByLabelText(/address of any existing server/i), 'not an address')
    await user.click(screen.getByRole('button', { name: /^join cluster$/i }))
    expect(await screen.findByText(/use host:port/i, { selector: '.error' })).toBeInTheDocument()

    const address = screen.getByLabelText(/address of any existing server/i)
    await user.clear(address)
    await user.type(address, '10.0.0.11:8091')
    await user.type(screen.getByLabelText(/administrator password/i), 'demo-password')
    await user.click(screen.getByRole('button', { name: /^join cluster$/i }))

    expect(await screen.findByRole('button', { name: /continue to sign in/i })).toBeInTheDocument()
    expect(getState().node.state).toBe('clustered')
  })
})
