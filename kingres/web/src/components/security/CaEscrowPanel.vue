<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The escrow of the key of the cluster certificate authority (contract 0.26.0). With it on, the manager leader keeps a
// sealed copy of the CA key on every server that hosts a ready standby of the map group, so that a new leader can issue,
// renew and revoke certificates without the old one. Switching it on and off and rotating its keys is for administrators
// and always goes through a confirmation that says what it means. All text from the server is shown as text.
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import QueryBoundary from '../QueryBoundary.vue'
import GateButton from '../GateButton.vue'
import TableWrap from '../TableWrap.vue'
import ConfirmDialog from '../ConfirmDialog.vue'
import ProblemNotice from '../ProblemNotice.vue'
import SafeName from '../SafeName.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { formatDateTime } from '@/lib/format'
import type { CaEscrowRecipient } from '@/api/types'

const client = useQueryClient()
const escrow = useQuery({ queryKey: keys.caEscrow, queryFn: endpoints.getCaEscrow })
const status = computed(() => escrow.data.value ?? null)

type Asked = 'on' | 'off' | 'rotate' | null
const asked = ref<Asked>(null)
const failure = ref<unknown>(null)

const change = useMutation({
  mutationFn: (what: Exclude<Asked, null>) =>
    what === 'rotate' ? endpoints.rotateCaEscrow() : endpoints.setCaEscrow(what === 'on'),
  onSuccess: (data) => {
    failure.value = null
    client.setQueryData(keys.caEscrow, data)
  },
  onError: (e) => {
    failure.value = e
  },
  onSettled: () => {
    asked.value = null
    return client.invalidateQueries({ queryKey: keys.caEscrow })
  },
})

interface StateView {
  label: string
  icon: string
  tone: string
  hint: string
}
const STATE: Record<CaEscrowRecipient['state'], StateView> = {
  current: { label: 'Current', icon: '✓', tone: 'ok', hint: 'The copy is the newest.' },
  pending: {
    label: 'Pending',
    icon: '◷',
    tone: 'info',
    hint: 'Not delivered yet or out of date; the leader sends again.',
  },
  refused: {
    label: 'Refused',
    icon: '✖',
    tone: 'danger',
    hint: 'The server answered the last send with an error.',
  },
  dropPending: {
    label: 'Deleting',
    icon: '▲',
    tone: 'warn',
    hint: 'The copy is to be deleted and is not yet. Until it is, treat the server as knowing the CA key.',
  },
}
// A state the console does not know is shown as it came, not hidden.
const stateOf = (s: string): StateView =>
  (STATE as Record<string, StateView | undefined>)[s] ?? {
    label: s,
    icon: '?',
    tone: 'neutral',
    hint: '',
  }
const lingering = computed(
  () => status.value?.recipients.filter((r) => r.state === 'dropPending') ?? [],
)
</script>

<template>
  <section class="stack" aria-labelledby="ca-h" data-testid="ca-escrow">
    <h2 id="ca-h">Escrow of the certificate authority key</h2>
    <p class="muted">
      A sealed copy of the CA key on the servers that can become the manager leader lets a new
      leader issue, renew and revoke certificates without the old one. Without it, the new leader
      raises the alert <code>certificateAuthorityUnavailable</code> until an operator restores the
      key files by hand.
    </p>
    <QueryBoundary
      :loading="escrow.isLoading.value"
      :error="escrow.error.value"
      @retry="escrow.refetch()"
    >
      <template v-if="status">
        <div class="row-between">
          <span
            class="badge"
            :class="status.enabled ? 'tone-ok' : 'tone-neutral'"
            data-testid="ca-escrow-state"
          >
            {{ status.enabled ? 'On' : 'Off' }}
          </span>
          <span class="row">
            <GateButton
              v-if="!status.enabled"
              op="setCaEscrow"
              variant="primary"
              data-testid="ca-escrow-on"
              :blocked-reason="change.isPending.value ? 'Working ...' : ''"
              @click="asked = 'on'"
              >Switch escrow on</GateButton
            >
            <template v-else>
              <GateButton
                op="rotateCaEscrow"
                data-testid="ca-escrow-rotate"
                :blocked-reason="change.isPending.value ? 'Working ...' : ''"
                @click="asked = 'rotate'"
                >Rotate escrow keys</GateButton
              >
              <GateButton
                op="setCaEscrow"
                variant="danger"
                data-testid="ca-escrow-off"
                :blocked-reason="change.isPending.value ? 'Working ...' : ''"
                @click="asked = 'off'"
                >Switch escrow off</GateButton
              >
            </template>
          </span>
        </div>
        <dl class="kv">
          <dt>This server holds the CA key</dt>
          <dd data-testid="ca-holds-key">{{ status.holdsKey ? 'Yes' : 'No' }}</dd>
          <dt>Generation</dt>
          <dd data-testid="ca-generation">{{ status.generation }}</dd>
          <dt>Every copy is the newest</dt>
          <dd data-testid="ca-complete">{{ status.complete ? 'Yes' : 'No' }}</dd>
        </dl>
        <p
          class="notice"
          :class="status.enabled && !status.complete ? 'tone-warn' : 'tone-info'"
          role="status"
          data-testid="ca-message"
        >
          <span aria-hidden="true">{{ status.enabled && !status.complete ? '▲' : 'ⓘ' }}</span>
          <SafeName :value="status.message" kind="text" :max="600" multiline />
        </p>
        <p
          v-if="lingering.length > 0"
          class="notice tone-danger"
          role="alert"
          data-testid="ca-lingering"
        >
          <span aria-hidden="true">&#10006;</span>
          <span>
            <template v-for="(r, i) in lingering" :key="r.serverId"
              ><SafeName :value="r.serverId" kind="text" :max="60" /><template
                v-if="i < lingering.length - 1"
                >,
              </template></template
            >
            still {{ lingering.length === 1 ? 'holds' : 'hold' }} a copy that is to be deleted.
            Treat {{ lingering.length === 1 ? 'it' : 'them' }} as knowing the CA key until the copy
            is gone.
          </span>
        </p>
        <TableWrap v-if="status.recipients.length > 0">
          <table data-testid="ca-recipients">
            <caption>
              Servers that hold a copy
            </caption>
            <thead>
              <tr>
                <th scope="col">Server</th>
                <th scope="col">Copy</th>
                <th scope="col">Key</th>
                <th scope="col">Generation</th>
                <th scope="col">Sealed</th>
                <th scope="col">Last error</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="r in status.recipients"
                :key="r.serverId"
                :data-testid="`ca-recipient-${r.serverId}`"
              >
                <th scope="row"><SafeName :value="r.serverId" kind="text" :max="60" /></th>
                <td>
                  <span
                    class="badge"
                    :class="`tone-${stateOf(r.state).tone}`"
                    :title="stateOf(r.state).hint"
                  >
                    <span aria-hidden="true">{{ stateOf(r.state).icon }}</span>
                    <span>{{ stateOf(r.state).label }}</span>
                  </span>
                </td>
                <td class="mono"><SafeName :value="r.keyId ?? ''" kind="text" :max="40" /></td>
                <td>{{ r.generation ?? '' }}</td>
                <td>{{ r.sealedAt ? formatDateTime(r.sealedAt) : '' }}</td>
                <td><SafeName :value="r.error ?? ''" kind="text" :max="300" multiline /></td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
        <p v-else class="muted" data-testid="ca-no-recipients">
          {{
            status.enabled
              ? 'No server qualifies yet: a copy is made for each server that hosts a ready standby of the map group.'
              : 'No server holds a copy.'
          }}
        </p>
      </template>
    </QueryBoundary>
    <ProblemNotice v-if="failure" :error="failure" data-testid="ca-error" />
  </section>

  <ConfirmDialog
    v-if="asked === 'on'"
    title="Switch the escrow on"
    confirm-label="Switch escrow on"
    danger
    :busy="change.isPending.value"
    @confirm="change.mutate('on')"
    @cancel="asked = null"
  >
    <p>
      The leader seals the CA key and the revocation list to every server that hosts a ready standby
      of the map group, and keeps the copies current.
    </p>
    <p class="notice tone-warn" role="note">
      <span aria-hidden="true">&#9650;</span>
      <span
        ><strong>A server that holds a copy can become the CA.</strong> Its disk is as sensitive as
        the leader's. Switch this on only when those servers are as trusted as the leader.</span
      >
    </p>
  </ConfirmDialog>
  <ConfirmDialog
    v-if="asked === 'off'"
    title="Switch the escrow off"
    confirm-label="Switch escrow off"
    danger
    :busy="change.isPending.value"
    @confirm="change.mutate('off')"
    @cancel="asked = null"
  >
    <p>
      Every copy and every escrow key is taken away from the servers. A new manager leader then has
      no CA key until an operator restores it by hand. A server that cannot be reached is asked
      again every minute and stays listed until it has deleted its copy; treat it as knowing the CA
      key until then.
    </p>
  </ConfirmDialog>
  <ConfirmDialog
    v-if="asked === 'rotate'"
    title="Rotate the escrow keys"
    confirm-label="Rotate keys"
    :busy="change.isPending.value"
    @confirm="change.mutate('rotate')"
    @cancel="asked = null"
  >
    <p>
      Every recipient makes a new escrow key and the leader seals a new copy to it; the old key and
      copy are deleted when the new one is stored. Use it when escrow keys may have been exposed. It
      does not change the CA key.
    </p>
  </ConfirmDialog>
</template>
