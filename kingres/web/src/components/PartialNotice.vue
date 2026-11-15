<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// "This list is incomplete": a cluster-wide answer that some servers did not give. Calm (information,
// not an error), names every missing server and why in plain words, and is drawn only while the latest
// answer is partial. It is not a live region itself: `useCompletenessAnnouncement` tells assistive
// technology once, so a poll that repeats the same answer does not repeat the announcement.
// Server ids are untrusted text and go through SafeName.
import SafeName from './SafeName.vue'
import { reasonWords, type Completeness } from '@/lib/partial'

withDefaults(
  defineProps<{
    completeness: Completeness
    /** What is missing from the view, for example "Alerts held by these servers are not shown." */
    consequence?: string
  }>(),
  { consequence: 'What these servers know is not shown.' },
)
</script>

<template>
  <div v-if="completeness.partial" class="notice tone-info" data-testid="partial-notice">
    <span aria-hidden="true">&#9432;</span>
    <div class="stack" style="gap: var(--s-1)">
      <strong>This list is incomplete</strong>
      <span>
        {{
          completeness.missing.length > 0
            ? 'Some servers did not give their part of the answer.'
            : 'Some servers did not give their part of the answer, and the answer does not say which.'
        }}
        {{ consequence }}
      </span>
      <ul v-if="completeness.missing.length > 0" class="partial-list">
        <li v-for="m in completeness.missing" :key="`${m.serverId}-${m.reason}`">
          Server <SafeName :value="m.serverId" kind="text" :max="60" /> {{ reasonWords(m.reason) }}.
        </li>
      </ul>
    </div>
  </div>
</template>

<style scoped>
.partial-list {
  margin: 0;
  padding-left: 1.2em;
}
</style>
