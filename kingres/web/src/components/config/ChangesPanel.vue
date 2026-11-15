<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The Changes tab: the history of configuration changes, newest first, of one parameter or of all. A change that is
// the latest of its level and scope can be reverted (a new change, previewed like any other); an older one says why not.
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import TableWrap from '@/components/TableWrap.vue'
import GateButton from '@/components/GateButton.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import ConfigValue from './ConfigValue.vue'
import { useConfigChanges } from '@/api/queries'
import { TASK_STATE } from '@/lib/status'
import { formatDateTime } from '@/lib/format'
import { levelWords } from '@/lib/config'
import type { ConfigChange } from '@/api/types'

const emit = defineEmits<{ revert: [change: ConfigChange] }>()
const route = useRoute()
const router = useRouter()

const name = computed(() => (typeof route.query.name === 'string' ? route.query.name : ''))
const search = ref(name.value)
let timer: ReturnType<typeof setTimeout> | undefined
watch(search, (v) => {
  clearTimeout(timer)
  timer = setTimeout(() => {
    const { name: _old, ...rest } = route.query
    void _old
    void router.replace({ query: v ? { ...rest, name: v } : rest })
  }, 300)
})
watch(name, (v) => {
  if (v !== search.value) search.value = v
})

const { query, items } = useConfigChanges(name)

/** Why an older change has no Revert, in a sentence. */
const WHY =
  'A later change touched the same parameter at the same level and scope. Revert the later one first.'

function taskMeta(state: string | null) {
  return state === null ? null : TASK_STATE[state as keyof typeof TASK_STATE]
}
</script>

<template>
  <div class="stack">
    <form class="form-row" role="search" aria-label="Filter the changes" @submit.prevent>
      <div class="field">
        <label for="cfg-change-name">Parameter name</label>
        <input
          id="cfg-change-name"
          v-model="search"
          class="input"
          type="search"
          autocomplete="off"
        />
        <span class="hint"
          >Exact name, any letter case. Empty: the changes of every parameter.</span
        >
      </div>
    </form>
    <QueryBoundary
      :loading="query.isPending.value"
      :error="query.error.value"
      :empty="items.length === 0"
      empty-title="No changes yet"
      :empty-hint="
        name
          ? 'No change of this parameter was made from the console.'
          : 'Nothing was changed from the console.'
      "
      @retry="query.refetch()"
    >
      <TableWrap>
        <table class="table">
          <caption class="sr-only">
            Configuration changes, newest first
          </caption>
          <thead>
            <tr>
              <th scope="col">Time</th>
              <th scope="col">Who</th>
              <th scope="col">Parameter</th>
              <th scope="col">Level and scope</th>
              <th scope="col">Old value</th>
              <th scope="col">New value</th>
              <th scope="col">Task</th>
              <th scope="col" class="num">Revision</th>
              <th scope="col">Action</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="c in items" :key="c.changeId" :data-testid="`change-${c.changeId}`">
              <td class="nowrap">{{ formatDateTime(c.changedAt) }}</td>
              <td><SafeName :value="c.changedBy" kind="text" :max="60" /></td>
              <td>
                <code><SafeName :value="c.name" kind="text" :max="60" /></code>
              </td>
              <td>
                {{ levelWords(c.level) }}
                <template v-if="c.scopeId !== null"
                  ><SafeName :value="c.scopeId" kind="text" :max="60"
                /></template>
              </td>
              <td>
                <ConfigValue v-if="c.oldValue !== null" :value="c.oldValue" />
                <span v-else class="muted">not set</span>
              </td>
              <td>
                <ConfigValue v-if="c.newValue !== null" :value="c.newValue" />
                <span v-else class="muted">removed</span>
              </td>
              <td>
                <StatusBadge v-if="taskMeta(c.taskState)" :meta="taskMeta(c.taskState)!" />
                <span v-else class="muted">no longer listed</span>
                <div v-if="c.reverts !== null" class="muted">reverts change {{ c.reverts }}</div>
              </td>
              <td class="num">{{ c.revision }}</td>
              <td>
                <GateButton
                  v-if="c.revertible"
                  op="revertConfigChange"
                  small
                  :aria-label="`Revert change ${c.changeId}`"
                  @click="emit('revert', c)"
                  >Revert</GateButton
                >
                <span v-else class="muted" :title="WHY">Superseded: a later change exists.</span>
              </td>
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
  </div>
</template>
