<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The Schemas section of the database page (contract 0.16.0): every schema with its distribution and the three actions.
// The buttons are GateButtons: a read-only role sees them disabled with the role that is needed.
import { ref } from 'vue'
import { RouterLink } from 'vue-router'
import SafeName from './SafeName.vue'
import TableWrap from './TableWrap.vue'
import QueryBoundary from './QueryBoundary.vue'
import GateButton from './GateButton.vue'
import NullableValue from './NullableValue.vue'
import SchemaDialogs from './SchemaDialogs.vue'
import { useSchemas } from '@/api/queries'
import { formatBytes, formatNumber } from '@/lib/format'
import type { SchemaSummary } from '@/api/types'

const props = defineProps<{ databaseName: string; mode?: string }>()
const schemas = useSchemas(() => props.databaseName)
const dialog = ref<{ kind: 'distribute' | 'move' | 'local'; schema: SchemaSummary } | null>(null)

const canDistribute = (): boolean => props.mode === 'schema' || props.mode === 'both'
</script>

<template>
  <section aria-labelledby="schemas-h" class="stack" data-testid="schemas-section">
    <h2 id="schemas-h">Schemas</h2>
    <QueryBoundary
      :loading="schemas.isPending.value"
      :error="schemas.error.value"
      :empty="(schemas.data.value?.length ?? 0) === 0"
      empty-title="No schemas"
      empty-hint="A database without sharding lists its schemas here, all local."
      @retry="schemas.refetch()"
    >
      <TableWrap>
        <table class="table">
          <caption class="sr-only">
            Schemas of the database
          </caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Owner</th>
              <th scope="col">Distribution</th>
              <th scope="col">Server group</th>
              <th scope="col" class="num">Tables</th>
              <th scope="col" class="num">Size</th>
              <th scope="col">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="s in schemas.data.value" :key="s.name" data-testid="schema-row">
              <th scope="row"><SafeName :value="s.name" :max="80" /></th>
              <td><SafeName :value="s.owner" :max="60" /></td>
              <td>
                <span
                  v-if="s.distribution && s.distribution.state === 'moving'"
                  class="badge tone-info"
                  data-testid="schema-moving"
                  ><span aria-hidden="true">&#9684;</span> Moving to group
                  {{ s.distribution.movingToShardGroupId ?? '?' }}
                  <RouterLink :to="{ name: 'tasks' }">task</RouterLink></span
                >
                <span v-else-if="s.distribution" class="badge tone-ok"
                  ><span aria-hidden="true">&#10003;</span> Distributed</span
                >
                <span v-else class="badge tone-neutral">Local</span>
              </td>
              <td>
                <template v-if="s.distribution"
                  >Group {{ s.distribution.shardGroupId
                  }}<template v-if="s.distribution.serverId">
                    on <SafeName :value="s.distribution.serverId" :max="60" /></template
                ></template>
                <template v-else>-</template>
              </td>
              <td class="num">{{ formatNumber(s.tableCount) }}</td>
              <td class="num"><NullableValue :value="s.sizeBytes" :format="formatBytes" /></td>
              <td>
                <div class="row">
                  <GateButton
                    v-if="!s.distribution"
                    op="distributeSchema"
                    small
                    :blocked-reason="
                      canDistribute()
                        ? ''
                        : 'The sharding mode of this database does not allow distributed schemas.'
                    "
                    data-testid="distribute-schema"
                    @click="dialog = { kind: 'distribute', schema: s }"
                    >Distribute schema</GateButton
                  >
                  <template v-else>
                    <GateButton
                      op="moveSchema"
                      small
                      :blocked-reason="
                        s.distribution.state === 'moving' ? 'The schema is being moved.' : ''
                      "
                      data-testid="move-schema"
                      @click="dialog = { kind: 'move', schema: s }"
                      >Move schema</GateButton
                    >
                    <GateButton
                      op="undistributeSchema"
                      small
                      variant="danger"
                      :blocked-reason="
                        s.distribution.state === 'moving' ? 'The schema is being moved.' : ''
                      "
                      data-testid="make-local"
                      @click="dialog = { kind: 'local', schema: s }"
                      >Make local</GateButton
                    >
                  </template>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </TableWrap>
    </QueryBoundary>
  </section>
  <SchemaDialogs
    v-if="dialog"
    :kind="dialog.kind"
    :database-name="databaseName"
    :schema="dialog.schema"
    @close="dialog = null"
  />
</template>
