// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The words and the small rules of the Signing and Publication tabs of the Ledger page (contract 0.11.0, design of
// phase 7, sections 3.2 to 3.4, 4.2, 4.9, 9.2). Nothing here touches a key: the panel never asks for, shows, stores or
// logs a private key, a seed or a passphrase; it shows public keys, fingerprints and states the server reports.
import type { components } from '@/api/schema'
import type { StatusMeta } from './status'
import { tolerant } from './status'

type S = components['schemas']
export type SigningKey = S['LedgerSigningKey']
export type Signing = S['LedgerSigning']

/** How the state of a signing key looks. Never colour alone: an icon and a word. */
export const KEY_STATE: Record<SigningKey['state'], StatusMeta> = tolerant({
  next: {
    tone: 'info',
    icon: '◐',
    label: 'Next',
    hint: 'Generated and not signing yet.',
  },
  active: { tone: 'ok', icon: '✓', label: 'Active', hint: 'Signs the checkpoints.' },
  retired: {
    tone: 'neutral',
    icon: '○',
    label: 'Retired',
    hint: 'Signs nothing more. What it signed stays valid.',
  },
  revoked: {
    tone: 'danger',
    icon: '✖',
    label: 'Revoked',
    hint: 'Compromised. A signature of this key is rejected unless an anchor proves the statement older than the stated time.',
  },
})

/** What an event of the key history is, in words; an unknown type is shown as sent by the caller. */
const EVENT_TYPE: Record<string, string> = {
  'key-added': 'Key added',
  'key-retired': 'Key retired',
  'key-revoked': 'Key revoked',
  'epoch-closed': 'Epoch closed',
  'chain-retired': 'Chain retired',
  'group-removed': 'Shard group removed',
}

/** The words for an event type, or null for a type from a newer server (shown as sent). */
export function eventTypeLabel(type: string): string | null {
  return EVENT_TYPE[type] ?? null
}

/** The 64 hexadecimal digits of a fingerprint in groups of four, so that two can be read to each other by voice. */
export function fingerprintGroups(hex: string): string {
  return hex.replace(/(.{4})(?=.)/g, '$1 ')
}

/** True for a fingerprint as the contract writes it (64 lower-case hexadecimal digits). */
export const isFingerprint = (text: string): boolean => /^[0-9a-f]{64}$/.test(text)

/** The keys that sign now (state `active`). */
export const activeKeys = (keys: readonly SigningKey[]): SigningKey[] =>
  keys.filter((k) => k.state === 'active')

/**
 * Whether a key can be retired or revoked without leaving the ledger with no key in force. The server refuses with
 * 409 `last_signing_key` and the panel says it before: a revocation never creates a key, so a key that no other
 * active key replaces is not offered for ending (the button explains why).
 */
export function hasOtherActiveKey(keys: readonly SigningKey[], fingerprint: string): boolean {
  return keys.some((k) => k.state === 'active' && k.fingerprint !== fingerprint)
}

/** Why a key cannot be retired, or an empty text when it can. */
export function retireBlockedReason(keys: readonly SigningKey[], key: SigningKey): string {
  if (key.state !== 'active') return 'Only an active key can be retired.'
  if (!hasOtherActiveKey(keys, key.fingerprint))
    return 'This is the only key in force. Rotate first: a retirement never leaves the ledger without a signing key.'
  return ''
}

/** Why a key cannot be revoked, or an empty text when it can. */
export function revokeBlockedReason(keys: readonly SigningKey[], key: SigningKey): string {
  if (key.state === 'revoked') return 'This key is already revoked.'
  if (!hasOtherActiveKey(keys, key.fingerprint))
    return 'No other key is active. A revocation is signed by another active key and never creates one: rotate first, then revoke.'
  return ''
}

/** The text of the dialog that revokes a key: what a revocation means for old signatures (design 3.3). */
export const REVOKE_MEANING = [
  'A signature made by this key is rejected by the verifier, unless an anchor proves that the statement it signs is older than the time you give below.',
  'A checkpoint that has no such anchor is untrusted after that time, even if its text is old: whoever holds a stolen key can sign a back-dated checkpoint, and only an anchor tells the difference.',
  'An earlier time is the safe direction. The server never accepts a time later than now.',
  'The private key file is deleted from every server that holds it.',
  'An auditor whose only pinned key is this one cannot check the revocation, because it is signed by another key. That auditor needs a new trust file, obtained out of band.',
] as const

/** The text of the dialog that retires a key (design 3.3). */
export const RETIRE_MEANING = [
  'The key stops signing and the key file is deleted from every server that holds it. Archive an offline copy first if you want one.',
  'What the key signed stays valid for ever.',
] as const

/** What an operator must keep outside the cluster for verification to mean anything (design 2.3). */
export const KEEP_OUTSIDE = [
  'The trust file (trust.json): the public signing keys and the origin prefix. Download it once and keep a copy for the auditor and one in a safe place that is not this cluster.',
  'The fingerprint of the key, read to the auditor by voice or on paper, so that the trust file is not the only source of trust.',
  'At least one anchor in another administrative domain (a WORM bucket under other administrators, time-stamp tokens or OpenTimestamps proofs).',
  'A monitor, a place that periodically fetches the publication and keeps the last checkpoint it saw (kingres-verify monitor), so that a rollback of the cluster is seen even if every anchor target is silent.',
  'A backup of the signing key files and of the passphrase file, kept separately and off the cluster, where its database administrators cannot reach it.',
  'The verifier, obtained from a source other than this cluster.',
] as const

/** The reminder of design 9.2 shown while a backup of the key files is advised. */
export const BACKUP_TEXT =
  'Back up the key directory and the passphrase file separately, off the cluster, now that a key was created, rotated or revoked. Ticking that you made a backup (in the dialog of the next key operation) only clears this reminder; it is not a proof.'

/** Words for the codes a key task can end with (the codes are not an enum of the contract: others are shown as sent). */
const KEY_TASK_ERROR: Record<string, string> = {
  key_files_remain:
    'A server that holds the key could not remove its file. Nothing was lost, but the key is still on that server: repeat the action, the step can be retried.',
  passphrase_mismatch:
    'A server that holds a key file cannot open it with its own passphrase. Every holder must use the passphrase of the file it was given; the task names the server.',
  key_set_damaged:
    'The stored key set is damaged. No checkpoint can be signed until it is repaired.',
  key_file_missing:
    'The key file of an active or next key is missing on a server. Restore the key directory from the backup.',
  signer_unavailable: 'The signer did not answer, so nothing could be signed.',
  cannot_sign: 'The new key could not sign the proof that it is held. Nothing was activated.',
  cannot_activate: 'The key could not be activated. The task can be repeated.',
  cannot_retire: 'The key could not be retired. The task can be repeated.',
  cannot_revoke: 'The key could not be revoked. The task can be repeated.',
  last_signing_key:
    'No other key is active and in force. Create a key first (a rotation), then end this one.',
  term_over: 'Another manager leader took over while the task ran. Repeat the action.',
  not_leader: 'This server is not the manager leader. Repeat the action.',
  clock: 'The clocks of the servers differ too much for this step. Fix the time first.',
  trust_file: 'The trust file could not be built from the key set.',
  key_id_collision: 'The key has the same short id as another key of the set. Create it again.',
}

/** The sentence for a task error code of a key task, or null for a code the panel has no words for. */
export function keyTaskErrorHint(code: string | undefined | null): string | null {
  return code ? (KEY_TASK_ERROR[code] ?? null) : null
}

/** The origin prefix that the first key will lock, as the console asks for it: the default is only a proposal. */
export const ORIGIN_PREFIX_HELP =
  'Every signature of the cluster is made under this name (no scheme, no trailing slash, at most 160 characters). It is locked when the first key is created and cannot be changed afterwards; another prefix needs a new cluster generation. The value shown is the default of this cluster: confirm it or change it before you create the first key.'

/** What the publication reveals (design 4.9). */
export const PUBLICATION_EXPOSES = [
  'That a chain exists (its chain identifier) and how many blocks it has.',
  'The time of every block and the number of transactions in each block, so when the database is busy and roughly how busy.',
  'The root of the transactions of each block and the hash of the block before it.',
  'From the number of chains of one aggregate, the number of shard groups of a database.',
] as const
export const PUBLICATION_HIDES =
  'It does not reveal table names, user names, row contents, row hashes, the database name (hidden in the identifier of the aggregate) or the shard group ids.'
export const PUBLICATION_GRANULARITY =
  'The checkpoint interval is the granularity of the timing information beyond the block times. An operator who does not want the activity pattern published keeps the publication off and exports bundles on demand.'

/** The verifier text of design 7.7 for an auditor. */
export const VERIFIER_TEXT =
  'An auditor checks the publication with kingres-verify, obtained from a source other than this cluster, against the trust file and the fingerprint they were given out of band.'

/** How the publication state of a database or of a log looks. */
const PUBLICATION_STATE: Record<string, StatusMeta> = tolerant({
  off: { tone: 'neutral', icon: '○', label: 'Not published' },
  ok: { tone: 'ok', icon: '✓', label: 'Published' },
  behind: {
    tone: 'warn',
    icon: '▲',
    label: 'Behind',
    hint: 'The newest signed checkpoint is older than five publication intervals while the chain has advanced.',
  },
  diverged: {
    tone: 'danger',
    icon: '✖',
    label: 'Diverged',
    hint: 'The chain no longer extends its published log. Publication of this log is stopped.',
  },
  active: { tone: 'ok', icon: '✓', label: 'Active' },
  closed: { tone: 'neutral', icon: '○', label: 'Closed epoch' },
})

/** How the anchoring state of a database looks (`none` is a state, not an absence). */
const ANCHORING_STATE: Record<string, StatusMeta> = tolerant({
  none: {
    tone: 'warn',
    icon: '▲',
    label: 'Not anchored',
    hint: 'No anchor target is enabled.',
  },
  ok: { tone: 'ok', icon: '✓', label: 'Anchored' },
  behind: { tone: 'warn', icon: '▲', label: 'Anchoring behind' },
  failed: { tone: 'danger', icon: '✖', label: 'Anchoring failed' },
})

/** The badge of a publication state (`off`, `ok`, `behind`, `diverged`, a log's `active` or `closed`); a state of a newer server is shown as sent. */
export const publicationMeta = (state: string): StatusMeta => PUBLICATION_STATE[state] as StatusMeta
/** The badge of an anchoring state (`none`, `ok`, `behind`, `failed`). */
export const anchoringMeta = (state: string): StatusMeta => ANCHORING_STATE[state] as StatusMeta

/** The warning of design 6.9 shown while no anchor target is enabled. */
export const NOT_ANCHORED_TEXT =
  'Not anchored: this ledger can be rewritten undetected by anyone who controls this cluster and its backups. Add a target.'

/** Words for a flag of a member of an aggregate; a flag from a newer server is shown as sent by the caller. */
const MEMBER_FLAG: Record<string, string> = {
  stale:
    'Stale: this group could not be read in the last round, the member repeats its last good head.',
  diverged: 'Diverged: the chain log is stopped, the member repeats its last good head.',
  retired: 'Retired: the last aggregate in which this chain appears.',
}
export function memberFlagText(flag: string): string | null {
  return MEMBER_FLAG[flag] ?? null
}

/** True for text the contract allows in the reason of a revocation: at most 200 printable ASCII characters. */
export function validRevokeReason(reason: string): boolean {
  return reason.length >= 1 && reason.length <= 200 && /^[\x20-\x7e]*$/.test(reason)
}

/** The value of an `<input type="datetime-local">` (local time, to the minute) for an instant. */
export function toLocalInput(date: Date): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${p(date.getMonth() + 1)}-${p(date.getDate())}T${p(date.getHours())}:${p(date.getMinutes())}`
}

/** The instant a local date-time input means, as ISO 8601 in UTC, or null for an input that is not a date. */
export function fromLocalInput(text: string): string | null {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(text)) return null
  const t = new Date(text)
  return Number.isNaN(t.getTime()) ? null : t.toISOString()
}

/** The state of the dashboard card "Ledger integrity". */
export type CardState = 'off' | 'attention' | 'notAnchored' | 'healthy'

/** The kinds of alert that belong to the ledger of phase 7 (and the two that existed before). */
export const LEDGER_ALERT_KINDS: readonly string[] = [
  'ledgerVerificationFailed',
  'ledgerPendingTooOld',
  'ledgerNotAnchored',
  'ledgerAnchorBehind',
  'ledgerAnchorFailed',
  'ledgerAnchorConflict',
  'ledgerChainDiverged',
  'ledgerGroupUnread',
  'ledgerPublicationBehind',
  'ledgerSignerUnavailable',
  'ledgerDigestOverdue',
]
