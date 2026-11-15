<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Compact bucket map: one cell per bucket, coloured by the owning shard group. Every cell also
// shows the group number, so colour is never the only signal; moving buckets get a dashed outline
// and an arrow, orphans a cross. A per-group summary table is the accessible alternative.
import { computed } from 'vue'
import TableWrap from './TableWrap.vue'
import type { Bucket } from '@/api/types'
import { formatNumber } from '@/lib/format'

const props = defineProps<{
  buckets: Bucket[]
  /** Group ids whose colours are fixed in this order (the map group is not a data group). */
  groupIds: number[]
  /** Dim buckets that do not match. Used by the server and state filters. */
  isDimmed?: (bucket: Bucket) => boolean
  selected?: number | null
}>()
const emit = defineEmits<{ select: [bucket: Bucket] }>()

function colorVar(groupId: number): string {
  const i = props.groupIds.indexOf(groupId)
  return `var(--g-${(i < 0 ? 0 : i % 8) + 1})`
}

const cells = computed(() =>
  props.buckets.map((b) => ({
    b,
    style: { background: colorVar(b.shardGroupId) },
    mark: b.state === 'moving' ? '→' : b.state === 'orphan' ? '✕' : String(b.shardGroupId),
    title:
      `Bucket ${b.bucket}: group ${b.shardGroupId}` +
      (b.state === 'moving' ? `, moving to group ${b.movingToShardGroupId ?? '?'}` : '') +
      (b.state === 'orphan' ? ', orphan' : ''),
    dim: props.isDimmed?.(b) ?? false,
  })),
)

const summary = computed(() => {
  const rows = props.groupIds.map((id) => ({
    id,
    total: 0,
    moving: 0,
    orphan: 0,
  }))
  for (const b of props.buckets) {
    const r = rows.find((x) => x.id === b.shardGroupId)
    if (!r) continue
    r.total += 1
    if (b.state === 'moving') r.moving += 1
    if (b.state === 'orphan') r.orphan += 1
  }
  return rows
})
</script>

<template>
  <div class="stack">
    <div
      class="bucket-grid"
      role="group"
      :aria-label="`Bucket map with ${formatNumber(buckets.length)} buckets. See the table below for counts per shard group.`"
      data-testid="bucket-grid"
    >
      <button
        v-for="c in cells"
        :key="c.b.bucket"
        type="button"
        tabindex="-1"
        aria-hidden="true"
        class="cell"
        :class="[`state-${c.b.state}`, { dim: c.dim, selected: selected === c.b.bucket }]"
        :style="c.style"
        :title="c.title"
        data-testid="bucket-cell"
        :data-bucket="c.b.bucket"
        :data-group="c.b.shardGroupId"
        :data-state="c.b.state"
        @click="emit('select', c.b)"
      >
        {{ c.mark }}
      </button>
    </div>
    <ul class="legend">
      <li v-for="g in groupIds" :key="g">
        <span class="swatch" :style="{ background: colorVar(g) }">{{ g }}</span>
        {{ g === 0 ? 'Map group (0)' : `Shard group ${g}` }}
      </li>
      <li><span class="swatch moving">&rarr;</span> Moving</li>
      <li><span class="swatch orphan">&#10005;</span> Orphan</li>
    </ul>
    <TableWrap>
      <table class="table">
        <caption class="sr-only">
          Buckets per shard group
        </caption>
        <thead>
          <tr>
            <th scope="col">Shard group</th>
            <th scope="col" class="num">Buckets</th>
            <th scope="col" class="num">Moving</th>
            <th scope="col" class="num">Orphan</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in summary" :key="r.id">
            <th scope="row">{{ r.id === 0 ? 'Map group (0)' : `Group ${r.id}` }}</th>
            <td class="num">{{ r.total }}</td>
            <td class="num">{{ r.moving }}</td>
            <td class="num">{{ r.orphan }}</td>
          </tr>
        </tbody>
      </table>
    </TableWrap>
  </div>
</template>

<style scoped>
.bucket-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(22px, 1fr));
  gap: 2px;
  max-width: 100%;
}
.cell {
  aspect-ratio: 1;
  min-width: 0;
  padding: 0;
  border: 2px solid transparent;
  border-radius: 3px;
  color: var(--g-text);
  font: 600 10px/1 var(--font-sans);
  font-variant-numeric: tabular-nums;
  cursor: pointer;
}
.cell.state-moving {
  border: 2px dashed var(--c-text);
}
.cell.state-orphan {
  border: 2px solid var(--c-danger-solid);
  background-image: repeating-linear-gradient(45deg, transparent 0 3px, rgb(0 0 0 / 35%) 3px 5px);
}
.cell.dim {
  opacity: 0.18;
}
.cell.selected {
  outline: 2px solid var(--c-focus);
  outline-offset: 1px;
}
.legend {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-wrap: wrap;
  gap: var(--s-1) var(--s-4);
  font-size: var(--fs-sm);
}
.legend li {
  display: inline-flex;
  align-items: center;
  gap: var(--s-1);
}
.swatch {
  display: inline-grid;
  place-items: center;
  width: 18px;
  height: 18px;
  border-radius: 3px;
  color: var(--g-text);
  font: 600 10px/1 var(--font-sans);
  border: 2px solid transparent;
}
.swatch.moving {
  border: 2px dashed var(--c-text);
  color: var(--c-text);
}
.swatch.orphan {
  border: 2px solid var(--c-danger-solid);
  color: var(--c-danger-fg);
}
</style>
