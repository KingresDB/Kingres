<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Drain a server (contract 0.15.0): the preview says what would move and what stands in the way, then the
// operation starts the task. A drain is a rebalance run (trigger "drain"); the Rebalance page shows its progress.
import { computed, reactive } from 'vue'
import SafeName from './SafeName.vue'
import TaskActionDialog from './TaskActionDialog.vue'
import QueryBoundary from './QueryBoundary.vue'
import NullableValue from './NullableValue.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useDrainPreview } from '@/api/queries'
import { formatBytes, formatNumber } from '@/lib/format'
import type { Server } from '@/api/types'

const props = defineProps<{ server: Server }>()
const emit = defineEmits<{ close: [] }>()

const preview = useDrainPreview(props.server.id)
const p = computed(() => preview.data.value)
// The server that runs the primary of the map group (the manager leader) is drained too: the task pauses once the buckets have moved,
// a planned switchover hands the map group to a standby on another server, and the drain goes on under the new leader (contract 0.27.0).
const hostsMapGroup = computed(() =>
  props.server.instances.some((i) => i.isMapGroup && i.role === 'primary'),
)
const form = reactive({
  removeWhenDone: false,
  acceptFewerReplicas: false,
  acknowledgeLocalData: false,
})

// The button waits for what the preview says must be acknowledged.
const needsFewer = computed(() => p.value !== undefined && !p.value.canKeepReplicas)
const needsLocal = computed(() => (p.value?.localData.length ?? 0) > 0)
const hardBlockers = computed(
  () => p.value?.blockers.filter((b) => b.code !== 'replicas_cannot_be_kept') ?? [],
)
const blockedReason = computed(() => {
  if (!p.value) return 'The preview has not been read yet.'
  if (hardBlockers.value.length) return 'Something stands in the way (see above).'
  if (needsFewer.value && !form.acceptFewerReplicas) return 'Accept fewer standby copies to go on.'
  if (needsLocal.value && !form.acknowledgeLocalData) return 'Acknowledge the local data to go on.'
  return ''
})

const start = () =>
  endpoints.drainServer(props.server.id, {
    removeWhenDone: form.removeWhenDone,
    acceptFewerReplicas: form.acceptFewerReplicas,
    acknowledgeLocalData: form.acknowledgeLocalData,
  })
</script>

<template>
  <TaskActionDialog
    :title="`Drain server ${server.id}`"
    confirm-label="Drain server"
    wide
    :start="start"
    :confirm-disabled="blockedReason !== ''"
    :invalidate="[keys.servers, keys.topology, keys.rebalance, keys.tasks]"
    @close="emit('close')"
  >
    <p>
      The server stops receiving new buckets, then every bucket it owns moves to the other servers
      while the cluster stays in service. A server with no bucket can be removed.
    </p>
    <QueryBoundary
      :loading="preview.isPending.value"
      :error="preview.error.value"
      @retry="preview.refetch()"
    >
      <div v-if="p" class="stack" data-testid="drain-preview">
        <p>
          <strong>{{ formatNumber(p.moveCount) }}</strong> buckets will move,
          <NullableValue :value="p.totalBytes" :format="formatBytes" />.
        </p>
        <ul v-if="p.blockers.length" class="plain" data-testid="drain-blockers">
          <li v-for="b in p.blockers" :key="b.code" class="notice tone-warn">
            <span aria-hidden="true">&#9650;</span>
            <span><SafeName :value="b.detail" kind="text" :max="300" /></span>
          </li>
        </ul>
        <p v-if="hostsMapGroup" class="notice tone-info" data-testid="drain-mapgroup">
          <span aria-hidden="true">&#9432;</span>
          <span
            >This server runs the primary of the map group (the manager leader). After the buckets
            have moved, the drain pauses (the rebalance shows it as stopped by a safety rule) while
            a planned switchover hands the map group to a standby on another server; it goes on by
            itself under the new leader. If the switchover cannot be made the drain fails and the
            server stays draining.</span
          >
        </p>
        <p v-if="p.standbyShardGroupIds.length" class="muted" data-testid="drain-standbys">
          The server hosts a standby of the shard groups {{ p.standbyShardGroupIds.join(', ') }}.
          They are moved to the other servers first. If the servers that remain cannot hold them,
          lower the replica count first or accept fewer copies.
        </p>
        <div v-if="p.localData.length" data-testid="drain-local">
          <p class="notice tone-warn">
            <span aria-hidden="true">&#9650;</span>
            <span
              >Ordinary (not distributed) tables are not moved; their data leaves with the
              server:</span
            >
          </p>
          <ul>
            <li v-for="l in p.localData" :key="l.databaseName">
              <SafeName :value="l.databaseName" :max="80" />: {{ l.tableCount }} tables,
              <NullableValue :value="l.sizeBytes" :format="formatBytes" />
            </li>
          </ul>
          <label class="check"
            ><input v-model="form.acknowledgeLocalData" type="checkbox" /> I know that this data is
            lost when the server is removed</label
          >
        </div>
        <label v-if="needsFewer" class="check"
          ><input v-model="form.acceptFewerReplicas" type="checkbox" /> Accept fewer standby copies
          than the replica count (too few servers would remain)</label
        >
        <label class="check"
          ><input v-model="form.removeWhenDone" type="checkbox" /> Remove the server when it owns no
          bucket</label
        >
        <p v-if="blockedReason" class="hint" data-testid="drain-why">{{ blockedReason }}</p>
      </div>
    </QueryBoundary>
  </TaskActionDialog>
</template>
