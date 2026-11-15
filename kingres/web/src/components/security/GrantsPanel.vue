<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Grant matrix for one role on one database: rows are the database, its schemas and tables,
// columns are the privileges. A checkbox issues GRANT or REVOKE immediately. Three templates
// set up common access in one step.
import { computed, ref, watch } from 'vue'
import TableWrap from '../TableWrap.vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import QueryBoundary from '../QueryBoundary.vue'
import GateButton from '../GateButton.vue'
import ProblemNotice from '../ProblemNotice.vue'
import ConfirmDialog from '../ConfirmDialog.vue'
import SafeName from '../SafeName.vue'
import { endpoints } from '@/api/endpoints'
import { fetchAllPages, keys, useDatabases, useRoles, useTables } from '@/api/queries'
import type { Grant, GrantLevel, GrantRequest, Privilege } from '@/api/types'
import { useSessionStore } from '@/stores/session'
import { safeText } from '@/lib/safe-text'

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
  queryFn: () =>
    fetchAllPages((cursor) =>
      endpoints.listGrants({ grantee: grantee.value, database: database.value, cursor }),
    ),
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
  /** Kind of object and its name, shown with SafeName: `Database shop`. */
  kind: 'Database' | 'Schema' | 'Table'
  name: string
  label: string
  level: GrantLevel
  schema: string | null
  table: string | null
}
const rows = computed<Row[]>(() => {
  const list: Row[] = [
    {
      key: 'db',
      kind: 'Database',
      name: database.value,
      label: `Database ${safeText(database.value)}`,
      level: 'database',
      schema: null,
      table: null,
    },
  ]
  const items = tables.data.value?.items ?? []
  const schemas = [...new Set(['public', ...items.map((t) => t.schema)])]
  for (const s of schemas) {
    list.push({
      key: JSON.stringify(['s', s]),
      kind: 'Schema',
      name: s,
      label: `Schema ${safeText(s)}`,
      level: 'schema',
      schema: s,
      table: null,
    })
    for (const t of items.filter((x) => x.schema === s)) {
      list.push({
        // A name may contain a dot: the key must not be ambiguous.
        key: JSON.stringify(['t', s, t.name]),
        kind: 'Table',
        name: `${s}.${t.name}`,
        label: `Table ${safeText(`${s}.${t.name}`)}`,
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

const failedChange = ref<string | null>(null)
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
  const box = event.target as HTMLInputElement
  const on = box.checked
  failedChange.value = null
  toggle.mutate(
    { row, p, on },
    {
      // The box was flipped by the click. When the server did not make the change, put it back to
      // what the server has, so that the matrix never shows a grant that does not exist.
      onError: () => {
        box.checked = !on
        failedChange.value = `${on ? 'Granting' : 'Revoking'} ${p.toUpperCase()} on ${safeText(row.name)}`
      },
    },
  )
}

const template = ref<'readOnly' | 'readWrite' | 'owner'>('readOnly')
const TEMPLATE_HELP = {
  readOnly: 'Connect and read every table (CONNECT, USAGE, SELECT).',
  readWrite: 'Connect, read and change data in every table (SELECT, INSERT, UPDATE, DELETE).',
  owner: 'Full control of all objects in the database, including DDL.',
}
const reviewingTemplate = ref(false)
const TEMPLATE_LABEL = {
  readOnly: 'read-only',
  readWrite: 'read-write',
  owner: 'database owner',
} as const
const TEMPLATE_GRANTS = {
  readOnly:
    'CONNECT on the database, USAGE on its schemas, and SELECT on every table. Nothing can be changed.',
  readWrite:
    'CONNECT and TEMPORARY on the database, USAGE on its schemas, and SELECT, INSERT, UPDATE and DELETE on every table.',
  owner:
    'CONNECT, CREATE and TEMPORARY on the database, USAGE and CREATE on its schemas, and every privilege on every table. The role can change the structure (create, alter and drop objects).',
}
const applyTemplate = useMutation({
  mutationFn: () =>
    endpoints.applyGrantTemplate({
      template: template.value,
      grantee: grantee.value,
      database: database.value,
    }),
  onSuccess: () => {
    reviewingTemplate.value = false
  },
  onSettled: refresh,
})
</script>

<template>
  <div class="stack">
    <div class="form-row">
      <div class="field">
        <label for="g-role">Role</label>
        <select id="g-role" v-model="grantee">
          <option v-for="r in roles.data.value?.items" :key="r.name" :value="r.name">
            {{ safeText(r.name) }}
          </option>
        </select>
      </div>
      <div class="field">
        <label for="g-db">Database</label>
        <select id="g-db" v-model="database">
          <option v-for="d in databases.data.value?.items" :key="d.name" :value="d.name">
            {{ safeText(d.name) }}
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
          @click="reviewingTemplate = true"
        >
          Apply template to <SafeName :value="grantee || 'role'" kind="text" :max="40" />
        </GateButton>
      </div>
    </fieldset>

    <p v-if="roles.data.value?.moreNotShown" class="notice tone-warn" role="status">
      <span aria-hidden="true">&#9650;</span>
      <span>More roles exist than are listed here.</span>
    </p>
    <p v-if="tables.data.value?.moreNotShown" class="notice tone-warn" role="status">
      <span aria-hidden="true">&#9650;</span>
      <span>More tables exist than are shown in the matrix.</span>
    </p>
    <div v-if="failedChange" class="notice tone-danger" role="alert">
      <span aria-hidden="true">&#10006;</span>
      <span>{{ failedChange }} was not applied. The matrix shows what the server has.</span>
    </div>
    <ProblemNotice v-if="toggle.error.value" :error="toggle.error.value" />

    <QueryBoundary
      :loading="grants.isPending.value || tables.isPending.value"
      :error="grants.error.value ?? tables.error.value"
      :empty="!grantee || !database"
      empty-title="Choose a role and a database"
    >
      <TableWrap>
        <table class="table matrix">
          <caption class="sr-only">
            Privileges of
            {{
              safeText(grantee)
            }}
            on
            {{
              safeText(database)
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
              <th scope="row">{{ row.kind }} <SafeName :value="row.name" :max="120" /></th>
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
      </TableWrap>
    </QueryBoundary>
  </div>

  <ConfirmDialog
    v-if="reviewingTemplate"
    :title="`Apply the ${TEMPLATE_LABEL[template]} template to ${grantee} on ${database}?`"
    confirm-label="Apply template"
    :danger="template !== 'readOnly'"
    :type-to-confirm="template === 'owner' ? database : undefined"
    :busy="applyTemplate.isPending.value"
    @cancel="reviewingTemplate = false"
    @confirm="applyTemplate.mutate()"
  >
    <p>
      Role <strong><SafeName :value="grantee" :max="60" /></strong> is given, on database
      <strong><SafeName :value="database" :max="60" /></strong>:
    </p>
    <p class="notice">{{ TEMPLATE_GRANTS[template] }}</p>
    <p class="muted">
      Grants the role already has stay. You can revoke single privileges in the matrix afterwards.
    </p>
    <ProblemNotice v-if="applyTemplate.error.value" :error="applyTemplate.error.value" />
  </ConfirmDialog>
</template>

<style scoped>
.matrix td,
.matrix th {
  text-align: center;
}
.matrix th:first-child {
  text-align: left;
}
</style>
