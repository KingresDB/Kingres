<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<script setup lang="ts">
// One aggregate checkpoint of a database (contract 0.11.0, design 5.2, 5.3 and 5.6): the signed checkpoint with a copy
// button, the members (one per chain, with the flags stale, diverged and retired), the anchors and the statement of what
// the aggregate proves and does not prove. Everything the server sends is text and is drawn through SafeName.
import { computed, ref } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import AppDialog from '@/components/AppDialog.vue'
import ProblemNotice from '@/components/ProblemNotice.vue'
import SafeName from '@/components/SafeName.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import TableWrap from '@/components/TableWrap.vue'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { formatDateTime, formatNumber, shortHash } from '@/lib/format'
import { groupList } from '@/lib/ledger-coverage'
import { memberFlagText, NOT_ANCHORED_TEXT } from '@/lib/ledger-signing'
import { saveTextFile } from '@/lib/download'

const props = defineProps<{ databaseName: string; size: number }>()
defineEmits<{ close: [] }>()

const query = useQuery({
  queryKey: computed(() => [...keys.ledger(props.databaseName), 'aggregate', props.size]),
  queryFn: () => endpoints.getLedgerAggregate(props.databaseName, props.size),
  retry: false,
})
const agg = computed(() => query.data.value)
const copied = ref<'' | 'done' | 'failed'>('')

async function copyCheckpoint(): Promise<void> {
  try {
    await navigator.clipboard.writeText(agg.value?.checkpoint ?? '')
    copied.value = 'done'
  } catch {
    copied.value = 'failed'
  }
}
/** The signed note exactly as sent: what is signed is its bytes. */
function download(): void {
  if (agg.value)
    saveTextFile(`aggregate-checkpoint-${agg.value.size}.txt`, agg.value.checkpoint, 'text/plain')
}
const KNOWN_ANCHOR_STATE: Record<string, string> = {
  pending: 'Pending',
  submitted: 'Submitted, not final',
  anchored: 'Anchored',
  failed: 'Failed',
  superseded: 'Replaced by a newer checkpoint',
  expired: 'Never confirmed in time',
}
</script>

<template>
  <AppDialog :title="`Aggregate checkpoint ${size}`" wide @close="$emit('close')">
    <p v-if="query.isPending.value" role="status">Loading the aggregate checkpoint ...</p>
    <ProblemNotice v-else-if="query.error.value" :error="query.error.value" />
    <div v-else-if="agg" class="stack-lg" data-testid="aggregate">
      <p class="muted">
        Size {{ formatNumber(agg.size) }}, made {{ formatDateTime(agg.generatedAt) }}. Root
        <code class="mono wrap" :title="agg.root"
          ><SafeName :value="agg.root" kind="text" :max="80" /></code
        >.
      </p>
      <p
        v-if="!agg.complete"
        class="notice tone-warn"
        role="status"
        data-testid="aggregate-incomplete"
      >
        <span aria-hidden="true">&#9650;</span>
        <span
          >This aggregate does not cover {{ groupList(agg.uncoveredShardGroupIds) }}: it proves
          nothing about them.</span
        >
      </p>

      <section class="stack" aria-labelledby="agg-statement-h">
        <h3 id="agg-statement-h">What this aggregate proves</h3>
        <p class="notice tone-info" data-testid="aggregate-statement">
          <span aria-hidden="true">&#9432;</span>
          <span><SafeName :value="agg.statement" kind="text" :max="2000" multiline /></span>
        </p>
      </section>

      <section class="stack" aria-labelledby="agg-members-h">
        <h3 id="agg-members-h">Members</h3>
        <TableWrap>
          <table class="table">
            <caption class="sr-only">
              Members of the aggregate
            </caption>
            <thead>
              <tr>
                <th scope="col">Chain (shard group)</th>
                <th scope="col" class="num">Epoch</th>
                <th scope="col" class="num">Height</th>
                <th scope="col">Block hash</th>
                <th scope="col">Log root</th>
                <th scope="col">Flags</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="m in agg.members" :key="`${m.chainId}-${m.epoch}`">
                <th scope="row">
                  Group {{ m.shardGroupId }}
                  <div class="mono muted" :title="m.chainId">
                    <SafeName :value="shortHash(m.chainId)" kind="text" :max="30" />
                  </div>
                </th>
                <td class="num">{{ m.epoch }}</td>
                <td class="num">{{ formatNumber(m.height) }}</td>
                <td class="mono" :title="m.blockHash">{{ shortHash(m.blockHash) }}</td>
                <td class="mono" :title="m.logRoot">
                  <SafeName :value="shortHash(m.logRoot)" kind="text" :max="30" />
                </td>
                <td>
                  <ul v-if="m.flags.length" class="plain">
                    <li v-for="f in m.flags" :key="f" :data-testid="`flag-${f}`">
                      <StatusBadge
                        :meta="{
                          tone: f === 'diverged' ? 'danger' : 'warn',
                          icon: f === 'diverged' ? '✖' : '▲',
                          label: memberFlagText(f) ? f : 'Unknown flag',
                        }"
                      />
                      <span class="muted"
                        ><SafeName :value="memberFlagText(f) ?? f" kind="text" :max="200"
                      /></span>
                    </li>
                  </ul>
                  <span v-else class="muted">None</span>
                </td>
              </tr>
            </tbody>
          </table>
        </TableWrap>
      </section>

      <section class="stack" aria-labelledby="agg-anchors-h">
        <h3 id="agg-anchors-h">Anchors</h3>
        <p
          v-if="agg.anchors.length === 0"
          class="notice tone-warn"
          data-testid="aggregate-not-anchored"
        >
          <span aria-hidden="true">&#9650;</span>
          <span>{{ NOT_ANCHORED_TEXT }}</span>
        </p>
        <ul v-else class="plain" data-testid="aggregate-anchors">
          <li v-for="a in agg.anchors" :key="a.id">
            <SafeName :value="a.targetName" kind="name" :max="63" />
            (<SafeName :value="a.kind" kind="text" :max="10" />):
            <SafeName :value="KNOWN_ANCHOR_STATE[a.state] ?? a.state" kind="text" :max="60" />
            <template v-if="a.evidenceTime">, time {{ formatDateTime(a.evidenceTime) }}</template>
            <template v-if="a.lastError"
              >. <SafeName :value="a.lastError" kind="text" :max="200"
            /></template>
          </li>
        </ul>
      </section>

      <section class="stack" aria-labelledby="agg-note-h">
        <h3 id="agg-note-h">Signed checkpoint</h3>
        <pre class="note mono" tabindex="0" aria-label="The signed checkpoint">{{
          agg.checkpoint
        }}</pre>
        <div class="row">
          <button type="button" class="btn btn-small" @click="copyCheckpoint">
            Copy the checkpoint
          </button>
          <button type="button" class="btn btn-small" @click="download">Download checkpoint</button>
          <span v-if="copied === 'done'" role="status">Copied.</span>
          <span v-else-if="copied === 'failed'" role="status"
            >The browser did not allow copying. Select the text instead.</span
          >
        </div>
      </section>
    </div>
    <template #footer>
      <button type="button" class="btn btn-primary" @click="$emit('close')">Close</button>
    </template>
  </AppDialog>
</template>

<style scoped>
.note {
  margin: 0;
  padding: var(--s-3);
  overflow-x: auto;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  /* A hidden character in the text cannot reorder what is drawn: the note is always read left to right. */
  direction: ltr;
  unicode-bidi: isolate-override;
  background: var(--c-surface-2);
  border-radius: var(--radius-sm);
}
.plain {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--s-1);
}
</style>
