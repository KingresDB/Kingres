<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// "Shard by" Rows, Schemas or Both with one sentence each, and the checkbox "Distribute new schemas automatically" for the
// modes that have schemas. The create dialog and the settings dialog use it. For a role that cannot change the setting
// (`readonly`) the same facts are shown as text. Choosing a mode sets the default of the checkbox (on for Schemas, off for Both).
import { computed } from 'vue'
import {
  canDistributeNew,
  DISTRIBUTE_NEW_SENTENCE,
  distributeNewDefault,
  MODE_LABEL,
  MODE_SENTENCE,
  type ShardingMode,
} from '@/lib/sharding-mode'

const props = withDefaults(
  defineProps<{
    modes: readonly ShardingMode[]
    /** Show the choice as text: the account may not change it. */
    readonly?: boolean
    /** The controls are shown but cannot be changed now (while a request runs). */
    disabled?: boolean
    /**
     * Choosing a mode sets the checkbox to the default of that mode (a new database). Turned off for a database that exists, where
     * the setting keeps its value and only becomes false in a mode that has no schemas.
     */
    resetDefault?: boolean
  }>(),
  { readonly: false, disabled: false, resetDefault: true },
)
const mode = defineModel<ShardingMode>('mode', { required: true })
const distributeNew = defineModel<boolean>('distributeNew', { required: true })

const group = `mode-${Math.random().toString(36).slice(2, 9)}`
const showDistributeNew = computed(() => canDistributeNew(mode.value))

function choose(next: ShardingMode): void {
  if (next === mode.value) return
  mode.value = next
  if (props.resetDefault) distributeNew.value = distributeNewDefault(next)
  else if (!canDistributeNew(next)) distributeNew.value = false
}
</script>

<template>
  <div class="stack" data-testid="sharding-mode">
    <template v-if="readonly">
      <p>
        Shard by: <strong data-testid="mode-text">{{ MODE_LABEL[mode] }}</strong>
        <span class="muted"> ({{ MODE_SENTENCE[mode] }})</span>
      </p>
      <p v-if="showDistributeNew">
        Distribute new schemas automatically:
        <strong data-testid="distribute-new-text">{{ distributeNew ? 'Yes' : 'No' }}</strong>
      </p>
    </template>
    <template v-else>
      <fieldset>
        <legend>Shard by</legend>
        <label v-for="m in props.modes" :key="m" class="check">
          <input
            type="radio"
            :name="group"
            :value="m"
            :checked="mode === m"
            :disabled="disabled"
            @change="choose(m)"
          />
          <span
            ><strong>{{ MODE_LABEL[m] }}</strong
            >: {{ MODE_SENTENCE[m] }}</span
          >
        </label>
      </fieldset>
      <template v-if="showDistributeNew">
        <label class="check">
          <input v-model="distributeNew" type="checkbox" :disabled="disabled" />
          Distribute new schemas automatically
        </label>
        <p class="hint muted">{{ DISTRIBUTE_NEW_SENTENCE }}</p>
      </template>
    </template>
  </div>
</template>
