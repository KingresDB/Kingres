<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Start, stop or restart one local instance. Says what happens, explains each refusal of the
// server in terms of what to do next, and offers "force" only with its consequence stated.
import { computed, ref } from 'vue'
import TaskActionDialog from '@/components/TaskActionDialog.vue'
import { ApiError } from '@/api/problem'
import { rememberInstances } from '@/api/availability'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { instanceLabel } from '@/lib/cluster'
import type { Instance } from '@/api/types'

const props = defineProps<{ kind: 'start' | 'stop' | 'restart'; instance: Instance }>()
defineEmits<{ close: [] }>()

const force = ref(false)
const verb = computed(() => `${props.kind[0]?.toUpperCase()}${props.kind.slice(1)}`)
/** Stopping the instance of the system database pauses the whole panel: ask for typing. */
const hostsSystemDatabase = computed(() => props.instance.isMapGroup && props.kind === 'stop')
/** A restart of the same instance pauses the same operations, for the time the restart takes. */
const restartsSystemDatabase = computed(() => props.instance.isMapGroup && props.kind === 'restart')

function start() {
  if (props.instance.isMapGroup) rememberInstances([props.instance])
  return endpoints.instanceAction(props.instance.id, props.kind, force.value)
}

/** What the administrator can do about a refusal (the heading and cause come from the problem). */
const NEXT_STEP: Record<string, string> = {
  instance_already_running:
    'Nothing to start: the instance is already running or starting. Close this window; the list updates on its own.',
  instance_already_stopped:
    'Nothing to stop or restart: the instance is already stopped or stopping. Start it first if you need it running.',
  operation_in_progress:
    'Another start, stop or restart of this instance is still running. Wait until it has finished (see Tasks), then try again.',
  no_replica_available:
    'This instance is the only up-to-date copy of its shard group: a standby is missing or still catching up, so writes that were not replicated yet would be lost. Wait until the standby has caught up (see Replication), or tick "Force" below to stop it anyway.',
}
function nextStep(error: unknown): string | null {
  return error instanceof ApiError ? (NEXT_STEP[error.code] ?? null) : null
}
</script>

<template>
  <TaskActionDialog
    :title="`${verb} instance #${instance.id}`"
    :confirm-label="`${verb} instance`"
    :danger="kind !== 'start'"
    :type-to-confirm="hostsSystemDatabase ? 'stop' : undefined"
    :start="start"
    :invalidate="[keys.thisServer, keys.servers]"
    @close="$emit('close')"
  >
    <template #default="{ error }">
      <p v-if="kind === 'start'">
        Start instance #{{ instance.id }} ({{ instanceLabel(instance) }}).
      </p>
      <p v-else-if="kind === 'stop'">
        Instance #{{ instance.id }} ({{ instanceLabel(instance) }}) stops.
        <template v-if="instance.role === 'primary'"
          >Its shard group has no primary until a standby is promoted; its buckets are unavailable
          meanwhile.</template
        >
      </p>
      <p v-else>
        Instance #{{ instance.id }} ({{ instanceLabel(instance) }}) restarts. Connections to it are
        dropped for a short time.
      </p>
      <p v-if="hostsSystemDatabase" class="notice tone-danger" role="alert">
        <span aria-hidden="true">&#10006;</span>
        <span
          >This instance hosts the cluster system database. While it is stopped, every operation in
          this panel except "Start instance" is paused and nobody can sign in. The panel then shows
          a Start button until the instance runs again.</span
        >
      </p>
      <p v-if="restartsSystemDatabase" class="notice tone-warn" data-testid="restart-system-note">
        <span aria-hidden="true">&#9650;</span>
        <span
          >This instance hosts the cluster system database. While it restarts, operations that need
          it, including sign-in, are paused and answer that the system database is stopped. They
          work again when the instance has restarted.</span
        >
      </p>
      <label v-if="kind !== 'start'" class="check"
        ><input v-model="force" type="checkbox" /> Force: do not check whether this is the only
        up-to-date copy of its shard group. Data that a standby has not received yet can be
        lost.</label
      >
      <p v-if="nextStep(error)" class="notice tone-info" role="status">
        <span aria-hidden="true">&#9432;</span><span>{{ nextStep(error) }}</span>
      </p>
    </template>
  </TaskActionDialog>
</template>
