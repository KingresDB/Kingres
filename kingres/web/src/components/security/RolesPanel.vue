<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import QueryBoundary from '../QueryBoundary.vue'
import GateButton from '../GateButton.vue'
import ConfirmDialog from '../ConfirmDialog.vue'
import ProblemNotice from '../ProblemNotice.vue'
import RoleDialog from './RoleDialog.vue'
import { endpoints } from '@/api/endpoints'
import { keys, useRoles } from '@/api/queries'
import { formatDateTime } from '@/lib/format'
import type { Role } from '@/api/types'

const client = useQueryClient()
const search = ref('')
const roles = useRoles(search)
const rows = computed(() => roles.data.value?.items ?? [])

const dialog = ref<{ role?: Role } | null>(null)
const dropping = ref<Role | null>(null)

const refresh = () => client.invalidateQueries({ queryKey: keys.roles })
const toggleLogin = useMutation({
  mutationFn: (r: Role) => endpoints.updateRole(r.name, { canLogin: !r.canLogin }),
  onSettled: refresh,
})
const drop = useMutation({
  mutationFn: (r: Role) => endpoints.deleteRole(r.name),
  onSuccess: () => {
    dropping.value = null
  },
  onSettled: refresh,
})
</script>

<template>
  <div class="stack">
    <div class="row-between">
      <div class="field">
        <label for="role-search">Search roles</label>
        <input id="role-search" v-model="search" class="input" type="search" />
      </div>
      <GateButton op="createRole" variant="primary" @click="dialog = {}">Create role</GateButton>
    </div>
    <ProblemNotice v-if="toggleLogin.error.value" :error="toggleLogin.error.value" />
    <QueryBoundary
      :loading="roles.isPending.value"
      :error="roles.error.value"
      :empty="rows.length === 0"
      empty-title="No roles found"
      @retry="roles.refetch()"
    >
      <div class="table-wrap">
        <table class="table">
          <caption class="sr-only">
            Roles
          </caption>
          <thead>
            <tr>
              <th scope="col">Role</th>
              <th scope="col">Sign-in</th>
              <th scope="col">Cluster roles</th>
              <th scope="col">Member of</th>
              <th scope="col">Attributes</th>
              <th scope="col">Password valid until</th>
              <th scope="col">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in rows" :key="r.name">
              <th scope="row" class="mono">{{ r.name }}</th>
              <td>
                <span class="badge" :class="r.canLogin ? 'tone-ok' : 'tone-neutral'">
                  <span aria-hidden="true">{{ r.canLogin ? '✓' : '○' }}</span>
                  {{ r.canLogin ? 'Can sign in' : 'Disabled' }}
                </span>
              </td>
              <td>{{ r.clusterRoles.join(', ') || '-' }}</td>
              <td>{{ r.memberOf.join(', ') || '-' }}</td>
              <td>
                <span v-if="r.isSuperuser">superuser </span>
                <span v-if="r.canCreateDatabase">createdb </span>
                <span v-if="r.canCreateRole">createrole </span>
                <span v-if="r.connectionLimit >= 0">limit {{ r.connectionLimit }}</span>
              </td>
              <td>{{ r.validUntil ? formatDateTime(r.validUntil) : 'No expiry' }}</td>
              <td>
                <div class="row">
                  <GateButton op="updateRole" small @click="dialog = { role: r }">Edit</GateButton>
                  <GateButton op="updateRole" small @click="toggleLogin.mutate(r)">{{
                    r.canLogin ? 'Disable' : 'Enable'
                  }}</GateButton>
                  <GateButton op="deleteRole" small variant="danger" @click="dropping = r"
                    >Drop</GateButton
                  >
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </QueryBoundary>
  </div>

  <RoleDialog v-if="dialog" :role="dialog.role" @close="dialog = null" />
  <ConfirmDialog
    v-if="dropping"
    :title="`Drop role ${dropping.name}?`"
    confirm-label="Drop role"
    danger
    :type-to-confirm="dropping.name"
    :busy="drop.isPending.value"
    @cancel="dropping = null"
    @confirm="drop.mutate(dropping)"
  >
    <p>
      The role and its grants are removed on every server. Sessions of this role end. This cannot be
      undone.
    </p>
    <ProblemNotice v-if="drop.error.value" :error="drop.error.value" />
  </ConfirmDialog>
</template>
