<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The KingresDB logo as inline SVG. The shapes are the ones in kingres/brand/svg (copied by
// `npm run brand:sync` into brand.generated.ts) and are not redrawn here. The crown and "KINGRES"
// take --logo-crown and "DB" takes --logo-db, so the light theme shows the `color-light` coloring
// and the dark theme the `color-dark` one, both without a background. The shield always has the
// gold gradient.
import { computed, useId } from 'vue'
import { brandArt } from './brand.generated'

const props = withDefaults(
  defineProps<{
    variant?: 'mark' | 'horizontal' | 'stacked'
    /** Height in pixels (the width follows the proportions; mark and stacked are square). */
    size?: number
    /**
     * Decorative logos are hidden from assistive technology. Use it when the product name is also
     * written as text next to the logo, so that a screen reader does not say it twice.
     */
    decorative?: boolean
  }>(),
  { variant: 'mark', size: 32, decorative: false },
)

// Several logos can be on one page; the gradient and clip path need ids that are unique per instance.
const uid = useId()
const gradientId = `${uid}-gold`
const clipId = `${uid}-shield`

const art = computed(() => brandArt[props.variant])
const mark = brandArt.mark
const viewBox = computed(() => `0 0 ${art.value.viewBox[0]} ${art.value.viewBox[1]}`)
const width = computed(() => Math.round((props.size * art.value.viewBox[0]) / art.value.viewBox[1]))
const markTransform = computed(() =>
  props.variant === 'mark' ? undefined : (art.value as { markTransform: string }).markTransform,
)
const wordmark = computed(() =>
  props.variant === 'mark'
    ? null
    : (art.value as { wordmark: { kingres: string; db: string } }).wordmark,
)
const wordmarkTransform = computed(() =>
  props.variant === 'stacked'
    ? (art.value as { wordmarkTransform: string }).wordmarkTransform
    : undefined,
)
</script>

<template>
  <svg
    class="brand-logo"
    xmlns="http://www.w3.org/2000/svg"
    :viewBox="viewBox"
    :width="width"
    :height="size"
    :role="decorative ? undefined : 'img'"
    :aria-label="decorative ? undefined : 'KingresDB'"
    :aria-hidden="decorative ? 'true' : undefined"
    focusable="false"
    :data-variant="variant"
  >
    <defs>
      <linearGradient :id="gradientId" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0" :stop-color="mark.gradient[0]" />
        <stop offset="1" :stop-color="mark.gradient[1]" />
      </linearGradient>
      <clipPath :id="clipId"><path :d="mark.shieldClip" /></clipPath>
    </defs>
    <g :transform="markTransform">
      <g class="crown">
        <path :d="mark.crownPath" />
        <circle v-for="(c, i) in mark.crownCircles" :key="i" :cx="c[0]" :cy="c[1]" :r="c[2]" />
        <rect
          :x="mark.crownBand[0]"
          :y="mark.crownBand[1]"
          :width="mark.crownBand[2]"
          :height="mark.crownBand[3]"
          :rx="mark.crownBand[4]"
        />
      </g>
      <g :fill="`url(#${gradientId})`" :clip-path="`url(#${clipId})`" fill-rule="evenodd">
        <path v-for="(d, i) in mark.glyphPaths" :key="i" :d="d" />
      </g>
    </g>
    <g v-if="wordmark" :transform="wordmarkTransform">
      <path class="crown" :d="wordmark.kingres" />
      <path class="db" :d="wordmark.db" />
    </g>
  </svg>
</template>

<style scoped>
.brand-logo {
  display: block;
  flex: none;
}
.crown {
  fill: var(--logo-crown);
}
.db {
  fill: var(--logo-db);
}
</style>
