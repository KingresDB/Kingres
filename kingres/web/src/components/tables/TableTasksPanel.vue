<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The changes of one table that the control plane runs or has run lately (tasks `table.distribute`, `table.undistribute`,
// `table.alter` and `table.partition`, contract 0.14.0): the one that runs, with its steps, and Cancel while the task can be
// cancelled; the last one that failed, in words, with the step and the code; and the last one that finished. A cancelled task
// says what the control plane reports about undoing it (never that the table is as it was). The change buttons of the page are off while one runs.
import { computed } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import GateButton from '../GateButton.vue'
import ProblemNotice from '../ProblemNotice.vue'
import SafeName from '../SafeName.vue'
import StatusBadge from '../StatusBadge.vue'
import TaskProgress from '../TaskProgress.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useTasks } from '@/api/queries'
import { TASK_KIND_LABEL, TASK_STATE } from '@/lib/status'
import { formatDateTime } from '@/lib/format'
import { CANCELLED_TEXT, TABLE_TASKS_LIMIT, tasksOfTable } from '@/lib/tables'
import type { Task } from '@/api/types'

const props = defineProps<{ database: string; schema: string; table: string }>()
const tasks = useTasks(TABLE_TASKS_LIMIT)
const client = useQueryClient()
const mine = computed(() =>
  tasksOfTable(tasks.data.value?.items ?? [], props.database, props.schema, props.table),
)
const running = computed(
  () =>
    mine.value.find(
      (t) => t.state === 'pending' || t.state === 'running' || t.state === 'paused',
    ) ?? null,
)
const lastFailed = computed(() => mine.value.find((t) => t.state === 'failed') ?? null)
const lastDone = computed(() => mine.value.find((t) => t.state === 'done') ?? null)
// The failure that was followed by a later success is old news.
const failureShown = computed(() => {
  const f = lastFailed.value
  if (!f) return null
  const d = lastDone.value
  return d && Date.parse(d.updatedAt) > Date.parse(f.updatedAt) ? null : f
})

const cancel = useMutation({
  mutationFn: (id: number) => endpoints.cancelTask(id),
  onSettled: () =>
    Promise.all([
      client.invalidateQueries({ queryKey: keys.tasks }),
      client.invalidateQueries({ queryKey: keys.table(props.database, props.schema, props.table) }),
    ]),
})
const cancelBlocked = (t: Task): string =>
  t.cancellable ? '' : 'This task cannot be cancelled now: it is at a step that goes on to its end.'
const kindLabel = (t: Task): string => TASK_KIND_LABEL[t.kind] ?? t.kind
/** A cancelled task is recorded as failed with the code `cancelled`; any other failure is shown as the server said it. */
const cancelled = (t: Task): boolean => t.error?.code === 'cancelled'
</script>

<template>
  <section
    v-if="running || failureShown || lastDone"
    class="stack"
    aria-labelledby="table-tasks-h"
    data-testid="table-tasks"
  >
    <h2 id="table-tasks-h">Changes of this table</h2>
    <div v-if="running" class="card stack" data-testid="table-task-running">
      <div class="row-between">
        <strong>{{ kindLabel(running) }}</strong>
        <StatusBadge :meta="TASK_STATE[running.state]" />
      </div>
      <TaskProgress :task="running" />
      <div class="row">
        <GateButton
          op="cancelTask"
          data-testid="table-task-cancel"
          :blocked-reason="cancelBlocked(running)"
          :disabled="cancel.isPending.value"
          @click="cancel.mutate(running.id)"
        >
          Cancel the task
        </GateButton>
        <span v-if="cancelBlocked(running)" class="muted" data-testid="table-task-cancel-reason">{{
          cancelBlocked(running)
        }}</span>
      </div>
      <ProblemNotice v-if="cancel.error.value" :error="cancel.error.value" />
    </div>
    <div
      v-if="failureShown"
      class="notice"
      :class="cancelled(failureShown) ? 'tone-info' : 'tone-danger'"
      :role="cancelled(failureShown) ? 'status' : 'alert'"
      data-testid="table-task-failed"
    >
      <span aria-hidden="true">{{ cancelled(failureShown) ? 'ⓘ' : '✖' }}</span>
      <div class="stack" style="gap: var(--s-1)">
        <strong>
          {{ kindLabel(failureShown) }}:
          {{ cancelled(failureShown) ? 'cancelled' : 'failed' }}
        </strong>
        <template v-if="cancelled(failureShown)">
          <span>{{ CANCELLED_TEXT }}</span>
          <span v-if="failureShown.detail" data-testid="table-task-detail"
            ><SafeName :value="failureShown.detail" kind="text" :max="400" multiline
          /></span>
        </template>
        <template v-else>
          <span v-if="failureShown.error"
            ><SafeName :value="failureShown.error.message" kind="text" :max="400" multiline
          /></span>
          <span v-if="failureShown.error?.step" class="muted"
            >At the step <SafeName :value="failureShown.error.step" kind="text" :max="120" />.</span
          >
          <span v-if="failureShown.detail" data-testid="table-task-detail"
            ><SafeName :value="failureShown.detail" kind="text" :max="400" multiline
          /></span>
          <span
            v-if="failureShown.error?.code === 'verification_failed'"
            data-testid="table-task-verification"
          >
            The old copy was kept for inspection, and the new table was not used. Ask an
            administrator to look at it before anything is changed again.
          </span>
          <span class="muted">
            Read the message and the detail for what is left to do. Task {{ failureShown.id }}.
          </span>
        </template>
      </div>
    </div>
    <p v-if="lastDone && !running" class="muted" data-testid="table-task-done">
      Last change: {{ kindLabel(lastDone) }}, finished
      {{ formatDateTime(lastDone.updatedAt) }} (task {{ lastDone.id }}).
    </p>
  </section>
</template>
