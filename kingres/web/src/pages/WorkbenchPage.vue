<!-- SPDX-License-Identifier: AGPL-3.0-only -->
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
import { ApiError, isSessionLost } from '@/api/problem'
import { useAnnounce } from '@/composables/useAnnounce'
import { useApproximateAggregates, useDatabases } from '@/api/queries'
import { formatNumber } from '@/lib/format'
import { vBlockEnterSubmit } from '@/lib/forms'
import { hasHidden, safeText } from '@/lib/safe-text'
import { firstError, integerBetween } from '@/lib/validators'
import { planLines, summarizePlan } from '@/lib/plan-summary'
import { useSessionStore } from '@/stores/session'
import type { QueryResult } from '@/api/types'

const PAGE = 500
const LONG = 300

const session = useSessionStore()
const announce = useAnnounce()
const databases = useDatabases()
const database = ref('')
/** Whether the database has the sketch types (contract 0.18.0); the help text next to the editor lists the functions. */
const aggregates = useApproximateAggregates(
  () => database.value,
  () => database.value !== '',
)
const sketchesOn = computed(() => aggregates.data.value?.enabled === true)
const SKETCH_FUNCTIONS =
  'hll_add_agg, hll_union_agg, hll_cardinality, approx_count_distinct, topn_add_agg, topn_union_agg, topn_list, tdigest_agg, tdigest_quantile'
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
/** The id the running statement was sent with (0.7.0): it is what `cancelStatement` names. */
const statementId = ref<string | null>(null)
/** The id of a statement the person stopped waiting for; it may still run on the server. */
const abandonedId = ref<string | null>(null)
const cancelling = ref(false)
const cancelRequested = ref(false)
const cancelError = ref<unknown>(null)
const abandonedCancelled = ref(false)

/** A fresh random id, 32 hexadecimal digits: unique among the statements of one role on one server. */
function newStatementId(): string {
  const bytes = new Uint8Array(16)
  globalThis.crypto.getRandomValues(bytes)
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
}

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
  const id = newStatementId()
  statementId.value = id
  running.value = true
  error.value = null
  stopped.value = false
  abandonedId.value = null
  abandonedCancelled.value = false
  cancelRequested.value = false
  cancelError.value = null
  try {
    const data = await endpoints.executeQuery(
      {
        database: started.database,
        sql: started.sql,
        statementId: id,
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
      statementId.value = null
      cancelling.value = false
    }
  }
}

/**
 * Asks the server to end the running statement (0.7.0, `cancelStatement`). The request that runs it then ends with
 * 422 `sql_error` and SQLSTATE 57014, which the page shows as "cancelled by you". Nothing here is a task: the effect
 * is immediate.
 */
async function cancelStatement(): Promise<void> {
  const id = statementId.value
  if (!id || cancelling.value) return
  cancelling.value = true
  cancelError.value = null
  try {
    await endpoints.cancelStatement(id)
    cancelRequested.value = true
    announce('Cancel requested. The server is ending the statement.')
  } catch (e) {
    if (!isSessionLost(e)) cancelError.value = e
  } finally {
    cancelling.value = false
  }
}

/** Cancels, on the server, a statement that this page stopped waiting for (it kept running there). */
async function cancelAbandoned(): Promise<void> {
  const id = abandonedId.value
  if (!id || cancelling.value) return
  cancelling.value = true
  cancelError.value = null
  try {
    await endpoints.cancelStatement(id)
    abandonedCancelled.value = true
    announce('Cancel requested. The server is ending the statement.')
  } catch (e) {
    if (!isSessionLost(e)) cancelError.value = e
  } finally {
    cancelling.value = false
  }
}

/**
 * Stops waiting for the answer. This does not end the statement: the server keeps running it until it finishes or
 * reaches the timeout (and it keeps one of the role's two workbench slots). "Cancel statement" ends it.
 */
function stopWaiting(): void {
  controller?.abort()
  controller = null
  running.value = false
  stopped.value = true
  abandonedId.value = statementId.value
  statementId.value = null
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
/**
 * A cut result of a plain read (SELECT, TABLE, VALUES, SHOW) changed nothing. Any other statement is never cut
 * short (contract 0.6.0): it ran to the end and was committed, only the rows it returns were dropped.
 */
const cutAppliedChange = computed(() => {
  const r = result.value
  if (!r || !r.data.truncated) return false
  const plainRead =
    /^(select|table|values|show)\b/i.test(r.data.commandTag) &&
    /^[\s(]*(select|table|values|show|with)\b/i.test(r.sql)
  return !plainRead
})

/** Plain words for the answers that have a special meaning in the workbench. */
const errorHint = computed(() => {
  const e = error.value
  if (!(e instanceof ApiError)) return ''
  if (e.code === 'sql_error' && e.problem.sqlState === '57014' && cancelRequested.value)
    return 'You cancelled this statement (SQLSTATE 57014). The server ended it, and a statement that was cancelled changed nothing.'
  if (e.code === 'sql_error' && e.problem.sqlState === '57014')
    return 'The statement was cancelled because it ran longer than the time limit (SQLSTATE 57014). This is not a fault of the server: raise the timeout above, or make the statement cheaper. A statement that was cancelled changed nothing.'
  if (e.code === 'unprocessable')
    return 'A value above 1 MiB, or a row above 16 MiB, cannot be returned. The statement failed; if it changes data, it was rolled back. Select fewer or smaller columns, for example a part of the value.'
  if (e.code === 'workbench_busy')
    return 'One database role can run at most two statements at a time on one server (the server runs four in all). A statement that you stopped waiting for still counts until it ends. Wait for one to finish, or cancel one, and run this statement again; it was not started.'
  return ''
})
/** The lines of an EXPLAIN result (first column QUERY PLAN) and the one-line summary read from them; null for any other result. */
const plan = computed(() => {
  const r = result.value
  const lines = r ? planLines(r.data) : null
  return lines ? { lines, summary: summarizePlan(lines) } : null
})
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
      log. There is no read-only mode in the API. A result cut at the row limit does not stop a
      statement that changes data: it is still carried out and committed. EXPLAIN ANALYZE and MERGE
      run the statement too.
    </span>
  </p>
  <form v-block-enter-submit class="stack" @submit.prevent>
    <div class="form-row">
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
      <GateButton
        v-if="running"
        op="cancelStatement"
        variant="danger"
        :disabled="cancelling || cancelRequested || statementId === null"
        @click="cancelStatement"
        >Cancel statement</GateButton
      >
      <button v-if="running" type="button" class="btn" @click="stopWaiting">Stop waiting</button>
    </div>
    <p v-if="running" class="hint muted" data-testid="cancel-help">
      <strong>Cancel statement</strong> asks the server to end the statement now; it is rolled back.
      <strong>Stop waiting</strong> only stops this page from waiting: the statement keeps running
      on the server and keeps one of your two workbench slots until it ends.
    </p>
    <p v-if="sketchesOn" class="hint muted" data-testid="sketch-help">
      Approximate aggregates are on for this database. Functions of the sketch types hll, topn and
      tdigest: {{ SKETCH_FUNCTIONS }}.
    </p>
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
    <p v-if="stopped" class="notice tone-warn" role="status" data-testid="stopped-notice">
      <span aria-hidden="true">&#9650;</span>
      <span>
        You stopped waiting for the answer. The statement may still be running on the server until
        it finishes or reaches its timeout, and its effects are not undone.
        <template v-if="abandonedId && !abandonedCancelled">
          You can still end it:
          <GateButton
            op="cancelStatement"
            small
            variant="danger"
            :disabled="cancelling"
            @click="cancelAbandoned"
            >Cancel it on the server</GateButton
          >
        </template>
        <template v-if="abandonedCancelled"> Cancel requested: the server is ending it.</template>
      </span>
    </p>
    <ProblemNotice v-if="cancelError" :error="cancelError" />
    <p v-if="cancelError" class="muted" data-testid="cancel-error-hint">
      The statement may have ended already (then its answer is shown), or it runs on another server
      than the one that answered this request: a statement is bound to the server it was started on.
    </p>
    <p
      v-if="cancelRequested && running"
      class="notice tone-info"
      role="status"
      data-testid="cancel-requested"
    >
      <span aria-hidden="true">&#9432;</span>
      <span>Cancel requested. The server is ending the statement.</span>
    </p>
    <ProblemNotice v-if="error" :error="error" />
    <p v-if="errorHint" class="muted" data-testid="query-error-hint">{{ errorHint }}</p>
    <section
      v-if="result"
      class="stack"
      :class="{ 'is-stale': stale }"
      aria-labelledby="res-h"
      data-testid="result"
      :data-stale="stale ? 'true' : 'false'"
    >
      <h2 id="res-h">Result</h2>
      <p
        v-if="cutAppliedChange"
        class="notice tone-danger"
        role="status"
        data-testid="truncated-applied"
      >
        <span aria-hidden="true">&#10006;</span>
        <span>
          <strong>The statement WAS applied.</strong> Only the rows it returns are cut at the limit;
          the statement itself ran to the end and was committed ({{ result.data.commandTag }}). Do
          not run it again to see the other rows: that would change the data a second time.
        </span>
      </p>
      <p class="muted">
        Database <SafeName :value="result.database" :max="60" />: {{ result.data.commandTag }},
        {{ formatNumber(result.data.rowCount) }} row(s) in
        {{ result.data.durationMs.toFixed(1) }} ms.
        <span v-if="result.data.truncated" class="badge tone-warn wrap"
          ><span aria-hidden="true">&#9650;</span> Cut at the row limit or at the 16 MiB size limit
          of a result<template v-if="cutAppliedChange">; the statement was still applied</template
          ><template v-else>; the statement only read data</template></span
        >
      </p>
      <p v-if="stale" class="notice tone-warn" role="status">
        <span aria-hidden="true">&#9650;</span>
        <span>
          This result does not match the statement now in the editor. Run it again to see its
          result.
        </span>
      </p>
      <div v-if="plan" class="stack" data-testid="plan">
        <p v-if="plan.summary" class="notice tone-info" role="status" data-testid="plan-summary">
          <span aria-hidden="true">&#9432;</span>
          <span>
            <strong>Path: {{ plan.summary.path }}</strong
            ><template v-if="plan.summary.groups"> ({{ plan.summary.groups }} groups)</template>.
            <template v-for="n in plan.summary.notUsed" :key="'n' + n"
              ><span data-testid="plan-not-used">
                Not used: <SafeName :value="n" kind="text" :max="300" />.</span
              ></template
            >
            <template v-for="r in plan.summary.rewritten" :key="'r' + r"
              ><span data-testid="plan-rewritten">
                Rewritten: <SafeName :value="r" kind="text" :max="300" />.</span
              ></template
            >
          </span>
        </p>
        <pre
          class="mono plan"
          data-testid="plan-text"
        ><template v-for="(line, i) in plan.lines" :key="i"><template v-if="plain(line)">{{ line }}</template><SafeName v-else :value="line" kind="text" :max="LONG" />
</template></pre>
      </div>
      <TableWrap v-else>
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
.plan {
  margin: 0;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
</style>
