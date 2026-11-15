<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Leave the cluster (leaveCluster). The server decides first from its own copy of the catalog and
// refuses while it still matters to the cluster; each refusal is explained as what to do first.
// "Force" exists for one situation only, a manager leader that cannot be reached, and is offered
// only after the first attempt met exactly that. When the server accepts, it resets itself: it
// answers 503 for a few seconds and is then uninitialized. The dialog follows that calmly (the
// rest of the panel stops asking, no sign-out call is made), and ends at the setup wizard.
import { computed, onBeforeUnmount, ref } from 'vue'
import { useRouter } from 'vue-router'
import AppDialog from '@/components/AppDialog.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import TaskProgress from '@/components/TaskProgress.vue'
import TypeToConfirm from '@/components/TypeToConfirm.vue'
import { ApiError } from '@/api/problem'
import { availability, leavePolling, setResetting } from '@/api/availability'
import { endpoints } from '@/api/endpoints'
import { useSessionStore } from '@/stores/session'
import { isSettled } from '@/composables/taskPoller'
import type { Task } from '@/api/types'

const props = defineProps<{ serverName: string }>()
const emit = defineEmits<{ close: [] }>()

const router = useRouter()
const session = useSessionStore()

const typed = ref('')
const force = ref(false)
const error = ref<unknown>(null)
const starting = ref(false)
const task = ref<Task | null>(null)
const phase = ref<'confirm' | 'resetting' | 'stuck'>('confirm')
let timer: ReturnType<typeof setTimeout> | undefined
let alive = true

const expected = computed(() => props.serverName || 'leave')
const typedOk = computed(() => typed.value === expected.value)
const code = computed(() => (error.value instanceof ApiError ? error.value.code : null))
/** The first attempt met a leader that cannot be reached: only now "force" can be offered. */
/** Only an answer of this server counts: a failure the panel made up (network, proxy) says nothing about the leader. */
const answeredByServer = computed(() => error.value instanceof ApiError && !error.value.synthetic)
const leaderUnreachable = computed(
  () =>
    answeredByServer.value &&
    (code.value === 'service_unavailable' || code.value === 'leader_unavailable'),
)
const noAnswer = computed(() => error.value instanceof ApiError && error.value.synthetic)

/** For each refusal: what the administrator has to do first (the cause comes from the problem). */
const TO_DO_FIRST: Record<string, string> = {
  last_server:
    'This is the only server of the cluster. There is no cluster to leave; keep the server, or reset it by hand on the machine.',
  server_hosts_map_group:
    'This server runs an instance of the map group (shard group 0) that holds the cluster metadata, or it is the manager leader. First move the manager role and the map group to another server; a server in that role cannot leave.',
  server_owns_buckets:
    'This server still holds a copy of at least one bucket (as primary or as replica). First empty it: on the Servers page turn off "should hold buckets" for it, then start a rebalance and wait until it has finished. Then try again.',
  rebalance_in_progress:
    'A rebalance is running. Wait until it has finished, or stop it on the Rebalance page, then try again.',
  operation_in_progress:
    'Another operation of this server (for example an earlier leave) is still running. Wait for it to finish on the Tasks page, then try again.',
}
const toDoFirst = computed(() => (code.value ? (TO_DO_FIRST[code.value] ?? null) : null))

async function leave(): Promise<void> {
  error.value = null
  starting.value = true
  try {
    const accepted = await endpoints.leaveCluster(force.value)
    task.value = accepted
    beginReset(accepted)
  } catch (e) {
    error.value = e
    // A refusal is final for "force" too (the checks still run): only an unreachable leader
    // keeps the force option.
    if (!(
      e instanceof ApiError &&
      !e.synthetic &&
      (e.code === 'service_unavailable' || e.code === 'leader_unavailable')
    ))
      force.value = false
  } finally {
    starting.value = false
  }
}

/** From here on the server may stop answering at any moment: ask as little as possible. */
function beginReset(accepted: Task): void {
  phase.value = 'resetting'
  setResetting(true)
  const started = Date.now()
  // With an unreachable leader the task has id 0 and cannot be fetched (it never existed there).
  let followTask = accepted.id !== 0 && !isSettled(accepted)
  const step = async (): Promise<void> => {
    if (!alive) return
    if (followTask) {
      try {
        const next = await endpoints.getTask(accepted.id)
        task.value = next
        if (isSettled(next)) followTask = false
      } catch (e) {
        // 503 while the server resets itself (or already uninitialized): that is the expected end.
        if (e instanceof ApiError && e.status === 503) followTask = false
      }
    }
    if (!followTask) {
      try {
        const state = await endpoints.getNodeState()
        if (state.state === 'uninitialized') return finish()
      } catch (e) {
        if (e instanceof ApiError && e.code === 'node_uninitialized') return finish()
      }
    }
    if (Date.now() - started > leavePolling.giveUpMs) {
      phase.value = 'stuck'
      return
    }
    timer = setTimeout(() => void step(), leavePolling.intervalMs)
  }
  timer = setTimeout(() => void step(), leavePolling.intervalMs)
}

function finish(): void {
  availability.leftNotice = true
  // Nothing to sign out of: the server forgot the session. Everything held from it is dropped.
  session.end()
  setResetting(false)
  emit('close')
  void router.push({ name: 'setup' })
}

onBeforeUnmount(() => {
  alive = false
  if (timer !== undefined) clearTimeout(timer)
})
</script>

<template>
  <AppDialog
    title="Leave the cluster"
    :persistent="starting || phase === 'resetting'"
    @close="emit('close')"
  >
    <template v-if="phase === 'confirm'">
      <p class="notice tone-danger" role="alert">
        <span aria-hidden="true">&#10006;</span>
        <span
          >This server is removed from the cluster, its instance and data are deleted, and it resets
          itself to a fresh, uninitialized server. Its panel then shows the setup wizard. The
          cluster no longer includes it.</span
        >
      </p>
      <section aria-labelledby="leave-first">
        <h3 id="leave-first">Before you leave</h3>
        <p class="muted">The server refuses to leave while any of these is true:</p>
        <ul>
          <li>it is the only server of the cluster;</li>
          <li>it hosts the map group (shard group 0) or is the manager leader;</li>
          <li>
            it holds a copy of any bucket: move the data away first (turn off "should hold buckets"
            for it and run a rebalance);
          </li>
          <li>a rebalance, or another operation of this server, is running.</li>
        </ul>
      </section>
      <TypeToConfirm v-model="typed" :expected="expected" />
      <ProblemNotice v-if="error" :error="error" />
      <p v-if="toDoFirst" class="notice tone-info" role="status">
        <span aria-hidden="true">&#9432;</span>
        <span><strong>What to do first.</strong> {{ toDoFirst }}</span>
      </p>
      <p v-if="noAnswer" class="notice tone-info" role="status" data-testid="leave-no-answer">
        <span aria-hidden="true">&#9432;</span>
        <span
          >No answer came back from this server, so nothing is known about the manager leader. The
          request may or may not have been carried out: reload the page and check whether this
          server is still a member of the cluster before you try again.</span
        >
      </p>
      <div v-if="leaderUnreachable" class="stack" data-testid="force-leave">
        <p class="notice tone-warn" role="alert">
          <span aria-hidden="true">&#9650;</span>
          <span
            >This server could not reach the manager leader, or the answer was lost. You can make
            this server leave anyway. The cluster is then not told, and its certificate is
            <strong>not revoked</strong>: the cluster keeps listing this server, and the certificate
            stays valid, until an administrator removes the server on the cluster with "Remove
            server". The checks above still apply.</span
          >
        </p>
        <label class="check"
          ><input v-model="force" type="checkbox" /> Leave anyway (force): I understand that the
          certificate of this server is not revoked.</label
        >
      </div>
    </template>
    <template v-else-if="phase === 'resetting'">
      <TaskProgress v-if="task && task.id !== 0" :task="task" />
      <p role="status">
        This server is resetting itself. It does not answer for a few seconds, then it is
        uninitialized. You are taken to the setup page as soon as that is the case; nothing else is
        asked of the server meanwhile.
      </p>
    </template>
    <template v-else>
      <p class="notice tone-warn" role="alert">
        <span aria-hidden="true">&#9650;</span>
        <span
          >The server has not finished resetting. Wait a minute and reload this page; if the setup
          wizard does not appear, look at the log of kingresd on the machine.</span
        >
      </p>
    </template>

    <template #footer>
      <template v-if="phase === 'confirm'">
        <button type="button" class="btn" :disabled="starting" @click="emit('close')">
          Cancel
        </button>
        <button
          type="button"
          class="btn btn-danger"
          :disabled="!typedOk || starting || (leaderUnreachable && !force)"
          @click="leave"
        >
          {{ leaderUnreachable ? 'Leave anyway' : 'Leave cluster' }}
        </button>
      </template>
      <button v-else-if="phase === 'stuck'" type="button" class="btn" @click="emit('close')">
        Close
      </button>
    </template>
  </AppDialog>
</template>
