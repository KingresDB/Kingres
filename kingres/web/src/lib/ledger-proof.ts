// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// The ledger documents the panel hands to the standalone verifier `kingres-verify`: the inclusion
// proof and the digest of a chain. Both are kept as the exact text the server sent (the panel
// never re-serializes them); what is shown on screen is read from a parsed copy.
import type { components } from '@/api/schema'
import { rawSlices } from './raw-json'

type S = components['schemas']

/** The members of a proof document the panel shows (the document has more; see the contract). */
export interface ProofView {
  chain_id: string
  relation?: string
  shard_group_id: number
  table_id: number
  ledger_txid: number
  seq: number
  op: 'I' | 'D'
  row_hash: string
  entry_index: number
  entry_count: number
  entry_path: string[]
  tx_root: string
  user_name: string
  tx_index: number
  tx_path: string[]
  block: { height: number; tx_count: number; block_hash?: string; tx_merkle_root: string }
  chain: unknown[]
}

/** A proof as received: the bytes to save, the parsed view, and numbers read from the raw text. */
export interface RawProof {
  raw: string
  view: ProofView
  /** `table_id` exactly as written (an integer may be larger than a float holds). */
  tableId: string
}

export function parseProof(raw: string): RawProof {
  const view = JSON.parse(raw) as ProofView
  const tableId = rawSlices(raw, (p) => p.length === 1 && p[0] === 'table_id')[0]?.raw
  return { raw, view, tableId: tableId ?? String(view.table_id) }
}

export type DigestChain = S['LedgerDigest']['chains'][number] & {
  /** The `document` member exactly as the server sent it (what `kingres-verify --digest` reads). */
  rawDocument: string
}
export type RawDigest = Omit<S['LedgerDigest'], 'chains'> & { chains: DigestChain[] }

/** Adds `rawDocument` to every chain of the digests in a response text. `base` is the path of the digest list. */
export function withRawDocuments(
  parsed: { chains?: S['LedgerDigest']['chains']; items?: S['LedgerDigest'][] },
  text: string,
): void {
  const slices = rawSlices(
    text,
    (p) => p.at(-1) === 'document' && p.at(-3) === 'chains' && typeof p.at(-2) === 'number',
  )
  const attach = (chains: S['LedgerDigest']['chains'], prefix: (string | number)[]): void => {
    chains.forEach((chain, i) => {
      const hit = slices.find(
        (s) =>
          s.path.length === prefix.length + 3 &&
          s.path[prefix.length + 1] === i &&
          prefix.every((v, k) => s.path[k] === v),
      )
      ;(chain as DigestChain).rawDocument = hit?.raw ?? JSON.stringify(chain.document)
    })
  }
  if (parsed.chains) attach(parsed.chains, [])
  parsed.items?.forEach((d, n) => attach(d.chains, ['items', n]))
}
