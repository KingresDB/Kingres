<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// What a failover would do (design 8.2 step 2, contract `FailoverPlan`): a block per group with the move, the
// candidates and their positions, the verdict about lost data in words, the estimated time, for a switchover the time
// writes pause, and the steps. Blockers are listed as messages; a running rebalance that will be stopped is stated.
import { computed } from 'vue'
import SafeName from './SafeName.vue'
import DataLossText from './DataLossText.vue'
import FailoverCandidates from './FailoverCandidates.vue'
import StatusBadge from './StatusBadge.vue'
import { blockerLabel } from '@/lib/failover'
import { groupTitle } from '@/lib/failover'
import { formatDuration } from '@/lib/format'
import type { FailoverPlan } from '@/api/types'

const props = defineProps<{ plan: FailoverPlan }>()
const none = computed(() => props.plan.groups.length === 0)
</script>

<template>
  <div class="stack" data-testid="failover-plan">
    <p
      v-if="plan.willStopRebalance"
      class="notice tone-info"
      role="status"
      data-testid="plan-stops-rebalance"
    >
      <span aria-hidden="true">&#9432;</span>
      <span>The running rebalance will be stopped first.</span>
    </p>
    <div v-if="plan.blockers.length > 0" class="notice tone-warn" data-testid="plan-blockers">
      <span aria-hidden="true">&#9650;</span>
      <div class="stack" style="gap: var(--s-1)">
        <strong>{{
          plan.blockers.every((b) => b.code === 'failoverNeedsDecision')
            ? 'This failover waits for your decision'
            : 'This failover cannot start as it is'
        }}</strong>
        <ul class="blockers">
          <li v-for="(b, i) in plan.blockers" :key="`${i}-${b.code}-${b.shardGroupId}`">
            <strong>{{ blockerLabel(b) }}.</strong>{{ ' ' }}
            <SafeName :value="b.message" kind="text" :max="300" />
          </li>
        </ul>
      </div>
    </div>
    <p v-if="none" class="muted" data-testid="plan-empty">
      No group would change: nothing is led by this server.
    </p>
    <section
      v-for="g in plan.groups"
      :key="g.shardGroupId"
      class="card stack plan-group"
      :aria-label="groupTitle(g.shardGroupId, g.isMapGroup)"
      :data-testid="`plan-group-${g.shardGroupId}`"
    >
      <h3 class="row">
        {{ groupTitle(g.shardGroupId, g.isMapGroup) }}
        <StatusBadge
          v-if="g.movesTheManager"
          :meta="{ tone: 'warn', icon: '▲', label: 'Moves the manager of the cluster' }"
        />
      </h3>
      <dl class="kv">
        <dt>From</dt>
        <dd><SafeName :value="g.fromServerId" kind="text" :max="60" /></dd>
        <dt>To</dt>
        <dd>
          <SafeName v-if="g.toServerId" :value="g.toServerId" kind="text" :max="60" />
          <span v-else class="muted">no standby qualifies</span>
        </dd>
        <dt>Estimated time</dt>
        <dd>
          <template v-if="g.estimatedSeconds !== null">{{
            formatDuration(g.estimatedSeconds)
          }}</template>
          <span v-else class="muted">not known</span>
        </dd>
        <template v-if="g.writesPausedSeconds !== null">
          <dt>Writes are paused for about</dt>
          <dd data-testid="plan-writes-paused">{{ formatDuration(g.writesPausedSeconds) }}</dd>
        </template>
      </dl>
      <DataLossText :data-loss="g.dataLoss" :group-id="g.shardGroupId" :at="plan.generatedAt" />
      <FailoverCandidates
        :candidates="g.candidates"
        :caption="`Standbys of ${groupTitle(g.shardGroupId, g.isMapGroup)} that could be promoted`"
      />
      <details v-if="g.steps.length > 0">
        <summary>Steps ({{ g.steps.length }})</summary>
        <ol>
          <li v-for="(st, i) in g.steps" :key="`${i}-${st}`">
            <SafeName :value="st" kind="text" :max="120" />
          </li>
        </ol>
      </details>
    </section>
  </div>
</template>

<style scoped>
.blockers {
  margin: 0;
  padding-left: var(--s-4);
}
</style>
