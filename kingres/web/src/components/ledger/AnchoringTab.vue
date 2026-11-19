<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The Anchoring tab of the Ledger page (contract 0.11.0, design 6.9 and 8.5): the anchor targets of the cluster with their
// kind, whether they are used, their state, how old their newest anchor is, whether credentials are stored (never their
// value) and the result of the last test; the actions Add, Edit, Test, turn on or off, and Delete; the notice of design 6.9
// while no target is enabled; the advice that one kind of target is one point of failure; the settings of anchoring; and,
// for the database chosen, its anchor records. An operation that the server does not build yet is shown as what it is
// (`?phase7=today`): the calm "not available yet" notice in the place of that part, and the rest of the tab goes on.
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import GateButton from '@/components/GateButton.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import TableWrap from '@/components/TableWrap.vue'
import AnchorRecordsPanel from '@/components/ledger/AnchorRecordsPanel.vue'
import AnchorTargetDialog from '@/components/ledger/AnchorTargetDialog.vue'
import AnchorTestDialog from '@/components/ledger/AnchorTestDialog.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useLedgerAnchorTargets, useLedgerSettings, useLedgerStatusList } from '@/api/queries'
import { formatDateTime } from '@/lib/format'
import { safeText } from '@/lib/safe-text'
import {
  configSummary,
  kindLabel,
  NO_TARGET_ENABLED_TEXT,
  ONE_KIND_TEXT,
  targetStateMeta,
  testSummary,
  windowText,
} from '@/lib/ledger-anchor'
import { useSessionStore } from '@/stores/session'
import type { LedgerAnchorTarget } from '@/api/types'

const props = defineProps<{ databaseName: string }>()
const emit = defineEmits<{ 'update:databaseName': [name: string] }>()
const client = useQueryClient()
const session = useSessionStore()
const targets = useLedgerAnchorTargets()
const settings = useLedgerSettings()
const statusList = useLedgerStatusList()

const list = computed(() => targets.data.value?.items ?? [])
const enabled = computed(() => list.value.filter((t) => t.enabled))
const kindsInUse = computed(() => new Set(enabled.value.map((t) => t.kind)))
const noneEnabled = computed(() => targets.data.value !== undefined && enabled.value.length === 0)
const oneKind = computed(() => kindsInUse.value.size === 1)
const databases = computed(() => statusList.data.value?.items.map((s) => s.databaseName) ?? [])
const dbModel = computed({
  get: () => props.databaseName,
  set: (v: string) => emit('update:databaseName', v),
})
const anchor = computed(() => settings.data.value?.anchor)
const canSettings = computed(() => session.can('updateLedgerSettings'))

const dialog = ref<
  | { kind: 'edit'; target: LedgerAnchorTarget | null }
  | { kind: 'test'; target: LedgerAnchorTarget }
  | { kind: 'delete'; target: LedgerAnchorTarget }
  | { kind: 'disable'; target: LedgerAnchorTarget }
  | null
>(null)
const notice = ref<string | null>(null)

async function refresh(): Promise<void> {
  await Promise.all([
    client.invalidateQueries({ queryKey: keys.ledgerAnchorTargets }),
    client.invalidateQueries({ queryKey: keys.ledgerStatus }),
    client.invalidateQueries({ queryKey: ['ledger'] }),
  ])
}
const toggle = useMutation({
  mutationFn: (v: { t: LedgerAnchorTarget; enabled: boolean }) =>
    endpoints.updateLedgerAnchorTarget(v.t.id, { enabled: v.enabled }),
  onSuccess: async (t) => {
    dialog.value = null
    notice.value = `${safeText(t.name, 63)} is ${t.enabled ? 'used' : 'not used'} from now on.`
    await refresh()
  },
})
const remove = useMutation({
  mutationFn: (t: LedgerAnchorTarget) => endpoints.deleteLedgerAnchorTarget(t.id),
  onSuccess: async (_r, t) => {
    dialog.value = null
    notice.value = `${safeText(t.name, 63)} was deleted. Its records stay and keep its name.`
    await refresh()
  },
})
function askDisable(t: LedgerAnchorTarget): void {
  toggle.reset()
  if (t.enabled && enabled.value.length === 1) dialog.value = { kind: 'disable', target: t }
  else toggle.mutate({ t, enabled: !t.enabled })
}
function closeDialog(): void {
  dialog.value = null
  toggle.reset()
  remove.reset()
}
function saved(t: LedgerAnchorTarget): void {
  notice.value = `${safeText(t.name, 63)} was saved.`
}
</script>

<template>
  <div class="stack-lg">
    <section class="card stack" aria-labelledby="targets-h" data-testid="anchor-targets">
      <div class="row-between">
        <h2 id="targets-h">Anchor targets</h2>
        <GateButton
          op="createLedgerAnchorTarget"
          variant="primary"
          @click="dialog = { kind: 'edit', target: null }"
          >Add a target</GateButton
        >
      </div>
      <p class="muted">
        An anchor stores the signed checkpoint of the ledger, or a proof of its time, somewhere that
        the people who run this cluster cannot change. Without one, the ledger can be rewritten and
        nobody outside would know. Nothing is anchored until you add a target and turn it on.
      </p>
      <p v-if="notice" class="notice tone-ok" role="status" data-testid="anchoring-notice">
        <span aria-hidden="true">&#10003;</span><span>{{ notice }}</span>
      </p>
      <QueryBoundary
        :loading="targets.isPending.value"
        :error="targets.error.value"
        @retry="targets.refetch()"
      >
        <p
          v-if="noneEnabled"
          class="notice tone-warn"
          role="status"
          data-testid="anchoring-not-anchored"
        >
          <span aria-hidden="true">&#9650;</span><span>{{ NO_TARGET_ENABLED_TEXT }}</span>
        </p>
        <p
          v-else-if="oneKind"
          class="notice tone-info"
          role="status"
          data-testid="anchoring-one-kind"
        >
          <span aria-hidden="true">&#9432;</span><span>{{ ONE_KIND_TEXT }}</span>
        </p>
        <div v-if="list.length === 0" class="empty" data-testid="no-targets">
          <strong>No anchor target yet</strong>
          <span class="muted"
            >Start with a WORM store: it works on a private network, under a different
            administrator, and needs no third party.</span
          >
        </div>
        <TableWrap v-else>
          <table class="table">
            <caption class="sr-only">
              Anchor targets
            </caption>
            <thead>
              <tr>
                <th scope="col">Target</th>
                <th scope="col">Used</th>
                <th scope="col">State</th>
                <th scope="col">Newest anchor</th>
                <th scope="col">Credentials</th>
                <th scope="col">Last test</th>
                <th scope="col"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="t in list" :key="t.id" :data-testid="`target-${t.id}`">
                <th scope="row">
                  <SafeName :value="t.name" kind="text" :max="63" />
                  <div class="muted">{{ kindLabel(t.kind) }}</div>
                  <div class="muted wrap">{{ configSummary(t) }}</div>
                </th>
                <td>
                  <span class="badge" :class="t.enabled ? 'tone-ok' : 'tone-neutral'">
                    <span aria-hidden="true">{{ t.enabled ? '✓' : '○' }}</span>
                    {{ t.enabled ? 'Used' : 'Not used' }}
                  </span>
                </td>
                <td data-testid="target-state"><StatusBadge :meta="targetStateMeta(t.state)" /></td>
                <td data-testid="target-window">{{ windowText(t.newestAnchoredAt) }}</td>
                <td data-testid="target-credentials">
                  {{ t.credentialsSet ? 'Stored' : 'Not set' }}
                </td>
                <td data-testid="target-last-test">
                  <template v-if="t.lastTest">
                    <StatusBadge :meta="testSummary(t.lastTest)" />
                    <div v-if="t.lastTest.testedAt" class="muted">
                      {{ formatDateTime(t.lastTest.testedAt) }}
                    </div>
                  </template>
                  <span v-else class="muted">Never tested</span>
                </td>
                <td>
                  <div class="row">
                    <GateButton
                      op="testLedgerAnchorTarget"
                      small
                      :aria-label="`Test ${safeText(t.name, 63)}`"
                      @click="dialog = { kind: 'test', target: t }"
                      >Test</GateButton
                    >
                    <GateButton
                      op="updateLedgerAnchorTarget"
                      small
                      :aria-label="`Edit ${safeText(t.name, 63)}`"
                      @click="dialog = { kind: 'edit', target: t }"
                      >Edit</GateButton
                    >
                    <GateButton
                      op="updateLedgerAnchorTarget"
                      small
                      :disabled="toggle.isPending.value"
                      :aria-label="`${t.enabled ? 'Turn off' : 'Turn on'} ${safeText(t.name, 63)}`"
                      @click="askDisable(t)"
                      >{{ t.enabled ? 'Turn off' : 'Turn on' }}</GateButton
                    >
                    <GateButton
                      op="deleteLedgerAnchorTarget"
                      small
                      variant="danger"
                      :aria-label="`Delete ${safeText(t.name, 63)}`"
                      @click="dialog = { kind: 'delete', target: t }"
                      >Delete</GateButton
                    >
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
        <ProblemNotice v-if="toggle.error.value && dialog === null" :error="toggle.error.value" />
      </QueryBoundary>
    </section>

    <section class="card stack" aria-labelledby="anchor-settings-h" data-testid="anchor-settings">
      <h2 id="anchor-settings-h">When it is anchored</h2>
      <ProblemNotice v-if="settings.error.value" :error="settings.error.value" />
      <p v-else-if="anchor" data-testid="anchor-settings-text">
        The newest aggregate checkpoint is sent to every used target every
        <strong>{{ anchor.intervalSeconds }} seconds</strong>. A checkpoint counts as anchored when
        at least <strong>{{ anchor.minimumAnchored }}</strong>
        {{ anchor.minimumAnchored === 1 ? 'target has' : 'targets have' }} anchored it.
        {{
          anchor.required
            ? 'Anchoring is required: the absence of an anchor raises a warning.'
            : 'Anchoring is not required: the absence of an anchor raises a notice.'
        }}
        Nothing is blocked either way.
        <RouterLink v-if="canSettings" :to="{ name: 'settings' }" data-testid="anchor-settings-link"
          >Change these on the Settings page</RouterLink
        >
      </p>
      <p v-else class="muted" role="status">Loading.</p>
    </section>

    <section class="stack" aria-labelledby="anchor-db-h">
      <h2 id="anchor-db-h">Records of a database</h2>
      <p v-if="databases.length === 0" class="muted" data-testid="no-ledger-database">
        No database uses the ledger, so there are no anchor records.
      </p>
      <template v-else>
        <div class="field" style="max-width: 20em">
          <label for="an-db">Database</label>
          <select id="an-db" v-model="dbModel">
            <option v-for="d in databases" :key="d" :value="d">{{ safeText(d) }}</option>
          </select>
        </div>
        <AnchorRecordsPanel
          v-if="props.databaseName"
          :key="props.databaseName"
          :database-name="props.databaseName"
          :targets="list"
          :targets-known="targets.data.value !== undefined"
        />
      </template>
    </section>

    <AnchorTargetDialog
      v-if="dialog?.kind === 'edit'"
      :key="dialog.target?.id ?? 'new'"
      :target="dialog.target"
      @close="closeDialog"
      @saved="saved"
    />
    <AnchorTestDialog v-if="dialog?.kind === 'test'" :target="dialog.target" @close="closeDialog" />
    <ConfirmDialog
      v-if="dialog?.kind === 'delete'"
      title="Delete this anchor target?"
      confirm-label="Delete the target"
      danger
      :type-to-confirm="dialog.target.name"
      :busy="remove.isPending.value"
      @cancel="closeDialog"
      @confirm="remove.mutate((dialog as { target: LedgerAnchorTarget }).target)"
    >
      <p>
        The target stops being used and its stored credentials are removed. Its anchor records stay
        and keep its name, and the evidence of anchored records stays valid. This cannot be undone.
      </p>
      <p
        v-if="dialog.target.enabled && enabled.length === 1"
        class="notice tone-warn"
        role="status"
      >
        <span aria-hidden="true">&#9650;</span>
        <span>This is the last target that is used. Afterwards the ledger is not anchored.</span>
      </p>
      <ProblemNotice v-if="remove.error.value" :error="remove.error.value" />
    </ConfirmDialog>
    <ConfirmDialog
      v-if="dialog?.kind === 'disable'"
      title="Turn off the last anchor target?"
      confirm-label="Turn it off"
      danger
      :busy="toggle.isPending.value"
      @cancel="closeDialog"
      @confirm="
        toggle.mutate({ t: (dialog as { target: LedgerAnchorTarget }).target, enabled: false })
      "
    >
      <p>
        This is the only target that is used. Afterwards nothing is anchored: the ledger can be
        rewritten undetected by anyone who controls this cluster and its backups. Records that are
        already anchored stay anchored.
      </p>
      <ProblemNotice v-if="toggle.error.value" :error="toggle.error.value" />
    </ConfirmDialog>
  </div>
</template>
