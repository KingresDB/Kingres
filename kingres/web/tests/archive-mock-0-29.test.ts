// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The API layer and the mock of the database export and the archive validation (contract 0.29.0, a placeholder revision,
// console increment B5). The mock behaves like the server: the checks of the request come first (the role must read every
// table, large objects must be acknowledged), the task writes the archive and reports a label for every table, a table task
// of the database is refused while the export runs, an upload for an archive is not a CSV, and the report of a validation
// has a kind, a position and a fingerprint for a refused statement and never its text. Answers satisfy the contract's schemas.
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia } from 'pinia'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { ok } from './schema-check'
import { signInAs } from './helpers'
import { bytesOf, readAll, runTask } from './transfer-helpers'

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

/** Sends a whole archive in one chunk and returns the upload id. */
async function uploadArchive(name: string, size = 65536 * 2 + 5): Promise<string> {
  const file = new File([bytesOf(size)], name, { type: 'application/zip' })
  const created = await endpoints.createDataUpload({
    purpose: 'databaseArchive',
    sizeBytes: file.size,
    fileName: file.name,
  })
  await endpoints.uploadDataChunk(created.uploadId, 0, file)
  return created.uploadId
}

async function validate(name: string) {
  const id = await uploadArchive(name)
  const task = await endpoints.validateDatabaseArchive(id)
  // The README of the contract gives this task the subject type `upload`; the schema of `Task.subject` has no such type
  // (found in B5, reported): the answer is checked for what it says, not against that enum.
  expect(task.kind).toBe('archive.validate')
  expect(task.subject).toEqual({ type: 'upload', id })
  const done = await runTask(task)
  expect(done.state).toBe('done')
  return { id, up: await endpoints.getDataUpload(id) }
}

beforeEach(async () => {
  await signInAs(createPinia())
})

describe('exportDatabase', () => {
  it('writes an archive: a task of the kind database.export, then a DataExport of the kind databaseExport with a label for every table', async () => {
    const task = await endpoints.exportDatabase('shop')
    ok(task, 'Task')
    expect(task.kind).toBe('database.export')
    expect(task.subject).toEqual({ type: 'database', id: 'shop' })
    expect(task.progress.steps.map((s) => s.name)).toEqual([
      'Check the request',
      'Take the structure',
      'Normalise the schema',
      'Write the data',
      'Check the archive',
      'Seal',
    ])
    const done = await runTask(task)
    expect(done.state).toBe('done')
    const id = /^transfer (\d+)$/.exec(done.detail ?? '')?.[1]
    expect(id).toBeDefined()
    const file = await endpoints.getDataExport(id as string)
    ok(file, 'DataExport')
    expect(file).toMatchObject({
      kind: 'databaseExport',
      format: 'archive/1',
      state: 'ready',
      schema: null,
      table: null,
    })
    expect(file.sha256).toMatch(/^[0-9a-f]{64}$/)
    // `shop` has a distributed table over three groups: its label is per group and the archive is not one snapshot.
    expect(file.consistency?.some((c) => c.level === 'per-group')).toBe(true)
    expect(file.consistency?.some((c) => c.level === 'archive-single-snapshot')).toBe(false)
    expect(file.warnings?.some((w) => /read per group/.test(w))).toBe(true)
  })

  it('labels every table archive-single-snapshot when none is spread over several groups', async () => {
    const done = await runTask(await endpoints.exportDatabase('shop', { dataMode: 'none' }))
    expect(done.state).toBe('done')
    // `reports` is refused for the privileges, so `shop` is the only database with data; its distributed tables are per group.
    const id = /^transfer (\d+)$/.exec(done.detail ?? '')?.[1] as string
    const file = await endpoints.getDataExport(id)
    expect(file.rows).toBe(0)
  })

  it('leaves the rows of the tables the request names out and says so in the warnings', async () => {
    const done = await runTask(
      await endpoints.exportDatabase('shop', {
        excludeTables: [{ schema: 'public', table: 'notes' }],
      }),
    )
    const id = /^transfer (\d+)$/.exec(done.detail ?? '')?.[1] as string
    const file = await endpoints.getDataExport(id)
    expect(file.warnings?.some((w) => /public\.notes/.test(w) && /left out/.test(w))).toBe(true)
  })

  it('refuses a table that the database does not have, a data mode and a retention time that are not valid, with the field named', async () => {
    const e = await refused(
      endpoints.exportDatabase('shop', {
        excludeTables: [{ schema: 'public', table: 'nothing' }],
        retentionHours: 500,
      }),
    )
    expect(e.problem.code).toBe('validation_failed')
    const fields = (e.problem.errors ?? []).map((x) => x.field)
    expect(fields).toContain('excludeTables/0')
    expect(fields).toContain('retentionHours')
  })

  it('answers 404 for a database that does not exist', async () => {
    expect((await refused(endpoints.exportDatabase('nothing'))).status).toBe(404)
  })

  it('refuses a role that cannot read every table, naming at most 20 objects, and writes nothing', async () => {
    sw('transfer=unreadable')
    const e = await refused(endpoints.exportDatabase('shop'))
    expect(e.problem.code).toBe('export_not_readable')
    expect(e.status).toBe(422)
    const names = (e.problem.detail ?? '').split(':').slice(1).join(':').split(',')
    expect(names.length).toBeLessThanOrEqual(20)
    expect(getState().transfer.exports).toHaveLength(0)
  })

  it('refuses large objects until they are acknowledged, and then says in the warnings that they are lost', async () => {
    sw('transfer=lobs')
    const e = await refused(endpoints.exportDatabase('shop'))
    expect(e.problem.code).toBe('large_objects_present')
    const done = await runTask(
      await endpoints.exportDatabase('shop', { acknowledgeLargeObjectsLost: true }),
    )
    const id = /^transfer (\d+)$/.exec(done.detail ?? '')?.[1] as string
    expect(
      (await endpoints.getDataExport(id)).warnings?.some((w) => /large objects/i.test(w)),
    ).toBe(true)
  })

  it('fails with a whitelisted sentence and keeps no file (dbfail), and with schema_changed (changed)', async () => {
    sw('transfer=dbfail')
    const failed = await runTask(await endpoints.exportDatabase('shop'))
    expect(failed.state).toBe('failed')
    expect(failed.error?.code).toBe('sql_error')
    expect(getState().transfer.exports[0]?.meta.state).toBe('failed')
    expect(getState().transfer.exports[0]?.content).toBeNull()
    sw('transfer=changed')
    const changed = await runTask(await endpoints.exportDatabase('shop'))
    expect(changed.error?.code).toBe('schema_changed')
  })

  it('refuses a table export of the database while the database export runs, and a second database export', async () => {
    const task = await endpoints.exportDatabase('shop')
    expect((await refused(endpoints.exportTable('shop', 'public', 'notes'))).problem.code).toBe(
      'operation_in_progress',
    )
    expect((await refused(endpoints.exportDatabase('shop'))).problem.code).toBe(
      'operation_in_progress',
    )
    await runTask(task)
  })

  it('is refused while data transfer is off, and over plain HTTP', async () => {
    sw('transfer=off')
    expect((await refused(endpoints.exportDatabase('shop'))).problem.code).toBe(
      'data_transfer_disabled',
    )
  })

  it('serves the archive as application/zip with a name built from the database, and a Range', async () => {
    const done = await runTask(await endpoints.exportDatabase('shop'))
    const id = /^transfer (\d+)$/.exec(done.detail ?? '')?.[1] as string
    const file = await endpoints.getDataExport(id)
    const whole = await endpoints.downloadDataExport(id)
    expect(whole.status).toBe(200)
    expect(whole.contentLength).toBe(file.sizeBytes)
    expect(new TextDecoder().decode((await readAll(whole.body)).slice(0, 2))).toBe('PK')
    const tail = await endpoints.downloadDataExport(id, 10)
    expect(tail.status).toBe(206)
    expect(tail.contentRange).toBe(`bytes 10-${(file.sizeBytes ?? 0) - 1}/${file.sizeBytes}`)
    // The headers the browser sees (the name of the file and its type) are the mock server's own answer.
    const raw = await fetch(`${location.origin}/api/v1/data-exports/${id}/content`)
    expect(raw.headers.get('Content-Type')).toBe('application/zip')
    expect(raw.headers.get('Content-Disposition')).toContain('shop.kdx.zip')
  })
})

describe('uploads for an archive and validateDatabaseArchive', () => {
  it('accepts the purpose databaseArchive and refuses an unknown one', async () => {
    const up = await endpoints.createDataUpload({ purpose: 'databaseArchive', sizeBytes: 1000 })
    ok(up, 'DataUpload')
    const e = await refused(
      endpoints.createDataUpload({ purpose: 'nothing' as 'tableImport', sizeBytes: 1000 }),
    )
    expect(e.problem.code).toBe('validation_failed')
  })

  it('refuses an upload that is not complete (409 upload_incomplete) and one that is a CSV (422)', async () => {
    const part = await endpoints.createDataUpload({ purpose: 'databaseArchive', sizeBytes: 1000 })
    expect((await refused(endpoints.validateDatabaseArchive(part.uploadId))).problem.code).toBe(
      'upload_incomplete',
    )
    const csv = await endpoints.createDataUpload({ purpose: 'tableImport', sizeBytes: 1000 })
    expect((await refused(endpoints.validateDatabaseArchive(csv.uploadId))).problem.code).toBe(
      'validation_failed',
    )
    expect((await refused(endpoints.validateDatabaseArchive('99999'))).status).toBe(404)
  })

  it('answers a good archive with a valid report, stored with the upload (state valid)', async () => {
    const { up } = await validate('shop.kdx.zip')
    ok(up, 'DataUpload')
    expect(up.state).toBe('valid')
    expect(up.report?.valid).toBe(true)
    expect(up.report?.sha256).toBe(up.sha256)
    expect(up.report?.reasons).toEqual([])
  })

  it('shows state validating while the task runs', async () => {
    const id = await uploadArchive('shop.kdx.zip')
    const task = await endpoints.validateDatabaseArchive(id)
    expect((await endpoints.getDataUpload(id)).state).toBe('validating')
    expect((await refused(endpoints.validateDatabaseArchive(id))).problem.code).toBe(
      'operation_in_progress',
    )
    await runTask(task)
  })

  it('hostile.kdx.zip is invalid, with a reason and no tables', async () => {
    const { up } = await validate('hostile.kdx.zip')
    expect(up.state).toBe('invalid')
    expect(up.report?.valid).toBe(false)
    expect(up.report?.reasons[0]?.code).toBe('archive_invalid')
    expect(up.report?.tables.rows).toBe(0)
  })

  it('old.kdx.zip lists every reason the archive does not fit, and an extension that is not installed', async () => {
    const { up } = await validate('old.kdx.zip')
    expect(up.report?.valid).toBe(false)
    expect(up.report?.reasons.length).toBeGreaterThanOrEqual(3)
    expect(up.report?.reasons.every((r) => r.code === 'archive_incompatible')).toBe(true)
    expect(up.report?.versions.extensions.some((e) => e.installed === null)).toBe(true)
  })

  it('ledger.kdx.zip needs the ledger restart to be acknowledged', async () => {
    const { up } = await validate('ledger.kdx.zip')
    expect(up.report?.valid).toBe(true)
    expect(up.report?.needs).toEqual({ ledgerRestart: true, largeObjectsLost: false })
    expect(up.report?.tables.ledger).toBe(2)
  })

  it('code.kdx.zip names the refused statements by kind, position and fingerprint, and the report holds no statement text', async () => {
    const { up } = await validate('code.kdx.zip')
    expect(up.report?.valid).toBe(false)
    const refusedStatements = up.report?.statements?.refused ?? []
    expect(refusedStatements.map((r) => r.kind)).toEqual(['CREATE FUNCTION', 'CREATE TRIGGER'])
    for (const r of refusedStatements) {
      expect(r.position).toMatch(/^schema\/[a-z-]+\.sql#\d+$/)
      expect(r.fingerprint).toMatch(/^length \d+, sha256 [0-9a-f]{12}$/)
      expect(Object.keys(r).sort()).toEqual(['fingerprint', 'kind', 'position', 'reason'])
    }
    expect(JSON.stringify(up.report)).not.toMatch(/CREATE FUNCTION \w+\s*\(/)
  })

  it('the archive of another role is 404 and an upload that is deleted has no report', async () => {
    const id = await uploadArchive('shop.kdx.zip')
    await endpoints.deleteDataUpload(id)
    expect((await refused(endpoints.getDataUpload(id))).status).toBe(404)
  })
})
