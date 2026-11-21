<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Distribute a local schema (contract 0.15.0). The dialog reads the schema first (`getSchema`) and lists the objects that keep it
// from being distributed (the blockers); while there are any, the button stays disabled and says why. The group to place an empty
// schema on is optional; a schema that has rows stays where its rows are.
import { computed, reactive } from 'vue'
import { safeText } from '@/lib/safe-text'
import SafeName from './SafeName.vue'
import TaskActionDialog from './TaskActionDialog.vue'
import FormField from './FormField.vue'
import QueryBoundary from './QueryBoundary.vue'
import TableWrap from './TableWrap.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useSchemaDetail } from '@/api/queries'
import { useShardGroupChoices } from '@/composables/useShardGroupChoices'
import type { DistributeSchemaRequest, SchemaSummary } from '@/api/types'

const props = defineProps<{ databaseName: string; schema: SchemaSummary }>()
const emit = defineEmits<{ close: [] }>()

const detail = useSchemaDetail(props.databaseName, props.schema.name)
const groups = useShardGroupChoices()
const blockers = computed(() => detail.data.value?.distributionBlockers ?? [])
const form = reactive({ group: '', setIdentity: false })

/** Why the button is disabled now, or an empty text. */
const reason = computed(() => {
  if (detail.isPending.value) return 'The schema is being read.'
  if (detail.error.value) return 'The schema could not be read, so it cannot be distributed.'
  if (blockers.value.length > 0)
    return `The schema has ${blockers.value.length} ${blockers.value.length === 1 ? 'object' : 'objects'} that distributed schemas do not support. Remove or change them first.`
  return ''
})

const start = () => {
  const body: DistributeSchemaRequest = {
    ...(form.group !== '' ? { shardGroupId: Number(form.group) } : {}),
    ...(form.setIdentity ? { setReplicaIdentityFull: true } : {}),
  }
  return endpoints.distributeSchema(props.databaseName, props.schema.name, body)
}
</script>

<template>
  <TaskActionDialog
    :title="`Distribute schema ${safeText(schema.name, 60)}`"
    confirm-label="Distribute schema"
    wide
    :start="start"
    :confirm-disabled="reason !== ''"
    :confirm-disabled-reason="reason"
    :invalidate="[
      keys.schemas(databaseName),
      keys.schema(databaseName, schema.name),
      keys.databases,
      keys.database(databaseName),
      keys.colocation(databaseName),
      keys.tables(databaseName),
    ]"
    @close="emit('close')"
  >
    <template #default="{ fieldErrors }">
      <p>
        The data stays where it is. Other servers receive empty tables. You can move the schema
        afterwards.
      </p>
      <QueryBoundary
        :loading="detail.isPending.value"
        :error="detail.error.value"
        @retry="detail.refetch()"
      >
        <div v-if="blockers.length" class="stack" data-testid="schema-blockers">
          <p class="notice tone-warn" role="alert">
            <span aria-hidden="true">&#9650;</span>
            <span
              >This schema cannot be distributed yet. {{ blockers.length }}
              {{ blockers.length === 1 ? 'object keeps' : 'objects keep' }} it from it. Remove or
              change them, then open this dialog again.</span
            >
          </p>
          <TableWrap>
            <table class="table">
              <caption class="sr-only">
                Objects that keep the schema from being distributed
              </caption>
              <thead>
                <tr>
                  <th scope="col">Kind</th>
                  <th scope="col">Object</th>
                  <th scope="col">Why</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="(b, i) in blockers" :key="`${i}-${b.objectName}`">
                  <td><SafeName :value="b.objectKind" kind="text" :max="40" /></td>
                  <th scope="row" class="mono"><SafeName :value="b.objectName" :max="120" /></th>
                  <td><SafeName :value="b.reason" kind="text" :max="300" multiline /></td>
                </tr>
              </tbody>
            </table>
          </TableWrap>
        </div>
        <template v-else>
          <FormField
            v-slot="slot"
            label="Place on server group"
            hint="Automatic puts an empty schema where the placement rule says; a schema that has rows stays where its rows are."
            :error="fieldErrors['/shardGroupId'] ?? fieldErrors.shardGroupId"
          >
            <select v-bind="slot" v-model="form.group" data-autofocus>
              <option value="">Automatic (in place)</option>
              <option v-for="g in groups.choices.value" :key="g.id" :value="String(g.id)">
                {{ g.label }}
              </option>
            </select>
          </FormField>
          <label class="check">
            <input v-model="form.setIdentity" type="checkbox" />
            Also set REPLICA IDENTITY FULL on the tables that have no primary key, so that the
            schema can be moved later
          </label>
        </template>
      </QueryBoundary>
    </template>
  </TaskActionDialog>
</template>
