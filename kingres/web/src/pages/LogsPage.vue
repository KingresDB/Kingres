<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Cluster logs, or (with `local`) the logs of this server only.
import { ref } from 'vue'
import SafeName from '@/components/SafeName.vue'
import TableWrap from '@/components/TableWrap.vue'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import PartialNotice from '@/components/PartialNotice.vue'
import CursorResetNotice from '@/components/CursorResetNotice.vue'
import { useCompletenessAnnouncement } from '@/composables/useCompletenessAnnouncement'
import { endpoints } from '@/api/endpoints'
import { keys, usePagedQuery } from '@/api/queries'
import { formatDateTime } from '@/lib/format'
import type { LogComponent, LogLevel } from '@/api/types'

const props = withDefaults(defineProps<{ local?: boolean }>(), { local: false })

const level = ref<'' | LogLevel>('')
const component = ref<'' | LogComponent>('')
const q = ref('')
const serverId = ref('')

const { query, items, completeness, cursorReset, dismissCursorReset } = usePagedQuery(
  () => [...keys.logs, props.local, level.value, component.value, q.value, serverId.value],
  (cursor) => {
    const filter = {
      cursor,
      limit: 100,
      level: level.value || undefined,
      component: component.value || undefined,
      q: q.value || undefined,
    }
    return props.local
      ? endpoints.listThisServerLogs(filter)
      : endpoints.listLogs({ ...filter, serverId: serverId.value || undefined })
  },
  10_000,
)
useCompletenessAnnouncement(completeness, 'list of log lines')
const LEVELS: LogLevel[] = ['debug', 'info', 'warning', 'error']
const COMPONENTS: LogComponent[] = [
  'api',
  'agent',
  'manager',
  'router',
  'etcd',
  'patroni',
  'postgres',
  'metrics',
]
const LEVEL_ICON: Record<LogLevel, string> = { debug: '·', info: 'ⓘ', warning: '▲', error: '✖' }
</script>

<template>
  <PageHeader
    :title="local ? 'Local logs' : 'Logs'"
    :subtitle="local ? 'Log lines of this server only.' : 'Log lines of all servers, newest first.'"
  />
  <form
    class="row"
    style="align-items: flex-end; gap: var(--s-4); margin-bottom: var(--s-3)"
    @submit.prevent
  >
    <div class="field">
      <label for="lg-level">Minimum level</label
      ><select id="lg-level" v-model="level">
        <option value="">All</option>
        <option v-for="l in LEVELS" :key="l" :value="l">{{ l }}</option>
      </select>
    </div>
    <div class="field">
      <label for="lg-comp">Component</label
      ><select id="lg-comp" v-model="component">
        <option value="">All</option>
        <option v-for="c in COMPONENTS" :key="c" :value="c">{{ c }}</option>
      </select>
    </div>
    <div v-if="!local" class="field">
      <label for="lg-srv">Server id</label
      ><input id="lg-srv" v-model="serverId" class="input" placeholder="10.0.0.11" />
    </div>
    <div class="field">
      <label for="lg-q">Contains text</label
      ><input id="lg-q" v-model="q" class="input" type="search" />
    </div>
  </form>
  <CursorResetNotice :shown="cursorReset" @dismiss="dismissCursorReset" />
  <PartialNotice
    :completeness="completeness"
    consequence="Log lines of these servers are not shown."
  />
  <QueryBoundary
    :loading="query.isPending.value"
    :error="query.error.value"
    :empty="items.length === 0"
    :empty-title="
      completeness.partial ? 'No log lines from the servers that answered' : 'No log lines'
    "
    :empty-hint="
      completeness.partial
        ? 'This does not mean there are none: the servers named above did not answer, so their lines are not shown.'
        : 'Nothing matches these filters.'
    "
    @retry="query.refetch()"
  >
    <TableWrap>
      <table class="table">
        <caption class="sr-only">
          Log lines
        </caption>
        <thead>
          <tr>
            <th scope="col">Time</th>
            <th scope="col">Level</th>
            <th scope="col">Component</th>
            <th scope="col">Server</th>
            <th scope="col">Message</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(l, i) in items" :key="i">
            <td class="mono">{{ formatDateTime(l.timestamp) }}</td>
            <td>
              <span
                class="badge"
                :class="
                  l.level === 'error'
                    ? 'tone-danger'
                    : l.level === 'warning'
                      ? 'tone-warn'
                      : 'tone-neutral'
                "
                ><span aria-hidden="true">{{ LEVEL_ICON[l.level] }}</span> {{ l.level }}</span
              >
            </td>
            <td>{{ l.component }}</td>
            <td>
              <SafeName v-if="l.serverId" :value="l.serverId" kind="text" :max="60" />
              <template v-else>-</template>
            </td>
            <td class="mono"><SafeName :value="l.message" kind="text" :max="400" multiline /></td>
          </tr>
        </tbody>
      </table>
    </TableWrap>
    <button
      v-if="query.hasNextPage.value"
      type="button"
      class="btn"
      :disabled="query.isFetchingNextPage.value"
      @click="query.fetchNextPage()"
    >
      Load more
    </button>
  </QueryBoundary>
</template>
