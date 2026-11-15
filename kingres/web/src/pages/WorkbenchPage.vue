<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Query workbench: one statement per request, run as the signed-in PostgreSQL role. The statement
// really runs (there is no read-only mode in the API), a result belongs to the database and the
// statement that produced it, and a large result is shown in parts.
import { computed, ref, watch } from 'vue'
import PageHeader from '@/components/PageHeader.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import SafeName from '@/components/SafeName.vue'
import TableWrap from '@/components/TableWrap.vue'
import { endpoints } from '@/api/endpoints'
import { isSessionLost } from '@/api/problem'
import { useDatabases } from '@/api/queries'
import { formatNumber } from '@/lib/format'
import { vBlockEnterSubmit } from '@/lib/forms'
import { hasHidden, safeText } from '@/lib/safe-text'
import { firstError, integerBetween } from '@/lib/validators'
import { useSessionStore } from '@/stores/session'
import type { QueryResult } from '@/api/types'

const PAGE = 500
const LONG = 300

const session = useSessionStore()
const databases = useDatabases()
const database = ref('')
const sql = ref('select * from orders')
const rowLimit = ref<number | string>(1000)
/** Seconds; empty means the server default (30 s). */
const timeoutSeconds = ref<number | string>(30)

interface Shown {
  data: QueryResult
  database: string
  sql: string
}
const result = ref<Shown | null>(null)
const visibleRows = ref(PAGE)
const running = ref(false)
const error = ref<unknown>(null)
const stopped = ref(false)
let controller: AbortController | null = null

watch(
  () => databases.data.value,
  (d) => {
    if (!database.value) database.value = d?.items[0]?.name ?? ''
  },
  { immediate: true },
)
// A result belongs to the database it was run on: another database starts from nothing.
watch(database, () => {
  result.value = null
  error.value = null
  stopped.value = false
})
const stale = computed(() => result.value !== null && result.value.sql !== sql.value)

const limitError = computed(() =>
  firstError<number | null>(
    typeof rowLimit.value === 'number' ? rowLimit.value : null,
    integerBetween('The row limit', 1, 10_000),
  ),
)
const timeoutError = computed(() =>
  timeoutSeconds.value === ''
    ? undefined
    : firstError<number | null>(
        typeof timeoutSeconds.value === 'number' ? timeoutSeconds.value : null,
        integerBetween('The timeout', 1, 300),
      ),
)
const canRun = computed(
  () =>
    !!database.value &&
    sql.value.trim() !== '' &&
    !running.value &&
    !limitError.value &&
    !timeoutError.value,
)

async function runStatement(): Promise<void> {
  if (!canRun.value) return
  const started = { database: database.value, sql: sql.value }
  controller = new AbortController()
  const mine = controller
  running.value = true
  error.value = null
  stopped.value = false
  try {
    const data = await endpoints.executeQuery(
      {
        database: started.database,
        sql: started.sql,
        rowLimit: rowLimit.value as number,
        ...(typeof timeoutSeconds.value === 'number'
          ? { timeoutMs: timeoutSeconds.value * 1000 }
          : {}),
      },
      mine.signal,
    )
    if (mine.signal.aborted || database.value !== started.database) return
    result.value = { data, ...started }
    visibleRows.value = PAGE
  } catch (e) {
    if (mine.signal.aborted) return
    result.value = null
    if (!isSessionLost(e)) error.value = e
  } finally {
    if (controller === mine) {
      running.value = false
      controller = null
    }
  }
}

/**
 * Stops waiting for the answer. The API has no way to cancel a statement, so the server may still
 * be running it until it finishes or reaches the timeout.
 */
function stopWaiting(): void {
  controller?.abort()
  controller = null
  running.value = false
  stopped.value = true
}

function onKey(e: KeyboardEvent): void {
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') void runStatement()
}

type Cell = { kind: 'null' } | { kind: 'empty' } | { kind: 'text'; text: string; quote: boolean }
function describeCell(value: unknown): Cell {
  if (value === null || value === undefined) return { kind: 'null' }
  if (typeof value === 'string') {
    if (value === '') return { kind: 'empty' }
    // The text NULL must not look like SQL NULL, and padding must be visible.
    const confusing = value.trim().toUpperCase() === 'NULL' || value !== value.trim()
    return { kind: 'text', text: value, quote: confusing }
  }
  return {
    kind: 'text',
    text: typeof value === 'object' ? JSON.stringify(value) : String(value),
    quote: false,
  }
}
const plain = (text: string) => text.length <= LONG && !hasHidden(text)

const shownRows = computed(() =>
  (result.value?.data.rows.slice(0, visibleRows.value) ?? []).map((row) => row.map(describeCell)),
)
const hiddenRows = computed(() => (result.value?.data.rows.length ?? 0) - shownRows.value.length)
</script>

<template>
  <PageHeader
    title="Query workbench"
    subtitle="Runs one statement as your own PostgreSQL role, so your privileges apply."
  />
  <p class="notice tone-warn" data-testid="mode">
    <span aria-hidden="true">&#9650;</span>
    <span>
      <strong>Mode: read-write.</strong> The statement runs for real, as role
      <strong><SafeName :value="session.username ?? ''" kind="text" :max="40" /></strong>. INSERT,
      UPDATE, DELETE and DDL statements are executed, not simulated, and are recorded in the audit
      log. There is no read-only mode in the API.
    </span>
  </p>
  <form v-block-enter-submit class="stack" @submit.prevent>
    <div class="row" style="align-items: flex-end; gap: var(--s-4)">
      <div class="field">
        <label for="w-db">Database</label>
        <select id="w-db" v-model="database" :disabled="running">
          <option v-for="d in databases.data.value?.items" :key="d.name" :value="d.name">
            {{ safeText(d.name) }}
          </option>
        </select>
      </div>
      <div class="field">
        <label for="w-limit">Row limit</label>
        <input
          id="w-limit"
          v-model.number="rowLimit"
          class="input num"
          type="number"
          min="1"
          max="10000"
          style="width: 7em"
          :aria-invalid="limitError ? 'true' : undefined"
          :aria-describedby="limitError ? 'w-limit-err' : undefined"
        />
        <span v-if="limitError" id="w-limit-err" class="error" role="alert">{{ limitError }}</span>
      </div>
      <div class="field">
        <label for="w-timeout">Timeout (seconds)</label>
        <input
          id="w-timeout"
          v-model.number="timeoutSeconds"
          class="input num"
          type="number"
          min="1"
          max="300"
          style="width: 7em"
          :aria-invalid="timeoutError ? 'true' : undefined"
          :aria-describedby="timeoutError ? 'w-timeout-err' : 'w-timeout-hint'"
        />
        <span v-if="timeoutError" id="w-timeout-err" class="error" role="alert">{{
          timeoutError
        }}</span>
        <span v-else id="w-timeout-hint" class="hint">Empty uses the server default.</span>
      </div>
      <GateButton op="executeQuery" variant="primary" :disabled="!canRun" @click="runStatement">{{
        running ? 'Running...' : 'Run'
      }}</GateButton>
      <button v-if="running" type="button" class="btn" @click="stopWaiting">Stop waiting</button>
    </div>
    <div class="field">
      <label for="w-sql">SQL (Ctrl+Enter runs)</label>
      <textarea
        id="w-sql"
        v-model="sql"
        class="mono"
        rows="8"
        spellcheck="false"
        :readonly="running"
        @keydown="onKey"
      />
    </div>
  </form>

  <div class="stack" style="margin-top: var(--s-4)" aria-live="polite">
    <p v-if="stopped" class="notice tone-warn" role="status">
      <span aria-hidden="true">&#9650;</span>
      <span>
        You stopped waiting for the answer. The statement may still be running on the server until
        it finishes or reaches its timeout, and its effects are not undone.
      </span>
    </p>
    <ProblemNotice v-if="error" :error="error" />
    <section
      v-if="result"
      class="stack"
      :class="{ 'is-stale': stale }"
      aria-labelledby="res-h"
      data-testid="result"
      :data-stale="stale ? 'true' : 'false'"
    >
      <h2 id="res-h">Result</h2>
      <p class="muted">
        Database <SafeName :value="result.database" :max="60" />: {{ result.data.commandTag }},
        {{ formatNumber(result.data.rowCount) }} row(s) in
        {{ result.data.durationMs.toFixed(1) }} ms.
        <span v-if="result.data.truncated" class="badge tone-warn"
          ><span aria-hidden="true">&#9650;</span> Cut at the row limit or at the 16 MiB size limit
          of a result</span
        >
      </p>
      <p v-if="stale" class="notice tone-warn" role="status">
        <span aria-hidden="true">&#9650;</span>
        <span>
          This result does not match the statement now in the editor. Run it again to see its
          result.
        </span>
      </p>
      <TableWrap>
        <table class="table">
          <caption class="sr-only">
            Query result
          </caption>
          <thead>
            <tr>
              <th v-for="(c, j) in result.data.columns" :key="j" scope="col">
                <SafeName :value="c.name" :max="80" />
                <div class="muted mono"><SafeName :value="c.type" kind="text" :max="40" /></div>
              </th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(row, i) in shownRows" :key="i">
              <td v-for="(cell, j) in row" :key="j" class="mono">
                <template v-if="true">
                  <em v-if="cell.kind === 'null'" class="null-cell"
                    ><span aria-hidden="true">NULL</span><span class="sr-only">null value</span></em
                  >
                  <em v-else-if="cell.kind === 'empty'" class="empty-cell"
                    ><span aria-hidden="true">""</span><span class="sr-only">empty string</span></em
                  >
                  <template v-else-if="plain(cell.text)"
                    ><template v-if="cell.quote">"{{ cell.text }}"</template
                    ><template v-else>{{ cell.text }}</template></template
                  >
                  <SafeName v-else :value="cell.text" kind="text" :max="LONG" multiline />
                </template>
              </td>
            </tr>
            <tr v-if="result.data.rows.length === 0">
              <td :colspan="Math.max(1, result.data.columns.length)" class="muted">No rows.</td>
            </tr>
          </tbody>
        </table>
      </TableWrap>
      <p v-if="hiddenRows > 0" class="row">
        <span class="muted"
          >Showing the first {{ formatNumber(shownRows.length) }} of
          {{ formatNumber(result.data.rows.length) }} rows; the other
          {{ formatNumber(hiddenRows) }} are not shown.</span
        >
        <button type="button" class="btn btn-small" @click="visibleRows += PAGE">
          Show {{ PAGE }} more rows
        </button>
      </p>
    </section>
  </div>
</template>

<style scoped>
.is-stale {
  opacity: 0.7;
}
</style>
