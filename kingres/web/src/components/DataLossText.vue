<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// The verdict about lost data of one group, in words (design 2.6 and 8.2): the sentence, a badge with symbol and
// text (never colour alone), and on request the numbers behind it. The same view is used by the dialog, the progress
// and the report. Every string that came from the server goes through SafeName.
import { computed } from 'vue'
import StatusBadge from './StatusBadge.vue'
import SafeName from './SafeName.vue'
import { dataLossWords } from '@/lib/failover'
import { formatDuration } from '@/lib/format'
import type { DataLossAssessment } from '@/api/types'

const props = withDefaults(
  defineProps<{
    dataLoss: DataLossAssessment
    groupId: number | null
    /** The time of the plan or report this belongs to (for "since <time>"). */
    at?: string | null
    /** Also list the positions, the age of the last check and the databases. */
    numbers?: boolean
  }>(),
  { at: null, numbers: false },
)
const words = computed(() => dataLossWords(props.dataLoss, props.groupId, props.at))
</script>

<template>
  <div class="stack data-loss" data-testid="data-loss" :data-verdict="dataLoss.verdict">
    <p class="row">
      <StatusBadge :meta="{ tone: words.tone, icon: words.icon, label: words.label }" />
      <span data-testid="data-loss-text"
        ><SafeName :value="words.text" kind="text" :max="400"
      /></span>
    </p>
    <p v-if="dataLoss.message" class="muted" data-testid="data-loss-message">
      <SafeName :value="dataLoss.message" kind="text" :max="400" multiline />
    </p>
    <dl v-if="numbers" class="kv" data-testid="data-loss-numbers">
      <dt>Last position of the old primary the leader saw</dt>
      <dd class="mono">{{ dataLoss.lastKnownPrimaryLsn ?? 'not known' }}</dd>
      <dt>Position of the chosen standby</dt>
      <dd class="mono">{{ dataLoss.chosenReceiveLsn ?? 'not known' }}</dd>
      <dt>Age of that check at the failure</dt>
      <dd>
        <template v-if="dataLoss.lastKnownAgeSeconds !== null">{{
          formatDuration(dataLoss.lastKnownAgeSeconds)
        }}</template>
        <template v-else>not known</template>
      </dd>
      <dt>Ran without its synchronous standby</dt>
      <dd>{{ dataLoss.groupRanWithoutSynchronousStandby ? 'yes' : 'no' }}</dd>
      <template v-if="dataLoss.asyncDatabases.length > 0">
        <dt>Databases that acknowledge asynchronously</dt>
        <dd>
          <template v-for="(d, i) in dataLoss.asyncDatabases" :key="`${i}-${d}`"
            ><template v-if="i > 0">, </template><SafeName :value="d" :max="60"
          /></template>
        </dd>
      </template>
      <template v-if="dataLoss.unreadableServerIds.length > 0">
        <dt>Standbys that could not be read</dt>
        <dd>
          <template v-for="(d, i) in dataLoss.unreadableServerIds" :key="`${i}-${d}`"
            ><template v-if="i > 0">, </template><SafeName :value="d" kind="text" :max="60"
          /></template>
        </dd>
      </template>
    </dl>
  </div>
</template>
