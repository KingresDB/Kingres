<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The Digests tab of the Ledger page: the digests of a database (with the aggregate hash and the anchors), the aggregate
// checkpoints, and the row inclusion proof (which is compared with the digests listed here).
import { computed, ref } from 'vue'
import TableWrap from '@/components/TableWrap.vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import QueryBoundary from '@/components/QueryBoundary.vue'
import GateButton from '@/components/GateButton.vue'
import NullableValue from '@/components/NullableValue.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import SafeName from '@/components/SafeName.vue'
import CursorResetNotice from '@/components/CursorResetNotice.vue'
import { endpoints } from '@/api/endpoints'
import { keys, usePagedQuery } from '@/api/queries'
import { formatDateTime, shortHash } from '@/lib/format'
import ProofResult from '@/components/ledger/ProofResult.vue'
import AggregateDialog from '@/components/ledger/AggregateDialog.vue'
import { saveTextFile } from '@/lib/download'
import { coverageOf, digestCoverageText, groupList } from '@/lib/ledger-coverage'
import {
  digestRelation,
  digestRelationText,
  type DigestRelation,
  type DigestChain,
  type RawDigest,
  type RawProof,
} from '@/lib/ledger-proof'
import { ApiError } from '@/api/problem'

const props = defineProps<{ databaseName: string }>()
const dbName = computed(() => props.databaseName)
const client = useQueryClient()
const openAggregate = ref<number | null>(null)
// 0.11.0: the aggregate checkpoints of the database, newest first (a server that does not build them yet says so).
const aggregates = usePagedQuery(
  () => [...keys.ledger(dbName.value), 'aggregates'],
  (cursor) => endpoints.listLedgerAggregates(dbName.value, { cursor, limit: 10 }),
  undefined,
  () => !!dbName.value,
)
/** The signed checkpoint, saved as the text the server sent (a signed note: its bytes are what is signed). */
function downloadCheckpoint(size: number, note: string): void {
  saveTextFile(`aggregate-checkpoint-${size}.txt`, note, 'text/plain')
}

// Ten digests a page, newest first; "Load more digests" follows the cursor (it is never stored).
const digests = usePagedQuery(
  () => [...keys.ledger(dbName.value), 'digests'],
  (cursor) => endpoints.listLedgerDigests(dbName.value, { cursor, limit: 10 }),
  undefined,
  () => !!dbName.value,
)

const generate = useMutation({
  mutationFn: () => endpoints.generateLedgerDigest(dbName.value),
  onSuccess: () => client.invalidateQueries({ queryKey: keys.ledger(dbName.value) }),
})

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
/** The proof on screen, when there is one that is still valid (no error after it). */
const shownProof = computed(() =>
  proof.value && !proof.value.shapeError && !proofApiError.value ? proof.value : null,
)
/** How each listed digest relates to the proof on screen (identifiers only; nothing is verified). */
const relations = computed(() => {
  const p = shownProof.value
  const map = new Map<number, DigestRelation>()
  if (p) for (const d of digests.items.value) map.set(d.id, digestRelation(p.view, d))
  return map
})
const relationCovers = (id: number): boolean => relations.value.get(id)?.state === 'covers'
const relationText = (id: number): string => {
  const r = relations.value.get(id)
  return r ? digestRelationText(r) : ''
}
const coveringCount = computed(
  () => [...relations.value.values()].filter((r) => r.state === 'covers').length,
)
const downloaded = ref<{
  id: number
  group: number
  relation: DigestRelation | null
  coverage: string
} | null>(null)
/** The file is saved exactly as sent; the warning about what the digest does not cover is only text on screen. */
function downloadDigest(d: RawDigest, c: DigestChain): void {
  saveTextFile(`digest-${d.id}-group-${c.shardGroupId}.json`, c.rawDocument)
  downloaded.value = {
    id: d.id,
    group: c.shardGroupId,
    relation: relations.value.get(d.id) ?? null,
    coverage: digestCoverageText(coverageOf(d)),
  }
}
</script>

<template>
  <div class="stack-lg">
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
        <strong>outside the database</strong>: the verifier needs one that an attacker with database
        access could not have changed. A document is saved byte for byte as the server sent it: do
        not open and save it again in an editor, the verifier needs the exact bytes. "By" names the
        role that asked for the digest, or <code>system</code> for the scheduled one.
      </p>
      <ProblemNotice v-if="generate.error.value" :error="generate.error.value" />
      <CursorResetNotice :shown="digests.cursorReset.value" @dismiss="digests.dismissCursorReset" />
      <QueryBoundary
        :loading="digests.query.isPending.value"
        :error="digests.query.error.value"
        :empty="digests.items.value.length === 0"
        empty-title="No digests yet"
        @retry="digests.query.refetch()"
      >
        <div
          v-if="shownProof"
          class="notice"
          :class="coveringCount > 0 ? 'tone-info' : 'tone-warn'"
          data-testid="digest-proof-summary"
        >
          <span aria-hidden="true">{{ coveringCount > 0 ? 'ⓘ' : '▲' }}</span>
          <span>
            <template v-if="coveringCount > 0"
              >{{ coveringCount }} of the {{ digests.items.value.length }} digests listed here have
              the same chain as the proof on screen and are not older than its block.</template
            >
            <template v-else
              >None of the {{ digests.items.value.length }} digests listed here covers the proof on
              screen<template v-if="digests.query.hasNextPage.value"
                >; load more digests or generate a new one</template
              >.</template
            >
            This compares identifiers only; it does not verify anything. The check is done by
            <code>kingres-verify</code>.
          </span>
        </div>
        <p v-if="downloaded" class="notice tone-info" role="status" data-testid="digest-downloaded">
          <span aria-hidden="true">&#9432;</span>
          <span>
            Saved the document of chain {{ downloaded.group }} of digest {{ downloaded.id }}.
            <strong v-if="downloaded.coverage" data-testid="digest-downloaded-coverage">{{
              downloaded.coverage
            }}</strong>
            <template v-if="downloaded.relation">{{
              digestRelationText(downloaded.relation)
            }}</template>
          </span>
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
                <th v-if="shownProof" scope="col">Compared with the proof</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="d in digests.items.value" :key="d.id">
                <th scope="row">{{ d.id }}</th>
                <td>
                  {{ formatDateTime(d.generatedAt) }} by
                  <NullableValue :value="d.generatedBy" safe />
                </td>
                <td>
                  <p
                    v-if="digestCoverageText(coverageOf(d))"
                    class="badge tone-warn"
                    :data-testid="`digest-coverage-${d.id}`"
                  >
                    <span aria-hidden="true">&#9650;</span>
                    {{ digestCoverageText(coverageOf(d)) }}
                  </p>
                  <ul class="chain-heads">
                    <li v-for="c in d.chains" :key="c.shardGroupId">
                      g{{ c.shardGroupId }}@{{ c.blockHeight }}
                      <button
                        type="button"
                        class="btn btn-small"
                        :aria-label="
                          'Download document of chain ' + c.shardGroupId + ' of digest ' + d.id
                        "
                        @click="downloadDigest(d, c)"
                      >
                        Download document
                      </button>
                    </li>
                  </ul>
                </td>
                <td>
                  <span v-if="d.aggregateHash" class="mono" :title="d.aggregateHash">{{
                    shortHash(d.aggregateHash)
                  }}</span>
                  <span v-else class="muted">None (not for an incomplete digest)</span>
                  <div
                    v-if="d.aggregateHash && d.aggregateSize != null"
                    :data-testid="`digest-aggregate-${d.id}`"
                  >
                    <span class="muted">Aggregate of {{ d.aggregateSize }} rounds.</span>
                    <button
                      type="button"
                      class="btn btn-small"
                      :aria-label="'Open aggregate ' + d.aggregateSize"
                      @click="openAggregate = d.aggregateSize"
                    >
                      Open aggregate
                    </button>
                  </div>
                </td>
                <td>
                  <template v-if="d.anchors.length">
                    <span v-for="(a, i) in d.anchors" :key="i"
                      ><template v-if="i > 0">, </template
                      ><SafeName :value="a.target" kind="text" :max="80" /> (<SafeName
                        :value="a.status"
                        kind="text"
                        :max="40"
                      />)</span
                    >
                  </template>
                  <template v-else
                    ><span class="badge tone-warn" data-testid="digest-not-anchored"
                      ><span aria-hidden="true">&#9650;</span> Not anchored</span
                    ></template
                  >
                </td>
                <td v-if="shownProof" :data-testid="`digest-relation-${d.id}`">
                  <template v-if="relations.get(d.id)">
                    <span class="badge" :class="relationCovers(d.id) ? 'tone-info' : 'tone-warn'"
                      ><span aria-hidden="true">{{ relationCovers(d.id) ? 'ⓘ' : '▲' }}</span>
                      {{ relationCovers(d.id) ? 'Same chain' : 'Does not cover the proof' }}</span
                    >
                    <div class="muted">{{ relationText(d.id) }}</div>
                  </template>
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
        <button
          v-if="digests.query.hasNextPage.value"
          type="button"
          class="btn"
          :disabled="digests.query.isFetchingNextPage.value"
          @click="digests.query.fetchNextPage()"
        >
          Load more digests
        </button>
      </QueryBoundary>
    </section>

    <section class="card stack" aria-labelledby="aggregates-h">
      <h2 id="aggregates-h">Aggregate checkpoints</h2>
      <p class="muted">
        An aggregate checkpoint is signed and commits to the head of every chain of the database at
        one round. It is what an anchor covers. Open one to see its members and what it proves and
        does not prove.
      </p>
      <QueryBoundary
        :loading="aggregates.query.isPending.value"
        :error="aggregates.query.error.value"
        :empty="aggregates.items.value.length === 0"
        empty-title="No aggregate checkpoints yet"
        empty-hint="They exist once the publication is on and a signing key is active."
        @retry="aggregates.query.refetch()"
      >
        <TableWrap>
          <table class="table">
            <caption class="sr-only">
              Aggregate checkpoints
            </caption>
            <thead>
              <tr>
                <th scope="col" class="num">Size</th>
                <th scope="col">Generated</th>
                <th scope="col">Root</th>
                <th scope="col">Covers</th>
                <th scope="col"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="a in aggregates.items.value" :key="a.size">
                <th scope="row" class="num">{{ a.size }}</th>
                <td>{{ formatDateTime(a.generatedAt) }}</td>
                <td class="mono" :title="a.root">{{ shortHash(a.root) }}</td>
                <td>
                  <span v-if="a.complete" class="badge tone-ok"
                    ><span aria-hidden="true">&#10003;</span> Every shard group</span
                  >
                  <span v-else class="badge tone-warn"
                    ><span aria-hidden="true">&#9650;</span> Does not cover
                    {{ groupList(a.uncoveredShardGroupIds) }}</span
                  >
                </td>
                <td>
                  <button
                    type="button"
                    class="btn btn-small"
                    :aria-label="'Open aggregate checkpoint ' + a.size"
                    @click="openAggregate = a.size"
                  >
                    Open
                  </button>
                  <button
                    type="button"
                    class="btn btn-small"
                    :aria-label="'Download checkpoint ' + a.size"
                    @click="downloadCheckpoint(a.size, a.checkpoint)"
                  >
                    Download checkpoint
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
        <button
          v-if="aggregates.query.hasNextPage.value"
          type="button"
          class="btn"
          :disabled="aggregates.query.isFetchingNextPage.value"
          @click="aggregates.query.fetchNextPage()"
        >
          Load more aggregate checkpoints
        </button>
      </QueryBoundary>
    </section>

    <section class="card stack" aria-labelledby="proof-h">
      <h2 id="proof-h">Row inclusion proof</h2>
      <p class="muted">
        Shows that a ledger entry for a row is part of a block, and gives you the proof as a file
        for the verifier <code>kingres-verify</code>. Name the row by its whole primary key; the
        server reads the row with your own database role.
      </p>
      <form class="form-row" @submit.prevent="proofMutation.mutate()">
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
            >The ledger transaction that removed it; only the owner of the table can read it.</span
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
      <ProblemNotice v-if="proofApiError" :error="proofApiError" @retry="proofMutation.mutate()" />
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
  </div>
  <AggregateDialog
    v-if="openAggregate !== null"
    :database-name="dbName"
    :size="openAggregate"
    @close="openAggregate = null"
  />
</template>

<style scoped>
.chain-heads {
  margin: 0;
  padding: 0;
  list-style: none;
}
</style>
