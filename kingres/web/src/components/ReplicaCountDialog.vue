<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Changes the replica count of the cluster (contract 0.7.0): `updateClusterSettings` with `defaultReplicaCount`.
// The settings operation answers with the new settings, not with a task; what it starts is the task
// `standby.reconcile`, which adds or removes standbys group by group. So the dialog sends the change, then looks for
// the task that the change started (a task that did not exist before the request) and follows it with the usual
// progress view. When the standbys already match, no task starts, and the dialog says so.
import { computed, getCurrentScope, onScopeDispose, ref } from 'vue'
import { useQueryClient } from '@tanstack/vue-query'
import AppDialog from './AppDialog.vue'
import FormField from './FormField.vue'
import ProblemNotice from './ProblemNotice.vue'
import TaskProgress from './TaskProgress.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import { keys } from '@/api/queries'
import { useTask, isSettled } from '@/composables/useTask'
import { useAnnounce } from '@/composables/useAnnounce'
import { useTaskNoticeStore } from '@/stores/taskNotices'
import { taskDiscovery } from '@/lib/standby'
import { firstError, integerBetween } from '@/lib/validators'
import type { Task } from '@/api/types'

const props = defineProps<{
  /** The current `defaultReplicaCount`. */
  current: number
  /** How many servers the cluster has: a group never has two instances on one server. */
  serverCount: number
}>()
const emit = defineEmits<{ close: [] }>()

const client = useQueryClient()
const announce = useAnnounce()
const notices = useTaskNoticeStore()

const count = ref<number | string>(props.current)
const phase = ref<'form' | 'sending' | 'looking' | 'following' | 'none'>('form')
const error = ref<unknown>(null)
const submitted = ref(false)
let disposed = false

const validation = computed(() =>
  firstError<number | null>(
    typeof count.value === 'number' ? count.value : null,
    integerBetween('The replica count', 0, 3),
  ),
)
const fieldErrors = computed(() => (error.value ? describeProblem(error.value).fieldErrors : {}))
const fieldError = computed(
  () =>
    (submitted.value ? validation.value : undefined) ?? fieldErrors.value['defaultReplicaCount'],
)
const wanted = computed(() => (typeof count.value === 'number' ? count.value : props.current))
const possible = computed(() => Math.max(0, Math.min(wanted.value, props.serverCount - 1)))
const lowering = computed(() => wanted.value < props.current)

function refresh(): void {
  for (const key of [keys.topology, keys.clusterSettings, keys.cluster, keys.tasks, keys.alerts])
    void client.invalidateQueries({ queryKey: key })
}
const tracker = useTask({ onSettled: () => refresh() })

if (getCurrentScope()) {
  onScopeDispose(() => {
    disposed = true
    const t = tracker.task.value
    // Closed while the task still runs: the notice store reports its end.
    if (t && !isSettled(t)) notices.follow(t, { label: 'Change the replica count', refresh })
  })
}

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms))

async function confirm(): Promise<void> {
  submitted.value = true
  error.value = null
  if (validation.value) return
  phase.value = 'sending'
  try {
    // The tasks that exist now: the one the change starts is the one that is new afterwards.
    const before = new Set(
      (await endpoints.listTasks({ kind: 'standby.reconcile', limit: 50 })).items.map((t) => t.id),
    )
    await endpoints.updateClusterSettings({ defaultReplicaCount: wanted.value })
    refresh()
    announce('Replica count saved.')
    phase.value = 'looking'
    let running: Task | undefined
    for (let attempt = 0; attempt < taskDiscovery.attempts && !disposed; attempt += 1) {
      const items = (await endpoints.listTasks({ kind: 'standby.reconcile', limit: 50 })).items
      const started = items.find((t) => !before.has(t.id))
      if (started) {
        phase.value = 'following'
        tracker.track(started)
        return
      }
      running = items.find((t) => t.state === 'running')
      if (attempt + 1 < taskDiscovery.attempts) await sleep(taskDiscovery.intervalMs)
    }
    if (disposed) return
    if (running) {
      // A check is already running: it carries this change in its next round, but its progress is worth showing.
      phase.value = 'following'
      tracker.track(running)
      return
    }
    phase.value = 'none'
  } catch (e) {
    error.value = e
    phase.value = 'form'
  }
}
</script>

<template>
  <AppDialog
    title="Change the replica count"
    :persistent="phase === 'sending'"
    @close="emit('close')"
  >
    <template v-if="phase === 'form' || phase === 'sending'">
      <p>
        Every shard group gets this many standbys, as far as the servers allow: a group never has
        two instances on one server, so {{ serverCount }}
        {{ serverCount === 1 ? 'server allows' : 'servers allow' }} at most
        {{ Math.max(0, serverCount - 1) }}. The manager leader then adds or removes standbys one
        group at a time in the task "Add or remove standbys". This is not a rebalance.
      </p>
      <form class="form-row" @submit.prevent="confirm">
        <FormField
          v-slot="slot"
          label="Standbys per shard group"
          hint="0 to 3. The current value is the one shown on the page."
          :error="fieldError"
        >
          <input
            v-bind="slot"
            v-model.number="count"
            class="input num"
            type="number"
            min="0"
            max="3"
            style="width: 7em"
          />
        </FormField>
      </form>
      <p v-if="lowering" class="notice tone-warn" role="status" data-testid="replica-lowering">
        <span aria-hidden="true">&#9650;</span>
        <span>
          Standbys beyond {{ wanted }} are removed with their replication slots. A group that is
          left without a standby acknowledges a commit with the primary alone, and its primary is no
          longer stopped when it loses its lease.
        </span>
      </p>
      <p v-else-if="wanted > current" class="muted" data-testid="replica-raising">
        Creating a standby copies the whole shard group with a base backup, which takes time and
        load. A new standby does not hold up commits until it streams.
        <template v-if="possible < wanted"
          >With {{ serverCount }} server(s) only {{ possible }} can be created, so groups stay under
          replicated and the alert "Shard group has too few standbys" is raised.</template
        >
      </p>
      <ProblemNotice v-if="error" :error="error" />
    </template>
    <template v-else>
      <p v-if="phase === 'looking'" class="muted" role="status" data-testid="replica-looking">
        The change was saved. Waiting for the leader to start the standby task...
      </p>
      <TaskProgress
        v-if="tracker.task.value"
        :task="tracker.task.value"
        :poll-error="tracker.pollError.value"
      />
      <p v-if="phase === 'none'" class="notice tone-info" role="status" data-testid="replica-none">
        <span aria-hidden="true">&#9432;</span>
        <span>
          The change was saved, and no standby task was started: the standbys already match it, or a
          standby check is already running and will pick it up. The leader checks every ten seconds.
        </span>
      </p>
      <p
        v-if="tracker.state.value === 'done'"
        class="tone-ok notice"
        role="status"
        data-testid="replica-done"
      >
        <span aria-hidden="true">&#10003;</span> Finished.
      </p>
    </template>
    <template #footer>
      <template v-if="phase === 'form' || phase === 'sending'">
        <button type="button" class="btn" :disabled="phase === 'sending'" @click="emit('close')">
          Cancel
        </button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="phase === 'sending'"
          @click="confirm"
        >
          {{ phase === 'sending' ? 'Saving...' : 'Change replica count' }}
        </button>
      </template>
      <button v-else type="button" class="btn btn-primary" @click="emit('close')">
        {{ tracker.isActive.value ? 'Hide (keeps running)' : 'Close' }}
      </button>
    </template>
  </AppDialog>
</template>
