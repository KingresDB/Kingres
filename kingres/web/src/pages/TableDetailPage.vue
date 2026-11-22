<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// One table of a database (contract 0.14.0, increment 9.7 of phase 9): its classes, rows, size and distribution, its columns, the
// changes that can be made to its distribution (distribute, undistribute, alter) and, for a table the user partitioned (RANGE or
// LIST), its partitions with create, attach, detach and drop. Every change is a task: the page shows the one that runs, with
// Cancel, and the one that failed, in words. A role that may not change a table sees the buttons disabled with the reason.
import { computed, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import GateButton from '@/components/GateButton.vue'
import NullableValue from '@/components/NullableValue.vue'
import SafeName from '@/components/SafeName.vue'
import TableWrap from '@/components/TableWrap.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import DistributeTableDialog from '@/components/DistributeTableDialog.vue'
import UndistributeTableDialog from '@/components/tables/UndistributeTableDialog.vue'
import AlterDistributionDialog from '@/components/tables/AlterDistributionDialog.vue'
import CreatePartitionDialog from '@/components/tables/CreatePartitionDialog.vue'
import AttachPartitionDialog from '@/components/tables/AttachPartitionDialog.vue'
import PartitionActionDialog from '@/components/tables/PartitionActionDialog.vue'
import TableTasksPanel from '@/components/tables/TableTasksPanel.vue'
import { useDatabase, useTable, useTablePartitions, useTables, useTasks } from '@/api/queries'
import { formatBytes, formatNumber } from '@/lib/format'
import { TABLE_TASKS_LIMIT, runningTaskOfTable } from '@/lib/tables'
import type { TablePartition, TableClass } from '@/api/types'

const props = defineProps<{ name: string; schema: string; table: string }>()
const database = useDatabase(() => props.name)
const detail = useTable(
  () => props.name,
  () => props.schema,
  () => props.table,
)
const all = useTables(() => props.name)
const tasks = useTasks(TABLE_TASKS_LIMIT)
const t = computed(() => detail.data.value)

const CLASS_LABEL: Record<TableClass, string> = {
  plain: 'Plain',
  distributed: 'Distributed',
  reference: 'Reference',
  ledger: 'Ledger',
}
const shardingOn = computed(() => database.data.value?.sharding.enabled === true)
const isPlain = computed(() => t.value?.classes.includes('plain') === true)
const isDistributed = computed(() => t.value?.classes.includes('distributed') === true)
const isReference = computed(() => t.value?.classes.includes('reference') === true)
const partitioned = computed(() => t.value?.partitioning != null)
/** The tables of this database that can be the colocation target of this one (distributed, not itself). */
const others = computed(() =>
  (all.data.value?.items ?? []).filter(
    (x) =>
      x.classes.includes('distributed') && !(x.schema === props.schema && x.name === props.table),
  ),
)
const distributedAll = computed(() =>
  (all.data.value?.items ?? []).filter((x) => x.classes.includes('distributed')),
)

const partitions = useTablePartitions(
  () => props.name,
  () => props.schema,
  () => props.table,
  partitioned,
)
const partitionRows = computed(() => partitions.data.value?.items ?? [])

/** The change that is running now: it holds the table, so no other change starts until it ends. */
const holding = computed(() =>
  runningTaskOfTable(tasks.data.value?.items ?? [], props.name, props.schema, props.table),
)
const heldReason = computed(() =>
  holding.value ? `A change of this table is running (task ${holding.value.id}).` : '',
)
const distributeReason = computed(
  () => heldReason.value || (shardingOn.value ? '' : 'Turn on sharding for this database first.'),
)
const undistributeReason = computed(
  () =>
    heldReason.value ||
    (partitioned.value ? 'A user-partitioned table cannot be undistributed in this version.' : ''),
)
const alterReason = computed(
  () =>
    heldReason.value ||
    (partitioned.value ? 'A user-partitioned table cannot be altered in this version.' : ''),
)

const dialog = ref<
  'distribute' | 'undistribute' | 'alter' | 'create' | 'attach' | 'detach' | 'drop' | null
>(null)
const picked = ref<TablePartition | null>(null)
function openPartition(kind: 'detach' | 'drop', p: TablePartition): void {
  picked.value = p
  dialog.value = kind
}
function closeDialog(): void {
  dialog.value = null
  picked.value = null
}
// The page is reused when the address changes to another table: a dialog of the old one must not act on the new one.
watch(
  () => [props.name, props.schema, props.table],
  () => closeDialog(),
)
const title = computed(() => `Table ${props.schema}.${props.table}`)
</script>

<template>
  <PageHeader :title="title">
    <template #actions>
      <GateButton
        v-if="isPlain"
        op="distributeTable"
        data-testid="distribute-table"
        :disabled="!t"
        :blocked-reason="distributeReason"
        @click="dialog = 'distribute'"
      >
        Distribute table
      </GateButton>
      <GateButton
        v-if="isDistributed || isReference"
        op="undistributeTable"
        data-testid="undistribute-table"
        :blocked-reason="undistributeReason"
        @click="dialog = 'undistribute'"
      >
        Undistribute table
      </GateButton>
      <GateButton
        v-if="isDistributed && t?.distribution?.method === 'hash'"
        op="alterTableDistribution"
        data-testid="alter-distribution"
        :blocked-reason="alterReason"
        @click="dialog = 'alter'"
      >
        Change the distribution
      </GateButton>
    </template>
  </PageHeader>
  <p>
    <RouterLink :to="{ name: 'database', params: { name } }"
      >&larr; Database <SafeName :value="name" kind="name" :max="80"
    /></RouterLink>
  </p>

  <QueryBoundary
    :loading="detail.isPending.value"
    :error="detail.error.value"
    @retry="detail.refetch()"
  >
    <div v-if="t" class="stack-lg">
      <section class="card stack" aria-labelledby="table-facts-h" data-testid="table-facts">
        <h2 id="table-facts-h">The table</h2>
        <dl class="kv">
          <dt>Class</dt>
          <dd>
            <span class="row" style="gap: var(--s-1)">
              <span v-for="c in t.classes" :key="c" class="badge tone-neutral">{{
                CLASS_LABEL[c]
              }}</span>
              <span v-if="partitioned" class="badge tone-neutral" data-testid="partitioned-badge"
                >Partitioned by the user</span
              >
            </span>
          </dd>
          <dt>Rows (estimate)</dt>
          <dd><NullableValue :value="t.estimatedRows" :format="formatNumber" /></dd>
          <dt>Size</dt>
          <dd><NullableValue :value="t.sizeBytes" :format="formatBytes" /></dd>
          <template v-if="t.distribution">
            <dt>Distribution</dt>
            <dd data-testid="table-distribution">
              <template v-if="t.distribution.method === 'hash'">
                Hash of
                <code><SafeName :value="t.distribution.column ?? '-'" :max="60" /></code>, in
                {{ t.distribution.bucketCount ?? '-' }} buckets, colocation group
                {{ t.distribution.colocationId ?? '-' }}
              </template>
              <template v-else>A full copy on every shard group</template>
            </dd>
          </template>
          <template v-else>
            <dt>Home group</dt>
            <dd data-testid="table-home">
              <template v-if="t.homeShardGroupId !== null">Group {{ t.homeShardGroupId }}</template>
              <template v-else>-</template>
            </dd>
          </template>
          <template v-if="t.partitioning">
            <dt>Partitions</dt>
            <dd data-testid="table-partitioning">
              {{ t.partitioning.method === 'range' ? 'RANGE' : 'LIST' }} on
              <code><SafeName :value="t.partitioning.key" :max="60" /></code>,
              {{ t.partitioning.partitionCount }}
              {{ t.partitioning.partitionCount === 1 ? 'partition' : 'partitions' }}
            </dd>
          </template>
          <template v-if="t.ledger">
            <dt>Ledger</dt>
            <dd>{{ t.ledger.mode === 'appendOnly' ? 'Append only' : 'Updatable' }}</dd>
          </template>
        </dl>
      </section>

      <TableTasksPanel :database="name" :schema="schema" :table="table" />

      <section class="stack" aria-labelledby="table-columns-h">
        <h2 id="table-columns-h">Columns</h2>
        <TableWrap>
          <table class="table">
            <caption class="sr-only">
              Columns of the table
            </caption>
            <thead>
              <tr>
                <th scope="col">Column</th>
                <th scope="col">Type</th>
                <th scope="col">Can be empty</th>
                <th v-if="isPlain" scope="col">Can be the distribution column</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="c in detail.data.value?.columns ?? []" :key="c.name">
                <th scope="row" class="mono"><SafeName :value="c.name" :max="80" /></th>
                <td><SafeName :value="c.type" kind="text" :max="60" /></td>
                <td>{{ c.nullable ? 'Yes' : 'No' }}</td>
                <td v-if="isPlain">
                  <StatusBadge
                    :meta="
                      c.canBeDistributionColumn
                        ? { tone: 'ok', icon: '✓', label: 'Yes' }
                        : { tone: 'neutral', icon: '○', label: 'No' }
                    "
                  />
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </section>

      <section
        v-if="isDistributed"
        class="stack"
        aria-labelledby="table-partitions-h"
        data-testid="table-partitions"
      >
        <div class="row-between">
          <h2 id="table-partitions-h">Partitions</h2>
          <span v-if="partitioned" class="row">
            <GateButton
              op="createTablePartition"
              small
              data-testid="create-partition"
              :blocked-reason="heldReason"
              @click="dialog = 'create'"
            >
              Create partition
            </GateButton>
            <GateButton
              op="attachTablePartition"
              small
              data-testid="attach-partition"
              :blocked-reason="heldReason"
              @click="dialog = 'attach'"
            >
              Attach a table
            </GateButton>
          </span>
        </div>
        <p v-if="!partitioned" class="muted" data-testid="not-partitioned">
          This table is not partitioned by the user. A table that was created as a RANGE or LIST
          partitioned table has partitions here once it is distributed.
        </p>
        <QueryBoundary
          v-else
          :loading="partitions.isPending.value"
          :error="partitions.error.value"
          :empty="partitionRows.length === 0"
          empty-title="No partitions"
          empty-hint="Create a partition, or attach a distributed table of this colocation group."
          @retry="partitions.refetch()"
        >
          <TableWrap>
            <table class="table" data-testid="partition-list">
              <caption class="sr-only">
                Partitions of the table
              </caption>
              <thead>
                <tr>
                  <th scope="col">Partition</th>
                  <th scope="col">Bound</th>
                  <th scope="col" class="num">Leaves</th>
                  <th scope="col" class="num">Size on this server</th>
                  <th scope="col">Actions</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="p in partitionRows" :key="p.name" :data-testid="`partition-${p.name}`">
                  <th scope="row" class="mono">
                    <SafeName :value="p.name" :max="80" />
                    <span v-if="p.isDefault" class="badge tone-neutral"> Default</span>
                  </th>
                  <td class="mono">
                    <SafeName :value="p.bound" kind="text" :max="200" />
                  </td>
                  <td class="num">{{ formatNumber(p.leafCount) }}</td>
                  <td class="num"><NullableValue :value="p.sizeBytes" :format="formatBytes" /></td>
                  <td>
                    <div class="row">
                      <GateButton
                        op="detachTablePartition"
                        small
                        :blocked-reason="heldReason"
                        :data-testid="`detach-${p.name}`"
                        @click="openPartition('detach', p)"
                      >
                        Detach
                      </GateButton>
                      <GateButton
                        op="dropTablePartition"
                        small
                        variant="danger"
                        :blocked-reason="heldReason"
                        :data-testid="`drop-${p.name}`"
                        @click="openPartition('drop', p)"
                      >
                        Drop
                      </GateButton>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
        </QueryBoundary>
      </section>
    </div>
  </QueryBoundary>

  <DistributeTableDialog
    v-if="dialog === 'distribute' && t"
    :database-name="name"
    :table="t"
    :candidates="distributedAll"
    @close="closeDialog"
  />
  <UndistributeTableDialog
    v-if="dialog === 'undistribute' && t"
    :database-name="name"
    :table="t"
    @close="closeDialog"
  />
  <AlterDistributionDialog
    v-if="dialog === 'alter' && t"
    :database-name="name"
    :table="t"
    :candidates="others"
    @close="closeDialog"
  />
  <CreatePartitionDialog
    v-if="dialog === 'create' && t"
    :database-name="name"
    :table="t"
    @close="closeDialog"
  />
  <AttachPartitionDialog
    v-if="dialog === 'attach' && t"
    :database-name="name"
    :table="t"
    :candidates="others"
    @close="closeDialog"
  />
  <PartitionActionDialog
    v-if="(dialog === 'detach' || dialog === 'drop') && t && picked"
    :database-name="name"
    :table="t"
    :partition="picked"
    :action="dialog"
    @close="closeDialog"
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
