<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The weight of a server (contract 0.15.0): its capacity relative to the others. A server of weight 2 gets twice the
// buckets (or bytes) of one of weight 1 at the next rebalance.
import { computed, ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import AppDialog from './AppDialog.vue'
import FormField from './FormField.vue'
import ProblemNotice from './ProblemNotice.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import type { Server } from '@/api/types'

const props = defineProps<{ server: Server }>()
const emit = defineEmits<{ close: [] }>()
const client = useQueryClient()

const weight = ref<number | null>(props.server.weight ?? 1)
const submitted = ref(false)
const error = computed(() => {
  if (!submitted.value) return undefined
  const w = weight.value
  return w === null || !(w > 0) || w > 1000 ? 'Enter a number above 0 and at most 1000.' : undefined
})
const mutation = useMutation({
  mutationFn: () => endpoints.updateServer(props.server.id, { weight: weight.value as number }),
  onSuccess: () => {
    void client.invalidateQueries({ queryKey: keys.servers })
    void client.invalidateQueries({ queryKey: keys.rebalance })
    emit('close')
  },
})
function save(): void {
  submitted.value = true
  if (
    error.value === undefined &&
    weight.value !== null &&
    weight.value > 0 &&
    weight.value <= 1000
  )
    mutation.mutate()
}
</script>

<template>
  <AppDialog :title="`Weight of server ${server.id}`" @close="emit('close')">
    <p>
      The weight is the capacity of this server relative to the others. At the next rebalance a
      server of weight 2 receives twice the buckets, or the bytes, of one of weight 1. Nothing moves
      until a rebalance runs.
    </p>
    <FormField
      v-slot="slot"
      label="Weight"
      hint="Above 0, at most 1000. The default is 1."
      :error="error"
      required
    >
      <input
        v-bind="slot"
        v-model.number="weight"
        class="input num"
        type="number"
        min="0.01"
        max="1000"
        step="any"
        data-autofocus
        @keydown.enter.prevent="save"
      />
    </FormField>
    <ProblemNotice v-if="mutation.error.value" :error="mutation.error.value" />
    <template #footer>
      <button type="button" class="btn" :disabled="mutation.isPending.value" @click="emit('close')">
        Cancel
      </button>
      <button
        type="button"
        class="btn btn-primary"
        :disabled="mutation.isPending.value"
        @click="save"
      >
        Save weight
      </button>
    </template>
  </AppDialog>
</template>
