<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// One verification run of a ledger: its result, the findings (at most 100) and the task that
// recorded it. Findings are text of the server and are drawn through SafeName.
import { computed } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import AppDialog from '@/components/AppDialog.vue'
import NullableValue from '@/components/NullableValue.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import TableWrap from '@/components/TableWrap.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import {
  coverageOf,
  groupList,
  verificationBadge,
  verificationCoverageText,
} from '@/lib/ledger-coverage'
import { formatDateTime, formatNumber } from '@/lib/format'

const props = defineProps<{ databaseName: string; verificationId: number }>()
defineEmits<{ close: [] }>()

const query = useQuery({
  queryKey: computed(() => [
    ...keys.ledger(props.databaseName),
    'verification',
    props.verificationId,
  ]),
  queryFn: () => endpoints.getLedgerVerification(props.databaseName, props.verificationId),
})
const v = computed(() => query.data.value)
const coverage = computed(() => (v.value ? coverageOf(v.value) : null))
const coverageText = computed(() =>
  coverage.value ? verificationCoverageText(coverage.value) : '',
)

const KIND_LABEL = {
  blockHashMismatch: 'Block hash differs',
  prevHashMismatch: 'Link to the previous block differs',
  merkleRootMismatch: 'Merkle root differs',
  rowHashMismatch: 'Row hash differs',
  digestMismatch: 'Differs from the digest',
  missingBlock: 'Block is missing',
  other: 'Other check (see the text)',
} as const
</script>

<template>
  <AppDialog :title="`Verification ${verificationId}`" wide @close="$emit('close')">
    <p v-if="query.isPending.value" role="status">Loading the verification ...</p>
    <ProblemNotice v-else-if="query.error.value" :error="query.error.value" />
    <template v-else-if="v">
      <p
        v-if="v.state === 'passed' && coverageText"
        class="notice tone-warn"
        role="status"
        data-testid="verification-coverage"
      >
        <span aria-hidden="true">&#9650;</span>
        <span>{{ coverageText }}</span>
      </p>
      <dl class="kv">
        <dt>Result</dt>
        <dd><StatusBadge :meta="verificationBadge(v)" /></dd>
        <dt>Shard groups covered</dt>
        <dd data-testid="verification-covered">
          <template v-if="coverage?.state === 'complete'"
            >All shard groups of the database</template
          >
          <template v-else-if="coverage?.state === 'incomplete'"
            >Not all. Not covered: {{ groupList(coverage.uncovered) }}</template
          >
          <template v-else>Not stated by the server</template>
        </dd>
        <dt>Scope</dt>
        <dd>
          {{
            v.scope === 'full' ? 'Everything from the first block' : 'Since the last verified block'
          }}
        </dd>
        <dt>Started</dt>
        <dd>{{ formatDateTime(v.startedAt) }} by <NullableValue :value="v.startedBy" safe /></dd>
        <dt>Finished</dt>
        <dd>
          <NullableValue :value="v.finishedAt" :format="formatDateTime" />
        </dd>
        <dt>Checked</dt>
        <dd>
          <NullableValue :value="v.blocksChecked" :format="formatNumber" /> block(s),
          <NullableValue :value="v.rowsChecked" :format="formatNumber" /> row(s)
        </dd>
        <dt>Task</dt>
        <dd>
          <RouterLink
            v-if="v.taskId !== null"
            :to="{ name: 'tasks', query: { task: String(v.taskId) } }"
            >Task {{ v.taskId }}</RouterLink
          >
          <span v-else class="muted">Recorded without a task (not started from this API).</span>
        </dd>
      </dl>
      <h3>Findings</h3>
      <p v-if="v.inconsistencies.length === 0" class="muted">None.</p>
      <template v-else>
        <p v-if="v.inconsistencies.length >= 100" class="muted" role="status">
          Only the first 100 findings are kept.
        </p>
        <TableWrap>
          <table class="table">
            <caption class="sr-only">
              Findings
            </caption>
            <thead>
              <tr>
                <th scope="col">Kind</th>
                <th scope="col">Chain</th>
                <th scope="col" class="num">Block</th>
                <th scope="col">Table</th>
                <th scope="col">Detail</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="(i, n) in v.inconsistencies" :key="n">
                <td>{{ KIND_LABEL[i.kind] }}</td>
                <td>Group {{ i.shardGroupId }}</td>
                <td class="num">
                  <NullableValue :value="i.blockHeight ?? null" :format="formatNumber" />
                </td>
                <td><NullableValue :value="i.table ?? null" safe /></td>
                <td><SafeName :value="i.detail" kind="text" :max="300" multiline /></td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </template>
    </template>
    <template #footer>
      <button type="button" class="btn" @click="$emit('close')">Close</button>
    </template>
  </AppDialog>
</template>

<style scoped>
.kv {
  display: grid;
  grid-template-columns: fit-content(40%) minmax(0, 1fr);
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.kv dd {
  margin: 0;
  overflow-wrap: anywhere;
}
</style>
