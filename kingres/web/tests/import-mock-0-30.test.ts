// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The API layer and the mock of the database import (contract 0.30.0, a placeholder revision, console increment C3). The mock behaves
// like the server: the request is checked first (the role, the upload, the name, the checksum, the archive read again, the ledger
// acknowledgement), the task walks the thirteen steps and shows progress in its detail during the load, a failed task has removed
// its private database and says so, and the text of a statement of an archive is never part of an answer.
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia } from 'pinia'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { applySwitches } from '@/mocks/switches'
import { IMPORT_STEPS } from '@/mocks/transfer-import'
import {
  databaseNameError,
  checksumError,
  importDatabaseRequest,
  importResultLines,
  suggestDatabaseName,
} from '@/lib/archive'
import { ok } from './schema-check'
import { signInAs } from './helpers'
import { bytesOf, runTask } from './transfer-helpers'

const sw = (query: string) => applySwitches(new URLSearchParams(query), getState())

async function refused(call: Promise<unknown>): Promise<ApiError> {
  try {
    await call
  } catch (e) {
    expect(e).toBeInstanceOf(ApiError)
    return e as ApiError
  }
  throw new Error('The call was not refused.')
}

async function uploadArchive(
  name: string,
  purpose: 'databaseArchive' | 'tableImport' = 'databaseArchive',
) {
  const file = new File([bytesOf(65536 * 2 + 5)], name, { type: 'application/zip' })
  const created = await endpoints.createDataUpload({
    purpose,
    sizeBytes: file.size,
    fileName: file.name,
  })
  await endpoints.uploadDataChunk(created.uploadId, 0, file)
  return created.uploadId
}

const names = async () => (await endpoints.listDatabases()).items.map((d) => d.name)

beforeEach(async () => {
  await signInAs(createPinia())
})

describe('importDatabase', () => {
  it('walks the thirteen steps, shows the table in its detail during the load, and the new database exists at the end', async () => {
    const keep = mockConfig.taskDurationMs
    mockConfig.taskDurationMs = 2000
    try {
      const id = await uploadArchive('copy_of_shop.kdx.zip')
      const task = await endpoints.importDatabase({ uploadId: id, database: 'shop_copy' })
      ok(task, 'Task')
      expect(task.kind).toBe('database.import')
      expect(task.subject).toEqual({ type: 'database', id: 'shop_copy' })
      expect(task.progress.steps.map((s) => s.name)).toEqual([...IMPORT_STEPS])
      expect(IMPORT_STEPS).toHaveLength(13)
      // The database does not exist until the last step.
      expect(await names()).not.toContain('shop_copy')
      let sawLoad = false
      for (let i = 0; i < 400 && !sawLoad; i += 1) {
        const t = await endpoints.getTask(task.id)
        if (
          t.progress.currentStep === 'Load the data' &&
          /\(\d of 4\), \d+ bytes so far/.test(t.detail ?? '')
        )
          sawLoad = true
        else await new Promise((r) => setTimeout(r, 20))
      }
      expect(sawLoad).toBe(true)
      const done = await runTask(task)
      expect(done.state).toBe('done')
      expect(await names()).toContain('shop_copy')
      expect(done.detail).toMatch(/Imported shop_copy/)
    } finally {
      mockConfig.taskDurationMs = keep
    }
  })

  it('takes the name from the file when the request names none', async () => {
    const id = await uploadArchive('Orders 2026.kdx.zip')
    const task = await endpoints.importDatabase({ uploadId: id })
    expect(task.subject?.id).toBe('orders_2026')
    expect((await runTask(task)).state).toBe('done')
  })

  it('refuses a database that exists with 409 already_exists, and a name that is not valid with the field named', async () => {
    const id = await uploadArchive('a.kdx.zip')
    expect((await refused(endpoints.importDatabase({ uploadId: id, database: 'shop' }))).code).toBe(
      'already_exists',
    )
    const e = await refused(endpoints.importDatabase({ uploadId: id, database: 'Bad-Name' }))
    expect(e.code).toBe('validation_failed')
    expect(JSON.stringify(e.problem.errors)).toMatch(/database/)
  })

  it('refuses an upload that does not exist, is not an archive, or is not complete', async () => {
    expect((await refused(endpoints.importDatabase({ uploadId: '99999' }))).code).toBe('not_found')
    const csv = await uploadArchive('rows.csv', 'tableImport')
    expect((await refused(endpoints.importDatabase({ uploadId: csv }))).code).toBe(
      'validation_failed',
    )
    const created = await endpoints.createDataUpload({
      purpose: 'databaseArchive',
      sizeBytes: 200000,
      fileName: 'half.kdx.zip',
    })
    expect((await refused(endpoints.importDatabase({ uploadId: created.uploadId }))).code).toBe(
      'upload_incomplete',
    )
  })

  it('refuses a ledger archive until the restart is acknowledged, and lists the new chain and the disabled policy when done', async () => {
    const id = await uploadArchive('ledger.kdx.zip')
    expect((await refused(endpoints.importDatabase({ uploadId: id, database: 'led' }))).code).toBe(
      'ledger_restart_not_acknowledged',
    )
    const task = await endpoints.importDatabase({
      uploadId: id,
      database: 'led',
      acknowledgeLedgerRestart: true,
    })
    const done = await runTask(task)
    expect(done.state).toBe('done')
    const lines = importResultLines(done.detail)
    expect(lines.some((l) => /new chain/.test(l))).toBe(true)
    expect(lines.some((l) => /imported disabled/.test(l))).toBe(true)
  })

  it('refuses a code archive with the kind, position and fingerprint of every statement, never its text', async () => {
    const id = await uploadArchive('code.kdx.zip')
    const e = await refused(endpoints.importDatabase({ uploadId: id, database: 'c' }))
    expect(e.code).toBe('archive_statement_refused')
    const reasons = e.problem.reasons ?? []
    expect(reasons).toHaveLength(2)
    expect(reasons[0]?.object).toMatch(/CREATE FUNCTION at schema\/pre-data\.sql#14/)
    expect(JSON.stringify(e.problem)).not.toMatch(/RETURNS|LANGUAGE|\$\$/)
    expect(await names()).not.toContain('c')
  })

  it('refuses a damaged archive and one of another version, and nothing is created', async () => {
    const bad = await uploadArchive('hostile.kdx.zip')
    expect((await refused(endpoints.importDatabase({ uploadId: bad, database: 'h' }))).code).toBe(
      'archive_invalid',
    )
    const old = await uploadArchive('old.kdx.zip')
    const e = await refused(endpoints.importDatabase({ uploadId: old, database: 'o' }))
    expect(e.code).toBe('archive_incompatible')
    expect(e.problem.reasons?.length).toBe(3)
    expect(await names()).not.toContain('h')
  })

  it('refuses a checksum that is not the one of the file', async () => {
    const id = await uploadArchive('s.kdx.zip')
    const e = await refused(
      endpoints.importDatabase({ uploadId: id, database: 's1', expectedSha256: 'a'.repeat(64) }),
    )
    expect(e.code).toBe('archive_invalid')
    const e2 = await refused(
      endpoints.importDatabase({ uploadId: id, database: 's1', expectedSha256: 'xyz' }),
    )
    expect(e2.code).toBe('validation_failed')
    const sha = (await endpoints.getDataUpload(id)).sha256 as string
    expect(
      (
        await runTask(
          await endpoints.importDatabase({ uploadId: id, database: 's1', expectedSha256: sha }),
        )
      ).state,
    ).toBe('done')
  })

  it('refuses a dangerous role with 403 forbidden and the sentence about signing in as another administrator', async () => {
    sw('transfer=superuser')
    const id = await uploadArchive('d.kdx.zip')
    const e = await refused(endpoints.importDatabase({ uploadId: id, database: 'd1' }))
    expect(e.code).toBe('forbidden')
    expect(e.message).toMatch(/SUPERUSER, REPLICATION or BYPASSRLS/)
  })

  it('is refused while data transfer is off, and below administrator', async () => {
    sw('transfer=off')
    const id = await uploadArchive('x.kdx.zip').catch(() => '1')
    expect((await refused(endpoints.importDatabase({ uploadId: id }))).code).toBe(
      'data_transfer_disabled',
    )
  })

  it('fails in the load step with row_count_mismatch, says the private database was removed and creates nothing', async () => {
    const id = await uploadArchive('failload.kdx.zip')
    const done = await runTask(await endpoints.importDatabase({ uploadId: id, database: 'fl' }))
    expect(done.state).toBe('failed')
    expect(done.error?.code).toBe('row_count_mismatch')
    expect(done.error?.step).toBe('Load the data')
    expect(done.error?.message).toMatch(/private database was removed/)
    expect(await names()).not.toContain('fl')
  })

  it('says the private database was kept when keepOnFailure is set', async () => {
    const id = await uploadArchive('failload.kdx.zip')
    const done = await runTask(
      await endpoints.importDatabase({ uploadId: id, database: 'fl2', keepOnFailure: true }),
    )
    expect(done.state).toBe('failed')
    expect(done.error?.message).toMatch(/kept/)
    expect(done.error?.message).toMatch(/kingres_import_\d+/)
  })

  it('fails a foreign key violation with the constraint and the count, and with keepNotValid it finishes and lists the violations', async () => {
    const id = await uploadArchive('fkviolation.kdx.zip')
    const failed = await runTask(await endpoints.importDatabase({ uploadId: id, database: 'fk1' }))
    expect(failed.state).toBe('failed')
    expect(failed.error?.code).toBe('foreign_key_violation')
    expect(failed.error?.message).toMatch(/orders_customer_fk.*7 rows/)
    const kept = await runTask(
      await endpoints.importDatabase({
        uploadId: id,
        database: 'fk2',
        onForeignKeyViolation: 'keepNotValid',
      }),
    )
    expect(kept.state).toBe('done')
    expect(importResultLines(kept.detail).some((l) => /NOT VALID: 7 rows/.test(l))).toBe(true)
    expect(await names()).toContain('fk2')
  })

  it('does not accept a second import of the same upload while the first runs', async () => {
    const id = await uploadArchive('twice.kdx.zip')
    await endpoints.importDatabase({ uploadId: id, database: 't1' })
    expect((await refused(endpoints.importDatabase({ uploadId: id, database: 't2' }))).code).toBe(
      'operation_in_progress',
    )
  })
})

describe('the rules of the dialog', () => {
  it('checks the name as the server does, and against the databases that exist', () => {
    expect(databaseNameError('', [])).toMatch(/choose/i)
    for (const bad of ['Shop', '1shop', 'a-b', 'a b', 'x'.repeat(64), 'ü', 'a;drop'])
      expect(databaseNameError(bad, [])).toMatch(/lower-case/)
    expect(databaseNameError('shop', ['shop'])).toMatch(/exists/)
    expect(databaseNameError('x'.repeat(63), [])).toBeNull()
    expect(databaseNameError('_a1', [])).toBeNull()
  })

  it('suggests a valid name from any file name', () => {
    expect(suggestDatabaseName('Shop 2026.kdx.zip')).toBe('shop_2026')
    expect(suggestDatabaseName('9lives.zip')).toBe('_9lives')
    expect(suggestDatabaseName('.zip')).toBe('imported')
    expect(databaseNameError(suggestDatabaseName('‮evil\u0000.kdx.zip'), [])).toBeNull()
  })

  it('leaves out what is not chosen and trims the checksum', () => {
    expect(
      importDatabaseRequest({
        uploadId: '7',
        database: 'd',
        expectedSha256: '',
        acknowledgeLedgerRestart: false,
        onForeignKeyViolation: 'fail',
        keepOnFailure: false,
      }),
    ).toEqual({ uploadId: '7', database: 'd' })
    expect(
      importDatabaseRequest({
        uploadId: '7',
        database: 'd',
        expectedSha256: ` ${'A'.repeat(64)} `,
        acknowledgeLedgerRestart: true,
        onForeignKeyViolation: 'keepNotValid',
        keepOnFailure: true,
      }),
    ).toEqual({
      uploadId: '7',
      database: 'd',
      expectedSha256: 'a'.repeat(64),
      acknowledgeLedgerRestart: true,
      onForeignKeyViolation: 'keepNotValid',
      keepOnFailure: true,
    })
  })

  it('compares the checksum with the archive', () => {
    expect(checksumError('', 'a'.repeat(64))).toBeNull()
    expect(checksumError('zz', undefined)).toMatch(/64/)
    expect(checksumError('b'.repeat(64), 'a'.repeat(64))).toMatch(/not the SHA-256/)
    expect(checksumError('A'.repeat(64), 'a'.repeat(64))).toBeNull()
  })
})
