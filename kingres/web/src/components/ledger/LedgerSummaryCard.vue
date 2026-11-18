<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The dashboard card "Ledger integrity" (contract 0.11.0, design 8.5 and 6.9): one line of state, the age of the newest
// anchored aggregate, the number of chains read of the number expected, the last verification and its result, with links to
// the tabs of the Ledger page. It shows the states that exist without anchors: `Off`, `Not anchored` (information, never
// green), `Healthy` and `Attention` with the worst alert. A server that does not build an operation yet is said to.
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import NotAvailable from '@/components/NotAvailable.vue'
import { useAlerts, useLedgerStatusList } from '@/api/queries'
import { ledgerCard } from '@/lib/ledger-card'
import { NOT_ANCHORED_TEXT } from '@/lib/ledger-signing'
import { alertKindLabel, LEDGER_STATE, SEVERITY, type StatusMeta } from '@/lib/status'
import { formatAgo } from '@/lib/format'
import { useSessionStore } from '@/stores/session'

const session = useSessionStore()
const list = useLedgerStatusList()
const alerts = useAlerts('open', () => session.can('listAlerts'))
const card = computed(() =>
  list.data.value
    ? ledgerCard(
        list.data.value.items,
        alerts.data.value?.items ?? [],
        list.data.value.unreadable.length,
      )
    : null,
)
const STATE: Record<'off' | 'attention' | 'notAnchored' | 'healthy', StatusMeta> = {
  off: { tone: 'neutral', icon: '○', label: 'Off' },
  notAnchored: { tone: 'info', icon: 'ⓘ', label: 'Not anchored' },
  healthy: { tone: 'ok', icon: '✓', label: 'Healthy' },
  attention: { tone: 'warn', icon: '▲', label: 'Attention' },
}
</script>

<template>
  <section class="card stack" aria-labelledby="ledgercard-h" data-testid="ledger-card">
    <div class="row-between">
      <h2 id="ledgercard-h">Ledger integrity</h2>
      <RouterLink :to="{ name: 'ledger' }">Open</RouterLink>
    </div>
    <p v-if="list.isPending.value" class="muted" role="status">Loading.</p>
    <ProblemNotice v-else-if="list.error.value" :error="list.error.value" @retry="list.refetch()" />
    <template v-else-if="card">
      <p class="row" data-testid="ledger-card-state">
        <StatusBadge :meta="STATE[card.state]" />
        <span v-if="card.state === 'off'" class="muted">No database uses the ledger.</span>
      </p>
      <p v-if="card.state === 'notAnchored'" class="muted" data-testid="ledger-card-not-anchored">
        {{ NOT_ANCHORED_TEXT }}
      </p>
      <div
        v-if="card.state === 'attention'"
        class="stack"
        data-testid="ledger-card-attention"
        style="gap: var(--s-1)"
      >
        <p v-if="card.worstAlert" class="row">
          <StatusBadge :meta="SEVERITY[card.worstAlert.severity]" />
          <span
            ><SafeName :value="alertKindLabel(card.worstAlert.kind)" kind="text" :max="120" />:
            <SafeName :value="card.worstAlert.title" kind="text" :max="160"
          /></span>
        </p>
        <ul v-if="card.reasons.length" class="plain">
          <li v-for="r in card.reasons" :key="r">{{ r }}</li>
        </ul>
        <RouterLink :to="{ name: 'alerts' }">All alerts</RouterLink>
      </div>
      <ul v-if="card.state !== 'off'" class="plain facts">
        <li data-testid="ledger-card-anchor">
          <span class="label">Newest anchored aggregate</span>
          <span v-if="card.newestAnchoredAt" class="value">{{
            formatAgo(card.newestAnchoredAt)
          }}</span>
          <span v-else class="value">none</span>
        </li>
        <li data-testid="ledger-card-chains">
          <span class="label">Chains read</span>
          <span class="value">
            {{ card.chainsRead }}
            <template v-if="card.chainsExpected !== null">of {{ card.chainsExpected }}</template>
            <NotAvailable v-else />
          </span>
        </li>
        <li data-testid="ledger-card-verification">
          <span class="label">Last verification</span>
          <span v-if="card.lastVerifiedAt" class="value">
            <StatusBadge
              v-if="card.lastVerificationState"
              :meta="LEDGER_STATE[card.lastVerificationState]"
            />
            {{ formatAgo(card.lastVerifiedAt) }}
          </span>
          <span v-else class="value">never</span>
        </li>
      </ul>
      <p v-if="card.state !== 'off'" class="row">
        <RouterLink :to="{ name: 'ledger' }">Overview</RouterLink>
        <RouterLink :to="{ name: 'ledger', query: { tab: 'signing' } }">Signing</RouterLink>
        <RouterLink :to="{ name: 'ledger', query: { tab: 'publication' } }">Publication</RouterLink>
      </p>
    </template>
  </section>
</template>

<style scoped>
.plain {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--s-2);
}
.facts li {
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  gap: var(--s-1) var(--s-3);
}
.value {
  font-variant-numeric: tabular-nums;
  font-weight: 600;
}
</style>
