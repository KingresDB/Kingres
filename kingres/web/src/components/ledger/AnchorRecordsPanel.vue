<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The anchor records of one database (contract 0.11.0, design 6.2, 6.6 and 8.5): one record per target for each aggregate
// checkpoint, with its state in words, the attempts, the next attempt, the time of the anchor and the last error. A record
// that failed or expired can be tried again by an operator; an anchored record has evidence that can be downloaded in the
// formats of its kind. "Anchor now" asks for the newest checkpoint to be anchored without waiting for the interval. Each
// operation that the server does not build yet is shown as what it is. A name or an error text from the server is drawn
// through SafeName only.
import { computed, ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import CursorResetNotice from '@/components/CursorResetNotice.vue'
import GateButton from '@/components/GateButton.vue'
import NullableValue from '@/components/NullableValue.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import TableWrap from '@/components/TableWrap.vue'
import { endpoints } from '@/api/endpoints'
import { keys, usePagedQuery } from '@/api/queries'
import { saveBlobFile } from '@/lib/download'
import { safeText } from '@/lib/safe-text'
import { formatDateTime, formatNumber } from '@/lib/format'
import {
  canRetry,
  evidenceFormats,
  EVIDENCE_TEXT,
  FORMAT_LABEL,
  hasEvidence,
  kindShort,
  OTS_WAIT_TEXT,
  recordStateMeta,
  type EvidenceFormat,
} from '@/lib/ledger-anchor'
import type { LedgerAnchor, LedgerAnchorTarget } from '@/api/types'

// `targetsKnown` is false while the list of targets could not be read (an error, or a server that does not know it yet):
// then nothing is said about whether a target is enabled, and Anchor now is left to the server to answer.
const props = withDefaults(
  defineProps<{ databaseName: string; targets: LedgerAnchorTarget[]; targetsKnown?: boolean }>(),
  { targetsKnown: true },
)
const client = useQueryClient()
const db = computed(() => props.databaseName)

const targetFilter = ref('')
const stateFilter = ref('')
const STATES = [
  ['pending', 'Waiting to be sent'],
  ['submitted', 'Sent, not final'],
  ['anchored', 'Anchored'],
  ['failed', 'Failed'],
  ['expired', 'Never confirmed'],
  ['superseded', 'Replaced by a newer checkpoint'],
] as const

const records = usePagedQuery(
  () => [...keys.ledger(db.value), 'anchors', targetFilter.value, stateFilter.value],
  (cursor) =>
    endpoints.listLedgerAnchors(db.value, {
      cursor,
      limit: 20,
      ...(targetFilter.value ? { targetId: Number(targetFilter.value) } : {}),
      ...(stateFilter.value ? { state: stateFilter.value as LedgerAnchor['state'] } : {}),
    }),
  undefined,
  () => !!db.value,
)

const enabledTargets = computed(() => props.targets.filter((t) => t.enabled))
const message = ref<{ tone: 'ok' | 'info'; text: string } | null>(null)

async function refresh(): Promise<void> {
  await Promise.all([
    client.invalidateQueries({ queryKey: keys.ledger(db.value) }),
    client.invalidateQueries({ queryKey: keys.ledgerAnchorTargets }),
    client.invalidateQueries({ queryKey: keys.ledgerStatus }),
  ])
}

const anchorNow = useMutation({
  mutationFn: () => endpoints.anchorLedgerNow(db.value),
  onSuccess: async (list) => {
    const n = list.items.length
    message.value = {
      tone: 'ok',
      text: `${n} ${n === 1 ? 'record was' : 'records were'} created. The servers send them within a minute.`,
    }
    await refresh()
  },
})

const retrying = ref<number | null>(null)
const retry = useMutation({
  mutationFn: (r: LedgerAnchor) => {
    retrying.value = r.id
    return endpoints.retryLedgerAnchor(db.value, r.id)
  },
  onSuccess: async (r) => {
    message.value = { tone: 'ok', text: `Record ${r.id} will be sent again.` }
    await refresh()
  },
  onSettled: () => {
    retrying.value = null
  },
})

const downloading = ref<string | null>(null)
const download = useMutation({
  mutationFn: async ({ r, format }: { r: LedgerAnchor; format: EvidenceFormat }) => {
    downloading.value = `${r.id}-${format}`
    const blob = await endpoints.downloadLedgerAnchor(db.value, r.id, format)
    saveBlobFile(`anchor-${r.id}-evidence.${format}`, blob)
  },
  onSettled: () => {
    downloading.value = null
  },
})

const anyLate = computed(() =>
  records.items.value.some((r) => r.kind === 'ots' && r.state === 'submitted'),
)
const targetOptions = computed(() => props.targets)
const noEnabled = computed(() => props.targetsKnown && enabledTargets.value.length === 0)
const anchorNowBlocked = computed(() =>
  noEnabled.value ? 'No anchor target is enabled. Add and enable one first.' : '',
)
</script>

<template>
  <section class="card stack" aria-labelledby="records-h" data-testid="anchor-records">
    <div class="row-between">
      <h2 id="records-h">Anchor records</h2>
      <GateButton
        op="anchorLedgerNow"
        :blocked-reason="anchorNowBlocked"
        :disabled="anchorNow.isPending.value || !db"
        @click="anchorNow.mutate()"
        >Anchor now</GateButton
      >
    </div>
    <p class="muted">
      A record is made for each enabled target and each aggregate checkpoint. The newest checkpoint
      covers every earlier one, so only the newest waits to be anchored; an older one that was not
      sent yet is replaced.
    </p>
    <p
      v-if="message"
      class="notice"
      :class="`tone-${message.tone}`"
      role="status"
      data-testid="records-message"
    >
      <span aria-hidden="true">&#10003;</span><span>{{ message.text }}</span>
    </p>
    <ProblemNotice v-if="anchorNow.error.value" :error="anchorNow.error.value" />
    <ProblemNotice v-if="retry.error.value" :error="retry.error.value" />
    <ProblemNotice v-if="download.error.value" :error="download.error.value" />

    <div class="form-row">
      <div class="field">
        <label for="rec-target">Target</label>
        <select id="rec-target" v-model="targetFilter">
          <option value="">All targets</option>
          <option v-for="t in targetOptions" :key="t.id" :value="String(t.id)">
            {{ safeText(t.name, 63) }}
          </option>
        </select>
      </div>
      <div class="field">
        <label for="rec-state">State</label>
        <select id="rec-state" v-model="stateFilter">
          <option value="">All states</option>
          <option v-for="s in STATES" :key="s[0]" :value="s[0]">{{ s[1] }}</option>
        </select>
      </div>
    </div>

    <CursorResetNotice :shown="records.cursorReset.value" @dismiss="records.dismissCursorReset()" />
    <QueryBoundary
      :loading="records.query.isPending.value"
      :error="records.query.error.value"
      :empty="records.items.value.length === 0"
      :empty-title="targetFilter || stateFilter ? 'No record matches' : 'No anchor records yet'"
      :empty-hint="
        targetFilter || stateFilter
          ? 'Change the filters to see more.'
          : noEnabled
            ? 'Nothing is anchored while no target is enabled.'
            : 'Records appear when the next aggregate checkpoint is anchored, or when you choose Anchor now.'
      "
      @retry="records.query.refetch()"
    >
      <p v-if="anyLate" class="muted" data-testid="ots-wait">{{ OTS_WAIT_TEXT }}</p>
      <TableWrap>
        <table class="table">
          <caption class="sr-only">
            Anchor records
          </caption>
          <thead>
            <tr>
              <th scope="col">Target</th>
              <th scope="col">Checkpoint size</th>
              <th scope="col">State</th>
              <th scope="col">Attempts</th>
              <th scope="col">Next attempt</th>
              <th scope="col">Anchored</th>
              <th scope="col">Time vouched for</th>
              <th scope="col">Last error</th>
              <th scope="col"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in records.items.value" :key="r.id" :data-testid="`record-${r.id}`">
              <th scope="row">
                <SafeName :value="r.targetName" kind="text" :max="63" />
                <span v-if="r.targetId === null" class="muted"> (deleted)</span>
                <div class="muted">{{ kindShort(r.kind) }}</div>
              </th>
              <td class="num">{{ formatNumber(r.aggregateSize) }}</td>
              <td data-testid="record-state"><StatusBadge :meta="recordStateMeta(r.state)" /></td>
              <td class="num">{{ r.attempts }}</td>
              <td><NullableValue :value="r.nextAttemptAt" :format="formatDateTime" /></td>
              <td><NullableValue :value="r.anchoredAt" :format="formatDateTime" /></td>
              <td><NullableValue :value="r.evidenceTime" :format="formatDateTime" /></td>
              <td data-testid="record-error">
                <SafeName
                  v-if="r.lastError"
                  :value="r.lastError"
                  kind="text"
                  :max="300"
                  multiline
                />
                <span v-else class="muted">None</span>
              </td>
              <td>
                <div class="row">
                  <GateButton
                    v-if="canRetry(r)"
                    op="retryLedgerAnchor"
                    small
                    :disabled="retrying === r.id"
                    :aria-label="`Retry record ${r.id}`"
                    @click="retry.mutate(r)"
                    >Retry</GateButton
                  >
                  <template v-if="hasEvidence(r)">
                    <button
                      v-for="f in evidenceFormats(r.kind)"
                      :key="f"
                      type="button"
                      class="btn btn-small"
                      :disabled="downloading === `${r.id}-${f}`"
                      :aria-label="`Download the evidence of record ${r.id} as ${FORMAT_LABEL[f]}`"
                      :data-testid="`download-${r.id}-${f}`"
                      @click="download.mutate({ r, format: f })"
                    >
                      Evidence ({{ FORMAT_LABEL[f] }})
                    </button>
                  </template>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </TableWrap>
      <button
        v-if="records.query.hasNextPage.value"
        type="button"
        class="btn"
        :disabled="records.query.isFetchingNextPage.value"
        @click="records.query.fetchNextPage()"
      >
        Load more records
      </button>
      <p class="muted">{{ EVIDENCE_TEXT }}</p>
    </QueryBoundary>
  </section>
</template>
