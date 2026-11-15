<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { computed } from 'vue'
import SafeName from '@/components/SafeName.vue'
import { RouterLink } from 'vue-router'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import NullableValue from '@/components/NullableValue.vue'
import LineChart from '@/components/LineChart.vue'
import PartialNotice from '@/components/PartialNotice.vue'
import { useCompletenessAnnouncement } from '@/composables/useCompletenessAnnouncement'
import { completenessOf, mergeCompleteness } from '@/lib/partial'
import { useAlerts, useCluster, useClusterSummary, useMetric, useTasks } from '@/api/queries'
import { HEALTH, SEVERITY, TASK_STATE } from '@/lib/status'
import { formatAgo, formatNumber } from '@/lib/format'
import { toChartSeries } from '@/lib/metrics'
import { isUnsupported } from '@/api/problem'
import ProblemNotice from '@/components/ProblemNotice.vue'
import { useSessionStore } from '@/stores/session'

const session = useSessionStore()
// What a kingres_stats account cannot read is left out instead of failing.
const canCluster = computed(() => session.can('getCluster'))
const canAlerts = computed(() => session.can('listAlerts'))
const canTasks = computed(() => session.can('listTasks'))
const cluster = useCluster(canCluster)
// A stats-only account cannot read /cluster; GET /cluster/summary is open to it.
const summary = useClusterSummary(() => !canCluster.value)
const alerts = useAlerts('open', canAlerts)
const tasks = useTasks(5, canTasks)
const cpu = useMetric('cpuUsagePercent', 60, 'server')
const tps = useMetric('transactionsPerSecond', 60)
const lag = useMetric('replicationLagSeconds', 60, 'server')
const memory = useMetric('memoryUsedBytes', 60, 'server')

const c = computed(() => cluster.data.value)
// Charts and the alert list are cluster-wide answers: a server that did not answer is named, never
// silently left out (contract 0.5.0).
const metricsCompleteness = computed(() =>
  mergeCompleteness([cpu, tps, lag, memory].map((q) => q.data.value)),
)
const alertsCompleteness = computed(() => completenessOf(alerts.data.value))
useCompletenessAnnouncement(metricsCompleteness, 'set of charts')
useCompletenessAnnouncement(alertsCompleteness, 'list of open alerts', canAlerts)
// When the server has no metrics at all, say so once instead of in four cards.
const metricsUnsupported = computed(() =>
  [cpu, tps, lag, memory].every((q) => isUnsupported(q.error.value)),
)
</script>

<template>
  <PageHeader title="Dashboard" subtitle="Cluster health at a glance." />
  <div class="stack-lg">
    <QueryBoundary
      v-if="!canCluster"
      :loading="summary.isPending.value"
      :error="summary.error.value"
      @retry="summary.refetch()"
    >
      <section
        v-if="summary.data.value"
        class="grid-cards"
        aria-label="Summary"
        data-testid="cluster-summary"
      >
        <div class="card kpi">
          <span class="label">Cluster health</span>
          <span class="value"><StatusBadge :meta="HEALTH[summary.data.value.health]" /></span>
          <span class="muted"><NullableValue :value="summary.data.value.name" /></span>
        </div>
        <div class="card kpi">
          <span class="label">Servers</span>
          <span class="value">{{ summary.data.value.serverCount }}</span>
        </div>
        <div class="card kpi">
          <span class="label">Databases</span>
          <span class="value">{{ summary.data.value.databaseCount }}</span>
        </div>
        <div class="card kpi">
          <span class="label">Open alerts</span>
          <span class="value">{{ summary.data.value.openAlerts }}</span>
        </div>
      </section>
    </QueryBoundary>
    <QueryBoundary
      v-if="canCluster"
      :loading="cluster.isPending.value"
      :error="cluster.error.value"
      @retry="cluster.refetch()"
    >
      <section v-if="c" class="grid-cards" aria-label="Summary">
        <div class="card kpi">
          <span class="label">Cluster health</span>
          <span class="value"><StatusBadge :meta="HEALTH[c.health]" /></span>
          <span class="muted">Map version {{ formatNumber(c.mapVersion) }}</span>
        </div>
        <div class="card kpi">
          <span class="label">Servers</span>
          <span class="value">{{ c.servers.live }} / {{ c.servers.total }} live</span>
          <span class="muted">
            {{ c.servers.suspect }} suspect, {{ c.servers.dead }} dead,
            {{ c.servers.pendingRebalance }} pending rebalance
          </span>
          <RouterLink :to="{ name: 'servers' }">View servers</RouterLink>
        </div>
        <div class="card kpi" data-testid="kpi-under-replicated">
          <span class="label">Under-replicated buckets</span>
          <span class="value">
            {{ formatNumber(c.buckets.underReplicated) }}
            <StatusBadge
              v-if="c.buckets.underReplicated > 0"
              :meta="{ tone: 'warn', icon: '▲', label: 'Needs attention' }"
            />
          </span>
          <span class="muted">of {{ formatNumber(c.buckets.total) }} buckets</span>
        </div>
        <div class="card kpi" data-testid="kpi-unavailable">
          <span class="label">Unavailable buckets</span>
          <span class="value">
            {{ formatNumber(c.buckets.unavailable) }}
            <StatusBadge
              v-if="c.buckets.unavailable > 0"
              :meta="{ tone: 'danger', icon: '✖', label: 'Data unreachable' }"
            />
          </span>
          <span class="muted">{{ c.buckets.moving }} moving, {{ c.buckets.orphan }} orphan</span>
        </div>
        <div class="card kpi">
          <span class="label">Manager leader</span>
          <span class="value">
            <template v-if="c.managerLeader"
              >Server <SafeName :value="c.managerLeader.serverId" kind="text" :max="60"
            /></template>
            <StatusBadge v-else :meta="{ tone: 'neutral', icon: '○', label: 'Not reported' }" />
          </span>
          <span class="muted mono"
            ><SafeName
              v-if="c.managerLeader"
              :value="c.managerLeader.address"
              kind="text"
              :max="80"
          /></span>
        </div>
        <div class="card kpi">
          <span class="label">Auto-failover</span>
          <span class="value">{{ c.autoFailover.enabled ? 'On' : 'Off' }}</span>
          <span class="muted"
            >{{ c.autoFailover.eventsSinceReset }} of {{ c.autoFailover.maxCount }} events
            used</span
          >
        </div>
      </section>
    </QueryBoundary>
    <section aria-labelledby="charts-h" class="stack">
      <h2 id="charts-h">Last hour</h2>
      <ProblemNotice v-if="metricsUnsupported" :error="cpu.error.value" />
      <PartialNotice
        :completeness="metricsCompleteness"
        consequence="The charts show only what the other servers know; the values of these servers are missing."
      />
      <div v-if="!metricsUnsupported" class="grid-2">
        <div class="card">
          <QueryBoundary :loading="cpu.isPending.value" :error="cpu.error.value">
            <LineChart title="CPU usage" unit="percent" :series="toChartSeries(cpu.data.value)" />
          </QueryBoundary>
        </div>
        <div class="card">
          <QueryBoundary :loading="tps.isPending.value" :error="tps.error.value">
            <LineChart
              title="Transactions per second"
              unit="countPerSecond"
              :series="toChartSeries(tps.data.value)"
            />
          </QueryBoundary>
        </div>
        <div class="card">
          <QueryBoundary :loading="lag.isPending.value" :error="lag.error.value">
            <LineChart
              title="Replication lag"
              unit="seconds"
              :series="toChartSeries(lag.data.value)"
            />
          </QueryBoundary>
        </div>
        <div class="card">
          <QueryBoundary :loading="memory.isPending.value" :error="memory.error.value">
            <LineChart
              title="Memory used"
              unit="bytes"
              :series="toChartSeries(memory.data.value)"
            />
          </QueryBoundary>
        </div>
      </div>
    </section>
    <div v-if="canAlerts || canTasks" class="grid-2">
      <section v-if="canAlerts" class="card" aria-labelledby="alerts-h">
        <div class="row-between">
          <h2 id="alerts-h">Open alerts</h2>
          <RouterLink :to="{ name: 'alerts' }">All alerts</RouterLink>
        </div>
        <PartialNotice
          :completeness="alertsCompleteness"
          consequence="Alerts held by these servers are not shown."
        />
        <QueryBoundary
          :loading="alerts.isPending.value"
          :error="alerts.error.value"
          :empty="alerts.data.value?.items.length === 0"
          :empty-title="
            alertsCompleteness.partial
              ? 'No open alerts from the servers that answered'
              : 'No open alerts'
          "
          :empty-hint="
            alertsCompleteness.partial
              ? 'This does not mean there are none: the servers named above did not answer.'
              : 'Everything looks calm.'
          "
        >
          <ul class="plain-list">
            <li v-for="a in alerts.data.value?.items.slice(0, 5)" :key="a.id" class="row">
              <StatusBadge :meta="SEVERITY[a.severity]" />
              <span><SafeName :value="a.title" kind="text" :max="120" /></span>
              <span class="muted">{{ formatAgo(a.raisedAt) }}</span>
            </li>
          </ul>
        </QueryBoundary>
      </section>
      <section v-if="canTasks" class="card" aria-labelledby="tasks-h">
        <div class="row-between">
          <h2 id="tasks-h">Recent tasks</h2>
          <RouterLink :to="{ name: 'tasks' }">All tasks</RouterLink>
        </div>
        <QueryBoundary
          :loading="tasks.isPending.value"
          :error="tasks.error.value"
          :empty="tasks.data.value?.items.length === 0"
          empty-title="No tasks yet"
          empty-hint="Operations such as creating a database appear here."
        >
          <ul class="plain-list">
            <li v-for="t in tasks.data.value?.items" :key="t.id" class="row">
              <StatusBadge :meta="TASK_STATE[t.state]" />
              <span class="mono"><SafeName :value="t.kind" kind="text" :max="40" /></span>
              <span v-if="t.subject" class="muted"
                ><SafeName :value="t.subject.id" kind="text" :max="60"
              /></span>
              <span class="muted">{{ formatAgo(t.updatedAt) }}</span>
            </li>
          </ul>
        </QueryBoundary>
      </section>
    </div>
  </div>
</template>

<style scoped>
.plain-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--s-2);
}
</style>
