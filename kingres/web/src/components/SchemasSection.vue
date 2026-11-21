<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The schemas of a database (contract 0.15.0) with where each distributed one lives, and the three actions on a schema:
// Distribute, Move and Make local. The actions are GateButtons (a role below the one the operation needs sees them disabled and
// told which role); a schema that is moving has all of them disabled with the reason.
import { computed, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import GateButton from './GateButton.vue'
import NullableValue from './NullableValue.vue'
import QueryBoundary from './QueryBoundary.vue'
import SafeName from './SafeName.vue'
import TableWrap from './TableWrap.vue'
import DistributeSchemaDialog from './DistributeSchemaDialog.vue'
import MoveSchemaDialog from './MoveSchemaDialog.vue'
import MakeLocalDialog from './MakeLocalDialog.vue'
import { useSchemaMoveTasks, useSchemas } from '@/api/queries'
import { formatBytes, formatNumber } from '@/lib/format'
import { MODE_LABEL, type ShardingMode } from '@/lib/sharding-mode'
import type { SchemaSummary } from '@/api/types'

const props = defineProps<{ databaseName: string; shardingOn: boolean; mode: ShardingMode }>()

const schemas = useSchemas(() => props.databaseName)
const rows = computed(() => schemas.data.value?.items ?? [])
const anyMoving = computed(() => rows.value.some((s) => s.distribution?.state === 'moving'))
// The list has the target group of a move but not its task; the badge finds the task through the task list.
const moveTasks = useSchemaMoveTasks(() => props.databaseName, anyMoving)

const dialog = ref<{ kind: 'distribute' | 'move' | 'local'; schema: SchemaSummary } | null>(null)
// The page is reused when the address changes to another database: a dialog opened for the previous one must not stay.
watch(
  () => props.databaseName,
  () => {
    dialog.value = null
  },
)

/** The task that moves a schema, when the task list has it (a link to the task, or nothing). */
const taskOf = (s: SchemaSummary): number | undefined => moveTasks.data.value?.get(s.name)

const movingTo = (s: SchemaSummary): number | null =>
  s.distribution?.state === 'moving' ? s.distribution.movingToShardGroupId : null

/** Why an action cannot be used on this schema right now (not a role problem), or an empty text. */
function movingReason(s: SchemaSummary): string {
  const target = movingTo(s)
  return s.distribution?.state === 'moving'
    ? `This schema is moving${target === null ? '' : ` to group ${target}`}. Wait until the move has finished.`
    : ''
}
function distributeReason(s: SchemaSummary): string {
  if (!props.shardingOn) return 'Turn on sharding for this database first.'
  if (props.mode === 'row')
    return 'The sharding mode of this database is Rows. Change it to Schemas or Both in the settings first.'
  return movingReason(s)
}
</script>

<template>
  <section aria-labelledby="schemas-h" class="stack" data-testid="schemas-section">
    <h2 id="schemas-h">Schemas</h2>
    <p v-if="!shardingOn" class="muted">
      Sharding is off, so schemas cannot be distributed. Turn it on in the settings.
    </p>
    <p v-else-if="mode === 'row'" class="muted">
      This database shards by <strong>{{ MODE_LABEL[mode] }}</strong>, so schemas are not
      distributed. Change the mode in the settings to distribute schemas.
    </p>
    <QueryBoundary
      :loading="schemas.isPending.value"
      :error="schemas.error.value"
      :empty="rows.length === 0"
      empty-title="No schemas"
      empty-hint="Create schemas with SQL in the query workbench."
      @retry="schemas.refetch()"
    >
      <TableWrap>
        <table class="table" data-testid="schemas-table">
          <caption class="sr-only">
            Schemas of the database
          </caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Owner</th>
              <th scope="col">Distribution</th>
              <th scope="col">Server group and server</th>
              <th scope="col" class="num">Tables</th>
              <th scope="col" class="num">Size</th>
              <th scope="col">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="s in rows" :key="JSON.stringify(s.name)">
              <th scope="row" class="mono"><SafeName :value="s.name" :max="80" /></th>
              <td><SafeName :value="s.owner" :max="60" /></td>
              <td>
                <template v-if="s.distribution === null">
                  <span class="badge tone-neutral">Local</span>
                </template>
                <template v-else-if="s.distribution.state === 'moving'">
                  <span class="badge tone-info" data-testid="moving-badge"
                    >Moving to group {{ s.distribution.movingToShardGroupId ?? '?' }}</span
                  >
                  <RouterLink
                    v-if="taskOf(s) !== undefined"
                    :to="{ name: 'tasks', query: { task: taskOf(s) } }"
                    data-testid="moving-task-link"
                  >
                    Task {{ taskOf(s) }}
                  </RouterLink>
                </template>
                <template v-else>
                  <span class="badge tone-ok">Distributed</span>
                </template>
              </td>
              <td>
                <template v-if="s.distribution">
                  Group {{ s.distribution.shardGroupId }}
                  <span class="muted">
                    <template v-if="s.distribution.serverId"
                      >on <SafeName :value="s.distribution.serverId" :max="60"
                    /></template>
                    <template v-else>(no primary now)</template>
                  </span>
                </template>
                <template v-else>-</template>
              </td>
              <td class="num">{{ formatNumber(s.tableCount) }}</td>
              <td class="num"><NullableValue :value="s.sizeBytes" :format="formatBytes" /></td>
              <td>
                <div class="row">
                  <GateButton
                    v-if="s.distribution === null"
                    op="distributeSchema"
                    small
                    :blocked-reason="distributeReason(s)"
                    @click="dialog = { kind: 'distribute', schema: s }"
                  >
                    Distribute schema
                  </GateButton>
                  <template v-else>
                    <GateButton
                      op="moveSchema"
                      small
                      :blocked-reason="movingReason(s)"
                      @click="dialog = { kind: 'move', schema: s }"
                    >
                      Move schema
                    </GateButton>
                    <GateButton
                      op="undistributeSchema"
                      small
                      :blocked-reason="movingReason(s)"
                      @click="dialog = { kind: 'local', schema: s }"
                    >
                      Make local
                    </GateButton>
                  </template>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </TableWrap>
      <p v-if="schemas.data.value?.moreNotShown" class="notice tone-warn" role="status">
        <span aria-hidden="true">&#9650;</span>
        <span>More schemas exist than are shown here.</span>
      </p>
    </QueryBoundary>
  </section>

  <DistributeSchemaDialog
    v-if="dialog?.kind === 'distribute'"
    :database-name="databaseName"
    :schema="dialog.schema"
    @close="dialog = null"
  />
  <MoveSchemaDialog
    v-if="dialog?.kind === 'move'"
    :database-name="databaseName"
    :schema="dialog.schema"
    @close="dialog = null"
  />
  <MakeLocalDialog
    v-if="dialog?.kind === 'local'"
    :database-name="databaseName"
    :schema="dialog.schema"
    @close="dialog = null"
  />
</template>
