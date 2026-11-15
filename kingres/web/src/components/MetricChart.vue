<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// One metric of the last hour as a chart in a card, one series per server.
import QueryBoundary from './QueryBoundary.vue'
import LineChart from './LineChart.vue'
import { useMetric } from '@/api/queries'
import { toChartSeries } from '@/lib/metrics'
import type { MetricName } from '@/api/types'

const props = defineProps<{
  metric: MetricName
  title: string
  unit: 'percent' | 'bytes' | 'count' | 'countPerSecond' | 'seconds'
}>()
const query = useMetric(props.metric, 60, 'server')
</script>

<template>
  <div class="card" :data-testid="`metric-${metric}`">
    <QueryBoundary :loading="query.isPending.value" :error="query.error.value">
      <LineChart :title="title" :unit="unit" :series="toChartSeries(query.data.value)" />
    </QueryBoundary>
  </div>
</template>
