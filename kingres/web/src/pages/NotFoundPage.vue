<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { RouterLink } from 'vue-router'
import StatePanel from '@/components/StatePanel.vue'
</script>

<template>
  <StatePanel title="Page not found">
    <p>
      This page does not exist.
      <RouterLink :to="{ name: 'dashboard' }">Go to the dashboard</RouterLink>.
    </p>
  </StatePanel>
</template>
