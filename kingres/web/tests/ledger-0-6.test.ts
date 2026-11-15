// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.6.0, ledger: `complete` and `uncoveredShardGroupIds` of a digest and of a verification. A passed
// verification or a digest that did not cover every shard group is never shown as covering the whole database,
// and the badge for "passed but incomplete" differs from "passed" in words and in symbol, not only in colour.
// Also: the generated InclusionProof type is used for the parsed proof.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import {
  coverageOf,
  digestCoverageText,
  verificationBadge,
  verificationCoverageText,
} from '@/lib/ledger-coverage'
import { LEDGER_STATE } from '@/lib/status'
import { parseProof } from '@/lib/ledger-proof'
import { renderApp } from './helpers'

const saved: { name: string; blob: Blob }[] = []

beforeEach(() => {
  saved.length = 0
  URL.createObjectURL = (blob: Blob) => {
    saved.push({ name: '', blob })
    return `blob:test/${saved.length}`
  }
  URL.revokeObjectURL = () => undefined
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
    this: HTMLAnchorElement,
  ) {
    const last = saved[saved.length - 1]
    if (last) last.name = this.download
  })
})
afterEach(() => vi.restoreAllMocks())

describe('coverage of a digest or a verification', () => {
  it('complete only when the server said so and named no uncovered group', () => {
    expect(coverageOf({ complete: true, uncoveredShardGroupIds: [] }).state).toBe('complete')
    expect(coverageOf({ complete: true }).state).toBe('complete')
    expect(coverageOf({ complete: false, uncoveredShardGroupIds: [3] })).toEqual({
      state: 'incomplete',
      uncovered: [3],
    })
    // A contradiction is read the safe way.
    expect(coverageOf({ complete: true, uncoveredShardGroupIds: [2] }).state).toBe('incomplete')
    expect(coverageOf({ complete: false }).state).toBe('incomplete')
  })
  it('an answer without the members is "unknown", never "complete"', () => {
    expect(coverageOf({}).state).toBe('unknown')
    expect(verificationCoverageText(coverageOf({}))).toMatch(
      /did not say whether every shard group is covered/i,
    )
    expect(digestCoverageText(coverageOf({}))).toMatch(
      /did not say whether every shard group is covered/i,
    )
  })
  it('names the uncovered groups as numbers', () => {
    const c = coverageOf({ complete: false, uncoveredShardGroupIds: [3, 5] })
    expect(digestCoverageText(c)).toBe(
      'This digest does not cover shard groups 3, 5: it proves nothing about them.',
    )
    expect(verificationCoverageText(c)).toMatch(/did not cover shard groups 3, 5/)
  })
})

describe('the badge of "passed"', () => {
  const full = verificationBadge({ state: 'passed', complete: true, uncoveredShardGroupIds: [] })
  const part = verificationBadge({ state: 'passed', complete: false, uncoveredShardGroupIds: [3] })
  const unknown = verificationBadge({ state: 'passed' })
  it('differs in label and in symbol, not only in tone', () => {
    expect(part.label).toBe('Passed, incomplete')
    expect(full.label).not.toBe(part.label)
    expect(full.icon).not.toBe(part.icon)
    expect(unknown.label).not.toBe(part.label)
    expect(unknown.label).not.toBe(full.label)
    expect(unknown.icon).not.toBe(full.icon)
    expect(unknown.icon).not.toBe(part.icon)
  })
  it('keeps the other states as they were', () => {
    expect(verificationBadge({ state: 'failed' }).label).toBe('Inconsistent')
    expect(verificationBadge({ state: 'running' }).label).toBe('Verifying')
    expect(verificationBadge({ state: 'never' }).label).toBe('Never verified')
  })
  it('the summary badge of a database never says "verified"', () => {
    expect(LEDGER_STATE.passed.label).toBe('Last check passed')
  })
})

/** An incomplete passed verification (id 17) and an incomplete digest (id 4), newest first. */
function seedIncomplete(): void {
  const s = getState()
  const base = s.ledger.verifications[0]!
  s.ledger.verifications.unshift({
    ...structuredClone(base),
    id: 17,
    state: 'passed',
    inconsistencies: [],
    complete: false,
    uncoveredShardGroupIds: [3],
  })
  const d = s.ledger.digests[0]!
  s.ledger.digests.unshift({
    ...structuredClone(d),
    id: 4,
    chains: d.chains.filter((c) => c.shardGroupId !== 3),
    aggregateHash: null,
    complete: false,
    uncoveredShardGroupIds: [3],
  })
}

async function openLedger() {
  const user = userEvent.setup()
  const app = await renderApp('/ledger')
  await screen.findByText(/status of shop/i)
  return { user, ...app }
}
const section = (name: string) =>
  screen.getByRole('heading', { name }).closest('section') as HTMLElement

describe('ledger page', () => {
  it('the verification list tells a complete pass from an incomplete one', async () => {
    seedIncomplete()
    await openLedger()
    const list = section('Verification')
    await waitFor(() => expect(within(list).getByText('Passed, incomplete')).toBeInTheDocument())
    expect(within(list).getAllByText('Passed, all groups').length).toBeGreaterThan(0)
    expect(within(list).getByTestId('verification-coverage-17')).toHaveTextContent(
      /did not cover shard group 3/i,
    )
    // A complete pass carries no warning.
    expect(within(list).queryByTestId('verification-coverage-16')).toBeNull()
  })

  it('the detail dialog says which groups were not covered', async () => {
    seedIncomplete()
    const { user } = await openLedger()
    await user.click(await screen.findByRole('button', { name: 'Details of verification 17' }))
    const dialog = await screen.findByRole('dialog')
    await within(dialog).findByText('Passed, incomplete')
    expect(within(dialog).getByTestId('verification-coverage')).toHaveTextContent(
      /not a check of the whole database/i,
    )
    expect(within(dialog).getByTestId('verification-covered')).toHaveTextContent(
      /not all\. not covered: shard group 3/i,
    )
  })

  it('the dialog of a complete verification says it covered every group, without a warning', async () => {
    const { user } = await openLedger()
    await user.click(await screen.findByRole('button', { name: 'Details of verification 16' }))
    const dialog = await screen.findByRole('dialog')
    await within(dialog).findByText('Passed, all groups')
    expect(within(dialog).getByTestId('verification-covered')).toHaveTextContent(
      /all shard groups of the database/i,
    )
    expect(within(dialog).queryByTestId('verification-coverage')).toBeNull()
  })

  it('an answer without the members is shown as "coverage not stated"', async () => {
    const s = getState()
    for (const v of s.ledger.verifications) {
      delete v.complete
      delete v.uncoveredShardGroupIds
    }
    const { user } = await openLedger()
    const list = section('Verification')
    await waitFor(() =>
      expect(within(list).getAllByText('Passed, coverage not stated').length).toBeGreaterThan(0),
    )
    expect(within(list).queryByText('Passed, all groups')).toBeNull()
    await user.click(screen.getAllByRole('button', { name: /details of verification/i })[0]!)
    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByTestId('verification-covered')).toHaveTextContent(
      /not stated by the server/i,
    )
  })

  it('the digest list names the groups a digest does not cover and shows no aggregate hash', async () => {
    seedIncomplete()
    await openLedger()
    const list = section('Digests')
    const note = await within(list).findByTestId('digest-coverage-4')
    expect(note).toHaveTextContent(
      'This digest does not cover shard group 3: it proves nothing about them.',
    )
    expect(within(list).queryByTestId('digest-coverage-3')).toBeNull()
    // Its chains: groups 1 and 2 only; the aggregate hash is null and says so.
    const row = within(list).getByRole('rowheader', { name: '4' }).closest('tr')!
    expect(within(row).getAllByRole('button', { name: /download document/i })).toHaveLength(2)
    expect(row).toHaveTextContent(/none \(not for an incomplete digest\)/i)
  })

  it('says next to a download that the digest does not cover a group, and saves the bytes unchanged', async () => {
    seedIncomplete()
    const { user } = await openLedger()
    const list = section('Digests')
    const button = await within(list).findByRole('button', {
      name: 'Download document of chain 1 of digest 4',
    })
    await user.click(button)
    const note = await screen.findByTestId('digest-downloaded')
    expect(within(note).getByTestId('digest-downloaded-coverage')).toHaveTextContent(
      /does not cover shard group 3: it proves nothing about them/i,
    )
    expect(saved).toHaveLength(1)
    const text = await saved[0]!.blob.text()
    expect(text).not.toMatch(/cover/i) // the warning is on screen only; the file is the server's text
    expect(JSON.parse(text)).toMatchObject({ chain_id: 'chain-1' })
  })

  it('a download of a complete digest has no coverage warning', async () => {
    const { user } = await openLedger()
    await user.click(
      await within(section('Digests')).findByRole('button', {
        name: 'Download document of chain 1 of digest 3',
      }),
    )
    const note = await screen.findByTestId('digest-downloaded')
    expect(within(note).queryByTestId('digest-downloaded-coverage')).toBeNull()
  })

  it('a hostile group number does not break the layout', async () => {
    seedIncomplete()
    getState().ledger.digests[0]!.uncoveredShardGroupIds = [2147483647, 0, -1]
    await openLedger()
    const note = await within(section('Digests')).findByTestId('digest-coverage-4')
    expect(note).toHaveTextContent(/shard groups 2147483647, 0, -1/)
  })

  it('the status summary says it is the latest check only', async () => {
    await openLedger()
    expect(await screen.findAllByText('Last check passed')).not.toHaveLength(0)
    expect(screen.queryByText('Verified')).toBeNull()
    expect(screen.getByText(/is the state of the latest verification only/i)).toBeInTheDocument()
  })
})

describe('the mock follows the contract', () => {
  it('sends complete and uncoveredShardGroupIds on every digest and verification', () => {
    const s = getState()
    for (const x of [...s.ledger.digests, ...s.ledger.verifications]) {
      expect(typeof x.complete).toBe('boolean')
      expect(Array.isArray(x.uncoveredShardGroupIds)).toBe(true)
    }
  })

  it('with incompleteLedger a new digest leaves a group out and has no aggregate hash', async () => {
    mockConfig.incompleteLedger = true
    const user = userEvent.setup()
    await renderApp('/ledger')
    await screen.findByText(/status of shop/i)
    await user.click(await screen.findByRole('button', { name: /generate digest/i }))
    const list = section('Digests')
    await waitFor(() => expect(within(list).getAllByRole('rowheader').length).toBe(2))
    const fresh = getState().ledger.digests[0]!
    expect(fresh.complete).toBe(false)
    expect(fresh.aggregateHash).toBeNull()
    expect(fresh.uncoveredShardGroupIds).toHaveLength(1)
    expect(fresh.chains).toHaveLength(2)
    expect(within(list).getByTestId(`digest-coverage-${fresh.id}`)).toBeInTheDocument()
  })
})

describe('the parsed proof uses the generated type', () => {
  it('keeps integers from the raw text', () => {
    const raw =
      '{"format":1,"chain_id":"a","shard_group_id":1,"table_id":9007199254740993,"ledger_txid":1,"seq":1,"op":"I","row_hash":"b","entry_index":0,"entry_count":1,"entry_path":[],"tx_root":"c","user_name":"u","tx_index":0,"tx_path":[],"block":{"height":2,"tx_count":1,"tx_merkle_root":"d","block_hash":"e"},"chain":[]}'
    const p = parseProof(raw)
    expect(p.tableId).toBe('9007199254740993')
    expect(p.view.block.height).toBe(2)
    expect(p.shapeError).toBeNull()
  })
})
