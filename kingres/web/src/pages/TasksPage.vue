<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { computed, ref } from 'vue'
import SafeName from '@/components/SafeName.vue'
import TableWrap from '@/components/TableWrap.vue'
import { useRoute } from 'vue-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import AppDialog from '@/components/AppDialog.vue'
import TaskProgress from '@/components/TaskProgress.vue'
import { endpoints } from '@/api/endpoints'
import { keys, usePagedQuery } from '@/api/queries'
import { taskKindLabel, TASK_STATE } from '@/lib/status'
import { formatAgo } from '@/lib/format'
import type { TaskState } from '@/api/types'

const client = useQueryClient()
const state = ref<'' | TaskState>('')
const { query, items } = usePagedQuery(
  () => [...keys.tasks, 'page', state.value],
  (cursor) => endpoints.listTasks({ cursor, limit: 50, state: state.value || undefined }),
  3000,
)
const cancel = useMutation({
  mutationFn: (id: number) => endpoints.cancelTask(id),
  onSettled: () => client.invalidateQueries({ queryKey: keys.tasks }),
})
// A link from another page (`/tasks?task=12`) opens that task, also when it is not on the first page.
const route = useRoute()
const linked = Number(route.query.task)
const openId = ref<number | null>(Number.isSafeInteger(linked) && linked > 0 ? linked : null)
const listed = computed(() => items.value.find((t) => t.id === openId.value) ?? null)
const fetched = useQuery({
  queryKey: computed(() => [...keys.tasks, 'one', openId.value]),
  queryFn: () => endpoints.getTask(openId.value as number),
  enabled: computed(() => openId.value !== null && listed.value === null),
  retry: false,
})
const open = computed(() => listed.value ?? fetched.data.value ?? null)
</script>

<template>
  <PageHeader
    title="Tasks"
    subtitle="Long-running operations. They continue even if you close the page."
  />
  <div class="field" style="max-width: 14em; margin-bottom: var(--s-3)">
    <label for="t-state">State</label>
    <select id="t-state" v-model="state">
      <option value="">All</option>
      <option v-for="(m, s) in TASK_STATE" :key="s" :value="s">{{ m.label }}</option>
    </select>
  </div>
  <ProblemNotice v-if="cancel.error.value" :error="cancel.error.value" />
  <QueryBoundary
    :loading="query.isPending.value"
    :error="query.error.value"
    :empty="items.length === 0"
    empty-title="No tasks"
    empty-hint="Operations such as creating a database or adding a server appear here."
    @retry="query.refetch()"
  >
    <TableWrap>
      <table class="table">
        <caption class="sr-only">
          Tasks
        </caption>
        <thead>
          <tr>
            <th scope="col" class="num">Id</th>
            <th scope="col">Kind</th>
            <th scope="col">Subject</th>
            <th scope="col">State</th>
            <th scope="col" class="num">Progress</th>
            <th scope="col">Updated</th>
            <th scope="col">By</th>
            <th scope="col">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="t in items" :key="t.id">
            <th scope="row" class="num">{{ t.id }}</th>
            <td>
              <span class="mono"><SafeName :value="t.kind" kind="text" :max="40" /></span>
              <div class="muted task-kind-label">
                <SafeName :value="taskKindLabel(t.kind)" kind="text" :max="80" />
              </div>
            </td>
            <td>
              <SafeName
                v-if="t.subject"
                :value="`${t.subject.type} ${t.subject.id}`"
                kind="text"
                :max="120"
              />
              <template v-else>-</template>
            </td>
            <td>
              <StatusBadge :meta="TASK_STATE[t.state]" />
              <div v-if="t.error" class="muted">
                <SafeName :value="t.error.message" kind="text" :max="300" />
              </div>
              <div v-if="t.detail" class="muted">
                <SafeName :value="t.detail" kind="text" :max="200" />
              </div>
            </td>
            <td class="num">{{ Math.round(t.progress.percent) }}%</td>
            <td>{{ formatAgo(t.updatedAt) }}</td>
            <td><SafeName :value="t.createdBy" :max="60" /></td>
            <td>
              <div class="row">
                <button type="button" class="btn btn-small" @click="openId = t.id">Details</button>
                <GateButton v-if="t.cancellable" op="cancelTask" small @click="cancel.mutate(t.id)"
                  >Cancel</GateButton
                >
              </div>
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
  <AppDialog v-if="openId !== null" :title="`Task ${openId}`" @close="openId = null">
    <TaskProgress v-if="open" :task="open" />
    <ProblemNotice v-else-if="fetched.error.value" :error="fetched.error.value" />
    <p v-else role="status">Loading the task ...</p>
  </AppDialog>
</template>
