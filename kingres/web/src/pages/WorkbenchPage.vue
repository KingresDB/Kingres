<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Query workbench: one statement per request, run as the signed-in PostgreSQL role.
import { computed, ref, watch } from 'vue'
import { useMutation } from '@tanstack/vue-query'
import PageHeader from '@/components/PageHeader.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import { endpoints } from '@/api/endpoints'
import { useDatabases } from '@/api/queries'
import { formatNumber } from '@/lib/format'
import type { QueryResult } from '@/api/types'

const databases = useDatabases()
const database = ref('')
const sql = ref('select * from orders')
const rowLimit = ref(1000)
const result = ref<QueryResult | null>(null)

watch(
  () => databases.data.value,
  (d) => {
    if (!database.value) database.value = d?.items[0]?.name ?? ''
  },
  { immediate: true },
)

const run = useMutation({
  mutationFn: () =>
    endpoints.executeQuery({ database: database.value, sql: sql.value, rowLimit: rowLimit.value }),
  onSuccess: (r) => {
    result.value = r
  },
  onError: () => {
    result.value = null
  },
})
const canRun = computed(() => !!database.value && sql.value.trim() !== '' && !run.isPending.value)

function format(value: unknown): string {
  if (value === null || value === undefined) return 'NULL'
  return typeof value === 'object' ? JSON.stringify(value) : String(value)
}
function onKey(e: KeyboardEvent): void {
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter' && canRun.value) run.mutate()
}
</script>

<template>
  <PageHeader
    title="Query workbench"
    subtitle="Runs one statement as your own PostgreSQL role, so your privileges apply."
  />
  <form class="stack" @submit.prevent="run.mutate()">
    <div class="row" style="align-items: flex-end; gap: var(--s-4)">
      <div class="field">
        <label for="w-db">Database</label>
        <select id="w-db" v-model="database">
          <option v-for="d in databases.data.value?.items" :key="d.name" :value="d.name">
            {{ d.name }}
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
        />
      </div>
      <GateButton op="executeQuery" type="submit" variant="primary" :disabled="!canRun">{{
        run.isPending.value ? 'Running...' : 'Run'
      }}</GateButton>
    </div>
    <div class="field">
      <label for="w-sql">SQL (Ctrl+Enter runs)</label>
      <textarea
        id="w-sql"
        v-model="sql"
        class="mono"
        rows="8"
        spellcheck="false"
        @keydown="onKey"
      />
    </div>
  </form>

  <div class="stack" style="margin-top: var(--s-4)" aria-live="polite">
    <ProblemNotice v-if="run.error.value" :error="run.error.value" />
    <section v-if="result" class="stack" aria-labelledby="res-h" data-testid="result">
      <h2 id="res-h">Result</h2>
      <p class="muted">
        {{ result.commandTag }}, {{ formatNumber(result.rowCount) }} row(s) in
        {{ result.durationMs.toFixed(1) }} ms.
        <span v-if="result.truncated" class="badge tone-warn"
          ><span aria-hidden="true">&#9650;</span> Truncated at the row limit</span
        >
      </p>
      <div class="table-wrap">
        <table class="table">
          <caption class="sr-only">
            Query result
          </caption>
          <thead>
            <tr>
              <th v-for="c in result.columns" :key="c.name" scope="col">
                {{ c.name }}
                <div class="muted mono">{{ c.type }}</div>
              </th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(row, i) in result.rows" :key="i">
              <td v-for="(cell, j) in row" :key="j" class="mono" :class="{ muted: cell === null }">
                {{ format(cell) }}
              </td>
            </tr>
            <tr v-if="result.rows.length === 0">
              <td :colspan="Math.max(1, result.columns.length)" class="muted">No rows.</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
  </div>
</template>
