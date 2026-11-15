<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The Parameters tab: every PostgreSQL setting with its value on every server, the level the value comes from and
// whether a restart is pending. The filters live in the address (so that the dashboard can link to them), the list is
// paged by cursor, and managed and refused settings are listed read-only with the reason.
import { computed, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import SafeName from '@/components/SafeName.vue'
import TableWrap from '@/components/TableWrap.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import PartialNotice from '@/components/PartialNotice.vue'
import CursorResetNotice from '@/components/CursorResetNotice.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import ConfigValue from './ConfigValue.vue'
import { useConfigParameters } from '@/api/queries'
import { useAnnounce } from '@/composables/useAnnounce'
import { useCompletenessAnnouncement } from '@/composables/useCompletenessAnnouncement'
import {
  CONFIG_SERVER_STATE,
  NO_FILTERS,
  TOP_CATEGORIES,
  filtersFromQuery,
  hasFilters,
  queryFromFilters,
  serverFilters,
  sourceWords,
  type ParameterFilters,
} from '@/lib/config'
import { safeText } from '@/lib/safe-text'

const emit = defineEmits<{ edit: [name: string] }>()
const route = useRoute()
const router = useRouter()
const announce = useAnnounce()

const filters = computed(() => filtersFromQuery(route.query))
/** The keys of the address that belong to the filters (the others, the tab, stay when a filter changes). */
const KEYS = Object.keys(NO_FILTERS)

/** Changes some filters in the address; the other keys (the tab) stay. */
function change(patch: Partial<ParameterFilters>): void {
  const next = queryFromFilters({ ...filters.value, ...patch })
  const kept = Object.fromEntries(
    Object.entries(route.query).filter(([k]) => !KEYS.includes(k)),
  ) as Record<string, string>
  void router.replace({ query: { ...kept, ...next } })
}

// The search box is debounced: the address changes when the typing pauses.
const search = ref(filters.value.q)
let timer: ReturnType<typeof setTimeout> | undefined
watch(search, (v) => {
  clearTimeout(timer)
  timer = setTimeout(() => {
    if (v !== filters.value.q) change({ q: v })
  }, 300)
})
watch(
  () => filters.value.q,
  (v) => {
    if (v !== search.value) search.value = v
  },
)

const { query, items, completeness, cursorReset, dismissCursorReset } = useConfigParameters(() =>
  serverFilters(filters.value),
)
useCompletenessAnnouncement(completeness, 'list of parameters')

watch(
  () => items.value.length,
  (n, before) => {
    if (n === before) return
    announce(
      `${n} ${n === 1 ? 'parameter' : 'parameters'} shown${query.hasNextPage.value ? '; more can be loaded' : ''}.`,
    )
  },
)

const showAll = computed(() => filters.value.all || filters.value.kind !== '')
const lead = computed(() => hasFilters(filters.value))
function clear(): void {
  search.value = ''
  change({ ...NO_FILTERS })
}

const KIND_WORDS = {
  managed: { icon: '⚿', label: 'Managed by KingresDB' },
  refused: { icon: '⊘', label: 'Not settable from the console' },
} as const
</script>

<template>
  <div class="stack">
    <form class="form-row" role="search" aria-label="Filter the parameters" @submit.prevent>
      <div class="field">
        <label for="cfg-q">Name or description</label>
        <input id="cfg-q" v-model="search" class="input" type="search" autocomplete="off" />
      </div>
      <div class="field">
        <label for="cfg-category">Category</label>
        <select
          id="cfg-category"
          :value="filters.category"
          @change="change({ category: ($event.target as HTMLSelectElement).value })"
        >
          <option value="">All categories</option>
          <option v-for="c in TOP_CATEGORIES" :key="c" :value="c">{{ c }}</option>
        </select>
      </div>
      <fieldset class="toggles">
        <legend>Show only</legend>
        <label class="check"
          ><input
            type="checkbox"
            :checked="filters.changed"
            @change="change({ changed: ($event.target as HTMLInputElement).checked })"
          />
          Changed (has an override)</label
        >
        <label class="check"
          ><input
            type="checkbox"
            :checked="filters.differing"
            @change="change({ differing: ($event.target as HTMLInputElement).checked })"
          />
          Differing between servers</label
        >
        <label class="check"
          ><input
            type="checkbox"
            :checked="filters.pendingRestart"
            @change="change({ pendingRestart: ($event.target as HTMLInputElement).checked })"
          />
          Pending restart</label
        >
      </fieldset>
      <label class="check show-all"
        ><input
          type="checkbox"
          :checked="showAll"
          :disabled="filters.kind !== ''"
          @change="change({ all: ($event.target as HTMLInputElement).checked })"
        />
        Show managed and refused</label
      >
    </form>
    <p v-if="filters.state || filters.kind" class="muted" data-testid="active-filter">
      <template v-if="filters.state">State: {{ safeText(filters.state, 40) }}. </template>
      <template v-if="filters.kind">Kind: {{ safeText(filters.kind, 40) }}.</template>
      <button type="button" class="btn-link" @click="change({ state: '', kind: '' })">
        Remove
      </button>
    </p>

    <CursorResetNotice :shown="cursorReset" @dismiss="dismissCursorReset" />
    <PartialNotice
      :completeness="completeness"
      consequence="The values of these servers are shown as not known."
    />

    <QueryBoundary
      :loading="query.isPending.value"
      :error="query.error.value"
      :empty="items.length === 0"
      :empty-title="
        completeness.partial
          ? 'No parameter from the servers that answered'
          : 'No parameter matches these filters'
      "
      :empty-hint="
        lead ? 'Change or clear the filters to see more.' : 'The servers did not list a parameter.'
      "
      @retry="query.refetch()"
    >
      <template #empty-action>
        <button v-if="lead" type="button" class="btn btn-small" @click="clear">
          Clear filters
        </button>
      </template>
      <TableWrap>
        <table class="table cfg-table">
          <caption class="sr-only">
            PostgreSQL parameters with their value on every server
          </caption>
          <thead>
            <tr>
              <th scope="col">Parameter</th>
              <th scope="col">Value on each server</th>
              <th scope="col">Default</th>
              <th scope="col">Range</th>
              <th scope="col">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="p in items" :key="p.name" :data-testid="`param-${p.name}`">
              <th scope="row" data-label="Parameter">
                <code class="name"><SafeName :value="p.name" kind="text" :max="80" /></code>
                <div class="muted">
                  <SafeName :value="p.category" kind="text" :max="80" />
                </div>
                <div class="muted desc">
                  <SafeName :value="p.description" kind="text" :max="200" />
                </div>
                <div class="chips">
                  <span v-if="p.restartRequired" class="badge tone-info"
                    ><span aria-hidden="true">&#8635;</span> restart</span
                  >
                  <span v-if="p.kind !== 'settable'" class="badge tone-neutral"
                    ><span aria-hidden="true">{{ KIND_WORDS[p.kind].icon }}</span>
                    {{ KIND_WORDS[p.kind].label }}</span
                  >
                  <span v-if="p.differs" class="badge tone-warn" data-testid="differs"
                    ><span aria-hidden="true">&#8800;</span> differs between servers</span
                  >
                  <span v-if="p.clusterOnly" class="badge tone-neutral" :title="p.clusterOnly"
                    ><span aria-hidden="true">&#9679;</span> equal on every server</span
                  >
                </div>
                <p v-if="p.reason" class="muted reason" data-testid="reason">
                  <SafeName :value="p.reason" kind="text" :max="300" />
                </p>
              </th>
              <td data-label="Value on each server">
                <ul class="plain">
                  <li v-for="v in p.servers" :key="v.serverId" class="server-value">
                    <span class="muted"
                      >Server <SafeName :value="v.serverId" kind="text" :max="40"
                    /></span>
                    <ConfigValue :value="v.value" :hidden="p.hidden" />
                    <span v-if="!p.hidden && v.state !== 'unknown'" class="muted"
                      >({{ sourceWords(v.source) }})</span
                    >
                    <StatusBadge
                      v-if="v.state !== 'applied'"
                      :meta="CONFIG_SERVER_STATE[v.state]"
                    />
                    <span v-if="v.pendingValue !== null" class="muted"
                      >&rarr; <ConfigValue :value="v.pendingValue" /> after a restart</span
                    >
                    <div v-if="v.error" class="muted error-text">
                      <SafeName :value="v.error" kind="text" :max="300" multiline />
                    </div>
                  </li>
                </ul>
              </td>
              <td data-label="Default">
                <ConfigValue :value="p.defaultValue" :hidden="p.hidden" />
              </td>
              <td data-label="Range">
                <div class="muted">
                  {{ p.type }}<template v-if="p.unit"> in {{ p.unit }}</template>
                </div>
                <div v-if="p.min !== null || p.max !== null">
                  {{ p.min ?? '' }} to {{ p.max ?? '' }}
                </div>
                <details v-if="p.enumValues">
                  <summary>{{ p.enumValues.length }} values</summary>
                  <ul class="plain">
                    <li v-for="e in p.enumValues" :key="e"><ConfigValue :value="e" /></li>
                  </ul>
                </details>
              </td>
              <td data-label="Actions">
                <div class="row">
                  <GateButton
                    v-if="p.kind === 'settable'"
                    op="applyConfigChange"
                    small
                    :aria-label="`Edit ${safeText(p.name, 80)}`"
                    @click="emit('edit', p.name)"
                    >Edit</GateButton
                  >
                  <RouterLink
                    class="btn btn-small"
                    :aria-label="`History of ${safeText(p.name, 80)}`"
                    :to="{ name: 'configuration', query: { tab: 'changes', name: p.name } }"
                    >History</RouterLink
                  >
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </TableWrap>
      <div class="row">
        <button
          v-if="query.hasNextPage.value"
          type="button"
          class="btn"
          :disabled="query.isFetchingNextPage.value"
          @click="query.fetchNextPage()"
        >
          Load more
        </button>
        <span class="muted" data-testid="shown-count"
          >{{ items.length }} {{ items.length === 1 ? 'parameter' : 'parameters' }} shown</span
        >
      </div>
    </QueryBoundary>
  </div>
</template>

<style scoped>
.toggles {
  border: 0;
  padding: 0;
  margin: 0;
  display: flex;
  flex-wrap: wrap;
  gap: var(--s-1) var(--s-3);
  align-items: center;
}
.toggles legend {
  font-weight: 600;
  padding: 0;
  margin-bottom: var(--s-1);
}
.plain {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--s-1);
}
.chips {
  display: flex;
  flex-wrap: wrap;
  gap: var(--s-1);
  margin-top: var(--s-1);
}
.server-value {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 0 var(--s-2);
}
.reason,
.desc {
  margin: var(--s-1) 0 0;
  max-width: 48ch;
}
.name {
  font-weight: 600;
}
.show-all {
  align-self: flex-end;
}
@media (max-width: 700px) {
  /* A row becomes a card: the same content, the cells stacked under their own heading. */
  .cfg-table,
  .cfg-table tbody,
  .cfg-table tr,
  .cfg-table th,
  .cfg-table td {
    display: block;
  }
  .cfg-table thead {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip-path: inset(50%);
  }
  .cfg-table tr {
    border: 1px solid var(--c-border);
    border-radius: var(--radius);
    margin-bottom: var(--s-3);
    padding: var(--s-2);
    background: var(--c-surface);
  }
  .cfg-table td::before {
    content: attr(data-label);
    display: block;
    font-weight: 600;
    color: var(--c-text-muted);
  }
}
</style>
