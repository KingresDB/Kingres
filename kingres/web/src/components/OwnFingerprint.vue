<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The fingerprint of THIS server's own internal certificate, for an administrator who has to give it to
// someone else (the "Add server" dialog of another server pins it). Shown in groups of eight digits so
// that two values can be compared by eye; the copy button puts the plain 64 digits on the clipboard.
// When the server does not report it (null or absent) the fallback text says where else to read it.
import { ref } from 'vue'
import SafeName from './SafeName.vue'
import { groupFingerprint, normalizeFingerprint } from '@/lib/fingerprint'

defineProps<{
  fingerprint: string | null | undefined
  /** Plain words that say what the fingerprint is for, shown above it. */
  purpose: string
  /** What to do when the server does not report it. */
  fallback: string
}>()

const copied = ref<'' | 'yes' | 'no'>('')

async function copy(hex: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(normalizeFingerprint(hex))
    copied.value = 'yes'
  } catch {
    copied.value = 'no'
  }
}
</script>

<template>
  <div class="stack" style="gap: var(--s-1)" data-testid="own-fingerprint">
    <p>{{ purpose }}</p>
    <template v-if="fingerprint">
      <code class="mono wrap" data-testid="own-fingerprint-value"
        ><SafeName :value="groupFingerprint(fingerprint)" kind="text" :max="100"
      /></code>
      <div class="row">
        <button type="button" class="btn btn-small" @click="copy(fingerprint)">
          Copy fingerprint
        </button>
        <span v-if="copied === 'yes'" role="status">Copied.</span>
        <span v-else-if="copied === 'no'" role="status"
          >The browser did not allow copying. Select the digits above and copy them.</span
        >
      </div>
    </template>
    <p v-else class="muted" data-testid="own-fingerprint-fallback">{{ fallback }}</p>
  </div>
</template>
