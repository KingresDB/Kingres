<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The readiness line on the card of a shard group (design 8.1, contract `ShardGroup.failover`): whether a failover of
// the group is possible now and what it would risk, or what stands in the way. It says when the leader checked, because
// it is the leader's last standby check and not a live probe. A server that sends no readiness is an older revision.
import { computed } from 'vue'
import StatusBadge from './StatusBadge.vue'
import SafeName from './SafeName.vue'
import { blockerLabel, readinessWords } from '@/lib/failover'
import { formatAgo } from '@/lib/format'
import type { FailoverReadiness } from '@/api/types'

const props = defineProps<{ readiness: FailoverReadiness | undefined }>()
const words = computed(() => (props.readiness ? readinessWords(props.readiness) : null))
</script>

<template>
  <div
    class="stack failover-readiness"
    data-testid="failover-readiness"
    :data-state="words?.state ?? 'absent'"
  >
    <h3>Can this group fail over?</h3>
    <p v-if="!readiness || !words" class="muted" data-testid="readiness-absent">
      This server does not report failover readiness (it is an older revision).
    </p>
    <template v-else>
      <p class="row">
        <StatusBadge :meta="{ tone: words.tone, icon: words.icon, label: words.label }" />
        <span v-if="readiness.possible" data-testid="readiness-text">
          <SafeName :value="words.text" kind="text" :max="300" />
        </span>
        <span v-else data-testid="readiness-text">Failover is not possible:</span>
      </p>
      <ul v-if="readiness.blockers.length > 0" class="blockers" data-testid="readiness-blockers">
        <li v-for="(b, i) in readiness.blockers" :key="`${i}-${b.code}`">
          <strong>{{ blockerLabel(b) }}.</strong>{{ ' ' }}
          <SafeName :value="b.message" kind="text" :max="300" />
        </li>
      </ul>
      <p v-else-if="!readiness.possible" class="muted">The server did not say why.</p>
      <p class="muted">Checked {{ formatAgo(readiness.checkedAt) }} by the manager leader.</p>
    </template>
  </div>
</template>

<style scoped>
.blockers {
  margin: 0;
  padding-left: var(--s-4);
}
</style>
