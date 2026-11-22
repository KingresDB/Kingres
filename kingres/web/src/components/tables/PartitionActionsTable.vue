<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The actions of a run of a time-partitioning policy, or of a plan (a dry run): what is created, converted, dropped or detached,
// for which partition and range, how it went, and the bytes before and after. A failed action says what happened in words when
// the code is one the scheduler is known to use; any other detail is shown as the server sent it (through SafeName).
import SafeName from '../SafeName.vue'
import StatusBadge from '../StatusBadge.vue'
import TableWrap from '../TableWrap.vue'
import NullableValue from '../NullableValue.vue'
import { formatBytes, formatDateTime } from '@/lib/format'
import {
  ACTION_KIND_LABEL,
  ACTION_STATE,
  actionDetailText,
  type PartitionAction,
} from '@/lib/analytics'

defineProps<{ actions: PartitionAction[]; table: string; caption: string }>()
</script>

<template>
  <TableWrap>
    <table class="table" data-testid="partition-actions">
      <caption class="sr-only">
        {{
          caption
        }}
      </caption>
      <thead>
        <tr>
          <th scope="col">Action</th>
          <th scope="col">Partition</th>
          <th scope="col">Range</th>
          <th scope="col">State</th>
          <th scope="col">What happened</th>
          <th scope="col" class="num">Before</th>
          <th scope="col" class="num">After</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="a in actions" :key="a.seq" :data-testid="`action-${a.seq}`">
          <td>{{ ACTION_KIND_LABEL[a.kind] ?? a.kind }}</td>
          <th scope="row" class="mono"><SafeName :value="a.partition" :max="80" /></th>
          <td class="mono">
            <template v-if="a.rangeFrom || a.rangeTo">
              {{ a.rangeFrom ? formatDateTime(a.rangeFrom) : '-' }} to
              {{ a.rangeTo ? formatDateTime(a.rangeTo) : '-' }}
            </template>
            <template v-else>-</template>
          </td>
          <td><StatusBadge :meta="ACTION_STATE[a.state]" /></td>
          <td>
            <template v-if="a.detail">
              <template v-if="actionDetailText(a, table)">{{
                actionDetailText(a, table)
              }}</template>
              <SafeName v-else :value="a.detail" kind="text" :max="200" />
            </template>
            <template v-else>-</template>
          </td>
          <td class="num"><NullableValue :value="a.bytesBefore" :format="formatBytes" /></td>
          <td class="num"><NullableValue :value="a.bytesAfter" :format="formatBytes" /></td>
        </tr>
      </tbody>
    </table>
  </TableWrap>
</template>
