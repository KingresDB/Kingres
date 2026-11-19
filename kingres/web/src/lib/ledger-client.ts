// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The words and the checks of the Client keys tab of the Ledger page (Console C, contract 0.11.0, design `ledger-phase7.md` 3.6 to
// 3.8 and 8.5, and the section "Client signatures" of `contrib/kingres_ledger/README.md`): what a client key is for and what an
// attestation proves, the keys in words, the consequences of the signature policy "required" (said before the click), the upgrade of
// the extension, and the refusals of each action in the words of its problem code. The console never asks for, shows or stores a
// private key: a client key here is a public key.
import { ApiError } from '@/api/problem'
import type { LedgerClientKey } from '@/api/types'
import { formatDateTime } from './format'
import { safeText } from './safe-text'

export const CLIENT_KEYS_TEXT =
  'A client key lets an application sign the rows it writes to a ledger table. The signature (an attestation) proves that the holder of the key wrote, or removed, exactly that row version in that chain. The server never signs for a client and never sees a private key: make the key pair where the application runs and register only the public key here.'

export const ATTESTATION_LIMITS_TEXT =
  'An attestation does not prove that the application saw more than the hash of the row: the application must recompute the row hash from its own data before it signs (kingres-verify row-hash does it), and must not sign a hash that the server returned without doing so. It does not prove who holds the key either: the label and the role are what an administrator chose.'

export const REGISTER_TEXT =
  'Make the key pair where the application runs: the server never sees a private key. Register only the public key of the application: 32 bytes of an Ed25519 key as standard base64 (44 characters). A key can be bound to one role, so that only that role may attest with it; without a role any role may. A key that is not a proper Ed25519 key (a point of small order) is refused. A key is registered once, and a key that was revoked is never registered again.'

export const REVOKE_TEXT =
  'A revoked key signs no further entry. The entries it signed before the revocation stay valid: an attestation counts when its block is before the block of the revocation. A revoked key cannot be registered again, and an application that still signs with it will have its writes refused when the table requires signatures.'

export const POLICY_BEFORE_TEXT: readonly string[] = [
  'From the moment the policy is switched, a transaction that writes this table commits only when every entry it wrote carries an attestation of a registered, active key. A transaction that leaves one out fails at COMMIT with the error KL010 and writes nothing.',
  'It waits for the open writers of the table first. The switch takes a lock that conflicts with every transaction that has written the table: it waits until each of them has committed or rolled back (and a prepared transaction until it is committed or rolled back), and a transaction that begins to write the table later waits for the switch. The wait is bounded by the limit of the lock hold of the extension; if it is not over in time the switch fails and changes nothing.',
  'Entries that were committed before the switch are not asked for an attestation. What the switch waited for stays as it was.',
  'It cannot be switched back from here or anywhere in the console: a table with the policy "required" stays so. A server without OpenSSL cannot attest, so no write to the table could commit there.',
  'It is meant for tables that are written by statements the application controls: a trigger that changes a row again makes entries the application did not foresee, and those need an attestation too.',
]

export const UPGRADE_TEXT =
  'Client keys and signature policies need version 0.3 of the ledger extension in every shard group of the database. The upgrade runs ALTER EXTENSION kingres_ledger UPDATE in each group, one task for the whole database. A group that cannot be upgraded fails the task, and the error names its server; the groups before it stay upgraded and the rest are untouched. Nothing of the existing chain changes: the hash format is the same.'

export const POLICY_NOT_REPORTED =
  'This server version does not report the policy of a table. It shows "Required" here once you have switched it, or once the server says it is already.'

export type KeyState = 'active' | 'revoked'
export const keyState = (k: Pick<LedgerClientKey, 'revokedAt'>): KeyState =>
  k.revokedAt === null ? 'active' : 'revoked'

/** The role a key is bound to, in words. */
export const keyRole = (k: Pick<LedgerClientKey, 'roleName'>): string =>
  k.roleName === null ? 'Any role' : safeText(k.roleName, 63)

/** How a revocation reads. */
export function revocationText(k: Pick<LedgerClientKey, 'revokedAt' | 'revocationReason'>): string {
  if (k.revokedAt === null) return ''
  const why = k.revocationReason ? `: ${safeText(k.revocationReason, 200)}` : ''
  return `Revoked ${formatDateTime(k.revokedAt)}${why}`
}

/** A public key as the console shows it in a list: the start and the end, the whole key is copied on request. */
export function shortKey(publicKey: string): string {
  return publicKey.length <= 20 ? publicKey : `${publicKey.slice(0, 10)}...${publicKey.slice(-8)}`
}

export const KEY32 = /^[A-Za-z0-9+/]{43}=$/

export interface RegisterForm {
  publicKey: string
  roleName: string
  label: string
}

/** The problems of a registration form, by field (the server's checks of the contract: 32 bytes, a label of 1 to 100). */
export function validateRegister(f: RegisterForm): Record<string, string> {
  const e: Record<string, string> = {}
  const key = f.publicKey.trim()
  if (!KEY32.test(key) || !decodes32(key))
    e.publicKey = 'Give the 32-byte public key as standard base64: 44 characters ending with "=".'
  const label = f.label.trim()
  if (label.length < 1 || label.length > 100) e.label = 'Give a label of 1 to 100 characters.'
  if (f.roleName.trim().length > 63) e.roleName = 'A role name has at most 63 characters.'
  return e
}
function decodes32(text: string): boolean {
  try {
    return atob(text).length === 32
  } catch {
    return false
  }
}

/** The body of the request: a role is sent only when one was given. */
export function registerBody(f: RegisterForm): {
  publicKey: string
  label: string
  roleName?: string
} {
  const role = f.roleName.trim()
  return {
    publicKey: f.publicKey.trim(),
    label: f.label.trim(),
    ...(role === '' ? {} : { roleName: role }),
  }
}

export type ClientAction = 'register' | 'revoke' | 'policy' | 'upgrade' | 'list'

export interface Refusal {
  tone: 'info' | 'warning' | 'danger'
  heading: string
  message: string
  /** The server's own detail may add something (shown below, never instead). */
  withDetail: boolean
  /** The state it says is already as the person wanted (a policy that is required already). */
  alreadyDone?: boolean
}

const OLDER =
  'The ledger extension of this database is older than version 0.3, or this server cannot make attestations (it has no OpenSSL), or this server version does not build the operation yet.'

/**
 * What a refusal means for the person at the action. Null for a code that has no words of its own here: the caller then shows the
 * generic words of `describeProblem`.
 */
export function clientRefusal(error: unknown, action: ClientAction): Refusal | null {
  if (!(error instanceof ApiError)) return null
  const code = error.code
  if (code === 'unsupported_operation')
    return {
      tone: 'info',
      heading: 'Not available on this server version yet',
      message: `${OLDER} ${
        action === 'upgrade' ? '' : 'Upgrade the extension of the database to version 0.3 first.'
      } Nothing was changed.`.replace('  ', ' '),
      withDetail: true,
    }
  if (code === 'not_found')
    return {
      tone: 'warning',
      heading: 'Not found',
      message:
        action === 'revoke'
          ? 'The key does not exist (any more) in this database.'
          : action === 'policy'
            ? 'The table does not exist (any more).'
            : 'The database does not exist (any more).',
      withDetail: false,
    }
  switch (action) {
    case 'register':
      if (code === 'already_exists')
        return {
          tone: 'warning',
          heading: 'This key is registered already',
          message:
            'A key is registered once in a database, and a key that was revoked is never registered again. Make a new key pair and register its public key.',
          withDetail: true,
        }
      break
    case 'revoke':
      if (code === 'conflict')
        return {
          tone: 'info',
          heading: 'The key is revoked already',
          message: 'Nothing was changed. A revoked key stays revoked.',
          withDetail: false,
          alreadyDone: true,
        }
      break
    case 'policy':
      if (code === 'conflict')
        return {
          tone: 'info',
          heading: 'The policy is "required" already',
          message: 'Nothing was changed. The policy of a table cannot go back.',
          withDetail: false,
          alreadyDone: true,
        }
      if (code === 'unprocessable')
        return {
          tone: 'warning',
          heading: 'This table is not a ledger table',
          message:
            'Only a ledger table has a signature policy. Turn on the ledger for the table first.',
          withDetail: true,
        }
      break
    case 'upgrade':
      if (code === 'operation_in_progress')
        return {
          tone: 'warning',
          heading: 'An upgrade is already running',
          message: 'Wait until it has finished. Follow it on the Tasks page.',
          withDetail: true,
        }
      break
    default:
      break
  }
  return null
}
