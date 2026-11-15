<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The metrics of the client entry point (contract 0.7.0): connections that clients make to the cluster through the
// entry point of kingresd, and the same for the standby entry point. The entry point is off by default, and a server
// whose entry point is off reports none of these names, so the group appears only when a probe finds a series with
// points, and shows nothing at all (not even a heading) otherwise.
import { computed } from 'vue'
import MetricChart from './MetricChart.vue'
import ProblemNotice from './ProblemNotice.vue'
import { ApiError, isUnsupported } from '@/api/problem'
import { useMetric } from '@/api/queries'
import type { MetricName } from '@/api/types'

type Unit = 'count' | 'bytes'

/** The eight names of one entry point, in the order they are shown. */
function names(
  prefix: 'client' | 'clientStandby',
): { metric: MetricName; title: string; unit: Unit }[] {
  return [
    { metric: `${prefix}ConnectionsActive`, title: 'Connections open now', unit: 'count' },
    {
      metric: `${prefix}ConnectionsAccepted`,
      title: 'Connections accepted (total)',
      unit: 'count',
    },
    {
      metric: `${prefix}ConnectionsRefused`,
      title: 'Connections refused, over the cap (total)',
      unit: 'count',
    },
    {
      metric: `${prefix}ConnectFailures`,
      title: 'Failed connects to a server (total)',
      unit: 'count',
    },
    {
      metric: `${prefix}ConnectionsMoved`,
      title: 'Connections closed, destination changed (total)',
      unit: 'count',
    },
    {
      metric: `${prefix}ConnectionsIdleClosed`,
      title: 'Connections closed when idle (total)',
      unit: 'count',
    },
    { metric: `${prefix}BytesIn`, title: 'Bytes received from clients (total)', unit: 'bytes' },
    { metric: `${prefix}BytesOut`, title: 'Bytes sent to clients (total)', unit: 'bytes' },
  ] as { metric: MetricName; title: string; unit: Unit }[]
}
const main = names('client')
const standby = names('clientStandby')

const probeMain = useMetric('clientConnectionsActive', 60, 'server')
const probeStandby = useMetric('clientStandbyConnectionsActive', 60, 'server')
const reports = (q: typeof probeMain): boolean =>
  (q.data.value?.series ?? []).some((s) => s.points.length > 0)
const hasMain = computed(() => reports(probeMain))
const hasStandby = computed(() => reports(probeStandby))
/**
 * An error that means "this server does not have it" is not shown (an older revision refuses the name, a server
 * without the entry point has no series); any other error is, so that a failure is never mistaken for "off".
 */
const hardError = computed(() =>
  [probeMain.error.value, probeStandby.error.value].find(
    (e) =>
      e !== null &&
      e !== undefined &&
      !isUnsupported(e) &&
      !(e instanceof ApiError && e.status >= 400 && e.status < 500),
  ),
)
</script>

<template>
  <section
    v-if="hasMain || hasStandby || hardError"
    class="stack"
    aria-labelledby="client-h"
    data-testid="client-entry-point"
  >
    <h2 id="client-h">Client entry point</h2>
    <ProblemNotice v-if="hardError && !hasMain && !hasStandby" :error="hardError" />
    <template v-if="hasMain">
      <p class="muted">
        Connections that clients make through the client entry point of each server. Counters are
        totals since the start of the server; only "open now" goes up and down.
      </p>
      <div class="charts">
        <MetricChart v-for="m in main" :key="m.metric" v-bind="m" />
      </div>
    </template>
    <template v-if="hasStandby">
      <h3>Standby entry point</h3>
      <div class="charts">
        <MetricChart v-for="m in standby" :key="m.metric" v-bind="m" />
      </div>
    </template>
  </section>
</template>

<style scoped>
.charts {
  display: grid;
  gap: var(--s-4);
  grid-template-columns: repeat(2, minmax(0, 1fr));
}
@media (min-width: 1900px) {
  .charts {
    grid-template-columns: repeat(4, minmax(0, 1fr));
  }
}
@media (max-width: 700px) {
  .charts {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
