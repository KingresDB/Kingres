<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import PageHeader from '@/components/PageHeader.vue'
import QueryBoundary from '@/components/QueryBoundary.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import LineChart from '@/components/LineChart.vue'
import { useAlerts, useCluster, useMetric, useTasks } from '@/api/queries'
import { HEALTH, SEVERITY, TASK_STATE } from '@/lib/status'
import { formatAgo, formatNumber } from '@/lib/format'
import { toChartSeries } from '@/lib/metrics'
import { useSessionStore } from '@/stores/session'

const session = useSessionStore()
// What a kingres_stats account cannot read is left out instead of failing.
const canCluster = computed(() => session.can('getCluster'))
const canAlerts = computed(() => session.can('listAlerts'))
const canTasks = computed(() => session.can('listTasks'))
const cluster = useCluster(canCluster)
const alerts = useAlerts('open', canAlerts)
const tasks = useTasks(5, canTasks)
const cpu = useMetric('cpuUsagePercent', 60, 'server')
const tps = useMetric('transactionsPerSecond', 60)
const lag = useMetric('replicationLagSeconds', 60, 'server')
const memory = useMetric('memoryUsedBytes', 60, 'server')

const c = computed(() => cluster.data.value)
</script>

<template>
  <PageHeader title="Dashboard" subtitle="Cluster health at a glance." />
  <div class="stack-lg">
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
            <template v-if="c.managerLeader">Server {{ c.managerLeader.serverId }}</template>
            <StatusBadge v-else :meta="{ tone: 'danger', icon: '✖', label: 'No leader' }" />
          </span>
          <span class="muted mono">{{ c.managerLeader?.address }}</span>
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
      <div class="grid-2">
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
        <QueryBoundary
          :loading="alerts.isPending.value"
          :error="alerts.error.value"
          :empty="alerts.data.value?.items.length === 0"
          empty-title="No open alerts"
          empty-hint="Everything looks calm."
        >
          <ul class="plain-list">
            <li v-for="a in alerts.data.value?.items.slice(0, 5)" :key="a.id" class="row">
              <StatusBadge :meta="SEVERITY[a.severity]" />
              <span>{{ a.title }}</span>
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
              <span class="mono">{{ t.kind }}</span>
              <span v-if="t.subject" class="muted">{{ t.subject.id }}</span>
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
