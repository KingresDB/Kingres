// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Words for the sharding mode of a database (contract 0.15.0): `row` (a table is split into buckets by a column),
// `schema` (a schema lives on one server group) or `both`. A server before 0.15.0 sends no mode: it is read as `row`.
import type { Database } from '@/api/types'

export type ShardingMode = 'row' | 'schema' | 'both'

export const MODE_LABEL: Record<ShardingMode, string> = {
  row: 'Rows',
  schema: 'Schemas',
  both: 'Both',
}

/** One sentence for each mode, as the design words them. */
export const MODE_SENTENCE: Record<ShardingMode, string> = {
  row: 'a table is split into buckets by a column.',
  schema:
    'each schema (a tenant or a service) lives on one server group, and every table in it with it.',
  both: 'choose per table or per schema.',
}

export const DISTRIBUTE_NEW_SENTENCE =
  'Every schema created in this database is placed on a server group and kept there. Tables in it can use any constraint or query. Schemas created by an extension, public and system schemas are not distributed.'

/** The mode of a database as read back; a missing or unknown mode (an older server) is `row`. */
export function modeOf(sharding: Database['sharding']): ShardingMode {
  const mode = sharding.mode
  return mode === 'schema' || mode === 'both' ? mode : 'row'
}

/** The label of a mode; a mode from a newer server is shown as sent. */
export function modeLabel(mode: string): string {
  return (MODE_LABEL as Record<string, string | undefined>)[mode] ?? mode
}

/** The modes a dialog offers, in the order the server gave them; only the three the panel knows. */
export function offeredModes(modes: readonly string[] | undefined): ShardingMode[] {
  return (modes ?? []).filter(
    (m): m is ShardingMode => m === 'row' || m === 'schema' || m === 'both',
  )
}

/** Whether a mode has hash buckets (and so a bucket count). */
export const hasBuckets = (mode: ShardingMode): boolean => mode !== 'schema'

/** Whether a mode distributes schemas by default (the checkbox "Distribute new schemas automatically"). */
export const canDistributeNew = (mode: ShardingMode): boolean => mode !== 'row'

/** The default of that checkbox for a mode: on for Schemas, off for Both (the console offers it so). */
export const distributeNewDefault = (mode: ShardingMode): boolean => mode === 'schema'
