<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The standbys that could be promoted (plan and report): their position, how far they are behind, their sync state
// and whether they qualify, in text and not by colour alone. The rank is the order the server chose them in (the best
// position first).
import StatusBadge from './StatusBadge.vue'
import SafeName from './SafeName.vue'
import TableWrap from './TableWrap.vue'
import { SYNC_STATE } from '@/lib/status'
import { formatBytes } from '@/lib/format'
import type { FailoverCandidate } from '@/api/types'

defineProps<{ candidates: FailoverCandidate[]; caption: string }>()
</script>

<template>
  <p v-if="candidates.length === 0" class="muted" data-testid="no-candidates">
    The group has no standby that could be promoted.
  </p>
  <TableWrap v-else :label="caption" data-testid="candidates">
    <table class="table">
      <caption class="sr-only">
        {{
          caption
        }}
      </caption>
      <thead>
        <tr>
          <th scope="col" class="num">Rank</th>
          <th scope="col">Server</th>
          <th scope="col">Position</th>
          <th scope="col" class="num">Behind</th>
          <th scope="col">Sync state</th>
          <th scope="col">Result</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="(c, i) in candidates" :key="`${i}-${c.serverId}`" :data-chosen="c.chosen">
          <td class="num">{{ i + 1 }}</td>
          <th scope="row"><SafeName :value="c.serverId" kind="text" :max="60" /></th>
          <td class="mono">
            <template v-if="c.readable">{{ c.receiveLsn ?? 'not known' }}</template>
            <span v-else class="muted">could not be read</span>
          </td>
          <td class="num">
            <template v-if="c.lagBytes !== null">{{ formatBytes(c.lagBytes) }}</template>
            <span v-else class="muted">not known</span>
          </td>
          <td>
            <StatusBadge v-if="c.syncState" :meta="SYNC_STATE[c.syncState]" />
            <span v-else class="muted">not streaming</span>
          </td>
          <td>
            <StatusBadge
              v-if="c.chosen"
              :meta="{ tone: 'ok', icon: '✓', label: 'Chosen' }"
              data-testid="candidate-chosen"
            />
            <template v-else-if="!c.eligible">
              <StatusBadge :meta="{ tone: 'neutral', icon: '○', label: 'Not eligible' }" />
              <div v-if="c.ineligibleReason" class="muted">
                <SafeName :value="c.ineligibleReason" kind="text" :max="200" />
              </div>
            </template>
            <span v-else class="muted">Eligible, not chosen</span>
          </td>
        </tr>
      </tbody>
    </table>
  </TableWrap>
</template>
