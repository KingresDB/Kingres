// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
import type { ChartSeries } from '@/components/LineChart.vue'
import type { MetricQueryResult } from '@/api/types'

/** Converts a metrics response to chart series, labelled per server or "Cluster". */
export function toChartSeries(result: MetricQueryResult | undefined): ChartSeries[] {
  if (!result) return []
  return result.series.map((s) => ({
    label:
      s.labels.serverId !== undefined
        ? `Server ${s.labels.serverId}`
        : (s.labels.databaseName ?? 'Cluster'),
    points: s.points.map((p) => ({ t: new Date(p.timestamp).getTime(), v: p.value })),
  }))
}
