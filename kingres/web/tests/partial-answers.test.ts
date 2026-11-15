// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.5.0: cluster-wide logs, alerts and metrics can be partial (`partial`, `missingServers`).
// The views say so calmly, name each missing server and why, announce it once, and never show a partial
// list as "no alerts" or "no log lines".
/* eslint-disable vue/one-component-per-file -- tiny host components for the announcer */
import { describe, expect, it, vi } from 'vitest'
import { defineComponent, h, nextTick, ref, watch, type Ref } from 'vue'
import { render, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import {
  completenessAnnouncement,
  completenessOf,
  completenessSignature,
  mergeCompleteness,
  reasonWords,
  type Completeness,
} from '@/lib/partial'
import { useCompletenessAnnouncement } from '@/composables/useCompletenessAnnouncement'
import { createAnnouncer } from '@/composables/useAnnounce'
import { renderApp } from './helpers'

const MISSING = [
  { serverId: '10.0.0.12', reason: 'timeout' },
  { serverId: '10.0.0.13', reason: 'unreachable' },
] as const

describe('partial answers as data', () => {
  it('has plain words for every reason of the contract, and a sentence for one it does not know', () => {
    expect(reasonWords('timeout')).toBe('did not answer in time')
    expect(reasonWords('unreachable')).toBe('could not be reached')
    expect(reasonWords('refused')).toBe('refused the request')
    expect(reasonWords('invalidAnswer')).toBe('sent an answer that could not be used')
    expect(reasonWords('somethingNew')).toMatch(/reason this panel does not know/)
    expect(reasonWords('somethingNew')).not.toContain('somethingNew')
  })

  it('merges the pages of one list: partial if any page is, each server once', () => {
    const merged = mergeCompleteness([
      { partial: true, missingServers: [{ serverId: 'b', reason: 'timeout' }] },
      { partial: false },
      {
        partial: true,
        missingServers: [
          { serverId: 'b', reason: 'timeout' },
          { serverId: 'a', reason: 'refused' },
        ],
      },
    ])
    expect(merged.partial).toBe(true)
    expect(merged.missing).toEqual([
      { serverId: 'a', reason: 'refused' },
      { serverId: 'b', reason: 'timeout' },
    ])
    expect(mergeCompleteness([{}, undefined]).partial).toBe(false)
    // `partial` without names is still incomplete; names without `partial` too.
    expect(completenessOf({ partial: true }).partial).toBe(true)
    expect(completenessOf({ missingServers: [{ serverId: 'a', reason: 'timeout' }] }).partial).toBe(
      true,
    )
  })

  it('names hidden characters of a server id in what a screen reader gets', () => {
    const text = completenessAnnouncement(
      completenessOf({ partial: true, missingServers: [{ serverId: 'a‮b', reason: 'timeout' }] }),
      'list of alerts',
    )
    expect(text).not.toContain('‮')
    expect(text).toContain('U+202E')
  })
})

describe('the announcement', () => {
  /** A parent that provides the announcer and a child that uses the composable, like the real app. */
  function host(source: Ref<Completeness>) {
    const spoken: string[] = []
    const Child = defineComponent({
      setup() {
        useCompletenessAnnouncement(source, 'list of alerts')
        return () => h('span')
      },
    })
    const Parent = defineComponent({
      setup() {
        const announcer = createAnnouncer()
        watch(announcer.polite, (v) => v && spoken.push(v), { flush: 'sync' })
        return () => h(Child)
      },
    })
    render(Parent)
    return spoken
  }
  const answer = (...ids: string[]) =>
    completenessOf({
      partial: true,
      missingServers: ids.map((serverId) => ({ serverId, reason: 'timeout' as const })),
    })

  it('speaks when the list becomes incomplete, not for every poll that finds the same servers missing, and when it is complete again', async () => {
    vi.useFakeTimers()
    try {
      const source = ref<Completeness>(completenessOf({}))
      const spoken = host(source)
      await vi.advanceTimersByTimeAsync(100)
      expect(spoken).toEqual([]) // complete from the start: silence
      source.value = answer('a')
      await vi.advanceTimersByTimeAsync(100)
      expect(spoken).toHaveLength(1)
      expect(spoken[0]).toMatch(/list of alerts is incomplete. server a did not answer in time/)
      // Ten polls with the same answer (new objects each time): nothing more.
      for (let i = 0; i < 10; i += 1) {
        source.value = answer('a')
        await vi.advanceTimersByTimeAsync(100)
      }
      expect(spoken).toHaveLength(1)
      // Another server missing is news.
      source.value = answer('a', 'b')
      await vi.advanceTimersByTimeAsync(100)
      expect(spoken).toHaveLength(2)
      // Complete again: said once.
      source.value = completenessOf({})
      await vi.advanceTimersByTimeAsync(100)
      source.value = completenessOf({})
      await vi.advanceTimersByTimeAsync(100)
      expect(spoken).toHaveLength(3)
      expect(spoken[2]).toMatch(/complete again/)
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('the views', () => {
  it('Logs: names each missing server and why, and the list stays visible', async () => {
    mockConfig.partial = [...MISSING]
    await renderApp('/logs')
    const notice = await screen.findByTestId('partial-notice')
    expect(notice).toHaveTextContent('This list is incomplete')
    expect(notice).toHaveTextContent(/server\s*10\.0\.0\.12\s*did not answer in time/i)
    expect(notice).toHaveTextContent(/server\s*10\.0\.0\.13\s*could not be reached/i)
    expect(notice).toHaveTextContent(/log lines of these servers are not shown/i)
    // Calm: information, not an alert.
    expect(within(notice).queryByRole('alert')).toBeNull()
    expect(notice).not.toHaveAttribute('role', 'alert')
    expect(notice.querySelector('[aria-hidden="true"]')).not.toBeNull()
    // The lines the other servers know are there.
    expect(await screen.findAllByRole('row')).not.toHaveLength(0)
  })

  it('Logs: an empty partial list does not say "no log lines", a complete empty one does', async () => {
    const user = userEvent.setup()
    mockConfig.partial = [...MISSING]
    await renderApp('/logs')
    await user.type(await screen.findByLabelText(/contains text/i), 'no-such-text-anywhere')
    expect(
      await screen.findByText('No log lines from the servers that answered'),
    ).toBeInTheDocument()
    expect(document.body).toHaveTextContent(/does not mean there are none/i)
    expect(screen.queryByText('No log lines')).toBeNull()
    expect(document.body).not.toHaveTextContent(/nothing matches these filters/i)
  })

  it('Logs: a complete empty list still says "No log lines"', async () => {
    const user = userEvent.setup()
    await renderApp('/logs')
    await user.type(await screen.findByLabelText(/contains text/i), 'no-such-text-anywhere')
    expect(await screen.findByText('No log lines')).toBeInTheDocument()
    expect(screen.queryByTestId('partial-notice')).toBeNull()
  })

  it('Alerts: names the missing servers, and an empty partial list is not "No alerts"', async () => {
    mockConfig.partial = [
      { serverId: '10.0.0.12', reason: 'refused' },
      { serverId: '10.0.0.13', reason: 'invalidAnswer' },
    ]
    getState().alerts = []
    await renderApp('/alerts')
    const notice = await screen.findByTestId('partial-notice')
    expect(notice).toHaveTextContent(/server\s*10\.0\.0\.12\s*refused the request/i)
    expect(notice).toHaveTextContent(
      /server\s*10\.0\.0\.13\s*sent an answer that could not be used/i,
    )
    expect(notice).toHaveTextContent(/alerts held by these servers are not shown/i)
    expect(screen.getByText('No alerts from the servers that answered')).toBeInTheDocument()
    expect(screen.queryByText('No alerts')).toBeNull()
  })

  it('Alerts: the list that is complete says "No alerts" when it is empty', async () => {
    getState().alerts = []
    await renderApp('/alerts')
    expect(await screen.findByText('No alerts')).toBeInTheDocument()
    expect(screen.queryByTestId('partial-notice')).toBeNull()
  })

  it('Dashboard: charts and the open-alerts card each say what is missing', async () => {
    mockConfig.partial = [...MISSING]
    await renderApp('/dashboard')
    await waitFor(() => expect(screen.getAllByTestId('partial-notice')).toHaveLength(2))
    const [charts, alerts] = screen.getAllByTestId('partial-notice')
    expect(charts).toHaveTextContent(/charts show only what the other servers know/i)
    expect(charts).toHaveTextContent(/server\s*10\.0\.0\.12\s*did not answer in time/i)
    expect(alerts).toHaveTextContent(/alerts held by these servers/i)
  })

  it('Dashboard: no open alerts from a partial answer is not "Everything looks calm"', async () => {
    mockConfig.partial = [...MISSING]
    getState().alerts = []
    await renderApp('/dashboard')
    expect(
      await screen.findByText('No open alerts from the servers that answered'),
    ).toBeInTheDocument()
    expect(screen.queryByText(/everything looks calm/i)).toBeNull()
  })

  it('a partial answer without names is still called incomplete', async () => {
    mockServer.use(
      http.get('*/api/v1/alerts', () =>
        HttpResponse.json({ items: [], nextCursor: null, partial: true }),
      ),
    )
    await renderApp('/alerts')
    const notice = await screen.findByTestId('partial-notice')
    expect(notice).toHaveTextContent(/does not say which/i)
    expect(screen.getByText('No alerts from the servers that answered')).toBeInTheDocument()
  })

  it('the notice goes away when a later answer is complete, and the screen reader hears about it', async () => {
    mockConfig.partial = [...MISSING]
    const { queryClient } = await renderApp('/alerts')
    await screen.findByTestId('partial-notice')
    const polite = () => document.querySelector('.sr-only[aria-live="polite"]')?.textContent ?? ''
    await waitFor(() =>
      expect(polite()).toMatch(
        /this list of alerts is incomplete\. server 10\.0\.0\.12 did not answer in time; server 10\.0\.0\.13 could not be reached/i,
      ),
    )
    mockConfig.partial = null
    await queryClient.invalidateQueries({ queryKey: ['alerts'] })
    await waitFor(() => expect(screen.queryByTestId('partial-notice')).toBeNull())
    await waitFor(() => expect(polite()).toMatch(/list of alerts is complete again/i))
  })

  it('a poll that finds the same servers missing does not announce again', async () => {
    mockConfig.partial = [...MISSING]
    const { queryClient } = await renderApp('/alerts')
    await screen.findByTestId('partial-notice')
    const region = document.querySelector('.sr-only[aria-live="polite"]') as HTMLElement
    await waitFor(() => expect(region.textContent).toMatch(/is incomplete/))
    // Count what the region holds from now on: a repeat clears it first, then sets the text again.
    let announcements = 0
    const observer = new MutationObserver(() => {
      if (/is incomplete/.test(region.textContent ?? '')) announcements += 1
    })
    observer.observe(region, { childList: true, characterData: true, subtree: true })
    for (let i = 0; i < 3; i += 1) {
      await queryClient.refetchQueries({ queryKey: ['alerts'] })
      await nextTick()
    }
    observer.disconnect()
    expect(announcements).toBe(0)
    // And when another server goes missing, that is news: one more announcement.
    mockConfig.partial = [...MISSING, { serverId: '10.0.0.14', reason: 'timeout' }]
    await queryClient.refetchQueries({ queryKey: ['alerts'] })
    await waitFor(() =>
      expect(region.textContent).toMatch(/server 10\.0\.0\.14 did not answer in time/i),
    )
  })
})

describe('the announcement logic', () => {
  it('has one signature per set of missing servers, and none for a complete answer', () => {
    const a = completenessOf({
      partial: true,
      missingServers: [{ serverId: 'a', reason: 'timeout' }],
    })
    const same = completenessOf({
      partial: true,
      missingServers: [{ serverId: 'a', reason: 'timeout' }],
    })
    const other = completenessOf({
      partial: true,
      missingServers: [{ serverId: 'a', reason: 'refused' }],
    })
    expect(completenessSignature(a)).toBe(completenessSignature(same))
    expect(completenessSignature(a)).not.toBe(completenessSignature(other))
    expect(completenessSignature(completenessOf({ partial: false }))).toBe('')
  })
})
