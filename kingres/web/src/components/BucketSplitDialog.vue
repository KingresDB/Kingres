<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// Split a bucket (contract 0.16.0): the preview shows the new buckets, the disk the split needs and what stands in the
// way; a split is permanent (there is no merge), which is why the action needs the administrator role.
import { computed, ref } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import SafeName from './SafeName.vue'
import TaskActionDialog from './TaskActionDialog.vue'
import QueryBoundary from './QueryBoundary.vue'
import NullableValue from './NullableValue.vue'
import FormField from './FormField.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { formatBytes } from '@/lib/format'

const props = defineProps<{ databaseName: string; colocationId: number; bucket: number }>()
const emit = defineEmits<{ close: [] }>()

const parts = ref<2 | 4 | 8 | 16>(2)
const preview = useQuery({
  queryKey: computed(() => [
    'bucketSplitPreview',
    props.databaseName,
    props.colocationId,
    props.bucket,
    parts.value,
  ]),
  queryFn: () =>
    endpoints.previewBucketSplit(props.databaseName, props.colocationId, props.bucket, {
      parts: parts.value,
    }),
})
const p = computed(() => preview.data.value)
const start = () =>
  endpoints.splitBucket(props.databaseName, props.colocationId, props.bucket, {
    parts: parts.value,
  })
</script>

<template>
  <TaskActionDialog
    :title="`Split bucket ${bucket}`"
    confirm-label="Split bucket"
    wide
    :start="start"
    :confirm-disabled="!p || !p.canSplit"
    :invalidate="[
      keys.colocation(databaseName),
      keys.buckets(databaseName, colocationId),
      keys.tasks,
    ]"
    @close="emit('close')"
  >
    <p>
      The bucket is replaced by smaller buckets on the same shard group. Writers of the group's
      tables wait for the switch only. <strong>A split cannot be undone</strong>; move the new
      buckets apart afterwards with a rebalance.
    </p>
    <FormField v-slot="slot" label="Number of new buckets">
      <select v-bind="slot" v-model.number="parts" data-autofocus>
        <option :value="2">2</option>
        <option :value="4">4</option>
        <option :value="8">8</option>
        <option :value="16">16</option>
      </select>
    </FormField>
    <QueryBoundary
      :loading="preview.isPending.value"
      :error="preview.error.value"
      @retry="preview.refetch()"
    >
      <div v-if="p" class="stack" data-testid="split-preview">
        <ul v-if="p.blockers.length" class="plain" data-testid="split-blockers">
          <li v-for="b in p.blockers" :key="b.code + b.detail" class="notice tone-warn">
            <span aria-hidden="true">&#9650;</span>
            <span><SafeName :value="b.detail" kind="text" :max="300" /></span>
          </li>
        </ul>
        <table class="table">
          <caption class="sr-only">
            The new buckets
          </caption>
          <thead>
            <tr>
              <th scope="col" class="num">Bucket</th>
              <th scope="col" class="num">Modulus</th>
              <th scope="col" class="num">Remainder</th>
              <th scope="col" class="num">Size (estimate)</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="c in p.children" :key="c.bucket">
              <td class="num">{{ c.bucket }}</td>
              <td class="num">{{ c.modulus }}</td>
              <td class="num">{{ c.remainder }}</td>
              <td class="num">
                <NullableValue :value="c.estimatedSizeBytes" :format="formatBytes" />
              </td>
            </tr>
          </tbody>
        </table>
        <p class="muted">
          Until the old partitions are dropped, the shard group and each of its standbys need about
          <NullableValue :value="p.extraDiskBytes" :format="formatBytes" /> more disk.
        </p>
      </div>
    </QueryBoundary>
  </TaskActionDialog>
</template>
