<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Export a whole database to one archive (contract 0.29.0, a placeholder revision). The dialog says, before the person
// confirms, what the archive holds and does not hold, how the tables will be read (one snapshot or per group, and that
// there is no global snapshot), that the role needs to read every table, and that an import checks the foreign keys at its end.
// It follows the export as a task, by table, and then shows the archive with its SHA-256 and a download that resumes.
import { computed, reactive, ref, watch } from 'vue'
import TaskActionDialog from '@/components/TaskActionDialog.vue'
import FormField from '@/components/FormField.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import SafeName from '@/components/SafeName.vue'
import ExportDatabaseResult from './ExportDatabaseResult.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem, ApiError } from '@/api/problem'
import { keys, useClusterSettings, useDatabase, useTables, useTopology } from '@/api/queries'
import {
  archiveConsistencyBefore,
  exportDatabaseRequest,
  READ_ORDER_TEXT,
  readPlan,
  tableKey,
  tableLabel,
  unreadableObjects,
} from '@/lib/archive'
import { formatBytes, formatNumber } from '@/lib/format'
import { integerBetween } from '@/lib/validators'

const props = defineProps<{ databaseName: string }>()
const emit = defineEmits<{ close: [] }>()

const settings = useClusterSettings()
const database = useDatabase(() => props.databaseName)
const tables = useTables(() => props.databaseName)
const topology = useTopology()
const off = computed(() => settings.data.value?.dataTransfer?.enabled === false)

const form = reactive({
  dataMode: 'all' as 'all' | 'none',
  retentionHours: settings.data.value?.dataTransfer?.retentionHours ?? 24,
  acknowledgeLargeObjectsLost: false,
})
const excluded = ref<string[]>([])
const submitted = ref(false)
const retentionEdited = ref(false)
watch(
  () => settings.data.value?.dataTransfer?.retentionHours,
  (hours) => {
    if (hours !== undefined && !retentionEdited.value) form.retentionHours = hours
  },
)

const rows = computed(() => tables.data.value?.items ?? [])
const groups = computed(() => topology.data.value?.groups.filter((g) => !g.isMapGroup).length ?? 0)
const plan = computed(() => readPlan(rows.value, groups.value))
const before = computed(() => archiveConsistencyBefore(plan.value))
const skewed = computed(() => plan.value.distributed > 0 && plan.value.groups > 1)
/** Recent write activity makes tables that refer to each other more likely to be read at different moments. */
const busy = computed(() => (database.data.value?.stats.transactionsPerSecond ?? 0) > 0)
const totalBytes = computed(() => rows.value.reduce((n, t) => n + (t.sizeBytes ?? 0), 0))

const errors = computed(() => {
  const e: Record<string, string> = {}
  const hours = integerBetween('The retention time', 1, 168)(form.retentionHours)
  if (hours) e.retentionHours = hours
  return e
})
const validate = (): boolean => {
  submitted.value = true
  return Object.keys(errors.value).length === 0
}
const start = () =>
  endpoints.exportDatabase(
    props.databaseName,
    exportDatabaseRequest({ ...form, excluded: excluded.value }),
  )

const codeOf = (error: unknown): string => describeProblem(error).code ?? ''
const objectsOf = (error: unknown): string[] =>
  unreadableObjects(error instanceof ApiError ? error.problem?.detail : undefined)
</script>

<template>
  <TaskActionDialog
    :title="`Export database ${databaseName} to an archive`"
    confirm-label="Export database"
    wide
    :start="start"
    :validate="validate"
    :confirm-disabled="off"
    cancelable
    cancelled-text="The export was cancelled. No file was kept."
    :invalidate="[keys.dataExports]"
    @close="emit('close')"
  >
    <template #default="{ fieldErrors }">
      <p v-if="off" class="notice tone-info" role="status" data-testid="export-db-off">
        <span aria-hidden="true">&#9432;</span>
        <span>Data transfer is turned off for this cluster (Settings, Data transfer).</span>
      </p>
      <p>
        The archive is one zip file with the structure of the database, how its tables are
        distributed, and the data of every table. It is made as you, so you need USAGE on every
        schema and SELECT on every table; if you cannot read some, nothing is written and the first
        ones are named. The file is written on the server and kept until its retention time ends.
        Writers are not stopped.
      </p>
      <p class="notice tone-info" data-testid="export-db-not-carried">
        <span aria-hidden="true">&#9432;</span>
        <span
          >An archive does not carry roles, tablespaces, the placement of buckets, the history of
          ledgers, functions, triggers, policies or rules. A database that uses an object kind it
          does not hold is refused, not shortened. In this version an archive can be inspected and
          validated; importing it is not available.</span
        >
      </p>
      <p
        class="notice"
        :class="skewed ? 'tone-warn' : 'tone-info'"
        data-testid="export-db-consistency-before"
        :data-skewed="skewed"
      >
        <span aria-hidden="true">{{ skewed ? '▲' : 'ⓘ' }}</span>
        <span>{{ before }}</span>
      </p>
      <p class="muted" data-testid="export-db-read-order">{{ READ_ORDER_TEXT }}</p>
      <p v-if="busy" class="notice tone-warn" data-testid="export-db-busy">
        <span aria-hidden="true">▲</span>
        <span
          >This database is being written to now ({{
            formatNumber(database.data.value?.stats.transactionsPerSecond ?? 0)
          }}
          transactions per second). Tables that refer to each other may be read at different
          moments. Export in a quiet period if you can; a change of the structure while the export
          runs ends it.</span
        >
      </p>

      <fieldset>
        <legend>Data</legend>
        <label class="check"
          ><input v-model="form.dataMode" type="radio" value="all" data-testid="mode-all" /> The
          rows of every table (except those you leave out below)</label
        >
        <label class="check"
          ><input v-model="form.dataMode" type="radio" value="none" data-testid="mode-none" /> The
          structure and the catalog only, no rows</label
        >
      </fieldset>

      <fieldset v-if="form.dataMode === 'all'">
        <legend>Tables whose rows are left out</legend>
        <QueryBoundary
          :loading="tables.isPending.value"
          :error="tables.error.value"
          :empty="rows.length === 0"
          empty-title="No tables"
          empty-hint="The archive will hold the structure only."
          @retry="tables.refetch()"
        >
          <p class="hint muted">
            {{ rows.length }} {{ rows.length === 1 ? 'table' : 'tables' }},
            {{ formatBytes(totalBytes) }} in all. The structure of a table that is left out stays in
            the archive.
          </p>
          <div
            class="stack"
            style="gap: var(--s-1); max-height: 14rem; overflow: auto"
            tabindex="0"
            role="group"
            aria-label="Tables whose rows are left out of the archive"
            data-testid="export-db-tables"
          >
            <label v-for="t in rows" :key="tableKey(t)" class="check"
              ><input v-model="excluded" type="checkbox" :value="tableKey(t)" />
              <code><SafeName :value="tableLabel(t)" :max="100" /></code>
              <span class="muted">
                {{ formatBytes(t.sizeBytes)
                }}<template v-if="t.classes.includes('distributed')">, distributed</template
                ><template v-else-if="t.classes.includes('reference')">, reference</template></span
              ></label
            >
          </div>
          <span v-if="fieldErrors.excludeTables" class="error" role="alert">{{
            fieldErrors.excludeTables
          }}</span>
        </QueryBoundary>
      </fieldset>

      <FormField
        v-slot="slot"
        label="Keep the file (hours)"
        hint="1 to 168. The cluster default is filled in."
        :error="(submitted ? errors.retentionHours : undefined) ?? fieldErrors.retentionHours"
      >
        <input
          v-bind="slot"
          v-model.number="form.retentionHours"
          class="input num"
          type="number"
          min="1"
          max="168"
          @input="retentionEdited = true"
        />
      </FormField>
    </template>

    <template #error="{ error }">
      <template v-if="codeOf(error) === 'export_not_readable'">
        <ProblemNotice :error="error" />
        <ul class="stack" style="gap: var(--s-1)" data-testid="unreadable-objects">
          <li v-for="o in objectsOf(error)" :key="o">
            <code><SafeName :value="o" :max="100" /></code>
          </li>
        </ul>
      </template>
      <template v-else-if="codeOf(error) === 'large_objects_present'">
        <ProblemNotice :error="error" />
        <label class="check" data-testid="acknowledge-lobs"
          ><input v-model="form.acknowledgeLargeObjectsLost" type="checkbox" /> I accept that large
          objects and oid columns are not in the archive. The archive says so in its
          warnings.</label
        >
        <p class="muted">Then choose Export database again.</p>
      </template>
      <ProblemNotice v-else :error="error" />
    </template>

    <template #result="{ task }">
      <ExportDatabaseResult v-if="task" :task="task" />
    </template>
  </TaskActionDialog>
</template>
