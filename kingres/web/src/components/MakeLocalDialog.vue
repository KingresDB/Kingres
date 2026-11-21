<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Make a distributed schema local again (contract 0.15.0): its tables stay where they are, the copies on the other servers go.
// The name of the schema is typed to confirm. A group can be named so that the local schema ends up there ("Move to group G
// first"); without it the schema becomes local on the group that owns it.
import { computed, reactive } from 'vue'
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
const form = reactive({ first: '' })

/** A schema that holds a ledger table cannot become local; shown as the problem notice of that code. */
const pinned = computed(() => {
  const reasonText = detail.data.value?.pinned?.reason
  return reasonText === undefined
    ? null
    : new ApiError({
        type: 'about:blank',
        title: 'schema_pinned',
        status: 409,
        code: 'schema_pinned',
        detail: reasonText,
      })
})
const moving = computed(() => detail.data.value?.moveInProgress ?? null)

/** Where the schema is when it is local: the group named in "Move to group first", else the group that owns it now. */
const endsOn = computed(() => (form.first !== '' ? Number(form.first) : current))
const endsOnServer = computed(() => {
  const g = groups.choices.value.find((x) => x.id === endsOn.value)
  return g?.serverId ?? (form.first === '' ? (props.schema.distribution?.serverId ?? null) : null)
})

const reason = computed(() => {
  if (detail.isPending.value) return 'The schema is being read.'
  if (detail.error.value) return 'The schema could not be read, so it cannot be made local.'
  if (pinned.value) return 'The schema is pinned. The reason is shown above.'
  if (moving.value !== null) return 'The schema is moving. Wait until the move has finished.'
  return ''
})

const start = () =>
  endpoints.undistributeSchema(
    props.databaseName,
    props.schema.name,
    form.first !== '' ? Number(form.first) : undefined,
  )
</script>

<template>
  <TaskActionDialog
    :title="`Make schema ${safeText(schema.name, 60)} local`"
    confirm-label="Make schema local"
    danger
    wide
    :type-to-confirm="schema.name"
    :start="start"
    :confirm-disabled="reason !== ''"
    :confirm-disabled-reason="reason"
    :invalidate="[
      keys.schemas(databaseName),
      keys.schema(databaseName, schema.name),
      keys.databases,
      keys.database(databaseName),
      keys.colocation(databaseName),
    ]"
    @close="emit('close')"
  >
    <template #default="{ fieldErrors }">
      <p class="notice tone-warn" data-testid="make-local-sentence">
        <span aria-hidden="true">&#9650;</span>
        <span>
          <strong><SafeName :value="schema.name" :max="80" /></strong> becomes a local schema on
          server group {{ endsOn ?? '?'
          }}<template v-if="endsOnServer">
            (server <SafeName :value="endsOnServer" :max="80" />)</template
          >. Its tables stay where they are, and the copies on the other servers are removed.
          Clients connected to other servers will no longer see it.
        </span>
      </p>
      <QueryBoundary
        :loading="detail.isPending.value"
        :error="detail.error.value"
        @retry="detail.refetch()"
      >
        <ProblemNotice v-if="pinned" :error="pinned" />
        <p v-else-if="moving !== null" class="notice tone-warn" role="alert">
          <span aria-hidden="true">&#9650;</span>
          <span>This schema is moving (task {{ moving }}). Wait until the move has finished.</span>
        </p>
        <FormField
          v-else
          v-slot="slot"
          label="Move to group first (optional)"
          hint="Without a group the schema becomes local on the group that owns it now."
          :error="fieldErrors.shardGroupId"
        >
          <select v-bind="slot" v-model="form.first">
            <option value="">Stay on group {{ current }}</option>
            <option v-for="g in groups.choices.value" :key="g.id" :value="String(g.id)">
              {{ g.label }}
            </option>
          </select>
        </FormField>
      </QueryBoundary>
    </template>
  </TaskActionDialog>
</template>
