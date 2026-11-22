<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The two choices every copy of a table has (contract 0.14.0): how the rows are copied (`mode`) and what happens to the plain views
// that use the table (`dependentViews`). An online copy is offered only when the preview says it is possible; when it is not,
// the reason is shown next to the choice and the choice stays visible.
import { DEPENDENT_VIEWS_HINT, DEPENDENT_VIEWS_LABEL, MODE_HINT, MODE_LABEL } from '@/lib/tables'
import SafeName from '../SafeName.vue'
import type { DependentViews, TableChangeMode } from '@/api/types'

const mode = defineModel<TableChangeMode>('mode', { required: true })
const views = defineModel<DependentViews>('views', { required: true })
withDefaults(
  defineProps<{
    /** False when the preview says an online copy is not possible; true while it is not known. */
    onlineAvailable?: boolean
    onlineReason?: string | null
    /** The change moves no rows (a bucket count that is a multiple...), so only the views matter. */
    showMode?: boolean
    showViews?: boolean
    /** Online is not offered at all (a change of colocated tables together is blocking only). */
    blockingOnly?: boolean
  }>(),
  {
    onlineAvailable: true,
    onlineReason: null,
    showMode: true,
    showViews: true,
    blockingOnly: false,
  },
)
const MODES: TableChangeMode[] = ['auto', 'online', 'blocking']
</script>

<template>
  <div class="stack">
    <fieldset v-if="showMode" data-testid="table-change-mode">
      <legend>How the rows are copied</legend>
      <label
        v-for="m in MODES"
        :key="m"
        class="check"
        :class="{
          muted:
            (m === 'online' && (!onlineAvailable || blockingOnly)) ||
            (blockingOnly && m === 'auto'),
        }"
      >
        <input
          v-model="mode"
          type="radio"
          :value="m"
          :disabled="(m !== 'blocking' && blockingOnly) || (m === 'online' && !onlineAvailable)"
        />
        {{ MODE_LABEL[m] }}
        <span class="muted"> - {{ MODE_HINT[m] }}</span>
      </label>
      <p v-if="blockingOnly" class="muted" data-testid="table-change-blocking-only">
        A change of the tables of a colocation group together is blocking only.
      </p>
      <p v-else-if="!onlineAvailable" class="muted" data-testid="table-change-online-reason">
        An online copy is not possible<template v-if="onlineReason"
          >: <SafeName :value="onlineReason" kind="text" :max="300" /></template
        >.
      </p>
    </fieldset>
    <fieldset v-if="showViews" data-testid="table-change-views">
      <legend>Views that use the table</legend>
      <label v-for="v in ['refuse', 'recreate'] as const" :key="v" class="check">
        <input v-model="views" type="radio" :value="v" />
        {{ DEPENDENT_VIEWS_LABEL[v] }}
        <span class="muted"> - {{ DEPENDENT_VIEWS_HINT[v] }}</span>
      </label>
    </fieldset>
  </div>
</template>
