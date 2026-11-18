// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Ledger page, known limits closed in this round: the digest list pages (older digests can be
// downloaded), and when a proof is on screen each listed digest says whether it has the proof's chain
// and is not older than the proof's block. That compares identifiers; it never claims a verification.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { digestRelation, digestRelationText, type DigestChain } from '@/lib/ledger-proof'
import { renderApp } from './helpers'

type User = ReturnType<typeof userEvent.setup>

function chain(group: number, id: unknown, height: number): DigestChain {
  return {
    shardGroupId: group,
    blockHeight: height,
    blockHash: 'a'.repeat(64),
    document: { chain_id: id, height },
    rawDocument: '{}',
  }
}
const proof = { chain_id: 'AB'.repeat(32), shard_group_id: 2, block: { height: 100 } } as never

describe('digestRelation', () => {
  it('covers: same chain id (any case) at the proof block height or later', () => {
    const same = 'ab'.repeat(32)
    expect(digestRelation(proof, { chains: [chain(2, same, 100)] }).state).toBe('covers')
    expect(digestRelation(proof, { chains: [chain(2, same, 250)] }).state).toBe('covers')
  })
  it('does not cover: the chain is older than the proof block', () => {
    const r = digestRelation(proof, { chains: [chain(2, 'ab'.repeat(32), 99)] })
    expect(r).toMatchObject({ state: 'tooEarly', digestHeight: 99, proofHeight: 100 })
    expect(digestRelationText(r)).toMatch(/before the proof's block at height 100/)
  })
  it('does not cover: another chain id, or no chain for the shard group, or no id to compare', () => {
    expect(digestRelation(proof, { chains: [chain(2, 'cd'.repeat(32), 500)] }).state).toBe(
      'otherChain',
    )
    expect(digestRelation(proof, { chains: [chain(1, 'ab'.repeat(32), 500)] }).state).toBe(
      'noChain',
    )
    expect(digestRelation(proof, { chains: [chain(2, undefined, 500)] }).state).toBe('unknown')
  })
  it('never says verified', () => {
    for (const c of [
      chain(2, 'ab'.repeat(32), 100),
      chain(2, 'ab'.repeat(32), 1),
      chain(2, 'cd'.repeat(32), 100),
      chain(9, 'ab'.repeat(32), 100),
      chain(2, undefined, 100),
    ])
      expect(digestRelationText(digestRelation(proof, { chains: [c] }))).not.toMatch(/verified/i)
  })
})

/** More digests than one page, newest first, like the server's list. */
function manyDigests(n: number): void {
  const s = getState()
  const base = s.ledger.digests[0]!
  const extra = Array.from({ length: n }, (_, i) => ({
    ...structuredClone(base),
    id: base.id + n - i,
  }))
  s.ledger.digests = [...extra, ...s.ledger.digests]
}

async function openLedger() {
  const user = userEvent.setup()
  const app = await renderApp('/ledger?tab=digests')
  await screen.findByRole('heading', { name: 'Digests' })
  return { user, ...app }
}

function digestsSection(): HTMLElement {
  return screen.getByRole('heading', { name: 'Digests' }).closest('section') as HTMLElement
}

describe('digest paging', () => {
  it('lists ten, loads more on request, and reaches the oldest digest', async () => {
    manyDigests(24) // 25 in all
    const { user } = await openLedger()
    const section = digestsSection()
    await waitFor(() => expect(within(section).getAllByRole('rowheader')).toHaveLength(10))
    expect(
      within(section).queryByRole('button', { name: /download document of chain 1 of digest 3$/i }),
    ).toBeNull()
    const more = within(section).getByRole('button', { name: /load more digests/i })
    await user.click(more)
    await waitFor(() => expect(within(section).getAllByRole('rowheader')).toHaveLength(20))
    await user.click(within(section).getByRole('button', { name: /load more digests/i }))
    await waitFor(() => expect(within(section).getAllByRole('rowheader')).toHaveLength(25))
    // The oldest digest is there, with its download, and nothing more is offered.
    expect(
      within(section).getByRole('button', { name: /download document of chain 1 of digest 3$/i }),
    ).toBeInTheDocument()
    expect(within(section).queryByRole('button', { name: /load more digests/i })).toBeNull()
    expect(document.body).not.toHaveTextContent(/more digests exist than are shown/i)
  })

  it('a short list has no "load more"', async () => {
    await openLedger()
    await waitFor(() => expect(within(digestsSection()).getAllByRole('rowheader')).toHaveLength(1))
    expect(
      within(digestsSection()).queryByRole('button', { name: /load more digests/i }),
    ).toBeNull()
  })
})

async function askProof(user: User) {
  const field = screen.getByLabelText('Primary key (JSON)')
  await user.clear(field)
  await user.click(field)
  await user.paste('{"id": 7}')
  await user.click(screen.getByRole('button', { name: /look up proof/i }))
  return screen.findByTestId('proof')
}

describe('digests compared with the proof on screen', () => {
  it('shows nothing about a proof while there is none', async () => {
    await openLedger()
    await waitFor(() => expect(within(digestsSection()).getAllByRole('rowheader')).toHaveLength(1))
    expect(screen.queryByTestId('digest-proof-summary')).toBeNull()
    expect(within(digestsSection()).queryByText(/compared with the proof/i)).toBeNull()
  })

  it('says which digest has the proof chain, and which does not cover it, without claiming a check', async () => {
    manyDigests(3)
    const { user, queryClient } = await openLedger()
    const proofView = await askProof(user)
    const group = Number(/shard group\s*(\d+)/i.exec(proofView.textContent ?? '')?.[1])
    const section = digestsSection()
    await waitFor(() => expect(within(section).getAllByRole('rowheader')).toHaveLength(4))
    // Every digest of the mock is of the proof's chain and later than its block: all are the same chain.
    for (const d of getState().ledger.digests)
      expect(within(section).getByTestId(`digest-relation-${d.id}`)).toHaveTextContent(
        /same chain/i,
      )

    // Arrange the four digests: covers, too early, another chain, no chain for the shard group.
    const [a, b, c, d] = getState().ledger.digests
    const proofHeight = Number(/height\s*(\d+)/i.exec(proofView.textContent ?? '')?.[1])
    const ch = (x: typeof a) => x!.chains.find((k) => k.shardGroupId === group)!
    ch(b).blockHeight = proofHeight - 1
    ;(ch(c).document as Record<string, unknown>).chain_id = 'not-the-chain'
    d!.chains = d!.chains.filter((k) => k.shardGroupId !== group)
    expect(ch(a)).toBeTruthy()
    await queryClient.invalidateQueries({ queryKey: ['ledger'] })
    await waitFor(() =>
      expect(within(section).getByTestId(`digest-relation-${b!.id}`)).toHaveTextContent(
        /does not cover the proof/i,
      ),
    )
    expect(within(section).getByTestId(`digest-relation-${a!.id}`)).toHaveTextContent(/same chain/i)
    expect(within(section).getByTestId(`digest-relation-${b!.id}`)).toHaveTextContent(
      /before the proof's block/i,
    )
    expect(within(section).getByTestId(`digest-relation-${c!.id}`)).toHaveTextContent(
      /not the proof's chain identifier/i,
    )
    expect(within(section).getByTestId(`digest-relation-${d!.id}`)).toHaveTextContent(
      /has no chain for shard group/i,
    )
    // The summary counts, and says what this is and what it is not.
    const summary = screen.getByTestId('digest-proof-summary')
    expect(summary).toHaveTextContent(/1 of the 4 digests listed here have the same chain/i)
    expect(summary).toHaveTextContent(/compares identifiers only; it does not verify anything/i)
    expect(summary).toHaveTextContent(/kingres-verify/)
    expect(section.textContent ?? '').not.toMatch(/\bverified\b/i)
  })

  it('warns when no listed digest covers the proof, and tells what to do', async () => {
    const { user, queryClient } = await openLedger()
    const proofView = await askProof(user)
    const group = Number(/shard group\s*(\d+)/i.exec(proofView.textContent ?? '')?.[1])
    getState().ledger.digests[0]!.chains.find((k) => k.shardGroupId === group)!.blockHeight = 0
    await queryClient.invalidateQueries({ queryKey: ['ledger'] })
    const summary = await screen.findByTestId('digest-proof-summary')
    await waitFor(() =>
      expect(summary).toHaveTextContent(/none of the 1 digests listed here covers the proof/i),
    )
    expect(summary).toHaveClass('tone-warn')
  })

  it('tells the person, when a digest is saved, whether it covers the proof', async () => {
    URL.createObjectURL = () => 'blob:test/1'
    URL.revokeObjectURL = () => undefined
    const { user, queryClient } = await openLedger()
    const proofView = await askProof(user)
    const group = Number(/shard group\s*(\d+)/i.exec(proofView.textContent ?? '')?.[1])
    getState().ledger.digests[0]!.chains.find((k) => k.shardGroupId === group)!.blockHeight = 0
    await queryClient.invalidateQueries({ queryKey: ['ledger'] })
    const section = digestsSection()
    const button = await within(section).findByRole('button', {
      name: new RegExp(`download document of chain ${group} of digest`, 'i'),
    })
    await user.click(button)
    const note = await screen.findByTestId('digest-downloaded')
    expect(note).toHaveTextContent(/does not cover the proof/i)
    expect(note).toHaveAttribute('role', 'status')
  })
})
