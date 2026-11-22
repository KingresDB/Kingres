// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Why the Export CSV and Import CSV actions of a table cannot be used right now, in words, or an empty text. The role is
// not part of it: `GateButton` says that. What is here is the cluster setting and an unfinished task on the table.
import { computed, toValue, type MaybeRefOrGetter } from 'vue'
import { useClusterSettings, useTasks } from '@/api/queries'

/** The subject of a table task (`database/schema.table`, contract 0.14.0 and 0.19.0). */
export const tableSubject = (database: string, schema: string, table: string): string =>
  `${database}/${schema}.${table}`

export function useTransferGate(
  database: MaybeRefOrGetter<string>,
  schema: MaybeRefOrGetter<string>,
  table: MaybeRefOrGetter<string>,
) {
  const settings = useClusterSettings()
  // The same query as the Tasks page and the table pages ask: one request is shared.
  const tasks = useTasks(100)
  const reason = computed(() => {
    if (settings.data.value?.dataTransfer?.enabled === false)
      return 'Data transfer is turned off for this cluster (Settings, Data transfer).'
    const subject = tableSubject(toValue(database), toValue(schema), toValue(table))
    const busy = (tasks.data.value?.items ?? []).some(
      (t) =>
        (t.state === 'running' || t.state === 'pending' || t.state === 'paused') &&
        t.subject?.type === 'table' &&
        t.subject.id === subject,
    )
    return busy ? 'Waiting for the operation on this table to end.' : ''
  })
  return { reason }
}
