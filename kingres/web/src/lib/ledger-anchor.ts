// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The words and the forms of the Anchoring tab of the Ledger page (contract 0.11.0, design `ledger-phase7.md` 6.2 to 6.9 and
// 8.5): the three kinds of target and what each needs, the state of a target and of an anchor record in plain words, the
// result of a test, and the checks of a form before it is sent. The configuration of a target is free-form in the contract
// (`config` is an object whose shape depends on `kind`), so the members named here are those of the design; a member that
// the console does not know is kept as it was when a target is edited. Credentials are write-only: nothing here reads one
// back, and a form never holds one that was not typed in this session.
import type {
  LedgerAnchor,
  LedgerAnchorKind,
  LedgerAnchorState,
  LedgerAnchorTarget,
  LedgerAnchorTest,
} from '@/api/types'
import { formatDateTime } from './format'
import { safeText } from './safe-text'
import type { StatusMeta } from './status'

export type AnchorKind = LedgerAnchorKind
export const ANCHOR_KINDS: readonly AnchorKind[] = ['worm', 'tsa', 'ots']

/** The name of a kind in full and in short, and what it is for, in a sentence. */
export const KIND_LABEL: Record<AnchorKind, string> = {
  worm: 'WORM object store (S3 Object Lock)',
  tsa: 'Time-stamp authority (RFC 3161)',
  ots: 'OpenTimestamps',
}
export const KIND_SHORT: Record<AnchorKind, string> = {
  worm: 'WORM store',
  tsa: 'Time-stamp authority',
  ots: 'OpenTimestamps',
}
export const KIND_HELP: Record<AnchorKind, string> = {
  worm: 'Keeps the signed checkpoint in a bucket that cannot be changed or deleted until its retention ends. Works on a private network and needs no third party; MinIO and other stores that support Object Lock also work, and the test tells you.',
  tsa: 'Asks a time-stamp authority to sign the time of the checkpoint. The answer is checked against the authority certificates you give, and gives a time that the authority vouches for.',
  ots: 'Sends the checkpoint to public calendars that commit it to the Bitcoin chain. It is free and needs no account, but confirmation takes hours.',
}
/** The kind label of a target; a kind from a newer server is shown as sent. */
export function kindLabel(kind: string): string {
  return (KIND_LABEL as Record<string, string | undefined>)[kind] ?? safeText(kind, 40)
}
export function kindShort(kind: string): string {
  return (KIND_SHORT as Record<string, string | undefined>)[kind] ?? safeText(kind, 40)
}

export const ONE_KIND_TEXT =
  'Only one kind of anchor target is enabled. A single anchor target is one point of failure and one administrator. Add a target of another kind: a WORM store first, then a time-stamp authority or OpenTimestamps.'

export const NO_TARGET_ENABLED_TEXT =
  'Not anchored: this ledger can be rewritten undetected by anyone who controls this cluster and its backups. Add a target.'

export const COMPLIANCE_TEXT =
  'Compliance mode cannot be undone: nobody, not even the owner of the account, can delete the anchored object or shorten its retention before the retention ends.'
export const GOVERNANCE_TEXT =
  'Governance mode is weaker: a person with the bypass permission of the store can delete the anchored object or shorten its retention. Only compliance mode cannot be bypassed.'
export const WORM_PROBE_TEXT =
  'The test writes one empty probe object under the key prefix, in a folder named _probe. It is zero bytes, and it stays until its own lock ends: about ten minutes, and where the bucket has a default retention in compliance mode, until that ends.'
export const OTS_WAIT_TEXT =
  'OpenTimestamps takes hours. A record stays "Sent, not final" until a Bitcoin block confirms it, and it counts as anchored only then.'
export const CREDENTIALS_TEXT =
  'Credentials are stored in a sealed file on the servers and are never shown again, here or anywhere else. To change them, type the new ones; leave the fields empty to keep what is stored.'
export const EVIDENCE_TEXT =
  'The evidence file is what an auditor needs to check the anchor with the verifier. The WORM evidence says what the cluster recorded; the verifier reads the object itself.'

// --- states ------------------------------------------------------------------------------------

const OK = '✓'
const WARN = '▲'
const BAD = '✖'
const RING = '○'
const HALF = '◐'

/** The state of a target as the alerts see it (`none`, `ok`, `behind`, `failed`); another value is shown as sent. */
const TARGET_STATE: Record<string, StatusMeta> = {
  none: {
    tone: 'neutral',
    icon: RING,
    label: 'Nothing anchored yet',
    hint: 'This target has not anchored a checkpoint yet.',
  },
  ok: { tone: 'ok', icon: OK, label: 'Anchoring' },
  behind: {
    tone: 'warn',
    icon: WARN,
    label: 'Behind',
    hint: 'The newest anchor of this target is older than the anchoring interval allows.',
  },
  failed: {
    tone: 'danger',
    icon: BAD,
    label: 'Failed',
    hint: 'A record of this target failed or expired and needs a person.',
  },
}
export function targetStateMeta(state: string): StatusMeta {
  return TARGET_STATE[state] ?? { tone: 'neutral', icon: RING, label: safeText(state, 40) }
}

const RECORD_STATE: Record<LedgerAnchorState, StatusMeta> = {
  pending: { tone: 'info', icon: HALF, label: 'Waiting to be sent' },
  submitted: { tone: 'info', icon: HALF, label: 'Sent, not final' },
  anchored: { tone: 'ok', icon: OK, label: 'Anchored' },
  failed: { tone: 'danger', icon: BAD, label: 'Failed', hint: 'A person has to look at it.' },
  superseded: { tone: 'neutral', icon: RING, label: 'Replaced by a newer checkpoint' },
  expired: {
    tone: 'danger',
    icon: BAD,
    label: 'Never confirmed',
    hint: 'It was not confirmed in the time allowed.',
  },
}
export function recordStateMeta(state: string): StatusMeta {
  return (
    (RECORD_STATE as Record<string, StatusMeta | undefined>)[state] ?? {
      tone: 'neutral',
      icon: RING,
      label: safeText(state, 40),
    }
  )
}
/** A record in one of these states can be put back to waiting by an operator (design 6.6 and the contract of retry). */
export const canRetry = (r: Pick<LedgerAnchor, 'state'>): boolean =>
  r.state === 'failed' || r.state === 'expired'
/** Only an anchored record has evidence. */
export const hasEvidence = (r: Pick<LedgerAnchor, 'state'>): boolean => r.state === 'anchored'

export type EvidenceFormat = 'json' | 'ots' | 'tsr'
/** The formats of the evidence of a kind: JSON for every kind, `.ots` for OpenTimestamps, `.tsr` for a time-stamp authority. */
export function evidenceFormats(kind: string): EvidenceFormat[] {
  return kind === 'ots' ? ['json', 'ots'] : kind === 'tsa' ? ['json', 'tsr'] : ['json']
}
export const FORMAT_LABEL: Record<EvidenceFormat, string> = {
  json: 'JSON',
  ots: '.ots file',
  tsr: '.tsr file',
}

// --- a test --------------------------------------------------------------------------------------

const CHECK_STATUS: Record<'ok' | 'warning' | 'failed', StatusMeta> = {
  ok: { tone: 'ok', icon: OK, label: 'OK' },
  warning: { tone: 'warn', icon: WARN, label: 'Warning' },
  failed: { tone: 'danger', icon: BAD, label: 'Failed' },
}
export function checkMeta(status: string): StatusMeta {
  return (
    (CHECK_STATUS as Record<string, StatusMeta | undefined>)[status] ?? {
      tone: 'neutral',
      icon: RING,
      label: safeText(status, 40),
    }
  )
}
/** One word for a whole test. */
export function testSummary(t: LedgerAnchorTest): StatusMeta {
  if (!t.ok) return { tone: 'danger', icon: BAD, label: 'Test failed' }
  if (t.checks.some((c) => c.status === 'warning'))
    return { tone: 'warn', icon: WARN, label: 'Passed with warnings' }
  return { tone: 'ok', icon: OK, label: 'Test passed' }
}

// --- how far behind the anchoring is ------------------------------------------------------------

/**
 * The newest anchor of a target as a length of time: it is the live length of the window in which a rewrite of the ledger
 * could still go unnoticed (design 6.1).
 */
export function windowText(newestAnchoredAt: string | null | undefined, now = Date.now()): string {
  if (!newestAnchoredAt) return 'Nothing anchored yet'
  const t = Date.parse(newestAnchoredAt)
  if (Number.isNaN(t)) return safeText(newestAnchoredAt, 40)
  const seconds = Math.max(0, Math.round((now - t) / 1000))
  const n = (v: number, unit: string): string => `${v} ${unit}${v === 1 ? '' : 's'}`
  const age =
    seconds < 90
      ? n(seconds, 'second')
      : seconds < 5400
        ? n(Math.round(seconds / 60), 'minute')
        : seconds < 172800
          ? n(Math.round(seconds / 3600), 'hour')
          : n(Math.round(seconds / 86400), 'day')
  return `${age} old (${formatDateTime(newestAnchoredAt)})`
}

/** The date on which a lock that starts now and lasts `days` days ends, in UTC. */
export function retentionEnds(days: number, now = Date.now()): string {
  return formatDateTime(new Date(now + days * 86_400_000).toISOString())
}

// --- the forms ---------------------------------------------------------------------------------

export type FieldValue = string | boolean
export type FormValues = Record<string, FieldValue>

export interface FieldSpec {
  key: string
  label: string
  hint?: string
  type: 'text' | 'number' | 'select' | 'checkbox' | 'lines'
  options?: { value: string; label: string }[]
  required?: boolean
  /** Shown under "More settings" and not in the main form. */
  advanced?: boolean
  /** The value of a new form. */
  initial: FieldValue
  mono?: boolean
}

const CA_HINT =
  'The path of a file with the certificates to trust, on every server that may become the manager leader.'

export const FIELDS: Record<AnchorKind, FieldSpec[]> = {
  worm: [
    {
      key: 'endpoint',
      label: 'Endpoint',
      hint: 'The address of the store, for example https://s3.example.com. Leave it empty for Amazon S3.',
      type: 'text',
      initial: '',
      mono: true,
    },
    {
      key: 'region',
      label: 'Region',
      hint: 'For example eu-west-1. Stores that do not use regions still need a name here.',
      type: 'text',
      initial: '',
      required: true,
    },
    {
      key: 'bucket',
      label: 'Bucket',
      hint: 'A bucket with versioning and Object Lock turned on.',
      type: 'text',
      initial: '',
      required: true,
      mono: true,
    },
    {
      key: 'keyPrefix',
      label: 'Key prefix',
      hint: 'The folder the notes are written under. The default is kingres-anchors.',
      type: 'text',
      initial: 'kingres-anchors',
      mono: true,
    },
    {
      key: 'mode',
      label: 'Lock mode',
      type: 'select',
      initial: 'compliance',
      options: [
        { value: 'compliance', label: 'Compliance (cannot be undone)' },
        {
          value: 'governance',
          label: 'Governance (a person with the bypass permission can delete)',
        },
      ],
    },
    {
      key: 'retentionDays',
      label: 'Retention in days',
      hint: 'From 1 to 36500. The default is 3650 (ten years).',
      type: 'number',
      initial: '3650',
    },
    {
      key: 'pathStyle',
      label: 'Address style',
      hint: 'Path style (host/bucket) is usual for a store you run yourself.',
      type: 'select',
      initial: '',
      advanced: true,
      options: [
        { value: '', label: 'Automatic' },
        { value: 'yes', label: 'Path style' },
        { value: 'no', label: 'Host style (bucket.host)' },
      ],
    },
    {
      key: 'ifNoneMatch',
      label: 'Refuse to overwrite',
      hint: 'Automatic asks the store not to replace an object that exists and checks first when the store does not understand the request.',
      type: 'select',
      initial: 'auto',
      advanced: true,
      options: [
        { value: 'auto', label: 'Automatic' },
        { value: 'on', label: 'Always ask the store' },
        { value: 'off', label: 'Check first' },
      ],
    },
    {
      key: 'caBundlePath',
      label: 'Certificates of the store',
      hint: `${CA_HINT} Empty uses the system certificates.`,
      type: 'text',
      initial: '',
      advanced: true,
      mono: true,
    },
    {
      key: 'maxSkewSeconds',
      label: 'Allowed difference of the store clock (seconds)',
      hint: 'From 1 to 86400. Empty means 300.',
      type: 'number',
      initial: '',
      advanced: true,
    },
    {
      key: 'allowInsecure',
      label: 'Allow an endpoint without TLS',
      hint: 'Only for a test store on a private network.',
      type: 'checkbox',
      initial: false,
      advanced: true,
    },
  ],
  tsa: [
    {
      key: 'url',
      label: 'Address of the authority',
      hint: 'An https address (http only with the box below), without a query.',
      type: 'text',
      initial: '',
      required: true,
      mono: true,
    },
    {
      key: 'caBundlePath',
      label: 'Certificates of the authority',
      hint: `${CA_HINT} Without them no answer can be checked.`,
      type: 'text',
      initial: '',
      required: true,
      mono: true,
    },
    {
      key: 'policyOid',
      label: 'Policy identifier',
      hint: 'Optional. Numbers separated by dots, for example 1.2.3.4.',
      type: 'text',
      initial: '',
      mono: true,
    },
    {
      key: 'timeoutSeconds',
      label: 'Time to wait for an answer (seconds)',
      hint: 'From 1 to 300. Empty means 20.',
      type: 'number',
      initial: '',
      advanced: true,
    },
    {
      key: 'maxSkewSeconds',
      label: 'Allowed difference of the authority clock (seconds)',
      hint: 'From 1 to 86400. Empty means 300.',
      type: 'number',
      initial: '',
      advanced: true,
    },
    {
      key: 'signerKeySha256',
      label: 'Pin the signing key',
      hint: 'Optional. 64 lower case hexadecimal digits: the SHA-256 of the public key of the signing certificate. With it an answer signed by any other key is refused.',
      type: 'text',
      initial: '',
      advanced: true,
      mono: true,
    },
    {
      key: 'allowInsecure',
      label: 'Allow an address without TLS',
      hint: 'The answer proves itself, but a user name and a password sent over plain HTTP do not.',
      type: 'checkbox',
      initial: false,
      advanced: true,
    },
  ],
  ots: [
    {
      key: 'calendars',
      label: 'Calendars',
      hint: 'One https address per line. Nothing is sent until you save and a checkpoint is anchored.',
      type: 'lines',
      initial: '',
      required: true,
      mono: true,
    },
    {
      key: 'minCalendars',
      label: 'Calendars that must answer',
      hint: 'A record is accepted when at least this many calendars answered. Empty means 2, or all of them when there are fewer.',
      type: 'number',
      initial: '',
    },
    {
      key: 'upgradeTimeoutDays',
      label: 'Days to wait for the Bitcoin confirmation',
      hint: 'After this many days a record that was never confirmed is marked as such. Empty means 7.',
      type: 'number',
      initial: '',
    },
    {
      key: 'allowInsecure',
      label: 'Allow calendars without TLS',
      hint: 'Only for tests and private networks.',
      type: 'checkbox',
      initial: false,
      advanced: true,
    },
  ],
}

/** The public calendars that opentimestamps.org lists; the console fills them in on request and sends nothing by itself. */
export const PUBLIC_CALENDARS: readonly string[] = [
  'https://alice.btc.calendar.opentimestamps.org',
  'https://bob.btc.calendar.opentimestamps.org',
  'https://finney.calendar.eternitywall.com',
]

export interface CredentialSpec {
  key: string
  label: string
  hint?: string
  /** Required for a new target (an edit keeps what is stored when the field is empty). */
  requiredOnCreate: boolean
  /** The field is a secret (hidden while typing). */
  secret: boolean
}
export const CREDENTIAL_FIELDS: Record<AnchorKind, CredentialSpec[]> = {
  worm: [
    { key: 'accessKeyId', label: 'Access key id', requiredOnCreate: true, secret: false },
    { key: 'secretAccessKey', label: 'Secret access key', requiredOnCreate: true, secret: true },
    {
      key: 'sessionToken',
      label: 'Session token',
      hint: 'Optional, for temporary credentials.',
      requiredOnCreate: false,
      secret: true,
    },
  ],
  tsa: [
    {
      key: 'username',
      label: 'User name',
      hint: 'Optional: only when the authority asks for HTTP Basic credentials.',
      requiredOnCreate: false,
      secret: false,
    },
    { key: 'password', label: 'Password', requiredOnCreate: false, secret: true },
  ],
  ots: [],
}

/** A new form: every field at its initial value. */
export function initialValues(kind: AnchorKind): FormValues {
  return Object.fromEntries(FIELDS[kind].map((f) => [f.key, f.initial]))
}

const asText = (v: unknown): string =>
  typeof v === 'string' ? v : typeof v === 'number' ? String(v) : ''

/** The form of an existing configuration. Members the console does not know are left to `extraMembers`. */
export function valuesFromConfig(kind: AnchorKind, config: Record<string, unknown>): FormValues {
  const values = initialValues(kind)
  for (const f of FIELDS[kind]) {
    const v = config[f.key]
    if (v === undefined || v === null) continue
    if (f.type === 'checkbox') values[f.key] = v === true
    else if (f.type === 'lines') values[f.key] = Array.isArray(v) ? v.map(asText).join('\n') : ''
    else if (f.key === 'pathStyle') values[f.key] = v === true ? 'yes' : v === false ? 'no' : ''
    else values[f.key] = asText(v)
  }
  return values
}

/** The members of a configuration that the console does not know (a newer server may have more); an edit keeps them. */
export function extraMembers(
  kind: AnchorKind,
  config: Record<string, unknown>,
): Record<string, unknown> {
  const known = new Set(FIELDS[kind].map((f) => f.key))
  return Object.fromEntries(Object.entries(config).filter(([k]) => !known.has(k)))
}

const text = (values: FormValues, key: string): string => {
  const v = values[key]
  return typeof v === 'string' ? v.trim() : ''
}
const lines = (values: FormValues, key: string): string[] =>
  text(values, key)
    .split(/\r?\n/)
    .map((x) => x.trim())
    .filter((x) => x !== '')

function wholeBetween(
  errors: Record<string, string>,
  values: FormValues,
  key: string,
  label: string,
  min: number,
  max: number,
): void {
  const v = text(values, key)
  if (v === '') return
  const n = Number(v)
  if (!/^\d+$/.test(v) || !Number.isInteger(n) || n < min || n > max)
    errors[key] = `${label} must be a whole number from ${min} to ${max}.`
}

/** The problems of a form, in words, by field key. An empty object means that the form can be sent. */
export function validateForm(
  kind: AnchorKind,
  name: string,
  values: FormValues,
  credentials: FormValues,
  creating: boolean,
): Record<string, string> {
  const e: Record<string, string> = {}
  const n = name.trim()
  if (n.length < 1 || n.length > 63) e.name = 'Give a name of 1 to 63 characters.'
  for (const f of FIELDS[kind])
    if (
      f.required &&
      f.type !== 'checkbox' &&
      text(values, f.key) === '' &&
      !(f.type === 'lines' && lines(values, f.key).length)
    )
      e[f.key] = `${f.label} is required.`
  const insecure = values.allowInsecure === true
  if (kind === 'worm') {
    if (!e.endpoint && text(values, 'endpoint') !== '') {
      const m = /^(https?):\/\/[^\s/?#@]+\/?$/i.exec(text(values, 'endpoint'))
      if (!m) e.endpoint = 'Use an address like https://host or https://host:9000, without a path.'
      else if (m[1]?.toLowerCase() === 'http' && !insecure)
        e.endpoint = 'An address without TLS needs the box "Allow an endpoint without TLS".'
    }
    if (!e.region && !/^[A-Za-z0-9._-]{1,64}$/.test(text(values, 'region')))
      e.region = 'Use letters, digits and - _ . only, at most 64 characters.'
    const b = text(values, 'bucket')
    if (!e.bucket && (!/^[A-Za-z0-9._-]{3,63}$/.test(b) || /^\.|\.$|\.\./.test(b)))
      e.bucket = 'A bucket name has 3 to 63 letters, digits, dots and dashes.'
    const p = text(values, 'keyPrefix')
    if (
      p !== '' &&
      (!/^[A-Za-z0-9._/-]{1,200}$/.test(p) || p.split('/').some((s) => s === '.' || s === '..'))
    )
      e.keyPrefix = 'Use letters, digits and - _ . / only, at most 200 characters.'
    wholeBetween(e, values, 'retentionDays', 'The retention', 1, 36500)
    wholeBetween(e, values, 'maxSkewSeconds', 'The allowed difference', 1, 86400)
  } else if (kind === 'tsa') {
    if (!e.url) {
      const u = text(values, 'url')
      const m = /^(https?):\/\/[^\s/?#@]+[^\s?#]*$/i.exec(u)
      if (!m || u.length > 2048)
        e.url = 'Use an https address without a query, for example https://tsa.example.com/tsr.'
      else if (m[1]?.toLowerCase() === 'http' && !insecure)
        e.url = 'An address without TLS needs the box "Allow an address without TLS".'
    }
    const oid = text(values, 'policyOid')
    if (oid !== '' && !/^[0-2](\.\d+)+$/.test(oid))
      e.policyOid = 'Use numbers separated by dots, for example 1.2.3.4.'
    const pin = text(values, 'signerKeySha256')
    if (pin !== '' && !/^[0-9a-f]{64}$/.test(pin))
      e.signerKeySha256 = 'Use 64 lower case hexadecimal digits.'
    wholeBetween(e, values, 'timeoutSeconds', 'The time to wait', 1, 300)
    wholeBetween(e, values, 'maxSkewSeconds', 'The allowed difference', 1, 86400)
  } else {
    const cals = lines(values, 'calendars')
    if (!e.calendars) {
      const bad = cals.find((c) => !/^https?:\/\/[^\s/?#@]+[^\s?#]*$/i.test(c))
      if (bad) e.calendars = 'Each line must be an https address.'
      else if (!insecure && cals.some((c) => c.toLowerCase().startsWith('http://')))
        e.calendars = 'An address without TLS needs the box "Allow calendars without TLS".'
      else if (new Set(cals.map((c) => c.replace(/\/+$/, '').toLowerCase())).size !== cals.length)
        e.calendars = 'A calendar is listed twice.'
      else if (cals.length > 8) e.calendars = 'Give at most 8 calendars.'
    }
    wholeBetween(e, values, 'minCalendars', 'The number of calendars', 1, Math.max(1, cals.length))
    wholeBetween(e, values, 'upgradeTimeoutDays', 'The days to wait', 1, 365)
  }
  const cred = CREDENTIAL_FIELDS[kind]
  for (const c of cred)
    if (creating && c.requiredOnCreate && text(credentials, c.key) === '')
      e[`credentials.${c.key}`] = `${c.label} is required.`
  if (kind === 'tsa') {
    const u = text(credentials, 'username')
    const p = text(credentials, 'password')
    if (u !== '' && p === '') e['credentials.password'] = 'Give the password for this user name.'
    if (p !== '' && u === '') e['credentials.username'] = 'Give the user name for this password.'
  }
  return e
}

/**
 * The configuration to send. Only what was filled in; a member that equals its default is left out so that the server's
 * default stays the server's. Members the console does not know are kept as they were.
 */
export function buildConfig(
  kind: AnchorKind,
  values: FormValues,
  extra: Record<string, unknown> = {},
): Record<string, unknown> {
  const out: Record<string, unknown> = { ...extra }
  for (const f of FIELDS[kind]) {
    const v = values[f.key]
    if (f.type === 'checkbox') {
      if (v === true) out[f.key] = true
      continue
    }
    if (f.type === 'lines') {
      const l = lines(values, f.key)
      if (l.length) out[f.key] = l
      continue
    }
    const t = text(values, f.key)
    if (t === '') continue
    if (f.type === 'number') out[f.key] = Number(t)
    else if (f.key === 'pathStyle') out[f.key] = t === 'yes'
    else out[f.key] = t
  }
  return out
}

/** The credentials to send: the fields that were typed, or nothing at all (so that the stored ones are kept). */
export function buildCredentials(
  kind: AnchorKind,
  credentials: FormValues,
): Record<string, string> | undefined {
  const out: Record<string, string> = {}
  for (const c of CREDENTIAL_FIELDS[kind]) {
    const v = credentials[c.key]
    if (typeof v === 'string' && v !== '') out[c.key] = v
  }
  return Object.keys(out).length ? out : undefined
}

/**
 * The field a message of the server is about: `config.bucket` and `/config/bucket` both name the field `bucket`, and
 * `credentials.password` its own field; `name` is the name of the target.
 */
export function fieldOfServerError(field: string): string {
  const f = field.replace(/^\//, '').replace(/\//g, '.')
  if (f.startsWith('config.')) return f.slice('config.'.length)
  return f
}

/** True when saving this configuration needs the typed confirmation of design 8.5 (compliance mode, which cannot be undone). */
export function needsComplianceConfirmation(
  kind: AnchorKind,
  values: FormValues,
  before: LedgerAnchorTarget | null,
): boolean {
  if (kind !== 'worm') return false
  const mode = text(values, 'mode') || 'compliance'
  if (mode !== 'compliance') return false
  if (!before) return true
  const was = typeof before.config.mode === 'string' ? before.config.mode : 'compliance'
  return was !== 'compliance'
}

/** The retention in days that a WORM form would send. */
export function retentionOf(values: FormValues): number {
  const t = text(values, 'retentionDays')
  return t === '' ? 3650 : Number(t)
}

/** The sentence under the list of targets about the advanced fields of a configuration; secrets are never part of it. */
export function configSummary(t: Pick<LedgerAnchorTarget, 'kind' | 'config'>): string {
  const c = t.config
  const s = (k: string): string => (typeof c[k] === 'string' ? safeText(c[k] as string, 120) : '')
  if (t.kind === 'worm') {
    const place = [s('endpoint'), s('bucket')].filter(Boolean).join(' / ')
    const mode = s('mode') || 'compliance'
    return `${place || 'Amazon S3'}, ${mode} mode`
  }
  if (t.kind === 'tsa') return s('url')
  const n = Array.isArray(c.calendars) ? c.calendars.length : 0
  return `${n} ${n === 1 ? 'calendar' : 'calendars'}`
}
