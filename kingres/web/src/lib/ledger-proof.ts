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

/** The integers of a proof that the panel shows. They are read from the raw text (see `RawProof.numbers`). */
const SHOWN_INTEGERS = [
  'table_id',
  'ledger_txid',
  'seq',
  'shard_group_id',
  'entry_index',
  'entry_count',
  'tx_index',
  'block.height',
  'block.tx_count',
] as const
export type ShownInteger = (typeof SHOWN_INTEGERS)[number]

/** A proof as received: the bytes to save, the parsed view, and numbers read from the raw text. */
export interface RawProof {
  raw: string
  view: ProofView
  /** `table_id` exactly as written (an integer may be larger than a float holds). */
  tableId: string
  /** Every integer the panel shows, exactly as written in the text (a float would round above 2^53). */
  numbers: Partial<Record<ShownInteger, string>>
  /** Null when the document has the members the panel shows; otherwise what is missing. The file is still saved as sent. */
  shapeError: string | null
}

const isArrayOfStrings = (v: unknown): boolean =>
  Array.isArray(v) && v.every((x) => typeof x === 'string')

/** What is wrong with the shape of a parsed proof for the panel's display, or null. */
export function proofShapeError(view: unknown): string | null {
  if (typeof view !== 'object' || view === null || Array.isArray(view))
    return 'the answer is not a JSON object'
  const v = view as Record<string, unknown>
  for (const key of ['chain_id', 'row_hash', 'tx_root', 'user_name'])
    if (typeof v[key] !== 'string') return `"${key}" is missing or not text`
  for (const key of [
    'shard_group_id',
    'ledger_txid',
    'seq',
    'entry_index',
    'entry_count',
    'tx_index',
  ])
    if (typeof v[key] !== 'number') return `"${key}" is missing or not a number`
  if (v.op !== 'I' && v.op !== 'D') return '"op" is not I or D'
  if (!isArrayOfStrings(v.entry_path)) return '"entry_path" is missing or not a list of hashes'
  if (!isArrayOfStrings(v.tx_path)) return '"tx_path" is missing or not a list of hashes'
  if (!Array.isArray(v.chain)) return '"chain" is missing or not a list'
  const b = v.block as Record<string, unknown> | null | undefined
  if (typeof b !== 'object' || b === null) return '"block" is missing'
  if (typeof b.height !== 'number' || typeof b.tx_count !== 'number')
    return '"block" has no height or no transaction count'
  return null
}

export function parseProof(raw: string): RawProof {
  const view = JSON.parse(raw) as ProofView
  const wanted = new Set<string>(SHOWN_INTEGERS)
  const numbers: Partial<Record<ShownInteger, string>> = {}
  for (const slice of rawSlices(raw, (p) => wanted.has(p.join('.')))) {
    numbers[slice.path.join('.') as ShownInteger] = slice.raw
  }
  const shapeError = proofShapeError(view)
  return {
    raw,
    view,
    tableId: numbers.table_id ?? String((view as { table_id?: unknown }).table_id),
    numbers,
    shapeError,
  }
}

/** A digest answer that cannot be saved exactly: the panel refuses to save a re-serialized copy. */
export class DigestDocumentError extends Error {
  constructor() {
    super(
      'This digest cannot be saved exactly: the answer does not hold the document of every chain as text.',
    )
    this.name = 'DigestDocumentError'
  }
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
      // No re-serialized fallback: a copy that is not byte for byte what the server sent is worth nothing
      // to the verifier, and saving it silently is the failure this module exists to prevent.
      if (!hit) throw new DigestDocumentError()
      ;(chain as DigestChain).rawDocument = hit.raw
    })
  }
  if (parsed.chains) attach(parsed.chains, [])
  parsed.items?.forEach((d, n) => attach(d.chains, ['items', n]))
}

/**
 * How a digest relates to the proof that is on screen, from identifiers the contract provides:
 * the chain (`chain_id` in the proof and in the digest document; `shard_group_id` picks the chain of
 * the digest) and the height (the digest must be of the same chain at the proof's block height or
 * later). This compares identifiers. It does not verify anything: only `kingres-verify` checks a
 * proof against a digest.
 */
export type DigestRelation =
  | { state: 'covers'; digestHeight: number; proofHeight: number }
  | { state: 'tooEarly'; digestHeight: number; proofHeight: number }
  | { state: 'otherChain'; shardGroupId: number }
  | { state: 'noChain'; shardGroupId: number }
  | { state: 'unknown'; shardGroupId: number }

export function digestRelation(
  proof: Pick<ProofView, 'chain_id' | 'shard_group_id' | 'block'>,
  digest: Pick<RawDigest, 'chains'>,
): DigestRelation {
  const group = proof.shard_group_id
  const chain = digest.chains.find((c) => c.shardGroupId === group)
  if (!chain) return { state: 'noChain', shardGroupId: group }
  const id: unknown = chain.document.chain_id
  if (typeof id !== 'string') return { state: 'unknown', shardGroupId: group }
  if (id.toLowerCase() !== proof.chain_id.toLowerCase())
    return { state: 'otherChain', shardGroupId: group }
  const digestHeight = chain.blockHeight
  const proofHeight = proof.block.height
  return digestHeight < proofHeight
    ? { state: 'tooEarly', digestHeight, proofHeight }
    : { state: 'covers', digestHeight, proofHeight }
}

/** The sentence for a relation. It never says "verified": the panel only compares identifiers. */
export function digestRelationText(relation: DigestRelation): string {
  switch (relation.state) {
    case 'covers':
      return `Same chain as the proof (identifiers match). Chain head at height ${relation.digestHeight}, the proof's block is at height ${relation.proofHeight}.`
    case 'tooEarly':
      return `Does not cover the proof: the chain was only at height ${relation.digestHeight} when this digest was made, before the proof's block at height ${relation.proofHeight}. Use a later digest.`
    case 'otherChain':
      return `Does not cover the proof: the chain identifier of this digest for shard group ${relation.shardGroupId} is not the proof's chain identifier.`
    case 'noChain':
      return `Does not cover the proof: this digest has no chain for shard group ${relation.shardGroupId}, the proof's chain.`
    default:
      return `Cannot be compared with the proof: the document of this digest for shard group ${relation.shardGroupId} has no chain identifier.`
  }
}
