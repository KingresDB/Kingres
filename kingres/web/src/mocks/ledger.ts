// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The 28 operations that contract 0.11.0 added to the tag `Ledger` (signing, publication, anchoring, aggregates, bundles,
// client keys, divergence) for the mock API. They follow the design of phase 7 and never answer with more than the
// contract allows. The answers validate against the schemas of the contract (tests/ledger-mock-0-11.test.ts checks each
// one). `?phase7=today` makes the 21 operations that the real `kingresd` does not build yet answer 422
// `unsupported_operation`, as it does. A private key, a seed or a passphrase is never part of anything here.
import type { components } from '@/api/schema'
import type { OperationId } from '@/api/operations.generated'
import { accepted, created, noContent, ok, paginate, problem, validationFailed } from './http'
import type { Ctx, Handler } from './http'
import { createTask, mockConfig, nowIso } from './state'
import type { DbRec, MockState } from './state'
import { hex64, NOT_BUILT_YET, prefixLocked, signingKey, type Ledger7State } from './ledger-data'
import {
  aggregateMembers,
  aggregateSummary,
  AGGREGATE_SIZE,
  groupsOf,
  hasLedger,
  isDiverged,
  l7,
  published,
  publicationLogs,
  STATEMENT,
} from './ledger-view'

type S = components['schemas']

const body = <T>(ctx: Ctx): T => (ctx.body ?? {}) as T

function db(s: MockState, name: string): DbRec {
  const rec = s.databases.find((d) => d.info.name === name)
  if (!rec) throw problem('not_found', `Database "${name}" does not exist.`)
  return rec
}

// --- signing -------------------------------------------------------------------------------------

function signingView(s: MockState): S['LedgerSigning'] {
  const g = l7(s).signing
  return {
    clusterId: g.clusterId,
    mode: g.mode,
    passphraseProtected: g.passphraseProtected,
    holders: [...g.holders],
    keys: structuredClone(g.keys),
    events: structuredClone(g.events),
    backupAdvised: g.backupAdvised,
    discontinuity: g.discontinuity,
    signerAlarm: g.signerAlarm,
  }
}

/** A key task is running: one at a time, cluster-wide (the subject `ledgerKey` with the id `signing`). */
function keyTaskRunning(s: MockState): boolean {
  return s.tasks.some((t) => t.task.state === 'running' && t.task.subject?.type === 'ledgerKey')
}

const KEY_SUBJECT = { type: 'ledgerKey', id: 'signing' } as const

function keyTask(c: Ctx, kind: S['TaskKind'], steps: string[], apply: () => void): S['Task'] {
  const l = l7(c.s)
  const failure = l.keyFailure
  l.keyFailure = null
  return createTask(c.s, {
    kind,
    subject: { ...KEY_SUBJECT },
    steps,
    apply,
    durationMs: 3000,
    ...(failure ? { failAt: { fraction: 0.6, code: failure.code, message: failure.message } } : {}),
  })
}

function addEvent(l: Ledger7State, type: string): void {
  l.signing.events.push({
    seq: l.signing.events.length,
    type,
    time: `${new Date().toISOString().slice(0, 19)}Z`,
  })
}

function keyOf(s: MockState, c: Ctx): S['LedgerSigningKey'] {
  const k = l7(s).signing.keys.find((x) => x.fingerprint === c.params.fingerprint)
  if (!k) throw problem('not_found', 'The key set has no key with this fingerprint.')
  return k
}

const PRINTABLE_ASCII = /^[\x20-\x7e]*$/

function createKey(c: Ctx) {
  const s = c.s
  const l = l7(s)
  const b = body<S['CreateLedgerSigningKeyRequest']>(c)
  if (
    b.overlapDays !== undefined &&
    (!Number.isInteger(b.overlapDays) || b.overlapDays < 1 || b.overlapDays > 365)
  )
    throw validationFailed([
      { field: 'overlapDays', message: 'Enter a whole number of days from 1 to 365.' },
    ])
  if (b.publicKey !== undefined)
    throw problem(
      'unsupported_operation',
      'A public key is accepted for the signing mode command only.',
    )
  if (keyTaskRunning(s)) throw problem('operation_in_progress', 'A key task is running.')
  const first = l.signing.keys.length === 0
  const hasActive = l.signing.keys.some((k) => k.state === 'active')
  if (!first && !hasActive)
    throw problem('signing_key_missing', 'No signing key is active, so there is nothing to rotate.')
  const seed = `k${l.signing.keys.length + 1}-${Date.now()}`
  const apply = () => {
    const key = signingKey(seed, 'active', 1000)
    l.signing.keys.push(key)
    if (first) l.signing.clusterId = hex64(`cluster-${seed}`)
    addEvent(l, 'key-added')
    l.signing.backupAdvised = b.backupMade !== true
    l.signing.signerAlarm = null
    l.settings.publication.originPrefixLocked = true
  }
  return accepted(
    keyTask(
      c,
      first ? 'ledger.key.create' : 'ledger.key.rotate',
      first
        ? ['generate', 'store', 'distribute', 'event', 'activate']
        : ['generate', 'store', 'distribute', 'event', 'overlap'],
      apply,
    ),
  )
}

function retireKey(c: Ctx) {
  const s = c.s
  const l = l7(s)
  const key = keyOf(s, c)
  if (key.state !== 'active') throw problem('conflict', 'Only an active key can be retired.')
  if (keyTaskRunning(s)) throw problem('operation_in_progress', 'A key task is running.')
  if (!l.signing.keys.some((k) => k.fingerprint !== key.fingerprint && k.state === 'active'))
    throw problem('last_signing_key', 'No other key is active and in force.')
  return accepted(
    keyTask(c, 'ledger.key.retire', ['bound', 'event', 'retire', 'remove files'], () => {
      key.state = 'retired'
      key.retiredAt = nowIso()
      key.maxTreeSize = 1800
      addEvent(l, 'key-retired')
    }),
  )
}

function revokeKey(c: Ctx) {
  const s = c.s
  const l = l7(s)
  const key = keyOf(s, c)
  const b = body<S['RevokeLedgerSigningKeyRequest']>(c)
  const errors: { field: string; message: string }[] = []
  if (b.confirm !== key.fingerprint)
    errors.push({ field: 'confirm', message: 'This is not the fingerprint of the key.' })
  if (typeof b.reason !== 'string' || b.reason.length > 200 || !PRINTABLE_ASCII.test(b.reason))
    errors.push({ field: 'reason', message: 'Use at most 200 printable ASCII characters.' })
  const since = Date.parse(b.compromisedSince ?? '')
  if (Number.isNaN(since))
    errors.push({ field: 'compromisedSince', message: 'Give the time as a date and time.' })
  if (errors.length > 0) throw validationFailed(errors)
  if (key.state === 'revoked') throw problem('conflict', 'The key is already revoked.')
  if (keyTaskRunning(s)) throw problem('operation_in_progress', 'A key task is running.')
  if (!l.signing.keys.some((k) => k.fingerprint !== key.fingerprint && k.state === 'active'))
    throw problem('last_signing_key', 'No other key is active and in force. Create a key first.')
  // The time is clamped: never later than the revocation, never before the creation of the key (design, 14.125).
  const clamped = Math.min(Date.now(), Math.max(since, Date.parse(key.createdAt)))
  return accepted(
    keyTask(c, 'ledger.key.revoke', ['event', 'revoke', 'remove files'], () => {
      key.state = 'revoked'
      key.revokedAt = nowIso()
      key.compromisedSince = new Date(clamped).toISOString()
      addEvent(l, 'key-revoked')
      l.signing.backupAdvised = true
    }),
  )
}

function trustFile(c: Ctx) {
  const l = l7(c.s)
  if (l.signing.keys.length === 0)
    throw problem('signing_key_missing', 'No signing key exists, so there is no trust file.')
  const file: S['LedgerTrustFile'] = {
    format: 1,
    origin_prefix: l.settings.publication.originPrefix,
    keys: l.signing.keys
      .filter((k) => k.state !== 'next')
      .map((k, i) => ({
        name: `key-${i + 1}`,
        public_key: k.publicKey,
        fingerprint: k.fingerprint,
        not_before: k.activatedAt,
        not_after: null,
        revoked: k.state === 'revoked',
        max_tree_size: k.state === 'retired' ? (k.maxTreeSize ?? null) : null,
      })),
    tsa_roots: [],
    client_keys: [],
    witnesses: [],
  }
  return {
    status: 200,
    rawJson: `${JSON.stringify(file, null, 2)}\n`,
    headers: { 'Content-Disposition': 'attachment; filename="trust.json"' },
  }
}

// --- settings ------------------------------------------------------------------------------------

function updateSettings(c: Ctx) {
  const l = l7(c.s)
  const b = body<S['LedgerSettingsUpdate']>(c)
  const errors: { field: string; message: string }[] = []
  const range = (v: number | undefined, field: string, lo: number, hi: number) => {
    if (v !== undefined && (!Number.isInteger(v) || v < lo || v > hi))
      errors.push({ field, message: `Enter a whole number from ${lo} to ${hi}.` })
  }
  range(b.publication?.intervalSeconds, 'publication.intervalSeconds', 10, 3600)
  range(b.anchor?.intervalSeconds, 'anchor.intervalSeconds', 60, 86400)
  range(b.anchor?.minimumAnchored, 'anchor.minimumAnchored', 1, 8)
  const prefix = b.publication?.originPrefix
  if (
    prefix !== undefined &&
    (prefix.length < 1 ||
      prefix.length > 160 ||
      /:\/\/|\/$|\s/.test(prefix) ||
      !PRINTABLE_ASCII.test(prefix))
  )
    errors.push({
      field: 'publication.originPrefix',
      message:
        'Use a name without a scheme, without a trailing slash and without spaces (at most 160 characters).',
    })
  const url = b.publication?.publicUrl
  if (url !== undefined && url !== null && !/^https?:\/\/[^\s/]+/.test(url))
    errors.push({
      field: 'publication.publicUrl',
      message: 'Give an address that starts with http:// or https://.',
    })
  if (errors.length > 0) throw validationFailed(errors)
  if (prefix !== undefined && prefix !== l.settings.publication.originPrefix && prefixLocked(l))
    throw problem(
      'origin_prefix_locked',
      'A signing key exists, and every signature is made under the origin prefix.',
    )
  const p = l.settings.publication
  if (b.publication?.enabled !== undefined) p.enabled = b.publication.enabled
  if (prefix !== undefined) p.originPrefix = prefix
  if (url !== undefined) p.publicUrl = url
  if (b.publication?.intervalSeconds !== undefined)
    p.intervalSeconds = b.publication.intervalSeconds
  Object.assign(l.settings.anchor, b.anchor ?? {})
  p.originPrefixLocked = prefixLocked(l)
  return ok(structuredClone(l.settings))
}

// --- anchoring -----------------------------------------------------------------------------------

function target(s: MockState, c: Ctx): S['LedgerAnchorTarget'] {
  const t = l7(s).targets.find((x) => String(x.id) === c.params.targetId)
  if (!t) throw problem('not_found', 'The anchor target does not exist.')
  return t
}

/** The members of the configuration of each kind (as `ParseConfig` of the kind has them): the server rejects any other. */
const CONFIG_MEMBERS: Record<string, string[]> = {
  worm: [
    'endpoint',
    'region',
    'bucket',
    'keyPrefix',
    'pathStyle',
    'mode',
    'retentionDays',
    'ifNoneMatch',
    'caBundlePath',
    'allowInsecure',
    'maxSkewSeconds',
    'credentialsPath',
  ],
  tsa: [
    'url',
    'hashAlgorithm',
    'policyOid',
    'caBundlePath',
    'timeoutSeconds',
    'maxSkewSeconds',
    'allowInsecure',
    'signerKeySha256',
    'credentialsPath',
  ],
  ots: [
    'calendars',
    'minCalendars',
    'upgradeTimeoutDays',
    'allowInsecure',
    'contradictionConfirmMinutes',
    'headerSource',
  ],
}

type FieldProblem = { field: string; message: string; code?: string }
const UNKNOWN_PREFIX: Record<string, string> = {
  worm: 'worm: configuration: ',
  tsa: 'tsa: config: ',
  ots: 'ots: configuration: ',
}

/**
 * The first thing wrong with a configuration, in the words of the server (`internal/ledgeranchor`, `anchor/worm`, `anchor/tsa`,
 * `anchor/ots`): the server answers one message at `/config`, not one per member. Null when it is good.
 */
function configProblem(kind: string, c: Record<string, unknown>): string | null {
  const known = CONFIG_MEMBERS[kind] ?? []
  const unknown = Object.keys(c).find((k) => !known.includes(k))
  if (unknown !== undefined)
    return `${UNKNOWN_PREFIX[kind] ?? ''}json: unknown field ${JSON.stringify(unknown)}`
  const str = (k: string) => (typeof c[k] === 'string' ? (c[k] as string) : '')
  const whole = (k: string, lo: number, hi: number): boolean =>
    c[k] === undefined ||
    (typeof c[k] === 'number' &&
      Number.isInteger(c[k]) &&
      (c[k] as number) >= lo &&
      (c[k] as number) <= hi)
  const insecure = c.allowInsecure === true
  if (kind === 'worm') {
    if (!/^[A-Za-z0-9._-]{1,64}$/.test(str('region')))
      return 'worm: region is required (letters, digits, - _ .)'
    const b = str('bucket')
    if (!/^[A-Za-z0-9._-]{3,63}$/.test(b) || /^\.|\.$|\.\./.test(b))
      return 'worm: bucket name is not valid'
    const pre = str('keyPrefix')
    if (
      pre !== '' &&
      (!/^[A-Za-z0-9._/-]{1,200}$/.test(pre) || pre.split('/').some((x) => x === '.' || x === '..'))
    )
      return 'worm: keyPrefix must be 1 to 200 characters of letters, digits, - _ . /'
    if (c.mode !== undefined && c.mode !== 'compliance' && c.mode !== 'governance')
      return 'worm: mode must be governance or compliance'
    if (!whole('retentionDays', 1, 36500)) return 'worm: retentionDays must be 1 to 36500'
    if (c.ifNoneMatch !== undefined && !['auto', 'on', 'off'].includes(String(c.ifNoneMatch)))
      return 'worm: ifNoneMatch must be auto, on or off'
    if (!whole('maxSkewSeconds', 0, 86400)) return 'worm: maxSkewSeconds must be 0 to 86400'
    if (str('endpoint') !== '') {
      const m = /^(https?):\/\/[^\s/?#@]+\/?$/.exec(str('endpoint'))
      if (!m)
        return 'worm: endpoint must be scheme://host[:port] without path, user information or query'
      if (m[1] === 'http' && !insecure)
        return 'worm: endpoint is not https and allowInsecure is not set'
    }
  } else if (kind === 'tsa') {
    if (!/^https?:\/\/[^\s/?#@]+[^\s#]*$/.test(str('url')))
      return 'tsa: config: url must be an http or https URL without user information'
    if (/\?/.test(str('url'))) return 'tsa: config: url must not have a query'
    if (c.hashAlgorithm !== undefined && c.hashAlgorithm !== '' && c.hashAlgorithm !== 'sha256')
      return 'tsa: config: hashAlgorithm must be sha256'
    if (str('policyOid') !== '' && !/^[0-2](\.\d+)+$/.test(str('policyOid')))
      return 'tsa: config: policyOid is not an OID'
    if (str('caBundlePath') === '') return 'tsa: config: caBundlePath is required'
    if (str('signerKeySha256') !== '' && !/^[0-9a-f]{64}$/.test(str('signerKeySha256')))
      return 'tsa: config: signerKeySha256 must be 64 lower case hexadecimal digits'
    if (!whole('timeoutSeconds', 0, 300)) return 'tsa: config: timeoutSeconds must be 0 to 300'
    if (!whole('maxSkewSeconds', 0, 86400)) return 'tsa: config: maxSkewSeconds must be 0 to 86400'
  } else if (kind === 'ots') {
    const cals = Array.isArray(c.calendars) ? (c.calendars as unknown[]) : []
    if (cals.length === 0) return 'ots: configuration: at least one calendar is required'
    if (cals.length > 16) return 'ots: configuration: at most 16 calendars'
    for (const [i, x] of cals.entries())
      if (
        typeof x !== 'string' ||
        !/^https?:\/\/[^\s/?#@]+/.test(x) ||
        (x.startsWith('http://') && !insecure)
      )
        return `ots: configuration: calendar ${i + 1}: must be an https URL`
    if (new Set(cals.map((x) => String(x).replace(/\/+$/, '').toLowerCase())).size !== cals.length)
      return 'ots: configuration: a calendar is a repeat'
    if (c.minCalendars !== undefined && !whole('minCalendars', 1, cals.length))
      return 'ots: configuration: minCalendars must be between 1 and the number of calendars'
    if (!whole('contradictionConfirmMinutes', 1, 10080))
      return 'ots: configuration: contradictionConfirmMinutes must be between 1 and 10080'
    if (!whole('upgradeTimeoutDays', 1, 365))
      return 'ots: configuration: upgradeTimeoutDays must be between 1 and 365'
    if (c.headerSource !== undefined) {
      const hs = c.headerSource as { url?: unknown } | null
      if (typeof hs !== 'object' || hs === null || Object.keys(hs).some((k) => k !== 'url'))
        return 'headerSource must be {"url": "..."}'
      const u = typeof hs.url === 'string' ? hs.url : ''
      if (!/^https?:\/\/[^\s/?#@]+[^\s?#]*$/.test(u) || (u.startsWith('http://') && !insecure))
        return 'headerSource.url must be an https URL (http only with allowInsecure) without user information or query'
    }
  }
  return null
}

/** The members of the credentials of each kind with the length the server accepts (`credMembers` of `creds.go`). */
const CRED_MEMBERS: Record<
  string,
  { name: string; required: boolean; min: number; max: number; space?: boolean }[]
> = {
  worm: [
    { name: 'accessKeyId', required: true, min: 4, max: 128 },
    { name: 'secretAccessKey', required: true, min: 4, max: 256 },
    { name: 'sessionToken', required: false, min: 4, max: 4096 },
  ],
  tsa: [
    { name: 'username', required: false, min: 1, max: 256 },
    { name: 'password', required: false, min: 1, max: 256, space: true },
  ],
  ots: [],
}

/** The problem of a credentials object (the member and the rule, never a value), or null; `{}` removes the stored ones. */
function credentialsProblem(kind: string, credentials: unknown): string | null {
  const members = CRED_MEMBERS[kind] ?? []
  if (members.length === 0) return 'this kind of target takes no credentials'
  if (typeof credentials !== 'object' || credentials === null || Array.isArray(credentials))
    return 'the credentials must be a JSON object'
  const c = credentials as Record<string, unknown>
  if (Object.keys(c).length === 0) return null
  for (const k of Object.keys(c)) {
    const m = members.find((x) => x.name === k)
    if (!m) return `${kind}: unknown credential member ${JSON.stringify(k)}`
    const v = c[k]
    if (typeof v !== 'string') return `credentials.${k} must be a string`
    if (v.length < m.min || v.length > m.max)
      return `credentials.${k} must have ${m.min} to ${m.max} characters`
    if (!(m.space ? /^[\x20-\x7e]*$/ : /^[\x21-\x7e]*$/).test(v))
      return `credentials.${k} may hold printable ASCII characters only (no white space or control character)`
  }
  for (const m of members) if (m.required && !c[m.name]) return `credentials.${m.name} is required`
  if (kind === 'tsa' && Boolean(c.username) !== Boolean(c.password))
    return 'credentials.username and credentials.password go together'
  return null
}

const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9 ._-]{0,62}$/

/** The checks of `shape` and `checkName` of `internal/ledgeranchor/rest.go`, with its fields and its words. */
function checkTarget(
  kind: S['LedgerAnchorKind'],
  name: unknown,
  config: unknown,
  credentials?: unknown,
  enabling?: { enabled: boolean; hasCredentials: boolean },
): void {
  const fail = (_title: string, f: FieldProblem): never => {
    throw validationFailed([f])
  }
  if (typeof name !== 'string' || !NAME_RE.test(name))
    fail('The name is not valid.', {
      field: '/name',
      message:
        '1 to 63 characters: letters, digits, space, dot, hyphen and underscore, starting with a letter or a digit',
      code: 'invalid',
    })
  if (!['worm', 'tsa', 'ots'].includes(kind))
    fail('The kind is not valid.', {
      field: '/kind',
      message: 'one of worm, tsa, ots',
      code: 'invalid',
    })
  if (typeof config !== 'object' || config === null || Array.isArray(config))
    fail('The configuration is not valid.', {
      field: '/config',
      message: 'the configuration must be a JSON object',
      code: 'invalid',
    })
  const cfg = config as Record<string, unknown>
  const { credentialsPath, ...rest } = cfg
  const bad = configProblem(kind, {
    ...rest,
    ...(credentialsPath === undefined ? {} : { credentialsPath }),
  })
  if (bad !== null)
    fail('The configuration is not valid.', { field: '/config', message: bad, code: 'invalid' })
  if (kind === 'tsa' && typeof cfg.caBundlePath === 'string' && !cfg.caBundlePath.startsWith('/'))
    fail('The configuration is not valid.', {
      field: '/config/caBundlePath',
      message: 'the bundle cannot be read by this server (an absolute path to a PEM file)',
      code: 'invalid',
    })
  const given = credentials !== undefined && credentials !== null
  if (given) {
    if (kind === 'ots')
      fail('The credentials are not valid.', {
        field: '/credentials',
        message: 'this kind of target takes no credentials',
        code: 'invalid',
      })
    if (typeof credentialsPath === 'string' && credentialsPath !== '')
      fail('The credentials are not valid.', {
        field: '/credentials',
        message: 'credentials and credentialsPath exclude each other',
        code: 'invalid',
      })
    const why = credentialsProblem(kind, credentials)
    if (why !== null)
      fail('The credentials are not valid.', {
        field: '/credentials',
        message: why,
        code: 'invalid',
      })
  }
  if (typeof credentialsPath === 'string' && credentialsPath !== '') {
    if (kind === 'ots')
      fail('The configuration is not valid.', {
        field: '/config/credentialsPath',
        message: 'this kind of target takes no credentials',
        code: 'invalid',
      })
    if (!credentialsPath.startsWith('/'))
      fail('The configuration is not valid.', {
        field: '/config/credentialsPath',
        message:
          'the credentials file cannot be read by this server (a regular file that only its owner can read, holding the members of the kind)',
        code: 'invalid',
      })
  }
  if (
    enabling?.enabled &&
    kind === 'worm' &&
    !enabling.hasCredentials &&
    !(typeof credentialsPath === 'string' && credentialsPath !== '')
  )
    fail('The credentials are not valid.', {
      field: '/credentials',
      message: 'credentials are required to enable a target of this kind',
      code: 'required',
    })
}

/** True when a credentials object holds something (`{}` and null hold nothing: they remove or leave out). */
function credentialsGiven(c: unknown): boolean {
  return typeof c === 'object' && c !== null && Object.keys(c).length > 0
}

/** `?anchors=refuse:<code>`: every change answers this problem code, as a server that refuses would. */
function refuseWrites(): void {
  const code = mockConfig.anchorRefuse
  if (code)
    throw problem(
      code as Parameters<typeof problem>[0],
      'The server refused the request (a state of the mock).',
    )
}

/** What `?anchors=testfail` and `testwarn` make a test find: the first check fails, or one check warns. */
function shaped(
  checks: S['LedgerAnchorTest']['checks'],
  kind: string,
): S['LedgerAnchorTest']['checks'] {
  const how = mockConfig.anchorTest
  if (how === 'fail')
    return [
      {
        name: kind === 'ots' ? 'calendars' : kind === 'tsa' ? 'reachable' : 'credentials',
        status: 'failed',
        detail:
          kind === 'worm'
            ? 'The store refused the credentials (403).'
            : kind === 'tsa'
              ? 'The authority did not answer within 20 seconds.'
              : 'None of the calendars answered.',
      },
      ...checks.slice(1).map((c) => ({ ...c, status: 'ok' as const })),
    ]
  if (how === 'warn')
    return checks.map((c, i) =>
      i === checks.length - 1
        ? {
            name: c.name,
            status: 'warning' as const,
            detail: 'The check passed, but the store keeps no default retention.',
          }
        : c,
    )
  return checks
}

function testResult(t: S['LedgerAnchorTarget']): S['LedgerAnchorTest'] {
  const checks: S['LedgerAnchorTest']['checks'] = shaped(
    t.kind === 'worm'
      ? [
          {
            name: 'credentials',
            status: t.credentialsSet ? 'ok' : 'failed',
            detail: t.credentialsSet
              ? 'The store accepted the credentials.'
              : 'No credentials are stored.',
          },
          { name: 'bucket', status: 'ok', detail: 'The bucket exists.' },
          { name: 'object lock', status: 'ok', detail: 'Object Lock is enabled on the bucket.' },
        ]
      : t.kind === 'tsa'
        ? [
            { name: 'reachable', status: 'ok', detail: 'The authority answered.' },
            {
              name: 'certificate',
              status: 'warning',
              detail: 'The certificate of the authority expires in 41 days.',
            },
          ]
        : [{ name: 'calendars', status: 'ok', detail: 'Two of three calendars answered.' }],
    t.kind,
  )
  return { checks, ok: !checks.some((x) => x.status === 'failed'), testedAt: nowIso() }
}

function anchorRecords(s: MockState, c: Ctx) {
  const rec = db(s, c.params.databaseName ?? '')
  const tid = c.url.searchParams.get('targetId')
  const st = c.url.searchParams.get('state')
  const items = l7(s)
    .anchors.filter(
      (a) =>
        a.databaseName === rec.info.name &&
        (!tid || String(a.targetId) === tid) &&
        (!st || a.state === st),
    )
    .sort((a, b) => b.id - a.id)
    .map(({ databaseName: _d, ...a }) => {
      void _d
      return a
    })
  return ok(paginate(items, c.url))
}

/** The anchor records a switch seeds: one WORM target with the newest aggregate anchored, late or failed. */
export function seedAnchors(s: MockState, how: 'ok' | 'behind' | 'failed'): void {
  const l = l7(s)
  const id = l.nextTargetId++
  l.targets.push({
    id,
    kind: 'worm',
    name: 'archive-worm',
    enabled: true,
    config: {
      endpoint: 'https://s3.archive.example.com',
      region: 'eu-west-1',
      bucket: 'kingres-ledger',
      mode: 'compliance',
    },
    credentialsSet: true,
    lastTest: testResult({ kind: 'worm', credentialsSet: true } as S['LedgerAnchorTarget']),
    newestAnchoredAt: new Date(
      Date.now() - (how === 'ok' ? 40 * 60_000 : 7 * 3600_000),
    ).toISOString(),
    state: how,
  })
  for (const rec of s.databases.filter(hasLedger)) {
    for (let i = 0; i < 3; i++) {
      const size = AGGREGATE_SIZE - i * 6
      const failed = how === 'failed' && i === 0
      const anchoredAt = new Date(
        Date.now() - (i * 3600_000 + (how === 'ok' ? 40 * 60_000 : 7 * 3600_000)),
      ).toISOString()
      l.anchors.push({
        databaseName: rec.info.name,
        id: l.nextAnchorId++,
        targetId: id,
        targetName: 'archive-worm',
        kind: 'worm',
        aggregateSize: size,
        state: failed ? 'failed' : 'anchored',
        attempts: failed ? 5 : 1,
        nextAttemptAt: null,
        anchoredAt: failed ? null : anchoredAt,
        evidenceTime: failed ? null : anchoredAt,
        lastError: failed ? 'The store refused the credentials (403).' : null,
      })
    }
  }
}

type Seeded = Omit<S['LedgerAnchor'], 'id' | 'targetId' | 'targetName' | 'kind'>

/** One record of a seeded target in every database that has a ledger. */
function seedRecords(
  s: MockState,
  t: S['LedgerAnchorTarget'],
  rows: { size: number; extra: Partial<Seeded> & Pick<Seeded, 'state'> }[],
): void {
  const l = l7(s)
  for (const rec of s.databases.filter(hasLedger))
    for (const row of rows)
      l.anchors.push({
        databaseName: rec.info.name,
        id: l.nextAnchorId++,
        targetId: t.id,
        targetName: t.name,
        kind: t.kind,
        aggregateSize: row.size,
        attempts: 1,
        nextAttemptAt: null,
        anchoredAt: null,
        evidenceTime: null,
        lastError: null,
        ...row.extra,
      })
}

/**
 * `?anchors=all`: the three kinds of target and a record in every state: the WORM store anchors in time; the time-stamp
 * authority is late, has one record waiting, one failed and one anchored; OpenTimestamps has one sent and not final, one
 * anchored and one never confirmed.
 */
export function seedAnchorsAll(s: MockState): void {
  // Once: a second call (the browser tests apply it while the page is open) leaves what is there.
  if (l7(s).targets.some((t) => t.name === 'public-tsa')) return
  seedAnchors(s, 'ok')
  const l = l7(s)
  const ago = (ms: number): string => new Date(Date.now() - ms).toISOString()
  const tsa: S['LedgerAnchorTarget'] = {
    id: l.nextTargetId++,
    kind: 'tsa',
    name: 'public-tsa',
    enabled: true,
    config: { url: 'https://tsa.example.com/tsr', caBundlePath: '/etc/kingres/tsa.pem' },
    credentialsSet: false,
    lastTest: null,
    newestAnchoredAt: ago(7 * 3600_000),
    state: 'behind',
  }
  tsa.lastTest = testResult(tsa)
  const ots: S['LedgerAnchorTarget'] = {
    id: l.nextTargetId++,
    kind: 'ots',
    name: 'calendars',
    enabled: true,
    config: {
      calendars: [
        'https://alice.btc.calendar.opentimestamps.org',
        'https://bob.btc.calendar.opentimestamps.org',
      ],
      minCalendars: 2,
    },
    credentialsSet: false,
    lastTest: null,
    newestAnchoredAt: ago(5 * 3600_000),
    state: 'ok',
  }
  l.targets.push(tsa, ots)
  const A = AGGREGATE_SIZE
  seedRecords(s, tsa, [
    { size: A, extra: { state: 'pending', attempts: 2, nextAttemptAt: ago(-60_000) } },
    {
      size: A - 6,
      extra: {
        state: 'failed',
        attempts: 4,
        lastError: 'The authority rejected the request: the policy is not accepted.',
      },
    },
    {
      size: A - 12,
      extra: { state: 'anchored', anchoredAt: ago(7 * 3600_000), evidenceTime: ago(7 * 3600_000) },
    },
  ])
  seedRecords(s, ots, [
    { size: A, extra: { state: 'submitted', attempts: 1, nextAttemptAt: ago(-600_000) } },
    {
      size: A - 6,
      extra: { state: 'anchored', anchoredAt: ago(5 * 3600_000), evidenceTime: ago(5 * 3600_000) },
    },
    {
      size: A - 12,
      extra: {
        state: 'expired',
        attempts: 7,
        lastError: 'No Bitcoin confirmation arrived within 7 days.',
      },
    },
  ])
}

/** `?anchors=disabled`: a target exists and is turned off, so nothing is anchored. */
export function seedAnchorsDisabled(s: MockState): void {
  const l = l7(s)
  l.targets.push({
    id: l.nextTargetId++,
    kind: 'worm',
    name: 'archive-worm',
    enabled: false,
    config: { region: 'eu-west-1', bucket: 'kingres-ledger', mode: 'compliance' },
    credentialsSet: true,
    lastTest: null,
    newestAnchoredAt: null,
    state: 'none',
  })
}

// --- client keys ---------------------------------------------------------------------------------

function isKey32(text: unknown): boolean {
  if (typeof text !== 'string' || !/^[A-Za-z0-9+/]{43}=$/.test(text)) return false
  try {
    return atob(text).length === 32
  } catch {
    return false
  }
}

// --- the handlers --------------------------------------------------------------------------------

export function ledgerHandlers(): Partial<Record<OperationId, Handler>> {
  return {
    getLedgerSettings: (c) => ok(structuredClone(l7(c.s).settings)),
    updateLedgerSettings: updateSettings,
    getLedgerSigning: (c) => ok(signingView(c.s)),
    createLedgerSigningKey: createKey,
    retireLedgerSigningKey: retireKey,
    revokeLedgerSigningKey: revokeKey,
    getLedgerTrustFile: trustFile,
    getLedgerPublication: (c) => {
      const l = l7(c.s)
      const p = l.settings.publication
      return ok({
        enabled: p.enabled,
        listener: structuredClone(l.publication.listener),
        logs: p.enabled ? publicationLogs(c.s) : [],
        lastCheckpointAt: p.enabled
          ? new Date(
              Date.now() - (l.publication.health === 'behind' ? 12 * 60_000 : 20_000),
            ).toISOString()
          : null,
        checks: {},
      } satisfies S['LedgerPublication'])
    },
    listLedgerAnchorTargets: (c) => ok({ items: structuredClone(l7(c.s).targets) }),
    createLedgerAnchorTarget: (c) => {
      refuseWrites()
      const l = l7(c.s)
      const b = body<S['LedgerAnchorTargetCreate']>(c)
      checkTarget(b.kind, b.name, b.config, b.credentials, {
        enabled: b.enabled ?? false,
        hasCredentials: credentialsGiven(b.credentials),
      })
      if (l.targets.some((t) => t.name === b.name))
        throw problem('already_exists', 'A target with this name exists.')
      const t: S['LedgerAnchorTarget'] = {
        id: l.nextTargetId++,
        kind: b.kind,
        name: b.name,
        enabled: b.enabled ?? false,
        config: b.config,
        credentialsSet: credentialsGiven(b.credentials),
        lastTest: null,
        newestAnchoredAt: null,
        state: 'none',
      }
      l.targets.push(t)
      return created(structuredClone(t))
    },
    getLedgerAnchorTarget: (c) => ok(structuredClone(target(c.s, c))),
    updateLedgerAnchorTarget: (c) => {
      refuseWrites()
      const t = target(c.s, c)
      const b = body<S['LedgerAnchorTargetUpdate']>(c)
      if (b.kind !== undefined && b.kind !== t.kind)
        throw problem('immutable_field', 'The kind of a target cannot change.')
      const stillHas =
        b.credentials === undefined ? t.credentialsSet : credentialsGiven(b.credentials)
      checkTarget(t.kind, b.name ?? t.name, b.config ?? t.config, b.credentials, {
        enabled: b.enabled ?? t.enabled,
        hasCredentials: stillHas,
      })
      if (b.name !== undefined && l7(c.s).targets.some((x) => x.name === b.name && x.id !== t.id))
        throw problem('already_exists', 'A target with this name exists.')
      if (b.name !== undefined) t.name = b.name
      if (b.enabled !== undefined) t.enabled = b.enabled
      if (b.config !== undefined) t.config = b.config
      if (b.credentials !== undefined) t.credentialsSet = credentialsGiven(b.credentials)
      return ok(structuredClone(t))
    },
    deleteLedgerAnchorTarget: (c) => {
      refuseWrites()
      const l = l7(c.s)
      const t = target(c.s, c)
      l.targets = l.targets.filter((x) => x.id !== t.id)
      for (const a of l.anchors) if (a.targetId === t.id) a.targetId = null
      return noContent()
    },
    testLedgerAnchorTarget: (c) => {
      refuseWrites()
      const t = target(c.s, c)
      t.lastTest = testResult(t)
      return ok(structuredClone(t.lastTest))
    },
    getLedgerAggregate: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      if (!published(c.s, rec))
        throw problem('ledger_not_published', 'This database has no published aggregate.')
      const raw = c.url.searchParams.get('size')
      const size = raw === null ? AGGREGATE_SIZE : Number(raw)
      if (!Number.isInteger(size) || size < 1 || size > AGGREGATE_SIZE)
        throw problem('not_found', 'No aggregate of this size was published.')
      return ok({
        ...aggregateSummary(c.s, rec, size),
        members: aggregateMembers(c.s, rec, size),
        anchors: l7(c.s)
          .anchors.filter((a) => a.databaseName === rec.info.name && a.aggregateSize === size)
          .map(({ databaseName: _d, ...a }) => {
            void _d
            return a
          }),
        statement: l7(c.s).hostileText ?? STATEMENT,
      } satisfies S['LedgerAggregate'])
    },
    listLedgerAggregates: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const items = published(c.s, rec)
        ? Array.from({ length: AGGREGATE_SIZE }, (_, i) =>
            aggregateSummary(c.s, rec, AGGREGATE_SIZE - i),
          )
        : []
      return ok(paginate(items, c.url))
    },
    listLedgerAnchors: (c) => anchorRecords(c.s, c),
    anchorLedgerNow: (c) => {
      refuseWrites()
      const l = l7(c.s)
      const rec = db(c.s, c.params.databaseName ?? '')
      if (!published(c.s, rec))
        throw problem('ledger_not_published', 'This database has no published aggregate.')
      const enabled = l.targets.filter((t) => t.enabled)
      if (enabled.length === 0) throw problem('conflict', 'No anchor target is enabled.')
      const items = enabled.map((t) => {
        const a = {
          databaseName: rec.info.name,
          id: l.nextAnchorId++,
          targetId: t.id,
          targetName: t.name,
          kind: t.kind,
          aggregateSize: AGGREGATE_SIZE,
          state: 'pending' as const,
          attempts: 0,
          nextAttemptAt: nowIso(),
          anchoredAt: null,
          evidenceTime: null,
          lastError: null,
        }
        l.anchors.push(a)
        return a
      })
      return created({
        items: items.map(({ databaseName: _d, ...a }) => {
          void _d
          return a
        }),
      } satisfies S['LedgerAnchorList'])
    },
    retryLedgerAnchor: (c) => {
      refuseWrites()
      const a = l7(c.s).anchors.find(
        (x) => String(x.id) === c.params.anchorId && x.databaseName === c.params.databaseName,
      )
      if (!a) throw problem('not_found', 'The anchor record does not exist.')
      if (a.state !== 'failed' && a.state !== 'expired')
        throw problem('conflict', 'Only a failed or expired record can be retried.')
      a.state = 'pending'
      a.nextAttemptAt = nowIso()
      const { databaseName: _d, ...rest } = a
      void _d
      return ok(rest)
    },
    downloadLedgerAnchor: (c) => {
      const a = l7(c.s).anchors.find(
        (x) => String(x.id) === c.params.anchorId && x.databaseName === c.params.databaseName,
      )
      if (!a || a.state !== 'anchored') throw problem('not_found', 'The record has no evidence.')
      const format = c.url.searchParams.get('format') ?? 'json'
      if (format === 'json')
        return ok({ kind: a.kind, aggregateSize: a.aggregateSize, evidenceTime: a.evidenceTime })
      if (!['ots', 'tsr'].includes(format) || (format === 'ots' ? 'ots' : 'tsa') !== a.kind)
        throw problem('unprocessable', 'This kind of target has no evidence in this format.')
      return {
        status: 200,
        rawBody: new Uint8Array([1, 2, 3, 4]),
        headers: { 'Content-Type': 'application/octet-stream' },
      }
    },
    createLedgerBundle: (c) => {
      const l = l7(c.s)
      const rec = db(c.s, c.params.databaseName ?? '')
      if (!published(c.s, rec))
        throw problem('ledger_not_published', 'This database has no published aggregate.')
      const b = body<S['LedgerBundleRequest']>(c)
      if (typeof b.includeAnchors !== 'boolean')
        throw validationFailed([
          { field: 'includeAnchors', message: 'Say whether to include the anchors.' },
        ])
      const id = l.nextBundleId++
      return accepted(
        createTask(c.s, {
          kind: 'ledger.bundle.create',
          subject: { type: 'database', id: rec.info.name },
          steps: ['collect', 'pack'],
          detail: `bundle ${id}`,
          apply: () => l.bundles.push({ id, databaseName: rec.info.name, createdAt: nowIso() }),
        }),
      )
    },
    getLedgerBundle: (c) => {
      const b = l7(c.s).bundles.find(
        (x) => String(x.id) === c.params.bundleId && x.databaseName === c.params.databaseName,
      )
      if (!b) throw problem('not_found', 'The bundle does not exist.')
      return {
        status: 200,
        rawBody: new Uint8Array([0x1f, 0x8b, 8, 0]),
        headers: { 'Content-Type': 'application/gzip' },
      }
    },
    listLedgerClientKeys: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      return ok({ items: structuredClone(l7(c.s).clientKeys[rec.info.name] ?? []) })
    },
    registerLedgerClientKey: (c) => {
      const l = l7(c.s)
      const rec = db(c.s, c.params.databaseName ?? '')
      const b = body<S['RegisterLedgerClientKeyRequest']>(c)
      const errors: { field: string; message: string }[] = []
      if (!isKey32(b.publicKey))
        errors.push({ field: 'publicKey', message: 'Give 32 bytes as standard base64.' })
      if (typeof b.label !== 'string' || b.label.length < 1 || b.label.length > 100)
        errors.push({ field: 'label', message: 'Give a label of 1 to 100 characters.' })
      if (errors.length > 0) throw validationFailed(errors)
      const keys = (l.clientKeys[rec.info.name] ??= [])
      if (keys.some((k) => k.publicKey === b.publicKey))
        throw problem('already_exists', 'This key is registered.')
      const key: S['LedgerClientKey'] = {
        keyId: hex64(b.publicKey).slice(0, 8),
        publicKey: b.publicKey,
        roleName: b.roleName ?? null,
        label: b.label,
        registeredBy: c.s.session?.username ?? 'admin',
        registeredAt: nowIso(),
        revokedAt: null,
        revocationReason: null,
      }
      keys.push(key)
      return created(structuredClone(key))
    },
    revokeLedgerClientKey: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const key = (l7(c.s).clientKeys[rec.info.name] ?? []).find((k) => k.keyId === c.params.keyId)
      if (!key) throw problem('not_found', 'The key does not exist.')
      if (key.revokedAt !== null) throw problem('conflict', 'The key is already revoked.')
      const b = body<S['RevokeLedgerClientKeyRequest']>(c)
      if (typeof b.reason !== 'string' || b.reason.length < 1 || b.reason.length > 200)
        throw validationFailed([
          { field: 'reason', message: 'Give a reason of 1 to 200 characters.' },
        ])
      key.revokedAt = nowIso()
      key.revocationReason = b.reason
      return ok(structuredClone(key))
    },
    setTableSignaturePolicy: (c) => {
      const l = l7(c.s)
      const rec = db(c.s, c.params.databaseName ?? '')
      const t = rec.tables.find(
        (x) => x.schema === c.params.schemaName && x.name === c.params.tableName,
      )
      if (!t) throw problem('not_found', 'Table does not exist.')
      if (!t.ledger)
        throw problem('unprocessable', `Table ${t.schema}.${t.name} is not a ledger table.`)
      const id = `${rec.info.name}:${t.schema}.${t.name}`
      if (l.policies.includes(id)) throw problem('conflict', 'The policy is already required.')
      l.policies.push(id)
      return ok(structuredClone(t))
    },
    upgradeLedgerExtension: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      if (c.s.tasks.some((t) => t.task.state === 'running' && t.task.kind === 'ledger.upgrade'))
        throw problem('operation_in_progress', 'An upgrade is running.')
      return accepted(
        createTask(c.s, {
          kind: 'ledger.upgrade',
          subject: { type: 'database', id: rec.info.name },
          steps: groupsOf(c.s, rec).map((g) => `upgrade group ${g}`),
          apply: () => undefined,
        }),
      )
    },
    acknowledgeLedgerDivergence: (c) => {
      const l = l7(c.s)
      const rec = db(c.s, c.params.databaseName ?? '')
      const g = Number(c.params.shardGroupId)
      const b = body<S['AcknowledgeLedgerDivergenceRequest']>(c)
      const errors: { field: string; message: string }[] = []
      if (b.confirm !== rec.info.name)
        errors.push({ field: 'confirm', message: 'This is not the name of the database.' })
      if (!['restore', 'failoverLoss', 'other'].includes(b.kind))
        errors.push({ field: 'kind', message: 'Choose what happened.' })
      if (typeof b.reason !== 'string' || b.reason.length < 1 || b.reason.length > 200)
        errors.push({ field: 'reason', message: 'Give a reason of 1 to 200 characters.' })
      if (errors.length > 0) throw validationFailed(errors)
      if (!isDiverged(c.s, rec.info.name, g))
        throw problem('chain_not_diverged', 'This chain has not diverged.')
      return accepted(
        createTask(c.s, {
          kind: 'ledger.epoch.start',
          subject: { type: 'database', id: rec.info.name },
          steps: ['close the old epoch', 'open the new epoch'],
          apply: () => {
            const k = `${rec.info.name}:${g}`
            l.diverged = l.diverged.filter((x) => x !== k)
            l.epochs[k] = (l.epochs[k] ?? 1) + 1
          },
        }),
      )
    },
  }
}

/** `?phase7=today`: the 21 operations the real server does not build yet answer 422, as it does. */
export function answerAsTheServerDoesToday(s: MockState): void {
  l7(s).today = true
  for (const op of NOT_BUILT_YET) mockConfig.unsupported.add(op)
}
