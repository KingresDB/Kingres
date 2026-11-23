// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The `?transfer=` switches of the mock API (contract 0.19.0): the states of the data transfer that are hard to reach
// by hand. `switches.ts` lists them; the tests call `applyTransferSwitch` with the same words.
import type { MockState } from './state'
import { KiB, MiB, transferSwitches } from './transfer-data'
import { nowIso } from './state'
import { hex64 } from './ledger-data'
import { sha256Hex } from './sha256'

/** A ready export with a small file of its own, for the demo of the Transfers panel. */
function seedExport(
  s: MockState,
  state: 'ready' | 'building' | 'expired',
  table: string,
  hoursLeft: number,
): void {
  const text = 'id,name\n1,one\n2,two\n3,three\n'
  const content = new TextEncoder().encode(text)
  const id = String(s.transfer.nextExportId++)
  s.transfer.exports.push({
    creator: s.session?.username ?? 'admin',
    content: state === 'ready' ? content : null,
    meta: {
      exportId: id,
      kind: 'tableExport',
      state,
      database: 'shop',
      schema: 'public',
      table,
      sizeBytes: state === 'building' ? null : content.length,
      sha256: state === 'building' ? null : state === 'ready' ? sha256Hex(content) : hex64(id),
      rows: state === 'building' ? null : 3,
      consistency:
        state === 'building'
          ? null
          : [
              {
                table: `public.${table}`,
                level: table === 'orders' ? 'per-group' : 'single-snapshot',
                groups: table === 'orders' ? 3 : 1,
              },
            ],
      rlsApplied: false,
      spreadsheetSafe: state === 'building' ? null : table !== 'customers',
      format: 'csv',
      taskId: 1,
      createdAt: nowIso(),
      expiresAt: new Date(Date.now() + hoursLeft * 3_600_000).toISOString(),
      downloadedAt: null,
    },
  })
}

export function applyTransferSwitch(entry: string, s: MockState): void {
  const dt = s.settings.dataTransfer
  if (entry === 'off' && dt) dt.enabled = false
  if (entry === 'insecure') transferSwitches.insecure = true
  if (entry === 'full' && dt) dt.stagingQuotaBytes = MiB
  if (entry === 'small') transferSwitches.chunkBytesMax = 128 * KiB
  if (entry === 'session') transferSwitches.loseSessionAtChunk = transferSwitches.chunkRequests + 1
  if (entry === 'cut') {
    transferSwitches.breakChunkAfterBytes = 100 * KiB
    transferSwitches.breakDownloadAfterBytes = KiB
  }
  if (entry === 'busy') transferSwitches.busyWriterChunks = 2
  if (entry === 'cut400') {
    transferSwitches.breakChunkAfterBytes = 100 * KiB
    transferSwitches.cutAnswers400 = true
  }
  if (entry === 'short') transferSwitches.truncateDownloadAt = 8
  if (entry === 'lobs') transferSwitches.largeObjects = true
  if (entry === 'unreadable') transferSwitches.unreadable = true
  if (entry === 'dbfail') transferSwitches.dbExportFails = true
  if (entry === 'changed') transferSwitches.schemaChanges = true
  if (entry === 'exports') {
    seedExport(s, 'ready', 'orders', 20)
    seedExport(s, 'building', 'customers', 24)
    seedExport(s, 'expired', 'notes', -1)
  }
}
