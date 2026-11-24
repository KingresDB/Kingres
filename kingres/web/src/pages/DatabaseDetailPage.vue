<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import TableWrap from '@/components/TableWrap.vue'
import { RouterLink, useRouter } from 'vue-router'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import NullableValue from '@/components/NullableValue.vue'
import NotAvailable from '@/components/NotAvailable.vue'
import SafeName from '@/components/SafeName.vue'
import { safeText } from '@/lib/safe-text'
import TaskActionDialog from '@/components/TaskActionDialog.vue'
import EditDatabaseDialog from '@/components/EditDatabaseDialog.vue'
import DistributeTableDialog from '@/components/DistributeTableDialog.vue'
import SchemasSection from '@/components/SchemasSection.vue'
import EnableLedgerDialog from '@/components/EnableLedgerDialog.vue'
import ApproximateAggregatesCard from '@/components/ApproximateAggregatesCard.vue'
import StorageBadge from '@/components/tables/StorageBadge.vue'
import SetStorageDialog from '@/components/tables/SetStorageDialog.vue'
import PartitionPolicyDialog from '@/components/tables/PartitionPolicyDialog.vue'
import { matchesStorage, type StorageFilter } from '@/lib/analytics'
import TransfersPanel from '@/components/transfers/TransfersPanel.vue'
import DatabaseTransferActions from '@/components/transfers/DatabaseTransferActions.vue'
import TableTransferActions from '@/components/transfers/TableTransferActions.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useDatabase, useTables } from '@/api/queries'
import { DATABASE_STATUS, DURABILITY_LABEL, LEDGER_MODE_LABEL, LEDGER_STATE } from '@/lib/status'
import { formatBytes, formatDateTime, formatNumber } from '@/lib/format'
import TableOperationBadge from '@/components/tables/TableOperationBadge.vue'
import { operationReason } from '@/lib/tables'
import type { Table, TableClass } from '@/api/types'

const props = defineProps<{ name: string }>()
const router = useRouter()
const database = useDatabase(() => props.name)
const tables = useTables(() => props.name)
const db = computed(() => database.data.value)
const tableRows = computed(() => tables.data.value?.items ?? [])
/** The Storage filter of the list (contract 0.18.0): a partitioned table matches columnar when any partition is. */
const storageFilter = ref<StorageFilter>('all')
const shownRows = computed(() =>
  tableRows.value.filter((t) => matchesStorage(t, storageFilter.value)),
)

const dialog = ref<'edit' | 'drop' | null>(null)
const distributing = ref<Table | null>(null)
const ledgerFor = ref<Table | null>(null)
const converting = ref<Table | null>(null)
const policyFor = ref<Table | null>(null)
// The page is reused when the address changes to another database. A dialog opened for the
// previous one must not stay open and act on the new one.
watch(
  () => props.name,
  () => {
    dialog.value = null
    distributing.value = null
    ledgerFor.value = null
    converting.value = null
    policyFor.value = null
    storageFilter.value = 'all'
  },
)

const CLASS_LABEL: Record<TableClass, string> = {
  plain: 'Plain',
  distributed: 'Distributed',
  reference: 'Reference',
  ledger: 'Ledger',
}
const distributedTables = computed(() =>
  tableRows.value.filter((t) => t.classes.includes('distributed')),
)
/** Databases made outside the control plane have no creation record (null). */
const createdText = computed(() => {
  const d = db.value
  if (!d) return undefined
  if (d.createdBy === null && d.createdAt === null) return 'Creation record not available.'
  const by = d.createdBy === null ? 'an unknown user' : safeText(d.createdBy, 60)
  return d.createdAt === null
    ? `Created by ${by}.`
    : `Created by ${by} on ${formatDateTime(d.createdAt)}`
})
const shardingOn = computed(() => db.value?.sharding.enabled === true)
/** Why a table cannot be converted from this list (the dialog says the rest); empty when it can. */
function storageBlocked(t: Table): string {
  if (t.operation) return operationReason(t.operation)
  if (t.classes.includes('distributed') || t.classes.includes('reference'))
    return 'Converting a distributed table is not available in this version.'
  if (t.partitioning) return 'Convert one partition at a time, on the page of the table.'
  return ''
}
</script>

<template>
  <PageHeader :title="`Database ${name}`" :subtitle="createdText">
    <template #actions>
      <DatabaseTransferActions v-if="db" :database-name="name" />
      <GateButton op="updateDatabase" :disabled="!db" @click="dialog = 'edit'"
        >Edit settings</GateButton
      >
      <GateButton op="dropDatabase" variant="danger" :disabled="!db" @click="dialog = 'drop'"
        >Drop database</GateButton
      >
    </template>
  </PageHeader>
  <p><RouterLink :to="{ name: 'databases' }">&larr; All databases</RouterLink></p>

  <QueryBoundary
    :loading="database.isPending.value"
    :error="database.error.value"
    @retry="database.refetch()"
  >
    <div v-if="db" class="stack-lg">
      <section class="grid-2" aria-label="Settings">
        <div class="card stack">
          <h2>General</h2>
          <dl class="kv">
            <dt>Status</dt>
            <dd><StatusBadge :meta="DATABASE_STATUS[db.status]" /></dd>
            <dt>Owner</dt>
            <dd><NullableValue :value="db.general.owner" safe /></dd>
            <dt>Encoding</dt>
            <dd>
              <NullableValue :value="db.general.encoding" /> <span class="muted">(fixed)</span>
            </dd>
            <dt>Collation</dt>
            <dd class="mono">
              <NullableValue :value="db.general.lcCollate" /> <span class="muted">(fixed)</span>
            </dd>
            <dt>Connection limit</dt>
            <dd>
              <NullableValue
                :value="db.general.connectionLimit"
                :format="(n) => (n < 0 ? 'Unlimited' : String(n))"
              />
            </dd>
            <dt>Size</dt>
            <dd><NullableValue :value="db.stats.sizeBytes" :format="formatBytes" /></dd>
          </dl>
        </div>
        <div class="card stack">
          <h2>Sharding and replication</h2>
          <dl class="kv">
            <dt>Sharding</dt>
            <dd>
              <template v-if="shardingOn"
                >On <span class="muted">(cannot be turned off)</span></template
              >
              <template v-else>Off <span class="muted">(can be turned on)</span></template>
            </dd>
            <dt>Bucket count</dt>
            <dd>
              <NullableValue :value="db.sharding.bucketCount" /> <span class="muted">(fixed)</span>
            </dd>
            <dt>Durability</dt>
            <dd>
              <NullableValue
                :value="db.replication.durability"
                :format="(v) => DURABILITY_LABEL[v]"
              />
            </dd>
            <dt>Replica count</dt>
            <dd>
              {{ db.replication.replicaCount }}
              <RouterLink :to="{ name: 'settings' }">cluster setting</RouterLink>
            </dd>
            <dt>Health</dt>
            <dd>
              <NullableValue :value="db.health.underReplicatedBuckets" :format="formatNumber" />
              under-replicated,
              <NullableValue :value="db.health.unavailableBuckets" :format="formatNumber" />
              unavailable buckets
            </dd>
          </dl>
          <RouterLink v-if="shardingOn" :to="{ name: 'shards', query: { db: db.name } }"
            >Show bucket map</RouterLink
          >
        </div>
        <div class="card stack">
          <h2>Ledger</h2>
          <dl class="kv">
            <dt>Mode</dt>
            <dd>
              <NullableValue :value="db.ledger.mode" :format="(v) => LEDGER_MODE_LABEL[v]" />
            </dd>
            <dt>Digest interval</dt>
            <dd>
              <NullableValue :value="db.ledger.digestIntervalSeconds" :format="(n) => `${n} s`" />
            </dd>
            <dt>Verification</dt>
            <dd>
              <StatusBadge
                v-if="db.ledgerSummary.verificationState"
                :meta="LEDGER_STATE[db.ledgerSummary.verificationState]"
              />
              <NotAvailable v-else />
            </dd>
          </dl>
          <RouterLink
            v-if="db.ledger.mode !== 'off'"
            :to="{ name: 'ledger', query: { db: db.name } }"
            >Open ledger</RouterLink
          >
        </div>
        <ApproximateAggregatesCard :database-name="name" />
      </section>

      <SchemasSection :database-name="name" :mode="db.sharding.mode" />

      <section aria-labelledby="tables-h" class="stack">
        <div class="row-between">
          <h2 id="tables-h">Tables</h2>
          <div v-if="tableRows.length > 0" class="field">
            <label for="storage-filter">Storage</label>
            <select id="storage-filter" v-model="storageFilter" data-testid="storage-filter">
              <option value="all">All formats</option>
              <option value="heap">Row</option>
              <option value="columnar">Columnar</option>
            </select>
          </div>
        </div>
        <QueryBoundary
          :loading="tables.isPending.value"
          :error="tables.error.value"
          :empty="shownRows.length === 0"
          :empty-title="tableRows.length === 0 ? 'No tables' : 'No table in this format'"
          :empty-hint="
            tableRows.length === 0
              ? 'Create tables with SQL in the query workbench.'
              : 'Choose another format.'
          "
          @retry="tables.refetch()"
        >
          <TableWrap>
            <table class="table">
              <caption class="sr-only">
                Tables of the database
              </caption>
              <thead>
                <tr>
                  <th scope="col">Table</th>
                  <th scope="col">Class</th>
                  <th scope="col">Storage</th>
                  <th scope="col">Distribution column</th>
                  <th scope="col" class="num">Rows (estimate)</th>
                  <th scope="col" class="num">Size</th>
                  <th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="t in shownRows" :key="JSON.stringify([t.schema, t.name])">
                  <th scope="row" class="mono">
                    <RouterLink
                      :to="{ name: 'table', params: { name, schema: t.schema, table: t.name } }"
                    >
                      <SafeName :value="`${t.schema}.${t.name}`" :max="120" />
                    </RouterLink>
                    <TableOperationBadge v-if="t.operation" :operation="t.operation" />
                  </th>
                  <td>
                    <span class="row" style="gap: var(--s-1)">
                      <span v-for="c in t.classes" :key="c" class="badge tone-neutral">{{
                        CLASS_LABEL[c]
                      }}</span>
                    </span>
                  </td>
                  <td><StorageBadge :table="t" /></td>
                  <td>
                    <template v-if="t.distribution?.column">
                      <code><SafeName :value="t.distribution.column" :max="60" /></code>
                      <span class="muted"> (colocation {{ t.distribution.colocationId }})</span>
                    </template>
                    <template v-else-if="t.distribution">Replicated to every group</template>
                    <template v-else>-</template>
                  </td>
                  <td class="num">
                    <NullableValue :value="t.estimatedRows" :format="formatNumber" />
                  </td>
                  <td class="num"><NullableValue :value="t.sizeBytes" :format="formatBytes" /></td>
                  <td>
                    <div class="row">
                      <GateButton
                        v-if="t.classes.includes('plain')"
                        op="distributeTable"
                        small
                        :blocked-reason="
                          operationReason(t.operation) ||
                          (shardingOn ? '' : 'Turn on sharding for this database first.')
                        "
                        :data-testid="`distribute-${t.name}`"
                        @click="distributing = t"
                      >
                        Distribute table
                      </GateButton>
                      <GateButton
                        v-if="!t.classes.includes('ledger')"
                        op="enableTableLedger"
                        small
                        :blocked-reason="operationReason(t.operation)"
                        @click="ledgerFor = t"
                      >
                        Enable ledger
                      </GateButton>
                      <GateButton
                        op="setTableStorage"
                        small
                        :data-testid="`convert-${t.name}`"
                        :blocked-reason="storageBlocked(t)"
                        @click="converting = t"
                      >
                        Convert storage...
                      </GateButton>
                      <GateButton
                        op="putPartitionPolicy"
                        small
                        :blocked-reason="operationReason(t.operation)"
                        :data-testid="`policy-${t.name}`"
                        @click="policyFor = t"
                      >
                        Time partitioning...
                      </GateButton>
                      <TableTransferActions :database-name="name" :table="t" />
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
          <p v-if="tables.data.value?.moreNotShown" class="notice tone-warn" role="status">
            <span aria-hidden="true">&#9650;</span>
            <span>More tables exist than are shown here.</span>
          </p>
          <p v-if="!shardingOn" class="muted">
            Sharding is off, so tables cannot be distributed. Turn it on in the settings.
          </p>
        </QueryBoundary>
      </section>
      <TransfersPanel :database="db.name" />
    </div>
  </QueryBoundary>

  <EditDatabaseDialog v-if="dialog === 'edit' && db" :database="db" @close="dialog = null" />
  <TaskActionDialog
    v-if="dialog === 'drop' && db"
    :title="`Drop database ${db.name}`"
    confirm-label="Drop database"
    danger
    :type-to-confirm="db.name"
    :start="() => endpoints.dropDatabase(db!.name)"
    :invalidate="[keys.databases]"
    @close="dialog = null"
    @done="router.push({ name: 'databases' })"
  >
    <p class="notice tone-danger" role="alert">
      <span aria-hidden="true">&#10006;</span>
      <span>
        All tables and data of <strong><SafeName :value="db.name" :max="80" /></strong>
        <template v-if="db.stats.sizeBytes !== null"
          >({{ formatBytes(db.stats.sizeBytes) }})</template
        >
        are deleted on every server, including the replicas. This cannot be undone.
      </span>
    </p>
    <p v-if="db.ledger.mode !== 'off'" class="notice tone-danger" data-testid="drop-ledger-note">
      <span aria-hidden="true">&#10006;</span>
      <span>
        This database has a ledger. Its hash chains, blocks and digests are deleted with it, so
        nobody can verify its history afterwards, not even with a digest or a proof that you saved.
        A ledger does not protect a database from being dropped; only the audit log keeps a record
        that it happened. Download the digests you need first (Ledger page).
      </span>
    </p>
  </TaskActionDialog>
  <EnableLedgerDialog
    v-if="ledgerFor"
    :database-name="name"
    :table="ledgerFor"
    @close="ledgerFor = null"
  />
  <SetStorageDialog
    v-if="converting"
    :database-name="name"
    :table="converting"
    @close="converting = null"
  />
  <PartitionPolicyDialog
    v-if="policyFor"
    :database-name="name"
    :table="policyFor"
    @close="policyFor = null"
  />
  <DistributeTableDialog
    v-if="distributing"
    :database-name="name"
    :table="distributing"
    :candidates="distributedTables"
    @close="distributing = null"
  />
</template>

<style scoped>
.kv {
  display: grid;
  grid-template-columns: fit-content(45%) minmax(0, 1fr);
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.kv dt {
  color: var(--c-text-muted);
}
.kv dd {
  margin: 0;
}
</style>
