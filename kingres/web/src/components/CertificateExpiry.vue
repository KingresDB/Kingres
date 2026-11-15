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
  <span v-else class="nowrap">
    {{ formatDateTime(expiresAt) }}
    <span v-if="state === 'expired'" class="badge tone-danger" data-testid="cert-flag"
      ><span aria-hidden="true">&#10006;</span> Certificate expired</span
    >
    <span v-else-if="state === 'expiring'" class="badge tone-warn" data-testid="cert-flag"
      ><span aria-hidden="true">&#9650;</span> Expires within {{ CERTIFICATE_WARN_DAYS }} days</span
    >
  </span>
</template>
