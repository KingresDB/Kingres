<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Restarts the instances that wait for a restart because of a configuration change (`restartPendingInstances`, task
// `config.restart`). A primary that restarts is downtime for its shard group and failover does not exist yet, so the
// dialog says so first and the button stays off until the administrator has acknowledged it. The task's steps are the
// order the server follows: a standby before its primary when a setting a standby needs at least as high goes up, the
// primary first when it goes down; a mixed change is refused with the settings named.
import { computed, ref } from 'vue'
import AppDialog from '@/components/AppDialog.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import SafeName from '@/components/SafeName.vue'
import TaskProgress from '@/components/TaskProgress.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useConfigServers } from '@/api/queries'
import { useTaskAction } from '@/composables/useTask'
import { DOWNTIME_NOTE } from '@/lib/config'

const props = defineProps<{
  /** Restart only these servers; by default every server that waits. */
  serverIds?: string[]
}>()
const emit = defineEmits<{ close: [] }>()

const servers = useConfigServers()
const waiting = computed(() =>
  (servers.data.value?.items ?? []).filter(
    (s) =>
      s.pendingRestart.length > 0 &&
      (props.serverIds === undefined || props.serverIds.includes(s.serverId)),
  ),
)
const acknowledged = ref(false)
const action = useTaskAction<void>(
  () =>
    endpoints.restartPendingInstances({
      acknowledgeDowntime: true,
      ...(props.serverIds ? { serverIds: props.serverIds } : {}),
    }),
  {
    invalidate: () => [
      keys.config,
      keys.configServers,
      keys.configSummary,
      keys.servers,
      keys.topology,
    ],
    label: () => 'Restart the instances that wait for a restart',
  },
)
const started = computed(() => action.task.value !== null)
</script>

<template>
  <AppDialog
    title="Restart pending instances"
    wide
    :persistent="action.isStarting.value"
    @close="emit('close')"
  >
    <template v-if="!started">
      <div class="notice tone-warn" data-testid="downtime-note">
        <span aria-hidden="true">&#9650;</span>
        <div class="stack">
          <strong>Restarting a primary is downtime for its shard group.</strong>
          <span>{{ DOWNTIME_NOTE }}</span>
        </div>
      </div>
      <p v-if="servers.isPending.value" class="muted" role="status">Loading.</p>
      <ProblemNotice
        v-else-if="servers.error.value"
        :error="servers.error.value"
        @retry="servers.refetch()"
      />
      <template v-else>
        <p v-if="waiting.length === 0" class="muted" data-testid="nothing-waiting">
          No instance waits for a restart.
        </p>
        <template v-else>
          <p>
            The instances of {{ waiting.length }}
            {{ waiting.length === 1 ? 'server' : 'servers' }} are restarted one server at a time,
            and the task stops at the first failure.
          </p>
          <ul class="plain" data-testid="waiting-servers">
            <li v-for="s in waiting" :key="s.serverId">
              <SafeName :value="s.serverId" kind="text" :max="60" />: waits for
              <template v-for="(n, i) in s.pendingRestart" :key="n"
                ><code><SafeName :value="n" kind="text" :max="60" /></code
                ><template v-if="i < s.pendingRestart.length - 1">, </template></template
              >
            </li>
          </ul>
          <p class="muted">
            The order follows what the settings need: when max_connections, max_worker_processes,
            max_wal_senders, max_prepared_transactions or max_locks_per_transaction goes up, a
            standby is restarted before its primary; when one goes down, the primary first. If one
            goes up and another down, the server refuses and says which. The steps of the task show
            the order it follows.
          </p>
        </template>
      </template>
      <label class="check"
        ><input v-model="acknowledged" type="checkbox" data-autofocus /> I understand that
        restarting a primary is downtime and that nothing takes over.</label
      >
      <ProblemNotice v-if="action.error.value" :error="action.error.value" />
    </template>
    <template v-else>
      <TaskProgress :task="action.task.value" :poll-error="action.pollError.value" />
      <p v-if="action.state.value === 'done'" class="notice tone-ok" role="status">
        <span aria-hidden="true">&#10003;</span> Every instance was restarted; the new values are
        running.
      </p>
    </template>
    <template #footer>
      <template v-if="!started">
        <button
          type="button"
          class="btn"
          :disabled="action.isStarting.value"
          @click="emit('close')"
        >
          Cancel
        </button>
        <GateButton
          op="restartPendingInstances"
          variant="danger"
          :disabled="action.isStarting.value"
          :blocked-reason="
            !acknowledged
              ? 'Tick the box to say that you accept the downtime.'
              : waiting.length === 0
                ? 'No instance waits for a restart.'
                : ''
          "
          @click="action.run()"
        >
          Restart now
        </GateButton>
      </template>
      <button v-else type="button" class="btn btn-primary" @click="emit('close')">
        {{ action.isActive.value ? 'Hide (keeps running)' : 'Close' }}
      </button>
    </template>
  </AppDialog>
</template>

<style scoped>
.plain {
  list-style: none;
  margin: 0;
  padding: 0;
}
</style>
