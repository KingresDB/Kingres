// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The words and the forms of the Anchoring tab (design 6.2 to 6.9, 8.5), without a page: the state of a target and of a
// record in words, the formats of evidence of each kind, the checks of a form for each of the three kinds, what is sent
// (only what was filled in, credentials only when typed), how a message of the server finds its field, the confirmation
// of the lock mode that cannot be undone, and the age of the newest anchor.
import { describe, expect, it } from 'vitest'
import type { LedgerAnchorTarget } from '@/api/types'
import {
  buildConfig,
  buildCredentials,
  canRetry,
  checkMeta,
  configSummary,
  evidenceFormats,
  extraMembers,
  fieldOfServerError,
  hasEvidence,
  initialValues,
  needsComplianceConfirmation,
  PUBLIC_CALENDARS,
  recordStateMeta,
  retentionEnds,
  targetStateMeta,
  testSummary,
  validateForm,
  valuesFromConfig,
  windowText,
} from '@/lib/ledger-anchor'

const target = (over: Partial<LedgerAnchorTarget> = {}): LedgerAnchorTarget => ({
  id: 1,
  kind: 'worm',
  name: 'archive',
  enabled: true,
  config: { region: 'eu-west-1', bucket: 'ledger-anchors', mode: 'governance' },
  credentialsSet: true,
  lastTest: null,
  newestAnchoredAt: null,
  state: 'ok',
  ...over,
})

describe('the state of a record and of a target in words', () => {
  it('has words for each of the six record states, and says what a person must do', () => {
    const labels = (
      ['pending', 'submitted', 'anchored', 'failed', 'superseded', 'expired'] as const
    ).map((s) => recordStateMeta(s).label)
    expect(labels).toEqual([
      'Waiting to be sent',
      'Sent, not final',
      'Anchored',
      'Failed',
      'Replaced by a newer checkpoint',
      'Never confirmed',
    ])
    expect(recordStateMeta('failed').tone).toBe('danger')
    expect(recordStateMeta('expired').tone).toBe('danger')
    expect(recordStateMeta('anchored').tone).toBe('ok')
  })
  it('shows a state of a newer server as sent, never as a known one', () => {
    expect(recordStateMeta('weird').label).toBe('weird')
    expect(targetStateMeta('weird').label).toBe('weird')
  })
  it('target states: none is neutral, not green', () => {
    expect(targetStateMeta('none')).toMatchObject({
      tone: 'neutral',
      label: 'Nothing anchored yet',
    })
    expect(targetStateMeta('ok').tone).toBe('ok')
    expect(targetStateMeta('behind').tone).toBe('warn')
    expect(targetStateMeta('failed').tone).toBe('danger')
  })
  it('only a failed or expired record can be retried and only an anchored one has evidence', () => {
    for (const s of ['failed', 'expired'] as const) expect(canRetry({ state: s })).toBe(true)
    for (const s of ['pending', 'submitted', 'anchored', 'superseded'] as const)
      expect(canRetry({ state: s })).toBe(false)
    expect(hasEvidence({ state: 'anchored' })).toBe(true)
    expect(hasEvidence({ state: 'submitted' })).toBe(false)
  })
  it('evidence: JSON for all, .ots for OpenTimestamps, .tsr for a time-stamp authority', () => {
    expect(evidenceFormats('worm')).toEqual(['json'])
    expect(evidenceFormats('ots')).toEqual(['json', 'ots'])
    expect(evidenceFormats('tsa')).toEqual(['json', 'tsr'])
  })
})

describe('the result of a test', () => {
  it('one word for the whole test and a word for each check', () => {
    expect(testSummary({ ok: true, checks: [{ name: 'a', status: 'ok', detail: '' }] }).label).toBe(
      'Test passed',
    )
    expect(
      testSummary({ ok: true, checks: [{ name: 'a', status: 'warning', detail: '' }] }).label,
    ).toBe('Passed with warnings')
    expect(
      testSummary({ ok: false, checks: [{ name: 'a', status: 'failed', detail: '' }] }).label,
    ).toBe('Test failed')
    expect(checkMeta('ok').label).toBe('OK')
    expect(checkMeta('warning').label).toBe('Warning')
    expect(checkMeta('failed').label).toBe('Failed')
    expect(checkMeta('new').label).toBe('new')
  })
})

describe('the checks of a form', () => {
  const worm = () => ({
    ...initialValues('worm'),
    region: 'eu-west-1',
    bucket: 'ledger-anchors',
  })
  const creds = { accessKeyId: 'AKIA', secretAccessKey: 's3cret' }

  it('a good WORM form has no problem; the first target needs the access key', () => {
    expect(validateForm('worm', 'archive', worm(), creds, true)).toEqual({})
    const e = validateForm('worm', 'archive', worm(), {}, true)
    expect(e['credentials.accessKeyId']).toBe('Access key id is required.')
    expect(e['credentials.secretAccessKey']).toBe('Secret access key is required.')
    // An edit keeps what is stored when the fields stay empty.
    expect(validateForm('worm', 'archive', worm(), {}, false)).toEqual({})
  })
  it('a name of 1 to 63 characters', () => {
    expect(validateForm('worm', '', worm(), creds, true).name).toBeTruthy()
    expect(validateForm('worm', 'x'.repeat(64), worm(), creds, true).name).toBeTruthy()
    expect(validateForm('worm', 'x'.repeat(63), worm(), creds, true).name).toBeUndefined()
  })
  it('WORM: region and bucket are needed, an address without TLS needs its box, ranges are checked', () => {
    const e = validateForm('worm', 'a', initialValues('worm'), creds, true)
    expect(e.region).toBe('Region is required.')
    expect(e.bucket).toBe('Bucket is required.')
    expect(
      validateForm('worm', 'a', { ...worm(), endpoint: 'http://minio:9000' }, creds, true).endpoint,
    ).toMatch(/without TLS needs the box/)
    expect(
      validateForm(
        'worm',
        'a',
        { ...worm(), endpoint: 'http://minio:9000', allowInsecure: true },
        creds,
        true,
      ).endpoint,
    ).toBeUndefined()
    expect(
      validateForm('worm', 'a', { ...worm(), endpoint: 'https://s3.example.com/path' }, creds, true)
        .endpoint,
    ).toBeTruthy()
    expect(
      validateForm('worm', 'a', { ...worm(), retentionDays: '0' }, creds, true).retentionDays,
    ).toBe('The retention must be a whole number from 1 to 36500.')
    expect(
      validateForm('worm', 'a', { ...worm(), retentionDays: '36501' }, creds, true).retentionDays,
    ).toBeTruthy()
    expect(
      validateForm('worm', 'a', { ...worm(), bucket: 'a..b' }, creds, true).bucket,
    ).toBeTruthy()
    expect(
      validateForm('worm', 'a', { ...worm(), keyPrefix: 'a/../b' }, creds, true).keyPrefix,
    ).toBeTruthy()
  })
  it('TSA: the address and the certificates are needed; the user name and the password go together', () => {
    const base = {
      ...initialValues('tsa'),
      url: 'https://tsa.example.com/tsr',
      caBundlePath: '/etc/tsa.pem',
    }
    expect(validateForm('tsa', 't', base, {}, true)).toEqual({})
    const e = validateForm('tsa', 't', initialValues('tsa'), {}, true)
    expect(e.url).toBe('Address of the authority is required.')
    expect(e.caBundlePath).toBe('Certificates of the authority is required.')
    expect(
      validateForm('tsa', 't', { ...base, url: 'https://t/x?key=1' }, {}, true).url,
    ).toBeTruthy()
    expect(validateForm('tsa', 't', { ...base, policyOid: 'abc' }, {}, true).policyOid).toBeTruthy()
    expect(
      validateForm('tsa', 't', { ...base, signerKeySha256: 'AB' }, {}, true).signerKeySha256,
    ).toBeTruthy()
    expect(validateForm('tsa', 't', base, { username: 'u' }, true)['credentials.password']).toBe(
      'Give the password for this user name.',
    )
    expect(validateForm('tsa', 't', base, { password: 'p' }, true)['credentials.username']).toBe(
      'Give the user name for this password.',
    )
    expect(
      validateForm('tsa', 't', { ...base, timeoutSeconds: '301' }, {}, true).timeoutSeconds,
    ).toBeTruthy()
  })
  it('OpenTimestamps: calendars are needed, https, no repeats, the minimum cannot exceed the list', () => {
    const base = { ...initialValues('ots'), calendars: PUBLIC_CALENDARS.join('\n') }
    expect(validateForm('ots', 'o', base, {}, true)).toEqual({})
    expect(validateForm('ots', 'o', initialValues('ots'), {}, true).calendars).toBe(
      'Calendars is required.',
    )
    expect(validateForm('ots', 'o', { ...base, calendars: 'ftp://x' }, {}, true).calendars).toBe(
      'Each line must be an https address.',
    )
    expect(
      validateForm('ots', 'o', { ...base, calendars: 'http://x.example' }, {}, true).calendars,
    ).toMatch(/needs the box/)
    expect(
      validateForm(
        'ots',
        'o',
        { ...base, calendars: 'https://x.example\nhttps://X.example/' },
        {},
        true,
      ).calendars,
    ).toBe('A calendar is listed twice.')
    expect(
      validateForm('ots', 'o', { ...base, minCalendars: '4' }, {}, true).minCalendars,
    ).toBeTruthy()
    expect(PUBLIC_CALENDARS.every((c) => c.startsWith('https://'))).toBe(true)
  })
})

describe('what is sent', () => {
  it('only what was filled in; a number is a number, a box is true only when ticked', () => {
    const v = {
      ...initialValues('worm'),
      region: ' eu-west-1 ',
      bucket: 'b-1',
      retentionDays: '365',
      pathStyle: 'yes',
      allowInsecure: false,
      endpoint: '',
    }
    expect(buildConfig('worm', v)).toEqual({
      region: 'eu-west-1',
      bucket: 'b-1',
      keyPrefix: 'kingres-anchors',
      mode: 'compliance',
      retentionDays: 365,
      pathStyle: true,
      ifNoneMatch: 'auto',
    })
    expect(buildConfig('worm', { ...v, allowInsecure: true }).allowInsecure).toBe(true)
  })
  it('OpenTimestamps sends the lines as a list', () => {
    expect(
      buildConfig('ots', {
        ...initialValues('ots'),
        calendars: ' https://a.example \n\nhttps://b.example\n',
      }),
    ).toEqual({ calendars: ['https://a.example', 'https://b.example'] })
  })
  it('a member the console does not know is kept when a target is edited', () => {
    const c = { region: 'r', bucket: 'bbb', futureSetting: { x: 1 } }
    expect(extraMembers('worm', c)).toEqual({ futureSetting: { x: 1 } })
    const out = buildConfig('worm', valuesFromConfig('worm', c), extraMembers('worm', c))
    expect(out.futureSetting).toEqual({ x: 1 })
    expect(out.bucket).toBe('bbb')
  })
  it('a form made from a configuration and sent again is the same configuration', () => {
    const c = {
      endpoint: 'https://s3.example.com',
      region: 'us-east-1',
      bucket: 'ledger',
      keyPrefix: 'kingres-anchors',
      mode: 'governance',
      retentionDays: 30,
      pathStyle: false,
      ifNoneMatch: 'on',
      allowInsecure: true,
    }
    expect(buildConfig('worm', valuesFromConfig('worm', c))).toEqual(c)
  })
  it('credentials: only the fields that were typed, or nothing at all', () => {
    expect(buildCredentials('worm', {})).toBeUndefined()
    expect(buildCredentials('worm', { accessKeyId: '', secretAccessKey: '' })).toBeUndefined()
    expect(
      buildCredentials('worm', { accessKeyId: 'A', secretAccessKey: 'S', sessionToken: '' }),
    ).toEqual({
      accessKeyId: 'A',
      secretAccessKey: 'S',
    })
    expect(buildCredentials('ots', { anything: 'x' })).toBeUndefined()
  })
})

describe("the server's messages find their field", () => {
  it('config.bucket and /config/bucket are the field bucket; credentials and name keep their own', () => {
    expect(fieldOfServerError('config.bucket')).toBe('bucket')
    expect(fieldOfServerError('/config/bucket')).toBe('bucket')
    expect(fieldOfServerError('/credentials/secretAccessKey')).toBe('credentials.secretAccessKey')
    expect(fieldOfServerError('name')).toBe('name')
  })
})

describe('compliance mode needs the word typed', () => {
  const values = (mode: string) => ({ ...initialValues('worm'), mode })
  it('a new WORM target in compliance mode, and a change to compliance; not governance, not other kinds', () => {
    expect(needsComplianceConfirmation('worm', values('compliance'), null)).toBe(true)
    expect(needsComplianceConfirmation('worm', values('governance'), null)).toBe(false)
    expect(needsComplianceConfirmation('tsa', initialValues('tsa'), null)).toBe(false)
    expect(needsComplianceConfirmation('worm', values('compliance'), target())).toBe(true)
    // Already in compliance mode: nothing new is locked by saving the same mode again.
    expect(
      needsComplianceConfirmation(
        'worm',
        values('compliance'),
        target({ config: { mode: 'compliance' } }),
      ),
    ).toBe(false)
  })
  it('shows the date on which a lock that starts now would end', () => {
    expect(retentionEnds(10, Date.parse('2026-11-19T10:00:00Z'))).toBe('29 Nov 2026, 10:00:00 UTC')
  })
})

describe('how old the newest anchor is', () => {
  const now = Date.parse('2026-11-19T12:00:00Z')
  it('in the unit that fits, with the time itself', () => {
    expect(windowText(null, now)).toBe('Nothing anchored yet')
    expect(windowText('2026-11-19T11:59:30Z', now)).toBe(
      '30 seconds old (19 Nov 2026, 11:59:30 UTC)',
    )
    expect(windowText('2026-11-19T11:20:00Z', now)).toMatch(/^40 minutes old/)
    expect(windowText('2026-11-19T05:00:00Z', now)).toMatch(/^7 hours old/)
    expect(windowText('2026-11-10T12:00:00Z', now)).toMatch(/^9 days old/)
  })
})

describe('the line under the name of a target', () => {
  it('names the store and the mode, the address, or the number of calendars; never a secret', () => {
    expect(
      configSummary({
        kind: 'worm',
        config: { endpoint: 'https://s3.x', bucket: 'b', accessKeyId: 'LEAK' },
      }),
    ).toBe('https://s3.x / b, compliance mode')
    expect(configSummary({ kind: 'tsa', config: { url: 'https://tsa.x/tsr' } })).toBe(
      'https://tsa.x/tsr',
    )
    expect(configSummary({ kind: 'ots', config: { calendars: ['a', 'b'] } })).toBe('2 calendars')
    expect(configSummary({ kind: 'ots', config: { calendars: ['a'] } })).toBe('1 calendar')
    expect(
      configSummary({ kind: 'worm', config: { bucket: 'b', secretAccessKey: 'LEAK' } }),
    ).not.toContain('LEAK')
  })
})
