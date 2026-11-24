<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The time-partitioning policy of a table, its health, and the runs of the scheduler (contract 0.18.0). The health line says how
// far ahead partitions exist (and warns when that is below what the policy keeps), when the next run is and how the last one
// ended. The buttons are for kingres_admin (set or edit, run now, remove); every other role reads the policy and the runs and
// sees the buttons disabled with the reason. The runs list opens one run with its actions.
import { computed, ref } from 'vue'
import GateButton from '../GateButton.vue'
import NullableValue from '../NullableValue.vue'
import QueryBoundary from '../QueryBoundary.vue'
import SafeName from '../SafeName.vue'
import StatusBadge from '../StatusBadge.vue'
import TableWrap from '../TableWrap.vue'
import PartitionPolicyDialog from './PartitionPolicyDialog.vue'
import PartitionRunDialog from './PartitionRunDialog.vue'
import RemovePolicyDialog from './RemovePolicyDialog.vue'
import RunPolicyDialog from './RunPolicyDialog.vue'
import OwnerNotice from './OwnerNotice.vue'
import { usePartitionPolicy, usePartitionRuns } from '@/api/queries'
import { formatDateTime } from '@/lib/format'
import {
  HEALTH_PROBLEM_LABEL,
  RUN_STATE,
  TRIGGER_LABEL,
  durationText,
  healthText,
} from '@/lib/analytics'
import type { Table } from '@/api/types'

const props = defineProps<{ databaseName: string; table: Table; heldReason: string }>()
const enabled = computed(() => props.table.partitioning?.columnType !== undefined)
const policy = usePartitionPolicy(
  () => props.databaseName,
  () => props.table.schema,
  () => props.table.name,
  enabled,
)
const runs = usePartitionRuns(
  () => props.databaseName,
  () => props.table.schema,
  () => props.table.name,
  computed(() => enabled.value && policy.data.value != null),
)
const p = computed(() => policy.data.value ?? null)
const dialog = ref<'edit' | 'run' | 'remove' | null>(null)
const openRun = ref<number | null>(null)

const seconds = (a: string, b: string | null): string | null => {
  if (b === null) return null
  const s = Math.max(0, Math.round((Date.parse(b) - Date.parse(a)) / 1000))
  return s < 60 ? `${s} s` : `${Math.floor(s / 60)} min ${s % 60} s`
}
const retentionText = (x: NonNullable<typeof p.value>) =>
  x.retention ? `${durationText(x.retention)} (${x.retentionAction ?? 'drop'})` : 'Off'
</script>

<template>
  <section class="stack" aria-labelledby="policy-h" data-testid="policy-panel">
    <div class="row-between">
      <h2 id="policy-h">Time partitioning</h2>
      <span class="row">
        <GateButton
          v-if="!policy.error.value"
          op="putPartitionPolicy"
          small
          data-testid="policy-edit"
          :blocked-reason="heldReason"
          @click="dialog = 'edit'"
        >
          {{ p ? 'Edit policy' : 'Set a policy' }}
        </GateButton>
        <GateButton
          v-if="p"
          op="runPartitionPolicy"
          small
          data-testid="policy-run"
          :blocked-reason="heldReason"
          @click="dialog = 'run'"
        >
          Run now
        </GateButton>
        <GateButton
          v-if="p"
          op="deletePartitionPolicy"
          small
          variant="danger"
          data-testid="policy-remove"
          :blocked-reason="heldReason"
          @click="dialog = 'remove'"
        >
          Remove policy
        </GateButton>
      </span>
    </div>
    <p v-if="!enabled" class="muted" data-testid="policy-no-time-key">
      A policy needs a table that is partitioned by range on a date or timestamp column. This table
      is not.
    </p>
    <QueryBoundary
      v-else
      :loading="policy.isPending.value"
      :error="policy.error.value"
      @retry="policy.refetch()"
    >
      <p v-if="!p" class="muted" data-testid="policy-none">
        This table has no policy, so its partitions are made and removed by hand.
      </p>
      <template v-else>
        <OwnerNotice v-if="p.ownerTrusted === false" :owner="p.tableOwner" />
        <div class="card stack" data-testid="policy-facts">
          <p data-testid="policy-health" :data-ok="p.health.ok ? 'true' : 'false'">
            <StatusBadge
              :meta="
                p.health.ok
                  ? { tone: 'ok', icon: '✓', label: 'Healthy' }
                  : { tone: 'warn', icon: '▲', label: 'Needs attention' }
              "
            />
            <span data-testid="policy-ahead-text">
              {{ healthText(p.health, p.createAhead ?? 'P7D') }}</span
            >
          </p>
          <ul v-if="p.health.problems.length > 0" class="problems" data-testid="policy-problems">
            <li v-for="x in p.health.problems" :key="x">
              {{ HEALTH_PROBLEM_LABEL[x] ?? x }}
            </li>
          </ul>
          <dl class="kv">
            <dt>Partition column</dt>
            <dd>
              <code><SafeName :value="p.partitionColumn" :max="60" /></code>
              <span class="muted"> ({{ p.partitionColumnType }})</span>
            </dd>
            <dt>Owner of the table</dt>
            <dd data-testid="policy-owner">
              <NullableValue :value="p.tableOwner" safe />
              <span v-if="p.ownerTrusted !== undefined" class="muted">
                ({{ p.ownerTrusted ? 'trusted' : 'not trusted' }})</span
              >
            </dd>
            <dt>Each partition covers</dt>
            <dd>{{ durationText(p.partitionEvery) }}</dd>
            <dt>Created ahead</dt>
            <dd>{{ durationText(p.createAhead ?? 'P7D') }}</dd>
            <dt>Retention</dt>
            <dd>{{ retentionText(p) }}</dd>
            <dt>Converted to columnar after</dt>
            <dd>{{ p.columnarAfter ? durationText(p.columnarAfter) : 'Off' }}</dd>
            <dt>Time zone</dt>
            <dd><SafeName :value="p.timezone ?? 'UTC'" kind="text" :max="60" /></dd>
            <dt>Runs every</dt>
            <dd>{{ durationText(p.runEvery ?? 'PT1H') }}</dd>
            <dt>Policy</dt>
            <dd>
              <StatusBadge
                :meta="
                  p.enabled === false
                    ? { tone: 'neutral', icon: '○', label: 'Off' }
                    : { tone: 'ok', icon: '✓', label: 'On' }
                "
              />
            </dd>
            <dt>Next run</dt>
            <dd><NullableValue :value="p.nextRunAt" :format="formatDateTime" /></dd>
            <dt>Last run</dt>
            <dd data-testid="policy-last-run">
              <template v-if="p.lastRun">
                <StatusBadge :meta="RUN_STATE[p.lastRun.state]" />
                {{ formatDateTime(p.lastRun.startedAt) }}
              </template>
              <template v-else>No run yet.</template>
            </dd>
          </dl>
        </div>

        <h3 id="runs-h">Runs of the scheduler</h3>
        <QueryBoundary
          :loading="runs.isPending.value"
          :error="runs.error.value"
          :empty="(runs.data.value?.items.length ?? 0) === 0"
          empty-title="No runs yet"
          empty-hint="The scheduler takes the table at its next tick, or use Run now."
          @retry="runs.refetch()"
        >
          <TableWrap>
            <table class="table" data-testid="policy-runs">
              <caption class="sr-only">
                Runs of the partition scheduler for this table
              </caption>
              <thead>
                <tr>
                  <th scope="col">Started</th>
                  <th scope="col">Took</th>
                  <th scope="col">Why</th>
                  <th scope="col">State</th>
                  <th scope="col">By</th>
                  <th scope="col">Details</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="r in runs.data.value?.items ?? []"
                  :key="r.id"
                  :data-testid="`run-${r.id}`"
                >
                  <th scope="row">{{ formatDateTime(r.startedAt) }}</th>
                  <td><NullableValue :value="seconds(r.startedAt, r.finishedAt)" /></td>
                  <td>{{ TRIGGER_LABEL[r.trigger] ?? r.trigger }}</td>
                  <td><StatusBadge :meta="RUN_STATE[r.state]" /></td>
                  <td><SafeName :value="r.startedBy" kind="text" :max="60" /></td>
                  <td>
                    <button
                      type="button"
                      class="btn btn-small"
                      :data-testid="`run-open-${r.id}`"
                      :aria-label="`Show the actions of run ${r.id}`"
                      @click="openRun = r.id"
                    >
                      Actions
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
        </QueryBoundary>
      </template>
    </QueryBoundary>
  </section>

  <PartitionPolicyDialog
    v-if="dialog === 'edit'"
    :database-name="databaseName"
    :table="table"
    @close="dialog = null"
  />
  <RunPolicyDialog
    v-if="dialog === 'run'"
    :database-name="databaseName"
    :table="table"
    @close="dialog = null"
  />
  <RemovePolicyDialog
    v-if="dialog === 'remove'"
    :database-name="databaseName"
    :table="table"
    @close="dialog = null"
  />
  <PartitionRunDialog
    v-if="openRun !== null"
    :database-name="databaseName"
    :table="table"
    :run-id="openRun"
    @close="openRun = null"
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
}
.problems {
  margin: 0;
  padding-left: 1.2em;
}
</style>
