<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The Overview tab of the Ledger page: the status of a database (one hash chain per shard group, and since 0.11.0 what is
// published and anchored), the verification and the blocks (with the column "Signed").
import { computed, ref, watch } from 'vue'
import TableWrap from '@/components/TableWrap.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import NullableValue from '@/components/NullableValue.vue'
import NotAvailable from '@/components/NotAvailable.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import TaskProgress from '@/components/TaskProgress.vue'
import { safeText } from '@/lib/safe-text'
import { keys, usePagedQuery } from '@/api/queries'
import { endpoints } from '@/api/endpoints'
import { useTaskAction } from '@/composables/useTask'
import { LEDGER_MODE_LABEL, LEDGER_STATE } from '@/lib/status'
import { formatAgo, formatDateTime, formatNumber, shortHash } from '@/lib/format'
import { anchoringMeta, NOT_ANCHORED_TEXT, publicationMeta } from '@/lib/ledger-signing'
import VerificationDialog from '@/components/ledger/VerificationDialog.vue'
import {
  coverageOf,
  statusCoverageText,
  verificationBadge,
  verificationCoverageText,
} from '@/lib/ledger-coverage'
import type { LedgerStatus } from '@/api/types'

const props = defineProps<{ databaseName: string; status: LedgerStatus }>()
const dbName = computed(() => props.databaseName)
const status = computed(() => props.status)
const statusCoverage = computed(() => (status.value ? coverageOf(status.value) : null))
/** The chains of the status, then the shard groups it names as not covered (their blocks can still be asked for). */
const chainOptions = computed(() => [
  ...(status.value?.chains ?? []).map((c) => ({ id: c.shardGroupId, covered: true })),
  ...(status.value?.uncoveredShardGroupIds ?? [])
    .filter((n) => !status.value?.chains.some((c) => c.shardGroupId === n))
    .map((id) => ({ id, covered: false })),
])
const chainId = ref<number | null>(null)
watch(
  status,
  (s) => {
    if (s && !chainOptions.value.some((c) => c.id === chainId.value))
      chainId.value = chainOptions.value[0]?.id ?? null
  },
  { immediate: true },
)

const blocks = usePagedQuery(
  () => [...keys.ledger(dbName.value), 'blocks', chainId.value],
  (cursor) =>
    endpoints.listLedgerBlocks(dbName.value, {
      cursor,
      limit: 25,
      shardGroupId: chainId.value ?? undefined,
    }),
  undefined,
  () => !!dbName.value && chainId.value !== null,
)
const verifications = usePagedQuery(
  () => [...keys.ledger(dbName.value), 'verifications'],
  (cursor) => endpoints.listLedgerVerifications(dbName.value, { cursor, limit: 10 }),
  5000,
  () => !!dbName.value,
)
const openVerification = ref<number | null>(null)

const scope = ref<'full' | 'sinceLastVerified'>('sinceLastVerified')
const verify = useTaskAction(
  () => endpoints.startLedgerVerification(dbName.value, { scope: scope.value }),
  {
    invalidate: () => [keys.ledger(dbName.value), keys.ledgerStatus, keys.databases],
  },
)
</script>

<template>
  <div class="stack-lg">
    <section class="card stack" aria-labelledby="chains-h">
      <div class="row-between">
        <h2 id="chains-h">Status of {{ safeText(status.databaseName, 80) }}</h2>
        <StatusBadge
          v-if="status.verificationState"
          :meta="LEDGER_STATE[status.verificationState]"
        />
        <NotAvailable v-else />
      </div>
      <p class="muted">
        <NullableValue :value="status.mode" :format="(m) => LEDGER_MODE_LABEL[m]" />,
        <NullableValue :value="status.ledgerTableCount" /> ledger table(s), digest every
        <NullableValue :value="status.digestIntervalSeconds" :format="(n) => `${n} s`" />.
        <template v-if="status.lastVerifiedAt"
          >Last verified {{ formatAgo(status.lastVerifiedAt) }}.</template
        >
      </p>
      <ul class="plain" data-testid="publication-anchoring">
        <li data-testid="status-publication">
          <span class="label">Publication</span>
          <template v-if="status.publication">
            <StatusBadge :meta="publicationMeta(status.publication.state)" />
            <span v-if="status.publication.lastCheckpointAt" class="muted"
              >newest signed checkpoint {{ formatAgo(status.publication.lastCheckpointAt) }}</span
            >
            <span v-else class="muted">no checkpoint signed yet</span>
          </template>
          <span v-else class="muted"
            >This server does not report the publication of the ledger.</span
          >
        </li>
        <li data-testid="status-anchoring">
          <span class="label">Anchoring</span>
          <template v-if="status.anchoring">
            <StatusBadge :meta="anchoringMeta(status.anchoring.state)" />
            <span class="muted">
              {{ status.anchoring.targets }}
              {{ status.anchoring.targets === 1 ? 'target' : 'targets' }} enabled<template
                v-if="status.anchoring.newestAnchoredAt"
                >, newest anchor {{ formatAgo(status.anchoring.newestAnchoredAt) }}</template
              >
            </span>
          </template>
          <span v-else class="muted">This server does not report the anchoring of the ledger.</span>
        </li>
      </ul>
      <p
        v-if="status.anchoring && status.anchoring.state === 'none'"
        class="notice tone-warn"
        role="status"
        data-testid="not-anchored"
      >
        <span aria-hidden="true">&#9650;</span>
        <span>{{ NOT_ANCHORED_TEXT }}</span>
      </p>
      <p
        v-if="statusCoverage && statusCoverageText(statusCoverage)"
        class="notice tone-warn"
        role="status"
        data-testid="status-coverage"
      >
        <span aria-hidden="true">&#9650;</span>
        <span>{{ statusCoverageText(statusCoverage) }}</span>
      </p>
      <p v-if="status.verificationState === 'passed'" class="muted">
        "Last check passed" is the state of the latest verification only. Open it under Verification
        to see which shard groups it covered.
      </p>
      <TableWrap>
        <table class="table">
          <caption class="sr-only">
            Chains
          </caption>
          <thead>
            <tr>
              <th scope="col">Chain (shard group)</th>
              <th scope="col" class="num">Latest block</th>
              <th scope="col">Latest hash</th>
              <th scope="col">Latest block time</th>
              <th scope="col" class="num">Last verified block</th>
              <th scope="col">State</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="c in status.chains" :key="c.shardGroupId">
              <th scope="row">Group {{ c.shardGroupId }}</th>
              <td class="num">
                <NullableValue :value="c.latestBlockHeight" :format="formatNumber" />
              </td>
              <td class="mono" :title="c.latestBlockHash ?? undefined">
                <NullableValue :value="c.latestBlockHash" :format="shortHash" />
              </td>
              <td><NullableValue :value="c.latestBlockAt" :format="(v) => formatAgo(v)" /></td>
              <td class="num">
                <NullableValue :value="c.lastVerifiedBlockHeight" :format="formatNumber" />
              </td>
              <td>
                <StatusBadge v-if="c.verificationState" :meta="LEDGER_STATE[c.verificationState]" />
                <NotAvailable v-else />
              </td>
            </tr>
          </tbody>
        </table>
      </TableWrap>
    </section>

    <section class="card stack" aria-labelledby="verify-h">
      <h2 id="verify-h">Verification</h2>
      <div class="form-row">
        <div class="field">
          <label for="v-scope">Scope</label>
          <select id="v-scope" v-model="scope">
            <option value="sinceLastVerified">Since last verified block</option>
            <option value="full">Everything from the first block</option>
          </select>
        </div>
        <GateButton
          op="startLedgerVerification"
          variant="primary"
          :disabled="verify.isBusy.value"
          @click="verify.run()"
          >Start verification</GateButton
        >
      </div>
      <ProblemNotice v-if="verify.error.value" :error="verify.error.value" />
      <TaskProgress :task="verify.task.value" :poll-error="verify.pollError.value" />
      <QueryBoundary
        :loading="verifications.query.isPending.value"
        :error="verifications.query.error.value"
        :empty="verifications.items.value.length === 0"
        empty-title="No verifications yet"
        empty-hint="Start a verification to record the first one."
        @retry="verifications.query.refetch()"
      >
        <TableWrap>
          <table class="table">
            <caption class="sr-only">
              Verifications
            </caption>
            <thead>
              <tr>
                <th scope="col">#</th>
                <th scope="col">State</th>
                <th scope="col">Scope</th>
                <th scope="col">Started</th>
                <th scope="col" class="num">Blocks</th>
                <th scope="col" class="num">Rows</th>
                <th scope="col">Findings</th>
                <th scope="col">Task</th>
                <th scope="col"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="v in verifications.items.value" :key="v.id">
                <th scope="row">{{ v.id }}</th>
                <td>
                  <StatusBadge :meta="verificationBadge(v)" />
                  <div
                    v-if="v.state === 'passed' && verificationCoverageText(coverageOf(v))"
                    class="muted"
                    :data-testid="`verification-coverage-${v.id}`"
                  >
                    {{ verificationCoverageText(coverageOf(v)) }}
                  </div>
                </td>
                <td>{{ v.scope === 'full' ? 'Full' : 'Since last verified' }}</td>
                <td>
                  {{ formatDateTime(v.startedAt) }} by
                  <NullableValue :value="v.startedBy" safe />
                </td>
                <td class="num">
                  <NullableValue :value="v.blocksChecked" :format="formatNumber" />
                </td>
                <td class="num">
                  <NullableValue :value="v.rowsChecked" :format="formatNumber" />
                </td>
                <td>
                  <template v-if="v.inconsistencies.length">{{
                    v.inconsistencies.length
                  }}</template>
                  <span v-else class="muted">None</span>
                </td>
                <td>
                  <RouterLink
                    v-if="v.taskId !== null"
                    :to="{ name: 'tasks', query: { task: String(v.taskId) } }"
                    >Task {{ v.taskId }}</RouterLink
                  >
                  <span v-else class="muted">None</span>
                </td>
                <td>
                  <button
                    type="button"
                    class="btn btn-small"
                    :aria-label="'Details of verification ' + v.id"
                    @click="openVerification = v.id"
                  >
                    Details
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
        <button
          v-if="verifications.query.hasNextPage.value"
          type="button"
          class="btn"
          :disabled="verifications.query.isFetchingNextPage.value"
          @click="verifications.query.fetchNextPage()"
        >
          Load more verifications
        </button>
      </QueryBoundary>
      <VerificationDialog
        v-if="openVerification !== null"
        :database-name="dbName"
        :verification-id="openVerification"
        @close="openVerification = null"
      />
    </section>

    <section class="card stack" aria-labelledby="blocks-h">
      <h2 id="blocks-h">Blocks</h2>
      <p class="muted">
        "Signed" means that a signed checkpoint of the chain log covers the block. The newest blocks
        are not covered until the next checkpoint, and no block is signed while nothing is
        published.
      </p>
      <div class="field" style="max-width: 16em">
        <label for="b-chain">Chain</label>
        <select id="b-chain" v-model="chainId">
          <option v-for="c in chainOptions" :key="c.id" :value="c.id">
            Shard group {{ c.id }}<template v-if="!c.covered"> (not in the status above)</template>
          </option>
        </select>
      </div>
      <QueryBoundary
        :loading="blocks.query.isPending.value"
        :error="blocks.query.error.value"
        :empty="blocks.items.value.length === 0"
        empty-title="No blocks yet"
        @retry="blocks.query.refetch()"
      >
        <TableWrap>
          <table class="table">
            <caption class="sr-only">
              Blocks
            </caption>
            <thead>
              <tr>
                <th scope="col" class="num">Height</th>
                <th scope="col">Block hash</th>
                <th scope="col">Previous hash</th>
                <th scope="col">Time</th>
                <th scope="col" class="num">Transactions</th>
                <th scope="col">Signed</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="b in blocks.items.value" :key="`${b.shardGroupId}-${b.height}`">
                <td class="num">{{ formatNumber(b.height) }}</td>
                <td class="mono" :title="b.blockHash">{{ shortHash(b.blockHash) }}</td>
                <td class="mono" :title="b.prevHash ?? undefined">
                  {{ shortHash(b.prevHash) }}
                </td>
                <td>{{ formatDateTime(b.timestamp) }}</td>
                <td class="num">{{ b.transactionCount }}</td>
                <td :data-testid="`block-signed-${b.height}`">
                  <span v-if="b.signed"><span aria-hidden="true">&#10003;</span> Signed</span>
                  <span v-else class="muted"
                    ><span aria-hidden="true">&#9675;</span> Not signed yet</span
                  >
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
        <button
          v-if="blocks.query.hasNextPage.value"
          type="button"
          class="btn"
          :disabled="blocks.query.isFetchingNextPage.value"
          @click="blocks.query.fetchNextPage()"
        >
          Load more blocks
        </button>
      </QueryBoundary>
    </section>
  </div>
</template>

<style scoped>
.plain {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--s-2);
}
.plain li {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--s-1) var(--s-3);
}
.label {
  font-weight: 600;
  min-width: 7em;
}
</style>
