<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The one way to show a name or a message that came from the database or from a log. It makes
// hidden characters visible (bidirectional controls, zero-width characters, control characters),
// isolates the text direction so the text cannot reorder what is around it, flags a name that can
// imitate another one, and cuts a very long value with a way to see all of it. What is sent to the
// server is never changed: this only decides how a string is drawn.
import { computed, ref } from 'vue'
import { nameWarnings, pieces } from '@/lib/safe-text'

const props = withDefaults(
  defineProps<{
    value: string
    /** `name` also flags look-alike names; `text` (messages, titles) only makes hidden characters visible. */
    kind?: 'name' | 'text'
    /** Characters shown before the rest is folded away. */
    max?: number
    /** Show line breaks (each one marked) instead of folding them into one line. */
    multiline?: boolean
  }>(),
  { kind: 'name', max: 160, multiline: false },
)

const expanded = ref(false)
const length = computed(() => [...props.value].length)
const folded = computed(() => length.value > props.max && !expanded.value)
const parts = computed(() => pieces(props.value, folded.value ? props.max : Infinity))
const warnings = computed(() => (props.kind === 'name' ? nameWarnings(props.value) : []))
const warningLabel = computed(() =>
  warnings.value.some((w) => w.startsWith('Contains'))
    ? 'hidden characters'
    : warnings.value.some((w) => w.startsWith('Has spaces'))
      ? 'spaces'
      : warnings.value.some((w) => w.startsWith('Mixes'))
        ? 'mixed scripts'
        : 'look-alike',
)
</script>

<template>
  <bdi class="safe" :class="{ multiline }"
    ><template v-for="(p, i) in parts" :key="i"
      ><span
        v-if="p.escape"
        class="esc"
        role="img"
        :aria-label="`hidden character ${p.escape.code}, ${p.escape.description}`"
        :title="`${p.escape.code}: ${p.escape.description}`"
        >{{ p.text }}</span
      ><template v-else>{{ p.text }}</template
      ><template
        v-if="multiline && p.escape && (p.escape.code === 'U+000A' || p.escape.code === 'U+2028')"
        ><br /></template></template
    ><span v-if="folded" class="muted">&hellip;</span
    ><button
      v-if="length > max"
      type="button"
      class="btn-link safe-more"
      :aria-expanded="expanded"
      @click="expanded = !expanded"
    >
      {{ expanded ? 'Show less' : `Show all ${length} characters` }}</button
    ><span v-if="warnings.length" class="badge tone-warn safe-flag" :title="warnings.join(' ')"
      ><span aria-hidden="true">&#9650;</span> {{ warningLabel
      }}<span class="sr-only">. {{ warnings.join(' ') }}</span></span
    ></bdi
  >
</template>
