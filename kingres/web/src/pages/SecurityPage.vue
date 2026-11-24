<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Security: roles, grant matrix, network access rules and the audit log. Panel accounts are
// PostgreSQL roles; the cluster role decides what a role can do in the panel.
import { computed, ref } from 'vue'
import PageHeader from '@/components/PageHeader.vue'
import TabBar from '@/components/TabBar.vue'
import RolesPanel from '@/components/security/RolesPanel.vue'
import GrantsPanel from '@/components/security/GrantsPanel.vue'
import NetworkPanel from '@/components/security/NetworkPanel.vue'
import AuditPanel from '@/components/security/AuditPanel.vue'
import CaEscrowPanel from '@/components/security/CaEscrowPanel.vue'
import { useSessionStore } from '@/stores/session'

const session = useSessionStore()
const tab = ref('roles')
// The escrow of the certificate authority key is for administrators, like the API (contract 0.26.0).
const tabs = computed(() => [
  { id: 'roles', label: 'Roles' },
  { id: 'access', label: 'Access' },
  { id: 'network', label: 'Network access' },
  ...(session.can('getCaEscrow') ? [{ id: 'ca', label: 'Certificate authority' }] : []),
  { id: 'audit', label: 'Audit log' },
])
</script>

<template>
  <PageHeader
    title="Security"
    subtitle="Accounts, access to data, allowed networks and the audit trail."
  />
  <TabBar v-model="tab" :tabs="tabs" label="Security sections" />
  <div :id="`panel-${tab}`" role="tabpanel" :aria-labelledby="`tab-${tab}`">
    <RolesPanel v-if="tab === 'roles'" />
    <GrantsPanel v-else-if="tab === 'access'" />
    <NetworkPanel v-else-if="tab === 'network'" />
    <CaEscrowPanel v-else-if="tab === 'ca'" />
    <AuditPanel v-else />
  </div>
</template>
