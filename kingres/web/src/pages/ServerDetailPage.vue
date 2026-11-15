<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { safeText } from '@/lib/safe-text'
import SafeName from '@/components/SafeName.vue'
import TableWrap from '@/components/TableWrap.vue'
import { RouterLink } from 'vue-router'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import ServerActions from '@/components/ServerActions.vue'
import NullableValue from '@/components/NullableValue.vue'
import CertificateExpiry from '@/components/CertificateExpiry.vue'
import FingerprintValue from '@/components/FingerprintValue.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import GroupLeaseInfo from '@/components/GroupLeaseInfo.vue'
import ServerConfigSection from '@/components/config/ServerConfigSection.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useServer, useServerGroups, useTopology } from '@/api/queries'
import { INSTANCE_STATE, SERVER_STATUS } from '@/lib/status'
import { groupLabel, instanceLabel } from '@/lib/cluster'
import { heartbeatAge } from '@/lib/liveness'
import {
  formatAgo,
  formatBytes,
  formatDateTime,
  formatDuration,
  formatLag,
  percent,
} from '@/lib/format'
import { useSessionStore } from '@/stores/session'

const props = defineProps<{ id: string }>()
const client = useQueryClient()
const session = useSessionStore()
const server = useServer(() => props.id)
const topology = useTopology(() => session.can('getReplicationTopology'))
/** The shard groups whose primary this server runs, with their leases as the answering server knows them (0.7.0). */
const leaseGroups = computed(() =>
  (topology.data.value?.groups ?? []).filter((g) => g.primary?.serverId === props.id),
)
const silent = computed(() => (server.data.value ? heartbeatAge(server.data.value) : null))
const groups = useServerGroups()
const s = computed(() => server.data.value)
const dialog = ref<'failover' | 'remove' | 'renew' | 'revoke' | null>(null)

/** Why "Renew now" cannot work for this server, or an empty text. */
const renewBlocked = computed(() =>
  s.value?.certificateRevokedAt
    ? 'The certificates of this server are revoked, so it gets no new certificate. Remove the server and join it again.'
    : '',
)
/** Why "Revoke certificates" cannot work for this server, or an empty text. */
const revokeBlocked = computed(() =>
  s.value?.certificateRevokedAt
    ? 'The certificates of this server are already revoked.'
    : s.value?.isManagerLeader
      ? 'The certificates of the manager leader cannot be revoked: the server refuses, because it hosts the cluster metadata.'
      : '',
)
const holdsBuckets = computed(() =>
  s.value ? s.value.buckets.primary + s.value.buckets.replica > 0 : false,
)

// Server group assignment: PATCH /servers/{id} with serverGroup (null removes the server from its group).
const group = ref('')
const groupSaved = ref(false)
// A refresh of the server must not replace a choice the person has made but not saved.
const groupTouched = ref(false)
watch(
  () => s.value?.serverGroup,
  (g) => {
    if (!groupTouched.value) group.value = g ?? ''
  },
  { immediate: true },
)
const assign = useMutation({
  mutationFn: () => endpoints.updateServer(props.id, { serverGroup: group.value || null }),
  onSuccess: () => {
    groupSaved.value = true
    groupTouched.value = false
  },
  onSettled: () =>
    Promise.all([
      client.invalidateQueries({ queryKey: keys.server(props.id) }),
      client.invalidateQueries({ queryKey: keys.servers }),
      client.invalidateQueries({ queryKey: keys.serverGroups }),
      client.invalidateQueries({ queryKey: keys.cluster }),
    ]),
})
// Placement: whether the server should hold buckets (PATCH shouldHaveShards).
const shouldHold = ref(true)
const holdTouched = ref(false)
const holdSaved = ref(false)
watch(
  () => s.value?.shouldHaveShards,
  (v) => {
    if (!holdTouched.value && v !== undefined) shouldHold.value = v
  },
  { immediate: true },
)
const placement = useMutation({
  mutationFn: () => endpoints.updateServer(props.id, { shouldHaveShards: shouldHold.value }),
  onSuccess: () => {
    holdSaved.value = true
    holdTouched.value = false
  },
  onSettled: () =>
    Promise.all([
      client.invalidateQueries({ queryKey: keys.server(props.id) }),
      client.invalidateQueries({ queryKey: keys.servers }),
      client.invalidateQueries({ queryKey: keys.cluster }),
    ]),
})
function savePlacement(): void {
  holdSaved.value = false
  placement.mutate()
}

function saveGroup(): void {
  groupSaved.value = false
  assign.mutate()
}
</script>

<template>
  <PageHeader :title="s ? `Server ${s.address}` : `Server ${id}`" :subtitle="s?.hostname">
    <template #actions>
      <GateButton op="failoverServer" @click="dialog = 'failover'">Failover</GateButton>
      <GateButton op="removeServer" variant="danger" @click="dialog = 'remove'">Remove</GateButton>
    </template>
  </PageHeader>
  <p><RouterLink :to="{ name: 'servers' }">&larr; All servers</RouterLink></p>

  <QueryBoundary
    :loading="server.isPending.value"
    :error="server.error.value"
    @retry="server.refetch()"
  >
    <div v-if="s" class="stack-lg">
      <p v-if="s.certificateRevokedAt" class="notice tone-danger" data-testid="revoked-notice">
        <span aria-hidden="true">&#10006;</span>
        <span>
          <strong>The certificates of this server are revoked.</strong> Every other server and every
          database instance refuses it, and it gets no new certificate. The only way back is to
          remove the server and join it again with a new setup token<template v-if="holdsBuckets">
            (removing works only for a server that holds no bucket copy, and this one holds
            {{ s.buckets.primary + s.buckets.replica }})</template
          >.
        </span>
      </p>
      <p
        v-if="s.status === 'suspect' || s.status === 'dead'"
        class="notice"
        :class="s.status === 'dead' ? 'tone-danger' : 'tone-warn'"
        role="status"
        data-testid="liveness-notice"
      >
        <span aria-hidden="true">{{ s.status === 'dead' ? '✖' : '▲' }}</span>
        <span>
          <strong>{{
            s.status === 'dead' ? 'No heartbeat for the failure timeout.' : 'Heartbeats are late.'
          }}</strong>
          <template v-if="silent !== null"
            >The server that answered last heard a heartbeat from this server
            {{ silent.text }}.</template
          ><template v-else
            >The server that answered has no heartbeat from this server on record.</template
          >
          A server is shown as suspect after 5 seconds and as dead after 10 seconds of silence (the
          defaults), and as live again as soon as it is heard. This is the view of the answering
          server: another server can differ for a moment, and a server that is cut off from the
          others sees them all as dead.
        </span>
      </p>
      <section class="grid-cards" aria-label="Summary">
        <div class="card kpi">
          <span class="label">Status</span>
          <span class="value"><StatusBadge :meta="SERVER_STATUS[s.status]" /></span>
          <span v-if="s.certificateRevokedAt" class="value">
            <span class="badge tone-danger" data-testid="revoked-badge"
              ><span aria-hidden="true">&#10006;</span> Certificates revoked</span
            >
          </span>
          <span class="muted"
            >Server group:
            <SafeName v-if="s.serverGroup" :value="s.serverGroup" :max="60" />
            <template v-else>none</template></span
          >
        </div>
        <div class="card kpi">
          <span class="label">Buckets (primary / replica)</span>
          <span class="value">{{ s.buckets.primary }} / {{ s.buckets.replica }}</span>
          <RouterLink :to="{ name: 'shards', query: { server: s.id } }">Show in Shards</RouterLink>
        </div>
        <div class="card kpi">
          <span class="label">Data size</span>
          <span class="value"><NullableValue :value="s.sizeBytes" :format="formatBytes" /></span>
          <span class="muted"
            >Max lag
            <NullableValue :value="s.replicationLagMaxSeconds" :format="(v) => formatLag(v)"
          /></span>
        </div>
        <div class="card kpi">
          <span class="label">Uptime</span>
          <span class="value"
            ><NullableValue :value="s.uptimeSeconds" :format="formatDuration"
          /></span>
          <span class="muted"
            >Joined <NullableValue :value="s.joinedAt" :format="formatDateTime"
          /></span>
          <span class="muted" data-testid="last-heartbeat"
            >Last heartbeat <NullableValue :value="s.lastHeartbeatAt" :format="formatDateTime" />
            <template v-if="s.lastHeartbeatAt">({{ formatAgo(s.lastHeartbeatAt) }})</template></span
          >
        </div>
      </section>

      <section class="card stack" aria-labelledby="grp-h">
        <h2 id="grp-h">Server group</h2>
        <form class="form-row" @submit.prevent="saveGroup">
          <div class="field">
            <label for="srv-group">Group</label>
            <select id="srv-group" v-model="group" @change="groupTouched = true">
              <option value="">None</option>
              <option v-for="g in groups.data.value?.items" :key="g.name" :value="g.name">
                {{ safeText(g.name) }}
              </option>
            </select>
          </div>
          <GateButton op="updateServer" type="submit" :disabled="assign.isPending.value">
            Save group
          </GateButton>
        </form>
        <p class="hint muted">
          Changing the group can make the current placement invalid; a rebalance is then suggested.
        </p>
        <ProblemNotice v-if="assign.error.value" :error="assign.error.value" />
        <p v-if="groupSaved" class="notice tone-ok" role="status">
          <span aria-hidden="true">&#10003;</span> Server group saved.
        </p>
      </section>

      <section class="card stack" aria-labelledby="plc-h">
        <h2 id="plc-h">Placement</h2>
        <form class="stack" @submit.prevent="savePlacement">
          <label class="check"
            ><input v-model="shouldHold" type="checkbox" @change="holdTouched = true" /> Should hold
            buckets</label
          >
          <p class="hint muted">
            Turning this off does not move data by itself. To empty the server, save this, then
            start a <RouterLink :to="{ name: 'rebalance' }">rebalance</RouterLink>; a server that
            holds no bucket copy can then be removed.
          </p>
          <div>
            <GateButton op="updateServer" type="submit" :disabled="placement.isPending.value">
              Save placement
            </GateButton>
          </div>
        </form>
        <ProblemNotice v-if="placement.error.value" :error="placement.error.value" />
        <p v-if="holdSaved" class="notice tone-ok" role="status">
          <span aria-hidden="true">&#10003;</span> Placement saved.
        </p>
      </section>

      <section class="card stack" aria-labelledby="cert-h">
        <h2 id="cert-h">Certificate and first contact</h2>
        <dl class="kv">
          <dt>Certificate valid until</dt>
          <dd><CertificateExpiry :expires-at="s.certificateExpiresAt" /></dd>
          <template v-if="s.certificateRevokedAt">
            <dt>Certificates revoked</dt>
            <dd>
              <span class="badge tone-danger"
                ><span aria-hidden="true">&#10006;</span> Revoked</span
              >
              <time :datetime="s.certificateRevokedAt">{{
                formatDateTime(s.certificateRevokedAt)
              }}</time>
              (this server cannot take part in the cluster until it is removed and joined again)
            </dd>
          </template>
          <dt>Fingerprint at first contact</dt>
          <dd>
            <FingerprintValue :fingerprint="s.seenFingerprint" :pinned="s.fingerprintPinned" />
          </dd>
        </dl>
        <p class="hint muted">
          The fingerprint is the SHA-256 of the certificate this server presented when it was added.
          Compare it with the log of that server. A server renews its own certificate before it
          expires; a certificate that is about to expire is flagged here and raised as an alert.
        </p>
        <div class="row" data-testid="certificate-actions">
          <GateButton
            op="renewServerCertificates"
            :blocked-reason="renewBlocked"
            @click="dialog = 'renew'"
            >Renew now</GateButton
          >
          <GateButton
            op="revokeServerCertificates"
            variant="danger"
            :blocked-reason="revokeBlocked"
            @click="dialog = 'revoke'"
            >Revoke certificates</GateButton
          >
        </div>
        <p v-if="renewBlocked" class="hint muted" data-testid="renew-blocked">
          Renew now is not available: {{ renewBlocked }}
        </p>
        <p v-if="revokeBlocked" class="hint muted" data-testid="revoke-blocked">
          Revoke certificates is not available: {{ revokeBlocked }}
        </p>
      </section>

      <section
        v-if="session.can('getReplicationTopology')"
        class="card stack"
        aria-labelledby="lease-h"
        data-testid="server-leases"
      >
        <h2 id="lease-h">Leases of the primaries on this server</h2>
        <p class="muted">
          A shard group has one primary at a time because its primary holds a lease that a majority
          of the servers grants. What is shown is what the server that answered knows.
        </p>
        <QueryBoundary
          :loading="topology.isPending.value"
          :error="topology.error.value"
          :empty="leaseGroups.length === 0"
          empty-title="No primary runs here"
          empty-hint="This server runs no primary, so it holds no lease."
          @retry="topology.refetch()"
        >
          <ul class="lease-list">
            <li
              v-for="g in leaseGroups"
              :key="g.id"
              class="stack"
              :data-testid="`lease-group-${g.id}`"
            >
              <h3>
                <RouterLink :to="{ name: 'replication', hash: `#g-${g.id}` }">{{
                  groupLabel(g.id, g.isMapGroup)
                }}</RouterLink>
              </h3>
              <GroupLeaseInfo :lease="g.lease" />
            </li>
          </ul>
        </QueryBoundary>
      </section>

      <ServerConfigSection v-if="session.can('listConfigServers')" :server-id="s.id" />

      <section class="card" aria-labelledby="inst-h">
        <h2 id="inst-h">Instances</h2>
        <TableWrap>
          <table class="table">
            <caption class="sr-only">
              Instances of the server
            </caption>
            <thead>
              <tr>
                <th scope="col">Instance</th>
                <th scope="col">Role</th>
                <th scope="col">State</th>
                <th scope="col" class="num">Port</th>
                <th scope="col" class="num">Size</th>
                <th scope="col" class="num">Lag</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="i in s.instances" :key="i.id">
                <th scope="row" class="mono">#{{ i.id }}</th>
                <td>{{ instanceLabel(i) }}</td>
                <td><StatusBadge :meta="INSTANCE_STATE[i.state]" /></td>
                <td class="num">{{ i.port }}</td>
                <td class="num"><NullableValue :value="i.sizeBytes" :format="formatBytes" /></td>
                <td class="num">
                  <template v-if="i.role === 'standby'">
                    <NullableValue
                      :value="i.replicationLagSeconds ?? i.replicationLagBytes"
                      :format="() => formatLag(i.replicationLagSeconds, i.replicationLagBytes)"
                    />
                  </template>
                  <template v-else>-</template>
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </section>

      <section class="card" aria-labelledby="db-h">
        <h2 id="db-h">Buckets by database</h2>
        <p v-if="s.bucketsByDatabase.length === 0" class="muted">No sharded databases.</p>
        <TableWrap v-else>
          <table class="table">
            <caption class="sr-only">
              Primary and replica buckets per database
            </caption>
            <thead>
              <tr>
                <th scope="col">Database</th>
                <th scope="col" class="num">Primary buckets</th>
                <th scope="col" class="num">Primary size</th>
                <th scope="col" class="num">Replica buckets</th>
                <th scope="col" class="num">Replica size</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="d in s.bucketsByDatabase" :key="d.databaseName">
                <th scope="row">
                  <RouterLink :to="{ name: 'shards', query: { db: d.databaseName, server: s.id } }"
                    ><SafeName :value="d.databaseName" :max="80"
                  /></RouterLink>
                </th>
                <td class="num">{{ d.primaryBuckets }}</td>
                <td class="num">
                  <NullableValue :value="d.primarySizeBytes" :format="formatBytes" />
                </td>
                <td class="num">{{ d.replicaBuckets }}</td>
                <td class="num">
                  <NullableValue :value="d.replicaSizeBytes" :format="formatBytes" />
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </section>

      <section class="card" aria-labelledby="res-h">
        <h2 id="res-h">Resources</h2>
        <dl class="kv">
          <dt>CPU</dt>
          <dd><NullableValue :value="s.resources.cpuUsagePercent" :format="percent" /></dd>
          <dt>Memory</dt>
          <dd>
            <NullableValue :value="s.resources.memoryUsedBytes" :format="formatBytes" /> of
            <NullableValue :value="s.resources.memoryTotalBytes" :format="formatBytes" />
          </dd>
          <dt>Disk</dt>
          <dd>
            <NullableValue :value="s.resources.diskUsedBytes" :format="formatBytes" /> of
            <NullableValue :value="s.resources.diskTotalBytes" :format="formatBytes" />
          </dd>
          <dt>Version</dt>
          <dd><NullableValue :value="s.version" /></dd>
        </dl>
      </section>
    </div>
  </QueryBoundary>

  <ServerActions
    v-if="dialog && s"
    :kind="dialog"
    :server="s"
    :invalidate="[keys.servers, keys.server(id), keys.topology, keys.cluster, keys.tasks]"
    @close="dialog = null"
  />
</template>

<style scoped>
.kv {
  display: grid;
  grid-template-columns: fit-content(45%) minmax(0, 1fr);
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.kv dt {
  color: var(--c-text-muted);
}
.kv dd {
  margin: 0;
  font-variant-numeric: tabular-nums;
}
.lease-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: var(--s-4);
}
</style>
