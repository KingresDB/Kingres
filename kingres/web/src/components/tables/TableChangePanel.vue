<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// What a change of a table would do, before it is made (`previewTableChange`, contract 0.14.0): the blockers that would refuse it,
// the warnings and notes, the tables it would touch, and an estimate of the work (rows, bytes, the mode the server would
// choose, whether an online copy is possible and who waits). It reads only. A blocker disables the start button of the
// dialog that shows this panel; the others never do.
import ProblemNotice from '../ProblemNotice.vue'
import NullableValue from '../NullableValue.vue'
import SafeName from '../SafeName.vue'
import TableBlockerList from './TableBlockerList.vue'
import TableWrap from '../TableWrap.vue'
import { affectedRoleLabel, MODE_LABEL, WRITERS_BLOCKED_TEXT } from '@/lib/tables'
import { formatBytes, formatNumber } from '@/lib/format'
import { isUnsupported } from '@/api/problem'
import type { TableChangePreview } from '@/api/types'

defineProps<{
  preview: TableChangePreview | undefined
  loading: boolean
  error: unknown
}>()
const emit = defineEmits<{ retry: [] }>()
</script>

<template>
  <section class="stack" aria-label="What this change would do" data-testid="table-change-panel">
    <p v-if="loading" role="status" data-testid="table-change-loading">
      Checking what the change would do ...
    </p>
    <template v-else-if="error">
      <p
        v-if="isUnsupported(error)"
        class="notice tone-info"
        role="status"
        data-testid="table-change-unavailable"
      >
        <span aria-hidden="true">&#9432;</span>
        <span
          >This server version cannot show what the change would do. The change can still be
          started; the server refuses it if the table is not eligible.</span
        >
      </p>
      <ProblemNotice v-else :error="error" @retry="emit('retry')" />
    </template>
    <template v-else-if="preview">
      <TableBlockerList
        :lines="preview.blockers"
        tone="blocker"
        heading="This change cannot be made as it is"
      />
      <TableBlockerList :lines="preview.warnings" tone="warning" heading="Take care" />
      <TableBlockerList :lines="preview.notes" tone="note" heading="Good to know" />
      <p
        v-if="preview.blockers.length === 0"
        class="notice tone-ok"
        role="status"
        data-testid="table-change-eligible"
      >
        <span aria-hidden="true">&#10003;</span>
        <span>The sharding extension finds nothing that stops this change.</span>
      </p>
      <TableWrap v-if="preview.affectedTables.length > 0">
        <table class="table" data-testid="table-change-affected">
          <caption>
            Tables this change touches
          </caption>
          <thead>
            <tr>
              <th scope="col">Table</th>
              <th scope="col">Why</th>
              <th scope="col" class="num">Rows (estimate)</th>
              <th scope="col" class="num">Size</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in preview.affectedTables" :key="JSON.stringify([t.schema, t.name])">
              <th scope="row" class="mono">
                <SafeName :value="`${t.schema}.${t.name}`" :max="120" />
              </th>
              <td>{{ affectedRoleLabel(t.role) }}</td>
              <td class="num"><NullableValue :value="t.estimatedRows" :format="formatNumber" /></td>
              <td class="num"><NullableValue :value="t.sizeBytes" :format="formatBytes" /></td>
            </tr>
          </tbody>
        </table>
      </TableWrap>
      <dl class="kv" data-testid="table-change-estimate">
        <dt>Rows to copy</dt>
        <dd><NullableValue :value="preview.estimate.rows" :format="formatNumber" /></dd>
        <dt>Bytes to copy</dt>
        <dd><NullableValue :value="preview.estimate.bytes" :format="formatBytes" /></dd>
        <dt>The server would choose</dt>
        <dd data-testid="table-change-recommended">
          {{ MODE_LABEL[preview.estimate.modeRecommended] }}
        </dd>
        <dt>An online copy</dt>
        <dd data-testid="table-change-online">
          <template v-if="preview.estimate.onlineAvailable">is possible</template>
          <template v-else
            >is not possible<template v-if="preview.estimate.onlineUnavailableReason"
              >:
              <SafeName
                :value="preview.estimate.onlineUnavailableReason"
                kind="text"
                :max="300" /></template
          ></template>
        </dd>
        <dt>Who waits</dt>
        <dd data-testid="table-change-writers">
          {{ WRITERS_BLOCKED_TEXT[preview.estimate.writersBlocked] }}
        </dd>
      </dl>
    </template>
  </section>
</template>

<style scoped>
.kv {
  display: grid;
  grid-template-columns: fit-content(45%) minmax(0, 1fr);
  gap: var(--s-1) var(--s-4);
  margin: 0;
}
.kv dt {
  color: var(--c-text-muted);
}
.kv dd {
  margin: 0;
}
</style>
