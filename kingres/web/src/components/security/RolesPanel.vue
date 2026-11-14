<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { computed, ref } from 'vue'
import TableWrap from '../TableWrap.vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import QueryBoundary from '../QueryBoundary.vue'
import GateButton from '../GateButton.vue'
import ConfirmDialog from '../ConfirmDialog.vue'
import ProblemNotice from '../ProblemNotice.vue'
import SafeName from '../SafeName.vue'
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
const disabling = ref<Role | null>(null)

const refresh = () => client.invalidateQueries({ queryKey: keys.roles })
const toggleLogin = useMutation({
  mutationFn: (r: Role) => endpoints.updateRole(r.name, { canLogin: !r.canLogin }),
  onSuccess: () => {
    disabling.value = null
  },
  onSettled: refresh,
})
/** Enabling is harmless and immediate; disabling ends the role's sign-in, so it is confirmed. */
function toggle(r: Role): void {
  if (r.canLogin) disabling.value = r
  else toggleLogin.mutate(r)
}
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
      <TableWrap>
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
              <th scope="row" class="mono"><SafeName :value="r.name" /></th>
              <td>
                <span class="badge" :class="r.canLogin ? 'tone-ok' : 'tone-neutral'">
                  <span aria-hidden="true">{{ r.canLogin ? '✓' : '○' }}</span>
                  {{ r.canLogin ? 'Can sign in' : 'Disabled' }}
                </span>
              </td>
              <td>{{ r.clusterRoles.join(', ') || '-' }}</td>
              <td>
                <template v-if="r.memberOf.length"
                  ><template v-for="(m, i) in r.memberOf" :key="i"
                    ><template v-if="i > 0">, </template><SafeName :value="m" :max="60" /></template
                ></template>
                <template v-else>-</template>
              </td>
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
                  <GateButton op="updateRole" small @click="toggle(r)">{{
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
      </TableWrap>
      <p v-if="roles.data.value?.moreNotShown" class="notice tone-warn" role="status">
        <span aria-hidden="true">&#9650;</span>
        <span>More roles exist than are shown here. Use the search to find one.</span>
      </p>
    </QueryBoundary>
  </div>

  <RoleDialog v-if="dialog" :role="dialog.role" @close="dialog = null" />
  <ConfirmDialog
    v-if="disabling"
    :title="`Disable role ${disabling.name}?`"
    confirm-label="Disable role"
    danger
    :type-to-confirm="disabling.name"
    :busy="toggleLogin.isPending.value"
    @cancel="disabling = null"
    @confirm="toggleLogin.mutate(disabling)"
  >
    <p>
      The role can no longer sign in, here or to the databases, on any server. You can enable it
      again later; its grants and memberships stay.
    </p>
    <ProblemNotice v-if="toggleLogin.error.value" :error="toggleLogin.error.value" />
  </ConfirmDialog>
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
