<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { RouterLink } from 'vue-router'
import StatePanel from '@/components/StatePanel.vue'
import SafeName from '@/components/SafeName.vue'
import { useSessionStore } from '@/stores/session'

const session = useSessionStore()
</script>

<template>
  <StatePanel title="Not allowed">
    <div class="notice tone-warn" role="status" style="text-align: left">
      <span aria-hidden="true">&#9650;</span>
      <div class="stack" style="gap: var(--s-1)">
        <strong>Your account cannot open this page.</strong>
        <span>
          You are signed in as
          <code><SafeName :value="session.username ?? ''" kind="text" :max="60" /></code> with
          <code>{{ session.role ?? 'no cluster role' }}</code>. Ask an administrator to grant a
          cluster role that includes this page.
        </span>
        <RouterLink :to="{ name: 'dashboard' }">Go to the dashboard</RouterLink>
      </div>
    </div>
  </StatePanel>
</template>
