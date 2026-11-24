<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// What a plan of a time-partitioning policy would drop: the partitions, by name and range, which is what the server always says.
// The size is shown only when the server sent one for every dropped partition (a plan of the real server carries no sizes);
// otherwise the notice says that the size is not known. A size is never made up and never shown as 0.
import { computed } from 'vue'
import SafeName from '../SafeName.vue'
import { formatBytes, formatDateTime } from '@/lib/format'
import type { PartitionAction } from '@/lib/analytics'

const props = defineProps<{ actions: PartitionAction[]; testid: string; tail: string }>()
const drops = computed(() => props.actions.filter((a) => a.kind === 'drop'))
/** The sum of the sizes, or null when the server did not send the size of every dropped partition. */
const bytes = computed(() =>
  drops.value.length > 0 && drops.value.every((a) => typeof a.bytesBefore === 'number')
    ? drops.value.reduce((n, a) => n + (a.bytesBefore as number), 0)
    : null,
)
</script>

<template>
  <div v-if="drops.length > 0" class="notice tone-danger" role="alert" :data-testid="testid">
    <span aria-hidden="true">&#10006;</span>
    <div class="stack" style="gap: var(--s-1)">
      <span>
        Dropping {{ drops.length }} {{ drops.length === 1 ? 'partition' : 'partitions' }} deletes
        their rows.
        <template v-if="bytes !== null">The size of the data is {{ formatBytes(bytes) }}.</template
        ><template v-else>The size of the data is not known.</template>
        {{ tail }}
      </span>
      <ul class="drops" data-testid="plan-drops">
        <li v-for="a in drops" :key="a.seq">
          <code><SafeName :value="a.partition" :max="80" /></code>
          <template v-if="a.rangeFrom || a.rangeTo">
            ({{ a.rangeFrom ? formatDateTime(a.rangeFrom) : '-' }} to
            {{ a.rangeTo ? formatDateTime(a.rangeTo) : '-' }})</template
          >
        </li>
      </ul>
    </div>
  </div>
</template>

<style scoped>
.drops {
  margin: 0;
  padding-left: var(--s-4);
}
</style>
