<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Ledger: per-database status (one hash chain per shard group), blocks, digests, verification and
// row inclusion proofs.
import { computed, ref, watch } from 'vue'
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
import { endpoints } from '@/api/endpoints'
import { every, keys, useLedgerStatusList, usePagedQuery } from '@/api/queries'
import { useTaskAction } from '@/composables/useTask'
import { LEDGER_MODE_LABEL, LEDGER_STATE } from '@/lib/status'
import { formatAgo, formatDateTime, formatNumber, shortHash } from '@/lib/format'
import type { InclusionProof } from '@/api/types'

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
  queryFn: () => endpoints.listLedgerDigests(dbName.value, { limit: 10 }),
  enabled: computed(() => !!dbName.value),
})
const verifications = useQuery({
  queryKey: computed(() => [...keys.ledger(dbName.value), 'verifications']),
  queryFn: () => endpoints.listLedgerVerifications(dbName.value, { limit: 10 }),
  enabled: computed(() => !!dbName.value),
  refetchInterval: every(5000),
})

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
})
const proof = ref<InclusionProof | null>(null)
const keyError = ref('')
const proofMutation = useMutation({
  mutationFn: () => {
    let primaryKey: Record<string, string | number | boolean>
    try {
      const parsed: unknown = JSON.parse(proofForm.value.key)
      if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed))
        throw new Error('not an object')
      primaryKey = parsed as Record<string, string | number | boolean>
    } catch {
      keyError.value = 'Enter the primary key as a JSON object, for example {"id": 42}.'
      return Promise.reject(new Error(keyError.value))
    }
    keyError.value = ''
    return endpoints.createInclusionProof(dbName.value, {
      schema: proofForm.value.schema,
      table: proofForm.value.table,
      primaryKey,
      op: proofForm.value.op,
    })
  },
  onSuccess: (p) => {
    proof.value = p
  },
})
const proofApiError = computed(() => (keyError.value ? null : proofMutation.error.value))
</script>

<template>
  <PageHeader
    title="Ledger"
    subtitle="Tamper-evident history: hash chains, digests and verification."
  />
  <QueryBoundary
    :loading="statusList.isPending.value"
    :error="statusList.error.value"
    :empty="(statusList.data.value?.items.length ?? 0) === 0"
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
            {{ s.databaseName }}
          </option>
        </select>
      </div>

      <template v-if="status">
        <section class="card stack" aria-labelledby="chains-h">
          <div class="row-between">
            <h2 id="chains-h">Status of {{ status.databaseName }}</h2>
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
          <div class="table-wrap">
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
          </div>
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
            :loading="verifications.isPending.value"
            :error="verifications.error.value"
            :empty="verifications.data.value?.items.length === 0"
            empty-title="No verifications yet"
          >
            <div class="table-wrap">
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
                    <th scope="col">Inconsistencies</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="v in verifications.data.value?.items" :key="v.id">
                    <th scope="row">{{ v.id }}</th>
                    <td><StatusBadge :meta="LEDGER_STATE[v.state]" /></td>
                    <td>{{ v.scope === 'full' ? 'Full' : 'Since last verified' }}</td>
                    <td>
                      {{ formatDateTime(v.startedAt) }} by
                      <NullableValue :value="v.startedBy" />
                    </td>
                    <td class="num">
                      <NullableValue :value="v.blocksChecked" :format="formatNumber" />
                    </td>
                    <td class="num">
                      <NullableValue :value="v.rowsChecked" :format="formatNumber" />
                    </td>
                    <td>
                      <ul v-if="v.inconsistencies.length" style="margin: 0; padding-left: 1.1em">
                        <li v-for="(i, n) in v.inconsistencies" :key="n">
                          {{ i.kind }} in group {{ i.shardGroupId }}: {{ i.detail }}
                        </li>
                      </ul>
                      <span v-else class="muted">None</span>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </QueryBoundary>
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
          <ProblemNotice v-if="generate.error.value" :error="generate.error.value" />
          <QueryBoundary
            :loading="digests.isPending.value"
            :error="digests.error.value"
            :empty="digests.data.value?.items.length === 0"
            empty-title="No digests yet"
          >
            <div class="table-wrap">
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
                      <NullableValue :value="d.generatedBy" />
                    </td>
                    <td>
                      {{ d.chains.map((c) => `g${c.shardGroupId}@${c.blockHeight}`).join(', ') }}
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
            </div>
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
            <div class="table-wrap">
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
            </div>
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
            Shows that a row is part of a block, with the Merkle path to the block and to the
            digest.
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
            <GateButton
              op="createInclusionProof"
              type="submit"
              variant="primary"
              :disabled="proofMutation.isPending.value"
              >Look up proof</GateButton
            >
          </form>
          <ProblemNotice v-if="proofApiError" :error="proofApiError" />
          <div v-if="proof" class="stack" data-testid="proof">
            <dl class="kv">
              <dt>Entry</dt>
              <dd>{{ proof.op === 'I' ? 'Insert' : 'Delete' }}</dd>
              <dt>Leaf hash</dt>
              <dd class="mono">{{ proof.leafHash }}</dd>
              <dt>Transaction</dt>
              <dd>{{ proof.ledgerTxId }} (leaf {{ proof.leafIndex }})</dd>
              <dt>Block</dt>
              <dd>
                group {{ proof.block.shardGroupId }}, height {{ formatNumber(proof.block.height) }},
                <span class="mono">{{ shortHash(proof.block.blockHash) }}</span>
              </dd>
              <dt>Merkle root</dt>
              <dd class="mono">{{ proof.transactionMerkleRoot }}</dd>
              <dt>Digest</dt>
              <dd>{{ proof.digestId ?? 'not covered by a digest yet' }}</dd>
            </dl>
            <ol class="mono path">
              <li v-for="(s, i) in proof.merklePath" :key="i">
                {{ s.position }}: {{ shortHash(s.hash) }}
              </li>
            </ol>
          </div>
        </section>
      </template>
    </div>
  </QueryBoundary>
</template>

<style scoped>
.kv {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.kv dd {
  margin: 0;
  overflow-wrap: anywhere;
}
.path {
  margin: 0;
}
</style>
