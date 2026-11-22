<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The lines of the report of the sharding extension (contract 0.14.0, `TableBlocker`): what the line is about, in words by its
// code when the panel knows it, and the extension's own text. A blocker is a refusal, a warning and a note are information.
import SafeName from '../SafeName.vue'
import { tableBlockerLabel } from '@/lib/tables'
import type { TableBlocker } from '@/api/types'

defineProps<{
  lines: TableBlocker[]
  tone: 'blocker' | 'warning' | 'note'
  heading: string
}>()
const ICON = { blocker: '✖', warning: '▲', note: 'ⓘ' } as const
const TONE = { blocker: 'tone-danger', warning: 'tone-warn', note: 'tone-info' } as const
</script>

<template>
  <div
    v-if="lines.length > 0"
    class="notice"
    :class="TONE[tone]"
    :role="tone === 'blocker' ? 'alert' : 'status'"
    :data-testid="`table-${tone}s`"
  >
    <span aria-hidden="true">{{ ICON[tone] }}</span>
    <div class="stack" style="gap: var(--s-1)">
      <strong>{{ heading }}</strong>
      <ul class="lines">
        <li v-for="(b, i) in lines" :key="`${i}-${b.code}-${b.object}`">
          <strong>{{ tableBlockerLabel(b) }}.</strong>{{ ' ' }}
          <code><SafeName :value="b.object" kind="name" :max="120" /></code>:{{ ' ' }}
          <SafeName :value="b.message" kind="text" :max="400" />
        </li>
      </ul>
    </div>
  </div>
</template>

<style scoped>
.lines {
  margin: 0;
  padding-left: var(--s-4);
}
</style>
