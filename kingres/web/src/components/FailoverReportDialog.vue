<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// One failover report (GET /failover/reports/{id}), modelled on the rebalance report (design 8.3): the summary, then for
// each group the move, the candidates in the order that chose, the verdict about lost data in words with its numbers,
// the terms before and after, the lease wait, the nodes that were not reached, the incidents with the extension's own
// text, the state of the old primary and the timeline of events. Every string from the server goes through SafeName.
import SafeName from './SafeName.vue'
import AppDialog from './AppDialog.vue'
import QueryBoundary from './QueryBoundary.vue'
import StatusBadge from './StatusBadge.vue'
import DataLossText from './DataLossText.vue'
import FailoverCandidates from './FailoverCandidates.vue'
import TableWrap from './TableWrap.vue'
import { useFailoverReport } from '@/api/queries'
import {
  groupOutcomeMeta,
  groupTitle,
  KIND_LABEL,
  MODE_LABEL,
  oldPrimaryWords,
  outcomeMeta,
  TRIGGER_LABEL,
} from '@/lib/failover'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'

const props = defineProps<{ reportId: number }>()
const emit = defineEmits<{ close: [] }>()
const report = useFailoverReport(() => props.reportId)
const bytes = (n: number): string => formatBytes(n)
</script>

<template>
  <AppDialog :title="`Failover report #${reportId}`" wide @close="emit('close')">
    <QueryBoundary
      :loading="report.isPending.value"
      :error="report.error.value"
      @retry="report.refetch()"
    >
      <template v-if="report.data.value">
        <dl class="kv" data-testid="report-summary">
          <dt>Outcome</dt>
          <dd data-testid="report-outcome">
            <StatusBadge :meta="outcomeMeta(report.data.value.outcome)" />
          </dd>
          <dt>Kind</dt>
          <dd>{{ KIND_LABEL[report.data.value.kind] ?? report.data.value.kind }}</dd>
          <dt>Mode</dt>
          <dd>{{ MODE_LABEL[report.data.value.mode] ?? report.data.value.mode }}</dd>
          <dt>Started</dt>
          <dd>
            {{ TRIGGER_LABEL[report.data.value.trigger] ?? report.data.value.trigger }}, by
            <SafeName :value="report.data.value.requestedBy" :max="60" />
          </dd>
          <template v-if="report.data.value.serverId">
            <dt>Server</dt>
            <dd><SafeName :value="report.data.value.serverId" kind="text" :max="60" /></dd>
          </template>
          <dt>Time</dt>
          <dd>
            {{ formatDateTime(report.data.value.startedAt) }} to
            {{
              report.data.value.finishedAt
                ? formatDateTime(report.data.value.finishedAt)
                : 'not finished'
            }},
            {{ formatDuration(report.data.value.totals.durationSeconds) }}
          </dd>
          <dt>Groups</dt>
          <dd>
            {{ report.data.value.totals.groupsDone }} done,
            {{ report.data.value.totals.groupsFailed }} failed,
            {{ report.data.value.totals.groupsSkipped }} skipped,
            {{ report.data.value.totals.groupsTotal }} in all
          </dd>
          <dt>Data loss</dt>
          <dd data-testid="report-data-loss-possible">
            {{
              report.data.value.dataLossPossible
                ? 'Possible for at least one group'
                : 'None possible'
            }}
          </dd>
          <template v-if="report.data.value.options.comment">
            <dt>Comment</dt>
            <dd>
              <SafeName
                :value="report.data.value.options.comment"
                kind="text"
                :max="500"
                multiline
              />
            </dd>
          </template>
          <template v-if="report.data.value.error">
            <dt>Reason</dt>
            <dd data-testid="report-error">
              <SafeName :value="report.data.value.error" kind="text" :max="400" multiline />
            </dd>
          </template>
        </dl>

        <section
          v-for="g in report.data.value.groups"
          :key="g.shardGroupId"
          class="card stack"
          :aria-label="groupTitle(g.shardGroupId, g.shardGroupId === 0)"
          :data-testid="`report-group-${g.shardGroupId}`"
          :data-outcome="g.outcome"
        >
          <h3 class="row">
            {{ groupTitle(g.shardGroupId, g.shardGroupId === 0) }}
            <StatusBadge :meta="groupOutcomeMeta(g.outcome)" />
          </h3>
          <dl class="kv">
            <dt>From</dt>
            <dd><SafeName :value="g.fromServerId" kind="text" :max="60" /></dd>
            <dt>To</dt>
            <dd>
              <SafeName v-if="g.toServerId" :value="g.toServerId" kind="text" :max="60" />
              <span v-else class="muted">no standby was promoted</span>
            </dd>
            <template v-if="g.skipReason">
              <dt>Skipped because</dt>
              <dd><SafeName :value="g.skipReason" kind="text" :max="300" /></dd>
            </template>
            <dt>Terms</dt>
            <dd data-testid="report-terms">
              <template v-if="g.termBefore !== null || g.termAfter !== null"
                >{{ g.termBefore ?? 'not known' }} before,
                {{ g.termAfter ?? 'not known' }} after<template v-if="g.timeline !== null"
                  >; timeline {{ g.timeline }}</template
                ></template
              >
              <span v-else class="muted">not recorded</span>
            </dd>
            <dt>Wait for the old lease</dt>
            <dd data-testid="report-lease-wait">
              <template v-if="g.leaseWaitMs !== null">{{
                formatDuration(g.leaseWaitMs / 1000)
              }}</template>
              <span v-else class="muted">none (the old primary answered)</span>
            </dd>
            <dt>Took</dt>
            <dd>{{ formatDuration(g.durationSeconds) }}</dd>
            <template v-if="g.unreachedServerIds.length > 0">
              <dt>Not reached</dt>
              <dd data-testid="report-unreached">
                <template v-for="(s, i) in g.unreachedServerIds" :key="`${i}-${s}`"
                  ><template v-if="i > 0">, </template><SafeName :value="s" kind="text" :max="60"
                /></template>
              </dd>
            </template>
          </dl>
          <div
            v-if="g.error"
            class="notice tone-danger"
            role="group"
            data-testid="report-group-error"
          >
            <span aria-hidden="true">&#10006;</span>
            <span><SafeName :value="g.error" kind="text" :max="400" multiline /></span>
          </div>
          <h4>Data loss</h4>
          <DataLossText
            :data-loss="g.dataLoss"
            :group-id="g.shardGroupId"
            :at="report.data.value.startedAt"
            numbers
          />
          <h4>Standbys, in the order that chose</h4>
          <FailoverCandidates
            :candidates="g.candidates"
            :caption="`Candidates of ${groupTitle(g.shardGroupId, g.shardGroupId === 0)}`"
          />
          <template v-if="g.incidents.length > 0">
            <h4>Incidents reported by the sharding extension</h4>
            <ul data-testid="report-incidents">
              <li v-for="(inc, i) in g.incidents" :key="`${i}-${inc.kind}`">
                <code><SafeName :value="inc.kind" kind="text" :max="80" /></code>:
                <SafeName :value="inc.message" kind="text" :max="400" multiline />
              </li>
            </ul>
          </template>
          <h4>The old primary</h4>
          <p data-testid="report-old-primary" :data-state="g.oldPrimary.state">
            <StatusBadge
              :meta="{
                tone: oldPrimaryWords(g.oldPrimary, bytes).tone,
                icon: oldPrimaryWords(g.oldPrimary, bytes).icon,
                label: oldPrimaryWords(g.oldPrimary, bytes).label,
              }"
            />
            {{ oldPrimaryWords(g.oldPrimary, bytes).text }}
          </p>
          <p v-if="g.oldPrimary.message" class="muted">
            <SafeName :value="g.oldPrimary.message" kind="text" :max="400" multiline />
          </p>
        </section>

        <h3>Timeline</h3>
        <p v-if="report.data.value.events.length === 0" class="muted">The report has no events.</p>
        <TableWrap v-else data-testid="report-events">
          <table class="table">
            <caption class="sr-only">
              Events of the failover
            </caption>
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col">Group</th>
                <th scope="col">Step</th>
                <th scope="col">What happened</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="(e, i) in report.data.value.events" :key="`${i}-${e.at}`">
                <td class="nowrap">{{ formatDateTime(e.at) }}</td>
                <td>
                  <template v-if="e.shardGroupId !== null">{{ e.shardGroupId }}</template>
                  <span v-else class="muted">all</span>
                </td>
                <td><SafeName :value="e.step" kind="text" :max="100" /></td>
                <td>
                  <code><SafeName :value="e.kind" kind="text" :max="60" /></code>{{ ' ' }}
                  <SafeName :value="e.message" kind="text" :max="400" multiline />
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </template>
    </QueryBoundary>
  </AppDialog>
</template>
