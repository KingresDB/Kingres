<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The first records of the file as the person's options read them (the preview follows the corrections). Every cell is
// text from a file: it is drawn through SafeName (hidden characters made visible, cut when long), never as markup.
import { computed } from 'vue'
import SafeName from '@/components/SafeName.vue'
import TableWrap from '@/components/TableWrap.vue'
import { previewRows } from '@/lib/csv-guess'
import type { CsvEncoding } from '@/api/types'

const props = defineProps<{
  bytes: Uint8Array
  encoding: CsvEncoding
  delimiter: string
  quote: string
  partial: boolean
  /** Whether the first record is names (`skip` or `match`) or data (`none`). */
  header: boolean
}>()

const rows = computed(() =>
  previewRows(props.bytes, props.encoding, props.delimiter, props.quote, props.partial, 6),
)
const width = computed(() => Math.max(0, ...rows.value.map((r) => r.length)))
const head = computed(() => (props.header ? (rows.value[0] ?? []) : []))
const body = computed(() => (props.header ? rows.value.slice(1) : rows.value))
</script>

<template>
  <div class="stack" data-testid="csv-preview">
    <p v-if="rows.length === 0" class="muted">The beginning of the file has no complete record.</p>
    <TableWrap v-else label="Preview of the first records of the file">
      <table class="table">
        <caption class="sr-only">
          The first records of the file, read with the options below
        </caption>
        <thead v-if="header">
          <tr>
            <th v-for="i in width" :key="i" scope="col">
              <SafeName :value="head[i - 1] ?? ''" kind="text" :max="40" />
            </th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(r, n) in body" :key="n">
            <td v-for="i in width" :key="i">
              <SafeName :value="r[i - 1] ?? ''" kind="text" :max="40" />
            </td>
          </tr>
        </tbody>
      </table>
    </TableWrap>
    <p class="muted">
      The beginning of the file ({{ rows.length }} {{ rows.length === 1 ? 'record' : 'records' }})
      is shown as read with these options. Check that every column is in its own field.
    </p>
  </div>
</template>
