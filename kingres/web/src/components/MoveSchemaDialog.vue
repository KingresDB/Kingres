<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Move a distributed schema to another shard group (contract 0.15.0). The target is chosen among the groups that may hold shards,
// the current one left out. The contract does not list the tables that lack a primary key (and have no full replica identity)
// before the request: the server refuses the move with 422 and names them. The dialog then shows what the server said and asks for
// the choice "Set REPLICA IDENTITY FULL on these tables", which has to be made before the move is sent again.
import { computed, reactive, ref, shallowRef } from 'vue'
import { safeText } from '@/lib/safe-text'
import SafeName from './SafeName.vue'
import TaskActionDialog from './TaskActionDialog.vue'
import FormField from './FormField.vue'
import QueryBoundary from './QueryBoundary.vue'
import ProblemNotice from './ProblemNotice.vue'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { keys, useSchemaDetail } from '@/api/queries'
import { useShardGroupChoices } from '@/composables/useShardGroupChoices'
import type { SchemaSummary } from '@/api/types'

const props = defineProps<{ databaseName: string; schema: SchemaSummary }>()
const emit = defineEmits<{ close: [] }>()

const current = props.schema.distribution?.shardGroupId ?? null
const detail = useSchemaDetail(props.databaseName, props.schema.name)
const groups = useShardGroupChoices(current)
const form = reactive({ target: '', setIdentity: false })
const submitted = ref(false)

/** The refusal that names the tables without a key (422 `unprocessable` of `moveSchema`). */
const keyless = shallowRef<ApiError | null>(null)
const isKeylessRefusal = (e: unknown): e is ApiError =>
  e instanceof ApiError &&
  e.code === 'unprocessable' &&
  /REPLICA IDENTITY/i.test(e.problem.detail ?? '')

/** Why the schema cannot move now (a ledger table pins it, or a move runs), as the problem notice of that code. */
const stuck = computed(() => {
  const d = detail.data.value
  if (!d) return null
  if (d.pinned)
    return new ApiError({
      type: 'about:blank',
      title: 'schema_pinned',
      status: 409,
      code: 'schema_pinned',
      detail: d.pinned.reason,
    })
  if (d.moveInProgress !== null)
    return new ApiError({
      type: 'about:blank',
      title: 'schema_move_in_progress',
      status: 409,
      code: 'schema_move_in_progress',
      detail: `Task ${d.moveInProgress} moves this schema.`,
    })
  return null
})

const targetError = computed(() =>
  submitted.value && form.target === '' ? 'Choose the group the schema moves to.' : undefined,
)
const reason = computed(() => {
  if (detail.isPending.value) return 'The schema is being read.'
  if (detail.error.value) return 'The schema could not be read, so it cannot be moved.'
  if (stuck.value) return 'The schema cannot move now. The reason is shown above.'
  if (keyless.value !== null && !form.setIdentity)
    return 'Choose "Set REPLICA IDENTITY FULL on these tables" to go on.'
  return ''
})

function validate(): boolean {
  submitted.value = true
  return form.target !== ''
}

async function start() {
  try {
    return await endpoints.moveSchema(props.databaseName, props.schema.name, {
      targetShardGroupId: Number(form.target),
      ...(form.setIdentity ? { setReplicaIdentityFull: true } : {}),
    })
  } catch (error) {
    if (isKeylessRefusal(error)) keyless.value = error
    throw error
  }
}
</script>

<template>
  <TaskActionDialog
    :title="`Move schema ${safeText(schema.name, 60)}`"
    confirm-label="Move schema"
    wide
    :start="start"
    :validate="validate"
    :confirm-disabled="reason !== ''"
    :confirm-disabled-reason="reason"
    :invalidate="[
      keys.schemas(databaseName),
      keys.schema(databaseName, schema.name),
      keys.databases,
      keys.database(databaseName),
      keys.colocation(databaseName),
      keys.rebalance,
    ]"
    @close="emit('close')"
  >
    <template #default="{ fieldErrors }">
      <p class="notice tone-info" data-testid="move-warning">
        <span aria-hidden="true">&#9432;</span>
        <span
          >Writers of this schema wait for a moment while the move switches over (normally well
          under a second). Other schemas are not affected.</span
        >
      </p>
      <QueryBoundary
        :loading="detail.isPending.value"
        :error="detail.error.value"
        @retry="detail.refetch()"
      >
        <ProblemNotice v-if="stuck" :error="stuck" />
        <template v-else>
          <FormField
            v-slot="slot"
            label="Move to server group"
            :hint="
              current === null
                ? undefined
                : `Now on group ${current}. Only groups whose server may hold shards are offered.`
            "
            :error="targetError ?? fieldErrors['/targetShardGroupId']"
            required
          >
            <select v-bind="slot" v-model="form.target" data-autofocus>
              <option value="" disabled>Choose a group</option>
              <option v-for="g in groups.choices.value" :key="g.id" :value="String(g.id)">
                {{ g.label }}
              </option>
            </select>
          </FormField>
          <div v-if="keyless" class="stack" data-testid="keyless-tables">
            <p class="notice tone-warn" role="alert">
              <span aria-hidden="true">&#9650;</span>
              <span class="stack" style="gap: var(--s-1)">
                <strong>Some tables have no primary key and no full replica identity.</strong>
                <span
                  ><SafeName :value="keyless.problem.detail ?? ''" kind="text" :max="600" multiline
                /></span>
              </span>
            </p>
            <label class="check">
              <input v-model="form.setIdentity" type="checkbox" />
              Set REPLICA IDENTITY FULL on these tables
            </label>
            <p class="hint muted">
              The move copies rows with logical replication, which needs a way to tell rows apart.
              This changes the tables of the schema; the new setting stays after the move.
            </p>
          </div>
        </template>
      </QueryBoundary>
    </template>
    <template #error="{ error }">
      <p v-if="error === keyless" class="muted" data-testid="keyless-refusal">
        The move was not started.
      </p>
      <ProblemNotice v-else :error="error" />
    </template>
  </TaskActionDialog>
</template>
