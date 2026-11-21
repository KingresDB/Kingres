<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Bucket map: one cell per bucket, coloured by the owning shard group, with filters by database,
// colocation group and server, and per-server bucket counts.
import { computed, ref, watch } from 'vue'
import { safeText } from '@/lib/safe-text'
import TableWrap from '@/components/TableWrap.vue'
import { useRoute, useRouter } from 'vue-router'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import BucketGrid from '@/components/BucketGrid.vue'
import NullableValue from '@/components/NullableValue.vue'
import SafeName from '@/components/SafeName.vue'
import {
  useAllBuckets,
  useBucketDistribution,
  useColocationGroups,
  useDatabases,
  useRebalanceStatus,
  useSchemas,
  useServers,
} from '@/api/queries'
import { formatBytes, formatNumber } from '@/lib/format'
import type { Bucket } from '@/api/types'

const route = useRoute()
const router = useRouter()
const databases = useDatabases()
const servers = useServers()

const sharded = computed(() => databases.data.value?.items.filter((d) => d.sharding.enabled) ?? [])
const dbName = ref<string>(typeof route.query.db === 'string' ? route.query.db : '')
const colocationId = ref<number | null>(null)
const serverFilter = ref<string>(typeof route.query.server === 'string' ? route.query.server : '')
const stateFilter = ref<'' | 'moving' | 'orphan'>('')
const lookup = ref('')
const selected = ref<Bucket | null>(null)

// Default to the first sharded database once the list is known.
watch(
  sharded,
  (list) => {
    // The list is empty until it has been read: a database named in the address (`?db=`) must survive that.
    if (list.length === 0) return
    if (!dbName.value || !list.some((d) => d.name === dbName.value))
      dbName.value = list[0]?.name ?? ''
  },
  { immediate: true },
)

// Only the hash groups (contract 0.15.0): a database that distributes schemas has one group of kind `schema` for every schema.
const colocations = useColocationGroups(() => dbName.value || null, 'hash')
watch(
  () => colocations.data.value,
  (data) => {
    const items = data?.items ?? []
    if (colocationId.value === null || !items.some((g) => g.colocationId === colocationId.value)) {
      colocationId.value = items[0]?.colocationId ?? null
    }
  },
  { immediate: true },
)

const buckets = useAllBuckets(
  () => dbName.value || null,
  () => colocationId.value,
)
const distribution = useBucketDistribution(
  () => dbName.value || null,
  () => colocationId.value ?? undefined,
)

// Distributed schemas per server group (a count and a size), and the moves of schemas that run now.
const schemas = useSchemas(
  () => dbName.value,
  () => !!dbName.value,
)
const rebalance = useRebalanceStatus()
interface GroupSchemas {
  groupId: number
  serverId: string | null
  count: number
  bytes: number
  /** Schemas whose size is not known here (null, shown as "n/a", never as 0). */
  unknown: number
}
const schemasPerGroup = computed<GroupSchemas[]>(() => {
  const byGroup = new Map<number, GroupSchemas>()
  for (const s of schemas.data.value?.items ?? []) {
    const d = s.distribution
    if (!d) continue
    const row = byGroup.get(d.shardGroupId) ?? {
      groupId: d.shardGroupId,
      serverId: d.serverId,
      count: 0,
      bytes: 0,
      unknown: 0,
    }
    row.count += 1
    if (s.sizeBytes === null) row.unknown += 1
    else row.bytes += s.sizeBytes
    byGroup.set(d.shardGroupId, row)
  }
  return [...byGroup.values()].sort((a, b) => a.groupId - b.groupId)
})
const schemaMoves = computed(
  () =>
    rebalance.data.value?.inFlightMoves?.filter(
      (m) => m.kind === 'schema' && m.databaseName === dbName.value,
    ) ?? [],
)
// `primarySchemas` is optional, and 0 for a database that distributes none: the column is for a database that does.
const hasSchemaCounts = computed(() =>
  distribution.data.value?.servers.some((s) => (s.primarySchemas ?? 0) > 0),
)

watch([dbName, serverFilter], () => {
  void router.replace({
    query: {
      ...route.query,
      db: dbName.value || undefined,
      server: serverFilter.value || undefined,
    },
  })
  selected.value = null
})

const items = computed(() => buckets.data.value?.items ?? [])
const groupIds = computed(() =>
  [
    ...new Set(
      items.value.flatMap((b) => [
        b.shardGroupId,
        ...(b.movingToShardGroupId !== null ? [b.movingToShardGroupId] : []),
      ]),
    ),
  ].sort((a, b) => a - b),
)

function dimmed(b: Bucket): boolean {
  const sid = serverFilter.value || null
  if (sid !== null && b.primaryServerId !== sid && !b.replicaServerIds.includes(sid)) return true
  if (stateFilter.value && b.state !== stateFilter.value) return true
  return false
}

const unusual = computed(() => items.value.filter((b) => b.state !== 'active'))

function findBucket(): void {
  const n = Number(lookup.value)
  selected.value = items.value.find((b) => b.bucket === n) ?? null
}
</script>

<template>
  <PageHeader
    title="Shards"
    subtitle="Which shard group owns each bucket. One cell is one bucket."
  />

  <QueryBoundary
    :loading="databases.isPending.value"
    :error="databases.error.value"
    :empty="sharded.length === 0"
    empty-title="No sharded databases"
    empty-hint="Turn on sharding for a database to see its bucket map."
    @retry="databases.refetch()"
  >
    <div class="stack-lg">
      <form class="form-row" aria-label="Filters" @submit.prevent="findBucket">
        <div class="field">
          <label for="f-db">Database</label>
          <select id="f-db" v-model="dbName">
            <option v-for="d in sharded" :key="d.name" :value="d.name">
              {{ safeText(d.name) }}
            </option>
          </select>
        </div>
        <div class="field">
          <label for="f-col">Colocation group</label>
          <select id="f-col" v-model="colocationId">
            <option
              v-for="g in colocations.data.value?.items"
              :key="g.colocationId"
              :value="g.colocationId"
            >
              {{ g.colocationId }} ({{ g.tableCount }} tables, {{ g.distributionColumnType }})
            </option>
          </select>
        </div>
        <div class="field">
          <label for="f-srv">Highlight server</label>
          <select id="f-srv" v-model="serverFilter">
            <option value="">All servers</option>
            <option v-for="s in servers.data.value?.items" :key="s.id" :value="s.id">
              {{ safeText(s.address) }}
            </option>
          </select>
        </div>
        <div class="field">
          <label for="f-state">State</label>
          <select id="f-state" v-model="stateFilter">
            <option value="">All</option>
            <option value="moving">Moving</option>
            <option value="orphan">Orphan</option>
          </select>
        </div>
        <div class="field">
          <label for="f-lookup">Find bucket number</label>
          <div class="row">
            <input
              id="f-lookup"
              v-model="lookup"
              class="input num"
              inputmode="numeric"
              style="width: 6em"
            />
            <button type="submit" class="btn">Find</button>
          </div>
        </div>
      </form>

      <QueryBoundary
        :loading="buckets.isPending.value && !!dbName && colocationId !== null"
        :error="buckets.error.value"
        :empty="items.length === 0"
        empty-title="No buckets"
        :empty-hint="
          colocationId === null
            ? 'This database has no hash-distributed tables, so there is no bucket map. Its distributed schemas are listed below.'
            : 'This colocation group has no buckets yet.'
        "
        @retry="buckets.refetch()"
      >
        <section class="card stack" aria-labelledby="map-h">
          <h2 id="map-h">Bucket map</h2>
          <p v-if="buckets.data.value?.moreNotShown" class="notice tone-warn" role="status">
            <span aria-hidden="true">&#9650;</span>
            <span>More buckets exist than are shown: the map below is incomplete.</span>
          </p>
          <p class="muted">
            Map version {{ formatNumber(buckets.data.value?.mapVersion) }},
            {{ items.length }} buckets.
          </p>
          <BucketGrid
            :buckets="items"
            :group-ids="groupIds"
            :is-dimmed="dimmed"
            :selected="selected?.bucket"
            @select="selected = $event"
          />
          <div v-if="selected" class="notice" role="status" data-testid="bucket-detail">
            <div class="stack" style="gap: var(--s-1)">
              <strong>Bucket {{ selected.bucket }}</strong>
              <span
                >Shard group {{ selected.shardGroupId }}, state {{ selected.state
                }}<template v-if="selected.movingToShardGroupId !== null">
                  (moving to group {{ selected.movingToShardGroupId }})</template
                ></span
              >
              <span
                >Primary on server {{ selected.primaryServerId ?? '-' }}, replicas on
                {{
                  selected.replicaServerIds.length
                    ? selected.replicaServerIds.map((s) => `server ${s}`).join(', ')
                    : 'none'
                }}, <NullableValue :value="selected.sizeBytes" :format="formatBytes"
              /></span>
            </div>
          </div>
          <p v-else-if="lookup" class="muted">No bucket with that number in this group.</p>
        </section>

        <section v-if="unusual.length" class="card" aria-labelledby="odd-h">
          <h2 id="odd-h">Moving and orphan buckets</h2>
          <TableWrap>
            <table class="table">
              <caption class="sr-only">
                Buckets that are moving or orphaned
              </caption>
              <thead>
                <tr>
                  <th scope="col" class="num">Bucket</th>
                  <th scope="col">State</th>
                  <th scope="col">Group</th>
                  <th scope="col">Moving to</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="b in unusual" :key="b.bucket">
                  <td class="num">{{ b.bucket }}</td>
                  <td>{{ b.state }}</td>
                  <td>{{ b.shardGroupId }}</td>
                  <td>{{ b.movingToShardGroupId ?? '-' }}</td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
        </section>
      </QueryBoundary>

      <section v-if="schemasPerGroup.length" class="card" aria-labelledby="schemas-group-h">
        <h2 id="schemas-group-h">Distributed schemas per server group</h2>
        <TableWrap>
          <table class="table" data-testid="schemas-per-group">
            <caption class="sr-only">
              Distributed schemas, their number and size, per server group
            </caption>
            <thead>
              <tr>
                <th scope="col">Server group</th>
                <th scope="col">Server</th>
                <th scope="col" class="num">Schemas</th>
                <th scope="col" class="num">Size</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="g in schemasPerGroup" :key="g.groupId">
                <th scope="row">Group {{ g.groupId }}</th>
                <td>
                  <SafeName v-if="g.serverId" :value="g.serverId" :max="60" />
                  <template v-else>-</template>
                </td>
                <td class="num">{{ g.count }}</td>
                <td class="num">
                  <NullableValue
                    :value="g.unknown === g.count ? null : g.bytes"
                    :format="formatBytes"
                  />
                  <span v-if="g.unknown > 0 && g.unknown < g.count" class="muted">
                    (and {{ g.unknown }} not known)</span
                  >
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </section>

      <section v-if="schemaMoves.length" class="card" aria-labelledby="schema-moves-h">
        <h2 id="schema-moves-h">Schema moves in flight</h2>
        <TableWrap>
          <table class="table" data-testid="schema-moves">
            <caption class="sr-only">
              Moves of distributed schemas that run now
            </caption>
            <thead>
              <tr>
                <th scope="col">Schema</th>
                <th scope="col">From</th>
                <th scope="col">To</th>
                <th scope="col" class="num">Done</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="m in schemaMoves" :key="`${m.colocationId}-${m.schemaName}`">
                <th scope="row" class="mono">
                  <SafeName :value="m.schemaName ?? '?'" :max="80" />
                </th>
                <td>Group {{ m.fromShardGroupId }}</td>
                <td>Group {{ m.toShardGroupId }}</td>
                <td class="num">{{ Math.round(m.percent) }}%</td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </section>

      <section class="card" aria-labelledby="per-server-h">
        <h2 id="per-server-h">Buckets per server</h2>
        <QueryBoundary
          :loading="distribution.isPending.value && !!dbName"
          :error="distribution.error.value"
        >
          <TableWrap>
            <table class="table">
              <caption class="sr-only">
                Primary and replica buckets per server
              </caption>
              <thead>
                <tr>
                  <th scope="col">Server</th>
                  <th scope="col" class="num">Primary buckets</th>
                  <th v-if="hasSchemaCounts" scope="col" class="num">Of them schemas</th>
                  <th scope="col" class="num">Primary size</th>
                  <th scope="col" class="num">Replica buckets</th>
                  <th scope="col" class="num">Replica size</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="s in distribution.data.value?.servers" :key="s.serverId">
                  <th scope="row">
                    <button
                      type="button"
                      class="btn-link btn"
                      :aria-pressed="serverFilter === s.serverId"
                      @click="serverFilter = serverFilter === s.serverId ? '' : s.serverId"
                    >
                      Server {{ s.serverId }}
                    </button>
                  </th>
                  <td class="num">{{ s.primaryBuckets }}</td>
                  <td v-if="hasSchemaCounts" class="num">{{ s.primarySchemas ?? 0 }}</td>
                  <td class="num">
                    <NullableValue :value="s.primarySizeBytes" :format="formatBytes" />
                  </td>
                  <td class="num">{{ s.replicaBuckets }}</td>
                  <td class="num">
                    <NullableValue :value="s.replicaSizeBytes" :format="formatBytes" />
                  </td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
        </QueryBoundary>
      </section>
    </div>
  </QueryBoundary>
</template>
