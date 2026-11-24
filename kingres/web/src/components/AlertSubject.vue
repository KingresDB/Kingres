<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// What an alert is about (contract 0.7.0, `Alert.subject`): the server, the server that is behind, the shard group,
// the database and a qualifier, each as a link to its page where the signed-in role may open that page. The members
// are typed; the message of the alert is never parsed. A server of an older revision sends no subject: then the
// members `serverId` and `databaseName` of the alert itself are used, as before.
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import SafeName from './SafeName.vue'
import { isAddressableSegment } from '@/lib/path-segment'
import { groupLabel } from '@/lib/cluster'
import { useSessionStore } from '@/stores/session'
import type { Alert } from '@/api/types'

const props = withDefaults(defineProps<{ alert: Alert; compact?: boolean }>(), { compact: false })
const session = useSessionStore()

const subject = computed(
  () =>
    props.alert.subject ?? {
      serverId: props.alert.serverId ?? null,
      behindServerId: null,
      shardGroupId: null,
      databaseName: props.alert.databaseName ?? null,
      aspect: null,
    },
)
const canServer = computed(() => session.can('getServer'))
const canGroup = computed(() => session.can('getReplicationTopology'))
const canDatabase = computed(() => session.can('getDatabase'))
const linkable = (id: string): boolean => canServer.value && isAddressableSegment(id)

/** Words for the qualifiers the contract names; any other one is shown as sent. */
const ASPECT: Record<string, string> = {
  ca: 'Cluster certificate authority',
  // 0.12.0: the system database is older than what the ledger published (the alert kind is `ledgerSignerUnavailable`).
  'published-state': 'The system database is older than what was published',
}
const isPublishedState = computed(() => subject.value.aspect === 'published-state')
// 0.11.0: `target:<id>` is an anchor target of the ledger (the number only; anything else is shown as sent).
const aspectText = computed(() => {
  const a = subject.value.aspect
  if (a === null || a === undefined) return null
  const target = /^target:(\d{1,18})$/.exec(a)
  return target ? `Anchor target ${target[1]}` : (ASPECT[a] ?? null)
})
// 0.10.0: an alert about a failover links to where its progress and report are shown.
const isFailoverAlert = computed(() =>
  [
    'failoverNeedsDecision',
    'failoverBlocked',
    'failoverDataLoss',
    'autoFailoverTriggered',
  ].includes(props.alert.kind),
)
// 0.10.0: the alert that the limit of automatic failovers is reached links to the counter and its reset on Settings.
const isLimitAlert = computed(() => props.alert.kind === 'autoFailoverLimitReached')
const canSettings = computed(() => session.can('getClusterSettings'))
const canFailover = computed(() => session.can('getFailoverStatus'))
// 0.11.0: an alert about the ledger links to the tab of the Ledger page where it is explained and handled.
const LEDGER_TAB: Record<string, string | undefined> = {
  ledgerSignerUnavailable: 'signing',
  ledgerNotAnchored: 'anchoring',
  ledgerAnchorBehind: 'anchoring',
  ledgerAnchorFailed: 'anchoring',
  ledgerAnchorConflict: 'anchoring',
  ledgerPublicationBehind: 'publication',
  ledgerChainDiverged: 'overview',
  ledgerGroupUnread: 'publication',
}
const isLedgerAlert = computed(() => props.alert.kind.startsWith('ledger'))
const canLedger = computed(() => session.can('listLedgerStatus'))
const ledgerLink = computed(() => {
  const db = subject.value.databaseName
  const tab = isPublishedState.value ? 'publication' : LEDGER_TAB[props.alert.kind]
  return {
    name: 'ledger',
    query: {
      ...(db !== null && isAddressableSegment(db) ? { db } : {}),
      ...(tab && tab !== 'overview' ? { tab } : {}),
    },
  }
})
const ledgerLinkText = computed(() => {
  if (isPublishedState.value) return 'What was published and how to go on'
  if (props.alert.kind === 'ledgerChainDiverged')
    return 'Acknowledge the divergence on the Overview tab'
  const tab = LEDGER_TAB[props.alert.kind]
  return tab === 'signing'
    ? 'Signing keys of the ledger'
    : tab === 'anchoring'
      ? 'Anchoring of the ledger'
      : tab === 'publication'
        ? 'Publication of the ledger'
        : 'Ledger page'
})
// 0.18.0: an alert about a time-partitioning policy names its table in the typed members `tableSchema` and `tableName` of the
// subject and links to the partitions panel of that table. The names are used as they are: a name with a dot is unambiguous.
const isPolicyAlert = computed(() =>
  ['partitionsAheadLow', 'partitionPolicyFailed'].includes(props.alert.kind),
)
const policyTable = computed(() => {
  const s = subject.value as { tableSchema?: string | null; tableName?: string | null }
  return s.tableSchema && s.tableName ? { schema: s.tableSchema, table: s.tableName } : null
})
const canTable = computed(() => session.can('getTable'))
const isPending = computed(() => props.alert.kind === 'replicationPending')
// 0.8.0: a clock that differs is explained and handled on the Time tab of the Configuration page.
const canTime = computed(
  () => props.alert.kind === 'clockDifference' && session.can('getClusterTime'),
)
const any = computed(
  () =>
    (isFailoverAlert.value && canFailover.value) ||
    (isLimitAlert.value && canSettings.value) ||
    (isLedgerAlert.value && canLedger.value) ||
    subject.value.serverId !== null ||
    subject.value.behindServerId !== null ||
    subject.value.shardGroupId !== null ||
    subject.value.databaseName !== null ||
    subject.value.aspect !== null,
)
</script>

<template>
  <ul v-if="any" class="subject muted" :class="{ compact }" data-testid="alert-subject">
    <li v-if="subject.serverId" data-testid="alert-server">
      {{ isPending ? 'Leader that keeps the change waiting: server' : 'Server' }}
      <RouterLink
        v-if="linkable(subject.serverId)"
        :to="{ name: 'server', params: { id: subject.serverId } }"
        ><SafeName :value="subject.serverId" kind="text" :max="60"
      /></RouterLink>
      <SafeName v-else :value="subject.serverId" kind="text" :max="60" />
    </li>
    <li v-if="subject.behindServerId" data-testid="alert-behind-server">
      Server that is behind:
      <RouterLink
        v-if="linkable(subject.behindServerId)"
        :to="{ name: 'server', params: { id: subject.behindServerId } }"
        ><SafeName :value="subject.behindServerId" kind="text" :max="60"
      /></RouterLink>
      <SafeName v-else :value="subject.behindServerId" kind="text" :max="60" />
    </li>
    <li v-if="subject.shardGroupId !== null" data-testid="alert-group">
      <RouterLink
        v-if="canGroup"
        :to="{ name: 'replication', hash: `#g-${subject.shardGroupId}` }"
        >{{ groupLabel(subject.shardGroupId, subject.shardGroupId === 0) }}</RouterLink
      >
      <template v-else>{{ groupLabel(subject.shardGroupId, subject.shardGroupId === 0) }}</template>
    </li>
    <li v-if="subject.databaseName" data-testid="alert-database">
      Database
      <RouterLink
        v-if="canDatabase && isAddressableSegment(subject.databaseName)"
        :to="{ name: 'database', params: { name: subject.databaseName } }"
        ><SafeName :value="subject.databaseName" :max="60"
      /></RouterLink>
      <SafeName v-else :value="subject.databaseName" :max="60" />
    </li>
    <li v-if="isFailoverAlert && canFailover" data-testid="alert-failover">
      <RouterLink :to="{ name: 'replication', hash: '#failover-top' }"
        >Failover progress and report</RouterLink
      >
    </li>
    <li v-if="isLimitAlert && canSettings" data-testid="alert-auto-failover-settings">
      <RouterLink :to="{ name: 'settings' }"
        >Automatic failover settings and the counter</RouterLink
      >
    </li>
    <li v-if="isLedgerAlert && canLedger" data-testid="alert-ledger">
      <RouterLink :to="ledgerLink">{{ ledgerLinkText }}</RouterLink>
    </li>
    <li
      v-if="isPolicyAlert && policyTable && subject.databaseName"
      data-testid="alert-policy-table"
    >
      Table
      <RouterLink
        v-if="
          canTable &&
          isAddressableSegment(subject.databaseName) &&
          isAddressableSegment(policyTable.schema) &&
          isAddressableSegment(policyTable.table)
        "
        :to="{
          name: 'table',
          params: {
            name: subject.databaseName,
            schema: policyTable.schema,
            table: policyTable.table,
          },
        }"
        ><SafeName :value="`${policyTable.schema}.${policyTable.table}`" :max="120"
      /></RouterLink>
      <SafeName v-else :value="`${policyTable.schema}.${policyTable.table}`" :max="120" />
      (time partitioning)
    </li>
    <li v-if="canTime" data-testid="alert-clock">
      <RouterLink :to="{ name: 'configuration', query: { tab: 'time' } }"
        >Clocks and what to do</RouterLink
      >
    </li>
    <li v-if="props.alert.kind === 'ledgerChainDiverged'" data-testid="alert-diverged">
      A log that was closed and answers again, or a chain that no longer extends its log, is shown
      here with the shard group, the height and why the log ended (the text of the alert). Read it,
      find out what happened, then acknowledge it to start a new epoch: the old log stays as it
      ended.
    </li>
    <li v-if="isPublishedState" data-testid="alert-published-state">
      The system database has gone back to an older state than what the ledger published, for
      example after a restore from a backup. Nothing is signed until this is resolved, so that no
      checkpoint contradicts what auditors already hold.
    </li>
    <li v-if="subject.aspect && !(isPolicyAlert && policyTable)" data-testid="alert-aspect">
      <template v-if="aspectText">{{ aspectText }}</template>
      <template v-else
        >Concerns <SafeName :value="subject.aspect" kind="text" :max="60"
      /></template>
    </li>
  </ul>
</template>

<style scoped>
.subject {
  list-style: none;
  margin: 0;
  padding: 0;
}
.subject a {
  display: inline-block;
  padding-block: 3px;
}
.subject.compact {
  display: flex;
  flex-wrap: wrap;
  gap: 0 var(--s-3);
}
</style>
