<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The long operation that runs on a table (contract 0.32.0, `Table.operation`) as a badge: what it is, how far it is, and for a table that
// a distribute with related tables takes along, that it is "with" another table. A kind that a newer server sends is shown as it was sent.
import { computed } from 'vue'
import SafeName from '../SafeName.vue'
import { operationProgress, operationShort } from '@/lib/tables'
import type { TableOperation } from '@/api/types'

const props = defineProps<{ operation: TableOperation }>()
const word = computed(() => operationShort(props.operation))
const known = computed(() => word.value !== props.operation.kind)
</script>

<template>
  <span
    class="badge tone-info wrap"
    data-testid="table-operation-badge"
    :data-kind="operation.kind"
  >
    <span aria-hidden="true">&#9685;</span>
    <template v-if="known">{{ word }}</template>
    <SafeName v-else :value="operation.kind" kind="text" :max="40" />
    <span class="muted" data-testid="table-operation-progress">{{
      operationProgress(operation)
    }}</span>
    <span v-if="operation.role === 'related'" class="muted">(with another table)</span>
  </span>
</template>
