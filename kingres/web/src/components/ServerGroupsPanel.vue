<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Server groups (racks or zones): list, create and delete. A server is assigned to a group on its
// own page. Replicas of one shard group never share a server group, so groups matter for placement.
import { computed, ref } from 'vue'
import SafeName from './SafeName.vue'
import TableWrap from './TableWrap.vue'
import { RouterLink } from 'vue-router'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import QueryBoundary from './QueryBoundary.vue'
import GateButton from './GateButton.vue'
import ConfirmDialog from './ConfirmDialog.vue'
import ProblemNotice from './ProblemNotice.vue'
import AppDialog from './AppDialog.vue'
import FormField from './FormField.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import { keys, useServerGroups } from '@/api/queries'
import type { ServerGroup } from '@/api/types'

const GROUP_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$/

const client = useQueryClient()
const groups = useServerGroups()
const rows = computed(() => groups.data.value?.items ?? [])

const creating = ref(false)
const name = ref('')
const description = ref('')
const submitted = ref(false)
const deleting = ref<ServerGroup | null>(null)

const nameError = computed(() =>
  GROUP_NAME_RE.test(name.value)
    ? undefined
    : 'Use letters, digits, ".", "_" and "-", starting with a letter or digit (up to 63 characters).',
)
const refresh = () => client.invalidateQueries({ queryKey: keys.serverGroups })

const create = useMutation({
  mutationFn: () =>
    endpoints.createServerGroup({
      name: name.value,
      ...(description.value ? { description: description.value } : {}),
    }),
  onSuccess: () => {
    creating.value = false
    name.value = ''
    description.value = ''
    submitted.value = false
  },
  onSettled: refresh,
})
const serverNameError = computed(() =>
  create.error.value ? describeProblem(create.error.value).fieldErrors.name : undefined,
)
const remove = useMutation({
  mutationFn: (g: ServerGroup) => endpoints.deleteServerGroup(g.name),
  onSuccess: () => {
    deleting.value = null
  },
  onSettled: refresh,
})

function submit(): void {
  submitted.value = true
  if (!nameError.value) create.mutate()
}
function openCreate(): void {
  create.reset()
  creating.value = true
}
</script>

<template>
  <section class="card stack" aria-labelledby="sg-h">
    <div class="row-between">
      <h2 id="sg-h">Server groups</h2>
      <GateButton op="createServerGroup" @click="openCreate">Create server group</GateButton>
    </div>
    <p class="muted">
      A server group is a rack or zone. The primary and the standbys of one shard group are never
      placed in the same server group.
    </p>
    <QueryBoundary
      :loading="groups.isPending.value"
      :error="groups.error.value"
      :empty="rows.length === 0"
      empty-title="No server groups"
      empty-hint="Create a group, then assign servers to it on their detail page."
      @retry="groups.refetch()"
    >
      <TableWrap>
        <table class="table">
          <caption class="sr-only">
            Server groups
          </caption>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Description</th>
              <th scope="col">Servers</th>
              <th scope="col">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="g in rows" :key="g.name">
              <th scope="row"><SafeName :value="g.name" :max="80" /></th>
              <td><SafeName :value="g.description || ''" kind="text" :max="200" /></td>
              <td>
                <template v-if="g.serverIds.length">
                  <span v-for="(id, i) in g.serverIds" :key="id">
                    <RouterLink :to="{ name: 'server', params: { id } }">{{
                      id
                    }}</RouterLink><template v-if="i < g.serverIds.length - 1">, </template>
                  </span>
                </template>
                <span v-else class="muted">None</span>
              </td>
              <td>
                <GateButton op="deleteServerGroup" small variant="danger" @click="deleting = g">
                  Delete
                </GateButton>
              </td>
            </tr>
          </tbody>
        </table>
      </TableWrap>
    </QueryBoundary>
  </section>

  <AppDialog
    v-if="creating"
    title="Create server group"
    :persistent="create.isPending.value"
    @close="creating = false"
  >
    <form id="sg-form" class="stack" novalidate @submit.prevent="submit">
      <ProblemNotice v-if="create.error.value" :error="create.error.value" />
      <FormField
        v-slot="slot"
        label="Name"
        :error="(submitted ? nameError : undefined) ?? serverNameError"
        required
      >
        <input v-bind="slot" v-model="name" class="input" autocomplete="off" data-autofocus />
      </FormField>
      <FormField v-slot="slot" label="Description (optional)" hint="Up to 200 characters.">
        <input v-bind="slot" v-model="description" class="input" maxlength="200" />
      </FormField>
    </form>
    <template #footer>
      <button
        type="button"
        class="btn"
        :disabled="create.isPending.value"
        @click="creating = false"
      >
        Cancel
      </button>
      <button
        type="submit"
        form="sg-form"
        class="btn btn-primary"
        :disabled="create.isPending.value"
      >
        Create server group
      </button>
    </template>
  </AppDialog>

  <ConfirmDialog
    v-if="deleting"
    :title="`Delete server group ${deleting.name}?`"
    confirm-label="Delete group"
    danger
    :busy="remove.isPending.value"
    @cancel="deleting = null"
    @confirm="remove.mutate(deleting)"
  >
    <p>
      The group is removed. A group that still has servers cannot be deleted: move each of them to
      another group, or to no group, on its page first.
    </p>
    <ProblemNotice v-if="remove.error.value" :error="remove.error.value" />
  </ConfirmDialog>
</template>
