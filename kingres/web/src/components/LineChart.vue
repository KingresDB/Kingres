<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// A small dependency-free SVG line chart for the metrics endpoint. Multiple series share axes.
// Lines differ by colour and by dash pattern, and the legend names every series.
import { computed } from 'vue'
import { formatBytes, formatNumber } from '@/lib/format'

export interface ChartSeries {
  label: string
  points: { t: number; v: number | null }[]
}

const props = withDefaults(
  defineProps<{
    title: string
    series: ChartSeries[]
    unit: 'percent' | 'bytes' | 'count' | 'countPerSecond' | 'seconds'
    height?: number
  }>(),
  { height: 140 },
)

const W = 400
const PAD = { l: 44, r: 8, t: 8, b: 18 }
const DASHES = ['', '5 3', '1 3', '8 3 1 3']

function fmt(v: number): string {
  switch (props.unit) {
    case 'percent':
      return `${v.toFixed(v < 10 ? 1 : 0)}%`
    case 'bytes':
      return formatBytes(v)
    case 'seconds':
      return `${v.toFixed(v < 10 ? 2 : 0)} s`
    case 'countPerSecond':
      return `${formatNumber(v, v < 10 ? 1 : 0)}/s`
    default:
      return formatNumber(v, v < 10 && !Number.isInteger(v) ? 1 : 0)
  }
}

const bounds = computed(() => {
  let tMin = Infinity
  let tMax = -Infinity
  let vMax = props.unit === 'percent' ? 100 : 0
  for (const s of props.series)
    for (const p of s.points) {
      tMin = Math.min(tMin, p.t)
      tMax = Math.max(tMax, p.t)
      if (p.v !== null && props.unit !== 'percent') vMax = Math.max(vMax, p.v)
    }
  if (!Number.isFinite(tMin)) return null
  return { tMin, tMax: tMax === tMin ? tMin + 1 : tMax, vMax: vMax === 0 ? 1 : vMax * 1.1 }
})

function x(t: number): number {
  const b = bounds.value
  if (!b) return PAD.l
  return PAD.l + ((t - b.tMin) / (b.tMax - b.tMin)) * (W - PAD.l - PAD.r)
}
function y(v: number): number {
  const b = bounds.value
  if (!b) return props.height - PAD.b
  return PAD.t + (1 - v / b.vMax) * (props.height - PAD.t - PAD.b)
}

const paths = computed(() =>
  props.series.map((s, i) => {
    let d = ''
    let pen = false
    for (const p of s.points) {
      if (p.v === null) {
        pen = false
        continue
      }
      d += `${pen ? 'L' : 'M'}${x(p.t).toFixed(1)} ${y(p.v).toFixed(1)}`
      pen = true
    }
    return { label: s.label, d, dash: DASHES[i % DASHES.length] ?? '', cls: `s${(i % 6) + 1}` }
  }),
)

const ticks = computed(() => {
  const b = bounds.value
  if (!b) return []
  return [0, 0.5, 1].map((f) => ({ v: b.vMax * f, y: y(b.vMax * f) }))
})

const timeLabels = computed(() => {
  const b = bounds.value
  if (!b) return null
  const f = (t: number) => new Date(t).toISOString().slice(11, 16)
  return { from: f(b.tMin), to: f(b.tMax) }
})

const latest = computed(() =>
  props.series.map((s) => {
    const last = [...s.points].reverse().find((p) => p.v !== null)
    return { label: s.label, value: last?.v ?? null }
  }),
)

const description = computed(() =>
  latest.value.map((l) => `${l.label}: ${l.value === null ? 'no data' : fmt(l.value)}`).join('; '),
)
</script>

<template>
  <figure class="chart">
    <figcaption>{{ title }}</figcaption>
    <svg
      v-if="bounds"
      :viewBox="`0 0 ${W} ${height}`"
      role="img"
      :aria-label="`${title}. Latest: ${description}`"
      preserveAspectRatio="none"
    >
      <g class="grid">
        <template v-for="t in ticks" :key="t.v">
          <line :x1="PAD.l" :x2="W - PAD.r" :y1="t.y" :y2="t.y" />
          <text :x="PAD.l - 4" :y="t.y + 3" text-anchor="end">{{ fmt(t.v) }}</text>
        </template>
      </g>
      <g v-if="timeLabels" class="grid">
        <text :x="PAD.l" :y="height - 4">{{ timeLabels.from }}</text>
        <text :x="W - PAD.r" :y="height - 4" text-anchor="end">{{ timeLabels.to }} UTC</text>
      </g>
      <path
        v-for="p in paths"
        :key="p.label"
        :d="p.d"
        :class="p.cls"
        :stroke-dasharray="p.dash"
        fill="none"
        stroke-width="1.8"
        vector-effect="non-scaling-stroke"
      />
    </svg>
    <p v-else class="muted">No data in this period.</p>
    <ul v-if="series.length > 1 || latest.length" class="legend">
      <li v-for="(l, i) in latest" :key="l.label">
        <svg width="22" height="8" aria-hidden="true">
          <line
            x1="0"
            x2="22"
            y1="4"
            y2="4"
            :class="`s${(i % 6) + 1}`"
            :stroke-dasharray="DASHES[i % DASHES.length]"
            stroke-width="2"
          />
        </svg>
        {{ l.label }}
        <span class="muted num">{{ l.value === null ? '-' : fmt(l.value) }}</span>
      </li>
    </ul>
  </figure>
</template>

<style scoped>
.chart {
  margin: 0;
}
figcaption {
  font-weight: 600;
  margin-bottom: var(--s-1);
}
svg {
  width: 100%;
  display: block;
}
.grid line {
  stroke: var(--c-border);
  stroke-width: 1;
}
.grid text {
  fill: var(--c-text-muted);
  font-size: 9px;
  font-variant-numeric: tabular-nums;
}
.s1 {
  stroke: var(--g-1);
}
.s2 {
  stroke: var(--g-2);
}
.s3 {
  stroke: var(--g-3);
}
.s4 {
  stroke: var(--g-4);
}
.s5 {
  stroke: var(--g-5);
}
.s6 {
  stroke: var(--g-6);
}
.legend {
  list-style: none;
  margin: var(--s-1) 0 0;
  padding: 0;
  display: flex;
  flex-wrap: wrap;
  gap: var(--s-1) var(--s-3);
  font-size: var(--fs-sm);
}
.legend li {
  white-space: nowrap;
  display: inline-flex;
  align-items: center;
  gap: var(--s-1);
}
</style>
