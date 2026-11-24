<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Export a table as a CSV file. The dialog says before the person confirms what the file will guarantee for this kind
// of table (one snapshot, or each group at its own moment), what the spreadsheet guard does and that it is not a security
// feature, follows the export as a task, and then shows the file with its SHA-256 and a download that resumes.
import { computed, reactive, ref, watch } from 'vue'
import TaskActionDialog from '@/components/TaskActionDialog.vue'
import FormField from '@/components/FormField.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import SafeName from '@/components/SafeName.vue'
import ExportResult from './ExportResult.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import { keys, useClusterSettings, useTableDetail, useTopology } from '@/api/queries'
import type { CsvDelimiter, CsvEncoding, Table } from '@/api/types'
import { DELIMITER_LABEL, ENCODING_LABEL, consistencyBefore } from '@/lib/transfer'
import { defaultExportForm, exportRequest, nullStringError, utf8Length } from '@/lib/transfer-form'
import { integerBetween } from '@/lib/validators'

const props = defineProps<{ databaseName: string; table: Table }>()
const emit = defineEmits<{ close: [] }>()

const settings = useClusterSettings()
const detail = useTableDetail(props.databaseName, props.table.schema, props.table.name)
const topology = useTopology()
const off = computed(() => settings.data.value?.dataTransfer?.enabled === false)

const form = reactive(defaultExportForm(settings.data.value?.dataTransfer?.retentionHours ?? 24))
const chooseColumns = ref(false)
const picked = ref<string[]>([])
const submitted = ref(false)
// The default retention is the cluster's; it follows the setting until the person edits the field.
const retentionEdited = ref(false)
watch(
  () => settings.data.value?.dataTransfer?.retentionHours,
  (hours) => {
    if (hours !== undefined && !retentionEdited.value) form.retentionHours = hours
  },
)

const allColumns = computed(() => detail.data.value?.columns.map((c) => c.name) ?? [])
const kind = computed<'local' | 'reference' | 'distributed'>(() =>
  props.table.classes.includes('distributed')
    ? 'distributed'
    : props.table.classes.includes('reference')
      ? 'reference'
      : 'local',
)
/** The shard groups that can hold buckets: every group but the map group. */
const groups = computed(() => topology.data.value?.groups.filter((g) => !g.isMapGroup).length ?? 0)
const before = computed(() => consistencyBefore(kind.value, groups.value))

const errors = computed(() => {
  const e: Record<string, string> = {}
  const nul = nullStringError(form.nullString, form.delimiter)
  if (nul) e.nullString = nul
  const hours = integerBetween('The retention time', 1, 168)(form.retentionHours)
  if (hours) e.retentionHours = hours
  if (chooseColumns.value && picked.value.length === 0) e.columns = 'Choose at least one column.'
  return e
})
const serverErrors = (fieldErrors: Record<string, string>, field: string) => fieldErrors[field]
const shown = (field: string, fieldErrors: Record<string, string>) =>
  (submitted.value ? errors.value[field] : undefined) ?? serverErrors(fieldErrors, field)

const validate = (): boolean => {
  submitted.value = true
  return Object.keys(errors.value).length === 0
}

const start = () => {
  const request = exportRequest({
    ...form,
    columns: chooseColumns.value ? allColumns.value.filter((c) => picked.value.includes(c)) : null,
  })
  return endpoints.exportTable(props.databaseName, props.table.schema, props.table.name, request)
}

/** After a refusal for code of another role the person can accept the risk and confirm again. */
const untrusted = (error: unknown): boolean =>
  describeProblem(error).code === 'export_untrusted_code'
</script>

<template>
  <TaskActionDialog
    :title="`Export ${table.schema}.${table.name} as CSV`"
    confirm-label="Export table"
    wide
    :start="start"
    :validate="validate"
    :confirm-disabled="off"
    :invalidate="[keys.dataExports]"
    @close="emit('close')"
  >
    <template #default="{ fieldErrors }">
      <p v-if="off" class="notice tone-info" role="status" data-testid="export-off">
        <span aria-hidden="true">&#9432;</span>
        <span>Data transfer is turned off for this cluster (Settings, Data transfer).</span>
      </p>
      <p>
        The table is read with one statement as you, so you need the SELECT privilege on the
        columns. The file is written on the server and kept until its retention time ends. Writes to
        the table are not stopped.
      </p>
      <p
        class="notice"
        :class="kind === 'distributed' ? 'tone-warn' : 'tone-info'"
        data-testid="export-consistency-before"
        :data-kind="kind"
      >
        <span aria-hidden="true">{{ kind === 'distributed' ? '▲' : 'ⓘ' }}</span>
        <span>{{ before }}</span>
      </p>

      <fieldset>
        <legend>Columns</legend>
        <label class="check"
          ><input v-model="chooseColumns" type="radio" :value="false" /> All columns, in table
          order</label
        >
        <label class="check"
          ><input v-model="chooseColumns" type="radio" :value="true" /> Choose columns</label
        >
        <QueryBoundary
          v-if="chooseColumns"
          :loading="detail.isPending.value"
          :error="detail.error.value"
          @retry="detail.refetch()"
        >
          <div
            class="stack"
            style="gap: var(--s-1); max-height: 12rem; overflow: auto"
            tabindex="0"
            role="group"
            aria-label="Columns to export"
          >
            <label v-for="c in allColumns" :key="c" class="check"
              ><input v-model="picked" type="checkbox" :value="c" />
              <code><SafeName :value="c" :max="80" /></code
            ></label>
          </div>
          <span v-if="shown('columns', fieldErrors)" class="error" role="alert">{{
            shown('columns', fieldErrors)
          }}</span>
        </QueryBoundary>
        <p class="hint muted">
          A generated column is exported (it is a value) and is skipped when the file is imported.
        </p>
      </fieldset>

      <div class="grid-2">
        <FormField v-slot="slot" label="Delimiter" :error="shown('delimiter', fieldErrors)">
          <select v-bind="slot" v-model="form.delimiter">
            <option v-for="(label, d) in DELIMITER_LABEL" :key="d" :value="d as CsvDelimiter">
              {{ label }}
            </option>
          </select>
        </FormField>
        <FormField v-slot="slot" label="Encoding" :error="shown('encoding', fieldErrors)">
          <select v-bind="slot" v-model="form.encoding">
            <option v-for="(label, e) in ENCODING_LABEL" :key="e" :value="e as CsvEncoding">
              {{ label }}
            </option>
          </select>
        </FormField>
        <FormField
          v-slot="slot"
          label="Text of a null"
          :hint="`${utf8Length(form.nullString)} of 32 bytes. Empty: a null is an empty field with no quotes.`"
          :error="shown('nullString', fieldErrors)"
        >
          <input v-bind="slot" v-model="form.nullString" class="input mono" autocomplete="off" />
        </FormField>
        <FormField
          v-slot="slot"
          label="Keep the file (hours)"
          hint="1 to 168. The cluster default is filled in."
          :error="shown('retentionHours', fieldErrors)"
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
      </div>

      <label class="check"
        ><input v-model="form.header" type="checkbox" /> The first line holds the column
        names</label
      >
      <label v-if="form.encoding === 'UTF8'" class="check"
        ><input v-model="form.utf8Bom" type="checkbox" /> Start the file with a byte order mark
        (some spreadsheets need it to read UTF-8)</label
      >
      <label class="check"
        ><input v-model="form.compression" type="checkbox" true-value="gzip" false-value="none" />
        Compress the file (gzip)</label
      >

      <fieldset>
        <legend>Spreadsheet-safe values</legend>
        <label class="check"
          ><input v-model="form.spreadsheetSafe" type="checkbox" data-testid="spreadsheet-safe" />
          Spreadsheet-safe values</label
        >
        <p class="hint muted" data-testid="spreadsheet-text">
          Values that begin with <code>=</code>, <code>+</code>, <code>-</code>, <code>@</code>,
          <code>|</code> or <code>%</code> are prefixed with an apostrophe so a spreadsheet shows
          them as text. This changes those values, and importing the file puts the apostrophe in. It
          is a convenience, not a security feature: a value of another kind (json, an array) that
          begins with such a character is not changed.
        </p>
        <p v-if="!form.spreadsheetSafe" class="notice tone-warn" data-testid="spreadsheet-warning">
          <span aria-hidden="true">▲</span>
          <span
            >The file will hold the values exactly. Do not open it in a spreadsheet: a value that
            begins with one of these characters can run as a formula.</span
          >
        </p>
      </fieldset>
    </template>
    <template #error="{ error }">
      <template v-if="untrusted(error)">
        <ProblemNotice :error="error" />
        <label class="check" data-testid="acknowledge-code"
          ><input v-model="form.acknowledgeOtherRolesCode" type="checkbox" /> I accept that this
          export runs code of another role. It is recorded in the audit log.</label
        >
        <p class="muted">Then choose Export table again.</p>
      </template>
      <ProblemNotice v-else :error="error" />
    </template>
    <template #result="{ task }">
      <ExportResult v-if="task" :task="task" :spreadsheet-safe="form.spreadsheetSafe" />
    </template>
  </TaskActionDialog>
</template>
