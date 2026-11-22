<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The card "Approximate aggregates" of a database (contract 0.18.0, `getApproximateAggregates`): whether the sketch types are on, the
// version of the extension, the mode of count(DISTINCT) and its error rate, and the state of the extension on every server.
// "Configure" is for kingres_admin; the other roles see the card and a disabled button with the reason. A server that does not
// know the operation (an older server) is shown as calm information by the query boundary.
import { ref } from 'vue'
import GateButton from './GateButton.vue'
import NullableValue from './NullableValue.vue'
import QueryBoundary from './QueryBoundary.vue'
import SafeName from './SafeName.vue'
import StatusBadge from './StatusBadge.vue'
import TableWrap from './TableWrap.vue'
import ApproximateAggregatesDialog from './ApproximateAggregatesDialog.vue'
import { useApproximateAggregates } from '@/api/queries'
import { COUNT_DISTINCT_LABEL, SERVER_STATE, rateToPercent } from '@/lib/analytics'

const props = defineProps<{ databaseName: string }>()
const aggregates = useApproximateAggregates(() => props.databaseName)
const open = ref(false)
</script>

<template>
  <div class="card stack" data-testid="aggregates-card">
    <div class="row-between">
      <h2 id="aggregates-h">Approximate aggregates</h2>
      <GateButton
        op="configureApproximateAggregates"
        small
        data-testid="aggregates-configure"
        :disabled="!aggregates.data.value"
        @click="open = true"
      >
        Configure
      </GateButton>
    </div>
    <p class="muted">
      Adds the sketch types hll, topn and tdigest to this database on every server. Approximate
      results are estimates with a stated error. The exact count(DISTINCT) is the default and
      changes only if you choose the approximate mode below.
    </p>
    <QueryBoundary
      :loading="aggregates.isPending.value"
      :error="aggregates.error.value"
      @retry="aggregates.refetch()"
    >
      <template v-if="aggregates.data.value">
        <dl class="kv" data-testid="aggregates-facts">
          <dt>State</dt>
          <dd>
            <StatusBadge
              :meta="
                aggregates.data.value.enabled
                  ? { tone: 'ok', icon: '✓', label: 'On' }
                  : { tone: 'neutral', icon: '○', label: 'Off' }
              "
            />
          </dd>
          <dt>Extension version</dt>
          <dd><NullableValue :value="aggregates.data.value.extensionVersion" safe /></dd>
          <dt>count(DISTINCT)</dt>
          <dd data-testid="aggregates-mode">
            {{ COUNT_DISTINCT_LABEL[aggregates.data.value.countDistinct] }}
            <template v-if="aggregates.data.value.errorRate !== null">
              <span class="muted">
                (error rate {{ rateToPercent(aggregates.data.value.errorRate) }} percent)</span
              >
            </template>
          </dd>
        </dl>
        <TableWrap>
          <table class="table" data-testid="aggregates-servers">
            <caption class="sr-only">
              The extension on every server
            </caption>
            <thead>
              <tr>
                <th scope="col">Server</th>
                <th scope="col">Installed version</th>
                <th scope="col">State</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="s in aggregates.data.value.servers" :key="s.serverId">
                <th scope="row" class="mono">
                  <SafeName :value="s.serverId" kind="text" :max="60" />
                </th>
                <td><NullableValue :value="s.installedVersion" safe /></td>
                <td><StatusBadge :meta="SERVER_STATE[s.state]" /></td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </template>
    </QueryBoundary>
  </div>
  <ApproximateAggregatesDialog
    v-if="open && aggregates.data.value"
    :database-name="databaseName"
    :current="aggregates.data.value"
    @close="open = false"
  />
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
