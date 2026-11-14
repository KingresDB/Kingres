<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Create or edit a role: attributes, password, membership of other roles and cluster roles.
// Membership and cluster roles are separate replace-style sub-resources in the API.
import { computed, reactive, ref } from 'vue'
import { useQueryClient } from '@tanstack/vue-query'
import AppDialog from '../AppDialog.vue'
import FormField from '../FormField.vue'
import ProblemNotice from '../ProblemNotice.vue'
import { endpoints } from '@/api/endpoints'
import { describeProblem } from '@/api/problem'
import { keys, useClusterRoles, useRoles } from '@/api/queries'
import type { ClusterRoleName, Role, RoleUpdateRequest } from '@/api/types'
import { compact, firstError, identifier, minLength } from '@/lib/validators'

const props = defineProps<{ role?: Role }>()
const emit = defineEmits<{ close: [] }>()

const client = useQueryClient()
const roles = useRoles()
const clusterRoleInfo = useClusterRoles()
const editing = computed(() => props.role !== undefined)
const r = props.role

const form = reactive({
  name: r?.name ?? '',
  password: '',
  canLogin: r?.canLogin ?? true,
  canCreateDatabase: r?.canCreateDatabase ?? false,
  canCreateRole: r?.canCreateRole ?? false,
  connectionLimit: r?.connectionLimit ?? -1,
  validUntil: r?.validUntil ? r.validUntil.slice(0, 16) : '',
  memberOf: [...(r?.memberOf ?? [])],
  clusterRoles: [...(r?.clusterRoles ?? [])] as ClusterRoleName[],
})
const submitted = ref(false)
const busy = ref(false)
const error = ref<unknown>(null)

const errors = computed(() =>
  compact({
    name: editing.value ? undefined : firstError(form.name, identifier('The name')),
    password:
      form.password === '' && editing.value
        ? undefined
        : form.password === '' && !form.canLogin
          ? undefined
          : firstError(form.password, minLength('The password', 8)),
  }),
)
const serverErrors = computed(() => (error.value ? describeProblem(error.value).fieldErrors : {}))
const err = (f: string) => (submitted.value ? errors.value[f] : undefined) ?? serverErrors.value[f]

const sameSet = (a: string[], b: string[]) => a.length === b.length && a.every((x) => b.includes(x))
const otherRoles = computed(() => roles.data.value?.items.filter((x) => x.name !== form.name) ?? [])

async function save(): Promise<void> {
  submitted.value = true
  if (Object.keys(errors.value).length) return
  busy.value = true
  error.value = null
  const validUntil = form.validUntil ? new Date(form.validUntil).toISOString() : null
  try {
    if (r) {
      const update: RoleUpdateRequest = {}
      if (form.password) update.password = form.password
      if (form.canLogin !== r.canLogin) update.canLogin = form.canLogin
      if (form.canCreateDatabase !== r.canCreateDatabase)
        update.canCreateDatabase = form.canCreateDatabase
      if (form.canCreateRole !== r.canCreateRole) update.canCreateRole = form.canCreateRole
      if (form.connectionLimit !== r.connectionLimit) update.connectionLimit = form.connectionLimit
      if (validUntil !== r.validUntil) update.validUntil = validUntil
      if (Object.keys(update).length) await endpoints.updateRole(r.name, update)
      if (!sameSet(form.memberOf, r.memberOf))
        await endpoints.setRoleMemberships(r.name, form.memberOf)
      if (!sameSet(form.clusterRoles, r.clusterRoles))
        await endpoints.setRoleClusterRoles(r.name, form.clusterRoles)
    } else {
      await endpoints.createRole({
        name: form.name,
        ...(form.password ? { password: form.password } : {}),
        canLogin: form.canLogin,
        canCreateDatabase: form.canCreateDatabase,
        canCreateRole: form.canCreateRole,
        connectionLimit: form.connectionLimit,
        validUntil,
        memberOf: form.memberOf,
        clusterRoles: form.clusterRoles,
      })
    }
    await client.invalidateQueries({ queryKey: keys.roles })
    emit('close')
  } catch (e) {
    error.value = e
    await client.invalidateQueries({ queryKey: keys.roles })
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <AppDialog
    :title="editing ? `Edit role ${role?.name}` : 'Create role'"
    wide
    :persistent="busy"
    @close="emit('close')"
  >
    <form id="role-form" class="stack" novalidate @submit.prevent="save">
      <ProblemNotice v-if="error" :error="error" />
      <fieldset>
        <legend>Account</legend>
        <FormField
          v-if="!editing"
          v-slot="slot"
          label="Name"
          :error="err('name')"
          hint="Lower-case letters, digits and underscore."
          required
        >
          <input
            v-bind="slot"
            v-model="form.name"
            class="input mono"
            autocomplete="off"
            data-autofocus
          />
        </FormField>
        <FormField
          v-slot="slot"
          :label="editing ? 'New password (leave empty to keep)' : 'Password'"
          :error="err('password')"
          hint="At least 8 characters. Never shown again."
        >
          <input
            v-bind="slot"
            v-model="form.password"
            class="input"
            type="password"
            autocomplete="new-password"
          />
        </FormField>
        <label class="check"><input v-model="form.canLogin" type="checkbox" /> Can sign in</label>
        <label class="check"
          ><input v-model="form.canCreateDatabase" type="checkbox" /> Can create databases</label
        >
        <label class="check"
          ><input v-model="form.canCreateRole" type="checkbox" /> Can create roles</label
        >
        <FormField v-slot="slot" label="Connection limit" hint="-1 means unlimited.">
          <input
            v-bind="slot"
            v-model.number="form.connectionLimit"
            class="input num"
            type="number"
            min="-1"
          />
        </FormField>
        <FormField v-slot="slot" label="Password valid until (optional)">
          <input v-bind="slot" v-model="form.validUntil" class="input" type="datetime-local" />
        </FormField>
      </fieldset>
      <fieldset>
        <legend>Cluster roles</legend>
        <label v-for="c in clusterRoleInfo.data.value?.items" :key="c.name" class="check">
          <input v-model="form.clusterRoles" type="checkbox" :value="c.name" />
          <span
            ><code>{{ c.name }}</code> <span class="muted">{{ c.description }}</span></span
          >
        </label>
        <p v-if="clusterRoleInfo.isPending.value" class="muted">Loading cluster roles...</p>
      </fieldset>
      <fieldset>
        <legend>Member of</legend>
        <p v-if="otherRoles.length === 0" class="muted">No other roles.</p>
        <label v-for="o in otherRoles" :key="o.name" class="check">
          <input v-model="form.memberOf" type="checkbox" :value="o.name" /> {{ o.name }}
        </label>
      </fieldset>
    </form>
    <template #footer>
      <button type="button" class="btn" :disabled="busy" @click="emit('close')">Cancel</button>
      <button type="submit" form="role-form" class="btn btn-primary" :disabled="busy">
        {{ editing ? 'Save role' : 'Create role' }}
      </button>
    </template>
  </AppDialog>
</template>
