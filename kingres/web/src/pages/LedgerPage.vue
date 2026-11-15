<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Ledger: per-database status (one hash chain per shard group), blocks, digests, verification and
// row inclusion proofs.
import { computed, ref, watch } from 'vue'
import { safeText } from '@/lib/safe-text'
import TableWrap from '@/components/TableWrap.vue'
import { useRoute, useRouter } from 'vue-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import GateButton from '@/components/GateButton.vue'
import NullableValue from '@/components/NullableValue.vue'
import NotAvailable from '@/components/NotAvailable.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import TaskProgress from '@/components/TaskProgress.vue'
import SafeName from '@/components/SafeName.vue'
import { describeProblem } from '@/api/problem'
import { endpoints } from '@/api/endpoints'
import { keys, useLedgerStatusList, usePagedQuery } from '@/api/queries'
import { useTaskAction } from '@/composables/useTask'
import { LEDGER_MODE_LABEL, LEDGER_STATE } from '@/lib/status'
import { formatAgo, formatDateTime, formatNumber, shortHash } from '@/lib/format'
import ProofResult from '@/components/ledger/ProofResult.vue'
import VerificationDialog from '@/components/ledger/VerificationDialog.vue'
import { saveTextFile } from '@/lib/download'
import type { RawProof } from '@/lib/ledger-proof'
import { ApiError } from '@/api/problem'

const route = useRoute()
const router = useRouter()
const client = useQueryClient()
const statusList = useLedgerStatusList()
const dbName = ref(typeof route.query.db === 'string' ? route.query.db : '')

watch(
  () => statusList.data.value,
  (d) => {
    const items = d?.items ?? []
    if (!items.some((s) => s.databaseName === dbName.value))
      dbName.value = items[0]?.databaseName ?? ''
  },
  { immediate: true },
)
watch(dbName, (v) => {
  void router.replace({ query: v ? { db: v } : {} })
})

const status = computed(() =>
  statusList.data.value?.items.find((s) => s.databaseName === dbName.value),
)
const chainId = ref<number | null>(null)
watch(
  status,
  (s) => {
    if (s && !s.chains.some((c) => c.shardGroupId === chainId.value))
      chainId.value = s.chains[0]?.shardGroupId ?? null
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
const digests = useQuery({
  queryKey: computed(() => [...keys.ledger(dbName.value), 'digests']),
  queryFn: async () => {
    const page = await endpoints.listLedgerDigests(dbName.value, { limit: 10 })
    return { items: page.items, moreNotShown: !!page.nextCursor }
  },
  enabled: computed(() => !!dbName.value),
})
const verifications = usePagedQuery(
  () => [...keys.ledger(dbName.value), 'verifications'],
  (cursor) => endpoints.listLedgerVerifications(dbName.value, { cursor, limit: 10 }),
  5000,
  () => !!dbName.value,
)
const openVerification = ref<number | null>(null)

const generate = useMutation({
  mutationFn: () => endpoints.generateLedgerDigest(dbName.value),
  onSuccess: () => client.invalidateQueries({ queryKey: keys.ledger(dbName.value) }),
})
const scope = ref<'full' | 'sinceLastVerified'>('sinceLastVerified')
const verify = useTaskAction(
  () => endpoints.startLedgerVerification(dbName.value, { scope: scope.value }),
  {
    invalidate: () => [keys.ledger(dbName.value), keys.ledgerStatus, keys.databases],
  },
)

// Row inclusion proof lookup
const proofForm = ref({
  schema: 'public',
  table: 'orders',
  key: '{"id": 9000000001}',
  op: 'I' as 'I' | 'D',
  ledgerTxId: '',
})
/** True when the text holds a number that a 64-bit float cannot represent exactly. */
function unsafeNumber(text: string): boolean {
  try {
    const parsed: unknown = JSON.parse(text)
    return (
      typeof parsed === 'object' &&
      parsed !== null &&
      Object.values(parsed).some(
        (v) => typeof v === 'number' && Number.isInteger(v) && !Number.isSafeInteger(v),
      )
    )
  } catch {
    return false
  }
}
const proof = ref<RawProof | null>(null)
const keyError = ref('')
const proofMutation = useMutation({
  mutationFn: () => {
    let primaryKey: Record<string, string | number | boolean>
    try {
      const parsed: unknown = JSON.parse(proofForm.value.key)
      if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed))
        throw new Error('not an object')
      primaryKey = parsed as Record<string, string | number | boolean>
      // JSON numbers become 64-bit floats here: a larger number would be sent as another number.
      for (const value of Object.values(primaryKey)) {
        if (typeof value === 'number' && !Number.isSafeInteger(value)) {
          throw new Error('unsafe number')
        }
      }
    } catch {
      keyError.value = unsafeNumber(proofForm.value.key)
        ? 'A number larger than 2^53 cannot be sent exactly as a JSON number. Write it as a string, for example {"id": "9007199254740993"}.'
        : 'Enter the primary key as a JSON object, for example {"id": 42}.'
      return Promise.reject(new Error(keyError.value))
    }
    keyError.value = ''
    let ledgerTxId: number | undefined
    if (proofForm.value.op === 'D') {
      ledgerTxId = Number(proofForm.value.ledgerTxId)
      if (!Number.isSafeInteger(ledgerTxId) || ledgerTxId < 0) {
        keyError.value = 'A removed version is named by its transaction id: enter a whole number.'
        return Promise.reject(new Error(keyError.value))
      }
    }
    return endpoints.createInclusionProof(dbName.value, {
      schema: proofForm.value.schema,
      table: proofForm.value.table,
      primaryKey,
      op: proofForm.value.op,
      ...(ledgerTxId === undefined ? {} : { ledgerTxId }),
    })
  },
  onSuccess: (p) => {
    proof.value = p
  },
  onError: () => {
    proof.value = null
  },
})
const proofApiError = computed(() => (keyError.value ? null : proofMutation.error.value))
const proofPending = computed(
  () =>
    proofApiError.value instanceof ApiError && proofApiError.value.code === 'ledger_entry_pending',
)
const proofHint = computed(() => {
  const e = proofApiError.value
  if (!(e instanceof ApiError)) return ''
  if (e.code === 'not_found')
    return 'No row with this primary key was found in this table, or a row-level policy hides it from your database role.'
  if (e.code === 'forbidden')
    return 'Your database role has no privilege on this table. The removed versions of a row can be read by the owner of the table only.'
  return ''
})
/** The row is not in a block yet: close the block with a digest, then ask again. */
async function generateAndAskAgain(): Promise<void> {
  await generate.mutateAsync()
  proofMutation.mutate()
}
function downloadDigest(id: number, group: number, text: string): void {
  saveTextFile(`digest-${id}-group-${group}.json`, text)
}
</script>

<template>
  <PageHeader
    title="Ledger"
    subtitle="Tamper-evident history: hash chains, digests and verification."
  />
  <section
    v-if="statusList.data.value?.unreadable.length"
    class="notice tone-danger"
    role="alert"
    aria-label="The ledger status could not be read"
  >
    <span aria-hidden="true">&#10006;</span>
    <div class="stack" style="gap: var(--s-1)">
      <strong
        >The ledger status could not be read for these databases; the list may be
        incomplete.</strong
      >
      <ul style="margin: 0; padding-left: 1.2em">
        <li v-for="u in statusList.data.value.unreadable" :key="u.databaseName">
          <SafeName :value="u.databaseName" :max="60" />:
          <SafeName :value="describeProblem(u.error).message" kind="text" :max="200" />
        </li>
      </ul>
    </div>
  </section>
  <QueryBoundary
    :loading="statusList.isPending.value"
    :error="statusList.error.value"
    :empty="
      (statusList.data.value?.items.length ?? 0) === 0 &&
      (statusList.data.value?.unreadable.length ?? 0) === 0
    "
    empty-title="No database uses the ledger"
    empty-hint="Turn on the ledger for a database to see its chains here."
    @retry="statusList.refetch()"
  >
    <div class="stack-lg">
      <div class="field" style="max-width: 20em">
        <label for="l-db">Database</label>
        <select id="l-db" v-model="dbName">
          <option
            v-for="s in statusList.data.value?.items"
            :key="s.databaseName"
            :value="s.databaseName"
          >
            {{ safeText(s.databaseName) }}
          </option>
        </select>
      </div>

      <template v-if="status">
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
                    <StatusBadge
                      v-if="c.verificationState"
                      :meta="LEDGER_STATE[c.verificationState]"
                    />
                    <NotAvailable v-else />
                  </td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
        </section>

        <section class="card stack" aria-labelledby="verify-h">
          <h2 id="verify-h">Verification</h2>
          <div class="row">
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
                    <td><StatusBadge :meta="LEDGER_STATE[v.state]" /></td>
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

        <section class="card stack" aria-labelledby="digests-h">
          <div class="row-between">
            <h2 id="digests-h">Digests</h2>
            <GateButton
              op="generateLedgerDigest"
              :disabled="generate.isPending.value"
              @click="generate.mutate()"
              >Generate digest</GateButton
            >
          </div>
          <p class="muted">
            A digest is the head of each chain. Save its documents
            <strong>outside the database</strong>: the verifier needs one that an attacker with
            database access could not have changed.
          </p>
          <ProblemNotice v-if="generate.error.value" :error="generate.error.value" />
          <QueryBoundary
            :loading="digests.isPending.value"
            :error="digests.error.value"
            :empty="digests.data.value?.items.length === 0"
            empty-title="No digests yet"
          >
            <p v-if="digests.data.value?.moreNotShown" class="notice tone-warn" role="status">
              <span aria-hidden="true">&#9650;</span>
              <span>More digests exist than are shown here; only the latest are listed.</span>
            </p>
            <TableWrap>
              <table class="table">
                <caption class="sr-only">
                  Digests
                </caption>
                <thead>
                  <tr>
                    <th scope="col">#</th>
                    <th scope="col">Generated</th>
                    <th scope="col">Chain heads</th>
                    <th scope="col">Aggregate hash</th>
                    <th scope="col">Anchors</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="d in digests.data.value?.items" :key="d.id">
                    <th scope="row">{{ d.id }}</th>
                    <td>
                      {{ formatDateTime(d.generatedAt) }} by
                      <NullableValue :value="d.generatedBy" safe />
                    </td>
                    <td>
                      <ul class="chain-heads">
                        <li v-for="c in d.chains" :key="c.shardGroupId">
                          g{{ c.shardGroupId }}@{{ c.blockHeight }}
                          <button
                            type="button"
                            class="btn btn-small"
                            :aria-label="
                              'Download document of chain ' + c.shardGroupId + ' of digest ' + d.id
                            "
                            @click="downloadDigest(d.id, c.shardGroupId, c.rawDocument)"
                          >
                            Download document
                          </button>
                        </li>
                      </ul>
                    </td>
                    <td class="mono" :title="d.aggregateHash ?? undefined">
                      {{ shortHash(d.aggregateHash) }}
                    </td>
                    <td>
                      {{
                        d.anchors.length
                          ? d.anchors.map((a) => `${a.target} (${a.status})`).join(', ')
                          : 'None yet'
                      }}
                    </td>
                  </tr>
                </tbody>
              </table>
            </TableWrap>
          </QueryBoundary>
        </section>

        <section class="card stack" aria-labelledby="blocks-h">
          <h2 id="blocks-h">Blocks</h2>
          <div class="field" style="max-width: 16em">
            <label for="b-chain">Chain</label>
            <select id="b-chain" v-model="chainId">
              <option v-for="c in status.chains" :key="c.shardGroupId" :value="c.shardGroupId">
                Shard group {{ c.shardGroupId }}
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

        <section class="card stack" aria-labelledby="proof-h">
          <h2 id="proof-h">Row inclusion proof</h2>
          <p class="muted">
            Shows that a ledger entry for a row is part of a block, and gives you the proof as a
            file for the verifier <code>kingres-verify</code>. Name the row by its whole primary
            key; the server reads the row with your own database role.
          </p>
          <form
            class="row"
            style="align-items: flex-end; gap: var(--s-4)"
            @submit.prevent="proofMutation.mutate()"
          >
            <div class="field">
              <label for="p-schema">Schema</label
              ><input id="p-schema" v-model="proofForm.schema" class="input mono" />
            </div>
            <div class="field">
              <label for="p-table">Table</label
              ><input id="p-table" v-model="proofForm.table" class="input mono" />
            </div>
            <div class="field">
              <label for="p-key">Primary key (JSON)</label>
              <input
                id="p-key"
                v-model="proofForm.key"
                class="input mono"
                :aria-invalid="keyError ? 'true' : undefined"
              />
              <span v-if="keyError" class="error" role="alert">{{ keyError }}</span>
            </div>
            <div class="field">
              <label for="p-op">Entry</label>
              <select id="p-op" v-model="proofForm.op">
                <option value="I">Insert (the row was written)</option>
                <option value="D">Delete (the row was removed or replaced)</option>
              </select>
            </div>
            <div v-if="proofForm.op === 'D'" class="field">
              <label for="p-txid">Transaction id of the removed version</label>
              <input
                id="p-txid"
                v-model="proofForm.ledgerTxId"
                class="input mono"
                inputmode="numeric"
              />
              <span class="hint"
                >The ledger transaction that removed it; only the owner of the table can read
                it.</span
              >
            </div>
            <GateButton
              op="createInclusionProof"
              type="submit"
              variant="primary"
              :disabled="proofMutation.isPending.value"
              >Look up proof</GateButton
            >
          </form>
          <ProblemNotice
            v-if="proofApiError"
            :error="proofApiError"
            @retry="proofMutation.mutate()"
          />
          <p v-if="proofHint" class="muted" data-testid="proof-hint">{{ proofHint }}</p>
          <div v-if="proofPending">
            <GateButton
              op="generateLedgerDigest"
              :disabled="generate.isPending.value || proofMutation.isPending.value"
              @click="generateAndAskAgain()"
              >Generate digest and ask again</GateButton
            >
          </div>
          <ProofResult v-if="proof && !proofApiError" :proof="proof" />
        </section>
      </template>
    </div>
  </QueryBoundary>
</template>

<style scoped>
.chain-heads {
  margin: 0;
  padding: 0;
  list-style: none;
}
</style>
