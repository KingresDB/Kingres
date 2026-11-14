<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Grant matrix for one role on one database: rows are the database, its schemas and tables,
// columns are the privileges. A checkbox issues GRANT or REVOKE immediately. Three templates
// set up common access in one step.
import { computed, ref, watch } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import QueryBoundary from '../QueryBoundary.vue'
import GateButton from '../GateButton.vue'
import ProblemNotice from '../ProblemNotice.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useDatabases, useRoles, useTables } from '@/api/queries'
import type { Grant, GrantLevel, GrantRequest, Privilege } from '@/api/types'
import { useSessionStore } from '@/stores/session'

const client = useQueryClient()
const session = useSessionStore()
const roles = useRoles()
const databases = useDatabases()
const grantee = ref('')
const database = ref('')

watch(
  () => roles.data.value,
  (d) => {
    if (!grantee.value) grantee.value = d?.items[0]?.name ?? ''
  },
  { immediate: true },
)
watch(
  () => databases.data.value,
  (d) => {
    if (!database.value) database.value = d?.items[0]?.name ?? ''
  },
  { immediate: true },
)

const tables = useTables(database)
const grants = useQuery({
  queryKey: computed(() => [...keys.grants, grantee.value, database.value]),
  queryFn: () => endpoints.listGrants({ grantee: grantee.value, database: database.value }),
  enabled: computed(() => !!grantee.value && !!database.value),
})

const PRIVILEGES: Record<GrantLevel, Privilege[]> = {
  database: ['connect', 'create', 'temporary'],
  schema: ['usage', 'create'],
  table: ['select', 'insert', 'update', 'delete', 'truncate', 'references', 'trigger'],
}
const COLUMNS: Privilege[] = [
  'connect',
  'temporary',
  'usage',
  'create',
  'select',
  'insert',
  'update',
  'delete',
  'truncate',
  'references',
  'trigger',
]

interface Row {
  key: string
  label: string
  level: GrantLevel
  schema: string | null
  table: string | null
}
const rows = computed<Row[]>(() => {
  const list: Row[] = [
    {
      key: 'db',
      label: `Database ${database.value}`,
      level: 'database',
      schema: null,
      table: null,
    },
  ]
  const items = tables.data.value?.items ?? []
  const schemas = [...new Set(['public', ...items.map((t) => t.schema)])]
  for (const s of schemas) {
    list.push({ key: `s:${s}`, label: `Schema ${s}`, level: 'schema', schema: s, table: null })
    for (const t of items.filter((x) => x.schema === s)) {
      list.push({
        key: `t:${s}.${t.name}`,
        label: `Table ${s}.${t.name}`,
        level: 'table',
        schema: s,
        table: t.name,
      })
    }
  }
  return list
})

function grantFor(row: Row): Grant | undefined {
  return grants.data.value?.items.find(
    (g) => g.level === row.level && g.schema === row.schema && g.table === row.table,
  )
}
const has = (row: Row, p: Privilege) => grantFor(row)?.privileges.includes(p) ?? false
const applicable = (row: Row, p: Privilege) => PRIVILEGES[row.level].includes(p)
const refresh = () => client.invalidateQueries({ queryKey: keys.grants })

const toggle = useMutation({
  mutationFn: ({ row, p, on }: { row: Row; p: Privilege; on: boolean }) => {
    const body: GrantRequest = {
      grantee: grantee.value,
      level: row.level,
      database: database.value,
      ...(row.schema ? { schema: row.schema } : {}),
      ...(row.table ? { table: row.table } : {}),
      privileges: [p],
    }
    return on ? endpoints.createGrant(body) : endpoints.revokeGrants(body)
  },
  onSettled: refresh,
})
function onToggle(row: Row, p: Privilege, event: Event): void {
  toggle.mutate({ row, p, on: (event.target as HTMLInputElement).checked })
}

const template = ref<'readOnly' | 'readWrite' | 'owner'>('readOnly')
const TEMPLATE_HELP = {
  readOnly: 'Connect and read every table (CONNECT, USAGE, SELECT).',
  readWrite: 'Connect, read and change data in every table (SELECT, INSERT, UPDATE, DELETE).',
  owner: 'Full control of all objects in the database, including DDL.',
}
const applyTemplate = useMutation({
  mutationFn: () =>
    endpoints.applyGrantTemplate({
      template: template.value,
      grantee: grantee.value,
      database: database.value,
    }),
  onSettled: refresh,
})
</script>

<template>
  <div class="stack">
    <div class="row filters">
      <div class="field">
        <label for="g-role">Role</label>
        <select id="g-role" v-model="grantee">
          <option v-for="r in roles.data.value?.items" :key="r.name" :value="r.name">
            {{ r.name }}
          </option>
        </select>
      </div>
      <div class="field">
        <label for="g-db">Database</label>
        <select id="g-db" v-model="database">
          <option v-for="d in databases.data.value?.items" :key="d.name" :value="d.name">
            {{ d.name }}
          </option>
        </select>
      </div>
    </div>

    <fieldset>
      <legend>Templates</legend>
      <div class="row">
        <label class="check"
          ><input v-model="template" type="radio" value="readOnly" /> Read-only on the
          database</label
        >
        <label class="check"
          ><input v-model="template" type="radio" value="readWrite" /> Read-write on the
          database</label
        >
        <label class="check"
          ><input v-model="template" type="radio" value="owner" /> Database owner</label
        >
      </div>
      <p class="hint muted">{{ TEMPLATE_HELP[template] }}</p>
      <div>
        <GateButton
          op="applyGrantTemplate"
          :disabled="!grantee || !database || applyTemplate.isPending.value"
          @click="applyTemplate.mutate()"
        >
          Apply template to {{ grantee || 'role' }}
        </GateButton>
      </div>
    </fieldset>

    <ProblemNotice v-if="toggle.error.value" :error="toggle.error.value" />
    <ProblemNotice v-if="applyTemplate.error.value" :error="applyTemplate.error.value" />

    <QueryBoundary
      :loading="grants.isPending.value || tables.isPending.value"
      :error="grants.error.value ?? tables.error.value"
      :empty="!grantee || !database"
      empty-title="Choose a role and a database"
    >
      <div class="table-wrap">
        <table class="table matrix">
          <caption class="sr-only">
            Privileges of
            {{
              grantee
            }}
            on
            {{
              database
            }}
          </caption>
          <thead>
            <tr>
              <th scope="col">Object</th>
              <th v-for="p in COLUMNS" :key="p" scope="col">{{ p.toUpperCase() }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in rows" :key="row.key">
              <th scope="row">{{ row.label }}</th>
              <td v-for="p in COLUMNS" :key="p">
                <input
                  v-if="applicable(row, p)"
                  type="checkbox"
                  :checked="has(row, p)"
                  :disabled="!session.can('createGrant') || toggle.isPending.value"
                  :aria-label="`${p} on ${row.label}`"
                  @change="onToggle(row, p, $event)"
                />
                <span v-else class="muted" aria-hidden="true">&ndash;</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </QueryBoundary>
  </div>
</template>

<style scoped>
.matrix td,
.matrix th {
  text-align: center;
}
.matrix th:first-child {
  text-align: left;
}
.filters {
  gap: var(--s-4);
}
</style>
