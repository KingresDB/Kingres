<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { computed, ref } from 'vue'
import TableWrap from '@/components/TableWrap.vue'
import { RouterLink } from 'vue-router'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import NullableValue from '@/components/NullableValue.vue'
import NotAvailable from '@/components/NotAvailable.vue'
import SafeName from '@/components/SafeName.vue'
import { isAddressableSegment } from '@/lib/path-segment'
import CreateDatabaseDialog from '@/components/CreateDatabaseDialog.vue'
import { useDatabaseCreateOptions, useDatabases } from '@/api/queries'
import { DATABASE_STATUS, DURABILITY_LABEL, LEDGER_MODE_LABEL, LEDGER_STATE } from '@/lib/status'
import { formatBytes, formatNumber } from '@/lib/format'
import { modeOf } from '@/lib/sharding-mode'

const databases = useDatabases()
// Loaded ahead of the dialog, so that it opens with its choices and defaults in place.
useDatabaseCreateOptions()
const rows = computed(() => databases.data.value?.items ?? [])
const creating = ref(false)
</script>

<template>
  <PageHeader
    title="Databases"
    subtitle="Databases with their sharding, replication and ledger settings."
  >
    <template #actions>
      <GateButton op="createDatabase" variant="primary" @click="creating = true"
        >Create database</GateButton
      >
    </template>
  </PageHeader>

  <QueryBoundary
    :loading="databases.isPending.value"
    :error="databases.error.value"
    :empty="rows.length === 0"
    empty-title="No databases yet"
    empty-hint="Create a database to choose its sharding, replication and ledger settings."
    @retry="databases.refetch()"
  >
    <TableWrap>
      <table class="table">
        <caption class="sr-only">
          Databases
        </caption>
        <thead>
          <tr>
            <th scope="col">Name</th>
            <th scope="col">Status</th>
            <th scope="col">Owner</th>
            <th scope="col">Sharding</th>
            <th scope="col">Replication</th>
            <th scope="col">Ledger</th>
            <th scope="col" class="num">Size</th>
            <th scope="col" class="num">Tables</th>
            <th scope="col" class="num">Txn/s</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="d in rows" :key="d.name">
            <th scope="row">
              <RouterLink
                v-if="isAddressableSegment(d.name)"
                :to="{ name: 'database', params: { name: d.name } }"
                ><SafeName :value="d.name"
              /></RouterLink>
              <template v-else>
                <SafeName :value="d.name" />
                <div class="muted">
                  This name cannot be opened by address; use SQL to manage it.
                </div>
              </template>
            </th>
            <td><StatusBadge :meta="DATABASE_STATUS[d.status]" /></td>
            <td><NullableValue :value="d.general.owner" safe /></td>
            <td>
              <template v-if="d.sharding.enabled">
                <template v-if="modeOf(d.sharding) === 'schema'">On, schemas</template>
                <template v-else
                  >On, <template v-if="modeOf(d.sharding) === 'both'">rows and schemas, </template
                  ><NullableValue :value="d.sharding.bucketCount" :format="(n) => `${n} buckets`"
                /></template>
              </template>
              <template v-else>Off</template>
              <div v-if="d.rebalanceRequired" class="muted">Rebalance needed</div>
            </td>
            <td>
              <NullableValue
                :value="d.replication.durability"
                :format="(v) => DURABILITY_LABEL[v]"
              />
              <div class="muted">{{ d.replication.replicaCount }} replica(s)</div>
              <div
                v-if="
                  d.health.underReplicatedBuckets === null || d.health.underReplicatedBuckets > 0
                "
                class="muted"
              >
                <NullableValue :value="d.health.underReplicatedBuckets" :format="formatNumber" />
                under-replicated
              </div>
            </td>
            <td>
              <NotAvailable v-if="d.ledger.mode === null" />
              <template v-else-if="d.ledger.mode === 'off'">Off</template>
              <template v-else>
                {{ LEDGER_MODE_LABEL[d.ledger.mode] }}
                <div>
                  <StatusBadge
                    v-if="d.ledgerSummary.verificationState"
                    :meta="LEDGER_STATE[d.ledgerSummary.verificationState]"
                  />
                  <NotAvailable v-else />
                  <span v-if="d.ledgerSummary.lastVerifiedBlockHeight !== null" class="muted">
                    block {{ formatNumber(d.ledgerSummary.lastVerifiedBlockHeight) }}
                  </span>
                </div>
              </template>
            </td>
            <td class="num"><NullableValue :value="d.stats.sizeBytes" :format="formatBytes" /></td>
            <td class="num"><NullableValue :value="d.stats.tableCount" /></td>
            <td class="num">
              <NullableValue
                :value="d.stats.transactionsPerSecond"
                :format="(n) => formatNumber(n, 1)"
              />
            </td>
          </tr>
        </tbody>
      </table>
    </TableWrap>
  </QueryBoundary>

  <CreateDatabaseDialog v-if="creating" @close="creating = false" />
</template>
