<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Shown in place of the page while the instance that hosts the system database is stopped.
// The server answers 503 `system_database_stopped` to everything except "start instance" then;
// that is a state to explain, not an error to repeat on every page. The panel keeps the session,
// asks nothing else, and watches calmly (one cheap call every few seconds) until the server
// answers again, so that it also notices an operator who started the instance on the machine.
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useQueryClient } from '@tanstack/vue-query'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import { availability, recovery, setSystemDatabaseStopped } from '@/api/availability'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { useSessionStore } from '@/stores/session'

const client = useQueryClient()
const session = useSessionStore()
const phase = ref<'idle' | 'starting' | 'waiting'>('idle')
const error = ref<unknown>(null)
let waitingSince = 0
const slow = ref(false)
let timer: ReturnType<typeof setTimeout> | undefined
let alive = true

async function check(): Promise<void> {
  try {
    await endpoints.getSession()
  } catch {
    // Still stopped (503), or the server is busy starting: keep watching. A lost session is
    // handled by the application as for any other request.
    schedule()
    return
  }
  if (!alive) return
  setSystemDatabaseStopped(false)
  // The session of this tab may have been restored from a copy (after a reload): take the server's own.
  void session.refresh()
  // Everything that was paused or answered 503 is asked again.
  void client.invalidateQueries()
}

function schedule(): void {
  if (!alive) return
  if (phase.value === 'waiting' && Date.now() - waitingSince > recovery.slowAfterMs)
    slow.value = true
  timer = setTimeout(
    () => void check(),
    phase.value === 'waiting' ? recovery.afterStartMs : recovery.watchMs,
  )
}

async function startInstance(): Promise<void> {
  const instanceId = availability.instanceId
  // Never a guess: without a known id there is nothing to start from here.
  if (instanceId === null) return
  error.value = null
  phase.value = 'starting'
  try {
    await endpoints.instanceAction(instanceId, 'start')
  } catch (e) {
    // "Already running" means someone started it a moment ago: wait for it like any start.
    if (!(e instanceof ApiError && e.code === 'instance_already_running')) {
      error.value = e
      phase.value = 'idle'
      return
    }
  }
  phase.value = 'waiting'
  waitingSince = Date.now()
  slow.value = false
  if (timer !== undefined) clearTimeout(timer)
  schedule()
}

const busy = computed(() => phase.value !== 'idle')

onMounted(schedule)
onBeforeUnmount(() => {
  alive = false
  if (timer !== undefined) clearTimeout(timer)
})
</script>

<template>
  <section class="card stack" aria-labelledby="sds-h" data-testid="system-database-stopped">
    <h1 id="sds-h">The system database is stopped</h1>
    <p class="notice tone-warn" role="status">
      <span aria-hidden="true">&#9650;</span>
      <span
        >The PostgreSQL instance that holds the cluster system database was stopped on purpose.
        Until it runs again, every other action in this panel is paused and nobody can sign in. You
        are still signed in, and nothing was lost.</span
      >
    </p>
    <p
      v-if="availability.instanceId === null"
      class="notice tone-warn"
      data-testid="instance-unknown"
    >
      <span aria-hidden="true">&#9650;</span>
      <span
        >This panel does not know which instance of this server hosts the system database: it could
        only be read while the server still answered, and that did not happen in this tab. It does
        not guess. Start the instance on the machine of this server (for example with
        <code>pg_ctl start</code>), or restart the <code>kingresd</code> service of this server;
        this page notices within seconds that the server answers again.</span
      >
    </p>
    <p v-else data-testid="instance-known">
      The instance to start is <strong>instance #{{ availability.instanceId }}</strong
      >, the PostgreSQL instance of this server that hosts the system database.
    </p>
    <p>
      Start the instance to continue. Starting can take a while (the instance recovers its data
      first); this page checks on its own and returns you to your work as soon as the server
      answers.
    </p>
    <div class="row">
      <GateButton
        v-if="availability.instanceId !== null"
        op="startThisServerInstance"
        variant="primary"
        :disabled="busy"
        @click="startInstance"
        >Start instance #{{ availability.instanceId }}</GateButton
      >
      <span v-if="phase === 'starting'" role="status">Asking the server to start it...</span>
      <span v-else-if="phase === 'waiting'" role="status"
        >Starting. Waiting for the server to answer again...</span
      >
    </div>
    <p v-if="slow" class="notice tone-warn" role="status">
      <span aria-hidden="true">&#9650;</span>
      <span
        >This is taking longer than usual. The log of this server (on the machine) says why the
        instance does not come up. This page keeps checking.</span
      >
    </p>
    <ProblemNotice v-if="error" :error="error" />
  </section>
</template>
