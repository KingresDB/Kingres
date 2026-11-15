<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// What the cluster recorded about the first contact with a server: the fingerprint of the
// certificate it presented and whether the administrator pinned it beforehand.
import SafeName from './SafeName.vue'
import { groupFingerprint } from '@/lib/fingerprint'

defineProps<{ fingerprint: string | null; pinned: boolean }>()
</script>

<template>
  <div v-if="fingerprint" class="stack" style="gap: var(--s-1)">
    <code class="mono wrap" data-testid="fingerprint"
      ><SafeName :value="groupFingerprint(fingerprint)" kind="text" :max="100"
    /></code>
    <span v-if="pinned" data-testid="fingerprint-state"
      ><span aria-hidden="true">&#10003;</span> Pinned: checked against the fingerprint you gave
      before the server was trusted.</span
    >
    <span v-else class="muted" data-testid="fingerprint-state"
      ><span aria-hidden="true">&#9650;</span> Seen on first contact, not pinned: nothing proved
      that this was the server you meant. Compare it with the fingerprint in that server's
      log.</span
    >
  </div>
  <span v-else class="muted" data-testid="fingerprint-state"
    >No first contact recorded. This is the case for the server that created the cluster and for a
    server that joined by its own request.</span
  >
</template>
