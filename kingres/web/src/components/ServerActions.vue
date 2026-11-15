<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The server operations, each with a confirmation that states the consequence: add server, remove
// server, failover (hard or graceful), restore preferred primaries, and (0.5.0) renew and revoke the
// certificates of a server.
import { computed, reactive, ref } from 'vue'
import { safeText } from '@/lib/safe-text'
import SafeName from './SafeName.vue'
import type { QueryKey } from '@tanstack/vue-query'
import TaskActionDialog from './TaskActionDialog.vue'
import FormField from './FormField.vue'
import { endpoints } from '@/api/endpoints'
import { useServerGroups, useServers } from '@/api/queries'
import { RouterLink } from 'vue-router'
import FingerprintHelp from './FingerprintHelp.vue'
import FingerprintValue from './FingerprintValue.vue'
import { fingerprintError, normalizeFingerprint } from '@/lib/fingerprint'
import { firstError, nodeAddress, required, compact } from '@/lib/validators'
import type { Server } from '@/api/types'

const props = defineProps<{
  kind: 'add' | 'remove' | 'failover' | 'restore' | 'renew' | 'revoke'
  server?: Server
  invalidate?: QueryKey[]
}>()
const emit = defineEmits<{ close: [] }>()

const form = reactive({ address: '', setupToken: '', serverGroup: '', expectedFingerprint: '' })
const mode = ref<'hard' | 'graceful'>('graceful')
const submitted = ref(false)
const groups = useServerGroups()
const servers = useServers()
/** The server a finished add task registered, with what the cluster recorded at first contact. */
function addedServer(task: { subject: { id: string } | null } | null) {
  return servers.data.value?.items.find((s) => s.id === task?.subject?.id) ?? null
}

const errors = computed(() =>
  compact({
    address: firstError(form.address, nodeAddress()),
    setupToken: firstError(form.setupToken, required('The setup token')),
    expectedFingerprint: fingerprintError(form.expectedFingerprint),
  }),
)

function validate(): boolean {
  submitted.value = true
  return props.kind !== 'add' || Object.keys(errors.value).length === 0
}

const primaries = computed(() => props.server?.instances.filter((i) => i.role === 'primary') ?? [])

const config = computed(() => {
  const id = props.server?.id ?? ''
  switch (props.kind) {
    case 'add':
      return {
        title: 'Add server',
        confirmLabel: 'Add server',
        danger: false,
        typeToConfirm: undefined as string | undefined,
        start: () =>
          endpoints.addServer({
            address: form.address,
            setupToken: form.setupToken,
            serverGroup: form.serverGroup || null,
            ...(form.expectedFingerprint.trim()
              ? { expectedFingerprint: normalizeFingerprint(form.expectedFingerprint) }
              : {}),
          }),
      }
    case 'remove':
      return {
        title: `Remove server ${props.server?.address ?? ''}`,
        confirmLabel: 'Remove server',
        danger: true,
        typeToConfirm: props.server?.address,
        start: () => endpoints.removeServer(id),
      }
    case 'failover':
      return {
        title: `Fail over server ${props.server?.address ?? ''}`,
        confirmLabel: mode.value === 'hard' ? 'Hard failover' : 'Graceful failover',
        danger: mode.value === 'hard',
        // Marking a server dead is not undone by a click: the address has to be typed.
        typeToConfirm: mode.value === 'hard' ? props.server?.address : undefined,
        start: () => endpoints.failoverServer(id, mode.value),
      }
    case 'renew':
      return {
        title: `Renew certificates of server ${props.server?.address ?? ''}`,
        confirmLabel: 'Renew now',
        danger: false,
        typeToConfirm: undefined as string | undefined,
        start: () => endpoints.renewServerCertificates(id),
      }
    case 'revoke':
      return {
        title: `Revoke certificates of server ${props.server?.address ?? ''}`,
        confirmLabel: 'Revoke certificates',
        danger: true,
        // The id, not the address: it is what the operation names and what the audit log records.
        typeToConfirm: props.server?.id,
        start: () => endpoints.revokeServerCertificates(id),
      }
    default:
      return {
        title: 'Restore preferred primaries',
        confirmLabel: 'Restore primaries',
        danger: false,
        typeToConfirm: undefined as string | undefined,
        start: () => endpoints.restorePreferredPrimaries(),
      }
  }
})
</script>

<template>
  <TaskActionDialog
    :title="config.title"
    :confirm-label="config.confirmLabel"
    :danger="config.danger"
    :type-to-confirm="config.typeToConfirm"
    :start="config.start"
    :validate="validate"
    :invalidate="invalidate"
    :wide="kind === 'revoke'"
    @close="emit('close')"
  >
    <template v-if="kind === 'add'" #result="{ task }">
      <div class="stack" data-testid="add-result">
        <template v-if="addedServer(task)">
          <strong>Fingerprint recorded for this server</strong>
          <FingerprintValue
            :fingerprint="addedServer(task)?.seenFingerprint ?? null"
            :pinned="addedServer(task)?.fingerprintPinned ?? false"
          />
        </template>
      </div>
    </template>
    <template #default="{ fieldErrors }">
      <template v-if="kind === 'add'">
        <p>
          The server joins the cluster and shows as <strong>pending rebalance</strong>. No data
          moves until you start a rebalance.
        </p>
        <FormField
          v-slot="slot"
          label="Server address"
          hint="host:port of the new server's panel port."
          :error="(submitted ? errors.address : undefined) ?? fieldErrors.address"
          required
        >
          <input v-bind="slot" v-model="form.address" class="input mono" data-autofocus />
        </FormField>
        <FormField
          v-slot="slot"
          label="Setup token of the new server"
          :error="(submitted ? errors.setupToken : undefined) ?? fieldErrors.setupToken"
          required
        >
          <input v-bind="slot" v-model="form.setupToken" class="input mono" autocomplete="off" />
        </FormField>
        <FormField
          v-slot="slot"
          label="Expected fingerprint (recommended)"
          hint="SHA-256 of the new server's certificate, 64 hex digits; colons are allowed."
          :error="
            (submitted ? errors.expectedFingerprint : undefined) ?? fieldErrors.expectedFingerprint
          "
        >
          <input
            v-bind="slot"
            v-model="form.expectedFingerprint"
            class="input mono"
            autocomplete="off"
            spellcheck="false"
          />
        </FormField>
        <FingerprintHelp purpose="add" />
        <FormField v-slot="slot" label="Server group (optional)">
          <select v-bind="slot" v-model="form.serverGroup">
            <option value="">None</option>
            <option v-for="g in groups.data.value?.items" :key="g.name" :value="g.name">
              {{ safeText(g.name) }}
            </option>
          </select>
        </FormField>
      </template>

      <template v-else-if="kind === 'remove'">
        <p>
          Server
          <strong><SafeName :value="server?.address ?? ''" kind="text" :max="80" /></strong> is
          unregistered, told to delete its PostgreSQL instance and the data of it on that machine,
          reset to an unconfigured server, and its certificate is revoked (the server shows as
          decommissioning meanwhile). It can join a cluster again afterwards.
        </p>
        <p>
          <strong>The server must not hold any bucket copy.</strong> Removing does not move data. To
          empty a server: turn off <em>Should hold buckets</em> for it on its page, start a
          <RouterLink :to="{ name: 'rebalance' }">rebalance</RouterLink> and wait until it ends,
          then remove the server. This server holds
          <strong>{{ server?.buckets.primary }}</strong> primary and
          <strong>{{ server?.buckets.replica }}</strong> replica buckets now.
        </p>
        <p class="muted">
          The server is refused while it hosts the cluster metadata (shard group 0) or is the
          manager leader, while it is the only server, while a rebalance runs, and when it is dead
          (fail it over first).
        </p>
        <p v-if="server?.certificateRevokedAt" class="notice tone-info">
          <span aria-hidden="true">&#9432;</span>
          <span>
            The certificates of this server are revoked. Removing it, and then joining it again with
            a new setup token, is the only way back. That works only for a server that holds no
            bucket copy<template v-if="server.buckets.primary + server.buckets.replica > 0"
              >, and this one still holds some</template
            >.
          </span>
        </p>
      </template>

      <template v-else-if="kind === 'renew'">
        <p>
          The manager leader issues a new certificate for server
          <strong><SafeName :value="server?.address ?? ''" kind="text" :max="80" /></strong> now and
          installs it on the server. Nothing is restarted and open connections keep working.
        </p>
        <p class="muted">
          A server renews its own certificate before it expires, so this is for when you do not want
          to wait, for example after the alert <em>Certificate renewal failed</em>. It is also the
          way to renew a certificate that has already expired: the leader can still reach the server
          although the server can no longer call the leader.
        </p>
      </template>

      <template v-else-if="kind === 'revoke'">
        <p class="notice tone-danger" data-testid="revoke-warning">
          <span aria-hidden="true">&#10006;</span>
          <span>
            <strong>This cannot be undone.</strong> Revoking the certificates of server
            <strong><SafeName :value="server?.id ?? ''" kind="text" :max="80" /></strong> makes
            every certificate it has invalid, in the whole cluster, for good.
          </span>
        </p>
        <h3>What happens</h3>
        <ul>
          <li>
            The revocation list is sent to every server and every database instance. The task waits
            for a server that is down, like every task that concerns the whole cluster.
          </li>
          <li>
            From then on every other server and every database instance refuses this server, in both
            directions: its calls to them (the internal API, forwarding, recording tasks, leaving)
            and the connections between the database instances (the sharding connections too) are
            refused, and so are their calls to it.
          </li>
          <li>Connections that are open now keep running until they end. New ones are refused.</li>
          <li>
            The server never gets a new certificate: <em>Renew now</em> answers that the
            certificates are revoked.
          </li>
        </ul>
        <h3>The only way back</h3>
        <p>
          Remove the server and join it again with a new setup token. Removing works
          <strong>only for a server that holds no bucket copy</strong>. A server that holds bucket
          copies can be removed only after a rebalance has emptied it, which the current server
          version cannot do yet, so for a server that holds data revocation is a one-way door.
          <template v-if="server">
            Server <SafeName :value="server.id" kind="text" :max="80" /> holds
            <strong>{{ server.buckets.primary }}</strong> primary and
            <strong>{{ server.buckets.replica }}</strong> replica buckets now.
          </template>
        </p>
        <p class="muted">
          The certificates of the manager leader cannot be revoked (the server refuses with
          <em>Server hosts the cluster metadata</em>). If the manager leader does not hold the key
          of the certificate authority, nothing is revoked and nothing is changed.
        </p>
      </template>

      <template v-else-if="kind === 'failover'">
        <fieldset>
          <legend>Failover mode</legend>
          <label class="check"
            ><input v-model="mode" type="radio" value="graceful" /> Graceful: wait for standbys to
            catch up, then switch roles. No data loss.</label
          >
          <label class="check"
            ><input v-model="mode" type="radio" value="hard" /> Hard: promote standbys now. Writes
            not yet replicated can be lost.</label
          >
        </fieldset>
        <p v-if="mode === 'hard'" class="notice tone-danger" role="alert">
          <span aria-hidden="true">&#10006;</span>
          <span>
            Hard failover marks
            <strong><SafeName :value="server?.address ?? ''" kind="text" :max="80" /></strong> as
            dead. Its {{ primaries.length }} primary instance(s) are replaced by their standbys, and
            their former neighbours carry double write load until you restore the preferred
            primaries.
          </span>
        </p>
        <p v-else>
          The {{ primaries.length }} primary instance(s) on
          <strong><SafeName :value="server?.address ?? ''" kind="text" :max="80" /></strong> hand
          over to their standbys. The server stays in the cluster as a standby.
        </p>
      </template>

      <template v-else>
        <p>
          Every shard group whose primary runs on a server other than its preferred one is switched
          back to its home server. Connections to those primaries are interrupted for a moment.
        </p>
      </template>
    </template>
  </TaskActionDialog>
</template>
