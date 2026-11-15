<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The end of a server certificate. An expired or soon-to-expire certificate is flagged with an
// icon and words (never colour alone); an unknown value is shown as "not available".
import { computed } from 'vue'
import NotAvailable from './NotAvailable.vue'
import { certificateState, CERTIFICATE_WARN_DAYS } from '@/lib/fingerprint'
import { formatDateTime } from '@/lib/format'

const props = defineProps<{ expiresAt: string | null }>()
const state = computed(() => certificateState(props.expiresAt))
</script>

<template>
  <NotAvailable v-if="state === 'unknown'" />
  <span v-else>
    <!-- The date stays in one piece; the flag may wrap below it on a narrow screen (W2-16). -->
    <span class="nowrap">{{ formatDateTime(expiresAt) }}</span>
    <span v-if="state === 'expired'" class="badge flag tone-danger" data-testid="cert-flag"
      ><span aria-hidden="true">&#10006;</span> Certificate expired</span
    >
    <span v-else-if="state === 'expiring'" class="badge flag tone-warn" data-testid="cert-flag"
      ><span aria-hidden="true">&#9650;</span> Expires within {{ CERTIFICATE_WARN_DAYS }} days</span
    >
  </span>
</template>

<style scoped>
/* In a narrow column (the details grid at 320 px) the flag wraps inside itself instead of pushing the page. */
.flag {
  max-width: 100%;
  white-space: normal;
}
</style>
