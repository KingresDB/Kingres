<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { computed, ref } from 'vue'
import SafeName from '@/components/SafeName.vue'
import TableWrap from '@/components/TableWrap.vue'
import { RouterLink } from 'vue-router'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import ServerActions from '@/components/ServerActions.vue'
import FailoverDialog from '@/components/FailoverDialog.vue'
import FailoverProgress from '@/components/FailoverProgress.vue'
import ServerGroupsPanel from '@/components/ServerGroupsPanel.vue'
import NullableValue from '@/components/NullableValue.vue'
import CertificateExpiry from '@/components/CertificateExpiry.vue'
import { useServers, keys } from '@/api/queries'
import { SERVER_STATUS } from '@/lib/status'
import { instanceLabel } from '@/lib/cluster'
import {
  formatAgo,
  formatBytes,
  formatDateTime,
  formatDuration,
  formatLag,
  percent,
} from '@/lib/format'
import type { Server } from '@/api/types'

const servers = useServers()
const rows = computed(() => servers.data.value?.items ?? [])

/** The dialog being shown: which action on which server. */
const dialog = ref<{ kind: 'add' | 'restore' | 'failover' | 'remove'; server?: Server } | null>(
  null,
)
</script>

<template>
  <PageHeader title="Servers" subtitle="Every server in the cluster, its instances and its load.">
    <template #actions>
      <GateButton op="restorePreferredPrimaries" @click="dialog = { kind: 'restore' }">
        Restore preferred primaries
      </GateButton>
      <GateButton op="addServer" variant="primary" @click="dialog = { kind: 'add' }"
        >Add server</GateButton
      >
    </template>
  </PageHeader>

  <FailoverProgress />

  <QueryBoundary
    :loading="servers.isPending.value"
    :error="servers.error.value"
    :empty="rows.length === 0"
    empty-title="No servers"
    @retry="servers.refetch()"
  >
    <TableWrap>
      <table class="table">
        <caption class="sr-only">
          Servers
        </caption>
        <thead>
          <tr>
            <th scope="col">Server</th>
            <th scope="col">Status</th>
            <th scope="col">Server group</th>
            <th scope="col">Instances</th>
            <th scope="col" class="num">Buckets (primary / replica)</th>
            <th scope="col" class="num">Size</th>
            <th scope="col" class="num">Max lag</th>
            <th scope="col">Resources</th>
            <th scope="col">Version</th>
            <th scope="col">Certificate valid until</th>
            <th scope="col">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="s in rows" :key="s.id" :data-testid="`server-row-${s.id}`">
            <th scope="row">
              <RouterLink :to="{ name: 'server', params: { id: s.id } }"
                ><SafeName :value="s.address" kind="text" :max="80"
              /></RouterLink>
              <div class="muted"><SafeName :value="s.hostname" kind="text" :max="80" /></div>
              <div v-if="s.isManagerLeader" class="muted">Manager leader</div>
            </th>
            <td>
              <StatusBadge :meta="SERVER_STATUS[s.status]" />
              <div v-if="s.status !== 'live'" class="muted" data-testid="last-heartbeat">
                <template v-if="s.lastHeartbeatAt"
                  >Last heartbeat
                  <time :datetime="s.lastHeartbeatAt" :title="formatDateTime(s.lastHeartbeatAt)">{{
                    formatAgo(s.lastHeartbeatAt)
                  }}</time></template
                >
                <template v-else>No heartbeat on record</template>
              </div>
              <div v-if="s.certificateRevokedAt" data-testid="revoked-badge">
                <span class="badge tone-danger"
                  ><span aria-hidden="true">&#10006;</span> Certificates revoked</span
                >
              </div>
            </td>
            <td>
              <SafeName v-if="s.serverGroup" :value="s.serverGroup" :max="60" />
              <template v-else>-</template>
            </td>
            <td>
              <ul class="plain">
                <li v-for="i in s.instances" :key="i.id" class="nowrap">
                  {{ instanceLabel(i)
                  }}<span v-if="i.state !== 'running'" class="muted"> ({{ i.state }})</span>
                </li>
                <li v-if="s.instances.length === 0" class="muted">None</li>
              </ul>
            </td>
            <td class="num">{{ s.buckets.primary }} / {{ s.buckets.replica }}</td>
            <td class="num nowrap"><NullableValue :value="s.sizeBytes" :format="formatBytes" /></td>
            <td class="num nowrap">
              <NullableValue :value="s.replicationLagMaxSeconds" :format="(v) => formatLag(v)" />
            </td>
            <td>
              <div>
                CPU <NullableValue :value="s.resources.cpuUsagePercent" :format="percent" />
              </div>
              <div class="muted">
                Mem <NullableValue :value="s.resources.memoryUsedBytes" :format="formatBytes" /> /
                <NullableValue :value="s.resources.memoryTotalBytes" :format="formatBytes" />
              </div>
              <div class="muted">
                Disk <NullableValue :value="s.resources.diskUsedBytes" :format="formatBytes" /> /
                <NullableValue :value="s.resources.diskTotalBytes" :format="formatBytes" />
              </div>
            </td>
            <td>
              <NullableValue :value="s.version" />
              <div class="muted">
                up <NullableValue :value="s.uptimeSeconds" :format="formatDuration" />
              </div>
            </td>
            <td>
              <CertificateExpiry :expires-at="s.certificateExpiresAt" />
              <div v-if="s.certificateRevokedAt" class="muted">
                Revoked {{ formatDateTime(s.certificateRevokedAt) }}
              </div>
              <div v-if="!s.seenFingerprint" class="muted">No fingerprint on record</div>
              <div v-else-if="!s.fingerprintPinned" class="muted">Fingerprint not pinned</div>
            </td>
            <td>
              <div class="row">
                <GateButton
                  op="failoverServer"
                  small
                  :variant="s.status === 'dead' ? 'primary' : 'default'"
                  @click="dialog = { kind: 'failover', server: s }"
                  >{{ s.status === 'dead' ? 'Fail over' : 'Failover' }}</GateButton
                >
                <GateButton
                  op="removeServer"
                  small
                  variant="danger"
                  @click="dialog = { kind: 'remove', server: s }"
                  >Remove</GateButton
                >
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </TableWrap>
    <p class="hint muted" data-testid="liveness-note">
      Servers send each other a heartbeat every second. A server that was silent for 5 seconds is
      suspect, for 10 seconds dead (the defaults), and it is live again as soon as it is heard.
      These two statuses are the view of the server that answered: another server can differ for a
      moment.
    </p>
  </QueryBoundary>

  <div style="margin-top: var(--s-5)"><ServerGroupsPanel /></div>

  <FailoverDialog
    v-if="dialog?.kind === 'failover' && dialog.server"
    :server="dialog.server"
    :invalidate="[keys.servers, keys.topology]"
    @close="dialog = null"
  />
  <ServerActions
    v-else-if="dialog && dialog.kind !== 'failover'"
    :kind="dialog.kind"
    :server="dialog.server"
    :invalidate="[keys.servers, keys.topology]"
    @close="dialog = null"
  />
</template>

<style scoped>
.plain {
  list-style: none;
  margin: 0;
  padding: 0;
}
</style>
