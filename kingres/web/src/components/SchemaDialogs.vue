<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The three dialogs of a distributed schema (contract 0.16.0, design shard-phase11.md, section 14.2): distribute a local
// schema (the blockers are listed and the button waits while there are any), move one to another shard group (with the
// tables that need REPLICA IDENTITY FULL), and make one local (type the name to confirm). Names can be hostile: they are
// shown through SafeName and the confirmation types the real name.
import { computed, reactive, ref } from 'vue'
import SafeName from './SafeName.vue'
import TaskActionDialog from './TaskActionDialog.vue'
import QueryBoundary from './QueryBoundary.vue'
import FormField from './FormField.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useSchemaDetail, useTopology } from '@/api/queries'
import type { SchemaSummary } from '@/api/types'

const props = defineProps<{
  kind: 'distribute' | 'move' | 'local'
  databaseName: string
  schema: SchemaSummary
}>()
const emit = defineEmits<{ close: [] }>()

const detail = useSchemaDetail(
  () => props.databaseName,
  () => props.schema.name,
)
const d = computed(() => detail.data.value)
const topology = useTopology()
/** The shard groups that can receive the schema: not the map group, and not the one it is on. */
const groups = computed(
  () =>
    topology.data.value?.groups.filter(
      (g) => !g.isMapGroup && g.id !== props.schema.distribution?.shardGroupId,
    ) ?? [],
)
const form = reactive({ target: '' as string, identityFull: false, moveFirst: false })
const submitted = ref(false)

const blockers = computed(() => d.value?.distributionBlockers ?? [])
const keyless = computed(() => d.value?.tablesWithoutReplicaIdentity ?? [])
const pinned = computed(() => d.value?.pinned ?? null)

const confirmDisabled = computed(() => {
  if (!d.value) return true
  if (props.kind === 'distribute') return blockers.value.length > 0
  if (props.kind === 'move')
    return (
      pinned.value !== null ||
      form.target === '' ||
      (keyless.value.length > 0 && !form.identityFull)
    )
  return pinned.value !== null
})
const why = computed(() => {
  if (props.kind === 'distribute' && blockers.value.length > 0)
    return 'Remove or replace the objects above first.'
  if (props.kind !== 'distribute' && pinned.value) return 'The schema is pinned.'
  if (props.kind === 'move' && form.target === '') return 'Choose the target shard group.'
  if (props.kind === 'move' && keyless.value.length > 0 && !form.identityFull)
    return 'Allow REPLICA IDENTITY FULL on the tables listed above.'
  return ''
})
const validate = (): boolean => {
  submitted.value = true
  return !confirmDisabled.value
}

const start = () => {
  if (props.kind === 'distribute')
    return endpoints.distributeSchema(props.databaseName, props.schema.name, {
      ...(form.target !== '' ? { shardGroupId: Number(form.target) } : {}),
      ...(form.identityFull ? { setReplicaIdentityFull: true } : {}),
    })
  if (props.kind === 'move')
    return endpoints.moveSchema(props.databaseName, props.schema.name, {
      targetShardGroupId: Number(form.target),
      ...(form.identityFull ? { setReplicaIdentityFull: true } : {}),
    })
  return endpoints.undistributeSchema(
    props.databaseName,
    props.schema.name,
    form.moveFirst && form.target !== '' ? Number(form.target) : undefined,
  )
}
const TITLE = {
  distribute: 'Distribute schema',
  move: 'Move schema',
  local: 'Make schema local',
} as const
const CONFIRM = {
  distribute: 'Distribute schema',
  move: 'Move schema',
  local: 'Make schema local',
} as const
</script>

<template>
  <TaskActionDialog
    :title="TITLE[kind]"
    :confirm-label="CONFIRM[kind]"
    :danger="kind === 'local'"
    :type-to-confirm="kind === 'local' ? schema.name : undefined"
    wide
    :start="start"
    :validate="validate"
    :confirm-disabled="confirmDisabled"
    :invalidate="[
      keys.schemas(databaseName),
      keys.schema(databaseName, schema.name),
      keys.databases,
      keys.tasks,
    ]"
    @close="emit('close')"
  >
    <p>
      Schema <strong><SafeName :value="schema.name" :max="80" /></strong>
    </p>
    <QueryBoundary
      :loading="detail.isPending.value"
      :error="detail.error.value"
      @retry="detail.refetch()"
    >
      <div v-if="d" class="stack">
        <template v-if="kind === 'distribute'">
          <p>
            The data stays where it is. Other servers receive empty tables. You can move the schema
            afterwards.
          </p>
          <div v-if="blockers.length" data-testid="schema-blockers">
            <p class="notice tone-warn">
              <span aria-hidden="true">&#9650;</span>
              <span>This schema cannot be distributed yet:</span>
            </p>
            <table class="table">
              <caption class="sr-only">
                Objects that are not supported in a distributed schema
              </caption>
              <thead>
                <tr>
                  <th scope="col">Object</th>
                  <th scope="col">Kind</th>
                  <th scope="col">Why</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="b in blockers" :key="b.objectName + b.objectKind">
                  <td><SafeName :value="b.objectName" :max="100" /></td>
                  <td><SafeName :value="b.objectKind" kind="text" :max="60" /></td>
                  <td><SafeName :value="b.reason" kind="text" :max="200" /></td>
                </tr>
              </tbody>
            </table>
          </div>
          <template v-else>
            <FormField
              v-slot="slot"
              label="Place on server group"
              hint="Automatic keeps the schema where it is and puts empty tables on the other servers; a schema that has rows stays where its rows are."
            >
              <select v-bind="slot" v-model="form.target" data-autofocus>
                <option value="">Automatic (in place)</option>
                <option v-for="g in groups" :key="g.id" :value="String(g.id)">
                  Shard group {{ g.id }}
                </option>
              </select>
            </FormField>
            <label class="check"
              ><input v-model="form.identityFull" type="checkbox" /> Also set REPLICA IDENTITY FULL
              on the tables that have no primary key, so that the schema can be moved later</label
            >
          </template>
        </template>

        <template v-else>
          <p v-if="pinned" class="notice tone-warn" role="status" data-testid="schema-pinned">
            <span aria-hidden="true">&#9650;</span>
            <span
              ><strong>Pinned.</strong> <SafeName :value="pinned.reason" kind="text" :max="300"
            /></span>
          </p>
          <template v-if="kind === 'move'">
            <p>
              Writers of this schema wait for a moment while the move switches over (normally well
              under a second). Other schemas are not affected.
            </p>
            <FormField
              v-slot="slot"
              label="Target shard group"
              required
              :error="submitted && form.target === '' ? 'Choose a shard group.' : undefined"
            >
              <select v-bind="slot" v-model="form.target" data-autofocus>
                <option value="" disabled>Choose a group</option>
                <option v-for="g in groups" :key="g.id" :value="String(g.id)">
                  Shard group {{ g.id }}
                </option>
              </select>
            </FormField>
            <div v-if="keyless.length" data-testid="schema-keyless">
              <p class="notice tone-warn">
                <span aria-hidden="true">&#9650;</span>
                <span
                  >These tables have no primary key and no replica identity, so a move cannot copy
                  them:</span
                >
              </p>
              <ul>
                <li v-for="t in keyless" :key="t"><SafeName :value="t" :max="80" /></li>
              </ul>
              <label class="check"
                ><input v-model="form.identityFull" type="checkbox" /> Set REPLICA IDENTITY FULL on
                these tables</label
              >
            </div>
          </template>
          <template v-else>
            <p>
              The schema stays on server group
              <strong>{{ schema.distribution?.shardGroupId }}</strong> as an ordinary schema;
              clients connected to other servers will no longer see it.
            </p>
            <label class="check"
              ><input v-model="form.moveFirst" type="checkbox" /> Move it to another group
              first</label
            >
            <FormField v-if="form.moveFirst" v-slot="slot" label="Move to group">
              <select v-bind="slot" v-model="form.target">
                <option value="" disabled>Choose a group</option>
                <option v-for="g in groups" :key="g.id" :value="String(g.id)">
                  Shard group {{ g.id }}
                </option>
              </select>
            </FormField>
          </template>
        </template>
        <p v-if="why" class="hint" data-testid="schema-why">{{ why }}</p>
      </div>
    </QueryBoundary>
    <template #error="{ error }">
      <slot name="error" :error="error" />
    </template>
  </TaskActionDialog>
</template>
