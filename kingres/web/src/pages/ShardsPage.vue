<!-- SPDX-License-Identifier: PostgreSQL -->
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
import {
  useAllBuckets,
  useBucketDistribution,
  useColocationGroups,
  useDatabases,
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
    if (!dbName.value || !list.some((d) => d.name === dbName.value))
      dbName.value = list[0]?.name ?? ''
  },
  { immediate: true },
)

const colocations = useColocationGroups(() => dbName.value || null)
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
        :loading="buckets.isPending.value && !!dbName"
        :error="buckets.error.value"
        :empty="items.length === 0"
        empty-title="No buckets"
        empty-hint="This colocation group has no buckets yet."
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
