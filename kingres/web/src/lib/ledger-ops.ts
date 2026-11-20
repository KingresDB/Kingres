// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The words and checks of Console D of the Ledger page (contract 0.11.0 and 0.12.0, design `ledger-phase7.md` 7.3, 8.5 and 9.4): the
// bundle for offline verification, the acknowledgement of a diverged chain (what it means for the history), the checks of a
// verification and the audit of the publication, and the refusals of each action in the words of its problem code.
import { ApiError } from '@/api/problem'
import type { Refusal } from './ledger-client'

export const BUNDLE_TEXT =
  'A bundle is one archive with the published tiles and checkpoints of the logs you choose, the evidence of the anchors, and (when you ask) inclusion proofs. Give it to an auditor: with kingres-verify they can check the ledger without any access to this cluster. It is kept for 24 hours; download it before then.'

export const PROOFS_WARNING =
  'A proof in a bundle shows the hash of the row it proves (and where it sits in the chain) to everyone who holds the bundle. The row itself is not in it, but a hash can be tested against guessed values. Add a proof only for a bundle that goes to a party you trust with that.'

export const ACK_TEXT: readonly string[] = [
  'The chain of this shard group no longer extends the log that was published for it. That happens after a restore from a backup, or after a failover that lost commits, and it can also be what a rewrite of history looks like.',
  'Acknowledging does not repair anything and does not hide anything. The old log is closed as it is: its published checkpoints stay, and auditors who hold them can see where it ended and why. A new log (a new epoch) starts from the chain as it is now, and every later checkpoint belongs to it.',
  'The record of this acknowledgement (what you say happened, your reason, who and when) is part of the history, and an auditor reads it next to the point where the old log ends.',
  'An acknowledgement makes the alert go away. It does not make what happened any less true: if you do not know why the chain diverged, find out before you acknowledge. It cannot be undone.',
]

export const ACK_KIND: Record<string, string> = {
  restore: 'The database was restored from a backup',
  failoverLoss: 'A failover lost commits that had been published',
  other: 'Something else (say what in the reason)',
}

export const CHECKS: { id: 'chain' | 'publication' | 'anchors'; label: string; hint: string }[] = [
  {
    id: 'chain',
    label: 'The chain',
    hint: 'Recompute the blocks, the Merkle roots and the row hashes and compare them with a digest.',
  },
  {
    id: 'publication',
    label: 'The publication',
    hint: 'Read every chain again from the first block, compare it with the published leaves, and verify every stored checkpoint signature and the consistency between them. It reads the whole chain: it can take long.',
  },
  {
    id: 'anchors',
    label: 'The anchors',
    hint: 'Check the evidence of the anchor records against the checkpoints they cover.',
  },
]
export type CheckId = (typeof CHECKS)[number]['id']

export const AUDIT_TEXT =
  'The audit of the publication is a verification that re-reads every chain of the database from block 0 and compares it with what was published. It finds a rewrite of the history, and a log that the publisher got wrong. The server also runs it every day by itself.'

/** The request of a verification: `checks` is sent only when it is not the default (the chain alone). */
export function verifyBody(
  scope: 'full' | 'sinceLastVerified',
  checks: readonly CheckId[],
): { scope: 'full' | 'sinceLastVerified'; checks?: CheckId[] } {
  const only = checks.length === 1 && checks[0] === 'chain'
  return { scope, ...(checks.length === 0 || only ? {} : { checks: [...checks] }) }
}

/** The number of a bundle in the detail of its task ("bundle 3"): the contract has no member for the result of a task. */
export function bundleIdOf(detail: string | null | undefined): number | null {
  const m = /\bbundle (\d{1,15})\b/.exec(detail ?? '')
  return m ? Number(m[1]) : null
}

export type OpsAction = 'bundle' | 'download' | 'acknowledge' | 'verify'

/** The refusals of Console D in words; null for a code without words of its own (the generic notice follows). */
export function opsRefusal(error: unknown, action: OpsAction): Refusal | null {
  if (!(error instanceof ApiError)) return null
  const c = error.code
  if (c === 'unsupported_operation')
    return {
      tone: 'info',
      heading: 'Not available on this server version yet',
      message: 'This server version does not build this operation yet. Nothing was changed.',
      withDetail: true,
    }
  if (action === 'download' && error.status === 410)
    return {
      tone: 'warning',
      heading: 'The bundle has expired',
      message: 'A bundle is kept for 24 hours. Create it again.',
      withDetail: false,
    }
  if (c === 'not_found')
    return {
      tone: 'warning',
      heading: 'Not found',
      message:
        action === 'download'
          ? 'There is no such bundle.'
          : action === 'acknowledge'
            ? 'The database does not exist (any more).'
            : 'The database does not exist (any more).',
      withDetail: false,
    }
  if (action === 'bundle' && c === 'ledger_not_published')
    return {
      tone: 'info',
      heading: 'The ledger is not published yet',
      message:
        'A bundle packs what was published. Turn publication on under Publication and wait for the first checkpoint, then try again.',
      withDetail: true,
    }
  if (action === 'download' && error.status === 410)
    return {
      tone: 'warning',
      heading: 'The bundle has expired',
      message: 'A bundle is kept for 24 hours. Create it again.',
      withDetail: false,
    }
  if (action === 'acknowledge' && c === 'chain_not_diverged')
    return {
      tone: 'info',
      heading: 'This chain has not diverged',
      message:
        'Nothing was changed. The chain extends its published log (or it was acknowledged by someone else a moment ago).',
      withDetail: false,
    }
  if (action === 'acknowledge' && c === 'validation_failed')
    return {
      tone: 'warning',
      heading: 'Check the highlighted fields',
      message:
        'The kind, the reason (1 to 200 characters) and the name of the database must be right. Nothing was changed.',
      withDetail: true,
    }
  if (
    (action === 'acknowledge' || action === 'verify') &&
    (c === 'operation_in_progress' || c === 'conflict')
  )
    return {
      tone: 'warning',
      heading:
        action === 'verify'
          ? 'A verification is already running'
          : 'Another change of this ledger is running',
      message: 'Wait until it has finished (the Tasks page shows it), then try again.',
      withDetail: true,
    }
  return null
}
